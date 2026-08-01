# Research Backlog Decisions

Decision date: 2026-08-01.

This record closes three broad research tracks whose actionable product
contracts have shipped or whose continuing scope is better handled by focused
evaluation issues.

## Agent runner interoperability

The supported interoperability boundary is now local, versioned, and
allowlist-driven:

- `runner_capability.v1`, `runner_result.v1`, `agent_task_lease.v1`, and
  `runner_quality_sample.v1` validate runner discovery, work, leases, and bounded
  quality telemetry.
- `universal_agent_adapter_response.v1` provides the cross-agent adapter result.
- `/ops/capabilities` and `/tools/capability_map` expose the active runtime map.
- `/memory/browser-context` accepts bounded text snapshots, passes them through
  the normal secret filter, and can be disabled by environment policy.
- Task progress exposes queued, claimed, partial, succeeded, and failed states.
- Worker tools are denied unless allowed by the local role policy.

A periodic remote MCP-registry sync is intentionally not part of the authority
path. Remote registry records may be evaluated as data, but cannot grant a tool,
expand an allowlist, or change execution policy. ContextLattice also does not add
an outbound callback/event relay merely to satisfy the old research outline;
local polling, SSE, and session events cover the supported lifecycle without a
new exfiltration surface.

## Candidate repository intake

The broad candidate catalog is closed as an evergreen implementation tracker.
It produced focused benchmark and architecture work, but repository popularity
and a static security scan do not establish product value.

Any future candidate gets its own bounded issue and must provide:

1. a pinned revision, license decision, dependency/secret scan, and sandboxed
   build path;
2. a current ContextLattice baseline and representative holdout;
3. retrieval quality plus p50/p95/p99, timeout, throughput, memory, and cost;
4. exact tool calls, failures, and a reproduction artifact;
5. a feature flag or isolated adapter and a tested rollback;
6. a keep, drop, or defer decision with an owner and retirement condition.

No candidate is integrated simply to keep a catalog issue active.

## Objective, goal, and mission harmonization

`objective_runtime_state.v1` and `policy_context_package.v1` are the product
contract for mission, objective, goal, hierarchy, lineage, action, evidence,
risk, and next action. Preflight, context packs, run advisor, objective graph,
session trace, and continuation flows preserve that contract instead of asking
each runner to invent its own interpretation.

The acceptance boundary is deterministic contract and recall behavior, not an
ever-changing comparison against popular agent repositories or social-media
discussion. New work requires a reproduced contract, coherence, provenance, or
objective-aware recall failure with a focused holdout.
