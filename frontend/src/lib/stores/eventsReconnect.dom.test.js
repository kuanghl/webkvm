/**
 * Regression: the SSE client used to settle into a permanent ~1s
 * reconnect loop after the first network blip. `_open()` closes the
 * old EventSource before creating the replacement, and close() fires
 * that dead source's 'error' event (browser contract). The stale
 * source's error handler then scheduled another reconnect, which
 * closed the healthy replacement, whose ghost error scheduled the
 * next one — forever. Each 'open' reset the backoff, so the cycle
 * never got slower than 1s, and every event broadcast in the gaps
 * was lost for good (a missed vm.state shutoff is exactly the stale
 * "N running" badge the onOpen resync exists to heal).
 *
 * The fake below reproduces the browser contract that bit us:
 * close() dispatches one 'error' event, asynchronously.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

class FakeEventSource {
  static instances = [];
  readyState = 0;
  closed = false;
  listeners = {};
  constructor(url) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] || []).filter((f) => f !== fn);
  }
  close() {
    if (this.closed) return;
    this.closed = true;
    this.readyState = 2;
    // Browsers fire the error event after close(), off the current task.
    queueMicrotask(() => this.fire('error'));
  }
  fire(type) {
    for (const fn of this.listeners[type] || []) fn();
  }
}

// Flushes queued microtasks (the fake's ghost errors) without touching timers.
async function flushMicrotasks() {
  for (let i = 0; i < 3; i++) await Promise.resolve();
}

describe('events store reconnect lifecycle', () => {
  let events;
  let auth;

  beforeEach(async () => {
    vi.useFakeTimers();
    FakeEventSource.instances = [];
    vi.stubGlobal('EventSource', FakeEventSource);
    vi.resetModules();
    ({ events } = await import('./events.svelte.js'));
    ({ auth } = await import('./auth.svelte.js'));
    auth.setSession('tester', 'admin');
  });

  afterEach(() => {
    events.disconnect();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('a ghost error from a replaced source must not schedule another reconnect', async () => {
    events.connect();
    const es1 = FakeEventSource.instances[0];
    expect(FakeEventSource.instances).toHaveLength(1);

    // A real failure of the live source schedules exactly one manual
    // reconnect (1s backoff, attempts = 1).
    es1.fire('error');
    expect(events.reconnecting).toBe(true);

    // The timer fires: es1 is closed (queueing its ghost error) and a
    // replacement source is created.
    await vi.advanceTimersByTimeAsync(1000);
    const es2 = FakeEventSource.instances[1];
    expect(es1.closed).toBe(true);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(events._es).toBe(es2);

    // es1's close() ghost error now runs. Pre-fix it scheduled a second
    // reconnect one second later, which would have closed the healthy
    // es2 and started the cycle again. The store must stay quiet.
    await vi.advanceTimersByTimeAsync(1000);
    expect(events._es).toBe(es2);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(events._reconnectTimer).toBeNull();
  });

  it('a live open clears the pending manual reconnect', async () => {
    events.connect();
    const es1 = FakeEventSource.instances[0];

    es1.fire('error');
    expect(events._reconnectTimer).not.toBeNull();

    // The browser auto-reconnected before the manual timer fired: the
    // socket is up, so the queued reconnect is redundant and must not
    // tear the fresh connection down one second later.
    es1.fire('open');
    expect(events.connected).toBe(true);
    expect(events.reconnecting).toBe(false);
    expect(events._reconnectTimer).toBeNull();

    await vi.advanceTimersByTimeAsync(3000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(events._es).toBe(es1);
  });

  it('fires onOpen on every (re)open and stops after unsubscribe', async () => {
    events.connect();
    const es1 = FakeEventSource.instances[0];

    let opens = 0;
    const off = events.onOpen(() => {
      opens += 1;
    });

    es1.fire('open');
    expect(opens).toBe(1);

    // A blip and a replacement socket must trigger the hook again —
    // that is the resync point for every stale-state consumer.
    es1.fire('error');
    await vi.advanceTimersByTimeAsync(1000);
    const es2 = FakeEventSource.instances[1];
    es2.fire('open');
    expect(opens).toBe(2);

    off();
    es2.fire('error');
    await vi.advanceTimersByTimeAsync(1000);
    const es3 = FakeEventSource.instances[2];
    es3.fire('open');
    expect(opens).toBe(2);
  });

  it('a deliberate disconnect leaves no reconnect armed behind', async () => {
    events.connect();
    const es1 = FakeEventSource.instances[0];

    events.disconnect();
    expect(es1.closed).toBe(true);

    // es1's close() ghost error runs against this._es === null: it must
    // be ignored, not turned into a brand-new connection.
    await flushMicrotasks();
    await vi.advanceTimersByTimeAsync(3000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(events._reconnectTimer).toBeNull();
    expect(events.connected).toBe(false);
  });
});
