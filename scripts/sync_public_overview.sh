#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/sync_public_overview.sh [--check|--dry-run]

  --check    Run local source, audit, and immutable release-proof gates only.
  --dry-run  Run the same gates and describe the target without cloning or publishing.

PUBLIC_RELEASE_PROOF_TAG may select a known version-scoped lane tag. The
selected tag must still be annotated and resolve exactly to source HEAD.
EOF
}

fail() {
  echo "Refusing to sync public overview: $*" >&2
  exit 1
}

MODE="publish"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --check)
      [[ "$MODE" == "publish" ]] || fail "choose only one of --check or --dry-run"
      MODE="check"
      ;;
    --dry-run)
      [[ "$MODE" == "publish" ]] || fail "choose only one of --check or --dry-run"
      MODE="dry-run"
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      fail "unknown argument: $1"
      ;;
  esac
  shift
done

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
CANONICAL_PUBLIC_SOURCE_DIR="${REPO_ROOT}/docs/public_overview"
PUBLIC_SOURCE_DIR="${PUBLIC_SOURCE_DIR:-$CANONICAL_PUBLIC_SOURCE_DIR}"
PUBLIC_REPO="${PUBLIC_REPO:-ContextLattice}"
PUBLIC_BRANCH="${PUBLIC_BRANCH:-gh-pages}"
PUBLIC_DIR="${PUBLIC_DIR:-${REPO_ROOT}/tmp/public-overview}"

GIT_ROOT=$(git -C "$REPO_ROOT" rev-parse --show-toplevel 2>/dev/null) || fail "source is not a Git worktree"
GIT_ROOT=$(cd "$GIT_ROOT" && pwd -P)
[[ "$GIT_ROOT" == "$REPO_ROOT" ]] || fail "script root $REPO_ROOT is not the Git root $GIT_ROOT"
[[ -d "$CANONICAL_PUBLIC_SOURCE_DIR" ]] || fail "canonical source directory is missing: $CANONICAL_PUBLIC_SOURCE_DIR"
RESOLVED_PUBLIC_SOURCE_DIR=$(cd "$PUBLIC_SOURCE_DIR" 2>/dev/null && pwd -P) || fail "source directory is missing: $PUBLIC_SOURCE_DIR"
[[ "$RESOLVED_PUBLIC_SOURCE_DIR" == "$CANONICAL_PUBLIC_SOURCE_DIR" ]] || fail "PUBLIC_SOURCE_DIR must be the audited canonical source"
PUBLIC_SOURCE_DIR="$RESOLVED_PUBLIC_SOURCE_DIR"

assert_clean_source() {
  local phase="$1"
  local git_status
  git_status=$(git -C "$REPO_ROOT" status --porcelain=v1 --untracked-files=all --ignore-submodules=none)
  if [[ -n "$git_status" ]]; then
    echo "$git_status" >&2
    fail "source worktree is dirty ${phase}"
  fi
}

assert_clean_source "before audit"

AUDIT="${REPO_ROOT}/scripts/agent/audit-public-product-truth"
[[ -x "$AUDIT" ]] || fail "product-truth audit is missing or not executable: $AUDIT"
if ! AUDIT_OUTPUT=$(cd "$REPO_ROOT" && "$AUDIT" --root "$REPO_ROOT" --lane "${PUBLIC_PRODUCT_TRUTH_LANE:-auto}" 2>&1); then
  echo "$AUDIT_OUTPUT" >&2
  fail "product-truth audit failed"
fi
AUDIT_LANE=$(printf '%s\n' "$AUDIT_OUTPUT" | python3 -c '
import json
import sys

payload = json.load(sys.stdin)
lane = payload.get("lane")
if payload.get("ok") is not True or lane not in {"public", "commercial"}:
    raise SystemExit(1)
print(lane)
') || fail "product-truth audit did not emit a valid successful lane result"
assert_clean_source "after audit"

PRODUCT_IDENTITY=$(python3 - "$REPO_ROOT/config/commercial_truth.v1.json" <<'PY'
import json
import re
import sys
from pathlib import Path

contract = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8"))
product = contract.get("product") or {}
version = str(product.get("version") or "")
stable_tag = str(product.get("stable_tag") or "")
if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version) or stable_tag != f"v{version}":
    raise SystemExit("canonical version and stable tag do not agree")
print(f"{version}|{stable_tag}")
PY
) || fail "cannot derive canonical release identity"
IFS='|' read -r PRODUCT_VERSION STABLE_TAG <<< "$PRODUCT_IDENTITY"

case "$AUDIT_LANE" in
  public) DEFAULT_PROOF_TAG="$STABLE_TAG" ;;
  commercial) DEFAULT_PROOF_TAG="${STABLE_TAG}-origin" ;;
esac
PROOF_TAG="${PUBLIC_RELEASE_PROOF_TAG:-$DEFAULT_PROOF_TAG}"
case "${AUDIT_LANE}:${PROOF_TAG}" in
  "public:${STABLE_TAG}"|"commercial:${STABLE_TAG}-origin"|"commercial:${STABLE_TAG}-public-paid") ;;
  *) fail "proof tag ${PROOF_TAG} is not an allowed lane tag for ${STABLE_TAG}" ;;
esac

TAG_REF="refs/tags/${PROOF_TAG}"
TAG_TYPE=$(git -C "$REPO_ROOT" cat-file -t "$TAG_REF" 2>/dev/null || true)
[[ "$TAG_TYPE" == "tag" ]] || fail "${PROOF_TAG} lacks an immutable annotated release-candidate tag"
SOURCE_COMMIT=$(git -C "$REPO_ROOT" rev-parse HEAD)
PROOF_COMMIT=$(git -C "$REPO_ROOT" rev-parse "${TAG_REF}^{commit}")
[[ "$PROOF_COMMIT" == "$SOURCE_COMMIT" ]] || fail "${PROOF_TAG} proves ${PROOF_COMMIT}, not source HEAD ${SOURCE_COMMIT}"
PROOF_TAG_OBJECT=$(git -C "$REPO_ROOT" rev-parse "$TAG_REF")
PROOF_TREE=$(git -C "$REPO_ROOT" rev-parse "${TAG_REF}^{tree}")

if [[ "${SKIP_PUBLIC_OVERVIEW_BOUNDARY_GUARD:-0}" != "1" ]]; then
  python3 - "$PUBLIC_SOURCE_DIR" "$REPO_ROOT/docs/architecture/scaling-memory.md" <<'PY'
from pathlib import Path
import sys

roots = [Path(arg) for arg in sys.argv[1:]]
markers = [
    "Private" + "/" + "Public Sync Notes",
    "publish_" + "execution_" + "tracker",
    "launch_" + "channel_" + "copybook",
    "submission_" + "requirements",
    "internal" + "-planning",
    "internal " + "docs",
    "internal " + "documentation",
    "private " + "operator",
    "private " + "operator " + "docs",
    "private " + "operator " + "runbooks",
    "private " + "runbooks",
]
text_exts = {"", ".bash", ".cjs", ".css", ".go", ".html", ".js", ".json", ".md", ".mjs", ".py", ".rs", ".sh", ".toml", ".txt", ".xml", ".yaml", ".yml"}
findings = []
for root in roots:
    paths = root.rglob("*") if root.is_dir() else [root]
    for path in paths:
        if not path.is_file() or path.suffix.lower() not in text_exts:
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        for marker in markers:
            if marker in text:
                findings.append((path, marker))
if findings:
    print("Refusing to sync public overview; public-boundary markers found:", file=sys.stderr)
    for path, marker in findings:
        print(f"- {path}: {marker}", file=sys.stderr)
    raise SystemExit(1)
PY
fi

canonical_html_files=()
while IFS= read -r source_path; do
  filename=${source_path##*/}
  if [[ "$filename" != "index-orb-white.html" ]]; then
    canonical_html_files+=("$filename")
  fi
done < <(find "$PUBLIC_SOURCE_DIR" -maxdepth 1 -type f -name '*.html' -print | LC_ALL=C sort)
[[ " ${canonical_html_files[*]} " == *" index.html "* ]] || fail "audited source has no canonical index.html"

required_files=(
  "${canonical_html_files[@]}"
  llms.txt
  commercial-truth.json
  robots.txt
  sitemap.xml
  styles.css
)
for filename in "${required_files[@]}"; do
  [[ -f "$PUBLIC_SOURCE_DIR/$filename" ]] || fail "audited source is missing required deployment file: $filename"
done

assert_clean_source "after preflight"
echo "Verified product=${STABLE_TAG} lane=${AUDIT_LANE} proof=${PROOF_TAG}: tag_object=${PROOF_TAG_OBJECT} commit=${PROOF_COMMIT} tree=${PROOF_TREE}"

if [[ "$MODE" == "check" ]]; then
  echo "Public overview check passed; no clone, destination write, commit, or push performed."
  exit 0
fi
if [[ "$MODE" == "dry-run" ]]; then
  echo "Dry run: would sync ${STABLE_TAG} proven by ${PROOF_TAG} to ${PUBLIC_OWNER:-<authenticated-owner>}/${PUBLIC_REPO} (${PUBLIC_BRANCH}); no clone, destination write, commit, or push performed."
  exit 0
fi

if [[ -z "${PUBLIC_OWNER:-}" ]]; then
  command -v gh >/dev/null 2>&1 || fail "PUBLIC_OWNER is unset and gh is unavailable"
  PUBLIC_OWNER=$(gh api user -q .login)
fi
PUBLIC_SLUG="${PUBLIC_OWNER}/${PUBLIC_REPO}"

if command -v gh >/dev/null 2>&1; then
  if ! gh repo view "$PUBLIC_SLUG" >/dev/null 2>&1; then
    echo "Warning: unable to verify $PUBLIC_SLUG via gh; continuing with git clone." >&2
  fi
fi

DEFAULT_BRANCH="${PUBLIC_DEFAULT_BRANCH:-}"
if [[ -z "$DEFAULT_BRANCH" ]] && command -v gh >/dev/null 2>&1; then
  DEFAULT_BRANCH=$(gh repo view "$PUBLIC_SLUG" --json defaultBranchRef -q '.defaultBranchRef.name' 2>/dev/null || true)
fi
DEFAULT_BRANCH="${DEFAULT_BRANCH:-main}"
if [[ "$PUBLIC_BRANCH" == "$DEFAULT_BRANCH" && "${ALLOW_SYNC_TO_DEFAULT_BRANCH:-0}" != "1" ]]; then
  fail "target branch '${PUBLIC_BRANCH}' is the default branch; choose a site branch or explicitly set ALLOW_SYNC_TO_DEFAULT_BRANCH=1"
fi

rm -rf "$PUBLIC_DIR"
mkdir -p "$(dirname "$PUBLIC_DIR")"

git clone --no-checkout "https://github.com/${PUBLIC_SLUG}.git" "$PUBLIC_DIR" >/dev/null
cd "$PUBLIC_DIR"
if git ls-remote --exit-code --heads origin "refs/heads/${PUBLIC_BRANCH}" >/dev/null 2>&1; then
  git checkout "$PUBLIC_BRANCH" >/dev/null
else
  git checkout --orphan "$PUBLIC_BRANCH" >/dev/null
  find . -mindepth 1 -maxdepth 1 ! -name ".git" -exec rm -rf {} +
fi

find . -mindepth 1 -maxdepth 1 ! -name ".git" -exec rm -rf {} +

for filename in "${required_files[@]}"; do
  cp "$PUBLIC_SOURCE_DIR/$filename" "$PUBLIC_DIR/$filename"
done

for optional_file in CNAME styles-gray.css styles-fracture.css .nojekyll; do
  if [[ -f "$PUBLIC_SOURCE_DIR/$optional_file" ]]; then
    cp "$PUBLIC_SOURCE_DIR/$optional_file" "$PUBLIC_DIR/$optional_file"
  fi
done
if [[ -d "$PUBLIC_SOURCE_DIR/assets" ]]; then
  cp -R "$PUBLIC_SOURCE_DIR/assets" "$PUBLIC_DIR/assets"
fi
if [[ -d "$PUBLIC_SOURCE_DIR/.well-known" ]]; then
  cp -R "$PUBLIC_SOURCE_DIR/.well-known" "$PUBLIC_DIR/.well-known"
fi

if [[ -n "$(git status --porcelain)" ]]; then
  git add -A .
  git commit -m "Sync public overview for ${STABLE_TAG} (proof ${PROOF_TAG}, ${SOURCE_COMMIT:0:12})" >/dev/null
  git push -u origin "$PUBLIC_BRANCH" >/dev/null
  echo "Synced public overview assets for ${STABLE_TAG} to $PUBLIC_SLUG ($PUBLIC_BRANCH)"
else
  echo "No public overview changes to sync."
fi
