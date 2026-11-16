import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

/**
 * Conflicts — the two halves of "don't silently destroy anything".
 *
 * The dialog asks once about a whole batch, and the uploader's rename
 * counter has to remember the names it invented for itself: deriving
 * every name from the pre-batch listing would hand "a (1).txt" to three
 * colliding files and let them overwrite each other on the remote side.
 */
const fx = vi.hoisted(() => ({ upload: [], mkdir: [], mkdirFail: null }));

vi.mock('$lib/stores/auth.svelte.js', () => {
  const api = {
    machineSFTPMkdir: (id, p) => {
      fx.mkdir.push(p);
      const why = fx.mkdirFail?.(p);
      if (why) return Promise.reject(new Error(`${why}: ${p}`));
      return Promise.resolve({ path: p });
    },
    machineTransferUpload: (id, file, remotePath) => {
      fx.upload.push(remotePath);
      return Promise.resolve({ id: `job-${fx.upload.length}`, status: 'running', machine_id: id });
    },
    machineSFTPList: () => Promise.resolve({ path: '/home/admin', entries: [] }),
    listMachines: () => Promise.resolve([]),
  };
  return {
    api: new Proxy(api, {
      get(target, prop) {
        if (prop in target) return target[prop];
        if (typeof prop !== 'string') return undefined;
        return () => Promise.resolve({});
      },
    }),
    auth: { user: 'admin', role: 'admin', isAdmin: () => true, canMutate: () => true },
    fx,
  };
});

vi.mock('$lib/stores/events.svelte.js', () => ({
  events: {
    onTransferProgress: () => () => {},
    onOpen: () => () => {},
    connect: () => {},
    close: () => {},
  },
}));

import ConflictDialog from '$lib/components/machine/files/ConflictDialog.svelte';
import { uploadToPane } from '$lib/components/machine/files/fileActions.js';
import { Pane } from '$lib/components/machine/files/paneModel.svelte.js';
import { setLocale } from '$lib/i18n.svelte.js';

const settle = async () => {
  for (let i = 0; i < 8; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
};

const dialog = () => [...document.body.querySelectorAll('[data-slot="dialog-content"]')].at(-1);
const named = (root, label) =>
  [...root.querySelectorAll('button')].find((b) => (b.textContent || '').trim() === label);
const f = (name) => ({ name, size: 3 });

let target, app;

beforeEach(() => {
  setLocale('en');
  fx.upload = [];
  fx.mkdir = [];
  fx.mkdirFail = null;
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(async () => {
  if (app) await unmount(app);
  target?.remove();
  app = null;
});

describe('ConflictDialog', () => {
  it('offers exactly the three policies the server understands', async () => {
    const answers = [];
    app = mount(ConflictDialog, {
      target,
      props: {
        open: true,
        conflicts: ['projects', 'notes.txt'],
        onresolve: (p) => answers.push(p),
      },
    });
    await settle();

    const root = dialog();
    expect(root).toBeTruthy();
    expect(root.textContent).toContain('projects');
    expect(root.textContent).toContain('notes.txt');
    expect(root.textContent).toContain('2 names already exist');

    for (const label of ['Skip', 'Overwrite', 'Keep both']) {
      named(root, label).click();
      await settle();
    }
    expect(answers).toEqual(['skip', 'overwrite', 'rename']);
  }, 30000);

  it('ignores the buttons while a batch is in flight', async () => {
    const answers = [];
    app = mount(ConflictDialog, {
      target,
      props: { open: true, conflicts: ['projects'], busy: true, onresolve: (p) => answers.push(p) },
    });
    await settle();

    const root = dialog();
    for (const label of ['Skip', 'Overwrite', 'Keep both']) {
      expect(named(root, label).disabled).toBe(true);
    }
    named(root, 'Skip').click();
    await settle();
    expect(answers).toEqual([]);
  }, 30000);
});

describe('uploadToPane naming', () => {
  const paneFor = (names) => {
    const pane = new Pane({ side: 'left', machineId: 'm-1' });
    pane.entries = names.map((name) => ({
      name,
      path: `/home/admin/${name}`,
      is_dir: false,
      size: 1,
    }));
    pane.path = '/home/admin';
    return pane;
  };

  it('accumulates the counter across one batch', async () => {
    const pane = paneFor(['a (1).txt']);
    await uploadToPane(pane, [
      { file: f('a.txt'), relPath: 'a.txt' },
      { file: f('a.txt'), relPath: 'a.txt' },
      { file: f('a.txt'), relPath: 'a.txt' },
    ]);

    // "a (1).txt" was already on disk, so the free name is taken by the
    // first of the three and the rest walk on from there.
    expect(fx.upload).toEqual([
      '/home/admin/a.txt',
      '/home/admin/a (2).txt',
      '/home/admin/a (3).txt',
    ]);
  }, 30000);

  it('creates the folder skeleton, shallow first, before any payload', async () => {
    const pane = paneFor([]);
    await uploadToPane(pane, [
      { dir: 'proj/sub/deep' },
      { file: f('x.txt'), relPath: 'proj/sub/deep/x.txt' },
      { file: f('readme.md'), relPath: 'proj/readme.md' },
    ]);

    expect(fx.mkdir).toEqual([
      '/home/admin/proj',
      '/home/admin/proj/sub',
      '/home/admin/proj/sub/deep',
    ]);
    expect(fx.upload).toEqual(['/home/admin/proj/sub/deep/x.txt', '/home/admin/proj/readme.md']);
    expect(fx.mkdir.length).toBe(3);
  }, 30000);

  it('keeps an empty folder of a drop', async () => {
    const pane = paneFor([]);
    await uploadToPane(pane, [{ dir: 'empty' }]);

    expect(fx.mkdir).toEqual(['/home/admin/empty']);
    expect(fx.upload).toEqual([]);
  }, 30000);

  it('treats an already-present folder as the outcome it wanted', async () => {
    const pane = paneFor([]);
    fx.mkdirFail = (p) => (p === '/home/admin/proj' ? 'file exists' : null);
    const res = await uploadToPane(pane, [
      { dir: 'proj' },
      { file: f('x.txt'), relPath: 'proj/x.txt' },
      { file: f('y.txt'), relPath: 'y.txt' },
    ]);

    expect(res.errors).toEqual([]);
    // The rest of the skeleton and both payloads still went out.
    expect(fx.mkdir).toEqual(['/home/admin/proj']);
    expect(fx.upload).toEqual(['/home/admin/proj/x.txt', '/home/admin/y.txt']);
    expect(res.uploaded).toBe(2);
  }, 30000);

  it('keeps a denied folder to itself instead of cancelling the drop', async () => {
    const pane = paneFor([]);
    fx.mkdirFail = (p) => (p === '/home/admin/proj' ? 'permission denied' : null);
    const res = await uploadToPane(pane, [
      { dir: 'proj/sub' },
      { file: f('x.txt'), relPath: 'proj/sub/x.txt' },
      { file: f('ok.txt'), relPath: 'ok.txt' },
    ]);

    expect(res.errors).toEqual(['proj: permission denied: /home/admin/proj']);
    // Its own folder and the files under it are skipped (the one error
    // already names them); everything outside the folder still lands.
    expect(fx.mkdir).toEqual(['/home/admin/proj']);
    expect(fx.upload).toEqual(['/home/admin/ok.txt']);
    expect(res.uploaded).toBe(1);
  }, 30000);
});
