import assert from "node:assert/strict";
import test from "node:test";

import { POST } from "../app/api/memory/agent-packet/route";

test("agent packet API forces the bounded compact synthesis contract", async () => {
  const originalFetch = globalThis.fetch;
  let requestBody: any = null;
  let requestedUrl = "";
  globalThis.fetch = async (input, init) => {
    requestedUrl = String(input);
    requestBody = JSON.parse(String(init?.body));
    return new Response(JSON.stringify({ ok: true, schema_id: "agent_packet.v1", evidence: [] }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };
  try {
    const response = await POST(new Request("http://127.0.0.1/api/memory/agent-packet", {
      method: "POST",
      body: JSON.stringify({ query: "release truth", project: "demo", output_mode: "full", hard_limit_tokens: 99999 }),
    }));
    assert.equal(response.status, 200);
    assert.match(requestedUrl, /\/memory\/synthesis-pack\/v2$/);
    assert.equal(requestBody.output_mode, "agent_packet.v1");
    assert.equal(requestBody.hard_limit_tokens, 4000);
    assert.equal(requestBody.query, "release truth");
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("agent packet API rejects missing queries before calling the gateway", async () => {
  const response = await POST(new Request("http://127.0.0.1/api/memory/agent-packet", {
    method: "POST",
    body: "{}",
  }));
  assert.equal(response.status, 400);
  assert.equal((await response.json()).error, "query_required");
});

test("agent packet API rejects non-object JSON", async () => {
  const response = await POST(new Request("http://127.0.0.1/api/memory/agent-packet", {
    method: "POST",
    body: "null",
  }));
  assert.equal(response.status, 400);
  assert.equal((await response.json()).error, "json_object_required");
});
