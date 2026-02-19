#!/usr/bin/env bash
set -euo pipefail

DURATION="${SMOKE_DURATION:-90}"
CONFIG_PATH="${CONFIG:-config.toml}"
WALLET_PATH="${WALLET:-wallet_devnet.json}"
ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"
SIDECAR_HEALTH_URL="${SIDECAR_HEALTH_URL:-http://127.0.0.1:8288/health}"
SIDECAR_URL="${SIDECAR_URL:-http://127.0.0.1:8288}"
REALISTIC_SIM="${REALISTIC_SIM:-0}"
VALIDATE_ENDPOINTS="${VALIDATE_ENDPOINTS:-1}"
SIMULATE_SYMBOLS="${SIMULATE_SYMBOLS:-SOL BONK WIF DOG}"
SIDECAR_ARGS="${SIDECAR_ARGS:-}"
BACKUP_DIR="${MEMMCP_LOCAL_BACKUP_DIR:-$(pwd)/tmp/devnet_backups}"
SPOOL_DIR="${MEMMCP_LOCAL_STORE_PATH:-$(pwd)/tmp/devnet_spool}"
RPC_URL="${SOLANA_RPC_URL:-https://api.devnet.solana.com}"
RUN_CARGO_SMOKE="${RUN_CARGO_SMOKE:-0}"
CARGO_PROJECT_DIR="${CARGO_PROJECT_DIR:-$(pwd)}"
SMOKE_WRITE="${SMOKE_WRITE:-1}"
MINDSDB_SMOKE="${MINDSDB_SMOKE:-1}"
MINDSDB_REQUIRED="${MINDSDB_REQUIRED:-1}"
MINDSDB_SQL_URL="${MINDSDB_SQL_URL:-http://127.0.0.1:47334/api/sql/query}"
MINDSDB_AUTOSYNC_ATTEMPTS="${MINDSDB_AUTOSYNC_ATTEMPTS:-10}"
MINDSDB_READY_TIMEOUT="${MINDSDB_READY_TIMEOUT:-120}"
SMOKE_BINS="${SMOKE_BINS:-}" # optional space-separated list
BOOTSTRAP_ORCH="${BOOTSTRAP_ORCH:-0}"
BOOTSTRAP_SIDECAR="${BOOTSTRAP_SIDECAR:-1}"
DEFAULT_ORCH_CMD="cd ~/.mcp-servers/mem_mcp_lobehub && docker compose up -d memmcp-orchestrator"
DEFAULT_SIDECAR_CMD="cd ~/Documents/Projects/crypto_trader_post_training_needs_godmode_and_finalization && poetry run uvicorn project.src.api.fastapi_server:app --host 0.0.0.0 --port 8288"
ORCH_START_CMD="${ORCH_START_CMD:-$DEFAULT_ORCH_CMD}"
SIDECAR_START_CMD="${SIDECAR_START_CMD:-$DEFAULT_SIDECAR_CMD}"

mkdir -p "$BACKUP_DIR" "$SPOOL_DIR"
export MEMMCP_LOCAL_BACKUP_DIR="$BACKUP_DIR"
export MEMMCP_LOCAL_STORE_PATH="$SPOOL_DIR"
export MEMMCP_ORCHESTRATOR_URL="$ORCH_URL"
export SOLANA_RPC_URL="$RPC_URL"
export SOLANA_CLUSTER="devnet"
export SIDECAR_URL

if command -v timeout >/dev/null 2>&1; then
  TIMEOUT_CMD="timeout"
elif command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_CMD="gtimeout"
else
  echo "timeout command is required for the smoke test" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required for the smoke test" >&2
  exit 1
fi

check_endpoint() {
  local url="$1"
  local name="$2"
  if ! curl -fsS "$url" >/dev/null; then
    echo "[devnet-smoke] ERROR: $name endpoint unreachable at $url" >&2
    exit 2
  fi
}

wait_for_endpoint() {
  local url="$1"
  local name="$2"
  local timeout="${3:-60}"
  local sleep_secs="${4:-2}"
  local elapsed=0
  while true; do
    if curl -fsS "$url" >/dev/null 2>&1; then
      echo "[devnet-smoke] $name healthy at $url"
      return 0
    fi
    if (( elapsed >= timeout )); then
      echo "[devnet-smoke] ERROR: $name not ready within ${timeout}s ($url)" >&2
      exit 3
    fi
    sleep "$sleep_secs"
    elapsed=$((elapsed+sleep_secs))
  done
}

wait_for_mindsdb() {
  local timeout="${1:-60}"
  local sleep_secs="${2:-2}"
  local elapsed=0
  while true; do
    if curl -fsS "$MINDSDB_SQL_URL" \
      -H "content-type: application/json" \
      -d '{"query":"SELECT 1"}' >/dev/null 2>&1; then
      echo "[devnet-smoke] MindsDB ready at $MINDSDB_SQL_URL"
      return 0
    fi
    if (( elapsed >= timeout )); then
      return 1
    fi
    sleep "$sleep_secs"
    elapsed=$((elapsed+sleep_secs))
  done
}

smoke_memory_write() {
  local project="${1:-_global}"
  local file_path="smoke/devnet_smoke_$(date +%Y%m%d_%H%M%S).txt"
  local content="devnet smoke test: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  curl -fsS "$ORCH_URL/memory/write" \
    -H "content-type: application/json" \
    "${ORCH_AUTH_HEADER[@]}" \
    -d "{\"projectName\":\"${project}\",\"fileName\":\"${file_path}\",\"content\":\"${content}\"}" >/dev/null
  curl -fsS "$ORCH_URL/memory/files/${project}/${file_path}" \
    "${ORCH_AUTH_HEADER[@]}" >/dev/null
  echo "[devnet-smoke] memory write/read ok: ${project}/${file_path}"
  export SMOKE_PROJECT="$project"
  export SMOKE_FILE="$file_path"
}

smoke_mindsdb() {
  if ! curl -fsS "$MINDSDB_SQL_URL" \
    -H "content-type: application/json" \
    -d '{"query":"SELECT 1"}' >/dev/null; then
    if [[ "$MINDSDB_REQUIRED" == "1" ]]; then
      echo "[devnet-smoke] MindsDB required but unreachable: $MINDSDB_SQL_URL" >&2
      exit 4
    fi
    echo "[devnet-smoke] MindsDB unreachable; skipping." >&2
    return
  fi

  echo "[devnet-smoke] MindsDB basic query ok."

  if [[ -n "${SMOKE_PROJECT:-}" && -n "${SMOKE_FILE:-}" ]]; then
    local query="SELECT project, file FROM files.memory_events WHERE project='${SMOKE_PROJECT}' AND file='${SMOKE_FILE}' LIMIT 1;"
    local found=0
    for _ in $(seq 1 "$MINDSDB_AUTOSYNC_ATTEMPTS"); do
      resp=$(curl -fsS "$MINDSDB_SQL_URL" -H "content-type: application/json" -d "{\"query\":\"${query}\"}") || resp=""
      if echo "$resp" | grep -q '"type":"table"' && echo "$resp" | grep -q "${SMOKE_FILE}"; then
        found=1
        break
      fi
      sleep 1
    done
    if [[ "$found" == "1" ]]; then
      echo "[devnet-smoke] MindsDB autosync row found."
    else
      msg="[devnet-smoke] MindsDB autosync row not found (may be disabled or delayed)."
      if [[ "$MINDSDB_REQUIRED" == "1" ]]; then
        echo "$msg" >&2
        exit 5
      fi
      echo "$msg" >&2
    fi
  fi
}

if [[ "${SKIP_ORCH_CHECK:-0}" != "1" ]]; then
  check_endpoint "$ORCH_URL/health" "orchestrator"
fi
if [[ "${SKIP_SIDECAR_CHECK:-0}" != "1" ]]; then
  check_endpoint "$SIDECAR_HEALTH_URL" "sidecar"
fi

cleanup() {
  if [[ -n "${SIDECAR_PID:-}" ]]; then
    kill "$SIDECAR_PID" 2>/dev/null || true
  fi
}

trap cleanup EXIT

if [[ "$BOOTSTRAP_ORCH" == "1" ]]; then
  echo "[devnet-smoke] Bootstrapping orchestrator via: $ORCH_START_CMD"
  bash -lc "$ORCH_START_CMD"
fi

if [[ -z "$SMOKE_BINS" ]]; then
  SMOKE_BIN_LIST=("unified_trader" "run_strategy")
else
  IFS=' ' read -r -a SMOKE_BIN_LIST <<<"$SMOKE_BINS"
fi

if [[ "$BOOTSTRAP_SIDECAR" == "1" ]]; then
  if curl -fsS "$SIDECAR_HEALTH_URL" >/dev/null 2>&1; then
    echo "[devnet-smoke] Sidecar already responding, skipping bootstrap"
    BOOTSTRAP_SIDECAR=0
  fi
fi

if [[ "$BOOTSTRAP_SIDECAR" == "1" ]]; then
  echo "[devnet-smoke] Bootstrapping sidecar via: $SIDECAR_START_CMD"
  bash -lc "$SIDECAR_START_CMD" > /tmp/devnet_sidecar.log 2>&1 &
  SIDECAR_PID=$!
  sleep 2
fi

if [[ "${SKIP_ORCH_CHECK:-0}" != "1" ]]; then
  wait_for_endpoint "$ORCH_URL/status" "orchestrator"
fi
if [[ "${SKIP_SIDECAR_CHECK:-0}" != "1" ]]; then
  wait_for_endpoint "$SIDECAR_HEALTH_URL" "sidecar" 40 1
fi

if [[ "$SMOKE_WRITE" == "1" ]]; then
  smoke_memory_write
fi
if [[ "$MINDSDB_SMOKE" == "1" ]]; then
  if ! wait_for_mindsdb "$MINDSDB_READY_TIMEOUT" 2; then
    msg="[devnet-smoke] MindsDB not ready within ${MINDSDB_READY_TIMEOUT}s ($MINDSDB_SQL_URL)"
    if [[ "$MINDSDB_REQUIRED" == "1" ]]; then
      echo "$msg" >&2
      exit 4
    fi
    echo "$msg; skipping MindsDB checks." >&2
    MINDSDB_SMOKE=0
  fi
fi
if [[ "$MINDSDB_SMOKE" == "1" ]]; then
  smoke_mindsdb
fi

before_count=$(wc -l "$BACKUP_DIR/telemetry.ndjson" 2>/dev/null | awk '{print $1}' || echo 0)

overall_status=0
if [[ "$RUN_CARGO_SMOKE" == "1" ]]; then
  if [[ ! -f "${CARGO_PROJECT_DIR}/Cargo.toml" ]]; then
    echo "[devnet-smoke] Cargo.toml not found at ${CARGO_PROJECT_DIR}; skipping cargo smoke." >&2
  else
    for bin in "${SMOKE_BIN_LIST[@]}"; do
      echo "[devnet-smoke] Running $bin in dry-run mode for ${DURATION}s"
      set +e
      if [[ "$bin" == "run_strategy" ]]; then
        "$TIMEOUT_CMD" "$DURATION" bash -lc "cd \"${CARGO_PROJECT_DIR}\" && cargo run --features cli --bin run_strategy -- live \\
          --config \"${CONFIG_PATH}\" \\
          --wallet \"${WALLET_PATH}\" \\
          --dry-run \\
          --min-balance 0 \\
          --max-position-pct 0.05 \\
          --max-positions 5 \\
          --profit-targets 3,5,10 \\
          --stop-loss-pct -5 \\
          --kelly-multiplier 0.4 \\
          --min-confidence 0.7 \\
          --enable-sidecar \\
          --sidecar-url \"${SIDECAR_URL}\" \\
          --status-interval-secs 10"
      else
        "$TIMEOUT_CMD" "$DURATION" bash -lc "cd \"${CARGO_PROJECT_DIR}\" && cargo run --features cli --bin \"${bin}\" -- \\
          --config \"${CONFIG_PATH}\" \\
          --wallet \"${WALLET_PATH}\" \\
          --dry-run \\
          --min-balance 0 \\
          --mode xavier \\
          --enable-sidecar \\
          --sidecar-url \"${SIDECAR_URL}\" \\
          --status-interval-secs 10"
      fi
      status=$?
      set -e

      if [[ $status -eq 124 ]]; then
        echo "[devnet-smoke] Timed out after ${DURATION}s (expected)."
        status=0
      fi

      if [[ $status -ne 0 ]]; then
        echo "[devnet-smoke] cargo run for $bin exited with status $status" >&2
        overall_status=$status
      fi
    done
  fi
else
  echo "[devnet-smoke] RUN_CARGO_SMOKE=0; skipping cargo bins."
fi

after_count=$(wc -l "$BACKUP_DIR/telemetry.ndjson" 2>/dev/null | awk '{print $1}' || echo 0)
spool_depth=$(find "$SPOOL_DIR" -maxdepth 1 -type f | wc -l | awk '{print $1}')
if [[ "$after_count" -gt "$before_count" ]]; then
  echo "[devnet-smoke] Telemetry backup updated ($before_count -> $after_count lines)."
else
  echo "[devnet-smoke] No telemetry backup growth detected. Check orchestrator/logs." >&2
fi
echo "[devnet-smoke] Sled spool files: $spool_depth in $SPOOL_DIR"

if [[ -f "$BACKUP_DIR/telemetry.ndjson" ]]; then
  echo "--- tail telemetry.ndjson ---"
  tail -n 5 "$BACKUP_DIR/telemetry.ndjson"
fi

exit $overall_status
if [[ -n "$ORCH_API_KEY" ]]; then
  ORCH_AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
else
  ORCH_AUTH_HEADER=()
fi
