#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

RETENTION_DAYS="${MEMORY_BANK_RETENTION_DAYS:-90}"
DRY_RUN="${MEMORY_BANK_PURGE_DRY_RUN:-0}"
VERBOSE="${MEMORY_BANK_PURGE_VERBOSE:-0}"
MEMMCP_DASHBOARD_URL="${MEMMCP_DASHBOARD_URL:-}"
MEMMCP_DASHBOARD_API_KEY="${MEMMCP_DASHBOARD_API_KEY:-}"

resolve_memory_root() {
  if [[ -n "${MEMORY_BANK_PATH:-}" ]]; then
    echo "$MEMORY_BANK_PATH"
    return
  fi
  if [[ -n "${MEMORY_BANK_DATA:-}" && "${MEMORY_BANK_DATA}" = /* ]]; then
    echo "${MEMORY_BANK_DATA}/memory-bank"
    return
  fi
  if [[ -d "$ROOT_DIR/data/memory-bank" ]]; then
    echo "$ROOT_DIR/data/memory-bank"
    return
  fi
  echo ""
}

root_path="$(resolve_memory_root)"
purge_count=0

if [[ -n "$root_path" && -d "$root_path" ]]; then
  echo "Purging memory bank at $root_path (>${RETENTION_DAYS} days)"
  purge_count=$(find "$root_path" -type f -mtime +"$RETENTION_DAYS" | wc -l | awk '{print $1}')
  if [[ "$DRY_RUN" == "1" ]]; then
    find "$root_path" -type f -mtime +"$RETENTION_DAYS" -print
  else
    if [[ "$VERBOSE" == "1" ]]; then
      find "$root_path" -type f -mtime +"$RETENTION_DAYS" -print -delete
    else
      find "$root_path" -type f -mtime +"$RETENTION_DAYS" -delete
    fi
  fi
  exit 0
fi

echo "Memory bank root not found on host. Falling back to container purge."
purge_count=$(docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
  "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} | wc -l" | tr -d '[:space:]')
if [[ "$DRY_RUN" == "1" ]]; then
  docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
    "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} -print"
else
  if [[ "$VERBOSE" == "1" ]]; then
    docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
      "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} -print -delete"
  else
    docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
      "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} -delete"
  fi
fi

if [[ -n "$MEMMCP_DASHBOARD_URL" && -n "$MEMMCP_DASHBOARD_API_KEY" ]]; then
  curl -fsS "$MEMMCP_DASHBOARD_URL/api/workspace/audit" \
    -H "content-type: application/json" \
    -H "x-api-key: $MEMMCP_DASHBOARD_API_KEY" \
    -d "{\"action\":\"memory.purge\",\"targetType\":\"memory\",\"metadata\":{\"retentionDays\":$RETENTION_DAYS,\"files\":$purge_count,\"dryRun\":$DRY_RUN}}" >/dev/null || true
fi
