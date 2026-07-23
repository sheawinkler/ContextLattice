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
contextlattice_agent_adapter profiles
```

Skills Index searches configured capability roots without loading every skill body into startup context.

## Advanced packets and traces

```zsh
contextlattice_synthesis_pack "release risk" --project contextlattice --pretty
contextlattice_agent_trace --session-id <session-id> --tree
contextlattice_agent_trace --session-id <session-id> --markdown --proof
```

Use synthesis packs when a complex decision needs findings, topic gravity, graph bridges, constraints, next actions, and open questions over the same bounded evidence. Use traces to inspect the run-shaping trail; missing links remain visible rather than inferred.

## Exit discipline

Commands that report JSON or `--pretty` output should still be judged by their exit status and concrete fields. Keep secrets out of pasted examples and logs.
