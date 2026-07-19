# Continuity Zero

Open an agent. Already there.

Continuity Zero turns the current repository and one unambiguous active objective into a bounded `continuity_zero.v1` manifest. It binds the session packet, latest checkpoint, effective Agent Fit profile, eligible preparation artifact, ownership scope, repository and commit identity, Context Passport provenance, optional Context Mesh grant, known risks, and the next useful move.

## Primary CLI

Run from the repository you want to resume:

```bash
contextlattice_continuity_zero \
  --project contextlattice \
  --agent codex \
  --output continuity-zero.json \
  --pretty
```

The CLI derives the Git origin, repository aliases, branch, and commit with argv-list subprocess calls. It strips URL credentials before reducing remotes to repository identity, and it does not send the current working directory or any local path to the gateway. When `--output` is present, it atomically writes the manifest with owner-only permissions.

Use `--session-id` when more than one active objective is possible. Optional `--passport-id` and `--mesh-grant-id` make provenance selection explicit.

## Decision Contract

`ready` requires one fresh, non-terminal, ownership-visible session whose project, agent identity, harness, repository, branch evidence, and recorded commit agree with the current request.

`abstain` means ContextLattice found no safe unique answer. Common reasons include multiple active objectives, no active session, or missing repository, branch, or commit evidence during implicit selection.

`rejected` means an explicitly selected session cannot prove continuity, including stale sessions, absent or mismatched repository, branch, or commit evidence, unsupported harness, expired or invalid Passport, or revoked Context Mesh grant.

Missing optional profile, preparation, Passport, or checkpoint evidence remains visible as a risk. It is never silently invented.

## Safety Boundary

The public route is advisory. It does not:

- create a hidden session;
- execute a model or runner;
- mutate files, worktrees, Git, or ordinary memory;
- push or transport a manifest;
- return or transmit local paths.

The manifest asks the consuming agent to checkpoint meaningful progress after use so durable memory advances with the work.

## Entitled Automation

Starter, Team, Operator, and Enterprise runtimes can govern explicit external-adapter intents at `/memory/continuity-zero/governance`.

Supported operations are:

- `configure`: set allowed intent modes, harnesses, and a bounded pending limit;
- `enqueue`: record an approved `push` or `workspace_prepare` intent bound to the exact manifest digest, adapter, and expiry;
- `complete`: attach the external adapter's terminal receipt as `delivered`, `failed`, or `canceled`;
- `rollback`: disable or restore a bounded policy version and cancel every pending intent;
- `status`: inspect policy, intents, immutable receipts, and bounded-store health.

Every mutation requires `operator_approved=true`, an expected generation, an idempotency key, and a reason. State is owner-only and atomically persisted. Receipts are hash-linked. The gateway records intent and proof only; the external adapter owns delivery and workspace preparation.

Public builds contain the manifest and CLI. Paid artifacts add the governed intent lifecycle, while production authorization remains fail-closed.
