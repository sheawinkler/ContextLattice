#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPORT_DIR="${REPO_ROOT}/reports"
mkdir -p "${REPORT_DIR}"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_NDJSON="${REPORT_DIR}/retrieval_soak_phase_b_step5_${TS}.ndjson"
OUT_LOG="${REPORT_DIR}/retrieval_soak_phase_b_step5_${TS}.log"
PID_FILE="${REPORT_DIR}/retrieval_soak_phase_b_step5.pid"
LATEST_FILE="${REPORT_DIR}/retrieval_soak_phase_b_step5.latest"

if [[ -f "${PID_FILE}" ]]; then
  OLD_PID="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if [[ -n "${OLD_PID}" ]] && kill -0 "${OLD_PID}" 2>/dev/null; then
    echo "phase-b-step5 soak already running (pid=${OLD_PID}); stop it first."
    exit 1
  fi
fi

cd "${REPO_ROOT}"
API_KEY="${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-${MEMMCP_ORCHESTRATOR_API_KEY:-}}"
if [[ -z "${API_KEY}" && -f "${REPO_ROOT}/.env" ]]; then
  API_KEY="$(
    grep -E '^(CONTEXTLATTICE_ORCHESTRATOR_API_KEY|MEMMCP_ORCHESTRATOR_API_KEY)=' "${REPO_ROOT}/.env" \
      | tail -n 1 \
      | cut -d= -f2- \
      | sed -e 's/^"//' -e 's/"$//'
  )"
fi

CMD=(
  python3 -u scripts/retrieval_soak_monitor.py
  --base-url http://127.0.0.1:8075
  --project contextlattice
  --query "memory bank quickwit staged fetch stability"
  --sources topic_rollups,memory_bank
  --memory-bank-backend quickwit_spike
  --modes balanced,deep
  --interval-secs 60
  --duration-hours 24
  --timeout-secs 30
  --traffic-class benchmark
  --sample-docker-memory
  --output "${OUT_NDJSON}"
)

CONTEXTLATTICE_ORCHESTRATOR_API_KEY="${API_KEY}" \
MEMMCP_ORCHESTRATOR_API_KEY="${API_KEY}" \
nohup "${CMD[@]}" \
  > "${OUT_LOG}" 2>&1 &

PID=$!
echo "${PID}" > "${PID_FILE}"
cat > "${LATEST_FILE}" <<EOF
pid=${PID}
started_at_utc=${TS}
output_ndjson=${OUT_NDJSON}
output_log=${OUT_LOG}
EOF

echo "started phase-b-step5 soak pid=${PID}"
echo "ndjson=${OUT_NDJSON}"
echo "log=${OUT_LOG}"
