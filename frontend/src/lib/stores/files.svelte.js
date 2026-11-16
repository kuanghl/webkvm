/**
 * files.svelte.js — the shared state behind the dual-pane workspace.
 *
 * One singleton on purpose: the same job list has to be visible from the
 * two panes, the sidebar and (later) the terminal's zmodem button, and
 * the SSE channel is the only source of progress. Nothing here polls —
 * `ensureSSE()` subscribes to the existing "transfer.progress" stream
 * once, every view reads the resulting Map, and a reconnect re-reads the
 * queue once to heal the frames the dead socket missed.
 */

import { api } from './auth.svelte.js';
import { events } from './events.svelte.js';
import { safeStorage } from '$lib/utils/storage.js';
import { openAttachment } from '$lib/utils/attachment.js';
import { isTerminalStatus, carriesFiles } from '$lib/utils/fileTransfer.js';

const NS = 'webkvm.files.';
const clamp = (n, lo, hi) => Math.min(hi, Math.max(lo, n));

class FilesStore {
  machines = $state([]);
  machinesLoaded = $state(false);
  jobs = $state(new Map());
  queueLoading = $state(false);
  queueError = $state('');
  splitPct = $state(clamp(Number(safeStorage.getItem(NS + 'split.v1')) || 50, 20, 80));

  constructor() {
    this._off = null;
    this._onOpen = null;
    // Jobs whose file the browser is still owed. Only an explicit
    // download puts an id here, which is what keeps a queue that a
    // reconnect re-reads from replaying someone's old files.
    this._toSave = new Set();
    // Tombstones for rows the user deleted. A delete that races a late
    // SSE frame (the done broadcast in flight while the user clicks
    // trash) or a reconnect resync would otherwise resurrect the row
    // from nowhere — the id is a uuid that never comes back, so
    // forgetting it for the rest of the session costs nothing.
    this.removed = new Set();
  }

  // ---- machines ----
  async loadMachines(force = false) {
    if (this.machinesLoaded && !force) return this.machines;
    try {
      const list = await api.listMachines();
      this.machines = Array.isArray(list) ? list : [];
      this.machinesLoaded = true;
    } catch {
      this.machines = [];
    }
    return this.machines;
  }

  get fileMachines() {
    // The file browser speaks SFTP or the host's own filesystem; a
    // telnet/serial machine has nothing to list. The built-in host card
    // is only ever in this list for admins, because the server adds it
    // to the registry response for that role alone.
    return this.machines.filter(carriesFiles);
  }

  machine(id) {
    return this.machines.find((m) => m.id === id) || null;
  }

  machineName(id) {
    return this.machine(id)?.name || id || '';
  }

  // ---- queue ----
  // Idempotent: every consumer calls it on mount and none owns the socket.
  ensureSSE() {
    if (this._off) return;
    // The progress stream is the only writer here, and a socket that
    // dropped and came back lost every frame sent in between. Re-reading
    // the queue on each open heals those rows; no poller is added.
    this._onOpen = events.onOpen(() => this.resync());
    this._off = events.onTransferProgress((env) => this.applyEvent(env?.data));
  }

  disposeSSE() {
    if (this._onOpen) {
      this._onOpen();
      this._onOpen = null;
    }
    if (this._off) {
      this._off();
      this._off = null;
    }
  }

  // Silent by design: this runs on a reconnect the user never asked for,
  // so it must not flip the queue panel to loading or paint an error
  // banner over rows that were fine.
  async resync() {
    try {
      const rows = await api.listAllMachineTransfers();
      for (const job of rows || []) this.upsert(job);
      this.prune(rows || []);
    } catch {
      /* the next frame, or the next reconnect, heals it */
    }
  }

  /** Snapshot-shaped job (POST/GET response) — replaces the record. */
  upsert(job) {
    if (!job?.id) return job;
    // The user deleted this row: a frame in flight or a resync that
    // still carries it must not bring it back.
    if (this.removed.has(job.id)) return null;
    const prev = this.jobs.get(job.id);
    // A finished job never walks back to running: the list refresh and a
    // late SSE frame race, and "done → running" would un-cancel a row.
    if (prev && isTerminalStatus(prev.status) && !isTerminalStatus(job.status)) return prev;
    const next = mergeKnown(prev, normalizeRates(job));
    this.jobs.set(job.id, next);
    if (next.status === 'done') this.deliver(next);
    else if (isTerminalStatus(next.status)) this._toSave.delete(next.id);
    return next;
  }

  /**
   * Register a job the browser is owed a file for. A download button that
   * only produced a queue row the user has to click again is the bug this
   * closes: the file arrives when the job says it is done.
   */
  trackDownload(job) {
    if (job?.id) this._toSave.add(job.id);
    return this.upsert(job);
  }

  deliver(job) {
    if (!this._toSave.has(job.id)) return;
    this._toSave.delete(job.id);
    openAttachment(api.machineTransferResultUrl(job.machine_id, job.id));
  }

  /** Progress frame: `job_id` keys the record, and it carries no error text. */
  applyEvent(d) {
    if (!d?.job_id) return;
    this.upsert({ ...d, id: d.job_id });
  }

  /** One request for every machine's queue (the sidebar). */
  async refreshQueue(machineIds) {
    this.queueLoading = true;
    this.queueError = '';
    try {
      const rows = await api.listAllMachineTransfers(machineIds);
      for (const job of rows || []) this.upsert(job);
      this.prune(rows || []);
    } catch (e) {
      this.queueError = e?.message || String(e);
    } finally {
      this.queueLoading = false;
    }
  }

  /**
   * Drop rows the server no longer reports. The server caps its own list
   * (50 rows, running jobs excluded), so forgetting the rest is exactly
   * what keeps a finished job from haunting the queue forever.
   */
  prune(alive) {
    const keep = new Set(alive.map((j) => j.id));
    for (const id of [...this.jobs.keys()]) {
      const job = this.jobs.get(id);
      if (!keep.has(id) && job?.status !== 'running') {
        this.jobs.delete(id);
        this._toSave.delete(id);
      }
    }
  }

  byMachine(id) {
    const out = [];
    for (const job of this.jobs.values()) {
      if (job.machine_id === id || job.dst_machine_id === id) out.push(job);
    }
    return out.sort(byNewest);
  }

  get allJobs() {
    return [...this.jobs.values()].sort(byNewest);
  }

  get activeCount() {
    let n = 0;
    for (const job of this.jobs.values()) if (job.status === 'running') n++;
    return n;
  }

  async cancel(job) {
    try {
      await api.cancelMachineTransferJob(job.id);
      this.upsert({ id: job.id, status: 'canceled' });
      return true;
    } catch {
      return false;
    }
  }

  // Pause parks the worker between 64 KiB chunks; the job keeps its
  // lease, so the row (and its resume) survive the park. Resume is the
  // only way out besides cancel — and cancel works from parked.
  async pauseJob(job) {
    try {
      await api.pauseMachineTransferJob(job.id);
      this.upsert({ id: job.id, status: 'paused' });
      return true;
    } catch {
      return false;
    }
  }

  async resumeJob(job) {
    try {
      await api.resumeMachineTransferJob(job.id);
      this.upsert({ id: job.id, status: 'running' });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Delete one row. Optimistic on purpose: the row leaves the screen
   * the moment the user asks, and the server call only decides whether
   * the deletion outlives a reconnect. A 404 means the server already
   * forgot the job (evicted past the cap) — the local row was the last
   * trace, and dropping it is the right answer, not an error. Any other
   * failure (the job turned out to be running) rolls the tombstone back
   * and rethrows, so the caller can say why and the next frame brings
   * the row back.
   */
  async remove(job) {
    if (!job?.id) return true;
    this.removed.add(job.id);
    this.jobs.delete(job.id);
    this._toSave.delete(job.id);
    try {
      await api.removeMachineTransferJob(job.id);
      return true;
    } catch (e) {
      if (e?.status === 404) return true;
      this.removed.delete(job.id);
      if (!this.jobs.has(job.id)) this.jobs.set(job.id, job);
      throw e;
    }
  }

  /**
   * The queue's "clear completed": every terminal row (done, error,
   * canceled) goes — locally first, then one server pass. Running and
   * paused rows are not part of it: the server skips them too, so a
   * clear can never stop a live transfer. On failure the tombstones are
   * rolled back, the rows are restored exactly as they were (a done
   * download still owes the browser its file) and the error is rethrown
   * so the caller can say why.
   */
  async clearFinished() {
    const finished = [...this.jobs.values()].filter((j) => isTerminalStatus(j.status));
    if (!finished.length) return 0;
    for (const j of finished) {
      this.removed.add(j.id);
      this.jobs.delete(j.id);
      this._toSave.delete(j.id);
    }
    try {
      await api.clearMachineTransfers();
      return finished.length;
    } catch (e) {
      for (const j of finished) {
        this.removed.delete(j.id);
        if (!this.jobs.has(j.id)) this.jobs.set(j.id, j);
      }
      throw e;
    }
  }

  // ---- preferences ----
  // Layout values are $state mirrors of localStorage: a getter that only
  // read safeStorage could not tell a dragging handle from a resize
  // that happened in another tab, so nothing re-rendered mid-drag.
  pref(key, fallback) {
    const raw = safeStorage.getItem(NS + key);
    if (raw === null) return fallback;
    try {
      return JSON.parse(raw);
    } catch {
      return raw;
    }
  }

  setPref(key, value) {
    safeStorage.setItem(NS + key, JSON.stringify(value));
  }

  get split() {
    return this.splitPct;
  }

  setSplit(pct) {
    this.splitPct = clamp(Math.round(Number(pct) || 50), 20, 80);
    this.setPref('split.v1', this.splitPct);
  }
}

// Undefined fields in a partial must not blank a value the snapshot set
// (a progress frame has no `error`, a cancel response has no `done`).
function mergeKnown(prev, next) {
  if (!prev) return next;
  const out = { ...prev };
  for (const [k, v] of Object.entries(next)) if (v !== undefined) out[k] = v;
  return out;
}

// The server omits speed and eta once a job stops; a stale number would
// otherwise keep counting down on a finished row.
function normalizeRates(job) {
  return { ...job, speed_bps: job.speed_bps ?? 0, eta_seconds: job.eta_seconds ?? 0 };
}

function byNewest(a, b) {
  return String(b.started_at || '').localeCompare(String(a.started_at || ''));
}

export const files = new FilesStore();
