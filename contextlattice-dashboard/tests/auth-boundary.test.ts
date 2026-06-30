import test from "node:test";
import assert from "node:assert/strict";

import { dashboardAuthRequired } from "../lib/authMode";
import { GET as authGET } from "../app/api/auth/[...nextauth]/route";

test("dashboard auth is disabled by default for local OSS mode", async () => {
  assert.equal(dashboardAuthRequired(), false);

  const response = await authGET(
    new Request("http://127.0.0.1:3000/api/auth/session"),
    {},
  );
  assert.equal(response.status, 404);

  const body = await response.json();
  assert.equal(body.ok, false);
  assert.equal(body.error, "auth_disabled");
  assert.equal(body.authRequired, false);
});
