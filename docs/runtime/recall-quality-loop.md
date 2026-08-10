# Recall Quality Loop

ContextLattice exposes a single recall quality contract across agent, terminal, and dashboard surfaces:

- `POST /memory/recall/evaluate/saved` runs saved recall cases and emits recall@K, MRR, numeric exactness, citation coverage, source diversity, latency, and graph-neighbor contribution metrics.
- `GET /telemetry/recall` reports the latest saved-eval sample beside source health.
- `GET /telemetry/recall/tuning` recommends threshold tuning, source order, and first-hop graph expansion limits from recent monitor samples.
- `scripts/agent/recall-quality-eval --tuning` gives a terminal quality view suitable for release gates and agent handoffs.
- `scripts/agent/recall-quality-eval --derive` proposes immutable review-only regressions from independently verified outcomes; it never admits cases automatically.
- `scripts/agent/recall-quality-eval --ablation` runs same-snapshot leave-one-source and leave-one-memory analysis without inferring utility from rank movement.
- `scripts/agent/recall-quality-eval --reputation --project <project>` shows advisory source/file/agent/memory calibration. Reputation cannot override quarantine, contradiction, or opposition.
- `scripts/agent/context-pack-quality-benchmark --json` evaluates the complete frozen v3 holdout against the production context-pack route, retains the unchanged server score, and reports mean, median, p10, Recall@5, MRR, citation, diversity, graph, no-hit, low-confidence, and latency evidence under one fail-closed promotion result.
- `scripts/agent/audit-open-core-boundary` checks that `origin/main`, `public/main`, and `public-paid/main` preserve the lite/full/paid feature boundary before sync.

## Frozen live case sets

`POST /memory/recall/eval-cases/refresh` writes a version-3 case set from the bounded gateway-go memory index. `max_cases` is capped at 300; an unscoped refresh samples the in-process current-state index rather than walking the external corpus. The selector is deterministic and stratified across every available project, topic, temporal-age, agent, session, source-family, lifecycle, horizon, query-intent, and difficulty dimension. Newer timestamped records are labeled `split: holdout`; older records are labeled `split: train`.

Each direct case names exactly one expected file. The query is derived from topic and summary with the path/base filename redacted. The persisted artifact includes a frozen source snapshot digest, case-set digest, population/sample counts, diversity minima, and non-synthetic custody metadata; project/topic/agent/session/source-family stratum counts are opaque digests so the proof does not publish identity/content labels. These fields are immutable evidence: hand-editing cases, duplicate direct files/queries, expected-file leakage, synthetic fallback cases, missing temporal metadata, or unmet available-stratum minima makes the saved benchmark ineligible and the evaluator fails closed.

An empty refresh is not a benchmark and never installs the built-in health/trading fallback cases. Write durable file-backed memory first, then refresh. `query_expansion` follows three-state semantics in saved evaluation: omitted means the product default remains authoritative; explicit `false` and explicit `true` are forwarded exactly.

Evaluation of more than four cases uses a fixed four-worker pool with request-context cancellation and index-ordered aggregation; ablation rows are accumulated under one global cap. The v3 cap is therefore 300 cases without inventing a larger outer timeout or multiplying per-case work.

The context-pack benchmark is also part of the default runtime install as `contextlattice_context_pack_quality_benchmark`. It requires the complete train-plus-holdout artifact and current-state-index custody even though only the frozen holdout is scored. Before the first case and after the saved-recall evaluation, it captures the bounded `/health` build identity and memory-store readiness receipt. Promotion requires the same source-bound commit, tree, version, channel, boot nonce, store reference, and owner-only writer across both receipts; complete per-case responses; zero ledger failures; mean/median/p10 quality scores of at least 90 under the server's unchanged formula; and a passing saved-recall evaluation bound to the same case-set digest.

The graph contribution score is evidence-only at evaluation time: it measures whether first-hop memory edges would recover a missed expected file or term without changing retrieval ranking in the evaluator. Use a positive `graphLift` signal to justify enabling graph expansion in product-boundary context packaging.

## Context-pack warning pressure

The context-pack quality score keeps its existing 0–100 weights. Its
`warning_count` penalty now counts only quality-impacting warnings. Every
warning remains in the agent-facing `warnings` response field; the quality
sample carries an audit-only projection:

- `warning_total_count`: all response warnings, including notices.
- `warning_count`: impacting warnings used by the score and retry model.
- `warning_notice_count`: informational or safety notices that do not reduce
  the score when their exact producer shape is recognized.
- `warning_impact_categories` and `warning_notice_categories`: bounded counts
  by deterministic category.

The notice allow-list is closed: disabled sources explicitly excluded from
effective coverage, authoritative stale-fallback suppression, exact staged
optional slow-source deferral, safety filtering of known ephemeral/current
state rows, positive lane/coverage-rescue notices, and the exact
sources-returned summary are notices. Timeouts, errors, budget exhaustion,
unavailable continuation, output clipping, coverage loss, and ambiguous or
malformed near-matches remain impacting. This prevents benign orchestration
information from lowering a high-quality pack while keeping degraded recall
and safety failures visible.
