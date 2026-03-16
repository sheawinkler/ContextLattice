import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { AlertsPanel } from "../components/AlertsPanel";

test("AlertsPanel surfaces warnings when metrics breach thresholds", () => {
  const html = renderToStaticMarkup(
    <AlertsPanel
      queueMetrics={{ queueDepth: 600, totals: { dropped: 2 } }}
      tradingMetrics={{
        dailyPnl: -750,
        unrealizedPnl: -1200,
        openPositions: 30,
        updatedAt: "2025-01-01T00:00:00Z",
        priceCacheEntries: 0,
        priceCacheMaxAge: 45,
        priceCachePenalty: 0.42,
      }}
      sidecar={{ healthy: false, detail: "502" }}
      signals={[
        { symbol: "RISK", risk_score: 0.9, momentum_score: 1.1 },
        { symbol: "SAFE", risk_score: 0.2, momentum_score: 1.3 },
      ]}
      strategies={[]}
    />,
  );
  assert.ok(html.includes("Sidecar unhealthy"));
  assert.ok(html.includes("Telemetry queue saturation"));
  assert.ok(html.includes("Drawdown alert"));
  assert.ok(html.includes("Risky discoveries"));
  assert.ok(html.includes("Price cache empty"));
  assert.ok(html.includes("Cache penalty severe"));
  assert.ok(html.includes("Price cache stalled"));
});
