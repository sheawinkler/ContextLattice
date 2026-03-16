# ContextLattice V3 Roadmap (Issues #68-#72)

Date: 2026-03-05

## V3 Objective
V3 turns ContextLattice from a fast memory layer into an adaptive **context efficacy system**:
- faster writes and reads under load,
- deeper retrieval without user-facing timeouts,
- higher recall precision for agent decisions,
- broader runner interoperability with safer fallbacks.

The point of V3 is not only speed. The objective is **more correct agent outcomes per request** at lower latency and lower operational risk.

## Efficacy Story (What Improves for the Application)
Today in v2, the runtime is already ~5x faster than the legacy path on the benchmarked search route. V3 extends that gain into deep retrieval and end-to-end task success:

1. Faster indexing + retrieval adapters reduce p95/p99 tail latency.
2. Better code/context-aware reranking improves recall quality at decision time.
3. Better task/runner contracts reduce orchestration friction and retries.
4. ANE sidecar + OSS runner support expands local inference throughput (M-series path) while preserving safe fallback.

### Expected efficacy chain
```text
Write speed up
  -> queue pressure down
  -> sink freshness up
  -> retrieval context freshness up
  -> recall quality up (higher hit@k / fewer misses)
  -> deep-read timeout rate down
  -> agent task completion reliability up
  -> user trust + application throughput up
```

## Roadmap Graph (Grouped)
```text
V3 Program (Issues #68-#72)

                    +-------------------------------+
                    |   V3 Objective: Context       |
                    |   Efficacy at Scale           |
                    +---------------+---------------+
                                    |
       +----------------------------+-----------------------------+
       |                            |                             |
+------+-------+            +-------+-------+             +-------+-------+
| Track A      |            | Track B       |             | Track C       |
| Performance  |            | Recall/Memory |             | Agent Surface |
| (#69 + #72)  |            | (#70 + #72)   |             | (#68 + #71)   |
+------+-------+            +-------+-------+             +-------+-------+
       |                            |                             |
       v                            v                             v
 Adapter spikes              Context enrichment            Runner contracts
 ANN + indexing              + reranking upgrades          + events + sidecar
       |                            |                             |
       +----------------------------+-----------------------------+
                                    |
                                    v
                         Unified test gauntlet
                  (security -> benchmark -> recall -> canary)
                                    |
                                    v
                           V3 staged production cutover
```

## Program Tracks and Integration Scope

## Track A: Performance and Deep Read Stability
Primary issues: [#69](https://github.com/sheawinkler/http-context-and-memory-orchestrator/issues/69), [#72](https://github.com/sheawinkler/http-context-and-memory-orchestrator/issues/72)

### Repos and additions
- `bosun-ai/swiftide`
- `Anush008/fastembed-rs`
- `StarlightSearch/EmbedAnything`
- `alibaba/zvec`
- `matte1782/edgevec`
- `qdrant/qdrant` (reference-only tuning source; no new core integration path)
- Additional pool from #72 (selected by benchmark gate):
  - `devflowinc/trieve`, `HelixDB/helix-db`, `chroma-core/chroma`, `lancedb/lancedb`,
  - `quickwit-oss/tantivy`, `quickwit-oss/quickwit`, `RediSearch/RediSearch`,
  - `meilisearch/meilisearch`, `infiniflow/infinity`, `milvus-io/milvus`, `unum-cloud/usearch`, `qdrant/rust-client`
  - `evilsocket/mini-rag`, `HKUDS/LightRAG`

### Integration work
- Add feature-flagged adapter boundaries:
  - `EmbeddingProviderAdapter`
  - `ANNQueryAdapter`
- Build candidate adapters as opt-in paths only.
- Extend deep-read retrieval strategy for lower tail latency (p95/p99) with no quality regression.

### Test plan
- Benchmark matrix per candidate (`p50`, `p95`, `p99`, throughput, timeout rate).
- Deep-read stress suite (slow sources + rerank + larger context windows).
- Regression against current Rust+Go default path.

### Exit criteria
- >=20% p95 improvement on at least one candidate path.
- Timeout/error non-regression (`<= baseline + 0.5%`).
- Full env-flag rollback validated.

## Track B: Recall Quality and Memory Semantics
Primary issues: [#70](https://github.com/sheawinkler/http-context-and-memory-orchestrator/issues/70), [#72](https://github.com/sheawinkler/http-context-and-memory-orchestrator/issues/72)

### Repos and additions
- `cogniplex/codemem`
- `Jakedismo/codegraph-rust`
- `zircote/subcog`
- `varun29ankuS/shodh-memory`
- `redis/mcp-redis`
- `mindsdb/mindsdb`
- Additional pool from #72:
  - `automataIA/graphrag-rs`, `mem0ai/mem0`, `ksaritek/rust-local-rag`, `Krira-Labs/krira-chunker`, `nubskr/satoriDB`

### Integration work
- Add code-context enrichment on writes (symbols/files/edges).
- Add code-aware reranking signals:
  - symbol overlap,
  - file-path proximity,
  - edit recency.
- Add optional Redis MCP mirror mode for cache-backed recall.
- Add source-quality telemetry by workload class.

### Test plan
- Recall evaluation suites (`saved`, code-heavy benchmark set).
- Hallucination/cross-file leakage regressions.
- Precision/coverage comparison across sources (`qdrant`, `topic_rollups`, `mindsdb`, `letta`, `memory_bank`).

### Exit criteria
- Recall `@k` lift on code-heavy test set.
- No increase in leakage/hallucination regression cases.
- Feature flags can disable all additions without behavior break.

## Track C: Agent Surface, Runners, and Compute Backends
Primary issues: [#68](https://github.com/sheawinkler/http-context-and-memory-orchestrator/issues/68), [#71](https://github.com/sheawinkler/http-context-and-memory-orchestrator/issues/71)

### Repos and additions
- `anomalyco/opencode`
- `openclaw/openclaw`
- `nearai/ironclaw`
- `unbrowse-ai/unbrowse`
- `modelcontextprotocol/registry`
- `BerriAI/litellm`
- OSS runner targets from #68:
  - OpenCode
  - Goose
  - Eliza
- ANE sidecar path (M-series macOS only) from #68

### Integration work
- Standardize task payload/result contracts across runners.
- Add strict enqueue/result schema validation.
- Add richer task status event lifecycle (`queued`, `claimed`, `partial`, `succeeded`, `failed`).
- Add optional browser-context ingest lane.
- Add sidecar provider routing:
  - primary: ANE custom API sidecar,
  - fallback: standard Ollama/local path.

### Test plan
- End-to-end runner claim/execute/report loop tests.
- Sidecar health/timeout/fallback tests.
- Security-policy tests for secret redaction/blocking on command surfaces.

### Exit criteria
- At least one OSS runner fully integrated.
- ANE sidecar path benchmarked and gated (M-series only).
- Automatic fallback works on sidecar failure.

## Unified Security and Validation Gates
Applies to all tracks and all repos from #68-#72:

1. Pin exact commit SHA before integration.
2. Sandbox all builds/tests; no privileged install scripts.
3. Block `curl|bash` in CI integration paths.
4. Restrict outbound networking in adapter tests.
5. Run dependency/license + secret scan gates.

## Integration Waves (Proposed)

### Wave 1 (2026-03-09 to 2026-03-22)
- Build benchmark harness extensions.
- Implement adapter interfaces and empty feature flags.
- Land baseline security gates.

### Wave 2 (2026-03-23 to 2026-04-12)
- Execute Track A candidate spikes.
- Execute Track B enrichment/rerank spikes.
- Publish benchmark and recall artifacts.

### Wave 3 (2026-04-13 to 2026-05-03)
- Execute Track C runner + ANE sidecar canary integration.
- Validate fallback and policy tests.

### Wave 4 (2026-05-04 to 2026-05-24)
- Cross-track hardening.
- Canary rollout and rollback validation.
- Final keep/drop decisions for #72 candidate pool.

## V3 Definition of Done
- Performance: measurable p95/p99 gain with stable timeout rate.
- Recall quality: measurable recall improvement without regression in factual precision.
- Agent efficacy: higher completed-task rate with fewer retries/timeouts.
- Safety: all new paths behind feature flags with tested rollback.
- Documentation: implementation notes, benchmark artifacts, and rollout runbooks published.
