#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: git_lane_guard.sh [--branch <name>] [--upstream <ref>] [--require-clean] [--require-synced]

Checks current git branch, dirtiness, upstream existence, and optional exact sync.
No mutation.
USAGE
}

EXPECT_BRANCH=""
UPSTREAM=""
REQUIRE_CLEAN=0
REQUIRE_SYNCED=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --branch) EXPECT_BRANCH="$2"; shift 2 ;;
    --upstream) UPSTREAM="$2"; shift 2 ;;
    --require-clean) REQUIRE_CLEAN=1; shift ;;
    --require-synced) REQUIRE_SYNCED=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

ROOT="$(repo_root)"
cd "$ROOT"
branch="$(git branch --show-current)"
[[ -n "$branch" ]] || fail "detached HEAD"
if [[ -n "$EXPECT_BRANCH" && "$branch" != "$EXPECT_BRANCH" ]]; then
  fail "expected branch ${EXPECT_BRANCH}, got ${branch}"
fi
if [[ "$REQUIRE_CLEAN" == "1" && -n "$(git status --short)" ]]; then
  git status --short >&2
  fail "working tree not clean"
fi
if [[ -z "$UPSTREAM" ]]; then
  UPSTREAM="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
fi
if [[ -n "$UPSTREAM" ]]; then
  git rev-parse --verify "$UPSTREAM" >/dev/null 2>&1 || fail "missing upstream ref: $UPSTREAM"
  ahead="$(git rev-list --count "${UPSTREAM}..HEAD")"
  behind="$(git rev-list --count "HEAD..${UPSTREAM}")"
  if [[ "$REQUIRE_SYNCED" == "1" && ( "$ahead" != "0" || "$behind" != "0" ) ]]; then
    fail "branch ${branch} not synced with ${UPSTREAM} (ahead=${ahead}, behind=${behind})"
  fi
else
  ahead="unknown"
  behind="unknown"
fi
emit_json_kv ok=true branch="$branch" upstream="$UPSTREAM" ahead="$ahead" behind="$behind" clean="$([[ -z "$(git status --short)" ]] && echo true || echo false)"
