#!/usr/bin/env python3
"""Qdrant snapshot + retention helper.

Typical usage:
  python scripts/qdrant_snapshot_prune.py \
    --qdrant-url http://localhost:6333 \
    --collection memmcp_notes \
    --retention-days 14 \
    --snapshot-dir ./.data/cold/qdrant

Hour-level retention is also supported:
  python scripts/qdrant_snapshot_prune.py \
    --qdrant-url http://localhost:6333 \
    --collection memmcp_notes \
    --retention-hours 1 \
    --snapshot-dir ./.data/cold/qdrant

Notes:
- Pruning only affects points that have a numeric payload field `ts` (epoch seconds).
  Older points without `ts` will not be deleted by this script.
- Snapshot download happens over HTTP; ensure Qdrant is reachable from the host.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict


def _request_json(method: str, url: str, timeout_secs: float = 30.0, **kwargs: Any) -> Dict[str, Any]:
    payload = kwargs.get("json")
    headers = {"accept": "application/json"}
    data: bytes | None = None
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["content-type"] = "application/json"
    req = urllib.request.Request(url, method=method.upper(), data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout_secs) as resp:
            body = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        text = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {exc.code} for {url}: {text[:400]}") from exc
    if not body:
        return {}
    parsed = json.loads(body)
    if not isinstance(parsed, dict):
        raise RuntimeError(f"Unexpected JSON payload from {url}: {type(parsed).__name__}")
    return parsed


def create_snapshot(qdrant_url: str, collection: str, timeout_secs: float = 300.0) -> str:
    data = _request_json(
        "POST",
        f"{qdrant_url.rstrip('/')}/collections/{collection}/snapshots",
        timeout_secs=timeout_secs,
    )
    name = data.get("result", {}).get("name")
    if not name:
        raise RuntimeError(f"Unexpected snapshot response: {data}")
    return str(name)


def download_snapshot(
    qdrant_url: str,
    collection: str,
    snapshot_name: str,
    out_path: Path,
    timeout_secs: float = 300.0,
) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    url = f"{qdrant_url.rstrip('/')}/collections/{collection}/snapshots/{snapshot_name}"
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout_secs) as resp:
            with out_path.open("wb") as handle:
                while True:
                    chunk = resp.read(1024 * 1024)
                    if not chunk:
                        break
                    handle.write(chunk)
    except urllib.error.HTTPError as exc:
        text = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {exc.code} for {url}: {text[:400]}") from exc


def prune_by_ts(
    qdrant_url: str,
    collection: str,
    retention_seconds: int,
    timeout_secs: float = 120.0,
) -> Dict[str, Any]:
    cutoff = int(time.time()) - int(retention_seconds)
    payload = {
        "filter": {
            "must": [
                {
                    "key": "ts",
                    "range": {"lt": cutoff},
                }
            ]
        }
    }
    return _request_json(
        "POST",
        f"{qdrant_url.rstrip('/')}/collections/{collection}/points/delete?wait=true",
        timeout_secs=timeout_secs,
        json=payload,
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--qdrant-url",
        default=os.getenv("QDRANT_URL", "http://localhost:6333"),
        help="Qdrant base URL (host-accessible)",
    )
    parser.add_argument(
        "--collection",
        default=os.getenv("QDRANT_COLLECTION", os.getenv("ORCH_QDRANT_COLLECTION", "memmcp_notes")),
        help="Collection name to snapshot/prune",
    )
    parser.add_argument(
        "--retention-days",
        type=int,
        default=int(os.getenv("QDRANT_RETENTION_DAYS", "14")),
        help="Delete points with payload.ts older than this many days (default: 14)",
    )
    parser.add_argument(
        "--retention-hours",
        type=int,
        default=int(os.getenv("QDRANT_RETENTION_HOURS", "0")),
        help="If >0, overrides --retention-days and prunes points older than this many hours",
    )
    parser.add_argument(
        "--snapshot-dir",
        default=os.getenv("MEMMCP_COLD_ROOT", "./.data/cold/qdrant"),
        help="Directory to write downloaded snapshots",
    )
    parser.add_argument(
        "--timeout-secs",
        type=float,
        default=float(os.getenv("QDRANT_HTTP_TIMEOUT_SECS", "300")),
        help="HTTP timeout in seconds for snapshot/prune operations",
    )
    parser.add_argument("--skip-snapshot", action="store_true")
    parser.add_argument("--skip-prune", action="store_true")
    args = parser.parse_args()

    qdrant_url = args.qdrant_url
    collection = args.collection
    snapshot_dir = Path(args.snapshot_dir)

    snapshot_name = None
    if not args.skip_snapshot:
        print(f"[qdrant] creating snapshot for collection={collection} ...")
        snapshot_name = create_snapshot(qdrant_url, collection, timeout_secs=args.timeout_secs)
        out_path = snapshot_dir / collection / snapshot_name
        print(f"[qdrant] downloading snapshot {snapshot_name} -> {out_path}")
        download_snapshot(
            qdrant_url,
            collection,
            snapshot_name,
            out_path,
            timeout_secs=args.timeout_secs,
        )

    if not args.skip_prune:
        if args.retention_hours and args.retention_hours > 0:
            retention_seconds = int(args.retention_hours) * 3600
            print(f"[qdrant] pruning points with ts older than {args.retention_hours} hours ...")
        else:
            retention_seconds = int(args.retention_days) * 86400
            print(f"[qdrant] pruning points with ts older than {args.retention_days} days ...")
        result = prune_by_ts(
            qdrant_url,
            collection,
            retention_seconds,
            timeout_secs=args.timeout_secs,
        )
        print(result)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        raise
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        sys.exit(1)
