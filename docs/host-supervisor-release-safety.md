# Host Supervisor Release Safety

Host supervisors, installers, schedulers, runtime start/stop paths, and recovery policies are host-lifecycle critical. A passing unit suite is necessary but not sufficient because the installed payload and scheduler identity can behave differently from a direct source invocation.

## Mandatory Gate

Run the deterministic contract and failure injection suite:

```sh
python3 scripts/agent/audit-host-supervisor-safety --pretty
python3 scripts/tests/test_orbstack_self_heal_install.py
scripts/agent/audit-agent-global-install-smoke --pretty
```

The source gate checks that destructive VM recovery is disabled by default, recovery has locks and circuit breakers, Docker commands select a context, and source contracts retain digest and upgrade guards. It does not prove an installed payload matches source. Operators restricted to Zsh may select `--syntax-shell zsh`; this is Zsh syntax evidence, not Bash or native installed execution.

## Source Merge Versus Release

Scheduled payloads, runtime supervision, cadence and recovery changes retain the full interactive pre-merge evidence below. Mixed or unclassified critical changes also retain that gate.

A source-guard-only PR may use the distinct source-guard checklist when at least one host-critical path changed and every changed host-critical path is one of `scripts/agent/audit-host-supervisor-safety`, `scripts/tests/test_host_supervisor_safety_audit.py`, or `scripts/tests/test_installer_transaction_readiness.py`. Every other changed path must appear in the audit's literal `SOURCE_GUARD_COMPANION_PATHS` allowlist. The generic companions are `AGENTS.md`, `.github/pull_request_template.md`, `docs/host-supervisor-release-safety.md`, `scripts/docker_anonymous_public.sh`, and `scripts/tests/test_docker_anonymous_public.py`. Unknown companions fail closed; these paths still retain their own applicable checks. A peer must review the full diff, preserve failure controls, and attest that no indirect runtime payload changed. Run the affected source audit and failure-injection checks. Installer, supervisor, scheduler, cadence, recovery, environment or workflow changes cannot use this mode. Do not select no-impact, bootstrap-only, or full-critical checkboxes together with it. This is a static path screen and peer attestation, not proof of runtime behavior. Native installed and final-artifact qualification remain release gates, not source-guard merge evidence.

A one-shot bootstrap-only source PR may merge after applicable local source audits, failure injection and peer review. The audit requires at least one bootstrap installer entry point and permits only those paths plus its explicit policy, checklist, runbook and audit/test files. This is a path-and-attestation screen, not automated behavioral classification. Check all bootstrap declarations and the shared source-suite item in the PR template. A peer must inspect the actual allowed-file hunks for scheduled/supervision/recovery effects; any such effect retains the full scheduled gate. A declaration cannot exempt other changed paths. Guard-only or test-only changes cannot claim bootstrap-only scope.

For bootstrap changes, also run `python3 -m unittest -v scripts.tests.test_installer_transaction_readiness scripts.tests.test_host_supervisor_safety_audit`. Record exact selectors, shell, failures and skips: POSIX/Zsh synthetic installer fixtures and Windows static guards are not native Windows acceptance. For a Zsh-only local operator, set `CONTEXTLATTICE_AUDIT_SYNTAX_SHELL=zsh` for audit tests and use `--syntax-shell zsh` for the audit CLI.

This narrow source-merge path does not authorize packaging, promotion, tags or release publication. Genuine native installed upgrades, same-final-byte platform acceptance and safe recovery remain release qualifications. Audit output explicitly reports `evidence_scope=source_merge` and `release_qualified=false`, even when every source check passes.

The macOS outer installer stages the shared Unix installer from `packaging/linux`; its directory name is not an OS restriction. Test the actual staged `ContextLattice.command` whenever shared launch preflights change. The unchanged global-tools helper can refresh an already-loaded Darwin supervisor when bootstrap invokes it: bootstrap-only classification is not a claim of zero host side effects. Record that delegation explicitly. Changes to its scheduled payload, cadence or recovery policy retain the full gate above.

## Required Interactive Evidence

Before merging scheduled/runtime-supervision/recovery changes, and before releasing any host-lifecycle change, collect applicable native evidence:

1. Install or upgrade the candidate through the same path users receive.
2. Verify the installed payload digest matches the reviewed source.
3. Use an isolated launchd label for every smoke or fixture. Never reuse an operator's production label.
4. Run failure injection for Docker unavailability, startup transition, application-health failure, high CPU, concurrent triggers, and unload failure.
5. Observe at least two scheduled intervals with unchanged process/container identities, zero unexpected restarts, and healthy application state.
6. Exercise or prove the rollback without deleting user data, resetting the VM, or touching unrelated services.

Do not tag or publish a release containing a host-lifecycle critical change until deterministic checks and interactive evidence both pass. A machine-specific trigger does not excuse a generally unsafe recovery policy.

The source audit screens the actual pull-request paths and checked attestations; it does not analyze behavior. Where hosted workflows are used they consume the same contract; operators using local-only CI run it locally with the exact changed-file list and PR body instead. No checklist is evidence of a native run by itself. Source merge and release qualification must be reported separately.

Changed-path inputs must include both the original and destination paths for renames and copies, and the original paths for deletions; a destination-only list cannot establish the scope. The peer review remains responsible for detecting indirect changes even when a path is classified noncritical.

## Lock Acquisition And Termination

The supervisor installs EXIT, INT and TERM handlers before acquiring its lock.
Signals received during acquisition are deferred until the directory/PID
publication attempt finishes; cleanup then removes only owned state. A unique
PID-scoped acquisition claim prevents a contender from reclaiming a peer's
not-yet-published ownership or racing a dead-owner recovery. The claim remains
until EXIT cleanup. Unowned directories and malformed owner metadata are
conservatively left alone, not guessed to be stale. Dead PID owners may be
recovered when no competing acquisition claim is active.

The failure-injection suite pauses the unmodified source before/after lock
directory creation and before/after PID publication, then sends INT or TERM.
It also exercises contention during unpublished ownership and stale-owner
recovery. Keep the existing exit-code and lock-cleanup assertions: waiting for
the PID file before sending a signal, accepting an unhandled signal exit, or
retrying until green would hide the acquisition window rather than repair it.
SIGKILL cannot run a cleanup handler and is not a clean-shutdown guarantee.
