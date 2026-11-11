import test from "node:test";
import assert from "node:assert/strict";
import { GET } from "@/app/api/telemetry/overview/route";

test("memory overview only fetches its eight consumed aggregates with source-age metadata", async () => {
  const original = globalThis.fetch;
  const calls: string[] = [];
  globalThis.fetch = async (input, init) => {
    calls.push(String(input));
    assert.ok(init?.signal, "upstream calls must be cancellable");
    return Response.json({ updatedAt: "2026-01-01T00:00:00Z" });
  };
  try {
    const result = await (await GET(new Request("http://localhost/api/telemetry/overview?view=memory"))).json();
    assert.equal(calls.length, 8);
    assert.ok(calls.every((url) => !/strategies|trading|sentrux|invocations|recall\/monitor/.test(url)));
    assert.equal(result.sourceFreshness.status.sourceAt, "2026-01-01T00:00:00Z");
    assert.ok(result.sourceFreshness.status.attemptedAt);
    assert.ok(result.sourceFreshness.status.succeededAt);
    assert.notEqual(result.sourceFreshness.status.succeededAt, result.sourceFreshness.status.sourceAt);
    assert.match(result.snapshotId, /^[a-f0-9-]{36}$/);
    assert.equal(result.snapshotAtomic, false);
  } finally { globalThis.fetch = original; }
});

test("failed upstream fetch is not a successful freshness observation", async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async (input) => String(input).endsWith("/status")
    ? new Response("offline", { status: 503 }) : Response.json({});
  try {
    const result = await (await GET(new Request("http://localhost/api/telemetry/overview?view=memory"))).json();
    assert.ok(result.errors.status);
    assert.equal(result.freshness.status, null);
    assert.equal(result.sourceFreshness.status.succeededAt, null);
    assert.equal(result.sourceFreshness.status.sourceAt, null);
    assert.ok(result.sourceFreshness.status.attemptedAt);
  } finally { globalThis.fetch = original; }
});

test("specialist default retains full coverage and cancellation reaches all upstreams", async () => {
  const original = globalThis.fetch;
  const signals: AbortSignal[] = [];
  const controller = new AbortController();
  globalThis.fetch = async (_input, init) => new Promise((_resolve, reject) => {
    signals.push(init!.signal!);
    init!.signal!.addEventListener("abort", () => reject(new Error("fixture aborted")), { once: true });
  });
  try {
    const pending = GET(new Request("http://localhost/api/telemetry/overview", { signal: controller.signal }));
    assert.equal(signals.length, 21);
    controller.abort();
    const result = await (await pending).json();
    assert.ok(signals.every((signal) => signal.aborted));
    assert.equal(Object.keys(result.errors).length, 21);
    assert.ok(Object.values(result.freshness).every((value) => value === null));
  } finally { globalThis.fetch = original; }
});
