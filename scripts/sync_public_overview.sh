#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PUBLIC_OWNER="${PUBLIC_OWNER:-$(gh api user -q .login)}"
PUBLIC_REPO="${PUBLIC_REPO:-ContextLattice}"
PUBLIC_SLUG="${PUBLIC_OWNER}/${PUBLIC_REPO}"
PUBLIC_BRANCH="${PUBLIC_BRANCH:-gh-pages}"
PUBLIC_DIR="${PUBLIC_DIR:-${REPO_ROOT}/tmp/public-overview}"
PUBLIC_SOURCE_DIR="${PUBLIC_SOURCE_DIR:-${REPO_ROOT}/docs/public_overview}"

if command -v gh >/dev/null 2>&1; then
  if ! gh repo view "$PUBLIC_SLUG" >/dev/null 2>&1; then
    echo "Warning: unable to verify $PUBLIC_SLUG via gh; continuing with git clone." >&2
  fi
fi

DEFAULT_BRANCH=$(gh repo view "$PUBLIC_SLUG" --json defaultBranchRef -q '.defaultBranchRef.name' 2>/dev/null || echo "main")
if [[ "$PUBLIC_BRANCH" == "$DEFAULT_BRANCH" && "${ALLOW_SYNC_TO_DEFAULT_BRANCH:-0}" != "1" ]]; then
  echo "Refusing to sync public overview into default branch '$DEFAULT_BRANCH'." >&2
  echo "Set PUBLIC_BRANCH to a site branch (recommended: gh-pages)." >&2
  echo "If you intentionally need default-branch sync, set ALLOW_SYNC_TO_DEFAULT_BRANCH=1." >&2
  exit 1
fi

rm -rf "$PUBLIC_DIR"
mkdir -p "$(dirname "$PUBLIC_DIR")"

git clone --no-checkout "https://github.com/${PUBLIC_SLUG}.git" "$PUBLIC_DIR" >/dev/null
cd "$PUBLIC_DIR"
if git ls-remote --heads origin "$PUBLIC_BRANCH" | grep -q "$PUBLIC_BRANCH"; then
  git checkout "$PUBLIC_BRANCH" >/dev/null
else
  git checkout --orphan "$PUBLIC_BRANCH" >/dev/null
  # Ensure the orphan branch starts from an empty tree.
  find . -mindepth 1 -maxdepth 1 ! -name ".git" -exec rm -rf {} +
fi

find . -mindepth 1 -maxdepth 1 ! -name ".git" -exec rm -rf {} +

if [[ -d "$PUBLIC_SOURCE_DIR" ]]; then
  cp "$PUBLIC_SOURCE_DIR/index.html" "$PUBLIC_DIR/index.html"
  cp "$PUBLIC_SOURCE_DIR/architecture.html" "$PUBLIC_DIR/architecture.html"
  cp "$PUBLIC_SOURCE_DIR/updates.html" "$PUBLIC_DIR/updates.html"
  cp "$PUBLIC_SOURCE_DIR/roadmap.html" "$PUBLIC_DIR/roadmap.html"
  cp "$PUBLIC_SOURCE_DIR/installation.html" "$PUBLIC_DIR/installation.html"
  cp "$PUBLIC_SOURCE_DIR/integration.html" "$PUBLIC_DIR/integration.html"
  cp "$PUBLIC_SOURCE_DIR/troubleshooting.html" "$PUBLIC_DIR/troubleshooting.html"
  cp "$PUBLIC_SOURCE_DIR/contact.html" "$PUBLIC_DIR/contact.html"
  cp "$PUBLIC_SOURCE_DIR/styles.css" "$PUBLIC_DIR/styles.css"
  if [[ -f "$PUBLIC_SOURCE_DIR/CNAME" ]]; then
    cp "$PUBLIC_SOURCE_DIR/CNAME" "$PUBLIC_DIR/CNAME"
  fi
  if [[ -f "$PUBLIC_SOURCE_DIR/styles-gray.css" ]]; then
    cp "$PUBLIC_SOURCE_DIR/styles-gray.css" "$PUBLIC_DIR/styles-gray.css"
  fi
  if [[ -f "$PUBLIC_SOURCE_DIR/.nojekyll" ]]; then
    cp "$PUBLIC_SOURCE_DIR/.nojekyll" "$PUBLIC_DIR/.nojekyll"
  fi
  if [[ -d "$PUBLIC_SOURCE_DIR/assets" ]]; then
    rm -rf "$PUBLIC_DIR/assets"
    cp -R "$PUBLIC_SOURCE_DIR/assets" "$PUBLIC_DIR/assets"
  fi
  if [[ -d "$PUBLIC_SOURCE_DIR/.well-known" ]]; then
    rm -rf "$PUBLIC_DIR/.well-known"
    cp -R "$PUBLIC_SOURCE_DIR/.well-known" "$PUBLIC_DIR/.well-known"
  fi
fi

if [[ -n "$(git status --porcelain)" ]]; then
  git add -A .
  git commit -m "Sync public overview assets from private repo" >/dev/null
  git push -u origin "$PUBLIC_BRANCH" >/dev/null
  echo "Synced public overview assets to $PUBLIC_SLUG ($PUBLIC_BRANCH)"
else
  echo "No public overview changes to sync."
fi
