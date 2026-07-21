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

Required when host-lifecycle critical:

- [ ] The source audit and failure injection suite pass.
- [ ] The installed payload matches the reviewed source and the upgrade path was exercised.
- [ ] Any launchd smoke uses an isolated launchd label and cannot touch an operator's loaded job.
- [ ] Docker-unavailable, startup-transition, health-failure, high-CPU, locking, and stop-failure cases were exercised.
- [ ] At least two real scheduled intervals preserved process/container identities and restart counts.
- [ ] Destructive recovery remains explicit opt-in, bounded, and has a tested rollback.
