import test from "node:test";
import assert from "node:assert/strict";

import { GET } from "../app/api/memory/search/continuations/[token]/route";

test("continuation status API proxies token and query string", async () => {
  const originalFetch = globalThis.fetch;
  let requestedUrl = "";
  globalThis.fetch = async (input) => {
    requestedUrl = String(input);
    return new Response(
      JSON.stringify({
        ok: true,
        token: "cont-test",
        retrieval_progress: {
          schema_id: "retrieval_progress.v1",
          status: "completed",
          result_state: "ready",
        },
      }),
      {
        status: 200,
        headers: { "content-type": "application/json" },
      },
    );
  };

  try {
    const req = {
      nextUrl: new URL("http://127.0.0.1/api/memory/search/continuations/cont-test?include_result=false"),
    };
    const res = await GET(req as any, { params: Promise.resolve({ token: "cont-test" }) });
    assert.equal(res.status, 200);
    const body = await res.json();
    assert.equal(body.ok, true);
    assert.equal(body.retrieval_progress.schema_id, "retrieval_progress.v1");
    assert.match(requestedUrl, /\/memory\/search\/continuations\/cont-test\?include_result=false$/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
