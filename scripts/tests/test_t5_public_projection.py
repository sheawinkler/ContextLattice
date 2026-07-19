from __future__ import annotations

import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GATEWAY = ROOT / "services/gateway-go"
PAID_T5_PATHS = (
    "services/gateway-go/frontier_t5_policy_lab_entitled.go",
    "services/gateway-go/frontier_t5_policy_lab_entitled_test.go",
    "services/gateway-go/cmd/contextlattice-agent-tools/frontier_t5_policy_lab_entitled.go",
    "services/gateway-go/cmd/contextlattice-agent-tools/frontier_t5_policy_lab_entitled_test.go",
)


def run(
    command: list[str],
    cwd: Path,
    *,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
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


def init_repo(root: Path) -> None:
    subprocess.run(["git", "init", "-q"], cwd=root, check=True)
    subprocess.run(["git", "config", "user.email", "t5-projection@example.test"], cwd=root, check=True)
    subprocess.run(["git", "config", "user.name", "T5 Projection Test"], cwd=root, check=True)


def blocklist_patterns() -> list[str]:
    patterns = []
    for raw in (ROOT / "config/public_sync_blocklist.txt").read_text(encoding="utf-8").splitlines():
        value = raw.split("#", 1)[0].strip()
        if value:
            patterns.append(value)
    return patterns


def install_fixture_rg(root: Path) -> dict[str, str]:
    bin_dir = root / "bin"
    bin_dir.mkdir()
    rg = bin_dir / "rg"
    rg.write_text(
        """#!/bin/sh
pattern=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -e) pattern=$2; shift 2 ;;
    --) shift; break ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ -z "$pattern" ]; then
  pattern=$1
  shift
fi
exec /usr/bin/grep -EnH -- "$pattern" "$@"
""",
        encoding="utf-8",
    )
    rg.chmod(0o755)
    env = os.environ.copy()
    env["PATH"] = f"{bin_dir}:/opt/homebrew/bin:/usr/bin:/bin"
    return env


class T5PublicProjectionTests(unittest.TestCase):
    def run_go_test(self, source: Path, package: str, selector: str) -> None:
        go = os.environ.get("GO") or shutil.which("go")
        if go:
            result = run([go, "test", "-run", selector, package], source)
        else:
            docker = os.environ.get("DOCKER") or shutil.which("docker")
            image = os.environ.get("CONTEXTLATTICE_T5_PROJECTION_GO_IMAGE")
            if not docker or not image:
                self.skipTest("Go is unavailable and no projection test image was configured")
            result = run(
                [
                    docker,
                    "--context",
                    os.environ.get("CONTEXTLATTICE_DOCKER_CONTEXT", "orbstack"),
                    "run",
                    "--rm",
                    "--network=none",
                    "-v",
                    f"{source.resolve()}:/src:ro",
                    "-w",
                    "/src",
                    image,
                    "go",
                    "test",
                    "-run",
                    selector,
                    package,
                ],
                source,
            )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def project_t5_without_paid_kernel(self, destination: Path) -> Path:
        projected = destination / "services/gateway-go"
        shutil.copytree(GATEWAY, projected)

        # Another blocklisted paid extension consumes the active-policy helper.
        # Keep only that compile-time seam while removing the T5 paid kernel.
        (projected / "frontier_t5_policy_lab_entitled.go").write_text(
            """package main

func frontierT5ActiveScopedPolicy(string, string, string) map[string]any { return nil }
""",
            encoding="utf-8",
        )
        (projected / "frontier_t5_policy_lab_entitled_test.go").write_text(
            "package main\n",
            encoding="utf-8",
        )
        for relative in PAID_T5_PATHS[2:]:
            target = projected / Path(relative).relative_to("services/gateway-go")
            target.write_text("package main\n", encoding="utf-8")
        return projected

    def test_public_projection_compiles_and_entitled_hooks_remain(self) -> None:
        self.run_go_test(GATEWAY, ".", "^TestFrontierT5EntitledRouteProjection$")
        self.run_go_test(
            GATEWAY,
            "./cmd/contextlattice-agent-tools",
            "^TestFrontierT5PolicyLabEntitledCLIProjection$",
        )

        with tempfile.TemporaryDirectory(prefix="t5-public-projection-") as tmp:
            projected = self.project_t5_without_paid_kernel(Path(tmp))
            self.assertTrue((projected / "frontier_t5_policy_lab.go").is_file())
            self.assertTrue((projected / "frontier_t5_policy_lab_store.go").is_file())

            paid_markers = re.compile(
                r"frontierT5PolicyGovernance|frontierT5(?:PolicySimulationHistory|"
                r"ScopedPolicyActivation|ContextPolicyCanary|LifecycleAutomation|"
                r"ContradictionReview|TemperatureAutomation)|CONTEXTLATTICE_FRONTIER_T5_|"
                r"frontier_t5_policy_laboratory_governance|"
                r"/memory/policy/simulation/history|/memory/policy/scoped/activation|"
                r"/memory/context-policy/canary|/memory/lifecycle/automation|"
                r"/memory/contradictions/review-queue|/memory/storage/temperature/automation|"
                r"simulation-history|scoped-activation|lifecycle-automation|"
                r"contradiction-review|temperature-automation"
            )
            for relative in (
                "main.go",
                "optional_paid_extensions.go",
                "context_boundary_audit.go",
                "strict_runtime_ownership.go",
                "cmd/contextlattice-agent-tools/main.go",
            ):
                content = (projected / relative).read_text(encoding="utf-8")
                self.assertIsNone(paid_markers.search(content), relative)

            self.run_go_test(projected, ".", "^$")
            self.run_go_test(projected, "./cmd/contextlattice-agent-tools", "^$")

    def test_public_sync_guard_rejects_t5_runtime_leakage(self) -> None:
        cases = (
            "var _ = frontierT5PolicySimulationHistoryPath\n",
            "const _ = \"CONTEXTLATTICE_FRONTIER_T5_POLICY_GOVERNANCE_ENABLED\"\n",
            "const _ = \"/memory/policy/scoped/activation\"\n",
            "const _ = \"simulation-history\"\n",
        )
        for source in cases:
            with self.subTest(source=source), tempfile.TemporaryDirectory(prefix="t5-guard-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                (repo / "scripts").mkdir()
                shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
                (repo / "config").mkdir()
                shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
                shared = repo / "services/gateway-go/main.go"
                shared.parent.mkdir(parents=True)
                shared.write_text("package main\n", encoding="utf-8")
                base = commit_all(repo, "public baseline")
                subprocess.run(
                    ["git", "update-ref", "refs/remotes/public/main", base],
                    cwd=repo,
                    check=True,
                )
                shared.write_text("package main\n" + source, encoding="utf-8")
                commit_all(repo, "leak T5 paid runtime")
                result = run(
                    ["bash", "scripts/public_sync_guard.sh", "public", "main"],
                    repo,
                    env=install_fixture_rg(repo),
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(source.strip().split()[-1].strip('"'), result.stderr)

    def test_public_paid_keeps_t5_entitled_projection_files(self) -> None:
        for relative in PAID_T5_PATHS:
            self.assertIn(relative, blocklist_patterns())

        with tempfile.TemporaryDirectory(prefix="t5-public-paid-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            (repo / "README.md").write_text("paid baseline\n", encoding="utf-8")
            base = commit_all(repo, "public-paid baseline")
            subprocess.run(
                ["git", "update-ref", "refs/remotes/public-paid/main", base],
                cwd=repo,
                check=True,
            )
            paid = repo / PAID_T5_PATHS[2]
            paid.parent.mkdir(parents=True)
            paid.write_text("package main\nconst route = \"simulation-history\"\n", encoding="utf-8")
            commit_all(repo, "retain entitled T5 CLI")
            result = run(
                ["bash", "scripts/public_sync_guard.sh", "public-paid", "main"],
                repo,
                env=install_fixture_rg(repo),
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
