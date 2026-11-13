#!/usr/bin/env python3
"""Probe native containment with trusted version commands, never an installer."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]


def probe(root: Path) -> dict[str, object]:
    report: dict[str, object] = {
        "schema_id": "contextlattice_native_toolchain_preflight.v1",
        "ok": False,
        "purpose": "benign_version_containment_only",
        "artifact_audit": False,
        "cwd": str(Path.cwd()),
        "tools": [],
    }
    if sys.platform != "linux":
        report["error"] = "native_probe_requires_linux"
        return report
    paths = {name: shutil.which(name) for name in ("bwrap", "msiinfo", "msiextract")}
    if any(value is None for value in paths.values()):
        report["error"] = "native_tool_unavailable"
        return report
    audit_path = root / "scripts/agent/audit-public-installer-package"
    audit = runpy.run_path(str(audit_path), run_name="native_preflight_import")
    report["audit_helper_sha256"] = hashlib.sha256(audit_path.read_bytes()).hexdigest()
    rows: list[dict[str, object]] = []
    with tempfile.TemporaryDirectory(prefix="contextlattice-native-preflight-") as temporary:
        scratch = Path(temporary)
        artifact = scratch / "benign.input"
        artifact.write_bytes(b"not an installer; never parsed\n")
        output = scratch / "output"
        output.mkdir(mode=0o700)
        for name in ("msiinfo", "msiextract"):
            tool = Path(paths[name]).resolve()
            argv = audit["_confined_bwrap_command"](
                bwrap_path=str(Path(paths["bwrap"]).resolve()),
                tool_path=str(tool),
                artifact=artifact,
                output_root=output,
                tool_arguments=["--version"],
            )
            row: dict[str, object] = {
                "tool": name,
                "executable": str(tool),
                "sha256": hashlib.sha256(tool.read_bytes()).hexdigest(),
                "argv": argv,
            }
            try:
                result = subprocess.run(
                    argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE, env={"PATH": "/usr/bin:/bin"},
                    check=False, timeout=15,
                )
                row.update({
                    "returncode": result.returncode,
                    "stdout": result.stdout[:2048].decode("utf-8", errors="replace"),
                    "stderr": result.stderr[:2048].decode("utf-8", errors="replace"),
                    "output_truncated": max(len(result.stdout), len(result.stderr)) > 2048,
                })
            except (OSError, subprocess.SubprocessError) as exc:
                row.update({"returncode": None, "error": type(exc).__name__})
            rows.append(row)
    report["tools"] = rows
    report["ok"] = bool(rows) and all(row["returncode"] == 0 for row in rows)
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT)
    args = parser.parse_args()
    result = probe(args.root.resolve())
    print(json.dumps(result, sort_keys=True))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
