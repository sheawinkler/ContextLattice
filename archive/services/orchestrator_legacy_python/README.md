# Orchestrator Legacy Python Archive

This directory preserves the historical Python orchestrator runtime for reference, tooling compatibility, and regression tests.

Current production/runtime ownership is Go + Rust via:

- `services/gateway-go`
- `services/orchestrator-go`

Notes:

- `services/orchestrator` is a compatibility symlink to this archive path.
- New runtime features should not be added here.
- Keep only tooling/tests compatibility updates in this tree.
