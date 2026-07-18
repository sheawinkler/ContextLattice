# Recall Quality Loop

ContextLattice exposes a single recall quality contract across agent, terminal, and dashboard surfaces:

- `POST /memory/recall/evaluate/saved` runs saved recall cases and emits recall@K, MRR, numeric exactness, citation coverage, source diversity, latency, and graph-neighbor contribution metrics.
- `GET /telemetry/recall` reports the latest saved-eval sample beside source health.
- `GET /telemetry/recall/tuning` recommends threshold tuning, source order, and first-hop graph expansion limits from recent monitor samples.
- `scripts/agent/recall-quality-eval --tuning` gives a terminal quality view suitable for release gates and agent handoffs.
- `scripts/agent/recall-quality-eval --derive` proposes immutable review-only regressions from independently verified outcomes; it never admits cases automatically.
- `scripts/agent/recall-quality-eval --ablation` runs same-snapshot leave-one-source and leave-one-memory analysis without inferring utility from rank movement.
- `scripts/agent/recall-quality-eval --reputation --project <project>` shows advisory source/file/agent/memory calibration. Reputation cannot override quarantine, contradiction, or opposition.
- `scripts/agent/audit-open-core-boundary` checks that `origin/main`, `public/main`, and `public-paid/main` preserve the lite/full/paid feature boundary before sync.

The graph contribution score is evidence-only at evaluation time: it measures whether first-hop memory edges would recover a missed expected file or term without changing retrieval ranking in the evaluator. Use a positive `graphLift` signal to justify enabling graph expansion in product-boundary context packaging.
