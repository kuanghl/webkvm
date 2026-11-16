package zmodem

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mirror is the pty end of a session: it records every frame the
// receiver writes so a test can assert on the protocol we speak back.
type mirror struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (m *mirror) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}

func (m *mirror) snapshot() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.buf.Bytes()...)
}

// replies decodes what the receiver has written so far, with our own
// parser: the encoder and the decoder are checked against each other on
// every assertion instead of against a hand-written expectation.
func (m *mirror) replies(t *testing.T) []Event {
	t.Helper()
	return drain(t, m.snapshot())
}

func (m *mirror) replyTypes(t *testing.T) []byte {
	t.Helper()
	var out []byte
	for _, e := range m.replies(t) {
		if e.Kind == evHeader {
			out = append(out, e.Header.Type)
		}
	}
	return out
}

// wantReply asserts that the receiver wrote a header of the given type
// as its n-th answer.
func (m *mirror) wantReply(t *testing.T, n int, typ byte) {
	t.Helper()
	got := m.replyTypes(t)
	if n >= len(got) {
		t.Fatalf("only %d replies so far, wanted a %02x as reply %d (types % x)", len(got), typ, n, got)
	}
	if got[n] != typ {
		t.Errorf("reply %d = %02x, want %02x (all: % x)", n, got[n], typ, got)
	}
}

type testOpts struct {
	dir      string
	arm      time.Duration
	frame    time.Duration
	progress func(Progress)
	file     func(FileEvent)
	cancel   func() bool
}

func newReceiver(t *testing.T, opt testOpts) (*Receiver, *mirror) {
	t.Helper()
	if opt.dir == "" {
		opt.dir = t.TempDir()
	}
	if opt.arm == 0 {
		opt.arm = 5 * time.Second
	}
	if opt.frame == 0 {
		opt.frame = 5 * time.Second
	}
	m := &mirror{}
	r := New(m, Options{
		Dir:          opt.dir,
		ArmTimeout:   opt.arm,
		FrameTimeout: opt.frame,
		OnProgress:   opt.progress,
		OnFile:       opt.file,
		Cancelled:    opt.cancel,
	})
	t.Cleanup(func() {
		if r != nil {
			r.Cancel()
		}
	})
	return r, m
}

func waitResult(t *testing.T, r *Receiver) Result {
	t.Helper()
	select {
	case <-r.Done():
		return r.Result()
	case <-time.After(3 * time.Second):
		t.Fatalf("session did not finish (phase %s)", r.Phase())
		return Result{}
	}
}

func feed(t *testing.T, r *Receiver, chunks ...[]byte) []byte {
	t.Helper()
	var forwarded []byte
	for _, c := range chunks {
		forwarded = append(forwarded, r.Feed(c)...)
	}
	return forwarded
}

// ---- the sender side of a batch ----

type batchFile struct {
	// name is what the ZFILE announces.
	name string
	// chunks are the data frames, in order.
	chunks [][]byte
	// mode, when non-zero, is announced in the metadata.
	mode  os.FileMode
	mtime int64
}

// batch renders the whole remote side of one sz run: ZRQINIT, then per
// file the ZFILE header plus its metadata packet, the data frames and
// ZEOF, and a final ZFIN. That is the sequence lrzsz produces, with the
// ZFILE header in hex and everything bulk in binary.
func batch(crc32On bool, files ...batchFile) []byte {
	var out []byte
	out = append(out, hexFrame(TypeZRQINIT, [4]byte{})...)
	for _, f := range files {
		meta := metaPacket(f)
		if crc32On {
			out = append(out, zframe(TypeZFILE, posBytes(uint32(len(meta))), true, subpkg{data: meta, ender: ZCRCE})...)
		} else {
			out = append(out, hexFrame(TypeZFILE, [4]byte{}, subpkg{data: meta, ender: ZCRCE})...)
		}
		var pos uint32
		for i, c := range f.chunks {
			ender := byte(ZCRCW)
			if i == len(f.chunks)-1 {
				ender = ZCRCE
			}
			// A multi-part file inside one frame exercises the ZCRCG
			// rule: the bytes after it are payload, not a new header.
			out = append(out, zframe(TypeZDATA, posBytes(pos), crc32On, subpkg{data: c, ender: ender})...)
			pos += uint32(len(c))
		}
		out = append(out, hexFrame(TypeZEOF, posBytes(pos))...)
	}
	out = append(out, hexFrame(TypeZFIN, [4]byte{})...)
	return out
}

func metaPacket(f batchFile) []byte {
	mode := f.mode
	if mode == 0 {
		mode = 0o644
	}
	when := f.mtime
	if when == 0 {
		when = time.Now().Add(-24 * time.Hour).Unix()
	}
	// PATHNAME NUL decimal-size SPACE octal-mtime SPACE octal-mode NUL
	return []byte(f.name + "\x00" +
		fmt.Sprintf("%d %o %o", totalSize(f), when, 0o100000|uint32(mode)) + "\x00")
}

func totalSize(f batchFile) int64 {
	var n int64
	for _, c := range f.chunks {
		n += int64(len(c))
	}
	return n
}

// ---- tests ----

// TestPassthroughWhenNothingIsArmed is the regression gate for the live
// terminal: with no session started, every byte must go through
// untouched, in both directions.
func TestPassthroughWhenNothingIsArmed(t *testing.T) {
	r, m := newReceiver(t, testOpts{})
	in := []byte("user@host:~$ ls\r\nfile.txt\r\n")
	if got := r.Feed(in); !bytes.Equal(got, in) {
		t.Errorf("downlink forwarded %q, want the input verbatim", got)
	}
	keys := []byte("cat file.txt\r")
	if got := r.Keys(keys); !bytes.Equal(got, keys) {
		t.Errorf("uplink forwarded %q, want the keys verbatim", got)
	}
	if m.snapshot() != nil && len(m.snapshot()) != 0 {
		t.Errorf("an unarmed receiver wrote %q to the pty", m.snapshot())
	}
	if r.Swallowing() {
		t.Error("an unarmed session must not claim the screen")
	}
}

func TestStartTypesTheCommandAndArms(t *testing.T) {
	r, m := newReceiver(t, testOpts{})
	if err := r.Start([]string{"/tmp/a file.txt"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := string(m.snapshot()), "sz -q '/tmp/a file.txt'\r"; got != want {
		t.Fatalf("typed %q, want %q", got, want)
	}
	if phase := r.Phase(); phase != "armed" {
		t.Errorf("phase = %s, want armed", phase)
	}
	if err := r.Start([]string{"x"}, false); err == nil {
		t.Error("a second download on the same terminal must be refused")
	}
	// Keystrokes are held for the duration of the arm window, so they can
	// be replayed if nothing answers.
	if got := r.Keys([]byte("ls\r")); got != nil {
		t.Errorf("keys during the arm window = %q, want them buffered", got)
	}
}

func TestDownloadSingleFile(t *testing.T) {
	dir := t.TempDir()
	r, m := newReceiver(t, testOpts{dir: dir})
	if err := r.Start([]string{"/tmp/hello.txt"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	payload := []byte("hello webkvm\n")
	forwarded := feed(t, r, batch(false, batchFile{name: "hello.txt", chunks: [][]byte{payload}}))
	if len(forwarded) != 0 {
		t.Errorf("protocol bytes leaked to the screen: %q", forwarded)
	}
	res := waitResult(t, r)
	if res.Status != StatusDone {
		t.Fatalf("status = %s (reason %s, err %v), want done", res.Status, res.Reason, res.Err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %v", res.Files)
	}
	body, err := os.ReadFile(res.Files[0])
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("landed %q, want %q", body, payload)
	}
	if rel, _ := filepath.Rel(dir, res.Files[0]); rel != "hello.txt" {
		t.Errorf("landed at %q, want hello.txt under the job dir", rel)
	}
	// The conversation we had: ZRINIT for the request, ZRPOS 0 for the
	// announced file, a second ZRINIT after ZEOF, then ZFIN for ZFIN. One
	// chunk ending with ZCRCE needs no ZACK, so none is sent.
	want := []byte{TypeZRINIT, TypeZRPOS, TypeZRINIT, TypeZFIN}
	if got := m.replyTypes(t); !bytes.Equal(got, want) {
		t.Errorf("replies = % x, want % x", got, want)
	}
	// Mode from the announcement is applied, not left at the 0600 the
	// file was created with.
	st, err := os.Stat(res.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 644", st.Mode())
	}
}

func TestDownloadCRC32Frames(t *testing.T) {
	dir := t.TempDir()
	r, _ := newReceiver(t, testOpts{dir: dir})
	if err := r.Start([]string{"/tmp/big.bin"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Bytes that need escaping and bytes that look like headers, so a
	// CRC32 sender is exercised on the hard cases too.
	payload := bytes.Repeat([]byte{ZPAD, ZPAD, ZDLE, 0x11, 0x13, 0xff, 0x7f, 'x'}, 700)
	feed(t, r, batch(true, batchFile{name: "big.bin", chunks: [][]byte{payload}}))
	res := waitResult(t, r)
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%s), want done", res.Status, res.Reason)
	}
	body, err := os.ReadFile(res.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("CRC32 download corrupted: %d bytes landed, want %d", len(body), len(payload))
	}
}

func TestDownloadMultiFileBatchWithSubdirectories(t *testing.T) {
	dir := t.TempDir()
	var files []string
	var got []FileEvent
	r, _ := newReceiver(t, testOpts{dir: dir, file: func(e FileEvent) {
		got = append(got, e)
		files = append(files, e.Name)
	}})
	if err := r.Start([]string{"-r", "tree"}, true); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := []byte("first\n")
	b := []byte("second file, split across frames\n")
	feed(t, r, batch(false,
		batchFile{name: "tree/a.txt", chunks: [][]byte{a}},
		batchFile{name: "tree/sub/b.txt", chunks: [][]byte{b[:10], b[10:]}},
	))
	res := waitResult(t, r)
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%s), want done", res.Status, res.Reason)
	}
	if strings.Join(files, ",") != "tree/a.txt,tree/sub/b.txt" {
		t.Errorf("OnFile names = %v", files)
	}
	for _, want := range []struct {
		rel  string
		body []byte
	}{{"tree/a.txt", a}, {"tree/sub/b.txt", b}} {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(want.rel)))
		if err != nil {
			t.Errorf("read %s: %v", want.rel, err)
			continue
		}
		if !bytes.Equal(body, want.body) {
			t.Errorf("%s = %q, want %q", want.rel, body, want.body)
		}
	}
}

// TestDownloadSurvivesEveryChunkSplit replays the whole exchange cut at
// every single byte, which is the only way to prove the incremental
// parser handles an escape pair or a header straddling a read boundary.
func TestDownloadSurvivesEveryChunkSplit(t *testing.T) {
	stream := batch(false, batchFile{name: "split.txt", chunks: [][]byte{
		[]byte("AAAA"),
		{ZPAD, ZDLE, 0x11, 0x13, 'B', 'B'},
		[]byte("CCCCCCCCCCCCCCCCCCCC"),
	}})
	for cut := 1; cut < len(stream); cut++ {
		dir := t.TempDir()
		r, _ := newReceiver(t, testOpts{dir: dir, arm: time.Hour, frame: time.Hour})
		if err := r.Start([]string{"split.txt"}, false); err != nil {
			t.Fatalf("cut %d Start: %v", cut, err)
		}
		feed(t, r, stream[:cut], stream[cut:])
		res := waitResult(t, r)
		if res.Status != StatusDone {
			t.Fatalf("cut %d: status = %s (%s, %v)", cut, res.Status, res.Reason, res.Err)
		}
		body, err := os.ReadFile(res.Files[0])
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if len(body) != 30 {
			t.Fatalf("cut %d: %d bytes landed", cut, len(body))
		}
	}
}

// TestRefusesPathsThatClimbOut: a sender that offers "../../escape.txt"
// is refused outright rather than quietly renamed.
func TestRefusesPathsThatClimbOut(t *testing.T) {
	trap := []byte("TRAPPED")
	for _, name := range []string{
		"../../escape.txt",
		"a/../../escape.txt",
		"..",
		"",
		"\x00etc/passwd",
	} {
		dir := t.TempDir()
		outside := filepath.Join(filepath.Dir(dir), "escape.txt")
		if err := os.WriteFile(outside, trap, 0o600); err != nil {
			t.Fatal(err)
		}
		r, _ := newReceiver(t, testOpts{dir: dir})
		if err := r.Start([]string{"x"}, false); err != nil {
			t.Fatalf("Start: %v", err)
		}
		feed(t, r, batch(false, batchFile{name: name, chunks: [][]byte{[]byte("nope")}}))
		if res := waitResult(t, r); res.Status == StatusDone {
			t.Errorf("name %q was accepted: %v", name, res.Files)
		}
		body, _ := os.ReadFile(outside)
		if !bytes.Equal(body, trap) {
			t.Errorf("name %q overwrote a file outside the job dir", name)
		}
		_ = os.Remove(outside)
	}
}

// TestOddNamesStayInside covers the names that are legal on Linux or
// that sz itself produces for absolute arguments: they must land inside
// the job directory, never escape it, and never be refused.
func TestOddNamesStayInside(t *testing.T) {
	dir := t.TempDir()
	weird := []string{
		"/tmp/escape.txt",    // sz of an absolute path: the root is dropped
		`..\escape.txt`,      // a backslash is an ordinary character here
		"a;touch /tmp/pwned", // shell metacharacters are just a name
		"$(id).txt",          //
		"ünïcode-ß.txt",      //
		"two  spaces.txt",    //
		"-rf",                // would be an option if it ever reached a shell
		"deep/nested/tree/f.txt",
	}
	var chunks [][]byte
	var files []batchFile
	for i, name := range weird {
		body := []byte(fmt.Sprintf("body %d of %s\n", i, name))
		chunks = append(chunks, body)
		files = append(files, batchFile{name: name, chunks: [][]byte{body}})
	}
	r, _ := newReceiver(t, testOpts{dir: dir})
	if err := r.Start([]string{"x"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	feed(t, r, batch(false, files...))
	res := waitResult(t, r)
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%s, %v), want every odd name accepted", res.Status, res.Reason, res.Err)
	}
	if len(res.Files) != len(weird) {
		t.Fatalf("%d files landed, want %d: %v", len(res.Files), len(weird), res.Files)
	}
	clean := filepath.Clean(dir)
	for i, p := range res.Files {
		inside := filepath.Clean(p)
		if !strings.HasPrefix(inside, clean+string(os.PathSeparator)) {
			t.Errorf("file %d (%q) is outside the job dir", i, p)
		}
		body, err := os.ReadFile(inside)
		if err != nil {
			t.Errorf("file %d: %v", i, err)
			continue
		}
		if !bytes.Equal(body, chunks[i]) {
			t.Errorf("file %d content mismatch", i)
		}
	}
	if _, err := os.Stat("/tmp/pwned"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file name was interpreted by a shell")
	}
}

// TestSafeJoin is the unit level of the same guarantee, including the
// shapes that must be kept: a relative tree from sz -r.
func TestSafeJoin(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		want string // relative to dir; "" means refused
	}{
		{"a.txt", "a.txt"},
		{"tree/sub/b.txt", "tree/sub/b.txt"},
		{"./x", "x"},
		{"a//b", "a/b"},
		{"/etc/passwd", "etc/passwd"},
		{`..\x`, `..\x`},
		{"a/../b", ""},
		{"../a.txt", ""},
		{"..", ""},
		{"", ""},
		{"a/\x00b", ""},
	} {
		got, err := safeJoin(dir, tc.name)
		if tc.want == "" {
			if err == nil {
				t.Errorf("safeJoin(%q) = %q, want refusal", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("safeJoin(%q): %v", tc.name, err)
			continue
		}
		rel, rerr := filepath.Rel(dir, got)
		if rerr != nil || rel != filepath.FromSlash(tc.want) {
			t.Errorf("safeJoin(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBadChecksumAsksForARetransmitThenGivesUp(t *testing.T) {
	dir := t.TempDir()
	r, m := newReceiver(t, testOpts{dir: dir})
	if err := r.Start([]string{"f"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	feed(t, r, hexFrame(TypeZRQINIT, [4]byte{}))
	feed(t, r, hexFrame(TypeZFILE, [4]byte{}, subpkg{
		data: []byte("f\x00 4 0 100644\x00"), ender: ZCRCE}))
	// Four frames whose payload checksum is wrong. The first three get a
	// NAK + ZRPOS; the fourth ends the session.
	corrupt := zframe(TypeZDATA, posBytes(0), false, subpkg{data: []byte("abcd"), ender: ZCRCE})
	corrupt[len(corrupt)-1] ^= 0xff
	for i := 0; i < maxBadCRC+1; i++ {
		feed(t, r, corrupt)
	}
	res := waitResult(t, r)
	if res.Status != StatusError || res.Reason != "bad-checksum" {
		t.Fatalf("status = %s/%s, want error/bad-checksum", res.Status, res.Reason)
	}
	types := string(m.replyTypes(t))
	if !strings.Contains(types, string([]byte{TypeZNAK})) {
		t.Errorf("no ZNAK was ever sent: replies % x", m.replyTypes(t))
	}
	// A failed transfer must tell the sender to stop.
	if !bytes.Contains(m.snapshot(), bytes.Repeat([]byte{ZDLE}, 10)) {
		t.Error("a failed session must shout the cancel sequence")
	}
}

func TestPeerAbortEndsTheSession(t *testing.T) {
	r, _ := newReceiver(t, testOpts{})
	if err := r.Start([]string{"f"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	feed(t, r, hexFrame(TypeZRQINIT, [4]byte{}))
	if phase := r.Phase(); phase != "wait-init" {
		t.Fatalf("phase = %s, want wait-init while the batch is live", phase)
	}
	// Now the sender gives up mid-flight: ten CANs in a row.
	feed(t, r, bytes.Repeat([]byte{ZDLE}, 10))
	done := waitResult(t, r)
	if done.Status != StatusError || done.Reason != "peer-abort" {
		t.Fatalf("status = %s/%s, want error/peer-abort", done.Status, done.Reason)
	}
}

func TestOffsetMismatchIsFatal(t *testing.T) {
	r, _ := newReceiver(t, testOpts{})
	if err := r.Start([]string{"f"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	feed(t, r, hexFrame(TypeZRQINIT, [4]byte{}))
	feed(t, r, hexFrame(TypeZFILE, [4]byte{}, subpkg{
		data: []byte("f\x00 4 0 100644\x00"), ender: ZCRCE}))
	// A sender that resumes somewhere else would corrupt the file
	// silently, so it is refused rather than patched over.
	feed(t, r, zframe(TypeZDATA, posBytes(99), false, subpkg{data: []byte("abcd"), ender: ZCRCE}))
	res := waitResult(t, r)
	if res.Status != StatusError || res.Reason != "bad-offset" {
		t.Fatalf("status = %s/%s, want error/bad-offset", res.Status, res.Reason)
	}
}

func TestCancelStopsTheTransfer(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("job canceled")
	dropped := false
	r, _ := newReceiver(t, testOpts{dir: dir, cancel: func() bool {
		dropped = true
		return true
	}})
	if err := r.Start([]string{"f"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = want
	feed(t, r, batch(false, batchFile{name: "f", chunks: [][]byte{[]byte("data")}}))
	res := waitResult(t, r)
	if res.Status != StatusCanceled {
		t.Fatalf("status = %s (%s), want canceled", res.Status, res.Reason)
	}
	if !dropped {
		t.Error("the cancel predicate was never consulted")
	}
	if len(res.Files) != 0 {
		t.Errorf("a canceled batch must not report finished files: %v", res.Files)
	}
}

// TestArmTimeoutDisarmsAndReplaysKeys is the no-lrzsz path: the command
// is typed, nothing answers, and the terminal comes back exactly as the
// user left it, their typing included.
func TestArmTimeoutDisarmsAndReplaysKeys(t *testing.T) {
	r, m := newReceiver(t, testOpts{arm: 80 * time.Millisecond})
	if err := r.Start([]string{"nope"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	typed := r.Keys([]byte("ls\r"))
	if typed != nil {
		t.Errorf("keys = %q, want them held during the arm window", typed)
	}
	// Shell chatter keeps flowing to the screen while we wait.
	chatter := []byte("bash: nope: command not found\r\n")
	if got := r.Feed(chatter); !bytes.Contains(got, chatter) {
		t.Errorf("shell output was swallowed: %q", got)
	}
	res := waitResult(t, r)
	if res.Status != StatusDisarmed {
		t.Fatalf("status = %s (%s), want disarmed", res.Status, res.Reason)
	}
	if !strings.Contains(res.Err.Error(), "lrzsz") {
		t.Errorf("the operator needs a hint about lrzsz, got %q", res.Err)
	}
	if got := string(r.Replay()); got != "ls\r" {
		t.Errorf("replayed keys = %q, want the typed ls", got)
	}
	if r.Replay() != nil {
		t.Error("replay must be one-shot")
	}
	// A session that never spoke ZMODEM must not shout CAN at a prompt.
	if bytes.Contains(m.snapshot(), []byte{ZDLE, ZDLE, ZDLE}) {
		t.Errorf("a disarmed session wrote an abort burst: % q", m.snapshot())
	}
	// And the terminal is normal again.
	if got := r.Feed(chatter); !bytes.Equal(got, chatter) {
		t.Errorf("after disarm, downlink = %q, want passthrough", got)
	}
	if got := r.Keys([]byte("pwd\r")); string(got) != "pwd\r" {
		t.Errorf("after disarm, uplink = %q, want passthrough", got)
	}
}

// TestChattyShellDoesNotExtendTheArmWindow: a busy remote (yes, a build
// log) must not postpone the deadline forever, because keystrokes are
// held while the session is armed.
func TestChattyShellDoesNotExtendTheArmWindow(t *testing.T) {
	r, _ := newReceiver(t, testOpts{arm: 200 * time.Millisecond})
	if err := r.Start([]string{"sz-missing"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.Feed([]byte("output output output\r\n"))
		if isClosed(r) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	res := waitResult(t, r)
	if res.Status != StatusDisarmed {
		t.Fatalf("status = %s, want disarmed despite a busy stream", res.Status)
	}
}

func isClosed(r *Receiver) bool {
	select {
	case <-r.Done():
		return true
	default:
		return false
	}
}

// TestFalseHeaderInShellOutputKeepsTerminalUsable: output that merely
// looks like a frame must neither swallow bytes nor end the session.
func TestFalseHeaderInShellOutputKeepsTerminalUsable(t *testing.T) {
	r, m := newReceiver(t, testOpts{arm: 3 * time.Second})
	if err := r.Start([]string{"x"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	bait := []byte("0000  **\x18B0100000023ffff\r\n" + "0010  aabb\r\n")
	if got := r.Feed(bait); !bytes.Equal(got, bait) {
		t.Errorf("forwarded %q, want the hexdump intact", got)
	}
	if isClosed(r) {
		t.Fatal("a false alarm must not end the session")
	}
	if bytes.Contains(m.snapshot(), []byte{ZDLE, ZDLE}) {
		t.Errorf("a false alarm wrote %q to the pty", m.snapshot())
	}
	// A real frame afterwards still starts the transfer.
	feed(t, r, hexFrame(TypeZRQINIT, [4]byte{}))
	if phase := r.Phase(); phase != "wait-init" {
		t.Fatalf("phase = %s, want wait-init: the real ZRQINIT was missed", phase)
	}
	r.Cancel()
	if res := waitResult(t, r); res.Status != StatusCanceled {
		t.Fatalf("status = %s (%s), want canceled", res.Status, res.Reason)
	}
}

// TestFrameTimeoutFailsAStartedSession: silence after a genuine frame is
// a broken transfer, and must not silently hang the queue forever.
func TestFrameTimeoutFailsAStartedSession(t *testing.T) {
	r, _ := newReceiver(t, testOpts{arm: time.Second, frame: 80 * time.Millisecond})
	if err := r.Start([]string{"x"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	feed(t, r, hexFrame(TypeZRQINIT, [4]byte{}))
	res := waitResult(t, r)
	if res.Status != StatusError || res.Reason != "frame-timeout" {
		t.Fatalf("status = %s/%s, want error/frame-timeout", res.Status, res.Reason)
	}
}

func TestProgressReportsMonotonicBytes(t *testing.T) {
	var seen []Progress
	r, _ := newReceiver(t, testOpts{progress: func(p Progress) { seen = append(seen, p) }})
	if err := r.Start([]string{"f"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	body := bytes.Repeat([]byte("0123456789"), 40)
	feed(t, r, batch(false, batchFile{name: "f", chunks: [][]byte{body[:200], body[200:]}}))
	waitResult(t, r)
	if len(seen) == 0 {
		t.Fatal("no progress was reported")
	}
	var last int64
	for _, p := range seen {
		if p.Done < last {
			t.Fatalf("progress went backwards: %d after %d", p.Done, last)
		}
		last = p.Done
	}
	if last != int64(len(body)) {
		t.Errorf("final Done = %d, want %d", last, len(body))
	}
	if p := seen[len(seen)-1]; p.Total != int64(len(body)) {
		t.Errorf("final Total = %d, want %d (announced size)", p.Total, len(body))
	}
}

func TestDuplicateNameKeepsBothCopies(t *testing.T) {
	dir := t.TempDir()
	r, _ := newReceiver(t, testOpts{dir: dir})
	if err := r.Start([]string{"f"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	feed(t, r, batch(false,
		batchFile{name: "same.txt", chunks: [][]byte{[]byte("one")}},
		batchFile{name: "same.txt", chunks: [][]byte{[]byte("two")}},
	))
	res := waitResult(t, r)
	if res.Status != StatusDone {
		t.Fatalf("status = %s (%s)", res.Status, res.Reason)
	}
	if len(res.Files) != 2 || res.Files[0] == res.Files[1] {
		t.Fatalf("files = %v, want two distinct paths", res.Files)
	}
	a, _ := os.ReadFile(res.Files[0])
	b, _ := os.ReadFile(res.Files[1])
	if string(a) != "one" || string(b) != "two" {
		t.Errorf("contents = %q and %q, want one and two", a, b)
	}
}

// ---- pure helpers ----

func TestSzCommand(t *testing.T) {
	for _, tc := range []struct {
		paths []string
		deep  bool
		want  string
		bad   bool
	}{
		{paths: []string{"a.txt"}, want: `sz -q 'a.txt'` + "\r"},
		{paths: []string{"a.txt", "b.txt"}, want: `sz -q 'a.txt' 'b.txt'` + "\r"},
		{paths: []string{"with space", "two  spaces"}, want: `sz -q 'with space' 'two  spaces'` + "\r"},
		{paths: []string{"'*'"}, want: `sz -q ''\'''*''\'''` + "\r"},
		{paths: []string{";rm -rf /"}, want: `sz -q ';rm -rf /'` + "\r"},
		{paths: []string{"$(id)"}, want: `sz -q '$(id)'` + "\r"},
		{paths: []string{"`id`"}, want: `sz -q '` + "`id`" + `'` + "\r"},
		{paths: []string{"-rf"}, want: `sz -q './-rf'` + "\r"},
		{paths: []string{"dir"}, deep: true, want: `sz -q -r 'dir'` + "\r"},
		// "*" and "?" are the only characters allowed to stand outside
		// the quotes: sz never expands a wildcard by itself.
		{paths: []string{"/var/log/*.log"}, want: `sz -q '/var/log/'*'.log'` + "\r"},
		{paths: []string{"a?b.txt"}, want: `sz -q 'a'?'b.txt'` + "\r"},
		{paths: []string{"*.txt", "b.txt"}, want: `sz -q *'.txt' 'b.txt'` + "\r"},
		{paths: []string{"-rf*"}, want: `sz -q './-rf'*` + "\r"},
		{paths: []string{"*;rm"}, want: `sz -q *';rm'` + "\r"},
		{paths: []string{"*`id`"}, want: `sz -q *'` + "`id`" + `'` + "\r"},
		{paths: []string{"[abc]"}, want: `sz -q '[abc]'` + "\r"},
		// The path length budget counts what the shell actually reads.
		{paths: []string{strings.Repeat("/var/log/", 400) + "*.log"}, bad: true},
		{paths: []string{"line\nbreak"}, bad: true},
		{paths: []string{"carriage\rreturn"}, bad: true},
		{paths: nil, bad: true},
	} {
		got, err := szCommand(tc.paths, tc.deep)
		if tc.bad {
			if err == nil {
				t.Errorf("szCommand(%q) = %q, want an error", tc.paths, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("szCommand(%q): %v", tc.paths, err)
			continue
		}
		if got != tc.want {
			t.Errorf("szCommand(%q) = %q, want %q", tc.paths, got, tc.want)
		}
	}
	// A command line that would be truncated by the line discipline is
	// refused rather than silently losing files.
	many := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		many = append(many, fmt.Sprintf("a-long-file-name-number-%d.dat", i))
	}
	if _, err := szCommand(many, false); err == nil {
		t.Error("400 paths must be refused, not truncated by the tty")
	}
}

func TestParseOffer(t *testing.T) {
	// The metadata is decimal size, then octal mtime, then octal mode —
	// so the wire form of 1700000000 is its octal spelling, not its
	// decimal one.
	const when = int64(1700000000)
	o, extra, err := parseOffer([]byte(fmt.Sprintf("tree/a.txt\x001234 %o 100644\x00", when)))
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "tree/a.txt" || o.Size != 1234 {
		t.Errorf("offer = %+v", o)
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

	// A bare name with no metadata (sz of a special file, or a sender
	// that omits the length).
	o, extra, err = parseOffer([]byte("plain.txt\x00\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "plain.txt" || o.Size != 0 || o.HasMode {
		t.Errorf("offer = %+v", o)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v", extra)
	}

	// A multi-name ZFILE: the remaining names ride along.
	_, extra, err = parseOffer([]byte("first\x0010 0 100644\x00second\x00third\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(extra, ",") != "second,third" {
		t.Errorf("extra = %v, want second and third", extra)
	}

	if _, _, err := parseOffer([]byte("\x00")); err == nil {
		t.Error("a ZFILE without a name must be refused")
	}
	// A name that happens to start with digits is still a name.
	o, _, err = parseOffer([]byte("2024-report.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "2024-report.txt" || o.Size != 0 {
		t.Errorf("offer = %+v", o)
	}
}

// TestCancelDuringArmWindowGivesTheKeysBack: the arm timeout is not the
// only thing that can end a session that never spoke. A cancel from the
// panel ends it the same way, and the keystrokes it swallowed are just
// as much the user's.
func TestCancelDuringArmWindowGivesTheKeysBack(t *testing.T) {
	r, _ := newReceiver(t, testOpts{arm: 30 * time.Second})
	if err := r.Start([]string{"nope"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if typed := r.Keys([]byte("ls\r")); typed != nil {
		t.Errorf("keys = %q, want them held during the arm window", typed)
	}
	r.Cancel()
	res := waitResult(t, r)
	if res.Status != StatusCanceled {
		t.Fatalf("status = %s (%s), want canceled", res.Status, res.Reason)
	}
	if got := string(r.Replay()); got != "ls\r" {
		t.Errorf("replayed keys = %q, want the typed ls", got)
	}
}

// TestDownloadBatchIsCapped: sz takes its list from a shell glob, so a
// mistyped one can mean tens of thousands of names landing in the job's
// staging directory.
func TestDownloadBatchIsCapped(t *testing.T) {
	dir := t.TempDir()
	r, _ := newReceiver(t, testOpts{dir: dir})
	if err := r.Start([]string{"tree"}, false); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.filesGot = maxDownloadFiles
	feed(t, r, batch(false, batchFile{name: "one.txt", chunks: [][]byte{[]byte("x")}}))
	res := waitResult(t, r)
	if res.Status != StatusError || res.Reason != "too-many-files" {
		t.Fatalf("status = %s (%s), want an error capped by file count", res.Status, res.Reason)
	}
}

// TestAvoidClashRefusesToGuess pins the two answers that are not guesses:
// a free name, and a counter on a taken one. Anything else — a probe
// that failed for a reason other than "absent", a name with no counter
// left — has to be an error, because the call site opens with O_TRUNC.
func TestAvoidClashRefusesToGuess(t *testing.T) {
	dir := t.TempDir()
	free := filepath.Join(dir, "a.txt")
	if got, err := avoidClash(free); err != nil || got != free {
		t.Fatalf("avoidClash(free) = %q, %v", got, err)
	}
	if err := os.WriteFile(free, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := avoidClash(free)
	if err != nil || got == free || !strings.HasSuffix(got, " (2).txt") {
		t.Fatalf("avoidClash(taken) = %q, %v", got, err)
	}

	// A regular file where the path expects a directory: every probe of
	// that subtree fails with ENOTDIR, which is not "nothing is there".
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := avoidClash(filepath.Join(blocker, "inside.txt")); err == nil {
		t.Errorf("an un-probeable name was answered as %q with no error", got)
	}
}
