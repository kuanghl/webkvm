package sshmachines

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The host card is only useful if the in-process session behaves like the
// remote one, because every guarantee the transfer engine makes (atomic
// replace, mode re-assertion, no scratch files left behind) is written
// against sftpFS and is never reached by a local-only code path.
func TestLocalSFTPServesTheRealFilesystem(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plain.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	cli, closer, err := dialLocalSFTP()
	if err != nil {
		t.Fatalf("dial local: %v", err)
	}
	defer func() { _ = closer.Close() }()
	fs := sftpBridge{cli}

	// The browser's "home" comes from Getwd; pinning it to / keeps the
	// host card at the filesystem root whatever cwd the binary started in.
	if wd, err := cli.Getwd(); err != nil || wd != "/" {
		t.Errorf("Getwd = %q, %v; want /", wd, err)
	}

	if err := putStream(context.Background(), fs, filepath.Join(dir, "new.txt"),
		strings.NewReader("payload"), putOpts{JobID: "j1", Mode: 0o640}); err != nil {
		t.Fatalf("putStream: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "payload" {
		t.Errorf("content = %q", b)
	}
	st, err := os.Stat(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 640 (putStream re-asserts it after the write)", st.Mode().Perm())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"+partSuffix("j1")+"*"))
	if len(left) != 0 {
		t.Errorf("scratch files left behind: %v", left)
	}

	// Overwriting an existing file is the case the rename matters for.
	if err := putStream(context.Background(), fs, filepath.Join(dir, "plain.txt"),
		strings.NewReader("replaced"), putOpts{JobID: "j2"}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "plain.txt")); err != nil || string(b) != "replaced" {
		t.Errorf("overwrite = %q, %v", b, err)
	}

	infos, err := fs.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, i := range infos {
		names = append(names, i.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "link,new.txt,plain.txt" {
		t.Errorf("names = %v", names)
	}

	// A symlink must arrive as a symlink: dirwalk reads these with Lstat
	// semantics and never follows one.
	for _, i := range infos {
		if i.Name() == "link" && i.Mode()&os.ModeSymlink == 0 {
			t.Errorf("link reported as %v", i.Mode())
		}
	}
	if target, err := fs.ReadLink(filepath.Join(dir, "link")); err != nil || target != "plain.txt" {
		t.Errorf("ReadLink = %q, %v", target, err)
	}

	if err := fs.MkdirAll(filepath.Join(dir, "a", "b")); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if fi, err := fs.Stat(filepath.Join(dir, "a", "b")); err != nil || !fi.IsDir() {
		t.Errorf("Stat nested dir = %v, %v", fi, err)
	}
	if err := fs.Chmod(filepath.Join(dir, "a", "b"), 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a", "b")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("nested mode after Chmod = %v, %v", fi.Mode().Perm(), err)
	}
	if err := fs.Chtimes(filepath.Join(dir, "new.txt"), time.Now(), time.Unix(1000000, 0)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil || fi.ModTime().Unix() != 1000000 {
		t.Errorf("mtime after Chtimes = %v, %v", fi.ModTime(), err)
	}

	if err := fs.RemoveAll(dir + "/a"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Errorf("RemoveAll left %v behind", err)
	}
}

// A canceled job is force-dropped by the watchdog; the local session has
// to die with it instead of leaving a Serve goroutine wedged on a pipe
// nobody reads.
func TestLocalSFTPCloseUnblocksTheServer(t *testing.T) {
	cli, closer, err := dialLocalSFTP()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.ReadDir("/"); err != nil {
		t.Fatalf("read before close: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = closer.Close()
		_ = closer.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the local session blocked")
	}
	if _, err := cli.ReadDir("/"); err == nil {
		t.Fatal("client still works after close")
	}
}

// The card is this machine, not a record: the registry has to answer it
// with nothing in the store, and still pool and lease it like a dialed
// session — otherwise a browser's click stream opens a pipe pair per
// request, and a long local walk loses its session to the reaper.
func TestRegistryPoolsAndLeasesTheHostCard(t *testing.T) {
	r := NewSFTPSessionRegistry(nil)
	defer r.CloseAll()
	defer r.Stop()

	_, release1, err := r.Acquire(HostMachineID)
	if err != nil {
		t.Fatalf("acquire host: %v", err)
	}
	fs, release2, err := r.Acquire(HostMachineID)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer release2()

	r.mu.Lock()
	e := r.sessions[HostMachineID]
	r.mu.Unlock()
	if e == nil {
		t.Fatal("the host session is not pooled")
	}
	if got := e.refs.Load(); got != 2 {
		t.Fatalf("refs = %d, want 2 (one per Acquire)", got)
	}
	if _, err := fs.Stat("/"); err != nil {
		t.Fatalf("Stat(/) = %v", err)
	}

	release1()
	if got := e.refs.Load(); got != 1 {
		t.Fatalf("refs after one release = %d, want 1", got)
	}

	r.Drop(HostMachineID)
	r.mu.Lock()
	gone := r.sessions[HostMachineID] == nil
	r.mu.Unlock()
	if !gone {
		t.Fatal("Drop left the host session pooled")
	}
	if _, _, err := r.Acquire(HostMachineID); err != nil {
		t.Fatalf("acquire after Drop: %v", err)
	}
}
