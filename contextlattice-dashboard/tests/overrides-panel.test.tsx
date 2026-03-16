import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { OverridesPanel } from "../components/OverridesPanel";

test("OverridesPanel renders rows", () => {
  const html = renderToStaticMarkup(
    <OverridesPanel
      overrides={[
        {
          symbol: "BOME",
          priority: "MEDIUM",
          reason: "test",
          size_before: 1,
          size_after: 1.3,
          final_size: 1.1,
          confidence_before: 0.55,
          confidence_after: 0.8,
          override_strength: 0.4,
          multiplier: 1.3,
          kelly_fraction: 0.12,
          kelly_target: 0.9,
        },
      ]}
    />,
  );
  assert.ok(html.includes("BOME"));
  assert.ok(html.includes("MEDIUM"));
  assert.ok(html.includes("Final 1.1"));
  assert.ok(html.includes("Kelly 12.0%"));
});
