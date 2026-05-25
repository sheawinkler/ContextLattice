# OSS Runner + ANE Sidecar Implementation Plan

Status: Implemented for gateway-go runtime path (2026-03-22)
Scope: Execution plan + completion notes
Related issue: `#68` ([Backlog] OSS agent runners + ANE sidecar compute backend for Ollama)

## Implementation Status (2026-03-22)

Completed in repository:
1. Inference provider routing now lives in `gateway-go`:
   - `ORCH_INFER_PROVIDER=auto|vllm|vllm-metal|mlx|mtplx|openai-compatible|ollama|ane_sidecar`
   - hardware-aware provider priority with health probes
   - ANE sidecar health probe + retry + fallback path
   - legacy Python routing helpers archived under `archive/scripts/`
2. Task worker integration completed:
   - `scripts/task_agent_worker.py`
   - `scripts/agent_runners/generic_runner.py`
3. Canary OSS runner integration expanded:
   - Added runner command routing for `opencode`, `goose`, `eliza`
   - Added wrappers in `scripts/agent_runners/{opencode_runner.py,goose_runner.py,eliza_runner.py}`
4. Runtime/env wiring added:
   - `config/env/strict_runtime.env`
   - `.env.example`
5. Verification:
   - `services/gateway-go/inference_test.go`
   - synthetic route benchmark artifact:
     `bench/results/ane_sidecar_route_bench_20260322T2005Z.json`

Notes:
- The benchmark above is a synthetic transport-path benchmark using mock local endpoints; it validates routing overhead/fallback behavior, not true ANE model-compute gains.
- Real ANE throughput/quality benchmarking remains an operational follow-up when a live ANE sidecar is available on target hardware.

## 1) Objective
Implement a production-safe path to:

1. Integrate at least one open-source agent runner end-to-end with the orchestrator task loop.
2. Add an ANE-backed inference sidecar route for Ollama-facing workloads.
3. Preserve fallback safety and quality parity with existing providers.
4. Validate measurable performance gains before broader rollout.

## 2) Platform Constraint
ANE acceleration applies only to macOS on Apple Silicon (M-series).
Non-M-series systems remain on existing provider routes (Ollama/CPU/GPU) with no ANE dependency.

## 3) Non-goals

1. No simultaneous integration of multiple OSS runners in the first pass.
2. No immediate hard cutover away from current inference providers.
3. No removal of existing fallback paths.
4. No quality regression acceptance for faster execution.

## 4) Success Criteria

1. One OSS runner adapter ships behind feature flags and passes end-to-end claim/execute/report tests.
2. ANE sidecar route is selectable and auto-fallback works without operator intervention.
3. Benchmark report shows sidecar path exceeds predefined performance gates.
4. Rollback can be completed by config change only (no code rollback required).
5. Documentation and runbooks are complete for local and production use.

## 5) Current-State Summary

1. Orchestrator already supports task claiming/reporting APIs and sidecar health telemetry endpoints.
2. Retrieval/caching stack has staged fetch and adaptive tuning; inference provider routing exists but is not ANE-sidecar-first.
3. Docker and runtime orchestration already run hybrid components (Python + Rust + Go), enabling incremental integration.

## 6) Target Architecture

```text
Client / Agent Runner
        |
        v
Go/Python Orchestrator Task APIs
        |
        +--> OSS Runner Adapter (canary)
        |       |
        |       +--> runner-native execution loop
        |
        +--> Inference Provider Router
                |
                +--> ANE Sidecar Client (primary when enabled + eligible)
                |
                +--> Ollama Client (fallback/default)
```

## 7) Workstreams

## 7.1 Workstream A: OSS Runner Adapter (Canary First)

### A1. Runner Selection Gate

Evaluate candidates (`OpenCode`, `Goose`, `Eliza`) using a weighted matrix:

1. Task lifecycle compatibility (`claim -> execute -> heartbeat -> complete/fail`)
2. Tool-call semantics compatibility
3. Streaming support maturity
4. Retry/error surface predictability
5. Installation/runtime footprint
6. Maintainer activity and release stability

Deliverable: `docs/runner-canary-selection.md` with selected canary and rationale.

### A2. Adapter Contract

Define internal adapter interface:

1. `prepare(task) -> RunContext`
2. `execute(context) -> RunResult`
3. `heartbeat(task_id, state)`
4. `finalize(task_id, outcome)`
5. `map_error(exception) -> standardized error class`

Deliverable: interface spec + compatibility matrix for canary runner.

### A3. Orchestrator Integration

1. Add runner routing policy:
   - `ORCH_RUNNER_MODE=legacy|canary|auto`
   - `ORCH_RUNNER_CANARY=<runner_id>`
2. Add per-task runner override support.
3. Add bounded retries with backoff per adapter.
4. Add runner-specific dead-letter reason taxonomy.

### A4. Acceptance Tests

1. Claim/execute/report success path
2. Runner timeout path
3. Runner crash restart path
4. Tool call failure mapping path
5. Idempotent completion/report retries

## 7.2 Workstream B: ANE Sidecar Provider Route

### B1. Sidecar API Contract

Define stable HTTP endpoints (OpenAI-like where practical):

1. `GET /healthz`
2. `GET /v1/models`
3. `POST /v1/chat/completions`
4. `POST /v1/embeddings`
5. `GET /metrics` (Prometheus/text or JSON)

Required response metadata:

1. model id
2. tokens in/out
3. latency ms
4. backend (`ane`, `cpu`, `gpu`)
5. request id

### B2. Sidecar Client Module

Add `services/orchestrator/providers/ane_sidecar.py` with:

1. timeout and retry policy
2. auth header injection
3. request/response schema validation
4. graceful degradation on transient failure
5. circuit breaker (open/half-open/closed)

### B3. Routing Policy

New provider mode:

1. `ORCH_INFER_PROVIDER=auto|ollama|ane_sidecar`
2. `auto` logic:
   - if ANE enabled + host eligible + sidecar healthy -> ANE sidecar
   - else -> Ollama
3. fallback behavior:
   - on timeout/5xx/invalid payload, fallback to Ollama
   - emit telemetry event with root cause + fallback reason

### B4. Host Eligibility Detection (M-series only)

Eligibility checks:

1. `platform.system() == "Darwin"`
2. `platform.machine() == "arm64"`
3. optional confirmation via `sysctl` probe (Apple Silicon signal)

If not eligible:

1. sidecar route is disabled at runtime
2. log once with explicit reason
3. continue with standard providers

### B5. Feature Flags / Env

Proposed configuration:

1. `ORCH_ANE_SIDECAR_ENABLED=false`
2. `ORCH_ANE_SIDECAR_URL=http://127.0.0.1:9099`
3. `ORCH_ANE_SIDECAR_API_KEY=`
4. `ORCH_ANE_SIDECAR_TIMEOUT_SECS=20`
5. `ORCH_ANE_SIDECAR_CONNECT_TIMEOUT_SECS=2`
6. `ORCH_ANE_SIDECAR_RETRIES=1`
7. `ORCH_ANE_SIDECAR_RETRY_BACKOFF_SECS=0.25`
8. `ORCH_ANE_SIDECAR_CIRCUIT_ERRORS=5`
9. `ORCH_ANE_SIDECAR_CIRCUIT_COOLDOWN_SECS=30`
10. `ORCH_ANE_SIDECAR_FALLBACK_ENABLED=true`
11. `ORCH_ANE_SIDECAR_HEALTH_TTL_SECS=10`
12. `ORCH_ANE_SIDECAR_REQUIRE_M_SERIES=true`

## 7.3 Workstream C: Telemetry, SLOs, and Quality Gates

### C1. Telemetry Additions

Add provider-level telemetry dimensions:

1. provider selected (`ane_sidecar` vs `ollama`)
2. fallback count and fallback reason
3. provider timeout/error rates
4. p50/p95/p99 latency by provider and model
5. throughput by provider

Add endpoints/payload fields:

1. `/telemetry/sidecar-health` extended fields for inference stats
2. retrieval/eval dashboards include provider quality parity status

### C2. Quality Parity Gate

Before broad rollout, verify ANE route does not degrade quality:

1. exact-numeric-copy rate unchanged or improved
2. recall@k gate unchanged or improved
3. no increase in hallucination proxies from eval suite

### C3. Performance Gate ("Blazing Fast" readiness)

Minimum canary gate (must pass):

1. p50 latency: at least `1.6x` faster than Ollama baseline
2. p95 latency: at least `1.35x` faster
3. sustained throughput: at least `1.5x` at equal error budget
4. timeout rate: not worse than baseline + `0.5%`
5. quality parity gates pass

## 7.4 Workstream D: Reliability + Rollback

### D1. Safe Rollout

1. local developer opt-in
2. canary subset enablement
3. staged percentage rollout
4. promote only if all gates pass

### D2. Rollback

Rollback knobs (no code deploy required):

1. set `ORCH_ANE_SIDECAR_ENABLED=false`
2. set `ORCH_INFER_PROVIDER=ollama`
3. set `ORCH_RUNNER_MODE=legacy`

### D3. Failure Modes to Validate

1. sidecar unreachable
2. sidecar slow responses
3. sidecar malformed payloads
4. adapter runner crash loops
5. mixed failure under concurrency

## 8) Phase-by-Phase Plan

## Phase 0: RFC and Contracts
Duration: 2-3 days

1. Finalize runner canary choice.
2. Finalize sidecar API request/response schema.
3. Define benchmark methodology and acceptance thresholds.

Definition of done:

1. RFC approved.
2. API contract approved.
3. Test and benchmark plan approved.

## Phase 1: Adapter and Router Interfaces
Duration: 2-4 days

1. Introduce stable runner adapter interface.
2. Introduce provider router abstraction for inference path selection.
3. Add feature flags and config parsing.

Definition of done:

1. Existing path behavior unchanged with defaults.
2. New interfaces covered by unit tests.

## Phase 2: Canary OSS Runner Integration
Duration: 4-6 days

1. Implement canary adapter.
2. Wire task lifecycle + retries + failure mapping.
3. Add integration tests for runner path.

Definition of done:

1. End-to-end task execution works with canary runner.
2. Failure paths are deterministic and observable.

## Phase 3: ANE Sidecar Client + Fallback
Duration: 4-6 days

1. Implement sidecar client.
2. Add M-series eligibility detection.
3. Add fallback to Ollama on failure.
4. Add circuit breaker behavior.

Definition of done:

1. Sidecar route functions on M-series test machine.
2. Automatic fallback verified in tests.

## Phase 4: Telemetry + Bench Harness
Duration: 3-4 days

1. Extend telemetry payloads.
2. Add benchmark scenarios to existing harness.
3. Capture baseline vs sidecar reports.

Definition of done:

1. Before/after benchmark report published.
2. Quality parity report published.

## Phase 5: Canary Rollout + Expandability Readiness
Duration: 3-5 days

1. Enable canary in controlled environments.
2. Validate performance and quality gates.
3. Document expansion template for second/third OSS runners.

Definition of done:

1. Canary stable over soak window.
2. Rollback tested.
3. Decision recorded: promote, hold, or revert.

## 9) Detailed Test Plan

## 9.1 Unit Tests

1. sidecar client serialization/deserialization
2. auth and timeout behavior
3. retry and circuit breaker transitions
4. host eligibility detection
5. provider router decision matrix
6. runner adapter error mapping

## 9.2 Integration Tests

1. fake sidecar healthy path
2. fake sidecar timeout path with fallback
3. fake sidecar 5xx path with fallback
4. runner adapter claim/execute/report cycle
5. mixed provider under concurrent calls

## 9.3 End-to-End Tests

1. orchestrator with canary runner + ollama
2. orchestrator with canary runner + ANE sidecar
3. orchestrator with sidecar disabled
4. rollback scenarios via env changes only

## 9.4 Soak Tests

1. 2-4 hour sustained mixed workload
2. track queue depth, timeout rate, deadletters
3. verify no memory leaks or unstable retry storms

## 10) Benchmark Methodology

## 10.1 Workloads

1. short prompt, high qps
2. medium prompt, mixed tool usage
3. long prompt, retrieval-heavy
4. burst test with concurrency ramps

## 10.2 Metrics

1. p50/p95/p99 latency
2. throughput requests/sec
3. timeout/error rates
4. fallback frequency
5. cost proxy (tokens/sec and compute utilization)

## 10.3 Reporting

1. publish markdown report under `docs/` with raw tables
2. include environment details (machine, model, versions)
3. include go/no-go gate decisions

## 11) Security and Compliance

1. require API key auth for sidecar endpoint in non-dev mode
2. redact sensitive fields in logs
3. avoid storing raw prompts in telemetry unless explicitly enabled
4. enforce strict timeouts to prevent hung calls
5. include origin allowlist if sidecar exposed beyond localhost

## 12) Operational Runbook

## 12.1 Enablement

1. set `ORCH_ANE_SIDECAR_ENABLED=true`
2. set sidecar URL/API key
3. set provider mode `auto` or `ane_sidecar`
4. verify health endpoint and orchestrator telemetry

## 12.2 Troubleshooting

1. if sidecar unhealthy, verify fallback counters increase and traffic shifts to Ollama
2. if timeout spikes, reduce sidecar timeout and increase fallback aggressiveness
3. if quality drift occurs, disable sidecar route and run eval comparison

## 12.3 Emergency Disable

1. set `ORCH_ANE_SIDECAR_ENABLED=false`
2. restart orchestrator
3. verify all calls route to Ollama

## 13) Risks and Mitigations

1. Risk: Sidecar instability under load
   Mitigation: circuit breaker + bounded retries + fallback.
2. Risk: Quality regression from faster inference route
   Mitigation: quality parity gate as rollout blocker.
3. Risk: OSS runner API churn
   Mitigation: adapter boundary + pinned tested versions.
4. Risk: M-series specific behaviors diverge from cross-platform expectations
   Mitigation: strict eligibility gating and default cross-platform fallback.
5. Risk: Increased operational complexity
   Mitigation: minimal initial surface, one runner canary first.

## 14) Resourcing and Timeline

Estimated execution: 3-5 weeks for canary-complete delivery.

1. Week 1: Phase 0-1
2. Week 2: Phase 2
3. Week 3: Phase 3
4. Week 4: Phase 4
5. Week 5: Phase 5 + release decision

## 15) Deliverables Checklist

1. Sidecar API contract doc
2. Runner canary selection doc
3. Feature flags + env docs
4. Unit/integration/e2e tests
5. Benchmark and quality parity reports
6. Rollback and operations runbook
7. Release notes entry for rollout decision

## 16) Go/No-Go Gates

Go requires all of:

1. canary runner stable in soak testing
2. ANE path passes performance gate
3. quality parity gate passes
4. fallback and rollback validated
5. telemetry coverage complete

No-go if any gate fails; remain on existing stable provider path and iterate.
