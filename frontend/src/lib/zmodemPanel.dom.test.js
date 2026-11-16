import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

// The ZMODEM client owns the state, the panel only draws it, so most of
// this file is about the client's contract with the terminal socket: what
// is sent, what a frame does to the banner, and what the browser is told
// to download when a batch lands.

const RESULT_URL = '/api/ssh-machines/m-9/transfers/job-1/result';

vi.mock('$lib/stores/auth.svelte.js', () => ({
  api: {
    machineTransferResultUrl: (id, job) => `/api/ssh-machines/${id}/transfers/${job}/result`,
    machineZmodemStage: (id, items) => {
      h.stage.push({ id, names: items.map((it) => it.name), sizes: items.map((it) => it.size) });
      if (h.fail) return Promise.reject(h.fail);
      return Promise.resolve({
        upload_id: 'u-1',
        files: items.map((it) => ({ name: it.name.split('/').pop(), size: it.size })),
        total: items.reduce((n, it) => n + it.size, 0),
      });
    },
  },
  auth: { isAdmin: () => true, isOperator: () => true, isViewer: () => false },
}));

// The real store opens an EventSource; the client only needs the job map
// and the idempotent subscription.
const h = vi.hoisted(() => ({ jobs: new Map(), sse: [], stage: [], fail: null }));
vi.mock('$lib/stores/files.svelte.js', () => ({
  files: {
    jobs: h.jobs,
    ensureSSE: () => h.sse.push(1),
  },
}));

import { ZmodemClient } from '$lib/components/machine/zmodemClient.svelte.js';
import ZmodemPanel from '$lib/components/machine/ZmodemPanel.svelte';

const file = (name, body = 'abc') => new File([body], name);
const dialog = () => [...document.body.querySelectorAll('[data-slot="dialog-content"]')].at(-1);

const settle = async () => {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
};

let sent;
let client;

beforeEach(() => {
  h.jobs.clear();
  h.sse.length = 0;
  h.stage.length = 0;
  h.fail = null;
  sent = [];
  client = new ZmodemClient('m-9');
  client.bind((frame) => sent.push(frame));
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('ZmodemClient', () => {
  it('refuses an empty request without touching the socket', async () => {
    client.input = '   \n  ';
    client.start();
    expect(sent).toEqual([]);
    expect(client.error).toMatch(/at least one path/i);
  }, 30000);

  it('sends one path per line, with the recursion flag', async () => {
    client.input = '/var/log/messages\n/etc/nginx/nginx.conf  \n\n';
    client.recursive = true;
    client.start();
    expect(sent).toHaveLength(1);
    expect(sent[0]).toEqual({
      type: 'zmodem',
      paths: ['/var/log/messages', '/etc/nginx/nginx.conf'],
      recursive: true,
    });
    expect(client.open).toBe(false);
    expect(client.kind).toBe('running');
    // The queue's progress feed is what keeps the byte counter alive.
    expect(h.sse).toHaveLength(1);
  }, 30000);

  it('reports a dead socket instead of dropping the request', async () => {
    client.bind(null);
    client.input = 'x';
    client.start();
    expect(sent).toEqual([]);
    expect(client.error).toMatch(/reconnect/i);
  }, 30000);

  it('takes the job id from the armed frame and reads progress off the queue', async () => {
    client.input = 'big.iso';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1', type: 'zmodem' });
    expect(client.job).toBe('job-1');
    h.jobs.set('job-1', { id: 'job-1', status: 'running', done: 2048, total: 4096 });
    expect(client.sizeLabel).toBe('2.0 KB / 4.0 KB');
    h.jobs.set('job-1', {
      id: 'job-1',
      status: 'running',
      done: 2048,
      total: 4096,
      files_total: 3,
      files_done: 1,
      current_path: 'logs/boot.log',
    });
    expect(client.fileLabel).toBe('1/3');
    expect(client.currentLabel).toBe('logs/boot.log');
  }, 30000);

  it('downloads a lone file under its own name and stops showing a live job', async () => {
    client.input = 'messages';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    const clicks = [];
    const realCreate = document.createElement.bind(document);
    vi.spyOn(document, 'createElement').mockImplementation((tag) => {
      const el = realCreate(tag);
      if (tag === 'a')
        el.click = () => clicks.push({ href: el.getAttribute('href'), dl: el.download });
      return el;
    });
    client.frame({ event: 'done', job_id: 'job-1', files: ['messages'] });
    expect(clicks).toEqual([{ href: RESULT_URL, dl: 'messages' }]);
    expect(client.kind).toBe('done');
    expect(client.notice).toMatch(/1 file\(s\) received/);
    expect(client.job).toBe('');
  }, 30000);

  it('lets a batch be saved as the tar.gz the server builds, without a filename claim', async () => {
    const realCreate = document.createElement.bind(document);
    const clicks = [];
    vi.spyOn(document, 'createElement').mockImplementation((tag) => {
      const el = realCreate(tag);
      if (tag === 'a') el.click = () => clicks.push(el.download);
      return el;
    });
    client.input = '/var/log';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    client.frame({ event: 'done', job_id: 'job-1', files: ['messages', 'nested/boot.log'] });
    expect(clicks).toEqual(['']);
    expect(client.notice).toMatch(/2 file\(s\) received/);
  }, 30000);

  it('explains a silent remote as the lrzsz case it usually is', async () => {
    client.input = 'x';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    client.frame({ event: 'disarmed', job_id: 'job-1', reason: 'no-frame' });
    expect(client.kind).toBe('error');
    expect(client.notice).toMatch(/lrzsz/);
    expect(client.job).toBe('');
  }, 30000);

  it('keeps a refused request inside the dialog, where the user can fix it', async () => {
    client.open = true;
    client.frame({ event: 'error', detail: 'another ZMODEM transfer is already running' });
    expect(client.error).toMatch(/already running/);
    expect(client.kind).toBe('error');
  }, 30000);

  it('shows a failed transfer with the server’s reason', async () => {
    client.input = 'x';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    client.frame({ event: 'error', job_id: 'job-1', error: 'zmodem: bad checksum' });
    expect(client.notice).toMatch(/bad checksum/);
    expect(client.error).toBe('');
  }, 30000);

  it('cancels over the same socket', async () => {
    client.input = 'x';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    client.cancel();
    expect(sent).toContainEqual({ type: 'zmodem', action: 'cancel' });
  }, 30000);

  it('treats a reconnect as the end of a transfer it can no longer see', async () => {
    client.input = 'x';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    client.bind(null);
    expect(client.kind).toBe('error');
    expect(client.notice).toMatch(/terminal closed/i);
    expect(client.job).toBe('');
  }, 30000);

  it('says so when a batch finished with nothing in it', async () => {
    client.input = 'x';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    client.frame({ event: 'done', job_id: 'job-1', files: [] });
    expect(client.notice).toMatch(/no file arrived/);
  }, 30000);

  it('refuses to arm an upload with nothing picked', async () => {
    client.mode = 'up';
    await client.start();
    expect(sent).toEqual([]);
    expect(h.stage).toHaveLength(0);
    expect(client.error).toMatch(/at least one file/i);
  }, 30000);

  it('posts the batch to this host before it asks the line for anything', async () => {
    client.mode = 'up';
    client.pick([{ file: file('notes.txt'), relPath: 'docs/notes.txt', size: 3 }]);
    await client.start();
    expect(h.stage).toEqual([{ id: 'm-9', names: ['docs/notes.txt'], sizes: [3] }]);
    expect(sent).toEqual([{ type: 'zmodem', dir: 'up', upload_id: 'u-1' }]);
    expect(client.staged).toBe(100);
    expect(client.kind).toBe('running');
  }, 30000);

  it('refuses a batch larger than one rz sequence can carry', async () => {
    client.mode = 'up';
    client.pick(Array.from({ length: 33 }, (_, i) => file(`f${i}.dat`)));
    expect(client.picked).toHaveLength(0);
    expect(client.error).toMatch(/32/);
  }, 30000);

  it('reports a staging failure without touching the terminal', async () => {
    h.fail = new Error('upload exceeds the 4 GiB limit');
    client.mode = 'up';
    client.pick([file('big.bin', 'x')]);
    await client.start();
    expect(sent).toEqual([]);
    expect(client.kind).toBe('error');
    expect(client.notice).toMatch(/4 GiB/);
    // The pick stands: the bytes never left the browser, so the user can
    // fix whatever the server complained about and press again.
    expect(client.picked).toHaveLength(1);
  }, 30000);

  it('keeps a stage the busy line refused, so a retry skips the post', async () => {
    client.mode = 'up';
    client.pick([file('notes.txt')]);
    await client.start();
    client.frame({ event: 'error', detail: 'another ZMODEM transfer is already running' });
    await client.start();
    expect(h.stage).toHaveLength(1);
    expect(sent.filter((f) => f.dir === 'up')).toHaveLength(2);
  }, 30000);

  it('forgets the batch once the terminal has taken it, and downloads nothing', async () => {
    const clicks = [];
    const realCreate = document.createElement.bind(document);
    vi.spyOn(document, 'createElement').mockImplementation((tag) => {
      const el = realCreate(tag);
      if (tag === 'a') el.click = () => clicks.push(el.getAttribute('href'));
      return el;
    });
    client.mode = 'up';
    client.pick([file('a.txt'), file('b.txt')]);
    await client.start();
    client.frame({ event: 'armed', job_id: 'job-1', dir: 'up' });
    client.frame({ event: 'done', job_id: 'job-1' });
    expect(clicks).toEqual([]);
    expect(client.notice).toMatch(/Sent 2 file/);
    expect(client.picked).toHaveLength(0);
    expect(client.stage).toBe(null);
  }, 30000);
});

describe('ZmodemPanel', () => {
  let host;

  beforeEach(() => {
    host = document.createElement('div');
    document.body.appendChild(host);
  });

  afterEach(() => {
    unmount(host);
    host.remove();
  });

  async function render(c) {
    const comp = mount(ZmodemPanel, { target: host, props: { client: c } });
    await settle();
    return comp;
  }

  it('renders the strip with its counters and the cancel action', async () => {
    client.input = '/var/log';
    client.start();
    client.frame({ event: 'armed', job_id: 'job-1' });
    h.jobs.set('job-1', {
      id: 'job-1',
      status: 'running',
      done: 1024,
      total: 4096,
      files_total: 2,
      files_done: 1,
      current_path: 'messages',
    });
    await render(client);
    const strip = host.querySelector('[role="status"]');
    expect(strip.textContent).toContain('Waiting for the remote');
    expect(strip.textContent).toContain('1/2');
    expect(strip.textContent).toContain('messages');
    expect(strip.textContent).toContain('1.0 KB / 4.0 KB');
    const cancel = [...host.querySelectorAll('button')].find((b) => /cancel/i.test(b.textContent));
    cancel.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await settle();
    expect(sent).toContainEqual({ type: 'zmodem', action: 'cancel' });
  }, 30000);

  it('switches the dialog between the two directions', async () => {
    client.open = true;
    await render(client);
    expect(dialog().querySelector('textarea')).toBeTruthy();
    const tab = [...dialog().querySelectorAll('button')].find(
      (b) => b.textContent.trim() === 'Send to the machine (rz)'
    );
    tab.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await settle();
    expect(client.mode).toBe('up');
    expect(dialog().querySelector('textarea')).toBeNull();
    expect(dialog().querySelector('input[type="file"]')).toBeTruthy();
  }, 30000);

  it('will not send an upload until something is picked', async () => {
    client.open = true;
    client.mode = 'up';
    await render(client);
    const send = [...dialog().querySelectorAll('button')].find(
      (b) => b.textContent.trim() === 'Send rz'
    );
    expect(send.disabled).toBe(true);
    client.pick([file('notes.txt'), file('boot.log', 'x')]);
    await settle();
    expect(send.disabled).toBe(false);
    const body = dialog().textContent;
    expect(body).toContain('notes.txt');
    expect(body).toContain('2 file(s)');
  }, 30000);

  it('disappears when dismissed', async () => {
    client.kind = 'done';
    client.notice = 'all done';
    const comp = await render(client);
    expect(host.querySelector('[role="status"]')).toBeTruthy();
    client.dismiss();
    await settle();
    expect(host.querySelector('[role="status"]')).toBeNull();
    expect(comp).toBeTruthy();
  }, 30000);
});
