#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROVIDER="${CONTEXTLATTICE_INFER_CONFORMANCE_PROVIDER:-openai-compatible}"
BASE_URL="${CONTEXTLATTICE_INFER_CONFORMANCE_BASE_URL:-}"
MODEL="${CONTEXTLATTICE_INFER_CONFORMANCE_MODEL:-${GO_DREAM_MODEL:-${TASK_MODEL:-}}}"
PROMPT="${CONTEXTLATTICE_INFER_CONFORMANCE_PROMPT:-Reply with exactly this JSON: {\"ok\":true,\"template\":\"final-content\"}}"
TIMEOUT="${CONTEXTLATTICE_INFER_CONFORMANCE_TIMEOUT_SECS:-30}"
MAX_TOKENS="${CONTEXTLATTICE_INFER_CONFORMANCE_MAX_TOKENS:-96}"

usage() {
  cat <<'EOF'
Usage: scripts/inference_template_conformance.sh [--provider mlx|ollama|openai-compatible|vllm|sglang|llama-cpp|lmstudio|tgi|tensorrt-llm] --model NAME [--base-url URL]

Runs a tiny chat completion against a local inference backend and fails unless
the response exposes non-empty final content. Reasoning-only outputs return a
repair instruction instead of being accepted by ContextLattice.
EOF
}

normalize_provider() {
  local provider
  provider="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]' | tr '_' '-')"
  case "$provider" in
    ollama-coreml) echo "ollama" ;;
    openai|openai-compat|openai-compatible) echo "openai-compatible" ;;
    llamacpp|llama-cpp) echo "llama-cpp" ;;
    sglang|sgl) echo "sglang" ;;
    tgi|text-generation-inference) echo "tgi" ;;
    tensorrt|tensorrt-llm|trtllm|trt-llm) echo "tensorrt-llm" ;;
    mlx|mlx-lm|mtplx) echo "mlx" ;;
    vllm-metal|vllm-metal-mlx|vllm-mlx) echo "vllm-metal" ;;
    "") echo "openai-compatible" ;;
    *) echo "$provider" ;;
  esac
}

trim_right_slash() {
  local value="${1:-}"
  while [[ "$value" == */ ]]; do
    value="${value%/}"
  done
  printf '%s' "$value"
}

default_base_url() {
  case "$(normalize_provider "$1")" in
    ollama) printf '%s' "${OLLAMA_BASE_URL:-${OLLAMA_API_BASE:-http://127.0.0.1:11434}}" ;;
    mlx) printf '%s' "${MLX_API_BASE:-http://127.0.0.1:18087/v1}" ;;
    vllm) printf '%s' "${VLLM_BASE_URL:-http://127.0.0.1:8000}" ;;
    vllm-metal) printf '%s' "${VLLM_METAL_BASE_URL:-http://127.0.0.1:8000}" ;;
    sglang) printf '%s' "${SGLANG_BASE_URL:-${SGLANG_API_BASE:-http://127.0.0.1:30000}}" ;;
    lmstudio) printf '%s' "${LMSTUDIO_BASE_URL:-${LM_STUDIO_BASE_URL:-http://127.0.0.1:1234}}" ;;
    llama-cpp) printf '%s' "${LLAMA_CPP_BASE_URL:-http://127.0.0.1:8080}" ;;
    tgi) printf '%s' "${TGI_BASE_URL:-${TEXT_GENERATION_INFERENCE_BASE_URL:-http://127.0.0.1:8080}}" ;;
    tensorrt-llm) printf '%s' "${TENSORRT_LLM_BASE_URL:-${TRTLLM_BASE_URL:-http://127.0.0.1:8000}}" ;;
    *) printf '%s' "${OPENAI_API_BASE:-}" ;;
  esac
}

openai_chat_url() {
  local base
  base="$(trim_right_slash "$1")"
  if [[ "$base" == */v1 ]]; then
    printf '%s/chat/completions' "$base"
  else
    printf '%s/v1/chat/completions' "$base"
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --provider) PROVIDER="${2:-}"; shift 2 ;;
    --base-url) BASE_URL="${2:-}"; shift 2 ;;
    --model) MODEL="${2:-}"; shift 2 ;;
    --prompt) PROMPT="${2:-}"; shift 2 ;;
    --timeout) TIMEOUT="${2:-}"; shift 2 ;;
    --max-tokens) MAX_TOKENS="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

PROVIDER="$(normalize_provider "$PROVIDER")"
if [[ -z "$MODEL" ]]; then
  echo "ERROR: --model NAME is required." >&2
  exit 2
fi
if [[ -z "$BASE_URL" ]]; then
  BASE_URL="$(default_base_url "$PROVIDER")"
fi
if [[ -z "$BASE_URL" ]]; then
  echo "ERROR: --base-url URL is required for provider $PROVIDER." >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required for conformance JSON handling." >&2
  exit 2
fi

payload="$(python3 - "$PROVIDER" "$MODEL" "$PROMPT" "$MAX_TOKENS" <<'PY'
import json
import sys

provider, model, prompt, max_tokens = sys.argv[1:5]
if provider == "ollama":
    print(json.dumps({
        "model": model,
        "messages": [
            {"role": "system", "content": "Return final answer content only. Do not emit hidden reasoning."},
            {"role": "user", "content": prompt},
        ],
        "stream": False,
        "options": {"num_predict": int(max_tokens), "temperature": 0},
    }, separators=(",", ":")))
else:
    print(json.dumps({
        "model": model,
        "messages": [
            {"role": "system", "content": "Return final answer content only. Do not emit hidden reasoning."},
            {"role": "user", "content": prompt},
        ],
        "max_tokens": int(max_tokens),
        "temperature": 0,
    }, separators=(",", ":")))
PY
)"

if [[ "$PROVIDER" == "ollama" ]]; then
  url="$(trim_right_slash "$BASE_URL")"
  if [[ "$url" == */v1 ]]; then
    url="${url%/v1}/api/chat"
  else
    url="$url/api/chat"
  fi
else
  url="$(openai_chat_url "$BASE_URL")"
fi

raw="$(curl -fsS --max-time "$TIMEOUT" -H 'content-type: application/json' -d "$payload" "$url")"

CONTEXTLATTICE_INFER_CONFORMANCE_RAW="$raw" \
CONTEXTLATTICE_INFER_CONFORMANCE_PROVIDER="$PROVIDER" \
CONTEXTLATTICE_INFER_CONFORMANCE_URL="$url" \
python3 - <<'PY'
import json
import os
import sys

raw = os.environ.get("CONTEXTLATTICE_INFER_CONFORMANCE_RAW", "")
provider = os.environ.get("CONTEXTLATTICE_INFER_CONFORMANCE_PROVIDER", "")
url = os.environ.get("CONTEXTLATTICE_INFER_CONFORMANCE_URL", "")
try:
    data = json.loads(raw)
except Exception as exc:
    print(json.dumps({"ok": False, "error": "invalid_json_response", "detail": str(exc), "provider": provider, "url": url}, separators=(",", ":")))
    sys.exit(2)

if provider == "ollama":
    message = data.get("message") or {}
else:
    choices = data.get("choices") or []
    message = (choices[0].get("message") if choices and isinstance(choices[0], dict) else {}) or {}

content = message.get("content")
if isinstance(content, list):
    parts = []
    for part in content:
        if not isinstance(part, dict):
            continue
        if part.get("type") in (None, "", "text", "output_text"):
            text = part.get("text") or part.get("content") or ""
            if isinstance(text, str) and text.strip():
                parts.append(text.strip())
    content = "\n".join(parts)
elif not isinstance(content, str):
    content = ""
content = content.strip()
reasoning = ""
for key in ("reasoning", "reasoning_content", "thinking"):
    value = message.get(key)
    if isinstance(value, str) and value.strip():
        reasoning = value.strip()
        break

if not content and reasoning:
    print(json.dumps({
        "ok": False,
        "error": "reasoning_without_content",
        "provider": provider,
        "url": url,
        "repair": "Use a final-content chat template, such as templates/inference/mlx/qwen-final-content.jinja for MLX Qwen models, or disable thinking for this runtime.",
    }, separators=(",", ":")))
    sys.exit(2)
if not content:
    print(json.dumps({"ok": False, "error": "missing_content", "provider": provider, "url": url}, separators=(",", ":")))
    sys.exit(2)
if "<think>" in content.lower():
    print(json.dumps({"ok": False, "error": "think_tag_in_content", "provider": provider, "url": url}, separators=(",", ":")))
    sys.exit(2)

print(json.dumps({
    "ok": True,
    "provider": provider,
    "url": url,
    "contentBytes": len(content.encode("utf-8")),
    "preview": content[:160],
}, separators=(",", ":")))
PY
