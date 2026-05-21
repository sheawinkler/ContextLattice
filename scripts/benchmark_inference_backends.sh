#!/usr/bin/env bash
set -euo pipefail

ORCH_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}}"
MODEL="${TASK_MODEL:-qwen3.5:9b}"
PROVIDERS="${ORCH_INFER_BENCH_PROVIDERS:-auto,vllm-metal,vllm,mlx,mtplx,openai-compatible,lmstudio,llama-cpp,ollama}"
TIMEOUT="${ORCH_INFER_BENCH_TIMEOUT_SECS:-30}"
PROMPT="${ORCH_INFER_BENCH_PROMPT:-Reply with exactly: ok}"

json_escape() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-infer-bench.XXXXXX")"
cleanup() {
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

printf '{"ok":true,"gateway":"%s","model":"%s","results":[' "$(json_escape "${ORCH_URL%/}")" "$(json_escape "$MODEL")"
first=1
IFS=',' read -r -a providers <<< "$PROVIDERS"
for raw_provider in "${providers[@]}"; do
  provider="$(echo "$raw_provider" | xargs)"
  [[ -z "$provider" ]] && continue
  body_file="$tmp_dir/${provider//[^A-Za-z0-9_.-]/_}.json"
  payload="{\"provider\":\"$(json_escape "$provider")\",\"model\":\"$(json_escape "$MODEL")\",\"messages\":[{\"role\":\"user\",\"content\":\"$(json_escape "$PROMPT")\"}]}"
  if [[ "$first" == "1" ]]; then
    first=0
  else
    printf ','
  fi
  http_code="$(
    curl -sS --max-time "$TIMEOUT" \
      -o "$body_file" \
      -w '%{http_code} %{time_total}' \
      -H 'content-type: application/json' \
      -d "$payload" \
      "${ORCH_URL%/}/v1/inference/chat" 2>"$body_file.err" || true
  )"
  status="${http_code%% *}"
  total="${http_code#* }"
  if [[ "$status" =~ ^2 ]]; then
    printf '{"provider":"%s","ok":true,"status":%s,"timeTotalSecs":%s}' "$(json_escape "$provider")" "$status" "$total"
  else
    error="$(tr '\n' ' ' < "$body_file.err" | cut -c1-300)"
    response="$(tr '\n' ' ' < "$body_file" | cut -c1-300)"
    printf '{"provider":"%s","ok":false,"status":%s,"timeTotalSecs":%s,"error":"%s","response":"%s"}' \
      "$(json_escape "$provider")" "${status:-0}" "${total:-0}" "$(json_escape "$error")" "$(json_escape "$response")"
  fi
done
printf ']}\n'
