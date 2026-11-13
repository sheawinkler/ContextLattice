#!/usr/bin/env python3
"""Run reviewed workflow-side containment tooling without changing tagged source."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


TOOL_PATHS = (
    "scripts/release_native_toolchain_preflight.py", "scripts/release_native_ci_profile.py",
    "scripts/release_native_containment_holdouts.py", "scripts/agent/audit-public-installer-package",
)


def extract_tooling(commit: str, directory: Path, source: Path) -> dict[str, str]:
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("invalid_workflow_tooling_commit")
    hashes = {}
    for name in TOOL_PATHS:
        result = subprocess.run(["git", "show", commit + ":" + name], capture_output=True, check=True, timeout=15)
        if len(result.stdout) > 1024 * 1024:
            raise ValueError("unbounded_workflow_tooling")
        path = directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(result.stdout)
        hashes[name] = hashlib.sha256(result.stdout).hexdigest()
    if (directory / TOOL_PATHS[-1]).read_bytes() != (source / TOOL_PATHS[-1]).read_bytes():
        raise ValueError("tagged_audit_containment_differs_from_reviewed_tooling")
    return hashes


def command_report(argv: list[str], output: Path) -> bool:
    result = subprocess.run(argv, capture_output=True, timeout=180, check=False)
    if len(result.stdout) > 4 * 1024 * 1024 or len(result.stderr) > 16384:
        raise ValueError("unbounded_native_diagnostic")
    output.write_bytes(result.stdout)
    report = json.loads(result.stdout)
    return result.returncode == 0 and report.get("ok") is True and report.get("artifact_audit") is False


def main() -> int:
    receipt: dict = {"schema_id": "contextlattice_native_ci_gate.v1", "ok": False, "artifact_audit": False}
    temporary = Path(os.environ["RUNNER_TEMP"])
    try:
        if Path("/proc/sys/kernel/apparmor_restrict_unprivileged_userns").read_text().strip() != "1":
            raise ValueError("global_user_namespace_restriction_not_enforced")
        commit = os.environ["NATIVE_TOOLING_COMMIT"]
        if commit != os.environ.get("GITHUB_WORKFLOW_SHA"):
            raise ValueError("tooling_not_bound_to_executing_workflow")
        with tempfile.TemporaryDirectory(prefix="native-reviewed-tooling-", dir=temporary) as directory:
            tooling = Path(directory)
            receipt["workflow_commit"] = commit
            receipt["source_commit"] = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True, timeout=10).strip()
            receipt["tooling_hashes"] = extract_tooling(commit, tooling, Path.cwd())
            scripts = tooling / "scripts"
            if not command_report([sys.executable, str(scripts / "release_native_toolchain_preflight.py")], temporary / "native-containment-baseline.json"):
                if not command_report([sys.executable, str(scripts / "release_native_ci_profile.py"), "--prepare-github-hosted-ci"], temporary / "native-containment-profile.json"):
                    raise ValueError("reviewed_native_preparation_failed")
            if not command_report([sys.executable, str(scripts / "release_native_toolchain_preflight.py")], temporary / "native-containment-final.json"):
                raise ValueError("final_native_probe_failed")
            if not command_report([sys.executable, str(scripts / "release_native_containment_holdouts.py")], temporary / "native-containment-holdouts.json"):
                raise ValueError("native_isolation_holdouts_failed")
            if Path("/proc/sys/kernel/apparmor_restrict_unprivileged_userns").read_text().strip() != "1":
                raise ValueError("global_user_namespace_restriction_changed")
            receipt["ok"] = True
    except (ValueError, OSError, subprocess.SubprocessError, KeyError) as exc:
        receipt["error"] = str(exc) if isinstance(exc, ValueError) else type(exc).__name__
    (temporary / "native-containment-gate.json").write_text(json.dumps(receipt, sort_keys=True) + "\n")
    print(json.dumps(receipt, sort_keys=True))
    return 0 if receipt["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
