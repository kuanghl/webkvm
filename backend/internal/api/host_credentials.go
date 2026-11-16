package api

// The built-in host card is the one machine whose "terminal" is this
// machine's own /bin/login. It gets the same credential management as
// the SSH machines — an account and a stored password — so the getty
// terminal signs itself in and a test endpoint verifies the pair.
//
// Rules that differ from the machine surface:
//   - every credential endpoint of the host card answers 404 to a
//     non-admin (the same refusal shape as gateHostCard: a 403 would
//     confirm the record); the dialing surfaces answer through
//     rejectBuiltin instead (404 for non-admins, 400 for admins);
//   - the record is created on first save (UpdateHost) and may never be
//     deleted (DeleteMachine keeps rejecting the id, and so does the
//     store's own guard);
//   - only password authentication exists: a getty login has no key
//     half, so there is no host key to pin either.

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"webkvm/internal/safego"
	"webkvm/internal/sshmachines"
)

// hostMachineRequest is the PUT body for the host card. It is narrower
// than machineRequest on purpose: there is no protocol, host, port, key
// or host key to talk about.
type hostMachineRequest struct {
	User     string `json:"user"`
	AuthType string `json:"auth_type"` // password | none
	Notes    string `json:"notes"`
	Secret   *struct {
		Password string `json:"password"`
	} `json:"secret"`
}

// UpdateHostMachine stores the host card's account + password. The
// route sits in the operator+ group; the handler narrows it to admins
// because the credential grants a shell on the machine webkvm runs on.
func (h *Handler) UpdateHostMachine(w http.ResponseWriter, r *http.Request) {
	if !requestIsAdmin(r) {
		jsonErr(w, http.StatusNotFound, "machine not found")
		return
	}
	var req hostMachineRequest
	if err := decodeBody(r, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	// A missing secret object, or a present-but-blank password, means
	// "keep" (the same rule as the machine surface): both normalize to
	// a nil Secret here, so only a non-empty password ever reaches the
	// store, and the UI's "an empty box keeps the current one" works in
	// every mode without a second rule.
	secret := (*sshmachines.SecretInput)(nil)
	if req.Secret != nil && req.Secret.Password != "" {
		secret = &sshmachines.SecretInput{Password: req.Secret.Password}
	}
	machine, err := h.machines.UpdateHost(sshmachines.UpdateHostRequest{
		User:     req.User,
		AuthType: req.AuthType,
		Notes:    req.Notes,
		Secret:   secret,
	})
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.update", "machine:"+sshmachines.HostMachineID, map[string]interface{}{
			"name": machine.Name,
			"auto": machine.AuthType == sshmachines.AuthPassword,
		}))
	}
	jsonResp(w, http.StatusOK, machine)
}

// TestHostMachine verifies the stored pair against the host's own
// login — the same bytes the terminal will auto-type, so "connected"
// here is "the next terminal session will not stop at the password
// prompt".
func (h *Handler) TestHostMachine(w http.ResponseWriter, r *http.Request) {
	if !requestIsAdmin(r) {
		jsonErr(w, http.StatusNotFound, "machine not found")
		return
	}
	start := time.Now()
	machine := h.machines.HostMachine()
	sec, _, err := h.machines.GetSecret(sshmachines.HostMachineID)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "read credentials: "+err.Error())
		return
	}
	res := testResult{}
	if machine.AuthType != sshmachines.AuthPassword || machine.User == "" || sec.Password == "" {
		res.OK = false
		res.Status = "no_credentials"
		res.Message = "no account or password is stored yet"
		jsonResp(w, http.StatusOK, res)
		return
	}
	outcome := hostLoginProbe(machine.User, sec.Password, 15*time.Second)
	switch outcome.status {
	case "connected":
		res.OK = true
		res.Status = "connected"
		res.LatencyMS = time.Since(start).Milliseconds()
	case "auth_failed":
		res.OK = false
		res.Status = "auth_failed"
		res.Message = "login rejected"
	default:
		res.OK = false
		res.Status = "error"
		res.Message = outcome.detail
	}
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.test", "machine:"+sshmachines.HostMachineID, map[string]interface{}{
			"result": res.Status,
		}))
	}
	jsonResp(w, http.StatusOK, res)
}

// hostLoginOutcome is the verdict of one credential check.
type hostLoginOutcome struct {
	status string // connected | auth_failed | error
	detail string
}

// hostLoginProbe verifies an account/password pair exactly the way the
// host terminal will use it: a real PTY, a real /bin/login, a real PAM
// stack. No shadow file access, no guessing. It is a variable so tests
// substitute a deterministic stand-in for the live dial.
var hostLoginProbe = func(user, password string, timeout time.Duration) hostLoginOutcome {
	if _, err := os.Stat("/bin/login"); err != nil {
		return hostLoginOutcome{status: "error", detail: "host login unavailable: /bin/login not found"}
	}
	cmd := exec.Command("/bin/login", "-p", user)
	cmd.Env = hostTerminalEnv()
	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: 120, Rows: 30}, &syscall.SysProcAttr{Setsid: true})
	if err != nil {
		return hostLoginOutcome{status: "error", detail: "start login: " + err.Error()}
	}
	defer ptmx.Close()
	defer func() {
		// Kill the process AND its group: the login is the
		// session/group leader (Setsid), and anything it spawned
		// before dying — a PAM side-process, a .bashrc daemon — would
		// otherwise survive the leader as an orphan running as the
		// stored user.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	deadlineT := time.NewTimer(timeout)
	defer deadlineT.Stop()
	// decide arms itself once the password was typed: a rejection
	// re-prompts or exits well inside the window, while a live shell
	// just waits — so "still alive three seconds later" is the success.
	decideT := time.NewTimer(3 * time.Second)
	decideT.Stop() // disarmed until the password is out
	defer decideT.Stop()
	feed := newHostAutoFeed(user, password, time.Now().Add(10*time.Second))
	buf := make([]byte, 4096)
	chunks := make(chan []byte, 16)
	readErr := make(chan error, 1)
	go func() {
		defer safego.Recover("host_login_probe_read")
		defer close(chunks)
		eio := 0
		for {
			n, rerr := ptmx.Read(buf)
			if rerr != nil {
				// Linux hands EIO to the master during execve /
				// tty transitions; the terminal handler retries
				// those, and so does this reader.
				if errors.Is(rerr, syscall.EIO) && eio < 20 {
					eio++
					<-time.After(20 * time.Millisecond)
					continue
				}
				select {
				case readErr <- rerr:
				default:
				}
				return
			}
			eio = 0
			cp := make([]byte, n)
			copy(cp, buf[:n])
			select {
			case chunks <- cp:
			default:
				// The probe already decided (or is leaving): the
				// unread output is dropped, not buffered.
			}
		}
	}()

	var decide <-chan time.Time
	var tail []byte
	for {
		select {
		case <-deadlineT.C:
			return hostLoginOutcome{status: "error", detail: "login did not reach a decision in time"}
		case <-decide:
			return hostLoginOutcome{status: "connected"}
		case c, ok := <-chunks:
			if !ok {
				// Reader is out because the process went away. A
				// successful login keeps its shell running, so an
				// early death is a rejection (or a crash).
				return hostLoginOutcome{status: "auth_failed", detail: sanitizePTYLine(hostLastNonEmptyLine(tail))}
			}
			tail = append(tail, c...)
			if len(tail) > 8192 {
				tail = tail[len(tail)-8192:]
			}
			if w, g := feed.observe(c, time.Now()); w != "" {
				if _, werr := ptmx.Write([]byte(w)); werr != nil {
					return hostLoginOutcome{status: "error", detail: "type auto-login: " + werr.Error()}
				}
				if feed.passwordSent() && decide == nil {
					decideT.Reset(3 * time.Second)
					decide = decideT.C
				}
			} else if g {
				// A deadline give-up is not a rejection of the
				// stored pair: the login never reached a prompt
				// the feed could act on (slow disk, long banner,
				// or a line the feed cannot read), and reporting
				// "login rejected" would send the operator to
				// retype a password that was never the problem.
				if feed.giveUpReason() == hostFeedGiveUpDeadline {
					return hostLoginOutcome{status: "error", detail: "login did not reach the password prompt in time"}
				}
				return hostLoginOutcome{status: "auth_failed", detail: sanitizePTYLine(hostLastNonEmptyLine(tail))}
			}
		case rerr := <-readErr:
			_ = rerr
			return hostLoginOutcome{status: "auth_failed", detail: sanitizePTYLine(hostLastNonEmptyLine(tail))}
		}
	}
}

// ---- the auto-feed state machine (shared: terminal pump and probe) ----

// The give-up reasons a settled feed can carry. The probe maps them:
// a rejection is a statement about the stored pair (auth_failed), but
// a deadline means the login never reached a prompt the feed could act
// on — an environment failure to report as such, not "wrong password".
const (
	hostFeedGiveUpDeadline = "deadline"
	hostFeedGiveUpRejected = "rejected"
	hostFeedGiveUpRePrompt = "re-prompt"
)

// hostAutoFeed drives the one-shot auto-login of the host terminal. The
// getty login prompts arrive as sequential, separate PTY reads — first
// `login:` (skipped when the user was passed to login as an argument),
// then `Password:` — so the feed keeps a two-stage state (user sent,
// password sent) and acts on the last non-empty line of what it has
// seen. Each stage fires at most once, and anything else — a rejection,
// a user re-prompt after we already typed one, the deadline — hands the
// prompt back to the operator untouched (today's behavior).
type hostAutoFeed struct {
	user      string
	password  string
	deadline  time.Time
	tail      []byte
	sentUser  bool
	sentPass  bool
	done      bool
	giveUpWhy string // one of the hostFeedGiveUp* constants, "" while live or after typing
}

func newHostAutoFeed(user, password string, deadline time.Time) *hostAutoFeed {
	return &hostAutoFeed{user: user, password: password, deadline: deadline}
}

// passwordSent reports whether the feed already typed the password; the
// probe uses it to arm the "still alive a moment later == success" timer.
func (a *hostAutoFeed) passwordSent() bool { return a.sentPass }

// giveUpReason reports why the attempt stopped without feeding: one of
// the hostFeedGiveUp* constants after a give-up, "" while the feed is
// still live or settled by typing.
func (a *hostAutoFeed) giveUpReason() string { return a.giveUpWhy }

// hostPasswordKeywords: the terminal environment forces LC_ALL=C, so
// the stock prompt is "Password:"; the other spellings are there
// because a PAM module or a non-C locale can change the line. A false
// positive costs one typed password; a false negative leaves the
// operator at the prompt (today's behavior).
var hostPasswordKeywords = []string{
	"password", "passwort", "passphrase", "senha", "contraseña", "contrasena",
	"mot de passe", "pwd", "密码", "口令", "パスワード",
}

// hostLoginRejectLines are PAM / util-linux rejections that reach the
// PTY. Most distros log "Failed password" instead of printing it; the
// re-prompt check below is what makes the failure case airtight.
var hostLoginRejectLines = []string{
	"login incorrect", "authentication failure", "failed password",
	"authentication token validation failed", "登录失败", "认证失败",
}

// ansiRe strips escape sequences: CSI codes (colors, cursor moves) and
// OSC sequences. A PAM module or a slow banner can surround the prompt
// with "\x1b[?25l … \x1b[0m", and the suffix check in
// hostLoginPromptLine would then miss the trailing ":" — the feed types
// nothing, the deadline fires, and the probe reports a false "login
// rejected".
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// hostLoginPromptLine classifies the line that will hold the next input.
func hostLoginPromptLine(line string) (password, userPrompt bool) {
	line = strings.TrimRight(ansiRe.ReplaceAllString(line, ""), "\r ")
	if line == "" {
		return false, false
	}
	if !strings.HasSuffix(line, ":") && !strings.HasSuffix(line, "：") {
		return false, false
	}
	low := strings.ToLower(line)
	for _, k := range hostPasswordKeywords {
		if strings.Contains(low, k) {
			return true, false
		}
	}
	if strings.Contains(low, "login") || strings.Contains(low, "username") {
		return false, true
	}
	return false, false
}

func hostLastNonEmptyLine(data []byte) string {
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimRight(lines[i], "\r "); l != "" {
			return l
		}
	}
	return ""
}

// sanitizePTYLine trims a PTY line for client display. The probe hands
// the last visible login line back in its error detail, and a
// pathological PAM module can echo anything into it — including, on a
// module that echoes input, the typed password itself — so control
// bytes and escape sequences are stripped and the line is capped.
func sanitizePTYLine(line string) string {
	line = ansiRe.ReplaceAllString(line, "")
	var b strings.Builder
	for _, r := range line {
		switch {
		case r == '\t':
			b.WriteRune(r)
		case r >= 0x20 && r != 0x7f:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " \t")
	if len(out) > 120 {
		out = out[:120] + "…"
	}
	return out
}

// hostLoginRejected reports a visible rejection anywhere in the tail.
func hostLoginRejected(tail []byte) bool {
	low := strings.ToLower(string(tail))
	for _, marker := range hostLoginRejectLines {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// observe consumes a fresh PTY chunk. It returns the text to type (at
// most once per stage: the user, then the password); giveUp=true when
// the attempt must stop without feeding. giveUp fires only on the
// transition — after done, every call returns ("", false), so the
// caller needs no extra flag.
func (a *hostAutoFeed) observe(chunk []byte, now time.Time) (write string, giveUp bool) {
	if a.done {
		return "", false
	}
	if now.After(a.deadline) {
		a.done = true
		a.giveUpWhy = hostFeedGiveUpDeadline
		return "", true
	}
	a.tail = append(a.tail, chunk...)
	if len(a.tail) > 4096 {
		a.tail = a.tail[len(a.tail)-4096:]
	}
	if hostLoginRejected(a.tail) {
		a.done = true
		a.giveUpWhy = hostFeedGiveUpRejected
		return "", true
	}
	isPassword, isUserPrompt := hostLoginPromptLine(hostLastNonEmptyLine(a.tail))
	switch {
	case isPassword && !a.sentPass:
		a.sentPass = true
		a.done = true
		return a.password + "\r", false
	case isUserPrompt && !a.sentUser:
		a.sentUser = true
		return a.user + "\r", false
	case isUserPrompt && a.sentUser:
		// login -p <user> re-asks for the name when the account is
		// unknown: the stored pair cannot log in.
		a.done = true
		a.giveUpWhy = hostFeedGiveUpRePrompt
		return "", true
	}
	return "", false
}
