package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"webkvm/internal/models"
)

// The built-in host card's credential surface: admin-only, narrow (no
// protocol/host/key to talk about), and its "test" must run the same
// bytes the terminal auto-types. The live login probe is swapped for a
// deterministic stand-in in every test that needs it.

func setHostProbe(t *testing.T, p func(user, password string, timeout time.Duration) hostLoginOutcome) {
	t.Helper()
	old := hostLoginProbe
	hostLoginProbe = p
	t.Cleanup(func() { hostLoginProbe = old })
}

func hostCardReq(t *testing.T, r *chi.Mux, token, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// The whole credential surface answers 404 — not 403 — to anyone who is
// not an admin, so the card neither confirms nor denies its existence.
// An operator CAN write a regular machine, which is what makes this
// narrowing worth pinning.
func TestHostCardEndpointsRefuseNonAdmins(t *testing.T) {
	r, token := liveRouter(t, "opuser", models.RoleOperator, nil)
	for _, c := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/api/ssh-machines/host", `{"user":"khl","auth_type":"password"}`},
		{http.MethodPost, "/api/ssh-machines/host/test", ``},
		{http.MethodGet, "/api/ssh-machines/host", ``},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if got, _ := hostCardReq(t, r, token, c.method, c.path, c.body); got != http.StatusNotFound {
				t.Fatalf("got %d, want 404", got)
			}
		})
	}
}

func TestHostCardUpdateReadback(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	body := `{"user":"khl","auth_type":"password","notes":"nightly","secret":{"password":"hunter2"}}`
	code, raw := hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host", body)
	if code != http.StatusOK {
		t.Fatalf("PUT host = %d: %s", code, raw)
	}
	var m struct {
		ID          string `json:"id"`
		User        string `json:"user"`
		AuthType    string `json:"auth_type"`
		HasPassword bool   `json:"has_password"`
		Builtin     bool   `json:"builtin"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "host" || m.User != "khl" || m.AuthType != "password" || !m.HasPassword || !m.Builtin {
		t.Fatalf("PUT response = %+v, want the configured host card", m)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("the response must not carry the stored password")
	}

	// The card shows up in the admin's listing with the flag.
	code, raw = hostCardReq(t, r, token, http.MethodGet, "/api/ssh-machines", "")
	if code != http.StatusOK {
		t.Fatalf("GET list = %d", code)
	}
	if !strings.Contains(string(raw), `"has_password":true`) {
		t.Fatalf("admin listing must carry has_password on the host card: %s", raw)
	}

	// Keep: no secret object, password mode, an account was stored.
	code, raw = hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host", `{"user":"khl","auth_type":"password"}`)
	if code != http.StatusOK {
		t.Fatalf("keep-password PUT = %d: %s", code, raw)
	}
	var m2 struct {
		HasPassword bool `json:"has_password"`
	}
	if err := json.Unmarshal(raw, &m2); err != nil || !m2.HasPassword {
		t.Fatalf("keep lost the stored password: %s", raw)
	}

	// Manual mode clears it.
	code, raw = hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host", `{"auth_type":"none"}`)
	if code != http.StatusOK {
		t.Fatalf("none PUT = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &m2); err != nil || m2.HasPassword {
		t.Fatalf("none must clear the stored password: %s", raw)
	}
}

func TestHostCardUpdateRejectsKeyAuth(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	for _, body := range []string{
		`{"user":"khl","auth_type":"key","secret":{"password":"x"}}`,
		`{"user":"khl","auth_type":"mosh"}`,
	} {
		code, raw := hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host", body)
		if code != http.StatusBadRequest {
			t.Fatalf("PUT %s = %d, want 400: %s", body, code, raw)
		}
	}
}

func TestHostCardTestNoCredentials(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	code, raw := hostCardReq(t, r, token, http.MethodPost, "/api/ssh-machines/host/test", "")
	if code != http.StatusOK {
		t.Fatalf("test = %d: %s", code, raw)
	}
	var res struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Status != "no_credentials" {
		t.Fatalf("without stored credentials = %+v, want no_credentials", res)
	}
}

func TestHostCardTestConnected(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	if code, raw := hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host",
		`{"user":"khl","auth_type":"password","secret":{"password":"hunter2"}}`); code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", code, raw)
	}
	setHostProbe(t, func(user, password string, timeout time.Duration) hostLoginOutcome {
		if user != "khl" || password != "hunter2" {
			t.Errorf("probe got %q/%q, want the stored pair", user, password)
		}
		return hostLoginOutcome{status: "connected"}
	})
	code, raw := hostCardReq(t, r, token, http.MethodPost, "/api/ssh-machines/host/test", "")
	if code != http.StatusOK {
		t.Fatalf("test = %d: %s", code, raw)
	}
	var res struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || !res.OK || res.Status != "connected" {
		t.Fatalf("test = %s, %+v; want connected", raw, res)
	}
}

func TestHostCardTestAuthFailed(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	if code, raw := hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host",
		`{"user":"khl","auth_type":"password","secret":{"password":"wrong"}}`); code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", code, raw)
	}
	setHostProbe(t, func(user, password string, timeout time.Duration) hostLoginOutcome {
		return hostLoginOutcome{status: "auth_failed", detail: "login incorrect"}
	})
	code, raw := hostCardReq(t, r, token, http.MethodPost, "/api/ssh-machines/host/test", "")
	if code != http.StatusOK {
		t.Fatalf("test = %d: %s", code, raw)
	}
	var res struct {
		OK      bool   `json:"ok"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.OK || res.Status != "auth_failed" {
		t.Fatalf("test = %s, %+v; want auth_failed", raw, res)
	}
}

// A deadline give-up is not "wrong password": the probe must surface it as
// status "error" with the environment detail, so the UI sends the
// operator to look at the machine instead of re-typing the pair.
func TestHostCardTestErrorStatus(t *testing.T) {
	r, token := liveRouter(t, "adminuser", models.RoleAdmin, nil)
	if code, raw := hostCardReq(t, r, token, http.MethodPut, "/api/ssh-machines/host",
		`{"user":"khl","auth_type":"password","secret":{"password":"hunter2"}}`); code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", code, raw)
	}
	setHostProbe(t, func(user, password string, timeout time.Duration) hostLoginOutcome {
		return hostLoginOutcome{status: "error", detail: "login did not reach the password prompt in time"}
	})
	code, raw := hostCardReq(t, r, token, http.MethodPost, "/api/ssh-machines/host/test", "")
	if code != http.StatusOK {
		t.Fatalf("test = %d: %s", code, raw)
	}
	var res struct {
		OK      bool   `json:"ok"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Status != "error" || res.Message != "login did not reach the password prompt in time" {
		t.Fatalf("test = %+v; want status=error with the give-up detail", res)
	}
}

// ---- the auto-feed state machine ----
//
// The terminal pump and the test probe share hostAutoFeed, so its prompt
// classification gets its own table: user stage, password stage,
// rejection, re-prompt, deadline, and idempotency after settle.

func TestHostAutoFeedStages(t *testing.T) {
	now := time.Now()
	t.Run("password prompt types the password once", func(t *testing.T) {
		f := newHostAutoFeed("khl", "hunter2", now.Add(30*time.Second))
		w, g := f.observe([]byte("Password: "), now)
		if g || w != "hunter2\r" {
			t.Fatalf("observe = %q, %v", w, g)
		}
		if w, g = f.observe([]byte("Password: "), now); w != "" || g {
			t.Fatalf("second feed = %q, %v; the feed is one-shot", w, g)
		}
	})
	t.Run("user prompt first, password second", func(t *testing.T) {
		f := newHostAutoFeed("khl", "hunter2", now.Add(30*time.Second))
		if w, g := f.observe([]byte("login: "), now); g || w != "khl\r" {
			t.Fatalf("user stage = %q, %v", w, g)
		}
		if w, g := f.observe([]byte("Password: "), now); g || w != "hunter2\r" {
			t.Fatalf("password stage = %q, %v", w, g)
		}
	})
	t.Run("re-prompt after the user is a rejection", func(t *testing.T) {
		f := newHostAutoFeed("khl", "hunter2", now.Add(30*time.Second))
		f.observe([]byte("login: "), now)
		if w, g := f.observe([]byte("login: "), now); !g || w != "" {
			t.Fatalf("re-prompt = %q, %v; want give-up", w, g)
		}
	})
	t.Run("rejection marker wins over the prompt", func(t *testing.T) {
		f := newHostAutoFeed("khl", "hunter2", now.Add(30*time.Second))
		if w, g := f.observe([]byte("Authentication failure, password retry\nPassword: "), now); !g || w != "" {
			t.Fatalf("rejection = %q, %v; want give-up", w, g)
		}
	})
	t.Run("deadline gives up", func(t *testing.T) {
		f := newHostAutoFeed("khl", "hunter2", now.Add(time.Millisecond))
		if _, g := f.observe([]byte("still loading…"), now.Add(2*time.Millisecond)); !g {
			t.Fatal("past deadline must give up")
		}
	})
}

func TestHostLoginPromptLine(t *testing.T) {
	cases := []struct {
		line       string
		password   bool
		userPrompt bool
	}{
		{"Password: ", true, false},
		{"password: ", true, false},
		{"Password:", true, false},
		{"Mot de passe: ", true, false},
		{"密码：", true, false},
		{"login: ", false, true},
		{"Username: ", false, true},
		{"Welcome to Ubuntu (GNU/Linux) 24.04", false, false},
		{"khl@webkvm:~$", false, false},
		{"", false, false},
		{"Password", false, false}, // no colon: not a prompt line
		// A PAM module that hides the cursor wraps the prompt in CSI
		// codes; the suffix check must still see the trailing colon.
		{"\x1b[?25lPassword: \x1b[0m", true, false},
		{"\x1b]0;webkvm\x07login: ", false, true},
	}
	for _, c := range cases {
		p, u := hostLoginPromptLine(c.line)
		if p != c.password || u != c.userPrompt {
			t.Errorf("hostLoginPromptLine(%q) = %v, %v; want %v, %v", c.line, p, u, c.password, c.userPrompt)
		}
	}
}

// sanitizePTYLine is the last stop before a PTY line reaches a browser:
// an echoing PAM module can put anything in it, including the password
// the feed just typed. Control bytes and escapes must not survive.
func TestSanitizePTYLine(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"plain line", "plain line"},
		{"\x1b[?25lPassword: \x1b[0m", "Password:"},
		{"a\x01b\x1bc\tok", "abc\tok"},
		{"  spaced  ", "spaced"},
		{"tab\tkept", "tab\tkept"},
	}
	for _, c := range cases {
		if got := sanitizePTYLine(c.in); got != c.out {
			t.Errorf("sanitizePTYLine(%q) = %q, want %q", c.in, got, c.out)
		}
	}
	// The cap: 130 visible characters come back as 120 plus the marker.
	long := strings.Repeat("x", 130)
	got := sanitizePTYLine(long)
	if len([]rune(got)) != 121 || !strings.HasSuffix(got, "…") {
		t.Errorf("sanitizePTYLine(130 chars) = %d runes, want 120 + ellipsis", len([]rune(got)))
	}
}
