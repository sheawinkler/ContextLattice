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
