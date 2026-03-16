import test from "node:test";
import assert from "node:assert/strict";

import { GET } from "../app/api/overrides/latest/route";

test("overrides API returns orchestrator payload", async () => {
  const mockPayload = {
    overrides: [
      {
        symbol: "BONK",
        priority: "HIGH",
        reason: "momentum",
        size_before: 1,
        size_after: 2,
        confidence_before: 0.5,
        confidence_after: 0.9,
        override_strength: 0.8,
        multiplier: 1.5,
      },
    ],
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
    const body = await res.json();
    assert.equal(body.overrides.length, 1);
    assert.equal(body.overrides[0].symbol, "BONK");
  } finally {
    globalThis.fetch = originalFetch;
  }
});
