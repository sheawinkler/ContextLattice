# Mainnet Execution Playbook

## Overview
This playbook describes the steps required to run the unified trading stack on
Solana mainnet (Helius RPC), including environment prep, health checks, dry-run
verification, and the live cut-over. It mirrors the devnet smoke workflow but
uses production wallets, RPC endpoints, and stricter safety checks.

## Prerequisites
1. **Production wallet**: JSON keypair funded with SOL to cover trading capital +
   fees. Store the file securely (e.g., `wallet_mainnet.json`).
2. **Helius RPC endpoint**: provision an API key and set
   `SOLANA_RPC_URL=https://rpc.helius.xyz/?api-key=<KEY>`.
3. **ContextLattice stack**: orchestrator + sidecar running locally or on a secured host
   (use `just devnet-up` for local tests or your deployment of choice).
4. **Telemetry destinations**: ContextLattice / Langfuse should be reachable; confirm via
   `scripts/storage_audit.py --json` and `curl $MEMMCP_ORCHESTRATOR_URL/health`.
5. **Runbook logging**: continue logging milestones to ContextLattice under
   `project=sol_scaler`, `kind=mainnet_run`.

## Dry-run verification (required)
1. Set env vars for mainnet:
   ```bash
   export SOLANA_RPC_URL="https://rpc.helius.xyz/?api-key=<KEY>"
   export WALLET=wallet_mainnet.json
   export MEMMCP_ORCHESTRATOR_URL=<prod orchestrator>
   export SIDECAR_HEALTH_URL=<prod sidecar health>
   ```
2. Run the smoke suite in **dry-run** first to ensure telemetry/orchestrator flows
   are healthy:
   ```bash
   just devnet-up
   just devnet-smoke CONFIG=config_mainnet.toml WALLET=$WALLET DURATION=120 BIN=unified_trader SKIP_SIDECAR_CHECK=0
   just devnet-smoke CONFIG=config_mainnet.toml WALLET=$WALLET DURATION=120 BIN=run_strategy SKIP_SIDECAR_CHECK=0
   ```
3. Verify `telemetry.ndjson` growth, ContextLattice spool, and Langfuse traces. Log the
   run in ContextLattice (`kind=mainnet_run`, mention "dry-run").

## Live execution
1. Double-check Helius RPC quota + wallet balance. Consider setting
   `HELIUS_RATE_LIMIT`/`SOLANA_RPC_TIMEOUT` if needed.
2. Launch orchestrator + sidecar manually or via `just devnet-up` (rename to
   `just mainnet-up` if you prefer); confirm health endpoints.
3. Run `unified_trader`:
   ```bash
   cargo run --bin unified_trader -- \
     --config config_mainnet.toml \
     --wallet wallet_mainnet.json \
     --min-balance 10 \
     --mode xavier \
     --status-interval-secs 15 \
     --enable-sidecar \
     --sidecar-url https://sidecar.yourdomain.com
   ```
4. Optionally run `run_strategy live` for single-strategy testing:
   ```bash
   cargo run --bin run_strategy -- live \
     --config config_mainnet.toml \
     --wallet wallet_mainnet.json \
     --min-balance 10 \
     --status-interval-secs 15 \
     --enable-sidecar \
     --sidecar-url https://sidecar.yourdomain.com \
     --duration-secs 600
   ```
5. Monitor telemetry dashboards (ContextLattice, Langfuse, Next.js UI). Capture
   `scripts/storage_audit.py --json` output for post-run records. Use `just devnet-down`
   (or future `mainnet-down`) to tear down local services when finished.

## Safety checklist
- ✅ Dry-run smoke executed successfully on mainnet RPC.
- ✅ ContextLattice/telemetry backups updating.
- ✅ Wallet balance > minimum threshold; no RPC errors.
- ✅ Sidecar models healthy (sidcar health endpoint passing).
- ✅ ContextLattice log entry created for the run (include wallet alias, config version,
  duration, PnL summary).

## Future enhancements
- Add a `just mainnet-up/down` wrapper with prod compose files.
- Integrate Helius webhooks for account events instead of polling.
- Automate transaction journaling into ContextLattice for compliance.
