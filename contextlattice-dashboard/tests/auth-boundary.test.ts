import test from "node:test";
import assert from "node:assert/strict";

import { dashboardAuthRequired } from "../lib/authMode";
import { GET as authGET } from "../app/api/auth/[...nextauth]/route";
import { endpointTargetsForMode } from "../lib/endpointCatalog";

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

test("dashboard auth requires explicit hosted opt-in", () => {
  const previous = process.env.AUTH_REQUIRED;
  process.env.AUTH_REQUIRED = "true";
  try {
    assert.equal(dashboardAuthRequired(), true);
  } finally {
    if (previous === undefined) {
      delete process.env.AUTH_REQUIRED;
    } else {
      process.env.AUTH_REQUIRED = previous;
    }
  }
});

test("runtime status probes respect local and hosted product boundaries", () => {
  const localPaths = endpointTargetsForMode("local").map((target) => target.path);
  const hostedPaths = endpointTargetsForMode("hosted").map((target) => target.path);

  assert.equal(localPaths.includes("/api/workspace/current"), false);
  assert.equal(hostedPaths.includes("/api/workspace/current"), true);
  assert.equal(localPaths.includes("/api/health"), true);
  assert.equal(hostedPaths.includes("/api/health"), true);
});
