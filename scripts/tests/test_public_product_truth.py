from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / "scripts/agent/audit-public-product-truth"
SYNC = ROOT / "scripts/sync_public_overview.sh"
DESCRIPTION = (
    "The local-first intelligence layer that gives AI agents durable continuity, "
    "explainable retrieval, portable context, and verified learning across harnesses."
)
PILLARS = (
    "Durable continuity",
    "Explainable retrieval",
    "Portable context",
    "Verified skill evolution",
    "Aggregate Signal",
)
WORKFLOW_PATHS = (
    "AGENTS.md",
    ".github/pull_request_template.md",
    "README.md",
    "LICENSE",
    "Makefile",
    "config/commercial_truth.v1.json",
    "crates/Cargo.toml",
    "crates/**/Cargo.toml",
    "docs/public_overview/**",
    "docs/wiki/**",
    "docs/releases/**",
    "docs/host-supervisor-release-safety.md",
    "packaging/**",
    "scripts/build_release_payload.sh",
    "scripts/install_global_agent_tools.sh",
    "scripts/install_global_agent_tools.ps1",
    "scripts/public_sync_guard.sh",
    "scripts/agent_hooks/public_leak_guard.sh",
    "scripts/tests/test_public_boundary_guards.py",
    "scripts/tests/test_t*_public_projection.py",
    "scripts/sync_public_overview.sh",
    "scripts/install_public_overview_sync.sh",
    "scripts/agent/audit-public-product-truth",
    "scripts/agent/audit-agent-global-install-smoke",
    "scripts/agent/audit-host-supervisor-safety",
    "scripts/generate_commercial_truth.py",
    "scripts/build_public_docs.py",
    "scripts/tests/test_commercial_truth.py",
    "scripts/tests/test_public_docs.py",
    "scripts/tests/test_host_supervisor_safety_audit.py",
    "scripts/tests/test_public_product_truth.py",
    "services/gateway-go/commercial_contract_generated.go",
    "services/gateway-go/commercial_contract_generated_test.go",
    ".github/workflows/release-installers.yml",
    ".github/workflows/public-product-truth.yml",
)
SYNC_REQUIRED_FILES = (
    "index.html",
    "architecture.html",
    "local-ai-workspaces.html",
    "scaling-memory.html",
    "wiki.html",
    "updates.html",
    "roadmap.html",
    "installation.html",
    "cli.html",
    "integration.html",
    "premium.html",
    "app.html",
    "troubleshooting.html",
    "contact.html",
    "llms.txt",
    "commercial-truth.json",
    "robots.txt",
    "sitemap.xml",
    "styles.css",
)


def write(root: Path, relative: str, content: str | bytes) -> None:
    path = root / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    if isinstance(content, bytes):
        path.write_bytes(content)
    else:
        path.write_text(content, encoding="utf-8")


def fixture(root: Path, lane: str = "public") -> None:
    contract = {
        "product": {
            "version": "4.0.2",
            "stable_tag": "v4.0.2",
            "release_train": "4.0",
            "canonical_description": DESCRIPTION,
            "source_licenses": {
                "public": {"spdx": "Apache-2.0"},
                "commercial": {"spdx": "BUSL-1.1"},
            },
        }
    }
    write(root, "config/commercial_truth.v1.json", json.dumps(contract))
    if lane == "public":
        license_text = "Apache License\nVersion 2.0, January 2004\n"
        readme_license = "Apache License 2.0"
        component_license = "Apache-2.0"
    else:
        license_text = "Business Source License 1.1\nSPDX-License-Identifier: BUSL-1.1\n"
        readme_license = "BSL 1.1"
        component_license = "BUSL-1.1"
    write(root, "LICENSE", license_text)
    write(
        root,
        "README.md",
        f"# ContextLattice\n\n{DESCRIPTION}\n\n"
        f"CLI is the primary interface. Current: v4.0.2. {readme_license}.\n"
        "Primary interaction path: `gmake quickstart`; installers are bootstrap alternatives.\n"
        "Release notes, older entries are historical: docs/releases/v4.0.1.md.\n",
    )
    write(
        root,
        "crates/Cargo.toml",
        "[workspace]\nresolver = \"2\"\nmembers = [\"component\"]\n\n"
        f"[workspace.package]\nlicense = \"{component_license}\"\npublish = false\n",
    )
    write(
        root,
        "crates/component/Cargo.toml",
        "[package]\nname = \"component\"\nversion = \"0.0.0\"\n"
        "license.workspace = true\npublish.workspace = true\n",
    )

    pillar_text = " ".join(PILLARS)
    page = (
        "<!doctype html><html><head>"
        f'<meta name="description" content="{DESCRIPTION}">'
        '<script type="application/ld+json">{"name": "ContextLattice", '
        '"operatingSystem": "macOS, Linux, Windows (WSL2)"}</script>'
        "</head><body>"
        f"<p>{DESCRIPTION}</p><p>Current v4.0.2. Primary interface:</strong> CLI.</p>"
        f"<p>{pillar_text}</p>"
        '<a href="architecture.html">Architecture</a>'
        '<div class="mermaid">flowchart LR\n%% safe comment\nA --> B</div>'
        "</body></html>\n"
    )
    for name in (
        "index.html",
        "index-orb-white.html",
        "architecture.html",
        "integration.html",
        "roadmap.html",
        "troubleshooting.html",
        "updates.html",
    ):
        write(root, f"docs/public_overview/{name}", page)
    write(root, "docs/public_overview/cli.html", page)
    write(
        root,
        "docs/public_overview/docs/index.html",
        page.replace('href="architecture.html"', 'href="/architecture.html"'),
    )
    write(root, "docs/public_overview/docs/docs.css", "")
    write(root, "docs/public_overview/docs/docs.js", "")
    write(
        root,
        "docs/public_overview/docs/search-index.json",
        json.dumps({"schemaId": "contextlattice_public_docs_search.v1", "documents": []}),
    )
    write(
        root,
        "docs/public_overview/installation.html",
        page + "<p><code>gmake quickstart</code> remains the prescribed path for technical installs.</p>\n",
    )
    write(
        root,
        "docs/public_overview/premium.html",
        page
        + "Controlled activation preview. Production is hard-blocked until independent privacy and utility reviews pass.\n",
    )
    sitemap_pages = (
        "",
        "architecture.html",
        "docs/",
        "cli.html",
        "installation.html",
        "integration.html",
        "premium.html",
        "roadmap.html",
        "troubleshooting.html",
        "updates.html",
    )
    sitemap_urls = "\n".join(
        f"  <url><loc>https://contextlattice.io/{name}</loc></url>" for name in sitemap_pages
    )
    write(root, "docs/public_overview/robots.txt", "User-agent: *\nAllow: /\n\nSitemap: https://contextlattice.io/sitemap.xml\n")
    write(
        root,
        "docs/public_overview/sitemap.xml",
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n'
        f"{sitemap_urls}\n"
        "</urlset>\n",
    )
    write(root, "docs/public_overview/llms.txt", f"{DESCRIPTION}\nCLI is the primary interface. Current v4.0.2\n")
    write(root, "docs/public_overview/commercial-truth.json", json.dumps(contract))
    write(root, "docs/readme/contextlattice-architecture-readme-v2-2026-04-28.png", b"png")
    write(root, "docs/releases/v4.0.1.md", "# ContextLattice v4.0.1\n")
    write(root, "docs/releases/v4.0.2.md", "# ContextLattice v4.0.2\n")

    write(
        root,
        "services/gateway-go/commercial_contract_generated.go",
        '// Code generated by scripts/generate_commercial_truth.py; DO NOT EDIT.\n'
        'package main\n\nconst commercialTruthProductVersion = "4.0.2"\n'
        'const commercialTruthStableTag = "v4.0.2"\n'
        'const commercialTruthReleaseTrain = "4.0"\n',
    )
    write(
        root,
        "services/gateway-go/commercial_contract_generated_test.go",
        'package main\n\nconst commercialTruthContractPath = "../../config/commercial_truth.v1.json"\n'
        "func verifyReleaseTruth() { _, _, _ = commercialTruthProductVersion, commercialTruthStableTag, commercialTruthReleaseTrain }\n",
    )
    workflow_items = "\n".join(f'      - "{path}"' for path in WORKFLOW_PATHS)
    write(
        root,
        ".github/workflows/public-product-truth.yml",
        "on:\n  pull_request:\n    paths:\n"
        f"{workflow_items}\n"
        "  push:\n    branches:\n      - main\n    paths:\n"
        f"{workflow_items}\n"
        "  workflow_dispatch:\n",
    )

    write(
        root,
        "contextlattice-dashboard/app/layout.tsx",
        'export const metadata = { description: "Local-first intelligence layer" };\n',
    )
    write(root, "contextlattice-dashboard/app/overview/page.tsx", "export default function Overview() { return null; }")
    write(root, "contextlattice-dashboard/components/OverviewCommandDeck.tsx", pillar_text)
    write(root, "contextlattice-dashboard/app/pricing/page.tsx", "ALL_PLANS cl-page")
    write(
        root,
        "contextlattice-dashboard/lib/authMode.ts",
        'export const dashboardAuthRequired = () => process.env.AUTH_REQUIRED === "true";\n',
    )
    write(
        root,
        "contextlattice-dashboard/tests/auth-boundary.test.ts",
        'test("dashboard auth is disabled by default for local OSS mode", () => {});\n'
        'process.env.AUTH_REQUIRED = "true";\n',
    )


def run_audit(root: Path, lane: str = "auto") -> tuple[subprocess.CompletedProcess[str], dict]:
    result = subprocess.run(
        [str(AUDIT), "--root", str(root), "--lane", lane],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    return result, json.loads(result.stdout)


def failure_ids(payload: dict) -> set[str]:
    return {item["check_id"] for item in payload["failures"]}


def git(root: Path, *args: str) -> None:
    subprocess.run(
        ["git", *args],
        cwd=root,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=True,
    )


def sync_fixture(
    root: Path,
    *,
    audit_ok: bool = True,
    lane: str = "commercial",
    proof: str = "annotated",
    proof_tag: str | None = None,
    missing_file: str | None = None,
) -> None:
    if lane not in {"public", "commercial"}:
        raise ValueError(f"unknown audit lane: {lane}")
    write(root, "scripts/sync_public_overview.sh", SYNC.read_text(encoding="utf-8"))
    (root / "scripts/sync_public_overview.sh").chmod(0o755)
    audit_exit = 0 if audit_ok else 9
    audit_payload = json.dumps({"ok": audit_ok, "lane": lane})
    write(
        root,
        "scripts/agent/audit-public-product-truth",
        "#!/usr/bin/env bash\n"
        'printf "called\\n" > "${AUDIT_MARKER:?}"\n'
        f"printf '%s\\n' '{audit_payload}'\nexit {audit_exit}\n",
    )
    (root / "scripts/agent/audit-public-product-truth").chmod(0o755)
    write(
        root,
        "config/commercial_truth.v1.json",
        json.dumps({"product": {"version": "4.0.2", "stable_tag": "v4.0.2"}}),
    )
    for filename in SYNC_REQUIRED_FILES:
        if filename != missing_file:
            write(root, f"docs/public_overview/{filename}", f"fixture {filename}\n")
    git(root, "init", "-q")
    git(root, "config", "user.name", "Public Product Truth Test")
    git(root, "config", "user.email", "public-product-truth@example.invalid")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", "fixture source")
    release_proof_tag = proof_tag or ("v4.0.2" if lane == "public" else "v4.0.2-origin")
    if proof in {"annotated", "mismatch"}:
        git(root, "tag", "-a", release_proof_tag, "-m", "immutable fixture proof")
    elif proof == "lightweight":
        git(root, "tag", release_proof_tag)
    elif proof != "none":
        raise ValueError(f"unknown proof mode: {proof}")
    if proof == "mismatch":
        write(root, "post-proof.txt", "committed after proof\n")
        git(root, "add", "post-proof.txt")
        git(root, "commit", "-q", "-m", "move source past proof")


def run_sync(
    root: Path,
    mode: str,
    marker: Path,
    target: Path,
    *,
    proof_tag: str | None = None,
    interpreter: str | None = None,
) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env.pop("PUBLIC_RELEASE_PROOF_TAG", None)
    env.update(
        {
            "AUDIT_MARKER": str(marker),
            "PUBLIC_DIR": str(target),
            "PUBLIC_OWNER": "fixture-owner",
            "PUBLIC_REPO": "fixture-site",
        }
    )
    if proof_tag is not None:
        env["PUBLIC_RELEASE_PROOF_TAG"] = proof_tag
    command = [str(root / "scripts/sync_public_overview.sh"), mode]
    if interpreter is not None:
        command = [interpreter, "-f", *command]
    return subprocess.run(
        command,
        cwd=root,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
        timeout=10,
    )


class PublicProductTruthTests(unittest.TestCase):
    def test_public_fixture_passes_with_dynamic_history_and_open_dashboard_posture(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-truth-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            result, payload = run_audit(root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["lane"], "public")
            self.assertEqual(payload["predecessor_version"], "4.0.1")

    def test_commercial_fixture_passes_with_busl(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-product-truth-") as tmp:
            root = Path(tmp)
            fixture(root, "commercial")
            result, payload = run_audit(root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["lane"], "commercial")

    def test_public_readme_rejects_busl_and_private_boundaries(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-boundary-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            readme = root / "README.md"
            readme.write_text(
                readme.read_text(encoding="utf-8")
                + "BSL 1.1. Internal"
                + " docs: docs/private. /Users/example/project\n",
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(
                {"readme_public_license", "readme_public_boundary", "readme_public_paths"}
                <= failure_ids(payload)
            )

    def test_site_rejects_broken_links_and_html_comments_inside_mermaid(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-site-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            index = root / "docs/public_overview/index.html"
            index.write_text(
                index.read_text(encoding="utf-8")
                .replace("%% safe comment", "<!-- invalid Mermaid comment -->")
                .replace("architecture.html", "missing.html"),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue({"site_local_links", "site_mermaid_comments"} <= failure_ids(payload))

    def test_historical_release_notes_updates_and_roadmaps_may_name_predecessors(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-history-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            for relative in ("docs/public_overview/updates.html", "docs/public_overview/roadmap.html"):
                path = root / relative
                path.write_text(
                    path.read_text(encoding="utf-8")
                    + "<p>Historical release note: v4.0.1 was the current V3-roadmap successor.</p>\n",
                    encoding="utf-8",
                )
            result, payload = run_audit(root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertTrue(payload["ok"])

    def test_present_tense_predecessor_claims_fail_across_public_surfaces(self) -> None:
        claims = {
            "README.md": "Current release: v4.0.1.\n",
            "docs/public_overview/index.html": "<p>Latest stable release: v4.0.1.</p>\n",
            "docs/public_overview/installation.html": "<p>Install v4.0.1 now.</p>\n",
            "docs/public_overview/integration.html": "<p>Use v4.0.1 as the current integration baseline.</p>\n",
            "docs/public_overview/roadmap.html": "<p>Current release: v4.0.1.</p>\n",
        }
        for relative, claim in claims.items():
            with self.subTest(relative=relative), tempfile.TemporaryDirectory(prefix="public-product-stale-") as tmp:
                root = Path(tmp)
                fixture(root, "public")
                path = root / relative
                path.write_text(path.read_text(encoding="utf-8") + claim, encoding="utf-8")
                result, payload = run_audit(root)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("present_version_claims", failure_ids(payload))

    def test_predecessor_is_derived_instead_of_hard_coded(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-version-history-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            contract_path = root / "config/commercial_truth.v1.json"
            contract = json.loads(contract_path.read_text(encoding="utf-8"))
            contract["product"].update(version="5.1.0", stable_tag="v5.1.0", release_train="5.1")
            contract_path.write_text(json.dumps(contract), encoding="utf-8")
            for path in [root / "README.md", *sorted((root / "docs/public_overview").rglob("*.html")), root / "docs/public_overview/llms.txt"]:
                path.write_text(path.read_text(encoding="utf-8").replace("4.0.2", "5.1.0"), encoding="utf-8")
            generated = root / "services/gateway-go/commercial_contract_generated.go"
            generated.write_text(
                generated.read_text(encoding="utf-8").replace("4.0.2", "5.1.0").replace('"4.0"', '"5.1"'),
                encoding="utf-8",
            )
            write(root, "docs/releases/v5.1.0.md", "# ContextLattice v5.1.0\n")
            result, payload = run_audit(root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(payload["predecessor_version"], "4.0.2")

    def test_generated_go_source_and_test_must_follow_canonical_contract(self) -> None:
        mutations = {
            "services/gateway-go/commercial_contract_generated.go": (
                'commercialTruthProductVersion = "4.0.2"',
                'commercialTruthProductVersion = "4.0.1"',
                "generated_go_source",
            ),
            "services/gateway-go/commercial_contract_generated_test.go": (
                "package main",
                'package main\n\nconst staleRelease = "v4.0.1"',
                "generated_go_test",
            ),
        }
        for relative, (old, new, expected_check) in mutations.items():
            with self.subTest(relative=relative), tempfile.TemporaryDirectory(prefix="public-product-go-") as tmp:
                root = Path(tmp)
                fixture(root, "public")
                path = root / relative
                path.write_text(path.read_text(encoding="utf-8").replace(old, new), encoding="utf-8")
                result, payload = run_audit(root)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected_check, failure_ids(payload))

    def test_workflow_requires_every_product_truth_trigger_for_pull_and_push(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-workflow-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            workflow = root / ".github/workflows/public-product-truth.yml"
            workflow.write_text(
                workflow.read_text(encoding="utf-8").replace('      - "packaging/**"\n', "", 1),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            failures = [item for item in payload["failures"] if item["check_id"] == "workflow_trigger_coverage"]
            self.assertTrue(any("pull_request" in item["detail"] and "packaging/**" in item["detail"] for item in failures))

    def test_install_channel_release_note_and_rust_policy_are_enforced(self) -> None:
        mutations = {
            "docs/public_overview/installation.html": ("remains the prescribed path", "is available", "install_channel_declaration"),
            "docs/releases/v4.0.2.md": ("v4.0.2", "release pending", "current_release_note"),
            "crates/Cargo.toml": ("publish = false", "publish = true", "rust_workspace_policy"),
            "crates/component/Cargo.toml": ("publish.workspace = true", "", "rust_workspace_policy"),
        }
        for relative, (old, new, expected_check) in mutations.items():
            with self.subTest(relative=relative), tempfile.TemporaryDirectory(prefix="public-product-policy-") as tmp:
                root = Path(tmp)
                fixture(root, "public")
                path = root / relative
                path.write_text(path.read_text(encoding="utf-8").replace(old, new), encoding="utf-8")
                result, payload = run_audit(root)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected_check, failure_ids(payload))

    def test_site_rejects_missing_fragments_and_css_assets(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-assets-") as tmp:
            root = Path(tmp)
            fixture(root, "public")
            index = root / "docs/public_overview/index.html"
            index.write_text(
                index.read_text(encoding="utf-8").replace("architecture.html", "architecture.html#missing"),
                encoding="utf-8",
            )
            write(root, "docs/public_overview/site.css", "body { background: url('missing.png'); }\n")
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue({"site_html_fragments", "site_css_assets"} <= failure_ids(payload))

    def test_robots_and_sitemap_are_required_and_sitemap_covers_canonical_html(self) -> None:
        mutations = {
            "docs/public_overview/robots.txt": (
                "Sitemap: https://contextlattice.io/sitemap.xml",
                "",
                "site_robots",
            ),
            "docs/public_overview/sitemap.xml": (
                "<urlset",
                "<broken",
                "site_sitemap",
            ),
            "docs/public_overview/sitemap.xml#missing": (
                "  <url><loc>https://contextlattice.io/integration.html</loc></url>\n",
                "",
                "site_sitemap",
            ),
        }
        for case, (old, new, expected_check) in mutations.items():
            with self.subTest(case=case), tempfile.TemporaryDirectory(prefix="public-product-sitemap-") as tmp:
                root = Path(tmp)
                fixture(root, "public")
                relative = case.split("#", 1)[0]
                path = root / relative
                path.write_text(path.read_text(encoding="utf-8").replace(old, new), encoding="utf-8")
                result, payload = run_audit(root)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected_check, failure_ids(payload))

    def test_sync_check_and_dry_run_audit_exact_clean_tag_without_publishing(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-sync-") as tmp:
            sandbox = Path(tmp)
            root = sandbox / "repo"
            sync_fixture(root)
            marker = sandbox / "audit-called"
            target = sandbox / "published-site"
            for mode in ("--check", "--dry-run"):
                with self.subTest(mode=mode):
                    marker.unlink(missing_ok=True)
                    result = run_sync(root, mode, marker, target)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertTrue(marker.is_file(), "product-truth audit was not invoked")
                    self.assertFalse(target.exists(), "non-publishing mode mutated the destination")
                    self.assertIn("no clone, destination write, commit, or push performed", result.stdout)
            status = subprocess.run(
                ["git", "status", "--porcelain"],
                cwd=root,
                text=True,
                stdout=subprocess.PIPE,
                check=True,
            )
            self.assertEqual(status.stdout, "")

    @unittest.skipUnless(Path("/bin/zsh").is_file(), "requires /bin/zsh")
    def test_sync_check_is_zsh_compatible(self) -> None:
        with tempfile.TemporaryDirectory(prefix="public-product-sync-zsh-") as tmp:
            sandbox = Path(tmp)
            root = sandbox / "repo"
            sync_fixture(root)
            result = run_sync(
                root,
                "--check",
                sandbox / "audit-called",
                sandbox / "published-site",
                interpreter="/bin/zsh",
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn("no clone, destination write, commit, or push performed", result.stdout)

    def test_sync_refuses_dirty_or_unaudited_source_before_publishing(self) -> None:
        cases = (("dirty", True), ("unaudited", False))
        for case, audit_ok in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory(prefix="public-product-sync-gate-") as tmp:
                sandbox = Path(tmp)
                root = sandbox / "repo"
                sync_fixture(root, audit_ok=audit_ok)
                if case == "dirty":
                    write(root, "untracked.txt", "dirty\n")
                marker = sandbox / "audit-called"
                target = sandbox / "published-site"
                result = run_sync(root, "--check", marker, target)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(target.exists())
                if case == "dirty":
                    self.assertFalse(marker.exists(), "dirty source reached the audit")
                    self.assertIn("source worktree is dirty before audit", result.stderr)
                else:
                    self.assertTrue(marker.exists())
                    self.assertIn("product-truth audit failed", result.stderr)

    def test_sync_requires_annotated_proof_for_exact_source_head(self) -> None:
        for proof in ("none", "lightweight", "mismatch"):
            with self.subTest(proof=proof), tempfile.TemporaryDirectory(prefix="public-product-sync-proof-") as tmp:
                sandbox = Path(tmp)
                root = sandbox / "repo"
                sync_fixture(root, proof=proof)
                marker = sandbox / "audit-called"
                target = sandbox / "published-site"
                result = run_sync(root, "--check", marker, target)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(target.exists())
                if proof == "mismatch":
                    self.assertIn("not source HEAD", result.stderr)
                else:
                    self.assertIn("lacks an immutable annotated release-candidate tag", result.stderr)

    def test_sync_release_proof_defaults_are_lane_aware_and_override_is_bounded(self) -> None:
        cases = (
            ("public", "v4.0.2", None),
            ("commercial", "v4.0.2-origin", None),
            ("commercial", "v4.0.2-public-paid", "v4.0.2-public-paid"),
        )
        for lane, fixture_tag, override in cases:
            with self.subTest(lane=lane, proof_tag=fixture_tag), tempfile.TemporaryDirectory(
                prefix="public-product-sync-lane-proof-"
            ) as tmp:
                sandbox = Path(tmp)
                root = sandbox / "repo"
                sync_fixture(root, lane=lane, proof_tag=fixture_tag)
                result = run_sync(
                    root,
                    "--check",
                    sandbox / "audit-called",
                    sandbox / "published-site",
                    proof_tag=override,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(f"lane={lane} proof={fixture_tag}", result.stdout)

        with tempfile.TemporaryDirectory(prefix="public-product-sync-invalid-proof-") as tmp:
            sandbox = Path(tmp)
            root = sandbox / "repo"
            sync_fixture(root)
            result = run_sync(
                root,
                "--check",
                sandbox / "audit-called",
                sandbox / "published-site",
                proof_tag="v4.0.2-unreviewed",
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("not an allowed lane tag for v4.0.2", result.stderr)

        with tempfile.TemporaryDirectory(prefix="public-product-sync-cross-lane-proof-") as tmp:
            sandbox = Path(tmp)
            root = sandbox / "repo"
            sync_fixture(root, lane="public")
            result = run_sync(
                root,
                "--check",
                sandbox / "audit-called",
                sandbox / "published-site",
                proof_tag="v4.0.2-origin",
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("not an allowed lane tag for v4.0.2", result.stderr)

    def test_sync_deployment_manifest_requires_robots_and_sitemap(self) -> None:
        for missing_file in ("robots.txt", "sitemap.xml"):
            with self.subTest(missing_file=missing_file), tempfile.TemporaryDirectory(prefix="public-product-sync-files-") as tmp:
                sandbox = Path(tmp)
                root = sandbox / "repo"
                sync_fixture(root, missing_file=missing_file)
                marker = sandbox / "audit-called"
                target = sandbox / "published-site"
                result = run_sync(root, "--check", marker, target)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(target.exists())
                self.assertIn(f"missing required deployment file: {missing_file}", result.stderr)


if __name__ == "__main__":
    unittest.main()
