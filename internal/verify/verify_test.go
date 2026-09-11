package verify

import "testing"

func TestParseSha1Line(t *testing.T) {
	const sum = "f572d396fae9206628714fb2ce00f72e94f2258f"
	cases := []struct {
		name, line, wantPath string
		wantOK               bool
	}{
		// Text mode: digest, space, space, path.
		{"text mode", sum + "  /sdcard/a.txt", "/sdcard/a.txt", true},
		// Binary mode. Observed from a real sha1sum: the second separator is an
		// asterisk, not a space. Trimming the separator away as whitespace left
		// the '*' glued to the path, which then matched no known file and filed
		// every single result as unreadable.
		{"binary mode", sum + " */sdcard/a.txt", "/sdcard/a.txt", true},
		// A filename may legitimately end or begin with a space, so the path is
		// taken verbatim rather than trimmed.
		{"trailing space in name", sum + "  /sdcard/a .txt ", "/sdcard/a .txt ", true},
		{"apostrophe in name", sum + "  /sdcard/it's.txt", "/sdcard/it's.txt", true},
		{"single separator", sum + " /sdcard/a.txt", "/sdcard/a.txt", true},

		{"too short", sum, "", false},
		{"empty", "", "", false},
		{"not hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz  /sdcard/a.txt", "", false},
		{"an error line", "sha1sum: /sdcard/x: Permission denied", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSum, gotPath, ok := parseSha1Line(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if gotSum != sum {
				t.Errorf("sum = %q, want %q", gotSum, sum)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}
