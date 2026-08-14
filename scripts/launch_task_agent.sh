#!/bin/zsh -f
set -euo pipefail

# The launcher is a thin, quoted argv boundary. Claimed work is still
# authorized by Gateway; this script does not accept an arbitrary shell
# command or create a second task authority.
TASK_AGENT="${TASK_AGENT:-trae}"
TASK_MODEL_PROVIDER="${TASK_MODEL_PROVIDER:-auto}"
TASK_MODEL="${TASK_MODEL:-qwen3.5:9b}"
TASK_BASE_URL="${TASK_BASE_URL:-}"
ORCH_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
WORKER_INSTANCE="${TASK_WORKER_INSTANCE:-}"
DISPATCHER_ID="${TASK_AGENT_DISPATCHER_ID:-${TASK_WORKER_DISPATCHER_ID:-}}"
TASK_WORKTREE_ROOT="${CONTEXTLATTICE_TASK_WORKTREE_ROOT:-}"

require_value() {
  if [[ $# -lt 2 || -z "$2" ]]; then
    echo "Missing value for $1" >&2
    exit 2
  fi
}

ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --task-agent)
      require_value "$1" "${2:-}"; TASK_AGENT="$2"; shift 2 ;;
    --model-provider)
      require_value "$1" "${2:-}"; TASK_MODEL_PROVIDER="$2"; shift 2 ;;
    --model)
      require_value "$1" "${2:-}"; TASK_MODEL="$2"; shift 2 ;;
    --base-url)
      require_value "$1" "${2:-}"; TASK_BASE_URL="$2"; shift 2 ;;
    --api-key)
      # Keep the secret out of ps argv; the worker's existing auth resolver
      # reads TASK_API_KEY from its environment and never forwards it to a
      # task-scoped child.
      require_value "$1" "${2:-}"; export TASK_API_KEY="$2"; shift 2 ;;
    --worker-instance)
      require_value "$1" "${2:-}"; WORKER_INSTANCE="$2"; shift 2 ;;
    --dispatcher-id)
      require_value "$1" "${2:-}"; DISPATCHER_ID="$2"; shift 2 ;;
    --task-worktree-root)
      require_value "$1" "${2:-}"; TASK_WORKTREE_ROOT="$2"; shift 2 ;;
    --once)
      ARGS+=("--once"); shift ;;
    --orchestrator-url)
      require_value "$1" "${2:-}"; ORCH_URL="$2"; shift 2 ;;
    *)
      echo "Unknown flag: $1" >&2
      exit 2 ;;
  esac
done

if [[ -z "$TASK_WORKTREE_ROOT" || "$TASK_WORKTREE_ROOT" != /* || ! -d "$TASK_WORKTREE_ROOT" ]]; then
  echo "A pre-created absolute server-owned task worktree root is required." >&2
  exit 2
fi
export CONTEXTLATTICE_TASK_WORKTREE_ROOT="$TASK_WORKTREE_ROOT"

if [[ -n "$WORKER_INSTANCE" ]]; then
  export TASK_WORKER_INSTANCE="$WORKER_INSTANCE"
fi

WORKER_ARGS=(
  --task-agent "$TASK_AGENT"
  --model-provider "$TASK_MODEL_PROVIDER"
  --model "$TASK_MODEL"
  --orchestrator-url "$ORCH_URL"
)
if [[ -n "$TASK_BASE_URL" ]]; then
  WORKER_ARGS+=(--base-url "$TASK_BASE_URL")
fi
if [[ -n "$WORKER_INSTANCE" ]]; then
  WORKER_ARGS+=(--worker-instance "$WORKER_INSTANCE")
fi
if [[ -n "$DISPATCHER_ID" ]]; then
  export TASK_AGENT_DISPATCHER_ID="$DISPATCHER_ID"
  WORKER_ARGS+=(--dispatcher-id "$DISPATCHER_ID")
fi

exec python3 scripts/task_agent_worker.py "${WORKER_ARGS[@]}" "${ARGS[@]}"
