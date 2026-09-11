package winusb

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// TestInUseErrorWrapsErrInUse guards the retry loop in adbproto.DialTimeout,
// which gives up immediately on ErrInUse. The wrapping used to keep only the
// raw errno, so that check never matched and a running adb server cost the user
// the full retry timeout before the same failure was reported anyway.
func TestInUseErrorWrapsErrInUse(t *testing.T) {
	for _, errno := range []syscall.Errno{errAccessDenied, errSharingViolation, errBusy, errNotSupported} {
		err := inUseError("CreateFile", errno)
		if !errors.Is(err, ErrInUse) {
			t.Errorf("errors.Is(inUseError(%d), ErrInUse) = false", errno)
		}
		if !errors.Is(err, errno) {
			t.Errorf("inUseError(%d) lost the underlying errno", errno)
		}
		if !strings.Contains(err.Error(), "adb kill-server") {
			t.Errorf("inUseError(%d) dropped the guidance text: %v", errno, err)
		}
	}
}

func TestInUseRecognisesTheRightErrnos(t *testing.T) {
	cases := []struct {
		err   error
		codes []syscall.Errno
		want  bool
	}{
		{syscall.Errno(errAccessDenied), []syscall.Errno{errAccessDenied, errSharingViolation}, true},
		{syscall.Errno(errSharingViolation), []syscall.Errno{errAccessDenied, errSharingViolation}, true},
		{syscall.Errno(errBusy), []syscall.Errno{errBusy, errAccessDenied, errNotSupported}, true},
		{syscall.Errno(errNotSupported), []syscall.Errno{errBusy, errAccessDenied, errNotSupported}, true},
		{syscall.Errno(errNoMoreItems), []syscall.Errno{errAccessDenied, errSharingViolation}, false},
		{errors.New("not an errno"), []syscall.Errno{errAccessDenied}, false},
	}
	for _, tc := range cases {
		if got := inUse(tc.err, tc.codes...); got != tc.want {
			t.Errorf("inUse(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
