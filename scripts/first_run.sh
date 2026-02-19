#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
BOOTSTRAP="${BOOTSTRAP:-0}"
MINDSDB_REQUIRED="${MINDSDB_REQUIRED:-1}"
MINDSDB_READY_TIMEOUT="${MINDSDB_READY_TIMEOUT:-180}"
RUN_CARGO_SMOKE="${RUN_CARGO_SMOKE:-0}"

EXTERNAL_LETTA=0
LETTA_API_KEY_OVERRIDE=""
LETTA_URL_OVERRIDE=""

usage() {
  cat <<'USAGE'
Usage: scripts/first_run.sh [options]

Options:
  --letta-api-key <key>   Use external Letta API key (disables local llm profile by default)
  --letta-url <url>       External Letta base URL (e.g. https://api.letta.com)
  --external-letta        Disable local llm profile even without key
  --local-letta           Force local llm profile
  -h, --help              Show this help

Env toggles:
  BOOTSTRAP=1             Run gmake mem-up before smoke test
  MINDSDB_REQUIRED=0/1    Whether smoke requires MindsDB readiness
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
  set_env_key "COMPOSE_PROFILES" "$profiles_csv"
  echo ">> COMPOSE_PROFILES=${profiles_csv}"
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

if [[ "$BOOTSTRAP" == "1" ]]; then
  echo ">> Bootstrapping stack with gmake mem-up"
  gmake mem-up
fi

echo ">> Running first-run smoke (no cargo bins)"
MINDSDB_REQUIRED="$MINDSDB_REQUIRED" \
MINDSDB_READY_TIMEOUT="$MINDSDB_READY_TIMEOUT" \
RUN_CARGO_SMOKE="$RUN_CARGO_SMOKE" \
scripts/devnet_smoke.sh
