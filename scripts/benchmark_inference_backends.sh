#!/usr/bin/env bash
set -euo pipefail

ORCH_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}}"
MODEL="${TASK_MODEL:-qwen3.5:9b}"
PROVIDERS="${ORCH_INFER_BENCH_PROVIDERS:-auto}"
TIMEOUT="${ORCH_INFER_BENCH_TIMEOUT_SECS:-30}"
PROMPT="${ORCH_INFER_BENCH_PROMPT:-Reply with exactly: ok}"
ALLOW_MULTI="${ORCH_INFER_BENCH_ALLOW_MULTI:-false}"

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

IFS=',' read -r -a providers <<< "$PROVIDERS"
provider_count=0
for raw_provider in "${providers[@]}"; do
  provider="$(echo "$raw_provider" | xargs)"
  [[ -n "$provider" ]] && provider_count=$((provider_count + 1))
done
if (( provider_count > 1 )) && [[ "$(printf '%s' "$ALLOW_MULTI" | tr '[:upper:]' '[:lower:]')" != "true" ]]; then
  printf '{"ok":false,"error":"multi_provider_benchmark_disabled","providerCount":%d,"hint":"Set ORCH_INFER_BENCH_ALLOW_MULTI=true only when the host can safely run/load-test multiple backends."}\n' "$provider_count"
  exit 2
fi

if [[ "${CONTEXTLATTICE_SINGLE_ACTIVE_INFER_BACKEND:-true}" != "false" ]]; then
  if ! scripts/inference_backend_guard.sh assert-one >"$tmp_dir/backend_guard.json"; then
    guard_payload="$(tr '\n' ' ' < "$tmp_dir/backend_guard.json" | cut -c1-500)"
    printf '{"ok":false,"error":"multiple_active_inference_backends","guard":%s}\n' "${guard_payload:-null}"
    exit 2
  fi
fi

printf '{"ok":true,"gateway":"%s","model":"%s","results":[' "$(json_escape "${ORCH_URL%/}")" "$(json_escape "$MODEL")"
first=1
for raw_provider in "${providers[@]}"; do
  provider="$(echo "$raw_provider" | xargs)"
  [[ -z "$provider" ]] && continue
  body_file="$tmp_dir/${provider//[^A-Za-z0-9_.-]/_}.json"
  : > "$body_file"
  : > "$body_file.err"
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
  status_num=0
  if [[ "$status" =~ ^[0-9]+$ ]]; then
    status_num=$((10#$status))
  fi
  total_num="${total:-0}"
  if [[ ! "$total_num" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    total_num=0
  fi
  if [[ "$status" =~ ^2 ]]; then
    printf '{"provider":"%s","ok":true,"status":%s,"timeTotalSecs":%s}' "$(json_escape "$provider")" "$status_num" "$total_num"
  else
    error="$(tr '\n' ' ' < "$body_file.err" | cut -c1-300)"
    response="$(tr '\n' ' ' < "$body_file" | cut -c1-300)"
    printf '{"provider":"%s","ok":false,"status":%s,"timeTotalSecs":%s,"error":"%s","response":"%s"}' \
      "$(json_escape "$provider")" "$status_num" "$total_num" "$(json_escape "$error")" "$(json_escape "$response")"
  fi
done
printf ']}\n'
