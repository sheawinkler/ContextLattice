# Decision Evidence Continuity

Remember the decision. Recheck its reasons.

A checkpoint can preserve yesterday's intent after its evidence has changed.
Manual decision continuity binds a session to one exact decision record and
rechecks that record's original owner versions when the session resumes.
It does not choose a new decision, refresh old pins, evaluate an assumption's
meaning, or authorize execution.

## CLI Workflow

The CLI is primary. HTTP exposes the same contracts for application adapters.
This is an optional manual workflow; no scheduler or paid automation is enabled.

1. Resolve the project's task identity with `contextlattice continuity-reconcile`.
2. Record a decision using `contextlattice decision-change --payload-file decision.json`.
3. Start or acknowledge the exact selected record, using the returned IDs.
4. Resume with `contextlattice resume --session-id <id> --project <project>`.
5. If evidence changed or is unavailable, verify or recompute before acting.

### Pin The Evidence Used

`decision.json` contains the existing decision-change fields plus this optional
object. IDs below illustrate the shape; use actual task and memory-owner receipts.

```json
{
  "schema_id": "decision_applicability.v1",
  "scope": {
    "project": "example",
    "objective_id": "obj_release",
    "task_identity_id": "task_release",
    "repo": "example-repo",
    "branch": "main"
  },
  "inputs": [{
    "memory_id": "example::notes/acceptance.md",
    "target_event_id": "evt_original",
    "target_content_hash": "0000000000000000000000000000000000000000000000000000000000000000"
  }],
  "assumptions": [{
    "assumption_id": "acceptance",
    "input_id": "example::notes/acceptance.md",
    "role": "acceptance_criteria"
  }]
}
```

Place that object under the top-level `applicability` key. Supply the real
original event/hash, not the illustrative values above or a newer version fetched
to make an old result pass. The decision's project, objective, and task must match
its applicability scope. Inputs are limited to 1-8 distinct same-project owners;
assumptions are an explicit array of 0-4 entries. `source_condition` is the other
supported assumption role. Typed objects are bounded, exact-field, non-null
contracts. Omitting applicability preserves legacy registration; malformed
presence does not become omission.

Equivalent hash prefix, case, and surrounding whitespace are canonicalized
without selecting a new owner event or changing the evidence's identity.

An acceptance owner proves only which criteria document was observed. Matching
its version does not prove the criteria are satisfied. No acceptance owner means
`acceptance_criteria: not_provided`, never inferred approval.

### Bind A Session

Session start/ensure accepts `--payload-file`. Put the normal session fields
and this top-level `decision_binding` object in that file:

```json
{
  "schema_id": "session_decision_binding.v1",
  "project": "example",
  "objective_id": "obj_release",
  "task_identity_id": "task_release",
  "decision_change_id": "dc_release",
  "after_decision_id": "decision_release",
  "repo": "example-repo",
  "branch": "main"
}
```

```sh
contextlattice_agent_session start --payload-file session.json --pretty
contextlattice_agent_session ensure --payload-file session.json --pretty
```

Use the selected decision-change and after-decision IDs, not a search hit or the
newest record. `record_digest` is server-owned and must not be supplied. Explicit
session flags override the corresponding file fields; defaults do not overwrite
file scope. Equal-binding ensure is idempotent. Omission preserves a binding;
changing a binding requires acknowledgement. Known task merges preserve both
historical IDs without changing the original record or silently replacing a row.

Accepted outer API aliases, such as `project_name`, `sessionId`, and
`taskIdentityId`, are normalized before defaults. Conflicting aliases reject
before writing. This does not permit aliases inside typed contracts.

### Acknowledge A Different Decision

Read the current session's `decision_baseline`:

```sh
contextlattice_agent_session status --session-id <id> --pretty
```

Submit an event file with `type: decision.bound`, a unique `event_id`, the exact
`session_id`, `project`, `agent_id`, and `decision_binding`. Add:

```json
{
  "schema_id": "session_decision_ack.v1",
  "sequence": 2,
  "expected_baseline": {
    "schema_id": "session_decision_baseline.v1",
    "incarnation": "sdi_from_current_session",
    "revision": 1,
    "binding_digest": "0000000000000000000000000000000000000000000000000000000000000000"
  }
}
```

Put this object under `decision_ack`; copy the actual complete baseline, and set
sequence to its revision plus one. New unbound sessions begin at revision zero.

```sh
contextlattice_agent_session event --payload-file acknowledgement.json --pretty
```

The baseline is compare-and-swap protection, not evidence currentness. Exact
historical retries return their original event without appending or reading new
owner evidence. The durable retry window retains 16 acknowledgements; older
requests, conflicting sequences, stale incarnations, and recreated rows reject.
Inspect state rather than inventing a new baseline after an uncertain write.
Acknowledgements do not manufacture utility, outcome, or success receipts.

## Resume States

Full output exposes `decision_continuity`; compact output carries the same result
in `provenance.decision_continuity` inside the packet's model-visible identity.

| State | Meaning |
| --- | --- |
| `verified_match` | Original owner versions and selected record match at the final validation boundary. Not semantic truth or execution permission. |
| `changed` | An available authoritative owner no longer matches an original event/hash pin. |
| `unknown` | Required custody, owner, record, or final session state cannot be confirmed. |
| `not_applicable` | Explicit scope or recorded/effective time does not apply. |
| `legacy_unpinned` | No explicit pinned decision is available; history is not a current instruction. |

Negative views retain bounded, explicitly historical intent but replace executable
next actions with verify/recompute guidance. Unchecked event text and nested action
aliases are omitted rather than repackaged as current instructions. Unrelated
owner writes do not invalidate unchanged original pins. Final owner/session drift
cannot turn a cached positive package into current proof; ownership drift rejects
the package. Compact transport limits are not enlarged to fit this marker.

```sh
contextlattice resume --session-id <id> --project <project> --pretty
contextlattice resume --session-id <id> --project <project> --full --pretty
contextlattice_agent_session context-package --session-id <id> --view compact --pretty
```

## Compatibility And Safety

Typed CLI writes first require `decisionContinuity` protocol version 1 from
`/ops/capabilities`, then a matching returned receipt. Older gateways cannot
silently discard the new fields. Runtime readiness is separate from protocol
support so an exact historical retry can work during an owner outage. Missing
post-write confirmation reports an uncertain result, not success or permission to
blindly retry. Ordinary untyped commands do not add this capability round trip.

There is no automatic latest-decision selection, pin repair, memory writeback,
repository mutation, confidence promotion, or paid feature activation. Agent
lifecycle remains separate from retrieval lifecycle. These safeguards establish
contract behavior; they do not establish causal task-success or token-savings lift.

## Reproduction

Focused Go tests cover original-pin correction, legal merges, durable replay,
concurrent acknowledgements, snapshot failures, both resume views, and actual
session mutation while final owner verification waits on a real file fence.
Build the native agent-tools binary once, then set
`CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY` to its absolute path when running
`TestAgentDecisionNativeCLIEndToEnd`. That test invokes the binary against isolated
real Go decision/session handlers; it is not an installed-operator or model-efficacy
test. Without the explicit binary, this optional native selector reports a skip.
