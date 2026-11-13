#!/usr/bin/env python3
"""Fixed benign isolation negatives; never execute or parse installer content."""

from __future__ import annotations

import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile

from release_native_toolchain_preflight import ROOT


HELPER = r'''
import ctypes, errno, json, os, pathlib, subprocess, sys
status = dict(line.split(":", 1) for line in pathlib.Path("/proc/self/status").read_text().splitlines() if ":" in line)
assert all(int(status[key].strip(), 16) == 0 for key in ("CapEff", "CapPrm", "CapAmb"))
label = pathlib.Path("/proc/self/attr/current").read_text().strip()
assert "unpriv_bwrap" in label and "enforce" in label
namespaces = {name: os.readlink("/proc/self/ns/" + name) for name in ("user", "mnt", "net", "pid")}
assert set(os.listdir("/sys/class/net")) <= {"lo"} if pathlib.Path("/sys/class/net").exists() else True
routes = pathlib.Path("/proc/net/route").read_text().splitlines()
assert len(routes) <= 1
try:
    pathlib.Path("/input/contextlattice.msi").write_bytes(b"must not be writable")
except OSError as exc:
    assert exc.errno in (errno.EROFS, errno.EACCES, errno.EPERM)
else:
    raise AssertionError("input mount writable")
pathlib.Path("/output/owned-scratch").write_text("writable only here")
libc = ctypes.CDLL(None, use_errno=True)
assert libc.sethostname(b"denied", 6) == -1 and ctypes.get_errno() == errno.EPERM
descendant = subprocess.run([sys.executable, "-I", "-c", "import pathlib; print(pathlib.Path('/proc/self/attr/current').read_text().strip())"], capture_output=True, check=True, timeout=5).stdout.decode().strip()
assert "unpriv_bwrap" in descendant and "enforce" in descendant
nested = libc.unshare(0x10000000 | 0x04000000)
if nested == 0:
    assert libc.sethostname(b"still-denied", 12) == -1 and ctypes.get_errno() == errno.EPERM
else:
    assert ctypes.get_errno() in (errno.EPERM, errno.EACCES)
print(json.dumps({"ok": True, "label": label, "descendant_label": descendant, "namespaces": namespaces, "capabilities_zero": True, "readonly_input": True, "scratch_writable": True, "host_routes_absent": True, "capability_operation_denied": True, "nested_namespace_created": nested == 0}))
'''


def main() -> int:
    report = {"schema_id": "contextlattice_native_containment_holdouts.v1", "ok": False, "artifact_audit": False}
    if os.geteuid() == 0 or not Path("/proc/self/ns").is_dir():
        print(json.dumps({**report, "error": "requires_unprivileged_linux"}))
        return 1
    host = {name: os.readlink("/proc/self/ns/" + name) for name in ("user", "mnt", "net", "pid")}
    audit = runpy.run_path(str(ROOT / "scripts/agent/audit-public-installer-package"), run_name="native_holdouts_import")
    with tempfile.TemporaryDirectory(prefix="contextlattice-isolation-") as directory:
        scratch = Path(directory)
        sentinel = scratch / "sentinel"
        original = b"controlled readonly sentinel, never an installer\n"
        sentinel.write_bytes(original)
        output = scratch / "output"
        output.mkdir()
        argv = audit["_confined_bwrap_command"](bwrap_path="/usr/bin/bwrap", tool_path=str(Path("/usr/bin/python3").resolve()), artifact=sentinel,
                                                output_root=output, tool_arguments=["-I", "-c", HELPER])
        result = subprocess.run(argv, stdin=subprocess.DEVNULL, capture_output=True, timeout=20, check=False, env={"PATH": "/usr/bin:/bin"})
        report.update({"returncode": result.returncode, "stderr": result.stderr[:2048].decode(errors="replace"), "host_namespaces": host})
        if result.returncode == 0 and len(result.stdout) <= 8192:
            child = json.loads(result.stdout)
            report["child"] = child
            report["ok"] = (child.get("ok") is True and all(child["namespaces"][key] != value for key, value in host.items()) and sentinel.read_bytes() == original)
    print(json.dumps(report, sort_keys=True))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
