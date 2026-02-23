# Sol Scaling Restoration Plan

## Objective
Ship an end-to-end Solana trading workflow that:
1. Runs deterministically on devnet for integration testing (engine + sidecar + ContextLattice).
2. Shares a single execution core between `xavier_mode` (godmode profile loops) and the
   more general CLI binaries (`real_algotrader`, `unified_trader`).
3. Streams telemetry + strategy metrics into ContextLattice so we can review and log every
   decision before demoing live mainnet trades.

## Surface Map (current state)
| Area | Current behavior | Key gaps |
| --- | --- | --- |
| `xavier_mode` | Monolithic file (~4k LOC). Handles godmode profiles, risk manager, telemetry, token cache, strategy snapshots. | Duplicates logic that `UnifiedTradingEngine` already implements (risk sizing, execution loops). Only binary emitting telemetry. |
| `real_algotrader` | Wraps `UnifiedTradingEngine` with real wallet loading, but no telemetry, no godmode awareness, separate config structs. | Drifted config vs. Xavier (hard-coded multipliers). Missing ContextLattice logging + sidecar bridge. |
| `unified_trader` / `run_strategy` | Thin shells around the engine. Useful for devnet CLI but benchmarked against older config layouts. | Need to ensure they call the same shared builder + telemetry hook. |
| Legacy bins/tests (`daily_ingest`, `walk_forward`, `xavier_minimal`, mint race example/test) | Disabled or broken. | Now gated behind `legacy_targets`; rebuild later per `docs/rebuild_todos.md`. |
| Shared modules | `engine/unified` exposes `UnifiedTradingEngine`, `RealPortfolioManager`, config builder. `monitoring/telemetry.rs` now supports sled hot-path. | We still lack a unifying “app harness” module so multiple bins can compose the same loop without duplicating code. |

## Overlaps & Consolidation Targets
1. **Config stack**
   - `xavier_mode` loads TOML/CLI -> `GodmodeProfile` -> inline risk params.
   - `UnifiedTradingEngine` expects `UnifiedTradingConfig`.
   - **Action:** build a `config::app_profile::{AppRunConfig, Mode}` module that
     converts godmode TOML or legacy CLI flags into a unified struct consumed by
     both binaries. Include risk/timing/environment toggles.

2. **Telemetry + ContextLattice**
   - Only Xavier instantiates `TelemetryClient` and strategy snapshots.
   - Real/unified binaries should opt-in via the same `TelemetryHarness` helper
     (wrapping `TelemetryClient`, `GodmodeProfile`, `LocalStore`).

3. **Execution loop**
   - Xavier spins its own async loop; `UnifiedTradingEngine::start()` provides similar functionality.
   - Need a shared runner (e.g., `app::runner::run_engine(config, wallet, telemetry)`)
     so CLI front-ends only manage I/O (arg parsing, wallet selection).

4. **Sidecar + FastAPI bridge**
   - Xavier integrates with the sidecar client (ContextLattice + FastAPI stub) for model guidance.
   - Real/unified binaries currently aren’t aware of it; they run purely deterministic strategies.
   - Consolidate the sidecar client into the shared runner so we can toggle it via config.

5. **Portfolio & risk modules**
   - Xavier implements `RiskManager` inline; unified engine has `RiskManager` module already.
   - We should delete drifted copies by:
     1. Moving any Xavier-only heuristics (token cache, mint DB) into modules under
        `engine/unified` or `features/`.
     2. Re-export them for both binaries.

## Action Plan (high-level)
1. **Consolidate config + runners (WIP / partially done)**
   - Add `app/harness.rs` (or similar) exposing `build_runtime(AppRunConfig)` that
     returns `(UnifiedTradingEngine, TelemetryHarness, SidecarClient?)`. ✅
   - Migrate `xavier_mode` and `real_algotrader` to use it incrementally. `xavier_mode`
     now rides entirely on the shared runtime + telemetry harness, bringing its
     file size below 200 LOC and making future extensions tractable. `real_algotrader`
     already adopted the same harness earlier.

2. **Telemetry everywhere**
   - Introduce `TelemetryHarness` to wrap `TelemetryClient` calls and share code
     for strategy/trading snapshots.
   - Update `real_algotrader`, `unified_trader`, tests to emit metrics and store
     spool depth.

3. **Devnet integration suite**
   - Create `tests/devnet_smoke.rs` that:
     1. Spins up the FastAPI sidecar stub.
     2. Runs the shared runner in paper/devnet mode for N cycles.
     3. Asserts telemetry hits the orchestrator (mock) and ContextLattice spool drains.

4. **Live demo readiness**
   - After devnet pass, add CLI commands to start/stop live trading, record run IDs,
     and stream updates to the Next.js dashboard for operator visibility.

All significant milestones (plan revisions, merges, test runs) should be logged in
ContextLattice under `project=sol_scaler` with `kind=app_plan`.
