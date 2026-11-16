package api

// ZMODEM over a machine terminal, in both directions.
//
// Telnet and serial machines have no SFTP subsystem, so the Files tab
// has nothing to offer them. ZMODEM does work there, because all it needs
// is a shell and a byte stream: the backend types `sz -q <paths>` or `rz`
// into the live terminal, watches the reply for protocol frames, and
// lands the bytes as an ordinary download job — or feeds it a batch the
// browser already posted to this host.
//
// Three properties matter more than the protocol details:
//
//   - The pump is transparent until asked. Nothing is armed, every byte
//     passes through, and the tap costs one atomic load. Detection by
//     sniffing for a ZMODEM magic in unsolicited output is what makes
//     other implementations eat a user's `cat` of a binary file; here the
//     browser must send an explicit frame first.
//   - A session that never started costs the user nothing. If no frame
//     answers (lrzsz missing, most likely) the keystrokes buffered during
//     the arm window are replayed into the shell and the terminal is
//     handed back.
//   - Both directions share one gate and one job row, so the queue shows
//     an upload exactly like a download: same cancel, same progress, and
//     no result to fetch for the one that left the building.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"webkvm/internal/safego"
	"webkvm/internal/sshmachines"
	"webkvm/internal/sshmachines/zmodem"
)

// errZmodemBusy answers a second arm request while one owns the line.
var errZmodemBusy = errors.New("another ZMODEM transfer is already running on this terminal")

// zmodemMaxPaths bounds one request. sz takes the whole list on a single
// command line, and the line discipline truncates well before that
// anyway; refusing is kinder than a silently missing file.
const zmodemMaxPaths = 64

// machineTerminalFrame is every JSON frame the browser may send over the
// terminal socket. Unknown types fall through and are typed into the
// shell, which is what a raw terminal has always done with them.
type machineTerminalFrame struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	// zmodem: "cancel" ends the live session, anything else (or an
	// omitted action) starts one.
	Action string   `json:"action"`
	Paths  []string `json:"paths"`
	// Recursive is what turns `sz` into `sz -r`: one job for a tree.
	Recursive bool `json:"recursive"`
	// Dir names the direction: "up" sends a staged batch to the remote,
	// anything else pulls paths from it. The two ask for different frames
	// of the same protocol, so the gate needs to know which end to be.
	Dir string `json:"dir"`
	// UploadID is the stage the browser filled with POST
	// action=zmodem-stage. It is a uuid, not a path: nothing here tells
	// the browser where on the host its bytes landed.
	UploadID string `json:"upload_id"`
}

// lockedWriter serialises writes to the far end. The receiver writes its
// protocol frames from the output pump's goroutine while the input pump
// writes the user's keystrokes, and a ZMODEM frame split around a
// keystroke is a corrupt frame.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	if l.mu != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
	}
	return l.w.Write(p)
}

// zmodemGate is one terminal's ZMODEM state. A nil gate is valid and
// means "this transport has no ZMODEM", which is how SSH connections keep
// the old code path.
type zmodemGate struct {
	h        *Handler
	machine  *sshmachines.Machine
	writeWS  func(msgType int, data []byte) error
	toRemote io.Writer

	// live is the fast path: feed and keys are on every byte of the
	// pump, and 99.9% of the time no session exists.
	live atomic.Bool

	mu sync.Mutex
	// sess is whichever end of the protocol is running: a Receiver for a
	// download, a Sender for an upload. The gate never asks which — the
	// framing, the keys and the teardown are the same questions.
	sess zmodem.Session
	sink *sshmachines.ZmodemSink
}

func newZmodemGate(h *Handler, machine *sshmachines.Machine, writeWS func(int, []byte) error, toRemote io.Writer) *zmodemGate {
	return &zmodemGate{h: h, machine: machine, writeWS: writeWS, toRemote: toRemote}
}

// session returns the live session, or nil. Every tap calls this on the
// pump's goroutine, so the lock is held for one field read.
func (g *zmodemGate) session() zmodem.Session {
	g.mu.Lock()
	s := g.sess
	g.mu.Unlock()
	return s
}

// feed is the remote-to-browser tap.
func (g *zmodemGate) feed(p []byte) []byte {
	if g == nil || !g.live.Load() {
		return p
	}
	if s := g.session(); s != nil {
		return s.Feed(p)
	}
	return p
}

// keys is the browser-to-remote tap: held during the arm window, dropped
// while the protocol owns the line.
func (g *zmodemGate) keys(p []byte) []byte {
	if g == nil || !g.live.Load() {
		return p
	}
	if s := g.session(); s != nil {
		return s.Keys(p)
	}
	return p
}

// active reports whether a session owns the terminal right now.
func (g *zmodemGate) active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sess != nil
}

// begin installs a session and its job row under the gate's lock, so two
// frames on one socket cannot both win the line.
func (g *zmodemGate) begin(sink *sshmachines.ZmodemSink) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sess != nil {
		return false
	}
	g.sink = sink
	g.live.Store(true)
	return true
}

// startDown arms a download. The command is typed by the receiver itself,
// so it lands in the same stream as the user's own keystrokes and the
// remote sees nothing unusual.
func (g *zmodemGate) startDown(paths []string, recursive bool) error {
	if err := g.protocolGuard(); err != nil {
		return err
	}
	paths, err := zmodemPaths(paths)
	if err != nil {
		return err
	}
	// Checked before the job row exists: a second click must not leave a
	// failed transfer in a queue that is busy doing something else. The
	// gate is driven by one input pump, so nothing races this; begin()
	// stays the atomic guard for the teardown of a previous session.
	if g.active() {
		return errZmodemBusy
	}
	sink, err := g.h.transfers.BeginZmodem(g.machine.ID, strings.Join(paths, " "))
	if err != nil {
		return err
	}
	if !g.begin(sink) {
		sink.Finish(sshmachines.TransferError, errZmodemBusy.Error(), nil)
		return errZmodemBusy
	}
	recv := zmodem.New(g.toRemote, zmodem.Options{
		Dir:       sink.Dir,
		Cancelled: sink.Cancelled,
		OnProgress: func(p zmodem.Progress) {
			sink.Progress(p.Done, p.Total, p.Current, p.FilesDone, p.FilesGot)
		},
		OnFile: func(f zmodem.FileEvent) {
			slog.Info("ssh_machine_zmodem_file", "machine", g.machine.Name, "file", f.Name, "size", f.Size)
		},
		Log: g.logf,
	})
	g.setSession(recv)
	// The queue's cancel button must reach the session even while it is
	// still waiting for the first frame.
	sink.WhenCanceled(recv.Cancel)

	if err := recv.Start(paths, recursive); err != nil {
		g.clear(recv)
		sink.Finish(sshmachines.TransferError, err.Error(), nil)
		return err
	}
	g.send("armed", sink.ID(), map[string]any{"dir": "down", "recursive": recursive, "paths": paths})
	go g.watch(recv, sink)
	return nil
}

// startUp arms an upload of a batch the browser already posted here. The
// backend becomes the ZMODEM sender and the remote's `rz` does the
// writing, so the only storage question is which stage to read — and
// BeginZmodemSend answers it by refusing anything that belongs to another
// machine or that never arrived.
func (g *zmodemGate) startUp(uploadID string) error {
	if err := g.protocolGuard(); err != nil {
		return err
	}
	if uploadID == "" {
		return errors.New("an upload needs the staged batch to send")
	}
	// Refusing before the claim keeps the staged batch intact: the browser
	// can arm again when the line frees up, instead of re-posting files.
	if g.active() {
		return errZmodemBusy
	}
	sink, err := g.h.transfers.BeginZmodemSend(g.machine.ID, uploadID)
	if err != nil {
		return err
	}
	if !g.begin(sink) {
		sink.Finish(sshmachines.TransferError, errZmodemBusy.Error(), nil)
		return errZmodemBusy
	}
	send, err := zmodem.NewSender(g.toRemote, zmodem.Options{
		Cancelled: sink.Cancelled,
		OnProgress: func(p zmodem.Progress) {
			sink.Progress(p.Done, p.Total, p.Current, p.FilesDone, p.FilesGot)
		},
		OnFile: func(f zmodem.FileEvent) {
			slog.Info("ssh_machine_zmodem_sent", "machine", g.machine.Name, "file", f.Name, "size", f.Size)
		},
		Log: g.logf,
	}, sink.Files)
	if err != nil {
		g.clear(nil)
		sink.Finish(sshmachines.TransferError, err.Error(), nil)
		return err
	}
	g.setSession(send)
	sink.WhenCanceled(send.Cancel)

	if err := send.Start(); err != nil {
		g.clear(send)
		sink.Finish(sshmachines.TransferError, err.Error(), nil)
		return err
	}
	g.send("armed", sink.ID(), map[string]any{"dir": "up"})
	go g.watch(send, sink)
	return nil
}

// protocolGuard is the one rule both directions share: a machine you can
// reach over SFTP should never be asked to speak ZMODEM. The framing
// costs a round trip per block on a line that has a real file protocol,
// and the terminal is the user's shell, not a data channel.
func (g *zmodemGate) protocolGuard() error {
	if g.machine.Protocol == sshmachines.ProtocolSSH {
		return errors.New("ssh machines transfer files over SFTP; ZMODEM is for telnet and serial links")
	}
	return nil
}

func (g *zmodemGate) logf(format string, args ...any) {
	slog.Debug("ssh_machine_zmodem", "machine", g.machine.Name, "detail", fmt.Sprintf(format, args...))
}

// setSession publishes the session the taps will call. begin() already
// reserved the slot; this only fills it, which is why it cannot fail.
func (g *zmodemGate) setSession(s zmodem.Session) {
	g.mu.Lock()
	g.sess = s
	g.mu.Unlock()
}

// cancel stops the live session. Going through the session rather than
// the job matters while it is still in its arm window: no data
// subpackets have arrived to poll the cancel flag, so only the session can
// hand the terminal back right away.
func (g *zmodemGate) cancel() error {
	g.mu.Lock()
	sess, sink := g.sess, g.sink
	g.mu.Unlock()
	if sess != nil {
		sess.Cancel()
		return nil
	}
	if sink != nil {
		return g.h.transfers.Cancel(sink.ID())
	}
	return errors.New("no ZMODEM transfer is running")
}

// handle dispatches one inbound zmodem control frame. Every failure is
// answered on the socket: the browser asked for a transfer by name, so
// silence would look like a hung terminal.
func (g *zmodemGate) handle(f *machineTerminalFrame) {
	var err error
	switch {
	case f.Action == "cancel":
		err = g.cancel()
	case f.Dir == "up":
		err = g.startUp(f.UploadID)
	default:
		err = g.startDown(f.Paths, f.Recursive)
	}
	if err != nil {
		g.send("error", "", map[string]any{"detail": err.Error(), "action": f.Action, "dir": f.Dir})
	}
}

// watch waits for the session to end and reports it once: the job row,
// the browser event, and the keystrokes a failed arm swallowed.
func (g *zmodemGate) watch(sess zmodem.Session, sink *sshmachines.ZmodemSink) {
	defer safego.Recover("machine_zmodem_watch")
	<-sess.Done()
	res := sess.Result()

	g.mu.Lock()
	cur := g.sess
	g.mu.Unlock()
	if cur == sess {
		g.clear(sess)
	}

	status, msg := zmodemJobStatus(res)
	sink.Finish(status, msg, res.Files)

	// A session that ended inside its arm window never took the line, so
	// the keystrokes it swallowed belong to the shell, not to a protocol.
	// Replay() is what knows: the arm timeout, a cancel from the panel and
	// a failed write all end the same way, and only a dialogue that really
	// started keeps them dropped.
	if k := sess.Replay(); len(k) > 0 {
		_, _ = g.toRemote.Write(k)
	}

	payload := map[string]any{"status": string(res.Status), "reason": res.Reason}
	if msg != "" {
		payload["error"] = msg
	}
	// Only a download has files to fetch: for an upload the names are the
	// remote's, and there is no /result behind them. Leaving the key out
	// is what stops the browser from asking for a file that never landed
	// on this host.
	if !sink.Uploads() {
		if files := zmodemNames(sink.Dir, res.Files); len(files) > 0 {
			payload["files"] = files
		}
	}
	g.send(string(res.Status), sink.ID(), payload)
}

// clear releases the terminal. A session that already ended can clear
// itself; a newer one must not be overwritten by an old watcher. Passing
// the sink instead of a session is the failure path's way of saying "no
// session ever got as far as existing".
func (g *zmodemGate) clear(sess zmodem.Session) {
	g.mu.Lock()
	if sess == nil || g.sess == sess {
		g.sess = nil
		g.sink = nil
		g.live.Store(false)
	}
	g.mu.Unlock()
}

// abort ends a live session when the WebSocket goes away, so the job does
// not linger as running forever and its partial tree is removed.
func (g *zmodemGate) abort() {
	if g == nil {
		return
	}
	g.mu.Lock()
	sess, sink := g.sess, g.sink
	g.mu.Unlock()
	if sess != nil {
		sess.Cancel()
	} else if sink != nil {
		sink.Finish(sshmachines.TransferCanceled, "terminal closed", nil)
	}
}

func (g *zmodemGate) send(event, jobID string, extra map[string]any) {
	body := map[string]any{"type": "zmodem", "event": event, "job_id": jobID}
	for k, v := range extra {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	if err := g.writeWS(websocket.TextMessage, b); err != nil {
		slog.Debug("ssh_machine_zmodem_event_dropped", "event", event, "err", err)
	}
}

// zmodemJobStatus maps a protocol outcome onto a transfer status.
func zmodemJobStatus(res zmodem.Result) (string, string) {
	switch res.Status {
	case zmodem.StatusDone:
		return sshmachines.TransferDone, ""
	case zmodem.StatusCanceled:
		return sshmachines.TransferCanceled, "canceled by user"
	case zmodem.StatusDisarmed:
		// Distinct from a plain failure: the frontend offers the lrzsz
		// hint for this reason only.
		return sshmachines.TransferError, "no ZMODEM answer from the remote (is lrzsz installed?)"
	}
	msg := "zmodem transfer failed"
	if res.Err != nil {
		msg = res.Err.Error()
	} else if res.Reason != "" {
		msg = "zmodem transfer failed: " + res.Reason
	}
	return sshmachines.TransferError, msg
}

// zmodemNames turns landed host paths into the names the user asked for,
// relative to the job's private directory. The browser never learns where
// on the host the bytes were written.
func zmodemNames(dir string, paths []string) []string {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimPrefix(p, prefix))
	}
	return out
}

// zmodemPaths validates the requested list. The remote's own permissions
// decide what `sz` can read; this only keeps a request from becoming a
// huge command line or a control-character injection into the shell.
func zmodemPaths(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > 1024 {
			return nil, errors.New("path is too long")
		}
		if strings.ContainsAny(p, "\n\r\x00") {
			return nil, fmt.Errorf("path %q cannot be sent to a shell", p)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("no paths to download")
	}
	if len(out) > zmodemMaxPaths {
		return nil, fmt.Errorf("too many paths (%d max)", zmodemMaxPaths)
	}
	return out, nil
}

// ---- upload staging ----

// stageZmodemUpload is the HTTP half of a ZMODEM upload: the browser hands
// the batch to this host, and a second frame on the live terminal says
// "now send what you kept". The reply names the stage and the files inside
// it, never where on the host the bytes landed.
func (h *Handler) stageZmodemUpload(w http.ResponseWriter, r *http.Request, machine *sshmachines.Machine) {
	switch machine.Protocol {
	case sshmachines.ProtocolTelnet, sshmachines.ProtocolSerial:
	default:
		jsonErr(w, http.StatusBadRequest, "staging is for telnet and serial machines; ssh machines upload over SFTP")
		return
	}
	parts := r.MultipartForm.File["file"]
	if len(parts) == 0 {
		jsonErr(w, http.StatusBadRequest, "missing file part")
		return
	}
	if len(parts) > sshmachines.MaxStagedBatch {
		jsonErr(w, http.StatusBadRequest, fmt.Sprintf("a ZMODEM batch holds at most %d files", sshmachines.MaxStagedBatch))
		return
	}
	stage, err := h.transfers.StageZmodemUpload(machine.ID)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	files := make([]map[string]any, 0, len(parts))
	var total int64
	for _, fh := range parts {
		part, err := fh.Open()
		if err != nil {
			h.transfers.DropStagedUpload(stage.ID)
			jsonErr(w, http.StatusBadGateway, "read "+fh.Filename+": "+err.Error())
			return
		}
		// Add owns the name: what the browser called the file is reduced to
		// one the remote can create, and a collision is renamed here so the
		// reply can show the user the name rz will actually receive.
		meta, err := stage.Add(fh.Filename, part)
		_ = part.Close()
		if err != nil {
			h.transfers.DropStagedUpload(stage.ID)
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		total += meta.Size
		files = append(files, map[string]any{"name": meta.Name, "size": meta.Size})
	}
	h.auditMachine(r, machine.ID, "ssh_machine.transfer.stage", map[string]any{"files": len(files), "bytes": total})
	jsonResp(w, http.StatusAccepted, map[string]any{"upload_id": stage.ID, "files": files, "total": total})
}
