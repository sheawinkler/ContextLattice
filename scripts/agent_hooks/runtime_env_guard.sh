#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: runtime_env_guard.sh [--active <envfile>] [--override <envfile>]... [--strict]

Compares runtime-critical keys in active env files against tuner override files.
Fails when stale/conflicting overrides would silently change launch behavior.
USAGE
}

ROOT="$(repo_root)"
ACTIVE=(".env" "config/env/strict_runtime.env")
OVERRIDES=("logs/knob_tuner/overrides.env" "logs/nightly_tuner/overrides.env")
STRICT=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --active) ACTIVE+=("$2"); shift 2 ;;
    --override) OVERRIDES+=("$2"); shift 2 ;;
    --strict) STRICT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
cd "$ROOT"

python3 - "$STRICT" "${ACTIVE[*]}" "${OVERRIDES[*]}" <<'PY'
import json, pathlib, re, sys, time
strict = sys.argv[1] == '1'
active_paths = [p for p in sys.argv[2].split() if p]
override_paths = [p for p in sys.argv[3].split() if p]
critical_exact = {
    'RISK_CIRCUIT_MAX_CONSEC_LOSING_CLOSES',
    'REAL_ALGOTRADER_WS_BUY_GATE_ENABLED',
    'AUTH_REQUIRED',
    'DASHBOARD_AUTH_REQUIRED',
    'REQUIRE_ACTIVE_SUBSCRIPTION',
    'ORCH_SECURITY_STRICT',
    'CONTEXTLATTICE_ENV',
}
critical_prefixes = ('PRE_TRADE_', 'REAL_ALGOTRADER_')
line_re = re.compile(r'^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$')

def parse(path):
    out = {}
    p = pathlib.Path(path)
    if not p.exists():
        return out
    for line in p.read_text(errors='ignore').splitlines():
        s = line.strip()
        if not s or s.startswith('#') or '=' not in s:
            continue
        m = line_re.match(line)
        if not m:
            continue
        key, value = m.group(1), m.group(2).strip().strip('"').strip("'")
        out[key] = value
    return out

def critical(key):
    return key in critical_exact or any(key.startswith(prefix) for prefix in critical_prefixes)

active = {}
active_sources = {}
for path in active_paths:
    for key, value in parse(path).items():
        if critical(key):
            active[key] = value
            active_sources[key] = path

conflicts = []
stale = []
now = time.time()
for path in override_paths:
    p = pathlib.Path(path)
    if not p.exists():
        continue
    age_hours = (now - p.stat().st_mtime) / 3600
    if age_hours > 24:
        stale.append({'path': path, 'age_hours': round(age_hours, 2)})
    for key, value in parse(path).items():
        if not critical(key):
            continue
        if key in active and active[key] != value:
            conflicts.append({'key': key, 'active': active[key], 'active_source': active_sources.get(key), 'override': value, 'override_source': path})

ok = not conflicts and not (strict and stale)
payload = {'ok': ok, 'conflicts': conflicts, 'stale_overrides': stale, 'active_files': active_paths, 'override_files': override_paths}
print(json.dumps(payload, separators=(',', ':')))
if not ok:
    raise SystemExit(1)
PY
