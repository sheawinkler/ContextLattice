#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: command_output_budget.sh [--max-bytes N] -- <command> [args...]

Runs a command, stores full stdout/stderr under /tmp or $CONTEXTLATTICE_ARTIFACT_DIR,
and prints a bounded tail plus artifact paths. Useful for agents reading noisy logs.
USAGE
}

MAX_BYTES="${CONTEXTLATTICE_COMMAND_OUTPUT_MAX_BYTES:-12000}"
ARTIFACT_DIR="${CONTEXTLATTICE_ARTIFACT_DIR:-/tmp/contextlattice-agent-artifacts}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --max-bytes) MAX_BYTES="$2"; shift 2 ;;
    --artifact-dir) ARTIFACT_DIR="$2"; shift 2 ;;
    --) shift; break ;;
    -h|--help) usage; exit 0 ;;
    *) break ;;
  esac
done
[[ $# -gt 0 ]] || fail "command required"
mkdir -p "$ARTIFACT_DIR"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
base="${ARTIFACT_DIR}/command-${stamp}-$$"
out="${base}.stdout"
err="${base}.stderr"
set +e
"$@" >"$out" 2>"$err"
code=$?
set -e
python3 - "$MAX_BYTES" "$code" "$out" "$err" "$*" <<'PY'
import json, pathlib, sys
max_bytes, code, out, err, cmd = int(sys.argv[1]), int(sys.argv[2]), pathlib.Path(sys.argv[3]), pathlib.Path(sys.argv[4]), sys.argv[5]
def tail(path):
    data = path.read_bytes() if path.exists() else b''
    truncated = len(data) > max_bytes
    if truncated:
        data = data[-max_bytes:]
    return {'path': str(path), 'bytes': path.stat().st_size if path.exists() else 0, 'truncated': truncated, 'tail': data.decode(errors='replace')}
print(json.dumps({'ok': code == 0, 'exit_code': code, 'command': cmd, 'stdout': tail(out), 'stderr': tail(err)}, separators=(',', ':')))
raise SystemExit(code)
PY
