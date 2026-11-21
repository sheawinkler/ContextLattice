# Cognition Proof Core

ContextLattice v3.12 adds three Go-native cognition surfaces without replacing
the v1 memory, context-pack, or synthesis contracts.

## Temporal Claim Graph

A memory result says, "this text was retrieved." A temporal claim says, "this
assertion was observed at this time, is valid over this interval, is backed by
these references, and may supersede or contradict these other assertions."

The ledger is bounded, append-only between compactions, local by default, and
safe to reload after restart. It records:

- subject, predicate, object, and human-readable statement;
- `valid_from`, `valid_to`, and `observed_at`;
- support and opposition references;
- supersession, contradiction, and causal links;
- verification state and method;
- project, topic, agent, session, branch, commit, and provenance;
- revision and durable timestamps.

CLI:

```bash
contextlattice_claim_write --project contextlattice --subject release --predicate current_version --object 3.12.0 --pretty
contextlattice_claim_query "current release" --project contextlattice --include-superseded --pretty
```

HTTP fallbacks:

- `POST /memory/claims`
- `POST /memory/claims/query`
- `POST /tools/claim_write`
- `POST /tools/claim_query`
- `GET /telemetry/claim-graph`

Contracts: `temporal_claim.v1`, `temporal_claim_query.v1`.

Environment:

- `CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED` defaults to `true`.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH` overrides the local ledger path.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_MAX` defaults to `10000`.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_COMPACT_EVERY` defaults to `512`.
- `CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC` defaults to `true`.

Invalid claims are rejected before the in-memory index changes. Superseded
claims remain queryable; history is not overwritten into a single false
"current truth."

### Policy Lab evidence and issuers

The claim APIs still accept and return the original `verification.status` and
other writer labels. They are not independently verified. A local source check
matches the current memory revision's canonical content hash in the same
project and topic at intake; it does not establish whether the claim is true.
An owner signature speaks for the configured subject and predicate (such as
the owner's preference), not for an external fact. Neither source checks nor
owner statements receive independent evidence weight.

A verifier receipt is a configured issuer's signed witness to a specific
observation and its relation to a claim. It binds the receipt to the scope,
claim, polarity, source digest, and observation/check times. It does not prove
that the witness is correct, that the issuer is organizationally independent,
or that two issuers know something from different sources. Provision verifier
keys only for trusted witnesses. Assign mirrors and key rotations the same
`origin_id`, and have them sign the original stable `observation_id` rather
than their own citation or event IDs. Policy Lab counts each origin and
observation pair once per claim and never treats Utility outcome credit as
claim evidence.

To provision issuers, configure
`CONTEXTLATTICE_TEMPORAL_CLAIM_EVIDENCE_ISSUERS` in the gateway environment as a
JSON array of up to 64 grants (64 KiB total). Each grant has an exact `project`
and normalized `topic_path`, a raw standard-base64 Ed25519 `public_key` (no
padding), and a `role` of `verifier` or `owner`. A verifier requires a stable
`origin_id`; an owner requires the exact normalized `subject` and
`predicate`. Keep signing keys outside the gateway. No configured grant
means no independently bound evidence.

Submit a `receipt` on a support or opposition reference with
`observation_id`, `checked_at` (RFC3339), and a `signature` object containing
`algorithm: "Ed25519"`, `key_id`, and raw standard-base64 `value`. The key
ID is `ed25519:` plus the first 24 hexadecimal SHA-256 characters of the
encoded public key. Sign the Go `encoding/json` serialization of the
`temporal_claim_evidence_receipt.v1` payload defined by
`temporalClaimEvidenceSigningPayload`: use the normalized claim and citation
fields, the configured role and origin, and the same polarity and receipt
fields. Provide explicit claim and citation observation times. A receipt
cannot predate either observation or claim a future check time. The gateway
does not fetch external URLs during admission.

Policy Lab compares only active, currently valid claims. The window starts at
`valid_from` (inclusive) and ends at `valid_to` (exclusive); explicitly
failed verification is ineligible even with an admitted receipt. Approved
manual decisions, appeals, and reopening remain separate from this advice.

To revoke an issuer, remove its grant from the gateway environment and apply
the configuration through the site's normal deployment procedure. Policy Lab
matches stored admissions against the currently configured exact grant and
reports unmatched attestations as inactive, not as current proof. Claims and
their original check timestamps remain readable. `evidence_admission_basis`
labels that history on claim reads; `source_rechecked: false` on a
recommendation means no new source read took place. Legacy records without
a grant-bound admission are not upgraded or rechecked automatically. Re-submit
a valid signed receipt under a current grant to admit new evidence.

## Adaptive Retrieval Planner

The planner classifies task phase, names evidence obligations, ranks configured
sources with observed reliability and p95 latency, proposes bounded query
expansion, allocates the token budget, and states marginal-value stop rules.

```bash
contextlattice_retrieval_plan "verify cross-project release readiness" --project contextlattice --pretty
```

HTTP fallbacks:

- `POST /memory/retrieval/plan`
- `POST /tools/retrieval_plan`

Contract: `retrieval_plan.v1`.

The public v3.12 planner is always `mode=advisor` and
`activation_state=shadow_only`. It does not mutate live retrieval policy. A
later outcome-trained canary must earn activation with holdouts and rollback.

## Proof-Carrying Synthesis v2

Synthesis v2 starts from the existing bounded Context Pack and Synthesis Pack
v1 evidence. It then emits only findings with an evidence identity and adds:

- support and opposition references per claim;
- structured temporal state and matching claim count;
- explicit causal and supersession context;
- confidence decomposition rather than one unexplained score;
- unresolved contradiction disclosure;
- missing-proof obligations;
- a count of unsupported findings excluded from the response.

```bash
contextlattice_synthesis_pack_v2 "prove release readiness" --project contextlattice --pretty
```

HTTP fallbacks:

- `POST /memory/synthesis-pack/v2`
- `POST /tools/synthesis_pack_v2`

Contract: `synthesis_pack.v2`.

The implementation is deterministic and reports `llm_used=false`. Lexical
matching can link structured claims to retrieved evidence, but it does not
claim semantic equivalence. Unverified claims cannot silently increase
confidence, and contradictions are never auto-resolved in the public core.

## Compatibility

The existing `contextlattice_pack`, `contextlattice_synthesis_pack`,
`/memory/context-pack`, and `/memory/synthesis-pack` surfaces are unchanged.
Agents can adopt v2 per workflow instead of migrating every consumer at once.

Machine-readable measurements and reproduction commands live in
`docs/evals/v3.12-cognition-proof-core.json`.
