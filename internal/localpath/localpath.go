// Package localpath maps Android file paths onto paths Windows will accept.
//
// Android filenames are far more permissive than NTFS: they may contain
// : ? * " < > |, may end in a dot or space, and may collide with reserved
// device names such as CON or NUL. Writing such a name straight to disk fails,
// so each offending component is percent-encoded.
package localpath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// reserved are Windows device names, which cannot be used as a filename even
// with an extension.
var reserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Encode converts a slash-separated device-relative path into a
// backslash-separated Windows-safe relative path.
func Encode(rel string) string {
	parts := strings.Split(rel, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, encodeComponent(p))
	}
	return filepath.Join(out...)
}

func encodeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20, r == '<', r == '>', r == ':', r == '"',
			r == '|', r == '?', r == '*', r == '\\', r == '%':
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			b.WriteRune(r)
		}
	}
	s = b.String()

	// A trailing dot or space is silently stripped by Windows, which would
	// make two distinct device files collide.
	for len(s) > 0 {
		switch s[len(s)-1] {
		case '.':
			return s[:len(s)-1] + "%2E"
		case ' ':
			return s[:len(s)-1] + "%20"
		}
		break
	}

	// Reserved device names apply to the stem, ignoring any extension.
	stem := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		stem = s[:i]
	}
	if reserved[strings.ToUpper(stem)] {
		return "%" + fmt.Sprintf("%02X", s[0]) + s[1:]
	}
	return s
}

// Collision describes two device paths that map onto the same file on disk.
//
// NTFS is case-insensitive by default, so "Photo.JPG" and "photo.jpg" are
// distinct on the phone but the same file here. Overwriting one with the other
// would lose data silently, so callers must skip and report these.
type Collision struct {
	Local string
	Rels  []string
}

// FindCollisions groups device paths whose encoded form matches case-insensitively.
func FindCollisions(rels []string) []Collision {
	groups := make(map[string][]string, len(rels))
	order := make([]string, 0)
	for _, rel := range rels {
		k := strings.ToLower(Encode(rel))
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], rel)
	}
	var out []Collision
	for _, k := range order {
		if len(groups[k]) > 1 {
			out = append(out, Collision{Local: k, Rels: groups[k]})
		}
	}
	return out
}
