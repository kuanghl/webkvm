/**
 * zmodemClient — the browser half of a terminal ZMODEM transfer.
 *
 * The protocol lives entirely on the server; this owns what the terminal
 * chrome needs: the paths of a download, the picked files of an upload,
 * the live job (whose byte counters the shared queue store already
 * tracks, so no second poller is started here) and the one-shot hand-off
 * to the browser's downloader.
 *
 * The upload is two requests by design: the batch is POSTed to this host
 * first (a telnet line cannot be interrupted to negotiate while the user
 * types), and a second frame on the live terminal says "now send what you
 * kept". A stage the terminal never claimed is the server's to expire.
 *
 * `send` is bound to whichever WebSocket is current, because the panel
 * reconnects and a stale socket would swallow the request.
 */
import { api } from '$lib/stores/auth.svelte.js';
import { files } from '$lib/stores/files.svelte.js';
import { t } from '$lib/i18n.svelte.js';
import { formatBytes } from '$lib/utils/format.js';

// Matches sshmachines.MaxStagedBatch: one rz batch, because every file
// costs a handshake of its own on a stop-and-wait line.
const MAX_BATCH = 32;

export class ZmodemClient {
  open = $state(false);
  // 'down' pulls paths off the remote with sz, 'up' pushes the picked
  // files to it with rz.
  mode = $state('down');
  input = $state('');
  recursive = $state(false);
  // Files carry a Blob inside, so they stay out of the deep proxy: a
  // proxied File is no longer a File to FormData.
  picked = $state.raw([]);
  stage = $state(null);
  // Percent of the POST to this host, before the line is even touched.
  staged = $state(0);
  error = $state('');
  job = $state('');
  notice = $state('');
  // Which direction the live session belongs to, so the frames can be
  // read without asking the user what they pressed.
  activeDir = $state('down');
  // '' (nothing to say) | running | done | error
  kind = $state('');

  constructor(machineId) {
    this.machineId = machineId;
    this.send = null;
    // Whether the current session reached the line. Not rendered, so it
    // stays out of the reactive graph on purpose.
    this.wasArmed = false;
  }

  bind(send) {
    this.send = send;
    // A reconnect drops the server-side session with the socket, so a
    // banner claiming a transfer is running would be a lie.
    if (!send && this.kind === 'running') {
      this.kind = 'error';
      this.notice = t('terminal.zmodemInterrupted');
      this.job = '';
    }
  }

  get paths() {
    return this.input
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
  }

  get running() {
    return this.kind === 'running';
  }

  get pickedBytes() {
    return this.picked.reduce((n, f) => n + (Number(f.size) || 0), 0);
  }

  get stagedLabel() {
    return `${this.picked.length}/${MAX_BATCH}`;
  }

  /**
   * Take a FileList, or the flattened items of a dropped folder. `name`
   * may keep its folders: rz receives one flat name per file and the
   * server decides the final one, so its reply is what the user should
   * see, not what they picked.
   */
  pick(list) {
    const items = Array.from(list || [])
      .map((it) => (it && it.file ? it : { file: it, name: it?.name, size: it?.size }))
      .filter((it) => it.file);
    this.error = '';
    if (!items.length) {
      this.picked = [];
      this.stage = null;
      return;
    }
    if (items.length > MAX_BATCH) {
      this.picked = [];
      this.error = t('terminal.zmodemTooManyFiles', { max: MAX_BATCH, count: items.length });
      return;
    }
    this.picked = items.map((it) => ({
      file: it.file,
      // relPath is what a dropped folder carries; keeping it here means the
      // list shows where a file came from, and the server is the one that
      // flattens it into the name rz will receive.
      name: it.name || it.relPath || it.file.name,
      size: Number(it.size ?? it.file.size) || 0,
    }));
    // Whatever was held on the host belongs to the previous pick.
    this.stage = null;
    this.staged = 0;
  }

  clearPicked() {
    this.picked = [];
    this.stage = null;
    this.staged = 0;
    this.error = '';
  }

  setMode(mode) {
    if (this.mode === mode) return;
    this.mode = mode;
    this.error = '';
  }

  start() {
    if (!this.send) {
      this.error = t('terminal.zmodemOffline');
      return;
    }
    if (this.mode === 'up') return this.startUp();
    const paths = this.paths;
    if (!paths.length) {
      this.error = t('terminal.zmodemNeedPath');
      return;
    }
    this.error = '';
    this.notice = '';
    this.open = false;
    this.wasArmed = false;
    this.activeDir = 'down';
    this.kind = 'running';
    this.notice = t('terminal.zmodemWaiting');
    // The queue row is already on the server's event stream; subscribing
    // here is what makes the banner's byte counter live without a second
    // socket.
    files.ensureSSE();
    this.send({ type: 'zmodem', paths, recursive: this.recursive });
  }

  /**
   * Upload: post the batch here, then arm the terminal. A stage the
   * terminal refused to take (the line was busy) is still on the host, so
   * pressing the button again sends the frame instead of the bytes twice.
   */
  async startUp() {
    if (!this.picked.length) {
      this.error = t('terminal.zmodemNeedFiles');
      return;
    }
    this.error = '';
    this.open = false;
    this.wasArmed = false;
    this.activeDir = 'up';
    this.kind = 'running';
    this.notice = t('terminal.zmodemStaging');
    files.ensureSSE();
    const key = this.pickKey;
    if (this.stage?.upload_id && this.stage.key === key) {
      this.notice = t('terminal.zmodemWaiting');
      this.send({ type: 'zmodem', dir: 'up', upload_id: this.stage.upload_id });
      return;
    }
    this.staged = 0;
    let res;
    try {
      res = await api.machineZmodemStage(this.machineId, this.picked, (p) => (this.staged = p));
    } catch (e) {
      this.kind = 'error';
      this.staged = 0;
      this.notice = e?.message || t('terminal.zmodemFailed');
      return;
    }
    if (!res?.upload_id) {
      this.kind = 'error';
      this.notice = t('terminal.zmodemFailed');
      return;
    }
    this.stage = { ...res, key };
    this.staged = 100;
    this.notice = t('terminal.zmodemWaiting');
    this.send({ type: 'zmodem', dir: 'up', upload_id: res.upload_id });
  }

  /** What a pick is, so an unchanged retry is recognisable. */
  get pickKey() {
    return this.picked.map((f) => `${f.name}:${f.size}`).join('|');
  }

  cancel() {
    if (!this.send) return;
    this.notice = t('terminal.zmodemWaiting');
    this.send({ type: 'zmodem', action: 'cancel' });
  }

  dismiss() {
    this.kind = '';
    this.notice = '';
    this.job = '';
  }

  /** One `{"type":"zmodem"}` frame from the terminal socket. */
  frame(body) {
    if (body.dir) this.activeDir = body.dir;
    switch (body.event) {
      case 'armed':
        this.job = body.job_id || '';
        this.wasArmed = true;
        this.kind = 'running';
        this.notice = t('terminal.zmodemWaiting');
        break;
      case 'done': {
        // Only a download has files to fetch: an upload's names belong to
        // the remote, and there is no result behind them here.
        const names = Array.isArray(body.files) ? body.files : [];
        this.kind = 'done';
        if (names.length) {
          this.notice = t('terminal.zmodemDone', {
            count: names.length,
            names: names.slice(0, 3).join(', '),
          });
          this.fetch(names);
        } else if (this.activeDir === 'up') {
          const count = this.stage?.files?.length || this.picked.length || 1;
          this.notice = t('terminal.zmodemUploaded', { count });
        } else {
          this.notice = t('terminal.zmodemDoneEmpty');
        }
        this.job = '';
        break;
      }
      case 'disarmed':
        // Nothing ever answered: lrzsz is the usual reason, and the
        // terminal is already back to a normal shell.
        this.kind = 'error';
        this.notice = t('terminal.zmodemNoLrzsz');
        this.job = '';
        break;
      case 'canceled':
        this.kind = 'error';
        this.notice = t('terminal.zmodemCanceled');
        this.job = '';
        break;
      default: {
        const detail = body.error || body.detail || '';
        this.kind = 'error';
        this.notice = detail || t('terminal.zmodemFailed');
        if (!body.job_id) this.error = this.notice;
        this.job = '';
        break;
      }
    }
    // A session that reached the line spent its stage either way: the
    // server removed those bytes with the claim, so the next upload starts
    // from a fresh pick. A refusal that never got there left the batch on
    // the host on purpose, and retrying must not post gigabytes twice.
    if (this.activeDir === 'up' && this.wasArmed && body.event !== 'armed') this.clearPicked();
  }

  /**
   * The queue row of the live job. Reading it from the shared store keeps
   * the banner's counters in step with the sidebar without a second
   * subscription.
   */
  get row() {
    return this.job ? files.jobs.get(this.job) || null : null;
  }

  get sizeLabel() {
    const done = Number(this.row?.done) || 0;
    if (!done) return '';
    const total = Number(this.row?.total) || 0;
    return total ? `${formatBytes(done)} / ${formatBytes(total)}` : formatBytes(done);
  }

  get fileLabel() {
    const total = Number(this.row?.files_total) || 0;
    const done = Number(this.row?.files_done) || 0;
    return total > 1 ? `${done}/${total}` : '';
  }

  get currentLabel() {
    return this.row?.current_path || '';
  }

  /**
   * Pull the finished job through the same result endpoint the Files
   * queue uses: a lone file arrives as itself, a batch as one tar.gz.
   */
  fetch(names) {
    if (!this.machineId || !this.job) return;
    const url = api.machineTransferResultUrl(this.machineId, this.job);
    const flat = names.length === 1 ? names[0].split('/').pop() : '';
    const link = document.createElement('a');
    link.href = url;
    link.rel = 'noopener';
    if (flat) link.download = flat;
    document.body.appendChild(link);
    link.click();
    link.remove();
  }
}
