# Fastembed Promotion Decision (2026-03-16)

## Decision

Promote `fastembed-rs` to active runtime use via explicit manual override.

- Gate artifact remains authoritative for benchmark evidence.
- Raw gate result: `failed` (`threshold_not_met`) against the current 20% threshold.
- Promotion path: explicit override, with reason and telemetry exposure.

## Why promote now

1. Measured median p95 improvement is still material (`16.063%`) and non-trivial for live retrieval UX.
2. Error regression is zero in sampled runs.
3. The runtime already has fallback protections and staged retrieval controls.
4. Team preference is to prioritize practical UX gains at this maturity stage rather than block on a strict threshold.

## Evidence snapshot

From `bench/results/perf_shortlist_matrix_20260316T004108Z.json`:

- baseline embedding_stress p95: `58.775ms`
- current sample p95 values: `49.334ms`, `30.753ms`, `56.807ms`
- aggregate strategy: `median`
- aggregate current p95: `49.334ms`
- improvement: `16.063%`
- gate target: `>=20%`
- gate status: `failed` (threshold not met)

## Runtime promotion controls

- `ORCH_ADAPTER_FASTEMBED_RS_ENABLED=true`
- `ORCH_ADAPTER_FASTEMBED_RS_REQUIRE_GATE=true`
- `ORCH_ADAPTER_FASTEMBED_RS_PROMOTE_OVERRIDE=true`
- `ORCH_ADAPTER_FASTEMBED_RS_PROMOTE_REASON=manual_16pct_promotion_2026-03-16`

Telemetry continues to expose:

- raw gate status (`passed`, `reason`, thresholds, metrics)
- override status (`promoteOverrideEnabled`, `promoteOverrideActive`, `promoteOverrideReason`)
- effective runtime pass (`effectivePassed`)
