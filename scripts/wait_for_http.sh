#!/usr/bin/env bash
set -euo pipefail
URL="${1:?usage: wait_for_http.sh <url> [timeout_sec]}"
TIMEOUT="${2:-90}"
SLEEP=2
ELAPSED=0
while true; do
  if curl -sS --fail --max-time 5 --retry 0 "$URL" >/dev/null; then
    echo "[ok] $URL is responding"
    exit 0
  fi
  if (( ELAPSED >= TIMEOUT )); then
    echo "[fail] $URL did not become ready in ${TIMEOUT}s" >&2
    exit 1
  fi
  sleep "$SLEEP"
  ELAPSED=$((ELAPSED+SLEEP))
done
