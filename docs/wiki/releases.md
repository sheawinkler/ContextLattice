---
title: Releases
summary: Verify the current release, upgrade with evidence, and distinguish local proof from hosted or deployment state.
eyebrow: Promote evidence
order: 8
slug: releases
---
# Releases

The current public release baseline is `v4.0.7`. Release identity comes from the canonical commercial truth contract, signed or attributable artifacts, exact source SHAs, and deterministic verification—not from a page label alone.

## Before upgrading

- Read the [release notes](https://github.com/sheawinkler/ContextLattice/releases/latest).
- Record the current source SHA, runtime profile, and storage locations.
- Preserve a bounded backup or snapshot appropriate to the active backends.
- Run a representative context query and save its evidence.
- Confirm enough free space for images, indexes, and any migration.

## Install or upgrade

Source operators should use the repository instructions and `gmake quickstart`. Homebrew and installer users should use the matching artifact from the same release.

```zsh
git fetch --tags origin
git checkout v4.0.7
gmake quickstart
```

Do not move a production checkout across releases while its owned runtime is writing. Follow the repository release notes when a version includes migration or rollback guidance.

## Verify after upgrade

```zsh
curl -fsS http://127.0.0.1:8075/health | jq
contextlattice doctor --pretty
contextlattice_agent_runtime_proof --pretty
```

Repeat the representative context query and compare scope, sources, latency, continuation, and returned evidence with the pre-upgrade record.

## Release evidence ledger

A credible release record contains:

1. source repository, branch, SHA, and tree identity;
2. exact local test commands with pass counts;
3. production build result;
4. runtime rebuild, restart, and real readback where runtime code changed;
5. public and paid projection SHAs when those lanes apply;
6. artifact hashes and signing or notarization posture;
7. hosted CI status labeled as passed, failed, skipped, or externally blocked;
8. rollback path and known limitations.

## Static website publication

The public website is release-bound. Source changes to `docs/public_overview` and generated `/docs` output can merge before publication, but the Pages sync must prove the immutable release tag and source commit it is publishing. A post-release docs change should not be mislabeled as part of an older tag.

## macOS posture

macOS release automation supports optional Developer ID signing and notarization when the credentials are configured. Unsigned technical-preview artifacts must continue to say so; successful local builds do not imply notarization.

## Hosted CI posture

Hosted CI is supplemental evidence after local deterministic gates. A job that never acquires a runner or executes a step is `unrun`, even if the provider renders a red status. Preserve the job link and external blocker instead of changing the local conclusion to fit platform state.

## Rollback

Use the release-specific rollback instructions, restore only the affected runtime and compatible data state, and rerun the same health and retrieval proofs. Avoid broad resets that discard unrelated user data or current memory.

## Follow updates

- [Public release updates](https://contextlattice.io/updates.html)
- [GitHub releases](https://github.com/sheawinkler/ContextLattice/releases)
- [Issue tracker](https://github.com/sheawinkler/ContextLattice/issues)
