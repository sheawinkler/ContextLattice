import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { chromium } from "playwright";

const baseUrl = process.env.PUBLIC_SITE_BASE_URL || "http://127.0.0.1:4173";
const outputDir = path.resolve(
  process.cwd(),
  process.env.PUBLIC_SITE_AUDIT_DIR || "tests/public-site-audit/current",
);
const browserExecutable = process.env.PUBLIC_SITE_BROWSER_EXECUTABLE
  || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const routes = (process.env.PUBLIC_SITE_AUDIT_ROUTES || "/,/wiki.html")
  .split(",")
  .map((route) => route.trim())
  .filter(Boolean);
const profiles = [
  { name: "desktop", width: 1440, height: 1000 },
  { name: "tablet", width: 768, height: 1024 },
  { name: "mobile", width: 390, height: 844 },
];

function routeName(route) {
  return route === "/" ? "home" : route.replace(/^\/|\.html$/g, "").replaceAll("/", "-");
}

async function audit() {
  await mkdir(outputDir, { recursive: true });
  const browser = await chromium.launch({ headless: true, executablePath: browserExecutable });
  const captures = [];
  const failures = [];

  try {
    for (const profile of profiles) {
      const context = await browser.newContext({
        viewport: { width: profile.width, height: profile.height },
        deviceScaleFactor: 1,
        reducedMotion: "reduce",
      });
      const page = await context.newPage();
      const consoleErrors = [];
      page.on("console", (message) => {
        if (message.type() === "error") consoleErrors.push(message.text());
      });
      page.on("pageerror", (error) => consoleErrors.push(error.message));

      for (const route of routes) {
        consoleErrors.length = 0;
        const url = new URL(route, baseUrl).toString();
        const response = await page.goto(url, { waitUntil: "networkidle", timeout: 30000 });
        await page.evaluate(() => document.fonts.ready);

        const inspection = await page.evaluate(() => {
          const root = document.documentElement;
          const nav = document.querySelector('[aria-label="Primary"]');
          const images = [...document.images];
          const interactive = [
            ...document.querySelectorAll("a[href], button:not([disabled]), summary"),
          ];
          return {
            title: document.title,
            h1Count: document.querySelectorAll("h1").length,
            navLinkCount: nav?.querySelectorAll("a[href]").length ?? 0,
            skipLinkCount: document.querySelectorAll('a[href="#main-content"]').length,
            width: root.clientWidth,
            scrollWidth: root.scrollWidth,
            height: root.scrollHeight,
            brokenImages: images
              .filter((image) => image.complete && image.naturalWidth === 0)
              .map((image) => image.getAttribute("src") || ""),
            unnamedInteractive: interactive
              .filter((element) => !(element.textContent || "").trim() && !element.getAttribute("aria-label"))
              .map((element) => element.outerHTML.slice(0, 120)),
          };
        });

        const status = response?.status() ?? null;
        const screenshot = path.join(outputDir, `${profile.name}-${routeName(route)}.png`);
        await page.screenshot({ path: screenshot, fullPage: true });

        const prefix = `${profile.name} ${route}`;
        if (status === null || status >= 400) failures.push(`${prefix}: HTTP ${status}`);
        if (inspection.h1Count !== 1) failures.push(`${prefix}: expected one h1, got ${inspection.h1Count}`);
        if (inspection.navLinkCount > 5) {
          failures.push(`${prefix}: primary navigation has ${inspection.navLinkCount} links; maximum is 5`);
        }
        if (inspection.scrollWidth - inspection.width > 1) {
          failures.push(`${prefix}: horizontal overflow ${inspection.scrollWidth - inspection.width}px`);
        }
        if (route === "/" && inspection.skipLinkCount !== 1) {
          failures.push(`${prefix}: homepage must expose one skip link`);
        }
        if (inspection.brokenImages.length) {
          failures.push(`${prefix}: broken images ${inspection.brokenImages.join(", ")}`);
        }
        if (inspection.unnamedInteractive.length) {
          failures.push(`${prefix}: unnamed controls ${inspection.unnamedInteractive.join(", ")}`);
        }
        if (consoleErrors.length) {
          failures.push(`${prefix}: browser errors ${consoleErrors.join(" | ")}`);
        }

        captures.push({
          route,
          profile,
          status,
          screenshot: path.relative(process.cwd(), screenshot),
          ...inspection,
        });
      }
      await context.close();
    }
  } finally {
    await browser.close();
  }

  const manifest = {
    schemaId: "contextlattice_public_site_visual_audit.v1",
    generatedAt: new Date().toISOString(),
    baseUrl,
    routes,
    captures,
    failures,
    ok: failures.length === 0,
  };
  await writeFile(path.join(outputDir, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`);
  console.log(JSON.stringify(manifest, null, 2));
  if (failures.length) process.exitCode = 1;
}

audit().catch((error) => {
  console.error(error);
  process.exit(1);
});
