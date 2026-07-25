---
title: CLI reference
summary: The primary commands for context, resume, remember, correct, finish, adoption, proof, and capability discovery.
eyebrow: Primary interface
order: 4
slug: cli
---
# CLI reference

The CLI is the primary ContextLattice interface. It keeps scope, evidence, corrections, and completion explicit while hiding lower-level adapter plumbing during ordinary work.

## Normal task lifecycle

```zsh
contextlattice context "<task>" --project <project> --pretty
contextlattice resume --project <project> --pretty
contextlattice remember "<checkpoint>" --project <project> --pretty
contextlattice correct "<note>" --category useful
contextlattice finish "<verified result>" --project <project> --success --pretty
```

Use one stable project name throughout a task. The CLI automatically associates the latest pending retrieval outcome when `finish` is called.

## Context

```zsh
contextlattice context "prepare the next release" --project contextlattice --pretty
```

This is the default pre-planning read. It returns a bounded `agent_packet.v1` with the objective, ranked evidence, uncertainty, risks, and next action. Use raw or file output only when a programmatic consumer needs it.

## Resume

```zsh
contextlattice resume --project contextlattice --pretty
```

Resume reconstructs the latest bounded objective state. It is preferable to replaying an entire transcript into a new session.

## Remember

```zsh
contextlattice remember "local gates passed at commit abc123" --project contextlattice --pretty
```

Persist decisions, verified results, blockers, and exact next actions. Do not use checkpoints as a dumping ground for logs or speculative narration.

## Correct

```zsh
contextlattice correct "the referenced branch was superseded" --category superseded
```

Available correction categories include `useful`, `wrong`, `stale`, and `superseded`. A correction should explain the evidence mismatch clearly enough that a later retrieval can avoid repeating it.

## Finish

```zsh
contextlattice finish "release built and smoke-tested" --project contextlattice --success --pretty
```

Choose `--success`, `--repair`, or `--failure` to match the verified outcome. Completion is an evidence record, not a confidence score.

## Readiness and adoption

```zsh
contextlattice doctor --pretty
contextlattice_adopt status --pretty
contextlattice_adopt profiles
contextlattice_adopt proof --agents codex --skip-provider-smoke --pretty
```

Use doctor when runtime readiness is unclear. Use adoption commands when a new machine, account, or repository needs profile-aware integration.

## Agent integration

```zsh
contextlattice_adopt integrate \
  --repo . \
  --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid \
  --check \
  --pretty
```

Remove `--check` to write or update managed instruction blocks. Existing user-authored content outside those blocks is preserved.

## Capability discovery

```zsh
contextlattice_skills_index search "browser automation" --pretty
contextlattice_skills_index discover "browser automation" --pretty
contextlattice_skills_index stage owner/repo@skill --pretty
contextlattice_skills_index refresh --due --pretty
contextlattice_skills_index promote owner/repo@skill --yes --pretty
contextlattice_agent_adapter profiles
```

`search` scans configured active roots without loading every skill body into startup context. `discover` normalizes results from Vercel's `npx skills find` without installing them. `stage` clones a bounded GitHub-backed source into quarantine, records its commit and digest, and scans for secrets and hazardous instructions. `refresh --due` updates registered quarantine candidates only; it never changes active skills. `promote` is the only activation path and requires `--yes`. Review findings additionally require `--accept-review`; collisions require `--replace` and preserve the previous directory as a recoverable backup.

The default refresh interval is 24 hours (`CONTEXTLATTICE_SKILLS_REFRESH_INTERVAL_HOURS`). Invoke `refresh --due` from an operator-owned scheduler when periodic checks are desired; ContextLattice does not create a background job implicitly.

## Skill efficacy review

```zsh
contextlattice_agent_tools skill-evolution usage-record --payload-file usage-stage.json
contextlattice_agent_tools skill-evolution efficacy-review --payload-file review.json
```

A usage receipt advances through `searched`, `selected`, `invoked`, and
`verified_outcome`. Search affects discoverability only. Efficacy requires the
same project, agent, and session in both the Utility Ledger and a matching
session outcome event. Reviews can retain, abstain, or create an inactive
bounded-note, revision, or retirement candidate. They never edit or activate
an installed skill, and third-party improvements remain local-overlay or
upstream-PR candidates.

Notes and revisions require three verified baseline uses, three disjoint
exact-matched holdouts, positive utility lift, no material regression, an exact
current skill digest, novelty, and fixed size limits. See the
[full receipt and review contract](https://github.com/sheawinkler/ContextLattice/blob/main/docs/skill-efficacy-review.md).

## Advanced packets and traces

```zsh
contextlattice_synthesis_pack "release risk" --project contextlattice --pretty
contextlattice_agent_trace --session-id <session-id>
contextlattice_agent_trace --session-id <session-id> --preset proof
contextlattice_agent_trace --session-id <session-id> --preset export
```

Use synthesis packs when a complex decision needs findings, topic gravity, graph bridges, constraints, next actions, and open questions over the same bounded evidence. Use traces to inspect the run-shaping trail; missing links remain visible rather than inferred.

## Exit discipline

Interactive terminals default to readable output; pipes and redirected output stay compact for automation. Override with `CONTEXTLATTICE_CLI_OUTPUT=pretty|compact`, `--pretty`, or `--raw`. Commands should still be judged by their exit status and concrete fields. Keep secrets out of pasted examples and logs.
