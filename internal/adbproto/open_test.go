package adbproto

import (
	"errors"
	"strings"
	"testing"
)

// TestOpenRefusesAnOversizedService: send does not split, so a service name
// longer than one message would be handed to the device as a malformed packet
// and adbd answers that by dropping the connection. Callers that build a
// command out of a variable number of paths need to be told, not disconnected.
func TestOpenRefusesAnOversizedService(t *testing.T) {
	c := &Conn{MaxData: 4096}
	_, err := c.Open("exec:sha1sum " + strings.Repeat("x", 8192))
	if err == nil {
		t.Fatal("Open accepted a service larger than one message")
	}
	if !errors.Is(err, ErrRequestTooLong) {
		t.Errorf("Open error = %v, want it to wrap ErrRequestTooLong", err)
	}
	if !strings.Contains(err.Error(), "4096") {
		t.Errorf("error %q does not say what the device would accept", err)
	}
}

// TestAliveReportsATornDownConnection is what the GUI asks after an operation,
// instead of matching on the error text: a cancelled run reports
// context.Canceled while the abort that unblocked it has already killed the
// link, and an aborted one reports ErrAborted. Neither says "usb".
func TestAliveReportsATornDownConnection(t *testing.T) {
	c := &Conn{MaxData: maxPayload, streams: map[uint32]*Stream{}, done: make(chan struct{})}
	if !c.Alive() {
		t.Fatal("a fresh connection reports itself dead")
	}
	c.fail(errors.New("device went away"))
	if c.Alive() {
		t.Error("a failed connection still reports itself alive")
	}
	// fail is idempotent, and so is the answer.
	c.fail(errors.New("again"))
	if c.Alive() {
		t.Error("Alive changed its mind on the second failure")
	}
}
