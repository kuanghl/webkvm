package zmodem

// The half of a session that does not care which end it plays.
//
// Both directions ride one terminal, and both owe the user the same
// promises: nothing is swallowed until the peer proves it speaks the
// protocol, the keystrokes typed during the arm window come back if it
// never does, a live session is bounded by a watchdog, and a cancel
// shouts the same CAN burst. Those rules are the reason a broken
// transfer never costs someone their shell, so they live in one place
// rather than in two copies that can drift.
//
// A direction contributes an endpoint: the parser callback, the answer
// to "does the protocol own the line right now", and the outcome to
// report.

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

func errf(format string, args ...any) error {
	return fmt.Errorf("zmodem: "+format, args...)
}

// Status classifies how a session ended.
type Status string

const (
	// StatusDone: the batch finished and both ends said goodbye.
	StatusDone Status = "done"
	// StatusError: a protocol, checksum or filesystem failure.
	StatusError Status = "error"
	// StatusCanceled: this side asked for it (job cancel, or the user).
	StatusCanceled Status = "canceled"
	// StatusDisarmed: no ZMODEM frame ever answered, so the terminal is
	// handed back exactly as it was. The usual cause is lrzsz missing on
	// the remote.
	StatusDisarmed Status = "disarmed"
)

// Progress is what the caller learns as bytes move.
//
// On a download the batch total is unknown until each ZFILE arrives, so
// Total covers the bytes announced so far and grows file by file. An
// upload knows every size upfront and fills Total exactly. Done is
// monotonic either way, which is all the queue's speed smoothing needs.
type Progress struct {
	Current   string
	Done      int64
	Total     int64
	FilesDone int
	FilesGot  int
}

// FileEvent reports one file that has moved.
type FileEvent struct {
	Name string
	Path string
	Size int64
	Mode os.FileMode
}

// Result is the outcome of a session.
type Result struct {
	Status Status
	// Files names what moved: on a download the host paths the bytes
	// landed in, on an upload the names the remote was handed. Neither is
	// ever a promise about the other end's filesystem.
	Files []string
	Err   error
	// Reason is machine-readable: "no-frame" (nothing answered, most
	// likely lrzsz missing), "frame-timeout", "bad-checksum", …
	Reason string
}

// Options configures a session. Every field is optional.
type Options struct {
	// Dir is the download landing directory: a per-job tree the caller
	// owns and removes. The upload direction reads its bytes from the
	// paths of its SendFile list and ignores this.
	Dir string
	// FrameTimeout bounds the silence between two protocol frames once
	// a transfer has started.
	FrameTimeout time.Duration
	// ArmTimeout bounds the wait for the very first frame after the
	// command was typed.
	ArmTimeout time.Duration
	// OnProgress is called from Feed's goroutine; it must not block.
	OnProgress func(Progress)
	// OnFile is called once per completed file.
	OnFile func(FileEvent)
	// Log receives human-readable diagnostics.
	Log func(string, ...any)
	// Cancelled is polled before each block: a canceled job stops the
	// session at the next subpacket.
	Cancelled func() bool
}

const (
	defaultFrameTimeout = 30 * time.Second
	defaultArmTimeout   = 10 * time.Second
	// maxKeyBuffer bounds what a user can type during the arm window.
	// Beyond it keystrokes are dropped rather than buffered without end.
	maxKeyBuffer = 4096
	// maxBadCRC is how often the same checksum failure is tolerated
	// before the session is abandoned: one retransmit request is normal,
	// a dozen means the two ends disagree about something fundamental.
	maxBadCRC = 3
	// maxDownloadFiles bounds the batch a remote `sz` may push into one
	// job. Its argument is a shell glob, so a mistyped one can mean tens
	// of thousands of names, and every accepted file costs a handle, a
	// path held in memory and an entry in the archive the browser is
	// handed at the end. A directory tree wider than this is not what the
	// panel asks for anyway; the sender is told why.
	maxDownloadFiles = 1024
)

// Session is what the terminal bridge needs from either direction: feed
// it the remote's bytes, hand it the user's keystrokes, and learn when
// the dialogue is over. Both the download end (Receiver) and the upload
// end (Sender) satisfy it through the core, so the WebSocket pump has no
// direction-specific branch — only the Start of each does.
type Session interface {
	Feed(p []byte) []byte
	Keys(p []byte) []byte
	Replay() []byte
	Cancel()
	Done() <-chan struct{}
	Result() Result
	Phase() string
	Swallowing() bool
}

// endpoint is what a direction contributes to the shared machinery. Its
// methods run with the core's lock already held, exactly like the parser
// callback they are reached from.
type endpoint interface {
	// event handles one parser event.
	event(e Event)
	// ownsLine reports whether the terminal's output currently belongs to
	// the protocol rather than to the screen.
	ownsLine() bool
	// resultLocked builds the outcome once the session state is final.
	resultLocked(status Status, reason string, err error) Result
	// cleanupLocked releases whatever direction-specific resource the
	// session still holds (an open file, for a download).
	cleanupLocked()
}

// core is the shared state of one terminal ZMODEM session.
type core struct {
	pty io.Writer
	opt Options
	end endpoint

	mu    sync.Mutex
	phase int
	armed bool

	// frameTimer disarms or fails the session when the stream goes
	// quiet; gen makes a stale timer harmless.
	frameTimer *time.Timer
	gen        uint64

	// keys are the keystrokes held during the arm window, replayed if
	// nothing ever answers.
	keys []byte
	// endedInArm records that the session was still waiting for the very
	// first frame when it closed, which is what makes those keys owed
	// back to the shell. finishLocked sets it from the phase before the
	// phase is rewritten by the outcome.
	endedInArm bool

	parser *Parser

	done   chan struct{}
	result Result
	closed bool
}

func newCore(pty io.Writer, opt Options, end endpoint) *core {
	if opt.FrameTimeout == 0 {
		opt.FrameTimeout = defaultFrameTimeout
	}
	if opt.ArmTimeout == 0 {
		opt.ArmTimeout = defaultArmTimeout
	}
	c := &core{pty: pty, opt: opt, end: end, done: make(chan struct{})}
	c.parser = NewParser(end.event)
	return c
}

// Done is closed when the session reaches a terminal state.
func (c *core) Done() <-chan struct{} { return c.done }

// Result returns the terminal outcome (zero value while running).
func (c *core) Result() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result
}

// Phase names the current protocol state, for logs and tests.
func (c *core) Phase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return phaseName(c.phase)
}

// Swallowing reports whether the terminal's output currently belongs to
// the protocol rather than to the screen.
func (c *core) Swallowing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.end.ownsLine()
}

// Feed takes the bytes the remote produced and returns the ones that
// must still reach the browser terminal. An empty result means the chunk
// was protocol, not output.
func (c *core) Feed(p []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return p
	}
	if c.closed {
		return p
	}
	out := c.parser.Feed(p)
	if err := c.parser.Err; err != nil && !c.closed {
		if c.phase == phaseArmed {
			// Nothing has answered yet, so this was text that merely
			// looked like a frame (a hexdump on screen is enough). Keep
			// the shell intact and stay armed.
			c.parser.ClearErr()
		} else {
			c.finishLocked(StatusError, "protocol", err)
			return out
		}
	}
	if c.closed {
		return out
	}
	// Once a real frame has arrived the watchdog changes job: it now
	// bounds the silence between the frames of a live transfer. Until
	// then the arm deadline stays absolute, so a chatty shell can
	// neither postpone it nor hold the user's keystrokes forever.
	if c.phase != phaseArmed {
		c.armTimerLocked(c.opt.FrameTimeout)
	}
	return out
}

// Keys takes what the user typed and returns the bytes the pump must
// send to the remote.
//
// During the arm window keystrokes are held back rather than typed into
// a half-started transfer; if nothing ever answers they are replayed.
// Once a transfer is under way they are dropped: the remote is not
// reading a shell any more, it is reading a protocol.
func (c *core) Keys(p []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed || c.closed {
		return p
	}
	if c.phase == phaseArmed {
		if len(c.keys)+len(p) <= maxKeyBuffer {
			c.keys = append(c.keys, p...)
		}
		return nil
	}
	if c.end.ownsLine() {
		return nil
	}
	return p
}

// Replay returns the keystrokes held during the arm window and clears
// them. They come back only if that window was still open when the
// session ended: once a frame answers, the line belongs to the protocol,
// and typing the old keys at it afterwards would surprise the user twice
// over. Anything that ends the session inside the window — the arm
// timeout, a cancel from the panel, a failed write — gets the same
// answer, which is why the test is on how the session ended and not on
// which status name it ended with.
func (c *core) Replay() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.keys
	c.keys = nil
	if !c.endedInArm {
		return nil
	}
	return k
}

// Cancel ends the session now, telling the peer to stop. Whatever has
// moved stays for the caller to remove.
func (c *core) Cancel() { c.finish(StatusCanceled, "canceled", nil) }

func (c *core) finish(status Status, reason string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(status, reason, err)
}

func (c *core) finishLocked(status Status, reason string, err error) {
	if c.closed {
		return
	}
	c.end.cleanupLocked()
	if c.frameTimer != nil {
		c.frameTimer.Stop()
		c.frameTimer = nil
	}
	c.gen++
	// A session that never reached the protocol must leave the shell
	// alone: the CAN burst is how a live transfer is told to stop, and
	// typing it into a prompt would only scare the user and eat the
	// line they were editing.
	endedInArm := c.phase == phaseArmed
	if status != StatusDone && !endedInArm && c.phase != phaseIdle {
		c.sendAbortSequenceLocked()
	}
	c.phase = phaseForStatus(status)
	c.endedInArm = endedInArm
	c.closed = true
	c.result = c.end.resultLocked(status, reason, err)
	close(c.done)
}

func phaseForStatus(s Status) int {
	switch s {
	case StatusDone:
		return phaseDone
	case StatusCanceled:
		return phaseCanceled
	case StatusDisarmed:
		return phaseIdle
	}
	return phaseFailed
}

// sendAbortSequenceLocked shouts the ZMODEM cancel sequence: ten CAN and
// ten backspace, which is what lrzsz and every terminal emulator send.
func (c *core) sendAbortSequenceLocked() {
	seq := make([]byte, 0, 20)
	for i := 0; i < 10; i++ {
		seq = append(seq, ZDLE)
	}
	for i := 0; i < 10; i++ {
		seq = append(seq, 0x08)
	}
	_ = c.writeFrame(seq)
}

// armLocked types the point of no return: the command has been handed to
// the pty, so the watchdog for the very first frame starts here.
func (c *core) armLocked() {
	c.armed = true
	c.phase = phaseArmed
	c.armTimerLocked(c.opt.ArmTimeout)
}

// armTimerLocked (re)sets the watchdog. Called for the arm window and
// then once per chunk of live protocol, so a steady stream never times
// out.
func (c *core) armTimerLocked(d time.Duration) {
	if c.frameTimer != nil {
		c.frameTimer.Stop()
		c.frameTimer = nil
	}
	c.gen++
	gen := c.gen
	inArmWindow := c.phase == phaseArmed
	c.frameTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed || gen != c.gen {
			return
		}
		if inArmWindow {
			// Nothing answered. Give the terminal back exactly as it
			// was, keystrokes included.
			c.finishLocked(StatusDisarmed, "no-frame",
				errf("no ZMODEM answer within %s (is lrzsz installed on the remote?)", d))
			return
		}
		c.finishLocked(StatusError, "frame-timeout", errf("no ZMODEM frame within %s", d))
	})
}

// writeFrame puts one complete frame on the wire. The loop matters: a
// pty or a serial port is free to accept part of a buffer, and half a
// ZDATA frame is a checksum failure the peer will NAK forever. The
// session's own writes are always whole frames built in one slice, so a
// partial write resumes where it stopped rather than interleaving.
func (c *core) writeFrame(b []byte) error {
	for len(b) > 0 {
		n, err := c.pty.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return errf("terminal accepted no bytes")
		}
		b = b[n:]
	}
	return nil
}

// writeLock writes while the caller holds the core's lock. Small
// handshake frames go this way; a bulk frame is written by the sender's
// worker without it, so a 9600-baud line cannot stall the terminal pump.
func (c *core) writeLock(b []byte) {
	if err := c.writeFrame(b); err != nil {
		c.logf("write to pty failed: %v", err)
	}
}

func (c *core) logf(format string, args ...any) {
	if c.opt.Log != nil {
		c.opt.Log("zmodem "+format, args...)
	}
}
