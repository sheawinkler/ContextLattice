#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
STRICT_ENV_FILE="${STRICT_ENV_FILE:-config/env/strict_runtime.env}"
MODE="${1:---apply}"

usage() {
  cat <<'USAGE'
Usage: scripts/enforce_strict_env.sh [--apply|--check]

--apply   Enforce strict_runtime.env values into ENV_FILE (default)
--check   Validate only; fail on drift
USAGE
}

if [[ "$MODE" != "--apply" && "$MODE" != "--check" ]]; then
  usage
  exit 2
fi

if [[ ! -f "$STRICT_ENV_FILE" ]]; then
  echo "ERROR: strict env file not found: $STRICT_ENV_FILE" >&2
  exit 1
fi

if [[ ! -f "$ENV_FILE" ]]; then
  if [[ -f ".env.example" ]]; then
    cp ".env.example" "$ENV_FILE"
    echo ">> created $ENV_FILE from .env.example"
  else
    echo "ERROR: missing $ENV_FILE and .env.example not found" >&2
    exit 1
  fi
fi

set_env_key() {
  local key="$1"
  local value="$2"
  local tmp_file
  tmp_file="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
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
  mv "$tmp_file" "$ENV_FILE"
}

get_env_key() {
  local key="$1"
  awk -F= -v k="$key" '$1 == k {print substr($0, index($0, "=") + 1)}' "$ENV_FILE" | tail -1
}

trim_spaces() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

declare -a keys
declare -a values

while IFS= read -r raw_line || [[ -n "${raw_line:-}" ]]; do
  line="${raw_line%$'\r'}"
  line="$(trim_spaces "$line")"
  [[ -z "$line" ]] && continue
  [[ "${line:0:1}" == "#" ]] && continue
  if [[ "$line" != *"="* ]]; then
    echo "ERROR: invalid line in $STRICT_ENV_FILE: $line" >&2
    exit 1
  fi
  key="$(trim_spaces "${line%%=*}")"
  value="${line#*=}"
  if [[ -z "$key" ]]; then
    echo "ERROR: empty key in $STRICT_ENV_FILE line: $line" >&2
    exit 1
  fi
  keys+=("$key")
  values+=("$value")
done < "$STRICT_ENV_FILE"

if [[ "${#keys[@]}" -eq 0 ]]; then
  echo "ERROR: no strict keys loaded from $STRICT_ENV_FILE" >&2
  exit 1
fi

drift_count=0
apply_count=0
for i in "${!keys[@]}"; do
  key="${keys[$i]}"
  expected="${values[$i]}"
  current="$(get_env_key "$key")"
  if [[ "$current" != "$expected" ]]; then
    drift_count=$((drift_count + 1))
    if [[ "$MODE" == "--apply" ]]; then
      set_env_key "$key" "$expected"
      apply_count=$((apply_count + 1))
      echo ">> locked $key=$expected"
    else
      echo "drift: $key current='${current}' expected='${expected}'"
    fi
  fi
done

# Keep legacy aliases aligned to prevent caller drift.
sync_alias_pair() {
  local canonical_key="$1"
  local alias_key="$2"
  local value
  value="$(get_env_key "$canonical_key")"
  if [[ -z "$value" ]]; then
    value="$(get_env_key "$alias_key")"
  fi
  [[ -n "$value" ]] || return 0

  local canonical_current alias_current
  canonical_current="$(get_env_key "$canonical_key")"
  alias_current="$(get_env_key "$alias_key")"

  if [[ "$canonical_current" != "$value" || "$alias_current" != "$value" ]]; then
    if [[ "$MODE" == "--apply" ]]; then
      set_env_key "$canonical_key" "$value"
      set_env_key "$alias_key" "$value"
      echo ">> synced $canonical_key <-> $alias_key"
    else
      echo "drift: $canonical_key / $alias_key mismatch"
      drift_count=$((drift_count + 1))
    fi
  fi
}

sync_alias_pair CONTEXTLATTICE_ORCHESTRATOR_API_KEY MEMMCP_ORCHESTRATOR_API_KEY
sync_alias_pair CONTEXTLATTICE_ORCHESTRATOR_URL MEMMCP_ORCHESTRATOR_URL
sync_alias_pair CONTEXTLATTICE_AGENT_ID MEMMCP_AGENT_ID

required_non_empty=(
  CONTEXTLATTICE_ORCHESTRATOR_API_KEY
  EMBEDDING_MODEL
  QDRANT_COLLECTION
  MONGODB_URI
  MINDSDB_APIS
)

missing_required=0
for key in "${required_non_empty[@]}"; do
  value="$(get_env_key "$key")"
  if [[ -z "$value" ]]; then
    echo "missing required non-empty key: $key"
    missing_required=$((missing_required + 1))
  fi
done

if [[ "$MODE" == "--check" ]]; then
  if (( drift_count > 0 || missing_required > 0 )); then
    echo "ERROR: strict env check failed (drift=$drift_count, missing_required=$missing_required)"
    exit 1
  fi
  echo "strict env check: OK (keys=${#keys[@]})"
  exit 0
fi

if (( missing_required > 0 )); then
  echo "ERROR: strict env apply completed but required keys are still missing ($missing_required)"
  exit 1
fi

if (( apply_count == 0 )); then
  echo "strict env apply: already compliant (keys=${#keys[@]})"
else
  echo "strict env apply: updated $apply_count key(s)"
fi
