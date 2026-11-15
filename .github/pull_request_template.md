## Outcome

What user or operator outcome changes?

## Lane And Boundary

- Lane: public / commercial / private development
- Interface: CLI / dashboard / HTTP / MCP / internal tooling
- Security, privacy, entitlement, or projection boundary affected:

## Verification

- Exact targeted checks:
- Live or interactive proof, if applicable:
- Not run, with exact reason:

## Rollback

How is the change disabled or reverted without rewriting release history?

## Release Truth

- [ ] Docs and generated contracts agree.
- [ ] No secrets, personal paths, private docs, or customer data are exposed.
- [ ] Unrelated work was preserved.

## Host Lifecycle Safety

<!-- contextlattice-host-lifecycle-safety -->

Impact classification:

- [ ] This PR does not change a host supervisor, installer, scheduler, runtime start/stop path, or recovery policy.
- [ ] This PR is host-lifecycle critical and includes every item below.

Source-guard-only source merge (select this scope instead of the other impact classifications):

- [ ] Source-guard-only: all host-lifecycle critical changes are source fixtures or guards, not installer, supervisor, scheduler, cadence, or recovery payload changes.
- [ ] A peer reviewed the full diff, preserved failure controls, and verified no indirect runtime payload change.
- [ ] The affected source audit and failure injection checks pass.
- [ ] Native installed and final-artifact qualification remain release gates; this source-guard merge does not claim them.

One-shot bootstrap-only source merge (all changed paths must fit the finite installer/source-guard scope; do not use for scheduled behavior):

- [ ] This PR changes only one-shot bootstrap installers and their source guards, not scheduled payloads, cadence, or recovery policy code.
- [ ] A peer reviewed the allowed-file changes; scheduled supervisor payloads, cadence and recovery policy code are unchanged.
- [ ] Native installed and final-artifact qualification remain release gates; source merge does not claim them.

Required for bootstrap-only and full critical source-merge paths:

- [ ] The source audit and failure injection suite pass.

Additionally required before merge for scheduled payloads, supervision, cadence, recovery, mixed or unclassified critical changes:

- [ ] The installed payload matches the reviewed source and the upgrade path was exercised.
- [ ] Any launchd smoke uses an isolated launchd label and cannot touch an operator's loaded job.
- [ ] Docker-unavailable, startup-transition, health-failure, high-CPU, locking, and stop-failure cases were exercised.
- [ ] At least two real scheduled intervals preserved process/container identities and restart counts.
- [ ] Destructive recovery remains explicit opt-in, bounded, and has a tested rollback.
