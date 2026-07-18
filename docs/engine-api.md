# Engine API (Phase 5)

This document defines the programmatic service boundary used when
`CONTEXTLATTICE_ENGINE_MODE=service`. Agents and operators should use the
`contextlattice` CLI first; HTTP and gRPC are contracts for harnesses, apps, and
advanced debugging rather than the prescribed human/agent workflow.

## Protocol Contract

- Proto file: [`proto/contextlattice_engine.proto`](./proto/contextlattice_engine.proto)
- Service: `contextlattice.engine.v1.ContextEngineService`
- RPCs:
  - `PutMemory`
  - `GetMemory`
  - `QueryMemory`
  - `BatchPut`
  - `BatchQuery`
  - `GetStats`

## HTTP Compatibility Endpoints

The Python migration proxies currently use HTTP while gRPC is rolled out.

- `POST /v1/memory/put`
- `POST /v1/memory/update`
- `GET /v1/memory/get?memory_id=...`
- `GET|POST /v1/memory/edges`
- `POST /v1/memory/edges/backfill`
- `POST /v1/memory/neighbors`
- `POST /v1/memory/batch-put`
- `POST /v1/retrieval/query`
- `POST /v1/retrieval/query-with-grounding`
- `POST /v1/retrieval/batch-query`
- `GET /v1/retrieval/health`

### Context Package Compiler

`contextlattice context "<task>" --project <project>` is the prescribed agent
front door and returns compact `agent_packet.v1` by default. At the protocol
layer, `POST /memory/context-pack` remains the canonical compiler boundary;
`POST /tools/context_pack` and `contextlattice_pack` remain compatibility and
debugging wrappers.

`POST /memory/synthesis-pack`, `POST /tools/synthesis_pack`, and
`contextlattice_synthesis_pack` build on the same compiler output and return
`synthesis_pack.v1`: grouped high-signal findings, topic gravity, memory-graph
and cross-project bridges, must-not-forget constraints, next actions, open
questions, semantic tags, quality signals, and a bounded synthesis
`reference_prompt`. This is deterministic synthesis over cited context-pack
evidence; it does not add uncited LLM claims.

Compiler responses include:

- `context_compiler`: strategy, intended use, recommended surface, guardrails,
  and ranked evidence count.
- `context_pack.ranked_evidence`: bounded evidence ordered by task usefulness
  with reason, confidence, source, file, and topic metadata when available.
- `context_pack.prompt_sections`: objective, project/topic/session objective
  hierarchy, objective lineage, next action, files, commands, checks, risks,
  capabilities, constraints, and source coverage.
- `objective_hierarchy` and `objective_lineage`: top-level bounded objects on
  context-pack responses, objective runtime state, policy packages, and session
  traces so agents can preserve the project objective through topic/subtopic and
  session handoffs.
- `reference_prompt`: a bounded human-readable block that can be handed to the
  next LLM call instead of raw logs or transcript replay.

The compiler preserves the same output boundary guarantees as other agent
contracts: no raw provider overflow shapes, no secrets fields, no unbounded
lists, and no log-level/volatile artifacts promoted into graph-backed evidence.

### Verified Utility Ledger

The CLI is the prescribed interface:

```bash
contextlattice utility status --project <project> --pretty
contextlattice utility record --session-id <id> --context-pack-quality-sample-id <id> --outcome-id <id> ... --pretty
contextlattice utility verify --agent-id <verifier-id> --session-id <id> --sample-id <id> --outcome-id <id> ... --pretty
contextlattice utility analytics --project <project> --pretty  # paid/private
contextlattice utility gate --project <project> --minimum-pairs 2 --pretty  # paid/private, advisory
```

HTTP integration fallbacks are:

- `GET /telemetry/utility`: public bounded `utility_ledger.v1` observations,
  exclusions, task classes, and aggregate economics.
- `GET /telemetry/utility/analytics`: entitlement-gated
  `utility_analytics.v1` daily cohorts, task-class economics, and interval
  readiness.
- `POST /telemetry/utility/policy/evaluate`: Operator/Enterprise entitlement-gated advisory policy evaluation
  `utility_policy_evaluation.v1`; it never activates policy or writes ordinary
  memory.
- `POST /telemetry/context-pack-quality/outcome`: records the outcome claim,
  observed provider usage when supplied, economics, and matched-control
  metadata.
- `POST /v1/agents/sessions/event`: records the exact
  `verification.completed` receipt linked by event ID.

The ledger persists bounded, fsync-backed NDJSON under the configured
memory-store root. It
does not persist prompts, completions, source text, or secrets. Wire tokens,
exact model-visible ContextLattice tokens, and observed provider totals remain
separate. Observed yield requires a verifier-bound event whose event
`agent_id` matches the declared verifier, plus exact utility evidence and an exact
model-visible denominator. Causal gain additionally requires one unique,
leakage-free matched control with the same task digest, experiment and
assignment digest, matching method, model, runner, harness, context
reconstruction contract, task class, project, and utility unit. Mixed utility
units are never summed into one metric. Both arms must also use the same exact
model-visible token count and tokenizer encoding; asymmetric denominators
abstain instead of becoming a causal claim. All reads accept optional `project`,
`task_class`, `utility_unit`, `from`, and `to` filters. No timestamp matching,
provider usage, cost, verification, or causal effect is inferred.

Durability is acknowledgement-critical: if the Utility Ledger is configured
but its owner-only file cannot initialize, replace atomically, fsync, or compact,
the outcome endpoint returns `503 utility_persistence_unavailable` and does not
add the claim to the in-memory Utility Ledger. The authoritative Context Pack
outcome remains separately recorded. An ambiguous persistence failure latches
the ledger closed until runtime restart; startup recovery binds any valid
uncertain bytes to the first source-claim digest before accepting more claims.
An owner-only lifetime lock enforces one writer per configured ledger path; a
second gateway process fails its Utility Ledger closed instead of serving stale
or conflicting observations.
A verification event that is durably recorded while reconciliation fails keeps
HTTP 200 to prevent blind replay but returns top-level `ok:false`, `partial:true`,
and `event_recorded:true`; the CLI mirrors that state and exits nonzero.
Explicitly disabling
`GO_UTILITY_LEDGER_ENABLED` stops derived Utility Ledger recording entirely.

### Memory Edge Backfill

`POST /v1/memory/edges/backfill` is dry-run by default. It supports deterministic explicit backfill relations (`same_topic`, `references`, `same_session`, low-confidence `same_agent` audit rows) and opt-in bounded inferred scoring.

Graph backfill intentionally excludes log-level and volatile operational artifacts before candidate generation. Telemetry topics, log/event file extensions, low-value rollups, and root JSON churn such as `*__latest.json` are not treated as graph memories; agents should write durable run analysis, decisions, summaries, and handoffs when they want those events to influence relationship recall.

Key optional fields:

- `dry_run` (default `true`): set `false` to persist eligible edges.
- `relations`: restrict generated relations, e.g. `["inferred_related"]`.
- `min_confidence` (default `0.95`): write threshold for all generated edges.
- `include_inferred` (default `false`): enables bounded same-project `inferred_related` scoring.
- `inferred_peer_limit` (default `2`): maximum inferred peers considered per source memory.
- `inferred_scan_limit` (default `5000`): maximum docs scanned for inferred scoring.
- `inferred_min_score` (default `0.90`): minimum inferred score to report.
- `inferred_min_shared_terms` (default `3`): minimum lexical overlap before scoring.
- `inferred_max_token_postings` (default `64`): skips overly-common terms to bound fanout.
- `corpus` (default `history_index`): `history_index` uses the live bounded hot index; `disk` scans project files and is intended for older project-scoped retrofills.

Operator-safe retrofill wrapper:

```bash
./scripts/agent/memory-edge-inferred-retrofill --project my-project
./scripts/agent/memory-edge-inferred-retrofill --project my-project --profile exploratory
./scripts/agent/memory-edge-inferred-retrofill --project my-project --profile exploratory --write --confirm-retrofill my-project
./scripts/agent/memory-edge-inferred-retrofill --project hermes-agent-ultra --corpus disk --profile exploratory
```

The wrapper restricts the request to `inferred_related`, runs a dry-run preflight before any write, refuses truncated preflight results unless `--allow-truncated` is set, and repeats write mode once to verify idempotency. Profiles provide density presets: `strict` (`0.90`, peer `1`, postings `64`), `balanced` (`0.85`, peer `3`, postings `128`), and `exploratory` (`0.80`, peer `5`, postings `256`). Explicit flags override the selected profile.

### Memory Graph Quality

`GET /telemetry/memory/graph` reports graph health, per-project quality score,
repair reasons, stale inferred edge counts, and over-connected anchor counts.
The bounded maintenance wrapper consumes that telemetry:

```bash
contextlattice_memory_graph_repair --project my-project --pretty
contextlattice_memory_graph_repair --project my-project --write --confirm-project my-project --max-writes 500 --pretty
contextlattice_memory_graph_efficacy --refresh-cases --project my-project --graph-max-cases 3 --pretty
./scripts/agent/memory-graph-quality --all-projects --pretty
```

Scheduled mode is available through `make memory-graph-quality-install`. The
launchd runner defaults to dry-run scoring and writes only when
`CONTEXTLATTICE_GRAPH_QUALITY_WRITE=1` is set at install time. Repairs are
project-scoped, dry-run first, capped by `--max-write-edges`, and use
`history_index` unless disk corpus is explicitly allowed. The gateway's
`max_writes` gate limits new edges rather than candidate scanning, so repeated
batches make forward progress instead of stopping at already-existing edges.

Graph-aware refresh stores `graph_expected_files` separately from the direct
seed's `expected_files`. Saved evaluation reports `graphEfficacyStatus=passed`
only when direct recall passes and at least one explicit graph target is added
and hydrated into bounded target-memory evidence; dangling edges fail the gate.
Ordinary cases with zero graph expectations do not dilute the lift denominator.

`POST /maintenance/memory/graph/prune-volatile` compacts the persisted edge log
under the same graph artifact policy. It is dry-run by default; send
`{"dry_run": false}` to rewrite `memory_edges.ndjson` without volatile legacy
edges and rebuild the active in-memory graph index.

## Runtime Flags

- `USE_RUST_CODEC`
- `USE_RUST_MEMORY`
- `USE_RUST_RETRIEVAL`
- `USE_GO_ORCHESTRATOR`
- `CONTEXTLATTICE_ENGINE_MODE` (`embedded` or `service`)
- `CONTEXTLATTICE_ENGINE_URL` (Rust engine service URL)
- `CONTEXTLATTICE_GO_ORCHESTRATOR_URL` (Go scheduler URL)
- `MIGRATION_SHADOW_DUAL_RUN`
- `MIGRATION_CANARY_ENABLED`

## Embedded vs Service Mode

- `embedded`: Python callbacks execute in-process; this is the default.
- `service`: Rust/Go proxies are allowed to call remote engine services.

Compatibility mode:

- `CONTEXTLATTICE_ENGINE_URL=http://contextlattice-orchestrator:8075` enables service-mode cutover against built-in `/v1/memory/*` and `/v1/retrieval/*` compatibility endpoints while Rust engine binaries are rolled out incrementally.

## Rollback

Set all feature flags to `false` to route everything through the legacy Python path without deploy rollback.
