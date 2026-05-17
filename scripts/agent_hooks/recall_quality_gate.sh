#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: recall_quality_gate.sh [--soft] [--saved] [--telemetry]

Runs recall quality checks before release-sensitive work. Defaults to saved eval
plus telemetry monitor if endpoints are available.
USAGE
}

SOFT=0
RUN_SAVED=1
RUN_TELEMETRY=1
TIMEOUT="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-30}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --soft) SOFT=1; shift ;;
    --saved) RUN_SAVED=1; RUN_TELEMETRY=0; shift ;;
    --telemetry) RUN_SAVED=0; RUN_TELEMETRY=1; shift ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
contextlattice_env
BASE="${CONTEXTLATTICE_ORCHESTRATOR_URL%/}"
python3 - "$BASE" "$TIMEOUT" "$RUN_SAVED" "$RUN_TELEMETRY" "$SOFT" <<'PY'
import json, sys, urllib.request, urllib.error
base, timeout, run_saved, run_tel, soft = sys.argv[1], float(sys.argv[2]), sys.argv[3] == '1', sys.argv[4] == '1', sys.argv[5] == '1'
checks = []

def request(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, method=method, headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            text = r.read().decode()
            return {'ok': 200 <= r.status < 300, 'status': r.status, 'body': json.loads(text) if text else {}}
    except Exception as exc:
        return {'ok': False, 'status': None, 'error': str(exc)}

if run_saved:
    checks.append(('saved', request('POST', '/memory/recall/evaluate/saved', {})))
if run_tel:
    checks.append(('monitor', request('GET', '/telemetry/recall/monitor')))
    checks.append(('tuning', request('GET', '/telemetry/recall/tuning')))
failures = [{'name': n, **r} for n, r in checks if not r.get('ok')]
payload = {'ok': not failures or soft, 'soft': soft, 'checks': [{'name': n, 'ok': r.get('ok'), 'status': r.get('status'), 'error': r.get('error')} for n, r in checks], 'failures': failures}
print(json.dumps(payload, separators=(',', ':')))
if failures and not soft:
    raise SystemExit(1)
PY
