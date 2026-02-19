#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# Load repo env so qdrant URL, collection, and snapshot path are consistent.
if [[ -f "$ROOT_DIR/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT_DIR/.env"
  set +a
fi

QDRANT_URL_HOST="${QDRANT_URL_HOST:-http://localhost:6333}"
ORCH_QDRANT_COLLECTION="${ORCH_QDRANT_COLLECTION:-memmcp_notes}"
MEMMCP_COLD_ROOT="${MEMMCP_COLD_ROOT:-./.data/cold/qdrant}"
QDRANT_HTTP_TIMEOUT_SECS="${QDRANT_HTTP_TIMEOUT_SECS:-300}"

echo ">> qdrant daily snapshot"
python3 scripts/qdrant_snapshot_prune.py \
  --qdrant-url "$QDRANT_URL_HOST" \
  --collection "$ORCH_QDRANT_COLLECTION" \
  --snapshot-dir "$MEMMCP_COLD_ROOT" \
  --timeout-secs "$QDRANT_HTTP_TIMEOUT_SECS" \
  --skip-prune
