#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="${ROOT}/launch_service/config/contextlattice.launch.json"

if [[ ! -f "${CONFIG}" ]]; then
  echo "error: missing config at ${CONFIG}" >&2
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "error: python3 is required" >&2
  exit 1
fi

if ! command -v open >/dev/null 2>&1; then
  echo "error: macOS 'open' command is required" >&2
  exit 1
fi

python3 - <<'PY' "${CONFIG}" | while IFS= read -r url; do
import json, sys
cfg = json.load(open(sys.argv[1]))
seen = set()
for ch in cfg.get("channels", []):
    status = str(ch.get("status", "")).lower()
    if status == "live":
        continue
    url = str(ch.get("listing_url", "")).strip()
    if not url or url in seen:
        continue
    seen.add(url)
    print(url)
PY
  echo "opening: ${url}"
  open "${url}" >/dev/null 2>&1 || true
done

echo "done: opened launch submission URLs for channels not marked Live."
