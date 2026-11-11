import test from "node:test";
import assert from "node:assert/strict";
import { createDashboardRefresh, type RefreshState } from "@/lib/dashboardRefresh";

function fixture(load: (signal: AbortSignal) => Promise<{ data: number; errors: string[] }>) {
  let now = 0;
  let visible = true;
  let id = 0;
  const timers = new Map<number, { at: number; fn: () => void }>();
  const states: RefreshState<number>[] = [];
  const controller = createDashboardRefresh(load, (state) => states.push(state), {
    initialData: 0, intervalMs: 300000, deadlineMs: 20000,
    now: () => now, visible: () => visible,
    setTimer: (fn, delay) => { timers.set(++id, { at: now + delay, fn }); return id; },
    clearTimer: (key) => { timers.delete(key as number); },
  });
  return {
    controller, states, timers,
    setVisible(value: boolean) { visible = value; controller.visibilityChanged(); },
    async advance(ms: number) {
      now += ms;
      for (const [key, timer] of [...timers]) {
        if (timer.at <= now) { timers.delete(key); timer.fn(); }
      }
      await new Promise<void>((resolve) => setImmediate(resolve));
    },
  };
}

test("refresh coalesces requests, polls every five minutes, and retains last good data on failure", async () => {
  let calls = 0;
  let fail = false;
  let resolve!: (value: { data: number; errors: string[] }) => void;
  const f = fixture(async () => {
    calls++;
    if (fail) throw new Error("offline");
    return new Promise((done) => { resolve = done; });
  });
  const first = f.controller.start();
  assert.equal(f.controller.refresh(), first);
  assert.equal(calls, 1);
  resolve({ data: 7, errors: [] });
  await first;
  assert.equal(f.states.at(-1)?.stale, false);
  assert.equal(f.states.at(-1)?.succeededAt, "1970-01-01T00:00:00.000Z");
  fail = true;
  await f.advance(299999);
  assert.equal(calls, 1);
  await f.advance(1);
  assert.equal(calls, 2);
  assert.equal(f.states.at(-1)?.data, 7);
  assert.equal(f.states.at(-1)?.stale, true);
  assert.equal(f.states.at(-1)?.succeededAt, "1970-01-01T00:00:00.000Z");
  assert.equal(f.states.at(-1)?.attemptedAt, "1970-01-01T00:05:00.000Z");
  assert.ok([...f.timers.values()].some((timer) => timer.at === 900000), "failure backs off to ten minutes");
  f.controller.stop();
});

test("hidden or stopped refreshes abort and late responses cannot overwrite newer data", async () => {
  const pending: { signal: AbortSignal; done: (value: { data: number; errors: string[] }) => void }[] = [];
  const f = fixture((signal) => new Promise((done) => pending.push({ signal, done })));
  const first = f.controller.start();
  f.setVisible(false);
  assert.equal(pending[0].signal.aborted, true);
  await f.advance(600000);
  assert.equal(pending.length, 1);
  f.setVisible(true);
  pending[1].done({ data: 22, errors: [] });
  await f.controller.refresh();
  pending[0].done({ data: 11, errors: [] });
  await first;
  assert.equal(f.states.at(-1)?.data, 22);
  const final = f.controller.refresh();
  const length = f.states.length;
  f.controller.stop();
  pending[2].done({ data: 33, errors: [] });
  await final;
  assert.equal(f.states.length, length);
  assert.equal(f.timers.size, 0);
});

test("deadline bounds an unresponsive fetch and partial refreshes never replace a complete snapshot", async () => {
  let count = 0;
  const f = fixture(async () => {
    count++;
    if (count === 1) return { data: 7, errors: [] };
    if (count === 2) return { data: 99, errors: ["one source failed"] };
    return new Promise(() => {});
  });
  await f.controller.start();
  await f.controller.refresh();
  assert.equal(f.states.at(-1)?.data, 7);
  assert.equal(f.states.at(-1)?.stale, true);
  const hanging = f.controller.refresh();
  await f.advance(20000);
  await hanging;
  assert.equal(f.states.at(-1)?.refreshing, false);
  assert.match(f.states.at(-1)?.errors.join() ?? "", /timed out/);
  assert.equal(f.states.at(-1)?.data, 7);
  f.controller.stop();
});

test("access-sensitive refresh clears prior counters after HTTP failure or a partial response", async () => {
  const states: RefreshState<number | null>[] = [];
  let mode = "success";
  const controller = createDashboardRefresh(async () => {
    if (mode === "denied") throw new Error("Unauthorized");
    if (mode === "partial") return { data: null, errors: ["source unavailable"] };
    return { data: 71, errors: [] };
  }, (state) => states.push(state), { initialData: null as number | null, retainLastGoodOnError: false });
  try {
    await controller.start();
    assert.equal(states.at(-1)?.data, 71);
    mode = "denied";
    await controller.refresh();
    assert.equal(states.at(-1)?.data, null);
    mode = "success";
    await controller.refresh();
    mode = "partial";
    await controller.refresh();
    assert.equal(states.at(-1)?.data, null);
    assert.equal(states.at(-1)?.stale, true);
  } finally { controller.stop(); }
});
