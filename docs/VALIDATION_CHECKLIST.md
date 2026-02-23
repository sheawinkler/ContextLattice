# Phase 5 Validation & Deployment Checklist

This is the single sheet to run before pushing a release candidate. It covers
smoke tests, devnet/dry-run rehearsals, and the hosted deployment drill.

## 1. Pre-flight Sanity
1. `cargo fmt --all -- --check`
2. `scripts/test_strategies.sh` (quick indicator compile check)
3. `PAPER_SMOKE_DURATION=180 scripts/smoke_paper_trading.sh`
   - Confirms the unified engine runs unattended in paper mode, with logs
     written to `logs/smoke/`. Inspect the tail printed at the end for
     warnings or ContextLattice errors.

## 2. Manual Paper Run (Full Logs)
- For longer sessions, run `cargo run --bin algotrader -- --paper` and leave it
  for ≥30 minutes. Watch `logs/algotrader.log` for:
  - Circuit-breaker triggers (`⚠️ Trade failure` or `⏸️ Trading halted`).
  - DEX routing `dex=Jupiter|Raydium|Photon` lines per trade.
- Stop with `Ctrl+C`. The breaker automatically resets after the configured
  cooldown; the log should show `Trading halted` → resume events.

## 3. Devnet / Local Deployment Rehearsal
1. `scripts/deploy_macos.sh` (or the appropriate VM variant) to start the stack
   with devnet RPC + paper trading.
2. Export minimal config overrides:
   ```bash
   export TRADING__paper_trading=true
   export TRADING__default_pair="SOL/USDC"
   ```
3. Run `scripts/monitor_live_trading.sh` to tail real-time metrics + failures.
4. Capture the resulting `logs/real_algotrader*.log` and stash under
   `reports/devnet/` for sign-off.

## 4. Hosted “core profile” Dry Run
1. Provision a clean VM, install Docker + compose.
2. `scripts/deploy_live.sh` → choose option 1 (paper) for the first pass.
3. Use the new `logs/smoke/` artifacts plus `services/orchestrator/logs/` to
   verify ContextLattice + telemetry ingestion.
4. Hit the dashboard (`memmcp-dashboard`) and confirm the SOL/USDC feed renders.

## 5. Observability & Memory
- Ensure `MEMMCP_ORCHESTRATOR_URL` is reachable: `curl $URL/health`.
- Use `src/monitoring/telemetry.rs`’s client via `scripts/monitor_live_trading.sh`
  to confirm trading metrics propagate (PnL, open positions, cache stats).
- Log review checklist:
  - `logs/smoke/paper_smoke_*.log`
  - `logs/algotrader.log`
  - `services/orchestrator/logs/*.log` (ContextLattice ingest)

## 6. Release Sign-off
- Update `docs/DEV_PROGRESS.md` and `docs/PHASE_5_PRODUCTION_READINESS.md` with
  the date, artifacts, and any circuit-breaker triggers observed.
- Capture screenshots (dashboard, CLI tail, ContextLattice inspector).
- Tag the release candidate once all above items are green.
