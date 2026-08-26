#!/usr/bin/env bash
set -euo pipefail

MODE="remote"
APP_URL="${APP_URL:-}"
API_URL="${API_URL:-}"
TIMEOUT_SECS="${TIMEOUT_SECS:-20}"
LOCAL_GATEWAY_PORT="${LOCAL_GATEWAY_PORT:-${GO_GATEWAY_HOST_PORT:-8091}}"

usage() {
  cat <<'EOF'
Usage:
  scripts/smoke_hosted_split.sh [--remote|--local] [--app-url URL] [--api-url URL]

Options:
  --remote           Check public hosts (default).
  --local            Check local compose ports at 127.0.0.1.
  --app-url URL      Override app host URL.
  --api-url URL      Override api host URL.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --remote) MODE="remote"; shift ;;
    --local) MODE="local"; shift ;;
    --app-url) APP_URL="$2"; shift 2 ;;
    --api-url) API_URL="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required for structural smoke-response validation." >&2
  exit 1
fi

if [[ "$MODE" == "local" ]]; then
  APP_URL="${APP_URL:-http://127.0.0.1:3000}"
  API_URL="${API_URL:-http://127.0.0.1:${LOCAL_GATEWAY_PORT}}"
else
  APP_URL="${APP_URL:-https://app.contextlattice.io}"
  API_URL="${API_URL:-https://api.contextlattice.io}"
fi

ts="$(date +%s)"
failures=0
CURL_APP_OPTS=()
CURL_API_OPTS=()
SMOKE_BODY="$(mktemp "${TMPDIR:-/tmp}/contextlattice-smoke-body.XXXXXX")"
trap 'rm -f "$SMOKE_BODY"' EXIT

check_route() {
  local name="$1"
  local url="$2"
  local expected="$3"
  shift 3
  local opts=("$@")
  echo "== $name: $url"
  local code
  code="$(curl -sS -m "$TIMEOUT_SECS" "${opts[@]}" -o "$SMOKE_BODY" -w "%{http_code}" "$url" || true)"
  if [[ "$code" != "$expected" ]]; then
    echo "  FAIL expected HTTP $expected got $code" >&2
    sed -n '1,4p' "$SMOKE_BODY" >&2 || true
    failures=$((failures + 1))
    return 0
  fi
  echo "  OK HTTP $code"
}

check_route_any() {
  local name="$1"
  local url="$2"
  shift 2
  local opts=()
  while [[ $# -gt 0 && "$1" == --* ]]; do
    opts+=("$1")
    shift
    if [[ $# -gt 0 && "$1" != --* ]]; then
      opts+=("$1")
      shift
    fi
  done
  local code
  code="$(curl -sS -m "$TIMEOUT_SECS" "${opts[@]}" -o "$SMOKE_BODY" -w "%{http_code}" "$url" || true)"
  for expected in "$@"; do
    if [[ "$code" == "$expected" ]]; then
      echo "  OK $name HTTP $code"
      return 0
    fi
  done
  echo "  FAIL $name expected one of [$*] got $code" >&2
  sed -n '1,4p' "$SMOKE_BODY" >&2 || true
  failures=$((failures + 1))
  return 0
}

check_ready_json() {
  local name="$1"
  local url="$2"
  shift 2
  local opts=("$@")
  echo "== $name: $url"
  local code
  code="$(curl -sS -m "$TIMEOUT_SECS" "${opts[@]}" -o "$SMOKE_BODY" -w "%{http_code}" "$url" || true)"
  if [[ "$code" != "200" ]]; then
    echo "  FAIL expected HTTP 200 got $code" >&2
    sed -n '1,4p' "$SMOKE_BODY" >&2 || true
    failures=$((failures + 1))
    return 0
  fi
  if ! jq -e 'type == "object" and .ready == true' "$SMOKE_BODY" >/dev/null 2>&1; then
    echo "  FAIL readiness body did not assert ready=true" >&2
    sed -n '1,4p' "$SMOKE_BODY" >&2 || true
    failures=$((failures + 1))
    return 0
  fi
  echo "  OK HTTP 200 ready=true"
}

check_retrieval_canary() {
  local name="$1"
  local url="$2"
  shift 2
  local opts=("$@")
  local request='{"query":"contextlattice hosted retrieval readiness canary","project":"contextlattice-smoke","sources":["qdrant"],"retrieval_mode":"fast","blocking":true,"limit":1}'
  echo "== $name: $url"
  local code
  code="$(curl -sS -m "$TIMEOUT_SECS" "${opts[@]}" -H 'Content-Type: application/json' -X POST --data "$request" -o "$SMOKE_BODY" -w "%{http_code}" "$url" || true)"
  if [[ "$code" != "200" ]]; then
    echo "  FAIL expected HTTP 200 got $code" >&2
    sed -n '1,4p' "$SMOKE_BODY" >&2 || true
    failures=$((failures + 1))
    return 0
  fi
  if ! jq -e '
    type == "object"
    and .degraded == false
    and (.result_state == "ready" or .result_state == "empty")
    and .retrieval_debug.source_owners.qdrant == "go_native"
    and (.retrieval_debug.source_errors.qdrant? == null)
    and ((.warnings? // []) | type == "array")
    and ((.warnings? // []) | all(.[]; type == "string" and (startswith("qdrant collection dimension probe failed;") | not)))
  ' "$SMOKE_BODY" >/dev/null 2>&1; then
    echo "  FAIL retrieval canary requires non-degraded ready|empty with qdrant owned by go_native, no qdrant source error, and no dimension-probe fallback warning" >&2
    sed -n '1,4p' "$SMOKE_BODY" >&2 || true
    failures=$((failures + 1))
    return 0
  fi
  echo "  OK HTTP 200 degraded=false result_state=ready|empty qdrant_owner=go_native"
}

check_not_placeholder() {
  local name="$1"
  local url="$2"
  shift 2
  local opts=("$@")
  local body
  body="$(curl -sS -m "$TIMEOUT_SECS" "${opts[@]}" "$url" || true)"
  if echo "$body" | grep -qi "no site :("; then
    echo "  FAIL $name still serving placeholder 'no site :('" >&2
    failures=$((failures + 1))
    return 0
  fi
  echo "  OK $name is not placeholder content"
}

if [[ "$MODE" == "remote" ]]; then
  echo "Mode: remote"
  echo "App URL: $APP_URL"
  echo "API URL: $API_URL"
  echo

  app_host="${APP_URL#https://}"
  app_host="${app_host#http://}"
  app_host="${app_host%%/*}"
  api_host="${API_URL#https://}"
  api_host="${api_host#http://}"
  api_host="${api_host%%/*}"
  echo "Authoritative DNS snapshot:"
  ns="$(dig +short NS contextlattice.io | head -n 1 || true)"
  if [[ -n "$ns" ]]; then
    echo "  NS: $ns"
    app_ip="$(dig +short A "$app_host" @"$ns" | head -n 1 || true)"
    api_ip="$(dig +short A "$api_host" @"$ns" | head -n 1 || true)"
    echo "  A $app_host: $(dig +short A "$app_host" @"$ns" | tr '\n' ' ')"
    echo "  A $api_host: $(dig +short A "$api_host" @"$ns" | tr '\n' ' ')"
    if [[ -n "$app_ip" ]]; then
      CURL_APP_OPTS=(--resolve "${app_host}:443:${app_ip}" --resolve "${app_host}:80:${app_ip}")
    fi
    if [[ -n "$api_ip" ]]; then
      CURL_API_OPTS=(--resolve "${api_host}:443:${api_ip}" --resolve "${api_host}:80:${api_ip}")
    fi
  fi
  echo

  check_route "Marketing premium page" "https://contextlattice.io/premium.html?v=${ts}" "200"
  check_route "Marketing app page" "https://contextlattice.io/app.html?v=${ts}" "200"
  check_route_any "App root" "$APP_URL/" "${CURL_APP_OPTS[@]}" "200" "301" "302" "307" "308"
  check_not_placeholder "App root" "$APP_URL/" "${CURL_APP_OPTS[@]}"
  check_route "API health" "$API_URL/health" "200" "${CURL_API_OPTS[@]}"
  check_ready_json "API retrieval readiness" "$API_URL/readyz" "${CURL_API_OPTS[@]}"
  check_retrieval_canary "API retrieval canary" "$API_URL/memory/search" "${CURL_API_OPTS[@]}"
else
  echo "Mode: local"
  check_route_any "Dashboard local root" "$APP_URL/" "200" "307"
  # Allow 200 or 307 for auth-guarded pages.
  code="$(curl -sS -m "$TIMEOUT_SECS" -o "$SMOKE_BODY" -w "%{http_code}" "$APP_URL/console" || true)"
  if [[ "$code" != "200" && "$code" != "307" ]]; then
    echo "  FAIL dashboard /console expected 200|307 got $code" >&2
    failures=$((failures + 1))
  else
    echo "  OK dashboard /console HTTP $code"
  fi
  check_ready_json "Gateway local retrieval readiness" "$API_URL/readyz"
  check_retrieval_canary "Gateway local retrieval canary" "$API_URL/memory/search"
fi

echo
if [[ "$failures" -gt 0 ]]; then
  echo "Hosted split smoke failed (${failures} check(s))." >&2
  exit 1
fi

echo "Hosted split smoke passed."
