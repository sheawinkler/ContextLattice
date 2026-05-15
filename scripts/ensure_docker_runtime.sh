#!/usr/bin/env bash
set -euo pipefail

PREFERRED_RUNTIME="${CONTEXTLATTICE_DOCKER_RUNTIME:-auto}"
DOCKER_PROBE_TIMEOUT_SECS="${DOCKER_PROBE_TIMEOUT_SECS:-8}"

probe_docker() {
  python3 - "$DOCKER_PROBE_TIMEOUT_SECS" <<'PY'
import subprocess, sys
timeout = float(sys.argv[1])
try:
    proc = subprocess.run(
        ["docker", "version", "--format", "{{.Server.Version}}"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=timeout,
        check=False,
    )
    if proc.returncode != 0 or not proc.stdout.strip():
        raise SystemExit(1)
except Exception:
    raise SystemExit(1)
raise SystemExit(0)
PY
}

ensure_colima() {
  if ! command -v colima >/dev/null 2>&1; then
    echo "[docker-runtime] colima not installed and docker engine probe failed." >&2
    return 1
  fi

  local colima_state
  colima_state="$(colima status 2>/dev/null || true)"
  if [[ "$colima_state" != *"colima is running"* ]]; then
    echo "[docker-runtime] starting colima..."
    colima start >/dev/null
  fi

  docker context use colima >/dev/null 2>&1 || true
  probe_docker
}

ensure_orbstack() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "[docker-runtime] docker CLI missing." >&2
    return 1
  fi

  docker context use orbstack >/dev/null 2>&1 || true
  probe_docker
}

main() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "[docker-runtime] docker CLI missing." >&2
    exit 1
  fi

  if probe_docker; then
    echo "[docker-runtime] docker engine healthy (context=$(docker context show 2>/dev/null || echo unknown))."
    exit 0
  fi

  case "$PREFERRED_RUNTIME" in
    colima)
      ensure_colima || { echo "[docker-runtime] failed to recover docker via colima." >&2; exit 1; }
      ;;
    orbstack)
      ensure_orbstack || { echo "[docker-runtime] failed to recover docker via orbstack." >&2; exit 1; }
      ;;
    auto|"")
      if ensure_colima; then
        :
      elif ensure_orbstack; then
        :
      else
        echo "[docker-runtime] failed to recover docker engine via auto runtime fallback." >&2
        exit 1
      fi
      ;;
    *)
      echo "[docker-runtime] unsupported CONTEXTLATTICE_DOCKER_RUNTIME=${PREFERRED_RUNTIME}" >&2
      exit 2
      ;;
  esac

  echo "[docker-runtime] recovered (context=$(docker context show 2>/dev/null || echo unknown))."
}

main "$@"
