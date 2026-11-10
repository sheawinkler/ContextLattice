# Host Supervisor Release Safety

Host supervisors, installers, schedulers, runtime start/stop paths, and recovery policies are host-lifecycle critical. A passing unit suite is necessary but not sufficient because the installed payload and scheduler identity can behave differently from a direct source invocation.

## Mandatory Gate

Run the deterministic contract and failure injection suite:

```bash
python3 scripts/agent/audit-host-supervisor-safety --pretty
python3 scripts/tests/test_orbstack_self_heal_install.py
scripts/agent/audit-agent-global-install-smoke --pretty
```

The gate proves that destructive VM recovery is disabled by default, recovery is bounded by locks and circuit breakers, Docker commands use an explicit context, the installed payload matches source, and upgrade behavior only migrates an already-loaded supervisor.

## Required Interactive Evidence

When a pull request changes a host-lifecycle critical path:

1. Install or upgrade the candidate through the same path users receive.
2. Verify the installed payload digest matches the reviewed source.
3. Use an isolated launchd label for every smoke or fixture. Never reuse an operator's production label.
4. Run failure injection for Docker unavailability, startup transition, application-health failure, high CPU, concurrent triggers, and unload failure.
5. Observe at least two scheduled intervals with unchanged process/container identities, zero unexpected restarts, and healthy application state.
6. Exercise or prove the rollback without deleting user data, resetting the VM, or touching unrelated services.

Do not tag or publish a release containing a host-lifecycle critical change until deterministic checks and interactive evidence both pass. A machine-specific trigger does not excuse a generally unsafe recovery policy.

The public product-truth workflow classifies the actual pull-request diff and fails if a critical change does not have every host-lifecycle evidence item checked in the pull-request body. Push and release gates rerun the deterministic contract independently of that human evidence.

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
