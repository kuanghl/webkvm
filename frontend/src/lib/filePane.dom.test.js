import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

/**
 * FilePane — the single-column behaviours a pane owns.
 *
 * What this protects:
 *  - "up one level" asks for the parent, not the same folder;
 *  - a stale listing is thrown away (two clicks, two RTTs, one answer);
 *  - a 409 host-key refusal opens the trust dialog instead of printing
 *    `hostkey_untrusted` at the user as if it were their mistake;
 *  - a copy asks once about the destination (one batched stat, however
 *    many rows are selected) and posts the shape the server expects;
 *  - a collision is answered in a single dialog whose policy rides on
 *    every request of the batch.
 */
const SSH1 = { id: 'm-1', name: 'alpha', protocol: 'ssh' };
const SSH2 = { id: 'm-2', name: 'beta', protocol: 'ssh' };

const fx = vi.hoisted(() => ({
  list: () => Promise.resolve({ path: '/home/admin', entries: [] }),
  stat: () => Promise.resolve({ stats: [] }),
  listCalls: [],
  statCalls: [],
  copyCalls: [],
}));

vi.mock('$lib/stores/auth.svelte.js', () => {
  const api = {
    me: () => Promise.resolve({ username: 'admin', role: 'admin' }),
    listMachines: () => Promise.resolve([SSH1, SSH2]),
    machineSFTPList: (id, p) => {
      fx.listCalls.push([id, p]);
      return fx.list(id, p);
    },
    machineSFTPStat: (id, paths) => {
      fx.statCalls.push([id, paths]);
      return fx.stat(paths);
    },
    machineTransferCopy: (id, body) => {
      fx.copyCalls.push([id, body]);
      return Promise.resolve({
        id: `job-${fx.copyCalls.length}`,
        status: 'running',
        machine_id: id,
      });
    },
    listAllMachineTransfers: () => Promise.resolve([]),
  };
  return {
    api: new Proxy(api, {
      get(target, prop) {
        if (prop in target) return target[prop];
        if (typeof prop !== 'string') return undefined;
        return () => Promise.resolve({});
      },
    }),
    auth: {
      user: 'admin',
      role: 'admin',
      isAdmin: () => true,
      isOperator: () => true,
      isViewer: () => false,
      canMutate: () => true,
    },
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

import FilePane from '$lib/components/machine/files/FilePane.svelte';
import { Pane } from '$lib/components/machine/files/paneModel.svelte.js';
import { files } from '$lib/stores/files.svelte.js';
import { setLocale } from '$lib/i18n.svelte.js';

const settle = async () => {
  for (let i = 0; i < 8; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
};

const dir = (name) => ({
  name,
  path: `/home/admin/${name}`,
  is_dir: true,
  size: 0,
  mode: 'drwxr-xr-x',
  mod_time: 1762000000,
});
const file = (name, size = 12) => ({
  name,
  path: `/home/admin/${name}`,
  is_dir: false,
  size,
  mode: '-rw-r--r--',
  mod_time: 1762000000,
});

const buttonByLabel = (root, label) =>
  [...root.querySelectorAll('button')].find(
    (b) => (b.getAttribute('aria-label') || '') === label || (b.textContent || '').includes(label)
  );

let target, app;

beforeEach(() => {
  setLocale('en');
  localStorage.clear();
  fx.list = () =>
    Promise.resolve({ path: '/home/admin', entries: [dir('projects'), file('notes.txt')] });
  fx.stat = () => Promise.resolve({ stats: [] });
  fx.listCalls = [];
  fx.statCalls = [];
  fx.copyCalls = [];
  files.jobs = new Map();
  // The copy button names the destination machine, so the pane has to be
  // able to look it up.
  files.machines = [SSH1, SSH2];
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(async () => {
  if (app) await unmount(app);
  target?.remove();
  app = null;
});

function mountPanes({ pinned = false } = {}) {
  const left = new Pane({ side: 'left' });
  const right = new Pane({ side: 'right' });
  app = mount(FilePane, {
    target,
    props: {
      pane: left,
      other: right,
      side: 'left',
      machines: [SSH1, SSH2],
      focused: true,
      pinned,
    },
  });
  return { left, right };
}

describe('FilePane', () => {
  it('browses up to the parent folder', async () => {
    const { left } = mountPanes();
    await settle();

    left.setMachine('m-1');
    await left.load('');
    await settle();
    expect(left.path).toBe('/home/admin');

    buttonByLabel(target, 'Up one level').click();
    await settle();

    expect(fx.listCalls.at(-1)).toEqual(['m-1', '/home']);
  }, 30000);

  // The history is $state on the Pane, and these two arrows are the only
  // thing that reads it: a plain field left them disabled no matter how
  // deep the user had browsed.
  it('re-arms the back/forward arrows on every path change', async () => {
    const { left } = mountPanes();
    await settle();

    const back = () => target.querySelector('[aria-label="Back"]');
    const forward = () => target.querySelector('[aria-label="Forward"]');

    // The fake answers with whatever it was asked for, so the pane really
    // moves and the history sees two distinct paths.
    fx.list = (id, p) =>
      Promise.resolve({ path: p || '/home/admin', entries: p ? [] : [dir('projects')] });

    left.setMachine('m-1');
    await left.load('');
    await settle();
    expect(back().disabled).toBe(true);

    await left.load('/home/admin/projects');
    await settle();
    expect(left.path).toBe('/home/admin/projects');
    expect(back().disabled).toBe(false);
    expect(forward().disabled).toBe(true);

    back().click();
    await settle();
    expect(left.path).toBe('/home/admin');
    expect(back().disabled).toBe(true);
    expect(forward().disabled).toBe(false);
  }, 30000);

  it('drops a listing that arrived after a newer one', async () => {
    const { left } = mountPanes();
    await settle();

    const gates = {};
    fx.list = (id, p) =>
      p === '/slow'
        ? new Promise((res) => {
            gates.slow = res;
          })
        : Promise.resolve({ path: p, entries: [file('fresh.txt')] });

    left.setMachine('m-1');
    const first = left.load('/slow');
    await settle();
    const second = left.load('/fast');
    await second;
    gates.slow({ path: '/slow', entries: [file('stale.txt')] });
    await first;
    await settle();

    expect(left.path).toBe('/fast');
    expect(left.entries.map((e) => e.name)).toEqual(['fresh.txt']);
    expect(target.textContent).not.toContain('stale.txt');
  }, 30000);

  it('turns a 409 host-key refusal into the trust dialog', async () => {
    fx.list = () =>
      Promise.reject({
        status: 409,
        data: { error: 'hostkey_untrusted', fingerprint: 'SHA256:aaaa:bbbb' },
      });
    const { left } = mountPanes();
    await settle();

    const select = target.querySelector('select');
    select.value = 'm-1';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    await settle();

    const dialogs = [...document.body.querySelectorAll('[data-slot="dialog-content"]')];
    expect(dialogs.some((d) => (d.textContent || '').includes('SHA256:aaaa:bbbb'))).toBe(true);
    expect(left.error).toBe('');
    expect(target.textContent).not.toContain('hostkey_untrusted');
  }, 30000);

  it('asks about the destination once and copies with the folder flag', async () => {
    const { left, right } = mountPanes();
    await settle();

    left.setMachine('m-1');
    await left.load('');
    right.setMachine('m-2');
    await right.load('');
    await settle();

    const row = [...target.querySelectorAll('tbody tr')].find((r) =>
      r.textContent.includes('projects')
    );
    row.dispatchEvent(new window.MouseEvent('mousedown', { bubbles: true, button: 0 }));
    await settle();

    buttonByLabel(target, 'Copy to beta').click();
    await settle();

    expect(fx.statCalls).toHaveLength(1);
    expect(fx.statCalls[0][1]).toEqual(['/home/admin/projects']);
    expect(fx.copyCalls).toHaveLength(1);
    expect(fx.copyCalls[0]).toEqual([
      'm-1',
      {
        // The server decodes `remote_path`; `src_path` used to reach the
        // wire and the job planned from the remote root.
        remote_path: '/home/admin/projects',
        dst_machine_id: 'm-2',
        dst_dir: '/home/admin',
        recursive: true,
        overwrite: 'overwrite',
      },
    ]);
  }, 30000);

  it('answers a collision with one dialog whose policy reaches the server', async () => {
    fx.stat = (paths) =>
      Promise.resolve({ stats: paths.map((p) => ({ path: p, exists: true, is_dir: true })) });
    const { left, right } = mountPanes();
    await settle();

    left.setMachine('m-1');
    await left.load('');
    right.setMachine('m-2');
    await right.load('');
    await settle();

    const row = [...target.querySelectorAll('tbody tr')].find((r) =>
      r.textContent.includes('projects')
    );
    row.dispatchEvent(new window.MouseEvent('mousedown', { bubbles: true, button: 0 }));
    await settle();

    buttonByLabel(target, 'Copy to beta').click();
    await settle();

    expect(fx.copyCalls).toHaveLength(0);
    const dialogs = [...document.body.querySelectorAll('[data-slot="dialog-content"]')];
    const conflict = dialogs.find((d) => (d.textContent || '').includes('names already exist'));
    expect(conflict).toBeTruthy();
    expect(conflict.textContent).toContain('projects');

    const skip = [...conflict.querySelectorAll('button')].find(
      (b) => (b.textContent || '').trim() === 'Skip'
    );
    skip.click();
    await settle();

    expect(fx.copyCalls).toHaveLength(1);
    expect(fx.copyCalls[0][1].overwrite).toBe('skip');
  }, 30000);

  it('refuses to re-point a pinned pane', async () => {
    mountPanes({ pinned: true });
    await settle();

    expect(target.querySelector('select').disabled).toBe(true);
  }, 30000);
});
