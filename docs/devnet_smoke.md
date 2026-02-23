# Devnet Smoke Test

Parent issue: [#23](https://github.com/sheawinkler/algotrader_rust/issues/23)

## Why
Before we attempt live trades, we need a deterministic workflow that proves the
runtime, telemetry, and ContextLattice wiring on Solana devnet. The steps below run the
`unified_trader` binary in paper/godmode mode, stream telemetry to the ContextLattice
orchestrator, and confirm events land in the local backup/spool for inspection.

## Prerequisites
1. ContextLattice stack running locally (at least the orchestrator on `http://127.0.0.1:8075`).
   If you set `BOOTSTRAP_ORCH=1`, the script will execute `ORCH_START_CMD` for you
   (default: `cd ~/.mcp-servers/mem_mcp_lobehub && docker compose up -d memmcp-orchestrator`).
2. FastAPI sidecar stub (optional but recommended). When `BOOTSTRAP_SIDECAR=1`, the
   script runs `SIDECAR_START_CMD` (default: change into
   `~/Documents/Projects/crypto_trader_post_training_needs_godmode_and_finalization`
   and launch `poetry run uvicorn project.src.api.fastapi_server:app --host 0.0.0.0 --port 8288`).
3. Devnet wallet JSON (64-byte keypair) with enough lamports for RPC requests.
   Export `WALLET=path/to/devnet_wallet.json` if it differs from `wallet_devnet.json`.
4. Config pointing at devnet (`solana.rpc_url = "https://api.devnet.solana.com"`).
   Override with `SOLANA_RPC_URL` as needed.
5. Healthy orchestrator/sidecar endpoints. The script curls
   `$MEMMCP_ORCHESTRATOR_URL/health` and (unless `SKIP_SIDECAR_CHECK=1`)
   `$SIDECAR_HEALTH_URL` (default `http://127.0.0.1:8288/health`) before running.

## Running the smoke test
```bash
cd ~/Documents/Projects/algotraderv2_rust
export CONFIG=config.toml
export WALLET=wallet_devnet.json
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export SIDECAR_HEALTH_URL=http://127.0.0.1:8288/health
export BOOTSTRAP_ORCH=1           # optional
export BOOTSTRAP_SIDECAR=1        # optional
export SMOKE_DURATION=120         # defaults to 90s
./scripts/devnet_smoke.sh
```

Or simply use Just:
```bash
just devnet-up                     # optional helper (boots orchestrator + sidecar)
just devnet-smoke CONFIG=config.toml WALLET=wallet_devnet.json DURATION=120
# run_strategy variant
just devnet-smoke CONFIG=config.toml WALLET=wallet_devnet.json DURATION=120 BIN=run_strategy
```

Key env toggles:
- `BOOTSTRAP_ORCH` / `ORCH_START_CMD`: auto-start orchestrator (default command uses
  docker compose in the ContextLattice repo).
- `BOOTSTRAP_SIDECAR` / `SIDECAR_START_CMD`: auto-start FastAPI stub (default command
  runs uvicorn via poetry). The script writes stdout/stderr to `/tmp/devnet_sidecar.log`
  and kills the process on exit.
- `SKIP_SIDECAR_CHECK=1`: skip the sidecar health probe (useful when the stub is offline).

The script will:
1. Create `tmp/devnet_backups` and `tmp/devnet_spool`.
2. Run `cargo run --bin unified_trader --mode xavier` in dry-run mode for the requested
   duration.
3. Compare telemetry backup line counts pre/post-run and print the new sled spool file count.

Manual invocation (without the wrapper script):
```bash
MEMMCP_LOCAL_BACKUP_DIR=tmp/devnet_backups \
MEMMCP_LOCAL_STORE_PATH=tmp/devnet_spool \
SOLANA_RPC_URL=https://api.devnet.solana.com \
SIDECAR_HEALTH_URL=http://127.0.0.1:8288/health \
cargo run --bin unified_trader -- \
  --config config.toml \
  --wallet wallet_devnet.json \
  --dry-run \
  --min-balance 0 \
  --mode xavier \
  --enable-sidecar \
  --sidecar-url http://127.0.0.1:8288 \
  --status-interval-secs 10
```

## Verification checklist
- `tmp/devnet_backups/telemetry.ndjson` gained new entries (script reports the delta).
- `scripts/storage_audit.py --json` shows increased point/doc counts.
- Optional: `curl $MEMMCP_ORCHESTRATOR_URL/telemetry/trading | jq` displays recent snapshots.

## Logging to ContextLattice
After each run, log a note via ContextLattice (`project=sol_scaler`, `kind=devnet_smoke`)
with config/wallet aliases, duration, telemetry delta, and any errors.

## Next steps
- Wrap FastAPI/orchestrator bootstrapping into a justfile target for even quicker
  bring-up (or extend docker compose profiles).
- Leverage the same harness for `run_strategy live` once sidecar toggles land, so we
  can demo single-strategy flows on devnet.
