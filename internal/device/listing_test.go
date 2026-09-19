package device

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

// TestCountMismatchIsRetryable: the two enumerations are separate commands, so
// a phone writing a thumbnail between them makes them disagree through no fault
// of the listing. List has to be able to tell that apart from a hard failure to
// know whether retrying is worth anything.
func TestCountMismatchIsRetryable(t *testing.T) {
	err := error(&countMismatchError{Counted: 25761, Listed: 25760})

	var mismatch *countMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatal("errors.As did not recognise the mismatch, so List would never retry")
	}
	if mismatch.Counted != 25761 || mismatch.Listed != 25760 {
		t.Errorf("counts came back as %d/%d", mismatch.Counted, mismatch.Listed)
	}

	// Anything else must not be retried: a truncated listing that keeps coming
	// back truncated is the failure this check exists to catch.
	if errors.As(errors.New("device refused"), &mismatch) {
		t.Error("an unrelated error was treated as a retryable mismatch")
	}

	msg := err.Error()
	for _, want := range []string{"25761", "25760", "refusing", "busy writing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, strconv.Itoa(listAttempts)) {
		t.Errorf("message does not say how many attempts were made:\n%s", msg)
	}
}

// TestListAttemptsIsBounded keeps the retry from becoming a way to paper over a
// listing that is genuinely being truncated.
func TestListAttemptsIsBounded(t *testing.T) {
	if listAttempts < 2 {
		t.Error("a single attempt makes the retry pointless")
	}
	if listAttempts > 5 {
		t.Errorf("listAttempts = %d: too willing to accept a moving target", listAttempts)
	}
	if total := time.Duration(listAttempts-1) * listRetryDelay; total > 5*time.Second {
		t.Errorf("retrying adds up to %s before the run is refused", total)
	}
}
