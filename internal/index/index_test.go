package index

import (
	"errors"
	"testing"
)

// TestCheckSourceRefusesAnotherDevice covers the case that makes this check
// worth having: a second phone backed up into the first one's folder finds none
// of the recorded files in its listing, so every one of them looks deleted and
// the whole previous backup gets archived.
func TestCheckSourceRefusesAnotherDevice(t *testing.T) {
	idx := New("SERIAL-A", "/storage/emulated/0")
	err := idx.CheckSource(`D:\Backup`, "SERIAL-B", "/storage/emulated/0")
	var mismatch *SourceMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want SourceMismatchError, got %v", err)
	}
	if mismatch.Field != "device" {
		t.Errorf("field = %q, want %q", mismatch.Field, "device")
	}
	if idx.Serial != "SERIAL-A" {
		t.Errorf("a refused run must not adopt the new serial, got %q", idx.Serial)
	}
}

func TestCheckSourceRefusesAnotherRoot(t *testing.T) {
	idx := New("SERIAL-A", "/storage/emulated/0")
	err := idx.CheckSource(`D:\Backup`, "SERIAL-A", "/storage/emulated/0/DCIM")
	var mismatch *SourceMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want SourceMismatchError, got %v", err)
	}
	if mismatch.Field != "device folder" {
		t.Errorf("field = %q, want %q", mismatch.Field, "device folder")
	}
}

func TestCheckSourceAdopts(t *testing.T) {
	cases := []struct {
		name                   string
		haveSerial, haveRoot   string
		wantSerial, wantRoot   string
		adoptSerial, adoptRoot string
	}{
		{"first run", "", "", "SERIAL-A", "/root", "SERIAL-A", "/root"},
		// An index written before the serial was recorded must keep working
		// rather than read as "a different phone".
		{"legacy index", "", "/root", "SERIAL-A", "/root", "SERIAL-A", "/root"},
		// A device that reports no serial cannot be checked against, but must
		// not wipe out the serial already on record either.
		{"device has no serial", "SERIAL-A", "/root", "", "/root", "SERIAL-A", "/root"},
		{"same source", "SERIAL-A", "/root", "SERIAL-A", "/root", "SERIAL-A", "/root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := New(tc.haveSerial, tc.haveRoot)
			if err := idx.CheckSource(`D:\Backup`, tc.wantSerial, tc.wantRoot); err != nil {
				t.Fatalf("CheckSource: %v", err)
			}
			if idx.Serial != tc.adoptSerial {
				t.Errorf("serial = %q, want %q", idx.Serial, tc.adoptSerial)
			}
			if idx.Root != tc.adoptRoot {
				t.Errorf("root = %q, want %q", idx.Root, tc.adoptRoot)
			}
		})
	}
}
