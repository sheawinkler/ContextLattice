import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { ChartsPanel } from "../components/ChartsPanel";

test("ChartsPanel renders sparkline cards when history exists", () => {
  const html = renderToStaticMarkup(
    <ChartsPanel
      trading={{
        history: [
          { timestamp: "2025-01-01T00:00:00Z", total_value_usd: 400, daily_pnl: 25 },
          { timestamp: "2025-01-02T00:00:00Z", total_value_usd: 600, daily_pnl: 125 },
          { timestamp: "2025-01-03T00:00:00Z", total_value_usd: 900, daily_pnl: 340 },
        ],
      }}
      sidecar={{
        history: [
          { timestamp: "2025-01-01T00:00:00Z", healthy: true },
          { timestamp: "2025-01-01T00:05:00Z", healthy: false },
          { timestamp: "2025-01-01T00:10:00Z", healthy: true },
        ],
      }}
    />,
  );
  assert.ok(html.includes("Equity Curve"));
  assert.ok(html.includes("Sidecar Uptime"));
});
