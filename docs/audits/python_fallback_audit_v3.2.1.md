# Python Fallback Audit (v3.2.1)

Date: 2026-03-19

## Objective

Validate whether Python code in the repository is still useful, with focus on fallback/runtime relevance after Go/Rust-first cutover.

## Scope and Method

1. Inventory tracked Python files (`git ls-files '*.py'`).
2. Verify runtime fallback lane health on `:18075` (`/health`, `/migration/runtime`, `/memory/search`).
3. Verify Python services/scripts used by compose, Makefile, and launch scripts.
4. Run focused Python tests for migration/runtime + agent integration paths.
5. Flag Python files with no in-repo references for manual utility review.

## Inventory

- Total tracked Python files: `58`
- Breakdown:
  - `services/`: `20`
  - `scripts/`: `29`
  - `bench/`: `8`
  - `launch_service/`: `1`

## Fallback-Critical Python (Required)

These remain active and necessary for fallback/runtime operation:

- `services/orchestrator/app.py`
- `services/orchestrator/runtime/*`
- `services/orchestrator/runtime/adapters/*`
- `services/fastembed_sidecar/app.py`
- `services/external_spike_adapter/app.py`
- `services/lancedb_spike_adapter/app.py`
- `services/fastembed_gate/runner.py`
- `scripts/mindsdb_http_proxy.py`
- `scripts/memorybank_http_proxy.py`

Observed live checks (2026-03-19):

- `GET http://127.0.0.1:18075/health` -> `ok: true`
- `GET http://127.0.0.1:18075/migration/runtime` -> runtime adapters healthy
- `POST http://127.0.0.1:18075/memory/search` -> returns ready results with staged warnings as expected

## Focused Test Results

Passed:

- `pytest -q services/orchestrator/tests/test_migration_runtime.py` (`4 passed`)
- `pytest -q services/orchestrator/tests/test_agent_orchestration_script.py services/orchestrator/tests/test_context_expansion_runtime.py` (`5 passed`)

Known broader suite status:

- `gmake test-py` currently has existing failures in retrieval-behavior expectations (`12 failed, 169 passed`) due policy/runtime evolution; not introduced by config path canonicalization in v3.2.1.

## Utility/Operator Python (Non-fallback, still useful)

These are operator/benchmark/manual tools and remain useful even if not fallback-critical:

- Bench harnesses under `bench/`
- Ops/audit scripts (retention, storage audit, service version audit, launch lock, submission preflight)
- External runner shims under `scripts/agent_runners/`
- Terminal/monitoring helpers (`scripts/terminal_dashboard.py`, `scripts/monitor_opus.py`)
- Launch/copy generation tooling (`launch_service/generate_launch_docs.py`)

## Unreferenced-in-repo Entry Scripts (Manual-use utilities)

The following have low/no direct in-repo references but are valid operator entrypoints (manual invocation):

- `launch_service/generate_launch_docs.py`
- `scripts/gateway_autoreg.py`
- `scripts/monitor_opus.py`
- `scripts/seed_qdrant.py`
- `scripts/terminal_dashboard.py`
- `scripts/agent_runners/autogen_runner.py`
- `scripts/agent_runners/crewai_runner.py`
- `scripts/agent_runners/langgraph_runner.py`
- `scripts/agent_runners/letta_runner.py`
- `scripts/agent_runners/openhands_runner.py`
- `scripts/agent_runners/trae_runner.py`

## Conclusion

Python remains necessary for fallback/runtime continuity and operational tooling.
No Python fallback-critical files were identified as removable in this audit pass.
