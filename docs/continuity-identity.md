# Continuity Identity

Agents should be able to change sessions, models, accounts, branches, and
worktrees without forgetting which piece of work they are actually advancing.
They should not be allowed to fuse two merely similar tasks and call that
continuity.

ContextLattice separates three things that agent stacks routinely collapse:

- **task identity**: the durable piece of work;
- **execution lane**: the branch, worktree, cwd, agent, and repo combination
  currently acting on it;
- **session identity**: one bounded runtime conversation or run.

That separation keeps parallel work visible without making every new session a
new objective or every similar sentence the same task.

## Exact first. Ambiguity stops.

`contextlattice_continuity_reconcile` resolves in this order:

1. explicit task identity;
2. exact external task ID;
3. exact normalized objective;
4. semantic candidate search only after every exact path misses;
5. new identity creation only when no qualifying semantic candidate exists.

Semantic candidates use deterministic bounded scoring. A candidate must clear
the score threshold and the margin over the runner-up, but even then it is an
advisory result: `semantic_auto_merge=false`, `abstained=true`, and
`requires_confirmation=true`. Similarity never silently reuses or merges a
task.

```bash
contextlattice_continuity_reconcile "ship continuity identity" \
  --project contextlattice \
  --repo contextlattice \
  --task-id frontier-t1 \
  --branch main \
  --agent-id codex_gpt5 \
  --pretty
```

Manual merge and split operations require explicit operator attribution and a
reason. They cannot cross project or repo scope, and every create, merge, and
split writes an immutable hash-chained receipt. Task identity IDs and execution
lane IDs are opaque and case-sensitive. Merge and split operations derive a
stable retry key, but durable operators should supply `--idempotency-key` and
reuse it unchanged after timeouts or ambiguous responses.

```bash
contextlattice_continuity_reconcile \
  --operation merge \
  --target-task-identity-id <target-id> \
  --source-task-identity-ids <source-id> \
  --actor <operator-id> \
  --idempotency-key merge-verified-deliverable-01 \
  --reason "Verified both records describe the same deliverable." \
  --pretty

contextlattice_continuity_reconcile \
  --operation split \
  --source-task-identity-id <source-id> \
  --task-identity-id <new-id> \
  --objective "Independent deliverable" \
  --actor <operator-id> \
  --idempotency-key split-independent-deliverable-01 \
  --reason "Evidence proves this work is independent." \
  --pretty
```

## Objectives that survive the handoff

`contextlattice_objective_transition` appends typed state and linkage events.
Lifecycle transitions include `created`, `started`, `progressed`, `blocked`,
`resumed`, `completed`, `abandoned`, and `reopened`. Provenance transitions can
link dependencies, supersession, tasks, sessions, lanes, decisions, outcomes,
and checkpoints without implicitly changing lifecycle state.

```bash
contextlattice_objective_transition "ship continuity identity" \
  --project contextlattice \
  --type progressed \
  --actor codex_gpt5 \
  --idempotency-key frontier-t1-progress-contracts \
  --task-identity-id <task-id> \
  --outcome-id <outcome-id> \
  --checkpoint-id <checkpoint-id> \
  --summary "Native contracts and route ownership verified." \
  --pretty

contextlattice_objective_graph \
  --project contextlattice \
  --objective-id <objective-id> \
  --as-of 2026-07-14T12:00:00Z \
  --pretty
```

The graph is replayed from append-only transitions. An as-of query includes an
event only when both its effective time and immutable ledger-recorded time are
inside the view, so late backdated evidence cannot appear before ContextLattice
actually knew it and future events do not leak into a default-now response.

One bounded limit governs objective nodes, typed edges, and returned
transitions. Indexed selection, relation traversal, and transition replay each
have hard inspection budgets. The complete, selection, traversal, replay,
node-link, edge, transition, and output-boundary compaction fields make every
partial view explicit instead of silently allocating or returning an unbounded
component. Contract enforcement keeps the serialized graph below 500 KB and
reconciles every count after compaction.

Objective writes are idempotent. The CLI creates a request key when one is not
supplied, while durable workflows should pass a stable `--idempotency-key` (or
an explicit transition ID) across retries. Replaying the same key and payload
returns the persisted transition without appending; reusing the key for
different content is rejected.

## Decision changes without thought surveillance

`decision_change.v1` captures the operational evidence behind a changed
choice:

- before and after decisions;
- evidence that triggered the change;
- confidence before, after, and delta;
- alternatives considered;
- actor, concise rationale, reason code, and verification.

It rejects chain-of-thought, hidden-reasoning, raw-reasoning, and analysis-trace
fields. The decision and its linked objective transition are persisted as one
ledger entry, so a crash cannot commit only half of the relationship.
ContextLattice needs durable provenance, not private scratch work.

```bash
contextlattice_decision_change \
  --project contextlattice \
  --objective-id <objective-id> \
  --idempotency-key frontier-t1-semantic-abstention \
  --before "reuse every semantic match" \
  --after "require confirmation after exact miss" \
  --confidence-before 0.45 \
  --confidence-after 0.92 \
  --evidence <evidence-ref> \
  --alternatives "exact-only,no-continuity" \
  --actor codex_gpt5 \
  --rationale "Ambiguous candidates must abstain." \
  --reason-code evidence_changed \
  --verification-status verified \
  --verification-method deterministic_test \
  --pretty

contextlattice_decision_change list \
  --project contextlattice \
  --objective-id <objective-id> \
  --limit 50 \
  --pretty
```

Decision writes use the same stable retry contract. Wording-only and
evidence-only restatements remain ordinary provenance; a changed conclusion or
a real confidence delta is decision history. List responses expose exactness,
omitted counts, hard inspection limits, and an opaque `next_cursor`. Continue a
partial query with `--cursor <next-cursor>`. Cursors are bound to the original
project, objective, and frozen as-of instant; changing any scope input requires
a fresh query.

## Storage and failure boundary

The continuity ledger is local, owner-only, bounded, append-only, and
hash-chained. Startup verifies sequence and every hash before enabling writes.
One process owns the ledger writer lock at a time; a second writer fails before
serving continuity traffic instead of racing append state.
An unparseable final fragment from an interrupted append is removed while every
previous committed hash remains intact; malformed committed data, tampering,
capacity exhaustion, and persistence ambiguity still fail closed.

Compaction is explicit, lossless, and off the request hot path. It atomically
rewrites the same verified entries into canonical NDJSON without deleting
history or changing the chain head:

```bash
contextlattice_continuity_reconcile \
  --operation compact \
  --actor <operator-id> \
  --reason "Canonical maintenance after verified readback." \
  --pretty
```

Environment controls:

- `CONTEXTLATTICE_CONTINUITY_ENABLED`, default `true`;
- `CONTEXTLATTICE_CONTINUITY_LEDGER_PATH`, optional local override; when unset,
  the ledger follows `GO_MEMORY_STORE_ROOT` before using the repo-local
  development fallback;
- `CONTEXTLATTICE_CONTINUITY_LEDGER_MAX_BYTES`, default 64 MiB;
- `CONTEXTLATTICE_CONTINUITY_LEDGER_MAX_ENTRIES`, default 100,000;
- `CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC`, default `true`.

The CLI is the primary interface. HTTP remains the integration fallback:

- `POST /memory/continuity/reconcile` for reconcile, merge, split, or lossless
  compaction;
- `POST /memory/objectives/transition`;
- `GET /memory/objectives/graph`;
- `POST|GET /memory/decision-changes`.

No new MCP tools, daemon, scheduler, subprocess runner, or Python live-runtime
dependency is introduced.
