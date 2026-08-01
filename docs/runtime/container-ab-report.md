# Container Runtime A/B Report

> Historical report generated from a Colima-only run on 2026-05-11. It did not
> compare candidates and therefore did not establish a performance winner. The
> current macOS operating decision is recorded in
> [Container Runtime Decision](container-runtime-decision.md).

## Objective
Identify the best container runtime/framework for ContextLattice while preserving reliability and reducing host pressure.

## Run metadata
- Generated at: 2026-05-11T06:39:55.799540Z
- Artifact generated_at: 2026-05-11T06:39:23.873300Z
- API base: http://127.0.0.1:8075
- Profiles tested: colima_docker

## Results matrix

| Runtime | Status | Requests | Success | Fail | Key p95 signal | Promotion decision |
|---|---:|---:|---:|---:|---:|---|
| colima_docker | ok | 22 | 22 | 0 | 118.758ms | candidate |

## Gate evaluation
- p95 gate: pending full matrix
- degraded/error gate: pending full matrix
- startup/health gate: pending full matrix
- host memory/disk gate: pending full matrix
- soak gate: pending finalists

## Final recommendation
- This historical run made no promotion decision.
- OrbStack is the current macOS operating runtime on separate operational
  evidence, not on a claimed A/B performance victory.
- Isolation runtimes remain deferred until the product has a concrete
  untrusted-execution or multi-tenant Linux threat model.

## Rollback path
1. Switch context/runtime back to baseline (`colima_docker`).
2. Confirm daemon readiness (`docker info`).
3. Re-run baseline profile benchmark.
4. Validate `/health`, `/status`, `/memory/search`.
