package sshmachines

// Tests for the ZMODEM sink: the job record a terminal-fed download
// leaves behind. The protocol itself is covered in internal/sshmachines/
// zmodem; what matters here is that whatever arrives through a telnet or
// serial line becomes a queue row the browser can already handle —
// single file for a lone flat file, tree for a batch, nothing on disk for
// a run that ended badly.

import (
	"os"
	"path/filepath"
	"testing"
)

func zmodemFixture(t *testing.T) (*TransferManager, string) {
	t.Helper()
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "machines.json"), filepath.Join(dir, "secrets.json"))
	machine, err := store.Create(CreateRequest{
		Name:     "router",
		Protocol: ProtocolTelnet,
		Host:     "10.0.0.1",
		Port:     23,
		AuthType: AuthNone,
	})
	if err != nil {
		t.Fatalf("create machine: %v", err)
	}
	return NewTransferManager(store, nil, nil, dir), machine.ID
}

func TestBeginZmodemRegistersARunningJob(t *testing.T) {
	m, id := zmodemFixture(t)
	sink, err := m.BeginZmodem(id, "/var/log/messages")
	if err != nil {
		t.Fatalf("BeginZmodem: %v", err)
	}
	if sink.Dir == "" {
		t.Fatal("sink has no landing directory")
	}
	if fi, err := os.Stat(sink.Dir); err != nil || !fi.IsDir() {
		t.Fatalf("landing directory missing: %v", err)
	}
	jobs := m.List(id)
	if len(jobs) != 1 {
		t.Fatalf("List = %d jobs, want 1", len(jobs))
	}
	row := jobs[0]
	if row["status"] != TransferRunning || row["kind"] != "download" {
		t.Fatalf("row = %v/%v, want running download", row["kind"], row["status"])
	}
	if row["file_name"] != "messages" {
		t.Fatalf("file_name = %v, want the basename of what was asked for", row["file_name"])
	}
	// A glob is not a name; the row must not pretend it is one.
	globSink, err := m.BeginZmodem(id, "/var/log/*.log")
	if err != nil {
		t.Fatalf("BeginZmodem glob: %v", err)
	}
	if got := globSink.t.FileName; got != "zmodem-receive" {
		t.Fatalf("glob job named %q", got)
	}
}

func TestBeginZmodemUnknownMachine(t *testing.T) {
	m, _ := zmodemFixture(t)
	if _, err := m.BeginZmodem("nope", "/x"); err == nil {
		t.Fatal("unknown machine must not start a job")
	}
}

func landFile(t *testing.T, dir, name string, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

// One flat file is the case the user notices: the browser must receive
// the file, not an archive wrapped around it.
func TestZmodemSingleFileCollapsesToTheJobPath(t *testing.T) {
	m, id := zmodemFixture(t)
	sink, err := m.BeginZmodem(id, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	file := landFile(t, sink.Dir, "notes.txt", "hello world")
	sink.Progress(11, 11, "notes.txt", 0, 1)
	sink.Finish(TransferDone, "", []string{file})

	job := m.Get(sink.ID())
	if job.Status() != TransferDone {
		t.Fatalf("status = %s", job.Status())
	}
	if job.IsDirJob() {
		t.Fatal("a lone file must not be reported as a tree")
	}
	if _, err := os.Stat(job.LocalFile); err != nil {
		t.Fatalf("result not where /result expects it: %v", err)
	}
	if _, err := os.Stat(sink.Dir); !os.IsNotExist(err) {
		t.Fatal("the job directory survived the collapse")
	}
	snap := job.Snapshot()
	if snap["file_name"] != "notes.txt" || snap["total"].(int64) != 11 {
		t.Fatalf("snapshot = %v", snap)
	}
	if snap["files_total"] != nil {
		t.Fatalf("single-file job must keep files_total unset, got %v", snap["files_total"])
	}
}

func TestZmodemBatchStaysATree(t *testing.T) {
	m, id := zmodemFixture(t)
	sink, err := m.BeginZmodem(id, "/var/log")
	if err != nil {
		t.Fatal(err)
	}
	a := landFile(t, sink.Dir, "messages", "aaa")
	b := landFile(t, sink.Dir, filepath.Join("nested", "boot.log"), "bb")
	sink.Finish(TransferDone, "", []string{a, b})

	job := m.Get(sink.ID())
	if !job.IsDirJob() {
		t.Fatal("a multi-file batch must stream as tar.gz")
	}
	snap := job.Snapshot()
	if snap["files_total"] != 2 || snap["total"].(int64) != 5 {
		t.Fatalf("snapshot = %v", snap)
	}
	if _, err := os.Stat(filepath.Join(job.LocalFile, "nested", "boot.log")); err != nil {
		t.Fatalf("tree was moved out from under the job: %v", err)
	}
}

// A run that ended badly leaves nothing: no row of bytes, no orphan tree.
func TestZmodemFailureCleansTheLandingDir(t *testing.T) {
	for _, status := range []string{TransferError, TransferCanceled} {
		m, id := zmodemFixture(t)
		sink, err := m.BeginZmodem(id, "big.iso")
		if err != nil {
			t.Fatal(err)
		}
		landFile(t, sink.Dir, "big.iso", "partial")
		sink.Finish(status, "boom", []string{filepath.Join(sink.Dir, "big.iso")})

		job := m.Get(sink.ID())
		if job.Status() != status {
			t.Fatalf("status = %s, want %s", job.Status(), status)
		}
		if job.Err() != "boom" {
			t.Fatalf("error = %q", job.Err())
		}
		if _, err := os.Stat(sink.Dir); !os.IsNotExist(err) {
			t.Fatalf("%s left the partial tree behind", status)
		}
	}
}

// The queue's cancel only reaches a ZMODEM session through the flag, so
// the sink has to expose it.
func TestZmodemCancelFlag(t *testing.T) {
	m, id := zmodemFixture(t)
	sink, err := m.BeginZmodem(id, "x")
	if err != nil {
		t.Fatal(err)
	}
	if sink.Cancelled() {
		t.Fatal("a fresh job is not canceled")
	}
	if err := m.Cancel(sink.ID()); err != nil {
		t.Fatal(err)
	}
	if !sink.Cancelled() {
		t.Fatal("Cancel did not reach the sink")
	}
}

// Progress is what the queue row shows mid-flight; the byte counters must
// be absolute (the receiver reports cumulative totals per batch) and the
// file counters must follow the batch.
func TestZmodemProgressIsCumulative(t *testing.T) {
	m, id := zmodemFixture(t)
	sink, err := m.BeginZmodem(id, "/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	sink.Progress(100, 400, "a", 1, 2)
	sink.Progress(250, 400, "b", 1, 2)
	snap := sink.t.Snapshot()
	if snap["done"].(int64) != 250 || snap["total"].(int64) != 400 {
		t.Fatalf("snapshot = %v", snap)
	}
	if snap["files_done"] != 1 || snap["files_total"] != 2 {
		t.Fatalf("file counters = %v/%v", snap["files_done"], snap["files_total"])
	}
	if snap["current_path"] != "b" {
		t.Fatalf("current_path = %v", snap["current_path"])
	}
	// A single-file batch never announces more than one name, so it must
	// not grow a file counter the UI would read as "1 of 1" forever.
	sink.Progress(400, 400, "b", 2, 1)
	if got := sink.t.Snapshot()["files_total"]; got != 2 {
		t.Fatalf("files_total regressed to %v", got)
	}
}

func TestZmodemDisplayNameFallbacks(t *testing.T) {
	for in, want := range map[string]string{
		"/var/log/messages": "messages",
		"messages":          "messages",
		"/var/log/*.log":    "zmodem-receive",
		"/var/log/":         "log",
		"":                  "zmodem-receive",
		"a?b":               "zmodem-receive",
	} {
		if got := zmodemDisplayName(in); got != want {
			t.Errorf("zmodemDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
