# Rust Hot-Path Store Spike

Goal: give the Solana trading engine a zero-latency persistence layer while keeping
memMCP as the long-term canonical memory. This spike lives in the
`~/Documents/Projects/algotraderv2_rust` repo and should prove:

1. **Fast writes** – telemetry and strategy metrics land locally in microseconds.
2. **Crash safety** – after a restart we can replay anything not yet flushed to
   memMCP.
3. **Clean hand-off** – a background exporter batches records back into memMCP
   (via the orchestrator) using the same project/kind tags we already defined.

## Option A: `sled`
- Embedded log-structured tree, no external dependency.
- Each namespace (`project`, `kind`) maps to a tree. Keys are ULIDs or
  monotonic timestamps. Values are JSON blobs.
- Sample setup (to be dropped into `src/monitoring/local_store.rs` in the
  trading repo):

```rust
use sled::{Config, Db};
use serde::{Deserialize, Serialize};

#[derive(Serialize, Deserialize)]
pub struct TelemetryRecord {
    pub project: String,
    pub kind: String,
    pub payload: serde_json::Value,
    pub captured_at: chrono::DateTime<chrono::Utc>,
}

pub struct LocalStore {
    db: Db,
}

impl LocalStore {
    pub fn open(path: &str) -> anyhow::Result<Self> {
        let db = Config::default().path(path).open()?;
        Ok(Self { db })
    }

    pub fn append(&self, record: &TelemetryRecord) -> anyhow::Result<()> {
        let tree = self.db.open_tree(&record.project)?;
        let key = format!("{}:{}", record.kind, record.captured_at.timestamp_nanos());
        let value = serde_json::to_vec(record)?;
        tree.insert(key, value)?;
        Ok(())
    }

    pub fn drain_batch(
        &self,
        project: &str,
        limit: usize,
    ) -> anyhow::Result<Vec<TelemetryRecord>> {
        let tree = self.db.open_tree(project)?;
        let mut out = Vec::with_capacity(limit);
        for item in tree.iter().take(limit) {
            let (key, value) = item?;
            let record: TelemetryRecord = serde_json::from_slice(&value)?;
            out.push(record);
            tree.remove(key)?;
        }
        Ok(out)
    }
}
```

## Option B: `sqlite` + `rusqlite`
- Pros: SQL queries, easier aggregations, plays nicely with analytical tooling.
- Cons: extra dependency + locking overhead, but still sub-millisecond writes.
- Schema sketch:

```sql
CREATE TABLE telemetry (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project TEXT NOT NULL,
  kind TEXT NOT NULL,
  captured_at TEXT NOT NULL,
  payload JSON NOT NULL,
  exported INTEGER DEFAULT 0
);
CREATE INDEX idx_telemetry_exported ON telemetry(exported, captured_at);
```

## Why not SurrealDB / GreptimeDB / RisingWave / LanceDB / TiKV / Neon / Tantivy?
| Engine | Fit for hot-path telemetry? | Notes |
| --- | --- | --- |
| **SurrealDB** | Partial | Brings its own SQL-like runtime and auth. Great for multi-tenant deployments, but embedded mode still requires a server process and the feature set overlaps with Mongo/Postgres we already run for memMCP. |
| **GreptimeDB** | No | Distributed time-series DB tuned for clusters + object storage. Tremendous for hosted analytics, overkill for a laptop hot-path cache. |
| **RisingWave** | No | Streaming SQL engine that shines when fed by Kafka/Pulsar. Not designed as an embedded write-ahead log. |
| **LanceDB** | No | Columnar + vector hybrid, similar to Qdrant. We already have Qdrant for vectors, and LanceDB does not buy us cheaper local persistence for scalar telemetry. |
| **TiKV** | No | Distributed transactional KV (the storage layer under TiDB). Requires a PD cluster; far outside the "lightweight local" requirement. |
| **Neon** | No | Serverless Postgres that depends on remote storage. Perfect for cloud, but not air-gapped/local-first. |
| **Tantivy** | Partial | Embedded Lucene-like inverted index. Great for full-text search, but it stores immutable segments—append/delete cycles for structured telemetry would require constant segment merges and provide little benefit vs. Qdrant. |

Conclusion: sled or sqlite remain the smallest-footprint tools that meet the goals (microsecond writes, no extra daemons, trivial crash recovery). We can still integrate the above engines later (e.g., GreptimeDB for hosted time-series analytics), but for the immediate hot-path cache we prioritize simplicity.

## Exporter Flow
1. Trading loop writes to local store synchronously.
2. Background task wakes every `EXPORT_INTERVAL_MS` (configurable) and grabs up
   to `EXPORT_BATCH_SIZE` records.
3. For each batch, POST to the memMCP orchestrator exactly as today. On success,
   mark the records as exported (delete from sled / set `exported=1` in sqlite).
4. If the exporter falls behind, memMCP still has its own HTTP queue, but the
   trading loop never blocks.

## Next Actions
- [ ] Add `local_store` module to `algotraderv2_rust` and gate it behind a
      feature flag (`local-store`).
- [ ] Record metrics (`local_store_queue_depth`) so the dashboard can alert when
      the backlog grows.
- [ ] Once validated, decide whether memMCP should ingest the sled/sqlite files
      directly for faster catch-up during maintenance windows.

Log all experiments using `project=sol_scaler` + `kind=local_store_spike` so
memMCP consumers can review findings.
