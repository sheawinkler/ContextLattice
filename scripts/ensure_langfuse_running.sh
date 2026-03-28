#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
LANGFUSE_ENSURE_ENABLED="${LANGFUSE_ENSURE_ENABLED:-1}"
LANGFUSE_ENSURE_TIMEOUT="${LANGFUSE_ENSURE_TIMEOUT:-180}"
LANGFUSE_ENSURE_INTERVAL="${LANGFUSE_ENSURE_INTERVAL:-2}"
LANGFUSE_HEALTH_PATH="${LANGFUSE_HEALTH_PATH:-/api/public/health}"
PROFILE_HINT="${PROFILES:-${COMPOSE_PROFILES:-}}"

log() {
  echo "[langfuse-ensure] $*"
}

get_env_key() {
  local key="$1"
  if [[ -f "$ENV_FILE" ]]; then
    awk -F= -v k="$key" '$1 == k {print substr($0, index($0, "=") + 1)}' "$ENV_FILE" | tail -1
  fi
}

is_profile_enabled() {
  local wanted="$1"
  local source="$2"
  local trimmed token
  IFS=',' read -r -a parts <<< "$source"
  for token in "${parts[@]}"; do
    trimmed="$(echo "$token" | xargs)"
    if [[ "$trimmed" == "$wanted" ]]; then
      return 0
    fi
  done
  return 1
}

service_state() {
  local service="$1"
  docker compose --env-file "$ENV_FILE" ps --all --format '{{.Service}} {{.State}}' \
    | awk -v s="$service" '$1 == s {print $2; found=1; exit} END {if (!found) print ""}'
}

if [[ "$LANGFUSE_ENSURE_ENABLED" != "1" ]]; then
  log "disabled by LANGFUSE_ENSURE_ENABLED=$LANGFUSE_ENSURE_ENABLED"
  exit 0
fi

if [[ -z "$PROFILE_HINT" ]]; then
  PROFILE_HINT="$(get_env_key COMPOSE_PROFILES)"
fi
if [[ -z "$PROFILE_HINT" ]]; then
  PROFILE_HINT="core"
fi
if ! is_profile_enabled "observability" "$PROFILE_HINT"; then
  log "skipping; observability profile not enabled (profiles=$PROFILE_HINT)"
  exit 0
fi

langfuse_state="$(service_state langfuse)"
worker_state="$(service_state langfuse-worker)"
needs_start=0
if [[ "$langfuse_state" != "running" ]]; then
  needs_start=1
fi
if [[ "$worker_state" != "running" ]]; then
  needs_start=1
fi

if [[ "$needs_start" == "1" ]]; then
  log "starting langfuse services (langfuse=${langfuse_state:-missing}, worker=${worker_state:-missing})"
  docker compose --env-file "$ENV_FILE" up -d --no-build langfuse langfuse-worker
fi

elapsed=0
while true; do
  langfuse_state="$(service_state langfuse)"
  worker_state="$(service_state langfuse-worker)"
  if [[ "$langfuse_state" == "running" && "$worker_state" == "running" ]]; then
    break
  fi
  if (( elapsed >= LANGFUSE_ENSURE_TIMEOUT )); then
    log "timeout waiting for langfuse services (langfuse=${langfuse_state:-missing}, worker=${worker_state:-missing})"
    docker compose --env-file "$ENV_FILE" ps --all langfuse langfuse-worker || true
    exit 1
  fi
  sleep "$LANGFUSE_ENSURE_INTERVAL"
  elapsed=$((elapsed + LANGFUSE_ENSURE_INTERVAL))
done

host_bind="${HOST_BIND_ADDRESS:-$(get_env_key HOST_BIND_ADDRESS)}"
if [[ -z "$host_bind" || "$host_bind" == "0.0.0.0" ]]; then
  host_bind="127.0.0.1"
fi
langfuse_port="${LANGFUSE_PORT:-$(get_env_key LANGFUSE_PORT)}"
if [[ -z "$langfuse_port" ]]; then
  langfuse_port="15510"
fi
health_url="http://${host_bind}:${langfuse_port}${LANGFUSE_HEALTH_PATH}"

if [[ -x scripts/wait_for_http.sh ]]; then
  scripts/wait_for_http.sh "$health_url" "$LANGFUSE_ENSURE_TIMEOUT" >/dev/null
else
  curl --fail --max-time 5 --retry 0 "$health_url" >/dev/null
fi

log "healthy at ${health_url}"
