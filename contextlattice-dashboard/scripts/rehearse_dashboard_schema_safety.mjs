#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { fileURLToPath } from "node:url";

const dashboard = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const args = process.argv.slice(2);
assert.equal(args.length, 2, "usage: --artifact-dir <new-directory>");
assert.equal(args[0], "--artifact-dir");
const root = resolve(args[1]);
mkdirSync(root, { mode: 0o700 });
const schema = readFileSync(join(dashboard, "prisma/schema.prisma"), "utf8");
const dockerfile = readFileSync(join(dashboard, "../Dockerfile.dashboard"), "utf8");
const startup = dockerfile.split("\n").find((line) => line.startsWith("CMD "));
assert.ok(startup?.includes("prisma db push --skip-generate && exec"));
assert.doesNotMatch(startup, /--accept-data-loss|--force-reset/);
const hash = (input) => createHash("sha256").update(input).digest("hex");
const receipt = {
  schema_id: "dashboard_schema_safety.v1", ok: false, production_deployment_claim: false,
  node: process.version, schema_sha256: hash(schema), dockerfile_sha256: hash(dockerfile), checks: [],
};
const database = join(root, "fixture.db");
const backup = join(root, "backup.db");
const restored = join(root, "restored.db");

function push(name, model, databasePath, expectedStatus = 0) {
  const schemaPath = join(root, `${name}.prisma`);
  writeFileSync(schemaPath, model);
  const result = spawnSync(process.execPath, [join(dashboard, "node_modules/prisma/build/index.js"), "db", "push", "--schema", schemaPath, "--skip-generate"], {
    cwd: dashboard, env: { ...process.env, DATABASE_URL: `file:${databasePath}` },
    encoding: "utf8", timeout: 30000, maxBuffer: 1024 * 1024, input: "",
  });
  writeFileSync(join(root, `${name}.log`), `${result.stdout || ""}\n${result.stderr || ""}`);
  if (result.error) throw result.error;
  if (expectedStatus === 0) assert.equal(result.status, 0, `${name}: schema synchronization failed; inspect retained log`);
  else {
    assert.notEqual(result.status, 0, "destructive synchronization unexpectedly succeeded");
    assert.match(`${result.stdout}\n${result.stderr}`, /data loss|accept-data-loss/i);
  }
  return result.status;
}

function user(databasePath) {
  const db = new DatabaseSync(databasePath, { readOnly: true });
  try {
    return JSON.stringify(db.prepare('SELECT "id", "email", "name" FROM "User" WHERE "id" = ?').get("schema-proof"));
  } finally {
    db.close();
  }
}

try {
  push("fresh", schema, database);
  const db = new DatabaseSync(database);
  try {
    db.prepare('INSERT INTO "User" ("id", "email", "name", "updatedAt") VALUES (?, ?, ?, ?)')
      .run("schema-proof", "schema-proof@example.invalid", "Retained fixture", new Date().toISOString());
    db.prepare("VACUUM INTO ?").run(backup);
  } finally {
    db.close();
  }
  const original = user(database);
  assert.equal(user(backup), original);
  receipt.checks.push({ name: "fresh_install_and_backup", user_sha256: hash(original) });
  const destructive = schema.replace(/^  name\s+String\?\r?\n/m, "");
  assert.notEqual(destructive, schema, "expected User.name field is absent; refresh the fixture deliberately");
  const rejection = push("destructive", destructive, database, 1);
  assert.equal(user(database), original);
  receipt.checks.push({ name: "populated_column_drop_rejected", exit_code: rejection, data_retained: true });
  const additive = `${schema}\nmodel SchemaSafetyProbe {\n  id String @id\n  message String?\n}\n`;
  push("additive", additive, database);
  assert.equal(user(database), original);
  receipt.checks.push({ name: "additive_upgrade", data_retained: true });
  // Restore into a fresh destination; no live database or fixture sidecars are deleted.
  copyFileSync(backup, restored);
  push("rollback", schema, restored);
  assert.equal(user(restored), original);
  receipt.checks.push({ name: "backup_restore_original_schema", data_retained: true });
  receipt.ok = true;
} catch (error) {
  receipt.error = String(error).slice(-4000);
} finally {
  writeFileSync(join(root, "receipt.json"), `${JSON.stringify(receipt, null, 2)}\n`);
}
process.stdout.write(`${JSON.stringify(receipt)}\n`);
process.exitCode = receipt.ok ? 0 : 1;
