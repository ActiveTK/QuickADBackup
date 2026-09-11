// Package verify checks a backup against the device it came from, by comparing
// SHA-1 digests computed independently on each side.
//
// Read-only on both sides.
package verify

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quickadbackup/internal/adbproto"
	"quickadbackup/internal/device"
	"quickadbackup/internal/localpath"
	"quickadbackup/internal/progress"
)

// batchBudget is how many characters of quoted paths go into one sha1sum
// command.
//
// Only the device's own argument limit applies here. The 32767-character
// Windows command line that constrained the adb-based design is gone, because
// the command travels as a protocol payload rather than a process argument.
const batchBudget = 60000

// sha1HexLen is the width of the digest sha1sum prints before the path.
const sha1HexLen = 40

type Options struct {
	Dest     string
	Root     string
	Excludes []string
	Sample   int // 0 means every file
	Progress progress.Func
}

type Result struct {
	Checked    int
	Matched    int
	Mismatch   []string // content differs
	Missing    []string // on the phone, absent from the backup
	Unreadable []string // the device could not hash it
	Elapsed    time.Duration
}

// Run compares the backup at Dest against the device.
func Run(ctx context.Context, c *adbproto.Conn, opts Options) (*Result, error) {
	start := time.Now()
	res := &Result{}
	defer func() { res.Elapsed = time.Since(start) }()

	// Cancelling has to work even if the phone has stopped answering, which it
	// otherwise would not: see device.AbortOnStall.
	defer device.AbortOnStall(ctx, c, device.AbortGrace)()

	dest, err := filepath.Abs(opts.Dest)
	if err != nil {
		return nil, err
	}

	// Established before any file is checked, because without sha1sum every
	// single batch would come back empty and every file would be filed as
	// "the device could not hash it" - a verify that tested nothing, reported
	// as if it had tested everything.
	if err := requireSha1sum(c); err != nil {
		return nil, err
	}

	opts.Progress.Log(progress.PhaseScanning, "scanning %s ...", opts.Root)
	listing, err := device.List(ctx, c, opts.Root, opts.Excludes)
	if err != nil {
		return nil, err
	}
	if n := len(listing.Errors); n > 0 {
		more := ""
		if listing.ErrorsTruncated {
			more = " or more"
		}
		opts.Progress.Log(progress.PhaseScanning,
			"warning: %d%s path(s) on the device could not be read, so they are outside "+
				"what this verify can say anything about", n, more)
	}

	entries := listing.Entries
	if opts.Sample > 0 && opts.Sample < len(entries) {
		// Sample without replacement, so a run cannot check the same file
		// twice and call the backup better verified than it is.
		picked := make([]device.Entry, len(entries))
		copy(picked, entries)
		rand.Shuffle(len(picked), func(i, j int) { picked[i], picked[j] = picked[j], picked[i] })
		entries = picked[:opts.Sample]
	}
	opts.Progress.Log(progress.PhaseVerifying, "verifying %d of %d files", len(entries), len(listing.Entries))

	// Hash the local copies first; anything absent needs no device round trip.
	//
	// This reads the whole backup off local disk, which on a large one runs for
	// long enough that it must both report progress and honour cancellation.
	opts.Progress.Log(progress.PhaseHashingLocal, "PC側のハッシュを計算しています...")
	local := make(map[string]string, len(entries))
	var toHash []device.Entry
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		sum, err := hashFile(filepath.Join(dest, localpath.Encode(e.Rel)))
		if err != nil {
			res.Missing = append(res.Missing, e.Rel)
		} else {
			local[e.Rel] = sum
			toHash = append(toHash, e)
		}
		if i%64 == 0 || i == len(entries)-1 {
			opts.Progress.Report(progress.Update{
				Phase: progress.PhaseHashingLocal,
				Done:  i + 1,
				Total: len(entries),
			})
		}
	}

	for i := 0; i < len(toHash); {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var batch []device.Entry
		length := len("sha1sum")
		for ; i < len(toHash); i++ {
			q := len(adbproto.ShellQuote(opts.Root+"/"+toHash[i].Rel)) + 1
			if length+q > batchBudget && len(batch) > 0 {
				break
			}
			batch = append(batch, toHash[i])
			length += q
		}
		if err := verifyBatch(c, opts, batch, local, res); err != nil {
			return res, err
		}
		res.Checked += len(batch)
		opts.Progress.Report(progress.Update{
			Phase: progress.PhaseVerifying,
			Done:  res.Checked,
			Total: len(toHash),
		})
	}

	return res, nil
}

// requireSha1sum confirms the device has the hashing tool this depends on.
func requireSha1sum(c *adbproto.Conn) error {
	res, err := c.ExecStatus("command -v sha1sum >/dev/null 2>&1 || which sha1sum >/dev/null 2>&1")
	if err != nil {
		return fmt.Errorf("checking for sha1sum on the device: %w", err)
	}
	if res.Code != 0 {
		return fmt.Errorf("the device has no sha1sum, so the backup cannot be verified against it.\n" +
			"Reporting every file as unverifiable would look far too much like a clean result, " +
			"so the run stops here instead")
	}
	return nil
}

// verifyBatch hashes one batch of paths on the device and files each result.
//
// A batch that comes back with no digests at all is not accepted as "none of
// these files could be read": one bad path would then condemn every file
// sharing its batch. The batch is halved and retried until each failure is
// pinned to the single file that caused it.
func verifyBatch(c *adbproto.Conn, opts Options, batch []device.Entry, local map[string]string, res *Result) error {
	if len(batch) == 0 {
		return nil
	}
	deviceSums, err := deviceHashes(c, opts.Root, batch)
	if err != nil {
		return err
	}
	if len(deviceSums) == 0 && len(batch) > 1 {
		half := len(batch) / 2
		if err := verifyBatch(c, opts, batch[:half], local, res); err != nil {
			return err
		}
		return verifyBatch(c, opts, batch[half:], local, res)
	}

	for _, e := range batch {
		got, ok := deviceSums[e.Rel]
		switch {
		case !ok:
			res.Unreadable = append(res.Unreadable, e.Rel)
		case got == local[e.Rel]:
			res.Matched++
		default:
			res.Mismatch = append(res.Mismatch, e.Rel)
		}
	}
	return nil
}

// deviceHashes runs sha1sum over one batch and returns the digests by path
// relative to root.
func deviceHashes(c *adbproto.Conn, root string, batch []device.Entry) (map[string]string, error) {
	var sb strings.Builder
	sb.WriteString("sha1sum")
	for _, e := range batch {
		sb.WriteByte(' ')
		sb.WriteString(adbproto.ShellQuote(root + "/" + e.Rel))
	}
	// A non-zero exit is expected and normal here: sha1sum reports failure if it
	// could not read any one of its arguments, and the per-file verdict comes
	// from which digests came back, not from the exit status.
	out, err := c.Exec(sb.String() + " 2>/dev/null")
	if err != nil {
		return nil, fmt.Errorf("hashing on the device: %w", err)
	}

	sums := make(map[string]string, len(batch))
	prefix := root + "/"
	for _, line := range strings.Split(string(out), "\n") {
		sum, path, ok := parseSha1Line(strings.TrimRight(line, "\r"))
		if !ok {
			continue
		}
		if rel, ok := strings.CutPrefix(path, prefix); ok {
			sums[rel] = sum
		}
	}
	return sums, nil
}

// parseSha1Line splits one "<digest>  <path>" line.
//
// The path is taken verbatim after the fixed-width separator rather than
// trimmed, because a filename may legitimately begin or end with a space and
// trimming it produced a path that matched nothing, filing a perfectly readable
// file as unreadable.
func parseSha1Line(line string) (sum, path string, ok bool) {
	if len(line) < sha1HexLen+2 {
		return "", "", false
	}
	sum = line[:sha1HexLen]
	if _, err := hex.DecodeString(sum); err != nil {
		return "", "", false
	}
	rest := line[sha1HexLen:]
	// GNU and toybox both emit two separator characters: a space, then a space
	// for text mode or '*' for binary mode.
	if rest[0] != ' ' {
		return "", "", false
	}
	if rest[1] == ' ' || rest[1] == '*' {
		return sum, rest[2:], true
	}
	return sum, rest[1:], true
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
