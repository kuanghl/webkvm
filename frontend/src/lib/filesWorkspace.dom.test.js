import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

/**
 * FilesWorkspace — one machine's pane, the transfer view behind a button
 * and the queue feed.
 *
 * What this protects:
 *  - the tab opens on the machine's own folder, alone, with no second
 *    column to explain;
 *  - the picker never offers a telnet machine, and does offer the
 *    built-in host card (the server only sends that to admins);
 *  - the transfer button is what brings the second pane up, and the copy
 *    is aimed by name rather than by "the right pane";
 *  - the split handle writes the clamped percentage down;
 *  - a 501-entry folder renders 500 rows, not 501 or one;
 *  - a late SSE frame never walks a finished job back to "running";
 *  - a download hands its file to the browser once, and once only, while
 *    a queue re-read of someone's old jobs never downloads anything.
 */
const SSH1 = {
  id: 'm-1',
  name: 'alpha',
  protocol: 'ssh',
  host: '10.0.0.1',
  port: 22,
  user: 'admin',
};
const SSH2 = {
  id: 'm-2',
  name: 'beta',
  protocol: 'ssh',
  host: '10.0.0.2',
  port: 22,
  user: 'admin',
};
const TELNET = {
  id: 'm-3',
  name: 'switch',
  protocol: 'telnet',
  host: '10.0.0.3',
  port: 23,
  user: 'admin',
};
const HOST = { id: 'host', name: 'webkvm host', protocol: 'local', builtin: true };

const fx = vi.hoisted(() => ({
  machines: [],
  list: () => Promise.resolve({ path: '/home/admin', entries: [] }),
  listCalls: [],
  progress: [],
  open: [],
  queue: [],
  downloads: [],
}));

vi.mock('$lib/stores/auth.svelte.js', () => {
  const api = {
    me: () => Promise.resolve({ username: 'admin', role: 'admin' }),
    listMachines: () => Promise.resolve(fx.machines),
    machineSFTPList: (id, p) => {
      fx.listCalls.push([id, p]);
      return fx.list(id, p);
    },
    machineSFTPStat: () => Promise.resolve({ stats: [] }),
    machineTransferCopy: () => Promise.resolve({ id: 'job-1', status: 'running' }),
    machineTransferDownload: (id, path) => {
      fx.downloads.push([id, path]);
      return Promise.resolve({ id: 'dl-1', machine_id: id, kind: 'download', status: 'running' });
    },
    machineTransferResultUrl: (id, job) => `/api/ssh-machines/${id}/transfers/${job}/result`,
    listAllMachineTransfers: () => Promise.resolve(fx.queue),
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
    onTransferProgress: (fn) => {
      fx.progress.push(fn);
      return () => {};
    },
    onOpen: (fn) => {
      fx.open.push(fn);
      return () => {};
    },
    connect: () => {},
    close: () => {},
  },
}));

import FilesWorkspace from '$lib/components/machine/files/FilesWorkspace.svelte';
import { files } from '$lib/stores/files.svelte.js';
import { PAGE_SIZE } from '$lib/utils/fileTransfer.js';
import { setLocale } from '$lib/i18n.svelte.js';

const settle = async () => {
  for (let i = 0; i < 8; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
};

const entry = (name, extra = {}) => ({
  name,
  path: `/home/admin/${name}`,
  is_dir: false,
  size: 10,
  mode: '-rw-r--r--',
  mod_time: 1762000000,
  ...extra,
});

let target, app, anchorClick;

beforeEach(() => {
  setLocale('en');
  localStorage.clear();
  fx.machines = [SSH1, SSH2, TELNET, HOST];
  fx.list = () => Promise.resolve({ path: '/home/admin', entries: [entry('alpha.txt')] });
  fx.listCalls = [];
  fx.progress = [];
  fx.open = [];
  fx.queue = [];
  fx.downloads = [];
  files.disposeSSE();
  files.jobs = new Map();
  files.machines = [];
  files.machinesLoaded = false;
  files.setSplit(50);
  // The pending-delivery set lives on the singleton, exactly as the rows
  // it refers to do; both have to start empty for these cases to mean
  // what they say.
  files._toSave.clear();
  anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(async () => {
  if (app) await unmount(app);
  target?.remove();
  app = null;
  vi.restoreAllMocks();
});

const selects = () => [...target.querySelectorAll('select')];
const tables = () => [...target.querySelectorAll('tbody')];

const buttonByLabel = (text) =>
  [...target.querySelectorAll('button')].find((b) => (b.textContent || '').includes(text));

const buttonByTitle = (title) =>
  [...target.querySelectorAll('button')].find((b) => b.getAttribute('title') === title);

async function pickMachine(select, id) {
  select.value = id;
  select.dispatchEvent(new Event('change', { bubbles: true }));
  await settle();
}

function openTransferView() {
  const b = buttonByLabel('Copy to another machine');
  expect(b).toBeTruthy();
  expect(b.disabled).toBe(false);
  b.click();
  return settle();
}

describe('FilesWorkspace', () => {
  it('opens on the machine alone, already listing its own folder', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    expect(selects()).toHaveLength(1);
    expect(selects()[0].disabled).toBe(true);
    expect(selects()[0].value).toBe('m-1');
    expect(tables()).toHaveLength(1);
    expect(tables()[0].textContent).toContain('alpha.txt');
    expect(target.querySelector('[role="separator"]')).toBeNull();
    expect(fx.listCalls).toEqual([['m-1', '']]);
  }, 30000);

  it('offers ssh machines and the host card, never telnet', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();
    await openTransferView();

    const options = [...selects()[1].querySelectorAll('option')].map((o) => o.textContent);
    expect(options).toContain('beta');
    expect(options).toContain('webkvm host');
    expect(options).not.toContain('switch');
  }, 30000);

  it('brings up the second pane, aimed at another machine, on the transfer button', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    fx.list = (id) =>
      Promise.resolve({
        path: '/home/admin',
        entries: [entry(id === 'm-1' ? 'alpha.txt' : 'beta.txt')],
      });
    await openTransferView();

    expect(selects()).toHaveLength(2);
    expect(target.querySelector('[role="separator"]')).toBeTruthy();
    expect(tables()).toHaveLength(2);
    expect(tables()[0].textContent).toContain('alpha.txt');
    expect(tables()[1].textContent).toContain('beta.txt');
    // The destination is named, not "the right pane".
    expect(buttonByLabel('Copy to beta')).toBeTruthy();
  }, 30000);

  it('re-aims the destination when the transfer picker changes', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();
    fx.list = (id) =>
      Promise.resolve({
        path: '/',
        entries: [entry(id === 'm-2' ? 'beta.txt' : id === 'host' ? 'push.sh' : 'alpha.txt')],
      });
    await openTransferView();

    // The second pane opens aimed at the first other machine.
    expect(selects()[1].value).toBe('m-2');
    expect(tables()[1].textContent).toContain('beta.txt');

    await pickMachine(selects()[1], 'host');
    expect(fx.listCalls.at(-1)).toEqual(['host', '']);
    expect(tables()[1].textContent).toContain('push.sh');
  }, 30000);

  it('keeps the copy button out of the single-pane layout', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    // One pane has nowhere to copy to, so the only copy affordance is the
    // button that opens the second pane.
    expect(buttonByLabel('Copy to beta')).toBeUndefined();
    expect(buttonByLabel('Copy to another machine')).toBeTruthy();
  }, 30000);

  it('writes the dragged split to storage, clamped to the usable range', async () => {
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockReturnValue({
      left: 0,
      top: 0,
      width: 1000,
      height: 600,
      right: 1000,
      bottom: 600,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();
    await openTransferView();

    const sep = target.querySelector('[role="separator"]');
    expect(sep).toBeTruthy();

    sep.dispatchEvent(new window.MouseEvent('pointerdown', { bubbles: true, clientX: 500 }));
    window.dispatchEvent(new window.MouseEvent('pointermove', { bubbles: true, clientX: 700 }));
    flushSync();
    expect(files.split).toBe(70);

    // Past the middle of the right pane the handle stops: a column that
    // narrow cannot show a path bar and a name column at once.
    window.dispatchEvent(new window.MouseEvent('pointermove', { bubbles: true, clientX: 2400 }));
    flushSync();
    expect(files.split).toBe(80);
    window.dispatchEvent(new window.MouseEvent('pointerup', { bubbles: true }));

    expect(localStorage.getItem('webkvm.files.split.v1')).toBe('80');
  }, 30000);

  it('renders one page of a huge folder and offers the rest', async () => {
    const many = Array.from({ length: PAGE_SIZE + 1 }, (_, i) =>
      entry(`f${String(i).padStart(4, '0')}.txt`)
    );
    fx.list = () => Promise.resolve({ path: '/home/admin', entries: many });
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    const rows = tables()[0].querySelectorAll('tr');
    expect(rows).toHaveLength(PAGE_SIZE);
    const more = buttonByLabel('Load more');
    expect(more).toBeTruthy();

    more.click();
    await settle();
    expect(tables()[0].querySelectorAll('tr')).toHaveLength(PAGE_SIZE + 1);
  }, 30000);

  it('never regresses a finished job when a late progress frame arrives', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    expect(fx.progress.length).toBeGreaterThan(0);
    const emit = fx.progress[0];
    emit({ data: { job_id: 'j-1', machine_id: 'm-1', status: 'running', done: 1, total: 10 } });
    emit({ data: { job_id: 'j-1', machine_id: 'm-1', status: 'done', done: 10, total: 10 } });
    flushSync();
    emit({ data: { job_id: 'j-1', machine_id: 'm-1', status: 'running', done: 3, total: 10 } });
    flushSync();

    const job = files.jobs.get('j-1');
    expect(job.status).toBe('done');
    expect(job.done).toBe(10);
  }, 30000);

  // The progress stream is the only writer, so a socket that dropped lost
  // every frame sent while it was down. Opening re-reads the queue once.
  it('heals the queue when the SSE socket comes back', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    fx.progress[0]({
      data: { job_id: 'j-1', machine_id: 'm-1', status: 'running', done: 1, total: 10 },
    });
    flushSync();
    expect(files.jobs.get('j-1').done).toBe(1);

    fx.queue = [
      { id: 'j-1', machine_id: 'm-1', status: 'done', done: 10, total: 10, current_path: '' },
    ];
    await fx.open[0]();
    await settle();

    const job = files.jobs.get('j-1');
    expect(job.status).toBe('done');
    expect(job.done).toBe(10);
  }, 30000);

  it('hands a finished download to the browser, once', async () => {
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    const download = buttonByTitle('Download');
    expect(download).toBeTruthy();
    expect(download.disabled).toBe(false);
    download.click();
    await settle();
    expect(fx.downloads).toEqual([['m-1', '/home/admin/alpha.txt']]);

    // Nothing has been fetched yet: the job is still running.
    expect(anchorClick).not.toHaveBeenCalled();
    fx.progress[0]({
      data: { job_id: 'dl-1', machine_id: 'm-1', status: 'done', done: 10, total: 10 },
    });
    flushSync();
    expect(anchorClick).toHaveBeenCalledTimes(1);
    expect(anchorClick.mock.instances[0].getAttribute('href')).toBe(
      '/api/ssh-machines/m-1/transfers/dl-1/result'
    );

    // A reconnect that re-reads the same row must not download it twice.
    fx.queue = [{ id: 'dl-1', machine_id: 'm-1', status: 'done', done: 10, total: 10 }];
    await fx.open[0]();
    await settle();
    expect(anchorClick).toHaveBeenCalledTimes(1);
  }, 30000);

  it('never downloads a job the user did not ask for', async () => {
    fx.queue = [
      { id: 'old-1', machine_id: 'm-1', kind: 'download', status: 'done', done: 1, total: 1 },
    ];
    app = mount(FilesWorkspace, { target, props: { machineId: 'm-1' } });
    await settle();

    expect(files.jobs.get('old-1').status).toBe('done');
    expect(anchorClick).not.toHaveBeenCalled();
  }, 30000);
});
