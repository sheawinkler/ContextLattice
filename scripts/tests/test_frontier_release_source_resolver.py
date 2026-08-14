from __future__ import annotations

import json
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RESOLVER = ROOT / "scripts/resolve_frontier_release_source.py"
WORKFLOW = ROOT / ".github/workflows/release-installers.yml"


def git(root: Path, *args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=root, text=True).strip()


class ResolverFixture:
    def __init__(
        self,
        root: Path,
        *,
        runtime_delta: bool = False,
        agent_install_smoke_delta: bool = False,
        private_receipt_test_addition: bool = False,
    ) -> None:
        self.root = root
        subprocess.run(["git", "init", "-q"], cwd=root, check=True)
        subprocess.run(["git", "config", "user.name", "ContextLattice Test"], cwd=root, check=True)
        subprocess.run(["git", "config", "user.email", "test@example.invalid"], cwd=root, check=True)
        files = {
            "config/frontier_t1_release_provenance.v1.json": "old\n",
            "docs/releases/v4.0.2.md": "old\n",
            "scripts/agent/audit-public-product-truth": "old\n",
            "scripts/tests/test_public_product_truth.py": "old\n",
        }
        if runtime_delta:
            files["services/gateway-go/main.go"] = "old\n"
        if agent_install_smoke_delta:
            files["scripts/agent/audit-agent-global-install-smoke"] = "old\n"
        for relative, content in files.items():
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")
        subprocess.run(["git", "add", "."], cwd=root, check=True)
        subprocess.run(["git", "commit", "-qm", "original proof"], cwd=root, check=True)
        self.original = git(root, "rev-parse", "HEAD")

        for relative in files:
            (root / relative).write_text("approved\n", encoding="utf-8")
        if private_receipt_test_addition:
            relative = "services/gateway-go/memory_recall_response_fallback_receipt_private_test.go"
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("approved\n", encoding="utf-8")
        subprocess.run(["git", "add", "."], cwd=root, check=True)
        subprocess.run(["git", "commit", "-qm", "approved source"], cwd=root, check=True)
        self.approved = git(root, "rev-parse", "HEAD")
        self.approved_tree = git(root, "rev-parse", "HEAD^{tree}")
        self.authorization = self.approved
        self.changed_paths = []
        for line in git(root, "diff", "--name-status", f"{self.original}..{self.approved}").splitlines():
            status, relative = line.split("\t")
            self.changed_paths.append(
                {
                    "path": relative,
                    "status": status,
                    "blob_oid": git(root, "rev-parse", f"HEAD:{relative}"),
                }
            )

    def registry(self, *, required: bool = True, include: bool = True) -> dict[str, object]:
        amendment = {
            "release_tag": "v4.0.2-public-paid",
            "source_commit": self.original,
            "original_private_proof_commit": self.original,
            "approved_private_source_commit": self.approved,
            "approved_private_source_tree": self.approved_tree,
            "reason": "Fixture-only non-runtime release repair.",
            "changed_paths": self.changed_paths,
        }
        return {
            "schema_id": "contextlattice_frontier_release_source_amendments.v1",
            "version": 1,
            "required_release_tags": ["v4.0.2-public-paid"] if required else [],
            "amendments": [amendment] if include else [],
        }

    def run(self, registry: dict[str, object]) -> tuple[subprocess.CompletedProcess[str], dict[str, object]]:
        registry_path = self.root / "amendments.json"
        output_path = self.root / "resolution.json"
        registry_path.write_text(json.dumps(registry), encoding="utf-8")
        result = subprocess.run(
            [
                "python3",
                str(RESOLVER),
                "--root",
                str(self.root),
                "--amendments",
                str(registry_path),
                "--release-tag",
                "v4.0.2-public-paid",
                "--source-commit",
                self.original,
                "--original-private-proof",
                self.original,
                "--private-main-ref",
                self.approved,
                "--authorization-ref",
                self.authorization,
                "--output",
                str(output_path),
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if output_path.exists():
            payload = json.loads(output_path.read_text(encoding="utf-8"))
        else:
            payload = json.loads(result.stdout)
        return result, payload


class FrontierReleaseSourceResolverTests(unittest.TestCase):
    def test_exact_non_runtime_amendment_passes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp))
            result, payload = fixture.run(fixture.registry())
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(payload["mode"], "amended")
            self.assertEqual(payload["approved_private_source_commit"], fixture.approved)
            self.assertEqual(payload["changed_paths"], fixture.changed_paths)

    def test_required_amendment_cannot_be_omitted(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp))
            result, payload = fixture.run(fixture.registry(include=False))
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("requires an explicit source amendment", payload["error"])

    def test_blob_identity_tampering_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp))
            registry = fixture.registry()
            registry["amendments"][0]["changed_paths"][0]["blob_oid"] = "0" * 40
            result, payload = fixture.run(registry)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("does not match", payload["error"])

    def test_unlisted_delta_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp))
            registry = fixture.registry()
            registry["amendments"][0]["changed_paths"].pop()
            result, payload = fixture.run(registry)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("does not match", payload["error"])

    def test_runtime_delta_is_rejected_even_when_registered(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp), runtime_delta=True)
            result, payload = fixture.run(fixture.registry())
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("runtime-unsafe", payload["error"])

    def test_agent_install_smoke_amendment_passes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp), agent_install_smoke_delta=True)
            result, payload = fixture.run(fixture.registry())
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(payload["mode"], "amended")
            self.assertIn(
                "scripts/agent/audit-agent-global-install-smoke",
                [row["path"] for row in payload["changed_paths"]],
            )

    def test_exact_private_test_addition_passes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-source-resolution-") as tmp:
            fixture = ResolverFixture(Path(tmp), private_receipt_test_addition=True)
            result, payload = fixture.run(fixture.registry())
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn(
                {
                    "path": "services/gateway-go/memory_recall_response_fallback_receipt_private_test.go",
                    "status": "A",
                    "blob_oid": git(
                        fixture.root,
                        "rev-parse",
                        "HEAD:services/gateway-go/memory_recall_response_fallback_receipt_private_test.go",
                    ),
                },
                payload["changed_paths"],
            )

    @unittest.skipUnless(WORKFLOW.is_file(), "private release workflow is not part of the public lane")
    def test_workflow_uses_and_publishes_resolution(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn("APPROVED_PRIVATE_SOURCE_COMMIT", workflow)
        self.assertIn("ContextLattice-release-provenance-resolution.json", workflow)
        self.assertIn("resolve_frontier_release_source.py", workflow)


if __name__ == "__main__":
    unittest.main()
