package adbproto

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"quickadbackup/internal/winusb"
)

// ErrAmbiguousDevice reports that the caller did not say which of several
// connected devices to use.
//
// Picking one silently, as this used to, makes the answer to "which phone did
// that back up" depend on Windows enumeration order. For a tool whose whole job
// is to produce a faithful copy of one device, that is not a detail worth
// guessing at.
var ErrAmbiguousDevice = errors.New("more than one Android device is connected")

// AmbiguousDeviceError names the interfaces that were found, so the caller can
// print them and the user can pick one.
type AmbiguousDeviceError struct {
	Paths []string
	Match string // the selector that failed to narrow them down, if any
}

func (e *AmbiguousDeviceError) Error() string {
	var b strings.Builder
	if e.Match == "" {
		fmt.Fprintf(&b, "%d Android devices are connected; this tool backs up one device at a time "+
			"and will not guess which", len(e.Paths))
	} else {
		fmt.Fprintf(&b, "%d connected devices match %q", len(e.Paths), e.Match)
	}
	b.WriteString(".\nUnplug the others, or pass -device with enough of one of these to tell it apart:")
	for _, p := range e.Paths {
		b.WriteString("\n  " + p)
	}
	return b.String()
}

func (e *AmbiguousDeviceError) Is(target error) bool { return target == ErrAmbiguousDevice }

// NoMatchingDeviceError reports that a selector matched nothing that is plugged
// in. It is kept distinct from "no device at all" because the fix is different.
type NoMatchingDeviceError struct {
	Paths []string
	Match string
}

func (e *NoMatchingDeviceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no connected device matches %q", e.Match)
	if len(e.Paths) == 0 {
		b.WriteString("; nothing with USB debugging is plugged in")
		return b.String()
	}
	b.WriteString(". Connected:")
	for _, p := range e.Paths {
		b.WriteString("\n  " + p)
	}
	return b.String()
}

// selectPath narrows the present interfaces down to the one to claim.
//
// match is compared case-insensitively against the interface path, so any
// distinguishing fragment of a path printed by an earlier error will do.
func selectPath(paths []string, match string) (string, error) {
	if match == "" {
		switch len(paths) {
		case 0:
			return "", errors.New("no Android device with USB debugging found")
		case 1:
			return paths[0], nil
		default:
			return "", &AmbiguousDeviceError{Paths: paths}
		}
	}
	var hits []string
	for _, p := range paths {
		if strings.Contains(strings.ToLower(p), strings.ToLower(match)) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return "", &NoMatchingDeviceError{Paths: paths, Match: match}
	case 1:
		return hits[0], nil
	default:
		return "", &AmbiguousDeviceError{Paths: hits, Match: match}
	}
}

// DialTimeoutSelect waits for a claimable device and authenticates to it.
//
// match, when not empty, is a fragment of the interface path identifying which
// device to use; an empty match is only valid when exactly one is connected.
// See DialTimeout for why the wait exists at all.
func DialTimeoutSelect(timeout time.Duration, match string, notify func()) (*Conn, error) {
	deadline := time.Now().Add(timeout)
	notified := false
	var lastErr error
	for {
		paths, err := winusb.DevicePaths()
		if err != nil {
			lastErr = err
		} else {
			path, err := selectPath(paths, match)
			switch {
			case err == nil:
				c, err := DialPath(path)
				if err == nil {
					return c, nil
				}
				// Another process holding the interface is a standing
				// condition, not a race with re-enumeration, so retrying only
				// stalls.
				if errors.Is(err, winusb.ErrInUse) {
					return nil, err
				}
				lastErr = err
			case errors.Is(err, ErrAmbiguousDevice):
				// Waiting cannot resolve this; only the user can.
				return nil, err
			default:
				lastErr = err
			}
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		if !notified && notify != nil {
			notify()
			notified = true
		}
		time.Sleep(250 * time.Millisecond)
	}
}
