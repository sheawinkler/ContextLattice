#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: public_leak_guard.sh [--mode changed|all] [--base <ref>] [--public]

Scans for machine-local paths and high-risk secret literals before public sync.
Placeholder env var names are allowed; real-looking secret values are not.
Set CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE for operator-specific path checks.
USAGE
}

MODE="changed"
BASE="origin/main"
PUBLIC=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode) MODE="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --public) PUBLIC=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
ROOT="$(repo_root)"
cd "$ROOT"

files=()
if [[ "$MODE" == "all" ]]; then
  mapfile -t files < <(git ls-files)
else
  if git rev-parse --verify "$BASE" >/dev/null 2>&1; then
    mapfile -t files < <(git diff --name-only "$BASE"...HEAD; git diff --name-only; git diff --name-only --cached)
  else
    mapfile -t files < <(git diff --name-only; git diff --name-only --cached)
  fi
  mapfile -t files < <(printf '%s\n' "${files[@]:-}" | sed '/^$/d' | sort -u)
fi

if [[ "${#files[@]}" -eq 0 ]]; then
  emit_json_kv ok=true scanned=0 mode="$MODE"
  exit 0
fi

python3 - "$PUBLIC" "${files[@]}" <<'PY'
import json, os, pathlib, re, sys
public = sys.argv[1] == '1'
files = sys.argv[2:]
blocked_paths = ('docs/private/', 'private_docs/', 'private/')
text_suffixes = {'.md','.txt','.sh','.py','.go','.rs','.ts','.tsx','.js','.jsx','.json','.yml','.yaml','.env','.example','.html','.css','.toml'}
patterns = [
    ('stripe_live_secret', re.compile(r'\bsk_live_[A-Za-z0-9]{16,}\b')),
    ('stripe_webhook_secret', re.compile(r'\bwhsec_[A-Za-z0-9]{16,}\b')),
    ('github_token', re.compile(r'\b(ghp|github_pat)_[A-Za-z0-9_]{20,}\b')),
    ('private_key_block', re.compile(r'-----BEGIN [A-Z ]*PRIVATE KEY-----')),
    ('aws_access_key', re.compile(r'\bAKIA[0-9A-Z]{16}\b')),
]
personal_path_raw = os.environ.get('CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE', '')
personal_path_pattern = re.compile(personal_path_raw) if personal_path_raw else None
findings = []
for raw in files:
    path = pathlib.Path(raw)
    s = raw.replace('\\','/')
    if public and s.startswith(blocked_paths):
        findings.append({'kind':'private_path', 'file':raw, 'line':0, 'match':s})
        continue
    if not path.exists() or path.is_dir():
        continue
    if path.suffix not in text_suffixes and not path.name.startswith('.env'):
        continue
    try:
        text = path.read_text(errors='ignore')
    except Exception:
        continue
    for i, line in enumerate(text.splitlines(), 1):
        if public and personal_path_pattern and personal_path_pattern.search(line):
            findings.append({'kind': 'personal_path', 'file': raw, 'line': i, 'match': line[:220]})
        for kind, pattern in patterns:
            if pattern.search(line):
                findings.append({'kind': kind, 'file': raw, 'line': i, 'match': line[:220]})
print(json.dumps({'ok': not findings, 'scanned': len(files), 'findings': findings}, separators=(',', ':')))
if findings:
    raise SystemExit(1)
PY
