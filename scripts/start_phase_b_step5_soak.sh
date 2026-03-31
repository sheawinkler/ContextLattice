#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPORT_DIR="${REPO_ROOT}/reports"
mkdir -p "${REPORT_DIR}"

DURATION_HOURS="${SOAK_DURATION_HOURS:-24}"
INTERVAL_SECS="${SOAK_INTERVAL_SECS:-60}"
TIMEOUT_SECS="${SOAK_TIMEOUT_SECS:-30}"
MODES="${SOAK_MODES:-balanced,deep}"
SOURCES="${SOAK_SOURCES:-topic_rollups,memory_bank}"
QUERY="${SOAK_QUERY:-memory bank shodh staged fetch stability}"
MEMORY_BANK_BACKEND="${SOAK_MEMORY_BANK_BACKEND:-shodh_spike}"
USE_LAUNCHD_DEFAULT="false"
if [[ "$(uname -s)" == "Darwin" ]] && command -v launchctl >/dev/null 2>&1; then
  USE_LAUNCHD_DEFAULT="true"
fi
USE_LAUNCHD="${SOAK_USE_LAUNCHD:-${USE_LAUNCHD_DEFAULT}}"
LAUNCHD_LABEL="${SOAK_LAUNCHD_LABEL:-io.contextlattice.retrieval-soak.phaseb-step5}"

usage() {
  cat <<EOF
Usage: scripts/start_phase_b_step5_soak.sh [options]

Options:
  --duration-hours <float>         Soak duration hours (default: ${DURATION_HOURS})
  --interval-secs <float>          Interval between cycles (default: ${INTERVAL_SECS})
  --timeout-secs <float>           Per-request timeout (default: ${TIMEOUT_SECS})
  --modes <csv>                    Retrieval modes (default: ${MODES})
  --sources <csv>                  Sources override (default: ${SOURCES})
  --query <text>                   Query text for soak reads
  --memory-bank-backend <name>     Backend policy memory_bank_backend (default: ${MEMORY_BANK_BACKEND})
  --use-launchd                    Run under launchd submit (default on macOS)
  --no-launchd                     Force classic nohup mode
  -h, --help                       Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --duration-hours)
      DURATION_HOURS="${2:-$DURATION_HOURS}"
      shift 2
      ;;
    --interval-secs)
      INTERVAL_SECS="${2:-$INTERVAL_SECS}"
      shift 2
      ;;
    --timeout-secs)
      TIMEOUT_SECS="${2:-$TIMEOUT_SECS}"
      shift 2
      ;;
    --modes)
      MODES="${2:-$MODES}"
      shift 2
      ;;
    --sources)
      SOURCES="${2:-$SOURCES}"
      shift 2
      ;;
    --query)
      QUERY="${2:-$QUERY}"
      shift 2
      ;;
    --memory-bank-backend)
      MEMORY_BANK_BACKEND="${2:-$MEMORY_BANK_BACKEND}"
      shift 2
      ;;
    --use-launchd)
      USE_LAUNCHD="true"
      shift
      ;;
    --no-launchd)
      USE_LAUNCHD="false"
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

TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_NDJSON="${REPORT_DIR}/retrieval_soak_phase_b_step5_${TS}.ndjson"
OUT_LOG="${REPORT_DIR}/retrieval_soak_phase_b_step5_${TS}.log"
PID_FILE="${REPORT_DIR}/retrieval_soak_phase_b_step5.pid"
LATEST_FILE="${REPORT_DIR}/retrieval_soak_phase_b_step5.latest"
RUNNER_SCRIPT="${REPORT_DIR}/retrieval_soak_phase_b_step5_${TS}.runner.sh"

if [[ -f "${PID_FILE}" ]]; then
  OLD_PID="$(cat "${PID_FILE}" 2>/dev/null || true)"
  if [[ -n "${OLD_PID}" ]] && kill -0 "${OLD_PID}" 2>/dev/null; then
    kill "${OLD_PID}" >/dev/null 2>&1 || true
    sleep 1
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

cat > "${RUNNER_SCRIPT}" <<EOF
#!/usr/bin/env bash
set -euo pipefail
cd "${REPO_ROOT}"
echo "starting soak: output=${OUT_NDJSON} duration_hours=${DURATION_HOURS} interval_secs=${INTERVAL_SECS} sample_docker_memory=True" >> "${OUT_LOG}"
CONTEXTLATTICE_ORCHESTRATOR_API_KEY="${API_KEY}" \
MEMMCP_ORCHESTRATOR_API_KEY="${API_KEY}" \
python3 -u scripts/retrieval_soak_monitor.py \
  --base-url http://127.0.0.1:8075 \
  --project contextlattice \
  --query "${QUERY}" \
  --sources "${SOURCES}" \
  --memory-bank-backend "${MEMORY_BANK_BACKEND}" \
  --modes "${MODES}" \
  --interval-secs "${INTERVAL_SECS}" \
  --duration-hours "${DURATION_HOURS}" \
  --timeout-secs "${TIMEOUT_SECS}" \
  --traffic-class benchmark \
  --sample-docker-memory \
  --output "${OUT_NDJSON}" >> "${OUT_LOG}" 2>&1
if [[ -n "${LAUNCHD_LABEL}" ]] && command -v launchctl >/dev/null 2>&1; then
  launchctl remove "${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
fi
EOF
chmod +x "${RUNNER_SCRIPT}"

PID=""
if [[ "${USE_LAUNCHD}" == "true" ]] && [[ "$(uname -s)" == "Darwin" ]] && command -v launchctl >/dev/null 2>&1; then
  launchctl remove "${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  launchctl submit -l "${LAUNCHD_LABEL}" -- /bin/bash "${RUNNER_SCRIPT}"
  sleep 1
  PID="$(launchctl print "gui/${UID}/${LAUNCHD_LABEL}" 2>/dev/null | awk -F'= ' '/pid =/{print $2; exit}' | tr -d '[:space:]')"
else
  nohup /bin/bash "${RUNNER_SCRIPT}" >/dev/null 2>&1 < /dev/null &
  PID="$!"
fi

if [[ -z "${PID}" ]]; then
  PID="unknown"
fi
echo "${PID}" > "${PID_FILE}"
cat > "${LATEST_FILE}" <<EOF
pid=${PID}
started_at_utc=${TS}
output_ndjson=${OUT_NDJSON}
output_log=${OUT_LOG}
runner_script=${RUNNER_SCRIPT}
memory_bank_backend=${MEMORY_BANK_BACKEND}
modes=${MODES}
duration_hours=${DURATION_HOURS}
interval_secs=${INTERVAL_SECS}
launch_mode=$([[ "${USE_LAUNCHD}" == "true" ]] && echo launchd || echo nohup)
launchd_label=${LAUNCHD_LABEL}
EOF

echo "started phase-b-step5 soak pid=${PID}"
echo "ndjson=${OUT_NDJSON}"
echo "log=${OUT_LOG}"
