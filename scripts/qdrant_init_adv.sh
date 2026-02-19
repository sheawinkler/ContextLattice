#!/usr/bin/env bash
set -euo pipefail

QURL="${QDRANT_URL:-http://localhost:6333}"
COLL="${COLLECTION_NAME:-memmcp_notes}"
DIM="${QDRANT_DIM:-384}"
DIST="${QDRANT_DISTANCE:-Cosine}"

HNSW_M="${QDRANT_HNSW_M:-16}"
HNSW_EF_CONSTRUCT="${QDRANT_HNSW_EF_CONSTRUCT:-256}"

QUANT="${QDRANT_QUANTIZATION:-scalar}"   # scalar | binary | product | none
QUANT_JSON="\"Disabled\""

case "$QUANT" in
  scalar)
    QUANT_JSON="{\"scalar\":{\"type\":\"int8\",\"quantile\":${QDRANT_QUANTILE:-0.99},\"always_ram\":${QDRANT_ALWAYS_RAM:-true}}}"
    ;;
  binary)
    # For 1.5/2-bit add: "encoding":"one_and_half_bits" | "two_bits"
    QUANT_JSON="{\"binary\":{\"always_ram\":${QDRANT_ALWAYS_RAM:-true}}}"
    ;;
  product)
    # compression can be x4, x8, x16, etc.
    QUANT_JSON="{\"product\":{\"compression\":\"x16\",\"always_ram\":${QDRANT_ALWAYS_RAM:-true}}}"
    ;;
  none) QUANT_JSON="\"Disabled\"" ;;
esac

# Try create (PUT). If exists (409), PATCH to update configs.
status=$(curl -s -o /dev/null -w "%{http_code}" -X PUT "$QURL/collections/$COLL" \
  -H 'Content-Type: application/json' \
  --data-raw "{
    \"vectors\": {\"size\": $DIM, \"distance\": \"$DIST\"},
    \"hnsw_config\": {\"m\": $HNSW_M, \"ef_construct\": $HNSW_EF_CONSTRUCT},
    \"quantization_config\": $QUANT_JSON
  }" || true)

if [ "$status" = "409" ]; then
  curl -sS -X PATCH "$QURL/collections/$COLL" \
    -H 'Content-Type: application/json' \
    --data-raw "{\"hnsw_config\": {\"m\": $HNSW_M, \"ef_construct\": $HNSW_EF_CONSTRUCT}, \"quantization_config\": $QUANT_JSON}" >/dev/null
  echo "Patched existing collection '$COLL' with HNSW & quantization"
else
  echo "Created collection '$COLL' (status $status)"
fi
