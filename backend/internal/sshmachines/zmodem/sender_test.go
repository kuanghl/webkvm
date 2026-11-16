package zmodem

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- staged files ----

type stageFile struct {
	name string
	body []byte
	mode os.FileMode
}

// stageFiles writes a batch the way the upload staging directory holds
// it: the flat name the remote is asked to save it under, and the bytes
// at a path only this host ever sees.
func stageFiles(t *testing.T, list ...stageFile) []SendFile {
	t.Helper()
	dir := t.TempDir()
	out := make([]SendFile, 0, len(list))
	for _, f := range list {
		p := filepath.Join(dir, f.name)
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(p, f.body, 0o600); err != nil {
			t.Fatal(err)
		}
		if mode != 0o600 {
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, SendFile{
			Name: f.name, Path: p, Size: int64(len(f.body)),
			Mode: mode, ModTime: time.Unix(1700000000, 0),
		})
	}
	return out
}

type sendOpts struct {
	arm      time.Duration
	frame    time.Duration
	progress func(Progress)
	file     func(FileEvent)
	cancel   func() bool
}

// newSender wires an upload to a mirror, exactly as newReceiver does a
// download. The cleanup cancel is what keeps a test that fails halfway
// from leaving a worker writing into a buffer nobody reads.
func newSender(t *testing.T, files []SendFile, opt sendOpts) (*Sender, *mirror) {
	t.Helper()
	if opt.arm == 0 {
		opt.arm = 5 * time.Second
	}
	if opt.frame == 0 {
		opt.frame = 5 * time.Second
	}
	m := &mirror{}
	s, err := NewSender(m, Options{
		ArmTimeout:   opt.arm,
		FrameTimeout: opt.frame,
		OnProgress:   opt.progress,
		OnFile:       opt.file,
		Cancelled:    opt.cancel,
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Cancel()
		}
	})
	return s, m
}

func sendResult(t *testing.T, s *Sender) Result {
	t.Helper()
	select {
	case <-s.Done():
		return s.Result()
	case <-time.After(3 * time.Second):
		t.Fatalf("session did not finish (phase %s)", s.Phase())
		return Result{}
	}
}

func senderFinished(s *Sender) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

// ---- the remote's half ----

// rzPeer plays the receiver: it reads the frames the sender has written
// into the mirror, answers each one the way lrzsz's rz does, and
// assembles what it claims to have taken so a test can compare it
// against the staged bytes.
//
// It is driven by the replies rather than by a script of expected frames
// because that is what a real receiver does: it reacts. It therefore
// never has to know how a batch is chopped into blocks.
type rzPeer struct {
	t *testing.T
	s *Sender
	m *mirror

	crc32 bool
	// byteFeed hands every frame to Feed one byte at a time: the framing
	// must survive whatever a read returns.
	byteFeed bool
	// resumeAt is the offset answered to a ZFILE, so a test can ask for a
	// tail. skipOnce declines the next file, nakOnce rejects the next
	// block and nakAll rejects every one of them.
	resumeAt uint32
	skipOnce bool
	nakOnce  bool
	nakAll   bool
	// crcAsk is the prefix a receiver asks to have summed, in place of
	// the acknowledgement of the block that completed it.
	crcAsk uint32
	// zackEarly is the keepalive that can be on its way while a file is
	// still being offered. pauseAt holds the acknowledgement of that
	// many-th block back until the test lets it go, which is how a test
	// cancels a job mid-batch.
	zackEarly bool
	pauseAt   int
	paused    chan struct{}
	resume    chan struct{}

	i       int    // reply events answered
	cur     string // the file the blocks belong to
	seen    []string
	got     map[string][]byte
	offered map[string]int64 // the length each ZFILE announced
	blocks  int
}

func newRZPeer(t *testing.T, s *Sender, m *mirror, crc32On bool) *rzPeer {
	return &rzPeer{t: t, s: s, m: m, crc32: crc32On,
		got: map[string][]byte{}, offered: map[string]int64{}}
}

func (p *rzPeer) greeting() []byte {
	return initFrame(p.crc32)
}

func (p *rzPeer) feed(b []byte) {
	if p.byteFeed {
		for i := 0; i < len(b); i++ {
			p.s.Feed(b[i : i+1])
		}
		return
	}
	p.s.Feed(b)
}

// step answers every reply that has arrived since it last ran.
func (p *rzPeer) step() {
	ev := p.m.replies(p.t)
	for p.i < len(ev) {
		e := ev[p.i]
		p.i++
		if e.Kind == evSub {
			p.sub(e)
			continue
		}
		if e.Kind != evHeader {
			continue
		}
		switch e.Header.Type {
		case TypeZFILE, TypeZDATA:
			// Their substance is in the subpacket.
		case TypeZEOF:
			p.feed(p.greeting())
		case TypeZFIN, TypeZACK, TypeZCRC:
			// Our own goodbye, our own answer to a ZSINIT, our own
			// checksum reply: nothing to do but let the dialogue move.
		default:
			p.t.Errorf("the sender wrote an unexpected frame %02x", e.Header.Type)
		}
	}
}

func (p *rzPeer) sub(e Event) {
	switch e.Header.Type {
	case TypeZFILE:
		o, _, err := parseOffer(e.Data)
		if err != nil {
			p.t.Fatalf("the descriptor we sent cannot be read back: %v (%q)", err, e.Data)
		}
		p.offered[o.Name] = o.Size
		p.cur = o.Name
		p.seen = append(p.seen, o.Name)
		if p.skipOnce {
			p.skipOnce = false
			p.feed(hexHeader(TypeZSKIP, [4]byte{}))
			return
		}
		if p.zackEarly {
			p.zackEarly = false
			p.feed(hexPosHeader(TypeZACK, 0))
		}
		p.feed(hexPosHeader(TypeZRPOS, p.resumeAt))
	case TypeZDATA:
		if !e.CRCOK {
			p.t.Errorf("a block we sent failed our own checksum")
		}
		p.blocks++
		if p.nakAll || p.nakOnce {
			p.nakOnce = false
			// A rejected block is not part of the file: the receiver
			// keeps nothing of it.
			p.feed(hexHeader(TypeZNAK, [4]byte{}))
			return
		}
		p.got[p.cur] = append(p.got[p.cur], e.Data...)
		if p.crcAsk > 0 && uint64(p.crcAsk) <= uint64(len(p.got[p.cur])) {
			// Once, when its own copy reaches that mark: a receiver that
			// demanded a checksum after every block would be auditing, not
			// transferring.
			p.feed(hexPosHeader(TypeZCRC, p.crcAsk))
			p.crcAsk = 0
		}
		if p.pauseAt > 0 && p.blocks == p.pauseAt && p.paused != nil {
			close(p.paused)
			<-p.resume
		}
		p.feed(hexPosHeader(TypeZACK, e.Header.Pos()+uint32(len(e.Data))))
	default:
		p.t.Errorf("a subpacket inside frame %02x", e.Header.Type)
	}
}

// run drives the dialogue to the end and fails if it stalls: a hung
// upload is exactly the bug this file exists to catch. Anything the
// caller passes is fed first, from this goroutine, which is what lets a
// test that pauses the peer still keep every Feed in one place.
func (p *rzPeer) run(start ...[]byte) {
	for _, b := range start {
		p.feed(b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-p.s.Done():
			p.step()
			return
		default:
		}
		p.step()
		if time.Now().After(deadline) {
			p.t.Fatalf("upload stalled in phase %s after %d replies", p.s.Phase(), p.i)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// until drives the peer until the sender has written n header frames.
// A test that wants to interrupt a live batch needs this instead of
// run(): the dialogue keeps moving on the test's own goroutine, so the
// interruption cannot race a second Feed against it.
func (p *rzPeer) until(n int) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.step()
		if len(p.m.replyTypes(p.t)) >= n {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("only %d replies after 3s, wanted %d (phase %s)",
				len(p.m.replyTypes(p.t)), n, p.s.Phase())
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// waitReply blocks until the sender has written n header frames and
// returns every type so far.
func waitReply(t *testing.T, m *mirror, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		types := m.replyTypes(t)
		if len(types) >= n {
			return types
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d replies after 3s, wanted %d (% x)", len(types), n, types)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func settle() { time.Sleep(50 * time.Millisecond) }

// ---- reply inspection ----

type replyFrame struct {
	pos  uint32
	data []byte
}

// framesOf collects the payload of every frame of one type, in the order
// they went out.
func framesOf(ev []Event, typ byte) []replyFrame {
	var out []replyFrame
	cur := -1
	for _, e := range ev {
		switch e.Kind {
		case evHeader:
			if e.Header.Type == typ {
				out = append(out, replyFrame{pos: e.Header.Pos()})
				cur = len(out) - 1
			} else {
				cur = -1
			}
		case evSub:
			if cur >= 0 {
				out[cur].data = append(out[cur].data, e.Data...)
			}
		}
	}
	return out
}

func countFrames(ev []Event, typ byte) int {
	n := 0
	for _, e := range ev {
		if e.Kind == evHeader && e.Header.Type == typ {
			n++
		}
	}
	return n
}

func lastPosOf(ev []Event, typ byte) uint32 {
	var last uint32
	for _, e := range ev {
		if e.Kind == evHeader && e.Header.Type == typ {
			last = e.Header.Pos()
		}
	}
	return last
}

func initFrame(crc32On bool) []byte {
	f := byte(flagCANFDX | flagCANOVIO)
	if crc32On {
		f |= flagCANFC32
	}
	return hexHeader(TypeZRINIT, [4]byte{0, 0, 0, f})
}

// blockingPTY is a terminal that stops taking bytes after the first n
// writes and accepts nothing more until a test lets it: a serial line
// whose flow control has closed, which is the shape the worker exists
// for.
type blockingPTY struct {
	mu      sync.Mutex
	wrote   bytes.Buffer
	release chan struct{}
	n       int // writes still allowed through
}

func (b *blockingPTY) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.n > 0 {
		b.n--
		b.mu.Unlock()
		return len(p), nil
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wrote.Write(p)
}

func (b *blockingPTY) written() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.wrote.Bytes()...)
}

// ---- tests ----

func TestRzCommandNeverAsksTheRemoteAnything(t *testing.T) {
	if got, want := rzCommand(), "rz -q -E\r"; got != want {
		t.Errorf("rzCommand = %q, want %q", got, want)
	}
	// -q hushes rz and -E answers the one question it would otherwise
	// ask: what to do with a file that is already there. Any other flag
	// on that line changes the dialogue and belongs here as a failure
	// before it changes anything.
	for _, flag := range []string{"-e", "-y", "-b", "-r"} {
		if strings.Contains(rzCommand(), flag) {
			t.Errorf("rzCommand carries %s: %q", flag, rzCommand())
		}
	}
}

func TestSenderStartTypesRzAndArms(t *testing.T) {
	s, m := newSender(t, stageFiles(t, stageFile{name: "a.txt", body: []byte("hi")}), sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if got := string(m.snapshot()); got != rzCommand() {
		t.Errorf("typed %q, want %q", got, rzCommand())
	}
	if ph := s.Phase(); ph != phaseName(phaseArmed) {
		t.Errorf("phase = %s, want %s", ph, phaseName(phaseArmed))
	}
	if err := s.Start(); err == nil {
		t.Error("a second Start must be refused: one session per terminal")
	}
	// While armed the terminal is still the user's: nothing is swallowed
	// and what they type is held, not thrown away.
	if s.Swallowing() {
		t.Error("an armed but silent session must not claim the screen")
	}
	if got := s.Keys([]byte("ls\r")); got != nil {
		t.Errorf("keys while armed = %q, want them buffered", got)
	}
	// A session that ends inside the window never took the line, so the
	// typing it held comes back — whatever ended it.
	s.Cancel()
	if res := sendResult(t, s); res.Status != StatusCanceled {
		t.Fatalf("status = %s (%s), want canceled", res.Status, res.Reason)
	}
	if got := string(s.Replay()); got != "ls\r" {
		t.Errorf("Replay = %q, want the keystrokes back", got)
	}
}

func TestSenderRefusesAnUnusableBatch(t *testing.T) {
	if _, err := NewSender(&mirror{}, Options{}, nil); err == nil {
		t.Error("an empty file list must be refused")
	}
	if _, err := NewSender(&mirror{}, Options{}, []SendFile{{Name: "x", Path: "", Size: 3}}); err == nil {
		t.Error("a file with no staged path must be refused")
	}
	// ZMODEM addresses with a 32-bit position, so past that horizon a
	// file could never be resumed correctly.
	_, err := NewSender(&mirror{}, Options{}, []SendFile{{Name: "x", Path: "/tmp/x", Size: int64(maxSendSize) + 1}})
	if err == nil {
		t.Error("a file past the 32-bit horizon must be refused")
	}
}

func TestUploadSingleFileCRC16(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 300) // 4800 bytes: five blocks
	files := stageFiles(t, stageFile{name: "notes.txt", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.feed(p.greeting())
	p.run()

	res := s.Result()
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s), want done", res.Status, res.Err, res.Reason)
	}
	if got := p.got["notes.txt"]; !bytes.Equal(got, body) {
		t.Errorf("received %d bytes, first diff at %d; staged %d",
			len(got), firstDiff(got, body), len(body))
	}
	want := []string{"notes.txt"}
	if !reflect.DeepEqual(p.seen, want) {
		t.Errorf("offered %v, want %v", p.seen, want)
	}
	if !reflect.DeepEqual(res.Files, want) {
		t.Errorf("Result.Files = %v, want %v", res.Files, want)
	}
	ev := m.replies(t)
	if n := countFrames(ev, TypeZEOF); n != 1 {
		t.Errorf("wrote %d ZEOF, want 1", n)
	}
	if n := countFrames(ev, TypeZFIN); n != 1 {
		t.Errorf("wrote %d ZFIN, want exactly one goodbye", n)
	}
	// Every block asks for an answer: that is the stop-and-wait dialogue
	// this build chose, so it should be visible in the frames.
	for _, f := range framesOf(ev, TypeZDATA) {
		if len(f.data) > sendBlock {
			t.Errorf("a %d byte block went out, the limit is %d", len(f.data), sendBlock)
		}
	}
}

func TestUploadSingleFileCRC32(t *testing.T) {
	// The receiver announces CANFC32 in its ZRINIT, and every data frame
	// after that carries a 32-bit checksum — the one place the encoder
	// and the decoder could disagree on byte order.
	body := bytes.Repeat([]byte{0x00, 0x11, 0x8a, 0xff, 'a', 0x7d, 0x18, 0x13}, 400) // 3200
	files := stageFiles(t, stageFile{name: "blob.bin", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, true)
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s), want done", res.Status, res.Err, res.Reason)
	}
	if got := p.got["blob.bin"]; !bytes.Equal(got, body) {
		t.Errorf("binary payload mangled: first diff at %d", firstDiff(got, body))
	}
	if n := countFrames(m.replies(t), TypeZDATA); n != 4 {
		t.Errorf("wrote %d ZDATA frames, want 4 for 3200 bytes", n)
	}
}

func TestUploadMultiFileBatch(t *testing.T) {
	a := bytes.Repeat([]byte("a"), 1500)
	b := []byte("short")
	c := bytes.Repeat([]byte("c"), 1024)
	files := stageFiles(t,
		stageFile{name: "first.txt", body: a},
		stageFile{name: "second.txt", body: b},
		stageFile{name: "third.txt", body: c})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.feed(p.greeting())
	p.run()

	res := s.Result()
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s), want done", res.Status, res.Err, res.Reason)
	}
	want := []string{"first.txt", "second.txt", "third.txt"}
	if !reflect.DeepEqual(p.seen, want) {
		t.Errorf("offered %v, want %v", p.seen, want)
	}
	for name, body := range map[string][]byte{"first.txt": a, "second.txt": b, "third.txt": c} {
		if got := p.got[name]; !bytes.Equal(got, body) {
			t.Errorf("%s: %d bytes received, want %d", name, len(got), len(body))
		}
	}
	if !reflect.DeepEqual(res.Files, want) {
		t.Errorf("Result.Files = %v, want %v", res.Files, want)
	}
	ev := m.replies(t)
	// One goodbye for the whole batch, and one ZEOF per file.
	if n := countFrames(ev, TypeZFIN); n != 1 {
		t.Errorf("wrote %d ZFIN, want 1", n)
	}
	if n := countFrames(ev, TypeZEOF); n != 3 {
		t.Errorf("wrote %d ZEOF, want 3", n)
	}
}

func TestUploadEmptyFileStillShakesHands(t *testing.T) {
	files := stageFiles(t, stageFile{name: "empty", body: nil})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v), want done", res.Status, res.Err)
	}
	ev := m.replies(t)
	// A zero-length file must not be a file the receiver waits forever
	// on: no data frame at all, straight from the offer to ZEOF.
	if n := countFrames(ev, TypeZDATA); n != 0 {
		t.Errorf("an empty file sent %d data frames", n)
	}
	types := m.replyTypes(t)
	for _, want := range []byte{TypeZFILE, TypeZEOF, TypeZFIN} {
		if indexOf(types, want) < 0 {
			t.Fatalf("no %02x in % x", want, types)
		}
	}
	if got := lastPosOf(ev, TypeZEOF); got != 0 {
		t.Errorf("ZEOF position = %d, want 0", got)
	}
}

// TestUploadZNAKResendsTheSameBlock is the ordinary retransmit: the
// receiver rejects a block, the identical bytes go out again, the batch
// finishes with the right file.
func TestUploadZNAKResendsTheSameBlock(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 2048)
	files := stageFiles(t, stageFile{name: "z.txt", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.nakOnce = true
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s), want done", res.Status, res.Err, res.Reason)
	}
	if got := p.got["z.txt"]; !bytes.Equal(got, body) {
		t.Errorf("received %d bytes, first diff at %d; staged %d",
			len(got), firstDiff(got, body), len(body))
	}
	data := framesOf(m.replies(t), TypeZDATA)
	if len(data) != 3 {
		t.Fatalf("wrote %d ZDATA frames, want the rejected one put on the wire again", len(data))
	}
	if data[0].pos != data[1].pos || !bytes.Equal(data[0].data, data[1].data) {
		t.Errorf("the resend was not the same block: %d@%d then %d@%d",
			len(data[0].data), data[0].pos, len(data[1].data), data[1].pos)
	}
	if data[2].pos != 1024 {
		t.Errorf("the batch resumed at %d, want 1024", data[2].pos)
	}
}

func TestUploadTooManyNaksGivesUp(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 4096)
	files := stageFiles(t, stageFile{name: "z.txt", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// A receiver that rejects everything: the session must end rather
	// than loop down the line forever.
	p := newRZPeer(t, s, m, false)
	p.nakAll = true
	p.feed(p.greeting())
	deadline := time.Now().Add(3 * time.Second)
	for !senderFinished(s) && time.Now().Before(deadline) {
		p.step()
		time.Sleep(200 * time.Microsecond)
	}
	res := sendResult(t, s)
	if res.Status != StatusError || res.Reason != "bad-checksum" {
		t.Fatalf("status = %s/%s (%v), want error/bad-checksum", res.Status, res.Reason, res.Err)
	}
	if s.Swallowing() {
		t.Error("a failed upload still claims the screen")
	}
	// The give-up is bounded: one block, re-sent maxBadCRC times.
	if n := countFrames(m.replies(t), TypeZDATA); n > maxBadCRC+1 {
		t.Errorf("wrote %d data frames before giving up, the bound is %d", n, maxBadCRC+1)
	}
}

// TestUploadZRPOSResumesFromTheMiddle is the free win of staging: the
// bytes are on this host, so a receiver that already holds part of the
// file gets exactly the tail.
func TestUploadZRPOSResumesFromTheMiddle(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 250) // 2500 bytes
	files := stageFiles(t, stageFile{name: "tail.bin", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.resumeAt = 1024
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s)", res.Status, res.Err, res.Reason)
	}
	if got := p.got["tail.bin"]; !bytes.Equal(got, body[1024:]) {
		t.Errorf("resumed stream = %d bytes, first diff at %d; want the tail of %d",
			len(got), firstDiff(got, body[1024:]), len(body)-1024)
	}
	data := framesOf(m.replies(t), TypeZDATA)
	if len(data) == 0 || data[0].pos != 1024 {
		t.Fatalf("the first block started at %v, want 1024", data)
	}
	// ZEOF announces the end of the file, not the end of what was sent
	// now: a receiver that compares the two would otherwise truncate it.
	if got := lastPosOf(m.replies(t), TypeZEOF); got != uint32(len(body)) {
		t.Errorf("ZEOF position = %d, want %d", got, len(body))
	}
}

func TestUploadZRPOSPastTheEndIsFatal(t *testing.T) {
	files := stageFiles(t, stageFile{name: "small", body: []byte("1234")})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.resumeAt = 9999
	p.run(p.greeting())
	res := sendResult(t, s)
	if res.Status != StatusError || res.Reason != "bad-offset" {
		t.Fatalf("status = %s/%s, want error/bad-offset", res.Status, res.Reason)
	}
	// A dead session hands the terminal back, and the abort burst that
	// tells the remote it is over went out.
	if s.Swallowing() {
		t.Error("a failed upload still claims the screen")
	}
	if !bytes.Contains(m.snapshot(), []byte{ZDLE, ZDLE, ZDLE, ZDLE, ZDLE}) {
		t.Errorf("a failed upload must shout the cancel sequence, wrote % q", m.snapshot())
	}
}

// TestUploadZCRCAnsweredFromStagedBytes: a receiver may ask "sum what
// you have up to here" to decide where its own partial copy ends. The
// answer must be the checksum of exactly that prefix.
func TestUploadZCRCAnsweredFromStagedBytes(t *testing.T) {
	body := bytes.Repeat([]byte("checksum me "), 200) // 2400 bytes
	for _, crc32On := range []bool{false, true} {
		files := stageFiles(t, stageFile{name: "sum.txt", body: body})
		s, m := newSender(t, files, sendOpts{})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		p := newRZPeer(t, s, m, crc32On)
		p.crcAsk = 1500
		p.feed(p.greeting())
		p.run()

		if res := s.Result(); res.Status != StatusDone {
			t.Fatalf("crc32=%v: status = %s (%v, %s)", crc32On, res.Status, res.Err, res.Reason)
		}
		if got := p.got["sum.txt"]; !bytes.Equal(got, body) {
			t.Errorf("crc32=%v: the checksum request lost %d of %d bytes",
				crc32On, len(body)-len(got), len(body))
		}
		want := uint32(crc16Of(body[:1500]))
		if crc32On {
			want = crc32Of(body[:1500])
		}
		if got := lastPosOf(m.replies(t), TypeZCRC); got != want {
			t.Errorf("crc32=%v: ZCRC answered %#x, want %#x", crc32On, got, want)
		}
	}
}

func TestUploadSkipMovesOnWithoutTheFile(t *testing.T) {
	a := bytes.Repeat([]byte("a"), 1200)
	b := []byte("bbbb")
	files := stageFiles(t,
		stageFile{name: "gone.txt", body: a},
		stageFile{name: "kept.txt", body: b})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.skipOnce = true
	p.feed(p.greeting())
	p.run()

	res := s.Result()
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s), want done", res.Status, res.Err, res.Reason)
	}
	if len(p.got["gone.txt"]) != 0 {
		t.Errorf("a declined file still got %d bytes", len(p.got["gone.txt"]))
	}
	if !bytes.Equal(p.got["kept.txt"], b) {
		t.Errorf("the file after a skip was lost: %q", p.got["kept.txt"])
	}
	if n := countFrames(m.replies(t), TypeZFILE); n != 2 {
		t.Errorf("offered %d files, want 2", n)
	}
	// A decline is still a file the batch dealt with, so the queue's
	// counter reaches the end instead of hanging one short.
	if !reflect.DeepEqual(res.Files, []string{"gone.txt", "kept.txt"}) {
		t.Errorf("Result.Files = %v, want both names", res.Files)
	}
}

// TestUnexpectedZackWhileOfferingIsIgnored is the tolerance lesson from
// the real machine: a keepalive can already be on its way while a file
// is still being announced, and reading it as the answer to the ZFILE
// stops the batch dead.
func TestUnexpectedZackWhileOfferingIsIgnored(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1200)
	files := stageFiles(t, stageFile{name: "k.txt", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.zackEarly = true
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s)", res.Status, res.Err, res.Reason)
	}
	if !bytes.Equal(p.got["k.txt"], body) {
		t.Errorf("got %d of %d bytes", len(p.got["k.txt"]), len(body))
	}
}

func TestUploadPeerAbortEndsTheSession(t *testing.T) {
	files := stageFiles(t, stageFile{name: "a", body: bytes.Repeat([]byte("q"), 3000)})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.feed(p.greeting())
	p.until(3) // offered, and at least one block away
	s.Feed(bytes.Repeat([]byte{ZDLE}, 5))

	res := sendResult(t, s)
	if res.Status != StatusError || res.Reason != "peer-abort" {
		t.Fatalf("status = %s/%s, want error/peer-abort", res.Status, res.Reason)
	}
	if s.Swallowing() {
		t.Error("a dead session must not claim the screen")
	}
	if out := s.Feed([]byte("back to the prompt\r\n")); string(out) != "back to the prompt\r\n" {
		t.Errorf("after the session died, Feed = %q, want every byte through", out)
	}
}

func TestSenderCancelShoutsAndDropsKeys(t *testing.T) {
	files := stageFiles(t, stageFile{name: "a", body: bytes.Repeat([]byte("q"), 5000)})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.feed(p.greeting())
	p.until(3) // at least one block is in flight
	if !s.Swallowing() {
		t.Error("a live upload must own the screen")
	}
	// The remote is reading a protocol, not a shell: keystrokes typed
	// now would be eaten, and saying so is better than lying about it.
	if got := s.Keys([]byte("whoops")); got != nil {
		t.Errorf("keys during a live upload = %q, want them dropped", got)
	}
	s.Cancel()

	res := sendResult(t, s)
	if res.Status != StatusCanceled || res.Reason != "canceled" {
		t.Fatalf("status = %s/%s, want canceled", res.Status, res.Reason)
	}
	if !bytes.Contains(m.snapshot(), bytes.Repeat([]byte{ZDLE}, 10)) {
		t.Error("cancel must put the abort burst on the wire")
	}
	// The user gets their terminal back.
	if got := s.Keys([]byte("ls\r")); string(got) != "ls\r" {
		t.Errorf("after cancel, keys = %q, want them through", got)
	}
}

func TestSenderJobCancelStopsTheWorker(t *testing.T) {
	// The queue's cancel is polled before each block, so a job that was
	// canceled with nobody watching does not keep pushing bytes.
	files := stageFiles(t, stageFile{name: "a", body: bytes.Repeat([]byte("q"), 5000)})
	canceled := make(chan struct{})
	s, m := newSender(t, files, sendOpts{cancel: func() bool {
		select {
		case <-canceled:
			return true
		default:
			return false
		}
	}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.pauseAt, p.paused, p.resume = 1, make(chan struct{}), make(chan struct{})
	go p.run(p.greeting())

	<-p.paused // the peer is holding one acknowledgement back
	close(canceled)
	close(p.resume)

	res := sendResult(t, s)
	if res.Status != StatusCanceled || res.Reason != "canceled" {
		t.Fatalf("status = %s/%s, want canceled/canceled", res.Status, res.Reason)
	}
	if s.Swallowing() {
		t.Error("a canceled upload must not own the screen")
	}
}

func TestSenderArmTimeoutDisarmsAndReplaysKeys(t *testing.T) {
	files := stageFiles(t, stageFile{name: "a", body: []byte("x")})
	s, m := newSender(t, files, sendOpts{arm: 60 * time.Millisecond})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if typed := s.Keys([]byte("echo hi\r")); typed != nil {
		t.Fatalf("keys while armed = %q, want them held", typed)
	}
	res := sendResult(t, s)
	if res.Status != StatusDisarmed || res.Reason != "no-frame" {
		t.Fatalf("status = %s/%s, want disarmed/no-frame", res.Status, res.Reason)
	}
	if got := string(s.Replay()); got != "echo hi\r" {
		t.Errorf("replayed %q, want the keystrokes back", got)
	}
	// A session that never started must leave the shell alone: no CAN
	// burst into a prompt the user was editing.
	if bytes.Contains(m.snapshot(), []byte{ZDLE, ZDLE}) {
		t.Errorf("a disarmed session wrote % q", m.snapshot())
	}
	// Bytes that merely look like frames are output, not protocol: the
	// remote's "command not found" has to stay readable.
	if out := s.Feed([]byte("rz: command not found\r\n")); string(out) != "rz: command not found\r\n" {
		t.Errorf("forwarded %q after disarming, want every byte", out)
	}
	if s.Swallowing() {
		t.Error("a disarmed session must not claim the screen")
	}
}

// TestSenderPumpNeverBlocksFeed is the whole reason the worker exists:
// with a line that has stopped taking bytes, the terminal pump must keep
// serving the user instead of waiting behind a megabyte.
func TestSenderPumpNeverBlocksFeed(t *testing.T) {
	// The typed command gets through; the frame after it does not.
	pty := &blockingPTY{release: make(chan struct{}), n: 1}
	var once sync.Once
	release := func() { once.Do(func() { close(pty.release) }) }

	files := stageFiles(t, stageFile{name: "big", body: bytes.Repeat([]byte("q"), 4096)})
	s, err := NewSender(pty, Options{ArmTimeout: time.Second, FrameTimeout: 10 * time.Second}, files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Cancel()
		release()
	})
	// Start's own write is the one the pty lets through: what must not
	// block the pump is the frame after it.
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan []byte, 1)
	go func() { done <- s.Feed(initFrame(false)) }()
	select {
	case b := <-done:
		if len(b) != 0 {
			t.Errorf("Feed forwarded %q, want the frame taken as protocol", b)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Feed blocked behind the terminal write")
	}
	if ph := s.Phase(); ph != phaseName(phaseSendPos) {
		t.Errorf("phase = %s, want the file offered", ph)
	}
	if !s.Swallowing() {
		t.Error("an upload waiting on the line still owns the screen")
	}
	if senderFinished(s) {
		t.Fatal("a slow write is not a failed session")
	}
	// Reading the state takes the same lock the dialogue runs under: if
	// anything tried to hold it across a write, this would hang.
	if ptyLen := len(pty.written()); ptyLen != 0 {
		t.Errorf("the writer moved %d bytes into a line that takes none", ptyLen)
	}

	release()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if countFrames(drain(t, pty.written()), TypeZFILE) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the file was never offered once the line moved: % q", pty.written())
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// TestZFILEDescriptorIsWhatOurOwnReaderExpects pins the encoder against
// the decoder on the field layout a real sz puts on the wire: the size
// decimal, the date and mode octal, then lrzsz's three trailing fields.
// A descriptor this build writes must not be one this build cannot read.
func TestZFILEDescriptorIsWhatOurOwnReaderExpects(t *testing.T) {
	const when = int64(1700000000)
	p := fileMetaPacket("notes.txt", 4800, 0o644, time.Unix(when, 0))
	o, extra, err := parseOffer(p)
	if err != nil {
		t.Fatalf("parseOffer(%q): %v", p, err)
	}
	if o.Name != "notes.txt" {
		t.Errorf("name = %q", o.Name)
	}
	// 4800 is the case that matters: a decimal total whose digits are
	// not octal must still read as the length, not as another filename.
	if o.Size != 4800 {
		t.Errorf("size = %d, want 4800 (%q)", o.Size, p)
	}
	if !o.HasMode || o.Mode != 0o644 {
		t.Errorf("mode = %v (%v)", o.Mode, o.HasMode)
	}
	if !o.HasTime || o.ModTime.Unix() != when {
		t.Errorf("mtime = %v (%v), want %d", o.ModTime, o.HasTime, when)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want none", extra)
	}
}

// TestUploadSurvivesOneByteFeeds replays a whole upload with every frame
// the receiver sends handed over a single byte at a time: a ZDLE, an
// escape pair or a checksum that straddles a read must change nothing.
// This is the mirror image of the download test of the same kind.
func TestUploadSurvivesOneByteFeeds(t *testing.T) {
	body := []byte{0x18, 0x18, ZPAD, ZPAD, 0x00, 0x8a, 'a', 0x11, 0x13, 'z', 0x7d, 0xff, 0x0d, 0x40, 0x10}
	files := stageFiles(t, stageFile{name: "odd.bin", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.byteFeed = true
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s), want done", res.Status, res.Err, res.Reason)
	}
	if got := p.got["odd.bin"]; !bytes.Equal(got, body) {
		t.Fatalf("byte-at-a-time dialogue mangled the batch: % x, want % x", got, body)
	}
}

func TestSenderZSINITAttentionSequenceIsRefused(t *testing.T) {
	files := stageFiles(t, stageFile{name: "a", body: []byte("x")})
	s, _ := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// An attention string is control characters typed at the remote to
	// unstick it. Doing that inside somebody's shell is not acceptable,
	// and pretending to honour it is worse.
	attn := []byte{0x08, 0x08, 0x18, 0x18, 0x08}
	s.Feed(hexFrame(TypeZSINIT, [4]byte{}, subpkg{data: attn, ender: ZCRCE}))
	res := sendResult(t, s)
	if res.Status != StatusError || res.Reason != "attention-sequence" {
		t.Fatalf("status = %s/%s, want error/attention-sequence", res.Status, res.Reason)
	}
}

func TestSenderEscapesControlBytesWhenAsked(t *testing.T) {
	// A ZSINIT with an empty attention string only means "escape every
	// control byte", which the dialogue can honour.
	body := bytes.Repeat([]byte{0x08, 'a', 0x01, 'b', 0x18, 'c'}, 400) // 2400
	files := stageFiles(t, stageFile{name: "ctl.bin", body: body})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	s.Feed(hexFrame(TypeZSINIT, [4]byte{}, subpkg{data: []byte{0}, ender: ZCRCE}))
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s)", res.Status, res.Err, res.Reason)
	}
	if got := p.got["ctl.bin"]; !bytes.Equal(got, body) {
		t.Errorf("escaping lost bytes: first diff at %d", firstDiff(got, body))
	}
	// 0x08 goes out as ZDLE 'H', never bare.
	if !bytes.Contains(m.snapshot(), []byte{ZDLE, 0x48}) {
		t.Error("ESCCTL was accepted but no control byte was escaped")
	}
}

func TestSenderProgressIsMonotonic(t *testing.T) {
	body := bytes.Repeat([]byte("m"), 3000)
	files := stageFiles(t,
		stageFile{name: "one", body: body},
		stageFile{name: "two", body: []byte("bb")})
	var mu sync.Mutex
	var seen []Progress
	s, m := newSender(t, files, sendOpts{progress: func(p Progress) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	p.feed(p.greeting())
	p.run()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no progress at all")
	}
	var last int64
	for _, pr := range seen {
		if pr.Done < last {
			t.Fatalf("progress went backwards: %d after %d", pr.Done, last)
		}
		if pr.Done > pr.Total {
			t.Fatalf("Done %d past Total %d", pr.Done, pr.Total)
		}
		if pr.FilesGot != 2 {
			t.Fatalf("FilesGot = %d, want the size of the batch", pr.FilesGot)
		}
		last = pr.Done
	}
	if want := int64(len(body) + 2); last != want {
		t.Errorf("progress ended at %d, want %d", last, want)
	}
}

// TestSenderWritesNoFileBeforeTheReceiverAsksForIt is the ordering rule
// the real machine taught: the offer goes out only once the receiver's
// ZRINIT has been read, and no data byte may precede the ZRPOS that
// asks for it.
func TestSenderWritesNoFileBeforeTheReceiverAsksForIt(t *testing.T) {
	files := stageFiles(t, stageFile{name: "a", body: bytes.Repeat([]byte("q"), 2000)})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	settle()
	if types := m.replyTypes(t); len(types) != 0 {
		t.Fatalf("wrote frames before the receiver greeted: % x", types)
	}

	s.Feed(hexHeader(TypeZRQINIT, [4]byte{}))
	waitReply(t, m, 1)
	// The courtesy answer to a probing receiver, and still no file.
	if got := m.replyTypes(t); len(got) != 1 || got[0] != TypeZRINIT {
		t.Fatalf("replies = % x, want one ZRINIT", got)
	}

	s.Feed(initFrame(false))
	waitReply(t, m, 2)
	if got := m.replyTypes(t); got[1] != TypeZFILE {
		t.Fatalf("replies = % x, want ZFILE second", got)
	}
	if n := countFrames(m.replies(t), TypeZDATA); n != 0 {
		t.Fatalf("%d data frames went out before any ZRPOS asked for them", n)
	}

	s.Feed(hexPosHeader(TypeZRPOS, 0))
	waitReply(t, m, 3)
	if got := m.replyTypes(t); got[2] != TypeZDATA {
		t.Fatalf("replies = % x, want ZDATA third", got)
	}
}

func TestSenderIgnoresShellChatterBeforeTheBatch(t *testing.T) {
	// Whatever the remote printed before rz got control must come back
	// as output, and the dialogue must still complete.
	files := stageFiles(t, stageFile{name: "a", body: bytes.Repeat([]byte("q"), 1200)})
	s, m := newSender(t, files, sendOpts{})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := newRZPeer(t, s, m, false)
	if out := s.Feed([]byte("\r\nrz: waiting.\r\n")); string(out) != "\r\nrz: waiting.\r\n" {
		t.Errorf("forwarded %q, want the chatter whole", out)
	}
	p.feed(p.greeting())
	p.run()

	if res := s.Result(); res.Status != StatusDone {
		t.Fatalf("status = %s (%v, %s)", res.Status, res.Err, res.Reason)
	}
	if got := len(p.got["a"]); got != 1200 {
		t.Errorf("received %d bytes, want 1200", got)
	}
}

func indexOf(types []byte, want byte) int {
	for i, t := range types {
		if t == want {
			return i
		}
	}
	return -1
}

// TestZACKActsOnThePositionItCarries pins the fix for the blind advance:
// any arriving ZACK used to mean "one block more", so a duplicate — or
// the answer to a block still sitting in the line's buffer — pushed a
// block that was never acknowledged. The file then arrived with a hole
// in it, and every checksum after that was computed over the wrong bytes.
func TestZACKActsOnThePositionItCarries(t *testing.T) {
	files := stageFiles(t, stageFile{name: "a.bin", body: bytes.Repeat([]byte("k"), 9000)})
	s, _ := newSender(t, files, sendOpts{})

	s.phase = phaseSendData
	s.off.Store(2000)
	s.next.Store(3000)

	// Behind the block in flight: the answer to an older block. It cannot
	// move us anywhere that is not a guess.
	s.header(Header{Type: TypeZACK, Raw: posBytes(1000)})
	if s.off.Load() != 2000 || s.next.Load() != 3000 {
		t.Errorf("stale ZACK moved the window to %d..%d, want 2000..3000", s.off.Load(), s.next.Load())
	}
	// Ahead of everything ever sent: impossible from a real peer, and the
	// wrong answer is to skip a file forward.
	s.header(Header{Type: TypeZACK, Raw: posBytes(5000)})
	if s.off.Load() != 2000 || s.next.Load() != 3000 {
		t.Errorf("impossible ZACK moved the window to %d..%d, want 2000..3000", s.off.Load(), s.next.Load())
	}
	// The acknowledgement of the block in flight is the one that counts.
	s.header(Header{Type: TypeZACK, Raw: posBytes(3000)})
	if s.off.Load() != 3000 {
		t.Errorf("in-order ZACK left off at %d, want 3000", s.off.Load())
	}
	if senderFinished(s) {
		t.Error("the session ended under a test that only fed it frames")
	}
}
