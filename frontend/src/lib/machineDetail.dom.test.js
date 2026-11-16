import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { mount, unmount, flushSync } from 'svelte';

/**
 * MachineDetail's Files tab — which machines are offered one — and the
 * built-in host card's Settings tab.
 *
 * The server answers the file plane for ssh records and for the built-in
 * host card (an in-process session), and refuses it for everything else.
 * A tab that appears on a refused machine is a click that lands on a 400,
 * and a tab missing from the host card is the feature not shipping, so
 * both halves of that rule are pinned here rather than trusted to the
 * route mounting cleanly. The host card is also the one machine an admin
 * can store credentials for (auto-login + test), so its Settings tab and
 * the save call it makes are pinned too.
 */
const fx = vi.hoisted(() => ({ machine: null, updateMachineCalls: [] }));

vi.mock('$lib/stores/auth.svelte.js', () => {
  const api = {
    me: () => Promise.resolve({ username: 'admin', role: 'admin' }),
    getMachine: () => Promise.resolve(fx.machine),
    updateMachine: (id, body) => {
      fx.updateMachineCalls.push([id, body]);
      return Promise.resolve({
        ...fx.machine,
        user: body.user,
        auth_type: body.auth_type,
        notes: body.notes,
        has_password:
          body.auth_type === 'none'
            ? false
            : body.secret?.password
              ? true
              : !!fx.machine?.has_password,
      });
    },
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

vi.mock('$lib/router.svelte.js', () => ({
  navigate: vi.fn(),
  getRoute: () => ({ name: 'machines.detail', path: '/', params: {}, query: {} }),
}));

import MachineDetail from '../routes/MachineDetail.svelte';
import { setLocale } from '$lib/i18n.svelte.js';

const settle = async () => {
  for (let i = 0; i < 8; i++) {
    await new Promise((r) => setTimeout(r, 0));
    flushSync();
  }
};

const HOST = {
  id: 'host',
  name: 'webkvm host',
  protocol: 'local',
  host: 'localhost',
  builtin: true,
};
const SSH = {
  id: 'm-1',
  name: 'alpha',
  protocol: 'ssh',
  host: '10.0.0.1',
  port: 22,
  user: 'admin',
  host_key: 'AAAAB3NzaC1yc2E=',
};
const TELNET = {
  id: 'm-2',
  name: 'switch',
  protocol: 'telnet',
  host: '10.0.0.2',
  port: 23,
  user: 'admin',
};

let target, app;

beforeEach(() => {
  setLocale('en');
  localStorage.clear();
  fx.updateMachineCalls = [];
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(async () => {
  if (app) await unmount(app);
  target?.remove();
  app = null;
  vi.restoreAllMocks();
});

async function tabNames(machine) {
  fx.machine = machine;
  app = mount(MachineDetail, { target, props: { machineId: machine.id } });
  await settle();
  return [...target.querySelectorAll('[role="tab"]')].map((t) => t.textContent.trim());
}

describe('MachineDetail Files tab', () => {
  it('offers it for the built-in host card', async () => {
    const names = await tabNames(HOST);
    expect(names).toContain('Files');
    // The card is this machine: nothing to trust, but an admin can
    // store the account the getty terminal auto-logs in with.
    expect(names).toContain('Settings');
  }, 30000);

  it('offers it for an ssh machine', async () => {
    expect(await tabNames(SSH)).toContain('Files');
  }, 30000);

  it('keeps it away from a telnet machine, which has no filesystem', async () => {
    const names = await tabNames(TELNET);
    expect(names).not.toContain('Files');
    expect(names).toContain('Terminal');
  }, 30000);
});

describe('MachineDetail host card settings', () => {
  it('saves the stored account and password through updateMachine', async () => {
    fx.machine = { ...HOST, user: 'khl', auth_type: 'password', has_password: true };
    app = mount(MachineDetail, { target, props: { machineId: 'host' } });
    await settle();

    [...target.querySelectorAll('[role="tab"]')]
      .find((x) => x.textContent.trim() === 'Settings')
      .click();
    await settle();

    const user = target.querySelector('#md-host-user');
    const pass = target.querySelector('#md-host-password');
    expect(user).toBeTruthy();
    expect(pass).toBeTruthy();
    user.value = 'khl';
    user.dispatchEvent(new Event('input', { bubbles: true }));
    pass.value = 'hunter2';
    pass.dispatchEvent(new Event('input', { bubbles: true }));

    [...target.querySelectorAll('button')]
      .find((b) => b.textContent.trim() === 'Save changes')
      .click();
    await settle();

    expect(fx.updateMachineCalls).toHaveLength(1);
    const [id, body] = fx.updateMachineCalls[0];
    expect(id).toBe('host');
    expect(body).toMatchObject({
      user: 'khl',
      auth_type: 'password',
      secret: { password: 'hunter2' },
    });
  }, 30000);

  it('sends an empty secret when the password is left blank (keep)', async () => {
    fx.machine = { ...HOST, user: 'khl', auth_type: 'password', has_password: true };
    app = mount(MachineDetail, { target, props: { machineId: 'host' } });
    await settle();

    [...target.querySelectorAll('[role="tab"]')]
      .find((x) => x.textContent.trim() === 'Settings')
      .click();
    await settle();

    // The form comes up with the user pre-filled and a blank password;
    // saving without typing a new one must mean "keep the current one".
    expect(target.querySelector('#md-host-user').value).toBe('khl');
    expect(target.querySelector('#md-host-password').value).toBe('');

    [...target.querySelectorAll('button')]
      .find((b) => b.textContent.trim() === 'Save changes')
      .click();
    await settle();

    expect(fx.updateMachineCalls).toHaveLength(1);
    expect(fx.updateMachineCalls[0][1].secret).toEqual({});
  }, 30000);
});
