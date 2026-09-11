// Package device enumerates files on the phone.
//
// Read-only: nothing here modifies the device.
package device

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"quickadbackup/internal/adbproto"
)

// DefaultRoot is the canonical path to shared storage.
//
// Use the real path rather than /sdcard: /sdcard is a symlink to
// /storage/self/primary, which is itself a symlink to /storage/emulated/0, and
// find will not descend into a symlinked starting point. `find /sdcard -type f`
// silently returns nothing at all.
const DefaultRoot = "/storage/emulated/0"

// DefaultExcludes are skipped unless the caller overrides them.
//
// Android/data and Android/obb are app-private scoped storage: unreadable to
// the adb shell user on Android 11+, and app caches rather than user data. On
// the test device they held 12960 files that would produce nothing but errors.
var DefaultExcludes = []string{"Android/data", "Android/obb"}

// maxReportedErrors bounds how many of find's complaints are carried back. They
// are for a human to read, not to process, so a sample is enough and an
// unreadable tree with thousands of entries must not balloon the payload.
const maxReportedErrors = 200

// Entry is one regular file on the device.
type Entry struct {
	Rel    string `json:"rel"`   // path relative to root, slash-separated
	Size   int64  `json:"size"`  //
	MTimeN int64  `json:"mtime"` // modification time, unix nanoseconds
}

// Listing is the result of one enumeration pass.
type Listing struct {
	Root    string
	Entries []Entry
	// Skipped counts files that find reported but that were filtered out.
	Skipped int
	// Errors are the paths find could not read, in its own words.
	//
	// They are surfaced rather than discarded because a directory the shell user
	// cannot open disappears from the listing and from the cross-check alike: the
	// two agree, the run looks clean, and everything underneath is missing from
	// the backup with nothing said about it.
	Errors []string
	// ErrorsTruncated reports that more paths failed than Errors names.
	ErrorsTruncated bool
}

func (l *Listing) TotalBytes() int64 {
	var n int64
	for _, e := range l.Entries {
		n += e.Size
	}
	return n
}

// SupportsPrintf reports whether the device's find implements -printf.
//
// It is a GNU extension, but the toybox build shipped on recent Android does
// provide it, including nanosecond %T@. Without it there is no safe one-shot
// listing command (see List).
func SupportsPrintf(ctx context.Context, c *adbproto.Conn, root string) bool {
	out, err := c.Exec("find " + adbproto.ShellQuote(root) + ` -maxdepth 0 -printf '%s|%T@|%p\n' 2>&1`)
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Count(s, "|") >= 2 &&
		!strings.Contains(s, "Unknown option") &&
		!strings.Contains(s, "bad flag")
}

// List enumerates every regular file under root in a single device command.
//
// This runs a shell command rather than walking the sync service's LIST,
// because LIST costs one round trip per directory. Measured on a Pixel 7 with
// 7959 directories, the recursive sync walk took 29.1 s against 2.9 s for one
// find.
//
// The result is cross-checked against a plain file count, because a truncated
// listing is the most dangerous failure mode this tool has: it would look like
// a successful backup while silently omitting files. Everything else here
// follows from the same rule. A line the parser cannot read is a hard error
// rather than a statistic, a cross-check that cannot be run is a hard error
// rather than a skipped check, and the paths find could not open are carried
// back for the caller to report.
func List(ctx context.Context, c *adbproto.Conn, root string, excludes []string) (*Listing, error) {
	if err := checkRoot(ctx, c, root); err != nil {
		return nil, err
	}
	if !SupportsPrintf(ctx, c, root) {
		return nil, fmt.Errorf("device's find does not support -printf, which this tool requires.\n" +
			"The obvious alternative, `find ... -exec stat -c ... {} +`, is not usable: toybox\n" +
			"aborts the batch with \"Argument list too long\" and returns a partial listing with\n" +
			"a zero exit status, which would silently produce an incomplete backup")
	}

	cmd := "find " + adbproto.ShellQuote(root) + ` -type f -printf '%s|%T@|%p\n' 2>/dev/null`
	res, err := c.ExecStatus(cmd)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", root, err)
	}

	l := &Listing{Root: root}
	prefix := strings.TrimSuffix(root, "/") + "/"
	seen := 0
	var unparsed []string
	for _, line := range strings.Split(string(res.Out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		seen++
		e, ok := parseLine(line, prefix)
		if !ok {
			if len(unparsed) < 5 {
				unparsed = append(unparsed, line)
			}
			continue
		}
		if isExcluded(e.Rel, excludes) {
			l.Skipped++
			continue
		}
		l.Entries = append(l.Entries, e)
	}

	// An unreadable line is a file this run would drop on the floor. Counting it
	// and carrying on, as this used to, is the silent-incomplete-backup failure
	// in miniature.
	if len(unparsed) > 0 {
		return nil, fmt.Errorf("could not parse %d line(s) of the device listing, so some files "+
			"would be left out of the backup with nothing to show for it.\n"+
			"A filename containing a newline is the usual cause. Examples:\n  %s",
			len(unparsed), strings.Join(unparsed, "\n  "))
	}

	// Cross-check: `find | wc -l` uses no -printf and no exec, so it cannot be
	// truncated the same way.
	count, err := fileCount(ctx, c, root)
	if err != nil {
		return nil, fmt.Errorf("could not cross-check the listing of %s: %w\n"+
			"This check is the only thing standing between a silently truncated listing and a "+
			"backup that looks complete, so a run without it is refused rather than warned about",
			root, err)
	}
	if count != seen {
		return nil, fmt.Errorf("listing is incomplete: find reported %d files but only %d lines "+
			"came back; refusing to treat this as a full picture of the device", count, seen)
	}

	// A non-zero exit from find means it could not read part of the tree. Naming
	// those paths costs one more traversal, which is why it is only paid on the
	// runs where something actually went wrong.
	if res.Code != 0 {
		l.Errors, l.ErrorsTruncated = findErrors(c, root)
	}
	return l, nil
}

// findErrors repeats the walk keeping only what find wrote to stderr, which is
// where it names the paths it could not open.
func findErrors(c *adbproto.Conn, root string) ([]string, bool) {
	out, err := c.Exec(fmt.Sprintf("find %s 2>&1 >/dev/null | head -%d",
		adbproto.ShellQuote(root), maxReportedErrors+1))
	if err != nil {
		return []string{fmt.Sprintf("(the device could not be asked what went wrong: %v)", err)}, false
	}
	var msgs []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			msgs = append(msgs, line)
		}
	}
	if len(msgs) > maxReportedErrors {
		return msgs[:maxReportedErrors], true
	}
	return msgs, false
}

func parseLine(line, prefix string) (Entry, bool) {
	// Format is size|mtime|path. Split on the first two separators only, since
	// a filename may itself contain '|'.
	i := strings.IndexByte(line, '|')
	if i < 0 {
		return Entry{}, false
	}
	j := strings.IndexByte(line[i+1:], '|')
	if j < 0 {
		return Entry{}, false
	}
	j += i + 1

	size, err := strconv.ParseInt(line[:i], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	mtime, ok := parseUnixNanos(line[i+1 : j])
	if !ok {
		return Entry{}, false
	}
	path := line[j+1:]
	rel, ok := strings.CutPrefix(path, prefix)
	if !ok || rel == "" {
		return Entry{}, false
	}
	return Entry{Rel: rel, Size: size, MTimeN: mtime}, true
}

// parseUnixNanos reads find's %T@, "seconds.nanoseconds".
func parseUnixNanos(s string) (int64, bool) {
	secStr, fracStr, hasFrac := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return 0, false
	}
	var nanos int64
	if hasFrac {
		// Pad or truncate the fraction to exactly 9 digits.
		if len(fracStr) > 9 {
			fracStr = fracStr[:9]
		}
		for len(fracStr) < 9 {
			fracStr += "0"
		}
		nanos, err = strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return 0, false
		}
	}
	return sec*1_000_000_000 + nanos, true
}

// checkRoot verifies the backup source before anything else, so that a bad
// path produces a clear message instead of surfacing later as a confusing
// capability failure.
func checkRoot(ctx context.Context, c *adbproto.Conn, root string) error {
	q := adbproto.ShellQuote(root)
	out, err := c.Exec("if [ -d " + q + " ]; then echo DIR; elif [ -e " + q + " ]; then echo NOTDIR; else echo MISSING; fi")
	if err != nil {
		return fmt.Errorf("checking %s on the device: %w", root, err)
	}
	switch strings.TrimSpace(string(out)) {
	case "DIR":
		return nil
	case "NOTDIR":
		return fmt.Errorf("%s is not a directory on the device", root)
	default:
		return fmt.Errorf("%s does not exist on the device", root)
	}
}

func fileCount(ctx context.Context, c *adbproto.Conn, root string) (int, error) {
	out, err := c.Exec("find " + adbproto.ShellQuote(root) + " -type f 2>/dev/null | wc -l")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("the device answered %q where a file count was expected", firstLine(string(out)))
	}
	return n, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

func isExcluded(rel string, excludes []string) bool {
	for _, ex := range excludes {
		ex = strings.Trim(ex, "/")
		if ex == "" {
			continue
		}
		if rel == ex || strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}
