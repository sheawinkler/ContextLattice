#!/bin/bash

# This script logs in to GitHub Container Registry using the PAT stored in the gh_pat environment variable,
# sets the GITHUB_PAT environment variable, and then starts the Docker Compose stack using the .env file
# in the current directory.

# Move into the repo directory that contains this launcher.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR" || {
  echo "Directory $ROOT_DIR not found." >&2
  exit 1
}

load_optional_env_file() {
  local env_path="$1"
  if [ -f "$env_path" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$env_path"
    set +a
    echo "Loaded env overlay: $env_path"
  fi
}

# Ensure the gh_pat environment variable is set
if [ -z "$gh_pat" ]; then
  echo "Environment variable gh_pat is not set. Please export it before running this script." >&2
  exit 1
fi

# Use your GitHub username for GHCR login. Adjust if different.
GITHUB_USERNAME="sheawinkler"

# Log in to GitHub Container Registry. The PAT is piped in via STDIN for security.
echo "$gh_pat" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin || {
  echo "Docker login failed." >&2
  exit 1
}

# Export GITHUB_PAT so it overrides any value in .env
export GITHUB_PAT="$gh_pat"

# Canonical premium overlay path (optional).
load_optional_env_file "${CONTEXTLATTICE_PREMIUM_ENV_FILE:-$HOME/.config/contextlattice/.env.premium}"
# Keep compose runtime naming stable regardless of clone directory naming.
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-contextlattice}"

# Install/update global CLI wrappers on launch (idempotent).
if [ "${CONTEXTLATTICE_SKIP_GLOBAL_TOOLS:-0}" != "1" ] && [ -x scripts/install_global_agent_tools.sh ]; then
  scripts/install_global_agent_tools.sh --quiet || echo "WARN: global agent tools install failed; continuing launch."
fi

# Start services via bounded v4 launcher by default.
if [ -x scripts/compose_v4_balanced.sh ]; then
  if [ "${WITH_OBSERVABILITY:-0}" = "1" ]; then
    scripts/compose_v4_balanced.sh --env-file .env --with-observability
  else
    scripts/compose_v4_balanced.sh --env-file .env
  fi
else
  docker compose --env-file .env up -d
fi

# Prevent observability drift where Langfuse stays in Created/Exited state.
if [ -x scripts/ensure_langfuse_running.sh ]; then
  PROFILES="${PROFILES:-$(awk -F= '/^COMPOSE_PROFILES=/{print substr($0,index($0,"=")+1)}' .env 2>/dev/null | tail -1)}" \
    ENV_FILE=".env" \
    scripts/ensure_langfuse_running.sh
fi
