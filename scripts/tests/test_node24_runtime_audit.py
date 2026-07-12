#!/usr/bin/env python3
"""Focused regression tests for the Node 24 runtime audit."""

from __future__ import annotations

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts" / "node24_runtime_audit.py"
SPEC = importlib.util.spec_from_file_location("node24_runtime_audit", MODULE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError(f"unable to load {MODULE_PATH}")
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


class Node24RuntimeAuditTests(unittest.TestCase):
    def test_repository_contract_passes(self) -> None:
        payload = AUDIT.audit(ROOT)
        self.assertTrue(payload["ok"], payload["findings"])

    def test_selected_runtime_matches_contract(self) -> None:
        payload = AUDIT.audit(ROOT, check_local=True)
        self.assertTrue(payload["ok"], payload["findings"])
        self.assertEqual(payload["local_runtime"], {"node": "24.18.0", "npm": "11.16.0"})

    def test_node20_action_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            workflow_dir = root / ".github" / "workflows"
            workflow_dir.mkdir(parents=True)
            (workflow_dir / "legacy.yml").write_text(
                "steps:\n  - uses: actions/checkout@v4\n  - uses: actions/setup-node@v4\n",
                encoding="utf-8",
            )
            findings: list[dict[str, str]] = []
            AUDIT.audit_workflows(root, findings)
            kinds = [item["kind"] for item in findings]
            self.assertIn("node20_action", kinds)
            self.assertIn("node24_ci_missing", kinds)

    def test_node20_container_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            (root / "Dockerfile.dashboard").write_text("FROM node:20-bookworm-slim\n", encoding="utf-8")
            (root / "Dockerfile.memorymcp").write_text("FROM node:24.18.0-alpine\n", encoding="utf-8")
            (root / "Dockerfile.mcp-gateway").write_text("FROM node:24.18.0-bookworm-slim\n", encoding="utf-8")
            findings: list[dict[str, str]] = []
            AUDIT.audit_dockerfiles(root, findings)
            kinds = [item["kind"] for item in findings]
            self.assertIn("docker_node_mismatch", kinds)
            self.assertIn("non_node24_base", kinds)

    def test_stale_install_script_approval_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            dashboard = root / "contextlattice-dashboard"
            dashboard.mkdir(parents=True)
            package = {
                "packageManager": "npm@11.16.0",
                "engines": {"node": ">=24.18.0 <25", "npm": ">=11.16.0 <12"},
                "devDependencies": {"@types/node": "^24.13.3"},
                "allowScripts": {"stale-package@1.0.0": True},
            }
            lock = {
                "packages": {
                    "": {
                        "engines": package["engines"],
                        "devDependencies": package["devDependencies"],
                    }
                }
            }
            (dashboard / "package.json").write_text(json.dumps(package), encoding="utf-8")
            (dashboard / "package-lock.json").write_text(json.dumps(lock), encoding="utf-8")
            (dashboard / ".npmrc").write_text(
                "engine-strict=true\nstrict-allow-scripts=true\n",
                encoding="utf-8",
            )
            findings: list[dict[str, str]] = []
            AUDIT.audit_dashboard_package(root, findings)
            self.assertIn("install_script_policy_stale", [item["kind"] for item in findings])


if __name__ == "__main__":
    unittest.main()
