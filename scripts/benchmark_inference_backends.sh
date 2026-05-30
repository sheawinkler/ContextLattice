#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIMEOUT="${CONTEXTLATTICE_INFER_BENCH_TIMEOUT_SECS:-8}"
CHAT_TIMEOUT="${CONTEXTLATTICE_INFER_BENCH_CHAT_TIMEOUT_SECS:-30}"
MODEL="${CONTEXTLATTICE_INFER_BENCH_MODEL:-${GO_DREAM_MODEL:-${TASK_MODEL:-qwen3.5:9b}}}"
PROMPT="${CONTEXTLATTICE_INFER_BENCH_PROMPT:-Reply with exactly one short sentence.}"
MAX_TOKENS="${CONTEXTLATTICE_INFER_BENCH_MAX_TOKENS:-24}"
RUN_CHAT=false

usage() {
  cat <<'EOF'
Usage: scripts/benchmark_inference_backends.sh [--chat] [--providers a,b,c] [--model name] [--prompt text]

Health probes are always lightweight. --chat adds a tiny OpenAI-compatible
/chat/completions request to healthy providers and never pulls or launches models.
EOF
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

now_ms() {
  if has_cmd python3; then
    python3 - <<'PY'
import time
print(int(time.time() * 1000))
PY
    return
  fi
  date +%s000
}

json_escape() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

trim_right_slash() {
  local value="${1:-}"
  while [[ "$value" == */ ]]; do
    value="${value%/}"
  done
  printf '%s' "$value"
}

normalize_provider() {
  local provider
  provider="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]' | tr '_' '-')"
  case "$provider" in
    ollama-coreml) echo "ollama" ;;
    ane|ane-sidecar) echo "ane_sidecar" ;;
    openai|openai-compat|openai-compatible) echo "openai-compatible" ;;
    llamacpp|llama-cpp) echo "llama-cpp" ;;
    sglang|sgl) echo "sglang" ;;
    tgi|text-generation-inference) echo "tgi" ;;
    tensorrt|tensorrt-llm|trtllm|trt-llm) echo "tensorrt-llm" ;;
    mlx|mlx-lm|mtplx) echo "mlx" ;;
    vllm-metal|vllm-metal-mlx|vllm-mlx) echo "vllm-metal" ;;
    "") echo "auto" ;;
    *) echo "$provider" ;;
  esac
}

provider_base_url() {
  local provider
  provider="$(normalize_provider "$1")"
  case "$provider" in
    ollama)
      printf '%s' "${OLLAMA_API_BASE:-${OLLAMA_BASE_URL:-http://127.0.0.1:11434/v1}}"
      ;;
    mlx)
      printf '%s' "${MLX_API_BASE:-http://127.0.0.1:18087/v1}"
      ;;
    vllm)
      printf '%s' "${VLLM_BASE_URL:-http://127.0.0.1:8000}"
      ;;
    vllm-metal)
      printf '%s' "${VLLM_METAL_BASE_URL:-http://127.0.0.1:8000}"
      ;;
    sglang)
      printf '%s' "${SGLANG_BASE_URL:-${SGLANG_API_BASE:-http://127.0.0.1:30000}}"
      ;;
    openai-compatible)
      [[ -n "${OPENAI_API_BASE:-}" ]] || return 1
      printf '%s' "$OPENAI_API_BASE"
      ;;
    lmstudio)
      printf '%s' "${LMSTUDIO_BASE_URL:-${LM_STUDIO_BASE_URL:-http://127.0.0.1:1234}}"
      ;;
    llama-cpp)
      printf '%s' "${LLAMA_CPP_BASE_URL:-http://127.0.0.1:8080}"
      ;;
    tgi)
      printf '%s' "${TGI_BASE_URL:-${TEXT_GENERATION_INFERENCE_BASE_URL:-http://127.0.0.1:8080}}"
      ;;
    tensorrt-llm)
      printf '%s' "${TENSORRT_LLM_BASE_URL:-${TRTLLM_BASE_URL:-http://127.0.0.1:8000}}"
      ;;
    *)
      return 1
      ;;
  esac
}

openai_url() {
  local base path
  base="$(trim_right_slash "$1")"
  path="$2"
  if [[ "$base" == */v1 ]]; then
    printf '%s/%s' "$base" "$path"
  else
    printf '%s/v1/%s' "$base" "$path"
  fi
}

curl_code() {
  local timeout="$1"
  shift
  curl -sS -o /dev/null -w '%{http_code}' --max-time "$timeout" "$@" 2>/dev/null || printf '000'
}

probe_provider() {
  local provider base url start end code latency
  provider="$(normalize_provider "$1")"
  base="$(provider_base_url "$provider" 2>/dev/null || true)"
  if [[ -z "$base" ]]; then
    printf '%-18s %-8s %-10s %s\n' "$provider" "skip" "-" "no base URL configured"
    return 0
  fi
  url="$(openai_url "$base" models)"
  start="$(now_ms)"
  code="$(curl_code "$TIMEOUT" "$url")"
  end="$(now_ms)"
  latency=$((end - start))
  if [[ "$code" =~ ^2 ]]; then
    printf '%-18s %-8s %-10sms %s\n' "$provider" "healthy" "$latency" "$url"
    if [[ "$RUN_CHAT" == "true" ]]; then
      chat_provider "$provider" "$base"
    fi
    return 0
  fi
  printf '%-18s %-8s %-10sms %s\n' "$provider" "down:$code" "$latency" "$url"
}

chat_provider() {
  local provider base url payload start end code latency
  provider="$1"
  base="$2"
  url="$(openai_url "$base" chat/completions)"
  payload='{"model":"'"$(json_escape "$MODEL")"'","messages":[{"role":"user","content":"'"$(json_escape "$PROMPT")"'"}],"max_tokens":'"$MAX_TOKENS"',"temperature":0}'
  start="$(now_ms)"
  code="$(curl_code "$CHAT_TIMEOUT" -H 'Content-Type: application/json' -d "$payload" "$url")"
  end="$(now_ms)"
  latency=$((end - start))
  printf '%-18s %-8s %-10sms %s\n' "${provider}/chat" "$code" "$latency" "$url"
}

default_providers() {
  if [[ -n "${ORCH_INFER_PROVIDER_PRIORITY:-}" ]]; then
    printf '%s' "$ORCH_INFER_PROVIDER_PRIORITY"
    return
  fi
  printf '%s' 'mlx,vllm-metal,sglang,vllm,openai-compatible,llama-cpp,lmstudio,tgi,tensorrt-llm,ollama'
}

PROVIDERS="$(default_providers)"
while (($#)); do
  case "$1" in
    --chat)
      RUN_CHAT=true
      shift
      ;;
    --providers)
      PROVIDERS="${2:?missing provider list}"
      shift 2
      ;;
    --model)
      MODEL="${2:?missing model}"
      shift 2
      ;;
    --prompt)
      PROMPT="${2:?missing prompt}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

cd "$ROOT_DIR"
printf 'provider           status   latency    endpoint\n'
printf '%s\n' '---------------------------------------------------------------'
IFS=',' read -r -a provider_list <<< "$PROVIDERS"
for provider in "${provider_list[@]}"; do
  probe_provider "$provider"
done
