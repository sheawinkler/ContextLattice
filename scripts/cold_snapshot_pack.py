#!/usr/bin/env python3
"""Compress cold snapshot files with integrity verification and catalog output.

This script targets only *.snapshot files under CONTEXTLATTICE_COLD_ROOT (or --cold-root)
and writes compressed siblings as *.snapshot.zst.

Safety properties:
- No deletion outside cold root
- Optional hash verification before original deletion
- Catalog JSONL written for audit/restore metadata
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Iterator


@dataclass
class PackResult:
    source: Path
    target: Path
    source_bytes: int
    target_bytes: int
    sha256: str
    verified: bool
    removed_source: bool
    skipped: bool = False
    reason: str = ""


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        while True:
            chunk = fh.read(1024 * 1024)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


def _iter_snapshots(root: Path) -> Iterator[Path]:
    for p in root.rglob("*.snapshot"):
        if p.is_file():
            yield p


def _run_zstd_compress(src: Path, dst: Path, level: int) -> None:
    dst.parent.mkdir(parents=True, exist_ok=True)
    cmd = [
        "zstd",
        "-q",
        f"-{max(1, min(19, int(level)))}",
        "-T0",
        "-f",
        "-o",
        str(dst),
        str(src),
    ]
    subprocess.run(cmd, check=True)


def _run_zstd_decompress_to_hash(src: Path) -> str:
    cmd = ["zstd", "-q", "-d", "-c", str(src)]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    assert proc.stdout is not None
    h = hashlib.sha256()
    while True:
        chunk = proc.stdout.read(1024 * 1024)
        if not chunk:
            break
        h.update(chunk)
    stderr = proc.stderr.read().decode("utf-8", errors="replace") if proc.stderr else ""
    rc = proc.wait()
    if rc != 0:
        raise RuntimeError(f"zstd decompress failed for {src}: {stderr[:400]}")
    return h.hexdigest()


def _safe_under(path: Path, root: Path) -> bool:
    try:
        path.resolve().relative_to(root.resolve())
        return True
    except Exception:
        return False


def _catalog_row(result: PackResult, cold_root: Path) -> dict[str, object]:
    now = datetime.now(timezone.utc).isoformat()
    return {
        "recorded_at": now,
        "source": str(result.source),
        "target": str(result.target),
        "source_rel": str(result.source.resolve().relative_to(cold_root.resolve())),
        "target_rel": str(result.target.resolve().relative_to(cold_root.resolve())),
        "source_bytes": int(result.source_bytes),
        "target_bytes": int(result.target_bytes),
        "savings_bytes": max(0, int(result.source_bytes - result.target_bytes)),
        "savings_ratio": round((1 - (result.target_bytes / result.source_bytes)), 6)
        if result.source_bytes > 0
        else 0.0,
        "sha256": result.sha256,
        "verified": bool(result.verified),
        "removed_source": bool(result.removed_source),
        "skipped": bool(result.skipped),
        "reason": result.reason,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--cold-root",
        default=os.getenv("CONTEXTLATTICE_COLD_ROOT", "./.data/cold"),
        help="Cold root directory",
    )
    parser.add_argument("--level", type=int, default=int(os.getenv("COLD_SNAPSHOT_ZSTD_LEVEL", "3")))
    parser.add_argument("--max-files", type=int, default=int(os.getenv("COLD_SNAPSHOT_PACK_MAX_FILES", "0")))
    parser.add_argument("--keep-original", action="store_true")
    parser.add_argument("--verify", action="store_true", default=True)
    parser.add_argument("--no-verify", dest="verify", action="store_false")
    parser.add_argument("--apply", action="store_true")
    parser.add_argument(
        "--catalog",
        default=os.getenv("COLD_SNAPSHOT_CATALOG", "_index/cold_snapshot_catalog.jsonl"),
        help="Catalog JSONL path under cold root (or absolute path)",
    )
    args = parser.parse_args()

    cold_root = Path(args.cold_root).expanduser()
    if not cold_root.exists():
        print(json.dumps({"ok": False, "error": f"cold root missing: {cold_root}"}))
        return 1

    snapshots = sorted(_iter_snapshots(cold_root))
    if args.max_files and args.max_files > 0:
        snapshots = snapshots[: args.max_files]

    if not snapshots:
        print(json.dumps({"ok": True, "scanned": 0, "packed": 0, "skipped": 0, "deleted": 0}))
        return 0
    if args.apply and shutil.which("zstd") is None:
        print(json.dumps({"ok": False, "error": "zstd binary not found on PATH"}))
        return 1

    catalog_path = Path(args.catalog)
    if not catalog_path.is_absolute():
        catalog_path = cold_root / catalog_path
    catalog_path.parent.mkdir(parents=True, exist_ok=True)

    results: list[PackResult] = []
    packed = 0
    skipped = 0
    deleted = 0

    for src in snapshots:
        dst = src.with_suffix(src.suffix + ".zst")
        if dst.exists() and dst.stat().st_size > 0:
            results.append(
                PackResult(
                    source=src,
                    target=dst,
                    source_bytes=src.stat().st_size,
                    target_bytes=dst.stat().st_size,
                    sha256="",
                    verified=False,
                    removed_source=False,
                    skipped=True,
                    reason="compressed_exists",
                )
            )
            skipped += 1
            continue

        source_bytes = src.stat().st_size
        sha = _sha256_file(src)

        if args.apply:
            _run_zstd_compress(src, dst, level=args.level)
            verified = True
            if args.verify:
                restored_sha = _run_zstd_decompress_to_hash(dst)
                verified = restored_sha == sha
                if not verified:
                    raise RuntimeError(f"hash verify failed for {src} -> {dst}")
            removed = False
            if not args.keep_original:
                if not _safe_under(src, cold_root):
                    raise RuntimeError(f"refusing delete outside cold root: {src}")
                src.unlink()
                removed = True
                deleted += 1
            target_bytes = dst.stat().st_size
            results.append(
                PackResult(
                    source=src,
                    target=dst,
                    source_bytes=source_bytes,
                    target_bytes=target_bytes,
                    sha256=sha,
                    verified=verified,
                    removed_source=removed,
                )
            )
            packed += 1
        else:
            # Dry-run estimation by temporarily compressing into /tmp can be expensive;
            # keep dry-run lightweight and deterministic.
            results.append(
                PackResult(
                    source=src,
                    target=dst,
                    source_bytes=source_bytes,
                    target_bytes=0,
                    sha256=sha,
                    verified=False,
                    removed_source=False,
                    skipped=True,
                    reason="dry_run",
                )
            )
            skipped += 1

    with catalog_path.open("a", encoding="utf-8") as fh:
        for row in results:
            fh.write(json.dumps(_catalog_row(row, cold_root), ensure_ascii=True) + "\n")

    source_total = sum(r.source_bytes for r in results if not r.skipped)
    target_total = sum(r.target_bytes for r in results if not r.skipped)
    savings = max(0, source_total - target_total)

    payload = {
        "ok": True,
        "apply": bool(args.apply),
        "verify": bool(args.verify),
        "keep_original": bool(args.keep_original),
        "scanned": len(snapshots),
        "packed": packed,
        "skipped": skipped,
        "deleted_originals": deleted,
        "source_bytes": source_total,
        "target_bytes": target_total,
        "savings_bytes": savings,
        "catalog": str(catalog_path),
    }
    print(json.dumps(payload))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        raise
    except Exception as exc:
        print(json.dumps({"ok": False, "error": str(exc)}))
        raise
