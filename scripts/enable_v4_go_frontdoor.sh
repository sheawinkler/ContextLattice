#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"

ENV_FILE="$ENV_FILE" STRICT_ENV_FILE="${STRICT_ENV_FILE:-config/env/strict_runtime.env}" \
  scripts/enforce_strict_env.sh --apply

echo "v4 go frontdoor strict lock applied to $ENV_FILE"
echo "  gateway-go host port: 8075"
echo "  python fallback host port: 18075"
echo "restart with: docker compose up -d --build gateway-go contextlattice-orchestrator orchestrator-go"
