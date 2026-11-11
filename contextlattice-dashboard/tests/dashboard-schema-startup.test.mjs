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

test("dashboard artifact preserves source provenance in both build stages", () => {
  const dockerfile = readFileSync(new URL("../../Dockerfile.dashboard", import.meta.url), "utf8");
  for (const [name, label] of Object.entries({
    CONTEXTLATTICE_BUILD_VERSION: "org.opencontainers.image.version",
    CONTEXTLATTICE_BUILD_CHANNEL: "io.contextlattice.build-channel",
    CONTEXTLATTICE_SOURCE_COMMIT: "org.opencontainers.image.revision",
    CONTEXTLATTICE_SOURCE_TREE: "io.contextlattice.source-tree",
  })) {
    assert.equal((dockerfile.match(new RegExp(`^ARG ${name}(?:=.*)?$`, "gm")) || []).length, 3, name);
    assert.equal(dockerfile.split("ENV " + name + "=${" + name + "}").length - 1, 2, name);
    assert.ok(dockerfile.includes(label + '="${' + name + '}"'), label);
  }
});
