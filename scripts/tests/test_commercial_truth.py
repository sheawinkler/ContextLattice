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
    Path("config/env/strict_runtime.env"),
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
        self.assertEqual(contract["product"]["version"], "3.26.0")
        self.assertEqual(contract["product"]["stable_tag"], "v3.26.0")
        self.assertEqual(contract["product"]["release_train"], "3.26")
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
        self.assertEqual(plans["free"]["limits"]["included_seats"], 1)
        self.assertEqual(plans["starter"]["limits"]["included_seats"], 1)
        self.assertEqual(plans["team"]["limits"]["included_seats"], 5)
        self.assertEqual(plans["operator"]["limits"]["included_seats"], 1)
        self.assertEqual(plans["enterprise"]["limits"]["included_seats"], 100)

        frontier_t1 = {
            "frontier_semantic_continuity_automation",
            "frontier_shared_objective_graph",
            "frontier_shared_decision_provenance",
        }
        packet_automation = "frontier_delta_packet_automation"
        shared_proof = "frontier_shared_proof_timeline"
        frontier = frontier_t1 | {packet_automation, shared_proof}
        utility_core = "frontier_verified_utility_ledger"
        utility_analytics = "frontier_utility_analytics"
        utility_operations = "frontier_verified_efficiency_operations"
        frontier_t4 = {
            "frontier_retrieval_receipt_governance",
            "frontier_causal_bridge_governance",
            "frontier_continuous_counterfactual_eval",
            "frontier_evidence_reputation_activation",
            "frontier_continuous_retrieval_regression",
            "frontier_adversarial_defense_operations",
        }
        policy_laboratory = "frontier_policy_laboratory_automation"
        agent_fit = "frontier_agent_fit_governance"
        portable_governance = "frontier_portable_continuation_governance"
        skill_evolution = "frontier_skill_evolution_governance"
        continuity_zero = "frontier_continuity_zero_automation"
        self.assertTrue(frontier.isdisjoint(plans["free"]["feature_ids"]))
        self.assertIn(utility_core, plans["free"]["feature_ids"])
        self.assertTrue((frontier_t1 | {shared_proof}).isdisjoint(plans["starter"]["feature_ids"]))
        self.assertIn(packet_automation, plans["starter"]["feature_ids"])
        for plan_id in {"starter", "team", "operator", "enterprise"}:
            self.assertTrue({utility_core, utility_analytics}.issubset(plans[plan_id]["feature_ids"]))
        for plan_id in {"free", "starter", "team"}:
            self.assertNotIn(utility_operations, plans[plan_id]["feature_ids"])
        for plan_id in {"operator", "enterprise"}:
            self.assertIn(utility_operations, plans[plan_id]["feature_ids"])
            self.assertTrue(frontier_t4.issubset(plans[plan_id]["feature_ids"]))
            self.assertIn(policy_laboratory, plans[plan_id]["feature_ids"])
            self.assertIn(agent_fit, plans[plan_id]["feature_ids"])
            self.assertIn(portable_governance, plans[plan_id]["feature_ids"])
            self.assertIn(skill_evolution, plans[plan_id]["feature_ids"])
            self.assertIn(continuity_zero, plans[plan_id]["feature_ids"])
        self.assertTrue(frontier_t4.isdisjoint(plans["free"]["feature_ids"]))
        self.assertTrue(frontier_t4.isdisjoint(plans["starter"]["feature_ids"]))
        for plan_id in {"free", "starter", "team"}:
            self.assertNotIn(policy_laboratory, plans[plan_id]["feature_ids"])
            self.assertNotIn(agent_fit, plans[plan_id]["feature_ids"])
            self.assertNotIn(portable_governance, plans[plan_id]["feature_ids"])
            self.assertNotIn(skill_evolution, plans[plan_id]["feature_ids"])
        self.assertNotIn(continuity_zero, plans["free"]["feature_ids"])
        self.assertIn(continuity_zero, plans["starter"]["feature_ids"])
        self.assertIn(continuity_zero, plans["team"]["feature_ids"])
        self.assertTrue(
            {"frontier_retrieval_receipt_governance", "frontier_causal_bridge_governance"}.issubset(
                plans["team"]["feature_ids"]
            )
        )
        self.assertTrue(frontier.issubset(plans["team"]["feature_ids"]))
        self.assertTrue(frontier.issubset(plans["enterprise"]["feature_ids"]))
        self.assertEqual(
            frontier - {"frontier_shared_decision_provenance"},
            frontier.intersection(plans["operator"]["feature_ids"]),
        )
        self.assertEqual(
            {row["feature_id"] for row in contract["paid_feature_route_contracts"]},
            frontier
            | frontier_t4
            | {
                policy_laboratory,
                agent_fit,
                portable_governance,
                skill_evolution,
                continuity_zero,
                utility_analytics,
                utility_operations,
            },
        )
        for feature_id in frontier | frontier_t4 | {
            policy_laboratory,
            agent_fit,
            portable_governance,
            skill_evolution,
            continuity_zero,
            utility_core,
            utility_analytics,
            utility_operations,
        }:
            self.assertEqual(
                contract["release_availability"][feature_id],
                {
                    "availability": "generally_available",
                    "release_gate": "PASS",
                    "release_decision": "PROVEN",
                    "production_proven": True,
                },
            )

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
        public_truth = json.loads(payload)
        self.assertEqual(public_truth["product"]["version"], "3.26.0")
        self.assertEqual(
            public_truth["release_availability"]["frontier_semantic_continuity_automation"]["availability"],
            "generally_available",
        )
        self.assertEqual(
            public_truth["release_availability"]["frontier_semantic_continuity_automation"]["release_decision"],
            "PROVEN",
        )

    def test_public_catalog_describes_paid_routes_without_shipping_paid_implementations(self) -> None:
        public_truth = json.loads((ROOT / "docs/public_overview/commercial-truth.json").read_text(encoding="utf-8"))
        self.assertIn("/memory/agent-packet/shared", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/agent-proof-timeline/shared", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/telemetry/utility/analytics", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/telemetry/utility/policy/evaluate", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/retrieval/receipts/governance", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/trust/defense/operations", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/agent-fit/selection/activation", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/portable-continuation/governance", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/skills/foundry/evolution/governance", public_truth["paid_route_contract"]["routes"])
        self.assertIn("/memory/continuity-zero/governance", public_truth["paid_route_contract"]["routes"])
        for relative in (
            "services/gateway-go/entitlement_policy.go",
            "services/gateway-go/frontier_t3_utility_entitled.go",
            "services/gateway-go/frontier_t3_utility_entitled_test.go",
            "services/gateway-go/frontier_t4_retrieval_entitled.go",
            "services/gateway-go/frontier_t4_retrieval_entitled_test.go",
            "services/gateway-go/frontier_t6_agent_fit_entitled.go",
            "services/gateway-go/frontier_t6_agent_fit_entitled_test.go",
            "services/gateway-go/frontier_t7_portable_continuation_entitled.go",
            "services/gateway-go/frontier_t7_portable_continuation_entitled_test.go",
            "services/gateway-go/frontier_t8_skill_evolution_entitled.go",
            "services/gateway-go/frontier_t8_skill_evolution_entitled_test.go",
            "services/gateway-go/frontier_t9_continuity_zero_entitled.go",
            "services/gateway-go/frontier_t9_continuity_zero_entitled_test.go",
            "docs/evals/v3.21-frontier-t4-paid-activation.json",
            "docs/evals/v3.23-frontier-t6-paid-activation.json",
            "docs/evals/v3.24-frontier-t7-paid-activation.json",
            "docs/evals/v3.25-frontier-t8-paid-activation.json",
            "docs/evals/v3.26-frontier-t9-paid-activation.json",
        ):
            self.assertFalse((ROOT / relative).exists(), relative)

    def test_ga_posture_is_public_without_mutating_runtime_entitlement_contract(self) -> None:
        premium = (ROOT / "docs/public_overview/premium.html").read_text(encoding="utf-8")
        self.assertNotIn("Controlled activation preview", premium)
        typescript = (ROOT / "contextlattice-dashboard/lib/billing/commercial.generated.ts").read_text(
            encoding="utf-8"
        )
        self.assertNotIn('"release_availability"', typescript)

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

    def test_audit_rejects_mixed_frontier_release_posture(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-mixed-frontier-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            contract_path = fixture / "config/commercial_truth.v1.json"
            contract = json.loads(contract_path.read_text(encoding="utf-8"))
            contract["release_availability"]["frontier_shared_objective_graph"] = {
                "availability": "controlled_activation_preview",
                "release_gate": "IN_PROGRESS",
                "release_decision": "UNPROVEN",
                "production_proven": False,
            }
            contract_path.write_text(json.dumps(contract, indent=2) + "\n", encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stdout)
            self.assertIn("frontier_release_truth_mismatch", {row["code"] for row in payload["findings"]})

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
        self.assertNotIn("GO_V4_MACHINE_BINDING", gateway_source)
        self.assertNotIn("runtimeLicenseVerifier", gateway_source)

    def test_audit_rejects_strict_runtime_protected_route_drift(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-strict-routes-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            strict = fixture / "config/env/strict_runtime.env"
            lines = strict.read_text(encoding="utf-8").splitlines()
            prefix = "GO_V4_ENTITLEMENT_PROTECTED_PATHS="
            for index, line in enumerate(lines):
                if line.startswith(prefix):
                    lines[index] = prefix + "/v1/inference/route"
                    break
            else:
                lines.append(prefix + "/v1/inference/route")
            strict.write_text("\n".join(lines) + "\n", encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stdout)
            self.assertIn("protected_route_env_drift", {row["code"] for row in payload["findings"]})

    def test_audit_rejects_stale_release_publish_input(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-launch-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            launch = fixture / "launch_service/config/contextlattice.launch.json"
            launch.write_text(
                launch.read_text(encoding="utf-8").replace("v3.26.0", "v9.9.9", 1),
                encoding="utf-8",
            )
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            payload = json.loads(result.stdout)
            self.assertIn("release_publish_version_drift", {row["code"] for row in payload["findings"]})

    def test_audit_rejects_distribution_specific_release_publish_input(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-launch-lane-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            launch = fixture / "launch_service/config/contextlattice.launch.json"
            payload = json.loads(launch.read_text(encoding="utf-8"))
            payload["release_lane"] = "public-paid"
            payload["channels"][0]["submission_path"] = "Tag v3.19.0-public-paid"
            launch.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            codes = {row["code"] for row in json.loads(result.stdout)["findings"]}
            self.assertIn("release_publish_lane_drift", codes)
            self.assertIn("release_publish_version_drift", codes)

    def test_audit_rejects_schedule_fields_when_launch_is_unscheduled(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-launch-schedule-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            launch = fixture / "launch_service/config/contextlattice.launch.json"
            payload = json.loads(launch.read_text(encoding="utf-8"))
            payload["channels"][0]["schedule_mt"] = "2026-02-23 00:00"
            launch.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(
                "release_publish_schedule_drift",
                {row["code"] for row in json.loads(result.stdout)["findings"]},
            )

    def test_audit_rejects_partial_legacy_schedule_when_marked_scheduled(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-launch-partial-schedule-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            launch = fixture / "launch_service/config/contextlattice.launch.json"
            payload = json.loads(launch.read_text(encoding="utf-8"))
            payload["schedule_state"] = "scheduled"
            payload["channels"][0]["schedule_mt"] = "2026-07-20 09:00"
            launch.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(
                "release_publish_schedule_drift",
                {row["code"] for row in json.loads(result.stdout)["findings"]},
            )

    def test_audit_accepts_canonical_scheduled_channel(self) -> None:
        with tempfile.TemporaryDirectory(prefix="commercial-truth-launch-canonical-schedule-") as tmp:
            fixture = Path(tmp)
            copy_fixture(fixture)
            launch = fixture / "launch_service/config/contextlattice.launch.json"
            payload = json.loads(launch.read_text(encoding="utf-8"))
            payload["schedule_state"] = "scheduled"
            payload["channels"][0]["schedule"] = "2026-07-20T15:00:00Z"
            launch.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
            result = run(str(fixture / "scripts/agent/audit-commercial-truth"), "--root", str(fixture), root=fixture)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_audit_rejects_noncanonical_stable_tag_suffixes(self) -> None:
        variants = (
            "v3.19.0+public-paid",
            "v3.19.0_origin",
            "v3.19.0/public",
            "v3.19.0.public",
        )
        for variant in variants:
            with self.subTest(variant=variant), tempfile.TemporaryDirectory(prefix="commercial-truth-tag-suffix-") as tmp:
                fixture = Path(tmp)
                copy_fixture(fixture)
                launch = fixture / "launch_service/config/contextlattice.launch.json"
                payload = json.loads(launch.read_text(encoding="utf-8"))
                payload["channels"][0]["submission_path"] = f"Publish tag {variant}"
                launch.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
                result = run(
                    str(fixture / "scripts/agent/audit-commercial-truth"),
                    "--root",
                    str(fixture),
                    root=fixture,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(
                    "release_publish_version_drift",
                    {row["code"] for row in json.loads(result.stdout)["findings"]},
                )


if __name__ == "__main__":
    unittest.main()
