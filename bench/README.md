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

Optional knobs:

- `--project`
- `--timeout`
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
