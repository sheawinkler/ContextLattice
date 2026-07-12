import assert from "node:assert/strict";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { AgentPacketWorkbench } from "../components/AgentPacketWorkbench";

test("agent packet workbench renders gate, evidence, and copy-only actions", () => {
  const html = renderToStaticMarkup(<AgentPacketWorkbench initialPacket={{
    schema_id: "agent_packet.v1",
    decision_gate: { decision: "verify" },
    uncertainty: { status: "partial_alignment" },
    token_budget: { actual_tokens: 720 },
    token_impact: { net_token_delta: 1280 },
    evidence: [{ text: "Release test passed.", citation: "release-proof.json", relevance: 0.92 }],
    next_actions: [{ label: "Inspect proof", command: "cat release-proof.json", reason: "Verify locally." }],
  }} />);
  assert.match(html, /Ask the lattice/);
  assert.match(html, /verify/);
  assert.match(html, /Release test passed/);
  assert.match(html, /Copy, never auto-run/);
  assert.doesNotMatch(html, /Execute/);
});
