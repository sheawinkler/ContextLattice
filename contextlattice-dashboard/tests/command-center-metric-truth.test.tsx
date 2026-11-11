import assert from "node:assert/strict";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { CommandCenterDashboard } from "@/components/CommandCenterDashboard";

function render(overview: unknown = {}) {
  return renderToStaticMarkup(<CommandCenterDashboard initialState={{ overview, mindmap: null, topics: null, health: null }} />);
}

function card(html: string, label: string): string {
  const result = html.match(/<article\b[^>]*>[\s\S]*?<\/article>/g)?.find((item) => item.includes(`>${label}</div>`));
  assert.ok(result, `missing metric card: ${label}`);
  return result;
}

function value(html: string, label: string): string {
  const result = card(html, label).match(/class="cl-metric-value[^"]*">([^<]*)<\/div>/);
  assert.ok(result, `missing metric value: ${label}`);
  return result[1];
}

test("negative scoped net leads while positive-only savings stays a non-causal subtotal", () => {
  const html = render({ tokenImpact: {
    sample_count: 20, transport_inclusive_sample_count: 12, net_token_delta: -12345,
    saved_tokens_estimate: 4086, baseline_tokens_estimate: 9800, packed_tokens_estimate: 22000,
    calibration_grade: "sampled_pack_estimate", confidence: "high",
  } });
  assert.equal(value(html, "signed net context delta"), "-12.3K");
  assert.equal(value(html, "positive-only savings subtotal"), "4.1K");
  assert.ok(html.indexOf("signed net context delta") < html.indexOf("positive-only savings subtotal"));
  assert.match(card(html, "signed net context delta"), /cl-metric--warn/);
  assert.match(card(html, "signed net context delta"), /12\/20 transport-inclusive rows/);
  assert.match(card(html, "positive-only savings subtotal"), /excludes negative rows; not signed net, exact causal savings, or provider billing savings/);
  assert.doesNotMatch(html, /measured context savings/);
});

test("provider missing and no-observation states do not render measured zero", () => {
  for (const quality of [{}, { observed_provider_usage_count: 0, observed_provider_total_tokens: 0, observed_average_provider_total_tokens: 0 }]) {
    const html = render({ contextPackQuality: quality });
    assert.equal(value(html, "observed provider tokens"), "unknown");
    assert.match(card(html, "observed provider tokens"), /not instrumented/);
    assert.match(card(html, "observed provider tokens"), /avg unknown\/complete row/);
    assert.doesNotMatch(card(html, "observed provider tokens"), /cl-metric--good/);
  }
});

test("provider observed zero differs from missing total or missing average", () => {
  const zero = render({ contextPackQuality: { observedProviderUsageCount: 2, providerUsageCoverageVersion: 1, observedProviderCompleteTotalCount: 2, observedProviderCompleteTotalTokens: 0, observedAverageProviderCompleteTotalTokens: 0 } });
  assert.equal(value(zero, "observed provider tokens"), "0");
  assert.match(card(zero, "observed provider tokens"), /2\/2 rows with complete totals.*avg 0\/complete row/);
  const absentAverage = render({ contextPackQuality: { observed_provider_usage_count: 2, provider_usage_coverage_version: 1, observed_provider_complete_total_count: 1, observed_provider_complete_total_tokens: 101 } });
  assert.equal(value(absentAverage, "observed provider tokens"), "101");
  assert.match(card(absentAverage, "observed provider tokens"), /avg unknown\/complete row/);
  const absentTotal = render({ contextPackQuality: { observed_provider_usage_count: 2, observed_provider_prompt_tokens: 101 } });
  assert.equal(value(absentTotal, "observed provider tokens"), "unknown");
});

test("signed zero needs coverage and is never inferred from baseline minus packed", () => {
  const zero = render({ status: { token_impact: { transport_inclusive_sample_count: 1, net_token_delta: 0 } } });
  assert.equal(value(zero, "signed net context delta"), "0");
  const absent = render({ tokenImpact: { baseline_tokens_estimate: 900, packed_tokens_estimate: 300 } });
  assert.equal(value(absent, "signed net context delta"), "unknown");
  assert.equal(value(absent, "positive-only savings subtotal"), "unknown");
});

test("mixed exactness and bounded cohorts do not replace aggregate windows", () => {
  const html = render({ tokenImpact: {
    sample_count: 100, exact_sample_count: 27, transport_inclusive_sample_count: 18,
    net_token_delta: -9001, baseline_tokens_estimate: 20001, packed_tokens_estimate: 31001,
    calibration_grade: "sampled_pack_estimate", cohort_window_sample_count: 9, cohort_omitted_count: 2,
    cohorts: [
      { scope: "tool", packed_kind: "wire", sample_count: 6, signed_net_delta_sample_count: 4, signed_net_token_delta: -101, wire_exact_sample_count: 3, wire_tokens_exact: 901, model_visible_exact_sample_count: 2, model_visible_context_tokens_exact: 72 },
      { scope: "prompt", packed_kind: "model", sample_count: 3, signed_net_delta_sample_count: 1, signed_net_token_delta: 7 },
    ],
  } });
  assert.equal(value(html, "signed net context delta"), "-9K");
  assert.match(html, /tokenizer-exact rows<\/span><strong>27\/100/);
  assert.match(html, /Rows without tokenizer-exact coverage are estimated or unknown/);
  assert.match(html, /Bounded cohort window: 9 retained rows; 2 cohorts omitted/);
  assert.match(html, /Separate from aggregate ledger totals; do not sum to reconcile/);
  assert.match(html, /tool \/ wire/);
  assert.match(html, /4\/6 signed-net rows; wire 901 \(3\/6 rows\); model-visible 72 \(2\/6 exact rows\)/);
  assert.match(html, /<strong>-101<\/strong>/);
  assert.match(html, /<strong>\+7<\/strong>/);
});

test("baseline and packed bars share a maximum scale in both directions without a minimum floor", () => {
  for (const [baseline, packed] of [[100, 250], [250, 100], [1, 10000], [131, 277], [900, 900], [0, 250], [250, 0], [0, 0]]) {
    const html = render({ tokenImpact: { baseline_tokens_estimate: baseline, packed_tokens_estimate: packed, sample_count: 2 } });
    const meter = html.slice(html.indexOf('aria-label="Token impact baseline and packed comparison"'));
    const widths = [...meter.matchAll(/<i style="width:([\d.]+)%"/g)].slice(0, 2).map((match) => Number(match[1]));
    assert.equal(widths.length, 2);
    assert.ok(Math.abs(widths[0] - baseline / Math.max(1, baseline, packed) * 100) < 1e-10);
    assert.ok(Math.abs(widths[1] - packed / Math.max(1, baseline, packed) * 100) < 1e-10);
    if (packed > baseline) assert.match(html, /packed exceeds baseline/);
  }
});

test("medium modeled confidence with zero calibration is explicitly uncalibrated", () => {
  const html = render({ contextPackQuality: {
    sample_count: 12, outcome_sample_count: 9, calibration_outcome_sample_count: 0,
    modeled_inference_tokens_avoided: 155000, modeled_extra_calls_avoided: 38.7,
    confidence: "medium", calibration_grade: "outcome_adjusted",
  } });
  const modeled = card(html, "modeled inference avoided");
  assert.match(modeled, /uncalibrated \(0 observed calibration rows\)/);
  assert.match(modeled, /medium model confidence, not measured savings/);
  assert.doesNotMatch(modeled, /cl-metric--good/);
  assert.match(card(render({ contextPackQuality: { outcome_sample_count: 9 } }), "modeled inference avoided"), /calibration unknown/);
});

test("missing queue depth is unknown and neutral rather than a healthy empty queue", () => {
  const missing = render();
  for (const label of ["memory write queue", "fanout queue"]) {
    assert.equal(value(missing, label), "unknown/unknown");
    assert.match(card(missing, label), /cl-metric--neutral/);
  }
  const observed = render({ memoryTelemetry: { memoryBank: { queueDepth: 0, queueMax: 8 }, fanout: { queueDepth: 3, queueMax: 8 } } });
  assert.equal(value(observed, "memory write queue"), "0/8");
  assert.match(card(observed, "memory write queue"), /cl-metric--good/);
  assert.match(card(observed, "fanout queue"), /cl-metric--warn/);
});
