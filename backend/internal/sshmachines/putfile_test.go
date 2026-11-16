package sshmachines

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

// errReader yields n bytes and then fails, the way a dropped SSH channel
// does mid-transfer.
type errReader struct {
	r     *bytes.Reader
	err   error
	sent  int
	after int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.sent >= e.after {
		return 0, e.err
	}
	n, err := e.r.Read(p)
	e.sent += n
	return n, err
}

func TestPutStreamWritesContentModeAndMtime(t *testing.T) {
	dst := newMemFS()
	dst.writeFile(t, "/srv", "", os.ModeDir|0o755)
	mod := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	var counted int64
	err := putStream(context.Background(), dst, "/srv/new.txt", strings.NewReader("payload"), putOpts{
		JobID:   "job1",
		Mode:    0o640,
		ModTime: mod,
		OnBytes: func(d int64) { counted += d },
	})
	if err != nil {
		t.Fatalf("putStream: %v", err)
	}
	n := dst.get("/srv/new.txt")
	if n == nil {
		t.Fatalf("destination never appeared: %v", dst.paths())
	}
	if string(n.data) != "payload" {
		t.Fatalf("content = %q", n.data)
	}
	// The mode has to be asserted after the create: OpenFile carries no
	// perm argument and the server umask would otherwise eat the bits.
	if n.mode.Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", n.mode.Perm())
	}
	if !n.mod.Equal(mod) {
		t.Fatalf("mtime = %v, want %v", n.mod, mod)
	}
	if counted != int64(len("payload")) {
		t.Fatalf("OnBytes total = %d, want %d", counted, len("payload"))
	}
	if got := dst.parts(); len(got) != 0 {
		t.Fatalf("scratch files left behind: %v", got)
	}
}

func TestPutStreamReplacesAnExistingDestinationAtomically(t *testing.T) {
	dst := newMemFS()
	dst.writeFile(t, "/srv/x.txt", "old-content-that-is-longer", 0o600)

	// posix-rename is available: the swap cannot be observed as a gap.
	if err := putStream(context.Background(), dst, "/srv/x.txt", strings.NewReader("new"), putOpts{JobID: "j"}); err != nil {
		t.Fatalf("putStream: %v", err)
	}
	if n := dst.get("/srv/x.txt"); n == nil || string(n.data) != "new" {
		t.Fatalf("destination not replaced: %+v", n)
	}
	if len(dst.parts()) != 0 {
		t.Fatalf("scratch left behind: %v", dst.parts())
	}
}

func TestPutStreamWithoutPosixRename(t *testing.T) {
	dst := newMemFS()
	dst.noPosix = true
	dst.writeFile(t, "/srv/x.txt", "old", 0o600)

	if err := putStream(context.Background(), dst, "/srv/x.txt", strings.NewReader("fresh"), putOpts{JobID: "j"}); err != nil {
		t.Fatalf("putStream with the rename fallback: %v", err)
	}
	if n := dst.get("/srv/x.txt"); n == nil || string(n.data) != "fresh" {
		t.Fatalf("fallback path did not publish: %+v", n)
	}
	if len(dst.parts()) != 0 {
		t.Fatalf("scratch left behind: %v", dst.parts())
	}
}

func TestPutStreamRemovesScratchWhenTheSourceBreaks(t *testing.T) {
	dst := newMemFS()
	dst.writeFile(t, "/srv", "", os.ModeDir|0o755)
	// Pre-existing destination must survive a failed write untouched.
	dst.writeFile(t, "/srv/keep.txt", "original", 0o644)

	src := &errReader{r: bytes.NewReader([]byte("abcdefghij")), err: errors.New("connection reset"), after: 3}
	err := putStream(context.Background(), dst, "/srv/keep.txt", src, putOpts{JobID: "job9"})
	if err == nil {
		t.Fatalf("want the source error to surface")
	}
	if n := dst.get("/srv/keep.txt"); n == nil || string(n.data) != "original" {
		t.Fatalf("a failed write clobbered the destination: %+v", n)
	}
	if got := dst.parts(); len(got) != 0 {
		t.Fatalf("failed write left %v behind", got)
	}
}

func TestPutStreamHonorsContext(t *testing.T) {
	dst := newMemFS()
	dst.writeFile(t, "/srv", "", os.ModeDir|0o755)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := putStream(ctx, dst, "/srv/x.txt", strings.NewReader("data"), putOpts{JobID: "j"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if dst.get("/srv/x.txt") != nil {
		t.Fatalf("a canceled write still published its destination")
	}
}

func TestPutStreamCreatesUnderAMissingDir(t *testing.T) {
	dst := newMemFS()
	err := putStream(context.Background(), dst, "/nope/x.txt", strings.NewReader("d"), putOpts{JobID: "j"})
	if err == nil {
		t.Fatalf("want an error when the parent directory is missing")
	}
	if got := dst.parts(); len(got) != 0 {
		t.Fatalf("scratch left behind: %v", got)
	}
}

func TestStreamChunkingAcrossSizes(t *testing.T) {
	for _, size := range []int{0, 1, 64 * 1024, 64*1024 + 7} {
		dst := newMemFS()
		if err := putStream(context.Background(), dst, "/x.bin", bytes.NewReader(bytes.Repeat([]byte("z"), size)), putOpts{JobID: "j"}); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		n := dst.get("/x.bin")
		if n == nil || len(n.data) != size {
			t.Fatalf("size %d: wrote %v bytes", size, func() int {
				if n == nil {
					return -1
				}
				return len(n.data)
			}())
		}
	}
}

func TestResolveConflictPolicies(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/d/taken.txt", "existing", 0o644)
	fs.writeFile(t, "/d", "", os.ModeDir|0o755)

	if p, act := resolveConflict(fs, "/d/taken.txt", OverwriteOverwrite); p != "/d/taken.txt" || act != conflictWrite {
		t.Fatalf("overwrite = %q/%v", p, act)
	}
	if p, act := resolveConflict(fs, "/d/taken.txt", OverwriteSkip); p != "/d/taken.txt" || act != conflictSkip {
		t.Fatalf("skip = %q/%v", p, act)
	}
	if p, act := resolveConflict(fs, "/d/taken.txt", OverwriteRename); p != "/d/taken (1).txt" || act != conflictWrite {
		t.Fatalf("rename = %q/%v, want the free slot", p, act)
	}
	// A name nobody has is not a conflict at all.
	if p, act := resolveConflict(fs, "/d/fresh.txt", OverwriteSkip); p != "/d/fresh.txt" || act != conflictWrite {
		t.Fatalf("missing target = %q/%v", p, act)
	}
}

func TestFreeNameKeepsDotfileNamesReadable(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/d", "", os.ModeDir|0o755)
	// The auto-rename shape is "name (n).ext", matching what uniterm
	// users already know; the counter has to see names minted earlier in
	// the same batch, which is why they are pre-created here.
	for _, n := range []string{".bashrc", ".bashrc (1)", "a.txt", "a (1).txt"} {
		fs.writeFile(t, "/d/"+n, "x", 0o644)
	}
	cases := map[string]string{
		"/d/.bashrc": "/d/.bashrc (2)",
		"/d/a.txt":   "/d/a (2).txt",
		// freeName is only ever called on a name that is already taken
		// (resolveConflict gates it), so the counter starts at (1).
		"/d/new.txt": "/d/new (1).txt",
	}
	for in, want := range cases {
		if got := freeName(fs, in); got != want {
			t.Fatalf("freeName(%q) = %q, want %q", in, got, want)
		}
	}
	// A leading dot belongs to the name: ".bashrc (1)", never " (1).bashrc".
	if got := freeName(fs, "/d/.profile"); got != "/d/.profile (1)" {
		t.Fatalf("dotfile rename = %q", got)
	}
}

func TestNormalizeOverwrite(t *testing.T) {
	for in, want := range map[string]string{
		"skip": OverwriteSkip, "rename": OverwriteRename, "overwrite": OverwriteOverwrite,
		"": OverwriteOverwrite, "nonsense": OverwriteOverwrite,
	} {
		if got := normalizeOverwrite(in); got != want {
			t.Fatalf("normalizeOverwrite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenameAtomicRejectsAMissingScratch(t *testing.T) {
	fs := newMemFS()
	fs.writeFile(t, "/d", "", os.ModeDir|0o755)
	if err := renameAtomic(fs, "/d/absent.part", "/d/x.txt"); err == nil {
		t.Fatalf("want an error when the scratch file does not exist")
	}
}

func TestJoinUnderRefusesToEscape(t *testing.T) {
	base := "/opt/webkvm/ssh-transfers/out/job"
	ok := []string{"", "a", "a/b", "a/b-c", "./a", "a/./b"}
	for _, rel := range ok {
		p, err := joinUnder(base, rel)
		if err != nil {
			t.Fatalf("joinUnder(%q) rejected: %v", rel, err)
		}
		if rel != "" && !strings.HasPrefix(p, base) {
			t.Fatalf("joinUnder(%q) = %q escaped the base", rel, p)
		}
	}
	for _, rel := range []string{"../evil", "a/../../evil", "/etc/passwd", "a/../.."} {
		if p, err := joinUnder(base, rel); err == nil {
			t.Fatalf("joinUnder(%q) = %q, want a refusal", rel, p)
		}
	}
}

func TestMemFSCoversTheSeam(t *testing.T) {
	// The fake must behave like the real client for the calls the engine
	// makes; these three invariants are the ones the transfer code relies
	// on and are easy to get subtly wrong in a stub.
	fs := newMemFS()
	fs.writeFile(t, "/a/b/c.txt", "12345", 0o644)
	if fi, err := fs.Stat("/a/b/c.txt"); err != nil || fi.Size() != 5 || fi.IsDir() {
		t.Fatalf("stat = %+v/%v", fi, err)
	}
	if entries, err := fs.ReadDir("/a/b"); err != nil || len(entries) != 1 {
		t.Fatalf("readdir = %+v/%v", entries, err)
	}
	f, err := fs.Open("/a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "12345" {
		t.Fatalf("read = %q/%v", got, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Open("/a/b"); err == nil {
		t.Fatalf("opening a directory must fail")
	}
	if err := fs.MkdirAll(path.Join("/a", "deep", "nest")); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if fi, err := fs.Lstat("/a/deep/nest"); err != nil || !fi.IsDir() {
		t.Fatalf("mkdirall did not create: %+v/%v", fi, err)
	}
}
