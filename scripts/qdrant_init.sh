#!/usr/bin/env bash
set -euo pipefail

Q_HOST="${QDRANT_HOST:-http://127.0.0.1}"
Q_PORT="${QDRANT_PORT:-6333}"
BASE="${Q_HOST}:${Q_PORT}"
COLL="${QDRANT_COLLECTION:-memmcp_notes}"

DIM="${EMBED_DIM:-384}"
HNSW_M="${HNSW_M:-16}"
HNSW_EF_CONSTRUCT="${HNSW_EF_CONSTRUCT:-256}"
HNSW_EF_SEARCH="${HNSW_EF_SEARCH:-64}"
ENABLE_PQ="${QDRANT_ENABLE_PQ:-false}"
PQ_CODEBOOK_SIZE="${QDRANT_PQ_CODEBOOK_SIZE:-256}"
FULL_SCAN_THRESHOLD="${FULL_SCAN_THRESHOLD:-0}"

echo "[qdrant-init] Waiting for Qdrant at ${BASE} ..."
for i in {1..60}; do
  if curl -fsS "${BASE}/collections" >/dev/null 2>&1; then break; fi
  sleep 1
done
echo "ok"

# 1) Create collection if missing
curl -fsS -X PUT "${BASE}/collections/${COLL}"       -H 'Content-Type: application/json'       -d @- <<JSON >/dev/null || true
{
  "vectors": { "size": ${DIM}, "distance": "Cosine" },
  "hnsw_config": { "m": ${HNSW_M}, "ef_construct": ${HNSW_EF_CONSTRUCT}, "ef": ${HNSW_EF_SEARCH} }
}
JSON

# 2) Tune HNSW
curl -fsS -X PATCH "${BASE}/collections/${COLL}/hnsw"       -H 'Content-Type: application/json'       -d @- <<JSON >/dev/null || true
{ "m": ${HNSW_M}, "ef_construct": ${HNSW_EF_CONSTRUCT}, "ef": ${HNSW_EF_SEARCH} }
JSON

# 3) Optional: optimizer full_scan_threshold
if [ -n "${FULL_SCAN_THRESHOLD}" ] && [ "${FULL_SCAN_THRESHOLD}" != "0" ]; then
  curl -fsS -X PATCH "${BASE}/collections/${COLL}"         -H 'Content-Type: application/json'         -d @- <<JSON >/dev/null || true
{ "optimizers_config": { "full_scan_threshold": ${FULL_SCAN_THRESHOLD} } }
JSON
fi

# 4) Optional: Product Quantization
if [ "${ENABLE_PQ}" = "true" ]; then
  curl -fsS -X POST "${BASE}/collections/${COLL}/quantization"         -H 'Content-Type: application/json'         -d @- <<JSON >/dev/null || true
{ "product": { "compression": { "type": "x8" }, "always_ram": true, "codebook_size": ${PQ_CODEBOOK_SIZE} } }
JSON
fi

echo "[qdrant-init] done."
