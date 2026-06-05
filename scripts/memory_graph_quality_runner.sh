#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

args=()
if [[ -n "${CONTEXTLATTICE_GRAPH_QUALITY_PROJECT:-}" ]]; then
  args+=(--project "${CONTEXTLATTICE_GRAPH_QUALITY_PROJECT}")
else
  args+=(--all-projects)
fi

args+=(--profile "${CONTEXTLATTICE_GRAPH_QUALITY_PROFILE:-balanced}")
args+=(--corpus "${CONTEXTLATTICE_GRAPH_QUALITY_CORPUS:-auto}")
args+=(--max-projects "${CONTEXTLATTICE_GRAPH_QUALITY_MAX_PROJECTS:-12}")
args+=(--max-write-edges "${CONTEXTLATTICE_GRAPH_QUALITY_MAX_WRITE_EDGES:-500}")
args+=(--max-candidates "${CONTEXTLATTICE_GRAPH_QUALITY_MAX_CANDIDATES:-20000}")
args+=(--inferred-scan-limit "${CONTEXTLATTICE_GRAPH_QUALITY_INFERRED_SCAN_LIMIT:-5000}")
args+=(--stale-inferred-days "${CONTEXTLATTICE_GRAPH_QUALITY_STALE_INFERRED_DAYS:-30}")
args+=(--timeout "${CONTEXTLATTICE_GRAPH_QUALITY_TIMEOUT_SECS:-180}")
args+=(--pretty)

if [[ "${CONTEXTLATTICE_GRAPH_QUALITY_ALLOW_DISK:-0}" == "1" ]]; then
  args+=(--allow-disk)
fi
if [[ "${CONTEXTLATTICE_GRAPH_QUALITY_WRITE:-0}" == "1" ]]; then
  args+=(--write)
  if [[ -n "${CONTEXTLATTICE_GRAPH_QUALITY_PROJECT:-}" ]]; then
    args+=(--confirm-repair "${CONTEXTLATTICE_GRAPH_QUALITY_PROJECT}")
  else
    args+=(--confirm-repair ALL_PROJECTS)
  fi
fi

DEFAULT_CACHE_ROOT="${CONTEXTLATTICE_CACHE_ROOT:-${XDG_CACHE_HOME:-$HOME/.cache}/contextlattice}"
export PYTHONPYCACHEPREFIX="${PYTHONPYCACHEPREFIX:-${DEFAULT_CACHE_ROOT}/pycache}"
exec scripts/agent/memory-graph-quality "${args[@]}"
