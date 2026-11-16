package sshmachines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copyFixture wires a manager over a fake session source with two
// registered SSH machines. This is the shape the plan needed sftpFS and
// sessionSource for: runCopy/runDownloadDir previously had no coverage at
// all because the only seam was a concrete *SFTPSessionRegistry.
func copyFixture(t *testing.T) (*TransferManager, *fakeSource, string, string) {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "machines.json"), filepath.Join(dir, "secrets.json"))
	a, err := store.Create(sshReq("alpha"))
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	b, err := store.Create(sshReq("beta"))
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}
	src := newFakeSource(a.ID, b.ID)
	m := NewTransferManagerSource(store, src, nil, filepath.Join(dir, "data"))
	return m, src, a.ID, b.ID
}

// waitJob blocks until the job leaves "running", failing the test after
// 5s. Every path out of runCopy/runDownloadDir sets a terminal status,
// so a timeout here means a worker hung or panicked.
func waitJob(t *testing.T, m *TransferManager, id string) *Transfer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tr := m.Get(id)
		if tr == nil {
			t.Fatalf("job %s vanished from the table", id)
		}
		if s := tr.Status(); s != TransferRunning {
			return tr
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s still running after 5s (status %s)", id, m.Get(id).Status())
	return nil
}

func waitCurrent(t *testing.T, tr *Transfer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tr.current() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job never reached file %q (stuck at %q)", want, tr.current())
}

func mustDone(t *testing.T, tr *Transfer) {
	t.Helper()
	if s := tr.Status(); s != TransferDone {
		t.Fatalf("status = %s, want done (err=%q)", s, tr.Err())
	}
}

// waitLeases polls the fake's lease counters down to zero. The deferred
// releases run after the worker has already set the terminal status, so
// reading them straight after waitJob would race the goroutine.
func waitLeases(t *testing.T, src *fakeSource, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		live := 0
		for _, id := range ids {
			live += src.live(id)
		}
		if live == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("leases never released on %v", ids)
}

func seedTree(t *testing.T, fs *memFS, root string) map[string]string {
	t.Helper()
	files := map[string]string{
		root + "/a.txt":          "alpha",
		root + "/sub/b.txt":      "bravo",
		root + "/sub/deep/c.bin": strings.Repeat("c", 100),
	}
	modes := map[string]os.FileMode{
		root + "/a.txt":          0o644,
		root + "/sub/b.txt":      0o600,
		root + "/sub/deep/c.bin": 0o750,
	}
	for p, data := range files {
		fs.writeFile(t, p, data, modes[p])
	}
	if err := fs.MkdirAll(root + "/empty"); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCopyTreeLandsOnTheDestinationMachine(t *testing.T) {
	m, src, a, b := copyFixture(t)
	aFS := src.fsFor(a)
	files := seedTree(t, aFS, "/data/app")

	tr, err := m.StartCopy(a, "/data/app", b, "/srv", "overwrite", true)
	if err != nil {
		t.Fatalf("StartCopy: %v", err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)

	bFS := src.fsFor(b)
	// The folder nests under the target directory; merging its contents
	// into /srv directly would be a data-loss-shaped surprise.
	for p, want := range files {
		dst := "/srv/app" + strings.TrimPrefix(p, "/data/app")
		n := bFS.get(dst)
		if n == nil {
			t.Fatalf("%s missing on the destination: %v", dst, bFS.paths())
		}
		if string(n.data) != want {
			t.Fatalf("%s = %q, want %q", dst, n.data, want)
		}
		if mode := aFS.get(p).mode.Perm(); n.mode.Perm() != mode {
			t.Fatalf("%s mode = %v, want %v", dst, n.mode.Perm(), mode)
		}
		if !n.mod.Equal(aFS.get(p).mod) {
			t.Fatalf("%s mtime = %v, want %v", dst, n.mod, aFS.get(p).mod)
		}
	}
	if fi, err := bFS.Lstat("/srv/app/empty"); err != nil || !fi.IsDir() {
		t.Fatalf("the empty subdirectory was dropped: %+v/%v", fi, err)
	}
	if parts := bFS.parts(); len(parts) != 0 {
		t.Fatalf("scratch files left on the destination: %v", parts)
	}

	snap := got.Snapshot()
	if snap["kind"] != "copy" || snap["dst_machine_id"] != b || snap["dst_machine"] != "beta" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap["dst_dir"] != "/srv" || snap["src_dir"] != "app" {
		t.Fatalf("snapshot paths = %q / %q", snap["dst_dir"], snap["src_dir"])
	}
	if snap["files_total"] != 3 || snap["files_done"] != 3 || snap["skipped"] != 0 {
		t.Fatalf("counters = %v/%v/%v, want 3/3/0", snap["files_total"], snap["files_done"], snap["skipped"])
	}
	if snap["total"].(int64) != int64(len("alpha")+len("bravo")+100) {
		t.Fatalf("byte total = %v", snap["total"])
	}
	if snap["done"] != snap["total"] {
		t.Fatalf("done = %v, total = %v: progress must finish at the byte total", snap["done"], snap["total"])
	}

	// The relay never spools to the webkvm host's disk.
	entries, derr := os.ReadDir(filepath.Join(m.dataDir, "ssh-transfers"))
	if derr == nil {
		for _, e := range entries {
			sub, _ := os.ReadDir(filepath.Join(m.dataDir, "ssh-transfers", e.Name()))
			if len(sub) != 0 {
				t.Fatalf("machine-to-machine copy wrote to the host: %v", e.Name())
			}
		}
	}
	// Leases are released on both ends, or the idle reaper would never
	// close either session.
	waitLeases(t, src, a, b)
}

func TestCopySingleFileIsNotNested(t *testing.T) {
	m, src, a, b := copyFixture(t)
	src.fsFor(a).writeFile(t, "/data/one.txt", "solo", 0o644)

	tr, err := m.StartCopy(a, "/data/one.txt", b, "/srv", "", false)
	if err != nil {
		t.Fatalf("StartCopy: %v", err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)

	n := src.fsFor(b).get("/srv/one.txt")
	if n == nil || string(n.data) != "solo" {
		t.Fatalf("file did not arrive: %v", src.fsFor(b).paths())
	}
	if src.fsFor(b).get("/srv/one.txt/one.txt") != nil {
		t.Fatalf("a single file got nested under its own name")
	}
	// The single-file denominator is published like a directory's: the
	// queue needs total to render the bar, and the file counter reads
	// files_total ?? 1 on the client, so a 1 renders exactly the row
	// the absent key used to imply.
	snap := got.Snapshot()
	if v, _ := snap["files_total"].(int); v != 1 {
		t.Fatalf("files_total = %v on a single-file job, want 1", snap["files_total"])
	}
	if v, _ := snap["total"].(int64); v != 4 {
		t.Fatalf("total = %v, want the file's 4 bytes (the bar's denominator)", snap["total"])
	}
}

func TestCopySkipPolicyLeavesExistingFilesAlone(t *testing.T) {
	m, src, a, b := copyFixture(t)
	seedTree(t, src.fsFor(a), "/data/app")
	dstFS := src.fsFor(b)
	dstFS.writeFile(t, "/srv/app/a.txt", "DO-NOT-TOUCH", 0o644)

	tr, err := m.StartCopy(a, "/data/app", b, "/srv", OverwriteSkip, true)
	if err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)

	if n := dstFS.get("/srv/app/a.txt"); string(n.data) != "DO-NOT-TOUCH" {
		t.Fatalf("skip policy overwrote the destination: %q", n.data)
	}
	if n := dstFS.get("/srv/app/sub/b.txt"); n == nil || string(n.data) != "bravo" {
		t.Fatalf("the non-conflicting file was not copied: %v", dstFS.paths())
	}
	snap := got.Snapshot()
	if snap["files_done"] != 3 {
		t.Fatalf("files_done = %v, want all three counted", snap["files_done"])
	}
	if snap["skipped"] != 1 {
		t.Fatalf("skipped = %v, want the one conflict", snap["skipped"])
	}
}

func TestCopyRenamePolicyKeepsBothCopies(t *testing.T) {
	m, src, a, b := copyFixture(t)
	seedTree(t, src.fsFor(a), "/data/app")
	dstFS := src.fsFor(b)
	dstFS.writeFile(t, "/srv/app/a.txt", "original", 0o644)

	tr, err := m.StartCopy(a, "/data/app", b, "/srv", OverwriteRename, true)
	if err != nil {
		t.Fatal(err)
	}
	mustDone(t, waitJob(t, m, tr.ID))

	if n := dstFS.get("/srv/app/a.txt"); n == nil || string(n.data) != "original" {
		t.Fatalf("rename policy clobbered the existing file: %+v", n)
	}
	if n := dstFS.get("/srv/app/a (1).txt"); n == nil || string(n.data) != "alpha" {
		t.Fatalf("the auto-renamed copy is missing: %v", dstFS.paths())
	}
	if got := dstFS.parts(); len(got) != 0 {
		t.Fatalf("scratch left behind: %v", got)
	}
}

func TestCopyCancelLeavesNoScratchOnTheDestination(t *testing.T) {
	m, src, a, b := copyFixture(t)
	aFS := src.fsFor(a)
	aFS.writeFile(t, "/data/app/first.txt", "1", 0o644)
	aFS.writeFile(t, "/data/app/second.txt", "22", 0o644)
	gate := aFS.blockOpen("/data/app/second.txt")

	tr, err := m.StartCopy(a, "/data/app", b, "/srv", "", true)
	if err != nil {
		t.Fatal(err)
	}
	waitCurrent(t, tr, "second.txt")
	if err := m.Cancel(tr.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(gate)

	got := waitJob(t, m, tr.ID)
	if s := got.Status(); s != TransferCanceled {
		t.Fatalf("status = %s (err %q), want canceled", s, got.Err())
	}
	dstFS := src.fsFor(b)
	if dstFS.get("/srv/app/first.txt") == nil {
		t.Fatalf("the file copied before the cancel disappeared")
	}
	if dstFS.get("/srv/app/second.txt") != nil {
		t.Fatalf("a canceled file was still published")
	}
	if got := dstFS.parts(); len(got) != 0 {
		t.Fatalf("cancel left scratch files behind: %v", got)
	}
	waitLeases(t, src, a, b)
}

func TestCopyWithoutPosixRenameStillPublishes(t *testing.T) {
	m, src, a, b := copyFixture(t)
	src.fsFor(a).writeFile(t, "/data/f.txt", "fresh", 0o644)
	dstFS := src.fsFor(b)
	dstFS.noPosix = true
	dstFS.writeFile(t, "/srv/f.txt", "stale-content", 0o600)

	tr, err := m.StartCopy(a, "/data/f.txt", b, "/srv", OverwriteOverwrite, true)
	if err != nil {
		t.Fatal(err)
	}
	mustDone(t, waitJob(t, m, tr.ID))

	if n := dstFS.get("/srv/f.txt"); n == nil || string(n.data) != "fresh" {
		t.Fatalf("the rename fallback did not replace the file: %v", dstFS.paths())
	}
	if got := dstFS.parts(); len(got) != 0 {
		t.Fatalf("the fallback left scratch behind: %v", got)
	}
}

func TestCopyPreservesSymlinksAsLinks(t *testing.T) {
	m, src, a, b := copyFixture(t)
	aFS := src.fsFor(a)
	aFS.writeFile(t, "/data/real.txt", "target", 0o644)
	if err := aFS.Symlink("/data/real.txt", "/data/link.txt"); err != nil {
		t.Fatal(err)
	}
	// A link that goes nowhere must not fail the whole job.
	if err := aFS.Symlink("/nowhere", "/data/dangling"); err != nil {
		t.Fatal(err)
	}

	tr, err := m.StartCopy(a, "/data", b, "/srv", "", true)
	if err != nil {
		t.Fatal(err)
	}
	mustDone(t, waitJob(t, m, tr.ID))

	dstFS := src.fsFor(b)
	target, lerr := dstFS.ReadLink("/srv/data/link.txt")
	if lerr != nil || target != "/data/real.txt" {
		t.Fatalf("link copied as %q/%v, want the link itself", target, lerr)
	}
	if dstFS.get("/srv/data/link.txt") == nil {
		t.Fatalf("the link node is missing")
	}
	if _, err := dstFS.ReadLink("/srv/data/dangling"); err != nil {
		t.Fatalf("the dangling link was not reproduced: %v", err)
	}
	// Nothing was dereferenced into a regular copy.
	if n := dstFS.get("/srv/data/link.txt"); n != nil && n.mode&os.ModeSymlink == 0 {
		t.Fatalf("the link became a regular file")
	}
}

func TestCopyRejectsNonSSHEndpoints(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "m.json"), filepath.Join(dir, "s.json"))
	ssh, err := store.Create(sshReq("sshbox"))
	if err != nil {
		t.Fatal(err)
	}
	tel, err := store.Create(CreateRequest{Name: "console", Protocol: ProtocolTelnet, Host: "10.0.0.9", Port: 23, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	m := NewTransferManagerSource(store, newFakeSource(ssh.ID, tel.ID), nil, dir)

	if _, err := m.StartCopy(tel.ID, "/x", ssh.ID, "/y", "", true); err == nil {
		t.Fatalf("want a rejection when the source is not ssh")
	}
	if _, err := m.StartCopy(ssh.ID, "/x", tel.ID, "/y", "", true); err == nil {
		t.Fatalf("want a rejection when the destination is not ssh")
	}
	if _, err := m.StartCopy(ssh.ID, "/x", "nope", "/y", "", true); err == nil {
		t.Fatalf("want a rejection for an unknown destination machine")
	}
	if _, err := m.StartCopy("nope", "/x", ssh.ID, "/y", "", true); err == nil {
		t.Fatalf("want a rejection for an unknown source machine")
	}
}

func TestCopyReportsAnAcquireFailure(t *testing.T) {
	m, src, a, b := copyFixture(t)
	src.fsFor(a).writeFile(t, "/data/f.txt", "x", 0o644)
	src.mu.Lock()
	src.acqErr[b] = errors.New("host key not trusted")
	src.mu.Unlock()

	tr, err := m.StartCopy(a, "/data/f.txt", b, "/srv", "", true)
	if err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, m, tr.ID)
	if s := got.Status(); s != TransferError {
		t.Fatalf("status = %s, want error", s)
	}
	if !strings.Contains(got.Err(), "not trusted") {
		t.Fatalf("error = %q, want the dial failure surfaced", got.Err())
	}
	// A failed dial must not leave the source lease pinned forever.
	waitLeases(t, src, a)
}

func TestCopyJobIsVisibleOnBothEndsWithoutDuplication(t *testing.T) {
	m, src, a, b := copyFixture(t)
	src.fsFor(a).writeFile(t, "/data/f.txt", "x", 0o644)
	tr, err := m.StartCopy(a, "/data/f.txt", b, "/srv", "", true)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, m, tr.ID)

	la, lb := m.List(a), m.List(b)
	if len(la) != 1 || la[0]["id"] != tr.ID {
		t.Fatalf("List(src) = %+v", la)
	}
	if len(lb) != 1 || lb[0]["id"] != tr.ID {
		t.Fatalf("List(dst) = %+v", lb)
	}
	// The global queue view must show one row, not one per end.
	if all := m.ListAll(0); len(all) != 1 {
		t.Fatalf("ListAll = %+v, want a single job", all)
	}
}

// The eviction cap is the dangerous one: dropping the wrong condition
// deletes the output tree of a job that is still running.
func seedDirJob(m *TransferManager, id, machineID string, started time.Time, status string) *Transfer {
	t := &Transfer{
		ID: id, MachineID: machineID, Machine: "alpha", Kind: "download",
		RemotePath: "/data/app", LocalFile: filepath.Join(m.outDir(), id),
		StartedAt: started, status: status, FilesTotal: 3, SrcDir: "app", kindDir: true,
	}
	_ = os.MkdirAll(filepath.Join(t.LocalFile, "tree"), 0o750)
	_ = os.WriteFile(filepath.Join(t.LocalFile, "tree", "f.txt"), []byte("payload"), 0o640)
	m.mu.Lock()
	m.transfers[id] = t
	m.mu.Unlock()
	return t
}

func TestRunningDirJobsSurviveTheCap(t *testing.T) {
	m, _, a, _ := copyFixture(t)
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 60; i++ {
		seedDirJob(m, fmt.Sprintf("dir-%02d", i), a, base.Add(time.Duration(i)*time.Minute), TransferDone)
	}
	running := seedDirJob(m, "running-dir", a, time.Now().UTC(), TransferRunning)

	got := m.List(a)
	if len(got) != 50 {
		t.Fatalf("List = %d, want the cap of 50", len(got))
	}
	if _, err := os.Stat(filepath.Join(running.LocalFile, "tree", "f.txt")); err != nil {
		t.Fatalf("the eviction sweep deleted a running job's tree: %v", err)
	}
	if m.Get(running.ID) == nil {
		t.Fatalf("the running job was evicted")
	}
	// Finished directory jobs lose their whole tree, not just a file.
	if _, err := os.Stat(filepath.Join(m.outDir(), "dir-00")); !os.IsNotExist(err) {
		t.Fatalf("an evicted directory job left %v behind", filepath.Join(m.outDir(), "dir-00"))
	}
}

func TestDownloadDirectoryProducesLocalTree(t *testing.T) {
	m, src, a, _ := copyFixture(t)
	files := seedTree(t, src.fsFor(a), "/data/app")

	tr, err := m.StartDownload(a, "/data/app", "app")
	if err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)

	got.mu.Lock()
	isDir := got.kindDir
	local := got.LocalFile
	got.mu.Unlock()
	if !isDir {
		t.Fatalf("a directory download must be flagged as a tree job")
	}
	for rel := range files {
		p := filepath.Join(local, strings.TrimPrefix(rel, "/data/app/"))
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatalf("missing %s: %v", p, rerr)
		}
		if string(data) != files[rel] {
			t.Fatalf("%s = %q, want %q", p, data, files[rel])
		}
	}
	if fi, err := os.Stat(filepath.Join(local, "empty")); err != nil || !fi.IsDir() {
		t.Fatalf("empty subdirectory not recreated locally: %+v/%v", fi, err)
	}
	if n := src.fsFor(a).get("/data/app/sub/deep/c.bin"); n != nil {
		if fi, err := os.Stat(filepath.Join(local, "sub/deep/c.bin")); err != nil {
			t.Fatalf("nested file missing: %v", err)
		} else if perm := fi.Mode().Perm(); perm != 0o750 {
			t.Fatalf("local mode = %v, want the remote 0750", perm)
		}
	}
	snap := got.Snapshot()
	if snap["files_total"] != 3 || snap["files_done"] != 3 {
		t.Fatalf("counters = %+v", snap)
	}
	if snap["src_dir"] != "app" || snap["file_name"] != "app" {
		t.Fatalf("snapshot = %+v, want src_dir/app naming for the tar", snap)
	}
	var debris []string
	_ = filepath.Walk(local, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.Contains(p, partSuffix("")) {
			debris = append(debris, p)
		}
		return nil
	})
	if len(debris) != 0 {
		t.Fatalf("scratch inside the local tree: %v", debris)
	}
}

func TestDownloadDirectoryCancelRemovesThePartialTree(t *testing.T) {
	m, src, a, _ := copyFixture(t)
	aFS := src.fsFor(a)
	aFS.writeFile(t, "/data/app/one.txt", "1", 0o644)
	aFS.writeFile(t, "/data/app/two.txt", "22", 0o644)
	gate := aFS.blockOpen("/data/app/two.txt")

	tr, err := m.StartDownload(a, "/data/app", "app")
	if err != nil {
		t.Fatal(err)
	}
	waitCurrent(t, tr, "two.txt")
	if err := m.Cancel(tr.ID); err != nil {
		t.Fatal(err)
	}
	close(gate)

	got := waitJob(t, m, tr.ID)
	if s := got.Status(); s != TransferCanceled {
		t.Fatalf("status = %s, want canceled", s)
	}
	// A canceled download must not leave a partial tree in outDir: the
	// browser can never fetch it, and /opt/webkvm has no other reaper.
	// The sweep is a defer in runDownload, so it lands just after the
	// canceled status is observable.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(got.LocalFile); os.IsNotExist(err) {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("partial tree left on disk: %v (%v)", got.LocalFile, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDownloadSingleFileKeepsLegacyKeys(t *testing.T) {
	m, src, a, _ := copyFixture(t)
	src.fsFor(a).writeFile(t, "/data/firmware.bin", strings.Repeat("z", 10), 0o644)

	tr, err := m.StartDownload(a, "/data/firmware.bin", "firmware.bin")
	if err != nil {
		t.Fatal(err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)

	snap := got.Snapshot()
	for _, k := range []string{"id", "machine_id", "machine", "kind", "remote_path", "file_name", "total", "done", "status", "error", "started_at"} {
		if _, ok := snap[k]; !ok {
			t.Fatalf("snapshot lost the shipped key %q: %+v", k, snap)
		}
	}
	if snap["done"].(int64) != 10 || snap["total"].(int64) != 10 {
		t.Fatalf("done/total = %v/%v", snap["done"], snap["total"])
	}
	got.mu.Lock()
	isDir, local := got.kindDir, got.LocalFile
	got.mu.Unlock()
	if isDir {
		t.Fatalf("a file download must not be flagged as a tree")
	}
	if fi, err := os.Stat(local); err != nil || fi.IsDir() {
		t.Fatalf("the flat result file is missing: %+v/%v", fi, err)
	}
}

func TestSweepOrphansEmptiesBothStagingDirs(t *testing.T) {
	m, _, a, _ := copyFixture(t)
	for _, d := range []string{m.tmpDir(), m.outDir()} {
		if err := os.MkdirAll(filepath.Join(d, "nested"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "left.part"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "nested", "old.bin"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := m.SweepOrphans()
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want the 4 entries in both trees", removed)
	}
	for _, d := range []string{m.tmpDir(), m.outDir()} {
		ents, rerr := os.ReadDir(d)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(ents) != 0 {
			t.Fatalf("%s still holds %d entries", d, len(ents))
		}
	}
	// A second sweep on a clean (or missing) directory is a no-op, not an
	// error: startup must be able to call it unconditionally.
	if err := os.RemoveAll(m.tmpDir()); err != nil {
		t.Fatal(err)
	}
	again, err := m.SweepOrphans()
	if err != nil || again != 0 {
		t.Fatalf("second sweep = %d/%v, want 0/nil", again, err)
	}
	// A live job's file would be wiped too, which is why startup is the
	// only caller: at that point the table is always empty.
	if m.List(a) == nil {
		t.Fatal("List must return an empty slice, not nil")
	}
}

// A batch download merges several roots into one local tree: every root
// lands under its own top-level entry named after its basename, deduped
// against the other roots (a second "logs" becomes "logs (2)"), and the
// /result endpoint packs the merged tree into the single tar.gz.
func TestDownloadBatchMergesRootsWithDedupedNames(t *testing.T) {
	m, src, a, _ := copyFixture(t)
	aFS := src.fsFor(a)
	aFS.writeFile(t, "/logs/app.log", "one", 0o644)
	aFS.writeFile(t, "/other/logs/app.log", "two", 0o644)
	aFS.writeFile(t, "/notes.txt", "three", 0o644)

	tr, err := m.StartDownloadBatch(a, []string{"/logs", "/other/logs", "/notes.txt"}, "selection")
	if err != nil {
		t.Fatalf("StartDownloadBatch: %v", err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)

	got.mu.Lock()
	isDir := got.kindDir
	local := got.LocalFile
	got.mu.Unlock()
	if !isDir {
		t.Fatal("a batch download must be flagged as a tree job")
	}
	// Directory roots keep their subtree under the entry; the file root
	// lands as the entry itself — no self-nesting, and dir/file roots
	// share the name space so neither can overwrite the other.
	for rel, want := range map[string]string{
		filepath.Join("logs", "app.log"):     "one",
		filepath.Join("logs (2)", "app.log"): "two",
		"notes.txt":                          "three",
	} {
		data, rerr := os.ReadFile(filepath.Join(local, rel))
		if rerr != nil {
			var names []string
			_ = filepath.Walk(local, func(p string, info os.FileInfo, err error) error {
				if err == nil && info != nil && !info.IsDir() {
					relp, _ := filepath.Rel(local, p)
					names = append(names, relp)
				}
				return nil
			})
			t.Fatalf("missing %s: %v (tree holds %v)", rel, rerr, names)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", rel, data, want)
		}
	}
	snap := got.Snapshot()
	if snap["files_total"] != 3 || snap["files_done"] != 3 {
		t.Fatalf("counters = %v/%v, want 3/3", snap["files_total"], snap["files_done"])
	}
	if snap["total"].(int64) != int64(3+3+5) {
		t.Fatalf("byte total = %v, want 11", snap["total"])
	}
	if snap["file_name"] != "selection" {
		t.Fatalf("file_name = %v, want the caller's archive name to survive", snap["file_name"])
	}
	if rp, ok := snap["remote_paths"].([]string); !ok || len(rp) != 3 || rp[0] != "/logs" || rp[1] != "/other/logs" || rp[2] != "/notes.txt" {
		t.Fatalf("remote_paths = %v, want the three cleaned roots", snap["remote_paths"])
	}
	if snap["remote_path"] != "" {
		t.Fatalf("remote_path = %v, want empty: there is no single root", snap["remote_path"])
	}
	waitLeases(t, src, a)
}

// The planner must not accept the filesystem root as a batch member:
// the single-root path is guarded at the API layer, this is the second
// line for a direct caller of the manager.
func TestDownloadBatchRefusesTheRoot(t *testing.T) {
	m, src, a, _ := copyFixture(t)
	src.fsFor(a).writeFile(t, "/x", "x", 0o644)
	if _, err := m.StartDownloadBatch(a, []string{"/", "/x"}, "all"); err == nil {
		t.Fatal("StartDownloadBatch([/]) must refuse")
	}
	if _, err := m.StartDownloadBatch(a, nil, "none"); err == nil {
		t.Fatal("StartDownloadBatch([]) must refuse")
	}
}
