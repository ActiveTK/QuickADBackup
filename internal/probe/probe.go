// Package probe inspects a connected device and reports whether this tool's
// requirements are met on it.
//
// Every probe here is strictly read-only.
package probe

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"quickadbackup/internal/adbproto"
	"quickadbackup/internal/device"
)

type Result struct {
	Name   string
	OK     bool
	Detail string
}

type Report struct{ Results []Result }

func (r *Report) add(name string, ok bool, format string, args ...any) {
	r.Results = append(r.Results, Result{name, ok, fmt.Sprintf(format, args...)})
}

// Run checks the device for everything the sync engine depends on.
func Run(ctx context.Context, c *adbproto.Conn, root string) (*Report, error) {
	rep := &Report{}

	rep.add("connection", true, "%s", summariseBanner(c.Banner))
	rep.add("max payload", c.MaxData > 0, "%d bytes per message", c.MaxData)

	// The exec service must not rewrite line endings; shell would, because it
	// allocates a pseudo-terminal.
	out, err := c.Exec("printf 'a\\nb\\n'")
	rep.add("binary-clean exec", err == nil && string(out) == "a\nb\n",
		"newlines survive the round trip")

	// Without -printf there is no safe one-shot listing: the `-exec stat +`
	// alternative truncates at ARG_MAX and reports success anyway.
	hasPrintf := device.SupportsPrintf(ctx, c, root)
	rep.add("find -printf", hasPrintf, "required for incremental scanning")

	// /sdcard is a symlink, and find will not descend into a symlinked start
	// point, so confirm the caller is using a path that actually works.
	dirs, _ := c.Exec("find " + adbproto.ShellQuote(root) + " -maxdepth 1 -type d 2>/dev/null | wc -l")
	n := strings.TrimSpace(string(dirs))
	rep.add("find descends root", n != "" && n != "0" && n != "1",
		"%s subdirectories under %s", n, root)

	sc, err := c.Sync()
	if err != nil {
		rep.add("sync service", false, "%s", firstLine(err.Error()))
		return rep, nil
	}
	defer sc.Close()
	fi, err := sc.Stat(root)
	rep.add("sync service", err == nil && fi.IsDir(), "STAT_V2 reports mode %o", fi.Mode)

	for _, d := range device.DefaultExcludes {
		out, err := c.Exec("ls -1 " + adbproto.ShellQuote(root+"/"+d) + " 2>&1 | head -2")
		readable := err == nil && !strings.Contains(string(out), "Permission denied") &&
			strings.TrimSpace(string(out)) != ""
		rep.add("readable: "+d, readable, "excluded by default either way")
	}

	start := time.Now()
	listing, listErr := device.List(ctx, c, root, device.DefaultExcludes)
	if listErr != nil {
		rep.add("full listing", false, "%s", firstLine(listErr.Error()))
		return rep, nil
	}
	rep.add("full listing", true, "%d files, %s, in %s",
		len(listing.Entries), humanBytes(listing.TotalBytes()), time.Since(start).Round(time.Millisecond))

	// A path find cannot open vanishes from the listing and from the count it is
	// checked against, so the run adds up perfectly while the files underneath
	// are missing. Probe is where that should be visible before a backup is run.
	detail := "every path under the root is readable"
	if n := len(listing.Errors); n > 0 {
		detail = fmt.Sprintf("%d path(s) unreadable, e.g. %s", n, firstLine(listing.Errors[0]))
		if listing.ErrorsTruncated {
			detail = fmt.Sprintf("%d+ path(s) unreadable, e.g. %s", n, firstLine(listing.Errors[0]))
		}
	}
	rep.add("readable throughout", len(listing.Errors) == 0, "%s", detail)
	return rep, nil
}

// summariseBanner trims the device's feature list down to its identity.
func summariseBanner(b string) string {
	b = strings.TrimPrefix(b, "device::")
	var model, name string
	for _, f := range strings.Split(b, ";") {
		if v, ok := strings.CutPrefix(f, "ro.product.model="); ok {
			model = v
		}
		if v, ok := strings.CutPrefix(f, "ro.product.device="); ok {
			name = v
		}
	}
	if model == "" {
		return firstLine(b)
	}
	return fmt.Sprintf("%s (%s), no adb server involved", model, name)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 110 {
		s = s[:110] + "..."
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// Print renders the report as a readable table.
func (r *Report) Print(w io.Writer) {
	width := 0
	for _, res := range r.Results {
		if len(res.Name) > width {
			width = len(res.Name)
		}
	}
	for _, res := range r.Results {
		mark := "NG"
		if res.OK {
			mark = "OK"
		}
		fmt.Fprintf(w, "  [%s] %-*s  %s\n", mark, width, res.Name, res.Detail)
	}
}
