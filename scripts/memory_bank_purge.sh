#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

RETENTION_DAYS="${MEMORY_BANK_RETENTION_DAYS:-90}"
DRY_RUN="${MEMORY_BANK_PURGE_DRY_RUN:-0}"
VERBOSE="${MEMORY_BANK_PURGE_VERBOSE:-0}"
CONTEXTLATTICE_DASHBOARD_URL="${CONTEXTLATTICE_DASHBOARD_URL:-}"
CONTEXTLATTICE_DASHBOARD_API_KEY="${CONTEXTLATTICE_DASHBOARD_API_KEY:-}"
if [[ -z "$CONTEXTLATTICE_DASHBOARD_URL" ]]; then
  CONTEXTLATTICE_DASHBOARD_URL="${CONTEXTLATTICE_DASHBOARD_URL:-}"
fi
if [[ -z "$CONTEXTLATTICE_DASHBOARD_API_KEY" ]]; then
  CONTEXTLATTICE_DASHBOARD_API_KEY="${CONTEXTLATTICE_DASHBOARD_API_KEY:-}"
fi

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
    find "$root_path" -type f -mtime +"$RETENTION_DAYS" -print 2>/dev/null || true
  else
    if [[ "$VERBOSE" == "1" ]]; then
      find "$root_path" -type f -mtime +"$RETENTION_DAYS" -print -delete 2>/dev/null || true
    else
      find "$root_path" -type f -mtime +"$RETENTION_DAYS" -delete 2>/dev/null || true
    fi
  fi
  exit 0
fi

echo "Memory bank root not found on host. Falling back to container purge."
purge_count=$(docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
  "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} | wc -l" | tr -d '[:space:]')
if [[ "$DRY_RUN" == "1" ]]; then
  docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
    "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} -print 2>/dev/null || true"
else
  if [[ "$VERBOSE" == "1" ]]; then
    docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
      "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} -print -delete 2>/dev/null || true"
  else
    docker compose -f docker-compose.yml exec -T memorymcp-http sh -lc \
      "find /data/memory-bank -type f -mtime +${RETENTION_DAYS} -delete 2>/dev/null || true"
  fi
fi

if [[ -n "$CONTEXTLATTICE_DASHBOARD_URL" && -n "$CONTEXTLATTICE_DASHBOARD_API_KEY" ]]; then
  curl -fsS "$CONTEXTLATTICE_DASHBOARD_URL/api/workspace/audit" \
    -H "content-type: application/json" \
    -H "x-api-key: $CONTEXTLATTICE_DASHBOARD_API_KEY" \
    -d "{\"action\":\"memory.purge\",\"targetType\":\"memory\",\"metadata\":{\"retentionDays\":$RETENTION_DAYS,\"files\":$purge_count,\"dryRun\":$DRY_RUN}}" >/dev/null || true
fi
