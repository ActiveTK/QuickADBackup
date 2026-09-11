// Package progress carries operation progress from the sync and verify engines
// to whatever is displaying it.
//
// The engines emit structured updates rather than preformatted lines, because a
// progress bar needs counts and a log needs sentences, and only the caller
// knows which it wants.
package progress

import "fmt"

type Phase string

const (
	PhaseScanning Phase = "scanning"
	PhaseCopying  Phase = "copying"
	// PhaseHashingLocal is the PC-side half of a verify. It is a separate
	// phase because it reads local disk rather than the device, and on a large
	// backup it runs long enough that leaving it unreported looks like a hang.
	PhaseHashingLocal Phase = "hashing-local"
	PhaseVerifying    Phase = "verifying"
	PhaseArchiving    Phase = "archiving"
	PhaseDone         Phase = "done"
)

// Update is one report from a running operation.
//
// Total of zero means the work is not yet countable, which is the case while
// the device is still being scanned.
type Update struct {
	Phase   Phase
	Message string // a complete sentence, when there is one worth logging
	Done    int
	Total   int
	Bytes   int64
	Current string // the file being handled, when relevant
}

// Fraction returns progress in the range 0..1, or 0 when it is unknown.
func (u Update) Fraction() float64 {
	if u.Total <= 0 {
		return 0
	}
	if u.Done >= u.Total {
		return 1
	}
	return float64(u.Done) / float64(u.Total)
}

// Func receives updates. It may be called from any goroutine, and often.
type Func func(Update)

// Report is a nil-safe way for an engine to emit an update.
func (f Func) Report(u Update) {
	if f != nil {
		f(u)
	}
}

// Log emits a message-only update for the given phase.
func (f Func) Log(phase Phase, format string, args ...any) {
	f.Report(Update{Phase: phase, Message: fmt.Sprintf(format, args...)})
}

// HumanBytes formats a byte count for display.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
