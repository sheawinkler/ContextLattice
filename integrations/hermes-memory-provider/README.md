# Hermes memory provider for ContextLattice

A native Hermes `MemoryProvider` plugin that replaces shell-hook `curl` retrieval
with the Hermes memory lifecycle.

## What it does

| Hook | Behavior |
|---|---|
| `prefetch` | Two parallel scoped `/memory/search` calls (project archive + cross-project), injected into the user message so the cached system prompt is untouched |
| `on_pre_compress` | Writes a handoff checkpoint to `runbooks/context-compaction-handoff` before the lossy summary rewrite |
| `on_session_switch` | Next prefetch searches the exact native parent's handoff file in the configured project; reset cancels continuity |
| `on_memory_write` | Mirrors built-in memory changes (primary agent only); replace/remove retires the prior file identity using the native `/memory/write` lifecycle field. Replace needs `metadata.previous_content`; otherwise retirement is explicitly blocked. Failed writes are queued in the bounded local outbox. |
| `pre_tool_call` | `delegate_task` children run with `skip_memory=True`; requests one project-scoped `/memory/context-pack` per task and verifies the returned project before injection; falls back only to project-scoped `/memory/search` |
| `post_tool_call` | Records a bounded session observation: host status, sample/task identity, exact rendered/context digests, and whether post-hook arguments still match. Does not infer first-pass success, retries, consumption, or independently verified utility. |
| `/cl-reconcile [--dry-run]` | Diffs every `MEMORY.md` / `USER.md` entry against its project-scoped mirror file and backfills gaps; dry-run performs no writes and does not drain the outbox |
| tools | `cl_search` (drains `continuation_async` within `drain_seconds`, merges late results, reports failed sources), `cl_write` (readback is verified only when a valid 64-hex SHA-256 digest matches the result and explicit project/file identity) |

Failure mode: if the orchestrator is unreachable, recall is an explicit
`CONTEXTLATTICE DEGRADED` line, never a silent empty result. When fast sources
return hits while slower sources are still running, the hits are labeled complete;
only an empty result with a pending continuation is labeled WARMING. Built-in
`MEMORY.md` / `USER.md` stay local and always load, so core rules survive an outage.

Continuation uses the normal authoritative `/memory/search` with the parent file
identity as its query and the handoff topic/project scope. There is no raw-file
GET fallback or lifecycle/quarantine bypass. A safe, explicit native parent ID
is required; missing or unsafe parents are reported unavailable, never replaced
by the receiver prompt or the previous session. Hits need exact project/file
identity, consistent aliases, a valid SHA-256 content identity, and no echoed
retirement, supersession, or quarantine marker. Supplied structured `payload`
project/file aliases and `content_hash` must agree with the required top-level
identity; they cannot fill missing top-level fields. The canonical
`memory_trust_assessment.quarantine.quarantined: true` flag also vetoes a hit.
Only these named carriers are inspected, not arbitrary nested data or words
in checkpoint text. Duplicate revisions with the same
hash are accepted once; competing hashes are ambiguous, not ranked by score.
Empty, warming, degraded, mismatched, and ambiguous results keep continuity
pending and emit a compact unavailable marker for a future prefetch retry.

Pre-compression snapshots the source session/project before processing messages
or writing. An acknowledged valid write hash pins a matching continuation in
this provider lifetime; a mismatch cannot silently downgrade to unpinned recall.
Without that receipt, accepted search hits remain explicitly unpinned. Reset
discards both pending continuity and the receipt. Generation, project, and
receiver-session guards prevent late or concurrent prefetches from consuming
or rendering a successor's handoff, including A-to-B-to-A switches. Consumption
means one bounded excerpt was returned by prefetch, not host dispatch or model
consumption. Checkpoints remain bounded verbatim excerpts, not inferred goals,
decisions, successful outcomes, or independently verified claims. Ordinary
archive/cross-project recall and the static system prompt are unchanged.

The mirror outbox uses a process lock and atomic same-directory replacement. It
is capped at 256 entries and 2 MiB; an overflow or corrupt queue is reported as
blocked rather than silently dropping entries. Drain batches are capped at 32.
Lifecycle updates use only the existing `/memory/write` metadata contract. If
the endpoint does not echo the requested `retired` or `superseded` lifecycle,
the item remains queued and is reported blocked; no separate lifecycle route is
assumed. Context-pack responses without the configured project identity and
fallback search hits without matching project identity are rejected.

Invocation IDs are unique per delegation call. On Hermes versions exposing both
`session_id` and `tool_call_id` to both hooks, observations join on that pair and
the configured project. Concurrent identical goals can complete in any order.
Older hosts without those identifiers retain conservative goal-based matching:
overlapping identical goals abstain rather than risk FIFO misattribution.
Duplicate IDs, changed goals, malformed/duplicate result indices, missing
results, and context changed by later middleware cannot establish successful
delivery or task success. Pending state is capped at 128 calls for one hour;
delegation recall is limited to eight tasks per call. Expired/missing
observations stay unknown, not failed or successful trials.

The plugin ensures a project/native-session-bound gateway session before a
delegation, verifies the returned identity, and binds context-pack requests to
that session and `hermes_agent`. Captured bindings survive native session
switches for in-flight callbacks. Cached bindings are revalidated after one
minute and invalidated on an event-post failure, for a future call; failed
observations are not rebound to a different session. `context.delivery.observed` session events
contain digests and counts, not raw prompts, goals, or tool results. These are
actor-side observations, not independent custody or proof that a model used
the context. Hermes execution middleware may replace arguments while post
hooks echo the earlier arguments. Consequently `post_hook_context_match` means
only an echo match: `dispatch_observed` and `delivery_confirmed` stay false,
and attributable `host_status` stays `unknown`. `host_reported_status` preserves
the literal known status reported by the hook without crediting that status to
the original context-pack task. A missing session binding or failed event acknowledgement emits
an explicit unrecorded-observation warning; recall still works. Observation
events are best-effort, not a durable delivery outbox or an evaluation ledger.

Hermes `completed` can mean a usable summary despite budget exhaustion or a
failed schema check. The plugin therefore no longer converts that status into
`first_pass_success=true`, `repair_required=false`, or `retry_count=0` quality
claims. Independent evaluation must supply those facts; historical inferred
claims are not retroactively repaired by this change.
Write readback accepts an optional `sha256:` prefix and normalizes case, but
requires exactly 64 hexadecimal digest characters and explicit matching
project and file fields (including supported camelCase/snake_case aliases).
Missing write confirmation (`ok: true`) is not treated as a successful write.

## Install

Select the intended Hermes profile and review its current memory provider before
changing it. These commands opt that profile into ContextLattice; do not replace
another provider without an explicit operator choice.

```zsh
: "${HERMES_HOME:?Set HERMES_HOME to the intended Hermes profile first}"
mkdir -p "$HERMES_HOME/plugins/contextlattice"
cp __init__.py plugin.yaml "$HERMES_HOME/plugins/contextlattice/"
hermes config set memory.provider contextlattice
```

When available, Hermes `agent.secret_scope.get_secret` is authoritative for the
profile. A scoped miss or resolver error fails closed; process environment and
env-file values are not used to fill a missing profile secret. Only Hermes
versions without that resolver use the legacy fallback order:
`CONTEXTLATTICE_ORCHESTRATOR_API_KEY` env, then
`memory.contextlattice.env_file` (or `CONTEXTLATTICE_ENV_FILE`). That legacy
fallback is process-wide and must not be used for per-profile credentials in a
multiplexed gateway.

Optional `config.yaml`:

```yaml
memory:
  provider: contextlattice
  contextlattice:
    url: http://127.0.0.1:8075
    project: hermes-agent
    mode: fast
    limit: 5
    timeout: 6.0
    delegation_recall: true
    delegation_tokens: 1200     # target_context_pack_tokens per child
    delegation_limit: 6
    delegation_max_chars: 6000
    drain_seconds: 8
    reconcile_hours: 24
```

If a shell `pre_llm_call` hook also seeds recall, gate it off when this provider
is active to avoid double injection.

Rollback: `hermes config set memory.provider <previous>`.

## Evidence Limits

The unit tests use a local fake HTTP server and verify plugin protocol behavior
only. They do not verify a live Hermes installation, live gateway behavior,
retrieval quality, context consumption, downstream actions, independent
verification, or efficacy. No efficacy claim is made from the test count or
from successful write/readback alone.

## Tests

```zsh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s integrations/hermes-memory-provider/tests
```

Stubs `agent.memory_provider` and fakes the orchestrator over local HTTP; no Hermes install needed.

For an installed Hermes version, use its already-installed Python environment:

```zsh
python -I -B integrations/hermes-memory-provider/tests/replay_installed_hooks.py \
  --harness-root /path/to/hermes-agent --receipt hook-replay.json
```

This zero-model probe imports the installed memory-provider and pre/post-hook
APIs under temporary Hermes state, blocks socket connections, disables tool
discovery and substitutes HTTP/lifecycle routing. It exercises argument merges,
identity propagation, reverse same-goal completions and exact echo digests.
Its source-hashed receipt does not establish actual agent execution, middleware
dispatch fidelity, authentication, model consumption or retrieval efficacy.
It never repairs or installs a harness or changes an operator profile.
