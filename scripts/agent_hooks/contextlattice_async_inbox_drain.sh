#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: contextlattice_async_inbox_drain.sh [contextlattice_async_inbox_drain args...]

Generic agent hook entrypoint for async continuation delivery. It drains the
bounded ContextLattice session inbox after a normal tool boundary and exits
successfully even when ContextLattice is unavailable.
USAGE
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

contextlattice_env

tool=""
if command -v contextlattice_async_inbox_drain >/dev/null 2>&1; then
  tool="$(command -v contextlattice_async_inbox_drain)"
else
  candidate="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}/bin/contextlattice_async_inbox_drain"
  if [[ -x "$candidate" ]]; then
    tool="$candidate"
  fi
fi

if [[ -z "$tool" ]]; then
  exit 0
fi

timeout="${CONTEXTLATTICE_ASYNC_INBOX_TIMEOUT_SECS:-1.5}"
max_items="${CONTEXTLATTICE_ASYNC_INBOX_MAX_ITEMS:-1}"

set +e
"$tool" --timeout "$timeout" --max-items "$max_items" "$@"
exit 0
