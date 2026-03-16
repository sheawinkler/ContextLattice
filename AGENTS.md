# ContextLattice Migration Rules

## Objective

Migrate the Python implementation to a hybrid architecture:

- Python: SDK + developer interface
- Rust: memory + retrieval engine
- Go: orchestration services

## Non-goals

- Do not rewrite the entire system at once.
- Preserve current behavior unless explicitly approved.

## Working Rules

1. Benchmark before optimizing.
2. Every phase must leave the repository runnable.
3. All migrations must be behind feature flags.
4. Preserve API compatibility.
5. Prefer small reviewable commits.
6. Add parity tests before replacing functionality.
7. Document service boundaries.
8. Never claim performance improvements without benchmarks.

## Priorities

1. correctness
2. benchmarked performance
3. rollback safety
4. code clarity
5. developer ergonomics

## Deliverables Per Phase

- code
- tests
- benchmarks
- documentation
- migration notes

## Current Tasking

Phase 1-8 migration execution is active:

- keep Python behavior as the default runtime path
- route hot paths through migration interfaces behind feature flags
- maintain rollback-safe Rust/Go scaffolding (`USE_*` flags)
- enforce parity tests and benchmark validation before any default cutover
- document service contracts and migration runtime health endpoints

Do not remove Python fallback paths until benchmark and parity gates pass.
