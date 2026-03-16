# Benchmark Harnesses

## Phase 0 Baseline

`bench/phase0_baseline.py` runs the baseline workloads described in the migration plan.

## Workloads

1. `single_agent_short_context`
2. `multi_agent_medium_context`
3. `retrieval_heavy_queries`
4. `high_frequency_state_updates`

## Usage

```bash
API_KEY=$(awk -F= '/^(CONTEXTLATTICE_ORCHESTRATOR_API_KEY|MEMMCP_ORCHESTRATOR_API_KEY)=/{print $2}' .env | tail -n1)
python3 bench/phase0_baseline.py --api-key "$API_KEY"
```

Optional knobs:

- `--timeout-secs`
- `--single-requests --single-concurrency`
- `--multi-requests --multi-concurrency`
- `--retrieval-requests --retrieval-concurrency`
- `--write-requests --write-concurrency`
- `--output`

Results are written to `bench/results/` as JSON.

All benchmark harnesses set `traffic_class=benchmark` on retrieval calls so benchmark traffic is partitioned from user-facing recall telemetry.

## Phase 1+ Runtime Comparison

`bench/phase1_runtime_comparison.py` records runtime adapter status plus retrieval latency for quick parity checks.

```bash
API_KEY=$(awk -F= '/^(CONTEXTLATTICE_ORCHESTRATOR_API_KEY|MEMMCP_ORCHESTRATOR_API_KEY)=/{print $2}' .env | tail -n1)
python3 bench/phase1_runtime_comparison.py --api-key "$API_KEY" --requests 20
```

Optional knobs:

- `--base-url`
- `--timeout`
- `--output`

## Shortlist Candidate Matrix

`bench/perf_shortlist_matrix.py` captures fast/balanced/deep retrieval metrics for the performance shortlist workstream.

```bash
API_KEY=$(awk -F= '/^(CONTEXTLATTICE_ORCHESTRATOR_API_KEY|MEMMCP_ORCHESTRATOR_API_KEY)=/{print $2}' .env | tail -n1)
python3 bench/perf_shortlist_matrix.py --api-key "$API_KEY" --runs 12
```

Benchmark-gated fastembed rollout (compare candidate vs baseline and emit gate artifact):

```bash
python3 bench/perf_shortlist_matrix.py \
  --api-key "$API_KEY" \
  --runs 12 \
  --gate-warmups 1 \
  --gate-repeats 3 \
  --gate-aggregate median \
  --baseline bench/results/perf_shortlist_matrix_baseline.json \
  --gate-output bench/results/fastembed_gate_latest.json
```

Optional knobs:

- `--project`
- `--timeout`
- `--baseline`
- `--gate-min-improvement-pct`
- `--gate-max-error-regression`
- `--gate-warmups`
- `--gate-repeats`
- `--gate-aggregate`
- `--gate-sleep-secs`
- `--gate-output`
- `--output`

## Qdrant Tuning Matrix

`bench/qdrant_tuning_matrix.py` records baseline/fast/deep profiles focused on Qdrant + rollups.

```bash
API_KEY=$(awk -F= '/^(CONTEXTLATTICE_ORCHESTRATOR_API_KEY|MEMMCP_ORCHESTRATOR_API_KEY)=/{print $2}' .env | tail -n1)
python3 bench/qdrant_tuning_matrix.py --api-key "$API_KEY" --runs 20
```

Optional knobs:

- `--project`
- `--timeout`
- `--output`

## Backend Lane Matrix

`bench/backend_lane_matrix.py` compares baseline `qdrant+topic_rollups` against:

- Rust backend lane request (`usearch_ann + tantivy_lexical`)
- memory-bank lexical spike requests (`meilisearch_spike`, `quickwit_spike`, `tantivy_spike`)

```bash
API_KEY=$(awk -F= '/^(CONTEXTLATTICE_ORCHESTRATOR_API_KEY|MEMMCP_ORCHESTRATOR_API_KEY)=/{print $2}' .env | tail -n1)
python3 bench/backend_lane_matrix.py --api-key "$API_KEY" --runs 3
```

Optional knobs:

- `--base-url`
- `--project`
- `--timeout`
- `--profiles`
- `--cases`
- `--cache-bust` / `--no-cache-bust`
- `--output`

## Direct Spike Backend Matrix

`bench/memory_bank_spike_direct_matrix.py` benchmarks Rust spike backends directly against the sidecar HTTP service (`/search`) so lexical/index performance is measured without orchestrator fanout.

```bash
python3 bench/memory_bank_spike_direct_matrix.py \
  --base-url http://127.0.0.1:8096 \
  --project contextlattice \
  --runs 5
```

Optional knobs:

- `--backends`
- `--cases`
- `--warmups`
- `--cache-bust` / `--no-cache-bust`
- `--timeout`
- `--output`
