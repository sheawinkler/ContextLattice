# Legacy Target Matrix

This table captures every legacy binary/example/test that still exists in
`src/bin`, `examples/`, or `tests/` and whether we plan to fix it now versus
archive for a later rebuild. Keeping the list explicit prevents forgotten
linters from blocking commits again.

| Target | Location | Current Status | Action | Notes |
| --- | --- | --- | --- | --- |
| `xavier_mode` | `src/bin/xavier_mode.rs` | Actively maintained | **Fix now** | Primary production binary; clippy clean. |
| `real_algotrader` | `src/bin/real_algotrader.rs` | Stale imports + metrics references | **Fix now** | Needed for legacy CLI; make lint-safe while wiring telemetry. |
| `unified_trader` | `src/bin/unified_trader.rs` | Requires minimal refresh | **Fix now** | Serves as thin wrapper around `UnifiedTradingEngine`. |
| `daily_ingest` | `src/bin/daily_ingest.rs` | Fails clippy (unused metrics module) | **Archive** | Replace with orchestrator ingestion once new pipelines land. |
| `walk_forward` | `src/bin/walk_forward.rs` | Clippy errors due to old metrics helpers | **Archive** | Documented in `docs/rebuild_todos.md`. |
| `xavier_minimal` | `src/bin/xavier_minimal.rs` | Stubbed after Jupiter refactor | **Archive** | Rebuild once new fast-path executor is ready. |
| `xavier_live` / `xavier_ultimate` | `src/bin/xavier_live.rs` etc. | Redundant vs. `xavier_mode` | **Archive** | Keep sources but exclude from default lint runs. |
| `force_emergency_sell` / `simple_bonk_sell` | `src/bin/` | Operational scripts | **Fix now** | Quick lint cleanup so we can run emergency ops without warnings. |
| `examples/parallel_mint_validation.rs` | `examples/` | Clippy failure (metrics module missing) | **Archive** | Will return as part of mint resolver rebuild. |
| `tests/parallel_mint_resolution_test.rs` | `tests/` | Integration test disabled | **Archive** | Re-enable alongside above example. |
| `tests/risk_sizer.rs` | `tests/` | Partially disabled | **Fix now** | Critical for Godmode config evaluation. |
| `tests/sidecar_roundtrip.rs` | `tests/` | Needs new FastAPI bridge mocks | **Fix now** | Ensures Perf+Sidecar integration stays working. |

Guidelines:
- **Fix now**: ensure `cargo clippy --all-targets --all-features` no longer
  fails because of the target. Update dependencies/tests immediately.
- **Archive**: gate them behind the new `legacy_targets` feature (added to
  `Cargo.toml`) or move them under `src/bin/_tests_archive/` so our default
  lint/test surface stays lean while keeping the source for future rebuilds
  (tracked via `docs/rebuild_todos.md`). Use `cargo clippy --features legacy_targets`
  when you want to lint the archived set explicitly.
- Update this file whenever a target changes categories so future agents know
  why something is temporarily ignored.
