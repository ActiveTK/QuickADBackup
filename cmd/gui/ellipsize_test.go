package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestEllipsize pins the rune behaviour. The label this feeds shows device
// paths, which on a Japanese phone are mostly Japanese, and byte slicing used
// to cut those characters in half.
func TestEllipsize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than max", "/sdcard/DCIM/a.jpg", 40, "/sdcard/DCIM/a.jpg"},
		{"exactly max", "abcdefghij", 10, "abcdefghij"},
		{"one over max", "abcdefghijk", 10, "...efghijk"},
		{"long ascii path", "/storage/emulated/0/Pictures/holiday/beach.jpg", 20, "...holiday/beach.jpg"},
		{"japanese shorter than max", "/写真/旅行/海.jpg", 40, "/写真/旅行/海.jpg"},
		{"japanese exactly max", "写真旅行海山川", 7, "写真旅行海山川"},
		{"japanese one over max", "写真旅行海山川空", 7, "...海山川空"},
		{"japanese long path", "/storage/emulated/0/ダウンロード/請求書/2026年4月分.pdf", 20, ".../請求書/2026年4月分.pdf"},
		{"basename longer than max", "/a/" + strings.Repeat("ほ", 60), 10, "..." + strings.Repeat("ほ", 7)},
		{"max of three", "/a/b/c/long.txt", 3, "txt"},
		{"max of zero", "/a/b/c/long.txt", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ellipsize(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("ellipsize(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("ellipsize(%q, %d) = %q, which is not valid UTF-8", tc.in, tc.max, got)
			}
			if n := utf8.RuneCountInString(got); n > tc.max {
				t.Errorf("ellipsize(%q, %d) = %q, %d runes long", tc.in, tc.max, got, n)
			}
		})
	}
}

// TestEllipsizeNeverCorrupts sweeps every width against a multi-byte path,
// because the old byte slicing only produced mojibake at some of them.
func TestEllipsizeNeverCorrupts(t *testing.T) {
	const path = "/storage/emulated/0/ダウンロード/請求書/2026年4月分の明細.pdf"
	for max := 0; max <= utf8.RuneCountInString(path)+5; max++ {
		got := ellipsize(path, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8: %q", max, got)
		}
		if n := utf8.RuneCountInString(got); n > max {
			t.Fatalf("max=%d produced %d runes: %q", max, n, got)
		}
	}
}
