package sync

import (
	"os"
	"path/filepath"
	"testing"

	"quickadbackup/internal/index"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestRemoveStalePartsKeepsRealDotPartFiles is the reason the sweep consults the
// device listing instead of matching on the suffix alone: a phone may perfectly
// well hold a file called something.part, and deleting the backup of it on
// every run would mean re-fetching it on every run.
func TestRemoveStaleParts(t *testing.T) {
	dest := t.TempDir()
	write(t, filepath.Join(dest, "DCIM", "a.jpg.part"))   // leftover, no device file
	write(t, filepath.Join(dest, "DCIM", "real.part"))    // an actual device file
	write(t, filepath.Join(dest, "DCIM", "b.jpg"))        // a finished copy
	write(t, filepath.Join(dest, ArchiveDir, "old.part")) // archived, hands off
	write(t, filepath.Join(dest, index.Dir, "x.part"))    // metadata, hands off

	wanted := map[string]bool{
		filepath.Join("DCIM", "real.part"): true,
		filepath.Join("DCIM", "b.jpg"):     true,
	}
	if n := removeStaleParts(dest, wanted); n != 1 {
		t.Errorf("removed %d files, want 1", n)
	}
	if exists(filepath.Join(dest, "DCIM", "a.jpg.part")) {
		t.Error("the leftover .part file should have been removed")
	}
	for _, keep := range []string{
		filepath.Join("DCIM", "real.part"),
		filepath.Join("DCIM", "b.jpg"),
		filepath.Join(ArchiveDir, "old.part"),
		filepath.Join(index.Dir, "x.part"),
	} {
		if !exists(filepath.Join(dest, keep)) {
			t.Errorf("%s should have been left alone", keep)
		}
	}
}

// TestPruneArchivedDirsLeavesStrangersAlone pins down the scope that matters:
// the destination is not necessarily a folder this tool created, so only the
// directories this run emptied may be removed.
func TestPruneArchivedDirs(t *testing.T) {
	dest := t.TempDir()
	// Emptied by archiving: Photos/2019/ and its parent are now bare.
	if err := os.MkdirAll(filepath.Join(dest, "Photos", "2019"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory the user happens to keep in the destination, unrelated to the
	// backup and equally empty.
	if err := os.MkdirAll(filepath.Join(dest, "my notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling that still holds a file must survive.
	write(t, filepath.Join(dest, "Photos", "2020", "keep.jpg"))

	pruneArchivedDirs(dest, []string{"Photos/2019/gone.jpg", "Photos/2020/also-gone.jpg"})

	if exists(filepath.Join(dest, "Photos", "2019")) {
		t.Error("an emptied directory should have been pruned")
	}
	if !exists(filepath.Join(dest, "Photos", "2020", "keep.jpg")) {
		t.Error("a directory that still has contents must survive")
	}
	if !exists(filepath.Join(dest, "my notes")) {
		t.Error("an empty directory unrelated to the backup must not be touched")
	}
	if !exists(dest) {
		t.Error("the destination itself must never be removed")
	}
}
