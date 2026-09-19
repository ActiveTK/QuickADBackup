package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"quickadbackup/internal/index"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func writeBody(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveTwiceInOneDayKeepsBoth is the whole point of freeName. The archive
// folder is named for the day, so a file deleted, restored and deleted again
// within one day is archived to the same path twice - and os.Rename on Windows
// replaces its destination without a word. The first copy used to be destroyed
// by the second, in the one place this tool promises nothing is ever lost.
func TestArchiveTwiceInOneDayKeepsBoth(t *testing.T) {
	dest := t.TempDir()
	stamp := time.Now().Format("2006-01-02")
	opts := Options{Dest: dest}

	archiveOnce := func(body string) {
		writeBody(t, filepath.Join(dest, "DCIM", "photo.jpg"), body)
		idx := index.New("serial", "/storage/emulated/0")
		idx.Put("DCIM/photo.jpg", int64(len(body)), 1)
		st := &Stats{}
		if err := archiveMissing(opts, idx, map[string]bool{}, st); err != nil {
			t.Fatalf("archiveMissing: %v", err)
		}
		if st.Archived != 1 {
			t.Fatalf("archived %d files, want 1", st.Archived)
		}
		if _, ok := idx.Files["DCIM/photo.jpg"]; ok {
			t.Error("archived file is still in the index")
		}
	}

	archiveOnce("first version")
	archiveOnce("second version")

	dir := filepath.Join(dest, ArchiveDir, stamp, "DCIM")
	if got := read(t, filepath.Join(dir, "photo.jpg")); got != "first version" {
		t.Errorf("photo.jpg = %q, want the first version to be untouched", got)
	}
	if got := read(t, filepath.Join(dir, "photo (2).jpg")); got != "second version" {
		t.Errorf("photo (2).jpg = %q, want the second version", got)
	}
}

// TestArchiveMovesRatherThanDeletes covers the ordinary path: the PC copy ends
// up under _archive and nothing is removed.
func TestArchiveMovesRatherThanDeletes(t *testing.T) {
	dest := t.TempDir()
	writeBody(t, filepath.Join(dest, "Download", "gone.txt"), "body")
	idx := index.New("serial", "/storage/emulated/0")
	idx.Put("Download/gone.txt", 4, 1)
	idx.Put("Download/stays.txt", 4, 1)
	writeBody(t, filepath.Join(dest, "Download", "stays.txt"), "body")

	st := &Stats{}
	present := map[string]bool{"Download/stays.txt": true}
	if err := archiveMissing(Options{Dest: dest}, idx, present, st); err != nil {
		t.Fatalf("archiveMissing: %v", err)
	}
	if st.Archived != 1 {
		t.Fatalf("archived %d, want 1", st.Archived)
	}
	if _, err := os.Stat(filepath.Join(dest, "Download", "gone.txt")); err == nil {
		t.Error("the original is still in place; it should have moved")
	}
	if got := read(t, filepath.Join(dest, ArchiveDir,
		time.Now().Format("2006-01-02"), "Download", "gone.txt")); got != "body" {
		t.Errorf("archived copy = %q", got)
	}
	if got := read(t, filepath.Join(dest, "Download", "stays.txt")); got != "body" {
		t.Errorf("a file still on the phone was disturbed: %q", got)
	}
}

func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "clip.mp4")

	got, err := freeName(base)
	if err != nil || got != base {
		t.Fatalf("freeName on a free name = %q, %v; want the name unchanged", got, err)
	}

	writeBody(t, base, "one")
	got, err = freeName(base)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "clip (2).mp4"); got != want {
		t.Errorf("freeName = %q, want %q", got, want)
	}

	writeBody(t, filepath.Join(dir, "clip (2).mp4"), "two")
	got, _ = freeName(base)
	if want := filepath.Join(dir, "clip (3).mp4"); got != want {
		t.Errorf("freeName = %q, want %q", got, want)
	}

	// A name with no extension keeps the suffix at the end.
	noExt := filepath.Join(dir, "README")
	writeBody(t, noExt, "x")
	got, _ = freeName(noExt)
	if want := filepath.Join(dir, "README (2)"); got != want {
		t.Errorf("freeName without an extension = %q, want %q", got, want)
	}
}

func TestArchiveNothingToDo(t *testing.T) {
	dest := t.TempDir()
	idx := index.New("serial", "/root")
	idx.Put("a.txt", 1, 1)
	st := &Stats{}
	if err := archiveMissing(Options{Dest: dest}, idx, map[string]bool{"a.txt": true}, st); err != nil {
		t.Fatal(err)
	}
	if st.Archived != 0 {
		t.Errorf("archived %d, want 0", st.Archived)
	}
	if len(st.Skipped) != 0 {
		t.Errorf("skipped %v, want none", st.Skipped)
	}
	if _, err := os.Stat(filepath.Join(dest, ArchiveDir)); err == nil {
		t.Error("an archive folder was created with nothing to put in it")
	}
}
