# Cognition Proof Core

ContextLattice v3.12 adds three Go-native cognition surfaces without replacing
the v1 memory, context-pack, or synthesis contracts.

## Temporal Claim Graph

A memory result says, "this text was retrieved." A temporal claim says, "this
assertion was observed at this time, is valid over this interval, is backed by
these references, and may supersede or contradict these other assertions."

The ledger is bounded, append-only between compactions, local by default, and
safe to reload after restart. It records:

- subject, predicate, object, and human-readable statement;
- `valid_from`, `valid_to`, and `observed_at`;
- support and opposition references;
- supersession, contradiction, and causal links;
- verification state and method;
- project, topic, agent, session, branch, commit, and provenance;
- revision and durable timestamps.

CLI:

```bash
contextlattice_claim_write --project contextlattice --subject release --predicate current_version --object 3.12.0 --pretty
contextlattice_claim_query "current release" --project contextlattice --include-superseded --pretty
```

HTTP fallbacks:

- `POST /memory/claims`
- `POST /memory/claims/query`
- `POST /tools/claim_write`
- `POST /tools/claim_query`
- `GET /telemetry/claim-graph`

Contracts: `temporal_claim.v1`, `temporal_claim_query.v1`.

Environment:

- `CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED` defaults to `true`.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH` overrides the local ledger path.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_MAX` defaults to `10000`.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_COMPACT_EVERY` defaults to `512`.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC` defaults to `true`.

Invalid claims are rejected before the in-memory index changes. Superseded
claims remain queryable; history is not overwritten into a single false
"current truth."

## Adaptive Retrieval Planner

The planner classifies task phase, names evidence obligations, ranks configured
sources with observed reliability and p95 latency, proposes bounded query
expansion, allocates the token budget, and states marginal-value stop rules.

```bash
contextlattice_retrieval_plan "verify cross-project release readiness" --project contextlattice --pretty
```

HTTP fallbacks:

- `POST /memory/retrieval/plan`
- `POST /tools/retrieval_plan`

Contract: `retrieval_plan.v1`.

The public v3.12 planner is always `mode=advisor` and
`activation_state=shadow_only`. It does not mutate live retrieval policy. A
later outcome-trained canary must earn activation with holdouts and rollback.

## Proof-Carrying Synthesis v2

Synthesis v2 starts from the existing bounded Context Pack and Synthesis Pack
v1 evidence. It then emits only findings with an evidence identity and adds:

- support and opposition references per claim;
- structured temporal state and matching claim count;
- explicit causal and supersession context;
- confidence decomposition rather than one unexplained score;
- unresolved contradiction disclosure;
- missing-proof obligations;
- a count of unsupported findings excluded from the response.

```bash
contextlattice_synthesis_pack_v2 "prove release readiness" --project contextlattice --pretty
```

HTTP fallbacks:

- `POST /memory/synthesis-pack/v2`
- `POST /tools/synthesis_pack_v2`

Contract: `synthesis_pack.v2`.

The implementation is deterministic and reports `llm_used=false`. Lexical
matching can link structured claims to retrieved evidence, but it does not
claim semantic equivalence. Unverified claims cannot silently increase
confidence, and contradictions are never auto-resolved in the public core.

## Compatibility

The existing `contextlattice_pack`, `contextlattice_synthesis_pack`,
`/memory/context-pack`, and `/memory/synthesis-pack` surfaces are unchanged.
Agents can adopt v2 per workflow instead of migrating every consumer at once.

Machine-readable measurements and reproduction commands live in
`docs/evals/v3.12-cognition-proof-core.json`.
