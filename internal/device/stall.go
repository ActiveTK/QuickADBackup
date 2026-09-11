package device

import (
	"context"
	"time"

	"quickadbackup/internal/adbproto"
)

// AbortGrace is how long a cancelled operation is given to stop on its own
// before the connection is torn down under it.
//
// A worker that is mid-file finishes that file and exits, which takes as long
// as the remaining bytes; a few seconds covers that comfortably.
const AbortGrace = 10 * time.Second

// AbortOnStall makes cancellation work even when the device has stopped
// answering, and returns a function the caller must call when the operation
// ends.
//
// A USB bulk read has no timeout, so a phone that goes unresponsive parks the
// protocol reader in the driver forever, and every worker goroutine behind it
// parks on a channel that will never be written. Cancelling the context does
// nothing in that state: the workers never notice, so the operation never
// returns, so Ctrl+C and the GUI's cancel button do nothing at all.
//
// The grace period exists because the normal case is not a stall. A cancelled
// run usually unwinds on its own within a file or two, and aborting immediately
// would throw away a connection that was about to come back cleanly. Only a
// cancellation that produces no progress at all gets the connection pulled.
func AbortOnStall(ctx context.Context, c *adbproto.Conn, grace time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		select {
		case <-done:
		case <-time.After(grace):
			c.Abort()
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(done)
		}
	}
}
