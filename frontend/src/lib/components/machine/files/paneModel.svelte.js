/**
 * paneModel.svelte.js — the state behind one file pane.
 *
 * Both panes, the sidebar's browse column and (later) zmodem's target
 * are the same shape, so the model is a class instead of prop-drilling
 * twelve fields through four components.
 *
 * Two invariants worth knowing before editing:
 *  - every list response carries a version, and a stale reply is thrown
 *    away. Remote filesystems answer in 30 ms or 3 s with no warning, so
 *    "click two folders fast" must not paint the first answer last.
 *  - a pane's `path` is always absolute; the remote home is only ever
 *    learned from the first response (the server resolves it via Getwd).
 */

import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import { api } from '$lib/stores/auth.svelte.js';
import { PAGE_SIZE, dirOf, sortEntries, typeAheadIndex } from '$lib/utils/fileTransfer.js';

const TYPEAHEAD_MS = 600;

// The server's per-request cap (sftpSizesMaxPaths): one request, 128 walks.
const SIZES_MAX_PATHS = 128;

export class Pane {
  machineId = $state('');
  path = $state('');
  home = $state('');
  entries = $state([]);
  loading = $state(false);
  error = $state('');
  // A 409 host-key problem is the pane's own state, not an error string:
  // it arrives from `load`, `refresh`, `back` and the mutating actions
  // alike, and every one of them has to raise the trust dialog instead of
  // printing "hostkey_untrusted" as if the user had done something wrong.
  hostKey = $state(null);
  selected = $state(new SvelteSet());
  active = $state(-1);
  shown = $state(PAGE_SIZE);
  sort = $state('name');
  // Folder sizes arrive in a second bounded request (a listing cannot
  // carry them). Maps entry path → { size, truncated } or { error }.
  // A path absent from the map is still pending ("…").
  dirSizes = $state(new SvelteMap());
  dirsFirst = $state(true);
  showHidden = $state(false);
  // Sorting inputs are memoized behind this counter. Every write that
  // can change the row order bumps it — dropping a bump would freeze the
  // list silently, since the cached branch reads no other $state.
  rev = $state(0);
  // The history is $state because `canBack`/`canForward` read it and the
  // two breadcrumb arrows are the only thing that renders from them: a
  // plain field left both arrows frozen at whatever they were when the
  // pane mounted. `idx` rides inside the object so one write republishes
  // both.
  hist = $state({ stack: [], idx: -1 });

  constructor({ side = 'left', machineId = '' } = {}) {
    this.side = side;
    this.machineId = machineId;
    this.version = 0;
    this._sorted = null;
    this._ta = { buf: '', timer: null };
  }

  get busy() {
    return this.loading;
  }

  get sorted() {
    const rev = this.rev;
    if (this._sorted && this._sorted.rev === rev) return this._sorted.list;
    const list = sortEntries(this.entries, {
      sort: this.sort,
      dirsFirst: this.dirsFirst,
      showHidden: this.showHidden,
    });
    this._sorted = { rev, list };
    return list;
  }

  get visible() {
    return this.sorted.slice(0, this.shown);
  }

  get hasMore() {
    return this.sorted.length > this.shown;
  }

  get totalVisible() {
    return this.sorted.length;
  }

  get selectedEntries() {
    if (this.selected.size) return this.sorted.filter((e) => this.selected.has(e.path));
    const one = this.sorted[this.active];
    return one ? [one] : [];
  }

  get selectedCount() {
    return this.selected.size || (this.sorted[this.active] ? 1 : 0);
  }

  // ---- machine ----
  setMachine(id) {
    if (id === this.machineId) return;
    this.machineId = id;
    this.version++;
    this.path = '';
    this.home = '';
    this.entries = [];
    this.error = '';
    this.hostKey = null;
    this.loading = false;
    this.shown = PAGE_SIZE;
    this.dirSizes = new SvelteMap();
    this.rev++;
    this.selected.clear();
    this.active = -1;
    this.hist = { stack: [], idx: -1 };
  }

  // ---- navigation ----
  async load(target = '', { push = true } = {}) {
    if (!this.machineId) return;
    const v = ++this.version;
    // Every navigation/refresh re-asks for sizes: "…" is the honest
    // pending state until the bounded probe answers.
    this.dirSizes = new SvelteMap();
    this.loading = true;
    this.error = '';
    try {
      const res = await api.machineSFTPList(this.machineId, target);
      if (v !== this.version) return;
      this.path = res?.path || '/';
      if (!this.home) this.home = this.path;
      this.entries = Array.isArray(res?.entries) ? res.entries : [];
      this.rev++;
      this.selected.clear();
      this.shown = PAGE_SIZE;
      this.active = this.entries.length ? 0 : -1;
      if (push) this._pushHistory(this.path);
      this._fetchDirSizes(v);
    } catch (e) {
      if (v !== this.version) return;
      const hk = hostKeyOf(e);
      if (hk) this.hostKey = hk;
      else this.error = messageOf(e);
    } finally {
      if (v === this.version) this.loading = false;
    }
  }

  refresh() {
    return this.load(this.path);
  }

  /**
   * One bounded request for every directory in the listing. Version-
   * guarded like the listing itself: a reply that loses the race is
   * dropped. Paths past the server's per-request cap are parked as an
   * error here (the cell renders "—") — the cap is shared with the
   * request, and asking again would just hit the same wall.
   */
  _fetchDirSizes(v) {
    const dirs = this.entries.filter((e) => e?.is_dir).map((e) => e.path);
    const asked = dirs.slice(0, SIZES_MAX_PATHS);
    const map = new SvelteMap(this.dirSizes);
    for (const p of dirs.slice(SIZES_MAX_PATHS)) map.set(p, { error: 'capped' });
    if (asked.length) this.dirSizes = map;
    if (!asked.length) return;
    const machineId = this.machineId;
    api
      .machineSFTPSizes(machineId, asked)
      .then((res) => {
        if (v !== this.version) return;
        const next = new SvelteMap(this.dirSizes);
        for (const s of res?.sizes || []) {
          if (!s?.path) continue;
          next.set(
            s.path,
            s.error ? { error: s.error } : { size: s.size || 0, truncated: !!s.truncated }
          );
        }
        this.dirSizes = next;
      })
      .catch(() => {
        /* a failed probe is a "—" in the cell, not a row error: the
           listing (the same 409 host-key problem if that is what it is)
           already surfaces the real one */
      });
  }

  open(entry) {
    if (!entry) return;
    if (entry.is_dir) return this.load(entry.path);
  }

  up() {
    const parent = dirOf(this.path || '/');
    if (parent !== this.path) return this.load(parent);
  }

  goHome() {
    if (this.home) return this.load(this.home);
  }

  get canBack() {
    return this.hist.idx > 0;
  }

  get canForward() {
    return this.hist.idx >= 0 && this.hist.idx < this.hist.stack.length - 1;
  }

  back() {
    if (!this.canBack) return;
    this.hist.idx -= 1;
    return this.load(this.hist.stack[this.hist.idx], { push: false });
  }

  forward() {
    if (!this.canForward) return;
    this.hist.idx += 1;
    return this.load(this.hist.stack[this.hist.idx], { push: false });
  }

  _pushHistory(p) {
    const stack = this.hist.stack.slice(0, this.hist.idx + 1);
    if (stack[stack.length - 1] !== p) stack.push(p);
    this.hist = { stack, idx: stack.length - 1 };
  }

  // ---- selection ----
  toggle(entry) {
    if (!entry) return;
    const next = new SvelteSet(this.selected);
    if (next.has(entry.path)) next.delete(entry.path);
    else next.add(entry.path);
    this.selected = next;
  }

  selectOnly(entry) {
    this.selected = new SvelteSet(entry ? [entry.path] : []);
  }

  selectAll() {
    this.selected = new SvelteSet(this.sorted.map((e) => e.path));
  }

  clearSelection() {
    this.selected = new SvelteSet();
  }

  // ---- view options ----
  setSort(sort) {
    if (sort === this.sort) return;
    this.sort = sort;
    this.rev++;
  }

  toggleHidden() {
    this.showHidden = !this.showHidden;
    this.rev++;
  }

  toggleDirsFirst() {
    this.dirsFirst = !this.dirsFirst;
    this.rev++;
  }

  loadMore() {
    this.shown += PAGE_SIZE;
  }

  // ---- keyboard ----
  moveActive(delta) {
    const list = this.sorted;
    if (!list.length) return;
    this.active = Math.min(list.length - 1, Math.max(0, this.active + delta));
    if (this.active >= this.shown) this.shown += PAGE_SIZE;
  }

  typeAhead(ch) {
    const list = this.sorted;
    if (!list.length) return;
    clearTimeout(this._ta.timer);
    this._ta.buf += ch;
    this._ta.timer = setTimeout(() => (this._ta.buf = ''), TYPEAHEAD_MS);
    const idx = typeAheadIndex(
      list.map((e) => e.name),
      this._ta.buf,
      this.active
    );
    if (idx >= 0) this.active = idx;
  }

  dispose() {
    clearTimeout(this._ta.timer);
    this._ta.buf = '';
  }
}

export function messageOf(e) {
  return e?.message ? String(e.message) : String(e);
}

/**
 * The host-key fields of a 409, or null for any other failure. A changed
 * key is the same dialog with a "re-pin" wording, so it travels here too.
 */
export function hostKeyOf(e) {
  const d = e?.data || {};
  if (e?.status !== 409 || (d.error !== 'hostkey_untrusted' && d.error !== 'hostkey_changed'))
    return null;
  return {
    presented: d.fingerprint || '',
    pinned: d.pinned || '',
    changed: d.error === 'hostkey_changed',
  };
}
