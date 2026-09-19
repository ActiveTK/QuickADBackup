package verify

import (
	"testing"

	"quickadbackup/internal/adbproto"
)

// TestBatchLimitRespectsTheDevice: the sha1sum command is the payload of one
// OPEN message, and a device that negotiated a small maximum payload answers an
// oversized one by closing the connection. A verify that kills the link on its
// first batch looks exactly like a broken cable.
func TestBatchLimitRespectsTheDevice(t *testing.T) {
	tests := []struct {
		maxData uint32
		want    int
	}{
		{256 * 1024, batchBudget},      // a modern device: the fixed budget wins
		{1 << 20, batchBudget},         // a generous one: still the fixed budget
		{4096, 4096 - commandOverhead}, // legacy adbd: the device wins
		{8192, 8192 - commandOverhead}, //
		{commandOverhead, 1},           // absurdly small, but never zero or negative
		{0, 1},                         //
	}
	for _, tc := range tests {
		got := batchLimit(&adbproto.Conn{MaxData: tc.maxData})
		if got != tc.want {
			t.Errorf("batchLimit(MaxData=%d) = %d, want %d", tc.maxData, got, tc.want)
		}
		if got < 1 {
			t.Errorf("batchLimit(MaxData=%d) = %d, which would never make progress", tc.maxData, got)
		}
	}
}

// TestBatchLimitNeverExceedsOneMessage is the property that matters: whatever
// the device said, a full batch plus the wrapper has to fit in one message.
func TestBatchLimitNeverExceedsOneMessage(t *testing.T) {
	for _, maxData := range []uint32{4096, 16384, 65536, 256 * 1024, 1 << 20} {
		limit := batchLimit(&adbproto.Conn{MaxData: maxData})
		if limit+commandOverhead > int(maxData) && limit > 1 {
			t.Errorf("MaxData=%d: a %d-byte batch plus %d of wrapper does not fit",
				maxData, limit, commandOverhead)
		}
	}
}
