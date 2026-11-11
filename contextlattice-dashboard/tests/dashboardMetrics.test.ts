import assert from "node:assert/strict";
import test from "node:test";

import { estimateContextPackQuality, estimateRunnerQuality, estimateTokenImpact, getTokenImpactTruth } from "@/lib/dashboardMetrics";

test("estimateTokenImpact prefers sampled token impact payloads", () => {
  const impact = estimateTokenImpact(
    {
      token_impact: {
        baseline_tokens_estimate: 50000,
        packed_tokens_estimate: 8000,
        saved_tokens_estimate: 42000,
        compression_ratio: 6.25,
        calibration_grade: "sampled_pack_estimate",
        confidence: "high",
        sample_count: 2,
        source: "/memory/context-pack",
        measurement_limit: "chars_div_4 until tokenizer accounting lands",
      },
    },
    null,
    null,
  );

  assert.equal(impact.estimatedSaved, 42000);
  assert.equal(impact.baselineTokens, 50000);
  assert.equal(impact.packedTokens, 8000);
  assert.equal(impact.compressionRatio, 6.3);
  assert.equal(impact.calibrationGrade, "sampled_pack_estimate");
  assert.equal(impact.confidence, "high");
  assert.match(impact.windowLabel, /2 sampled packs/);
  assert.match(impact.measurementLimit, /chars_div_4/);
});

test("estimateTokenImpact preserves tokenizer-exact calibration", () => {
  const impact = estimateTokenImpact(
    {
      tokenImpact: {
        baseline_tokens_estimate: 32000,
        packed_tokens_estimate: 12000,
        saved_tokens_estimate: 20000,
        compression_ratio: 2.67,
        calibration_grade: "tokenizer_exact",
        confidence: "high",
        sample_count: 4,
        tokenizer_exact: true,
        tokenizer_encoding: "o200k_base",
        measurement_limit: "Token counts use configured tiktoken encoding; no raw prompt text is persisted.",
      },
    },
    null,
    null,
  );

  assert.equal(impact.calibrationGrade, "tokenizer_exact");
  assert.equal(impact.confidence, "high");
  assert.equal(impact.qualityScore, 94);
  assert.match(impact.measurementLimit, /tiktoken/);
});

test("estimateTokenImpact bounds lifetime writes into a live working set", () => {
  const impact = estimateTokenImpact(
    {
      memoryTelemetry: {
        memoryBank: { processed: 12000 },
      },
      status: {
        metadataContract: { totalWrites: 12000 },
      },
      agentRuntime: {
        active: 5,
        sessions: [{ id: "a" }, { id: "b" }, { id: "c" }],
      },
      retrieval: {
        alerts: { active: [] },
        latency: {
          sources: {
            qdrant: { requests: 40, errors: 0, timeouts: 0, p95Ms: 120 },
            topic_rollups: { requests: 44, errors: 0, timeouts: 0, p95Ms: 60 },
          },
        },
      },
    },
    {
      summary: {
        totalWrites: 9000,
        totalEvents: 11000,
        totalNodes: 80,
        coveredTopics: 22,
        hotTopics: 9,
      },
      topPaths: [{ recentEventCount: 14 }],
    },
    null,
  );

  assert.equal(impact.calibrationGrade, "heuristic");
  assert.ok(impact.estimatedSaved > 0);
  assert.ok(impact.baselineTokens > impact.packedTokens);
  assert.ok(impact.compressionRatio > 5);
  assert.ok(impact.estimatedSaved < 9000 * 720, "fallback must not multiply every lifetime write into saved tokens");
  assert.ok(impact.factors.some((factor) => factor.label === "raw working-set writes"));
  assert.ok(impact.warnings.some((warning) => warning.includes("working set")));
});

test("estimateContextPackQuality preserves modeled inference avoidance and outcome calibration", () => {
  const quality = estimateContextPackQuality({
    contextPackQuality: {
      sample_count: 12,
      outcome_sample_count: 6,
      calibration_outcome_sample_count: 5,
      average_quality_score: 87,
      exact_prompt_tokens_saved: 42000,
      modeled_inference_tokens_avoided: 6800,
      modeled_extra_calls_avoided: 1.7,
      calibration_grade: "outcome_seeded",
      confidence: "medium",
      observed_first_pass_success_rate: 0.833,
      observed_repair_rate: 0.167,
      observed_followup_tokens: 1234,
      observed_provider_usage_count: 3,
      observed_provider_prompt_tokens: 18000,
      observed_provider_completion_tokens: 2400,
      observed_provider_total_tokens: 20400,
      observed_average_provider_total_tokens: 6800,
      measurement_limit: "Exact prompt-token savings are measured; modeled inference avoidance is confidence-banded.",
    },
  });

  assert.equal(quality.modeledInferenceSaved, 6800);
  assert.equal(quality.exactPromptSaved, 42000);
  assert.equal(quality.qualityScore, 87);
  assert.equal(quality.extraCallsAvoided, 1.7);
  assert.equal(quality.calibrationGrade, "outcome_seeded");
  assert.equal(quality.confidence, "medium");
  assert.equal(quality.outcomeSamples, 6);
  assert.equal(quality.calibrationOutcomeSamples, 5);
  assert.equal(quality.observedFirstPassRate, 0.833);
  assert.equal(quality.observedRepairRate, 0.167);
  assert.equal(quality.observedFollowupTokens, 1234);
  assert.equal(quality.observedProviderUsageCount, 3);
  assert.equal(quality.observedProviderTotalTokens, 20400);
  assert.equal(quality.observedAverageProviderTotalTokens, 6800);
  assert.match(quality.measurementLimit, /confidence-banded/);
});

test("estimateContextPackQuality falls back when no quality samples exist", () => {
  const quality = estimateContextPackQuality({});

  assert.equal(quality.modeledInferenceSaved, 0);
  assert.equal(quality.calibrationGrade, "heuristic");
  assert.equal(quality.confidence, "low");
  assert.ok(quality.warnings.some((warning) => warning.includes("Run a context pack")));
});

test("estimateRunnerQuality preserves advisor-only recommendations", () => {
  const quality = estimateRunnerQuality({
    runnerQuality: {
      sample_count: 2,
      total_sample_count: 3,
      task_class: "scout",
      by_status: { succeeded: 1, blocked: 1 },
      by_task_class: { scout: 2, implementer: 1 },
      recommendations: {
        mode: "advisor_only",
        task_class: "scout",
        confidence: "low",
        top_runner: "pi",
        candidates: [
          {
            runner: "pi",
            score: 87.123,
            sample_count: 1,
            success_rate: 1,
            blocked_rate: 0,
            failure_rate: 0,
            average_duration_secs: 1.2,
            reason: "100% success across one comparable sample",
          },
        ],
        guardrails: ["Never dispatch or mutate automatically from this summary."],
      },
      measurement_limit: "Aggregates bounded runner-quality rows only.",
      storage: { durability: "bounded_ndjson" },
    },
  });

  assert.equal(quality.sampleCount, 2);
  assert.equal(quality.totalSampleCount, 3);
  assert.equal(quality.mode, "advisor_only");
  assert.equal(quality.confidence, "low");
  assert.equal(quality.topRunner, "pi");
  assert.equal(quality.candidates[0].score, 87.1);
  assert.equal(quality.byTaskClass.scout, 2);
  assert.ok(quality.warnings.some((warning) => warning.includes("Never dispatch")));
});

test("estimateRunnerQuality falls back when no samples exist", () => {
  const quality = estimateRunnerQuality({});

  assert.equal(quality.sampleCount, 0);
  assert.equal(quality.confidence, "insufficient_samples");
  assert.equal(quality.mode, "advisor_only");
  assert.equal(quality.topRunner, "");
  assert.ok(quality.warnings.some((warning) => warning.includes("advisor-only")));
});

test("token truth preserves negative net separately from a positive-only subtotal without rounding", () => {
  const overview = { tokenImpact: {
    sample_count: 90,
    exact_sample_count: 24,
    transport_inclusive_sample_count: 17,
    net_token_delta: -12345678,
    baseline_tokens_estimate: 9876543,
    packed_tokens_estimate: 22000001,
    saved_tokens_estimate: 408643,
    calibration_grade: "sampled_pack_estimate",
  } };
  const truth = getTokenImpactTruth(overview);
  const estimate = estimateTokenImpact(overview, null, null);
  assert.equal(truth.netTokenDelta, -12345678);
  assert.equal(truth.positiveOnlySaved, 408643);
  assert.equal(truth.exactSampleCount, 24);
  assert.equal(truth.transportSampleCount, 17);
  assert.equal(estimate.estimatedSaved, 408643);
  assert.equal(estimate.baselineTokens, 9876543);
  assert.equal(estimate.packedTokens, 22000001);
  assert.notEqual(truth.netTokenDelta, estimate.baselineTokens - estimate.packedTokens);
});

test("explicit zero savings is not replaced by an aggregate subtraction", () => {
  const overview = { tokenImpact: { baseline_tokens_estimate: 700, packed_tokens_estimate: 300, saved_tokens_estimate: 0 } };
  assert.equal(estimateTokenImpact(overview, null, null).estimatedSaved, 0);
  assert.equal(getTokenImpactTruth(overview).positiveOnlySaved, 0);
  assert.equal(getTokenImpactTruth({ tokenImpact: { baseline_tokens_estimate: 700, packed_tokens_estimate: 300 } }).positiveOnlySaved, null);
});

test("signed net distinguishes measured zero, absent values, and uncovered rows", () => {
  assert.equal(getTokenImpactTruth({}).netTokenDelta, null);
  for (const count of [undefined, null, 0, "", false, "invalid", -1]) {
    assert.equal(getTokenImpactTruth({ tokenImpact: { transport_inclusive_sample_count: count, net_token_delta: 0 } }).netTokenDelta, null);
  }
  for (const delta of [undefined, null, "", false, "invalid", Infinity]) {
    assert.equal(getTokenImpactTruth({ tokenImpact: { transport_inclusive_sample_count: 1, net_token_delta: delta } }).netTokenDelta, null);
  }
  for (const delta of [-17, 0, 23]) {
    assert.equal(getTokenImpactTruth({ tokenImpact: { transport_inclusive_sample_count: 1, net_token_delta: delta } }).netTokenDelta, delta);
  }
});

test("token truth supports every existing overview envelope and camel-case aliases", () => {
  const row = { sampleCount: 5, exactSampleCount: 2, transportInclusiveSampleCount: 3, netTokenDelta: -71, transportTokensExact: 201, compiledPromptSampleCount: 1, compiledPromptTokensEstimate: 99, modelVisibleExactSampleCount: 1, modelVisibleContextTokensExact: 21, savedTokens: 13, windowLabel: "explicit ledger window" };
  const envelopes = [
    { tokenImpact: row }, { token_impact: row },
    { status: { tokenImpact: row } }, { status: { token_impact: row } },
    { telemetryMetrics: { tokenImpact: row } }, { telemetryMetrics: { token_impact: row } },
  ];
  for (const overview of envelopes) {
    const truth = getTokenImpactTruth(overview);
    assert.equal(truth.netTokenDelta, -71);
    assert.equal(truth.transportTokens, 201);
    assert.equal(truth.compiledPromptTokens, 99);
    assert.equal(truth.modelVisibleTokens, 21);
    assert.equal(truth.positiveOnlySaved, 13);
    assert.equal(truth.windowLabel, "explicit ledger window");
  }
});

test("bounded cohorts retain independent signed, wire, and model-visible coverage", () => {
  const truth = getTokenImpactTruth({ token_impact: {
    sample_count: 100, transport_inclusive_sample_count: 80, net_token_delta: -9001,
    cohort_window_sample_count: 9, cohort_omitted_count: 2,
    cohorts: [
      { scope: "tool", packed_kind: "wire", sample_count: 6, signed_net_delta_sample_count: 4, signed_net_token_delta: -101, wire_exact_sample_count: 3, wire_tokens_exact: 901, model_visible_exact_sample_count: 2, model_visible_context_tokens_exact: 72 },
      { scope: "prompt", packedKind: "model", sampleCount: 3, signedNetDeltaSampleCount: 1, signedNetTokenDelta: 7, wireExactSampleCount: 0, wireTokensExact: 0, modelVisibleExactSampleCount: 1, modelVisibleContextTokensExact: 19 },
    ],
  } });
  assert.equal(truth.netTokenDelta, -9001);
  assert.equal(truth.cohortWindowSampleCount, 9);
  assert.equal(truth.cohortOmittedCount, 2);
  assert.deepEqual(truth.cohorts[0], { scope: "tool", packedKind: "wire", sampleCount: 6, netSampleCount: 4, netTokenDelta: -101, wireSampleCount: 3, wireTokens: 901, modelVisibleSampleCount: 2, modelVisibleTokens: 72 });
  assert.equal(truth.cohorts[1].netTokenDelta, 7);
  assert.equal(truth.cohorts[1].wireTokens, null);
  assert.equal(truth.cohorts[1].modelVisibleTokens, 19);
  assert.notEqual(truth.netTokenDelta, truth.cohorts.reduce((sum, row) => sum + (row.netTokenDelta ?? 0), 0));
});

test("provider display truth is unknown without coverage while legacy numeric fields remain compatible", () => {
  for (const overview of [
    {},
    { contextPackQuality: { sample_count: 12, observed_provider_total_tokens: 99 } },
    { contextPackQuality: { observed_provider_usage_count: 0, observed_provider_total_tokens: 0, observed_average_provider_total_tokens: 0 } },
  ]) {
    const quality = estimateContextPackQuality(overview);
    assert.equal(quality.providerUsage.totalTokens, null);
    assert.equal(quality.providerUsage.averageTotalTokens, null);
    assert.equal(typeof quality.observedProviderTotalTokens, "number");
  }
  assert.equal(estimateContextPackQuality({}).providerUsage.sampleCount, null);
  assert.equal(estimateContextPackQuality({ contextPackQuality: { observed_provider_usage_count: 0 } }).providerUsage.sampleCount, 0);
});

test("covered provider zero is observed while omitted components and averages stay unknown", () => {
  const zero = estimateContextPackQuality({ contextPackQuality: {
    observed_provider_usage_count: 2, provider_usage_coverage_version: 1,
    observed_provider_complete_total_count: 2, observed_provider_prompt_count: 2, observed_provider_completion_count: 2,
    observed_provider_complete_total_tokens: 0, observed_provider_reported_prompt_tokens: 0,
    observed_provider_reported_completion_tokens: 0, observed_average_provider_complete_total_tokens: 0,
  } });
  assert.deepEqual(zero.providerUsage, { sampleCount: 2, completeTotalCount: 2, promptCount: 2, completionCount: 2, promptTokens: 0, completionTokens: 0, totalTokens: 0, averageTotalTokens: 0 });
  const partial = estimateContextPackQuality({ status: { context_pack_quality: {
    observedProviderUsageCount: 2, providerUsageCoverageVersion: 1,
    observedProviderPromptCount: 2, observedProviderCompleteTotalCount: 0, observedProviderCompletionCount: 0,
    observedProviderReportedPromptTokens: 17,
  } } });
  assert.deepEqual(partial.providerUsage, { sampleCount: 2, completeTotalCount: 0, promptCount: 2, completionCount: 0, promptTokens: 17, completionTokens: null, totalTokens: null, averageTotalTokens: null });
});

test("modeled confidence does not imply observed calibration", () => {
  const quality = estimateContextPackQuality({ contextPackQuality: {
    sample_count: 10, outcome_sample_count: 8, calibration_outcome_sample_count: 0,
    modeled_inference_tokens_avoided: 155000, confidence: "medium", calibration_grade: "modeled_counterfactual",
  } });
  assert.equal(quality.confidence, "medium");
  assert.equal(quality.calibrationOutcomeSamples, 0);
  assert.equal(quality.calibrationStatus, "uncalibrated");
  assert.match(quality.warnings.join(" "), /not exact causal or billing savings/);
  const legacy = estimateContextPackQuality({ contextPackQuality: { outcome_sample_count: 8 } });
  assert.equal(legacy.calibrationOutcomeSamples, 8);
  assert.equal(legacy.calibrationStatus, "unknown");
  assert.equal(estimateContextPackQuality({ contextPackQuality: { calibrationOutcomeSampleCount: 2 } }).calibrationStatus, "observed");
});

test("zero-valued token comparisons remain observed instead of falling back to working-set proxies", () => {
  for (const [baseline, packed] of [[0, 250], [250, 0], [0, 0]]) {
    const impact = estimateTokenImpact({
      tokenImpact: { baselineTokens: baseline, packedTokens: packed, sampleCount: 2, calibrationGrade: "tokenizer_exact" },
      memoryTelemetry: { memoryBank: { processed: 1000 } },
    }, null, null);
    assert.equal(impact.baselineTokens, baseline);
    assert.equal(impact.packedTokens, packed);
    assert.equal(impact.calibrationGrade, "tokenizer_exact");
    if (packed === 0) assert.equal(impact.compressionRatio, 0);
  }
});

test("provider malformed values stay unknown rather than becoming observed zero", () => {
  for (const value of [null, undefined, "", false, "invalid", -1, Infinity]) {
    const quality = estimateContextPackQuality({ contextPackQuality: {
      observed_provider_usage_count: 1, provider_usage_coverage_version: 1, observed_provider_complete_total_count: 1,
      observed_provider_complete_total_tokens: value, observed_average_provider_complete_total_tokens: value,
    } });
    assert.equal(quality.providerUsage.totalTokens, null);
    assert.equal(quality.providerUsage.averageTotalTokens, null);
  }
  const fractionalAverage = estimateContextPackQuality({ contextPackQuality: { observedProviderUsageCount: 2, providerUsageCoverageVersion: 1, observedProviderCompleteTotalCount: 2, observedAverageProviderCompleteTotalTokens: 17.5 } });
  assert.equal(fractionalAverage.providerUsage.averageTotalTokens, 17.5);
});

test("compiled estimate presence does not borrow transport coverage", () => {
  for (const count of [undefined, 0]) {
    const truth = getTokenImpactTruth({ tokenImpact: {
      transport_inclusive_sample_count: 3, compiled_prompt_tokens_estimate: 0,
      compiled_prompt_sample_count: count,
    } });
    assert.equal(truth.compiledPromptTokens, null);
  }
  const truth = getTokenImpactTruth({ tokenImpact: {
    transport_inclusive_sample_count: 3, compiled_prompt_tokens_estimate: 0,
    compiled_prompt_sample_count: 1,
  } });
  assert.equal(truth.compiledPromptTokens, 0);
});

test("provider partial and legacy aggregates never claim complete usage", () => {
  for (const coverage of [{}, {
    provider_usage_coverage_version: 1,
    observed_provider_prompt_count: 1,
    observed_provider_completion_count: 0,
    observed_provider_complete_total_count: 0,
    observed_provider_reported_prompt_tokens: 33,
    observed_provider_reported_completion_tokens: 0,
    observed_provider_complete_total_tokens: 0,
  }]) {
    const quality = estimateContextPackQuality({ contextPackQuality: {
      observed_provider_usage_count: 1, observed_provider_prompt_tokens: 33,
      observed_provider_completion_tokens: 0, observed_provider_total_tokens: 33,
      observed_average_provider_total_tokens: 33, ...coverage,
    } });
    assert.equal(quality.providerUsage.totalTokens, null);
    assert.equal(quality.providerUsage.completionTokens, null);
    assert.equal(quality.providerUsage.averageTotalTokens, null);
  }
});

test("coverage counts and versions must be integral and within their populations", () => {
  for (const count of [0.6, 3, Number.MAX_SAFE_INTEGER + 1]) {
    assert.equal(getTokenImpactTruth({ tokenImpact: {
      sample_count: 1, transport_inclusive_sample_count: 1, compiled_prompt_sample_count: count,
      compiled_prompt_tokens_estimate: 0,
    } }).compiledPromptTokens, null);
  }
  for (const [version, population] of [[1.4, 1], [1, 0.6], [2, 1]]) {
    const quality = estimateContextPackQuality({ contextPackQuality: {
      provider_usage_coverage_version: version, observed_provider_usage_count: population,
      observed_provider_complete_total_count: 1, observed_provider_complete_total_tokens: 0,
    } });
    assert.equal(quality.providerUsage.totalTokens, null);
  }
});
