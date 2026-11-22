# Provision local Dream transfer authority

In source checkouts with the native command available, one OS custodian must
be authorized to consent for both projects. The command creates an expiring
grant and its separate proof offline on Darwin or Linux. It does not contact the
gateway, perform a transfer, or activate any background operation. Do not delegate
authority for a project you do not control.

Create the grant, proof, and key directories beforehand. Each must be owned by
the invoking OS user and have mode 0700. Put the configured worker-role
`X-API-Key` in a private regular 0600 file. Use a worker key distinct from the
orchestrator key; the gateway must independently verify the worker role.
On the gateway, append `dream_transfer` to the existing
`GO_TOOL_CALLS_WORKER_ALLOWLIST` without removing other entries. This is a
permission for the manual protocol, not a new tool or route. Do **not** add
general `dream` access or enable all worker tools for this purpose. The worker
role must be enabled with a distinct key; an explicit `dream` or
`dream_transfer` denial still wins.
`CONTEXTLATTICE_DREAM_WORKER_KEY_FILE` names the file, not the key. Do not place
the key or proof in command-line arguments, shell output, a repository, or
shared memory.

```sh
export CONTEXTLATTICE_DREAM_WORKER_KEY_FILE="$owner_key_dir/worker.key"
contextlattice dream authorize-local \
  --source "$source_project" --destination "$destination_project" \
  --passport "$source_passport_id" --experience "$experience_ref" \
  --source-digest "$source_record_hash" \
  --grant-file "$owner_grant_dir/transfer.json" \
  --proof-file "$owner_proof_dir/transfer.proof" --ttl 10m
```

Use absolute file paths, exact distinct project names, the exact source
passport and experience references, and the matching `sha256:` digest. A grant
can last up to 15 minutes. Neither output may exist already. Success prints
only the grant ID and expiration. The grant binds the scope to hashes of the
worker key and proof; the proof file is secret and is published before the
grant. A failed command is not proof of authorization. Do not use leftover
files from an interrupted run; inspect them locally and use fresh names for
another grant.

Point the gateway's `CONTEXTLATTICE_DREAM_TRANSFER_AUTHORITY_FILE` at the
absolute grant path **as seen by the gateway process**. For a container, use a
dedicated, read-only directory mount, not a bind mount of the individual file:
removal or replacement must be visible inside the container. The directory
and file must belong to the gateway's OS user and have modes 0700 and 0600.
Do not mount the worker key or proof into the gateway's grant directory.

Give the authorized caller the separate proof over a private channel. The
[native transfer guide](manual-dream-transfer.md) describes the two calls.
The grant is not a result receipt. Removing the grant from that directory
withdraws it; putting the same bytes in a new file does not restore it.
After expiry or withdrawal, even a previously frozen prediction cannot be
completed under that grant. This authorization is limited to the two named
local projects under one custodian.
