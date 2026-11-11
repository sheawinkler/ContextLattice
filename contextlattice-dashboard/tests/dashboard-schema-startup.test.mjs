import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

test("ordinary dashboard startup cannot authorize destructive schema synchronization", () => {
  const dockerfile = readFileSync(new URL("../../Dockerfile.dashboard", import.meta.url), "utf8");
  const startup = dockerfile.split("\n").find((line) => line.startsWith("CMD "));
  assert.ok(startup);
  assert.match(startup, /prisma db push --skip-generate && exec/);
  assert.doesNotMatch(startup, /--accept-data-loss|--force-reset/);
});
