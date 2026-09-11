package adbproto

import "testing"

// ap is an apostrophe, and escAp is the escape ShellQuote must produce for one.
// Both are spelled as escapes so this file never contains the ambiguous runs of
// quotes it exists to check.
const (
	ap    = "\x27"
	escAp = "\x27\\\x27\x27"
)

// unquoteSh is a stand-in for the device's sh: it undoes single quoting the way
// a POSIX shell does, so a quoted token can be checked without a phone. The
// original bug was invisible precisely because nothing on this side ever parsed
// what was sent.
func unquoteSh(s string) (string, bool) {
	var out []byte
	inQuote := false
	for i := 0; i < len(s); {
		c := s[i]
		if inQuote {
			if c == '\x27' {
				inQuote = false
			} else {
				out = append(out, c)
			}
			i++
			continue
		}
		switch c {
		case '\x27':
			inQuote = true
			i++
		case '\\':
			if i+1 >= len(s) {
				return "", false
			}
			out = append(out, s[i+1])
			i += 2
		default:
			out = append(out, c)
			i++
		}
	}
	return string(out), !inQuote
}

// unescapedQuotes counts the apostrophes a shell would treat as quoting
// operators. An odd count means an unterminated string, which is what the old
// three-apostrophe escape produced.
func unescapedQuotes(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '\x27' {
			n++
		}
	}
	return n
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no quotes", "/sdcard/DCIM/IMG_0001.JPG", ap + "/sdcard/DCIM/IMG_0001.JPG" + ap},
		{"one apostrophe", "it" + ap + "s", ap + "it" + escAp + "s" + ap},
		{"multiple apostrophes", "a" + ap + "b" + ap + "c", ap + "a" + escAp + "b" + escAp + "c" + ap},
		{"only an apostrophe", ap, ap + escAp + ap},
		{"empty", "", ap + ap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShellQuote(tc.in)
			if got != tc.want {
				t.Errorf("ShellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if n := unescapedQuotes(got); n%2 != 0 {
				t.Errorf("ShellQuote(%q) = %q has %d unescaped quotes, so the token is unterminated",
					tc.in, got, n)
			}
			back, ok := unquoteSh(got)
			if !ok {
				t.Fatalf("ShellQuote(%q) = %q does not parse as a complete token", tc.in, got)
			}
			if back != tc.in {
				t.Errorf("ShellQuote(%q) = %q, which a shell reads back as %q", tc.in, got, back)
			}
		})
	}
}
