#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
TARGET_COLLECTION=""
LIMIT="${LIMIT:-2000}"
PROJECT="${PROJECT:-}"
FORCE_REQUEUE="${FORCE_REQUEUE:-1}"
SKIP_REHYDRATE=0

usage() {
  cat <<'USAGE'
Usage: scripts/qdrant_collection_cutover.sh --target <collection> [options]

Options:
  --target <name>       Target Qdrant collection (required)
  --limit <n>           Rehydrate file scan limit (default: 2000)
  --project <name>      Rehydrate a single project
  --skip-rehydrate      Only update .env, do not enqueue reindex jobs
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      [[ $# -ge 2 ]] || { echo "Missing value for --target" >&2; exit 2; }
      TARGET_COLLECTION="$2"
      shift 2
      ;;
    --limit)
      [[ $# -ge 2 ]] || { echo "Missing value for --limit" >&2; exit 2; }
      LIMIT="$2"
      shift 2
      ;;
    --project)
      [[ $# -ge 2 ]] || { echo "Missing value for --project" >&2; exit 2; }
      PROJECT="$2"
      shift 2
      ;;
    --skip-rehydrate)
      SKIP_REHYDRATE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

[[ -n "$TARGET_COLLECTION" ]] || { usage; exit 2; }

tmp_file="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
if [[ -f "$ENV_FILE" ]]; then
  awk -v value="$TARGET_COLLECTION" '
    BEGIN { updated = 0 }
    /^QDRANT_COLLECTION=/ {
      print "QDRANT_COLLECTION=" value
      updated = 1
      next
    }
    { print }
    END {
      if (!updated) {
        print "QDRANT_COLLECTION=" value
      }
    }
  ' "$ENV_FILE" > "$tmp_file"
else
  printf 'QDRANT_COLLECTION=%s\n' "$TARGET_COLLECTION" > "$tmp_file"
fi
mv "$tmp_file" "$ENV_FILE"
echo ">> Set QDRANT_COLLECTION=${TARGET_COLLECTION} in ${ENV_FILE}"

if [[ "$SKIP_REHYDRATE" == "1" ]]; then
  exit 0
fi

echo ">> Enqueueing Qdrant rehydrate jobs for collection ${TARGET_COLLECTION}"
LIMIT="$LIMIT" \
PROJECT="$PROJECT" \
TARGETS="qdrant" \
QDRANT_COLLECTION="$TARGET_COLLECTION" \
FORCE_REQUEUE="$FORCE_REQUEUE" \
scripts/rehydrate_fanout.sh

echo ">> Restart orchestrator to apply updated QDRANT_COLLECTION from .env"
