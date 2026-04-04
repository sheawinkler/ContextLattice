#!/usr/bin/env python3
"""Tiered retention for cold snapshot artifacts (.snapshot / .snapshot.zst).

Policy keeps recent dense recovery points while shrinking long-tail storage:
- latest K files always kept
- one per day for N days
- one per ISO week for W weeks beyond daily window
- one per month for M months beyond weekly window
"""

from __future__ import annotations

import argparse
import json
import os
import re
from collections import defaultdict
from dataclasses import dataclass
from datetime import datetime, timezone, timedelta
from pathlib import Path

_TS_RE = re.compile(r"(\d{4})-(\d{2})-(\d{2})-(\d{2})-(\d{2})-(\d{2})")


@dataclass
class Entry:
    path: Path
    ts: datetime
    size: int
    bucket: str


def _parse_ts(path: Path) -> datetime | None:
    m = _TS_RE.search(path.name)
    if not m:
        return None
    y, mo, d, hh, mm, ss = map(int, m.groups())
    return datetime(y, mo, d, hh, mm, ss, tzinfo=timezone.utc)


def _iter_entries(root: Path) -> list[Entry]:
    out: list[Entry] = []
    for p in root.rglob("*.snapshot"):
        if p.is_file():
            ts = _parse_ts(p)
            if ts:
                out.append(Entry(path=p, ts=ts, size=p.stat().st_size, bucket=p.parent.name))
    for p in root.rglob("*.snapshot.zst"):
        if p.is_file():
            ts = _parse_ts(p)
            if ts:
                out.append(Entry(path=p, ts=ts, size=p.stat().st_size, bucket=p.parent.name))
    return sorted(out, key=lambda x: x.ts, reverse=True)


def _safe_under(path: Path, root: Path) -> bool:
    try:
        path.resolve().relative_to(root.resolve())
        return True
    except Exception:
        return False


def _pick_keep(entries: list[Entry], keep_latest: int, keep_daily: int, keep_weekly: int, keep_monthly: int) -> set[Path]:
    now = datetime.now(timezone.utc)
    daily_cutoff = now - timedelta(days=keep_daily)
    weekly_cutoff = now - timedelta(weeks=keep_daily // 7 + keep_weekly)

    keep: set[Path] = set()

    for item in entries[: max(0, keep_latest)]:
        keep.add(item.path)

    daily_keys: set[tuple[str, str]] = set()
    weekly_keys: set[tuple[str, str]] = set()
    monthly_keys: set[tuple[str, str]] = set()

    for item in entries:
        if item.path in keep:
            continue
        day_key = (item.bucket, item.ts.strftime("%Y-%m-%d"))
        week_key = (item.bucket, f"{item.ts.isocalendar().year}-W{item.ts.isocalendar().week:02d}")
        month_key = (item.bucket, item.ts.strftime("%Y-%m"))

        if item.ts >= daily_cutoff:
            if day_key not in daily_keys:
                daily_keys.add(day_key)
                keep.add(item.path)
            continue

        if item.ts >= weekly_cutoff:
            if len([k for k in weekly_keys if k[0] == item.bucket]) < keep_weekly and week_key not in weekly_keys:
                weekly_keys.add(week_key)
                keep.add(item.path)
            continue

        if len([k for k in monthly_keys if k[0] == item.bucket]) < keep_monthly and month_key not in monthly_keys:
            monthly_keys.add(month_key)
            keep.add(item.path)

    return keep


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cold-root", default=os.getenv("CONTEXTLATTICE_COLD_ROOT", "./.data/cold"))
    parser.add_argument("--keep-latest", type=int, default=int(os.getenv("COLD_SNAPSHOT_KEEP_LATEST", "6")))
    parser.add_argument("--keep-daily", type=int, default=int(os.getenv("COLD_SNAPSHOT_KEEP_DAILY", "21")))
    parser.add_argument("--keep-weekly", type=int, default=int(os.getenv("COLD_SNAPSHOT_KEEP_WEEKLY", "12")))
    parser.add_argument("--keep-monthly", type=int, default=int(os.getenv("COLD_SNAPSHOT_KEEP_MONTHLY", "12")))
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()

    root = Path(args.cold_root).expanduser()
    if not root.exists():
        print(json.dumps({"ok": False, "error": f"cold root missing: {root}"}))
        return 1

    entries = _iter_entries(root)
    if not entries:
        print(json.dumps({"ok": True, "scanned": 0, "kept": 0, "deleted": 0, "reclaimed_bytes": 0}))
        return 0

    keep = _pick_keep(entries, args.keep_latest, args.keep_daily, args.keep_weekly, args.keep_monthly)
    to_delete = [e for e in entries if e.path not in keep]

    reclaimed = 0
    deleted = 0
    if args.apply:
        for entry in to_delete:
            if not _safe_under(entry.path, root):
                raise RuntimeError(f"refusing delete outside cold root: {entry.path}")
            reclaimed += entry.size
            entry.path.unlink(missing_ok=True)
            deleted += 1

    per_bucket = defaultdict(lambda: {"kept": 0, "deleted": 0, "kept_bytes": 0, "deleted_bytes": 0})
    for e in entries:
        b = per_bucket[e.bucket]
        if e.path in keep:
            b["kept"] += 1
            b["kept_bytes"] += e.size
        else:
            b["deleted"] += 1
            b["deleted_bytes"] += e.size

    print(
        json.dumps(
            {
                "ok": True,
                "apply": bool(args.apply),
                "scanned": len(entries),
                "kept": len(keep),
                "deleted": deleted if args.apply else len(to_delete),
                "reclaimed_bytes": reclaimed if args.apply else sum(e.size for e in to_delete),
                "per_bucket": per_bucket,
            }
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
