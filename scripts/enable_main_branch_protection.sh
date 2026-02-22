#!/usr/bin/env bash
set -euo pipefail

branch="${1:-main}"
approvals="${2:-1}"

if ! command -v gh >/dev/null 2>&1; then
  echo "error: gh CLI is required (https://cli.github.com/)" >&2
  exit 1
fi

repo_slug="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)"
if [[ -z "${repo_slug}" ]]; then
  echo "error: unable to resolve repo slug from gh; run from repo root and ensure gh auth is active" >&2
  exit 1
fi

echo "Applying branch protection to ${repo_slug}:${branch} ..."

payload="$(cat <<JSON
{
  "required_status_checks": null,
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "dismiss_stale_reviews": true,
    "require_code_owner_reviews": true,
    "required_approving_review_count": ${approvals},
    "require_last_push_approval": true
  },
  "restrictions": null,
  "required_linear_history": true,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "block_creations": false,
  "required_conversation_resolution": true,
  "lock_branch": false,
  "allow_fork_syncing": true
}
JSON
)"

set +e
response="$(gh api \
  -X PUT \
  -H "Accept: application/vnd.github+json" \
  "/repos/${repo_slug}/branches/${branch}/protection" \
  --input - <<<"${payload}" 2>&1)"
status=$?
set -e

if [[ ${status} -eq 0 ]]; then
  echo "ok: branch protection enabled for ${branch}"
  echo "requires PR + code owner approval + 1 approval + linear history"
  exit 0
fi

if grep -qi "Upgrade to GitHub Pro or make this repository public" <<<"${response}"; then
  echo "blocked: GitHub branch protection unavailable on current plan/repo visibility." >&2
  echo "action: make repo public OR upgrade account, then rerun:" >&2
  echo "  scripts/enable_main_branch_protection.sh ${branch} ${approvals}" >&2
  exit 2
fi

echo "error: failed to apply branch protection" >&2
echo "${response}" >&2
exit ${status}

