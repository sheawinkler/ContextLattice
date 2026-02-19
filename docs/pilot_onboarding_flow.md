# Pilot onboarding flow

Updated: 2026-02-18

This is the default pilot path used for GTM execution and ROI proof.

## 1) Intake and scoping

- Identify one high-volume workflow (agent ops, coding assistant, research).
- Capture baseline context-loss symptoms and token/cost pressure.
- Confirm data sensitivity profile and deployment mode (local-first vs hosted).

## 2) Baseline window (3-5 days)

- Record request volume, token spend, completion failures, latency.
- Track long-context frequency and truncation events.
- Save baseline metrics in a worksheet tied to project and date.

## 3) Integration and tuning (3-7 days)

- Connect clients to Context Lattice via orchestrator and MCP hub.
- Enable federated retrieval + learning loop defaults.
- Tune fanout/backpressure and retention for workload shape.
- Validate queue health and sink fanout with `/telemetry/fanout`.

## 4) Measured pilot run (7-14 days)

- Run the same workflow with memory-enabled retrieval.
- Track identical KPIs against the baseline period.
- Collect qualitative quality notes from end users and operators.

## 5) ROI readout and recommendation

- Compute net token savings and failure-rate deltas.
- Summarize reliability improvements and operational effort changes.
- Provide go-forward recommendation: stay local-first, move hosted, or hybrid.

## KPI set

- Prompt token reduction (%)
- Completion success rate (%)
- Retrieval quality score (operator-rated)
- Mean response latency (P50/P95)
- Estimated monthly cost delta
