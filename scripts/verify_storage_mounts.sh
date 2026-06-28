#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"
PROJECT_NAME="${COMPOSE_PROJECT_NAME:-contextlattice}"
REPAIR=0
QUIET=0

usage() {
  cat <<'USAGE'
Usage: scripts/verify_storage_mounts.sh [options]

Verifies core service storage mounts match configured host paths or named volumes.
Prevents silent storage-lane drift (for example pgvector using a fresh named volume).
When QDRANT_HOT_STORAGE_MAX_BYTES is set, also enforces the mounted Qdrant
hot-store size ceiling.

Options:
  --env-file <path>        Compose env file (default: .env)
  --compose-file <path>    Compose file (default: docker-compose.yml)
  --project <name>         Compose project name (default: contextlattice)
  --repair                 Force-recreate mismatched services and re-check.
                           If storage env vars are unset, infer base from healthy bind mounts.
  --quiet                  Suppress informational output (errors still print)
  -h, --help               Show help
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      ENV_FILE="${2:-}"
      shift 2
      ;;
    --compose-file)
      COMPOSE_FILE="${2:-}"
      shift 2
      ;;
    --project)
      PROJECT_NAME="${2:-}"
      shift 2
      ;;
    --repair)
      REPAIR=1
      shift
      ;;
    --quiet)
      QUIET=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "[storage-mount-check] unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ ! -f "$ENV_FILE" ]]; then
  echo "[storage-mount-check] env file not found: $ENV_FILE" >&2
  exit 2
fi

set -a
# shellcheck source=/dev/null
source "$ENV_FILE"
set +a

compose=(docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" -p "$PROJECT_NAME")

log() {
  if [[ "$QUIET" != "1" ]]; then
    echo "$@"
  fi
}

required_tools=(docker jq)
for tool in "${required_tools[@]}"; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "[storage-mount-check] required tool missing: $tool" >&2
    exit 2
  fi
done

services=(
  "pgvector-db|PGVECTOR_DATA|/home/postgres/pgdata/data"
  "qdrant|QDRANT_STORAGE|/qdrant/storage"
  "mongo|MONGO_DATA|/data/db"
  "gateway-go|MEMORY_BANK_DATA|/data"
  "memory-bank-spike-rs|MEMORY_BANK_DATA|/data"
)

mismatches=()

is_absolute_path() {
  [[ "${1:-}" == /* ]]
}

is_named_volume_value() {
  local value="${1:-}"
  [[ -n "$value" && "$value" != /* && "$value" != ./* && "$value" != ../* && "$value" != *:* ]]
}

expected_volume_name() {
  local value="$1"
  if [[ "$value" == "${PROJECT_NAME}_"* ]]; then
    echo "$value"
  else
    echo "${PROJECT_NAME}_${value}"
  fi
}

storage_suffix_for_env_key() {
  case "$1" in
    PGVECTOR_DATA) echo "pgvector_data" ;;
    QDRANT_STORAGE) echo "qdrant_storage" ;;
    MONGO_DATA) echo "mongo_data" ;;
    MEMORY_BANK_DATA) echo "memory_bank_data" ;;
    *) echo "" ;;
  esac
}

size_cap_env_for_key() {
  case "$1" in
    QDRANT_STORAGE) echo "QDRANT_HOT_STORAGE_MAX_BYTES" ;;
    *) echo "" ;;
  esac
}

container_path_bytes() {
  local cid="$1" path="$2"
  docker exec "$cid" sh -c '
    path="$1"
    if du -sb "$path" >/dev/null 2>&1; then
      du -sb "$path" | awk "{print \$1; exit}"
    else
      du -sk "$path" | awk "{printf \"%.0f\n\", \$1 * 1024; exit}"
    fi
  ' sh "$path" 2>/dev/null | tail -n1 | tr -d '[:space:]'
}

verify_size_cap() {
  local service="$1" env_key="$2" destination="$3" cid="$4"
  local cap_var cap_bytes used_bytes
  cap_var="$(size_cap_env_for_key "$env_key")"
  [[ -n "$cap_var" ]] || return 0

  cap_bytes="${!cap_var:-}"
  cap_bytes="$(echo "${cap_bytes:-}" | xargs)"
  if [[ -z "$cap_bytes" || "$cap_bytes" == "0" ]]; then
    return 0
  fi
  if [[ ! "$cap_bytes" =~ ^[0-9]+$ ]]; then
    mismatches+=("$service|$cap_var|invalid:$cap_bytes|positive integer bytes")
    return 1
  fi

  used_bytes="$(container_path_bytes "$cid" "$destination" || true)"
  if [[ ! "$used_bytes" =~ ^[0-9]+$ ]]; then
    mismatches+=("$service|$destination|size_bytes:unavailable|<=${cap_bytes} (${cap_var})")
    return 1
  fi
  if (( used_bytes > cap_bytes )); then
    mismatches+=("$service|$destination|size_bytes:$used_bytes|<=${cap_bytes} (${cap_var})")
    return 1
  fi

  log "[storage-mount-check] ok $service size_bytes=$used_bytes <= $cap_bytes ($cap_var)"
  return 0
}

inspect_mount() {
  local cid="$1" dest="$2"
  docker inspect "$cid" --format '{{json .Mounts}}' \
    | jq -r --arg d "$dest" '.[] | select(.Destination==$d) | [.Type,.Source,(.Name // "")] | @tsv' \
    | head -n1
}

inspect_env() {
  local cid="$1" key="$2"
  docker inspect "$cid" --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | awk -F= -v k="$key" '$1 == k {print substr($0, length(k) + 2); found=1; exit} END {exit found ? 0 : 1}'
}

infer_storage_base_from_running() {
  local candidates=()
  local row svc key dest suffix cid mount_line mount_type mount_source mount_name
  for row in "${services[@]}"; do
    IFS='|' read -r svc key dest <<<"$row"
    suffix="$(storage_suffix_for_env_key "$key")"
    [[ -n "$suffix" ]] || continue
    cid="$("${compose[@]}" ps -q "$svc" 2>/dev/null || true)"
    [[ -n "$cid" ]] || continue
    mount_line="$(inspect_mount "$cid" "$dest")"
    [[ -n "$mount_line" ]] || continue
    IFS=$'\t' read -r mount_type mount_source mount_name <<<"$mount_line"
    [[ "$mount_type" == "bind" ]] || continue
    [[ "$mount_source" == */"$suffix" ]] || continue
    candidates+=("${mount_source%/"$suffix"}")
  done

  if (( ${#candidates[@]} == 0 )); then
    return 1
  fi

  # Pick the most frequent candidate root from running services.
  local best="" best_count=0 candidate count other
  for candidate in "${candidates[@]}"; do
    count=0
    for other in "${candidates[@]}"; do
      [[ "$other" == "$candidate" ]] && count=$((count + 1))
    done
    if (( count > best_count )); then
      best="$candidate"
      best_count=$count
    fi
  done
  [[ -n "$best" ]] || return 1
  echo "$best"
  return 0
}

hydrate_repair_env_from_inferred_root() {
  local inferred row key expected suffix
  [[ "$REPAIR" == "1" ]] || return 0
  inferred="$(infer_storage_base_from_running || true)"
  [[ -n "$inferred" ]] || return 0
  log "[storage-mount-check] inferred storage base: $inferred"

  for row in "${services[@]}"; do
    IFS='|' read -r _svc key _dest <<<"$row"
    expected="${!key:-}"
    expected="$(echo "${expected:-}" | xargs)"
    if is_absolute_path "$expected" || is_named_volume_value "$expected"; then
      continue
    fi
    suffix="$(storage_suffix_for_env_key "$key")"
    [[ -n "$suffix" ]] || continue
    export "$key=$inferred/$suffix"
    log "[storage-mount-check] inferred $key=$inferred/$suffix"
  done
}

verify_service() {
  local service="$1" env_key="$2" destination="$3"
  local expected="${!env_key:-}"
  expected="$(echo "${expected:-}" | xargs)"
  if [[ -z "$expected" ]]; then
    expected="$(storage_suffix_for_env_key "$env_key")"
    if [[ -z "$expected" ]]; then
      log "[storage-mount-check] skip $service ($env_key unset)"
      return 0
    fi
  fi

  local cid
  cid="$("${compose[@]}" ps -q "$service" 2>/dev/null || true)"
  if [[ -z "$cid" ]]; then
    log "[storage-mount-check] skip $service (container not running)"
    return 0
  fi

  local mount_line mount_type mount_source mount_name expected_volume actual_volume
  mount_line="$(inspect_mount "$cid" "$destination")"
  if [[ -z "$mount_line" ]]; then
    mismatches+=("$service|$destination|missing|$expected")
    return 1
  fi
  IFS=$'\t' read -r mount_type mount_source mount_name <<<"$mount_line"

  if is_absolute_path "$expected"; then
    if [[ "$mount_type" != "bind" ]]; then
      mismatches+=("$service|$destination|$mount_type:$mount_source|$expected")
      return 1
    fi
    if [[ "$mount_source" != "$expected" ]]; then
      mismatches+=("$service|$destination|$mount_source|$expected")
      return 1
    fi
    log "[storage-mount-check] ok $service -> $mount_source ($destination)"
    verify_size_cap "$service" "$env_key" "$destination" "$cid" || return 1
    return 0
  fi

  if is_named_volume_value "$expected"; then
    expected_volume="$(expected_volume_name "$expected")"
    actual_volume="$mount_name"
    [[ -n "$actual_volume" ]] || actual_volume="$(basename "$mount_source")"
    if [[ "$mount_type" != "volume" ]]; then
      mismatches+=("$service|$destination|$mount_type:$mount_source|volume:$expected_volume")
      return 1
    fi
    if [[ "$actual_volume" != "$expected_volume" ]]; then
      mismatches+=("$service|$destination|volume:$actual_volume|volume:$expected_volume")
      return 1
    fi
    log "[storage-mount-check] ok $service -> volume:$actual_volume ($destination)"
    verify_size_cap "$service" "$env_key" "$destination" "$cid" || return 1
    return 0
  fi

  log "[storage-mount-check] skip $service ($env_key is neither absolute path nor named volume: $expected)"
  return 0
}

verify_qdrant_hot_store_policy() {
  local expected="${QDRANT_STORAGE:-}"
  expected="$(echo "${expected:-}" | xargs)"
  [[ -n "$expected" ]] || expected="qdrant_storage"

  local cap_var="QDRANT_HOT_STORAGE_MAX_BYTES"
  local cap_bytes="${!cap_var:-}"
  cap_bytes="$(echo "${cap_bytes:-}" | xargs)"
  [[ -n "$cap_bytes" && "$cap_bytes" != "0" ]] || return 0

  if [[ ! "$cap_bytes" =~ ^[0-9]+$ ]]; then
    mismatches+=("qdrant|$cap_var|invalid:$cap_bytes|positive integer bytes")
    return 1
  fi

  if ! is_named_volume_value "$expected"; then
    log "[storage-mount-check] qdrant hot-store cap active for non-volume path ($expected); runtime size check will use mounted path"
    return 0
  fi

  local volume_name
  volume_name="$(expected_volume_name "$expected")"
  if docker volume inspect "$volume_name" >/dev/null 2>&1; then
    log "[storage-mount-check] qdrant hot-store cap active for volume:$volume_name max_bytes=$cap_bytes"
  else
    log "[storage-mount-check] qdrant hot-store cap configured; volume:$volume_name not created yet"
  fi
  return 0
}

verify_gateway_storage_governance_root() {
  local service="gateway-go"
  local expected_destination="/data"
  local cid
  cid="$("${compose[@]}" ps -q "$service" 2>/dev/null || true)"
  if [[ -z "$cid" ]]; then
    log "[storage-mount-check] skip $service storage governance root (container not running)"
    return 0
  fi

  local root
  root="$(inspect_env "$cid" "ORCH_STORAGE_GOVERNANCE_DISK_ROOT" 2>/dev/null || true)"
  root="$(echo "${root:-}" | xargs)"
  if [[ -z "$root" ]]; then
    mismatches+=("$service|ORCH_STORAGE_GOVERNANCE_DISK_ROOT|unset|$expected_destination")
    return 1
  fi
  if [[ "$root" == "." || "$root" == "/" || "$root" != /* ]]; then
    mismatches+=("$service|ORCH_STORAGE_GOVERNANCE_DISK_ROOT|$root|$expected_destination")
    return 1
  fi
  if [[ "$root" != "$expected_destination" && "$root" != "$expected_destination"/* ]]; then
    mismatches+=("$service|ORCH_STORAGE_GOVERNANCE_DISK_ROOT|$root|$expected_destination")
    return 1
  fi

  local mount_line
  mount_line="$(inspect_mount "$cid" "$expected_destination")"
  if [[ -z "$mount_line" ]]; then
    mismatches+=("$service|$expected_destination|missing mount for storage governance root|$expected_destination")
    return 1
  fi

  log "[storage-mount-check] ok $service storage governance root -> $root"
  return 0
}

run_check_pass() {
  mismatches=()
  for row in "${services[@]}"; do
    IFS='|' read -r svc key dest <<<"$row"
    verify_service "$svc" "$key" "$dest" || true
  done
  verify_qdrant_hot_store_policy || true
  verify_gateway_storage_governance_root || true
  if (( ${#mismatches[@]} == 0 )); then
    log "[storage-mount-check] all checked storage mounts match expected targets"
    return 0
  fi
  return 1
}

hydrate_repair_env_from_inferred_root

if run_check_pass; then
  exit 0
fi

echo "[storage-mount-check] detected storage mount mismatch:" >&2
for row in "${mismatches[@]}"; do
  IFS='|' read -r svc dest actual expected <<<"$row"
  echo "  - service=$svc destination=$dest actual=$actual expected=$expected" >&2
done

if [[ "$REPAIR" != "1" ]]; then
  echo "[storage-mount-check] rerun with --repair to force-recreate mismatched services" >&2
  exit 1
fi

echo "[storage-mount-check] repairing mismatched services..." >&2
for row in "${mismatches[@]}"; do
  IFS='|' read -r svc _dest _actual _expected <<<"$row"
  echo "  - recreate $svc" >&2
  "${compose[@]}" up -d --force-recreate "$svc" >/dev/null
done

sleep 2
