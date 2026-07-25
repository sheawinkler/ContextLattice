# Skill Efficacy Review

Skill Efficacy Review connects the existing Skills Index, agent-session ledger,
Utility Ledger, and Skill Foundry. It measures whether a discovered skill was
actually selected, invoked, and associated with an independently verified
outcome. It does not create a second skill runtime or edit an installed skill.

## Evidence chain

One `usage_id` advances through four append-only snapshots:

1. `searched` records a query digest, result rank, and bounded matched terms.
   Raw queries are not stored, and search evidence receives no efficacy credit.
   The gateway resolves the skill and digest against the native index; rank and
   matched terms remain explicitly agent-reported search metadata.
2. `selected` records why the skill was chosen.
3. `invoked` records the invocation mode and the exact recorded skill digest.
4. `verified_outcome` binds the invocation to the same project, session, and
   agent in both the Utility Ledger and a matching
   `context_pack.outcome_reported` session event.

Stages cannot be skipped, repeated, or reversed. Every transition supplies the
previous receipt digest. Idempotency replays the exact original snapshot,
including after Skill Foundry compaction; conflicting material fails closed.

The final attribution retains verified utility, first-pass result, repairs,
retries, corrections, latency, cost, tool calls, failures, pairing metadata,
and exact token denominators. A caller assertion by itself is never outcome
proof.

## Review decisions

`efficacy-review` persists one inactive decision:

- `retain`
- `add_bounded_note`
- `revision_candidate`
- `retirement_candidate`
- `abstain`

Retain requires at least three verified uses. Retirement requires at least six
verified uses plus repeated failure, retry, or correction evidence. Notes and
revisions require at least three baseline uses and three disjoint exact-matched
holdouts, positive mean utility lift, and no regression in first-pass rate,
failure rate, retry rate, correction rate, latency, or cost.

Change candidates also require:

- an exact match between the recorded skill digest and the current indexed
  `SKILL.md`;
- at least 50 percent novel non-empty lines;
- no detected secret material;
- at most 8 lines and about 160 tokens for a note;
- at most 40 lines and about 800 tokens for a revision.

Third-party and quarantined skills can produce only an inactive local overlay
or upstream-PR candidate. System skills can produce only an inactive local
overlay. Local skills can produce an inactive Foundry revision or local
overlay. Reviews perform no model, provider, network, subprocess, installation,
activation, retirement, active-skill, or vendor-source mutation. Their only
filesystem mutation is the explicit owner-only Skill Foundry ledger write.

## CLI

The canonical interface reads one owner-only JSON file and prints formatted
JSON by default:

```bash
contextlattice_agent_tools skill-evolution usage-record \
  --payload-file searched.json

contextlattice_agent_tools skill-evolution efficacy-review \
  --payload-file review.json
```

A first-stage usage payload has this shape:

```json
{
  "project": "contextlattice",
  "usage_id": "usage_release_gate_001",
  "idempotency_key": "usage_release_gate_001_searched",
  "stage": "searched",
  "session_id": "session_001",
  "agent_id": "codex_gpt5",
  "skill": {
    "id": "skill_release_gate",
    "name": "verified-release-gate",
    "version": "1.0.0",
    "digest": "sha256:<64-hex-skill-md-digest>",
    "source_kind": "third_party",
    "source_ref": "owner/repository"
  },
  "search": {
    "query_digest": "sha256:<64-hex-query-digest>",
    "rank": 1,
    "matched_terms": ["release", "verification"]
  }
}
```

The next stages reuse `usage_id`, provide a new `idempotency_key`, and bind
`expected_previous_receipt_digest`:

```json
{
  "usage_id": "usage_release_gate_001",
  "idempotency_key": "usage_release_gate_001_selected",
  "stage": "selected",
  "expected_previous_receipt_digest": "sha256:<previous-receipt-digest>",
  "selection": {"reason_code": "agent_judgment"}
}
```

Use `invocation: {"mode": "workflow"}` for `invoked`, then
`outcome: {"outcome_id": "<verified-utility-outcome-id>"}` for
`verified_outcome`.

A bounded note review names explicit baseline and holdout usage identities:

```json
{
  "project": "contextlattice",
  "skill_id": "skill_release_gate",
  "name": "verified-release-gate",
  "idempotency_key": "review_release_gate_001",
  "baseline_usage_ids": ["usage_control_1", "usage_control_2", "usage_control_3"],
  "holdout_usage_ids": ["usage_holdout_1", "usage_holdout_2", "usage_holdout_3"],
  "proposal": {
    "kind": "note",
    "summary": "Keep one verified release cue.",
    "bounded_delta": "Before release, verify one source-bound artifact fact.",
    "delivery": "local_overlay"
  }
}
```

Use `proposal.kind=none`, empty holdouts, and zero or more considered usage IDs
for a retain-or-abstain review. Search-only evidence always produces
`abstain`.

## Persistence and contracts

Usage snapshots and reviews share the bounded, owner-only, fsync-capable Skill
Foundry ledger. Compaction retains the newest material plus exact transaction
replay evidence. Status exposes only bounded counts and recent review metadata.

Public response contracts:

- `skill_usage_receipt.v1`
- `skill_efficacy_review.v1`

The implementation evaluation ledger is
[`docs/evals/v4.0.6-skill-efficacy-review-bridge.json`](evals/v4.0.6-skill-efficacy-review-bridge.json).
