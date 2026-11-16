package sshmachines

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"webkvm/internal/safego"
)

// sftpIdle is how long a per-machine SFTP session may sit unused
// before the GC closes it. Terminal connections are NOT pooled: each
// WebSocket dials its own SSH session (a ticket-authorised, single-use
// connection), matching the HostTerminal precedent. SFTP, in contrast,
// serves many short file operations from the browser, so it is pooled
// per machine ID with idle reaping.
const sftpIdleTimeout = 5 * time.Minute

type sftpSessionEntry struct {
	mu     sync.Mutex
	client *sftp.Client
	// conn is the transport under the client: the SSH connection for a
	// dialed machine, both ends of the pipe for the built-in host.
	conn io.Closer
	// lastUsed is written from Get/Touch/Acquire and read by the reaper.
	// It used to be a plain int64 written by Touch after releasing r.mu,
	// which is a data race the -race detector flags as soon as two
	// machines are polled concurrently.
	lastUsed atomic.Int64
	// refs counts live Acquire leases. A directory transfer runs far
	// past sftpIdleTimeout, and the reaper must not close the client out
	// from under it.
	refs atomic.Int64
}

// SFTPSessionRegistry hands out one shared SFTP client per machine.
type SFTPSessionRegistry struct {
	mu       sync.Mutex
	store    *Store
	sessions map[string]*sftpSessionEntry
	stopGC   chan struct{}
}

func NewSFTPSessionRegistry(store *Store) *SFTPSessionRegistry {
	r := &SFTPSessionRegistry{
		store:    store,
		sessions: map[string]*sftpSessionEntry{},
		stopGC:   make(chan struct{}),
	}
	go func() {
		defer safego.Recover("ssh_machines_sftp_gc")
		r.gcLoop()
	}()
	return r
}

// Get returns a live SFTP client for the machine, dialing (strict
// host-key policy, stored credentials) when none is pooled. Host-key
// failures are returned as *HostKeyError so the API layer can turn them
// into the re-pin flow.
func (r *SFTPSessionRegistry) Get(machineID string) (*sftp.Client, error) {
	r.mu.Lock()
	if e, ok := r.sessions[machineID]; ok {
		e.lastUsed.Store(time.Now().Unix())
		r.mu.Unlock()
		e.mu.Lock()
		if e.client != nil {
			client := e.client
			e.mu.Unlock()
			return client, nil
		}
		e.mu.Unlock()
		r.closeEntry(machineID)
	}
	r.mu.Unlock()

	client, closer, err := openSFTP(r.store, machineID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	// A concurrent caller may have pooled a session while we dialed;
	// prefer theirs and drop ours. e.client has to be read under e.mu —
	// closeEntry nils it from another goroutine.
	if existing, ok := r.sessions[machineID]; ok {
		existing.mu.Lock()
		live := existing.client
		existing.mu.Unlock()
		if live != nil {
			r.mu.Unlock()
			_ = client.Close()
			_ = closer.Close()
			existing.lastUsed.Store(time.Now().Unix())
			return live, nil
		}
	}
	e := &sftpSessionEntry{client: client, conn: closer}
	e.lastUsed.Store(time.Now().Unix())
	r.sessions[machineID] = e
	r.mu.Unlock()
	return client, nil
}

// Acquire hands out a machine's client together with a release func.
// Callers that keep the client for more than one round trip (every
// transfer job) must hold a lease for that whole period, otherwise the
// idle reaper closes the session mid-transfer. release is idempotent.
//
// The reference is taken under e.mu and only on the entry that still
// holds exactly the client handed back by Get. Without that pairing, a
// reaper landing between the two steps would leave the count on the new
// session while the caller worked on an already-closed one: the job fails
// either way, but the leak was silent and outlived it.
func (r *SFTPSessionRegistry) Acquire(machineID string) (sftpFS, func(), error) {
	client, err := r.Get(machineID)
	if err != nil {
		return nil, nil, err
	}
	fs := sftpBridge{client}
	r.mu.Lock()
	e, ok := r.sessions[machineID]
	r.mu.Unlock()
	if !ok {
		return fs, func() {}, nil
	}
	e.mu.Lock()
	if e.client == client {
		e.refs.Add(1)
		e.lastUsed.Store(time.Now().Unix())
		e.mu.Unlock()
		var once sync.Once
		return fs, func() {
			once.Do(func() {
				e.refs.Add(-1)
				e.lastUsed.Store(time.Now().Unix())
			})
		}, nil
	}
	e.mu.Unlock()
	// Pooled session swapped out or closed while Get was returning; the
	// client we hold is still valid, so hand it back unleasped.
	return fs, func() {}, nil
}

// Touch marks a pooled session as in use (resets the idle timer).
func (r *SFTPSessionRegistry) Touch(machineID string) {
	r.mu.Lock()
	e, ok := r.sessions[machineID]
	r.mu.Unlock()
	if !ok {
		return
	}
	e.lastUsed.Store(time.Now().Unix())
}

// Close drops the pooled session for a machine (e.g. after delete or a
// failed dial). Safe to call when nothing is pooled.
func (r *SFTPSessionRegistry) Close(machineID string) {
	r.closeEntry(machineID)
}

// Drop force-closes a machine's session even when a job still holds a
// lease. Close is the polite teardown — the record is gone, nothing
// should be using it; Drop is the escape hatch for a canceled job whose
// worker cannot notice because the socket will never answer (see
// TransferManager.watchdog). Whatever is in flight on it fails, which is
// the only way to get that worker, its lease and its semaphore slot back.
// The next Acquire dials a fresh session.
func (r *SFTPSessionRegistry) Drop(machineID string) {
	r.closeEntry(machineID)
}

func (r *SFTPSessionRegistry) closeEntry(machineID string) {
	r.mu.Lock()
	e, ok := r.sessions[machineID]
	if ok {
		delete(r.sessions, machineID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	if e.client != nil {
		_ = e.client.Close()
		e.client = nil
	}
	if e.conn != nil {
		_ = e.conn.Close()
		e.conn = nil
	}
	e.mu.Unlock()
}

// CloseAll drops every pooled session (backend shutdown).
func (r *SFTPSessionRegistry) CloseAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.closeEntry(id)
	}
}

// Stop halts the GC loop.
func (r *SFTPSessionRegistry) Stop() {
	select {
	case <-r.stopGC:
	default:
		close(r.stopGC)
	}
}

func (r *SFTPSessionRegistry) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopGC:
			return
		case <-ticker.C:
			r.reapIdle()
		}
	}
}

func (r *SFTPSessionRegistry) reapIdle() {
	now := time.Now().Unix()
	timeout := int64(sftpIdleTimeout.Seconds())
	r.mu.Lock()
	var stale []string
	for id, e := range r.sessions {
		// A leased session is a running job's, however "idle" the
		// timestamp looks: a recursive walk touches the client once per
		// file and can sit past the 5-minute mark between them.
		if e.refs.Load() > 0 || now-e.lastUsed.Load() <= timeout {
			continue
		}
		stale = append(stale, id)
	}
	r.mu.Unlock()
	for _, id := range stale {
		slog.Debug("ssh_machines_sftp_session_reaped", "machine", id)
		r.closeEntry(id)
	}
}

// openSFTP establishes the transport behind a machine's client: an
// SSH-dialed session for a registered machine, the in-process pair for
// the built-in host card (see local_sftp.go). Authorization is not this
// function's business — the API layer is the only caller that can tell
// who asked, and it answers the host card to admins alone.
func openSFTP(store *Store, machineID string) (*sftp.Client, io.Closer, error) {
	if machineID == HostMachineID {
		return dialLocalSFTP()
	}
	machine, ok := store.Get(machineID)
	if !ok {
		return nil, nil, errors.New("machine not found")
	}
	sec, _, err := store.GetSecret(machineID)
	if err != nil {
		return nil, nil, fmt.Errorf("read stored credentials: %w", err)
	}
	client, conn, err := dialSFTP(machine, sec)
	if err != nil {
		return nil, nil, err
	}
	return client, conn, nil
}

// dialSFTP dials the machine (strict host key) and negotiates the SFTP
// subsystem. Returns (client, conn); on subsystem failure both are
// closed and a wrapped error is returned.
func dialSFTP(m *Machine, sec MachineSecret) (*sftp.Client, *ssh.Client, error) {
	conn, err := Dial(m, sec, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, &SFTPSubsystemError{Err: err, Machine: m.Name}
	}
	return client, conn, nil
}

// SFTPSubsystemError reports a machine whose SSH server does not
// provide the SFTP subsystem (the netcatty plan's "subsystem failure
// with an explicit error" case — SCP/zmodem modes are P2).
type SFTPSubsystemError struct {
	Err     error
	Machine string
}

func (e *SFTPSubsystemError) Error() string {
	return e.Machine + ": the SSH server does not provide an SFTP subsystem: " + e.Err.Error()
}

func (e *SFTPSubsystemError) Unwrap() error { return e.Err }
