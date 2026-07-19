# Context Passport and Encrypted Context Mesh

Context should survive the machine that assembled it without becoming a blind
archive, an unsigned prompt dump, or a new sync service.

Context Passport turns a bounded slice of ContextLattice cognition into a
portable object with proof attached. Context Mesh seals that passport for
explicit recipients. The sender chooses how to move the resulting JSON file.
ContextLattice never becomes the transport.

## The Contract

A `context_passport.v1` artifact contains:

- the project and topic scope;
- the objective and replay request;
- proof-carrying claims and bounded evidence references;
- lineage, parent digest, and revision;
- required runtime capabilities;
- creation and expiry times;
- a content digest and Ed25519 signature;
- a signed report of redactions applied before export.

The signature proves that the manifest has not changed and that revisions share
the same signing identity. It does not turn an unknown key into a trusted human
or organization. Pin or verify signing keys through an independent channel when
identity trust matters.

Imported content remains evidence. Replay returns a validated retrieval request;
it does not execute text from the passport, elevate it into policy, or write it
into ordinary memory.

## CLI First

Export a passport from Proof-Carrying Synthesis v2:

```bash
contextlattice_passport_export \
  "prepare the release handoff" \
  --project contextlattice \
  --ttl-secs 604800 \
  --output passport.json \
  --pretty
```

When `--output` is present, the CLI writes the artifact with owner-only file
permissions and emits a compact receipt instead of repeating the full context
in the agent transcript.

Verify, inspect, and replay without execution:

```bash
contextlattice_passport_verify --file passport.json --pretty
contextlattice_passport_replay --file passport.json --pretty
contextlattice_passport_status --pretty
```

Create a revision from a stored parent:

```bash
contextlattice_passport_export \
  "prepare the revised release handoff" \
  --project contextlattice \
  --parent-passport-id <passport-id> \
  --output passport-v2.json \
  --pretty

contextlattice_passport_diff \
  --base-file passport.json \
  --target-file passport-v2.json \
  --pretty
```

Import is explicit:

```bash
contextlattice_passport_import \
  --file passport.json \
  --project contextlattice \
  --pretty
```

The ledger is append-only by identity. An identical digest is idempotent. A
matching parent digest advances the lineage. A divergent root, parent, or
same-revision digest is preserved as a conflict branch; it never overwrites the
existing revision.

## Encrypt for a Recipient

Each ContextLattice instance owns two local identities:

- Ed25519 for Passport and Mesh provenance;
- native age X25519 for envelope decryption.

Private identity material is stored locally with owner-only permissions and is
never returned by an API or CLI command. Print only the public identity:

```bash
contextlattice_mesh_identity --pretty
```

On the sender, create an explicit project-scoped recipient grant using the
recipient's `mesh_recipient` value:

```bash
contextlattice_mesh_grant create \
  --recipient-id workstation-b \
  --recipient age1... \
  --projects contextlattice \
  --ttl-secs 604800 \
  --pretty
```

Encrypt a stored Passport to one or more grants:

```bash
contextlattice_mesh_export \
  --passport-id <passport-id> \
  --grant-ids <grant-id-a>,<grant-id-b> \
  --output context-mesh-envelope.json \
  --pretty
```

The envelope uses the stable `age-encryption.org/v1` format with native X25519
recipients and ChaCha20-Poly1305 payload protection. The encrypted inner payload
binds the project, expiry, envelope ID, sender identity, signed grants, and
signed Passport. The outer envelope is signed too, so tampering is rejected
before import.

Move the JSON through a channel you control. ContextLattice performs zero
delivery network calls.

## Continue The Work, Not Just The Passport

Portable Continuation layers an explicit work-continuation contract on top of
Context Passport and Context Mesh. A collaborative grant binds the project,
topics, data classes, actions, purpose, recipient key, subject snapshot, key
epoch, approval set, usage limit, delegation depth, and validity window. A
signed manifest then references the Passport, lineage, checkpoint, lifecycle
receipt, unresolved obligations, repository constraints, and destination
session by digest only.

The CLI is the prescribed interface:

```bash
contextlattice_agent_tools portable-continuation grant-create \
  --payload-file grant-request.json \
  --output grant.json \
  --pretty

contextlattice_agent_tools portable-continuation manifest-create \
  --payload-file continuation-request.json \
  --output continuation-envelope.json \
  --pretty

contextlattice_agent_tools portable-continuation manifest-reconcile \
  --payload-file reconciliation-request.json \
  --envelope-file continuation-envelope.json \
  --pretty
```

The receiver decrypts the envelope, verifies the outer and inner signatures,
checks sender/issuer identity, authorizes the collaborative grant again against
current time, revocation, usage, subject, purpose, key epoch, and recipient,
then records replay state atomically. Reconciliation remains a dry run and does
not write ordinary memory.

Provenance-preserving imports follow the same boundary. ContextLattice creates
a deterministic atomic plan; an external worker performs the import and
returns an opaque execution digest; ContextLattice records a resumable receipt.
The gateway never reads source content, executes the adapter, or claims the
import happened without that external proof.

## Dry Run Before Apply

The receiving instance defaults to verification and reconciliation planning:

```bash
contextlattice_mesh_import \
  --file context-mesh-envelope.json \
  --pretty
```

Apply only after reviewing the plan:

```bash
contextlattice_mesh_import \
  --file context-mesh-envelope.json \
  --apply \
  --pretty
```

Import fails closed when:

- the local X25519 identity is not a recipient;
- the outer or inner signature is invalid;
- ciphertext, envelope, Passport, or grant digests do not match;
- the envelope, Passport, or grant is expired;
- the grant is revoked or does not include the exact project;
- inner and outer metadata disagree;
- configured byte limits are exceeded.

Revoke a known grant or create a local denylist tombstone for an imported grant:

```bash
contextlattice_mesh_grant revoke \
  --grant-id <grant-id> \
  --reason "recipient retired" \
  --pretty
```

Because ContextLattice is not a transport, sender-side revocation cannot reach a
recipient by magic. Distribute revocation information through the same trusted
channel used for envelopes, then create the local tombstone before import.

## HTTP Fallbacks

The CLI is the prescribed interface. App integrations can use:

- `POST /memory/context-passport/export`
- `POST /memory/context-passport/verify`
- `POST /memory/context-passport/diff`
- `POST /memory/context-passport/replay`
- `POST /memory/context-passport/import`
- `GET /memory/context-mesh/identity`
- `GET|POST /memory/context-mesh/grants`
- `POST /memory/context-mesh/grants/revoke`
- `POST /memory/context-mesh/export`
- `POST /memory/context-mesh/import`
- `GET|POST /memory/portable-continuation/grants`
- `GET|POST /memory/portable-continuation/imports`
- `GET|POST /memory/portable-continuation/manifests`
- `GET /telemetry/portable-continuation`
- `GET /telemetry/context-passport`
- `GET /telemetry/context-mesh`

No MCP tool is added. Portable context should not inflate every agent's tool
list just because the capability exists.

## Storage and Limits

Passport, identity, grant, revocation, and receipt state is stored in the
existing durable data volume. Defaults are bounded and configurable:

```bash
CONTEXTLATTICE_CONTEXT_PASSPORT_MAX_BYTES=16777216
CONTEXTLATTICE_CONTEXT_PASSPORT_MAX_ENTRIES=512
CONTEXTLATTICE_CONTEXT_PASSPORT_MAX_ITEM_BYTES=184320
CONTEXTLATTICE_CONTEXT_MESH_STATE_MAX_BYTES=2097152
CONTEXTLATTICE_CONTEXT_MESH_MAX_GRANTS=512
CONTEXTLATTICE_CONTEXT_MESH_MAX_REVOCATIONS=1024
CONTEXTLATTICE_CONTEXT_MESH_MAX_RECEIPTS=512
CONTEXTLATTICE_CONTEXT_MESH_MAX_ENVELOPE_BYTES=393216
CONTEXTLATTICE_CONTEXT_MESH_MAX_PLAINTEXT_BYTES=262144
CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_ENABLED=true
CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_MAX_BYTES=8388608
```

Passport compaction preserves the newest complete signed records that fit the
configured byte budget. Mesh state keeps bounded grants, revocation tombstones,
and import receipts. Revocation capacity fails closed instead of silently
forgetting an older denylist entry; raise the explicit bound when necessary.
Neither store contains raw model prompts, provider keys, or
private identity material in telemetry responses.

Portable Continuation adds a bounded owner-only digest/provenance store for
signed grants, revocation tombstones, import plans and receipts, manifest
metadata, usage, and replay records. It stores neither ciphertext nor plaintext
continuation payloads; the caller owns the envelope artifact.

## Distribution Boundary

Public ContextLattice includes local Passport export/import, deterministic
diff/replay, recipient grants, encrypted envelope export/import, and manual
conflict-safe reconciliation.

Paid distributions may add entitlement-gated multi-party batch orchestration.
They still do not own transport, silently activate imported behavior, or replace
conflicts with last-write-wins.
