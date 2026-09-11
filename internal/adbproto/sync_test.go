package adbproto

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

const testRoot = "/storage/emulated/0"

func TestSyncStatListRecv(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a device")
	}
	c, err := Dial()
	if err != nil {
		t.Skipf("no device: %v", err)
	}
	defer c.Close()

	sc, err := c.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	defer sc.Close()

	fi, err := sc.Stat(testRoot + "/DCIM")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	t.Logf("STAT DCIM: mode=%o size=%d mtime=%d dir=%v", fi.Mode, fi.Size, fi.MTime, fi.IsDir())
	if !fi.IsDir() {
		t.Errorf("DCIM should be a directory")
	}

	entries, err := sc.List(testRoot)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("LIST %s: %d entries", testRoot, len(entries))
	if len(entries) == 0 {
		t.Fatal("no entries under the storage root")
	}

	// Find a regular file to pull, then check it byte for byte against the
	// device's own hash.
	var target string
	var wantSize int64
	for _, d := range entries {
		if !d.IsDir() || strings.HasPrefix(d.Name, ".") {
			continue
		}
		sub, err := sc.List(testRoot + "/" + d.Name)
		if err != nil {
			continue
		}
		for _, e := range sub {
			if e.IsRegular() && e.Size > 10000 && e.Size < 5_000_000 {
				target = testRoot + "/" + d.Name + "/" + e.Name
				wantSize = e.Size
				break
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		t.Skip("no suitable file to test RECV")
	}

	var buf bytes.Buffer
	n, err := sc.Recv(target, &buf)
	if err != nil {
		t.Fatalf("Recv %s: %v", target, err)
	}
	sum := sha1.Sum(buf.Bytes())
	local := hex.EncodeToString(sum[:])

	out, err := c.Exec("sha1sum " + ShellQuote(target))
	if err != nil {
		t.Fatalf("Exec sha1sum: %v", err)
	}
	device := strings.Fields(string(out))
	if len(device) == 0 {
		t.Fatalf("no sha1sum output for %s", target)
	}

	t.Logf("RECV %s: %d bytes (listed %d)", target, n, wantSize)
	t.Logf("  device sha1 %s", device[0])
	t.Logf("  local  sha1 %s", local)
	if n != wantSize {
		t.Errorf("got %d bytes, LIST said %d", n, wantSize)
	}
	if device[0] != local {
		t.Errorf("content mismatch")
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestExecIsBinaryClean guards the reason Exec uses the exec service instead of
// shell: a pseudo-terminal would turn LF into CRLF.
func TestExecIsBinaryClean(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a device")
	}
	c, err := Dial()
	if err != nil {
		t.Skipf("no device: %v", err)
	}
	defer c.Close()

	out, err := c.Exec("printf 'a\\nb\\n'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(out) != "a\nb\n" {
		t.Errorf("exec service mangled newlines: got %q, want %q", out, "a\nb\n")
	}
}
