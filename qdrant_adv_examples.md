# Qdrant ADV (ef/exact) — Quick Examples

## 0) What do these knobs do?
- `hnsw_ef` (search-time): larger → higher recall, more CPU/latency.
- `exact: true`: full-scan KNN (slow), useful for ground-truth checks.
- Index-time settings (`m`, `ef_construct`, `quantization_config`) are applied on the **collection**; `hnsw_ef`/`exact` are **per-query**.

---

## 1) Direct Qdrant search (curl)
Substitute a real embedding for the `vector` field (or fetch one from your embedder).

```bash
curl -s ${QDRANT_URL_HOST:-http://127.0.0.1:6333}/collections/${COLLECTION_NAME:-memmcp_notes}/points/search   -H 'Content-Type: application/json' -d @- <<JSON | jq .
{
  "vector": [0.01, 0.02, 0.03, 0.04],
  "limit": 10,
  "params": { "hnsw_ef": 256, "exact": false }
}
JSON
```

**Filter example** (tags = ["design","mindsdb"]):
```bash
curl -s ${QDRANT_URL_HOST:-http://127.0.0.1:6333}/collections/${COLLECTION_NAME:-memmcp_notes}/points/search   -H 'Content-Type: application/json' -d @- <<'JSON' | jq .
{
  "vector": [0.01, 0.02, 0.03, 0.04],
  "limit": 5,
  "params": { "hnsw_ef": 192, "exact": false },
  "filter": {
    "must": [
      { "key": "tags", "match": { "any": ["design","mindsdb"] } }
    ]
  },
  "with_payload": true
}
JSON
```

---

## 2) Call via our MCP server (qdrant-find-adv)
The new FastMCP server exposes tool **`qdrant-find-adv`** over **Streamable HTTP** at `/mcp`.

### A) Letta — ask an agent to use the tool
```bash
# Pick an agent (by tag or name); here we pick the first with default planner/coder tags.
AID=$(curl -s http://localhost:8283/v1/agents/ | jq -r '.[] | select((.tags|index("default-planner")) or (.tags|index("default-coder"))) | .id' | head -n1)

# Send a message instructing the agent to use qdrant-find-adv with runtime knobs
curl -s -X POST "http://localhost:8283/v1/agents/$AID/messages/"   -H 'Content-Type: application/json' -d @- <<'JSON' | jq -r '.data.response.text'
{
  "sender": "user",
  "text": "Use the MCP tool qdrant-find-adv with hnsw_ef=256 and exact=false to find our best notes about 'sharding MindsDB'. Return top 5 with payloads."
}
JSON
```

### B) Claude CLI — register & confirm
```bash
# Register the Streamable HTTP server (adv) with Claude CLI
claude mcp add --transport http qdrant-adv http://127.0.0.1:${QDRANT_ADV_PORT:-8022}/mcp

# List tools to confirm it's available
claude tools list
```

---

## 3) Verify HNSW & Quantization on the collection
```bash
curl -s ${QDRANT_URL_HOST:-http://127.0.0.1:6333}/collections/${COLLECTION_NAME:-memmcp_notes} | jq '.result'
```

You should see (example):
```json
{
  "config": {
    "params": { "vectors": { "size": 384, "distance": "Cosine" }, "...": "..." },
    "hnsw_config": { "m": 16, "ef_construct": 256, "full_scan_threshold": 10000, "on_disk": false },
    "quantization_config": { "scalar": { "type": "int8", "quantile": 0.99, "always_ram": true } }
  }
}
```

---

## 4) Tips
- For broad recall: try `hnsw_ef=256..512`, `exact=false`.
- For correctness checks on small K: set `"exact": true` once to compare results.
- Keep your collection’s `size` equal to your embedder’s dimension (e.g., `all-MiniLM-L6-v2` → 384).
- Prefer **scalar** quantization for balanced accuracy/compression; use **PQ** only when memory pressure is severe.

## 5) Helpful docs
- Search params (`hnsw_ef`, `exact`): https://qdrant.tech/documentation/concepts/search/
- Optimization & quantization guides:
  - https://qdrant.tech/documentation/guides/optimize/
  - https://qdrant.tech/documentation/guides/quantization/
