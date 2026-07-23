---
title: Getting started
summary: Install ContextLattice, launch the right local profile, prove health, and complete a first context lifecycle.
eyebrow: First run
order: 2
slug: getting-started
---
# Getting started

The canonical technical path is a repository checkout driven by `gmake quickstart`. Installers and the Homebrew cask are bootstrap alternatives; they do not replace the CLI-first operating model.

## Prerequisites

- macOS, Linux, or Windows through WSL2.
- A Compose v2-compatible container runtime.
- `gmake`, `jq`, `rg`, `python3`, and `curl`.
- Enough CPU, memory, disk, and free storage for the selected profile.

Lite is the sensible first profile. Move to full or operator surfaces only when the workload needs their additional sources and the host has measured headroom.

## Install from source

```zsh
git clone https://github.com/sheawinkler/ContextLattice.git
cd ContextLattice
cp .env.example .env
gmake quickstart
```

The quickstart creates missing local wiring, asks for the runtime profile when needed, starts the services, and runs its initial health and authentication checks.

## Verify the runtime

Start with the unauthenticated liveness surface, then use the CLI for the complete local readiness view.

```zsh
curl -fsS http://127.0.0.1:8075/health | jq
contextlattice_adopt status --pretty
contextlattice doctor --pretty
```

`/health` proves that the orchestrator is answering. It does not, by itself, prove that every retrieval source can serve a real read. The adoption and doctor commands add configuration, storage, profile, helper, and authenticated runtime evidence.

## Integrate an existing repository

Run integration from the repository that the agent will work in. Managed blocks preserve existing instructions around them.

```zsh
cd /path/to/target/repo
contextlattice_adopt integrate \
  --repo . \
  --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid \
  --pretty

contextlattice_adopt integrate \
  --repo . \
  --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid \
  --check \
  --pretty
```

This integrates supported profiles already present on the machine. It does not install third-party agent harnesses.

## Run the first lifecycle

```zsh
contextlattice context "map the repository before changing it" --project my-project --pretty
contextlattice remember "repository inspected; no edits yet" --project my-project --pretty
contextlattice resume --project my-project --pretty
contextlattice finish "inspection completed" --project my-project --success --pretty
```

The first command retrieves a compact proof-carrying packet. `remember` persists a high-signal checkpoint, `resume` reconstructs bounded task state, and `finish` records the verified outcome.

## Alternative distribution paths

- Homebrew: `brew tap sheawinkler/contextlattice && brew install --cask contextlattice`
- macOS, Windows, and Linux bootstrap artifacts: use the assets attached to the [latest public release](https://github.com/sheawinkler/ContextLattice/releases/latest).
- Container-only hosted experiments: follow the public deployment guidance in the repository.

After any bootstrap path, return to the verification and first-lifecycle sections above.

## Next

Read [Core concepts](concepts.md) to understand why ContextLattice separates liveness, retrieval evidence, checkpoints, and outcome feedback.
