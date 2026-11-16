import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * files store — the delete paths behind the queue's trash icon and the
 * "Clear N finished" button.
 *
 * What this protects:
 *  - a delete that races a late SSE frame (the done broadcast in flight
 *    while the user clicks trash) is a tombstone, not a resurrection;
 *  - a 404 from the server (the job was evicted past the cap) is a
 *    success, because the local row was the last trace of it;
 *  - any other failure rolls the optimistic deletion back and rethrows,
 *    so the UI can say why and a later frame brings the row back;
 *  - the batch clear touches only terminal rows, in one server pass,
 *    and restores everything it took when the pass fails.
 */

const fx = vi.hoisted(() => ({
  removeError: new Map(), // job id -> Error the DELETE rejects with
  clearError: null, // Error the batch POST rejects with
  deletes: [],
  clears: 0,
}));

vi.mock('$lib/stores/auth.svelte.js', () => ({
  api: {
    me: () => Promise.resolve({ username: 'admin', role: 'admin' }),
    listMachines: () => Promise.resolve([]),
    listAllMachineTransfers: () => Promise.resolve([]),
    removeMachineTransferJob: (id) => {
      fx.deletes.push(id);
      const e = fx.removeError.get(id);
      return e ? Promise.reject(e) : Promise.resolve({});
    },
    clearMachineTransfers: () => {
      fx.clears += 1;
      return fx.clearError ? Promise.reject(fx.clearError) : Promise.resolve({ removed: 0 });
    },
  },
}));

vi.mock('$lib/stores/events.svelte.js', () => ({
  events: {
    onTransferProgress: () => () => {},
    onOpen: () => () => {},
    connect: () => {},
    close: () => {},
  },
}));

import { files } from './files.svelte.js';

const err = (message, status) => Object.assign(new Error(message), { status });

beforeEach(() => {
  for (const id of [...files.jobs.keys()]) files.jobs.delete(id);
  files.removed.clear();
  fx.removeError.clear();
  fx.clearError = null;
  fx.deletes.length = 0;
  fx.clears = 0;
});

describe('files store: single-row remove', () => {
  it('deletes the row and a late frame cannot resurrect it', async () => {
    const job = files.upsert({ id: 'j1', kind: 'download', machine_id: 'm1', status: 'done' });
    expect(files.jobs.has('j1')).toBe(true);

    await expect(files.remove(job)).resolves.toBe(true);
    expect(files.jobs.has('j1')).toBe(false);
    expect(fx.deletes).toEqual(['j1']);

    // The done broadcast that was in flight when the trash was clicked:
    // the tombstone must swallow it, or the row comes back from nowhere.
    expect(files.upsert({ id: 'j1', status: 'done' })).toBeNull();
    expect(files.jobs.has('j1')).toBe(false);
  });

  it('treats a 404 (server already forgot it) as success', async () => {
    const job = files.upsert({ id: 'j2', kind: 'download', status: 'done' });
    fx.removeError.set('j2', err('transfer not found', 404));

    await expect(files.remove(job)).resolves.toBe(true);
    expect(files.jobs.has('j2')).toBe(false);
    // The tombstone stays: the server has no such job to resurrect.
    expect(files.removed.has('j2')).toBe(true);
  });

  it('rolls back and rethrows when the server refuses', async () => {
    const job = files.upsert({ id: 'j3', kind: 'download', status: 'running' });
    fx.removeError.set('j3', err('transfer is still running; cancel it first', 409));

    await expect(files.remove(job)).rejects.toThrow('transfer is still running');
    // The row is back exactly as it was, and a live frame can update it.
    expect(files.jobs.get('j3').status).toBe('running');
    expect(files.removed.has('j3')).toBe(false);
    expect(files.upsert({ id: 'j3', status: 'paused' })).not.toBeNull();
    expect(files.jobs.get('j3').status).toBe('paused');
  });
});

describe('files store: clearFinished', () => {
  it('clears every terminal row in one server pass and spares the live ones', async () => {
    files.upsert({ id: 'a', status: 'done' });
    files.upsert({ id: 'b', status: 'error' });
    files.upsert({ id: 'c', status: 'canceled' });
    files.upsert({ id: 'r', status: 'running' });
    files.upsert({ id: 'p', status: 'paused' });

    const n = await files.clearFinished();

    expect(n).toBe(3);
    expect(fx.clears).toBe(1);
    for (const id of ['a', 'b', 'c']) expect(files.jobs.has(id)).toBe(false);
    for (const id of ['r', 'p']) expect(files.jobs.has(id)).toBe(true);
    // Late frames for the cleared rows stay dead.
    expect(files.upsert({ id: 'a', status: 'done' })).toBeNull();
  });

  it('restores the rows and rethrows when the pass fails', async () => {
    const job = {
      id: 'a',
      kind: 'download',
      machine_id: 'm1',
      status: 'done',
      done: 10,
      total: 10,
    };
    files.upsert(job);
    fx.clearError = err('boom', 500);

    await expect(files.clearFinished()).rejects.toThrow('boom');
    expect(files.jobs.get('a').status).toBe('done');
    expect(files.jobs.get('a').total).toBe(10);
    expect(files.removed.has('a')).toBe(false);
    // The row is writable by frames again.
    expect(files.upsert({ id: 'a', done: 10, total: 10 })).not.toBeNull();
  });

  it('no-ops without a server call when nothing is finished', async () => {
    files.upsert({ id: 'r', status: 'running' });
    files.upsert({ id: 'p', status: 'paused' });

    expect(await files.clearFinished()).toBe(0);
    expect(fx.clears).toBe(0);
    expect(files.jobs.size).toBe(2);
  });
});
