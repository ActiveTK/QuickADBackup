package device

import "testing"

func TestParseLine(t *testing.T) {
	const prefix = "/storage/emulated/0/"
	tests := []struct {
		name    string
		line    string
		want    Entry
		wantErr bool
	}{
		{
			name: "typical",
			line: "5972049|1733552935.123456789|/storage/emulated/0/DCIM/Camera/IMG.jpg",
			want: Entry{Rel: "DCIM/Camera/IMG.jpg", Size: 5972049, MTimeN: 1733552935123456789},
		},
		{
			name: "whole seconds",
			line: "10|1700000000|/storage/emulated/0/a.txt",
			want: Entry{Rel: "a.txt", Size: 10, MTimeN: 1700000000000000000},
		},
		{
			// The separator also occurs in filenames, so only the first two
			// count.
			name: "pipe in filename",
			line: "7|1700000000.5|/storage/emulated/0/we|rd/na|me.txt",
			want: Entry{Rel: "we|rd/na|me.txt", Size: 7, MTimeN: 1700000000500000000},
		},
		{
			name: "short fraction pads to nanoseconds",
			line: "1|1700000000.25|/storage/emulated/0/a",
			want: Entry{Rel: "a", Size: 1, MTimeN: 1700000000250000000},
		},
		{
			name: "empty file",
			line: "0|1700000000.0|/storage/emulated/0/.nomedia",
			want: Entry{Rel: ".nomedia", Size: 0, MTimeN: 1700000000000000000},
		},
		{name: "outside root", line: "1|1700000000|/data/local/x", wantErr: true},
		{name: "root itself", line: "1|1700000000|/storage/emulated/0/", wantErr: true},
		{name: "no separators", line: "garbage", wantErr: true},
		{name: "one separator", line: "1|/storage/emulated/0/a", wantErr: true},
		{name: "non-numeric size", line: "x|1700000000|/storage/emulated/0/a", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLine(tt.line, prefix)
			if ok == tt.wantErr {
				t.Fatalf("parseLine ok=%v, want error=%v", ok, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseLine = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseUnixNanos(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1700000000", 1700000000_000000000, true},
		{"1700000000.000000001", 1700000000_000000001, true},
		{"1700000000.9", 1700000000_900000000, true},
		// find has been seen emitting more than nine fractional digits.
		{"1700000000.1234567891", 1700000000_123456789, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseUnixNanos(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("parseUnixNanos(%q) = %d,%v want %d,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestIsExcluded(t *testing.T) {
	ex := []string{"Android/data", "Android/obb"}
	cases := map[string]bool{
		"Android/data":            true,
		"Android/data/com.x/a.db": true,
		"Android/obb/x":           true,
		// A prefix match must not catch a sibling with a longer name.
		"Android/database/x": false,
		"Android/media/x":    false,
		"DCIM/Camera/a.jpg":  false,
	}
	for rel, want := range cases {
		if got := isExcluded(rel, ex); got != want {
			t.Errorf("isExcluded(%q) = %v, want %v", rel, got, want)
		}
	}
}
