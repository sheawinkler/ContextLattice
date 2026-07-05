# Runner Quality Loop

ContextLattice records compact runner-quality samples when repo-local runner adapters complete task-agent work.

This is decision support, not an automatic scheduler. The loop answers:

- Which runner succeeded, blocked, or failed?
- How long did it take?
- Which context-pack quality sample shaped the run?
- How many exact prompt tokens were saved by the context pack, when available?
- What modeled inference-avoidance signal was attached to the context pack?
- Did the task payload include explicit runner-quality feedback?

## What Gets Stored

Rows use `runner_quality_sample.v1` and are bounded/redacted NDJSON. They intentionally exclude raw prompts, raw completions, full stdout/stderr, and secrets.

Each row includes:

- runner identity: `runner`, `agent`, `agent_id`, `task_id`, `project`
- outcome: `status`, `ok`, `exit_code`, `duration_secs`
- context evidence: `context_pack_quality.sample_id`, quality score, exact prompt-token savings, modeled inference tokens avoided
- observed counters: provider token counters when supplied by the runner metadata
- feedback: optional task payload fields such as `runner_quality_rating` and `runner_feedback`
- privacy-safe hashes for result summary and stdout/stderr tails

## Ledger Location

Override the ledger path when needed:

```bash
CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_PATH=/path/to/runner_quality_ledger.ndjson
```

If unset, the ledger resolves from the ContextLattice data root when configured, then falls back to the local `.data/orchestrator/runner_quality_ledger.ndjson` path.

Bound the ledger:

```bash
CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_MAX_BYTES=2097152
CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_MAX_SAMPLES=1000
```

## Inspect Runner Quality

Primary CLI:

```bash
contextlattice_runner_quality --pretty
contextlattice_runner_quality --task-class scout --pretty
```

Development fallback from a repo checkout:

```bash
scripts/agent/runner-quality --pretty
```

The summary reports per-runner sample counts, success/block/failure rates, average duration, average context quality score, exact prompt tokens saved, modeled inference tokens avoided, task-class slices, and advisor-only recommendations. The dashboard and doctor surface the same signal as visibility, not control.

## Interpretation

Use this as operator advice, never automatic dispatch. ContextLattice may tell you which runner looks strongest for similar observed task classes, but it does not dispatch, mutate, merge, or push from this telemetry.

Low sample counts are weak evidence. A blocked Droid run caused by missing auth should improve the Droid readiness workflow, not prove Droid is a bad runner. A high prompt-token-savings number reflects context-pack compression, not guaranteed dollar savings unless provider usage counters are also present.

## Adapter Boundary

The runner-quality loop does not move subprocess execution into `gateway-go`, does not add Pi/Droid services, and does not create runner-specific gateway routes. Adapters execute runners; ContextLattice records bounded evidence about what happened.
