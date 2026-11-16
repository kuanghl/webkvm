package serialx

import (
	"bytes"
	"testing"
)

// Same rule the telnet session applies to its read buffer: a serial line
// at 921600 baud outruns a tab that stopped draining, and the pump only
// notices when its write deadline fires.
func TestPushCapsTheOverflowBuffer(t *testing.T) {
	s := &Session{}
	chunk := bytes.Repeat([]byte{'z'}, 64<<10)
	for i := 0; i < (maxOverflowBytes/(64<<10))+40; i++ {
		s.push(chunk)
	}
	s.push([]byte("NEWEST"))

	s.mu.Lock()
	got := append([]byte(nil), s.overflow...)
	s.mu.Unlock()

	if len(got) != maxOverflowBytes {
		t.Fatalf("overflow = %d bytes, want the cap %d", len(got), maxOverflowBytes)
	}
	if !bytes.HasSuffix(got, []byte("NEWEST")) {
		t.Error("the cap dropped the freshest output instead of the stale ones")
	}
}
