# Solana Hyper-Scaling Plan

## Mission
Build a memory-driven trading application that compounds capital from 2 SOL to 20,000+ SOL by:
- Proactively generating alpha (we move markets, not react to them).
- Logging every signal/decision into the shared ContextLattice service so all agents share context.
- Surfacing live telemetry and strategic insights through the dashboard for human + agent operators.

## Architecture Overview
1. **Trading Runtime (algotraderv2_rust)**
   - Unified engine (Xavier Mode) with godmode profiles, self-evolving strategies, and ContextLattice telemetry hooks.
   - Sidecar/LLM guidance (via FastAPI bridge) for scenario assessments.
   - Risk + portfolio managers tuned for aggressive compounding (kelly sizing, diversification, drawdown defenses).
2. **Memory & Orchestrator (mem_mcp_lobehub/services/orchestrator)**
   - HTTP entrypoint for `/memory/write`, `/telemetry/metrics`, `/telemetry/trading`, `/telemetry/trading/history`.
   - Persists snapshots to disk + Qdrant, fans out to Langfuse, exposes health for operators.
3. **Operator Dashboard (memmcp-dashboard)**
   - Next.js interface pulling orchestrator telemetry, showing queue stats, trading metrics, sol targets.
   - Planned panels: compounding progress, godmode profile in play, strategy heatmap, ContextLattice log browser.
4. **Shared Memory Consumers**
   - Trae agent, Windsurf, Cursor, etc. read/write via ContextLattice to maintain continuous context.

## Data Flow
```
Trading Engine -> ContextLattice logger -> Orchestrator -> (Langfuse, Qdrant, Disk)
                                    |
                                    +-> Dashboard (Polling APIs)
                                    +-> External agents (via HTTP/MCP)
```
- Each trading cycle emits telemetry batches + trading snapshots (positions, UPNL, SOL progress).
- Sidecar guidance logs (requests/responses) are written to `memory_bank` for downstream audits.
- Dashboard fetches `/telemetry/metrics` (queue stats) + `/telemetry/trading` and `/history` for operators.

## Scaling Strategy (2 SOL -> 20,000+ SOL)
1. **Foundation (2-20 SOL)**
   - Meme momentum focus, rapid rotations (anti-DCA engine already in Xavier Mode).
   - Tight telemetry to catch slippage + stuck queues.
2. **Acceleration (20-2,000 SOL)**
   - Activate self-evolving strategy engine with capital buckets.
   - Use ContextLattice to store per-strategy performance windows for auto-rotation.
   - Dashboard shows strategy-level PnL, capital allocation.
3. **Institutional Scale (2,000-20,000 SOL+)**
   - Distributed execution network (multi-wallet), cross-chain arb modules.
   - Memory service synchronizes fills/risks across agents to avoid overlap.
   - Dashboard exposes risk budget, exposure map, aggregated telemetry.

## Memory Integration Checklist
- [x] Godmode profile logs (`src/monitoring/telemetry.rs`).
- [x] Trading metric snapshots -> `/telemetry/trading` with persistence.
- [ ] Strategy performance digests -> ContextLattice projects (TODO for scaling tiers).
- [ ] Dashboard view for ContextLattice trajectories (link to memory explorer).

## Dashboard Enhancements Roadmap
1. **Telemetry Baseline** (DONE)
   - Queue stats + trading snapshot.
2. **Compounding Tracker** (NEXT)
   - Target vs current SOL, daily/weekly deltas, progress sparkline.
3. **Strategy Heatmap**
   - Pull per-strategy metrics (to implement in orchestrator) and show win rate, capital allocated, mem logs.
4. **Memory Browser**
   - Query ContextLattice for latest trajectories/godmode logs; display links for operators.
5. **Action Pad**
   - Kick off tasks (e.g., rebalance, strategy switch) using Trae agent with mem context.

## Implementation Phases
1. **Telemetric Hardening**
   - Ensure all trading modules log to ContextLattice + persist backups.
   - Add retry/error surfacing (already partly done in telemetry client).
2. **Dashboard Expansion**
   - Build Compounding panel + History list (depends on orchestrator history endpoint).
   - Add mem log browser and risk map.
3. **Strategy Memory Loop**
   - Write per-strategy performance snapshots into memory; orchestrator exposes `/strategies` endpoint.
   - Dashboard visualizes best/worst performers to guide capital allocation.
4. **Execution Scaling**
   - Connect distributed execution metrics to orchestrator (nodes, latency, wallet balances).
   - Add alerts (Langfuse + mem logs) for drawdowns.
5. **QA & Automation**
   - CLI tests for orchestrator APIs, Next.js route tests, scripted smoke for trading-to-memory path.

## Next Actions
1. Ship dashboard compounding & history panels.
2. Extend orchestrator API with strategy performance and mem log summaries.
3. Wire trading engine to emit strategy digests + capital tier data.
4. Build automated tests (Rust + Next.js + FastAPI) covering these flows.
