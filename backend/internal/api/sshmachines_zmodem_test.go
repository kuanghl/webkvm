package api

// Tests for the terminal's ZMODEM gate: the part that decides what may be
// armed, what the queue sees, and what the browser is told. The protocol
// state machine itself is covered in internal/sshmachines/zmodem, so
// these stay focused on the wiring between a live terminal and a transfer
// job.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"webkvm/internal/audit"
	"webkvm/internal/sshmachines"
	"webkvm/internal/sshmachines/zmodem"
)

// terminalDouble collects what the gate writes: text frames on the
// WebSocket side, raw bytes on the remote side.
type terminalDouble struct {
	mu     sync.Mutex
	frames []string
	wrote  []byte
}

func (d *terminalDouble) writeWS(msgType int, data []byte) error {
	if msgType == websocket.TextMessage {
		d.mu.Lock()
		d.frames = append(d.frames, string(data))
		d.mu.Unlock()
	}
	return nil
}

func (d *terminalDouble) Write(p []byte) (int, error) {
	d.mu.Lock()
	d.wrote = append(d.wrote, p...)
	d.mu.Unlock()
	return len(p), nil
}

func (d *terminalDouble) typed() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return string(d.wrote)
}

func (d *terminalDouble) lastEvent(name string) map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.frames) - 1; i >= 0; i-- {
		var body map[string]any
		if json.Unmarshal([]byte(d.frames[i]), &body) == nil && body["event"] == name {
			return body
		}
	}
	return nil
}

func (d *terminalDouble) event(t *testing.T, name string) map[string]any {
	t.Helper()
	body := d.lastEvent(name)
	if body == nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		t.Fatalf("no zmodem %q frame among %v", name, d.frames)
	}
	return body
}

// zmodemHarness bundles one gate with the machine it belongs to and the
// queue it writes into, so a test reads like a scenario.
type zmodemHarness struct {
	gate    *zmodemGate
	f       *machineTransferFixture
	d       *terminalDouble
	machine string
}

func newZmodemHarness(t *testing.T, telnet bool) *zmodemHarness {
	t.Helper()
	f := newMachineTransferFixture(t)
	id := f.nova
	if !telnet {
		id = f.alpha
	}
	machine, ok := f.store.Get(id)
	if !ok {
		t.Fatalf("machine %s missing", id)
	}
	d := &terminalDouble{}
	return &zmodemHarness{gate: newZmodemGate(f.h, machine, d.writeWS, d), f: f, d: d, machine: id}
}

func (h *zmodemHarness) row(t *testing.T, jobID string) map[string]any {
	t.Helper()
	for _, j := range h.f.tm.List(h.machine) {
		if j["id"] == jobID {
			return j
		}
	}
	t.Fatalf("job %s is not in %s's queue", jobID, h.machine)
	return nil
}

func waitZmodem(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The tap sits on every byte of the terminal. Idle it must be a no-op:
// the same bytes, unchanged.
func TestZmodemGatePassesThroughWhenIdle(t *testing.T) {
	h := newZmodemHarness(t, true)
	if got := string(h.gate.feed([]byte("prompt$ "))); got != "prompt$ " {
		t.Fatalf("feed = %q, want the bytes untouched", got)
	}
	if got := string(h.gate.keys([]byte("ls\r"))); got != "ls\r" {
		t.Fatalf("keys = %q, want the bytes untouched", got)
	}
	if h.gate.active() {
		t.Fatal("a fresh gate reports an active session")
	}
}

func TestZmodemNilGateIsTransparent(t *testing.T) {
	var g *zmodemGate
	if string(g.feed([]byte("x"))) != "x" || string(g.keys([]byte("x"))) != "x" {
		t.Fatal("nil gate altered the stream")
	}
	if g.active() {
		t.Fatal("nil gate cannot be active")
	}
	g.abort() // must not panic
}

func TestZmodemGateRefusesSSHMachines(t *testing.T) {
	h := newZmodemHarness(t, false)
	err := h.gate.startDown([]string{"log.txt"}, false)
	if err == nil || !strings.Contains(err.Error(), "SFTP") {
		t.Fatalf("start on an ssh machine = %v, want the SFTP explanation", err)
	}
	if jobs := h.f.tm.List(h.machine); len(jobs) != 0 {
		t.Fatalf("a refused request still queued %d jobs", len(jobs))
	}
	if h.d.typed() != "" {
		t.Fatalf("a refused request still typed %q", h.d.typed())
	}
}

func TestZmodemPathsRejectsShellUnsafeInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
	}{
		{"empty", nil},
		{"blank", []string{"  "}},
		{"newline", []string{"a\nsz --foo"}},
		{"carriage return", []string{"a\rb"}},
		{"nul", []string{"a\x00b"}},
		{"too long", []string{strings.Repeat("x", 1025)}},
		{"too many", manyPaths(zmodemMaxPaths + 1)},
	} {
		if _, err := zmodemPaths(tc.in); err == nil {
			t.Errorf("%s: accepted %q", tc.name, tc.in)
		}
	}
	got, err := zmodemPaths([]string{" a.txt ", "", "b c.txt"})
	if err != nil {
		t.Fatalf("legitimate paths refused: %v", err)
	}
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "b c.txt" {
		t.Fatalf("zmodemPaths = %q", got)
	}
}

func manyPaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "f"
	}
	return out
}

func TestZmodemGateLifecycle(t *testing.T) {
	h := newZmodemHarness(t, true)
	if err := h.gate.startDown([]string{"/var/log/messages"}, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !h.gate.active() {
		t.Fatal("arming did not take over the terminal")
	}
	jobID, _ := h.d.event(t, "armed")["job_id"].(string)
	if jobID == "" {
		t.Fatal("the armed frame carries no job id")
	}
	if row := h.row(t, jobID); row["status"] != sshmachines.TransferRunning {
		t.Fatalf("queue row = %v, want running", row["status"])
	}
	if !strings.Contains(h.d.typed(), `sz -q '/var/log/messages'`) {
		t.Fatalf("the remote saw %q", h.d.typed())
	}
	if err := h.gate.startDown([]string{"other"}, false); err == nil {
		t.Fatal("two sessions were allowed on one terminal")
	}
	// Keystrokes are held while the protocol owns the line: the pump gets
	// nothing back to write.
	if got := h.gate.keys([]byte("ls\r")); len(got) != 0 {
		t.Fatalf("keys returned %q during the arm window", got)
	}
	if err := h.gate.cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitZmodem(t, func() bool { return !h.gate.active() }, "the terminal to be handed back")
	waitZmodem(t, func() bool {
		return h.row(t, jobID)["status"] == sshmachines.TransferCanceled
	}, "the canceled row")
	h.d.event(t, "canceled")

	// The queue's cancel path reaches the same job and must not strand it.
	if err := h.gate.startDown([]string{"second"}, false); err != nil {
		t.Fatalf("re-arm after cancel: %v", err)
	}
	id2, _ := h.d.event(t, "armed")["job_id"].(string)
	if err := h.f.tm.Cancel(id2); err != nil {
		t.Fatalf("queue cancel: %v", err)
	}
	waitZmodem(t, func() bool { return !h.gate.active() }, "the queue cancel to reach the session")
}

// A terminal that goes away mid-transfer must not leave the row running
// forever, nor the partial tree on disk.
func TestZmodemAbortOnTerminalClose(t *testing.T) {
	h := newZmodemHarness(t, true)
	if err := h.gate.startDown([]string{"big.iso"}, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	jobID, _ := h.d.event(t, "armed")["job_id"].(string)
	h.gate.mu.Lock()
	dir := h.gate.sink.Dir
	h.gate.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "big.iso"), []byte("half a file"), 0o600); err != nil {
		t.Fatalf("landing dir unusable: %v", err)
	}
	h.gate.abort()
	waitZmodem(t, func() bool {
		return h.row(t, jobID)["status"] == sshmachines.TransferCanceled
	}, "the row to close out")
	waitZmodem(t, func() bool { return !dirExists(dir) }, "the partial tree to be removed")
}

func dirExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestZmodemHandleReportsErrorsAsFrames covers the browser's view: a bad
// request answers on the socket instead of vanishing.
func TestZmodemHandleReportsErrorsAsFrames(t *testing.T) {
	h := newZmodemHarness(t, false)
	h.gate.handle(&machineTerminalFrame{Type: "zmodem", Paths: []string{"x"}})
	body := h.d.event(t, "error")
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "SFTP") {
		t.Fatalf("error frame = %v", body)
	}
	if body["type"] != "zmodem" {
		t.Fatalf("frame is not tagged as zmodem: %v", body)
	}
}

func TestZmodemJobStatusMapping(t *testing.T) {
	cases := []struct {
		in       zmodem.Result
		status   string
		mentions string
	}{
		{zmodem.Result{Status: zmodem.StatusDone}, sshmachines.TransferDone, ""},
		{zmodem.Result{Status: zmodem.StatusCanceled, Reason: "canceled"}, sshmachines.TransferCanceled, ""},
		{zmodem.Result{Status: zmodem.StatusDisarmed, Reason: "no-frame"}, sshmachines.TransferError, "lrzsz"},
		{zmodem.Result{Status: zmodem.StatusError, Reason: "bad-checksum"}, sshmachines.TransferError, "bad-checksum"},
		{zmodem.Result{Status: zmodem.StatusError, Err: errors.New("disk full")}, sshmachines.TransferError, "disk full"},
	}
	for _, c := range cases {
		status, msg := zmodemJobStatus(c.in)
		if status != c.status {
			t.Errorf("status(%q) = %q, want %q", c.in.Status, status, c.status)
		}
		if c.mentions != "" && !strings.Contains(msg, c.mentions) {
			t.Errorf("msg for %q = %q, want it to mention %q", c.in.Status, msg, c.mentions)
		}
	}
	if status, _ := zmodemJobStatus(zmodem.Result{Status: zmodem.StatusDone}); status != sshmachines.TransferDone {
		t.Fatal("a done session must not be reported as anything else")
	}
}

func TestZmodemNamesAreRelativeToTheJob(t *testing.T) {
	got := zmodemNames("/opt/webkvm/ssh-transfers/out/abc.in",
		[]string{"/opt/webkvm/ssh-transfers/out/abc.in/messages", "/opt/webkvm/ssh-transfers/out/abc.in/nested/boot.log"})
	if len(got) != 2 || got[0] != "messages" || got[1] != "nested/boot.log" {
		t.Fatalf("zmodemNames = %q", got)
	}
}

func TestLockedWriterSerialises(t *testing.T) {
	var mu sync.Mutex
	d := &terminalDouble{}
	w := &lockedWriter{mu: &mu, w: d}
	if _, err := w.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if !mu.TryLock() {
		t.Fatal("lockedWriter did not release the mutex")
	}
	mu.Unlock()
	// A nil mutex is the "this writer owns the only stream" case.
	if _, err := (&lockedWriter{w: d}).Write([]byte("cd")); err != nil {
		t.Fatal(err)
	}
	if d.typed() != "abcd" {
		t.Fatalf("wrote %q", d.typed())
	}
}

// ---- upload staging over HTTP ----

// stageForm builds the multipart body the browser posts before a terminal
// is asked to send anything.
func stageForm(t *testing.T, action string, files [][2]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("action", action); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		w, err := mw.CreateFormFile("file", f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), &buf
}

func (f *machineTransferFixture) stage(t *testing.T, machineID string, files [][2]string) *httptest.ResponseRecorder {
	t.Helper()
	ct, body := stageForm(t, "zmodem-stage", files)
	req := mtRequest(http.MethodPost, "/api/ssh-machines/"+machineID+"/transfers",
		map[string]string{"id": machineID}, body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	f.h.MachineTransferStart(rr, req)
	return rr
}

// Staging holds the bytes under a fresh id, renames a collision instead of
// overwriting it, and tells the browser nothing about where on the host the
// files are.
func TestZmodemStageHoldsTheBatch(t *testing.T) {
	f := newMachineTransferFixture(t)
	auditFile := filepath.Join(t.TempDir(), "audit.log")
	aud, err := audit.New(auditFile)
	if err != nil {
		t.Fatal(err)
	}
	f.h.audit = aud

	rr := f.stage(t, f.nova, [][2]string{{"notes.txt", "one"}, {"src/notes.txt", "twotwo"}})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), f.h.cfg.DataDir) {
		t.Fatalf("the reply named a host directory: %s", rr.Body)
	}
	out := mtDecode(t, rr)
	uploadID, _ := out["upload_id"].(string)
	if uploadID == "" {
		t.Fatalf("no upload_id in %v", out)
	}
	if got := out["total"]; got != float64(9) {
		t.Fatalf("total = %v, want the whole batch", got)
	}
	files, _ := out["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files = %v", out["files"])
	}
	if files[0].(map[string]any)["name"] != "notes.txt" ||
		files[1].(map[string]any)["name"] != "notes (2).txt" {
		t.Fatalf("names = %v, want the collision renamed", files)
	}

	dir := filepath.Join(f.h.cfg.DataDir, "ssh-transfers", "tmp", "zmodem-"+uploadID)
	got, err := os.ReadFile(filepath.Join(dir, "notes (2).txt"))
	if err != nil {
		t.Fatalf("the second part never landed: %v", err)
	}
	if string(got) != "twotwo" {
		t.Fatalf("staged bytes = %q", got)
	}
	// Nothing is in flight yet: staging is not a transfer, and a queue row
	// here would show a job that no worker owns.
	if jobs := f.tm.List(f.nova); len(jobs) != 0 {
		t.Fatalf("staging queued %d transfers", len(jobs))
	}
	logged, err := os.ReadFile(auditFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "ssh_machine.transfer.stage") {
		t.Fatalf("the write left no audit line: %s", logged)
	}
}

// A machine with a real file protocol must be told to use it, and the
// built-in host stays unreachable through this route like every other.
func TestZmodemStageRefusesSSHAndBuiltin(t *testing.T) {
	f := newMachineTransferFixture(t)
	for _, tc := range []struct{ id, want string }{
		{f.alpha, "SFTP"},
		{sshmachines.HostMachineID, ""},
		{"no-such-machine", "not found"},
	} {
		rr := f.stage(t, tc.id, [][2]string{{"a.txt", "x"}})
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want a refusal (%s)", tc.id, rr.Code, rr.Body)
		}
		if tc.want != "" && !strings.Contains(rr.Body.String(), tc.want) {
			t.Fatalf("%s: %s, want it to mention %s", tc.id, rr.Body, tc.want)
		}
	}
	// A refusal must not leave a stage behind either.
	entries, _ := os.ReadDir(filepath.Join(f.h.cfg.DataDir, "ssh-transfers", "tmp"))
	for _, e := range entries {
		t.Fatalf("a refused request staged %q", e.Name())
	}
}

func TestZmodemStageCapsTheBatchAndNeedsAPart(t *testing.T) {
	f := newMachineTransferFixture(t)
	many := make([][2]string, 0, sshmachines.MaxStagedBatch+1)
	for i := 0; i <= sshmachines.MaxStagedBatch; i++ {
		many = append(many, [2]string{fmt.Sprintf("f%d.dat", i), "x"})
	}
	if rr := f.stage(t, f.nova, many); rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch: status = %d, want 400 (%s)", rr.Code, rr.Body)
	}
	if rr := f.stage(t, f.nova, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty batch: status = %d, want 400 (%s)", rr.Code, rr.Body)
	}
	entries, _ := os.ReadDir(filepath.Join(f.h.cfg.DataDir, "ssh-transfers", "tmp"))
	if len(entries) != 0 {
		t.Fatalf("refused requests staged %v", entries)
	}
}

// The second frame of an upload: the terminal claims the batch, types rz,
// and the queue shows one upload row that has no result to fetch.
func TestZmodemUploadArmsOneRowWithNoResult(t *testing.T) {
	h := newZmodemHarness(t, true)
	rr := h.f.stage(t, h.machine, [][2]string{{"up.txt", "payload"}})
	uploadID := mtDecode(t, rr)["upload_id"].(string)
	stageDir := filepath.Join(h.f.h.cfg.DataDir, "ssh-transfers", "tmp", "zmodem-"+uploadID)

	h.gate.handle(&machineTerminalFrame{Type: "zmodem", Dir: "up", UploadID: uploadID})
	armed := h.d.event(t, "armed")
	jobID, _ := armed["job_id"].(string)
	if jobID == "" {
		t.Fatalf("armed frame carries no job: %v", armed)
	}
	if armed["dir"] != "up" {
		t.Fatalf("armed frame = %v, want dir up", armed)
	}
	waitZmodem(t, func() bool { return strings.Contains(h.d.typed(), "rz -q -E") }, "the rz command")

	row := h.row(t, jobID)
	if row["kind"] != "upload" {
		t.Fatalf("queue row = %v", row["kind"])
	}
	// The result endpoint is download-only: an upload leaves nothing on this
	// host to serve, and the stage is not addressable by job.
	req := mtRequest(http.MethodGet, "/api/ssh-machines/"+h.machine+"/transfers/"+jobID+"/result",
		map[string]string{"id": h.machine, "job": jobID}, nil)
	rec := httptest.NewRecorder()
	h.f.h.MachineTransferResult(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("result of an upload = %d, want 404 (%s)", rec.Code, rec.Body)
	}

	// Cancel from the terminal's own frame: the row ends and the bytes go.
	h.gate.handle(&machineTerminalFrame{Type: "zmodem", Action: "cancel"})
	var ended map[string]any
	waitZmodem(t, func() bool { ended = h.d.lastEvent("canceled"); return ended != nil }, "the canceled event")
	if _, has := ended["files"]; has {
		t.Fatalf("an upload announced files to download: %v", ended)
	}
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Fatalf("the stage survived its job: %v", err)
	}
}

// A frame that asks to send without naming a stage is a bug in the browser,
// not a transfer: answer it, do not hang the banner.
func TestZmodemUploadFrameNeedsItsStage(t *testing.T) {
	h := newZmodemHarness(t, true)
	h.gate.handle(&machineTerminalFrame{Type: "zmodem", Dir: "up"})
	ev := h.d.event(t, "error")
	if !strings.Contains(fmt.Sprint(ev["detail"]), "staged") {
		t.Fatalf("error = %v, want the missing batch explained", ev)
	}
	if ev["dir"] != "up" {
		t.Fatalf("error frame lost the direction: %v", ev)
	}
	if h.gate.active() {
		t.Fatal("a refused upload armed the terminal")
	}
}

// SSH keeps its own file subsystem; the arm frame must say so rather than
// type rz into a session that has SFTP.
func TestZmodemUploadRefusesSSHMachines(t *testing.T) {
	h := newZmodemHarness(t, false)
	h.gate.handle(&machineTerminalFrame{Type: "zmodem", Dir: "up", UploadID: "whatever"})
	ev := h.d.event(t, "error")
	if !strings.Contains(fmt.Sprint(ev["detail"]), "SFTP") {
		t.Fatalf("error = %v, want the SFTP explanation", ev)
	}
	if h.d.typed() != "" {
		t.Fatalf("a refused arm still typed %q", h.d.typed())
	}
}
