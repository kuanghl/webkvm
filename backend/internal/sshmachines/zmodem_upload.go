package sshmachines

// ZMODEM upload staging.
//
// A telnet or serial machine has no SFTP channel, so an upload cannot
// stream end to end: the browser posts the files to the webkvm host
// first, and a second frame says "now send what you kept". A stage is
// that holding area — one private directory, one claim, and a deadline
// of its own, because the tab that filled it can be closed before
// anything ever reads it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"webkvm/internal/sshmachines/zmodem"
)

// zmodemStageTTL bounds how long a staged batch waits for a terminal.
// SweepOrphans() only runs at startup, so an abandoned pick would
// otherwise sit on the host until the next restart.
const zmodemStageTTL = 15 * time.Minute

// MaxStagedBatch is what one rz batch may carry. Every file costs a
// handshake of its own and the line is half-duplex stop-and-wait, so a
// huge pick would take minutes just to get going; the user can send the
// rest in a second batch. The HTTP handler checks it before writing
// anything, which is why it is exported.
const MaxStagedBatch = 32

// stagedFileMode is the permission the remote is told the file has. A
// browser reports none, and 0644 is what the same file looks like when
// uploaded from a shell. The copy kept here stays 0600: this number only
// ever travels inside the ZFILE descriptor.
const stagedFileMode = os.FileMode(0o644)

// maxStagedName is 255 — what both this host and any reasonable remote
// call the end of a filename — with room kept for the " (2)" counter
// uniqueName appends. Without that reservation two long names could be
// clipped into the same bytes and the counter would never find a slot.
const maxStagedName = 255 - 8

// ZmodemStage is one browser-to-host batch waiting for its terminal.
type ZmodemStage struct {
	ID        string
	MachineID string
	// Dir holds nothing but this batch. It is removed on the claim's
	// Finish, on expiry, and by the startup sweep if we crashed first.
	Dir string

	mu     sync.Mutex
	files  []zmodem.SendFile
	taken  map[string]bool
	total  int64
	claims int
	timer  *time.Timer
}

// StageZmodemUpload opens an empty batch for a machine to pull from.
func (m *TransferManager) StageZmodemUpload(machineID string) (*ZmodemStage, error) {
	if _, ok := m.store.Get(machineID); !ok {
		return nil, errors.New("machine not found")
	}
	if m.dataDir == "" {
		return nil, errors.New("no data directory to stage uploads in")
	}
	id := uuid.NewString()
	dir := filepath.Join(m.tmpDir(), "zmodem-"+id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	st := &ZmodemStage{ID: id, MachineID: machineID, Dir: dir, taken: map[string]bool{}}
	st.timer = time.AfterFunc(m.stageTTL, func() {
		m.mu.Lock()
		cur, live := m.stages[id]
		if live && cur == st {
			delete(m.stages, id)
		}
		m.mu.Unlock()
		if !live || cur != st {
			return // claimed: the job owns the directory now
		}
		st.discard()
		slog.Info("ssh_machine_zmodem_stage_expired", "machine", machineID, "stage", id)
	})

	m.mu.Lock()
	m.stages[id] = st
	m.mu.Unlock()
	return st, nil
}

// Add writes one part of the batch and returns what the sender will
// announce for it. The name is the caller's; the path is ours.
func (s *ZmodemStage) Add(name string, r io.Reader) (zmodem.SendFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims > 0 {
		return zmodem.SendFile{}, errors.New("this staged upload has already been sent")
	}
	if len(s.files) >= MaxStagedBatch {
		return zmodem.SendFile{}, fmt.Errorf("a batch holds at most %d files", MaxStagedBatch)
	}
	final := s.uniqueName(cleanStagedName(name))
	p := filepath.Join(s.Dir, final)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return zmodem.SendFile{}, fmt.Errorf("stage %s: %w", final, err)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// A half-written part would be sent as a short file with a size
		// the receiver trusts.
		_ = os.Remove(p)
		return zmodem.SendFile{}, fmt.Errorf("stage %s: %w", final, err)
	}
	s.files = append(s.files, zmodem.SendFile{
		Name: final,
		Path: p,
		Size: n,
		Mode: stagedFileMode,
		// rz hands this to the remote file's mtime, so it is the one
		// thing about the upload the browser cannot tell us. Now is the
		// honest answer.
		ModTime: time.Now().UTC(),
	})
	s.total += n
	return s.files[len(s.files)-1], nil
}

// uniqueName keeps two files called notes.txt from overwriting each
// other: the second becomes "notes (2).txt", the third "notes (3).txt".
// Names that arrived already spelled that way count as taken, so the
// counter never lands on a collision.
func (s *ZmodemStage) uniqueName(name string) string {
	if !s.taken[name] {
		s.taken[name] = true
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	// taken only ever grows, so a free slot exists within len(taken)+1
	// tries; there is no path where this loop runs on.
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !s.taken[cand] {
			s.taken[cand] = true
			return cand
		}
	}
}

// claim closes the batch to new parts and stops its deadline. The job
// that claimed it owns the directory from here on, including its cleanup.
func (s *ZmodemStage) claim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.timer != nil {
		s.timer.Stop()
	}
}

// discard throws an unclaimed batch away.
func (s *ZmodemStage) discard() {
	s.mu.Lock()
	claimed := s.claims > 0
	s.mu.Unlock()
	if claimed {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		slog.Warn("ssh_machine_zmodem_stage_cleanup", "dir", s.Dir, "err", err)
	}
}

// BeginZmodemSend claims a staged batch and registers the queue row that
// will move it. The job is an ordinary upload as far as the queue is
// concerned — same row, same cancel button, same progress channel — with
// two differences that matter: it has no LocalFile, because nothing is
// left behind here to fetch afterwards, and no SFTP session, because the
// terminal is the only pipe.
func (m *TransferManager) BeginZmodemSend(machineID, stageID string) (*ZmodemSink, error) {
	machine, ok := m.store.Get(machineID)
	if !ok {
		return nil, errors.New("machine not found")
	}
	m.mu.Lock()
	st := m.stages[stageID]
	if st != nil {
		delete(m.stages, stageID)
	}
	m.mu.Unlock()
	if st == nil {
		return nil, errors.New("staged upload not found: it was sent already or it expired")
	}
	if st.MachineID != machineID {
		// The stage was opened for a different machine. Putting it back
		// would let the retry loop guess its way to another user's files.
		st.discard()
		return nil, errors.New("that staged upload belongs to another machine")
	}
	st.mu.Lock()
	files := append([]zmodem.SendFile(nil), st.files...)
	total := st.total
	st.mu.Unlock()
	if len(files) == 0 {
		st.discard()
		return nil, errors.New("nothing was staged")
	}
	// Claiming closes the directory to new parts and stops the deadline:
	// from here the job owns these bytes.
	st.claim()

	id := uuid.NewString()
	t := &Transfer{
		ID:         id,
		MachineID:  machineID,
		Machine:    machine.Name,
		Kind:       "upload",
		RemotePath: zmodemSendLabel(files),
		FileName:   files[0].Name,
		StartedAt:  time.Now().UTC(),
		status:     TransferRunning,
	}
	t.mu.Lock()
	t.Total = total
	if len(files) > 1 {
		// A lone file gets no counter of its own: the queue would read it
		// as "1 of 1" forever, which is what zmodem_job.go avoids on the
		// download side too.
		t.FilesTotal = len(files)
	}
	t.mu.Unlock()
	// As with a download, the context is only the cancel handle: the
	// sender polls the job rather than being driven by it.
	_, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.register(t)

	m.broadcast(t)
	return &ZmodemSink{m: m, t: t, Dir: st.Dir, Files: files, send: true}, nil
}

// zmodemSendLabel says where the batch went. rz writes into whatever
// directory the remote shell happens to be sitting in, which this end
// cannot see, so the truth is the names and the count.
func zmodemSendLabel(files []zmodem.SendFile) string {
	if len(files) == 0 {
		return "zmodem-send"
	}
	if len(files) == 1 {
		return files[0].Name
	}
	return fmt.Sprintf("%s +%d", files[0].Name, len(files)-1)
}

// DropStagedUpload throws one batch away, if it has not been claimed. The
// handler that opened it uses this when a part of its own fails: waiting
// out the TTL would keep bytes on the host for a batch nobody will send.
func (m *TransferManager) DropStagedUpload(stageID string) {
	m.mu.Lock()
	st := m.stages[stageID]
	if st != nil {
		delete(m.stages, stageID)
	}
	m.mu.Unlock()
	if st != nil {
		st.discard()
	}
}

// DropStagedUploads throws every live batch away. Shutdown calls it: the
// queue is already canceled and nobody is left to claim these bytes, so
// waiting out the TTL would only keep them on disk until the next start.
// Claimed batches are not in the set — their job owns the directory now.
func (m *TransferManager) DropStagedUploads() {
	m.mu.Lock()
	live := make([]*ZmodemStage, 0, len(m.stages))
	for id, s := range m.stages {
		live = append(live, s)
		delete(m.stages, id)
	}
	m.mu.Unlock()
	for _, s := range live {
		s.discard()
	}
}

// cleanStagedName reduces whatever the browser called the file — a
// webkitRelativePath for a folder drop, an absolute path from a paste —
// to one name the remote can create. rz never recurses, which is why a
// dropped folder arrives here as its files, flattened.
func cleanStagedName(name string) string {
	// A Windows path reaches us with backslashes even when this host does
	// not read them as separators, and the remote is the one that has to
	// open the name afterwards.
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return '_'
		case strings.ContainsRune(`/:*?"<>|`, r):
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "." || name == ".." {
		return "unnamed"
	}
	if len(name) > maxStagedName {
		name = name[:maxStagedName]
	}
	return name
}
