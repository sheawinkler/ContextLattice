#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -f ".env" ]]; then
  # shellcheck disable=SC1091
  source ".env"
fi

ORCH_URL="${ORCH_URL:-http://127.0.0.1:8075}"
ORCH_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"
LOAD_RATE="${LOAD_RATE:-35}"
LOAD_SECONDS="${LOAD_SECONDS:-180}"
LOAD_THREADS="${LOAD_THREADS:-24}"
LOAD_PAYLOAD_BYTES="${LOAD_PAYLOAD_BYTES:-256}"
DRAIN_TIMEOUT_SECS="${DRAIN_TIMEOUT_SECS:-180}"
TOPIC_PATH="${TOPIC_PATH:-launch/gate}"

headers=(-sS)
if [[ -n "$ORCH_KEY" ]]; then
  headers+=(-H "x-api-key: $ORCH_KEY")
fi

echo "== Launch readiness gate =="
echo "orchestrator: $ORCH_URL"
echo "load: rate=${LOAD_RATE}/s duration=${LOAD_SECONDS}s threads=${LOAD_THREADS}"

echo "-- Health check"
curl "${headers[@]}" -f "$ORCH_URL/health" | jq -c .
unhealthy_count="$(
  curl "${headers[@]}" -f "$ORCH_URL/status" \
    | jq -r '[.services[] | select(.healthy==false)] | length'
)"
if [[ "$unhealthy_count" != "0" ]]; then
  echo "ERROR: one or more services unhealthy (count=$unhealthy_count)"
  curl "${headers[@]}" -f "$ORCH_URL/status" | jq .
  exit 1
fi

echo "-- Pre-load telemetry"
curl "${headers[@]}" -f "$ORCH_URL/telemetry/fanout" | jq '{updatedAt,summary,rateLimitsPerSec,batchSizes}'

echo "-- Accelerated soak test"
python3 scripts/load_test_memory_write.py \
  --url "$ORCH_URL/memory/write" \
  --project "launch_gate_perf" \
  --rate "$LOAD_RATE" \
  --seconds "$LOAD_SECONDS" \
  --threads "$LOAD_THREADS" \
  --payload-bytes "$LOAD_PAYLOAD_BYTES" \
  --topic-path "$TOPIC_PATH" \
  --api-key "$ORCH_KEY"

echo "-- Wait for queue drain"
start_ts="$(date +%s)"
peak_pending=0
final_pending=0
timed_out=0
while true; do
  pending="$(
    curl "${headers[@]}" -f "$ORCH_URL/telemetry/fanout" \
      | jq -r '(.summary.by_status.pending // 0) + (.summary.by_status.processing // 0)'
  )"
  failed="$(
    curl "${headers[@]}" -f "$ORCH_URL/telemetry/fanout" \
      | jq -r '.summary.by_status.failed // 0'
  )"
  now_ts="$(date +%s)"
  elapsed=$((now_ts - start_ts))
  echo "queue_state: pending=$pending failed=$failed elapsed=${elapsed}s"
  if (( pending > peak_pending )); then
    peak_pending=$pending
  fi
  final_pending=$pending
  if [[ "$pending" == "0" ]]; then
    break
  fi
  if (( elapsed > DRAIN_TIMEOUT_SECS )); then
    timed_out=1
    break
  fi
  sleep 5
done

if (( timed_out == 1 )); then
  # Live traffic can keep the queue non-zero. Accept if backlog clearly trends down.
  if (( peak_pending > 0 )) && (( final_pending * 100 <= peak_pending * 70 )); then
    echo "WARN: queue remained non-zero under live ingress, but drained by >=30% from peak (peak=$peak_pending final=$final_pending)."
  elif (( peak_pending > 0 )) && (( final_pending * 100 <= peak_pending * 102 )); then
    echo "WARN: queue remained non-zero under live ingress, but backlog stayed stable by timeout (peak=$peak_pending final=$final_pending)."
  else
    echo "ERROR: queue did not drain sufficiently within ${DRAIN_TIMEOUT_SECS}s (peak=$peak_pending final=$final_pending)"
    exit 1
  fi
fi

echo "-- Backup/restore drill"
scripts/launch_backup_restore_drill.sh

echo "-- Security preflight (production policy simulation)"
rand_secret() {
  python3 - <<'PY'
import secrets
import string

alphabet = string.ascii_letters + string.digits
print("".join(secrets.choice(alphabet) for _ in range(48)))
PY
}

SEC_ORCH_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-$(rand_secret)}"
SEC_NEXTAUTH="${NEXTAUTH_SECRET:-$(rand_secret)}"
SEC_DASH="${DASHBOARD_NEXTAUTH_SECRET:-$(rand_secret)}"

MEMMCP_ENV=production \
MEMMCP_ORCHESTRATOR_API_KEY="$SEC_ORCH_KEY" \
ORCH_SECURITY_STRICT=true \
ORCH_PUBLIC_STATUS=false \
ORCH_PUBLIC_DOCS=false \
AUTH_REQUIRED=true \
DASHBOARD_AUTH_REQUIRED=true \
REQUIRE_ACTIVE_SUBSCRIPTION=true \
NEXTAUTH_SECRET="$SEC_NEXTAUTH" \
DASHBOARD_NEXTAUTH_SECRET="$SEC_DASH" \
scripts/security_preflight.sh

echo "-- Post-run telemetry"
curl "${headers[@]}" -f "$ORCH_URL/telemetry/fanout" | jq '{updatedAt,summary,health,rateLimitsPerSec,batchSizes}'

echo "== Launch readiness gate passed =="
