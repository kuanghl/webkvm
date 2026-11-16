package sshmachines

// Tests for ZMODEM upload staging: what the browser leaves on the host
// before a terminal agrees to send it. The protocol is covered in
// internal/sshmachines/zmodem; what matters here is the shape of the
// holding area — names the remote can create, one queue row per batch, a
// directory that never outlives its claim, and no way for one machine to
// read another's files.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"webkvm/internal/sshmachines/zmodem"
)

func sendFixture(t *testing.T) (*TransferManager, string, string) {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "machines.json"), filepath.Join(dir, "secrets.json"))
	router, err := store.Create(CreateRequest{
		Name: "router", Protocol: ProtocolTelnet, Host: "10.0.0.1", Port: 23, AuthType: AuthNone,
	})
	if err != nil {
		t.Fatalf("create telnet machine: %v", err)
	}
	sw, err := store.Create(CreateRequest{
		Name: "switch", Protocol: ProtocolSerial, Host: "/dev/ttyUSB0", Port: 9600, AuthType: AuthNone,
	})
	if err != nil {
		t.Fatalf("create serial machine: %v", err)
	}
	return NewTransferManager(store, nil, nil, dir), router.ID, sw.ID
}

func stageOne(t *testing.T, m *TransferManager, machineID, name string, body []byte) zmodem.SendFile {
	t.Helper()
	st, err := m.StageZmodemUpload(machineID)
	if err != nil {
		t.Fatalf("StageZmodemUpload: %v", err)
	}
	f, err := st.Add(name, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Add %q: %v", name, err)
	}
	return f
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The name in the descriptor is the only thing the remote gets to go on:
// a browser path, a folder drop or a paste of an absolute path must all
// arrive as one plain name.
func TestStagedNamesAreWhatTheRemoteCanCreate(t *testing.T) {
	for in, want := range map[string]string{
		"notes.txt":                "notes.txt",
		"/home/me/notes.txt":       "notes.txt",
		"C:\\Users\\me\\notes.txt": "notes.txt",
		`src\main.go`:              "main.go",
		"src/main.go":              "main.go",
		"../escape":                "escape",
		"../../etc/passwd":         "passwd",
		"..":                       "unnamed",
		".":                        "unnamed",
		"":                         "unnamed",
		"   ":                      "unnamed",
		"weird\x01name\x00here":    "weird_name_here",
		"trailing.":                "trailing",
		"a:b?q*":                   "a_b_q_",
		"dir/":                     "dir",
		strings.Repeat("n", 300):   strings.Repeat("n", maxStagedName),
		".hidden":                  ".hidden",
		"script.sh ":               "script.sh",
	} {
		if got := cleanStagedName(in); got != want {
			t.Errorf("cleanStagedName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Two files called notes.txt from different folders are the common case
// of a dropped directory, and the second must not overwrite the first.
// The counter belongs to the stem it started from, so a file that arrives
// already named "notes (2).txt" gets its own suffix rather than the next
// number in the run.
func TestStagedNamesAccumulateInsteadOfColliding(t *testing.T) {
	m, id, _ := sendFixture(t)
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"a/notes.txt", "b/notes.txt", "c/notes.txt", "notes (2).txt"} {
		if _, err := st.Add(in, strings.NewReader("x")); err != nil {
			t.Fatalf("Add %q: %v", in, err)
		}
	}
	want := []string{"notes.txt", "notes (2).txt", "notes (3).txt", "notes (2) (2).txt"}
	st.mu.Lock()
	got := make([]string, 0, len(st.files))
	for _, f := range st.files {
		got = append(got, f.Name)
	}
	st.mu.Unlock()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("staged as %v, want %v", got, want)
	}
}

// Nothing a request can name may put a byte outside the stage.
func TestStagedWritesStayInsideTheDirectory(t *testing.T) {
	m, id, _ := sendFixture(t)
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../../../../etc/cron.d/evil", "..", "/etc/passwd", "a/../../b.txt"} {
		f, err := st.Add(name, strings.NewReader("nope"))
		if err != nil {
			t.Fatalf("Add %q: %v", name, err)
		}
		if f.Name != filepath.Base(f.Name) || strings.ContainsAny(f.Name, `/\`) {
			t.Fatalf("%q kept a separator: %q", name, f.Name)
		}
		if filepath.Dir(f.Path) != st.Dir {
			t.Fatalf("%q landed at %q, outside the stage %q", name, f.Path, st.Dir)
		}
	}
}

func TestStageRefusesMoreThanABatch(t *testing.T) {
	m, id, _ := sendFixture(t)
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxStagedBatch; i++ {
		if _, err := st.Add("f.dat", strings.NewReader("x")); err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	if _, err := st.Add("f.dat", strings.NewReader("x")); err == nil {
		t.Fatalf("a batch of %d files must not take one more", MaxStagedBatch)
	}
}

// The staged bytes are what gets sent: a truncated copy or a swapped
// reader is the kind of bug that only shows up as a corrupt file far away.
func TestStagedBytesAreExactlyWhatWasRead(t *testing.T) {
	body := bytes.Repeat([]byte("stage me "), 400) // 3600 bytes
	m, id, _ := sendFixture(t)
	f := stageOne(t, m, id, "payload.bin", body)
	if f.Size != int64(len(body)) {
		t.Fatalf("staged size = %d, want %d", f.Size, len(body))
	}
	got, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the staged copy differs from what was posted")
	}
	// The host copy is private even though the descriptor says 0644: the
	// mode the remote is told and the mode we keep are different questions.
	fi, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged file mode = %v, want 0600", fi.Mode().Perm())
	}
	if f.Mode != 0o644 {
		t.Fatalf("descriptor mode = %v, want 0644", f.Mode)
	}
	// A part that fails halfway must not stay behind as a short file.
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	// after=3 with a longer body is a channel that drops mid-transfer;
	// one shorter than the mark would reach EOF first and succeed.
	if _, err := st.Add("broken.bin",
		&errReader{
			r:     bytes.NewReader(bytes.Repeat([]byte("x"), 100)),
			err:   io.ErrUnexpectedEOF,
			after: 3,
		}); err == nil {
		t.Fatal("a failing reader must not stage a file")
	}
	if exists(filepath.Join(st.Dir, "broken.bin")) {
		t.Fatal("a half-written part survived its error")
	}
}

func TestBeginZmodemSendRegistersOneUploadRow(t *testing.T) {
	m, id, _ := sendFixture(t)
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"one.txt", "two.txt"} {
		if _, err := st.Add(n, strings.NewReader("1234567890")); err != nil {
			t.Fatal(err)
		}
	}
	sink, err := m.BeginZmodemSend(id, st.ID)
	if err != nil {
		t.Fatalf("BeginZmodemSend: %v", err)
	}
	if len(sink.Files) != 2 {
		t.Fatalf("the sink carries %d files, want 2", len(sink.Files))
	}
	jobs := m.List(id)
	if len(jobs) != 1 {
		t.Fatalf("List = %d jobs, want one row per batch", len(jobs))
	}
	row := jobs[0]
	if row["kind"] != "upload" || row["status"] != TransferRunning {
		t.Fatalf("row = %v/%v, want a running upload", row["kind"], row["status"])
	}
	if row["total"].(int64) != 20 {
		t.Fatalf("total = %v, want the whole batch", row["total"])
	}
	if row["files_total"] != 2 {
		t.Fatalf("files_total = %v, want 2", row["files_total"])
	}
	if row["file_name"] != "one.txt" {
		t.Fatalf("file_name = %v", row["file_name"])
	}
	// rz writes into the remote shell's cwd, which this end cannot see:
	// the label must say names and count, and invent no path.
	if !strings.Contains(row["remote_path"].(string), "one.txt") ||
		strings.HasPrefix(row["remote_path"].(string), "/") {
		t.Fatalf("remote_path = %v, want the names without a made-up directory", row["remote_path"])
	}
	// No LocalFile: /result must have nothing to serve for an upload, and
	// Remove must not be able to delete a directory it does not own.
	if sink.t.LocalFile != "" {
		t.Fatalf("LocalFile = %q, want empty for an upload", sink.t.LocalFile)
	}
	// A lone file must not grow a "1 of 1" counter.
	st2, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Add("only.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	sink2, err := m.BeginZmodemSend(id, st2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink2.t.Snapshot()["files_total"]; got != nil {
		t.Fatalf("single-file upload reports files_total %v, want unset", got)
	}
}

func TestBeginZmodemSendRefusesBadBatches(t *testing.T) {
	m, id, other := sendFixture(t)

	if _, err := m.BeginZmodemSend(id, "no-such-stage"); err == nil {
		t.Fatal("an unknown stage must not start a job")
	}
	if _, err := m.BeginZmodemSend("no-such-machine", "x"); err == nil {
		t.Fatal("an unknown machine must not start a job")
	}

	// Another machine may not claim this batch: the files are someone's
	// upload, and the stage id is a uuid a retry could stumble on.
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("secret.txt", strings.NewReader("not yours")); err != nil {
		t.Fatal(err)
	}
	sink, err := m.BeginZmodemSend(other, st.ID)
	if err == nil {
		t.Fatal("cross-machine claim of a stage must be refused")
	}
	if sink != nil {
		t.Fatal("a refused claim must not hand out a sink")
	}
	if exists(st.Dir) {
		t.Fatal("the refused stage stayed on the host")
	}
	if len(m.List(other)) != 0 || len(m.List(id)) != 0 {
		t.Fatal("a refused claim must not register a job for anyone")
	}

	// An empty batch is a request that posted no parts.
	empty, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginZmodemSend(id, empty.ID); err == nil {
		t.Fatal("an empty stage must not start a job")
	}
	if exists(empty.Dir) {
		t.Fatal("an empty stage stayed on the host after being refused")
	}
}

func TestSendClaimIsOnceOnly(t *testing.T) {
	m, id, _ := sendFixture(t)
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginZmodemSend(id, st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginZmodemSend(id, st.ID); err == nil {
		t.Fatal("one stage must not drive two uploads")
	}
	// Posting more parts after the batch started would move files the
	// remote is already receiving.
	if _, err := st.Add("late.txt", strings.NewReader("late")); err == nil {
		t.Fatal("a claimed stage must refuse new parts")
	}
}

// The staging directory is the one piece of this that must never
// outlive the transfer, on either ending.
func TestSendSinkFinishRemovesTheStage(t *testing.T) {
	for _, status := range []string{TransferDone, TransferError, TransferCanceled} {
		m, id, _ := sendFixture(t)
		st, err := m.StageZmodemUpload(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Add("a.txt", strings.NewReader("body")); err != nil {
			t.Fatal(err)
		}
		sink, err := m.BeginZmodemSend(id, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		sink.Progress(4, 4, "a.txt", 0, 1)
		sink.Finish(status, "boom", []string{"a.txt"})

		job := m.Get(sink.ID())
		if job.Status() != status {
			t.Fatalf("%s: status = %s", status, job.Status())
		}
		if exists(sink.Dir) {
			t.Fatalf("%s: the staging directory survived", status)
		}
		// The progress the session reported stands: Finish of an upload
		// has no local bytes to recount, and zeroing the bar would read
		// as a failed transfer either way.
		snap := job.Snapshot()
		if snap["done"].(int64) != 4 || snap["total"].(int64) != 4 {
			t.Fatalf("%s: counters = %v/%v", status, snap["done"], snap["total"])
		}
		if v, ok := snap["current_path"]; ok && v != "" {
			t.Fatalf("%s: current_path = %v, want nothing in flight", status, v)
		}
	}
}

func TestUnclaimedStageExpires(t *testing.T) {
	m, id, _ := sendFixture(t)
	m.stageTTL = 40 * time.Millisecond
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("never-sent.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for exists(st.Dir) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if exists(st.Dir) {
		t.Fatal("an abandoned batch outlived its deadline")
	}
	m.mu.Lock()
	_, live := m.stages[st.ID]
	m.mu.Unlock()
	if live {
		t.Fatal("an expired stage is still claimable")
	}
	if _, err := m.BeginZmodemSend(id, st.ID); err == nil {
		t.Fatal("an expired stage must not start a job")
	}
}

// A batch that is mid-flight must not be pulled out from under the
// sender because the timer that was armed before the claim went off.
func TestClaimedStageOutlivesItsDeadline(t *testing.T) {
	m, id, _ := sendFixture(t)
	m.stageTTL = 40 * time.Millisecond
	st, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add("in-flight.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	sink, err := m.BeginZmodemSend(id, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if !exists(sink.Dir) {
		t.Fatal("the deadline deleted a batch that was already sending")
	}
	sink.Finish(TransferDone, "", []string{"in-flight.txt"})
	if exists(sink.Dir) {
		t.Fatal("Finish left the batch behind")
	}
}

func TestDropStagedUploadsClearsUnclaimedBatches(t *testing.T) {
	m, id, other := sendFixture(t)
	a, err := m.StageZmodemUpload(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add("a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	b, err := m.StageZmodemUpload(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add("b.txt", strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	running, err := m.BeginZmodemSend(other, b.ID)
	if err != nil {
		t.Fatal(err)
	}

	m.DropStagedUploads()
	if exists(a.Dir) {
		t.Fatal("shutdown left an unclaimed batch on the host")
	}
	// The job that claimed its directory still owns it: dropping it here
	// would pull the files out from under a running sender.
	if !exists(running.Dir) {
		t.Fatal("shutdown deleted a batch that is being sent")
	}
	running.Finish(TransferDone, "", nil)
	if exists(running.Dir) {
		t.Fatal("the running batch's own job failed to clean up")
	}
}
