# Export a Dream source observation

Source checkout only; not part of the current public release.

This opt-in passport export carries one confirmed lesson from an exact native
memory revision. It does not search, synthesize, use a model, access another
project, or change the source memory. The source gateway signs and stores the
passport; exporting it does not grant a destination permission to import or
apply the lesson.

## Write and pin the observation

Record only a lesson already confirmed in the source project. Create
`observation.json` as a JSON object (4 KiB maximum) with exactly these fields:

```json
{
  "schema_id": "dream_source_observation.v1",
  "experience_ref": "exp:cache-key",
  "status": "confirmed",
  "mechanism": "cache by stable key",
  "observed_result": "The controlled source test reduced duplicate fetches."
}
```

```sh
contextlattice write --project source --topic-path observations \
  --file observations/cache-key.json --content-file observation.json --raw

```

Use `event_id` and `content_hash` from the successful native write receipt.
The memory ID in this example is `source::observations/cache-key.json`.
For an existing observation, use an authorized exact read to get the current
revision instead. The hash is 64 lowercase hex digits without the `sha256:`
prefix. Never guess a missing event ID. At export, the gateway independently
checks that this is still the exact revision.

## Provision the source grant offline

An operator who controls the source project configures the gateway's
`CONTEXTLATTICE_DREAM_SOURCE_EXPORT_FILE` with the absolute, canonical path of
a service-owner-owned regular file (mode `0600`, in a service-owner-owned
directory of mode `0700`). The file must have exactly these JSON fields:

```json
{
  "schema_id": "dream_source_export_authority.v1",
  "project": "source",
  "memory_id": "source::observations/cache-key.json",
  "event_id": "<exact event_id from the write receipt or original>",
  "content_hash": "<exact 64 lowercase hex digits from the write receipt or original>",
  "expires_at": "<RFC 3339 timestamp no more than 15 minutes in the future>"
}
```

Replace the placeholders with the actual values. Removing the grant file
withdraws the ability to make a new export. This local grant only authorizes
the pinned source revision. It is not a destination grant or a replacement
for explicit service authentication.

## Export the signed passport

Create `source-export.json` as follows, again using the actual event and hash:

```json
{
  "project": "source",
  "transfer_observation": {
    "memory_id": "source::observations/cache-key.json",
    "event_id": "<exact event_id from the write receipt or original>",
    "content_hash": "<exact 64 lowercase hex digits from the write receipt or original>"
  }
}
```

```sh
contextlattice passport-export --payload-file source-export.json \
  --output source-passport.json --raw
```

Use the configured gateway with native durable memory and passport storage
enabled, and supply its matching nonempty `CONTEXTLATTICE_ORCHESTRATOR_API_KEY`
in the CLI environment. Keep the key out of payloads and receipts. Do not add a
query or extra fields: this path accepts only the project and revision
pointer. The gateway obtains the lesson and its confirmed status from the
stored JSON, not from the export request.

The `transfer_source` receipt reports `source_passport_id`,
`experience_ref`, and `source_record_hash` for the exact portable claim
inside the signed passport. This last digest includes the `sha256:` prefix
and is not the memory content hash. Use the signed mechanism, which can
reflect portable redaction. Continue with the separately authorized
[manual transfer](manual-dream-transfer.md); an export receipt is not a
destination decision.
