// Package index records what has already been copied to the PC, so that the
// next run can transfer only what changed.
package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Dir is the metadata folder created inside the backup destination.
const Dir = ".quickadbackup"

const fileName = "index.json"

// Entry is the state of one file as of the last successful transfer.
type Entry struct {
	Size   int64 `json:"size"`
	MTimeN int64 `json:"mtime"`
}

type Index struct {
	Serial  string           `json:"serial"`
	Root    string           `json:"root"`
	Updated time.Time        `json:"updated"`
	Files   map[string]Entry `json:"files"`

	path string
}

func New(serial, root string) *Index {
	return &Index{Serial: serial, Root: root, Files: map[string]Entry{}}
}

// Load reads the index for a destination, returning an empty one if this is
// the first run.
func Load(dest string) (*Index, error) {
	p := filepath.Join(dest, Dir, fileName)
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		idx := New("", "")
		idx.path = p
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("index at %s is corrupt: %w", p, err)
	}
	if idx.Files == nil {
		idx.Files = map[string]Entry{}
	}
	idx.path = p
	return &idx, nil
}

// SourceMismatchError reports that this destination already holds a backup of
// something other than what the caller just asked to back up.
//
// It matters because the index is also the list of what the device is supposed
// to still have: a run against a different phone, or against a narrower device
// root, finds none of the recorded files in the new listing and concludes they
// were all deleted from the phone, moving the entire previous backup into the
// archive folder. Refusing is the only safe answer.
type SourceMismatchError struct {
	Field      string // "device" or "device folder"
	Have, Want string
	Dest       string
}

func (e *SourceMismatchError) Error() string {
	return fmt.Sprintf("%s already holds a backup of a different %s\n"+
		"  recorded: %s\n"+
		"  this run: %s\n\n"+
		"Backing up into it would treat every recorded file as deleted from the phone\n"+
		"and move the whole previous backup into %s. Use a separate destination for\n"+
		"each device and each device folder. If you really mean to start this one over,\n"+
		"delete %s first; the next run will then copy everything again.",
		e.Dest, e.Field, e.Have, e.Want, Dir, filepath.Join(e.Dest, Dir, fileName))
}

// CheckSource refuses a run whose device or device root differs from the one
// this index was built from, and otherwise adopts the values.
//
// An index written before these fields were recorded carries an empty serial;
// that is adopted silently rather than treated as a mismatch, so an existing
// backup keeps working.
func (i *Index) CheckSource(dest, serial, root string) error {
	if i.Serial != "" && serial != "" && i.Serial != serial {
		return &SourceMismatchError{Field: "device", Have: i.Serial, Want: serial, Dest: dest}
	}
	if i.Root != "" && i.Root != root {
		return &SourceMismatchError{Field: "device folder", Have: i.Root, Want: root, Dest: dest}
	}
	if serial != "" {
		i.Serial = serial
	}
	i.Root = root
	return nil
}

// Save writes the index atomically, so an interrupted run cannot leave a
// half-written index that would confuse the next one.
func (i *Index) Save(dest string) error {
	if i.path == "" {
		i.path = filepath.Join(dest, Dir, fileName)
	}
	if err := os.MkdirAll(filepath.Dir(i.path), 0o755); err != nil {
		return err
	}
	i.Updated = time.Now()
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	tmp := i.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, i.path)
}

// Unchanged reports whether a file with this size and mtime is already stored.
func (i *Index) Unchanged(rel string, size, mtimeN int64) bool {
	e, ok := i.Files[rel]
	return ok && e.Size == size && e.MTimeN == mtimeN
}

func (i *Index) Put(rel string, size, mtimeN int64) {
	i.Files[rel] = Entry{Size: size, MTimeN: mtimeN}
}

func (i *Index) Delete(rel string) { delete(i.Files, rel) }
