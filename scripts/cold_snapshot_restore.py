#!/usr/bin/env python3
"""Restore a compressed cold snapshot (.snapshot.zst) to raw .snapshot.

By default this selects the latest snapshot for a collection directory and
writes the restored file under --out-dir.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
from pathlib import Path


def _latest_snapshot_zst(collection_dir: Path) -> Path | None:
    items = sorted([p for p in collection_dir.glob("*.snapshot.zst") if p.is_file()])
    return items[-1] if items else None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cold-root", default=os.getenv("CONTEXTLATTICE_COLD_ROOT", "./.data/cold"))
    parser.add_argument("--collection", required=True, help="Collection directory (e.g. memmcp_notes)")
    parser.add_argument("--snapshot", default="", help="Explicit .snapshot.zst file path (optional)")
    parser.add_argument("--out-dir", default="", help="Restore output directory (default: collection dir)")
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()

    if shutil.which("zstd") is None:
        print(json.dumps({"ok": False, "error": "zstd binary not found on PATH"}))
        return 1

    cold_root = Path(args.cold_root).expanduser()
    collection_dir = cold_root / args.collection
    if not collection_dir.exists():
        print(json.dumps({"ok": False, "error": f"collection dir missing: {collection_dir}"}))
        return 1

    if args.snapshot:
        src = Path(args.snapshot).expanduser()
    else:
        src = _latest_snapshot_zst(collection_dir)
        if src is None:
            print(json.dumps({"ok": False, "error": f"no .snapshot.zst files in {collection_dir}"}))
            return 1

    if src.suffix != ".zst" or not src.name.endswith(".snapshot.zst"):
        print(json.dumps({"ok": False, "error": f"expected .snapshot.zst file, got: {src}"}))
        return 1

    out_dir = Path(args.out_dir).expanduser() if args.out_dir else collection_dir
    out_dir.mkdir(parents=True, exist_ok=True)
    out_file = out_dir / src.name.removesuffix(".zst")

    if out_file.exists() and not args.force:
        print(json.dumps({"ok": False, "error": f"output exists: {out_file}; pass --force to overwrite"}))
        return 1

    cmd = ["zstd", "-q", "-d", "-f", "-o", str(out_file), str(src)]
    subprocess.run(cmd, check=True)

    print(
        json.dumps(
            {
                "ok": True,
                "source": str(src),
                "output": str(out_file),
                "output_bytes": out_file.stat().st_size,
            }
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
