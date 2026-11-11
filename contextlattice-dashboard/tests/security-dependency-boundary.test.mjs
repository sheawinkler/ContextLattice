import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const root = new URL("../", import.meta.url);
const manifest = JSON.parse(readFileSync(new URL("package.json", root), "utf8"));
const lock = JSON.parse(readFileSync(new URL("package-lock.json", root), "utf8"));
const floors = {
  next: "16.4.1",
  sharp: "0.35.5",
  nodemailer: "10.1.4",
  "brace-expansion": "5.0.12",
  nanoid: "3.3.18",
  "source-map-js": "1.2.2",
  "baseline-browser-mapping": "2.11.0",
  qs: "6.16.1",
};

function atLeast(version, floor) {
  if (!/^\d+\.\d+\.\d+$/.test(version)) return false;
  const actual = version.split(".").map(Number);
  const minimum = floor.split(".").map(Number);
  for (let i = 0; i < 3; i += 1) {
    if (actual[i] !== minimum[i]) return actual[i] > minimum[i];
  }
  return true;
}

test("locked dependency graph meets reviewed security floors for every instance", () => {
  for (const [name, floor] of Object.entries(floors)) {
    const entries = Object.entries(lock.packages).filter(([path]) => path.endsWith(`node_modules/${name}`));
    if (["next", "sharp", "nodemailer"].includes(name)) {
      assert.ok(entries.length, `${name} must be represented in the public dashboard lock`);
    }
    for (const [path, entry] of entries) {
      assert.ok(atLeast(entry.version, floor), `${path}@${entry.version} is below ${floor}`);
    }
  }
  assert.equal(manifest.overrides["stream-json"], undefined, "do not force an incompatible stream-json major");
  assert.ok(!Object.keys(lock.packages).some((path) => path.endsWith("node_modules/stream-json")), "remove the affected parser through a tested parent, not an incompatible override");
});

test("manifest and lock retain framework boundaries and strict script approval", () => {
  assert.deepEqual(lock.packages[""].dependencies, manifest.dependencies);
  assert.deepEqual(lock.packages[""].devDependencies, manifest.devDependencies);
  assert.equal(manifest.dependencies["next-auth"], "4.24.15");
  assert.equal(manifest.dependencies.react, "18.3.1");
  assert.equal(manifest.dependencies["react-dom"], "18.3.1");
  assert.equal(lock.packages["node_modules/@prisma/client"].version, "5.22.0");
  assert.equal(lock.packages["node_modules/prisma"].version, "5.22.0");
  const next = lock.packages["node_modules/next"].version;
  assert.equal(lock.packages["node_modules/@next/env"].version, next);
  for (const [path, entry] of Object.entries(lock.packages)) {
    if (path.includes("node_modules/@next/swc-")) assert.equal(entry.version, next, path);
    if (entry.hasInstallScript) {
      const name = path.split("node_modules/").at(-1);
      assert.equal(manifest.allowScripts[`${name}@${entry.version}`], true, `unreviewed lifecycle: ${name}@${entry.version}`);
    }
  }
  assert.match(readFileSync(new URL(".npmrc", root), "utf8"), /^strict-allow-scripts=true$/m);
});

test("public graph excludes dependencies owned by other editions", () => {
  for (const name of ["@solana/web3.js", "@uwdata/mosaic-spec", "@uwdata/vgplot", "embedding-atlas", "stripe", "jayson", "ts-json-schema-generator"]) {
    assert.equal(manifest.dependencies[name], undefined, name);
    assert.ok(!Object.keys(lock.packages).some((path) => path.endsWith(`node_modules/${name}`)), name);
  }
});

test("normal test command retains metric, schema, refresh, and security boundaries", () => {
  for (const file of ["dashboardMetrics.test.ts", "command-center-metric-truth.test.tsx", "dashboard-schema-startup.test.mjs", "dashboard-refresh.test.ts", "overview-freshness.test.ts", "security-dependency-boundary.test.mjs"]) {
    assert.ok(manifest.scripts.test.includes(`tests/${file}`), file);
  }
});

test("Sharp native binding encodes and reads a bounded in-memory image", async () => {
  const sharp = require("sharp");
  const image = await sharp(Buffer.from([255, 0, 0, 0, 255, 0]), {
    raw: { width: 2, height: 1, channels: 3 },
  }).png().toBuffer();
  const metadata = await sharp(image).metadata();
  assert.equal(metadata.format, "png");
  assert.equal(metadata.width, 2);
  assert.equal(metadata.height, 1);
});

test("Nodemailer direct-mail API preserves recipients and rejects delivery errors", { concurrency: false }, async () => {
  const nodemailer = require("nodemailer");
  const { sendEmail } = require("../lib/email");
  const originalCreateTransport = nodemailer.createTransport;
  const names = ["SMTP_CONNECTION_URL", "EMAIL_FROM_ADDRESS"];
  const originalEnv = new Map(names.map((name) => [name, process.env[name]]));
  const messages = [];
  const jsonTransport = originalCreateTransport({ jsonTransport: true });
  try {
    delete process.env.SMTP_CONNECTION_URL;
    assert.deepEqual(await sendEmail({ to: "recipient@example.invalid", subject: "test", text: "hello" }), { ok: false, error: "SMTP not configured" });
    process.env.SMTP_CONNECTION_URL = "smtp://smtp.example.invalid";
    process.env.EMAIL_FROM_ADDRESS = "ContextLattice <sender@example.invalid>";
    nodemailer.createTransport = (url) => {
      assert.equal(url, "smtp://smtp.example.invalid");
      return { sendMail: async (options) => {
        const info = await jsonTransport.sendMail(options);
        messages.push(info);
        return info;
      } };
    };
    assert.deepEqual(await sendEmail({ to: '"Recipient, One" <recipient@example.invalid>', subject: "Verify account", text: "plain body", html: "<p>HTML body</p>" }), { ok: true });
    assert.deepEqual(messages[0].envelope, { from: "sender@example.invalid", to: ["recipient@example.invalid"] });
    const message = JSON.parse(messages[0].message);
    assert.equal(message.subject, "Verify account");
    assert.equal(message.text, "plain body");
    assert.equal(message.html, "<p>HTML body</p>");
    nodemailer.createTransport = () => ({ sendMail: async () => { throw new Error("fixture delivery rejected"); } });
    await assert.rejects(sendEmail({ to: "recipient@example.invalid", subject: "Reset password", text: "reset" }), /fixture delivery rejected/);
  } finally {
    nodemailer.createTransport = originalCreateTransport;
    jsonTransport.close();
    for (const [name, value] of originalEnv) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
});
