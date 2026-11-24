# Read a correction receipt

Use `contextlattice correct` when feedback changes what an agent should remember.
It is not an approval, a deployment receipt, or an independent verification.

```sh
contextlattice correct "The migration note refers to a retired table." \
  --project example --category stale --pretty
```

This records feedback without changing a factual claim. Use `--factual` only
when you intend to record a structured assertion. Supply a subject, predicate
and object; identify the claim being corrected for a wrong, stale or superseded
assertion. A selected session must belong to the caller and project before the
CLI sends feedback. Local argument or session-selection errors can stop the
command before any receipt exists.

| Status | What the receipt establishes |
| --- | --- |
| `feedback_only` | Feedback was saved. No factual change was requested. |
| `factual_persisted` | Feedback and the requested claim were confirmed by their storage owners. This does not make the claim independently verified. |
| `partial_failure` | Feedback was saved, but a later requested step was not confirmed. Read the individual fields and findings. |
| `failed` | The command could not confirm that feedback was saved. This is not proof that a write never reached storage. |

Check `feedback_saved`, `claim_persisted` and `event_recorded` separately. A
sessionless correction does not need a session event. A factual correction is
stored as an unverified reporter assertion. A `wrong` assertion can contradict
its same-project target; a `stale` or `superseded` label alone does not retire it.

For an interrupted request, retry the same input with the same caller, project
and session. Do not change the note to get around an uncertain receipt or
count a retry as new evidence.

The feedback ledger owns its records. Its SQLite index locates the original
bytes; it is not a substitute for them. Back up each owning ledger or session
snapshot and its corresponding consistent SQLite sidecar together, including
any active journal. After conversion, do not downgrade in place: keep the newer
gateway, or pause writes and restore a matched pre-conversion backup. If the
files do not agree after a restore, preserve them, keep typed writes paused and
reconcile the pair. Do not delete an index to make an error go away.

Older feedback is still readable but is not upgraded into correction receipts.
Typed feedback history without its valid offset index fails closed.

## Manual named decisions

Use a named decision when you want an explicit instruction, not reporter
feedback. Present the configured gateway key through the normal CLI credential
mechanism. Use a direct loopback gateway and explicitly select an active
session that belongs to the stated actor and project. A session ID alone is
not authority. This path is unavailable on an unkeyed gateway.

```sh
# SESSION_ID identifies an existing active session for reviewer.
approval() {
  command_name=$1
  shift
  contextlattice "$command_name" --project example --topic-path release/review \
    --agent-id reviewer --session-id "$SESSION_ID" --decision-id approval "$@"
}
approval correct --effect set --expect-version 0 --object "Ask first"
approval correct --effect exception --expect-version 1 --target-version 1 --scope preview --object "Publish the preview"
approval claim-query --scope preview
approval correct --effect retract --expect-version 2 --target-version 2 --scope preview
approval claim-query --scope preview
```

The first read selects version 2. The second selects version 1, **Ask first**,
with history at version 3. A fresh active session for the same actor and
project can read the same decision. Actor, project, topic, decision ID and
scope labels match exactly; `preview/mobile` is not `preview`.

| Effect | Selection rule |
| --- | --- |
| `set`, `replace` | Establish or permanently replace the default. |
| `exception`, `refine` | Substitute a value only in one nonempty exact scope. |
| `detail` | Add information without replacing the instruction. |
| `contradict` | Withhold the current instruction and details while the opposition is unresolved. |
| `retract`, `expire` | End the exact referenced version. Ending an exception reveals the default; ending a default does not revive an older one. |

`--expect-version` is the latest receipt version; `--target-version` is the
earlier version being changed. Identical retries replay the same receipt.
Stale edits and overflow (32 versions per decision) fail without publishing a
new version. RFC3339 `--valid-from` is inclusive and `--valid-to` exclusive;
`claim-query --as-of` reads that effective time. Ordinary claims cannot be
adopted as owned decisions: use an explicitly confirmed `set` instead.

The manual read and write use `decision_context` on `/memory/claims/query`
and `decision_change` on `/memory/claims`. They accept neither tool-role
credentials nor caller-supplied ownership records. No feedback, session event,
search ranking, automatic recall, or resume action is created. Back up the
claim owner and receipt index together.
