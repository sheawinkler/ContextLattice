#!/usr/bin/env bash
set -euo pipefail

TASK_AGENT="${TASK_AGENT:-trae}"
TASK_MODEL_PROVIDER="${TASK_MODEL_PROVIDER:-ollama}"
TASK_MODEL="${TASK_MODEL:-qwen2.5-coder:7b}"
TASK_BASE_URL="${TASK_BASE_URL:-}"
TASK_API_KEY="${TASK_API_KEY:-}"
ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"

ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --task-agent) TASK_AGENT="$2"; shift 2;;
    --model-provider) TASK_MODEL_PROVIDER="$2"; shift 2;;
    --model) TASK_MODEL="$2"; shift 2;;
    --base-url) TASK_BASE_URL="$2"; shift 2;;
    --api-key) TASK_API_KEY="$2"; shift 2;;
    --once) ARGS+=("--once"); shift 1;;
    --orchestrator-url) ORCH_URL="$2"; shift 2;;
    *) echo "Unknown flag: $1" >&2; exit 1;;
  esac
done

python3 scripts/task_agent_worker.py \
  --task-agent "$TASK_AGENT" \
  --model-provider "$TASK_MODEL_PROVIDER" \
  --model "$TASK_MODEL" \
  ${TASK_BASE_URL:+--base-url "$TASK_BASE_URL"} \
  ${TASK_API_KEY:+--api-key "$TASK_API_KEY"} \
  --orchestrator-url "$ORCH_URL" \
  "${ARGS[@]}"
