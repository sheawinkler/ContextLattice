#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

usage() {
  cat <<'USAGE'
Usage: scripts/public_sync_guard.sh [target-remote] [target-branch] [blocklist-file]

Checks worktree changes against the target lane and fails when a changed path
matches a private or paid-only blocklist entry.
USAGE
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
esac

TARGET_REMOTE="${1:-public}"
TARGET_BRANCH="${2:-main}"
TARGET_REF="refs/remotes/${TARGET_REMOTE}/${TARGET_BRANCH}"
BLOCKLIST_FILE="${3:-${PUBLIC_SYNC_GUARD_BLOCKLIST_FILE:-${REPO_ROOT}/config/public_sync_blocklist.txt}}"

if ! git rev-parse --verify "${TARGET_REF}" >/dev/null 2>&1; then
  echo "[guard] missing ref ${TARGET_REF}; run: git fetch ${TARGET_REMOTE}" >&2
  exit 2
fi

changed=()
if git merge-base --is-ancestor "${TARGET_REF}" HEAD >/dev/null 2>&1 || \
   git merge-base --is-ancestor HEAD "${TARGET_REF}" >/dev/null 2>&1; then
  mapfile -t changed < <(
    git diff --name-only "${TARGET_REF}"...HEAD
    git diff --name-only
    git diff --name-only --cached
  )
else
  echo "[guard] warning: no merge-base with ${TARGET_REF}; using fail-closed scan." >&2
  mapfile -t changed < <(
    git ls-tree -r --name-only HEAD
    git diff --name-only
    git diff --name-only --cached
  )
fi
mapfile -t changed < <(printf '%s\n' "${changed[@]:-}" | sed '/^$/d' | sort -u)

if [ "${#changed[@]}" -eq 0 ]; then
  echo "[guard] no diffs against ${TARGET_REMOTE}/${TARGET_BRANCH}."
  exit 0
fi

patterns=()
if [ -f "$BLOCKLIST_FILE" ]; then
  while IFS= read -r line; do
    line="${line%%#*}"
    line="$(echo "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    if [ -z "$line" ]; then
      continue
    fi
    patterns+=("$line")
  done <"$BLOCKLIST_FILE"
fi

if [ "${#patterns[@]}" -eq 0 ]; then
  patterns=(
    "docs/private/*"
    "private/*"
    "*.private.md"
    "contextlattice-dashboard/app/api/telemetry/pro-analytics/*"
    "contextlattice-dashboard/app/api/support/diagnostics/*"
    "contextlattice-dashboard/components/EntrancingDashboard.tsx"
    "services/gateway-go/entitlement_machine_binding.go"
  )
fi

if [ "$TARGET_REMOTE" = "public-paid" ]; then
  # Paid implementation is expected in public-paid. Only private-development,
  # cross-private automation, and non-public documentation remain forbidden.
  patterns=(
    "docs/private/*"
    "private/*"
    "*.private.md"
    ".github/workflows/capability-parity.yml"
    "config/env/premium_dev.env"
    "scripts/agent/audit-private-paid-superset"
    "scripts/launch_private_dev.sh"
    "scripts/setup_paid_local_env.sh"
    "scripts/tests/test_private_dev_posture.py"
    ".backup/*"
    "dev/backups/*"
    "development/*"
    "logs/*"
    "*.pid"
    "*.bak"
    "*.bak.*"
    "*.tmp"
    ".env"
    "*/.env"
    ".env_*"
    "*/.env_*"
    ".ops/snapshots/*"
  )
fi

blocked=0
for p in "${changed[@]}"; do
  # A blocked path removed by the candidate is the desired cleanup. Check the
  # candidate worktree/index/HEAD rather than rejecting a path solely because
  # it appears in the diff against the target lane.
  if [[ ! -e "$p" && ! -L "$p" ]]; then
    if ! git cat-file -e "HEAD:${p}" 2>/dev/null || \
       ! git diff --quiet -- "$p" || \
       ! git diff --cached --quiet -- "$p"; then
      continue
    fi
  fi
  for pattern in "${patterns[@]}"; do
    # shellcheck disable=SC2254 # Blocklist entries are intentional globs.
    case "$p" in
      $pattern)
        echo "[guard] BLOCKED by pattern '$pattern': $p" >&2
        blocked=1
        break
        ;;
    esac
  done
done

if [ "$blocked" -ne 0 ]; then
  echo "[guard] stop: remove private files from public sync PR." >&2
  exit 1
fi

echo "[guard] pass: no lane-blocked paths detected against ${TARGET_REF}."
