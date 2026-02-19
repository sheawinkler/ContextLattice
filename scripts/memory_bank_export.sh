#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"
EXPORT_BASE="${MEMORY_BANK_EXPORT_DIR:-$ROOT_DIR/tmp/memory_exports}"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
EXPORT_DIR="${EXPORT_DIR:-$EXPORT_BASE/$TIMESTAMP}"
MEMMCP_DASHBOARD_URL="${MEMMCP_DASHBOARD_URL:-}"
MEMMCP_DASHBOARD_API_KEY="${MEMMCP_DASHBOARD_API_KEY:-}"

if [[ -n "$ORCH_API_KEY" ]]; then
  ORCH_AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
else
  ORCH_AUTH_HEADER=()
fi

encode_path() {
  python3 - "$1" <<'PY'
import sys
from urllib.parse import quote

raw = sys.argv[1]
parts = [quote(p, safe="") for p in raw.split("/") if p]
print("/".join(parts))
PY
}

mkdir -p "$EXPORT_DIR"
export_count=0

projects_json="$(curl -fsS "$ORCH_URL/projects" "${ORCH_AUTH_HEADER[@]}")"
projects="$(python3 - <<'PY' "$projects_json"
import json,sys
data=json.loads(sys.argv[1])
for item in data.get("projects", []):
    name=item.get("name")
    if name:
        print(name)
PY
)"

if [[ -z "$projects" ]]; then
  echo "No projects found to export."
  exit 0
fi

echo "Exporting memory files to $EXPORT_DIR"

while IFS= read -r project; do
  files_json="$(curl -fsS "$ORCH_URL/projects/${project}/files" "${ORCH_AUTH_HEADER[@]}")"
  files="$(python3 - <<'PY' "$files_json"
import json,sys
data=json.loads(sys.argv[1])
for name in data.get("files", []):
    if name:
        print(name)
PY
)"
  if [[ -z "$files" ]]; then
    continue
  fi
  while IFS= read -r file_path; do
    encoded_path="$(encode_path "$file_path")"
    dest="$EXPORT_DIR/$project/$file_path"
    mkdir -p "$(dirname "$dest")"
    curl -fsS "$ORCH_URL/memory/files/${project}/${encoded_path}" \
      "${ORCH_AUTH_HEADER[@]}" > "$dest"
    export_count=$((export_count+1))
  done <<< "$files"
done <<< "$projects"

echo "Export complete."

if [[ -n "$MEMMCP_DASHBOARD_URL" && -n "$MEMMCP_DASHBOARD_API_KEY" ]]; then
  curl -fsS "$MEMMCP_DASHBOARD_URL/api/workspace/audit" \
    -H "content-type: application/json" \
    -H "x-api-key: $MEMMCP_DASHBOARD_API_KEY" \
    -d "{\"action\":\"memory.export\",\"targetType\":\"memory\",\"metadata\":{\"exportDir\":\"$EXPORT_DIR\",\"files\":$export_count}}" >/dev/null || true
fi
