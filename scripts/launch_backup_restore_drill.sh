#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -f ".env" ]]; then
  # shellcheck disable=SC1091
  source ".env"
fi

STAMP="$(date +%Y%m%d%H%M%S)"
DRILL_DIR="${DRILL_DIR:-$ROOT_DIR/tmp/launch_drill/$STAMP}"
MONGO_SERVICE="${MONGO_SERVICE:-mongo}"
MONGO_DB="${MONGO_DB:-${MONGO_RAW_DB:-memmcp_raw}}"
MONGO_RESTORE_DB="${MONGO_RESTORE_DB:-${MONGO_DB}_restore_drill_${STAMP}}"
QDRANT_URL="${QDRANT_URL_HOST:-http://127.0.0.1:6333}"
QDRANT_COLLECTION="${QDRANT_COLLECTION:-memmcp_notes}"

mkdir -p "$DRILL_DIR"

echo "== Launch backup/restore drill =="
echo "root: $ROOT_DIR"
echo "drill_dir: $DRILL_DIR"

echo "-- Mongo backup"
docker compose exec -T "$MONGO_SERVICE" mongodump \
  --db "$MONGO_DB" \
  --archive \
  --gzip > "$DRILL_DIR/mongo_${MONGO_DB}.archive.gz"

if [[ ! -s "$DRILL_DIR/mongo_${MONGO_DB}.archive.gz" ]]; then
  echo "ERROR: Mongo backup archive not created."
  exit 1
fi

echo "-- Mongo restore into scratch DB: $MONGO_RESTORE_DB"
docker compose exec -T "$MONGO_SERVICE" mongorestore \
  --archive \
  --gzip \
  --drop \
  --nsFrom "${MONGO_DB}.*" \
  --nsTo "${MONGO_RESTORE_DB}.*" < "$DRILL_DIR/mongo_${MONGO_DB}.archive.gz" >/dev/null

MONGO_VERIFY_JSON="$(
  docker compose exec -T "$MONGO_SERVICE" mongosh --quiet --eval "
const dbn='$MONGO_RESTORE_DB';
const dbx=db.getSiblingDB(dbn);
const names=dbx.getCollectionNames();
let total=0;
for (const n of names) {
  total += dbx.getCollection(n).countDocuments({});
}
print(JSON.stringify({db:dbn, collections:names.length, totalDocs:total}));
"
)"

echo "mongo_restore_verify: $MONGO_VERIFY_JSON"

echo "-- Qdrant snapshot create: $QDRANT_COLLECTION"
SNAPSHOT_NAME="$(
  curl -fsS -X POST "$QDRANT_URL/collections/$QDRANT_COLLECTION/snapshots" \
    | jq -r '.result.name // empty'
)"
if [[ -z "$SNAPSHOT_NAME" ]]; then
  echo "ERROR: Qdrant snapshot name not returned."
  exit 1
fi
echo "qdrant_snapshot_created: $SNAPSHOT_NAME"

curl -fsS "$QDRANT_URL/collections/$QDRANT_COLLECTION/snapshots" \
  | jq -e --arg name "$SNAPSHOT_NAME" '.result[] | select(.name==$name)' >/dev/null

echo "qdrant_snapshot_verify: found $SNAPSHOT_NAME"

echo "-- Optional cleanup of scratch Mongo restore DB"
docker compose exec -T "$MONGO_SERVICE" mongosh --quiet --eval "
db.getSiblingDB('$MONGO_RESTORE_DB').dropDatabase();
" >/dev/null || true

echo "== Launch backup/restore drill passed =="
echo "artifacts:"
echo "  $DRILL_DIR/mongo_${MONGO_DB}.archive.gz"
echo "  qdrant snapshot: $SNAPSHOT_NAME"
