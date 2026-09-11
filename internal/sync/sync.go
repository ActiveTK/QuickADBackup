// Package sync performs the incremental copy from device to PC.
//
// The device is strictly read-only. The only deletions this package performs
// are on the PC, and even then files are moved into a dated archive folder
// rather than removed.
package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"quickadbackup/internal/adbproto"
	"quickadbackup/internal/device"
	"quickadbackup/internal/index"
	"quickadbackup/internal/localpath"
	"quickadbackup/internal/progress"
)

// ArchiveDir holds files that disappeared from the phone.
const ArchiveDir = "_archive"

// partSuffix marks a transfer still in progress. A file only gets its real name
// once it is complete, so a partial file can never be mistaken for a finished
// one.
const partSuffix = ".part"

// DefaultWorkers is how many sync streams run at once.
//
// Every file costs a round trip to open on the device, and the protocol
// multiplexes independent streams over the one USB connection, so running
// several hides that latency. Measured on a Pixel 7, throughput rose from 117
// files/s on one stream to 817 files/s on eight; beyond that the USB link
// rather than the latency is the limit.
const DefaultWorkers = 8

type Options struct {
	Dest     string
	Root     string
	Excludes []string
	Workers  int
	DryRun   bool
	Progress progress.Func
}

type Stats struct {
	Scanned     int
	Unchanged   int
	Copied      int
	CopiedBytes int64
	Archived    int
	Skipped     []string // collisions, unreadable device paths, other refusals
	Failed      []string // wanted but not retrieved
	Elapsed     time.Duration
}

// result is one worker's outcome for a single file.
type result struct {
	entry device.Entry
	size  int64 // bytes actually transferred, which need not be entry.Size
	err   error
}

// Run executes one incremental synchronisation.
func Run(ctx context.Context, c *adbproto.Conn, opts Options) (*Stats, error) {
	start := time.Now()
	st := &Stats{}
	// Set on every path, including cancellation, so a stopped run still
	// reports how long it ran.
	defer func() { st.Elapsed = time.Since(start) }()

	// Cancelling has to work even if the phone has stopped answering, which it
	// otherwise would not: see device.AbortOnStall.
	defer device.AbortOnStall(ctx, c, device.AbortGrace)()

	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	dest, err := filepath.Abs(opts.Dest)
	if err != nil {
		return nil, err
	}
	opts.Dest = dest

	opts.Progress.Log(progress.PhaseScanning, "scanning %s ...", opts.Root)
	listing, err := device.List(ctx, c, opts.Root, opts.Excludes)
	if err != nil {
		return nil, err
	}
	st.Scanned = len(listing.Entries)
	opts.Progress.Log(progress.PhaseScanning, "found %d files, %s (skipped %d excluded)",
		len(listing.Entries), humanBytes(listing.TotalBytes()), listing.Skipped)
	reportDeviceErrors(listing, opts.Progress, st)

	idx, err := index.Load(dest)
	if err != nil {
		return nil, err
	}
	// Refuse to mix two sources in one destination. Without this the previous
	// backup's files are all absent from the new listing, so every one of them
	// looks deleted from the phone and the whole thing is moved into _archive.
	if err := idx.CheckSource(dest, c.Serial, opts.Root); err != nil {
		return nil, err
	}

	// Refuse to overwrite one device file with another that only differs by
	// case, which NTFS cannot tell apart.
	rels := make([]string, len(listing.Entries))
	for i, e := range listing.Entries {
		rels[i] = e.Rel
	}
	blocked := map[string]bool{}
	for _, col := range localpath.FindCollisions(rels) {
		for _, r := range col.Rels {
			blocked[r] = true
		}
		st.Skipped = append(st.Skipped,
			fmt.Sprintf("case collision on %q: %s", col.Local, strings.Join(col.Rels, ", ")))
	}

	var todo []device.Entry
	present := make(map[string]bool, len(listing.Entries))
	wanted := make(map[string]bool, len(listing.Entries))
	for _, e := range listing.Entries {
		present[e.Rel] = true
		wanted[localpath.Encode(e.Rel)] = true
		if blocked[e.Rel] {
			continue
		}
		if idx.Unchanged(e.Rel, e.Size, e.MTimeN) && localFileOK(dest, e) {
			st.Unchanged++
			continue
		}
		todo = append(todo, e)
	}

	var wantBytes int64
	for _, e := range todo {
		wantBytes += e.Size
	}
	opts.Progress.Log(progress.PhaseCopying, "to copy: %d files, %s (%d unchanged)",
		len(todo), humanBytes(wantBytes), st.Unchanged)

	if opts.DryRun {
		st.Copied = len(todo)
		st.CopiedBytes = wantBytes
		for rel := range idx.Files {
			if !present[rel] {
				st.Archived++
			}
		}
		return st, nil
	}

	// Leftovers from a run that died mid-transfer. Only this process can be
	// holding the device, so nothing here is in use by anyone.
	//
	// Gated on there being work to do, because the sweep walks the whole
	// destination and a run with nothing to copy is the one that has to stay
	// fast. Nothing is lost by the gate: a run that died mid-transfer never
	// recorded those files, so they are in todo on the next run by definition.
	if len(todo) > 0 {
		if n := removeStaleParts(dest, wanted); n > 0 {
			opts.Progress.Log(progress.PhaseCopying,
				"removed %d unfinished file(s) left by an earlier run", n)
		}
	}

	if err := copyFiles(ctx, c, opts, idx, todo, st); err != nil {
		idx.Save(dest)
		return st, err
	}

	// Files gone from the phone are archived here, never deleted from there.
	archErr := archiveMissing(opts, idx, present, st)
	// Save regardless: the copying above is real work, and losing the record of
	// it because the archive step tripped would make the next run fetch it all
	// again.
	if err := idx.Save(dest); err != nil && archErr == nil {
		return st, err
	}
	return st, archErr
}

// reportDeviceErrors surfaces the paths find could not open.
//
// They belong in the summary rather than in a debug log: a directory the shell
// user cannot read simply does not appear in the listing, so the files under it
// are missing from the backup and every count in the summary still adds up.
func reportDeviceErrors(l *device.Listing, p progress.Func, st *Stats) {
	if len(l.Errors) == 0 {
		return
	}
	more := ""
	if l.ErrorsTruncated {
		more = " or more"
	}
	p.Log(progress.PhaseScanning,
		"warning: %d%s path(s) on the device could not be read and are NOT in this backup",
		len(l.Errors), more)
	for _, e := range l.Errors {
		st.Skipped = append(st.Skipped, "unreadable on the device: "+e)
	}
	if l.ErrorsTruncated {
		st.Skipped = append(st.Skipped, "unreadable on the device: ... and more not listed")
	}
}

// localFileOK guards against an index that claims a file we no longer have.
func localFileOK(dest string, e device.Entry) bool {
	fi, err := os.Stat(filepath.Join(dest, localpath.Encode(e.Rel)))
	return err == nil && fi.Size() == e.Size
}

// removeStaleParts deletes unfinished transfers from a previous run.
//
// A name is only removed when the device has no file of exactly that name, so a
// real device file that genuinely ends in ".part" is left alone rather than
// deleted out of the backup on every run.
func removeStaleParts(dest string, wanted map[string]bool) int {
	removed := 0
	filepath.WalkDir(dest, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable corner of the destination is not worth failing the
			// backup over; the copy itself will report anything that matters.
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ArchiveDir, index.Dir:
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), partSuffix) {
			return nil
		}
		rel, relErr := filepath.Rel(dest, path)
		if relErr != nil || wanted[rel] {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	return removed
}

// copyFiles pulls every wanted file over a pool of sync streams.
//
// Each file is fetched with the sync service's RECV, which reports an explicit
// failure for a file it cannot read. That is the reason this replaced a batched
// tar: tar skipped unreadable files silently, so a missing file looked exactly
// like a successful backup.
func copyFiles(ctx context.Context, c *adbproto.Conn, opts Options, idx *index.Index, todo []device.Entry, st *Stats) error {
	if len(todo) == 0 {
		return nil
	}
	// Neighbouring paths tend to sit near each other on the device's storage.
	sort.Slice(todo, func(i, j int) bool { return todo[i].Rel < todo[j].Rel })

	workers := opts.Workers
	if workers > len(todo) {
		workers = len(todo)
	}

	jobs := make(chan device.Entry)
	results := make(chan result, workers*2)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		sc, err := c.Sync()
		if err != nil {
			close(jobs)
			wg.Wait()
			return fmt.Errorf("opening sync stream %d: %w", i, err)
		}
		wg.Add(1)
		go func(sc *adbproto.SyncConn) {
			defer wg.Done()
			defer sc.Close()
			for e := range jobs {
				local := filepath.Join(opts.Dest, localpath.Encode(e.Rel))
				if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
					results <- result{entry: e, err: err}
					continue
				}
				n, err := sc.RecvFile(opts.Root+"/"+e.Rel, local, time.Unix(0, e.MTimeN))
				results <- result{entry: e, size: n, err: err}
			}
		}(sc)
	}

	go func() {
		defer close(jobs)
		for _, e := range todo {
			select {
			case jobs <- e:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	done, lastSave := 0, time.Now()
	for r := range results {
		done++
		if r.err != nil {
			// Files genuinely vanish mid-run: browser temp files, app caches.
			// Record them rather than aborting the whole backup.
			st.Failed = append(st.Failed, r.entry.Rel)
		} else {
			// Record what actually arrived, not what the listing promised. The
			// two differ whenever a file was still being written on the phone,
			// and recording the promise would mark a short file as complete.
			idx.Put(r.entry.Rel, r.size, r.entry.MTimeN)
			st.Copied++
			st.CopiedBytes += r.size
		}
		opts.Progress.Report(progress.Update{
			Phase:   progress.PhaseCopying,
			Done:    done,
			Total:   len(todo),
			Bytes:   st.CopiedBytes,
			Current: r.entry.Rel,
		})
		// Persist periodically so an interrupted run resumes where it stopped.
		if time.Since(lastSave) > 5*time.Second {
			idx.Save(opts.Dest)
			lastSave = time.Now()
		}
	}
	return ctx.Err()
}

// archiveMissing moves PC copies of files that are gone from the phone into a
// dated folder. Nothing is deleted, here or on the device.
func archiveMissing(opts Options, idx *index.Index, present map[string]bool, st *Stats) error {
	var gone []string
	for rel := range idx.Files {
		if !present[rel] {
			gone = append(gone, rel)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	sort.Strings(gone)
	stamp := time.Now().Format("2006-01-02")
	var moved []string
	for _, rel := range gone {
		src := filepath.Join(opts.Dest, localpath.Encode(rel))
		if _, err := os.Stat(src); err != nil {
			// Never copied, or already moved: just forget it.
			idx.Delete(rel)
			continue
		}
		dst := filepath.Join(opts.Dest, ArchiveDir, stamp, localpath.Encode(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			// A same-named file archived earlier today is not a reason to stop.
			// Forget it either way: leaving it in the index made every later run
			// retry the same impossible rename forever.
			if !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("archiving %s: %w", rel, err)
			}
			idx.Delete(rel)
			st.Skipped = append(st.Skipped,
				fmt.Sprintf("already archived today, left in place: %s", rel))
			continue
		}
		idx.Delete(rel)
		moved = append(moved, rel)
		st.Archived++
	}
	opts.Progress.Log(progress.PhaseArchiving,
		"archived %d files removed from the phone into %s/%s", st.Archived, ArchiveDir, stamp)
	pruneArchivedDirs(opts.Dest, moved)
	return nil
}

// pruneArchivedDirs removes directories left empty by the archiving above.
//
// It walks up from the files that were actually moved rather than scanning the
// whole destination, because the destination is not necessarily a folder this
// tool created: point it at an existing one and a blanket sweep would delete
// empty folders that have nothing to do with the backup. Only directories this
// run emptied are candidates, and only if they are still empty.
//
// A directory that is empty on the phone never gets created here in the first
// place, since the listing contains files only, so nothing of value is lost.
func pruneArchivedDirs(dest string, moved []string) {
	candidates := map[string]bool{}
	for _, rel := range moved {
		d := filepath.Dir(filepath.Join(dest, localpath.Encode(rel)))
		for len(d) > len(dest) && strings.HasPrefix(d, dest+string(filepath.Separator)) {
			candidates[d] = true
			d = filepath.Dir(d)
		}
	}
	dirs := make([]string, 0, len(candidates))
	for d := range candidates {
		dirs = append(dirs, d)
	}
	// Deepest first, so a directory holding only empty directories also goes. A
	// child's path is always longer than its parent's, so length orders them.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if entries, err := os.ReadDir(d); err == nil && len(entries) == 0 {
			os.Remove(d)
		}
	}
}

func humanBytes(n int64) string { return progress.HumanBytes(n) }
