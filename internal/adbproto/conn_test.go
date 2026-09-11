package adbproto

import (
	"io"
	"strings"
	"testing"
)

// TestHandshakeAndShell needs a real phone with USB debugging on, and no adb
// server running, since the USB interface can only be claimed once.
func TestHandshakeAndShell(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a device")
	}
	c, err := Dial()
	if err != nil {
		t.Skipf("no device: %v", err)
	}
	defer c.Close()

	t.Logf("banner:  %s", c.Banner)
	t.Logf("maxdata: %d", c.MaxData)
	if !strings.HasPrefix(c.Banner, "device::") {
		t.Errorf("unexpected banner %q", c.Banner)
	}

	s, err := c.Open("shell:echo quickadbackup-canary && getprop ro.product.model")
	if err != nil {
		t.Fatalf("Open shell: %v", err)
	}
	defer s.Close()

	out, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("reading shell output: %v", err)
	}
	got := string(out)
	t.Logf("shell said: %q", strings.TrimSpace(got))
	if !strings.Contains(got, "quickadbackup-canary") {
		t.Errorf("canary missing from shell output %q", got)
	}
}
