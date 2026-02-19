#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PUBLIC_OWNER="${PUBLIC_OWNER:-$(gh api user -q .login)}"
PUBLIC_REPO="${PUBLIC_REPO:-memmcp-overview}"
PUBLIC_SLUG="${PUBLIC_OWNER}/${PUBLIC_REPO}"
PUBLIC_DIR="${PUBLIC_DIR:-${REPO_ROOT}/tmp/public-overview}"
PUBLIC_SOURCE_DIR="${PUBLIC_SOURCE_DIR:-${REPO_ROOT}/docs/public_overview}"

if command -v gh >/dev/null 2>&1; then
  if ! gh repo view "$PUBLIC_SLUG" >/dev/null 2>&1; then
    echo "Warning: unable to verify $PUBLIC_SLUG via gh; continuing with git clone." >&2
  fi
fi

rm -rf "$PUBLIC_DIR"
mkdir -p "$(dirname "$PUBLIC_DIR")"

git clone "https://github.com/${PUBLIC_SLUG}.git" "$PUBLIC_DIR" >/dev/null
if [[ -d "$PUBLIC_SOURCE_DIR" ]]; then
  cp "$PUBLIC_SOURCE_DIR/index.html" "$PUBLIC_DIR/index.html"
  cp "$PUBLIC_SOURCE_DIR/architecture.html" "$PUBLIC_DIR/architecture.html"
  cp "$PUBLIC_SOURCE_DIR/updates.html" "$PUBLIC_DIR/updates.html"
  cp "$PUBLIC_SOURCE_DIR/installation.html" "$PUBLIC_DIR/installation.html"
  cp "$PUBLIC_SOURCE_DIR/integration.html" "$PUBLIC_DIR/integration.html"
  cp "$PUBLIC_SOURCE_DIR/troubleshooting.html" "$PUBLIC_DIR/troubleshooting.html"
  cp "$PUBLIC_SOURCE_DIR/contact.html" "$PUBLIC_DIR/contact.html"
  cp "$PUBLIC_SOURCE_DIR/styles.css" "$PUBLIC_DIR/styles.css"
  if [[ -f "$PUBLIC_SOURCE_DIR/styles-gray.css" ]]; then
    cp "$PUBLIC_SOURCE_DIR/styles-gray.css" "$PUBLIC_DIR/styles-gray.css"
  fi
fi
rm -f "$PUBLIC_DIR/README.md"

cd "$PUBLIC_DIR"
if [[ -n "$(git status --porcelain)" ]]; then
  git add -A .
  git commit -m "Sync public overview assets from private repo" >/dev/null
  git push >/dev/null
  echo "Synced public overview assets to $PUBLIC_SLUG"
else
  echo "No public overview changes to sync."
fi
