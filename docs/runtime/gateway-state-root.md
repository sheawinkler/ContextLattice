# Gateway durable state root

ContextLattice gateway-owned durable state resolves beneath one canonical root:

```text
CONTEXTLATTICE_GATEWAY_STATE_ROOT
```

Resolution is independent of the process working directory. Existing per-surface variables such as `GO_AGENT_SESSIONS_PATH`, `FEEDBACK_HISTORY_PATH`, and `GO_RETRIEVAL_CONTINUATION_DURABLE_DIR` remain higher-priority compatibility overrides.

Set the canonical root to an absolute path. A relative `CONTEXTLATTICE_GATEWAY_STATE_ROOT` is reported unhealthy and is never converted through the process working directory.

## Resolution order

The gateway selects the first available root:

1. `CONTEXTLATTICE_GATEWAY_STATE_ROOT`
2. `GO_MEMORY_STORE_ROOT/_contextlattice`
3. `MEMORY_BANK_ROOT/_contextlattice`
4. `CONTEXTLATTICE_GLOBAL_HOME/state/gateway`
5. `~/.contextlattice/state/gateway`
6. a temporary fallback, reported unhealthy because it is not a durable production tier

Legacy defaults beginning with `.data/orchestrator` or `services/orchestrator/data` are rebased beneath that root. Absolute per-surface overrides are retained exactly.

## Compose ownership

Full and Lite Compose set the gateway root to `/data/memory-bank/_contextlattice`. The `gateway-go` consumer mounts `${MEMORY_BANK_DATA:-memory_bank_data}` at `/data`; therefore the state root and memory store share the operator-selected persistent volume without encoding a host-specific path.

To place the volume on external storage, set `MEMORY_BANK_DATA` to the operator-owned volume or bind-mount source. Keep `CONTEXTLATTICE_GATEWAY_STATE_ROOT` in the container filesystem namespace. Do not point it at an unmounted host path.

Public, public-paid, and private-development lanes use the same root contract. Entitlement controls affect features, not storage-path resolution.

The gateway handles `SIGTERM` and `SIGINT` by stopping new accepts and draining active requests for up to `GO_GATEWAY_SHUTDOWN_TIMEOUT_SECS` (20 seconds by default, hard-capped at 25 seconds). Full and Lite Compose allow a 30-second stop grace period. A completed drain exits zero and logs `gateway-go shutdown complete`; a drain timeout closes the listener and exits nonzero rather than claiming a clean stop.

## Inventory and doctor

The gateway emits the resolved root at startup and exposes a non-mutating inventory at:

- `GET /status` under `gatewayState`
- `GET /telemetry/storage` under `gatewayState`
- `contextlattice state status --pretty`
- `contextlattice doctor --pretty` as the `gateway_state` check

Each entry reports the exact path, storage tier, persistence class, override source, and writable/traversable result. `permission_denied` is used only when the operating system returns an exact access-denied error. Missing paths, type conflicts, and other configuration failures remain path evidence.

## Legacy migration

Migration is dry-run by default and requires an explicit legacy root. The root may be a directory tree or one regular file; a file-root migration preserves the file's basename beneath the canonical root:

```zsh
contextlattice state migrate \
  --legacy-root /absolute/legacy/root \
  --state-root /absolute/canonical/root \
  --pretty
```

For example, the v4.0.6 Compose session ledger can be planned without selecting its parent `/data` directory, which also contains the canonical root:

```zsh
contextlattice state migrate \
  --legacy-root /data/agent_sessions.json \
  --state-root /data/memory-bank/_contextlattice \
  --pretty
```

Before apply, identify the exact gateway PID/container and active dependents, then stop only that gateway writer. Apply only after reviewing every path, digest, byte count, conflict, and free-space result:

```zsh
contextlattice state migrate \
  --legacy-root /absolute/legacy/root \
  --state-root /absolute/canonical/root \
  --apply --yes --pretty
```

The apply path rejects symlinks, special files, and the reserved `.migrations` namespace; checks destination conflicts and free space before mutation; copies through same-directory temporary files; and revalidates every source and destination SHA-256 before renaming the selected legacy file or directory to a timestamped rollback backup. A root-type-bound, root-path-bound, plan-digest-bound manifest is stored beneath `<state-root>/.migrations/`.

Same-path files with the same SHA-256 are reused and are not copied. Different content blocks the migration before mutation. The timestamped backup is intentional duplication for rollback; retain it until restart, retrieval, and backup evidence is accepted, then retire it through the operator's normal backup policy.

## Rollback and restore

Rollback requires the exact manifest and confirmation:

```zsh
contextlattice state rollback \
  --manifest /absolute/canonical/root/.migrations/<migration-id>.json \
  --yes --pretty
```

Rollback first verifies the backup and every migration-created destination against the manifest. If canonical content changed, rollback stops without changing either side. A successful rollback restores the legacy root, removes only unchanged files created by that migration, preserves pre-existing deduplicated destinations, and updates the manifest with a rollback receipt.

For ordinary backup/restore, stop only the exact gateway consumer after identifying its dependents, snapshot the persistent volume and `.migrations` manifests together, restore to the same mounted state root, then run `contextlattice state status`, restart persistence tests, and a real retrieval readback before declaring recovery.
