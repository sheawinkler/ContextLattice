import assert from "node:assert/strict";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { RecallQualityPanel } from "@/components/RecallQualityPanel";

test("RecallQualityPanel renders quality metrics and tuning", () => {
  const html = renderToStaticMarkup(
    <RecallQualityPanel
      recall={{
        quality: {
          status: "healthy",
          totals: {
            recallAtK: 0.91,
            mrr: 0.78,
            citationCoverage: 1,
            sourceDiversity: 2.4,
            graphLift: 0.12,
            evalP95Ms: 184,
            lastEvalAt: "2026-05-29T12:00:00Z",
          },
          recommendations: ["Recall quality telemetry is inside current production thresholds."],
        },
      }}
      tuning={{
        recommended: {
          quality: {
            graphExpansion: { enabled: true, depth: 1, neighborLimit: 12 },
            sourceOrder: ["qdrant", "postgres_pgvector", "topic_rollups"],
          },
        },
      }}
    />,
  );

  assert.match(html, /Recall quality/);
  assert.match(html, /91%/);
  assert.match(html, /Graph lift/);
  assert.match(html, /qdrant/);
  assert.match(html, /1 \/ 12/);
});
