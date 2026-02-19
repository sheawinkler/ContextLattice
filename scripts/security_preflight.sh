#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -f ".env" ]]; then
  # shellcheck disable=SC1091
  source ".env"
fi

failures=0

check_non_empty() {
  local name="$1"
  local value="${!name:-}"
  if [[ -z "$value" ]]; then
    echo "FAIL: $name must be set"
    failures=$((failures + 1))
  fi
}

check_equals() {
  local name="$1"
  local expected="$2"
  local value="${!name:-}"
  if [[ "$value" != "$expected" ]]; then
    echo "FAIL: $name must be '$expected' (found '${value:-<empty>}')"
    failures=$((failures + 1))
  fi
}

check_not_placeholder() {
  local name="$1"
  local value="${!name:-}"
  if [[ -z "$value" || "$value" == change_me* || ${#value} -lt 32 ]]; then
    echo "FAIL: $name must be a real secret (>=32 chars and not placeholder)"
    failures=$((failures + 1))
  fi
}

echo ">> running production security preflight in $ROOT_DIR"

check_equals MEMMCP_ENV production
check_non_empty MEMMCP_ORCHESTRATOR_API_KEY
check_equals ORCH_SECURITY_STRICT true
check_equals ORCH_PUBLIC_STATUS false
check_equals ORCH_PUBLIC_DOCS false
check_equals AUTH_REQUIRED true
check_equals DASHBOARD_AUTH_REQUIRED true
check_equals REQUIRE_ACTIVE_SUBSCRIPTION true
check_not_placeholder NEXTAUTH_SECRET
check_not_placeholder DASHBOARD_NEXTAUTH_SECRET

if (( failures > 0 )); then
  echo "!! security preflight failed with $failures issue(s)"
  exit 1
fi

echo ">> security preflight passed"
