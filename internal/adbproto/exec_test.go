package adbproto

import "testing"

// TestSplitExecMarker pins down the stripping rules, because a caller compares
// pulled output byte for byte: nothing may be trimmed or appended, and output
// that happens to quote the marker itself must not shift the split.
func TestSplitExecMarker(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOut  string
		wantCode int
		wantOK   bool
	}{
		{"trailing newline kept", "a\nb\n" + execMarker + "0\n", "a\nb\n", 0, true},
		{"no newline before the marker", "x" + execMarker + "0\n", "x", 0, true},
		{"no output at all", execMarker + "1\n", "", 1, true},
		{"marker missing", "no marker at all", "", 0, false},
		{"marker quoted in the output", "see " + execMarker + "0 in the log\n" + execMarker + "42\n",
			"see " + execMarker + "0 in the log\n", 42, true},
		{"marker without digits", "oops\n" + execMarker + "\n", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code, ok := splitExecMarker([]byte(tc.in))
			if ok != tc.wantOK {
				t.Fatalf("splitExecMarker(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if string(out) != tc.wantOut {
				t.Errorf("splitExecMarker(%q) out = %q, want %q", tc.in, out, tc.wantOut)
			}
			if code != tc.wantCode {
				t.Errorf("splitExecMarker(%q) code = %d, want %d", tc.in, code, tc.wantCode)
			}
		})
	}
}
