from __future__ import annotations

import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("go_runtime_audit", ROOT / "scripts/go_runtime_audit.py")
AUDIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(AUDIT)


def fixture(root):
    for name, bases in AUDIT.EXPECTED_BASES.items():
        (root / name).write_text("".join(f"FROM {base}\n" for base in bases))
    for name in AUDIT.MODULES:
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("module fixture\n\ngo 1.26\n\ntoolchain go1.26.11\n")


class GoRuntimeAuditTests(unittest.TestCase):
    def test_repository_pins(self):
        result = AUDIT.audit(ROOT)
        self.assertTrue(result["ok"], result["findings"])

    def test_current_platform_qualified_and_registry_bases(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            fixture(root)
            (root / "Dockerfile.hf-lite").write_text(
                "from --platform=$BUILDPLATFORM docker.io/library/golang:1.26.11-bookworm AS build\nFROM debian:bookworm-slim\n"
            )
            self.assertTrue(AUDIT.audit(root)["ok"])

    def test_old_floating_missing_and_hidden_runtime_stages_fail(self):
        for stage in ("alpine:3.20", "alpine:3.24", "alpine", "golang:1.26.4-alpine", "scratch"):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory() as raw:
                root = Path(raw)
                fixture(root)
                (root / "Dockerfile.gateway-go").write_text(
                    "FROM golang:1.26.11-alpine3.24 AS build\nFROM " + stage + "\n"
                )
                self.assertFalse(AUDIT.audit(root)["ok"])

    def test_unknown_dockerfile_cannot_add_unreviewed_go_image(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            fixture(root)
            (root / "Dockerfile.new").write_text("FROM golang:1.26.4-bookworm\n")
            self.assertFalse(AUDIT.audit(root)["ok"])

    def test_module_toolchain_drift_is_rejected(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            fixture(root)
            path = root / AUDIT.MODULES[0]
            path.write_text(path.read_text().replace("go1.26.11", "go1.26.4"))
            self.assertFalse(AUDIT.audit(root)["ok"])

    def test_required_files_cannot_disappear(self):
        with tempfile.TemporaryDirectory() as raw:
            self.assertFalse(AUDIT.audit(Path(raw))["ok"])


if __name__ == "__main__":
    unittest.main()
