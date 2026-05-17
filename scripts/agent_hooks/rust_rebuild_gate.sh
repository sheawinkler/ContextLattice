#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: rust_rebuild_gate.sh [--base <ref>] [--marker <path>] [--mark] [--clear] [--check]

Detects Rust source changes and writes/checks a rebuild-required marker.
Use before launch to prevent accidental --skip-build after Rust changes.
USAGE
}

BASE="${CONTEXTLATTICE_RUST_REBUILD_BASE:-origin/main}"
MARKER="${CONTEXTLATTICE_RUST_REBUILD_MARKER:-.contextlattice.config/runtime/rebuild-required}"
MARK=0
CLEAR=0
CHECK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE="$2"; shift 2 ;;
    --marker) MARKER="$2"; shift 2 ;;
    --mark) MARK=1; shift ;;
    --clear) CLEAR=1; shift ;;
    --check) CHECK=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

ROOT="$(repo_root)"
cd "$ROOT"
mkdir -p "$(dirname "$MARKER")"

if [[ "$CLEAR" == "1" ]]; then
  rm -f "$MARKER"
  emit_json_kv ok=true marker="$MARKER" action=clear
  exit 0
fi

changed_rs=()
if git rev-parse --verify "$BASE" >/dev/null 2>&1; then
  while IFS= read -r p; do
    [[ "$p" == *.rs ]] && changed_rs+=("$p")
  done < <(git diff --name-only "$BASE"...HEAD; git diff --name-only; git diff --name-only --cached)
else
  while IFS= read -r p; do
    [[ "$p" == *.rs ]] && changed_rs+=("$p")
  done < <(git diff --name-only; git diff --name-only --cached)
fi
mapfile -t changed_rs < <(printf '%s\n' "${changed_rs[@]:-}" | sed '/^$/d' | sort -u)

if [[ "$MARK" == "1" && "${#changed_rs[@]}" -gt 0 ]]; then
  {
    printf 'required=true\n'
    printf 'created_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'base=%s\n' "$BASE"
    printf 'files=%s\n' "$(IFS=,; echo "${changed_rs[*]}")"
  } > "$MARKER"
fi

if [[ "$CHECK" == "1" && -f "$MARKER" ]]; then
  cat "$MARKER" >&2
  fail "full rebuild required before skip-build launch"
fi

python3 - "$MARKER" "${#changed_rs[@]}" "${changed_rs[@]:-}" <<'PY'
import json, pathlib, sys
marker = pathlib.Path(sys.argv[1])
count = int(sys.argv[2])
files = sys.argv[3:]
print(json.dumps({"ok": True, "rust_changed": count > 0, "changed_count": count, "marker": str(marker), "marker_exists": marker.exists(), "files": files}, separators=(",", ":")))
PY
