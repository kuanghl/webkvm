package api

import "time"

// Startup runs the one-shot housekeeping that must happen before the
// first request is served. It returns how many orphan files were
// removed so main can log it; a failure is never fatal (the directories
// are only staging areas).
//
// Sweeping belongs here and not on shutdown: CancelAll only flips
// statuses, so a worker can still be writing when Close() returns, and
// deleting underneath it would corrupt a tree nobody is serving anyway.
func (h *Handler) Startup() (int, error) {
	if h.transfers == nil {
		return 0, nil
	}
	return h.transfers.SweepOrphans()
}

// Close releases the background workers NewRouter starts, so an
// orderly shutdown (and the goleak checks in the API tests) does not
// leave them running. Safe to call more than once.
//
// The order matters: cancel first, then wait for the workers to return,
// and only then close the pooled sessions. A worker queued on the
// semaphore notices the cancel only when it finally runs — and if the
// registry were already closed, that run would dial a fresh session
// that nothing closes anymore. Closing the registry after the wait also
// unwedges any worker stuck on a dead socket: its read errors out and
// the worker returns.
func (h *Handler) Close() {
	if h.transfers != nil {
		h.transfers.CancelAll()
	}
	if h.sftpSessions != nil {
		h.sftpSessions.Stop()
	}
	if h.transfers != nil {
		// Bounded: a worker wedged past the deadline dies with the
		// process, and SweepOrphans picks up anything it stranded.
		_ = h.transfers.WaitAll(10 * time.Second)
	}
	if h.sftpSessions != nil {
		h.sftpSessions.CloseAll()
	}
	if h.transfers != nil {
		// Batches nobody ever armed for are not owned by any worker, so
		// they can go here rather than waiting for the next startup
		// sweep — a stopped service should not sit on someone's upload
		// for days. Claimed ones are not in that set: their job removes
		// its own directory when it settles.
		h.transfers.DropStagedUploads()
	}
}
