package zmodem

// The upload direction: this end plays `sz`, the remote runs `rz` and
// takes the bytes we push at it.
//
// The dialogue, and every choice in it, was read off a real lrzsz 0.12.21
// rather than off a description:
//
//	rz typed     →  ZRINIT(flags)      the receiver's capabilities
//	ZFILE+meta   →  ZRPOS(offset)     where it wants the bytes from
//	ZDATA(ZCRCW) →  ZACK(offset)      one block, then its answer
//	  … until the file is through, then ZEOF …
//	ZRINIT       →  the next ZFILE, or ZFIN and goodbye
//
// Two decisions are deliberate. The batch stops and waits on every block
// instead of honouring the window rz advertises in ZF1/ZF2: a NAK on a
// 1 KiB block costs one round trip, while a windowful of blocks lost on a
// phone line costs the whole window, and stop-and-wait is what lrzsz
// itself falls back to on the slowest links. And every block ends with
// ZCRCW, never ZCRCE, so the receiver has acknowledged the last byte
// before ZEOF arrives and the two ends cannot disagree about where the
// file ended.
//
// File reading and frame encoding happen in one worker goroutine, never
// in Feed: the terminal pump must stay responsive while a megabyte is
// pushed down a serial line, which can take minutes. Feed therefore only
// moves the state machine and wakes the worker; the worker renders and
// writes.

import (
	"io"
	"os"
	"sync/atomic"
	"time"

	"webkvm/internal/safego"
)

const (
	// sendBlock is the payload of one ZDATA frame — lrzsz's own default.
	// With the stop-and-wait dialogue above it only decides how many
	// round trips a batch costs.
	sendBlock = 1024
	// maxSendSize is the largest file this dialogue can move: ZMODEM
	// positions are 32-bit, so a bigger file could never be resumed
	// correctly even though the staging directory would take it.
	maxSendSize = uint64(^uint32(0))
	// freeBlocks is the answer to a receiver that asks how much room
	// there is: the largest count the format carries. This end cannot see
	// the remote's filesystem, and "assume there is space" is exactly
	// what sz itself answers when statfs fails.
	freeBlocks = "4294967295"
)

// Work owed to the worker, one bit per kind. Bits rather than a single
// slot because two instructions can be owed in the same instant — a ZCRC
// that lands between a block and the ZACK answering it — and with one
// slot the later instruction erased the earlier: batches ended with the
// last block acknowledged and no ZEOF ever written. The same kind owed
// twice is the same work once, because what goes out next is derived from
// `off`, never counted.
const (
	taskOffer = 1 << iota
	taskCRC
	taskBlock
	taskEOF
)

// SendFile is one staged file: what the browser handed to the webkvm
// host, and the name the remote is asked to save it under. The remote's
// `rz -E` resolves a name clash by renaming whatever was already there,
// so these names need no disambiguation of their own.
type SendFile struct {
	Name    string
	Path    string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
}

// Sender is the ZMODEM sender end of one terminal session.
type Sender struct {
	*core

	files []SendFile
	total int64

	// Written by Feed, read by the worker. Each is a statement about the
	// protocol, not about a file, so the worker never has to take the
	// lock to know what to send next.
	pending atomic.Uint32 // the work owed, one bit per kind
	off     atomic.Uint32 // offset of the block to render
	next    atomic.Uint32 // offset once that block is through
	crcAt   atomic.Uint32 // how many bytes a ZCRC asked to have summed
	idx     atomic.Int32  // the file the dialogue is pointing at
	acked   atomic.Uint64 // bytes the receiver has confirmed
	crc32   atomic.Bool   // the receiver's ZRINIT spoke CANFC32
	escCtl  atomic.Bool   // the receiver asked for every control byte escaped
	again   atomic.Bool   // the block at `off` goes out again as it was

	wake      chan struct{}
	lastFrame []byte // worker-owned: the exact bytes to put on the wire again
	lastOff   uint32 // worker-owned: the offset those bytes cover
	cur       *os.File
	buf       []byte // worker-owned read buffer

	// names and badCRC belong to the state machine, under the core's lock.
	names  []string
	badCRC int
}

// NewSender prepares an upload of the given files. The list must not be
// empty, and every file must be staged on this host and fit in a 32-bit
// position.
func NewSender(pty io.Writer, opt Options, files []SendFile) (*Sender, error) {
	if len(files) == 0 {
		return nil, errf("nothing to send")
	}
	s := &Sender{files: append([]SendFile(nil), files...), buf: make([]byte, sendBlock)}
	for _, f := range s.files {
		if f.Path == "" {
			return nil, errf("%s has no staged path", f.Name)
		}
		if f.Size < 0 || uint64(f.Size) > maxSendSize {
			return nil, errf("%s is %d bytes: ZMODEM cannot address past %d", f.Name, f.Size, maxSendSize)
		}
		s.total += f.Size
	}
	s.core = newCore(pty, opt, s)
	s.wake = make(chan struct{}, 1)
	return s, nil
}

// rzCommand is what gets typed into the terminal. -q keeps rz's own
// chatter off the screen; -E decides what happens to a file that is
// already there — rename the old one, never ask — because a question
// nobody is watching is a transfer that hangs forever. -b is left out on
// purpose: the handshake stays in hex, which is what both ends of this
// build already speak, and rz moves to binary headers by itself if it
// prefers them. -e is not ours to set: that flag tells *us* to escape
// every control byte, and the receiver says the same thing in its ZSINIT.
func rzCommand() string { return "rz -q -E\r" }

// Start arms the session and types the command. Like the receiver's,
// nothing is swallowed until a frame proves the remote speaks ZMODEM.
func (s *Sender) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed {
		return errf("a zmodem session is already running on this terminal")
	}
	s.armLocked()
	go func() {
		defer safego.Recover("zmodem_sender")
		s.work()
	}()
	return s.writeFrame([]byte(rzCommand()))
}

// ---- the state machine (runs with the core's lock held) ----

func (s *Sender) ownsLine() bool {
	switch s.phase {
	case phaseSendGreet, phaseSendPos, phaseSendData, phaseSendNext:
		return true
	}
	return false
}

func (s *Sender) resultLocked(status Status, reason string, err error) Result {
	return Result{
		Status: status, Reason: reason, Err: err,
		Files: append([]string(nil), s.names...),
	}
}

// cleanupLocked has nothing to release. The worker owns the file handle
// and the cached frame, and finishLocked runs on Feed's goroutine —
// closing a descriptor or clearing a frame the worker may be using is a
// use-after-free at best, and a data race the `-race` build is entitled
// to fail the suite over at worst.
func (s *Sender) cleanupLocked() {}

func (s *Sender) event(e Event) {
	if s.closed {
		return
	}
	switch e.Kind {
	case evHeader:
		s.header(e.Header)
	case evSub:
		s.sub(e)
	case evAbort:
		if s.phase == phaseArmed {
			return // shell text with five 0x18 bytes is not a canceled transfer
		}
		s.finishLocked(StatusError, "peer-abort", errf("the receiver canceled the transfer"))
	}
}

func (s *Sender) header(h Header) {
	switch h.Type {
	case TypeZRQINIT:
		if s.phase == phaseArmed || s.phase == phaseSendGreet {
			// A receiver probing before being asked. Answer the courtesy
			// so it stops probing; its ZRINIT is the frame that starts
			// the batch.
			s.writeLock(hexHeader(TypeZRINIT, [4]byte{0, 0, 0, initWithFlag}))
			s.phase = phaseSendGreet
		}
	case TypeZRINIT:
		switch s.phase {
		case phaseArmed, phaseSendGreet:
			s.crc32.Store(h.F0()&flagCANFC32 != 0)
			s.logf("receiver flags %02x, crc32=%v", h.F0(), s.crc32.Load())
			s.beginFile()
		case phaseSendNext:
			// It took the file that just ended: another one, or goodbye.
			s.retireFile(false)
			if !s.beginFile() {
				s.writeLock(hexHeader(TypeZFIN, [4]byte{}))
				s.finishLocked(StatusDone, "", nil)
			}
		default:
			s.logf("ignoring ZRINIT while %s", phaseName(s.phase))
		}
	case TypeZRPOS:
		if s.phase != phaseSendPos && s.phase != phaseSendData && s.phase != phaseSendNext {
			s.logf("ignoring ZRPOS while %s", phaseName(s.phase))
			return
		}
		s.seekTo(h.Pos())
	case TypeZACK:
		if s.phase != phaseSendData {
			// A keepalive that was on its way while we were still
			// offering the file is late, not wrong.
			s.logf("ignoring ZACK while %s", phaseName(s.phase))
			return
		}
		// ZACK carries the position the receiver actually holds, so the
		// position decides — not the mere arrival of a frame. Blindly
		// advancing on any ZACK turned a duplicate (or an answer to an
		// older block that was still in the line's buffer) into a skipped
		// block: the file arrived with a hole in it and every CRC after
		// that was recomputed over the wrong bytes.
		p := int64(h.Pos())
		off, next := int64(s.off.Load()), int64(s.next.Load())
		switch {
		case p == next:
			s.advance()
		case p == off:
			// Nothing of the block in flight reached it: same as a NAK.
			s.logf("ZACK at %d, block in flight starts there: resending", p)
			s.resend()
		case p > off && p < next:
			s.logf("receiver acknowledged %d, we sent %d: resuming there", p, next)
			s.seekTo(uint32(p))
		default:
			// Behind the block in flight (a stale answer) or ahead of
			// everything we sent (impossible): either way it cannot move
			// us. The stall watchdog is what recovers a peer that really
			// is stuck.
			s.logf("ignoring ZACK at %d (block spans %d..%d)", p, off, next)
		}
	case TypeZNAK:
		if s.phase != phaseSendData {
			s.logf("ignoring ZNAK while %s", phaseName(s.phase))
			return
		}
		s.resend()
	case TypeZSKIP:
		// The receiver declined this file — it has it already, or it ran
		// out of room. That is not an error, and the batch continues.
		if s.phase == phaseSendPos || s.phase == phaseSendData || s.phase == phaseSendNext {
			s.logf("receiver skipped %s", s.current().Name)
			s.retireFile(true)
			if !s.beginFile() {
				s.writeLock(hexHeader(TypeZFIN, [4]byte{}))
				s.finishLocked(StatusDone, "", nil)
			}
		}
	case TypeZCRC:
		s.crcAt.Store(h.Pos())
		s.kick(taskCRC)
	case TypeZSINIT:
		s.writeLock(hexHeader(TypeZACK, posBytes(0)))
	case TypeZFREECNT:
		s.writeLock(subpkgFrame(hexHeader(TypeZFREECNT, [4]byte{}),
			append([]byte(freeBlocks), 0), ZCRCW, false, false))
	case TypeZFIN:
		// Either the goodbye that answers ours, or a receiver that is
		// finished with this batch. Either way the terminal is ours again.
		s.finishLocked(StatusDone, "", nil)
	case TypeZABORT:
		s.finishLocked(StatusError, "peer-abort", errf("the receiver aborted the batch"))
	case TypeZFERR:
		s.finishLocked(StatusError, "remote-error", errf("the receiver refused the transfer"))
	case TypeZCAN:
		s.finishLocked(StatusError, "peer-abort", errf("the receiver canceled"))
	default:
		s.logf("ignoring frame %02x in %s", h.Type, phaseName(s.phase))
	}
}

func (s *Sender) sub(e Event) {
	if e.Header.Type != TypeZSINIT {
		return
	}
	// An attention sequence is a string of control characters the
	// receiver wants typed at it before the batch, to break out of
	// whatever the line is stuck in. There is no way to send one that
	// does not also land in the user's terminal, so this build refuses
	// rather than pretending. An empty one only means "escape everything".
	if len(e.Data) > 1 {
		s.finishLocked(StatusError, "attention-sequence",
			errf("the receiver asked for an attention sequence this build cannot honour"))
		return
	}
	s.escCtl.Store(true)
}

// current is the file the dialogue is working on; only called while the
// batch has one.
func (s *Sender) current() SendFile { return s.files[int(s.idx.Load())] }

// beginFile offers the file the batch is pointing at, or reports that
// there is none left.
func (s *Sender) beginFile() bool {
	i := int(s.idx.Load())
	if i >= len(s.files) {
		return false
	}
	s.off.Store(0)
	s.next.Store(0)
	s.badCRC = 0
	s.phase = phaseSendPos
	s.kick(taskOffer)
	s.progress()
	return true
}

// retireFile records that a file has left the dialogue, skipped or
// delivered, and moves the batch on. Only the receiver's own ZRINIT
// after our ZEOF counts as delivered: until then the bytes are still in
// its hands.
func (s *Sender) retireFile(skipped bool) {
	f := s.current()
	s.names = append(s.names, f.Name)
	if skipped {
		s.logf("skipped %s", f.Name)
	}
	s.idx.Add(1)
	// Everything the batch has moved past counts, whether it arrived,
	// was declined or the receiver already had it: a queue bar that
	// stalls is worse than one that finishes early.
	if d := s.deliveredBase(); d > s.acked.Load() {
		s.acked.Store(d)
	}
	s.progress()
}

// seekTo restarts the current file wherever the receiver says its copy
// ends. That is a real resume for free, because the bytes are staged on
// this host and every offset is still readable.
func (s *Sender) seekTo(p uint32) {
	f := s.current()
	if uint64(p) > uint64(f.Size) {
		s.finishLocked(StatusError, "bad-offset",
			errf("receiver asked to resume %s at %d, it has %d bytes", f.Name, p, f.Size))
		return
	}
	s.off.Store(p)
	s.next.Store(p)
	s.sendOnward()
}

// advance is what an acknowledged block means: the receiver has the
// bytes up to `next`, so send from there or close the file out.
func (s *Sender) advance() {
	s.acked.Store(s.deliveredBase() + uint64(s.next.Load()))
	s.off.Store(s.next.Load())
	s.badCRC = 0
	s.sendOnward()
}

// deliveredBase is the size of every file the batch has moved past. The
// queue's counter has to be the whole batch, not the current file, or
// the bar walks back to zero at every name.
func (s *Sender) deliveredBase() uint64 {
	var n uint64
	for _, f := range s.files[:int(s.idx.Load())] {
		n += uint64(f.Size)
	}
	return n
}

func (s *Sender) sendOnward() {
	f := s.current()
	if uint64(s.next.Load()) >= uint64(f.Size) {
		s.phase = phaseSendNext
		s.kick(taskEOF)
		return
	}
	s.phase = phaseSendData
	s.kick(taskBlock)
}

func (s *Sender) resend() {
	s.badCRC++
	if s.badCRC > maxBadCRC {
		s.finishLocked(StatusError, "bad-checksum",
			errf("%s: %d blocks refused to arrive cleanly", s.current().Name, s.badCRC))
		return
	}
	// The block in flight is exactly what the receiver rejected, so it
	// goes out again byte for byte instead of being rebuilt from disk.
	s.again.Store(true)
	s.kick(taskBlock)
}

func (s *Sender) progress() {
	if s.opt.OnProgress == nil {
		return
	}
	name := ""
	if i := int(s.idx.Load()); i < len(s.files) {
		name = s.files[i].Name
	}
	s.opt.OnProgress(Progress{
		Current:   name,
		Done:      int64(s.acked.Load()),
		Total:     s.total,
		FilesDone: len(s.names),
		FilesGot:  len(s.files),
	})
}

// kick adds work to the ledger and makes sure the worker looks at it
// again. It never blocks: Feed holds the core's lock while calling it, and
// a terminal whose bytes are owed to a stalled goroutine is a terminal
// nobody can type in.
func (s *Sender) kick(task uint32) {
	for {
		p := s.pending.Load()
		if p&task == task {
			break
		}
		if s.pending.CompareAndSwap(p, p|task) {
			break
		}
	}
	select {
	case s.wake <- struct{}{}:
	default:
		// A token is already queued, and the worker re-reads the ledger
		// after taking it, so this instruction cannot be missed.
	}
}

// take clears and returns the oldest owed kind of work, or 0 when the
// ledger is empty. Popping the lowest bit keeps the dialogue in order:
// offer, then checksum, then block, then the frame that closes the file.
func (s *Sender) take() uint32 {
	for {
		p := s.pending.Load()
		if p == 0 {
			return 0
		}
		bit := p &^ (p - 1)
		if s.pending.CompareAndSwap(p, p^bit) {
			return bit
		}
	}
}

// ---- the worker: render and write, never decide ----

func (s *Sender) work() {
	defer func() {
		if s.cur != nil {
			_ = s.cur.Close()
			s.cur = nil
		}
	}()
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			t := s.take()
			if t == 0 {
				break
			}
			if s.opt.Cancelled != nil && s.opt.Cancelled() {
				s.finish(StatusCanceled, "canceled", nil)
				return
			}
			select {
			case <-s.done:
				return // a frame on the wire already settled the batch
			default:
			}
			switch t {
			case taskOffer:
				s.doOffer()
			case taskCRC:
				s.doCRC()
			case taskBlock:
				s.doBlock()
			case taskEOF:
				s.sendEOF()
			}
		}
	}
}

func (s *Sender) doOffer() {
	f := s.current()
	st, err := os.Stat(f.Path)
	if err != nil {
		s.finish(StatusError, "open-failed", errf("read %s: %w", f.Path, err))
		return
	}
	if st.Size() != f.Size {
		s.finish(StatusError, "staged-changed",
			errf("%s changed while staging: %d bytes, was %d", f.Name, st.Size(), f.Size))
		return
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		s.finish(StatusError, "open-failed", errf("read %s: %w", f.Path, err))
		return
	}
	if s.cur != nil {
		_ = s.cur.Close()
	}
	s.cur = fh
	// The cached frame belongs to the file that just ended. Left over, a
	// ZNAK on the first block of the next file could replay bytes from the
	// previous one — same offset, different file, silently wrong.
	s.lastFrame, s.lastOff = nil, 0
	s.again.Store(false)
	s.writeNow(zfileFrame(fileMetaPacket(f.Name, f.Size, f.Mode, f.ModTime)))
}

// sendEOF closes the current file out and retires the cached block with
// it: an offset past the end of the file is nothing to replay.
func (s *Sender) sendEOF() {
	s.lastFrame, s.lastOff = nil, 0
	s.again.Store(false)
	s.writeNow(hexPosHeader(TypeZEOF, s.next.Load()))
}

func (s *Sender) doBlock() {
	f := s.current()
	off := s.off.Load()
	// A rejected block goes out again exactly as it was encoded: the
	// receiver is asking for the bytes it could not check, and rebuilding
	// from disk would let a staged file that changed underneath move them
	// without anyone noticing.
	if s.again.Swap(false) && s.lastFrame != nil && s.lastOff == off {
		s.writeNow(s.lastFrame)
		return
	}
	if uint64(off) >= uint64(f.Size) {
		s.sendEOF()
		return
	}
	if s.cur == nil {
		// A resume request after the file was closed: reopen rather than
		// fail a batch over a bookkeeping detail.
		h, err := os.Open(f.Path)
		if err != nil {
			s.finish(StatusError, "open-failed", errf("read %s: %w", f.Path, err))
			return
		}
		s.cur = h
	}
	n, err := s.cur.ReadAt(s.buf[:sendBlock], int64(off))
	if n <= 0 {
		// The guard above already rules out a clean end of file, so no
		// bytes here means the staged copy is shorter than it was offered.
		s.finish(StatusError, "short-file",
			errf("%s: no bytes at %d of %d", f.Name, off, f.Size))
		return
	}
	frame := dataFrame(off, s.buf[:n], ZCRCW, s.crc32.Load(), s.escCtl.Load())
	s.lastFrame, s.lastOff = frame, off
	s.next.Store(off + uint32(n))
	s.writeNow(frame)
	if err != nil {
		// Short read at the end of the file: the block went out, and the
		// ZACK that answers it will ask for ZEOF.
		s.logf("read %s: %v", f.Name, err)
	}
}

// doCRC answers the receiver's "tell me what you have up to here". The
// bytes are on this host, so the answer is exact rather than a guess —
// the one place an upload is more comfortable than a download.
func (s *Sender) doCRC() {
	f := s.current()
	want := uint64(s.crcAt.Load())
	if want > uint64(f.Size) {
		want = uint64(f.Size)
	}
	h, err := os.Open(f.Path)
	if err != nil {
		s.finish(StatusError, "open-failed", errf("read %s: %w", f.Path, err))
		return
	}
	defer h.Close()
	buf := make([]byte, sendBlock)
	var c16 uint16
	var c32 uint32
	for got := uint64(0); got < want; {
		n := len(buf)
		if want-got < uint64(n) {
			n = int(want - got)
		}
		read, err := h.ReadAt(buf[:n], int64(got))
		if read <= 0 && err != nil {
			s.finish(StatusError, "read-failed", errf("read %s: %w", f.Path, err))
			return
		}
		c16 = crc16Update(c16, buf[:read])
		c32 = crc32Update(c32, buf[:read])
		got += uint64(read)
	}
	sum := uint32(c16)
	if s.crc32.Load() {
		sum = c32
	}
	s.writeNow(hexPosHeader(TypeZCRC, sum))
}

// writeNow puts a finished frame on the wire with the core's lock
// deliberately not held, so a slow line cannot stall the terminal pump.
func (s *Sender) writeNow(frame []byte) {
	if err := s.writeFrame(frame); err != nil {
		s.finish(StatusError, "write-failed", errf("terminal write: %w", err))
	}
}
