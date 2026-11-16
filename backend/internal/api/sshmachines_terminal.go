package api

// Machine terminal WebSocket bridge.
//
// The handler is a thin dispatcher on machine.Protocol; the pumping
// (WS <-> remote, pings, idle timeout, resize JSON frames) is the
// shared pumpMachineTerminal, a clone of the HostTerminal loop from
// console_serial.go with the PTY end replaced by a
// machineRemoteTerminal:
//
//   - ssh    -> ssh.Session with a requested pty (TERM xterm-256color)
//   - telnet -> internal/telnet (uniterm port: IAC filter, NAW/TTYPE,
//     auto-login)
//   - serial -> internal/serialx (go.bug.st/serial, normalized newlines)
//   - mosh   -> refused (not in this build)
//
// A second tab opening the same machine force-closes the first one with
// close code 4409 ("replaced by a newer session"), the same pattern as
// serialSessions — the evicted front-end then stands down instead of
// fighting over the connection.

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"webkvm/internal/audit"
	"webkvm/internal/safego"
	"webkvm/internal/serialx"
	"webkvm/internal/sshmachines"
	"webkvm/internal/telnet"
)

// machineTerminalSessions: machineID -> the currently active WebSocket.
var machineTerminalSessions sync.Map

// machineRemoteTerminal is the protocol end of the bridge.
type machineRemoteTerminal interface {
	io.Reader
	io.Writer
	Resize(cols, rows int) error
	Close() error
}

func (h *Handler) MachineTerminal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == sshmachines.HostMachineID {
		// The host's own shell is served by /api/host/terminal (the
		// SPA switches TerminalPanel mode for the built-in card). Same
		// split as rejectBuiltin: non-admins cannot tell the card from
		// an unknown id, admins get a readable refusal.
		if requestIsAdmin(r) {
			http.Error(w, `{"error":"built-in machine has no terminal surface"}`, http.StatusBadRequest)
		} else {
			http.Error(w, `{"error":"machine not found"}`, http.StatusNotFound)
		}
		return
	}
	machine, secret, err := h.machineWithSecret(id)
	if err != nil {
		http.Error(w, `{"error":"machine not found"}`, http.StatusNotFound)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("machine_terminal_ws_upgrade_failed", "machine", machine.Name, "err", err)
		return
	}
	defer ws.Close()
	ws.SetReadLimit(4 << 20)

	user, _, ip := audit.FromRequest(r)
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.terminal.connect", "machine:"+id, map[string]interface{}{
			"machine":  machine.Name,
			"protocol": machine.Protocol,
			"user":     user,
			"ip":       ip,
		}))
	}

	// Newest viewer wins (4409), same as serialSessions.
	if old, loaded := machineTerminalSessions.Swap(id, ws); loaded {
		oldWS := old.(*websocket.Conn)
		_ = oldWS.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(4409, "replaced by a newer session"),
			time.Now().Add(time.Second))
		_ = oldWS.Close()
	}
	defer machineTerminalSessions.CompareAndDelete(id, ws)

	// A machine can be deleted while this request was still upgrading.
	// DeleteMachine sweeps this registry AFTER the record is gone, so the
	// re-check has to run AFTER the Swap above: whichever order the two
	// take, one of them sees the other, and no terminal is left serving a
	// machine that no longer exists.
	if _, ok := h.machines.Get(id); !ok {
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "machine deleted"),
			time.Now().Add(time.Second))
		return
	}

	// Initial grid: the browser sends its real size via ?cols=&rows= so
	// full-screen TUIs draw correctly from the first frame.
	cols, rows := 120, 30
	if v, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && v >= 20 && v <= 500 {
		cols = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && v >= 5 && v <= 200 {
		rows = v
	}

	switch machine.Protocol {
	case sshmachines.ProtocolSSH:
		h.machineTerminalSSH(ws, machine, secret, r, cols, rows)
	case sshmachines.ProtocolTelnet:
		h.machineTerminalTelnet(ws, machine, secret, r, cols, rows)
	case sshmachines.ProtocolSerial:
		h.machineTerminalSerial(ws, machine, r, cols, rows)
	default:
		_ = ws.WriteMessage(websocket.TextMessage,
			[]byte("\r\n\x1b[31m[Mosh is an experimental transport that is not part of this build. Configure the machine for SSH instead.]\x1b[0m\r\n"))
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "mosh not available"),
			time.Now().Add(time.Second))
	}
}

// machineTerminalSSH dials (strict host-key policy) and opens a pty
// shell session as the remote end.
func (h *Handler) machineTerminalSSH(ws *websocket.Conn, machine *sshmachines.Machine, secret sshmachines.MachineSecret, r *http.Request, cols, rows int) {
	client, err := sshmachines.Dial(machine, secret, 15*time.Second)
	if err != nil {
		h.writeMachineDialError(ws, machine, err)
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("\r\n[ssh: open session: "+err.Error()+"]\r\n"))
		return
	}
	fail := func(msg string) {
		_ = session.Close()
		_ = ws.WriteMessage(websocket.TextMessage, []byte("\r\n[ssh: "+msg+"]\r\n"))
	}
	// RequestPty(term, h, w, modes): height first, width second.
	if err := session.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		fail("pty request: " + err.Error())
		return
	}
	// The pipes must be created BEFORE Shell(): once the session starts
	// its own copy goroutines would fight this pump for the channel.
	stdin, err := session.StdinPipe()
	if err != nil {
		fail("stdin pipe: " + err.Error())
		return
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		fail("stdout pipe: " + err.Error())
		return
	}
	if err := session.Shell(); err != nil {
		fail("start shell: " + err.Error())
		return
	}
	rt := &sshRemoteTerminal{session: session, stdin: stdin, stdout: stdout}
	h.pumpMachineTerminal(ws, machine, r, rt)
}

// machineTerminalTelnet opens the TCP socket and runs the ported telnet
// state machine (auto-login included, from the stored credentials).
func (h *Handler) machineTerminalTelnet(ws *websocket.Conn, machine *sshmachines.Machine, secret sshmachines.MachineSecret, r *http.Request, cols, rows int) {
	sess, err := telnet.Dial(r.Context(), machine.Host, machine.Port, machine.User, secret.Password, "xterm-256color", cols, rows)
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("\r\n\x1b[31m[telnet: "+err.Error()+"]\x1b[0m\r\n"))
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "telnet dial failed"),
			time.Now().Add(time.Second))
		return
	}
	h.pumpMachineTerminal(ws, machine, r, sess)
}

// machineTerminalSerial opens the serial port at the stored baud rate.
func (h *Handler) machineTerminalSerial(ws *websocket.Conn, machine *sshmachines.Machine, r *http.Request, cols, rows int) {
	port, err := serialx.Open(machine.Host, machine.Port)
	if err != nil {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("\r\n\x1b[31m[serial: "+err.Error()+"]\x1b[0m\r\n"))
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "serial open failed"),
			time.Now().Add(time.Second))
		return
	}
	h.pumpMachineTerminal(ws, machine, r, port)
}

// writeMachineDialError turns the strict host-key failures (and generic
// dial errors) into a readable terminal message. The UNPINNED case also
// names the pin path so the operator has a next step; the CHANGED case
// is a security refusal and says so.
func (h *Handler) writeMachineDialError(ws *websocket.Conn, machine *sshmachines.Machine, err error) {
	var hke *sshmachines.HostKeyError
	if errors.As(err, &hke) {
		var msg string
		switch {
		case errors.Is(hke.Err, sshmachines.ErrHostKeyNotPinned):
			msg = "the host key of " + machine.Name + " is not pinned yet: " + hke.PresentedFull +
				"\r\nAccept it in the machine's Settings (\"pin host key\") to open the terminal."
		case errors.Is(hke.Err, sshmachines.ErrHostKeyChanged):
			msg = "HOST KEY OF " + machine.Name + " HAS CHANGED (possible MITM): presented " +
				hke.PresentedFull + ", pinned " + hke.Expected +
				"\r\nConnection refused. Re-pin in Settings only if you re-provisioned this host yourself."
		default:
			msg = err.Error()
		}
		_ = ws.WriteMessage(websocket.TextMessage, []byte("\r\n\x1b[31m["+msg+"]\x1b[0m\r\n"))
	} else {
		_ = ws.WriteMessage(websocket.TextMessage, []byte("\r\n\x1b[31m[connection to "+machine.Name+": "+err.Error()+"]\x1b[0m\r\n"))
	}
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "dial failed"),
		time.Now().Add(time.Second))
}

// pumpMachineTerminal is the shared WS<->remote pump (HostTerminal
// clone): binary frames, JSON resize sniffing, 20s pings, the
// terminal.idle_timeout_min watcher, and one error channel ending the
// session on either side.
func (h *Handler) pumpMachineTerminal(ws *websocket.Conn, machine *sshmachines.Machine, r *http.Request, remote machineRemoteTerminal) {
	defer remote.Close()

	const (
		pingPeriod = 20 * time.Second
		writeWait  = 10 * time.Second
	)

	idleTimeoutMin := 0
	if h.settings != nil {
		idleTimeoutMin = h.settings.GetInt("terminal.idle_timeout_min")
	}

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().Unix())

	var wsMu sync.Mutex
	writeWS := func(msgType int, data []byte) error {
		wsMu.Lock()
		defer wsMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
		return ws.WriteMessage(msgType, data)
	}

	// remoteMu serialises everything that writes to the far end: the
	// user's keystrokes and the receiver's own protocol frames travel on
	// one channel and must not interleave mid-frame.
	remoteMu := &sync.Mutex{}
	// ZMODEM exists for the transports with no file subsystem, but the
	// gate is built for every protocol so a request against an SSH
	// machine answers with a reason instead of vanishing. Its tap costs
	// one atomic load per chunk while nothing is armed, which is the
	// common case, and the terminal behaves exactly as it did before.
	gate := newZmodemGate(h, machine, writeWS, &lockedWriter{mu: remoteMu, w: remote})
	defer gate.abort()

	done := make(chan struct{})
	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			close(done)
			_ = remote.Close()
			_ = ws.Close()
		})
	}
	defer cleanup()

	// Idle timeout watcher (same semantics as HostTerminal: 0 = never).
	if idleTimeoutMin > 0 {
		go func() {
			defer safego.Recover("machine_terminal_idle_checker")
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			maxIdle := time.Duration(idleTimeoutMin) * time.Minute
			for {
				select {
				case <-ticker.C:
					if time.Since(time.Unix(lastActivity.Load(), 0)) > maxIdle {
						_ = writeWS(websocket.TextMessage, []byte("\r\n[Session closed due to inactivity timeout]\r\n"))
						cleanup()
						return
					}
				case <-done:
					return
				}
			}
		}()
	}

	// Ping ticker keeps the WebSocket alive through proxies.
	go func() {
		defer safego.Recover("machine_terminal_ping")
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				wsMu.Lock()
				_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
				err := ws.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(writeWait))
				wsMu.Unlock()
				if err != nil {
					cleanup()
					return
				}
			case <-done:
				return
			}
		}
	}()

	errc := make(chan error, 3)

	// remote → websocket (binary frames). The gate is transparent unless
	// a ZMODEM session owns the stream, so an SSH machine (nil gate) and
	// an idle telnet line take the same path they always did.
	go func() {
		defer safego.Recover("machine_terminal_remote_to_ws")
		buf := make([]byte, 65536)
		for {
			n, err := remote.Read(buf)
			if err != nil {
				errc <- err
				return
			}
			if n == 0 {
				continue
			}
			lastActivity.Store(time.Now().Unix())
			out := gate.feed(buf[:n])
			if len(out) == 0 {
				continue
			}
			if err := writeWS(websocket.BinaryMessage, out); err != nil {
				errc <- err
				return
			}
		}
	}()

	// websocket → remote (binary in; JSON control frames with an explicit
	// "type" of "resize" or "zmodem" are parsed out of the stream).
	go func() {
		defer safego.Recover("machine_terminal_ws_to_remote")
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				errc <- err
				return
			}
			lastActivity.Store(time.Now().Unix())
			if frame, ok := machineControlFrame(msg); ok {
				switch frame.Type {
				case "resize":
					remoteMu.Lock()
					_ = remote.Resize(frame.Cols, frame.Rows)
					remoteMu.Unlock()
					continue
				case "zmodem":
					gate.handle(&frame)
					continue
				}
			}
			if out := gate.keys(msg); len(out) > 0 {
				remoteMu.Lock()
				_, err := remote.Write(out)
				remoteMu.Unlock()
				if err != nil {
					errc <- err
					return
				}
			}
		}
	}()

	slog.Info("machine_terminal_connected", "machine", machine.Name, "protocol", machine.Protocol)
	<-errc
	cleanup()
	slog.Info("machine_terminal_disconnected", "machine", machine.Name)
	if h.audit != nil {
		h.audit.Log(auditFor(r, "ssh_machine.terminal.disconnect", "machine:"+machine.ID, map[string]interface{}{
			"machine": machine.Name,
		}))
	}
}

// machineControlFrame decides whether one inbound message is a terminal
// control frame rather than input. The "type" is what makes a frame a
// frame: anything else — a keystroke that happens to begin with '{', a
// pasted JSON document, `{ ls; }` at a shell prompt — is raw input, and
// sniffing on the first byte or on the mere presence of cols/rows used to
// swallow it into the void.
func machineControlFrame(msg []byte) (machineTerminalFrame, bool) {
	if len(msg) == 0 || msg[0] != '{' {
		return machineTerminalFrame{}, false
	}
	var frame machineTerminalFrame
	if json.Unmarshal(msg, &frame) != nil {
		return machineTerminalFrame{}, false
	}
	switch frame.Type {
	case "resize":
		// An ill-formed resize is not a resize; it goes to the shell like
		// any other bytes, which is what the old code did with it too.
		if frame.Cols > 0 && frame.Rows > 0 {
			return frame, true
		}
	case "zmodem":
		return frame, true
	}
	return machineTerminalFrame{}, false
}

// ---- protocol adapters ----

// sshRemoteTerminal adapts an ssh.Session (pty shell) to the bridge
// interface. I/O goes over the session's stdin/stdout pipes (created
// before Shell(), so this pump is the channel's only consumer).
type sshRemoteTerminal struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
}

func (t *sshRemoteTerminal) Read(p []byte) (int, error)  { return t.stdout.Read(p) }
func (t *sshRemoteTerminal) Write(p []byte) (int, error) { return t.stdin.Write(p) }
func (t *sshRemoteTerminal) Resize(cols, rows int) error {
	// WindowChange(h, w): height first, width second.
	return t.session.WindowChange(rows, cols)
}
func (t *sshRemoteTerminal) Close() error {
	// CloseWrite tells the remote side our stdin has ended, then the
	// channel closes; the session owns nothing else.
	_ = t.stdin.Close()
	return t.session.Close()
}

// netDialTCP is the tiny TCP dial used by the telnet test path.
func netDialTCP(host string, port int, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
}

// serialxProbe opens and immediately closes the port to prove it is
// available (the test endpoint); it does not stream anything.
func serialxProbe(device string, baudRate int) (*serialx.Session, error) {
	return serialx.Open(device, baudRate)
}
