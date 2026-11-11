import test from "node:test";
import assert from "node:assert/strict";
import { renderToStaticMarkup } from "react-dom/server";
import { ALL_PLANS, planHighlights, planSelectionHref } from "../lib/billing/plans";
import { sanitizeCallbackUrl } from "../lib/callback-url";
import PricingPage from "../app/pricing/page";

test("paid highlights describe included differences, not the free baseline", () => {
  const free = ALL_PLANS.find((plan) => !plan.paid)!;
  assert.deepEqual(planHighlights(free), free.features.slice(0, 6));
  for (const plan of ALL_PLANS.filter((entry) => entry.paid)) {
    const highlights = planHighlights(plan);
    assert.ok(highlights.length > 0 && highlights.length <= 6);
    assert.equal(new Set(highlights).size, highlights.length);
    for (const value of highlights) {
      assert.ok(plan.features.includes(value));
      assert.ok(!free.features.includes(value));
    }
  }
});

test("plan choice survives public pricing and sanitized hosted-login callback", () => {
  for (const id of ["starter", "team", "operator"]) {
    assert.equal(planSelectionHref(id, false), `https://contextlattice.io/premium.html#plan-${id}`);
    const billing = planSelectionHref(id, true);
    assert.equal(billing, `/billing?plan=${id}`);
    const login = new URL("/auth/login", "https://app.contextlattice.io");
    login.searchParams.set("callbackUrl", billing);
    assert.equal(sanitizeCallbackUrl(login.searchParams.get("callbackUrl")), billing);
  }
  assert.equal(planSelectionHref("free", true), "/downloads");
  assert.ok(planSelectionHref("enterprise", true).startsWith("mailto:"));
  for (const invalid of ["", "unknown", "//evil.invalid", "starter&next=https://evil.invalid"]) {
    assert.equal(planSelectionHref(invalid, true), "/pricing");
    assert.equal(planSelectionHref(invalid, false), "/pricing");
  }
  assert.equal(sanitizeCallbackUrl("https://evil.invalid/billing?plan=team"), "/console");
});

test("account-free pricing renders selected hosted links without a local billing dependency", () => {
  const before = process.env.AUTH_REQUIRED;
  try {
    delete process.env.AUTH_REQUIRED;
    const html = renderToStaticMarkup(PricingPage());
    for (const id of ["starter", "team", "operator"]) {
      assert.ok(html.includes(`href="https://contextlattice.io/premium.html#plan-${id}"`));
    }
    assert.equal(html.includes('href="/billing'), false);
    assert.ok(html.includes("1 included seat"));
    assert.ok(html.includes("5 included seats"));
    assert.equal(html.includes("+ 8 governed capabilities"), false);
  } finally {
    if (before === undefined) delete process.env.AUTH_REQUIRED;
    else process.env.AUTH_REQUIRED = before;
  }
});
