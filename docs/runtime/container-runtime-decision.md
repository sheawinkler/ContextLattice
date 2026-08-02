# Container Runtime Decision

Decision date: 2026-08-01.

## Decision

OrbStack's Docker context is the canonical macOS runtime for local development,
installed-runtime verification, and release proof:

```zsh
docker --context orbstack ...
```

This is an operating decision, not a claim that OrbStack is universally faster
than every container or microVM runtime. The historical A/B artifact tested only
Colima, so it cannot support that claim.

## Evidence basis

- The installed host exposes the `orbstack` context and an OrbStack Docker
  server.
- The full ContextLattice service set is currently running through that context,
  including healthy gateway, dashboard, orchestrator, memory, and supporting
  services.
- ContextLattice's host-forward guard, lifecycle supervision, Compose checks,
  and recent installed release proof all target the explicit OrbStack context.
- The benchmark harness remains available for future comparative measurements;
  the May 2026 report is retained as historical evidence rather than rewritten
  as a comparison it never performed.

## Operator contract

- Pass `--context orbstack` to Docker commands. Do not rely on an ambient context
  when recording release evidence.
- Identify exact processes, containers, dependents, source commit, and image
  identity before a stop or restart.
- A successful Compose render or health endpoint is necessary but insufficient:
  installed proof also exercises write/read retrieval, storage dependencies,
  restart persistence, rollback, and OOM/restart counters.
- Colima remains a compatibility and rollback reference, not the current macOS
  release-proof authority.

## Deferred isolation research

Do not keep Firecracker, Kata, gVisor, Sysbox, or Kuasar research open as an
evergreen performance task. Open a focused evaluation only when at least one of
these conditions exists:

1. untrusted code must execute inside a ContextLattice-managed boundary;
2. a multi-tenant Linux deployment needs stronger tenant isolation;
3. a supported host cannot satisfy the OrbStack or standard Compose contract;
4. a measured runtime bottleneck is attributable to the current container
   boundary.

That evaluation must name the threat model, pin versions and source revisions,
hold application code constant, capture p50/p95/p99, throughput, errors, CPU,
memory, I/O, startup, and cost, and include a tested rollback path. A candidate
is promoted only for the lane actually measured.

## Rollback

Changing runtime is an operator action, not an automatic repair. Preserve the
current Compose project and volume identities, switch only after an explicit
operator decision, and rerun the same installed write/read and restart proof on
the replacement runtime.
