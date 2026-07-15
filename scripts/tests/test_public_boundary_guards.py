from __future__ import annotations

import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def run(command: list[str], cwd: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, cwd=cwd, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)


def init_repo(root: Path) -> None:
    run(["git", "init", "-q"], root)
    run(["git", "config", "user.email", "boundary@example.test"], root)
    run(["git", "config", "user.name", "Boundary Test"], root)


def commit_all(root: Path, message: str) -> None:
    result = run(["git", "add", "-A"], root)
    if result.returncode:
        raise AssertionError(result.stderr)
    result = run(["git", "commit", "-qm", message], root)
    if result.returncode:
        raise AssertionError(result.stderr)


class PublicBoundaryGuardTests(unittest.TestCase):
    def test_public_sync_guard_help_is_not_treated_as_remote(self) -> None:
        result = subprocess.run(
            [str(ROOT / "scripts/public_sync_guard.sh"), "--help"],
            cwd=ROOT,
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Usage: scripts/public_sync_guard.sh", result.stdout)

    def test_unrelated_ref_leak_scan_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-leak-unrelated-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/public_leak_guard.sh", hooks / "public_leak_guard.sh")
            (repo / "base.txt").write_text("base\n", encoding="utf-8")
            commit_all(repo, "base")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "checkout", "--orphan", "public-orphan"], repo)
            run(["git", "rm", "-rf", "."], repo)
            hooks.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/public_leak_guard.sh", hooks / "public_leak_guard.sh")
            canary = "sk_" + "live_" + ("a" * 26)
            (repo / "leak.txt").write_text(canary + "\n", encoding="utf-8")
            commit_all(repo, "orphan")
            orphan = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            result = run(
                ["bash", "scripts/agent_hooks/public_leak_guard.sh", "--base", base, "--ref", orphan, "--public"],
                repo,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            payload = json.loads(result.stdout.strip().splitlines()[-1])
            self.assertGreater(payload["scanned"], 0)
            self.assertFalse(payload["ok"])

    def test_unrelated_worktree_leak_scan_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-leak-worktree-unrelated-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/public_leak_guard.sh", hooks / "public_leak_guard.sh")
            (repo / "base.txt").write_text("base\n", encoding="utf-8")
            commit_all(repo, "base")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "checkout", "--orphan", "worktree-orphan"], repo)
            run(["git", "rm", "-rf", "."], repo)
            hooks.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/public_leak_guard.sh", hooks / "public_leak_guard.sh")
            (repo / "clean.txt").write_text("clean\n", encoding="utf-8")
            commit_all(repo, "orphan")
            result = run(
                ["bash", "scripts/agent_hooks/public_leak_guard.sh", "--base", base, "--public"],
                repo,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("no merge base", result.stderr)

    def test_public_paid_sync_allows_paid_code_but_blocks_private_files(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-guard-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            (repo / "README.md").write_text("base\n", encoding="utf-8")
            commit_all(repo, "base")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/remotes/public-paid/main", base], repo)
            paid = repo / "contextlattice-dashboard/app/api/billing/summary/route.ts"
            paid.parent.mkdir(parents=True)
            paid.write_text("export const paid = true;\n", encoding="utf-8")
            invitation = repo / "contextlattice-dashboard/app/api/workspace/members/route.ts"
            invitation.parent.mkdir(parents=True)
            invitation.write_text("export const generateWorkspaceInvitationToken = true;\n", encoding="utf-8")
            shared = repo / "contextlattice-dashboard/app/settings/page.tsx"
            shared.parent.mkdir(parents=True)
            shared.write_text("const activeWorkspaceId = 'paid';\n", encoding="utf-8")
            commit_all(repo, "paid")
            result = run(["bash", "scripts/public_sync_guard.sh", "public-paid", "main"], repo)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            private = repo / "docs/private/internal.md"
            private.parent.mkdir(parents=True)
            private.write_text("internal\n", encoding="utf-8")
            commit_all(repo, "private")
            result = run(["bash", "scripts/public_sync_guard.sh", "public-paid", "main"], repo)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("docs/private", result.stderr)

    def test_public_paid_sync_allows_removing_blocked_paths(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-cleanup-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            private = repo / "docs/private/internal.md"
            private.parent.mkdir(parents=True)
            private.write_text("remove me\n", encoding="utf-8")
            backup = repo / ".backup/old/config.json"
            backup.parent.mkdir(parents=True)
            backup.write_text("remove me\n", encoding="utf-8")
            commit_all(repo, "blocked baseline")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/remotes/public-paid/main", base], repo)
            run(["git", "rm", "-r", "docs/private", ".backup"], repo)
            commit_all(repo, "clean paid lane")
            result = run(["bash", "scripts/public_sync_guard.sh", "public-paid", "main"], repo)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_public_paid_sync_uses_exact_remote_tracking_ref(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-exact-ref-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            (repo / "README.md").write_text("base\n", encoding="utf-8")
            commit_all(repo, "base")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/remotes/public-paid/main", base], repo)
            private = repo / "docs/private/internal.md"
            private.parent.mkdir(parents=True)
            private.write_text("must not ship\n", encoding="utf-8")
            commit_all(repo, "blocked candidate")
            candidate = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/heads/public-paid/main", candidate], repo)
            result = run(["bash", "scripts/public_sync_guard.sh", "public-paid", "main"], repo)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("docs/private", result.stderr)
            self.assertNotIn("ambiguous", result.stderr.lower())

    def test_public_paid_sync_rejects_private_program_paths(self) -> None:
        for blocked_path in (
            "scripts/agent/audit-frontier-30-program",
            "scripts/lib/private_dev_posture.sh",
        ):
            with self.subTest(path=blocked_path), tempfile.TemporaryDirectory(prefix="public-paid-program-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                (repo / "scripts").mkdir()
                shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
                (repo / "config").mkdir()
                shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
                (repo / "README.md").write_text("base\n", encoding="utf-8")
                commit_all(repo, "base")
                base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
                run(["git", "update-ref", "refs/remotes/public-paid/main", base], repo)
                blocked = repo / blocked_path
                blocked.parent.mkdir(parents=True, exist_ok=True)
                blocked.write_text("must not ship\n", encoding="utf-8")
                commit_all(repo, "blocked private program")
                result = run(["bash", "scripts/public_sync_guard.sh", "public-paid", "main"], repo)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(blocked_path, result.stderr)

    def test_public_paid_sync_rejects_dangling_private_posture_calls(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-launcher-sync-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            launcher = repo / "scripts/compose_v4_balanced.sh"
            launcher.write_text("echo clean\n", encoding="utf-8")
            (repo / "launch.sh").write_text("echo clean\n", encoding="utf-8")
            commit_all(repo, "clean paid baseline")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/remotes/public-paid/main", base], repo)
            launcher.write_text("apply_private_dev_posture\n", encoding="utf-8")
            commit_all(repo, "dangling private launcher call")
            result = run(["bash", "scripts/public_sync_guard.sh", "public-paid", "main"], repo)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("private_dev_posture", result.stderr)

    def test_public_lane_rejects_paid_markers_in_shared_ui(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-ui-guard-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            ui = repo / "contextlattice-dashboard/components/InstallArtifactsPage.tsx"
            ui.parent.mkdir(parents=True)
            ui.write_text('fetch("/api/billing/entitlement/issue");\n', encoding="utf-8")
            commit_all(repo, "paid marker")
            result = run(["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public", "--ref", "HEAD"], repo)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("api/billing/entitlement", result.stderr)

    def test_public_sync_rejects_workspace_collaboration_markers_in_shared_files(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-workspace-sync-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            shared = repo / "contextlattice-dashboard/app/settings/page.tsx"
            shared.parent.mkdir(parents=True)
            shared.write_text("export default function Settings() { return null; }\n", encoding="utf-8")
            commit_all(repo, "public baseline")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/remotes/public/main", base], repo)
            shared.write_text("const activeWorkspaceId = 'paid-only';\n", encoding="utf-8")
            commit_all(repo, "paid collaboration marker")
            result = run(["bash", "scripts/public_sync_guard.sh", "public", "main"], repo)
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("activeWorkspaceId", result.stderr)

    def test_public_sync_allows_ordinary_shared_dashboard_changes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-shared-sync-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            (repo / "scripts").mkdir()
            shutil.copy2(ROOT / "scripts/public_sync_guard.sh", repo / "scripts/public_sync_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            shared = repo / "contextlattice-dashboard/app/settings/page.tsx"
            shared.parent.mkdir(parents=True)
            shared.write_text("export default function Settings() { return null; }\n", encoding="utf-8")
            commit_all(repo, "public baseline")
            base = run(["git", "rev-parse", "HEAD"], repo).stdout.strip()
            run(["git", "update-ref", "refs/remotes/public/main", base], repo)
            shared.write_text("export default function Settings() { return <main />; }\n", encoding="utf-8")
            commit_all(repo, "ordinary shared update")
            result = run(["bash", "scripts/public_sync_guard.sh", "public", "main"], repo)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_public_lane_rejects_workspace_collaboration_markers_in_shared_files(self) -> None:
        cases = (
            ("contextlattice-dashboard/prisma/schema.prisma", "model WorkspaceInvitation {}\n", "WorkspaceInvitation"),
            ("contextlattice-dashboard/app/settings/page.tsx", "const activeWorkspaceId = 'paid';\n", "activeWorkspaceId"),
        )
        for path, content, marker in cases:
            with self.subTest(path=path), tempfile.TemporaryDirectory(prefix="public-workspace-marker-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                (repo / "config").mkdir()
                shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
                candidate = repo / path
                candidate.parent.mkdir(parents=True, exist_ok=True)
                candidate.write_text(content, encoding="utf-8")
                commit_all(repo, "paid workspace marker")
                result = run(
                    ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public", "--ref", "HEAD"],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(marker, result.stderr)

    def test_public_lane_rejects_frontier_t1_paid_sources_and_evals(self) -> None:
        blocked_paths = (
            "contextlattice-dashboard/app/api/workspace/invitations/accept/route.ts",
            "contextlattice-dashboard/app/api/workspace/members/route.ts",
            "contextlattice-dashboard/app/auth/invite/page.tsx",
            "contextlattice-dashboard/lib/workspaceInvitations.ts",
            "contextlattice-dashboard/tests/workspace-invitations.test.ts",
            "services/gateway-go/frontier_t1_governance_entitled.go",
            "services/gateway-go/frontier_t1_governance_entitled_test.go",
            "services/gateway-go/frontier_t1_eval_test.go",
            "docs/entitled-frontier-t1.md",
            "docs/evals/v3.18-frontier-t1-paid-activation.json",
            "config/frontier_t1_release_provenance.v1.json",
            "scripts/agent/audit-frontier-30-program",
            "scripts/agent/generate-frontier-t1-source-manifest",
            "scripts/frontier_t1_source_contract.py",
            "scripts/tests/Test-ReleaseInstallerIdentity.ps1",
            "scripts/tests/test_frontier_t1_source_archive.py",
            "scripts/tests/test_frontier_t1_source_manifest.py",
            "scripts/tests/test_release_payload_provenance.py",
            "scripts/validate_frontier_t1_release_provenance.py",
            "scripts/validate_frontier_t1_source_archive.py",
        )
        for blocked_path in blocked_paths:
            with self.subTest(path=blocked_path), tempfile.TemporaryDirectory(prefix="public-frontier-t1-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                (repo / "config").mkdir()
                shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
                candidate = repo / blocked_path
                candidate.parent.mkdir(parents=True, exist_ok=True)
                candidate.write_text("paid-only Frontier T1 fixture\n", encoding="utf-8")
                commit_all(repo, "frontier paid fixture")
                result = run(
                    ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public", "--ref", "HEAD"],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(blocked_path, result.stderr)

    def test_distribution_lanes_reject_dangling_private_posture_calls(self) -> None:
        for lane in ("public", "public-paid"):
            with self.subTest(lane=lane), tempfile.TemporaryDirectory(prefix=f"{lane}-launcher-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                (repo / "config").mkdir()
                shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
                launcher = repo / "scripts/compose_v4_balanced.sh"
                launcher.parent.mkdir(parents=True, exist_ok=True)
                launcher.write_text("apply_private_dev_posture\n", encoding="utf-8")
                commit_all(repo, "dangling private launcher call")
                result = run(
                    ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", lane, "--ref", "HEAD"],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("private_dev_posture", result.stderr)

    def test_distribution_lanes_reject_case_variant_private_roots(self) -> None:
        cases = (
            ("public", "Private/frontier-program.json"),
            ("public", "Docs/Private/internal.md"),
            ("public-paid", "PRIVATE/frontier-program.json"),
            ("public-paid", "docs/PRIVATE/internal.md"),
            ("public-paid", "Private_Docs/internal.md"),
        )
        for lane, blocked_path in cases:
            with self.subTest(lane=lane, blocked_path=blocked_path), tempfile.TemporaryDirectory(
                prefix=f"{lane}-case-private-"
            ) as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                candidate = repo / blocked_path
                candidate.parent.mkdir(parents=True, exist_ok=True)
                candidate.write_text("must not ship\n", encoding="utf-8")
                commit_all(repo, "case-variant private root")
                result = run(
                    [
                        "bash",
                        "scripts/agent_hooks/branch_lane_guard.sh",
                        "--lane",
                        lane,
                        "--ref",
                        "HEAD",
                    ],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(blocked_path, result.stderr)

    def test_public_paid_lane_rejects_private_and_scratch_tree_paths(self) -> None:
        blocked_paths = [
            "docs/private/internal.md",
            "config/env/premium_dev.env",
            "scripts/launch_private_dev.sh",
            "scripts/lib/private_dev_posture.sh",
            "scripts/setup_paid_local_env.sh",
            "scripts/tests/test_private_dev_posture.py",
            ".backup/old/config.json",
            "dev/backups/old.yml",
            "development/docker-compose-bak/old.yml",
            "logs/runtime.log",
            "infra/compose/.env",
            "config/mcp/runtime.json.bak.1",
            ".router.pid",
        ]
        for blocked_path in blocked_paths:
            with self.subTest(path=blocked_path), tempfile.TemporaryDirectory(prefix="public-paid-tree-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                (repo / "config").mkdir(exist_ok=True)
                shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
                candidate = repo / blocked_path
                candidate.parent.mkdir(parents=True, exist_ok=True)
                candidate.write_text("must not ship\n", encoding="utf-8")
                commit_all(repo, "blocked paid path")
                result = run(
                    ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public-paid", "--ref", "HEAD"],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(blocked_path, result.stderr)

    def test_distribution_docs_reject_private_development_language(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-docs-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            (repo / "README.md").write_text("Private development remains the keyless superset.\n", encoding="utf-8")
            commit_all(repo, "internal dev language")
            result = run(
                ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public-paid", "--ref", "HEAD"],
                repo,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("Private development", result.stderr)

    def test_public_paid_lane_allows_private_development_workspace_identifier(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-workspace-id-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            installer = repo / "packaging/linux/ContextLattice-Install.sh"
            installer.parent.mkdir(parents=True)
            installer.write_text(
                "CONTEXTLATTICE_FRONTIER_T1_DEV_WORKSPACE=private-development\n",
                encoding="utf-8",
            )
            commit_all(repo, "paid installer workspace identifier")
            result = run(
                ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public-paid", "--ref", "HEAD"],
                repo,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_public_paid_lane_rejects_private_dev_distribution_lane(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-paid-private-lane-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
            (repo / "config").mkdir()
            shutil.copy2(ROOT / "config/public_sync_blocklist.txt", repo / "config/public_sync_blocklist.txt")
            installer = repo / "packaging/linux/ContextLattice-Install.sh"
            installer.parent.mkdir(parents=True)
            installer.write_text("CONTEXTLATTICE_DISTRIBUTION_LANE=private-dev\n", encoding="utf-8")
            commit_all(repo, "private dev lane leak")
            result = run(
                ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", "public-paid", "--ref", "HEAD"],
                repo,
            )
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("private-dev", result.stderr)


if __name__ == "__main__":
    unittest.main()
