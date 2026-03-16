import test from "node:test";
import assert from "node:assert/strict";

import { GET } from "../app/api/telemetry/sidecar-health/route";

test("sidecar health API proxies orchestrator", async () => {
  const mockPayload = {
    updatedAt: "2025-01-01T00:00:00Z",
    healthy: true,
    detail: "mode full",
    history: [],
  };
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () =>
    new Response(JSON.stringify(mockPayload), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  try {
    const res = await GET();
    assert.equal(res.status, 200);
    const json = await res.json();
    assert.equal(json.healthy, true);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
