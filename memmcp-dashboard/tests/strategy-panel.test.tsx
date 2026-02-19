import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { StrategyPanel } from "../components/StrategyPanel";

test("StrategyPanel renders kelly/risk columns", () => {
  const html = renderToStaticMarkup(
    <StrategyPanel
      strategies={{
        updatedAt: "2025-01-01T00:00:00Z",
        strategies: [
          {
            name: "Aggressive",
            capital: 4200,
            win_rate: 0.62,
            daily_pnl: 125.55,
            kelly_fraction: 0.34,
            risk_score: 0.55,
            price_cache_entries: 8,
            price_cache_max_age: 2.5,
            price_cache_freshness: 0.82,
            price_cache_penalty: 0.95,
            notes: "Top BONK",
            memory_ref: "https://example.com",
          },
        ],
      }}
    />,
  );
  assert.ok(html.includes("Aggressive"));
  assert.ok(html.includes("34.0%"));
  assert.ok(html.includes("55.0%"));
  assert.ok(html.includes("8 entries"));
  assert.ok(html.includes("(2.5s)"));
  assert.ok(html.includes("penalty x0.95"));
  assert.ok(html.includes("fresh 82%"));
  assert.ok(html.includes("Top BONK"));
});
