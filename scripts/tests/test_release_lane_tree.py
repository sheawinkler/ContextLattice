from __future__ import annotations

import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / "scripts/agent/audit-release-lane-tree"


def run(command: list[str], cwd: Path, *, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        command,
        cwd=cwd,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def commit_all(root: Path, message: str) -> str:
    subprocess.run(["git", "add", "-A"], cwd=root, check=True)
    subprocess.run(["git", "commit", "-qm", message], cwd=root, check=True)
    return subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()


class ReleaseLaneTreeTests(unittest.TestCase):
    def test_public_gate_compiles_exact_commit_and_emits_tree_bound_proof(self) -> None:
        with tempfile.TemporaryDirectory(prefix="release-lane-tree-") as tmp:
            repo = Path(tmp) / "repo"
            repo.mkdir()
            subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
            subprocess.run(["git", "config", "user.email", "release-gate@example.test"], cwd=repo, check=True)
            subprocess.run(["git", "config", "user.name", "Release Gate"], cwd=repo, check=True)

            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
            agent_scripts = repo / "scripts/agent"
            agent_scripts.mkdir(parents=True)
            fixture_audit = agent_scripts / "audit-release-lane-tree"
            shutil.copy2(AUDIT, fixture_audit)
            fixture_audit.chmod(0o755)
            shutil.copy2(ROOT / "scripts/public_leak_guard.py", repo / "scripts/public_leak_guard.py")
            generator = repo / "scripts/generate_commercial_truth.py"
            generator.write_text("#!/usr/bin/env python3\n", encoding="utf-8")
            config = repo / "config"
            config.mkdir()
            (config / "public_sync_blocklist.txt").write_text("# fixture\n", encoding="utf-8")
            (config / "commercial_truth.v1.json").write_text("{}\n", encoding="utf-8")
            gateway = repo / "services/gateway-go"
            gateway.mkdir(parents=True)
            (gateway / "go.mod").write_text("module example.test/releasegate\n\ngo 1.23\n", encoding="utf-8")
            (gateway / "continuity_identity.go").write_text("package releasegate\n", encoding="utf-8")
            (gateway / "objective_continuity.go").write_text("package releasegate\n", encoding="utf-8")
            orchestrator = repo / "services/orchestrator-go"
            orchestrator.mkdir(parents=True)
            (orchestrator / "go.mod").write_text("module example.test/orchestrator\n\ngo 1.23\n", encoding="utf-8")
            (orchestrator / "main.go").write_text("package orchestrator\n", encoding="utf-8")
            shared = gateway / "shared.go"
            shared.write_text("package releasegate\n\nfunc Shared() { paidOnly() }\n", encoding="utf-8")
            broken = commit_all(repo, "broken public projection")

            failed = run(
                [str(fixture_audit), "--lane", "public", "--ref", broken, "--expected-commit", broken],
                repo,
            )
            self.assertNotEqual(failed.returncode, 0, failed.stdout + failed.stderr)
            self.assertIn("paidOnly", failed.stdout + failed.stderr)

            shared.write_text("package releasegate\n\nfunc Shared() {}\n", encoding="utf-8")
            candidate = commit_all(repo, "compilable public projection")
            proof = Path(tmp) / "proof.json"
            passed = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "public",
                    "--ref",
                    candidate,
                    "--expected-commit",
                    candidate,
                    "--proof-out",
                    str(proof),
                ],
                repo,
            )
            self.assertEqual(passed.returncode, 0, passed.stdout + passed.stderr)
            payload = json.loads(proof.read_text(encoding="utf-8"))
            self.assertEqual(payload["schema_id"], "contextlattice_release_tree_proof.v1")
            self.assertEqual(payload["lane"], "public")
            self.assertEqual(payload["source_commit"], candidate)
            self.assertEqual(
                payload["source_tree"],
                subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], cwd=repo, text=True).strip(),
            )
            self.assertEqual(payload["gates"]["services_gateway_go_test"], "pass")
            self.assertEqual(payload["gates"]["public_leak_guard"], "pass")
            self.assertTrue(payload["required_blobs"])

            launch = repo / "launch_service/config"
            launch.mkdir(parents=True)
            (launch / "contextlattice.launch.json").write_text(
                '{"listing_url":"https://github.com/sheawinkler/'
                + 'http-context-and-memory-orchestrator/releases/new"}\n',
                encoding="utf-8",
            )
            leaked = commit_all(repo, "leaked private repository reference")
            leak_failed = run(
                [str(fixture_audit), "--lane", "public", "--ref", leaked, "--expected-commit", leaked],
                repo,
            )
            self.assertNotEqual(leak_failed.returncode, 0, leak_failed.stdout + leak_failed.stderr)
            self.assertIn("Public boundary guard failed", leak_failed.stdout + leak_failed.stderr)

            mismatch = run(
                [str(fixture_audit), "--lane", "public", "--ref", candidate, "--expected-commit", "0" * 40],
                repo,
            )
            self.assertNotEqual(mismatch.returncode, 0)
            self.assertIn("candidate commit mismatch", mismatch.stderr)


if __name__ == "__main__":
    unittest.main()
