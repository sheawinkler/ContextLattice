from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GATEWAY = ROOT / "services/gateway-go"
PAID_T10_PATHS = (
    "services/gateway-go/frontier_t10_aggregate_signal_entitled.go",
    "services/gateway-go/frontier_t10_aggregate_signal_entitled_test.go",
)
PRIVATE_T10_PATHS = (
    "services/gateway-go/frontier_t10_secure_aggregation_research_private.go",
    "services/gateway-go/frontier_t10_secure_aggregation_research_private_test.go",
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


def init_repo(root: Path) -> None:
    subprocess.run(["git", "init", "-q"], cwd=root, check=True)
    subprocess.run(
        ["git", "config", "user.email", "t10-projection@example.test"],
        cwd=root,
        check=True,
    )
    subprocess.run(
        ["git", "config", "user.name", "T10 Projection Test"],
        cwd=root,
        check=True,
    )


def commit_all(root: Path, message: str) -> str:
    subprocess.run(["git", "add", "-A"], cwd=root, check=True)
    subprocess.run(["git", "commit", "-qm", message], cwd=root, check=True)
    return subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=root, text=True
    ).strip()


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


class T10PublicProjectionTests(unittest.TestCase):
    def test_public_projection_compiles_without_paid_or_private_kernels(self) -> None:
        go = os.environ.get("GO") or shutil.which("go")
        if not go:
            self.skipTest("Go is unavailable")
        with tempfile.TemporaryDirectory(prefix="t10-public-projection-") as tmp:
            projected = Path(tmp) / "gateway-go"
            shutil.copytree(GATEWAY, projected)
            for relative in PAID_T10_PATHS + PRIVATE_T10_PATHS:
                target = projected / Path(relative).relative_to("services/gateway-go")
                target.unlink(missing_ok=True)
            result = run([go, "test", "-run", "^$", "."], projected)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_public_sync_guard_rejects_t10_paid_or_private_runtime_leakage(self) -> None:
        cases = (
            "var _ = frontierT10GovernanceRuntime\n",
            'const _ = "CONTEXTLATTICE_FRONTIER_T10_AGGREGATE_GOVERNANCE_ENABLED"\n',
            'const _ = "/memory/aggregate-signal/governance"\n',
            'const _ = "frontier_t10_secure_aggregation_research.v1"\n',
        )
        for source in cases:
            with self.subTest(source=source), tempfile.TemporaryDirectory(
                prefix="t10-guard-"
            ) as tmp:
                repo = Path(tmp)
                init_repo(repo)
                (repo / "scripts").mkdir()
                shutil.copy2(
                    ROOT / "scripts/public_sync_guard.sh",
                    repo / "scripts/public_sync_guard.sh",
                )
                (repo / "config").mkdir()
                shutil.copy2(
                    ROOT / "config/public_sync_blocklist.txt",
                    repo / "config/public_sync_blocklist.txt",
                )
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
                commit_all(repo, "leak T10 runtime")
                result = run(
                    ["bash", "scripts/public_sync_guard.sh", "public", "main"],
                    repo,
                    env=install_fixture_rg(repo),
                )
                self.assertNotEqual(
                    result.returncode, 0, result.stdout + result.stderr
                )

    def test_public_paid_retains_entitled_kernel_but_rejects_private_research(self) -> None:
        blocklist = {
            line.split("#", 1)[0].strip()
            for line in (ROOT / "config/public_sync_blocklist.txt")
            .read_text(encoding="utf-8")
            .splitlines()
            if line.split("#", 1)[0].strip()
        }
        self.assertIn("scripts/tests/test_runtime_license_installers.py", blocklist)
        self.assertTrue(set(PAID_T10_PATHS + PRIVATE_T10_PATHS).issubset(blocklist))
        with tempfile.TemporaryDirectory(prefix="t10-public-paid-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(
                ROOT / "scripts/public_sync_guard.sh",
                repo / "scripts/public_sync_guard.sh",
            )
            (repo / "config").mkdir()
            shutil.copy2(
                ROOT / "config/public_sync_blocklist.txt",
                repo / "config/public_sync_blocklist.txt",
            )
            (repo / "README.md").write_text("paid baseline\n", encoding="utf-8")
            base = commit_all(repo, "public-paid baseline")
            subprocess.run(
                ["git", "update-ref", "refs/remotes/public-paid/main", base],
                cwd=repo,
                check=True,
            )
            paid = repo / PAID_T10_PATHS[0]
            paid.parent.mkdir(parents=True)
            paid.write_text(
                'package main\nconst marker = "frontier_t10_aggregate_governance.v1"\n',
                encoding="utf-8",
            )
            commit_all(repo, "retain entitled T10 kernel")
            env = install_fixture_rg(repo)
            result = run(
                ["bash", "scripts/public_sync_guard.sh", "public-paid", "main"],
                repo,
                env=env,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

            research = repo / PRIVATE_T10_PATHS[0]
            research.write_text(
                'package main\nconst marker = "frontier_t10_secure_aggregation_research.v1"\n',
                encoding="utf-8",
            )
            commit_all(repo, "leak private T10 research")
            result = run(
                ["bash", "scripts/public_sync_guard.sh", "public-paid", "main"],
                repo,
                env=env,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
