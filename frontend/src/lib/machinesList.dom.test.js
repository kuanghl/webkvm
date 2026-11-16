import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

// The built-in host record (protocol "local") and one SSH machine —
// the exact shape /api/ssh-machines returns to an admin.
const HOST = {
  id: 'host',
  name: 'ubuntu-test',
  protocol: 'local',
  host: 'localhost',
  port: 0,
  user: '',
  auth_type: 'none',
  builtin: true,
  created_at: '0001-01-01T00:00:00Z',
  updated_at: '0001-01-01T00:00:00Z',
  has_password: false,
  has_key: false,
};
const SSH = {
  id: 'm-1',
  name: 'router',
  protocol: 'ssh',
  host: '192.168.4.155',
  port: 22,
  user: 'admin',
  auth_type: 'password',
  host_key: 'AAAAC3',
  created_at: '2026-11-11T03:46:58Z',
  updated_at: '2026-11-11T03:46:58Z',
  has_password: true,
  has_key: false,
};

vi.mock('$lib/stores/auth.svelte.js', () => {
  const api = new Proxy(
    {
      me: () => Promise.resolve({ username: 'admin', role: 'admin' }),
      listMachines: () => Promise.resolve([HOST, SSH]),
    },
    {
      get(target, prop) {
        if (prop in target) return target[prop];
        if (typeof prop !== 'string') return undefined;
        return () => Promise.resolve([]);
      },
    }
  );
  return {
    api,
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

const navigate = vi.fn();
vi.mock('$lib/router.svelte.js', () => ({
  navigate: (...a) => navigate(...a),
  getRoute: () => ({ name: 'machines', path: '/machines', params: {}, query: {} }),
}));

const settle = async () => {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
};

describe('MachinesList with the built-in host machine', () => {
  let target, app;

  beforeEach(() => {
    target = document.createElement('div');
    document.body.appendChild(target);
  });

  afterEach(async () => {
    if (app) await unmount(app);
    target?.remove();
    app = null;
    navigate.mockClear();
  });

  async function mountList() {
    const mod = await import('../routes/MachinesList.svelte');
    app = mount(mod.default, { target });
    await settle();
    return target;
  }

  // A StatCard fed with a raw number used to throw "snippet is not a
  // function" mid-render, which killed the whole card grid: the page
  // showed its header and stats but no machines at all, and clicking
  // anywhere did nothing.
  it('renders one card per machine after the stats load', async () => {
    const el = await mountList();
    const cards = [...el.querySelectorAll('[role="button"]')].filter((d) =>
      d.className.includes('group')
    );
    expect(cards.length).toBe(2);
  }, 30000);

  it('counts the built-in machine in the protocol buckets', async () => {
    const el = await mountList();
    const subtitle = el.querySelector('h1')?.nextElementSibling?.textContent || '';
    expect(subtitle).toContain('2');
    expect(subtitle).toContain('1 SSH');
    expect(subtitle).toContain('1 Local');
  }, 30000);

  it('navigates to the built-in host terminal', async () => {
    const el = await mountList();
    const hostCard = [...el.querySelectorAll('[role="button"]')].find((d) =>
      d.textContent.includes('ubuntu-test')
    );
    expect(hostCard).toBeTruthy();
    hostCard.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await settle();
    expect(navigate).toHaveBeenCalledWith('/machines/host');
  }, 30000);
});
