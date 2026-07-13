from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GENERATOR = ROOT / "scripts/generate_commercial_truth.py"
AUDIT = ROOT / "scripts/agent/audit-commercial-truth"

OWNED_PROJECTIONS = (
    Path("config/commercial_truth.v1.json"),
    Path("scripts/generate_commercial_truth.py"),
    Path("scripts/agent/audit-commercial-truth"),
    Path("contextlattice-dashboard/lib/billing/commercial.generated.ts"),
    Path("services/gateway-go/commercial_contract_generated.go"),
    Path("docs/public_overview/commercial-truth.json"),
    Path("docs/public_overview/premium.html"),
    Path("docs/public_overview/index.html"),
    Path("docs/public_overview/index-orb-white.html"),
    Path("docs/public_overview/llms.txt"),
    Path("docs/public_overview/cli.html"),
    Path("docs/public_overview/installation.html"),
    Path("launch_service/config/contextlattice.launch.json"),
)


def run(*args: str, root: Path = ROOT) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, *args],
        cwd=root,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def copy_fixture(destination: Path) -> None:
    for relative in OWNED_PROJECTIONS:
        source = ROOT / relative
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)


class CommercialTruthTests(unittest.TestCase):
    def test_contract_decisions(self) -> None:
        contract = json.loads((ROOT / "config/commercial_truth.v1.json").read_text(encoding="utf-8"))
        self.assertEqual(contract["product"]["version"], "3.17.3")
        self.assertEqual(contract["product"]["stable_tag"], "v3.17.3")
        self.assertEqual(contract["product"]["release_train"], "3.17")
        self.assertEqual(contract["product"]["primary_interface"], "cli")
        self.assertEqual(contract["product"]["python_role"], "build_and_development_tooling_only")

        plans = {plan["id"]: plan for plan in contract["plans"]}
        self.assertEqual(set(plans), {"free", "starter", "team", "operator", "enterprise"})
        self.assertEqual(plans["free"]["pricing"], {"monthly_usd": 0, "annual_usd": 0, "custom": False})
        self.assertEqual(plans["starter"]["pricing"]["monthly_usd"], 24)
        self.assertEqual(plans["starter"]["pricing"]["annual_usd"], 240)
        self.assertEqual(plans["team"]["pricing"]["monthly_usd"], 99)
        self.assertEqual(plans["team"]["pricing"]["annual_usd"], 990)
        self.assertEqual(plans["operator"]["pricing"]["monthly_usd"], 299)
        self.assertEqual(plans["operator"]["pricing"]["annual_usd"], 2990)
        self.assertTrue(plans["enterprise"]["pricing"]["custom"])
        self.assertIsNone(plans["enterprise"]["pricing"]["monthly_usd"])
        self.assertIsNone(plans["enterprise"]["pricing"]["annual_usd"])
        self.assertFalse(plans["enterprise"]["self_serve_purchasable"])

        self.assertEqual(contract["aliases"]["exact"]["pro"], "operator")
        self.assertEqual(contract["aliases"]["exact"]["business"], "team")
        self.assertEqual(contract["aliases"]["patterns"][0]["target"], "enterprise")

        features = {feature["id"]: feature for feature in contract["features"]}
        runtime_license = features["premium_runtime_keys"]
        self.assertEqual(runtime_license["buyer_label"], "Signed runtime licenses")
        self.assertIn("verifiable offline", runtime_license["description"])
        self.assertNotIn("runtime API keys", runtime_license["description"])

    def test_repository_generator_check_is_clean(self) -> None:
        result = run(str(GENERATOR), "--check")
        self.assertEqual(result.returncode, 0, result.stderr or result.stdout)
        payload = json.loads(result.stdout)
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["drift"], [])

    def test_public_projection_has_no_private_paths(self) -> None:
        payload = (ROOT / "docs/public_overview/commercial-truth.json").read_text(encoding="utf-8")
        self.assertNotIn("/Users/", payload)
        self.assertNotIn("/Volumes/", payload)
        self.assertNotIn("file://", payload)
        self.assertNotIn("BEGIN PRIVATE KEY", payload)
        self.assertEqual(json.loads(payload)["product"]["version"], "3.17.3")

    def test_pages_sync_publishes_commercial_truth(self) -> None:
        sync_script = (ROOT / "scripts/sync_public_overview.sh").read_text(encoding="utf-8")
        self.assertIn(
            'cp "$PUBLIC_SOURCE_DIR/commercial-truth.json" "$PUBLIC_DIR/commercial-truth.json"',
            sync_script,
        )

    def test_clean_fixture_audit_passes(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-clean-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            payload = json.loads(result.stdout)
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["findings"], [])

    def test_generator_check_rejects_drift(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-drift-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            public_json = fixture / "docs/public_overview/commercial-truth.json"
            public_json.write_text(public_json.read_text(encoding="utf-8") + "\n", encoding="utf-8")
            result = run(
                str(fixture / "scripts/generate_commercial_truth.py"),
                "--root",
                str(fixture),
                "--check",
                root=fixture,
            )
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stderr)
            self.assertIn("docs/public_overview/commercial-truth.json", payload["drift"])

    def test_audit_rejects_competing_active_catalog(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-catalog-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            plans = fixture / "contextlattice-dashboard/lib/billing/plans.ts"
            plans.write_text('export const PLANS = [{ id: "shadow", monthly: 1 }];\n', encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stdout)
            self.assertIn("competing_active_catalog", {row["code"] for row in payload["findings"]})

    def test_public_tree_excludes_paid_runtime_policy(self) -> None:
        self.assertFalse((ROOT / "services/gateway-go/entitlement_policy.go").exists())
        gateway_source = "\n".join(
            path.read_text(encoding="utf-8", errors="ignore")
            for path in (ROOT / "services/gateway-go").glob("*.go")
        )
        self.assertNotIn("GO_V4_ENTITLEMENT", gateway_source)
        self.assertNotIn("runtimeLicenseVerifier", gateway_source)

    def test_audit_rejects_stale_release_publish_input(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-launch-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            launch = fixture / "launch_service/config/contextlattice.launch.json"
            launch.write_text(
                launch.read_text(encoding="utf-8").replace("v3.17.3", "v9.9.9", 1),
                encoding="utf-8",
            )
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stdout)
            self.assertIn("release_publish_version_drift", {row["code"] for row in payload["findings"]})


if __name__ == "__main__":
    unittest.main()
