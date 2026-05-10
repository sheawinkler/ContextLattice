# Phase 0 Performance Baseline

## Scope

Phase 0 baseline for the current Python implementation prior to Rust/Go migration.

Date: `2026-03-04`  
Run ID: `20260304T102535Z`  
Harness: [tooling/python/bench/phase0_baseline.py](tooling/python/bench/phase0_baseline.py)  
Raw results: [bench/results/phase0_baseline_20260304T102535Z.json](bench/results/phase0_baseline_20260304T102535Z.json)

## Python Architecture Inventory

Python footprint in-repo:

- Total Python files: `31`
- Core hot path: [services/orchestrator/app.py](services/orchestrator/app.py) (`15,544` LOC)
- Agent/task scripts: `scripts/` + `scripts/agent_runners/`
- Existing perf-related scripts:
  - [scripts/load_test_memory_write.py](scripts/load_test_memory_write.py)
  - [scripts/retrieval_soak_monitor.py](scripts/retrieval_soak_monitor.py)

## Workload Results

### 1) Single-agent short context

- Requests: `15`
- Success: `15/15`
- p50: `1797.719 ms`
- p95: `18050.941 ms`
- p99: `18073.732 ms`
- Throughput: `0.1993 rps`

### 2) Multi-agent medium context

- Requests: `12`
- Success: `8/12`
- p50: `21053.903 ms`
- p95: `40002.845 ms`
- p99: `40002.880 ms`
- Throughput: `0.2460 rps`
- Failures: timed out (`4`)

### 3) Retrieval-heavy queries

- Requests: `2`
- Success: `0/2`
- p50: `40002.760 ms`
- p95: `40003.008 ms`
- p99: `40003.030 ms`
- Throughput: `0.0500 rps`
- Failures: timed out (`2`)

### 4) High-frequency state updates

- Requests: `30`
- Success: `30/30`
- p50: `449.122 ms`
- p95: `4892.891 ms`
- p99: `5096.665 ms`
- Throughput: `5.6101 rps`

## Aggregate

- Total requests: `59`
- Success: `53`
- Fail: `6`
- Success rate: `89.83%`

## Runtime Resource Snapshot (during baseline)

Representative docker stats (`docker stats --no-stream`) at run end:

- `letta`: CPU `69.86%`, memory `1.449GiB`
- `contextlattice-orchestrator`: CPU `3.81%`, memory `322.3MiB`
- `memorymcp-http`: CPU `0.19%`, memory `135.6MiB`
- `mindsdb-http-proxy`: CPU `0.25%`, memory `95.24MiB`
- `qdrant`: CPU `0.14%`, memory `569.3MiB`

## Top 5 Bottlenecks (Phase 0)

1. Slow-source timeout behavior (Letta, memory bank)
- Retrieval-heavy workloads hit timeout ceilings consistently.
- Live retrieval telemetry shows elevated Letta p95/p99 and timeout alerts.

2. High tail latency in core retrieval paths
- Even fast-path requests show large p95 tails (`8s+` in live telemetry for qdrant path).
- p95/p99 dominate user-perceived latency.

3. Monolithic orchestrator hot path
- `app.py` is large (`15k+` LOC) with broad responsibilities (routing, retrieval, scheduling, telemetry).
- Increases complexity and optimization friction.

4. Medium-context workload instability under concurrency
- Multi-agent context workload showed `33%` timeout/failure in baseline run.
- Indicates brittle behavior in parallel retrieval/context assembly.

5. Write-path tail latency spikes
- Write throughput is acceptable, but p95/p99 (`~4.9-5.1s`) indicates burst sensitivity under concurrency.

## Migration Interface Proposal

Phase 1 interface proposal is documented in:

- [docs/migration-interfaces.md](docs/migration-interfaces.md)

Feature flags proposed:

- `USE_RUST_CODEC`
- `USE_RUST_MEMORY`
- `USE_RUST_RETRIEVAL`
- `USE_GO_ORCHESTRATOR`

## Definition of Done Check (Phase 0)

- Python architecture inventory: complete
- Benchmark harness: complete
- Baseline metrics recorded: complete
- Top bottlenecks identified: complete
- Migration interfaces proposed: complete

## Next Step

Proceed to Phase 1: extract stable interfaces behind flags while preserving behavior and rollback safety.
