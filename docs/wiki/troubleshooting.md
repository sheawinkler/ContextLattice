---
title: Troubleshooting
summary: Diagnose installation, authentication, retrieval, continuation, and storage failures from the exact boundary.
eyebrow: Recover cleanly
order: 7
slug: troubleshooting
---
# Troubleshooting

Start with the exact failing command, endpoint, project, runtime identity, and error. Do not convert a generic bootstrap failure into a permission, network, or data-loss diagnosis without matching evidence.

## The runtime does not answer

```zsh
curl -fsS http://127.0.0.1:8075/health | jq
contextlattice doctor --pretty
```

If `/health` fails, inspect the orchestrator process or container and its immediate logs. Confirm that the caller is reaching the intended host and port before restarting anything.

## Health passes but retrieval stalls

A healthy container can coexist with a stalled dependency. Run the exact scoped retrieval and inspect source-level timing, continuation, DNS, queue, and backend evidence.

```zsh
contextlattice context "the query that stalls" --project affected-project --pretty
```

If the same process identity remains stable while a request spends minutes waiting, investigate the retrieval path before labeling the event a restart.

## HTTP returns 401

- Confirm the protected route actually requires `x-api-key`.
- Confirm the caller reads the current local runtime configuration.
- Restart only the caller that cached stale configuration.
- Do not paste the key into issue reports, screenshots, or shell history.

`GET /health` is intentionally unauthenticated. `/status` and protected memory routes are not.

## The CLI cannot find context

Check project spelling, topic scope, session identity, and whether the expected record was ever written. Retry a broader scoped read once if the narrow recall is empty or degraded, then inspect local source truth rather than inventing a memory.

## Results are wrong or stale

```zsh
contextlattice correct "explain the mismatch and current evidence" --category stale
```

Use `wrong` when the retrieved claim was false, `stale` when it aged out, `superseded` when a newer decision replaced it, and `useful` when it materially helped.

## Continuation never completes

```zsh
contextlattice_async_inbox_drain --session-id <session-id>
contextlattice_agent_trace --session-id <session-id> --preset proof
```

Inspect whether the slow source is still running, terminally degraded, or disconnected. Do not hide terminal degradation by repeatedly launching the same deep query.

## Disk or index pressure

Check free space at the canonical storage root, current index size, snapshots, and active write load. Resolve symlinks and mounts before cleanup or relocation. Prefer owner-native retention and snapshot tools over direct deletion of uncertain backend files.

## Agent integration is missing

```zsh
contextlattice_adopt status --pretty
contextlattice_adopt integrate \
  --repo . \
  --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid \
  --check \
  --pretty
```

Run the check from the target repository. A failed headless bootstrap without `EACCES`, `EPERM`, `Permission denied`, or `Operation not permitted` is environment evidence, not proof of a filesystem permission problem.

## A local gate and hosted CI disagree

Treat both as evidence. Reproduce a hosted test failure locally when it actually executed. If a hosted job has zero steps and no runner because of an external billing or platform block, report it as unrun infrastructure—not as a product regression and not as a passing gate.

## Still blocked

Open an [issue](https://github.com/sheawinkler/ContextLattice/issues) with the release tag, operating system, selected profile, exact command, sanitized output, and the smallest reproducible scope. Never attach `.env`, API keys, tokens, or private memory records.
