package telnet

import (
	"bytes"
	"testing"
)

// The read buffer exists because the consumer is a WebSocket pump that
// can stall; nobody reading must not become unbounded memory. The bytes
// kept are the newest ones, because a terminal that comes back shows the
// current screen, not what the remote printed while nobody was looking.
func TestPushDataCapsTheReadBuffer(t *testing.T) {
	s := newTestSession()
	chunk := bytes.Repeat([]byte{'z'}, 64<<10)
	for i := 0; i < (maxPendingBytes/(64<<10))+40; i++ {
		s.pushData(chunk)
	}
	s.pushData([]byte("NEWEST"))

	s.mu.Lock()
	got := append([]byte(nil), s.pending...)
	s.mu.Unlock()

	if len(got) != maxPendingBytes {
		t.Fatalf("pending = %d bytes, want the cap %d", len(got), maxPendingBytes)
	}
	if !bytes.HasSuffix(got, []byte("NEWEST")) {
		t.Error("the cap dropped the freshest output instead of the stale ones")
	}
}

// A peer that opens IAC SB and never sends IAC SE used to grow the
// filter's hold-back without end, recopying it whole on every read.
func TestUnterminatedSubnegotiationStopsGrowing(t *testing.T) {
	s := newTestSession()
	s.filterIAC([]byte{iac, sb, optTerminalType}) // never terminated
	junk := bytes.Repeat([]byte{'x'}, 1024)
	for i := 0; i < 4000; i++ {
		s.filterIAC(junk)
		if len(s.iacPending) > maxIacPending+len(junk) {
			t.Fatalf("hold-back grew to %d bytes at read %d", len(s.iacPending), i)
		}
	}

	// A sequence that merely straddles a read boundary is still held:
	// the cap must not have cost the filter its real job.
	s2 := newTestSession()
	if out := s2.filterIAC(append([]byte("hello"), iac)); string(out) != "hello" {
		t.Errorf("split IAC leaked %q", out)
	}
	if out := s2.filterIAC([]byte{will, optEcho, '!'}); string(out) != "!" {
		t.Errorf("IAC continuation leaked %q, want just the data byte", out)
	}
}
