package sshmachines

// Zmodem-driven terminal transfers.
//
// A ZMODEM session has no worker goroutine of its own: the bytes arrive
// through the terminal's pump, which feeds them to a zmodem.Receiver, or
// leave through it when a zmodem.Sender is waiting for an answer. Either
// way the manager still owns everything else — the queue row, the cancel
// flag, the on-host directory, the progress broadcast — so a transfer
// over telnet or a serial line shows up in the same queue and behaves
// like an SFTP one.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"webkvm/internal/sshmachines/zmodem"
)

// ZmodemSink is the transfer-manager half of one terminal ZMODEM
// session. It is registered before the command is typed, so the queue
// shows the job while the remote is still answering, and finished
// exactly once.
type ZmodemSink struct {
	m *TransferManager
	t *Transfer

	// Dir is the job-private tree the transfer works in: where the
	// receiver lands files, or the stage the sender reads from. Nothing
	// outside it is ever touched.
	Dir string

	// Files and send belong to the upload direction only: the staged
	// batch to hand the sender, and the fact that these bytes leave the
	// host instead of arriving on it.
	Files []zmodem.SendFile
	send  bool

	// lastBeat throttles the broadcast; Progress runs per subpacket.
	lastBeat time.Time
}

// BeginZmodem registers a download job fed from the machine's terminal.
// origin is whatever the user asked for (a path, a glob) and only ever
// used for display.
func (m *TransferManager) BeginZmodem(machineID, origin string) (*ZmodemSink, error) {
	machine, ok := m.store.Get(machineID)
	if !ok {
		return nil, errors.New("machine not found")
	}
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	// The batch lands beside the job's result path rather than on it, so
	// a lone file can be renamed onto LocalFile at Finish without
	// fighting the directory that collected it.
	dir := filepath.Join(m.outDir(), id+".in")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	t := &Transfer{
		ID:         id,
		MachineID:  machineID,
		Machine:    machine.Name,
		Kind:       "download",
		RemotePath: CleanRemotePath(origin),
		FileName:   zmodemDisplayName(origin),
		LocalFile:  dir,
		StartedAt:  time.Now().UTC(),
		status:     TransferRunning,
		// A ZMODEM batch is a tree until exactly one flat file proves
		// otherwise; Finish flips this back for the single case.
		kindDir: true,
	}
	// The context is only a cancellation handle: unlike an SFTP job there
	// is no worker to watch it, the receiver polls the job's status.
	_, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.register(t)

	m.broadcast(t)
	return &ZmodemSink{m: m, t: t, Dir: dir}, nil
}

// ID is the job id, which the terminal echoes back to the browser so the
// queue row and the banner describe the same transfer.
func (s *ZmodemSink) ID() string { return s.t.ID }

// Uploads reports which end of the protocol this sink feeds. An upload has
// no result to fetch afterwards — the bytes belong to the remote — so the
// terminal must not hand the browser a list of names to download.
func (s *ZmodemSink) Uploads() bool { return s.send }

// Cancelled is polled by the receiver between blocks; wiring it to the
// job's cancel flag is what makes the queue's cancel button stop a
// transfer that is mid-batch.
func (s *ZmodemSink) Cancelled() bool { return s.t.cancelRequested() }

// WhenCanceled installs the hook the queue's cancel button calls. A
// session still in its arm window never reaches the polling path, so
// without this a cancel would only take effect at the arm deadline.
func (s *ZmodemSink) WhenCanceled(fn func()) {
	s.t.mu.Lock()
	s.t.onCancel = fn
	s.t.mu.Unlock()
}

// Progress mirrors one zmodem.Progress into the job. done and total are
// cumulative over the batch, so total grows as each ZFILE announces its
// size — the only honest figure before the sender is done talking.
func (s *ZmodemSink) Progress(done, total int64, current string, filesDone, filesGot int) {
	t := s.t
	t.setDone(done)
	t.setTotal(total)
	t.setCurrent(current)
	t.mu.Lock()
	t.FilesDone = filesDone
	if filesGot > 1 {
		t.FilesTotal = filesGot
	}
	t.mu.Unlock()
	if time.Since(s.lastBeat) > 300*time.Millisecond {
		s.lastBeat = time.Now()
		s.m.broadcast(t)
	}
}

// Finish closes the job. status is one of the Transfer* constants; files
// are the paths the receiver actually landed, in arrival order. Anything
// but TransferDone throws the partial tree away, which is the same rule
// runDownload follows for a broken SFTP pull.
//
// For an upload, files are the *remote* names, so there is nothing to
// count, collapse or serve back afterwards: the progress the session
// reported while it ran already is the job's, and the staging tree is
// the only thing left to clean up — batch landed or not.
func (s *ZmodemSink) Finish(status, errMsg string, files []string) {
	t := s.t
	// This is the job's worker: the terminal's pump is the only thing
	// feeding it, so "returned from Finish" is what "nothing else will
	// touch this directory" means for Remove and the eviction cap.
	defer t.settle()
	if s.send {
		_ = os.RemoveAll(s.Dir)
		t.setCurrent("")
		t.finish(status, errMsg)
		s.m.broadcast(t)
		return
	}
	var bytes int64
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			bytes += fi.Size()
		}
	}

	if status == TransferDone && len(files) == 1 && filepath.Dir(files[0]) == s.Dir {
		// One flat file: move it onto the job's own path so /result
		// streams it as itself instead of wrapping a tar around it.
		final := filepath.Join(s.m.outDir(), t.ID)
		if err := os.Rename(files[0], final); err == nil {
			_ = os.RemoveAll(s.Dir)
			t.mu.Lock()
			t.LocalFile = final
			t.kindDir = false
			t.FileName = filepath.Base(files[0])
			t.mu.Unlock()
		} else {
			slog.Warn("ssh_machine_zmodem_collapse_failed", "job", t.ID, "err", err)
		}
	}

	if status == TransferDone {
		if len(files) > 1 {
			t.mu.Lock()
			t.FilesTotal = len(files)
			t.mu.Unlock()
		}
	} else {
		_ = os.RemoveAll(s.Dir)
	}

	t.setTotal(bytes)
	t.setDone(bytes)
	t.setCurrent("")
	t.finish(status, errMsg)
	s.m.broadcast(t)
}

// zmodemDisplayName turns whatever the user typed into something the
// queue row can show. A glob degrades to the job's generic label rather
// than a path that does not exist.
func zmodemDisplayName(origin string) string {
	base := path.Base(filepath.ToSlash(origin))
	if base == "." || base == "/" || base == ".." || stringContainsGlob(base) {
		return "zmodem-receive"
	}
	return base
}

func stringContainsGlob(s string) bool {
	for _, r := range s {
		if r == '*' || r == '?' || r == '[' {
			return true
		}
	}
	return false
}
