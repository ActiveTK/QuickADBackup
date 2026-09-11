package winusb

import "testing"

// TestOpenDevice needs a real phone with USB debugging on, and the adb server
// stopped, since the interface can only be claimed once.
func TestOpenDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a device")
	}
	paths, err := DevicePaths()
	if err != nil {
		t.Fatalf("DevicePaths: %v", err)
	}
	if len(paths) == 0 {
		t.Skip("no ADB interface present")
	}
	for _, p := range paths {
		t.Logf("found: %s", p)
	}

	d, err := Open(paths[0])
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	in, out := d.Endpoints()
	t.Logf("claimed, bulk endpoints in=%#x out=%#x", in, out)
	if in&0x80 == 0 {
		t.Errorf("IN endpoint %#x lacks the direction bit", in)
	}
	if out&0x80 != 0 {
		t.Errorf("OUT endpoint %#x has the direction bit set", out)
	}
}
