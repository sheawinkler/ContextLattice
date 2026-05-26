#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: branch_lane_guard.sh [--lane auto|private|public|public-paid] [--ref <git-ref>]

Enforces lane-specific repository hygiene. Public lanes must not carry private
paths or machine-specific storage references.

Lane model:
  private     origin/main; everything allowed.
  public      public/main; OSS-safe only.
  public-paid public-paid/main; premium paid lane, including paid docs.
USAGE
}

LANE="auto"
REF="HEAD"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

ROOT="$(repo_root)"
cd "$ROOT"
branch="$(git branch --show-current)"
case "$REF" in
  origin/public/main|refs/remotes/origin/public/main)
    fail "deprecated public production alias: use refs/remotes/public/main"
    ;;
  origin/public-paid/main|refs/remotes/origin/public-paid/main)
    fail "deprecated public-paid production alias: use refs/remotes/public-paid/main"
    ;;
esac
git rev-parse --verify "$REF" >/dev/null 2>&1 || fail "missing ref: $REF"
if [[ "$LANE" == "auto" ]]; then
  lane_source="$branch"
  [[ "$REF" != "HEAD" ]] && lane_source="$REF"
  case "$lane_source" in
    */public/main|public/main|public-main|main-public) LANE="public" ;;
    */public-paid/main|public-paid/main|public-paid-main|public-paid*) LANE="public-paid" ;;
    *) LANE="private" ;;
  esac
fi

blocked=0
if [[ "$LANE" == "public" ]]; then
  while IFS= read -r path; do
    case "$path" in
      docs/private/*|private_docs/*|private/*)
        printf '[branch_lane_guard] BLOCK private path in %s lane: %s\n' "$LANE" "$path" >&2
        blocked=1
        ;;
    esac
  done < <(git ls-tree -r --name-only "$REF")
fi

if [[ "$LANE" == "public-paid" ]]; then
  while IFS= read -r path; do
    case "$path" in
      private_docs/*|private/*|*.private.md)
        printf '[branch_lane_guard] BLOCK private-only path in %s lane: %s\n' "$LANE" "$path" >&2
        blocked=1
        ;;
    esac
  done < <(git ls-tree -r --name-only "$REF")
fi

if [[ "$LANE" == "public" ]]; then
  machine_pattern="${CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE:-}"
  if [[ -n "$machine_pattern" ]]; then
    if [[ "$REF" == "HEAD" ]]; then
      if rg -n --hidden --glob '!.git/**' --glob '!node_modules/**' --glob '!tmp/**' --glob '!archive/**' --glob '!private_docs/**' --glob '!docs/private/**' "$machine_pattern" . >/tmp/contextlattice_lane_hits.txt 2>/dev/null; then
        cat /tmp/contextlattice_lane_hits.txt >&2
        blocked=1
      fi
    else
      if git grep -n -I -E "$machine_pattern" "$REF" -- . ':(exclude)docs/private/**' ':(exclude)private_docs/**' ':(exclude)node_modules/**' ':(exclude)tmp/**' >/tmp/contextlattice_lane_hits.txt 2>/dev/null; then
        cat /tmp/contextlattice_lane_hits.txt >&2
        blocked=1
      fi
    fi
  fi
fi

[[ "$blocked" == "0" ]] || fail "lane hygiene failed for ${LANE}"
emit_json_kv ok=true lane="$LANE" branch="$branch" ref="$REF"
