package sshmachines

import (
	"sync"
	"testing"
	"time"
)

// The lease-aware reaper is the riskiest new logic in the registry: a
// recursive job holds one session for far longer than the idle timeout,
// and closing it mid-transfer would surface as a broken pipe inside a
// user's directory copy. These tests drive reapIdle directly because
// Acquire/Get need a dialable sshd.

func pooledRegistry() *SFTPSessionRegistry {
	return &SFTPSessionRegistry{
		sessions: map[string]*sftpSessionEntry{},
		stopGC:   make(chan struct{}),
	}
}

func staleEntry(refs int) *sftpSessionEntry {
	e := &sftpSessionEntry{}
	e.lastUsed.Store(time.Now().Add(-2 * sftpIdleTimeout).Unix())
	e.refs.Store(int64(refs))
	return e
}

func TestReapIdleClosesStaleSessions(t *testing.T) {
	r := pooledRegistry()
	r.sessions["idle"] = staleEntry(0)
	r.reapIdle()
	if len(r.sessions) != 0 {
		t.Fatalf("an idle session survived the reaper: %v", len(r.sessions))
	}
}

func TestReapIdleKeepsLeasedSessions(t *testing.T) {
	r := pooledRegistry()
	e := staleEntry(1)
	r.sessions["busy"] = e
	r.reapIdle()
	if r.sessions["busy"] == nil {
		t.Fatalf("a leased session was reaped: the reaper must honour refs")
	}
	// The refs guard is an exemption, not a permanent pin: once the job
	// releases its lease the stale session must be reaped on the next pass.
	e.refs.Store(0)
	r.reapIdle()
	if len(r.sessions) != 0 {
		t.Fatalf("the session stayed after its lease was released")
	}
	r.Stop()
}

func TestReapIdleKeepsFreshSessions(t *testing.T) {
	r := pooledRegistry()
	e := &sftpSessionEntry{}
	e.lastUsed.Store(time.Now().Unix())
	r.sessions["fresh"] = e
	r.reapIdle()
	if r.sessions["fresh"] == nil {
		t.Fatalf("a session inside the idle window was reaped")
	}
}

func TestCloseEntryIsIdempotentAndNilSafe(t *testing.T) {
	// client/conn are nil in these fixtures, so closeEntry must skip the
	// Close calls instead of panicking — the shape a unit test reaches,
	// and also the shape a half-failed dial leaves behind.
	r := pooledRegistry()
	r.sessions["m"] = &sftpSessionEntry{}
	r.closeEntry("m")
	r.closeEntry("m")
	if len(r.sessions) != 0 {
		t.Fatalf("closeEntry left the entry in the map")
	}
	r.Close("absent")
	r.CloseAll()
}

func TestStopIsIdempotent(t *testing.T) {
	r := NewSFTPSessionRegistry(nil)
	r.Stop()
	r.Stop() // a second Stop must not panic on the closed channel
	select {
	case <-r.stopGC:
	default:
		t.Fatalf("Stop did not signal the GC loop")
	}
}

func TestConcurrentTouchAndReapIsRaceFree(t *testing.T) {
	// The bug this guards: lastUsed used to be a plain int64 written
	// outside r.mu, which -race flagged the moment two machines were
	// polled at once.
	r := pooledRegistry()
	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		r.sessions[id] = &sftpSessionEntry{}
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := ids[i%len(ids)]
			r.Touch(id)
			r.reapIdle()
			r.Close(id)
			r.Touch(id)
		}(i)
	}
	wg.Wait()
	r.Stop()
}
