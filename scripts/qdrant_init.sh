#!/usr/bin/env bash
set -euo pipefail

Q_HOST="${QDRANT_HOST:-http://127.0.0.1}"
Q_PORT="${QDRANT_PORT:-6333}"
BASE="${Q_HOST}:${Q_PORT}"
COLL="${QDRANT_COLLECTION:-contextlattice_notes}"

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
create_code="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "${BASE}/collections/${COLL}"       -H 'Content-Type: application/json'       -d @- <<JSON || true
{
  "vectors": { "size": ${DIM}, "distance": "Cosine" },
  "hnsw_config": { "m": ${HNSW_M}, "ef_construct": ${HNSW_EF_CONSTRUCT}, "ef": ${HNSW_EF_SEARCH} }
}
JSON
)"
if [[ "$create_code" != "200" && "$create_code" != "201" && "$create_code" != "202" && "$create_code" != "409" ]]; then
  echo "[qdrant-init] warn: collection create returned HTTP ${create_code}" >&2
fi

# 2) Tune HNSW via collection update API
patch_code="$(curl -sS -o /dev/null -w '%{http_code}' -X PATCH "${BASE}/collections/${COLL}"       -H 'Content-Type: application/json'       -d @- <<JSON || true
{ "hnsw_config": { "m": ${HNSW_M}, "ef_construct": ${HNSW_EF_CONSTRUCT} } }
JSON
)"
if [[ "$patch_code" != "200" && "$patch_code" != "202" ]]; then
  echo "[qdrant-init] warn: hnsw patch returned HTTP ${patch_code}" >&2
fi

# 3) Optional: optimizer full_scan_threshold
if [ -n "${FULL_SCAN_THRESHOLD}" ] && [ "${FULL_SCAN_THRESHOLD}" != "0" ]; then
  optimizer_code="$(curl -sS -o /dev/null -w '%{http_code}' -X PATCH "${BASE}/collections/${COLL}"         -H 'Content-Type: application/json'         -d @- <<JSON || true
{ "optimizers_config": { "full_scan_threshold": ${FULL_SCAN_THRESHOLD} } }
JSON
)"
  if [[ "$optimizer_code" != "200" && "$optimizer_code" != "202" ]]; then
    echo "[qdrant-init] warn: optimizer patch returned HTTP ${optimizer_code}" >&2
  fi
fi

# 4) Optional: Product Quantization
if [ "${ENABLE_PQ}" = "true" ]; then
  pq_code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "${BASE}/collections/${COLL}/quantization"         -H 'Content-Type: application/json'         -d @- <<JSON || true
{ "product": { "compression": { "type": "x8" }, "always_ram": true, "codebook_size": ${PQ_CODEBOOK_SIZE} } }
JSON
)"
  if [[ "$pq_code" != "200" && "$pq_code" != "202" && "$pq_code" != "409" ]]; then
    echo "[qdrant-init] warn: quantization enable returned HTTP ${pq_code}" >&2
  fi
fi

echo "[qdrant-init] done."
