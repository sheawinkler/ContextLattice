#!/bin/bash

# This script logs in to GitHub Container Registry using the PAT stored in the gh_pat environment variable,
# sets the GITHUB_PAT environment variable, and then starts the Docker Compose stack using the .env file
# in the current directory.

# Move into the mcp-servers directory (adjust this path if you've moved it elsewhere)
cd "$HOME/mcp-servers" || {
  echo "Directory $HOME/mcp-servers not found." >&2
  exit 1
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

# Start the services using Docker Compose with the .env file
docker compose --env-file .env up -d
