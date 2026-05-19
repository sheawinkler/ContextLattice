#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: orbstack_forward_guard.sh [--runtime auto|orbstack|colima] [--url http://127.0.0.1:8075/health]

Ensures Docker runtime responds and repairs stale ContextLattice host-forwarding
through scripts/ensure_docker_runtime.sh when available.
USAGE
}

RUNTIME="${CONTEXTLATTICE_DOCKER_RUNTIME:-orbstack}"
URL="${CONTEXTLATTICE_HEAL_ORCH_URL:-http://127.0.0.1:8075/health}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --runtime) RUNTIME="$2"; shift 2 ;;
    --url) URL="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

ROOT="$(repo_root)"
export CONTEXTLATTICE_DOCKER_RUNTIME="$RUNTIME"
export CONTEXTLATTICE_HEAL_ORCH_FORWARD="${CONTEXTLATTICE_HEAL_ORCH_FORWARD:-1}"
export CONTEXTLATTICE_HEAL_ORCH_URL="$URL"
export DOCKER_PROBE_TIMEOUT_SECS="${DOCKER_PROBE_TIMEOUT_SECS:-60}"
export DOCKER_CMD_TIMEOUT_SECS="${DOCKER_CMD_TIMEOUT_SECS:-60}"
export CONTEXTLATTICE_HEAL_ORCH_TIMEOUT_SECS="${CONTEXTLATTICE_HEAL_ORCH_TIMEOUT_SECS:-60}"

if curl -fsS --max-time "$CONTEXTLATTICE_HEAL_ORCH_TIMEOUT_SECS" "$URL" >/tmp/contextlattice_orbstack_health.$$ 2>/dev/null; then
  if grep -q '"ok":true' /tmp/contextlattice_orbstack_health.$$; then
    rm -f /tmp/contextlattice_orbstack_health.$$
    python3 - "$RUNTIME" "$URL" <<'PY'
import json, sys
print(json.dumps({"ok": True, "runtime": sys.argv[1], "url": sys.argv[2], "message": "orchestrator health already ok"}, separators=(',', ':')))
PY
    exit 0
  fi
fi
rm -f /tmp/contextlattice_orbstack_health.$$

if [[ ! -x "${ROOT}/scripts/ensure_docker_runtime.sh" ]]; then
  fail "missing ${ROOT}/scripts/ensure_docker_runtime.sh"
fi
out_file="$(mktemp "${TMPDIR:-/tmp}/contextlattice_orbstack_guard.XXXXXX.out")"
err_file="$(mktemp "${TMPDIR:-/tmp}/contextlattice_orbstack_guard.XXXXXX.err")"
trap 'rm -f "$out_file" "$err_file"' EXIT
"${ROOT}/scripts/ensure_docker_runtime.sh" >"$out_file" 2>"$err_file" || {
  cat "$err_file" >&2 || true
  fail "docker/runtime guard failed"
}
python3 - "$out_file" "$RUNTIME" "$URL" <<'PY'
import json, pathlib, sys
out_file = pathlib.Path(sys.argv[1])
runtime = sys.argv[2]
url = sys.argv[3]
print(json.dumps({
  "ok": True,
  "runtime": runtime,
  "url": url,
  "message": out_file.read_text(errors='ignore').strip(),
}, separators=(',', ':')))
PY
