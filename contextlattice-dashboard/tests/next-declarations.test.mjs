import test from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);

test("locked Next generator reproduces committed app type declarations", async (t) => {
  const root = fileURLToPath(new URL("../", import.meta.url));
  const packageJson = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8"));
  const installed = require("next/package.json");
  assert.equal(installed.version, packageJson.dependencies.next);
  assert.equal(installed.version, lock.packages["node_modules/next"].version);
  const { default: loadConfig } = require("next/dist/server/config");
  const { PHASE_PRODUCTION_BUILD } = require("next/constants");
  const { getBuildDistDir } = require("next/dist/export/utils");
  const config = await loadConfig(PHASE_PRODUCTION_BUILD, root, { silent: true });
  const { writeAppTypeDeclarations } = require("next/dist/lib/typescript/writeAppTypeDeclarations");
  const directory = mkdtempSync(join(tmpdir(), "next-declarations-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const output = join(directory, "next-env.d.ts");
  const committed = readFileSync(join(root, "next-env.d.ts"), "utf8");
  // The framework generator, not a copied golden string, owns these bytes.
  const optionsFor = (normalized) => ({
    baseDir: directory,
    distDir: getBuildDistDir(normalized),
    imageImportsEnabled: !normalized.images.disableStaticImages,
    hasPagesDir: existsSync(join(root, "pages")) || existsSync(join(root, "src/pages")),
    hasAppDir: existsSync(join(root, "app")) || existsSync(join(root, "src/app")),
    strictRouteTypes: Boolean(normalized.experimental.strictRouteTypes),
    typedRoutes: normalized.typedRoutes,
  });
  const options = optionsFor(config);
  writeFileSync(output, "// stale generated declaration\n");
  await writeAppTypeDeclarations(options);
  assert.equal(readFileSync(output, "utf8"), committed);
  assert.notEqual(committed.replace(/import .*root-params\.d\.ts.*\n/, ""), committed);
  await writeAppTypeDeclarations({ ...options, distDir: "different-output" });
  assert.notEqual(readFileSync(output, "utf8"), committed);
  await writeAppTypeDeclarations({ ...options, imageImportsEnabled: !options.imageImportsEnabled });
  assert.notEqual(readFileSync(output, "utf8"), committed);
  const strictRoot = join(directory, "async-config");
  mkdirSync(strictRoot);
  writeFileSync(join(strictRoot, "next.config.mjs"),
    "export default async () => ({ experimental: { strictRouteTypes: true }, typedRoutes: true });\n");
  const strict = await loadConfig(PHASE_PRODUCTION_BUILD, strictRoot, { silent: true });
  await writeAppTypeDeclarations(optionsFor(strict));
  const strictContent = readFileSync(output, "utf8");
  assert.match(strictContent, /types\/cache-life\.d\.ts/);
  assert.match(strictContent, /types\/validator\.ts/);
  assert.match(strictContent, /types\/link\.d\.ts/);
  assert.notEqual(strictContent, committed);
  const exportRoot = join(directory, "export-config");
  mkdirSync(exportRoot);
  writeFileSync(join(exportRoot, "next.config.mjs"),
    'export default () => ({ output: "export", distDir: "exported-site" });\n');
  const exported = await loadConfig(PHASE_PRODUCTION_BUILD, exportRoot, { silent: true });
  assert.equal(exported.distDir, "exported-site");
  assert.equal(getBuildDistDir(exported), ".next");
  await writeAppTypeDeclarations(optionsFor(exported));
  assert.equal(readFileSync(output, "utf8"), committed);
});
