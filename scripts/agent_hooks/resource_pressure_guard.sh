#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: resource_pressure_guard.sh [--min-root-gb N] [--min-external-gb N] [--max-docker-rss-gb N] [--soft]

Lightweight host pressure sampler. No mutation.
USAGE
}

MIN_ROOT_GB="${CONTEXTLATTICE_MIN_ROOT_FREE_GB:-30}"
MIN_EXTERNAL_GB="${CONTEXTLATTICE_MIN_EXTERNAL_FREE_GB:-100}"
MAX_DOCKER_RSS_GB="${CONTEXTLATTICE_MAX_DOCKER_RSS_GB:-32}"
EXTERNAL_PATH="${CONTEXTLATTICE_EXTERNAL_DATA_ROOT:-}"
SOFT=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --min-root-gb) MIN_ROOT_GB="$2"; shift 2 ;;
    --min-external-gb) MIN_EXTERNAL_GB="$2"; shift 2 ;;
    --max-docker-rss-gb) MAX_DOCKER_RSS_GB="$2"; shift 2 ;;
    --external-path) EXTERNAL_PATH="$2"; shift 2 ;;
    --soft) SOFT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

python3 - "$MIN_ROOT_GB" "$MIN_EXTERNAL_GB" "$MAX_DOCKER_RSS_GB" "$EXTERNAL_PATH" "$SOFT" <<'PY'
import json, pathlib, shutil, subprocess, sys
min_root, min_ext, max_docker, external, soft = float(sys.argv[1]), float(sys.argv[2]), float(sys.argv[3]), sys.argv[4], sys.argv[5] == '1'
GB = 1024 ** 3
checks = []

def disk(path, min_free):
    p = pathlib.Path(path)
    if not p.exists():
        return {'path': path, 'exists': False, 'ok': soft, 'free_gb': None, 'min_free_gb': min_free}
    usage = shutil.disk_usage(str(p))
    free = usage.free / GB
    return {'path': path, 'exists': True, 'ok': free >= min_free, 'free_gb': round(free, 2), 'min_free_gb': min_free}

checks.append({'name': 'root_disk', **disk('/', min_root)})
if external:
    checks.append({'name': 'external_disk', **disk(external, min_ext)})
else:
    checks.append({'name': 'external_disk', 'ok': True, 'exists': None, 'path': '', 'free_gb': None, 'min_free_gb': min_ext, 'skipped': True})
try:
    proc = subprocess.run(['ps', '-axo', 'rss=,comm='], text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=5)
    docker_rss_kb = 0
    for line in proc.stdout.splitlines():
        parts = line.strip().split(None, 1)
        if len(parts) == 2 and ('OrbStack' in parts[1] or 'Docker' in parts[1] or 'colima' in parts[1].lower()):
            try:
                docker_rss_kb += int(parts[0])
            except ValueError:
                pass
    docker_gb = docker_rss_kb * 1024 / GB
    checks.append({'name': 'container_runtime_rss', 'ok': docker_gb <= max_docker, 'rss_gb': round(docker_gb, 2), 'max_rss_gb': max_docker})
except Exception as exc:
    checks.append({'name': 'container_runtime_rss', 'ok': soft, 'error': str(exc)})
failures = [c for c in checks if not c.get('ok')]
payload = {'ok': not failures or soft, 'soft': soft, 'checks': checks, 'failures': failures}
print(json.dumps(payload, separators=(',', ':')))
if failures and not soft:
    raise SystemExit(1)
PY
