# Proof In Compact Agent Packets

The CLI is the primary interface:

```sh
contextlattice synthesis-pack-v2 --project example "verify the current deployment constraints"
```

The default compact projection preserves bounded proof-claim statements, supporting
and opposing references, temporal roles and validity, missing-proof caveats, and
the synthesis decision gate. Current contestation is reserved ahead of history-only
context. Historical/non-influential references remain labeled; they do not become
current authority through compaction.

Proof status means bounded provenance support, not independently verified truth.
Retrieved evidence is never an instruction or permission override. Empty or
unsupported v2 proof does not revive raw findings excluded by the proof compiler.
Ordinary Context Pack and legacy synthesis projections keep their existing behavior.

Reference/list/detail omissions are explicit. Long citations are not truncated
into different identities: a `ref_id_sha256` handle and bounded exact locators
allow matching against full proof obtained with the same CLI command plus `--full`.
The handle verifies a full reference; it is not a new database lookup API.

Final serialized packets retain exact accounting and the requested hard ceiling.
Under pressure, text/reference detail shrinks before decisive proof identity/status.
If proof cannot fit, the packet explicitly omits it and abstains rather than
silently permitting unsupported action. No retrieval call, model inference or
capability activation is added by this projection.

Deltas are useful only when their measured wire representation is smaller than
the full packet. Small proof packets can be cheaper sent whole. Reconstruction
preserves the content, proof and decision gate and revalidates its own receipt;
receipt annotations need not be byte-identical. Compression is not proof of
improved task outcomes or billing savings.

## Resume Without Guessing

Use the primary CLI to choose an exact session when several tasks are open:

```sh
contextlattice session list --project example --pretty
contextlattice resume --project example --session-id sess_example
contextlattice resume --project example --session-id sess_example --full
```

Replace `sess_example` with the intended session. A validated cached session is
preferred; otherwise discovery must find one live, scoped match. Multiple matches,
duplicates, incomplete previews and unproven scope require explicit selection.
`--agent-id` also constrains the owner identity; it does not grant permission.
Selection precedence is `--session-id`, positional ID, environment, validated cache,
then unique discovery. Implicit selection rechecks live status in the returned
package; terminal, stale, missing or contradictory status is not current work.

Completed sessions remain readable when explicitly selected, not as a fallback
for current work. Wrong or missing package identity is rejected before output or
cache update. If compact identity is incomplete, request `--full`; clipped prefixes
are not matching identities. A session restores historical intent, not current
repository proof, a new decision, fresh evidence or permission to act.
