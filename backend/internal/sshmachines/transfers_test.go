package sshmachines

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedJob(m *TransferManager, id string, machineID string, started time.Time, status string) *Transfer {
	t := &Transfer{
		ID:         id,
		MachineID:  machineID,
		Machine:    "router",
		Kind:       "download",
		RemotePath: "/firmware.bin",
		LocalFile:  filepath.Join(m.outDir(), id),
		StartedAt:  started,
		status:     status,
	}
	if err := os.WriteFile(t.LocalFile, []byte("payload"), 0o640); err != nil {
		panic(err)
	}
	m.mu.Lock()
	m.transfers[id] = t
	m.mu.Unlock()
	return t
}

// A finished download's local file is what /transfers/{job}/result
// streams: List must keep it for every job it does not evict.
func TestListKeepsFinishedJobFilesUnderTheCap(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		seedJob(m, fmt.Sprintf("job-%d", i), "m1", base.Add(time.Duration(i)*time.Minute), TransferDone)
	}
	if got := m.List("m1"); len(got) != 5 {
		t.Fatalf("List = %d jobs, want 5", len(got))
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(m.outDir(), fmt.Sprintf("job-%d", i))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("finished job %d lost its result file: %v", i, err)
		}
	}
}

// Over the cap the OLDEST finished jobs are evicted, newest-first order
// is preserved, and the eviction loop cannot walk off the slice.
func TestListEvictsOldestFinishedJobsBeyondCap(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 60; i++ {
		seedJob(m, fmt.Sprintf("job-%02d", i), "m1", base.Add(time.Duration(i)*time.Minute), TransferDone)
	}
	got := m.List("m1")
	if len(got) != 50 {
		t.Fatalf("List = %d jobs, want the cap of 50", len(got))
	}
	if got[0]["id"] != "job-59" {
		t.Fatalf("List is not newest-first: first = %v", got[0]["id"])
	}
	// The 10 oldest are gone from both the table and the disk.
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("job-%02d", i)
		if m.Get(id) != nil {
			t.Fatalf("%s survived the eviction", id)
		}
		if _, err := os.Stat(filepath.Join(m.outDir(), id)); !os.IsNotExist(err) {
			t.Fatalf("%s left its local file behind (err=%v)", id, err)
		}
	}
	for i := 10; i < 60; i++ {
		id := fmt.Sprintf("job-%02d", i)
		if _, err := os.Stat(filepath.Join(m.outDir(), id)); err != nil {
			t.Fatalf("%s is still listed but lost its result file: %v", id, err)
		}
	}
}

// Running jobs are never evicted, even when the table is over the cap —
// dropping one would strand its worker with a deleted output file.
func TestListNeverEvictsRunningJobs(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 60; i++ {
		seedJob(m, fmt.Sprintf("job-%02d", i), "m1", base.Add(time.Duration(i)*time.Minute), TransferRunning)
	}
	if got := m.List("m1"); len(got) != 60 {
		t.Fatalf("List = %d running jobs, want all 60 kept", len(got))
	}
}

// RemoveFinished is the server side of the queue's "clear completed":
// every terminal job unloads (table AND its local file), live jobs are
// untouched, and the machines list covers both ends of a copy.
func TestRemoveFinishedClearsOnlyTerminalJobs(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	seedJob(m, "done-1", "m1", base, TransferDone)
	seedJob(m, "done-2", "m2", base.Add(time.Minute), TransferDone)
	seedJob(m, "error-1", "m1", base.Add(2*time.Minute), TransferError)
	seedJob(m, "canceled-1", "m2", base.Add(3*time.Minute), TransferCanceled)
	seedJob(m, "running-1", "m1", base.Add(4*time.Minute), TransferRunning)
	seedJob(m, "paused-1", "m2", base.Add(5*time.Minute), TransferPaused)
	// A copy job: its destination is part of the audit surface, so it
	// must be reported even though the destination has no job of its own.
	cp := seedJob(m, "copy-1", "m1", base.Add(6*time.Minute), TransferDone)
	cp.DstMachineID = "m3"
	cp.DstMachine = "m3"
	cp.DstDir = "/srv"

	n, machines := m.RemoveFinished()
	if n != 5 {
		t.Fatalf("RemoveFinished removed %d, want the 5 terminal jobs", n)
	}
	for _, id := range []string{"done-1", "done-2", "error-1", "canceled-1", "copy-1"} {
		if m.Get(id) != nil {
			t.Errorf("%s survived RemoveFinished", id)
		}
		if _, err := os.Stat(filepath.Join(m.outDir(), id)); !os.IsNotExist(err) {
			t.Errorf("%s left its local file behind (err=%v)", id, err)
		}
	}
	for _, id := range []string{"running-1", "paused-1"} {
		if m.Get(id) == nil {
			t.Fatalf("%s was removed although its worker is still alive", id)
		}
		if _, err := os.Stat(filepath.Join(m.outDir(), id)); err != nil {
			t.Fatalf("%s lost its scratch file: %v", id, err)
		}
	}
	want := []string{"m1", "m2", "m3"}
	if len(machines) != len(want) {
		t.Fatalf("machines = %v, want %v", machines, want)
	}
	for i := range want {
		if machines[i] != want[i] {
			t.Fatalf("machines = %v, want %v", machines, want)
		}
	}

	// The second pass sees only live jobs and must touch nothing.
	if n, _ = m.RemoveFinished(); n != 0 {
		t.Fatalf("second RemoveFinished removed %d, want 0", n)
	}
}

// List is scoped to the requested machine.
func TestListScopedToMachine(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedJob(m, "a", "m1", now, TransferDone)
	seedJob(m, "b", "m2", now, TransferDone)
	got := m.List("m1")
	if len(got) != 1 || got[0]["id"] != "a" {
		t.Fatalf("List(m1) = %+v, want only job a", got)
	}
}

// CancelAll is the shutdown hook: every running job is flipped to
// canceled so its worker returns at the next chunk boundary.
func TestCancelAllStopsRunningJobs(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	running := seedJob(m, "running", "m1", time.Now().UTC(), TransferRunning)
	done := seedJob(m, "done", "m1", time.Now().UTC(), TransferDone)

	m.CancelAll()

	if s := running.Status(); s == TransferRunning {
		t.Fatalf("running job survived CancelAll (status %s)", s)
	}
	if done.Status() != TransferDone {
		t.Fatalf("a finished job must keep its status, got %s", done.Status())
	}
}

// Pause/Resume is a two-state gate between chunks, not a status the test
// can fake into a worker: the verbs refuse every job that is not in the
// state they leave, and the state is what the queue row renders.
func TestPauseResumeStateMachine(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	done := seedJob(m, "done", "m1", now, TransferDone)
	if err := m.Pause(done.ID); err == nil {
		t.Fatal("Pause on a finished job must refuse")
	}
	if err := m.Resume(done.ID); err == nil {
		t.Fatal("Resume on a finished job must refuse")
	}

	run := seedJob(m, "run", "m1", now, TransferRunning)
	if err := m.Resume(run.ID); err == nil {
		t.Fatal("Resume on a running job must refuse")
	}
	if err := m.Pause(run.ID); err != nil {
		t.Fatalf("Pause on a running job: %v", err)
	}
	if s := run.Status(); s != TransferPaused {
		t.Fatalf("status = %s, want paused", s)
	}
	if err := m.Pause(run.ID); err == nil {
		t.Fatal("a second Pause must refuse")
	}
	if err := m.Resume(run.ID); err != nil {
		t.Fatalf("Resume on a paused job: %v", err)
	}
	if s := run.Status(); s != TransferRunning {
		t.Fatalf("status = %s, want running again", s)
	}
}

// Cancel is the only other way out of paused. It flips the job and
// releases its context, which is the event a parked worker selects on.
func TestCancelFromPaused(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	paused := seedJob(m, "paused", "m1", time.Now().UTC(), TransferPaused)

	if err := m.Cancel(paused.ID); err != nil {
		t.Fatalf("Cancel on a paused job: %v", err)
	}
	if s := paused.Status(); s != TransferCanceled {
		t.Fatalf("status = %s, want canceled", s)
	}
	if !paused.cancelRequested() {
		t.Fatal("cancelRequested must see the flip, or the parked worker never wakes")
	}
}

// Remove refuses a paused job: its worker is parked mid-transfer and a
// resume after the removal would write into a deleted file.
func TestRemoveRefusesPausedJobs(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	paused := seedJob(m, "paused", "m1", time.Now().UTC(), TransferPaused)

	if err := m.Remove(paused.ID); err == nil {
		t.Fatal("Remove on a paused job must refuse")
	}
	if m.Get(paused.ID) == nil {
		t.Fatal("the refused removal must not unload the job")
	}
	if _, err := os.Stat(paused.LocalFile); err != nil {
		t.Fatalf("the refused removal deleted the in-flight file: %v", err)
	}
}

// Eviction skips paused jobs the same way it skips running ones: the
// worker is alive and would resume into a deleted tree.
func TestListEvictionSkipsPausedJobs(t *testing.T) {
	m := NewTransferManager(nil, nil, nil, t.TempDir())
	if err := os.MkdirAll(m.outDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 55; i++ {
		seedJob(m, fmt.Sprintf("done-%02d", i), "m1", base.Add(time.Duration(i)*time.Minute), TransferDone)
	}
	// Newest: the one alive job in the table.
	paused := seedJob(m, "paused", "m1", base.Add(56*time.Minute), TransferPaused)

	if got := m.List("m1"); len(got) != 50 {
		t.Fatalf("List = %d jobs, want the cap of 50", len(got))
	}
	if m.Get(paused.ID) == nil {
		t.Fatal("the paused job was evicted")
	}
	if _, err := os.Stat(paused.LocalFile); err != nil {
		t.Fatalf("the paused job lost its in-flight file: %v", err)
	}
}

// waitParkedWithLease blocks until the worker has actually parked. The
// status alone flips the moment Pause returns, which can be before the
// worker even acquired its lease — asserting on the status would race
// the goroutine's start. Parked-with-lease is the state the assertion
// needs, and once reached it holds until resume or cancel.
func waitParkedWithLease(t *testing.T, tr *Transfer, live func() int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if live() > 0 {
			s := tr.Status()
			if s == TransferPaused {
				return
			}
			if s != TransferRunning {
				t.Fatalf("job left running without parking (status %s, err %q)", s, tr.Err())
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job never parked with its lease (status %s, live %d)", tr.Status(), live())
}

// The integration shape of the gate: a real download worker parks inside
// the file loop, holds its lease while parked, and only resume or cancel
// move it again.
func TestPauseParksAWorkerBetweenChunks(t *testing.T) {
	const chunk = 64 * 1024
	const total = 5*chunk + 10

	startParked := func(t *testing.T, m *TransferManager, src *fakeSource, id string) *Transfer {
		t.Helper()
		fs := src.fsFor(id)
		fs.writeFile(t, "/data/big", strings.Repeat("x", total), 0o644)
		hold := fs.holdFirstRead("/data/big")

		tr, err := m.StartDownload(id, "/data/big", "big")
		if err != nil {
			t.Fatalf("StartDownload: %v", err)
		}
		// The worker is inside its first Read while this runs (the hold
		// is one-shot and the file is five chunks), so the pause always
		// lands on a running job.
		if err := m.Pause(tr.ID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		close(hold)
		waitParkedWithLease(t, tr, func() int { return src.live(id) })
		return tr
	}

	t.Run("resume finishes the transfer", func(t *testing.T) {
		m, src, a, _ := copyFixture(t)
		tr := startParked(t, m, src, a)

		if got := tr.Done(); got >= total {
			t.Fatalf("a parked job already has all its bytes (done=%d)", got)
		}
		// Parking is not a failure: the lease stays held, which is what
		// keeps the idle reaper from closing the session mid-pause.
		if live := src.live(a); live != 1 {
			t.Fatalf("live leases while paused = %d, want 1", live)
		}

		if err := m.Resume(tr.ID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		got := waitJob(t, m, tr.ID)
		mustDone(t, got)
		data, err := os.ReadFile(got.LocalFile)
		if err != nil {
			t.Fatalf("downloaded file missing: %v", err)
		}
		if len(data) != total {
			t.Fatalf("downloaded %d bytes, want %d", len(data), total)
		}
		waitLeases(t, src, a)
	})

	t.Run("cancel from parked sweeps the partial file", func(t *testing.T) {
		m, src, a, _ := copyFixture(t)
		tr := startParked(t, m, src, a)

		if err := m.Cancel(tr.ID); err != nil {
			t.Fatalf("Cancel from parked: %v", err)
		}
		got := waitJob(t, m, tr.ID)
		if s := got.Status(); s != TransferCanceled {
			t.Fatalf("status = %s, want canceled", s)
		}
		// The sweep is a defer in runDownload, so it lands just after the
		// canceled status is observable.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(got.LocalFile); os.IsNotExist(err) {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("partial file left on disk: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}
		waitLeases(t, src, a)
	})
}

// The pause can land before the worker has taken its lease: Pause flips
// the status the moment it returns, while the worker is still in
// Acquire and no lease exists yet. The worker must still park once it
// gets the lease — at zero bytes, holding it — because the reaper's
// contract (a parked job keeps its session) is the same before and after
// the first chunk.
func TestPauseBeforeTheWorkerTakesItsLease(t *testing.T) {
	const chunk = 64 * 1024
	const total = 3*chunk + 7

	m, src, a, _ := copyFixture(t)
	fs := src.fsFor(a)
	fs.writeFile(t, "/data/big", strings.Repeat("y", total), 0o644)
	hold := src.holdAcquire(a)

	tr, err := m.StartDownload(a, "/data/big", "big")
	if err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	// The worker is inside Acquire while this runs, so the pause is
	// guaranteed to land on a job whose worker holds no lease yet.
	if err := m.Pause(tr.ID); err != nil {
		t.Fatalf("Pause before the worker leases: %v", err)
	}
	close(hold)
	waitParkedWithLease(t, tr, func() int { return src.live(a) })

	if got := tr.Done(); got != 0 {
		t.Fatalf("a job parked before its first byte moved some (done=%d)", got)
	}
	if live := src.live(a); live != 1 {
		t.Fatalf("live leases while parked before start = %d, want 1", live)
	}

	if err := m.Resume(tr.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got := waitJob(t, m, tr.ID)
	mustDone(t, got)
	data, err := os.ReadFile(got.LocalFile)
	if err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
	if len(data) != total {
		t.Fatalf("downloaded %d bytes, want %d", len(data), total)
	}
	waitLeases(t, src, a)
}

// WaitAll is the shutdown pair of CancelAll: it observes the workers
// actually returning, not just their statuses flipping.
func TestWaitAllBlocksUntilWorkersSettle(t *testing.T) {
	m, src, a, _ := copyFixture(t)
	aFS := src.fsFor(a)
	aFS.writeFile(t, "/data/x", "payload", 0o644)
	aFS.writeFile(t, "/data/never", "stuck", 0o644)

	tr, err := m.StartDownload(a, "/data/x", "x")
	if err != nil {
		t.Fatal(err)
	}
	gate := aFS.blockOpen("/data/never")
	if _, err := m.StartDownload(a, "/data/never", "never"); err != nil {
		t.Fatal(err)
	}

	// The wedged worker keeps WaitAll honest: a status-only check would
	// have returned true the moment the quick job flipped to done.
	if m.WaitAll(300 * time.Millisecond) {
		t.Fatal("WaitAll returned true while a worker is wedged in Open")
	}
	if s := tr.Status(); s != TransferDone {
		t.Fatalf("the quick job should have settled (status %s)", s)
	}

	// Release the wedge and cancel: the worker notices at its first
	// chunk boundary and WaitAll sees it return.
	close(gate)
	m.CancelAll()
	if !m.WaitAll(5 * time.Second) {
		t.Fatal("WaitAll timed out after the wedge was released")
	}
	waitLeases(t, src, a)

	// An empty manager settles immediately.
	empty := NewTransferManager(nil, nil, nil, t.TempDir())
	if !empty.WaitAll(time.Second) {
		t.Fatal("WaitAll on an empty manager must return true at once")
	}
}
