#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACTION="${1:-status}"
TARGET="${2:-${ORCH_INFER_PROVIDER:-auto}}"
TIMEOUT="${CONTEXTLATTICE_INFER_GUARD_TIMEOUT_SECS:-0.35}"
SINGLE_ACTIVE="${CONTEXTLATTICE_SINGLE_ACTIVE_INFER_BACKEND:-true}"
STOP_OTHERS="${CONTEXTLATTICE_INFER_GUARD_STOP_OTHERS:-true}"
KILL_PROCESSES="${CONTEXTLATTICE_INFER_GUARD_KILL_PROCESSES:-false}"

json_escape() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

truthy() {
  case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
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

trim_right_slash() {
  local value="${1:-}"
  while [[ "$value" == */ ]]; do
    value="${value%/}"
  done
  printf '%s' "$value"
}

openai_models_url() {
  local base
  base="$(trim_right_slash "$1")"
  [[ -z "$base" ]] && return 1
  if [[ "$base" == */v1 ]]; then
    printf '%s/models' "$base"
  else
    printf '%s/v1/models' "$base"
  fi
}

provider_health_url() {
  local provider
  provider="$(normalize_provider "$1")"
  case "$provider" in
    ollama)
      local base="${OLLAMA_BASE_URL:-}"
      [[ -z "$base" ]] && base="${OLLAMA_API_BASE:-http://127.0.0.1:11434/v1}"
      base="$(trim_right_slash "$base")"
      if [[ "$base" == */v1 ]]; then
        printf '%s/models' "$base"
      else
        printf '%s/api/tags' "$base"
      fi
      ;;
    mlx)
      openai_models_url "${MLX_API_BASE:-http://127.0.0.1:18087/v1}"
      ;;
    vllm)
      openai_models_url "${VLLM_BASE_URL:-http://127.0.0.1:8000}"
      ;;
    sglang)
      openai_models_url "${SGLANG_BASE_URL:-${SGLANG_API_BASE:-http://127.0.0.1:30000}}"
      ;;
    vllm-metal)
      openai_models_url "${VLLM_METAL_BASE_URL:-http://127.0.0.1:8000}"
      ;;
    tgi)
      openai_models_url "${TGI_BASE_URL:-${TEXT_GENERATION_INFERENCE_BASE_URL:-http://127.0.0.1:8080}}"
      ;;
    tensorrt-llm)
      openai_models_url "${TENSORRT_LLM_BASE_URL:-${TRTLLM_BASE_URL:-http://127.0.0.1:8000}}"
      ;;
    openai-compatible)
      [[ -z "${OPENAI_API_BASE:-}" ]] && return 1
      openai_models_url "$OPENAI_API_BASE"
      ;;
    lmstudio)
      openai_models_url "${LMSTUDIO_BASE_URL:-${LM_STUDIO_BASE_URL:-http://127.0.0.1:1234}}"
      ;;
    llama-cpp)
      openai_models_url "${LLAMA_CPP_BASE_URL:-http://127.0.0.1:8080}"
      ;;
    ane_sidecar)
      truthy "${ORCH_ANE_SIDECAR_ENABLED:-false}" || return 1
      printf '%s/health' "$(trim_right_slash "${ORCH_ANE_SIDECAR_URL:-http://127.0.0.1:9099}")"
      ;;
    *)
      return 1
      ;;
  esac
}

probe_url() {
  curl -fsS --max-time "$TIMEOUT" "$1" >/dev/null 2>&1
}

stop_pidfile() {
  local path="$1"
  if [[ -f "$path" ]]; then
    local pid
    pid="$(cat "$path" 2>/dev/null || true)"
    if [[ "$pid" =~ ^[0-9]+$ ]]; then
      kill "$pid" >/dev/null 2>&1 || true
    fi
    rm -f "$path"
  fi
}

stop_provider() {
  local provider
  provider="$(normalize_provider "$1")"
  case "$provider" in
    ollama)
      stop_pidfile "$ROOT_DIR/.ollama.pid"
      if command -v docker >/dev/null 2>&1; then
        (cd "$ROOT_DIR" && docker compose -p contextlattice stop ollama >/dev/null 2>&1 || true)
      fi
      if truthy "$KILL_PROCESSES"; then
        pkill -f "ollama serve" >/dev/null 2>&1 || true
      fi
      ;;
    mlx)
      stop_pidfile "$ROOT_DIR/.mlx.pid"
      if truthy "$KILL_PROCESSES"; then
        pkill -f "mlx_lm.server" >/dev/null 2>&1 || true
      fi
      ;;
    vllm)
      stop_pidfile "$ROOT_DIR/.vllm.pid"
      if truthy "$KILL_PROCESSES"; then
        pkill -f "vllm.*serve" >/dev/null 2>&1 || true
      fi
      ;;
    sglang)
      stop_pidfile "$ROOT_DIR/.sglang.pid"
      if truthy "$KILL_PROCESSES"; then
        pkill -f "sglang.*launch_server" >/dev/null 2>&1 || true
      fi
      ;;
    vllm-metal)
      stop_pidfile "$ROOT_DIR/.vllm-metal.pid"
      if truthy "$KILL_PROCESSES"; then
        pkill -f "vllm.*metal" >/dev/null 2>&1 || true
      fi
      ;;
    tgi)
      stop_pidfile "$ROOT_DIR/.tgi.pid"
      ;;
    tensorrt-llm)
      stop_pidfile "$ROOT_DIR/.tensorrt-llm.pid"
      ;;
    llama-cpp)
      stop_pidfile "$ROOT_DIR/.llama-cpp.pid"
      ;;
  esac
}

is_same_endpoint() {
  local needle="$1"
  shift
  local item
  for item in "$@"; do
    [[ "$item" == "$needle" ]] && return 0
  done
  return 1
}

collect_active() {
  local providers=(mlx vllm-metal sglang vllm ane_sidecar llama-cpp lmstudio openai-compatible tgi tensorrt-llm ollama)
  active_keys=()
  active_providers=()
  active_urls=()
  local provider url key
  for provider in "${providers[@]}"; do
    url="$(provider_health_url "$provider" 2>/dev/null || true)"
    [[ -z "$url" ]] && continue
    if probe_url "$url"; then
      key="$url"
      if ! is_same_endpoint "$key" "${active_keys[@]}"; then
        active_keys+=("$key")
        active_providers+=("$(normalize_provider "$provider")")
        active_urls+=("$url")
      fi
    fi
  done
}

target_is_active() {
  local target
  target="$(normalize_provider "$1")"
  [[ "$target" == "auto" ]] && return 0
  local provider
  for provider in "${active_providers[@]}"; do
    [[ "$(normalize_provider "$provider")" == "$target" ]] && return 0
  done
  return 1
}

print_status() {
  local ok=true
  if truthy "$SINGLE_ACTIVE" && (( ${#active_keys[@]} > 1 )); then
    ok=false
  fi
  printf '{"ok":%s,"singleActive":%s,"activeCount":%d,"active":[' "$ok" "$(truthy "$SINGLE_ACTIVE" && echo true || echo false)" "${#active_keys[@]}"
  local first=1 i
  for ((i = 0; i < ${#active_keys[@]}; i++)); do
    if [[ "$first" == "1" ]]; then
      first=0
    else
      printf ','
    fi
    printf '{"provider":"%s","healthUrl":"%s"}' "$(json_escape "${active_providers[$i]}")" "$(json_escape "${active_urls[$i]}")"
  done
  printf ']}\n'
}

cd "$ROOT_DIR"

case "$ACTION" in
  status)
    collect_active
    print_status
    ;;
  assert-one)
    collect_active
    print_status
    if truthy "$SINGLE_ACTIVE" && (( ${#active_keys[@]} > 1 )); then
      exit 2
    fi
    ;;
  prepare)
    target="$(normalize_provider "$TARGET")"
    if truthy "$SINGLE_ACTIVE" && truthy "$STOP_OTHERS" && [[ "$target" != "auto" ]]; then
      for provider in mlx vllm-metal sglang vllm ane_sidecar llama-cpp lmstudio openai-compatible tgi tensorrt-llm ollama; do
        [[ "$(normalize_provider "$provider")" == "$target" ]] && continue
        stop_provider "$provider"
      done
    fi
    collect_active
    print_status
    if truthy "$SINGLE_ACTIVE"; then
      if (( ${#active_keys[@]} > 1 )); then
        exit 2
      fi
      if (( ${#active_keys[@]} == 1 )) && ! target_is_active "$target"; then
        exit 3
      fi
    fi
    ;;
  *)
    echo "usage: scripts/inference_backend_guard.sh status|assert-one|prepare [provider]" >&2
    exit 64
    ;;
esac
