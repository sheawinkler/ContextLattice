#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-${ORCH_BASE:-http://127.0.0.1:8075}}"
Q_URL="${QDRANT_URL:-http://127.0.0.1:6333}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"

TARGET_COLLECTION=""
VECTOR_SIZE="${VECTOR_SIZE:-768}"
DISTANCE="${DISTANCE:-Cosine}"
PROJECT="${PROJECT:-}"
LIMIT="${LIMIT:-120000}"
MAX_PENDING_JOBS="${MAX_PENDING_JOBS:-6000}"
CHECK_INTERVAL="${CHECK_INTERVAL:-200}"
FORCE_REQUEUE="${FORCE_REQUEUE:-false}"
RECREATE="${RECREATE:-false}"
CUTOVER="${CUTOVER:-true}"
RESTART_ORCHESTRATOR="${RESTART_ORCHESTRATOR:-true}"

usage() {
  cat <<'USAGE'
Usage: scripts/qdrant_migrate_from_mongo.sh --target <collection> [options]

Options:
  --target <name>          Target Qdrant collection name (required)
  --dim <n>                Vector size (default: 768)
  --distance <name>        Vector distance metric (default: Cosine)
  --project <name>         Optional single project scope
  --limit <n>              Mongo rows to scan (default: 120000)
  --max-pending <n>        Stop enqueueing if fanout outstanding jobs exceed this value
  --check-interval <n>     Queue pressure check interval while scanning
  --force-requeue          Force requeue already-succeeded fanout jobs
  --recreate               Delete target collection first if it exists
  --skip-cutover           Do not update .env QDRANT_COLLECTION / ORCH_EMBED_DIM
  --skip-restart           Do not restart orchestrator
  --orchestrator-url <u>   Override orchestrator URL
  --qdrant-url <u>         Override qdrant URL
USAGE
}

bool_is_true() {
  local v
  v="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
  [[ "$v" == "1" || "$v" == "true" || "$v" == "yes" || "$v" == "on" ]]
}

set_env_key() {
  local key="$1"
  local value="$2"
  local tmp_file
  tmp_file="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
  if [[ -f "$ENV_FILE" ]]; then
    awk -v k="$key" -v v="$value" '
      BEGIN { updated = 0 }
      $0 ~ ("^" k "=") {
        if (!updated) {
          print k "=" v
          updated = 1
        }
        next
      }
      { print }
      END {
        if (!updated) {
          print k "=" v
        }
      }
    ' "$ENV_FILE" > "$tmp_file"
  else
    printf '%s=%s\n' "$key" "$value" > "$tmp_file"
  fi
  mv "$tmp_file" "$ENV_FILE"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      [[ $# -ge 2 ]] || { echo "Missing value for --target" >&2; exit 2; }
      TARGET_COLLECTION="$2"
      shift 2
      ;;
    --dim)
      [[ $# -ge 2 ]] || { echo "Missing value for --dim" >&2; exit 2; }
      VECTOR_SIZE="$2"
      shift 2
      ;;
    --distance)
      [[ $# -ge 2 ]] || { echo "Missing value for --distance" >&2; exit 2; }
      DISTANCE="$2"
      shift 2
      ;;
    --project)
      [[ $# -ge 2 ]] || { echo "Missing value for --project" >&2; exit 2; }
      PROJECT="$2"
      shift 2
      ;;
    --limit)
      [[ $# -ge 2 ]] || { echo "Missing value for --limit" >&2; exit 2; }
      LIMIT="$2"
      shift 2
      ;;
    --max-pending)
      [[ $# -ge 2 ]] || { echo "Missing value for --max-pending" >&2; exit 2; }
      MAX_PENDING_JOBS="$2"
      shift 2
      ;;
    --check-interval)
      [[ $# -ge 2 ]] || { echo "Missing value for --check-interval" >&2; exit 2; }
      CHECK_INTERVAL="$2"
      shift 2
      ;;
    --force-requeue)
      FORCE_REQUEUE=true
      shift
      ;;
    --recreate)
      RECREATE=true
      shift
      ;;
    --skip-cutover)
      CUTOVER=false
      shift
      ;;
    --skip-restart)
      RESTART_ORCHESTRATOR=false
      shift
      ;;
    --orchestrator-url)
      [[ $# -ge 2 ]] || { echo "Missing value for --orchestrator-url" >&2; exit 2; }
      ORCH_URL="$2"
      shift 2
      ;;
    --qdrant-url)
      [[ $# -ge 2 ]] || { echo "Missing value for --qdrant-url" >&2; exit 2; }
      Q_URL="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

[[ -n "$TARGET_COLLECTION" ]] || { usage; exit 2; }

AUTH_HEADER=()
if [[ -n "$ORCH_API_KEY" ]]; then
  AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
fi

if bool_is_true "$RECREATE"; then
  echo ">> deleting existing collection (if present): ${TARGET_COLLECTION}"
  curl -fsS -X DELETE "$Q_URL/collections/$TARGET_COLLECTION" >/dev/null || true
fi

echo ">> creating/updating collection ${TARGET_COLLECTION} (dim=${VECTOR_SIZE}, distance=${DISTANCE})"
curl -fsS -X PUT "$Q_URL/collections/$TARGET_COLLECTION" \
  -H 'content-type: application/json' \
  -d "{\"vectors\":{\"size\":${VECTOR_SIZE},\"distance\":\"${DISTANCE}\"}}" | jq

if bool_is_true "$CUTOVER"; then
  echo ">> updating ${ENV_FILE}: QDRANT_COLLECTION=${TARGET_COLLECTION}, ORCH_EMBED_DIM=${VECTOR_SIZE}"
  set_env_key "QDRANT_COLLECTION" "$TARGET_COLLECTION"
  set_env_key "ORCH_EMBED_DIM" "$VECTOR_SIZE"
fi

if bool_is_true "$RESTART_ORCHESTRATOR"; then
  echo ">> rebuilding/restarting orchestrator"
  docker compose build memmcp-orchestrator >/dev/null
  docker compose up -d memmcp-orchestrator >/dev/null
  for i in {1..90}; do
    if curl -fsS "$ORCH_URL/health" "${AUTH_HEADER[@]}" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
fi

echo ">> enqueueing controlled mongo_raw -> qdrant backfill"
payload="$(jq -n \
  --arg project "$PROJECT" \
  --argjson limit "$LIMIT" \
  --arg qdrant_collection "$TARGET_COLLECTION" \
  --arg force_requeue "$FORCE_REQUEUE" \
  --argjson max_pending_jobs "$MAX_PENDING_JOBS" \
  --argjson check_interval "$CHECK_INTERVAL" '
  {
    project: (if $project == "" then null else $project end),
    limit: $limit,
    targets: ["qdrant"],
    qdrant_collection: $qdrant_collection,
    force_requeue: ((["1","true","yes","on"] | index($force_requeue | ascii_downcase)) != null),
    max_pending_jobs: $max_pending_jobs,
    check_interval: $check_interval
  }')"

curl -fsS "$ORCH_URL/maintenance/fanout/backfill/mongo" \
  -H 'content-type: application/json' \
  "${AUTH_HEADER[@]}" \
  -d "$payload" | jq
