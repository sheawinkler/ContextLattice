# Container Runtime A/B Baseline

> Historical benchmark input. This file preserves the May 2026 A/B control and
> artifact contract; it is not the current macOS operating decision. See
> [Container Runtime Decision](container-runtime-decision.md).

This document captures baseline assumptions, candidate tiers, and promotion gates for container-runtime A/B testing.

## Canonical issue
- `#88` — [research]: containerization options & performance

## Historical control runtime
- `colima_docker` (Colima + Docker Engine)

## Candidate pool (expanded, non-lazy)

### Tier 1 — macOS-local practical candidates
1. `colima_docker` (control)
2. `orbstack`
3. `podman_machine`
4. `apple_container`
5. `finch`

### Tier 2 — Linux/AWS microVM lanes (performance-focused)
6. `kuasar_linux`
7. `firecracker_linux`

Notes:
- Docker Desktop is intentionally removed from active candidate pool.
- Tier 2 runs are staged on Linux/AWS when local feasibility is insufficient.

## Benchmark harness
- `bench/container_runtime_matrix.py`
- profile definitions: `bench/runtime_profiles/*.yaml`

## Safety policy
- Sequential profile execution only.
- Conservative workload defaults.
- Runtime switching is opt-in (`--apply-switch`).
- Soak disabled by default unless explicitly requested.
- Keep heavy benchmark artifacts under WD Black paths.

## Promotion gates
- `p95 <= control * 0.95` OR parity with materially lower host pressure.
- Error/degraded rate no worse than control.
- Startup health no worse than +10% unless compensated by significantly lower resource usage.
- No daemon instability/crash loops.

## Artifact contract
- Machine readable: `bench/results/container_runtime_matrix_<timestamp>.json`
- Stable pointer: `bench/results/container_runtime_matrix_latest.json`
- Human summary: `docs/runtime/container-ab-report.md`
