package telnet

import (
	"testing"
)

// newTestSession builds a Session with an empty negotiation memory,
// ready for direct handleNegotiation drives (no socket needed: the
// reply snapshot of s.conn stays nil and writes are skipped).
func newTestSession() *Session {
	return &Session{nego: newNego(), loginBuf: newLoginOutputBuffer()}
}

func TestNegotiationAcceptTable(t *testing.T) {
	tests := []struct {
		cmd, opt byte
		wantSent bool
	}{
		{will, optSuppressGoAhead, true}, // WILL SGA -> DO
		{will, optEcho, true},            // WILL ECHO -> DO
		{will, optTerminalType, true},    // WILL TTYPE -> DO
		{do, optNAWS, true},              // DO NAW -> WILL
		{do, optSuppressGoAhead, true},   // DO SGA -> WILL
		{do, optTerminalType, true},      // DO TTYPE -> WILL
		{do, optEcho, true},              // DO ECHO -> WONT (never echo)
		{will, 5, true},                  // WILL STATUS -> DONT (unknown)
		{do, 5, true},                    // DO STATUS -> WONT
		{will, optBinaryLegacy, false},   // BINARY: refused once...
		{do, optBinaryLegacy, false},     // ...but DONT/WONT still go out
	}
	for i, tt := range tests {
		s := newTestSession()
		s.handleNegotiation(tt.cmd, tt.opt)
		if len(s.nego.sentDo)+len(s.nego.sentDont)+len(s.nego.sentWill)+len(s.nego.sentWont) == 0 {
			t.Errorf("case %d: cmd=%d opt=%d produced no latch", i, tt.cmd, tt.opt)
		}
	}
}

func TestNegotiationBinaryNeverAccepted(t *testing.T) {
	s := newTestSession()
	s.handleNegotiation(will, optBinaryLegacy) // peer offers BINARY
	if s.nego.sentDo[optBinaryLegacy] {
		t.Error("BINARY must never be answered with DO (spec §3)")
	}
	if !s.nego.sentDont[optBinaryLegacy] {
		t.Error("BINARY must be refused with DONT")
	}
	s.handleNegotiation(do, optBinaryLegacy) // peer asks us to enable it
	if s.nego.sentWill[optBinaryLegacy] {
		t.Error("BINARY must never be answered with WILL")
	}
}

// The regression that matters: a server that echoes every frame back
// (as its own) must see a BOUNDED number of outbound frames, because
// each (cmd, opt) answer is latched and never re-sent. Before the
// state machine, WILL/DO pairs ping-ponged forever and pegged the CPU.
func TestNegotiationEchoServerConverges(t *testing.T) {
	s := newTestSession()
	// Our proactive open, as Dial would latch it.
	s.nego.sentDo[optSuppressGoAhead] = true
	s.nego.sentWill[optTerminalType] = true

	frames := []struct{ cmd, opt byte }{
		{do, optSuppressGoAhead}, // echoes of our two startup frames...
		{will, optTerminalType},
		{dont, optTerminalType}, // ...and of whatever we answer back
		{wont, optSuppressGoAhead},
	}
	for i := 0; i < 50; i++ {
		for _, f := range frames {
			s.handleNegotiation(f.cmd, f.opt)
		}
	}
	if got := latches(s); got > 8 { // hard ceiling: 4 answer kinds, each latched once
		t.Fatalf("outbound frames unbounded under echo storm: %d latches after 200 frames", got)
	}
}

func TestNegotiationRepeatFramesChangeNothing(t *testing.T) {
	s := newTestSession()
	s.handleNegotiation(will, optEcho)
	first := snapshot(s)
	for i := 0; i < 5; i++ {
		s.handleNegotiation(will, optEcho)
	}
	if got := snapshot(s); got != first {
		t.Errorf("repeated WILL ECHO changed behavior: %q -> %q", first, got)
	}
}

func TestNegotiationUnsolicitedWontStaysSilent(t *testing.T) {
	s := newTestSession()
	s.handleNegotiation(wont, optEcho) // teardown of a state we never saw
	if s.nego.sentDont[optEcho] || s.nego.sentWill[optEcho] || s.nego.sentWont[optEcho] || s.nego.sentDo[optEcho] {
		t.Error("unsolicited WONT must not generate any answer")
	}
}

func TestNegotiationDontConfirmsWithWontOnce(t *testing.T) {
	s := newTestSession()
	s.handleNegotiation(do, optNAWS) // we answer WILL NAW
	s.handleNegotiation(dont, optNAWS)
	if !s.nego.sentWont[optNAWS] {
		t.Error("DONT tearing down an agreed option must be confirmed with WONT (RFC 1143)")
	}
	before := snapshot(s)
	for i := 0; i < 10; i++ {
		s.handleNegotiation(do, optNAWS)
		s.handleNegotiation(dont, optNAWS)
	}
	if got := snapshot(s); got != before {
		t.Errorf("flip-flop peer must not produce repeat answers: %q -> %q", before, got)
	}
}

func TestCRNULFoldUnconditional(t *testing.T) {
	// The NVT fold no longer depends on BINARY state (spec §3: the
	// mechanism is orthogonal to the option).
	s := newTestSession()
	s.handleNegotiation(will, optBinaryLegacy) // even if the peer insists on binary
	out := s.filterIAC([]byte{'a', '\r', 0, 'b'})
	if string(out) != "a\rb" {
		t.Errorf("CR NUL not folded: %q", out)
	}
}

func snapshot(s *Session) string {
	b := make([]byte, 0, 6)
	for _, m := range []map[byte]bool{s.nego.sentDo, s.nego.sentDont, s.nego.sentWill, s.nego.sentWont, s.nego.peerWill, s.nego.peerDo} {
		n := 0
		for _, v := range m {
			if v {
				n++
			}
		}
		b = append(b, byte(n))
	}
	return string(b)
}

func latches(s *Session) int {
	n := 0
	for _, m := range []map[byte]bool{s.nego.sentDo, s.nego.sentDont, s.nego.sentWill, s.nego.sentWont, s.nego.peerWill, s.nego.peerDo} {
		for _, v := range m {
			if v {
				n++
			}
		}
	}
	return n
}
