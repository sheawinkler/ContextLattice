#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORCH_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}}"
TIMEOUT="${CONTEXTLATTICE_INFER_POLICY_TIMEOUT_SECS:-3}"

json_escape() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

http_get() {
  local url="$1"
  if has_cmd curl; then
    curl -fsS --max-time "$TIMEOUT" "$url"
    return $?
  fi
  return 1
}

detect_profile() {
  local uname_s uname_m
  uname_s="$(uname -s 2>/dev/null || true)"
  uname_m="$(uname -m 2>/dev/null || true)"
  if [[ "$uname_s" == "Darwin" && ( "$uname_m" == "arm64" || "$uname_m" == "aarch64" ) ]]; then
    echo "apple_silicon"
    return
  fi
  if [[ -n "${CUDA_VISIBLE_DEVICES:-}" || -n "${NVIDIA_VISIBLE_DEVICES:-}" ]] || has_cmd nvidia-smi || [[ -e /proc/driver/nvidia/version || -e /dev/nvidia0 ]]; then
    echo "nvidia_cuda"
    return
  fi
  if [[ -n "${ROCR_VISIBLE_DEVICES:-}" || -n "${HIP_VISIBLE_DEVICES:-}" || -e /dev/kfd ]]; then
    echo "amd_rocm"
    return
  fi
  echo "generic_cpu"
}

priority_for_profile() {
  case "$1" in
    apple_silicon) echo "mlx,vllm-metal,ane_sidecar,llama-cpp,ollama" ;;
    nvidia_cuda|amd_rocm) echo "sglang,vllm,openai-compatible,llama-cpp,lmstudio,ollama" ;;
    *) echo "openai-compatible,llama-cpp,lmstudio,ollama" ;;
  esac
}

main() {
  local endpoint="${ORCH_URL%/}/v1/inference/runtime-policy"
  if payload="$(http_get "$endpoint" 2>/dev/null)"; then
    printf '%s\n' "$payload"
    return 0
  fi

  local profile priority selected
  profile="$(detect_profile)"
  priority="${ORCH_INFER_PROVIDER_PRIORITY:-$(priority_for_profile "$profile")}"
  selected="${ORCH_INFER_PROVIDER:-auto}"
  cat <<EOF
{"ok":false,"source":"local-host-probe","error":"gateway runtime policy unavailable","gateway":"$(json_escape "$endpoint")","hardware":{"profile":"$(json_escape "$profile")"},"recommendedPriority":"$(json_escape "$priority")","selected":"$(json_escape "$selected")","embeddingRecommendation":"fastembed-rs","recommendation":{"modelStrategy":"Use MLX on Apple Silicon, SGLang/vLLM on CUDA or ROCm, and llama.cpp/LM Studio/Ollama GGUF when resources are unknown or smaller.","fallbackWhenBlind":"Start with Q4/IQ4 7B-9B models and benchmark before moving to 27B/35B-A3B.","shortlistDoc":"docs/runtime/local-model-options.md"},"note":"Start ContextLattice gateway-go to get live health-ranked provider candidates."}
EOF
}

cd "$ROOT_DIR"
main "$@"
