#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: native_endpoint_smoke.sh [--base-url URL] [--project <name>] [--timeout secs] [--soft]

Fast smoke for critical go-native endpoints after restart/redeploy.
Checks:
  - GET  /health
  - GET  /status
  - POST /memory/search
  - GET  /telemetry/recall
  - GET  /telemetry/recall/monitor
  - GET  /telemetry/recall/tuning
USAGE
}

BASE_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
PROJECT="${CONTEXTLATTICE_HOOK_PROJECT:-contextlattice}"
TIMEOUT="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-20}"
SOFT=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url) BASE_URL="$2"; shift 2 ;;
    --project|-p) PROJECT="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --soft) SOFT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

contextlattice_env
BASE_URL="${BASE_URL%/}"

python3 - "$BASE_URL" "$PROJECT" "$TIMEOUT" "$SOFT" <<'PY'
import json
import os
import sys
import time
import urllib.error
import urllib.request

base, project, timeout, soft = sys.argv[1], sys.argv[2], float(sys.argv[3]), sys.argv[4] == '1'

checks = []

def request(method, path, body=None, retries=2):
    payload = None if body is None else json.dumps(body).encode()
    attempt_error = None
    headers = {'Content-Type': 'application/json'}
    api_key = os.environ.get('CONTEXTLATTICE_ORCHESTRATOR_API_KEY', '').strip()
    if api_key:
        headers['x-api-key'] = api_key
    for attempt in range(max(1, retries)):
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
            attempt_error = str(exc)
            if attempt + 1 < retries:
                time.sleep(0.4)
                continue
            return {'ok': False, 'status': None, 'error': attempt_error}
    if attempt_error is not None:
        return {'ok': False, 'status': None, 'error': attempt_error}
    return {'ok': False, 'status': None, 'error': 'request_failed'}

checks.append(('health', request('GET', '/health')))
checks.append(('status', request('GET', '/status')))
checks.append((
    'memory_search',
    request('POST', '/memory/search', {
        'project': project,
        'query': 'native endpoint smoke check',
        'limit': 1,
        'include_retrieval_debug': True,
    }),
))
checks.append(('telemetry_recall', request('GET', '/telemetry/recall')))
checks.append(('telemetry_recall_monitor', request('GET', '/telemetry/recall/monitor?limit=1')))
checks.append(('telemetry_recall_tuning', request('GET', '/telemetry/recall/tuning?lookback_hours=24&min_samples=1&max_samples=24')))

results = []
for name, res in checks:
    ok = bool(res.get('ok'))
    body = res.get('body') if isinstance(res.get('body'), dict) else {}
    status = res.get('status')
    if name == 'health':
        ok = ok and status == 200 and bool(body.get('ok'))
    elif name == 'status':
        ok = ok and status == 200 and bool(body.get('ok'))
    elif name == 'memory_search':
        ok = ok and status == 200 and isinstance(body.get('results'), list)
    elif name == 'telemetry_recall':
        ok = ok and status == 200 and isinstance(body.get('quality'), dict)
    elif name == 'telemetry_recall_monitor':
        ok = ok and status == 200 and isinstance(body.get('history'), list)
    elif name == 'telemetry_recall_tuning':
        ok = ok and status == 200 and isinstance(body.get('recommended'), dict)
    results.append({
        'name': name,
        'ok': ok,
        'status': status,
        'error': res.get('error'),
    })

failures = [item for item in results if not item.get('ok')]
payload = {
    'ok': (not failures) or soft,
    'soft': soft,
    'base_url': base,
    'project': project,
    'checks': results,
    'failures': failures,
}
print(json.dumps(payload, separators=(',', ':')))
if failures and not soft:
    raise SystemExit(1)
PY
