---
title: Operations
summary: Operate ContextLattice with evidence-first health checks, scoped restarts, storage discipline, and release gates.
eyebrow: Run the system
order: 6
slug: operations
---
# Operations

Stable uptime comes from separating liveness, dependency readiness, real retrieval, process identity, and storage pressure. Diagnose the failing layer before changing the runtime.

## The operating ladder

1. Confirm the exact host and runtime profile under test.
2. Check `/health` for orchestrator liveness.
3. Check authenticated `/status` and `contextlattice doctor --pretty`.
4. Run a bounded real search or write/readback in the affected scope.
5. Inspect dependency, queue, continuation, and storage evidence.
6. Change only the smallest reversible component implicated by that evidence.

## Health checks

```zsh
curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS http://127.0.0.1:8075/healthz | jq
curl -fsS http://127.0.0.1:8075/readyz | jq
contextlattice doctor --pretty
contextlattice_agent_runtime_proof --pretty
```

`/healthz` is gateway liveness and remains HTTP 200 while a large local store
hydrates behind a fail-closed route gate. `/readyz` is dependency readiness: it
returns HTTP 503 until required memory state and Qdrant payload indexes are
ready. In strict Go/Rust mode the legacy Python backend is explicitly
`not_required`; it is not reported as a failed dependency.

The one-command runtime proof exercises bootstrap, scoped recall, checkpoint, handoff, context package, completion, status, and telemetry. Use it when a release or incident needs more than a liveness check.

## Retrieval verification

Use the normal CLI against the affected project and query.

```zsh
contextlattice context "the exact failing retrieval question" --project affected-project --pretty
```

Inspect which sources answered, which degraded, whether continuation is pending, and whether returned evidence belongs to the requested scope.

## Restart discipline

Before stopping or restarting anything, identify the exact process or container, executable, parent, working directory, and dependents. Avoid family-wide kills and broad container restarts.

For a runtime change, rebuild the affected artifact fully, restart only its owned service, and rerun the real retrieval proof before claiming live verification.

## Storage discipline

- Resolve symlinks and mount ownership before moving data.
- Confirm the canonical hot and cold storage tiers.
- Measure free space and current write load.
- Keep snapshot, index, and telemetry growth inside explicit ceilings.
- Avoid placing a large read or rebuild on an already hot write lane.

Storage guardrails should fail visibly before a backend silently consumes the host.

## Retrieval modes under load

Use fast, scoped reads during tight engineering loops. Let continuation handle slower sources. Escalate to balanced or deep only when coverage is insufficient, and preserve the exact query and source evidence when comparing modes.

## Checkpoints

Write checkpoints after meaningful verified transitions: a reproduced failure, a merged fix, a passed gate, a deployment identity, or a known blocker. A checkpoint should be small enough for a future agent to use without re-reading the entire incident.

```zsh
contextlattice_checkpoint \
  --project contextlattice \
  --topic-path operations/incident \
  --file notes/operator/checkpoint.md \
  --stdin
```

## Release posture

Local deterministic gates are the primary engineering proof. Hosted CI adds a second environment and audit trail but cannot convert an unrun or externally blocked job into a green claim.

Record:

- exact source SHA and tree;
- commands and counts for local gates;
- build and runtime identities;
- real endpoint or retrieval proof;
- projected public and paid lane identities;
- hosted status, including infrastructure blockers.

## Next

Use [Troubleshooting](troubleshooting.md) for symptom-oriented diagnosis or [Releases](releases.md) for promotion and upgrade posture.
