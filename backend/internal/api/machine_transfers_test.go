package api

// Tests for the machine transfer API surface: the ownership guard that
// every job endpoint shares, the batch stat the dual-pane copy dialog
// reads, the global queue, and the tar.gz streamer that turns a
// finished directory download into one browser file.
//
// The dial-dependent paths are exercised against 127.0.0.1 on a port
// that nothing listens on, so a job's worker fails in microseconds
// instead of hanging a test on the 15s SSH timeout. What is asserted
// here is the guard logic and the payload shapes, both of which must
// hold regardless of whether the remote end ever answers.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"webkvm/internal/config"
	"webkvm/internal/sshmachines"
)

type machineTransferFixture struct {
	h     *Handler
	store *sshmachines.Store
	tm    *sshmachines.TransferManager
	dir   string

	alpha string // ssh, unreachable
	beta  string // ssh, unreachable
	nova  string // telnet
}

// mtClosedPort hands back a port that was listening and is now closed.
func mtClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	return p
}

func newMachineTransferFixture(t *testing.T) *machineTransferFixture {
	t.Helper()
	dir := t.TempDir()
	store := sshmachines.NewStore(
		filepath.Join(dir, "ssh-machines.json"),
		filepath.Join(dir, "ssh-machine-secrets.json"),
	)
	port := mtClosedPort(t)

	add := func(name, protocol string, req sshmachines.CreateRequest) string {
		t.Helper()
		req.Name = name
		req.Protocol = protocol
		req.Host = "127.0.0.1"
		req.Port = port
		m, err := store.Create(req)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return m.ID
	}
	sshReq := sshmachines.CreateRequest{
		User:     "root",
		AuthType: sshmachines.AuthPassword,
		Secret:   &sshmachines.SecretInput{Password: "hunter2"},
	}
	f := &machineTransferFixture{store: store}
	f.alpha = add("alpha", sshmachines.ProtocolSSH, sshReq)
	f.beta = add("beta", sshmachines.ProtocolSSH, sshReq)
	f.nova = add("nova", sshmachines.ProtocolTelnet, sshmachines.CreateRequest{
		User: "root", AuthType: sshmachines.AuthNone,
	})

	reg := sshmachines.NewSFTPSessionRegistry(store)
	f.tm = sshmachines.NewTransferManager(store, reg, nil, dir)
	// Settle before closing: a paused job's worker is parked in
	// waitIfPaused and nothing else wakes it, and a worker still queued
	// on the semaphore would dial the host card after CloseAll and pool
	// a session nothing closes anymore (goleak). The same order as
	// Handler.Close.
	t.Cleanup(func() {
		f.tm.CancelAll()
		if !f.tm.WaitAll(5 * time.Second) {
			t.Error("transfer jobs did not settle within 5s of CancelAll")
		}
		reg.CloseAll()
		reg.Stop()
	})
	f.h = &Handler{
		cfg:          &config.Config{DataDir: dir},
		machines:     store,
		sftpSessions: reg,
		transfers:    f.tm,
	}
	return f
}

// mtRequest carries the chi URL params the handler reads.
func mtRequest(method, url string, params map[string]string, body io.Reader) *http.Request {
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	req := httptest.NewRequest(method, url, body)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
}

func mtDecode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return out
}

// ---- ownership guard ----

// TestMachineJobGuardIsTheOnlyDoorAtFourEndpoints pins the fix for the
// IDOR found in review: with a job id alone, a console account used to
// be able to cancel/remove/read another machine's transfer (and so
// delete its downloaded file). Every answer for a job that does not
// belong to {id} must be a 404, not a 403 and not a 200.
func TestMachineJobGuardIsTheOnlyDoorAtFourEndpoints(t *testing.T) {
	f := newMachineTransferFixture(t)
	// One job per request: MachineTransferRemove unloads the job it is
	// given, and a shared one would make the later rows fail for the
	// wrong reason.
	freshJob := func(t *testing.T) string {
		t.Helper()
		job, err := f.tm.StartDownload(f.alpha, "/etc/hosts", "hosts")
		if err != nil {
			t.Fatalf("start download: %v", err)
		}
		return job.ID
	}

	cases := []struct {
		name string
		id   string
		want int
	}{
		{"owner sees it", f.alpha, 0},
		{"other machine gets 404", f.beta, http.StatusNotFound},
		// The built-in host card answers its file plane to admins only,
		// and this request carries no admin identity: the answer must be
		// the one for an id that is not there at all.
		{"builtin to a non-admin reads as absent", sshmachines.HostMachineID, http.StatusNotFound},
		{"unknown id gets 404", "nope", http.StatusNotFound},
	}
	for _, c := range cases {
		for _, ep := range []struct {
			method, path string
			call         func(*Handler, http.ResponseWriter, *http.Request)
		}{
			{http.MethodPost, "/cancel", (*Handler).MachineTransferCancel},
			{http.MethodDelete, "", (*Handler).MachineTransferRemove},
			{http.MethodGet, "/result", (*Handler).MachineTransferResult},
		} {
			id := freshJob(t)
			rr := httptest.NewRecorder()
			ep.call(f.h, rr, mtRequest(ep.method, "/api/ssh-machines/"+c.id+"/transfers/"+id+ep.path,
				map[string]string{"id": c.id, "job": id}, nil))
			if c.want == 0 {
				if rr.Code == http.StatusNotFound || rr.Code == http.StatusBadRequest {
					t.Errorf("%s %s as owner: got %d (%s), want a non-404 answer",
						ep.method, ep.path, rr.Code, rr.Body.String())
				}
				continue
			}
			if rr.Code != c.want {
				t.Errorf("%s %s with id=%s: got %d (%s), want %d",
					ep.method, ep.path, c.name, rr.Code, rr.Body.String(), c.want)
			}
		}
	}
}

// TestMachineJobGuardAcceptsTheCopyDestination covers the other half of
// the ownership rule: a copy job must be reachable from BOTH ends, so
// the destination page can show and cancel what is arriving.
func TestMachineJobGuardAcceptsTheCopyDestination(t *testing.T) {
	f := newMachineTransferFixture(t)
	job, err := f.tm.StartCopy(f.alpha, "/srv/app", f.beta, "/srv", "skip", true)
	if err != nil {
		t.Fatalf("start copy: %v", err)
	}
	if job.DstMachineID != f.beta {
		t.Fatalf("fixture: dst_machine_id = %q, want %q", job.DstMachineID, f.beta)
	}
	for _, id := range []string{f.alpha, f.beta} {
		rr := httptest.NewRecorder()
		f.h.MachineTransferCancel(rr, mtRequest(http.MethodPost, "/x", map[string]string{"id": id, "job": job.ID}, nil))
		if rr.Code == http.StatusNotFound {
			t.Errorf("cancel from %s: got 404, want the job to be visible (%s)", id, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	f.h.MachineTransferCancel(rr, mtRequest(http.MethodPost, "/x", map[string]string{"id": f.nova, "job": job.ID}, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("cancel from an unrelated machine: got %d, want 404", rr.Code)
	}
}

// TestMachineTransferResultWrongKind: /result only ever streams
// downloads. An upload job's LocalFile is the client's spooled body,
// which is deleted the moment the job ends — serving it would be a
// confusing 404 at best.
func TestMachineTransferResultWrongKind(t *testing.T) {
	f := newMachineTransferFixture(t)
	src := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(src, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	job, err := f.tm.StartUpload(f.alpha, "/tmp/payload", src, 0)
	if err != nil {
		t.Fatalf("start upload: %v", err)
	}
	rr := httptest.NewRecorder()
	f.h.MachineTransferResult(rr, mtRequest(http.MethodGet, "/x", map[string]string{"id": f.alpha, "job": job.ID}, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("result on an upload job: got %d (%s), want 404", rr.Code, rr.Body.String())
	}

	// A download that never finished must not stream a partial file.
	dl, err := f.tm.StartDownload(f.alpha, "/var/log/syslog", "")
	if err != nil {
		t.Fatalf("start download: %v", err)
	}
	rr = httptest.NewRecorder()
	f.h.MachineTransferResult(rr, mtRequest(http.MethodGet, "/x", map[string]string{"id": f.alpha, "job": dl.ID}, nil))
	if rr.Code == http.StatusOK {
		t.Fatalf("result on a %q download: got 200, want 409", dl.Status())
	}
	if rr.Code != http.StatusConflict && rr.Code != http.StatusNotFound {
		t.Fatalf("result on an unfinished download: got %d (%s), want 409", rr.Code, rr.Body.String())
	}
}

// ---- batch stat ----

type mtStatNode struct {
	mode os.FileMode
	size int64
	err  error
}

type mtStatClient struct {
	nodes map[string]mtStatNode
	calls int
}

func (c *mtStatClient) Lstat(p string) (os.FileInfo, error) {
	c.calls++
	n, ok := c.nodes[p]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: p, Err: os.ErrNotExist}
	}
	if n.err != nil {
		return nil, n.err
	}
	return mtStatInfo{name: path.Base(p), mode: n.mode, size: n.size}, nil
}

type mtStatInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (i mtStatInfo) Name() string       { return i.name }
func (i mtStatInfo) Size() int64        { return i.size }
func (i mtStatInfo) Mode() os.FileMode  { return i.mode }
func (i mtStatInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (i mtStatInfo) IsDir() bool        { return i.mode.IsDir() }
func (i mtStatInfo) Sys() any           { return nil }

func TestSFTPStatPathsParsing(t *testing.T) {
	// One path per repeated ?paths= parameter, and nothing else. Splitting
	// on commas was the first attempt and it mangles legal remote names.
	got, err := sftpStatPaths([]string{"/srv/a.txt", "report, 2026.txt", "/srv/a.txt"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Duplicates survive: the caller asked for them, and de-duping here
	// would silently shrink a 200-path batch.
	want := []string{"/srv/a.txt", "/report, 2026.txt", "/srv/a.txt"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Every shape is rooted: nothing may escape to a relative path.
	rel, err := sftpStatPaths([]string{"../../etc/passwd", "tmp/x", ""})
	if err != nil {
		t.Fatalf("parse relative: %v", err)
	}
	if len(rel) != 2 || rel[0] != "/etc/passwd" || rel[1] != "/tmp/x" {
		t.Fatalf("cleaning = %v, want [/etc/passwd /tmp/x]", rel)
	}

	if _, err := sftpStatPaths(nil); err == nil || err.Error() != "paths is required" {
		t.Errorf("empty paths: err = %v, want \"paths is required\"", err)
	}
	if _, err := sftpStatPaths([]string{"", ""}); err == nil {
		t.Error("only-blank paths must be rejected, not dial the machine")
	}

	var many []string
	for i := 0; i <= sftpStatMaxPaths; i++ {
		many = append(many, "/f"+string(rune('a'+i%26))+".txt")
	}
	if _, err := sftpStatPaths(many); err == nil {
		t.Errorf("%d paths accepted, want rejection past %d", len(many), sftpStatMaxPaths)
	}
}

func TestSFTPStatEntriesShapes(t *testing.T) {
	client := &mtStatClient{nodes: map[string]mtStatNode{
		"/srv/app":    {mode: os.ModeDir | 0o755, size: 4096},
		"/srv/a.txt":  {mode: 0o644, size: 12},
		"/srv/link":   {mode: os.ModeSymlink | 0o777, size: 9},
		"/srv/broken": {err: os.ErrNotExist},
	}}
	stats := sftpStatEntries(client, []string{"/srv/app", "/srv/a.txt", "/srv/link", "/srv/broken", "/srv/absent"})
	if len(stats) != 5 {
		t.Fatalf("len(stats) = %d, want 5 (one entry per path, in order)", len(stats))
	}
	// Round-trip through JSON so the assertions are about what the
	// browser actually receives, not about Go's int64.
	buf, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var onWire []map[string]any
	if err := json.Unmarshal(buf, &onWire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stats = onWire
	for i, want := range []string{"/srv/app", "/srv/a.txt", "/srv/link", "/srv/broken", "/srv/absent"} {
		if stats[i]["path"] != want {
			t.Errorf("stats[%d].path = %v, want %q", i, stats[i]["path"], want)
		}
		if _, ok := stats[i]["exists"]; !ok {
			t.Errorf("stats[%d] missing the exists key: %v", i, stats[i])
		}
	}
	if stats[0]["exists"] != true || stats[0]["is_dir"] != true {
		t.Errorf("dir entry = %v, want exists+is_dir true", stats[0])
	}
	if stats[1]["size"] != float64(12) || stats[1]["mode"] != "-rw-r--r--" {
		t.Errorf("file entry = %v, want size 12 and mode -rw-r--r--", stats[1])
	}
	if stats[1]["mod_time"] != float64(1700000000) {
		t.Errorf("file mod_time = %v, want 1700000000", stats[1]["mod_time"])
	}
	// A dangling link occupies its name: the copy dialog must not
	// overwrite-blind through it.
	if stats[2]["is_symlink"] != true || stats[2]["exists"] != true || stats[2]["is_dir"] == true {
		t.Errorf("symlink entry = %v, want exists+is_symlink, not is_dir", stats[2])
	}
	for _, i := range []int{3, 4} {
		if stats[i]["exists"] != false {
			t.Errorf("missing path %v reported exists", stats[i])
		}
		if _, ok := stats[i]["mode"]; ok {
			t.Errorf("missing path %v carries a mode; the UI would render an empty row as a file", stats[i])
		}
	}
	if client.calls != 5 {
		t.Errorf("Lstat calls = %d, want 5 (one RTT per path)", client.calls)
	}
}

// TestMachineSFTPStatValidatesBeforeDialing is why the endpoint does not
// use sftpClientFor: a request with no paths must fail with 400 without
// ever opening a session to the machine.
func TestMachineSFTPStatValidatesBeforeDialing(t *testing.T) {
	f := newMachineTransferFixture(t)
	cases := []struct {
		name, id, query string
		want            int
		wantErr         string
	}{
		{"missing paths", f.alpha, "", http.StatusBadRequest, "paths is required"},
		{"blank paths", f.alpha, "?paths=&paths=", http.StatusBadRequest, "paths is required"},
		{"telnet machine", f.nova, "?paths=/etc", http.StatusBadRequest, "files need an ssh machine"},
		{"builtin to a non-admin reads as absent", sshmachines.HostMachineID, "?paths=/etc", http.StatusNotFound, "machine not found"},
		{"unknown machine", "nope", "?paths=/etc", http.StatusNotFound, "machine not found"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		f.h.MachineSFTPStat(rr, mtRequest(http.MethodGet, "/api/ssh-machines/"+c.id+"/sftp/stat"+c.query,
			map[string]string{"id": c.id}, nil))
		if rr.Code != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, rr.Code, rr.Body.String(), c.want)
			continue
		}
		if got := mtDecode(t, rr); !strings.Contains(got["error"].(string), c.wantErr) {
			t.Errorf("%s: error = %v, want it to mention %q", c.name, got["error"], c.wantErr)
		}
	}
}

// ---- job-granular queue endpoints ----

func TestJobGrainedEndpointsResolveWithoutTheMachineID(t *testing.T) {
	f := newMachineTransferFixture(t)
	job, err := f.tm.StartDownload(f.alpha, "/etc/hosts", "hosts")
	if err != nil {
		t.Fatalf("start download: %v", err)
	}

	rr := httptest.NewRecorder()
	f.h.MachineTransferCancelJob(rr, mtRequest(http.MethodPost, "/api/machine-transfers/nope/cancel",
		map[string]string{"job": "nope"}, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("cancel unknown job: got %d, want 404", rr.Code)
	}

	rr = httptest.NewRecorder()
	f.h.MachineTransferCancelJob(rr, mtRequest(http.MethodPost, "/api/machine-transfers/"+job.ID+"/cancel",
		map[string]string{"job": job.ID}, nil))
	if rr.Code != http.StatusOK && rr.Code != http.StatusConflict {
		t.Fatalf("cancel own job: got %d (%s), want 200 or 409 once it already ended", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	f.h.MachineTransferRemoveJob(rr, mtRequest(http.MethodDelete, "/api/machine-transfers/"+job.ID,
		map[string]string{"job": job.ID}, nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("remove job: got %d (%s), want 204", rr.Code, rr.Body.String())
	}
	if f.tm.Get(job.ID) != nil {
		t.Error("job still in the manager after remove")
	}
	if _, err := os.Stat(job.LocalFile); err == nil {
		t.Errorf("remove left the download staging file on disk: %s", job.LocalFile)
	}
}

// mtRequestAsAdmin stamps the headers the auth middleware would set, so
// requestIsAdmin sees an admin where the plain mtRequest sees a
// non-admin.
func mtRequestAsAdmin(method, url string, params map[string]string, body io.Reader) *http.Request {
	req := mtRequest(method, url, params, body)
	req.Header.Set("X-User", "adminuser")
	req.Header.Set("X-Role", "admin")
	return req
}

// ---- host-card job gating ----

// TestHostCardJobsHiddenFromNonAdmins: the global queue is the one
// surface where a host-card job row (its paths, its destination) would
// leak to any console account, so the row itself disappears — the same
// answer as if the job had never existed.
func TestHostCardJobsHiddenFromNonAdmins(t *testing.T) {
	f := newMachineTransferFixture(t)
	if _, err := f.tm.StartDownload(f.alpha, "/etc/hosts", "hosts"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tm.StartDownload(sshmachines.HostMachineID, "/etc/hostname", "hostname"); err != nil {
		t.Fatalf("start host download: %v", err)
	}

	countMachines := func(r *http.Request) map[string]bool {
		t.Helper()
		rr := httptest.NewRecorder()
		f.h.MachineTransfersAll(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("queue = %d (%s)", rr.Code, rr.Body.String())
		}
		var rows []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, row := range rows {
			if id, ok := row["machine_id"].(string); ok {
				seen[id] = true
			}
		}
		return seen
	}

	if seen := countMachines(mtRequest(http.MethodGet, "/api/machine-transfers", nil, nil)); seen[sshmachines.HostMachineID] {
		t.Errorf("a non-admin sees the host-card job row in the queue: %v", seen)
	} else if !seen[f.alpha] {
		t.Errorf("the ordinary machine's job row vanished: %v", seen)
	}
	// The machines= filter must not become an existence probe either.
	rr := httptest.NewRecorder()
	f.h.MachineTransfersAll(rr, mtRequest(http.MethodGet, "/api/machine-transfers?machines="+sshmachines.HostMachineID, nil, nil))
	var rows []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil || len(rows) != 0 {
		t.Errorf("machines=host for a non-admin returned %v, want nothing", rows)
	}

	seen := countMachines(mtRequestAsAdmin(http.MethodGet, "/api/machine-transfers", nil, nil))
	if !seen[sshmachines.HostMachineID] {
		t.Errorf("the admin must see their own host-card job: %v", seen)
	}
	if !seen[f.alpha] {
		t.Errorf("the admin's listing lost the ordinary job: %v", seen)
	}
}

// TestHostCardJobRefusedForNonAdmins: the job-granular buttons must answer
// a host-card job with the same 404 as an id nobody owns — for both ends
// of a job (source AND copy destination) — while the admin still reaches
// the real answer (200, or 409 once the unreachable worker has settled).
func TestHostCardJobRefusedForNonAdmins(t *testing.T) {
	f := newMachineTransferFixture(t)
	endpoints := []struct {
		name, method, path string
		call               func(*Handler, http.ResponseWriter, *http.Request)
	}{
		{"cancel", http.MethodPost, "/cancel", (*Handler).MachineTransferCancelJob},
		{"remove", http.MethodDelete, "", (*Handler).MachineTransferRemoveJob},
		{"pause", http.MethodPost, "/pause", (*Handler).MachineTransferPauseJob},
		{"resume", http.MethodPost, "/resume", (*Handler).MachineTransferResumeJob},
	}
	startHostJob := func(dst string) *sshmachines.Transfer {
		t.Helper()
		if dst == "" {
			job, err := f.tm.StartDownload(sshmachines.HostMachineID, "/etc/hostname", "hostname")
			if err != nil {
				t.Fatal(err)
			}
			return job
		}
		job, err := f.tm.StartCopy(f.alpha, "/srv/app", sshmachines.HostMachineID, "/tmp", "skip", true)
		if err != nil {
			t.Fatal(err)
		}
		if job.DstMachineID != sshmachines.HostMachineID {
			t.Fatalf("fixture: dst_machine_id = %q, want host", job.DstMachineID)
		}
		return job
	}
	for _, dst := range []string{"", f.alpha} {
		for _, ep := range endpoints {
			// One job per assertion: remove unloads its job, and the
			// later rows must not 404 for that reason.
			job := startHostJob(dst)
			rr := httptest.NewRecorder()
			ep.call(f.h, rr, mtRequest(ep.method, "/api/machine-transfers/"+job.ID+ep.path,
				map[string]string{"job": job.ID}, nil))
			if rr.Code != http.StatusNotFound {
				t.Errorf("%s host job (dst=%q): got %d (%s), want 404 for a non-admin",
					ep.name, dst, rr.Code, rr.Body.String())
			}

			// The admin is not refused: 200 while the worker is alive,
			// 409 once the dead dial settled it — never 404.
			rr = httptest.NewRecorder()
			ep.call(f.h, rr, mtRequestAsAdmin(ep.method, "/api/machine-transfers/"+job.ID+ep.path,
				map[string]string{"job": job.ID}, nil))
			if rr.Code == http.StatusNotFound {
				t.Errorf("%s host job as admin: got 404 (%s), the gate must pass the admin through", ep.name, rr.Body.String())
			}
		}
	}
}

// TestPauseResumeAPIPlumbing pins the handler layer: unknown ids 404,
// and a settled job 409s on both verbs (the state machine itself —
// pause taking a live job, resume releasing it — is pinned at the
// manager level, where a running worker can be held open).
func TestPauseResumeAPIPlumbing(t *testing.T) {
	f := newMachineTransferFixture(t)
	job, err := f.tm.StartDownload(f.alpha, "/etc/hosts", "hosts")
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the dead dial to settle the worker.
	deadline := time.Now().Add(2 * time.Second)
	for job.Status() == sshmachines.TransferRunning && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status() == sshmachines.TransferRunning {
		t.Fatal("the fixture job never settled; the 409 assertions below would race")
	}

	rr := httptest.NewRecorder()
	f.h.MachineTransferPauseJob(rr, mtRequest(http.MethodPost, "/api/machine-transfers/nope/pause",
		map[string]string{"job": "nope"}, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("pause unknown: got %d, want 404", rr.Code)
	}
	rr = httptest.NewRecorder()
	f.h.MachineTransferResumeJob(rr, mtRequest(http.MethodPost, "/api/machine-transfers/nope/resume",
		map[string]string{"job": "nope"}, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("resume unknown: got %d, want 404", rr.Code)
	}

	rr = httptest.NewRecorder()
	f.h.MachineTransferPauseJob(rr, mtRequest(http.MethodPost, "/api/machine-transfers/"+job.ID+"/pause",
		map[string]string{"job": job.ID}, nil))
	if rr.Code != http.StatusConflict {
		t.Errorf("pause a settled job: got %d (%s), want 409", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	f.h.MachineTransferResumeJob(rr, mtRequest(http.MethodPost, "/api/machine-transfers/"+job.ID+"/resume",
		map[string]string{"job": job.ID}, nil))
	if rr.Code != http.StatusConflict {
		t.Errorf("resume a settled job: got %d (%s), want 409", rr.Code, rr.Body.String())
	}
}

func TestMachineTransfersAllLimitAndFilter(t *testing.T) {
	f := newMachineTransferFixture(t)
	if _, err := f.tm.StartDownload(f.alpha, "/a", ""); err != nil {
		t.Fatal(err)
	}
	dstJob, err := f.tm.StartCopy(f.alpha, "/srv/app", f.beta, "/srv", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tm.StartDownload(f.nova, "/a", ""); err == nil {
		t.Fatal("fixture: a telnet machine must not accept a download")
	}

	badLimits := []string{"0", "-1", "1001", "abc", "1.5"}
	for _, v := range badLimits {
		rr := httptest.NewRecorder()
		f.h.MachineTransfersAll(rr, mtRequest(http.MethodGet, "/api/machine-transfers?limit="+v, nil, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: got %d, want 400", v, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	f.h.MachineTransfersAll(rr, mtRequest(http.MethodGet, "/api/machine-transfers?limit=1", nil, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("limit=1: got %d (%s)", rr.Code, rr.Body.String())
	}
	var one []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 {
		t.Fatalf("limit=1 returned %d rows, want 1", len(one))
	}

	countFor := func(q string) int {
		t.Helper()
		rr := httptest.NewRecorder()
		f.h.MachineTransfersAll(rr, mtRequest(http.MethodGet, "/api/machine-transfers?"+q, nil, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d (%s)", q, rr.Code, rr.Body.String())
		}
		var rows []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if n := countFor("machines=" + f.alpha); n != 2 {
		t.Errorf("machines=alpha returned %d rows, want 2 (its download + the copy it sent)", n)
	}
	if n := countFor("machines=" + f.beta); n != 1 {
		t.Errorf("machines=beta returned %d rows, want 1 (the copy arriving)", n)
	}
	if n := countFor("machines=nonsense"); n != 0 {
		t.Errorf("an unrelated id returned %d rows, want 0 rather than an existence probe", n)
	}

	// The dst side of a copy is filterable, which is what lets the
	// sidebar group a queue by machine on one request.
	rr = httptest.NewRecorder()
	f.h.MachineTransfersAll(rr, mtRequest(http.MethodGet, "/api/machine-transfers?machines="+f.beta, nil, nil))
	var rows []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["id"] != dstJob.ID || rows[0]["kind"] != "copy" {
		t.Fatalf("filter rows = %v, want exactly the copy job %s", rows, dstJob.ID)
	}
}

// TestMachineSFPTSizesValidatesBeforeDialing mirrors the stat rule: a
// malformed batch must 400 before an SSH dial is paid for, and the
// built-in card reads as absent to a non-admin on this surface too.
func TestMachineSFPTSizesValidatesBeforeDialing(t *testing.T) {
	f := newMachineTransferFixture(t)
	cases := []struct {
		name, id, query string
		want            int
		wantErr         string
	}{
		{"missing paths", f.alpha, "", http.StatusBadRequest, "paths is required"},
		{"telnet machine", f.nova, "?paths=/etc", http.StatusBadRequest, "files need an ssh machine"},
		{"builtin to a non-admin reads as absent", sshmachines.HostMachineID, "?paths=/etc", http.StatusNotFound, "machine not found"},
		{"unknown machine", "nope", "?paths=/etc", http.StatusNotFound, "machine not found"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		f.h.MachineSFPTSizes(rr, mtRequest(http.MethodGet, "/api/ssh-machines/"+c.id+"/sftp/sizes"+c.query,
			map[string]string{"id": c.id}, nil))
		if rr.Code != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, rr.Code, rr.Body.String(), c.want)
			continue
		}
		if got := mtDecode(t, rr); !strings.Contains(got["error"].(string), c.wantErr) {
			t.Errorf("%s: error = %v, want it to mention %q", c.name, got["error"], c.wantErr)
		}
	}
}

func TestSFPTSizesPathsParsing(t *testing.T) {
	got, err := sftpSizesPaths([]string{"/srv/a", "/srv/b", "/srv/a"})
	if err != nil || len(got) != 3 || got[0] != "/srv/a" || got[1] != "/srv/b" || got[2] != "/srv/a" {
		t.Fatalf("parse = %v, %v; duplicates survive, order kept", got, err)
	}
	if _, err := sftpSizesPaths(nil); err == nil || err.Error() != "paths is required" {
		t.Errorf("empty: err = %v, want \"paths is required\"", err)
	}
	var many []string
	for i := 0; i <= sftpSizesMaxPaths; i++ {
		many = append(many, "/f"+string(rune('a'+i%26)))
	}
	if _, err := sftpSizesPaths(many); err == nil {
		t.Errorf("%d paths accepted, want rejection past %d", len(many), sftpSizesMaxPaths)
	}
}

// ---- copy branch ----

func TestTransferStartCopyBranchCodes(t *testing.T) {
	f := newMachineTransferFixture(t)
	start := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		f.h.MachineTransferStart(rr, mtRequest(http.MethodPost, "/api/ssh-machines/"+f.alpha+"/transfers",
			map[string]string{"id": f.alpha}, strings.NewReader(body)))
		return rr
	}

	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown action", `{"action":"teleport"}`, http.StatusBadRequest},
		{"no destination", `{"action":"copy","remote_path":"/srv/app","dst_dir":"/srv"}`, http.StatusBadRequest},
		{"unknown destination", `{"action":"copy","remote_path":"/srv/app","dst_machine_id":"ghost","dst_dir":"/srv"}`, http.StatusNotFound},
		{"telnet destination", `{"action":"copy","remote_path":"/srv/app","dst_machine_id":"` + f.nova + `","dst_dir":"/srv"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		rr := start(c.body)
		if rr.Code != c.want {
			t.Errorf("%s: got %d (%s), want %d", c.name, rr.Code, rr.Body.String(), c.want)
		}
	}

	rr := start(`{"action":"copy","remote_path":"/srv/app","dst_machine_id":"` + f.beta + `","dst_dir":"/srv","overwrite":"rename"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("valid copy: got %d (%s), want 202", rr.Code, rr.Body.String())
	}
	snap := mtDecode(t, rr)
	if snap["kind"] != "copy" || snap["dst_machine_id"] != f.beta || snap["dst_machine"] != "beta" {
		t.Errorf("copy snapshot = %v, want kind/dst_machine_id/dst_machine", snap)
	}
	if snap["dst_dir"] != "/srv" || snap["file_name"] != "app" {
		t.Errorf("copy snapshot dirs = %v/%v, want /srv and app", snap["dst_dir"], snap["file_name"])
	}
	if snap["machine_id"] != f.alpha {
		t.Errorf("copy snapshot machine_id = %v, want the source %s", snap["machine_id"], f.alpha)
	}
	if f.tm.Get(snap["id"].(string)) == nil {
		t.Fatal("202 returned an id the manager does not know")
	}
}

// TestTransferStartEmptyRemotePathRejected pins the 400 guards that keep a
// missing or whitespace-only `remote_path` from decoding to "/" and
// planning a transfer of the remote root (the client once sent the wrong
// key, `src_path`, and the copy silently walked `/`).
func TestTransferStartEmptyRemotePathRejected(t *testing.T) {
	f := newMachineTransferFixture(t)
	start := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		f.h.MachineTransferStart(rr, mtRequest(http.MethodPost, "/api/ssh-machines/"+id+"/transfers",
			map[string]string{"id": id}, strings.NewReader(body)))
		return rr
	}
	cases := []struct{ name, body string }{
		{"download without path", `{"action":"download"}`},
		{"download whitespace path", `{"action":"download","remote_path":"   "}`},
		{"copy without path", `{"action":"copy","remote_path":"","dst_machine_id":"` + f.beta + `","dst_dir":"/srv"}`},
	}
	for _, c := range cases {
		rr := start(f.alpha, c.body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d (%s), want 400", c.name, rr.Code, rr.Body.String())
		}
	}
}

// ---- clear finished ----

// TestMachineTransfersClearFinished pins the batch unload behind the
// queue's "clear completed": one pass removes every terminal job and
// reports the count; a second pass finds nothing. The per-job DELETE
// remains the row-level door, and the row-level 404 (a job the server
// already forgot) is what the client treats as "already gone".
func TestMachineTransfersClearFinished(t *testing.T) {
	f := newMachineTransferFixture(t)
	job, err := f.tm.StartDownload(f.alpha, "/etc/hosts", "hosts")
	if err != nil {
		t.Fatalf("start download: %v", err)
	}
	// The dial fails in microseconds against the closed port; wait for
	// the terminal state with a deadline instead of sleeping blind.
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.tm.Get(job.ID).Status()
		if st != sshmachines.TransferRunning || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := f.tm.Get(job.ID).Status(); st == sshmachines.TransferRunning {
		t.Fatalf("job did not settle within the deadline (status %s)", st)
	}

	rr := httptest.NewRecorder()
	f.h.MachineTransfersClearFinished(rr, mtRequest(http.MethodPost, "/api/machine-transfers/clear", nil, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("clear: got %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if out := mtDecode(t, rr); out["removed"] != float64(1) {
		t.Errorf("clear removed = %v, want 1", out["removed"])
	}
	if f.tm.Get(job.ID) != nil {
		t.Error("cleared job still in the table")
	}
	if rows := f.tm.ListAll(100); len(rows) != 0 {
		t.Errorf("ListAll = %d rows after the clear, want 0", len(rows))
	}

	rr = httptest.NewRecorder()
	f.h.MachineTransfersClearFinished(rr, mtRequest(http.MethodPost, "/api/machine-transfers/clear", nil, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("second clear: got %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if out := mtDecode(t, rr); out["removed"] != float64(0) {
		t.Errorf("second clear removed = %v, want 0", out["removed"])
	}
}

func TestTransferStartDownloadBatchBranch(t *testing.T) {
	f := newMachineTransferFixture(t)
	start := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		f.h.MachineTransferStart(rr, mtRequest(http.MethodPost, "/api/ssh-machines/"+id+"/transfers",
			map[string]string{"id": id}, strings.NewReader(body)))
		return rr
	}

	rr := start(f.alpha, `{"action":"download-batch","file_name":"sel"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("no paths: got %d (%s), want 400", rr.Code, rr.Body.String())
	}
	paths := make([]string, 0, downloadBatchMaxPaths+1)
	for i := 0; i <= downloadBatchMaxPaths; i++ {
		paths = append(paths, `"/f`+string(rune('a'+i%26))+`"`)
	}
	rr = start(f.alpha, `{"action":"download-batch","paths": [`+strings.Join(paths, ",")+`]}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("65 paths: got %d (%s), want 400", rr.Code, rr.Body.String())
	}
	rr = start(f.nova, `{"action":"download-batch","paths":["/a"]}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("telnet source: got %d (%s), want 400", rr.Code, rr.Body.String())
	}

	rr = start(f.alpha, `{"action":"download-batch","paths":["/srv/a","/notes.txt"],"file_name":"selection"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("valid batch: got %d (%s), want 202", rr.Code, rr.Body.String())
	}
	snap := mtDecode(t, rr)
	if snap["kind"] != "download" {
		t.Errorf("batch snapshot kind = %v, want download", snap["kind"])
	}
	if snap["file_name"] != "selection" {
		t.Errorf("batch snapshot file_name = %v, want selection", snap["file_name"])
	}
	pathsOut, _ := snap["remote_paths"].([]any)
	if len(pathsOut) != 2 || pathsOut[0] != "/srv/a" || pathsOut[1] != "/notes.txt" {
		t.Errorf("batch snapshot remote_paths = %v, want the cleaned roots", snap["remote_paths"])
	}
	job := f.tm.Get(snap["id"].(string))
	if job == nil {
		t.Fatal("202 returned an id the manager does not know")
	}
	if job.RemotePath != "" {
		t.Errorf("batch job remote_path = %q, want empty (there is no single one)", job.RemotePath)
	}
}

// ---- tar.gz of a finished directory download ----

func TestStreamTransferDirPacksOneArchive(t *testing.T) {
	f := newMachineTransferFixture(t)
	base := filepath.Join(t.TempDir(), "app")
	mkdirAll(t, filepath.Join(base, "sub", "deep"))
	writeFile(t, filepath.Join(base, "a.txt"), "alpha", 0o644)
	writeFile(t, filepath.Join(base, "sub", "b.txt"), "bravo", 0o600)
	writeFile(t, filepath.Join(base, "sub", "deep", "c.bin"), "\x00\x01", 0o755)
	if err := os.Symlink("../a.txt", filepath.Join(base, "sub", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	job := &sshmachines.Transfer{ID: "job-1", Kind: "download", LocalFile: base}
	rr := httptest.NewRecorder()
	f.h.streamTransferDir(rr, job, "app")

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != `attachment; filename="app.tar.gz"` {
		t.Errorf("Content-Disposition = %q, want the archive named after the folder", cd)
	}
	if _, ok := rr.Header()["Content-Length"]; ok {
		t.Error("Content-Length set: the archive is produced while it streams, so any length would be a lie")
	}

	// The tree's own root is not a member (walk starts below it), but
	// every entry is rooted at the folder name so extracting cannot
	// scatter files next to whatever the user already has there.
	want := map[string]string{
		"app/a.txt":          "alpha",
		"app/sub/":           "dir",
		"app/sub/b.txt":      "bravo",
		"app/sub/deep/":      "dir",
		"app/sub/deep/c.bin": "\x00\x01",
		"app/sub/link":       "link:../a.txt",
	}

	gz, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	got := map[string]string{}
	names := []string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			got[hdr.Name] = "dir"
		case tar.TypeSymlink:
			got[hdr.Name] = "link:" + hdr.Linkname
		case tar.TypeReg:
			b, rerr := io.ReadAll(tr)
			if rerr != nil {
				t.Fatalf("read %s: %v", hdr.Name, rerr)
			}
			got[hdr.Name] = string(b)
		default:
			t.Errorf("unexpected member %s type %d", hdr.Name, hdr.Typeflag)
		}
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("member %q = %q, want %q (members: %v)", name, got[name], content, names)
		}
	}
	if len(got) != len(want) {
		t.Errorf("archive has %d members %v, want %d", len(got), names, len(want))
	}
	// Every member is rooted at the folder name: an extracted tree must
	// never scatter itself next to the user's other files.
	for _, n := range names {
		if !strings.HasPrefix(n, "app/") {
			t.Errorf("member %q escapes the archive root", n)
		}
	}
}

func TestStreamTransferDirMissingTree(t *testing.T) {
	f := newMachineTransferFixture(t)
	job := &sshmachines.Transfer{ID: "job-2", Kind: "download", LocalFile: filepath.Join(t.TempDir(), "gone")}
	rr := httptest.NewRecorder()
	f.h.streamTransferDir(rr, job, "gone")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if rr.Header().Get("Content-Type") == "application/gzip" {
		t.Error("headers committed before the stat: a 404 must be able to say so in JSON")
	}
}

func TestWriteTarEntrySkipsWhatHasNoArchiveForm(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := t.TempDir()
	real := filepath.Join(dir, "f")
	writeFile(t, real, "data", 0o644)

	// A socket/fifo has no archive form; dereferencing or blocking on it
	// would hang the stream.
	if err := writeTarEntry(tw, real, "root/sock", mtStatInfo{name: "sock", mode: os.ModeSocket}); err != nil {
		t.Fatalf("socket entry: %v", err)
	}
	if err := writeTarEntry(tw, real, "root/fifo", mtStatInfo{name: "fifo", mode: os.ModeNamedPipe | 0o644}); err != nil {
		t.Fatalf("fifo entry: %v", err)
	}
	if err := writeTarEntry(tw, real, "root/ok.txt", mtStatInfo{name: "ok.txt", mode: 0o644, size: 4}); err != nil {
		t.Fatalf("regular entry: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, hdr.Name)
	}
	if len(names) != 1 || names[0] != "root/ok.txt" {
		t.Fatalf("members = %v, want exactly [root/ok.txt]", names)
	}
}

func TestSanitizeArchiveRoot(t *testing.T) {
	cases := map[string]string{
		"app":              "app",
		"/srv/var/log":     "log",
		`a"b$c`:            `a_b_c`,
		"..":               "download",
		"":                 "download",
		".":                "download",
		"../../..":         "download",
		"  spaced  name ":  "spaced  name",
		"emoji-✓":          "emoji-_",
		"nested/dir/name":  "name",
		"...":              "download",
		"-dash_dot.space-": "-dash_dot.space-",
	}
	for in, want := range cases {
		if got := sanitizeArchiveRoot(in); got != want {
			t.Errorf("sanitizeArchiveRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- upload mode ----

func TestParseSFTPMode(t *testing.T) {
	ok := map[string]string{
		"755":  "-rwxr-xr-x",
		"644":  "-rw-r--r--",
		"0755": "-rwxr-xr-x",
		"600":  "-rw-------",
		"000":  "----------",
		// Special bits never travel to the remote: a 4755 upload would
		// otherwise be a setuid binary owned by the uploader.
		"4755": "-rwxr-xr-x",
		"7555": "-r-xr-xr-x",
	}
	for in, want := range ok {
		got, err := parseSFTPMode(in)
		if err != nil {
			t.Errorf("parseSFTPMode(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("parseSFTPMode(%q) = %s, want %s", in, got, want)
		}
		if got&^os.ModePerm != 0 {
			t.Errorf("parseSFTPMode(%q) kept special bits: %s", in, got)
		}
	}
	for _, in := range []string{"999", "75", "abc", "0x755", "", " 755"} {
		if got, err := parseSFTPMode(in); err == nil {
			t.Errorf("parseSFTPMode(%q) = %s, want an error", in, got)
		}
	}
}

// ---- helpers ----

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func writeFile(t *testing.T, p, data string, mode os.FileMode) {
	t.Helper()
	mkdirAll(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
}
