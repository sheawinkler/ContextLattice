#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -f .env ]]; then
  set -a
  # shellcheck source=/dev/null
  source .env
  set +a
fi

endpoint="${QDRANT_CLUSTER_ENDPOINT:-}"
api_key="${QDRANT_API_KEY:-}"
if [[ -z "$endpoint" ]]; then
  endpoint="$(printenv 'QDRANT-CLUSTER-ENDPOINT' || true)"
fi
if [[ -z "$api_key" ]]; then
  api_key="$(printenv 'QDRANT-API-KEY' || true)"
fi
endpoint="${endpoint%/}"
grpc_port="${QDRANT_CLOUD_GRPC_PORT:-6334}"

if [[ -z "$endpoint" || -z "$api_key" ]]; then
  echo "Missing cloud vars. Set QDRANT_CLUSTER_ENDPOINT and QDRANT_API_KEY (or QDRANT-CLUSTER-ENDPOINT / QDRANT-API-KEY)." >&2
  exit 2
fi

echo "== Qdrant Cloud HTTP check =="
http_count="$(curl -fsS -H "api-key: $api_key" "$endpoint/collections" | jq '.result.collections | length')"
echo "http_collections=$http_count"

echo "== Qdrant Cloud gRPC check (via memmcp-orchestrator container) =="
unset DOCKER_API_VERSION || true
docker compose exec -T \
  -e QDRANT_CLUSTER_ENDPOINT="$endpoint" \
  -e QDRANT_API_KEY="$api_key" \
  -e QDRANT_CLOUD_GRPC_PORT="$grpc_port" \
  memmcp-orchestrator python - <<'PY'
import json
import os
from qdrant_client import QdrantClient

endpoint = os.environ["QDRANT_CLUSTER_ENDPOINT"]
api_key = os.environ["QDRANT_API_KEY"]
grpc_port = int(os.getenv("QDRANT_CLOUD_GRPC_PORT", "6334"))

client = QdrantClient(url=endpoint, api_key=api_key, prefer_grpc=True, grpc_port=grpc_port, timeout=12)
count = len(client.get_collections().collections)
print(json.dumps({"grpc_ok": True, "grpc_collections": count, "grpc_port": grpc_port}))
PY

echo "Qdrant cloud connectivity: OK"
