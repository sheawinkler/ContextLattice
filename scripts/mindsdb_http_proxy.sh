#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."   # repo root

# Use your router venv
if [ -d ".venv-router" ]; then
  source .venv-router/bin/activate
else
  echo "ERROR: .venv-router not found"; exit 1
fi

# Ensure deps are present in this venv
python -m pip install -U pip >/dev/null
python -m pip install -U fastmcp fastapi uvicorn httpx anyio >/dev/null

# Launch your existing proxy script (already in scripts/)
# If your script binds internally, keep that; otherwise we’ll default to :8004
export HOST=127.0.0.1
export PORT=${PORT:-8004}

# Prefer uvicorn if your script exposes `app`; else just run python
if grep -qE '^\s*app\s*=' scripts/mindsdb_http_proxy.py 2>/dev/null; then
  exec uvicorn scripts.mindsdb_http_proxy:app --host "$HOST" --port "$PORT"
else
  exec python scripts/mindsdb_http_proxy.py
fi
