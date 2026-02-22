#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
BOOTSTRAP="${BOOTSTRAP:-0}"
MINDSDB_REQUIRED="${MINDSDB_REQUIRED:-auto}"
MINDSDB_READY_TIMEOUT="${MINDSDB_READY_TIMEOUT:-180}"
RUN_CARGO_SMOKE="${RUN_CARGO_SMOKE:-0}"

EXTERNAL_LETTA=0
LETTA_API_KEY_OVERRIDE=""
LETTA_URL_OVERRIDE=""
INSECURE_LOCAL=0
SECRETS_STORAGE_MODE_OVERRIDE=""
COMPOSE_PROFILES_EFFECTIVE=""

usage() {
  cat <<'USAGE'
Usage: scripts/first_run.sh [options]

Options:
  --letta-api-key <key>   Use external Letta API key (disables local llm profile by default)
  --letta-url <url>       External Letta base URL (e.g. https://api.letta.com)
  --external-letta        Disable local llm profile even without key
  --local-letta           Force local llm profile
  --allow-secrets-storage Store write payloads as-is (no redaction)
  --block-secrets-storage Reject writes that include secret-like values
  --redact-secrets-storage Force redaction mode (default)
  --insecure-local        Opt out of secure production defaults for local-only experimentation
  -h, --help              Show this help

Env toggles:
  BOOTSTRAP=1             Run gmake mem-up before smoke test
  MINDSDB_REQUIRED=auto/0/1  Whether smoke requires MindsDB readiness
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --letta-api-key)
      [[ $# -ge 2 ]] || { echo "Missing value for --letta-api-key" >&2; exit 2; }
      LETTA_API_KEY_OVERRIDE="$2"
      EXTERNAL_LETTA=1
      shift 2
      ;;
    --letta-url)
      [[ $# -ge 2 ]] || { echo "Missing value for --letta-url" >&2; exit 2; }
      LETTA_URL_OVERRIDE="$2"
      EXTERNAL_LETTA=1
      shift 2
      ;;
    --external-letta)
      EXTERNAL_LETTA=1
      shift
      ;;
    --local-letta)
      EXTERNAL_LETTA=0
      shift
      ;;
    --allow-secrets-storage)
      SECRETS_STORAGE_MODE_OVERRIDE="allow"
      shift
      ;;
    --block-secrets-storage)
      SECRETS_STORAGE_MODE_OVERRIDE="block"
      shift
      ;;
    --redact-secrets-storage)
      SECRETS_STORAGE_MODE_OVERRIDE="redact"
      shift
      ;;
    --insecure-local)
      INSECURE_LOCAL=1
      shift
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

set_env_key() {
  local key="$1"
  local value="$2"
  local tmp_file
  tmp_file="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
  if [[ -f "$ENV_FILE" ]]; then
    awk -v k="$key" -v v="$value" '
      BEGIN { updated = 0 }
      $0 ~ ("^" k "=") {
        print k "=" v
        updated = 1
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

configure_profiles_for_letta() {
  local current profiles_csv has_llm=0
  current="$(awk -F= '/^COMPOSE_PROFILES=/{print substr($0,index($0,"=")+1)}' "$ENV_FILE" 2>/dev/null | tail -1)"
  if [[ -z "${current}" ]]; then
    current="core,analytics,llm,observability"
  fi

  IFS=',' read -r -a parts <<< "$current"
  local cleaned=()
  for part in "${parts[@]}"; do
    part="$(echo "$part" | xargs)"
    [[ -n "$part" ]] || continue
    if [[ "$part" == "llm" ]]; then
      has_llm=1
      if [[ "$EXTERNAL_LETTA" == "1" ]]; then
        continue
      fi
    fi
    cleaned+=("$part")
  done

  if [[ "$EXTERNAL_LETTA" == "0" && "$has_llm" == "0" ]]; then
    cleaned+=("llm")
  fi
  if [[ "${#cleaned[@]}" -eq 0 ]]; then
    cleaned=("core")
  fi

  profiles_csv="$(IFS=,; echo "${cleaned[*]}")"
  COMPOSE_PROFILES_EFFECTIVE="$profiles_csv"
  set_env_key "COMPOSE_PROFILES" "$profiles_csv"
  echo ">> COMPOSE_PROFILES=${profiles_csv}"
}

configure_mindsdb_smoke_requirement() {
  if [[ "$MINDSDB_REQUIRED" != "auto" ]]; then
    echo ">> MINDSDB_REQUIRED=${MINDSDB_REQUIRED} (explicit)"
    return 0
  fi
  if [[ ",${COMPOSE_PROFILES_EFFECTIVE}," == *",analytics,"* ]]; then
    MINDSDB_REQUIRED="1"
  else
    MINDSDB_REQUIRED="0"
  fi
  echo ">> MINDSDB_REQUIRED=${MINDSDB_REQUIRED} (auto from COMPOSE_PROFILES)"
}

get_env_key() {
  local key="$1"
  if [[ ! -f "$ENV_FILE" ]]; then
    return 0
  fi
  awk -F= -v k="$key" '$1 == k {print substr($0, index($0, "=") + 1)}' "$ENV_FILE" | tail -1
}

generate_api_key() {
  if command -v openssl >/dev/null 2>&1; then
    printf 'cl_%s' "$(openssl rand -hex 24)"
    return 0
  fi
  python3 - <<'PY'
import secrets
print(f"cl_{secrets.token_hex(24)}")
PY
}

configure_security_posture() {
  local api_key secrets_mode
  secrets_mode="${SECRETS_STORAGE_MODE_OVERRIDE:-redact}"
  set_env_key "SECRETS_STORAGE_MODE" "$secrets_mode"

  if [[ "$INSECURE_LOCAL" == "1" ]]; then
    set_env_key "MEMMCP_ENV" "development"
    set_env_key "ORCH_SECURITY_STRICT" "false"
    set_env_key "ORCH_PUBLIC_STATUS" "true"
    set_env_key "ORCH_PUBLIC_DOCS" "true"
    set_env_key "MESSAGING_WEBHOOK_PUBLIC" "true"
    set_env_key "HOST_BIND_ADDRESS" "0.0.0.0"
    echo ">> security posture: insecure local overrides applied"
    return 0
  fi

  set_env_key "MEMMCP_ENV" "production"
  set_env_key "ORCH_SECURITY_STRICT" "true"
  set_env_key "ORCH_PUBLIC_STATUS" "false"
  set_env_key "ORCH_PUBLIC_DOCS" "false"
  set_env_key "MESSAGING_WEBHOOK_PUBLIC" "false"
  set_env_key "HOST_BIND_ADDRESS" "127.0.0.1"

  api_key="$(get_env_key MEMMCP_ORCHESTRATOR_API_KEY)"
  if [[ -z "${api_key}" ]]; then
    api_key="$(generate_api_key)"
    set_env_key "MEMMCP_ORCHESTRATOR_API_KEY" "$api_key"
    echo ">> generated MEMMCP_ORCHESTRATOR_API_KEY"
  fi
  echo ">> security posture: production defaults (loopback + auth) applied"
}

if [[ "$EXTERNAL_LETTA" == "1" ]]; then
  if [[ -n "$LETTA_API_KEY_OVERRIDE" ]]; then
    set_env_key "LETTA_API_KEY" "$LETTA_API_KEY_OVERRIDE"
  fi
  if [[ -n "$LETTA_URL_OVERRIDE" ]]; then
    set_env_key "LETTA_URL" "$LETTA_URL_OVERRIDE"
  fi
  set_env_key "LETTA_REQUIRE_API_KEY" "true"
  configure_profiles_for_letta
  if [[ -z "$LETTA_URL_OVERRIDE" ]]; then
    echo ">> external Letta mode enabled; pass --letta-url if not using the default LETTA_URL in .env"
  fi
else
  set_env_key "LETTA_REQUIRE_API_KEY" "false"
  configure_profiles_for_letta
fi

configure_security_posture

configure_mindsdb_smoke_requirement

if [[ "$BOOTSTRAP" == "1" ]]; then
  echo ">> Bootstrapping stack with gmake mem-up"
  gmake mem-up
fi

echo ">> Running first-run smoke (no cargo bins)"
MINDSDB_REQUIRED="$MINDSDB_REQUIRED" \
MINDSDB_READY_TIMEOUT="$MINDSDB_READY_TIMEOUT" \
RUN_CARGO_SMOKE="$RUN_CARGO_SMOKE" \
scripts/devnet_smoke.sh
