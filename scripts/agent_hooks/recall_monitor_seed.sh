#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: recall_monitor_seed.sh [--base-url URL] [--timeout secs] [--limit N] [--force] [--soft]

Seeds recall monitor once on cold start so tuning has at least one sample.
Flow:
  1) GET /telemetry/recall/monitor?limit=N
  2) If empty (or --force), POST /telemetry/recall/monitor/snapshot
  3) GET /telemetry/recall/monitor?limit=N and verify sample present
USAGE
}

BASE_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
TIMEOUT="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-20}"
LIMIT=3
FORCE=0
SOFT=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url) BASE_URL="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --limit) LIMIT="$2"; shift 2 ;;
    --force) FORCE=1; shift ;;
    --soft) SOFT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

contextlattice_env
BASE_URL="${BASE_URL%/}"

python3 - "$BASE_URL" "$TIMEOUT" "$LIMIT" "$FORCE" "$SOFT" <<'PY'
import json
import os
import sys
import urllib.error
import urllib.request

base, timeout, limit, force, soft = sys.argv[1], float(sys.argv[2]), max(1, int(sys.argv[3])), sys.argv[4] == '1', sys.argv[5] == '1'


def request(method, path, body=None):
    payload = None if body is None else json.dumps(body).encode()
    headers = {'Content-Type': 'application/json'}
    api_key = os.environ.get('CONTEXTLATTICE_ORCHESTRATOR_API_KEY', '').strip()
    if api_key:
        headers['x-api-key'] = api_key
    req = urllib.request.Request(
        base + path,
        data=payload,
        method=method,
        headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            parsed = json.loads(raw) if raw else {}
            return {'ok': True, 'status': r.status, 'body': parsed}
    except urllib.error.HTTPError as e:
        text = e.read().decode(errors='ignore')
        try:
            parsed = json.loads(text) if text else {}
        except Exception:
            parsed = {'raw': text[-600:]}
        return {'ok': False, 'status': e.code, 'body': parsed, 'error': f'http_{e.code}'}
    except Exception as exc:
        return {'ok': False, 'status': None, 'error': str(exc)}

before = request('GET', f'/telemetry/recall/monitor?limit={limit}')
before_body = before.get('body') if isinstance(before.get('body'), dict) else {}
before_count = int(before_body.get('count') or 0)
if before_count <= 0 and isinstance(before_body.get('history'), list):
    before_count = len(before_body['history'])

seeded = False
snapshot = None
seed_error = None
if force or before_count == 0:
    snapshot = request('POST', '/telemetry/recall/monitor/snapshot', {})
    if snapshot.get('ok') and snapshot.get('status') == 200:
        seeded = True
    else:
        seed_error = snapshot.get('error') or f'status_{snapshot.get("status")}'

after = request('GET', f'/telemetry/recall/monitor?limit={limit}')
after_body = after.get('body') if isinstance(after.get('body'), dict) else {}
after_count = int(after_body.get('count') or 0)
if after_count <= 0 and isinstance(after_body.get('history'), list):
    after_count = len(after_body['history'])

latest_sample = None
if isinstance(after_body.get('history'), list) and after_body['history']:
    first = after_body['history'][0]
    if isinstance(first, dict):
        latest_sample = first.get('timestamp')

ok = bool(before.get('ok')) and bool(after.get('ok')) and ((before_count > 0) or (after_count > 0))
if seed_error and not soft:
    ok = False

payload = {
    'ok': ok or soft,
    'soft': soft,
    'base_url': base,
    'forced': force,
    'beforeCount': before_count,
    'afterCount': after_count,
    'seeded': seeded,
    'seedError': seed_error,
    'latestSampleTimestamp': latest_sample,
    'monitorStatus': {
        'before': before.get('status'),
        'after': after.get('status'),
        'snapshot': snapshot.get('status') if isinstance(snapshot, dict) else None,
    },
}
print(json.dumps(payload, separators=(',', ':')))
if not payload['ok'] and not soft:
    raise SystemExit(1)
PY
