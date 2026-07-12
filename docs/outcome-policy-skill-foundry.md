# Outcome Policy and Skill Foundry

ContextLattice v3.13 closes two loops without giving either loop permission to
silently change an agent runtime.

## Outcome-trained context policy

The policy engine reads the existing bounded Context Pack Quality Ledger. Only
rows marked `calibration_eligible=true` can seed a candidate. Infrastructure
failures, missing runners, timeouts, and blocked safety outcomes remain visible
but cannot poison context calibration.

Candidate lifecycle:

1. `candidate`: at least 20 eligible historical outcomes establish a bounded
   proposal and its evidence lineage. The candidate ID binds a digest of that
   evidence set, so a different dataset cannot inherit an older evaluation.
2. `shadow`: the proposal is observed without changing live retrieval.
3. `canary`: separate control and candidate arms must meet sample floors and
   quality, repair, follow-up-token, and provider-token guardrails. Persisted
   evidence must match the candidate ID, project, and current lifecycle phase;
   shadow rows cannot satisfy a canary gate.
4. `promoted` or `rolled_back`: one transition per evaluation. A material
   regression goes directly to rollback; phases cannot be skipped. Replaying
   candidate generation cannot reset a later phase, and stale concurrent
   transitions are rejected. Both terminal phases reject further evaluation;
   new evidence creates a new evidence-bound candidate.

Public v3.13 is advisory. Even a `promoted` record reports
`runtime_activation=false`. Entitled distributions may add operator-controlled
activation, but that boundary is not part of the public runtime.

Primary CLI:

```bash
contextlattice_policy_candidate --project contextlattice --pretty
contextlattice_policy_evaluate --candidate-id <id> --apply-transition --pretty
contextlattice_policy_status --pretty
```

To collect controlled evidence through any supported agent adapter:

```bash
contextlattice_agent_adapter outcome \
  --policy-id <candidate-id> \
  --policy-arm control \
  --policy-phase shadow \
  --first-pass-success true \
  --repair-required false
```

Use `--policy-arm canary` for candidate-arm outcomes. Do not put prompts,
completions, source text, or secrets in outcome metadata.

## Skill Foundry

Skill Foundry accepts repeated workflow-run evidence, not a prose wish. A draft
requires at least three distinct, verified, successful runs with the same
normalized step sequence, verification checks, and evidence references. The
draft records run identities, provenance, verification checks, an
existing-skill collision check, and an inactive `SKILL.md` artifact. Its ID
binds a content fingerprint over the project, version, description,
supersession, steps, and checks so changed behavior cannot reuse an old
holdout result.

Promotion boundaries:

1. Training runs and holdouts must have disjoint identities.
2. At least three independent holdouts must reproduce the workflow and pass
   their checks. Every holdout needs a distinct identity and bounded evidence
   references; boolean assertions alone are rejected.
3. Export requires `human_approved=true` and a named approver.
4. Export does not write into an active skill root or activate anything.
5. Same-name exports must declare the skill they supersede. Draft retirement is
   explicit, terminal, immutable, and non-destructive. Retirement changes draft
   history only and never uninstalls a separately installed skill.
6. Collision state is refreshed at export, not trusted from an older draft snapshot.
7. Installation remains a separate Skills Index review action.

Primary CLI:

```bash
contextlattice_skill_draft --payload-file workflow-runs.json --pretty
contextlattice_skill_draft --payload-file workflow-runs.json --skill-version 2 --supersedes bounded-release-proof --pretty
contextlattice_skill_evaluate --draft-id <id> --payload-file holdouts.json --pretty
contextlattice_skill_export --draft-id <id> --human-approved --approver <identity> --pretty
contextlattice_skill_retire --draft-id <id> --operator <identity> --reason "temporary proof completed" --pretty
contextlattice_skill_foundry_status --pretty
```

Minimal draft input:

```json
{
  "project": "contextlattice",
  "name": "bounded-release-proof",
  "description": "Use for repeatable bounded release proof.",
  "workflow_runs": [
    {
      "run_id": "run-1",
      "verified": true,
      "success": true,
      "steps": ["Inspect scoped state", "Apply one bounded change", "Run deterministic verification"],
      "checks": ["Tests pass", "Diff is bounded"],
      "evidence_refs": ["check:run-1"]
    }
  ]
}
```

Provide at least three distinct rows. Holdouts use the same shape plus
`checks_passed=true`, with IDs not used during drafting.

## HTTP fallbacks

CLI is the prescribed local-agent interface. HTTP remains available for app
integration:

- `POST /memory/context-policy/candidate`
- `POST /memory/context-policy/evaluate`
- `GET /telemetry/context-policy`
- `POST /memory/skills/foundry/draft`
- `POST /memory/skills/foundry/evaluate`
- `POST /memory/skills/foundry/export`
- `POST /memory/skills/foundry/retire`
- `GET /telemetry/skills/foundry`

Tool-call hosts use the existing `/tools/context_policy_*` and Foundry
draft/evaluate/export wrappers. Retirement stays CLI/HTTP-only so lifecycle
hygiene does not increase the agent tool surface.

## Persistence and limits

Both ledgers are bounded NDJSON stores under the configured ContextLattice data
root. They support fsync and compaction. Current candidate/draft state is kept
when history is trimmed, along with the newest evaluation per candidate or
draft. Public status responses report counts and errors, not filesystem paths.
Replaying an identical draft cannot regress `evaluated`, `exported`, or
`retired` state. Retirement records operator, reason, timestamp, fingerprint,
and explicit proof that no deletion or runtime mutation occurred.

Contracts:

- `context_policy_candidate.v1`
- `context_policy_evaluation.v1`
- `skill_draft.v1`
- `skill_evaluation.v1`
- `skill_export.v1`
- `skill_retirement.v1`
