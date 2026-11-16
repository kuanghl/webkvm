package api

// The built-in host card is the only machine that is never dialed: its
// file plane reads and writes this very machine with webkvm's own
// privileges, which makes it the most powerful route in the machine
// surface. Admin-only has to hold at every entry point, and a refusal
// must be indistinguishable from an unknown id.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"webkvm/internal/models"
	"webkvm/internal/sshmachines"
)

func hostCardFileRoutes() []capRoute {
	return []capRoute{
		{http.MethodGet, "/api/ssh-machines/host/sftp/list"},
		{http.MethodGet, "/api/ssh-machines/host/sftp/list?path=/etc"},
		{http.MethodGet, "/api/ssh-machines/host/sftp/stat?paths=/etc/hostname"},
		{http.MethodGet, "/api/ssh-machines/host/sftp/realpath?path=/etc"},
		{http.MethodPost, "/api/ssh-machines/host/sftp/mkdir"},
		{http.MethodPost, "/api/ssh-machines/host/sftp/delete"},
		{http.MethodPost, "/api/ssh-machines/host/transfers"},
		{http.MethodGet, "/api/ssh-machines/host/transfers"},
		{http.MethodPost, "/api/ssh-machines/host/transfers/job-1/cancel"},
		{http.MethodGet, "/api/ssh-machines/host/transfers/job-1/result"},
		{http.MethodDelete, "/api/ssh-machines/host/transfers/job-1"},
	}
}

// can_console=true is the support-technician grant: it opens a registered
// machine's files, and must stop short of this host's disk. An operator
// without the grant never got the file plane at all, so both roles have
// to land on the same answer here.
func TestHostCardFilePlaneNeedsAdmin(t *testing.T) {
	for _, who := range []struct {
		role string
		perm *bool
	}{
		{models.RoleOperator, boolTrue()},
		{models.RoleViewer, boolTrue()},
	} {
		r, token := liveRouter(t, "user-"+who.role, who.role, &models.UserPermissions{CanConsole: who.perm})
		for _, rt := range hostCardFileRoutes() {
			t.Run(who.role+" "+rt.method+" "+rt.path, func(t *testing.T) {
				if got := liveStatus(t, r, token, rt.method, rt.path); got != http.StatusNotFound {
					t.Fatalf("%s %s = %d, want 404: the host card's files are admin-only, and a "+
						"403 would confirm the built-in record exists", rt.method, rt.path, got)
				}
			})
		}
	}
}

// The mirror case: the card is not a 404 for everybody. Listing it is the
// point of the feature, and it runs on the in-process session, so this is
// also the test that the local pair answers the real filesystem at all.
func TestHostCardFileListingServesTheHost(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/ssh-machines/host/sftp/list", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/ssh-machines/host/sftp/list = %d %s; want 200 for an admin",
			rr.Code, rr.Body.String())
	}
	var out struct {
		Path    string `json:"path"`
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if out.Path != "/" {
		t.Errorf("path = %q, want / (the host card opens on the filesystem root)", out.Path)
	}
	if len(out.Entries) < 3 {
		t.Errorf("entries = %d; the host's root should not read as nearly empty", len(out.Entries))
	}
}

// Terminal ticket and delete still refuse the card outright: those
// surfaces need a dial or a record, and there is nothing to dial or
// remove here. The /test route is the exception since the host card
// learned to store credentials: for an admin it verifies the stored
// pair (see TestHostCardTestEndpoint), for anyone else it 404s.
//
// The refusal is role-aware: an admin gets the readable 400 ("the card
// is read-only there") because the admin knows the card exists, while a
// non-admin gets the same 404 as an unknown id — confirming the
// built-in record would be a disclosure.
func TestHostCardTerminalPlaneStaysRefused(t *testing.T) {
	routes := []capRoute{
		{http.MethodPost, "/api/ssh-machines/host/terminal-ticket"},
		{http.MethodDelete, "/api/ssh-machines/host"},
	}
	for _, who := range []struct {
		name string
		role string
		want int
	}{
		{"admin", models.RoleAdmin, http.StatusBadRequest},
		{"operator", models.RoleOperator, http.StatusNotFound},
	} {
		r, token := liveRouter(t, "user-"+who.name, who.role, &models.UserPermissions{CanConsole: boolTrue()})
		for _, rt := range routes {
			t.Run(who.name+" "+rt.method+" "+rt.path, func(t *testing.T) {
				if got := liveStatus(t, r, token, rt.method, rt.path); got != who.want {
					t.Fatalf("%s %s = %d, want %d: the dialing surface must stay refused for the host card",
						rt.method, rt.path, got, who.want)
				}
			})
		}
	}
}

// Reading the host is only half of the feature: files have to land on this
// disk and come back off it. This walks one small file through
// upload → list → download → /result, which is the whole path the Files
// tab drives, and it is the only test that proves the in-process session
// writes (putFile's scratch-and-rename, the mode re-assertion) really
// reaches the filesystem.
func TestHostCardFilePlaneRoundTrip(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	stage := t.TempDir()
	dst := filepath.Join(stage, "push.sh")
	payload := []byte("#!/bin/sh\necho host card\n")

	body, ctype := uploadForm(dst, "push.sh", payload, "0750")
	rr := hostCardDo(t, r, token, http.MethodPost, "/api/ssh-machines/host/transfers", body, ctype)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("upload = %d %s, want 202", rr.Code, rr.Body.String())
	}
	waitHostJob(t, r, token, hostCardJobID(t, rr))

	// The destination is the real path the browser asked for, with the
	// mode the form carried: the SFTP create takes no perm argument, so
	// only an explicit chmod after the write can hold 0750 against a
	// umask of 077.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("uploaded file is not on disk: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("uploaded content = %q, want %q", got, payload)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("uploaded mode = %v, want 750", fi.Mode().Perm())
	}

	rr = hostCardDo(t, r, token, http.MethodGet,
		"/api/ssh-machines/host/sftp/list?path="+url.QueryEscape(stage), nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", rr.Code, rr.Body.String())
	}
	var listing struct {
		Entries []struct {
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			IsDir bool   `json:"is_dir"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "push.sh" ||
		listing.Entries[0].Size != int64(len(payload)) {
		t.Fatalf("listing = %+v, want exactly push.sh at %d bytes (a leftover scratch file "+
			"would show up here)", listing.Entries, len(payload))
	}

	rr = hostCardDo(t, r, token, http.MethodPost, "/api/ssh-machines/host/transfers",
		jsonBody(map[string]any{"action": "download", "remote_path": dst}), "application/json")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("download = %d %s, want 202", rr.Code, rr.Body.String())
	}
	jobID := hostCardJobID(t, rr)
	waitHostJob(t, r, token, jobID)

	rr = hostCardDo(t, r, token, http.MethodGet,
		"/api/ssh-machines/host/transfers/"+jobID+"/result", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("result = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), payload) {
		t.Errorf("result body = %q, want %q", rr.Body.Bytes(), payload)
	}
}

func hostCardDo(t *testing.T, r *chi.Mux, token, method, path string, body io.Reader, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func uploadForm(dst, name string, content []byte, mode string) (io.Reader, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("action", "upload")
	// An empty remote_path would make the handler fall back to the
	// upload's bare filename, i.e. the filesystem root.
	_ = w.WriteField("remote_path", dst)
	if mode != "" {
		_ = w.WriteField("mode", mode)
	}
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		panic(err)
	}
	if _, err := part.Write(content); err != nil {
		panic(err)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func jsonBody(v any) io.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bytes.NewReader(b)
}

func hostCardJobID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		ID        string `json:"id"`
		MachineID string `json:"machine_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode job: %v (%s)", err, rr.Body.String())
	}
	if out.ID == "" {
		t.Fatalf("no job id in %s", rr.Body.String())
	}
	if out.MachineID != sshmachines.HostMachineID {
		t.Errorf("job machine_id = %q, want %q", out.MachineID, sshmachines.HostMachineID)
	}
	return out.ID
}

// The progress stream is SSE, so a handler-level test has to re-read the
// queue to see a worker finish. Production never polls it.
func waitHostJob(t *testing.T, r *chi.Mux, token, jobID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rr := hostCardDo(t, r, token, http.MethodGet, "/api/ssh-machines/host/transfers", nil, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("queue = %d %s", rr.Code, rr.Body.String())
		}
		var jobs []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &jobs); err != nil {
			t.Fatalf("decode queue: %v", err)
		}
		for _, j := range jobs {
			if j["id"] != jobID {
				continue
			}
			status, _ := j["status"].(string)
			switch status {
			case "done":
				return
			case "error":
				t.Fatalf("job %s failed: %v", jobID, j["error"])
			case "canceled":
				t.Fatalf("job %s was canceled", jobID)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished (queue: %s)", jobID, rr.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
