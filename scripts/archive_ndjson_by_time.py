#!/usr/bin/env python3
"""Archive NDJSON files by timestamp, keeping a hot window.

This is meant for append-only NDJSON history files (one JSON object per line),
like the memmcp-orchestrator telemetry histories.

Example:
  python scripts/archive_ndjson_by_time.py \
    --file ./.data/orchestrator/trading_metrics.ndjson \
    --retention-hours 48 \
    --cold-dir ./.data/cold/telemetry

Behavior:
- Lines with a parseable timestamp older than the cutoff are appended to a gzipped
  cold file (partitioned by date), then removed from the hot file.
- Lines that can't be parsed are kept in the hot file (to avoid accidental loss).

Timestamp parsing:
- Accepts ISO8601, with or without timezone. 'Z' is treated as UTC.
"""

from __future__ import annotations

import argparse
import gzip
import json
import os
import sys
from collections import defaultdict
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import IO, Any, Dict, Iterable


def _parse_iso_ts(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()

    # Normalize common UTC suffix
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"

    try:
        dt = datetime.fromisoformat(text)
    except ValueError:
        return None

    if dt.tzinfo is None:
        # Assume UTC if timezone is missing.
        dt = dt.replace(tzinfo=UTC)
    return dt.astimezone(UTC)


def _iter_lines(path: Path) -> Iterable[str]:
    with path.open("r", encoding="utf-8") as handle:
        for line in handle:
            yield line


def _atomic_write(path: Path, lines: list[str]) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("w", encoding="utf-8") as handle:
        for line in lines:
            handle.write(line)
    os.replace(tmp, path)


def archive_file(
    file_path: Path,
    cold_dir: Path,
    retention_hours: int,
    timestamp_field: str,
) -> Dict[str, int]:
    cutoff = datetime.now(tz=UTC) - timedelta(hours=retention_hours)

    kept: list[str] = []
    moved_count = 0
    kept_count = 0

    # date_key -> list of raw ndjson lines
    buckets: dict[str, list[str]] = defaultdict(list)

    for raw in _iter_lines(file_path):
        line = raw.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            kept.append(raw)
            kept_count += 1
            continue

        dt = _parse_iso_ts(obj.get(timestamp_field)) if isinstance(obj, dict) else None
        if dt is None:
            kept.append(raw)
            kept_count += 1
            continue

        if dt < cutoff:
            date_key = dt.strftime("%Y%m%d")
            buckets[date_key].append(line + "\n")
            moved_count += 1
        else:
            kept.append(line + "\n")
            kept_count += 1

    # Write cold buckets
    if buckets:
        base = file_path.stem
        for date_key, lines in buckets.items():
            out_dir = cold_dir / base
            out_dir.mkdir(parents=True, exist_ok=True)
            out_path = out_dir / f"{base}.{date_key}.ndjson.gz"
            with gzip.open(out_path, "at", encoding="utf-8") as gz:
                for l in lines:
                    gz.write(l)

    # Rewrite hot file
    _atomic_write(file_path, kept)

    return {"moved": moved_count, "kept": kept_count}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--file",
        action="append",
        dest="files",
        help="NDJSON file to archive (repeatable)",
    )
    parser.add_argument(
        "--data-dir",
        default=os.getenv("ORCHESTRATOR_DATA_DIR", ""),
        help="Directory containing NDJSON files (alternative to --file)",
    )
    parser.add_argument(
        "--retention-hours",
        type=int,
        default=int(os.getenv("TELEMETRY_HOT_HOURS", "48")),
    )
    parser.add_argument(
        "--cold-dir",
        default=os.getenv("MEMMCP_COLD_ROOT", "./.data/cold/telemetry"),
    )
    parser.add_argument(
        "--timestamp-field",
        default=os.getenv("TELEMETRY_TIMESTAMP_FIELD", "timestamp"),
    )
    args = parser.parse_args()

    cold_dir = Path(args.cold_dir)

    file_paths: list[Path] = []
    if args.files:
        file_paths.extend(Path(p) for p in args.files)
    if args.data_dir:
        data_dir = Path(args.data_dir)
        if data_dir.exists():
            for name in (
                "trading_metrics.ndjson",
                "strategy_metrics.ndjson",
                "solana_signals.ndjson",
                "solana_overrides.ndjson",
            ):
                p = data_dir / name
                if p.exists():
                    file_paths.append(p)

    file_paths = [p for p in file_paths if p.exists()]
    if not file_paths:
        print("No files found to archive. Use --file or --data-dir.", file=sys.stderr)
        sys.exit(2)

    for fp in file_paths:
        stats = archive_file(
            fp,
            cold_dir=cold_dir,
            retention_hours=args.retention_hours,
            timestamp_field=args.timestamp_field,
        )
        print(f"{fp}: moved={stats['moved']} kept={stats['kept']}")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        raise
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        sys.exit(1)
