package adbproto

import (
	"testing"
	"time"

	"quickadbackup/internal/winusb"
)

// TestReconnectAfterClose records what the USB interface does between one
// connection closing and the next opening, which is the case hit by simply
// running the tool twice in a row.
func TestReconnectAfterClose(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a device")
	}
	c, err := Dial()
	if err != nil {
		t.Skipf("no device: %v", err)
	}
	before, _ := winusb.DevicePaths()
	t.Logf("while connected: %d path(s): %v", len(before), before)
	c.Close()

	deadline := time.Now().Add(8 * time.Second)
	start := time.Now()
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		paths, err := winusb.DevicePaths()
		if err != nil {
			t.Logf("t=%5dms enumerate error: %v", time.Since(start).Milliseconds(), err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if len(paths) == 0 {
			t.Logf("t=%5dms no interface present", time.Since(start).Milliseconds())
			time.Sleep(200 * time.Millisecond)
			continue
		}
		c2, err := DialPath(paths[0])
		if err != nil {
			t.Logf("t=%5dms present but dial failed: %v", time.Since(start).Milliseconds(), err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		t.Logf("t=%5dms reconnected on attempt %d: %s",
			time.Since(start).Milliseconds(), attempt, c2.Banner[:40])
		c2.Close()
		return
	}
	t.Fatal("never reconnected within 8s")
}
