# Agent Packet and Session Truth Eval Ledger

Date: 2026-07-12

## Objective

Make ContextLattice's normal agent path compact, coherent, honest about evidence
and token transport, stable across task continuation, and capable of learning
from outcomes without expanding the default tool surface.

## Frozen Baseline

Measured against the pre-change local runtime with the same representative
ContextLattice task query:

| Surface | Serialized response tokens |
| --- | ---: |
| Retrieval plan | 1,838 |
| Run advisor | 2,366 |
| Context pack | 15,363 |
| Synthesis Pack v2 | 21,757 |
| Session context package | 7,895 |
| Session status | 21,813 |
| Session list, 20 rows | 66,240 |
| Search, 6 rows | 10,481 |

The context-pack payload reported 2,510 compiled prompt tokens while its full
serialized response cost 15,363 tokens. Transport cost was not represented in
the aggregate ledger. Runtime quality telemetry held 1,027 context-pack samples
but only 11 outcomes, all positive; observed provider-usage and runner-quality
samples were both zero. Session starts proliferated because repeated task entry
did not have an exact reusable task identity.

## Metrics And Gates

| Metric | Gate |
| --- | --- |
| Compact packet transport | Target 2,000 tokens; hard maximum 4,000 |
| Transport accounting | Serialized JSON counted with configured tokenizer |
| Savings integrity | Never claim savings when serialized transport exceeds baseline |
| Session identity | Same agent/project/task/repo/branch/worktree reuses one live session |
| Terminal state | Completed, failed, canceled, and expired sessions never reopen |
| Async lifecycle | At most one progress steering and one absorbing terminal steering |
| Retrieval action | Weak evidence abstains; partial or contradictory evidence verifies |
| Outcome learning | Normal `finish` reports the pending retrieval outcome automatically |
| Mutation safety | Dashboard actions are copy-only; correction mutates facts only with explicit claim fields |

## Holdouts

- Duplicate evidence crossing source and provenance boundaries.
- Current task-aligned evidence competing with stale or superseded claims.
- Missing, low-alignment, partial-source, and contradictory evidence.
- Transient async pressure followed by durable retry and late duplicate events.
- Cached session reuse, terminal-session conflict, and idle expiry.
- Historical token-impact rows without transport-inclusive fields.
- Dashboard requests attempting to override full output or exceed token limits.

## Cost, Latency, And Tool Calls

- Normal agent workflow is six CLI verbs under one installed executable:
  `context`, `resume`, `remember`, `finish`, `correct`, and `doctor`.
- `context` performs one operator-visible call and uses the existing session and
  synthesis boundaries internally; no new MCP tools or services are added.
- Compact projection is deterministic and adds no LLM inference cost.
- Token counting reuses the configured tokenizer and persists only bounded
  numeric telemetry, never source, prompt, or response text.
- Live latency and post-deploy packet sizes are recorded in the release proof;
  unit gates cover bounds and state invariants independently of machine speed.

## Reproduction

```bash
cd services/gateway-go
go test -count=3 ./...

cd ../..
scripts/agent/generate-agent-contract-types --check
scripts/agent/audit-agent-output-contracts --pretty
scripts/agent/audit-agent-global-install-smoke --pretty
cd crates
cargo test -p context_codec

cd ../contextlattice-dashboard
npm test
npx tsc --noEmit
npm run build
```

Live proof after deployment:

```bash
contextlattice doctor --pretty
contextlattice context "agent packet release proof" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice remember "release proof checkpoint" --project contextlattice --pretty
contextlattice finish "release proof completed" --success --project contextlattice --pretty
```

## Known Failure Evidence

- A focused CLI suite initially exposed a stale per-user test cache crossing test
  cases. The fixture now isolates its home directory; production behavior keeps
  validating cached session IDs before reuse.
- Dashboard tests initially lacked generated Prisma client artifacts after an
  install with lifecycle scripts disabled. `prisma generate` restored the
  committed build workflow; no runtime architecture was changed.
