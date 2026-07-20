#!/usr/bin/env python3
"""Audit ContextLattice-owned Node runtimes and GitHub Actions for Node 24."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
from pathlib import Path
from typing import Any


TARGET_NODE = "24.18.0"
TARGET_NPM = "11.16.0"
NODE_ENGINE = ">=24.18.0 <25"
NPM_ENGINE = ">=11.16.0 <12"

ACTION_MINIMUM_MAJORS = {
    "actions/checkout": 5,
    "actions/setup-node": 5,
    "actions/setup-go": 6,
    "actions/cache": 5,
    "actions/upload-artifact": 6,
    "actions/download-artifact": 6,
    "actions/labeler": 6,
    "actions/attest-build-provenance": 4,
    "softprops/action-gh-release": 3,
}

EXPECTED_DOCKER_BASES = {
    "Dockerfile.dashboard": f"node:{TARGET_NODE}-bookworm-slim",
    "Dockerfile.mcp-gateway": f"node:{TARGET_NODE}-bookworm-slim",
    "Dockerfile.memorymcp": f"node:{TARGET_NODE}-alpine",
}

USES_RE = re.compile(r"\buses:\s*([^@\s]+)@v(\d+)\b")
NODE_FROM_RE = re.compile(r"^\s*FROM\s+node:([^\s]+)", re.MULTILINE)
NODE_VERSION_RE = re.compile(r"\bnode-version:\s*['\"]?([^\s'\"]+)")


def finding(kind: str, path: str, detail: str) -> dict[str, str]:
    return {"kind": kind, "path": path, "detail": detail}


def read_text(root: Path, relative: str, findings: list[dict[str, str]]) -> str:
    path = root / relative
    if not path.is_file():
        findings.append(finding("missing_file", relative, "required Node 24 contract file is absent"))
        return ""
    return path.read_text(encoding="utf-8")


def read_json(root: Path, relative: str, findings: list[dict[str, str]]) -> dict[str, Any]:
    text = read_text(root, relative, findings)
    if not text:
        return {}
    try:
        payload = json.loads(text)
    except json.JSONDecodeError as exc:
        findings.append(finding("invalid_json", relative, str(exc)))
        return {}
    if not isinstance(payload, dict):
        findings.append(finding("invalid_json_shape", relative, "expected a JSON object"))
        return {}
    return payload


def audit_version_files(root: Path, findings: list[dict[str, str]]) -> None:
    for relative in (".node-version", ".nvmrc"):
        actual = read_text(root, relative, findings).strip()
        if actual and actual != TARGET_NODE:
            findings.append(finding("node_version_mismatch", relative, f"expected {TARGET_NODE}, got {actual}"))


def audit_dashboard_package(root: Path, findings: list[dict[str, str]]) -> None:
    package_path = "contextlattice-dashboard/package.json"
    lock_path = "contextlattice-dashboard/package-lock.json"
    package = read_json(root, package_path, findings)
    lock = read_json(root, lock_path, findings)

    if package.get("packageManager") != f"npm@{TARGET_NPM}":
        findings.append(finding("package_manager_mismatch", package_path, f"expected npm@{TARGET_NPM}"))

    engines = package.get("engines") if isinstance(package.get("engines"), dict) else {}
    if engines.get("node") != NODE_ENGINE:
        findings.append(finding("node_engine_mismatch", package_path, f"expected {NODE_ENGINE}"))
    if engines.get("npm") != NPM_ENGINE:
        findings.append(finding("npm_engine_mismatch", package_path, f"expected {NPM_ENGINE}"))

    dev_dependencies = package.get("devDependencies") if isinstance(package.get("devDependencies"), dict) else {}
    node_types = str(dev_dependencies.get("@types/node", ""))
    if not re.match(r"^[~^]?24\.", node_types):
        findings.append(finding("node_types_mismatch", package_path, f"expected @types/node 24.x, got {node_types or 'missing'}"))

    packages = lock.get("packages") if isinstance(lock.get("packages"), dict) else {}
    root_package = packages.get("") if isinstance(packages.get(""), dict) else {}
    if not root_package:
        findings.append(finding("lock_root_missing", lock_path, "packages[''] is missing"))
        return
    if root_package.get("engines") != package.get("engines"):
        findings.append(finding("lock_engine_drift", lock_path, "root engines do not match package.json"))
    lock_dev = root_package.get("devDependencies") if isinstance(root_package.get("devDependencies"), dict) else {}
    if lock_dev.get("@types/node") != dev_dependencies.get("@types/node"):
        findings.append(finding("lock_node_types_drift", lock_path, "root @types/node does not match package.json"))

    allow_scripts = package.get("allowScripts") if isinstance(package.get("allowScripts"), dict) else {}
    if not allow_scripts:
        findings.append(finding("install_script_policy_missing", package_path, "reviewed dependency install scripts must be pinned"))
    locked_packages = {
        f"{path.rsplit('node_modules/', 1)[-1]}@{metadata['version']}"
        for path, metadata in packages.items()
        if path and "node_modules/" in path and isinstance(metadata, dict) and isinstance(metadata.get("version"), str)
    }
    for dependency, allowed in allow_scripts.items():
        if allowed is not True or not re.search(r"@[^@]+$", dependency):
            findings.append(finding("install_script_policy_unpinned", package_path, f"expected a true version pin, got {dependency}: {allowed}"))
        elif dependency not in locked_packages:
            findings.append(
                finding(
                    "install_script_policy_stale",
                    package_path,
                    f"approved install script is absent from package-lock.json: {dependency}",
                )
            )

    npmrc = read_text(root, "contextlattice-dashboard/.npmrc", findings)
    if not re.search(r"^engine-strict=true$", npmrc, re.MULTILINE):
        findings.append(finding("engine_strict_disabled", "contextlattice-dashboard/.npmrc", "engine-strict=true is required"))
    if not re.search(r"^strict-allow-scripts=true$", npmrc, re.MULTILINE):
        findings.append(finding("install_script_policy_advisory", "contextlattice-dashboard/.npmrc", "strict-allow-scripts=true is required"))


def audit_dockerfiles(root: Path, findings: list[dict[str, str]]) -> None:
    for relative, expected in EXPECTED_DOCKER_BASES.items():
        text = read_text(root, relative, findings)
        match = NODE_FROM_RE.search(text)
        actual = f"node:{match.group(1)}" if match else "missing"
        if actual != expected:
            findings.append(finding("docker_node_mismatch", relative, f"expected {expected}, got {actual}"))

    for path in sorted(root.glob("Dockerfile*")):
        text = path.read_text(encoding="utf-8")
        for match in NODE_FROM_RE.finditer(text):
            if not match.group(1).startswith("24."):
                findings.append(finding("non_node24_base", path.name, f"found node:{match.group(1)}"))


def audit_dashboard_production_contract(root: Path, findings: list[dict[str, str]]) -> None:
    relative = "Dockerfile.dashboard"
    text = read_text(root, relative, findings)
    required_patterns = {
        "dashboard_build_missing": r"\b(?:npm run build|next build)\b",
        "dashboard_production_env_missing": r"^\s*ENV\s+NODE_ENV=production\s*$",
        "dashboard_start_missing": r"\b(?:npm run start|next start)\b",
        "dashboard_healthcheck_missing": r"\bHEALTHCHECK\b[\s\S]*?/api/public/auth-mode",
        "dashboard_runtime_schema_push_missing": r"\bprisma db push\b",
    }
    for kind, pattern in required_patterns.items():
        if not re.search(pattern, text, re.MULTILINE):
            findings.append(finding(kind, relative, f"required production contract pattern is absent: {pattern}"))

    forbidden_patterns = {
        "dashboard_dev_server": r"\b(?:npm run dev|next dev)\b",
        "dashboard_npx_network_install": r"\bnpx\s+--yes\b",
    }
    for kind, pattern in forbidden_patterns.items():
        if re.search(pattern, text):
            findings.append(finding(kind, relative, f"forbidden dashboard runtime pattern found: {pattern}"))


def audit_workflows(root: Path, findings: list[dict[str, str]]) -> dict[str, int]:
    action_majors: dict[str, int] = {}
    workflow_root = root / ".github" / "workflows"
    workflow_paths = sorted((*workflow_root.glob("*.yml"), *workflow_root.glob("*.yaml")))
    for path in workflow_paths:
        text = path.read_text(encoding="utf-8")
        relative = path.relative_to(root).as_posix()
        for action, raw_major in USES_RE.findall(text):
            major = int(raw_major)
            action_majors[action] = min(major, action_majors.get(action, major))
            minimum = ACTION_MINIMUM_MAJORS.get(action)
            if minimum is not None and major < minimum:
                findings.append(finding("node20_action", relative, f"{action}@v{major} must be v{minimum} or newer"))
        for version in NODE_VERSION_RE.findall(text):
            if not version.startswith("24"):
                findings.append(finding("workflow_node_version", relative, f"expected Node 24, got {version}"))

    if action_majors.get("actions/setup-node", 0) < ACTION_MINIMUM_MAJORS["actions/setup-node"]:
        findings.append(finding("node24_ci_missing", ".github/workflows", "a Node 24 setup-node gate is required"))
    return action_majors


def command_version(command: str) -> str:
    proc = subprocess.run([command, "--version"], capture_output=True, check=False, text=True)
    if proc.returncode != 0:
        return ""
    return proc.stdout.strip().lstrip("v")


def audit(root: Path, check_local: bool = False) -> dict[str, Any]:
    findings: list[dict[str, str]] = []
    audit_version_files(root, findings)
    audit_dashboard_package(root, findings)
    audit_dockerfiles(root, findings)
    audit_dashboard_production_contract(root, findings)
    action_majors = audit_workflows(root, findings)
    local_runtime: dict[str, str] = {}
    if check_local:
        local_runtime = {"node": command_version("node"), "npm": command_version("npm")}
        if local_runtime["node"] != TARGET_NODE:
            findings.append(finding("local_node_mismatch", "PATH", f"expected {TARGET_NODE}, got {local_runtime['node'] or 'missing'}"))
        if local_runtime["npm"] != TARGET_NPM:
            findings.append(finding("local_npm_mismatch", "PATH", f"expected {TARGET_NPM}, got {local_runtime['npm'] or 'missing'}"))
    return {
        "schema_id": "contextlattice_node_runtime_audit.v1",
        "ok": not findings,
        "target": {"node": TARGET_NODE, "npm": TARGET_NPM, "major": 24},
        "action_majors": dict(sorted(action_majors.items())),
        "local_runtime": local_runtime,
        "findings": findings,
        "checked": {
            "version_files": 2,
            "dockerfiles": len(EXPECTED_DOCKER_BASES),
            "dashboard_package": True,
            "dashboard_production_contract": True,
            "workflow_count": len(list((root / ".github" / "workflows").glob("*.y*ml"))),
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(Path(__file__).resolve().parents[1]))
    parser.add_argument("--pretty", action="store_true")
    parser.add_argument("--summary", action="store_true")
    parser.add_argument("--check-local", action="store_true")
    args = parser.parse_args()

    payload = audit(Path(args.root).resolve(), check_local=args.check_local)
    if args.summary and payload["ok"]:
        print(f"node24 runtime audit passed: node={TARGET_NODE} npm={TARGET_NPM}")
    else:
        print(json.dumps(payload, indent=2 if args.pretty else None, sort_keys=True))
    return 0 if payload["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
