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

    def test_distribution_docs_reject_private_doc_references(self) -> None:
        for lane in ("public", "public-paid"):
            with self.subTest(lane=lane), tempfile.TemporaryDirectory(prefix=f"{lane}-private-ref-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                (repo / "README.md").write_text("See docs/private/operator.md\n", encoding="utf-8")
                commit_all(repo, "private doc reference")
                result = run(
                    ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", lane, "--ref", "HEAD"],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("docs/private", result.stderr)

    def test_distribution_text_scan_normalizes_private_and_operator_paths(self) -> None:
        cases = (
            ("README.md", "/Users/contributor/Documents/Projects/operator-only/README.md\n", "/Users/contributor"),
            ("docs/public_overview/integration.md", "See ../private/operator.md\n", "../private"),
            ("CONTRIBUTING.md", "See ./private/operator.md\n", "./private"),
            ("docs/installation.md", "See docs/private/operator.md\n", "docs/private"),
            ("docs/windows.md", "See C:\\Users\\contributor\\Documents\\Projects\\operator-only\n", "C:\\Users"),
            ("SECURITY.md", "See /home/contributor/operator-only\n", "/home/contributor"),
        )
        for lane in ("public", "public-paid"):
            for relative, content, marker in cases:
                with self.subTest(lane=lane, relative=relative), tempfile.TemporaryDirectory(
                    prefix=f"{lane}-normalized-private-ref-"
                ) as tmp:
                    repo = Path(tmp)
                    init_repo(repo)
                    hooks = repo / "scripts/agent_hooks"
                    hooks.mkdir(parents=True)
                    shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                    shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                    candidate = repo / relative
                    candidate.parent.mkdir(parents=True, exist_ok=True)
                    candidate.write_text(content, encoding="utf-8")
                    commit_all(repo, "normalized private reference")
                    result = run(
                        ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", lane, "--ref", "HEAD"],
                        repo,
                    )
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn(marker, result.stderr)

    def test_distribution_lanes_reject_operator_checkout_defaults(self) -> None:
        for lane in ("public", "public-paid"):
            with self.subTest(lane=lane), tempfile.TemporaryDirectory(prefix=f"{lane}-operator-path-") as tmp:
                repo = Path(tmp)
                init_repo(repo)
                hooks = repo / "scripts/agent_hooks"
                hooks.mkdir(parents=True)
                shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
                shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
                (repo / "justfile").write_text(
                    "sidecar-up:\n    cd ~/Documents/Projects/operator-only && true\n",
                    encoding="utf-8",
                )
                commit_all(repo, "operator checkout default")
                result = run(
                    ["bash", "scripts/agent_hooks/branch_lane_guard.sh", "--lane", lane, "--ref", "HEAD"],
                    repo,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("Documents/Projects", result.stderr)

    def test_distribution_scan_failures_fail_closed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-scan-failure-") as tmp:
            repo = Path(tmp)
            init_repo(repo)
            hooks = repo / "scripts/agent_hooks"
            hooks.mkdir(parents=True)
            shutil.copy2(ROOT / "scripts/agent_hooks/common.sh", hooks / "common.sh")
            shutil.copy2(ROOT / "scripts/agent_hooks/branch_lane_guard.sh", hooks / "branch_lane_guard.sh")
            forbidden_marker = "crypto_trader_post_training_needs_godmode_and_finalization"
            (repo / "README.md").write_text(f"Forbidden path: {forbidden_marker}\n", encoding="utf-8")
            commit_all(repo, "forbidden operator path")

            fake_bin = repo / "fake-bin"
            fake_bin.mkdir()
            real_git = shutil.which("git")
            self.assertIsNotNone(real_git)
            fake_git = fake_bin / "git"
            fake_git.write_text(
                "#!/bin/sh\n"
                "case \"$*\" in\n"
                "  *crypto_trader_post_training_needs_godmode_and_finalization*)\n"
                "    case \"${FAKE_GIT_MODE:-}\" in\n"
                "      contradictory) printf 'HEAD:README.md:1:forbidden\\n'; exit 1 ;;\n"
                "      error) printf 'simulated git grep failure\\n' >&2; exit 128 ;;\n"
                "    esac\n"
                "    ;;\n"
                "esac\n"
                f'exec "{real_git}" "$@"\n',
                encoding="utf-8",
            )
            fake_git.chmod(0o755)

            for mode, marker in (
                ("contradictory", "contradictory no-match output"),
                ("error", "operator checkout reference scan failed with status 128"),
            ):
                with self.subTest(mode=mode):
                    result = run(
                        [
                            "env",
                            f"PATH={fake_bin}:/usr/bin:/bin",
                            f"FAKE_GIT_MODE={mode}",
                            "bash",
                            "scripts/agent_hooks/branch_lane_guard.sh",
                            "--lane",
                            "public",
                            "--ref",
                            "HEAD",
                        ],
                        repo,
                    )
                    self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn(marker, result.stderr)

            read_only_scan = repo / "read-only-scan"
            read_only_scan.mkdir()
            read_only_scan.chmod(0o555)
            fake_mktemp = fake_bin / "mktemp"
            fake_mktemp.write_text(
                "#!/bin/sh\n"
                "printf '%s\\n' \"${FAKE_MKTEMP_DIR:?}\"\n",
                encoding="utf-8",
            )
            fake_mktemp.chmod(0o755)
            unwritable = run(
                [
                    "env",
                    f"PATH={fake_bin}:/usr/bin:/bin",
                    f"FAKE_MKTEMP_DIR={read_only_scan}",
                    "bash",
                    "scripts/agent_hooks/branch_lane_guard.sh",
                    "--lane",
                    "public",
                    "--ref",
                    "HEAD",
                ],
                repo,
            )
            self.assertNotEqual(unwritable.returncode, 0, unwritable.stdout + unwritable.stderr)
            self.assertIn("could not open scan output", unwritable.stderr)

    def test_distribution_smoke_defaults_are_repo_local_and_opt_in(self) -> None:
        smoke = (ROOT / "scripts/devnet_smoke.sh").read_text(encoding="utf-8")
        recipes = (ROOT / "justfile").read_text(encoding="utf-8")
        env_example = (ROOT / ".env.example").read_text(encoding="utf-8")
        combined = "\n".join((smoke, recipes))
        for marker in (
            "Documents/Projects",
            "context-lattice-private",
            "crypto_trader_post_training_needs_godmode_and_finalization",
        ):
            self.assertNotIn(marker, combined)
        self.assertIn('BOOTSTRAP_SIDECAR="${BOOTSTRAP_SIDECAR:-0}"', smoke)
        self.assertIn('SKIP_SIDECAR_CHECK="${SKIP_SIDECAR_CHECK:-1}"', smoke)
        self.assertIn('SIDECAR_START_CMD="${SIDECAR_START_CMD:-}"', smoke)
        self.assertIn("docker compose up -d gateway-go", smoke)
        self.assertIn('SMOKE_PROJECT_NAME="${SMOKE_PROJECT_NAME:-contextlattice-smoke}"', smoke)
        self.assertNotIn('local project="${1:-_global}"', smoke)
        self.assertIn('CHECK_LOCAL_TELEMETRY="${CHECK_LOCAL_TELEMETRY:-0}"', smoke)
        self.assertIn('MINDSDB_SMOKE="${MINDSDB_SMOKE:-0}"', smoke)
        self.assertIn("GATEWAY_HOST_PORT=8091", smoke)
        self.assertNotIn('ORCH_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"', smoke)
        self.assertIn('devnet-smoke RUN_CARGO="0" CONFIG="config.toml"', recipes)
        self.assertIn("GATEWAY_IDENTITY_REQUIRED=1", recipes)
        self.assertIn("EXPECTED_GATEWAY_SOURCE_COMMIT", recipes)
        self.assertIn("EXPECTED_GATEWAY_SOURCE_TREE", recipes)
        self.assertIn("ORCH_BUILD=1", recipes)
        self.assertIn('cd "{{repo_root}}"', recipes)
        self.assertIn("devnet-up: sidecar-config-check orch-up sidecar-up", recipes)
        self.assertIn("BOOTSTRAP_SIDECAR=0", env_example)
        self.assertIn("SKIP_SIDECAR_CHECK=1", env_example)

    def test_requested_cargo_smoke_fails_when_manifest_is_absent(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-cargo-smoke-") as tmp:
            result = run(
                [
                    "env",
                    "RUN_CARGO_SMOKE=1",
                    f"CARGO_PROJECT_DIR={tmp}",
                    "BOOTSTRAP_ORCH=0",
                    "SKIP_ORCH_CHECK=1",
                    "SKIP_SIDECAR_CHECK=1",
                    "SMOKE_WRITE=0",
                    "MINDSDB_SMOKE=0",
                    "bash",
                    str(ROOT / "scripts/devnet_smoke.sh"),
                ],
                ROOT,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("RUN_CARGO_SMOKE=1 requires", result.stderr)
            self.assertNotIn("skipping cargo smoke", result.stderr.lower())

    def test_gateway_identity_mismatch_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-identity-smoke-") as tmp:
            fake_bin = Path(tmp) / "bin"
            fake_bin.mkdir()
            fake_curl = fake_bin / "curl"
            fake_curl.write_text(
                """#!/usr/bin/env bash
url=""
for arg in "$@"; do
  case "$arg" in http://*|https://*) url="$arg" ;; esac
done
case "$url" in
  */health) printf '%s\\n' '{"ok":true,"service":"gateway-go","build":{"version":"development","source_bound":true,"source_commit":"wrong","source_tree":"wrong"}}' ;;
  */status) printf '%s\\n' '{"ok":true}' ;;
esac
""",
                encoding="utf-8",
            )
            fake_curl.chmod(0o755)
            for skip_orch_check in ("0", "1"):
                with self.subTest(skip_orch_check=skip_orch_check):
                    result = run(
                        [
                            "env",
                            f"PATH={fake_bin}:/usr/bin:/bin",
                            "CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8091",
                            "GATEWAY_IDENTITY_REQUIRED=1",
                            "EXPECTED_GATEWAY_VERSION=development",
                            "EXPECTED_GATEWAY_SOURCE_COMMIT=expected-commit",
                            "EXPECTED_GATEWAY_SOURCE_TREE=expected-tree",
                            "BOOTSTRAP_ORCH=0",
                            f"SKIP_ORCH_CHECK={skip_orch_check}",
                            "SKIP_SIDECAR_CHECK=1",
                            "SMOKE_WRITE=0",
                            "MINDSDB_SMOKE=0",
                            "RUN_CARGO_SMOKE=0",
                            f"CONTEXTLATTICE_LOCAL_BACKUP_DIR={tmp}/backup",
                            f"CONTEXTLATTICE_LOCAL_STORE_PATH={tmp}/spool",
                            "bash",
                            str(ROOT / "scripts/devnet_smoke.sh"),
                        ],
                        ROOT,
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("gateway commit does not match", result.stderr)

    def test_memory_smoke_rejects_stale_readback(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-readback-smoke-") as tmp:
            fake_bin = Path(tmp) / "bin"
            fake_bin.mkdir()
            fake_curl = fake_bin / "curl"
            fake_curl.write_text(
                """#!/usr/bin/env bash
url=""
for arg in "$@"; do
  case "$arg" in http://*|https://*) url="$arg" ;; esac
done
case "$url" in
  */memory/files/*) printf '%s\\n' '{"content":"stale payload"}' ;;
esac
""",
                encoding="utf-8",
            )
            fake_curl.chmod(0o755)
            result = run(
                [
                    "env",
                    f"PATH={fake_bin}:/usr/bin:/bin",
                    "SKIP_ORCH_CHECK=1",
                    "SKIP_SIDECAR_CHECK=1",
                    "SMOKE_WRITE=1",
                    "MINDSDB_SMOKE=0",
                    "RUN_CARGO_SMOKE=0",
                    f"CONTEXTLATTICE_LOCAL_BACKUP_DIR={tmp}/backup",
                    f"CONTEXTLATTICE_LOCAL_STORE_PATH={tmp}/spool",
                    "bash",
                    str(ROOT / "scripts/devnet_smoke.sh"),
                ],
                ROOT,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("memory readback did not exactly match", result.stderr)

    def test_memory_smoke_rejects_readback_that_only_contains_payload(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-readback-superset-") as tmp:
            fake_bin = Path(tmp) / "bin"
            fake_bin.mkdir()
            fake_curl = fake_bin / "curl"
            fake_curl.write_text(
                """#!/usr/bin/env bash
state="${FAKE_CURL_STATE:?}"
url=""
payload=""
while (($#)); do
  case "$1" in
    -d|--data|--data-binary) payload="${2:-}"; shift 2 ;;
    http://*|https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  */memory/write)
    printf '%s' "$payload" | sed -E 's/.*"content":"([^"]*)".*/\\1/' > "$state"
    printf '%s\n' '{}'
    ;;
  */memory/files/*) printf 'prefix %s suffix\n' "$(cat "$state")" ;;
esac
""",
                encoding="utf-8",
            )
            fake_curl.chmod(0o755)
            result = run(
                [
                    "env",
                    f"PATH={fake_bin}:/usr/bin:/bin",
                    f"FAKE_CURL_STATE={tmp}/written-content",
                    "SKIP_ORCH_CHECK=1",
                    "SKIP_SIDECAR_CHECK=1",
                    "SMOKE_WRITE=1",
                    "MINDSDB_SMOKE=0",
                    "RUN_CARGO_SMOKE=0",
                    f"CONTEXTLATTICE_LOCAL_BACKUP_DIR={tmp}/backup",
                    f"CONTEXTLATTICE_LOCAL_STORE_PATH={tmp}/spool",
                    "bash",
                    str(ROOT / "scripts/devnet_smoke.sh"),
                ],
                ROOT,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("memory readback did not exactly match", result.stderr)

    def test_shared_readme_does_not_link_paid_only_docs(self) -> None:
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        self.assertNotIn("docs/entitled-frontier-t1.md", readme)
        self.assertNotIn("docs/runtime-license.md", readme)
        self.assertIn("docs/public_overview/premium.html", readme)

    def test_sidecar_bootstrap_requires_an_explicit_command(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-sidecar-smoke-") as tmp:
            result = run(
                [
                    "env",
                    "BOOTSTRAP_SIDECAR=1",
                    "SIDECAR_START_CMD=",
                    "SIDECAR_HEALTH_URL=http://127.0.0.1:9/health",
                    "SKIP_ORCH_CHECK=1",
                    "SKIP_SIDECAR_CHECK=1",
                    "SMOKE_WRITE=0",
                    "MINDSDB_SMOKE=0",
                    "RUN_CARGO_SMOKE=0",
                    f"CONTEXTLATTICE_LOCAL_BACKUP_DIR={tmp}/backup",
                    f"CONTEXTLATTICE_LOCAL_STORE_PATH={tmp}/spool",
                    "bash",
                    str(ROOT / "scripts/devnet_smoke.sh"),
                ],
                ROOT,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("requires an explicit SIDECAR_START_CMD", result.stderr)

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
