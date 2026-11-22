# Manual Dream transfer

Source checkout only; not part of the current public release.

A manual Dream transfer tests whether a lesson learned in one project also
applies in a different project. It records a prediction before the destination
test and lets the destination owner record what happened. It does not run the
experiment, call a model, fetch evidence from the web, accept a recommendation,
or change the source project.

## Prepare the transfer

1. Choose one confirmed source lesson from an authorized, imported, signed
   Context Passport. Identify the exact passport revision and the experience
   within it. A topic, a mutable filename, or a recollection alone does not pin
   the source evidence.
2. Write the mechanism you expect to carry over, a material difference at the
   destination, a measurable prediction, and an observation that would falsify
   it. Record these before anyone checks the result in the second project.
   The API calls this step `freeze`: the prediction cannot be edited to fit
   what you learned afterward.
3. Have the destination owner decide whether to run the test and who may record
   its result. Use destination-scoped evidence, including a negative control
   when possible. Record whether the evidence confirms, narrows, rejects, or
   leaves the prediction unproven. The result applies to this destination; it
   does not revise the origin lesson or promote a global rule.

For example, a lesson from a small queue might be that batching reduces
duplicate work. A destination queue with strict latency targets is materially
different. The prediction could be fewer duplicates at the same throughput;
the falsifier could be a missed latency target compared with the same workload
without batching. The destination owner, not the source author or the CLI,
decides whether the test supports applying the lesson.

## Service prerequisites

Use a gateway that supports the manual transfer protocol, with durable
destination memory, the verified source passport and its authorized import
receipt, and a grant scoped to the source revision, destination and caller.
Start with the [pinned source export](dream-transfer-source.md). A source
reporter's confirmation is not an independent replication.
On Darwin or Linux, a local custodian authorized for both projects provisions
the grant offline with `contextlattice dream authorize-local`. The grant TTL
must be positive and no more than 15 minutes. See [offline provisioning](dream-transfer-authority.md)
for the exact flags, private worker-key file (`CONTEXTLATTICE_DREAM_WORKER_KEY_FILE`),
and grant/proof output requirements. The operator configures the gateway's
`CONTEXTLATTICE_DREAM_TRANSFER_AUTHORITY_FILE` to point to the grant file.
Neither transfer operation grants access, and there is no self-service HTTP
grant or revoke step.

Set `CONTEXTLATTICE_WORKER_API_KEY` to the separate worker credential and
`CONTEXTLATTICE_DREAM_TRANSFER_PROOF_FILE` to the existing local proof file
bound to the grant. A shared gateway key, a worker key alone, or a caller name
does not establish ownership. The proof file must be a regular, caller-owned
file with mode `0600` or `0400` and contain 64 lowercase hex digits, optionally
followed by a newline. The client requires POSIX file-permission checks for
this operation. It sends credentials to the configured gateway only, without
following redirects, and requires HTTPS outside loopback. Do not include
credentials or source document contents in the JSON payload.

## Submit the two operations

Run one explicit operation at a time with a local JSON object of at most 64 KiB:

```sh
contextlattice dream transfer --project destination --payload-file freeze.json
```

The freeze payload uses `action: "freeze"` and includes `grant_id`,
`source_passport_id`, `experience_ref`, `destination_project`, `mechanism`,
`mismatch`, `prediction`, `falsifier` and `expires_at` (RFC 3339). The
destination must match `--project`. A successful response reports
`status: "frozen"` and issues a `transfer_id`; the stored record awaits the
destination result.

After the destination owner makes and documents the decision, submit:

```sh
contextlattice dream transfer --project destination --payload-file result.json
```

Use `action: "record_result"`, the exact `transfer_id` from freeze, and
`result` set to `confirmed`, `narrowed`, `rejected` or `unproven`. Use
`evidence_refs` for up to eight destination memory IDs (for example,
`destination::evidence/control.json`). Only `unproven` may omit evidence.
For a supported result the destination event must have been written after
the prediction was frozen. An older file, a transfer record citing itself,
or a file with no provable event cannot serve as confirmation. This checks
recording order, not what a human may already have known.
Either file can optionally declare `"schema_id": "dream_experience_transfer.v1"`.

The CLI reports success only for a receipt that matches the destination,
operation, expected outcome, exact request digest and native write hash
format. An abstention, an unbound receipt, or an explicitly unpersisted
projection is not success. If the CLI cannot confirm the request, check the
destination through an authorized read before retrying: the gateway may have
committed the operation before the response was lost or rejected.

If the receipt says `ledger_recorded: true` and `projection_persisted: false`,
the decision was signed but its readable destination copy was not confirmed.
Keep the original request. With the same live grant, retry that exact request:
the gateway can restore its signed copy without accepting a second decision.
It refuses changed evidence, a revoked grant, or a different observation
already at that path. If the destination writer cannot verify its write lock,
leave the result pending; do not delete or replace the destination record.
