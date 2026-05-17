#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: branch_lane_guard.sh [--lane auto|private|public|public-paid]

Enforces lane-specific repository hygiene. Public lanes must not carry private
paths or machine-specific storage references.
USAGE
}

LANE="auto"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

ROOT="$(repo_root)"
cd "$ROOT"
branch="$(git branch --show-current)"
if [[ "$LANE" == "auto" ]]; then
  case "$branch" in
    public/main|public-main|main-public) LANE="public" ;;
    public-paid/main|public-paid-main|public-paid*) LANE="public-paid" ;;
    *) LANE="private" ;;
  esac
fi

blocked=0
if [[ "$LANE" == "public" || "$LANE" == "public-paid" ]]; then
  while IFS= read -r path; do
    case "$path" in
      docs/private/*|private_docs/*|private/*)
        printf '[branch_lane_guard] BLOCK private path in %s lane: %s\n' "$LANE" "$path" >&2
        blocked=1
        ;;
    esac
  done < <(git ls-tree -r --name-only HEAD)
fi

if [[ "$LANE" == "public" ]]; then
  machine_pattern="${CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE:-}"
  if [[ -n "$machine_pattern" ]]; then
    if rg -n --hidden --glob '!.git/**' --glob '!node_modules/**' --glob '!tmp/**' --glob '!archive/**' --glob '!private_docs/**' --glob '!docs/private/**' "$machine_pattern" . >/tmp/contextlattice_lane_hits.txt 2>/dev/null; then
      cat /tmp/contextlattice_lane_hits.txt >&2
      blocked=1
    fi
  fi
fi

[[ "$blocked" == "0" ]] || fail "lane hygiene failed for ${LANE}"
emit_json_kv ok=true lane="$LANE" branch="$branch"
