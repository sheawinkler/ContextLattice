# Check the original before acting

When two current claims disagree, the compact CLI packet can name the exact pair and suggest a distinguishing check. The packet is not a permission grant. You can continue unaffected work, inspect an original yourself, or ask its owner. Nothing runs in the background.

For one unattended check on a qualified POSIX host, the operator may supply an expiring JSON grant in an owner-only directory (0700), in a regular owner-only file (0600). Set `CONTEXTLATTICE_CONFLICT_GRANT_FILE` to its absolute path and `CONTEXTLATTICE_ATTEMPT_ID` to the current attempt. Do not put the grant in the packet, the repository, or shared memory. Without a grant the ordinary CLI behaves as before. Windows retains the manual check; unattended grants are not enabled there.

Example shape, not a usable grant: substitute the live packet reference, exact actor/session/task/attempt, gateway origin, timestamps, both claim IDs and the two expected values. Issue for at most 24 hours. A grant is for one named original, not for a directory or search.

```json
{
  "schema_id": "conflict_source_grant.v1",
  "binding": {
    "gateway": "http://127.0.0.1:8075", "project": "example", "topic_path": "review",
    "session_id": "<session>", "task_id": "<task>", "attempt_id": "<attempt>", "agent_id": "<actor>"
  },
  "case_ref": "<case_ref>",
  "issued_at": "<RFC3339>", "expires_at": "<RFC3339>",
  "source": {"kind":"memory_json","memory_id":"example::review/acceptance.json","field":"stage"},
  "values_by_claim": {"<first-claim>":"submitted","<second-claim>":"accepted"}
}
```

An exact native memory original must contain the named string field and RFC3339 `effective_at`. Add `object_id` to limit the grant to one immutable revision; otherwise the authorized file's current revision is checked. A legacy or fallback read with no proven native identity is insufficient. For a GitHub PR, use `{"kind":"github_pr","repo":"owner/repo","pr":42,"head":"<40-hex-head-sha>"}` instead. Expected values are `draft`, `open`, `closed`, or `merged`. Private reads require a separately configured read credential, `CONTEXTLATTICE_GITHUB_TOKEN`. Never put credentials in the JSON. A changed head is not evidence about the approved revision.

Use normal `contextlattice context` with the matching project, topic, task and session. The signed packet stays on stdout. If eligible, a small advisory observation goes to stderr and its event is bound to that session. Or ask Policy Lab to review an already obtained compact packet:

```sh
contextlattice policy-lab contradiction --operation research --project example --payload-file packet.json
```

This explicit operation requires the matching `CONTEXTLATTICE_AGENT_ID` and attempt. It does not call the claim-decision endpoint. Only a confirmed observation can identify a supported claim, at the reported observation time; it never grants permission to act or certifies independent acceptance. An optional `as_known_at` cutoff does not let evidence first seen today establish what the agent knew yesterday. A missing original, unrecognized value, revoked grant, changed claim or lost event acknowledgement stays unresolved. Removing the grant stops further checks. An already checked revision returns its existing session event ID when scope still matches, not a newly endorsed recommendation. After an unavailable read or uncertain handoff, a grant allows at most one further attempt after five minutes. A new question or changed case needs a new grant.
