#!/usr/bin/env bash
set -euo pipefail

# Load env for MONGODB_URI, MEMORY_BANK_ROOT, etc.
if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

# Default the child command if not provided
: "${MEMORYBANK_CMD:=npx -y @allpepper/memory-bank-mcp}"
export MEMORYBANK_CMD

mkdir -p logs data/memory-bank

# Activate the correct venv
source .venv-router/bin/activate

# Run the FastAPI app
exec uvicorn scripts.memorybank_http_proxy:app --host 127.0.0.1 --port 59081
