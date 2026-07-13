#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST="${MLX_SERVER_HOST:-0.0.0.0}"
PORT="${MLX_SERVER_PORT:-18087}"
MODEL="${MLX_MODEL_PATH:-}"
TEMPLATE_PROFILE="${MLX_CHAT_TEMPLATE_PROFILE:-qwen-final-content}"
CHAT_TEMPLATE="${MLX_CHAT_TEMPLATE:-}"
LOG_FILE="${MLX_SERVER_LOG:-}"

usage() {
  cat <<'EOF'
Usage: scripts/inference_mlx_server.sh --model PATH [--port 18087] [--host 0.0.0.0] [--template-profile qwen-final-content|none] [--chat-template PATH] [--log PATH]

Starts mlx_lm.server with ContextLattice-safe defaults. The qwen-final-content
template is intended for Qwen thinking templates that otherwise emit reasoning
without final message.content through OpenAI-compatible APIs.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --model) MODEL="${2:-}"; shift 2 ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --host) HOST="${2:-}"; shift 2 ;;
    --template-profile) TEMPLATE_PROFILE="${2:-}"; shift 2 ;;
    --chat-template) CHAT_TEMPLATE="${2:-}"; shift 2 ;;
    --log) LOG_FILE="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$MODEL" ]]; then
  echo "ERROR: --model PATH is required." >&2
  exit 2
fi
if [[ ! -e "$MODEL" ]]; then
  echo "ERROR: model path does not exist: $MODEL" >&2
  exit 2
fi
if ! command -v mlx_lm.server >/dev/null 2>&1; then
  echo "ERROR: mlx_lm.server is not on PATH. Install mlx-lm in the host Python/Homebrew environment." >&2
  exit 2
fi

if [[ -z "$CHAT_TEMPLATE" ]]; then
  case "$TEMPLATE_PROFILE" in
    qwen-final-content)
      CHAT_TEMPLATE="$ROOT_DIR/templates/inference/mlx/qwen-final-content.jinja"
      ;;
    none|"")
      CHAT_TEMPLATE=""
      ;;
    *)
      if [[ -f "$TEMPLATE_PROFILE" ]]; then
        CHAT_TEMPLATE="$TEMPLATE_PROFILE"
      else
        echo "ERROR: unknown template profile: $TEMPLATE_PROFILE" >&2
        exit 2
      fi
      ;;
  esac
fi
if [[ -n "$CHAT_TEMPLATE" && ! -f "$CHAT_TEMPLATE" ]]; then
  echo "ERROR: chat template does not exist: $CHAT_TEMPLATE" >&2
  exit 2
fi

export KMP_DUPLICATE_LIB_OK="${KMP_DUPLICATE_LIB_OK:-TRUE}"
export HF_HOME="${HF_HOME:-${CONTEXTLATTICE_CACHE_ROOT:-${XDG_CACHE_HOME:-$HOME/.cache}/contextlattice}/huggingface}"
export TRANSFORMERS_CACHE="${TRANSFORMERS_CACHE:-$HF_HOME/transformers}"

args=(--model "$MODEL" --host "$HOST" --port "$PORT")
if [[ -n "$CHAT_TEMPLATE" ]]; then
  args+=(--chat-template "$CHAT_TEMPLATE")
fi

if [[ -n "$LOG_FILE" ]]; then
  mkdir -p "$(dirname "$LOG_FILE")"
  exec > >(tee -a "$LOG_FILE") 2>&1
fi

printf '{"ok":true,"provider":"mlx","base_url":"http://%s:%s/v1","model":"%s","chat_template":"%s"}\n' "$HOST" "$PORT" "$MODEL" "$CHAT_TEMPLATE"
exec mlx_lm.server "${args[@]}"
