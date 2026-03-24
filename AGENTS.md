# ContextLattice Migration Rules

## Objective

Migrate the Python implementation to a hybrid architecture:

- Python: SDK + developer interface
- Rust: memory + retrieval engine
- Go: orchestration services

## Agent Endpoint Pinning (Required)

- External agents must call the orchestrator endpoint at `http://127.0.0.1:8075` by default.
- If users run this on a different host/port, they must explicitly override:
  - `CONTEXTLATTICE_ORCHESTRATOR_URL`
  - `CONTEXTLATTICE_ORCHESTRATOR_URL`

## Agent Integration Defaults

- External agents should use named preflight profiles:
  - `codex`
  - `claude-code`
  - `opencode`
  - `hermes-agent`
  - `chatgpt-web`, `chatgpt-desktop`
  - `claude-web`, `claude-desktop`
- Use stable agent identity for reads/writes so profile defaults apply:
  - `CONTEXTLATTICE_AGENT_ID` (defaults to `codex_gpt5`)
  - `CONTEXTLATTICE_AGENT_ID`
- Before major work, run one of:
  - `python3 scripts/agent_orchestration.py preflight contextlattice runbooks/codex-integration`
  - `python3 scripts/agent_orchestration.py preflight-agent claude-code contextlattice`
  - `python3 scripts/agent_orchestration.py preflight-agent opencode contextlattice`
  - `python3 scripts/agent_orchestration.py preflight-agent hermes-agent contextlattice`
- Gateway preflight routes:
  - `POST /v1/codex/preflight` (compatibility alias)
  - `POST /v1/agents/preflight` (generic profile-aware preflight)

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
