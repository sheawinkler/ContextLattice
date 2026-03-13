#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT_DIR/logs"
mkdir -p "$LOG_DIR"

WINDOW_MINUTES="${DOCKER_FS_WATCHDOG_WINDOW_MINUTES:-20}"
PATTERN="${DOCKER_FS_WATCHDOG_PATTERN:-service fs failed: injecting event blocked for 60s}"
BACKEND_LOG_GLOB="${DOCKER_FS_WATCHDOG_LOG_GLOB:-$HOME/Library/Containers/com.docker.docker/Data/log/host/com.docker.backend.log*}"
STATE_FILE="${DOCKER_FS_WATCHDOG_STATE_FILE:-$ROOT_DIR/tmp/docker-fs-watchdog.state}"
OUT_LOG="$LOG_DIR/docker-fs-watchdog.log"

mkdir -p "$(dirname "$STATE_FILE")"

if ! command -v rg >/dev/null 2>&1; then
  echo "ripgrep (rg) is required" >&2
  exit 2
fi

if ! ls $BACKEND_LOG_GLOB >/dev/null 2>&1; then
  echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] no docker backend logs found for glob: $BACKEND_LOG_GLOB" >> "$OUT_LOG"
  exit 0
fi

now_epoch="$(date -u +%s)"
window_secs="$((WINDOW_MINUTES * 60))"
cutoff_epoch="$((now_epoch - window_secs))"

last_seen_epoch=0
if [[ -f "$STATE_FILE" ]]; then
  read -r last_seen_epoch < "$STATE_FILE" || true
fi
if [[ -z "${last_seen_epoch:-}" ]] || ! [[ "$last_seen_epoch" =~ ^[0-9]+$ ]]; then
  last_seen_epoch=0
fi

newest_epoch="$last_seen_epoch"
hit=0

while IFS= read -r line; do
  ts="$(echo "$line" | sed -E 's/^\[([0-9TZ:\.-]+)\].*$/\1/')"
  if [[ "$ts" == "$line" ]]; then
    continue
  fi

  if ! epoch="$(date -j -u -f '%Y-%m-%dT%H:%M:%S.%NZ' "$ts" +%s 2>/dev/null)"; then
    continue
  fi

  if (( epoch > newest_epoch )); then
    newest_epoch="$epoch"
  fi

  if (( epoch < cutoff_epoch )); then
    continue
  fi

  if (( epoch <= last_seen_epoch )); then
    continue
  fi

  hit=1
  echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] docker_fs_watchdog alert: $line" >> "$OUT_LOG"
done < <(rg -n "$PATTERN" $BACKEND_LOG_GLOB -S | sed -E 's/^[^:]+:[0-9]+://')

echo "$newest_epoch" > "$STATE_FILE"

if (( hit == 1 )); then
  echo "docker fs watchdog detected fresh fs-injector stalls in the last ${WINDOW_MINUTES}m; see $OUT_LOG"
  exit 1
fi

echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] docker_fs_watchdog ok (window=${WINDOW_MINUTES}m)" >> "$OUT_LOG"
