#!/usr/bin/env bash
set -euo pipefail

# --- Config ---
LETTA_BASE="${LETTA_BASE:-http://127.0.0.1:8283/v1}"
OLLAMA_HOST="${OLLAMA_HOST:-http://127.0.0.1:11434}"
LETTA_CHAT_MODEL="${LETTA_CHAT_MODEL:-qwen3.5:9b}"
LETTA_EMBED_MODEL="${LETTA_EMBED_MODEL:-nomic-embed-text:latest}"

# --- Safe auth header for set -u ---
declare -a AUTH_HEADER=()
if [[ -n "${LETTA_API_KEY:-}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer ${LETTA_API_KEY}")
fi

echo "==> Pulling local models into Ollama (may take a while on first run)..."
if command -v docker >/dev/null 2>&1; then
  compose_args=()
  if [[ -n "${ENV_FILE:-}" ]]; then
    compose_args+=(--env-file "${ENV_FILE}")
  fi
  if [[ -n "${COMPOSE_FILE:-}" ]]; then
    IFS=':' read -r -a compose_files <<< "${COMPOSE_FILE}"
    for compose_file in "${compose_files[@]}"; do
      compose_args+=(-f "${compose_file}")
    done
  else
    compose_args+=(-f docker-compose.yml)
  fi
  compose_project_args=()
  if [[ -n "${COMPOSE_PROJECT_NAME:-}" ]]; then
    compose_project_args=(-p "${COMPOSE_PROJECT_NAME}")
  fi
  ollama_container="$(docker compose "${compose_project_args[@]}" "${compose_args[@]}" ps -q ollama 2>/dev/null || true)"
  if [[ -n "${ollama_container}" ]]; then
    docker compose "${compose_project_args[@]}" "${compose_args[@]}" exec -T ollama ollama pull "${LETTA_CHAT_MODEL}" || true
    docker compose "${compose_project_args[@]}" "${compose_args[@]}" exec -T ollama ollama pull "${LETTA_EMBED_MODEL}" || true
  else
    echo "   (compose service 'ollama' not found; skipping docker pulls)"
  fi
else
  echo "   (docker not found; skipping docker pulls)"
fi

echo "==> Verifying Letta is up at ${LETTA_BASE} ..."
for i in {1..60}; do
  if curl -fsS ${AUTH_HEADER+"${AUTH_HEADER[@]}"} "${LETTA_BASE}/health" >/dev/null 2>&1; then
    echo "   OK"
    break
  fi
  sleep 1
done

echo "==> (Optional) Creating starter agent: Sigma Coder (Qwen2.5-Coder-7B via Ollama)"
curl -fsS -X POST "${LETTA_BASE}/agents" \
  -H "Content-Type: application/json" \
  ${AUTH_HEADER+"${AUTH_HEADER[@]}"} \
  -d "{\"name\":\"Sigma Coder\",\"model\":\"ollama:${LETTA_CHAT_MODEL}\",\"embedding\":\"ollama:${LETTA_EMBED_MODEL}\",\"purpose\":\"software development and refactoring\",\"tools\":[\"browser\",\"shell\",\"mcp\"]}" \
  >/dev/null 2>&1 || true

echo "==> Letta seed completed"
