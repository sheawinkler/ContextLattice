#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: contextlattice_preflight.sh [agent] [project] [topic_path]

Runs the standard ContextLattice turn-start preflight with pinned local endpoint
and stable agent identity. Emits the underlying JSON payload.

Defaults:
  agent      codex
  project    contextlattice
  topic_path runbooks/codex-integration
USAGE
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

contextlattice_env
ROOT="$(repo_root)"
AGENT="${1:-codex}"
PROJECT="${2:-contextlattice}"
TOPIC="${3:-runbooks/codex-integration}"

cd "$ROOT"
if [[ -x "${ROOT}/scripts/agent_orchestration.sh" ]]; then
  exec "${ROOT}/scripts/agent_orchestration.sh" preflight "$PROJECT" "$TOPIC" "$AGENT"
fi
exec python3 "${ROOT}/scripts/agent_orchestration.py" preflight "$PROJECT" "$TOPIC" "$AGENT"
