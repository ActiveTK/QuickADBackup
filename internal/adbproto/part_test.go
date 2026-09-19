package adbproto

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPartPathCannotCollideWithADeviceFile is the bug this naming exists for.
// A phone may hold both "clip" and "clip.part"; with the scratch file named
// local+".part", one worker's temporary file and another worker's finished name
// were the same path, and eight workers run at once.
func TestPartPathCannotCollideWithADeviceFile(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "clip")
	realPart := filepath.Join(dir, "clip"+PartSuffix)

	tmp := partPath(plain)
	if tmp == realPart {
		t.Fatalf("the scratch file for %q is %q, which is a name the device may itself have",
			plain, tmp)
	}
	if !strings.HasSuffix(tmp, PartSuffix) {
		t.Errorf("%q does not end in %q, so the stale-part sweep would not find it", tmp, PartSuffix)
	}
	if !strings.HasPrefix(tmp, plain+".") {
		t.Errorf("%q does not sit beside %q, so it would not land on the same volume", tmp, plain)
	}
}

// TestPartPathIsUniquePerCall keeps two concurrent transfers of the same file
// from sharing a scratch file.
func TestPartPathIsUniquePerCall(t *testing.T) {
	local := filepath.Join(t.TempDir(), "clip")
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		p := partPath(local)
		if seen[p] {
			t.Fatalf("partPath returned %q twice", p)
		}
		seen[p] = true
	}
}

// TestCheckBlobLen guards the lengths the device supplies and the host then
// trusts. A stream that has lost its framing hands over four stray bytes as a
// length; without a ceiling that is a demand for gigabytes, or a data chunk
// long enough to pour the rest of the conversation into the file being saved.
func TestCheckBlobLen(t *testing.T) {
	tests := []struct {
		what    string
		n, max  uint32
		wantErr bool
	}{
		{"failure message", 0, maxSyncBlob, false},
		{"failure message", maxSyncBlob, maxSyncBlob, false},
		{"failure message", maxSyncBlob + 1, maxSyncBlob, true},
		{"failure message", 0xFFFFFFFF, maxSyncBlob, true},
		{"filename", 255, maxSyncName, false},
		{"filename", maxSyncName + 1, maxSyncName, true},
		{"data chunk for /x", 64 << 10, maxSyncBlob, false}, // adbd's real chunk size
		{"data chunk for /x", 0xFFFFFFFF, maxSyncBlob, true},
	}
	for _, tc := range tests {
		err := checkBlobLen(tc.what, tc.n, tc.max)
		if (err != nil) != tc.wantErr {
			t.Errorf("checkBlobLen(%q, %d, %d) = %v, wantErr %v", tc.what, tc.n, tc.max, err, tc.wantErr)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), tc.what) {
			t.Errorf("error %q does not say which field was wrong", err)
		}
	}
}

// TestSyncBlobCeilingsLeaveRoom: the ceilings must be comfortably above what
// the protocol really uses, or a legitimate transfer starts failing.
func TestSyncBlobCeilingsLeaveRoom(t *testing.T) {
	const adbdChunk = 64 << 10 // SYNC_DATA_MAX in AOSP
	if maxSyncBlob < 4*adbdChunk {
		t.Errorf("maxSyncBlob = %d, too close to adbd's %d-byte chunks", maxSyncBlob, adbdChunk)
	}
	const linuxNameMax = 255
	if maxSyncName < 4*linuxNameMax {
		t.Errorf("maxSyncName = %d, too close to the %d-byte limit Linux enforces",
			maxSyncName, linuxNameMax)
	}
}
