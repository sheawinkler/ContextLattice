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
        self.assertEqual(payload["local_runtime"], {"node": "24.22.2", "npm": "11.19.1"})

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
            (root / "Dockerfile.memorymcp").write_text("FROM node:24.22.2-alpine\n", encoding="utf-8")
            (root / "Dockerfile.mcp-gateway").write_text("FROM node:24.22.2-bookworm-slim\n", encoding="utf-8")
            findings: list[dict[str, str]] = []
            AUDIT.audit_dockerfiles(root, findings)
            kinds = [item["kind"] for item in findings]
            self.assertIn("docker_node_mismatch", kinds)
            self.assertIn("non_node24_base", kinds)

    def test_every_node_container_stage_requires_the_reviewed_pin(self) -> None:
        stages = {
            "current": ("FROM node:24.22.2-bookworm-slim AS runtime", False),
            "stale_runtime": ("FROM node:24.18.0-bookworm-slim AS runtime", True),
            "platform_override": ("FROM --platform=$TARGETPLATFORM node:24.18.0-bookworm-slim AS runtime", True),
            "lowercase": ("from node:24.18.0-bookworm-slim as runtime", True),
            "default_registry": ("FROM docker.io/library/node:24.18.0-bookworm-slim AS runtime", True),
            "untagged": ("FROM node AS runtime", True),
            "digest_only": ("FROM node@sha256:" + "a" * 64 + " AS runtime", True),
            "missing": ("FROM scratch", True),
        }
        for name, (stage, rejected) in stages.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as raw:
                root = Path(raw)
                for relative, expected in AUDIT.EXPECTED_DOCKER_BASES.items():
                    (root / relative).write_text(f"FROM {expected}\n", encoding="utf-8")
                builder = "" if name == "missing" else "FROM node:24.22.2-bookworm-slim AS build\n"
                (root / "Dockerfile.dashboard").write_text(builder + stage + "\n", encoding="utf-8")
                findings: list[dict[str, str]] = []
                AUDIT.audit_dockerfiles(root, findings)
                self.assertEqual(bool(findings), rejected, findings)

    def test_new_node_dockerfiles_cannot_hide_stale_or_floating_pins(self) -> None:
        tags = ("24.18.0-alpine", "24-alpine", "24.22-alpine", "latest", "24.22.20-alpine")
        for tag in tags:
            with self.subTest(tag=tag), tempfile.TemporaryDirectory() as raw:
                root = Path(raw)
                for relative, expected in AUDIT.EXPECTED_DOCKER_BASES.items():
                    (root / relative).write_text(f"FROM {expected}\n", encoding="utf-8")
                (root / "Dockerfile.new-service").write_text(f"FROM node:{tag}\n", encoding="utf-8")
                findings: list[dict[str, str]] = []
                AUDIT.audit_dockerfiles(root, findings)
                self.assertTrue(findings, f"unreviewed Node pin accepted: {tag}")

    def test_dashboard_dev_server_contract_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            (root / "Dockerfile.dashboard").write_text(
                "FROM node:24.22.2-bookworm-slim\n"
                "ENV NODE_ENV=development\n"
                'CMD ["sh", "-c", "npx --yes prisma db push && npm run dev"]\n',
                encoding="utf-8",
            )
            findings: list[dict[str, str]] = []
            AUDIT.audit_dashboard_production_contract(root, findings)
            kinds = [item["kind"] for item in findings]
            self.assertIn("dashboard_dev_server", kinds)
            self.assertIn("dashboard_npx_network_install", kinds)
            self.assertIn("dashboard_production_env_missing", kinds)
            self.assertIn("dashboard_build_missing", kinds)
            self.assertIn("dashboard_start_missing", kinds)
            self.assertIn("dashboard_healthcheck_missing", kinds)

    def test_stale_install_script_approval_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            dashboard = root / "contextlattice-dashboard"
            dashboard.mkdir(parents=True)
            package = {
                "packageManager": "npm@11.19.1",
                "engines": {"node": ">=24.22.2 <25", "npm": ">=11.19.1 <12"},
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
