import assert from "node:assert/strict";
import test from "node:test";

import { estimateContextPackQuality, estimateTokenImpact } from "@/lib/dashboardMetrics";

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
