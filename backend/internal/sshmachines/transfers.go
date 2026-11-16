package sshmachines

// Transfer jobs (single-file upload/download with progress + cancel).
//
// Every remote write a job performs — upload, download-to-host, tree copy
// — is an atomic publish (see putfile.go): the destination keeps its old
// contents until the last byte has landed, so a broken or cancelled job
// leaves no scratch file, no truncated destination and nothing to clean
// up by hand.
//
//   - progress + status broadcast on the events hub
//     ("transfer.progress") so the SPA updates the transfer list
//     without polling every second;
//   - cancellation, with a watchdog that frees the queue when a worker is
//     wedged on a socket that will never answer.
//
// SCP mode (subsystem "scp") is P2 in the plan; the SFTP-subsystem
// failure path (SFTPSubsystemError) is the v1 answer.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"webkvm/internal/events"
	"webkvm/internal/safego"
)

const (
	TransferRunning  = "running"
	TransferPaused   = "paused"
	TransferDone     = "done"
	TransferError    = "error"
	TransferCanceled = "canceled"
)

// Transfer is one in-flight (or finished, not yet pruned) file job.
//
// A recursive transfer — a directory download, or a machine-to-machine
// copy — is ONE job carrying N files, not N jobs: the queue needs a
// single aggregated row per user action and cancellation has to be
// atomic for the whole tree. Every job, single file included, carries
// its byte total and file count: the queue's progress bar is
// total-based and a zero denominator renders no bar at all.
type Transfer struct {
	ID         string     `json:"id"`
	MachineID  string     `json:"machine_id"`
	Machine    string     `json:"machine"`
	Kind       string     `json:"kind"` // "upload" | "download" | "copy"
	RemotePath string     `json:"remote_path"`
	LocalFile  string     `json:"-"`         // webkvm-host temp file backing the job
	FileName   string     `json:"file_name"` // for downloads: suggested download name
	Total      int64      `json:"total"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Copy jobs stream machine → machine without touching the webkvm
	// host's disk; DstMachineID is empty for the browser-facing
	// upload/download kinds.
	DstMachineID string `json:"dst_machine_id,omitempty"`
	DstMachine   string `json:"dst,omitempty"`
	DstDir       string `json:"dst_dir,omitempty"`

	// RemotePaths is non-empty for a batch download: several roots
	// pulled into one local tree and delivered as a single tar.gz.
	// RemotePath stays empty (there is no single one) and Snapshot
	// carries both, so the queue row shows what the job actually moved.
	RemotePaths []string `json:"remote_paths,omitempty"`

	FilesTotal int    `json:"files_total,omitempty"` // 0 = nothing planned yet
	FilesDone  int    `json:"files_done"`
	Skipped    int    `json:"skipped"`
	SrcDir     string `json:"src_dir,omitempty"` // tree root, for tar/rename mapping

	mu         sync.Mutex
	done       int64
	status     string
	err        string
	cancel     context.CancelFunc
	settled    chan struct{} // closed when the worker is done with LocalFile
	settleOnce sync.Once
	fetching   int // live /result streams over LocalFile

	// paused parks the worker in waitIfPaused; resumeCh is closed by
	// Resume. The pair is separate from status because Remove and the
	// eviction cap must treat a parked job as alive — the worker is
	// still mid-transfer — while every terminal status is not.
	paused   bool
	resumeCh chan struct{}

	// onCancel wakes a job whose worker is not a goroutine of this
	// package. A terminal-fed ZMODEM session polls the cancel flag between
	// data blocks, which is useless during its arm window: without this
	// the queue's cancel button would leave the terminal held (and the
	// user's keystrokes buffered) until the arm deadline expired.
	onCancel func()

	// cur is the file being moved right now (recursive jobs only);
	// speed/eta are smoothed server-side so every client agrees.
	cur       string
	overwrite string // copy jobs: skip | overwrite | rename
	truncated bool   // tree hit the walk's depth/entry caps
	lastDone  int64
	lastAt    time.Time
	speed     int64
	eta       int64

	// kindDir marks a tree job (LocalFile is a directory), relay marks a
	// machine-to-machine job (no local file at all). Both drive cleanup
	// and the /result streaming shape.
	kindDir bool
	relay   bool

	// mode is an explicit permission requested by an upload. Zero means
	// "leave whatever the server's umask produced", which is what every
	// upload did before the multipart form grew an optional mode field.
	mode os.FileMode
}

// current returns the file a recursive job is on right now ("" for
// single-file jobs).
func (t *Transfer) current() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

func (t *Transfer) setCurrent(p string) {
	t.mu.Lock()
	t.cur = p
	t.mu.Unlock()
}

// currentPolicy reads the copy job's overwrite rule.
func (t *Transfer) currentPolicy() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.overwrite
}

func (t *Transfer) setTotal(n int64) {
	t.mu.Lock()
	t.Total = n
	t.mu.Unlock()
}

// fileDone advances the per-file counter of a recursive job. skipped is
// the number of entries the walk refused to move (special files,
// symlink failures) and is only ever positive.
func (t *Transfer) fileDone(skipped int) {
	t.mu.Lock()
	t.FilesDone++
	t.Skipped += skipped
	t.mu.Unlock()
}

func (t *Transfer) Status() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *Transfer) Done() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

func (t *Transfer) Err() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// IsDirJob reports whether LocalFile is a directory tree rather than one
// file, which decides whether /result streams it as tar.gz.
func (t *Transfer) IsDirJob() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.kindDir
}

// setDone records an absolute byte count, as reported by the chunk
// loops, and folds it into the smoothed speed/eta shown in the queue.
func (t *Transfer) setDone(n int64) {
	t.mu.Lock()
	delta := n - t.done
	t.mu.Unlock()
	if delta > 0 {
		t.noteProgress(delta)
	}
}

// noteProgress accumulates transferred bytes. It runs inside the 64 KiB
// copy loop, so the speed sample is only refreshed twice a second.
func (t *Transfer) noteProgress(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done += n
	now := time.Now()
	if t.lastAt.IsZero() {
		t.lastAt, t.lastDone = now, t.done
		return
	}
	elapsed := now.Sub(t.lastAt).Seconds()
	if elapsed < 0.5 {
		return
	}
	inst := int64(float64(t.done-t.lastDone) / elapsed)
	if t.speed == 0 {
		t.speed = inst
	} else {
		t.speed = (inst + 3*t.speed) / 4 // EMA, weighted toward the past
	}
	if t.speed < 0 {
		t.speed = 0
	}
	t.lastDone, t.lastAt = t.done, now
	if t.Total > t.done && t.speed > 0 {
		t.eta = (t.Total - t.done) / t.speed
	} else {
		t.eta = 0
	}
}

func (t *Transfer) finish(status, errMsg string) {
	t.mu.Lock()
	if t.status == TransferDone || t.status == TransferCanceled || t.status == TransferError {
		t.mu.Unlock()
		return
	}
	t.status = status
	t.err = errMsg
	now := time.Now().UTC()
	t.FinishedAt = &now
	t.mu.Unlock()
}

func (t *Transfer) cancelRequested() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status == TransferCanceled
}

// waitIfPaused parks the worker while the job is paused. Every chunk
// boundary the job already checks — the file loops, the single-file
// download loop, and inside stream() — calls it, so a pause takes
// effect within one 64 KiB chunk. Cancel is the release valve: a
// cancel issued while the worker is parked unblocks it, because the
// context release is the same event the gate selects on.
func (t *Transfer) waitIfPaused(ctx context.Context) error {
	t.mu.Lock()
	paused := t.paused
	resume := t.resumeCh
	t.mu.Unlock()
	if !paused || resume == nil {
		return nil
	}
	select {
	case <-resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// settle records that the worker has stopped touching LocalFile. It is
// idempotent because not every job has a worker goroutine of this
// package: run() settles an SFTP job once its lease is back, while a
// terminal-fed ZMODEM session settles itself from Finish.
func (t *Transfer) settle() {
	t.settleOnce.Do(func() {
		t.mu.Lock()
		ch := t.settled
		t.mu.Unlock()
		if ch != nil {
			close(ch)
		}
	})
}

// PinFetch holds the queue's eviction cap off this job while a browser
// downloads its result. Without it, another user's list request can pass
// the 50-job cap and RemoveAll the very tree being streamed — after the
// response already committed a 200 and a Content-Length. It doubles as the
// "finished" gate the /result handler needs. A Done job's bytes are all on
// disk already (the worker's remaining business is its lease), so this
// deliberately does not wait for settle: a client that reacts to the SSE
// event would otherwise race itself into a 409.
func (t *Transfer) PinFetch() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != TransferDone {
		return false
	}
	t.fetching++
	return true
}

func (t *Transfer) UnpinFetch() {
	t.mu.Lock()
	t.fetching--
	t.mu.Unlock()
}

func (t *Transfer) inFetch() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fetching > 0
}

// setTruncated marks a tree job whose walk stopped at a depth or entry
// cap. The job still succeeds — it moved everything it was willing to
// walk — but the UI has to say the result is a partial copy, or a user
// will trust a folder that silently lost two thirds of its files.
func (t *Transfer) setTruncated() {
	t.mu.Lock()
	t.truncated = true
	t.mu.Unlock()
}

// Snapshot is the JSON-safe view the API layer returns (list and start
// responses). It takes a consistent view under the lock so a response
// never mixes statuses from two different moments.
func (t *Transfer) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"id":          t.ID,
		"machine_id":  t.MachineID,
		"machine":     t.Machine,
		"kind":        t.Kind,
		"remote_path": t.RemotePath,
		"file_name":   t.FileName,
		"total":       t.Total,
		"done":        t.done,
		"status":      t.status,
		"error":       t.err,
		"started_at":  t.StartedAt.UTC().Format(time.RFC3339),
	}
	if t.FinishedAt != nil {
		out["finished_at"] = t.FinishedAt.UTC().Format(time.RFC3339)
	}
	// Everything below is additive: the keys above are what the shipped
	// SPA already reads, and single-file jobs keep the new counters at
	// their zero values.
	if t.DstMachineID != "" {
		out["dst_machine_id"] = t.DstMachineID
		out["dst_machine"] = t.DstMachine
		out["dst_dir"] = t.DstDir
	}
	if len(t.RemotePaths) > 0 {
		out["remote_paths"] = t.RemotePaths
	}
	if t.FilesTotal > 0 {
		out["files_total"] = t.FilesTotal
	}
	out["files_done"] = t.FilesDone
	out["skipped"] = t.Skipped
	if t.truncated {
		out["truncated"] = true
	}
	if t.SrcDir != "" {
		out["src_dir"] = t.SrcDir
	}
	if t.cur != "" {
		out["current_path"] = t.cur
	}
	if t.status == TransferRunning && t.speed > 0 {
		out["speed_bps"] = t.speed
		out["eta_seconds"] = t.eta
	}
	return out
}

// TransferManager owns the job table and the temp-file areas on the
// webkvm host (dataDir/ssh-transfers/{tmp,out}).
type TransferManager struct {
	mu        sync.Mutex
	transfers map[string]*Transfer
	// stages holds the browser-to-host batches waiting for a terminal.
	// They are files, not jobs, so they live apart from the queue table
	// and die on their own timer.
	stages map[string]*ZmodemStage
	// stageTTL is how long a staged batch may wait for its terminal. It
	// lives on the manager so a test can watch a deadline pass instead of
	// sleeping a quarter of an hour; zmodemStageTTL is the production
	// answer.
	stageTTL time.Duration
	// stallGrace is the watchdog's patience with a canceled job, same
	// reason as stageTTL: a test that wants to watch it fire should not
	// have to sleep a quarter of a minute.
	stallGrace time.Duration
	store      *Store
	sftp       sessionSource
	hub        *events.Hub
	dataDir    string
	// jobs bounds how many transfers run at once. Each one holds a
	// leased SFTP session, and a recursive job already keeps its two
	// ends busy back to back; three is enough to saturate a gigabit
	// link without turning the host into a file server for one user.
	jobs chan struct{}
}

// maxConcurrentJobs is the size of the job semaphore. Files inside one
// job move sequentially on purpose: it keeps the progress and
// cancellation semantics of a tree atomic and the UI honest.
const maxConcurrentJobs = 3

// maxRememberedJobs is how many finished jobs one machine's view keeps for
// the queue's history before the oldest are swept along with their
// on-host files.
const maxRememberedJobs = 50

func NewTransferManager(store *Store, sftp *SFTPSessionRegistry, hub *events.Hub, dataDir string) *TransferManager {
	// The nil check matters: assigning a nil *SFTPSessionRegistry
	// straight into the interface would leave a non-nil interface
	// holding a nil pointer, and lease() would panic instead of
	// reporting that sftp is not wired.
	var src sessionSource
	if sftp != nil {
		src = sftp
	}
	return NewTransferManagerSource(store, src, hub, dataDir)
}

// NewTransferManagerSource is the seam the tests use to inject a fake
// filesystem. The production constructor above keeps the single call
// site's signature unchanged.
func NewTransferManagerSource(store *Store, sftp sessionSource, hub *events.Hub, dataDir string) *TransferManager {
	return &TransferManager{
		transfers:  map[string]*Transfer{},
		stages:     map[string]*ZmodemStage{},
		stageTTL:   zmodemStageTTL,
		stallGrace: defaultStallGrace,
		store:      store,
		sftp:       sftp,
		hub:        hub,
		dataDir:    dataDir,
		jobs:       make(chan struct{}, maxConcurrentJobs),
	}
}

// register puts a job in the table and hands it the lifecycle channel
// Remove and the eviction cap read. Every constructor goes through here,
// so a job can never be listed before it is watchable or watched before
// it is listed.
func (m *TransferManager) register(t *Transfer) {
	t.settled = make(chan struct{})
	m.mu.Lock()
	m.transfers[t.ID] = t
	m.mu.Unlock()
}

// run schedules a job worker under the semaphore. The job settles only
// after work() has returned, which is after the lease release it
// deferred: that ordering is what makes "settled" mean nothing is still
// writing into LocalFile.
func (m *TransferManager) run(name string, t *Transfer, work func()) {
	go func() {
		defer safego.Recover(name)
		defer t.settle()
		m.jobs <- struct{}{}
		defer func() { <-m.jobs }()
		go m.watchdog(t)
		work()
	}()
}

// defaultStallGrace is how long a job gets to notice its own cancellation
// before the watchdog concludes it is wedged rather than merely slow. A
// healthy worker reacts within one 64 KiB chunk.
const defaultStallGrace = 15 * time.Second

// watchdog frees the queue when a canceled job's worker cannot be
// interrupted. Cancellation is only observed between chunks, and a
// pkg/sftp read over a half-dead TCP connection has no deadline at all:
// it blocks forever, holding that job's lease, one of the three semaphore
// slots and its on-host temp area, with nothing a user can do about it
// until webkvm restarts. Dropping the machine's pooled session turns the
// pending request into an error, which returns the worker.
//
// Sibling jobs on the same machine break with it. That is the trade: a
// permanent stall for everybody, or one retriable failure for whoever
// happened to share the session. The next Acquire dials a fresh one.
func (m *TransferManager) watchdog(t *Transfer) {
	defer safego.Recover("ssh_machines_transfer_watchdog")
	if m.sftp == nil || m.stallGrace <= 0 {
		return
	}
	t.mu.Lock()
	settled := t.settled
	t.mu.Unlock()
	if settled == nil {
		return
	}
	for {
		select {
		case <-settled:
			return
		case <-time.After(m.stallGrace):
		}
		if !t.cancelRequested() {
			continue
		}
		slog.Warn("ssh_machines_transfer_stalled", "job", t.ID, "machine", t.Machine, "dst_machine", t.DstMachineID)
		m.sftp.Drop(t.MachineID)
		if t.DstMachineID != "" {
			m.sftp.Drop(t.DstMachineID)
		}
		select {
		case <-settled:
			return
		case <-time.After(m.stallGrace):
		}
		// The wedge is not the socket (a blocked local write, a hung
		// filesystem). Nothing else can be pulled from here.
		slog.Error("ssh_machines_transfer_stuck", "job", t.ID, "machine", t.Machine)
		return
	}
}

// lease acquires a machine's SFTP session, translating a missing
// registry (unit tests, or a backend built without machine support)
// into an error instead of a nil-interface panic.
func (m *TransferManager) lease(machineID string) (sftpFS, func(), error) {
	if m.sftp == nil {
		return nil, nil, errors.New("sftp session registry is not wired")
	}
	return m.sftp.Acquire(machineID)
}

// tmpDir / outDir are the on-host staging areas.
func (m *TransferManager) tmpDir() string { return filepath.Join(m.dataDir, "ssh-transfers", "tmp") }
func (m *TransferManager) outDir() string { return filepath.Join(m.dataDir, "ssh-transfers", "out") }

// machineForTransfer resolves the record a job is named after. The
// built-in host card may sit in the store since UpdateHost, but
// List()/Get() exclude it, so it resolves to the synthetic record: the
// queue row only needs its name.
func (m *TransferManager) machineForTransfer(machineID string) (*Machine, bool) {
	if machineID == HostMachineID {
		return SyntheticHost(), true
	}
	return m.store.Get(machineID)
}

// StartUpload pushes srcFile (already on the webkvm host, e.g. the
// spooled multipart upload) to remotePath. Returns the job immediately;
// the worker runs in the background. A non-zero mode is asserted on the
// scratch file before it is published (the sftp subsystem takes no perm
// on create and the server's umask would otherwise eat the bits).
func (m *TransferManager) StartUpload(machineID, remotePath string, srcFile string, mode os.FileMode) (*Transfer, error) {
	machine, ok := m.machineForTransfer(machineID)
	if !ok {
		return nil, errors.New("machine not found")
	}
	if !machine.CarriesFiles() {
		return nil, fmt.Errorf("transfers are only available for ssh machines (this machine is %s)", machine.Protocol)
	}
	fi, err := os.Stat(srcFile)
	if err != nil {
		return nil, fmt.Errorf("read local file: %w", err)
	}

	id := uuid.NewString()
	t := &Transfer{
		ID:         id,
		MachineID:  machineID,
		Machine:    machine.Name,
		Kind:       "upload",
		RemotePath: CleanRemotePath(remotePath),
		LocalFile:  srcFile,
		StartedAt:  time.Now().UTC(),
		status:     TransferRunning,
		mode:       mode,
	}
	t.mu.Lock()
	t.Total = fi.Size()
	t.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.register(t)

	m.run("ssh_machines_transfer_upload", t, func() { m.runUpload(ctx, t) })
	return t, nil
}

// runUpload publishes the spooled file with putStream, so the destination
// keeps its previous contents until the last byte has landed: a failure
// costs a scratch file the same call removes, never the user's data.
func (m *TransferManager) runUpload(ctx context.Context, t *Transfer) {
	defer func() {
		_ = os.Remove(t.LocalFile) // the spooled temp file is job-private
	}()
	fail := func(err error) {
		t.finish(TransferError, err.Error())
		m.broadcast(t)
	}

	client, release, err := m.lease(t.MachineID)
	if err != nil {
		fail(err)
		return
	}
	defer release()

	dst := t.RemotePath
	if parent := path.Dir(dst); parent != "" && parent != "/" {
		fi, err := client.Stat(parent)
		if err != nil {
			fail(fmt.Errorf("destination directory %s does not exist: %w", parent, err))
			return
		}
		if !fi.IsDir() {
			fail(fmt.Errorf("destination directory %s is not a directory", parent))
			return
		}
	}

	src, err := os.Open(t.LocalFile)
	if err != nil {
		fail(err)
		return
	}
	defer src.Close()

	lastBroadcast := time.Time{}
	err = putStream(ctx, client, dst, src, putOpts{
		JobID: t.ID,
		Mode:  t.mode,
		Gate:  t.waitIfPaused,
		OnBytes: func(delta int64) {
			t.noteProgress(delta)
			if time.Since(lastBroadcast) > 300*time.Millisecond {
				lastBroadcast = time.Now()
				m.broadcast(t)
			}
		},
	})
	if err != nil {
		// Cancel() already flipped the status; the context error is the
		// same event seen from the worker, so it must not overwrite it.
		if t.cancelRequested() || errors.Is(err, context.Canceled) {
			m.markCanceled(t)
			return
		}
		fail(fmt.Errorf("upload %s: %w", dst, err))
		return
	}
	t.finish(TransferDone, "")
	m.broadcast(t)
}

func (m *TransferManager) markCanceled(t *Transfer) {
	t.finish(TransferCanceled, "canceled by user")
	m.broadcast(t)
}

// StartDownload pulls remotePath into an on-host temp file (outDir/<id>).
// The SPA then fetches it via the /transfers/{id}/result endpoint.
func (m *TransferManager) StartDownload(machineID, remotePath, fileName string) (*Transfer, error) {
	machine, ok := m.machineForTransfer(machineID)
	if !ok {
		return nil, errors.New("machine not found")
	}
	if !machine.CarriesFiles() {
		return nil, fmt.Errorf("transfers are only available for ssh machines (this machine is %s)", machine.Protocol)
	}
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		return nil, err
	}

	id := uuid.NewString()
	localFile := filepath.Join(m.outDir(), id)
	t := &Transfer{
		ID:         id,
		MachineID:  machineID,
		Machine:    machine.Name,
		Kind:       "download",
		RemotePath: CleanRemotePath(remotePath),
		LocalFile:  localFile,
		FileName:   fileName,
		StartedAt:  time.Now().UTC(),
		status:     TransferRunning,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	m.register(t)

	m.run("ssh_machines_transfer_download", t, func() { m.runDownload(ctx, t) })
	return t, nil
}

func (m *TransferManager) runDownload(ctx context.Context, t *Transfer) {
	// A DONE download must survive for MachineTransferResult to stream
	// it; Remove(jobID) is the only thing allowed to delete it. Anything
	// else (error/cancel) unloads a partial file immediately. RemoveAll
	// because a directory job's LocalFile is a tree.
	defer func() {
		if t.Status() != TransferDone {
			_ = os.RemoveAll(t.LocalFile)
		}
	}()
	fail := func(err error) {
		t.finish(TransferError, err.Error())
		m.broadcast(t)
	}

	client, release, err := m.lease(t.MachineID)
	if err != nil {
		fail(err)
		return
	}
	defer release()

	// A batch download pulls every root into one tree (planForBatch)
	// and streams it back as a single tar.gz; runDownloadDir and the
	// /result endpoint already know that shape.
	if len(t.RemotePaths) > 0 {
		m.runDownloadDir(ctx, t, client)
		return
	}

	// Stat follows a symlink, so a linked-to directory downloads as a
	// tree and a broken link fails with the SFTP error.
	if fi, serr := client.Stat(t.RemotePath); serr == nil && fi.IsDir() {
		m.runDownloadDir(ctx, t, client)
		return
	}

	remote, err := client.Open(t.RemotePath)
	if err != nil {
		fail(fmt.Errorf("open remote file: %w", err))
		return
	}
	defer remote.Close()
	fi, err := remote.Stat()
	if err != nil {
		fail(err)
		return
	}
	t.setTotal(fi.Size())

	out, err := os.OpenFile(t.LocalFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		fail(err)
		return
	}
	var copied int64
	buf := make([]byte, 64*1024)
	lastBroadcast := time.Time{}
	for {
		if t.cancelRequested() {
			_ = out.Close()
			m.markCanceled(t)
			return
		}
		if err := t.waitIfPaused(ctx); err != nil {
			_ = out.Close()
			m.markCanceled(t)
			return
		}
		n, rerr := remote.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				_ = out.Close()
				fail(fmt.Errorf("write local file: %w", werr))
				return
			}
			copied += int64(n)
			t.setDone(copied)
			if time.Since(lastBroadcast) > 300*time.Millisecond {
				lastBroadcast = time.Now()
				m.broadcast(t)
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				_ = out.Close()
				fail(fmt.Errorf("download %s: %w", t.RemotePath, rerr))
				return
			}
			break
		}
	}
	if cerr := out.Close(); cerr != nil {
		fail(cerr)
		return
	}
	t.finish(TransferDone, "")
	m.broadcast(t)
}

// Cancel flips a job to canceled and releases its context. The worker
// notices at its next chunk boundary and cleans up whatever it had
// started; if it never notices, watchdog() drops the machine's session to
// force the issue. The API answers as soon as the flag is set. A paused
// job cancels the same way: the context release is what unblocks the
// worker parked in waitIfPaused.
func (m *TransferManager) Cancel(id string) error {
	m.mu.Lock()
	t, ok := m.transfers[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("transfer not found")
	}
	t.mu.Lock()
	status := t.status
	t.mu.Unlock()
	if !t.cancelRequested() && (status == TransferRunning || status == TransferPaused) {
		t.finish(TransferCanceled, "canceling…")
		if t.cancel != nil {
			t.cancel()
		}
		t.mu.Lock()
		hook := t.onCancel
		t.mu.Unlock()
		if hook != nil {
			hook()
		}
		// If the job already finished between the read above and
		// finish(), finish() is a no-op and the real status stands.
		return nil
	}
	return errors.New("transfer is not running")
}

// Pause parks a running job between chunks. The job keeps its lease and
// its semaphore slot — parking is not a failure, and a resume must not
// re-dial — and its scratch files stay where they are, so the remaining
// bytes of the current file are still the atomic publish they started
// as. Cancel remains available from paused; Remove is not (the worker
// would resume into a deleted file).
func (m *TransferManager) Pause(id string) error {
	m.mu.Lock()
	t, ok := m.transfers[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("transfer not found")
	}
	t.mu.Lock()
	if t.status != TransferRunning {
		t.mu.Unlock()
		return fmt.Errorf("transfer is %s, not running", t.status)
	}
	t.paused = true
	t.resumeCh = make(chan struct{})
	t.status = TransferPaused
	t.mu.Unlock()
	m.broadcast(t)
	return nil
}

// Resume releases a paused job. resumeCh is closed, not reset, so a
// worker already parked in waitIfPaused wakes on it, while a worker
// still between chunks sees paused=false at its next check. It is the
// only path out of paused, and the only one that may close resumeCh.
func (m *TransferManager) Resume(id string) error {
	m.mu.Lock()
	t, ok := m.transfers[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("transfer not found")
	}
	t.mu.Lock()
	if t.status != TransferPaused {
		t.mu.Unlock()
		return fmt.Errorf("transfer is %s, not paused", t.status)
	}
	t.paused = false
	ch := t.resumeCh
	t.status = TransferRunning
	t.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	m.broadcast(t)
	return nil
}

// List returns the machine's jobs, newest first (capped; finished
// jobs are kept for UI history until Remove or the cap evicts them).
// A copy job is listed on BOTH ends: the source page shows what it
// pushed, the destination page shows what arrived.
func (m *TransferManager) List(machineID string) []map[string]any {
	return m.listMatching(func(t *Transfer) bool {
		return t.MachineID == machineID || t.DstMachineID == machineID
	})
}

// ListAll returns every job the manager still remembers, newest first.
// The dual-pane transfer sidebar uses it to avoid one request per
// machine. Eviction is deliberately NOT performed here: pruning on a
// global view could drop a job that is still visible on a machine page,
// so ListAll is a pure read and List owns the cap.
func (m *TransferManager) ListAll(limit int) []map[string]any {
	m.mu.Lock()
	jobs := make([]*Transfer, 0, len(m.transfers))
	for _, t := range m.transfers {
		jobs = append(jobs, t)
	}
	m.mu.Unlock()
	sortTransfersNewestFirst(jobs)
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Snapshot())
	}
	return out
}

func (m *TransferManager) listMatching(keep func(*Transfer) bool) []map[string]any {
	m.mu.Lock()
	jobs := make([]*Transfer, 0, len(m.transfers))
	for _, t := range m.transfers {
		if keep(t) {
			jobs = append(jobs, t)
		}
	}
	m.mu.Unlock()
	// Cap: drop the oldest finished ones beyond 50. Walk the
	// newest-first slice from the tail so the eviction order is
	// deterministic and the live map is never mutated while being
	// iterated.
	sortTransfersNewestFirst(jobs)
	var evicted []*Transfer
	if len(jobs) > maxRememberedJobs {
		for i := len(jobs) - 1; i >= 0 && len(jobs)-len(evicted) > maxRememberedJobs; i-- {
			t := jobs[i]
			// Finished is not enough: a job some browser is currently
			// downloading would answer that request with a truncated file
			// and a 200 that cannot be taken back. A paused job is alive
			// for the same reason from the other side — its worker is
			// parked mid-transfer, and a resume would write into a tree
			// the eviction just deleted. Eviction skips both and the
			// next list call collects them instead.
			st := t.Status()
			if st != TransferRunning && st != TransferPaused && !t.inFetch() {
				evicted = append(evicted, t)
			}
		}
		if len(evicted) > 0 {
			m.mu.Lock()
			for _, t := range evicted {
				delete(m.transfers, t.ID)
			}
			m.mu.Unlock()
			for _, t := range evicted {
				_ = os.RemoveAll(t.LocalFile)
			}
		}
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		if !containsTransfer(evicted, j) {
			out = append(out, j.Snapshot())
		}
	}
	return out
}

func containsTransfer(list []*Transfer, t *Transfer) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}

func (m *TransferManager) Get(id string) *Transfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transfers[id]
}

// CancelAll flips every running job to canceled on backend shutdown.
// The workers notice via cancelRequested() at their next chunk
// boundary; nothing blocks on them returning.
func (m *TransferManager) CancelAll() {
	m.mu.Lock()
	all := make([]*Transfer, 0, len(m.transfers))
	for _, t := range m.transfers {
		all = append(all, t)
	}
	m.mu.Unlock()
	for _, t := range all {
		m.Cancel(t.ID)
	}
}

// WaitAll blocks until every job this manager remembers has a worker
// that has fully returned — lease released, scratch file swept, nothing
// still touching a machine session. It is the shutdown pair of
// CancelAll: cancel only flips the flag, and a worker queued on the
// semaphore notices it only when it finally runs, which can be after the
// caller has moved on and closed the session registry out from under
// it. timeout bounds the wait; a worker wedged on a dead socket is the
// watchdog's business, not this one's.
func (m *TransferManager) WaitAll(timeout time.Duration) bool {
	m.mu.Lock()
	jobs := make([]*Transfer, 0, len(m.transfers))
	for _, t := range m.transfers {
		jobs = append(jobs, t)
	}
	m.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for _, t := range jobs {
		t.mu.Lock()
		ch := t.settled
		t.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
			return false
		}
	}
	return true
}

// Remove deletes a finished job and its local temp file. A paused job
// is refused the same way a running one is: its worker is parked in the
// middle of a transfer, and deleting now would leave the resume to write
// into a file that no longer exists.
func (m *TransferManager) Remove(id string) error {
	m.mu.Lock()
	t, ok := m.transfers[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("transfer not found")
	}
	if t.Status() == TransferRunning {
		return errors.New("transfer is still running; cancel it first")
	}
	if t.Status() == TransferPaused {
		return errors.New("transfer is paused; resume or cancel it first")
	}
	// No wait for the worker to return: a job that is not Running will
	// sweep LocalFile itself when it does (runDownload's deferred cleanup
	// covers every non-Done status), so deleting now cannot strand a tree.
	m.mu.Lock()
	delete(m.transfers, id)
	m.mu.Unlock()
	_ = os.RemoveAll(t.LocalFile)
	return nil
}

// RemoveFinished unloads every job that is no longer alive — done, error,
// canceled — in one pass. It is the server side of the queue's "clear
// completed", and it returns the machines it touched so the caller can
// audit each of them. A running or paused job is skipped for exactly the
// reason eviction skips it (a live worker would lose its scratch file or
// park on a tree that is being deleted), and a job some browser is
// mid-download on is skipped because those bytes are already on their
// way.
func (m *TransferManager) RemoveFinished() (int, []string) {
	m.mu.Lock()
	removed := make([]*Transfer, 0, 16)
	machines := map[string]bool{}
	for id, t := range m.transfers {
		if st := t.Status(); st == TransferRunning || st == TransferPaused {
			continue
		}
		if t.inFetch() {
			continue
		}
		removed = append(removed, t)
		delete(m.transfers, id)
		if t.MachineID != "" {
			machines[t.MachineID] = true
		}
		if t.DstMachineID != "" {
			machines[t.DstMachineID] = true
		}
	}
	m.mu.Unlock()
	// The files are swept after the lock: RemoveAll on a finished
	// download's tree is the slow part, and it does not need the table
	// locked while it runs.
	for _, t := range removed {
		_ = os.RemoveAll(t.LocalFile)
	}
	out := make([]string, 0, len(machines))
	for id := range machines {
		out = append(out, id)
	}
	sort.Strings(out)
	return len(removed), out
}

// SweepOrphans empties both staging areas. It is meant for startup,
// where the job table is empty by construction, so anything left in
// ssh-transfers/{tmp,out} is a crash or upgrade remnant that no request
// can ever reach again. Without it a killed backend leaks every spooled
// upload and downloaded tree into the datadir forever.
func (m *TransferManager) SweepOrphans() (int, error) {
	if m.dataDir == "" {
		return 0, nil
	}
	removed := 0
	var errs []string
	for _, dir := range []string{m.tmpDir(), m.outDir()} {
		ents, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, e := range ents {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			removed++
		}
	}
	if len(errs) > 0 {
		return removed, errors.New(strings.Join(errs, "; "))
	}
	return removed, nil
}

// broadcast pushes a progress event onto the hub. VmID is left empty
// on purpose: the SSE ACL filter in api/events.go only gates events that
// carry a VM id, and machines are not VMs.
func (m *TransferManager) broadcast(t *Transfer) {
	if m.hub == nil {
		return
	}
	t.mu.Lock()
	data := map[string]any{
		"job_id":      t.ID,
		"machine_id":  t.MachineID,
		"machine":     t.Machine,
		"kind":        t.Kind,
		"remote_path": t.RemotePath,
		"done":        t.done,
		"total":       t.Total,
		"status":      t.status,
		"files_done":  t.FilesDone,
		"skipped":     t.Skipped,
		"file_name":   t.FileName,
	}
	if t.truncated {
		// The walk stopped at a cap: the job succeeded, but the tree it
		// describes is partial and the queue has to say so.
		data["truncated"] = true
	}
	// A terminal-driven ZMODEM job fails for reasons the user can act on
	// ("lrzsz is not installed"), and the banner reads them straight off
	// the event instead of refetching the whole list.
	if t.err != "" {
		data["error"] = t.err
	}
	if t.DstMachineID != "" {
		data["dst_machine_id"] = t.DstMachineID
		data["dst_machine"] = t.DstMachine
	}
	if t.FilesTotal > 0 {
		data["files_total"] = t.FilesTotal
	}
	if t.SrcDir != "" {
		data["src_dir"] = t.SrcDir
	}
	if t.cur != "" {
		data["current_path"] = t.cur
	}
	if t.status == TransferRunning && t.speed > 0 {
		data["speed_bps"] = t.speed
		data["eta_seconds"] = t.eta
	}
	t.mu.Unlock()
	m.hub.Broadcast(events.Event{
		Type:      "transfer.progress",
		Name:      t.Machine,
		Data:      data,
		Timestamp: time.Now().Unix(),
	})
}

// CleanRemotePath normalizes a user-supplied remote path (SFTP-side;
// the remote user's own permissions are the security boundary, not us).
//
// Emptiness is the only thing trimmed: a remote directory can genuinely
// be named "log ", and stripping that would address a different file than
// the one the user picked.
//
// It is exported because the API layer normalizes the same strings before
// handing them here: two copies of this rule (one rooting, one not) would
// mean a job that writes somewhere the browser never named.
func CleanRemotePath(p string) string {
	if strings.TrimSpace(p) == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

func sortTransfersNewestFirst(jobs []*Transfer) {
	for i := 1; i < len(jobs); i++ {
		for j := i; j > 0 && jobs[j].StartedAt.After(jobs[j-1].StartedAt); j-- {
			jobs[j], jobs[j-1] = jobs[j-1], jobs[j]
		}
	}
}
