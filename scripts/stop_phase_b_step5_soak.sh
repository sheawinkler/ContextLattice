#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPORT_DIR="${REPO_ROOT}/reports"
PID_FILE="${REPORT_DIR}/retrieval_soak_phase_b_step5.pid"
LATEST_FILE="${REPORT_DIR}/retrieval_soak_phase_b_step5.latest"

if [[ ! -f "${PID_FILE}" ]]; then
  echo "no active phase-b-step5 soak pid file found."
  exit 0
fi

PID="$(cat "${PID_FILE}" 2>/dev/null || true)"
if [[ -z "${PID}" ]]; then
  echo "pid file empty; removing."
  rm -f "${PID_FILE}"
  exit 0
fi

if kill -0 "${PID}" 2>/dev/null; then
  kill "${PID}" || true
  for _ in $(seq 1 20); do
    if ! kill -0 "${PID}" 2>/dev/null; then
      break
    fi
    sleep 0.5
  done
  if kill -0 "${PID}" 2>/dev/null; then
    kill -9 "${PID}" || true
  fi
  echo "stopped phase-b-step5 soak pid=${PID}"
else
  echo "process not running (pid=${PID})"
fi

rm -f "${PID_FILE}"

if [[ -f "${LATEST_FILE}" ]]; then
  # shellcheck disable=SC1090
  source "${LATEST_FILE}"
  if [[ -n "${output_ndjson:-}" && -f "${output_ndjson}" ]]; then
    SUMMARY_OUT="${output_ndjson%.ndjson}_summary.json"
    python3 "${REPO_ROOT}/scripts/retrieval_soak_summary.py" \
      --input "${output_ndjson}" \
      --output "${SUMMARY_OUT}"
    echo "summary=${SUMMARY_OUT}"
  fi
fi
