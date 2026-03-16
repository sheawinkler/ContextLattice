import test from "node:test";
import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { SidecarHealthPanel } from "../components/SidecarHealthPanel";

test("SidecarHealthPanel renders latest and history", () => {
  const html = renderToStaticMarkup(
    <SidecarHealthPanel
      data={{
        updatedAt: "2025-01-01T00:00:00Z",
        healthy: true,
        detail: "mode full",
        history: [
          {
            timestamp: "2025-01-01T00:00:00Z",
            healthy: true,
            detail: "mode full",
          },
          {
            timestamp: "2025-01-01T00:05:00Z",
            healthy: false,
            detail: "HTTP 500",
          },
        ],
      }}
    />,
  );
  assert.ok(html.includes("Sidecar Health"));
  assert.ok(html.includes("Healthy"));
  assert.ok(html.includes("HTTP 500"));
});
