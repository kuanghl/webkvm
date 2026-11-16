/**
 * fileActions.js — the operations one pane can perform on its machine.
 *
 * Kept out of the components so a pane never mixes "what to call" with
 * "when to re-render", and so the sequential semantics are stated once:
 * deletions, downloads and uploads run one at a time and collect their
 * errors instead of aborting the batch. A user who selected 40 files
 * wants to hear about the 2 that failed, not to lose the other 38.
 */

import { api } from '$lib/stores/auth.svelte.js';
import { files } from '$lib/stores/files.svelte.js';
import { openAttachment } from '$lib/utils/attachment.js';
import {
  basename,
  copyRequests,
  dirOf,
  dstPathsFor,
  existingOf,
  joinPath,
  planDownload,
  planUploads,
} from '$lib/utils/fileTransfer.js';
import { messageOf } from './paneModel.svelte.js';

export function makeFolder(pane, name) {
  return api.machineSFTPMkdir(pane.machineId, joinPath(pane.path, name)).then(() => pane.refresh());
}

export async function renameEntry(pane, entry, name) {
  if (!name || name === entry.name) return;
  await api.machineSFTPRename(pane.machineId, entry.path, joinPath(dirOf(entry.path), name));
  await pane.refresh();
}

export async function chmodEntry(pane, entry, mode) {
  await api.machineSFTPChmod(pane.machineId, entry.path, mode);
  await pane.refresh();
}

export async function deleteEntries(pane, entries) {
  const errors = [];
  for (const entry of entries) {
    try {
      await api.machineSFTPDelete(pane.machineId, entry.path);
    } catch (e) {
      errors.push(`${entry.name}: ${messageOf(e)}`);
    }
  }
  await pane.refresh();
  return errors;
}

/**
 * One queue row for the whole selection: a single entry is the legacy
 * single-file job, several entries are one batch job (one merged tree,
 * one tar.gz) — the browser owes one save per job, and N jobs would be a
 * queue to close N times. The batch's archive name is the i18n default
 * the caller passes in.
 */
export async function downloadEntries(pane, entries, defaultBatchName) {
  const plan = planDownload(entries);
  if (!plan) return [];
  try {
    const job =
      plan.mode === 'single'
        ? await api.machineTransferDownload(pane.machineId, plan.path, plan.file_name)
        : await api.machineTransferDownloadBatch(pane.machineId, plan.paths, defaultBatchName);
    files.trackDownload(job);
    return [];
  } catch (e) {
    return [messageOf(e)];
  }
}

/** Where a copy/upload would land in this pane, before the first load. */
export function destDirOf(pane) {
  return pane.path || pane.home || '/';
}

/**
 * Names already present at the destination, from one batched stat —
 * the precondition for asking about every conflict in one dialog.
 */
export async function conflictsFor(srcEntries, dstPane) {
  const paths = dstPathsFor(srcEntries, destDirOf(dstPane));
  if (!paths.length) return [];
  const res = await api.machineSFTPStat(dstPane.machineId, paths);
  return existingOf(res?.stats).map((p) => basename(p));
}

/**
 * Start one job per selected entry, from `srcId` into `dstId`.
 *
 * Takes ids and entries rather than two panes so a cross-pane drag
 * (whose source is described by the drag payload, not by a pane that is
 * still mounted) can use the same call.
 */
export async function copyEntries(srcId, entries, dstId, dstDir, policy) {
  const reqs = copyRequests(entries, dstId, dstDir, policy);
  const errors = [];
  for (const req of reqs) {
    try {
      files.upsert(await api.machineTransferCopy(srcId, req));
    } catch (e) {
      errors.push(messageOf(e));
    }
  }
  return { started: reqs.length - errors.length, errors };
}

/**
 * Upload a drop, recreating the folder skeleton first.
 *
 * The remote leg needs its parent to exist, so the mkdirs precede the
 * jobs; `taken` seeds from the pane's own listing, which is why the
 * counter is correct for the visible directory and merges (rather than
 * renames) inside newly created subfolders.
 *
 * Every folder stands on its own: the server answers 200 for one that is
 * already there, and a folder it genuinely cannot make (permission denied
 * on someone else's tree) only disqualifies what would have landed inside
 * it, never the rest of the drop.
 */
export async function uploadToPane(pane, items, onProgress) {
  const taken = new Set(pane.entries.map((e) => e.name));
  const { dirs, files: queued } = planUploads(items, destDirOf(pane), taken);
  const errors = [];
  const broken = new Set();
  // `dirs` is shallow-first, so a parent is already known broken before
  // its children are tried, and one failure is reported once.
  for (const dir of dirs) {
    if (broken.has(dirOf(dir))) {
      broken.add(dir);
      continue;
    }
    try {
      await api.machineSFTPMkdir(pane.machineId, dir);
    } catch (e) {
      if (alreadyExists(e)) continue;
      broken.add(dir);
      errors.push(`${basename(dir)}: ${messageOf(e)}`);
    }
  }
  let uploaded = 0;
  for (const item of queued) {
    const name = basename(item.remotePath);
    if (broken.has(dirOf(item.remotePath))) continue;
    try {
      const job = await api.machineTransferUpload(
        pane.machineId,
        item.file,
        item.remotePath,
        (pct) => onProgress?.({ path: item.remotePath, name, pct })
      );
      if (job) files.upsert(job);
      uploaded++;
    } catch (e) {
      errors.push(`${name}: ${messageOf(e)}`);
    }
  }
  onProgress?.(null);
  await pane.refresh();
  return { uploaded, errors };
}

// "The directory you asked for is a directory" is the outcome we wanted.
// An older server, or a second pane making the same skeleton, still
// answers with one of these instead of the 200 the current one gives.
function alreadyExists(e) {
  return /file exists|already exists|EEXIST/i.test(messageOf(e));
}

export async function previewEntry(pane, entry) {
  const res = await api.machineSFTPRead(pane.machineId, entry.path, 0);
  return { text: b64ToText(res?.data || ''), truncated: !!res?.truncated };
}

export function b64ToText(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new TextDecoder('utf-8', { fatal: false }).decode(bytes);
}

/** Cookie-authenticated attachment: the browser download bar takes over. */
export function saveJobResult(job) {
  openAttachment(api.machineTransferResultUrl(job.machine_id, job.id));
}
