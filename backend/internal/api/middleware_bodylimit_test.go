package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLimitRequestBodyCapsJSONHandlers reproduces a measured DoS.
//
// Handlers all over the API call json.NewDecoder(r.Body).Decode(&req)
// with no bound, and there was no global cap. Verified against the live
// backend: a single 200 MB POST to /api/nodes — answered 400
// "uri is required", so it never reached any business logic — pushed
// the process RSS from 100 MB to 754 MB. Go returns that memory to the
// OS only lazily, so a few concurrent requests from any authenticated
// user are enough to OOM the backend, killing every VM console and the
// UI along with it.
func TestLimitRequestBodyCapsJSONHandlers(t *testing.T) {
	var decoded struct {
		Name string `json:"name"`
	}
	var decodeErr error
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeErr = json.NewDecoder(r.Body).Decode(&decoded)
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"name":"` + strings.Repeat("A", int(defaultMaxBodyBytes)+4096) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if decodeErr == nil {
		t.Fatalf("an oversized body must fail to decode; handler read %d bytes", len(decoded.Name))
	}
	if !strings.Contains(decodeErr.Error(), "too large") {
		t.Errorf("expected a MaxBytesReader error, got: %v", decodeErr)
	}
}

// TestLimitRequestBodyAllowsNormalPayloads: the cap must be invisible
// to real traffic. A few KB of JSON is the normal shape of every
// mutation in the API.
func TestLimitRequestBodyAllowsNormalPayloads(t *testing.T) {
	var decoded struct {
		Name string `json:"name"`
	}
	var decodeErr error
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeErr = json.NewDecoder(r.Body).Decode(&decoded)
	}))

	body := `{"name":"` + strings.Repeat("A", 8192) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/nodes", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if decodeErr != nil {
		t.Fatalf("an 8 KB payload must be accepted, got: %v", decodeErr)
	}
	if len(decoded.Name) != 8192 {
		t.Errorf("payload was truncated: got %d bytes", len(decoded.Name))
	}
}

// TestLimitRequestBodySkipsBulkUploads: ISO/disk/media/VM-import
// streams are gigabytes by nature and install their own, much larger
// MaxBytesReader. Applying the 1 MB cap to them would break every
// upload in the product.
func TestLimitRequestBodySkipsBulkUploads(t *testing.T) {
	for _, path := range bulkUploadPrefixes {
		read := 0
		h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 64)
			for {
				n, err := r.Body.Read(buf)
				read += n
				if err != nil {
					return
				}
			}
		}))
		size := int(defaultMaxBodyBytes) + 4096
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", size)))
		h.ServeHTTP(httptest.NewRecorder(), req)

		if read != size {
			t.Errorf("%s: handler read %d of %d bytes; the global cap must not apply here",
				path, read, size)
		}
	}
}

// Bug 9: /api/vms/{id}/cover has an id segment, so it was not matched by
// bulkUploadPrefixes and the global 1 MiB cap overrode UploadCover's own
// 8 MB limit. Only the exact per-VM cover route is exempt.
func TestExemptFromGlobalLimitCoverRoute(t *testing.T) {
	cases := map[string]bool{
		"/api/vms/abc-123/cover":    true,
		"/api/vms/import-ova":       true,
		"/api/vms//cover":           false,
		"/api/vms/cover":            false,
		"/api/vms/a/b/cover":        false,
		"/api/vms/abc-123/cover/x":  false,
		"/api/vms/abc-123/metadata": false,
		"/api/nodes":                false,
	}
	for path, want := range cases {
		if got := exemptFromGlobalLimit(path); got != want {
			t.Errorf("exemptFromGlobalLimit(%q) = %v, want %v", path, got, want)
		}
	}

	read := 0
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			read += n
			if err != nil {
				return
			}
		}
	}))
	size := int(defaultMaxBodyBytes) * 2
	req := httptest.NewRequest(http.MethodPost, "/api/vms/abc/cover", strings.NewReader(strings.Repeat("x", size)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if read != size {
		t.Fatalf("cover upload was capped by the global limit: read %d of %d", read, size)
	}
}

// The transfers route is exempt from the global cap for its multipart
// branch, which left its JSON branch (action=download / action=copy)
// unbounded: a 2 GB JSON body was buffered straight into memory by the
// decoder. MachineTransferStart now installs its own 1 MiB limit there.
func TestDecodeJSONLimitedBoundsTheExemptJSONBranch(t *testing.T) {
	var out struct {
		RemotePath string `json:"remote_path"`
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ssh-machines/m-1/transfers",
		strings.NewReader(`{"remote_path":"`+strings.Repeat("A", 2<<20)+`"}`))
	rec := httptest.NewRecorder()
	err := decodeJSONLimited(rec, req, &out, 1<<20)
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		t.Fatalf("err = %v, want a MaxBytesError", err)
	}

	small := httptest.NewRequest(http.MethodPost, "/api/ssh-machines/m-1/transfers",
		strings.NewReader(`{"remote_path":"/var/log","action":"copy"}`))
	if err := decodeJSONLimited(httptest.NewRecorder(), small, &out, 1<<20); err != nil {
		t.Fatalf("a normal copy request must decode, got %v", err)
	}
	if out.RemotePath != "/var/log" {
		t.Fatalf("decoded = %+v", out)
	}
}

// A path that is exempt must still be readable in full, and one that is
// not must be capped: the exemption list is the whole defence here.
func TestExemptFromGlobalLimitKeepsOtherRoutesCapped(t *testing.T) {
	read := 0
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			read += n
			if err != nil {
				return
			}
		}
	}))
	size := int(defaultMaxBodyBytes) * 2
	// Same machine id, different suffix: /sftp/mkdir is a small JSON
	// route and must not inherit the upload exemption.
	req := httptest.NewRequest(http.MethodPost, "/api/ssh-machines/m-1/sftp/mkdir",
		strings.NewReader(strings.Repeat("x", size)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if read > int(defaultMaxBodyBytes) {
		t.Fatalf("a JSON route read %d bytes; the global cap must apply", read)
	}
}

// SFTP upload transfers land on /api/ssh-machines/{id}/transfers: an id
// segment in the middle, so no prefix match, and a multipart body the
// handler bounds itself at 4 GiB. Under the global 1 MiB cap every file
// over that was truncated into a 400 before the handler ever ran.
func TestExemptFromGlobalLimitTransferRoute(t *testing.T) {
	cases := map[string]bool{
		"/api/ssh-machines/m-1/transfers":          true,
		"/api/ssh-machines/host/transfers":         true,
		"/api/ssh-machines//transfers":             false, // empty id segment
		"/api/ssh-machines/transfers":              false, // no id segment
		"/api/ssh-machines/a/b/transfers":          false, // deeper than one segment
		"/api/ssh-machines/m-1/transfers/x/result": false, // the download-result GET
		"/api/ssh-machines/m-1":                    false,
		"/api/ssh-machines/m-1/sftp/mkdir":         false,
		"/api/ssh-machines/m-1/terminal/ticket":    false,
	}
	for path, want := range cases {
		if got := exemptFromGlobalLimit(path); got != want {
			t.Errorf("exemptFromGlobalLimit(%q) = %v, want %v", path, got, want)
		}
	}

	var handlerSaw int64
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		handlerSaw = n
	}))
	size := int64(defaultMaxBodyBytes) * 3
	req := httptest.NewRequest(http.MethodPost, "/api/ssh-machines/m-1/transfers",
		strings.NewReader(strings.Repeat("x", int(size))))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if handlerSaw != size {
		t.Fatalf("transfer upload was capped by the global limit: read %d of %d", handlerSaw, size)
	}
}
