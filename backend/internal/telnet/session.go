// Package telnet is the webkvm port of uniterm's telnet session
// (backend/session/telnet_session.go), trimmed to what the machine
// terminal needs:
//
//   - IAC filtering with cross-read sequence hold-back (issue #1415
//     logic: an incomplete sequence is stashed and merged with the next
//     read instead of leaking bytes into the data stream);
//   - option negotiation: BINARY (NVT CR NUL collapse), SGA, ECHO,
//     TTYPE sub-negotiation, NAW window change;
//   - SO_OOBINLINE so telnet urgent data (the SYNCH IAC DM pair) is
//     delivered in the normal stream (issue #1415);
//   - prompt-driven auto-login: 5s prompt watch, 50ms poll, 3s blind
//     grace — credentials are sent when the server asks, not on a timer;
//   - loginPhaseOver (shell-prompt detection) kept for parity with the
//     uniterm porting checklist.
//
// Deliberately NOT ported (P2): character-encoding transcoding (output
// is treated as UTF-8), line send-mode / local-echo options, and the
// post-login expect script.
package telnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"webkvm/internal/safego"
)

// Telnet protocol constants.
const (
	iac  = 255
	will = 251
	wont = 252
	do   = 253
	dont = 254
	sb   = 250
	se   = 240
	nul  = 0

	// BINARY (0) is intentionally absent from the accept table: the
	// protocol spec §3 rules it out, and the NVT CR-NUL fold stays
	// unconditional as a result. The constant is kept for the refusal
	// tests.
	optBinaryLegacy    = 0
	optEcho            = 1
	optSuppressGoAhead = 3
	optTerminalType    = 24
	optNAWS            = 31

	ttypeIs   = 0
	ttypeSend = 1
)

// nego is the negotiation memory. Its whole job is RFC 1143
// quiescence: each answer kind (DO/DONT/WILL/WONT) is latched per
// option and never re-sent, and a frame repeating the peer's last
// direction is an ACK that gets no answer. Together these make an
// echo-style server (one that reflexively returns every frame as its
// own) converge to silence within two rounds — before this, it
// ping-ponged with us and pegged the CPU. Only the readLoop goroutine
// touches nego after Dial returns.
type nego struct {
	sentDo   map[byte]bool
	sentDont map[byte]bool
	sentWill map[byte]bool
	sentWont map[byte]bool
	peerWill map[byte]bool // peer's last announced direction: WILL
	peerDo   map[byte]bool // peer's last announced direction: DO
}

func newNego() nego {
	return nego{
		sentDo:   map[byte]bool{},
		sentDont: map[byte]bool{},
		sentWill: map[byte]bool{},
		sentWont: map[byte]bool{},
		peerWill: map[byte]bool{},
		peerDo:   map[byte]bool{},
	}
}

// Grace periods for prompt-driven auto-login (uniterm values; variables
// so tests can shorten them).
var (
	autoLoginPromptTimeout = 5 * time.Second
	autoLoginPromptPoll    = 50 * time.Millisecond
	autoLoginGraceTimeout  = 3 * time.Second
	// autoLoginPhaseWaitTimeout bounds the post-credential wait for a
	// shell prompt (0 disables the wait; tests shorten it).
	autoLoginPhaseWaitTimeout = 10 * time.Second
)

// Session is one live telnet connection bridged between the WebSocket
// terminal and the remote host. It implements io.Reader (the pump calls
// Read) and io.Writer.
type Session struct {
	mu      sync.Mutex
	conn    net.Conn
	closed  bool
	closeCh chan struct{}
	// Read-side buffer shared with the readLoop goroutine.
	pending []byte
	waiter  chan struct{}

	// IAC filter state spanning TCP read boundaries (issue #1415).
	iacPending []byte
	lastWasCR  bool

	nego     nego
	termType string
	loginBuf *loginOutputBuffer

	OnClose func(error)
}

// Dial opens the connection, enables OOB-inline urgent data, performs
// the proactive negotiation (DO SGA + WILL TTYPE + initial NAW; per
// protocol spec §3 we never offer BINARY) and starts the read loop.
// When user is non-empty, auto-login runs in the background with the
// given credentials.
func Dial(ctx context.Context, host string, port int, user, password, termType string, cols, rows int) (*Session, error) {
	if termType == "" {
		termType = "xterm-256color"
	}
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 30
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("telnet dial: %w", err)
	}
	enableOOBInline(conn)

	s := &Session{
		conn:     conn,
		termType: termType,
		closeCh:  make(chan struct{}),
		nego:     newNego(),
		loginBuf: newLoginOutputBuffer(),
	}

	// Proactive negotiation, one frame so old telnetds don't reply to
	// each piece separately (spec §4.2 step 4: no WILL BINARY). The
	// readLoop is not running yet, so writing conn directly is safe.
	s.nego.sentDo[optSuppressGoAhead] = true
	s.nego.sentWill[optTerminalType] = true
	startup := []byte{iac, do, optSuppressGoAhead, iac, will, optTerminalType}
	_, _ = conn.Write(startup)
	s.sendNAWS(cols, rows)

	go func() {
		defer safego.Recover("telnet_read_loop")
		s.readLoop()
	}()
	if user != "" {
		go func() {
			defer safego.Recover("telnet_auto_login")
			s.autoLogin(user, password)
		}()
	}
	return s, nil
}

func (s *Session) readLoop() {
	buf := make([]byte, 16384)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			filtered := s.filterIAC(buf[:n])
			if len(filtered) > 0 {
				s.loginBuf.Append(filtered)
				s.pushData(filtered)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
				s.pushData([]byte("\r\n\x1b[31m[telnet read error: " + err.Error() + "]\x1b[0m\r\n"))
			} else {
				s.pushData([]byte("\r\n\x1b[90m[Connection closed by remote host.]\x1b[0m\r\n"))
			}
			s.finish()
			return
		}
	}
}

// maxPendingBytes is the read buffer's ceiling (see pushData). Eight
// megabytes is a hundred frames of full-screen output at any sane baud
// rate, and far beyond what one stalled tab is owed.
const maxPendingBytes = 8 << 20

// pushData appends to the read buffer and wakes a blocked Read.
//
// maxPendingBytes bounds it: the reader is the WebSocket pump, and a tab
// that stops draining (a stalled write, a frozen page) must not let a
// chatty remote grow this without end while the pump runs out its write
// deadline. The freshest bytes survive because that is what a terminal
// shows: an operator who comes back wants the current screen, not the
// first megabyte the remote printed while nobody was looking.
func (s *Session) pushData(data []byte) {
	s.mu.Lock()
	s.pending = append(s.pending, data...)
	if over := len(s.pending) - maxPendingBytes; over > 0 {
		s.pending = append(s.pending[:0], s.pending[over:]...)
	}
	w := s.waiter
	s.waiter = nil
	s.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// Read implements io.Reader: it blocks until the read loop delivers
// data or the session is closed.
func (s *Session) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			s.mu.Unlock()
			return n, nil
		}
		if s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		w := make(chan struct{})
		s.waiter = w
		s.mu.Unlock()
		select {
		case <-w:
			// data arrived; loop back
		case <-s.closeCh:
			s.mu.Lock()
			hasData := len(s.pending) > 0
			s.closed = true
			s.mu.Unlock()
			if !hasData {
				return 0, io.EOF
			}
			// drain what is left first
		}
	}
}

// Write implements io.Writer.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return 0, errors.New("telnet session not connected")
	}
	return conn.Write(p)
}

// Resize sends a NAW window-change sub-negotiation.
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	s.sendNAWS(cols, rows)
	return nil
}

// Close tears the session down. Safe to call multiple times and from
// either the read loop or the API layer.
func (s *Session) Close() error {
	s.finish()
	return nil
}

func (s *Session) finish() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conn := s.conn
	s.conn = nil
	w := s.waiter
	s.waiter = nil
	s.mu.Unlock()
	close(s.closeCh)
	if w != nil {
		close(w)
	}
	if conn != nil {
		_ = conn.Close()
	}
	if s.OnClose != nil {
		s.OnClose(nil)
	}
}

// filterIAC strips telnet protocol bytes, answering negotiation as it
// arrives (port of uniterm's filterIAC, incl. the hold-back for
// sequences split across reads).
func (s *Session) filterIAC(data []byte) []byte {
	buf := data
	if len(s.iacPending) > 0 {
		buf = append(s.iacPending, data...)
		s.iacPending = nil
	}
	var out []byte
	i := 0
	for i < len(buf) {
		b := buf[i]
		if b != iac {
			if b == nul && s.lastWasCR {
				// CR NUL = escaped bare CR (NVT rule, orthogonal to
				// BINARY which this build never negotiates).
				s.lastWasCR = false
				i++
				continue
			}
			s.lastWasCR = b == '\r'
			out = append(out, b)
			i++
			continue
		}
		s.lastWasCR = false
		if i+1 >= len(buf) {
			s.holdPending(buf[i:])
			return out
		}
		switch buf[i+1] {
		case will, wont, do, dont:
			if i+2 >= len(buf) {
				s.holdPending(buf[i:])
				return out
			}
			s.handleNegotiation(buf[i+1], buf[i+2])
			i += 3
		case sb:
			end := subnegotiationEnd(buf, i)
			if end < 0 {
				s.holdPending(buf[i:])
				return out
			}
			s.handleSubNegotiation(buf[i:end])
			i = end
		case iac:
			out = append(out, iac) // escaped literal 0xFF
			i += 2
		default:
			// IAC DM (SYNCH) and friends: consume silently so the DM
			// byte never leaks into the data stream.
			i += 2
		}
	}
	return out
}

// maxIacPending bounds the hold-back for an IAC sequence split across
// reads. A real sub-negotiation is a handful of bytes (TTYPE, NEW-ENVIRON),
// so anything bigger is not a sequence waiting for its tail: it is a peer
// that opened IAC SB and never sends IAC SE. Held without limit that
// buffer grows forever and is recopied whole on every read.
const maxIacPending = 4096

func (s *Session) holdPending(tail []byte) {
	if len(tail) > maxIacPending {
		// Give up on the negotiation instead of on the connection. Those
		// bytes are protocol and never became output, so dropping them
		// costs the terminal nothing it could have shown; from here the
		// rest of the junk streams through as text, which is the honest
		// outcome for a broken telnetd.
		s.iacPending = nil
		return
	}
	s.iacPending = append([]byte(nil), tail...)
}

func subnegotiationEnd(buf []byte, start int) int {
	for j := start + 2; j+1 < len(buf); j++ {
		if buf[j] == iac && buf[j+1] == se {
			return j + 2
		}
	}
	return -1
}

// handleSubNegotiation answers TTYPE SEND with the configured type.
func (s *Session) handleSubNegotiation(data []byte) {
	if len(data) < 4 {
		return
	}
	opt := data[2]
	if opt == optTerminalType && data[3] == ttypeSend {
		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()
		if conn == nil {
			return
		}
		term := []byte(s.termType)
		msg := make([]byte, 0, 4+len(term))
		msg = append(msg, iac, sb, optTerminalType, ttypeIs)
		msg = append(msg, term...)
		msg = append(msg, iac, se)
		_, _ = conn.Write(msg)
	}
}

// accepts reports the options this build is willing to negotiate.
// Anything else is refused once, then remembered as refused so repeats
// never re-ignite the exchange (BINARY is deliberately NOT here: see
// protocol spec §3 "BINARY 不接").
func accepts(opt byte) bool {
	switch opt {
	case optSuppressGoAhead, optEcho, optTerminalType, optNAWS:
		return true
	}
	return false
}

func (s *Session) handleNegotiation(cmd byte, opt byte) {
	switch cmd {
	case will:
		if s.nego.peerWill[opt] {
			return // repeats of the peer's direction are ACKs
		}
		s.nego.peerWill[opt] = true
		if accepts(opt) {
			s.reply(do, opt)
		} else {
			s.reply(dont, opt)
		}
	case wont:
		if !s.nego.peerWill[opt] {
			return // teardown of a state never announced
		}
		s.nego.peerWill[opt] = false
		s.nego.peerDo[opt] = false
		s.reply(dont, opt) // RFC 1143 teardown confirmation
	case do:
		if s.nego.peerDo[opt] {
			return
		}
		s.nego.peerDo[opt] = true
		if accepts(opt) && opt != optEcho {
			// We never echo locally (spec §3 accept table).
			s.reply(will, opt)
		} else {
			s.reply(wont, opt)
		}
	case dont:
		if !s.nego.peerDo[opt] {
			return
		}
		s.nego.peerDo[opt] = false
		s.nego.peerWill[opt] = false
		s.reply(wont, opt)
	}
}

// reply sends one negotiation frame — at most once per (cmd, opt)
// pair, ever. That latch is the hard quiescence guarantee: even if
// the direction bookkeeping above is driven by a pathological peer,
// the outbound alphabet is finite (4 × option count) and the
// exchange always dies out.
func (s *Session) reply(cmd byte, opt byte) {
	n := &s.nego
	switch cmd {
	case do:
		if n.sentDo[opt] {
			return
		}
		n.sentDo[opt] = true
	case dont:
		if n.sentDont[opt] {
			return
		}
		n.sentDont[opt] = true
	case will:
		if n.sentWill[opt] {
			return
		}
		n.sentWill[opt] = true
	case wont:
		if n.sentWont[opt] {
			return
		}
		n.sentWont[opt] = true
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	_, _ = conn.Write([]byte{iac, cmd, opt})
}

func (s *Session) sendNAWS(cols, rows int) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	data := []byte{
		iac, sb, optNAWS,
		byte(cols >> 8), byte(cols & 0xff),
		byte(rows >> 8), byte(rows & 0xff),
		iac, se,
	}
	_, _ = conn.Write(data)
}

// ---- auto-login (uniterm port: sendAutoLogin / waitForLoginPrompt) ----

var (
	autoLoginUserMarkers     = []string{"login:", "username:"}
	autoLoginPasswordMarkers = []string{"assword:"}
)

func (s *Session) autoLogin(user, password string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		<-s.closeCh
		cancel()
	}()
	defer func() { close(done) }()

	if !s.waitForLoginPrompt(ctx, autoLoginUserMarkers) {
		// No recognizable prompt: send blind after the grace period —
		// the same net effect as uniterm's old fixed-delay schedule.
		if !sleepCtx(ctx, autoLoginGraceTimeout) {
			return
		}
	}
	s.writeCredential(ctx, user)

	if password == "" {
		return
	}
	if !s.waitForLoginPrompt(ctx, autoLoginPasswordMarkers) {
		if !sleepCtx(ctx, autoLoginGraceTimeout) {
			return
		}
	}
	s.writeCredential(ctx, password)
	s.waitForLoginPhaseOver(ctx)
}

// waitForLoginPhaseOver blocks until the accumulated output shows a
// shell prompt past the login phase (spec §3 post-login row: anything
// injected after auto-login must not land on a "login:" prompt). v1
// has no startupCommand field, so the gate simply times out quietly —
// the interactive session is unaffected because the WS pump reads
// from the socket, not from this goroutine.
func (s *Session) waitForLoginPhaseOver(ctx context.Context) {
	if autoLoginPhaseWaitTimeout <= 0 {
		return
	}
	deadline := time.Now().Add(autoLoginPhaseWaitTimeout)
	for time.Now().Before(deadline) {
		if s.loginPhaseOver() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(autoLoginPromptPoll):
		}
	}
}

func (s *Session) waitForLoginPrompt(ctx context.Context, markers []string) bool {
	deadline := time.Now().Add(autoLoginPromptTimeout)
	for time.Now().Before(deadline) {
		if s.loginPhaseHasMarker(markers) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(autoLoginPromptPoll):
		}
	}
	return false
}

func (s *Session) loginPhaseHasMarker(markers []string) bool {
	out := strings.ToLower(s.loginBuf.Text())
	for _, m := range markers {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}

func (s *Session) writeCredential(ctx context.Context, cred string) {
	if cred == "" {
		return
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil || ctx.Err() != nil {
		return
	}
	_, _ = conn.Write([]byte(cred + "\r\n"))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// loginPhaseOver reports whether the output has progressed past the
// login phase (uniterm port, kept for checklist parity): a shell
// prompt is visible with no login prompt at all, or a prompt appeared
// after the last login/password prompt.
func (s *Session) loginPhaseOver() bool {
	clean := stripANSI([]byte(s.loginBuf.Text()))
	lower := strings.ToLower(string(clean))
	last := -1
	for _, m := range []string{"login:", "username:", "assword:"} {
		if i := strings.LastIndex(lower, m); i > last {
			last = i
		}
	}
	if last < 0 {
		return endsWithShellPrompt(clean)
	}
	return endsWithShellPrompt(clean[last:])
}

func endsWithShellPrompt(text []byte) bool {
	t := strings.TrimRight(string(text), " \t\r\n\x00")
	if t == "" {
		return false
	}
	line := t
	if i := strings.LastIndexByte(t, '\n'); i >= 0 {
		line = t[i+1:]
	}
	line = strings.TrimRight(line, " \t")
	if line == "" {
		return false
	}
	switch line[len(line)-1] {
	case '$', '#', '>':
		return true
	}
	return false
}

// stripANSI removes escape sequences so prompt markers can be matched.
// Heuristic for auto-login timing only — not a terminal emulator.
func stripANSI(in []byte) []byte {
	out := make([]byte, 0, len(in))
	i := 0
	for i < len(in) {
		b := in[i]
		if b == 0x1b {
			if i+1 < len(in) && in[i+1] == '[' {
				j := i + 2
				for j < len(in) && !(in[j] >= 0x40 && in[j] <= 0x7E) {
					j++
				}
				if j < len(in) {
					i = j + 1
					continue
				}
				break // incomplete CSI at the tail: drop the rest
			}
			if i+1 < len(in) && in[i+1] == ']' {
				j := i + 2
				for j < len(in) && in[j] != 0x07 {
					if in[j] == 0x1b && j+1 < len(in) && in[j+1] == '\\' {
						j++
						break
					}
					j++
				}
				if j < len(in) {
					i = j + 1
					continue
				}
				break
			}
			i += 2
			continue
		}
		if b != 0x07 { // stray BEL dropped
			out = append(out, b)
		}
		i++
	}
	return out
}

// loginOutputBuffer is a capped ring of recent decoded output
// (uniterm's postLoginOutputBuffer, simplified to a 4 KiB tail).
type loginOutputBuffer struct {
	mu   sync.Mutex
	text []byte
}

const loginOutputLimit = 4096

func newLoginOutputBuffer() *loginOutputBuffer { return &loginOutputBuffer{} }

func (b *loginOutputBuffer) Append(data []byte) {
	if len(data) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text = append(b.text, data...)
	if len(b.text) > loginOutputLimit {
		b.text = b.text[len(b.text)-loginOutputLimit:]
	}
}

func (b *loginOutputBuffer) Text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.text)
}
