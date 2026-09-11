package localpath

import (
	"path/filepath"
	"testing"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string // path components
	}{
		{"plain", "DCIM/Camera/IMG.jpg", []string{"DCIM", "Camera", "IMG.jpg"}},
		{"colon", "Download/a:b.txt", []string{"Download", "a%3Ab.txt"}},
		{"question mark", "x/what?.png", []string{"x", "what%3F.png"}},
		{"star and pipe", `a*b|c`, []string{"a%2Ab%7Cc"}},
		{"quote and angles", `<a>"b"`, []string{"%3Ca%3E%22b%22"}},
		// Windows silently strips these, which would merge two distinct files.
		{"trailing dot", "dir/name.", []string{"dir", "name%2E"}},
		{"trailing space", "dir/name ", []string{"dir", "name%20"}},
		// Reserved device names are unusable even with an extension.
		{"reserved", "CON", []string{"%43ON"}},
		{"reserved with ext", "nul.txt", []string{"%6Eul.txt"}},
		{"not reserved", "CONSOLE.txt", []string{"CONSOLE.txt"}},
		// The escape character itself must be escaped, or "a%3A" on the device
		// would decode to the same name as a literal "a:".
		{"percent", "a%3A", []string{"a%253A"}},
		{"unicode kept", "写真/日本語.jpg", []string{"写真", "日本語.jpg"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := filepath.Join(tt.want...)
			if got := Encode(tt.in); got != want {
				t.Errorf("Encode(%q) = %q, want %q", tt.in, got, want)
			}
		})
	}
}

func TestEncodeDistinguishesNames(t *testing.T) {
	// Files that differ on the device must not collapse into one name here.
	pairs := [][2]string{
		{"name.", "name"},
		{"name ", "name"},
		{"a:b", "a%3Ab"},
	}
	for _, p := range pairs {
		if Encode(p[0]) == Encode(p[1]) {
			t.Errorf("Encode collapsed %q and %q into %q", p[0], p[1], Encode(p[0]))
		}
	}
}

func TestFindCollisions(t *testing.T) {
	// NTFS is case-insensitive, so these three are one file on disk.
	got := FindCollisions([]string{
		"DCIM/Photo.JPG",
		"DCIM/photo.jpg",
		"DCIM/PHOTO.jpg",
		"DCIM/other.jpg",
	})
	if len(got) != 1 {
		t.Fatalf("got %d collisions, want 1: %+v", len(got), got)
	}
	if len(got[0].Rels) != 3 {
		t.Errorf("collision covers %v, want the three photo variants", got[0].Rels)
	}
}

func TestFindCollisionsNone(t *testing.T) {
	if got := FindCollisions([]string{"a/b.jpg", "a/c.jpg", "d/b.jpg"}); len(got) != 0 {
		t.Errorf("unexpected collisions: %+v", got)
	}
}
