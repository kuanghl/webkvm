/**
 * Pure helpers behind the file workspace in the machine detail tab.
 *
 * Everything here is deliberately free of Svelte and of the network: the
 * copy/plan decisions are the part users judge as "correct or it ate my
 * file", so they are unit-tested in isolation instead of through a
 * mounted component with a stubbed API.
 */

// Rendered rows per page. A remote home directory can hold tens of
// thousands of entries, and the project has no virtual list — building
// the DOM in slices keeps first paint bounded.
export const PAGE_SIZE = 500;

/**
 * Whether a machine record has a browsable filesystem.
 *
 * This is the client half of `Machine.CarriesFiles()` on the server: an ssh
 * machine answers over SFTP, the built-in host card over an in-process
 * session, nothing else has a plane to list. Both halves have to agree, or
 * a tab opens on a route that answers 400.
 */
export function carriesFiles(m) {
  return !!m && (m.protocol === 'ssh' || (m.builtin === true && m.protocol === 'local'));
}

// Payload type for a drag that starts and ends inside the workspace (as
// opposed to a drag from the operating system, which has no type at all).
export const DRAG_TYPE = 'application/x-webkvm-file';

const TERMINAL = new Set(['done', 'error', 'canceled']);

/** A finished job must never be walked back to "running" by a late event. */
export function isTerminalStatus(status) {
  return TERMINAL.has(status);
}

export function joinPath(dir, name) {
  if (!dir || dir === '/') return '/' + name;
  return dir + '/' + name;
}

export function dirOf(p) {
  const i = (p || '/').lastIndexOf('/');
  return i <= 0 ? '/' : p.slice(0, i);
}

export function basename(p) {
  const i = (p || '').lastIndexOf('/');
  return i < 0 ? p : p.slice(i + 1);
}

/** Destination paths one copy/upload would occupy, for one batch stat. */
export function dstPathsFor(entries, dstDir) {
  return entries.map((e) => joinPath(dstDir, e.name));
}

/** Which of the batch-stat results are already taken (dangling links count). */
export function existingOf(stats) {
  return (stats || []).filter((s) => s && s.exists).map((s) => s.path);
}

/**
 * "a.txt" → "a (1).txt", "app" → "app (1)", ".bashrc" → ".bashrc (1)".
 *
 * `taken` is owned by the caller and mutated with each result, so three
 * colliding files in one batch become (1), (2), (3) — re-deriving from
 * the pre-batch listing alone would hand out "a (1).txt" three times.
 */
export function autoRename(name, taken = new Set()) {
  if (!taken.has(name)) {
    taken.add(name);
    return name;
  }
  const dot = name.lastIndexOf('.');
  const stem = dot > 0 ? name.slice(0, dot) : name;
  const ext = dot > 0 ? name.slice(dot) : '';
  for (let n = 1; n < 1000; n++) {
    const candidate = `${stem} (${n})${ext}`;
    if (!taken.has(candidate)) {
      taken.add(candidate);
      return candidate;
    }
  }
  const fallback = `${stem} (${Date.now()})${ext}`;
  taken.add(fallback);
  return fallback;
}

/**
 * Copy requests for one batch of source entries. The server resolves
 * per-file conflicts under `rename`, so the client only decides the
 * policy and keeps one job per selected entry (a directory expands
 * server-side into a single aggregated queue row).
 */
export function copyRequests(entries, dstMachineId, dstDir, policy) {
  return entries.map((e) => ({
    // The server decodes `remote_path` (not `src_path`): a wrong key
    // arrives as "" and the job would plan from the remote root.
    remote_path: e.path,
    dst_machine_id: dstMachineId,
    dst_dir: dstDir,
    recursive: !!e.is_dir,
    overwrite: policy,
  }));
}

/**
 * Serialize a cross-pane drag. `is_dir` rides along because the target
 * pane cannot re-list the source, and a directory copied as a file would
 * arrive as an empty one.
 */
export function dropPayload(machineId, dir, entries) {
  return JSON.stringify({
    m: machineId,
    d: dir,
    items: entries.map((e) => ({ path: e.path, is_dir: !!e.is_dir })),
  });
}

export function entriesFromPayload(payload) {
  return (payload?.items || []).map((i) => ({
    path: i.path,
    name: basename(i.path),
    is_dir: !!i.is_dir,
  }));
}

/**
 * Plan a drop/upload into `destDir`.
 *
 * `items` come from flattenDrop: `{ file, relPath }` for files plus
 * `{ dir: relPath }` for every directory visited, so an empty folder
 * still arrives. The mkdir list is returned separately because the
 * server's upload job fails when the remote parent is missing.
 */
export function planUploads(items, destDir, taken = new Set()) {
  const dirs = new Set();
  const files = [];
  for (const it of items || []) {
    if (it.dir) {
      addAncestors(dirs, destDir, it.dir);
      continue;
    }
    if (!it.file || !it.relPath) continue;
    const relDir = dirOfRelative(it.relPath);
    const parent = relDir === '.' ? destDir : joinPath(destDir, relDir);
    if (relDir !== '.') addAncestors(dirs, destDir, relDir);
    files.push({
      file: it.file,
      remotePath: joinPath(parent, autoRename(basename(it.relPath), taken)),
    });
  }
  // Parents before children: MkdirAll tolerates either order, but a
  // deterministic list makes the failure message reproducible.
  return { dirs: [...dirs].sort(byDepth), files };
}

function dirOfRelative(rel) {
  const i = rel.lastIndexOf('/');
  return i < 0 ? '.' : rel.slice(0, i);
}

function addAncestors(set, destDir, rel) {
  let acc = '';
  for (const part of rel.split('/')) {
    if (!part) continue;
    acc = acc ? `${acc}/${part}` : part;
    set.add(joinPath(destDir, acc));
  }
}

function byDepth(a, b) {
  const d = a.split('/').length - b.split('/').length;
  return d !== 0 ? d : a.localeCompare(b);
}

export function sortEntries(entries, { sort = 'name', dirsFirst = true, showHidden = false } = {}) {
  const list = (entries || []).filter((e) => e && (showHidden || !String(e.name).startsWith('.')));
  const cmp = (a, b) => {
    if (dirsFirst && a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    if (sort === 'size')
      return (a.size || 0) - (b.size || 0) || String(a.name).localeCompare(String(b.name));
    if (sort === 'time')
      return (b.mod_time || 0) - (a.mod_time || 0) || String(a.name).localeCompare(String(b.name));
    return String(a.name).localeCompare(String(b.name), undefined, {
      numeric: true,
      sensitivity: 'base',
    });
  };
  return [...list].sort(cmp);
}

/**
 * First-row index for a type-ahead buffer (600 ms of keystrokes).
 *
 * Search starts after `from` so repeated presses walk the list, then
 * wraps once; a buffer that matches nothing leaves the selection alone.
 */
export function typeAheadIndex(names, buffer, from = -1) {
  const len = (names || []).length;
  const needle = String(buffer).toLowerCase();
  if (!len || !needle) return -1;
  for (let step = 1; step <= len; step++) {
    const idx = (from + step + len) % len;
    if (
      String(names[idx] ?? '')
        .toLowerCase()
        .startsWith(needle)
    )
      return idx;
  }
  return -1;
}

/**
 * Percentage for a queue row. A job without a known total (a tree still
 * being walked, an in-flight file count) has no honest bar, so it
 * reports null and the caller shows an indeterminate state.
 */
export function jobPercent(job) {
  if (!job) return null;
  if (job.status === 'done') return 100;
  const total = job.total || 0;
  if (!total) return null;
  return Math.max(0, Math.min(99, Math.round(((job.done || 0) / total) * 100)));
}

/** Rows for a tree job: `files_done / files_total`, single file = 1. */
export function jobFileCount(job) {
  const total = job?.files_total ?? 1;
  return { done: job?.files_done ?? 0, total };
}

/**
 * Flatten a `drop` event's DataTransfer into `{ file, relPath }` items.
 *
 * `readEntries()` returns at most 100 entries per call and signals the
 * end of a directory with an empty array, not with an error — one call
 * silently loses everything past the hundredth child.
 */
export async function flattenDrop(dataTransfer) {
  const out = [];
  const items = Array.from(dataTransfer?.items || []);
  const entries = items
    .map((i) => (typeof i.webkitGetAsEntry === 'function' ? i.webkitGetAsEntry() : null))
    .filter(Boolean);
  if (entries.length) {
    for (const entry of entries) await walkEntry(entry, '', out);
    return out;
  }
  return Array.from(dataTransfer?.files || []).map((file) => ({ file, relPath: file.name }));
}

async function walkEntry(entry, prefix, out) {
  const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
  if (entry.isFile) {
    out.push({ file: await fileFromEntry(entry), relPath: rel });
    return;
  }
  if (!entry.isDirectory) return;
  out.push({ dir: rel });
  const reader = entry.createReader();
  for (;;) {
    const batch = await readBatch(reader);
    if (!batch.length) break;
    for (const child of batch) await walkEntry(child, rel, out);
  }
}

function readBatch(reader) {
  return new Promise((resolve, reject) => reader.readEntries(resolve, reject));
}

function fileFromEntry(entry) {
  return new Promise((resolve, reject) => entry.file(resolve, reject));
}

/**
 * Download plan for a selection. One entry keeps the legacy single-file
 * path (one job, same archive name as today); several entries become one
 * batch download — one merged tree, one tar.gz — because the browser owes
 * a save per job and N jobs would be a queue the user closes N times.
 * The batch's archive name is supplied by the caller (it is i18n).
 */
export function planDownload(entries) {
  const list = (entries || []).filter(Boolean);
  if (list.length === 1) return { mode: 'single', path: list[0].path, file_name: list[0].name };
  if (!list.length) return null;
  return { mode: 'batch', paths: list.map((e) => e.path) };
}
