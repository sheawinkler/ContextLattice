#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"
LITE_COMPOSE_FILE="${LITE_COMPOSE_FILE:-docker-compose.lite.yml}"
REPORT_FILE="${SERVICE_UPDATE_REPORT:-tmp/service-version-report.json}"
APPLY_UPDATES="${APPLY_UPDATES:-1}"
ALLOW_MAJOR_UPDATES="${ALLOW_MAJOR_UPDATES:-0}"
REDEPLOY_AFTER_UPDATE="${REDEPLOY_AFTER_UPDATE:-1}"
REDEPLOY_SCOPE="${REDEPLOY_SCOPE:-changed}" # changed | all
RUN_UNIT_TESTS="${RUN_UNIT_TESTS:-0}"
RUN_SMOKE_TESTS="${RUN_SMOKE_TESTS:-1}"
ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"
TEST_VENV_DIR="${SERVICE_UPDATE_TEST_VENV:-.venv-service-update}"
PYTHON_BIN="${SERVICE_UPDATE_PYTHON_BIN:-}"

if [[ -z "$PYTHON_BIN" ]]; then
  if command -v python3.12 >/dev/null 2>&1; then
    PYTHON_BIN="$(command -v python3.12)"
  elif command -v python3.11 >/dev/null 2>&1; then
    PYTHON_BIN="$(command -v python3.11)"
  else
    PYTHON_BIN="$(command -v python3)"
  fi
fi

if [[ ! -f "$COMPOSE_FILE" ]]; then
  echo "Compose file not found: $COMPOSE_FILE" >&2
  exit 2
fi

declare -a PROFILE_ARGS
PROFILE_ARGS=()
if [[ -f .env ]]; then
  profiles_from_env="$(awk -F= '/^COMPOSE_PROFILES=/{print substr($0,index($0,"=")+1)}' .env | tail -1 | tr -d '[:space:]')"
  if [[ -n "${profiles_from_env}" ]]; then
    IFS=',' read -r -a _profiles <<<"$profiles_from_env"
    for profile in "${_profiles[@]}"; do
      [[ -n "$profile" ]] || continue
      PROFILE_ARGS+=(--profile "$profile")
    done
  fi
fi

echo "== Service update audit =="
audit_cmd=(python3 scripts/service_version_audit.py --compose-file "$COMPOSE_FILE")
if [[ -f "$LITE_COMPOSE_FILE" ]]; then
  audit_cmd+=(--compose-file "$LITE_COMPOSE_FILE")
fi
audit_cmd+=(--report-file "$REPORT_FILE")
if [[ "$ALLOW_MAJOR_UPDATES" == "1" ]]; then
  audit_cmd+=(--allow-major)
fi
if [[ "$APPLY_UPDATES" == "1" ]]; then
  audit_cmd+=(--apply)
fi
"${audit_cmd[@]}"

echo "== Compose validation =="
docker compose -f "$COMPOSE_FILE" config -q
if [[ -f "$LITE_COMPOSE_FILE" ]]; then
  docker compose -f "$LITE_COMPOSE_FILE" config -q
fi

if [[ "$REDEPLOY_AFTER_UPDATE" == "1" ]]; then
  if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required for service-scoped redeploy selection" >&2
    exit 2
  fi

  mapfile -t CHANGED_SERVICES < <(
    jq -r '.entries[] | select(.update_available == true and .target_tag != null) | .service' "$REPORT_FILE" | sort -u
  )

  if [[ "$REDEPLOY_SCOPE" == "all" ]]; then
    echo "== Pull latest images for active profiles (all services) =="
    docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" pull
    echo "== Redeploy stack (all services) =="
    docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" up -d --build
  elif [[ "${#CHANGED_SERVICES[@]}" -gt 0 ]]; then
    echo "== Pull latest images (changed services only): ${CHANGED_SERVICES[*]} =="
    docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" pull "${CHANGED_SERVICES[@]}"
    echo "== Redeploy changed services only =="
    docker compose -f "$COMPOSE_FILE" "${PROFILE_ARGS[@]}" up -d --build "${CHANGED_SERVICES[@]}"
  else
    echo "== No version-tag changes detected; skipping compose pull/up =="
  fi
fi

if [[ "$RUN_UNIT_TESTS" == "1" ]]; then
  echo "== Orchestrator unit tests =="
  echo "Using test python: $PYTHON_BIN"
  rm -rf "$TEST_VENV_DIR"
  "$PYTHON_BIN" -m venv "$TEST_VENV_DIR"
  # shellcheck source=/dev/null
  source "$TEST_VENV_DIR/bin/activate"
  python -m pip install -q --upgrade pip
  python -m pip install -q -r services/orchestrator/requirements.txt pytest pytest-asyncio
  pytest -q services/orchestrator/tests/test_orchestrator_retrieval.py
  deactivate
fi

declare -a AUTH_HEADER
AUTH_HEADER=()
if [[ -n "$ORCH_API_KEY" ]]; then
  AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
fi

echo "== Post-deploy health checks =="
for path in /health /status /telemetry/fanout; do
  ok=0
  for _ in $(seq 1 20); do
    if curl -fsS "$ORCH_URL$path" "${AUTH_HEADER[@]}" >/dev/null 2>&1; then
      ok=1
      break
    fi
    sleep 2
  done
  if [[ "$ok" != "1" ]]; then
    echo "Health check failed: $ORCH_URL$path" >&2
    exit 3
  fi
  echo "  OK $path"
done

if [[ "$RUN_SMOKE_TESTS" == "1" ]]; then
  echo "== Write/read smoke check =="
  smoke_project="_global"
  smoke_file="service_update_$(date +%Y%m%d_%H%M%S).txt"
  smoke_content="service update smoke $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  curl -fsS "$ORCH_URL/memory/write" \
    -H "content-type: application/json" \
    "${AUTH_HEADER[@]}" \
    -d "{\"projectName\":\"${smoke_project}\",\"fileName\":\"${smoke_file}\",\"content\":\"${smoke_content}\"}" >/dev/null
  smoke_read_ok=0
  for _ in $(seq 1 20); do
    if curl -fsS "$ORCH_URL/memory/files/${smoke_project}/${smoke_file}" "${AUTH_HEADER[@]}" >/dev/null 2>&1; then
      smoke_read_ok=1
      break
    fi
    sleep 1
  done
  if [[ "$smoke_read_ok" != "1" ]]; then
    echo "Smoke read failed: $ORCH_URL/memory/files/${smoke_project}/${smoke_file}" >&2
    exit 3
  fi
  echo "  OK /memory/write + /memory/files smoke"
fi

echo "== Service update pipeline complete =="
echo "Report: $REPORT_FILE"
