from __future__ import annotations

import json
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


class ResearchBacklogCloseoutTests(unittest.TestCase):
    def read(self, relative: str) -> str:
        return (ROOT / relative).read_text(encoding="utf-8")

    def test_container_runtime_decision_separates_operation_from_benchmark_claim(self) -> None:
        decision = self.read("docs/runtime/container-runtime-decision.md")
        baseline = self.read("docs/runtime/decision-baseline.md")
        report = self.read("docs/runtime/container-ab-report.md")

        self.assertIn("docker --context orbstack", decision)
        self.assertIn("not a claim that OrbStack is universally faster", decision)
        self.assertIn("untrusted code", decision)
        self.assertIn("Historical benchmark input", baseline)
        self.assertIn("Colima-only run", report)

    def test_runtime_toolset_decision_matches_enforced_defaults(self) -> None:
        decision = self.read("docs/runtime/v4-runtime-toolset-decision.md")
        lite = self.read("docker-compose.lite.yml")
        full = self.read("docker-compose.yml")
        env = self.read(".env.example")

        self.assertIn('MINDSDB_ENABLED: "false"', lite)
        self.assertIn("ORCH_MINDSDB_FANOUT_ENABLED: ${ORCH_MINDSDB_FANOUT_ENABLED:-false}", full)
        self.assertIn("TASK_MODEL=qwen3.5:9b", env)
        self.assertIn("FASTEMBED_DEFAULT_MODEL=BAAI/bge-small-en-v1.5", env)
        self.assertIn("Modeled progress is advisory", decision)
        self.assertIn("deprecated `qwen2.5-coder`", decision)

    def test_provider_boundary_preserves_discovery_and_auth_separation(self) -> None:
        decision = self.read("docs/runtime/external-provider-auth-boundary.md")
        smoke = self.read("scripts/agent/audit-external-provider-cli-smoke")

        self.assertIn("network-free by default", decision)
        self.assertIn("explicit `--execute`", decision)
        self.assertIn("must not run provider login/logout", decision)
        self.assertIn('parser.add_argument("--execute", action="store_true"', smoke)
        self.assertIn('"--no-session"', smoke)
        self.assertIn('exec_args=("run", "--pure", "--format", "default"', smoke)

    def test_research_decisions_keep_remote_metadata_non_authoritative(self) -> None:
        decision = self.read("docs/runtime/research-backlog-decisions.md")
        contracts = self.read("config/agent_contracts/agent_output_contracts.json")
        capabilities = self.read("services/gateway-go/ops_endpoints.go")

        for contract_id in (
            "runner_capability.v1",
            "runner_result.v1",
            "agent_task_lease.v1",
            "runner_quality_sample.v1",
            "universal_agent_adapter_response.v1",
        ):
            self.assertIn(contract_id, contracts)
        self.assertIn("remote MCP-registry sync is intentionally not part", decision)
        self.assertIn('"queued", "claimed", "partial", "succeeded", "failed"', capabilities)

    def test_unsigned_macos_posture_cannot_be_misread_as_notarized(self) -> None:
        signing = self.read("docs/releases/macos-signing-notarization.md")
        readme = self.read("README.md")

        self.assertIn("unsigned technical preview", signing)
        self.assertIn("must not call that artifact signed, notarized", signing)
        self.assertIn("macOS technical preview: unsigned DMG", readme)

    def test_gateway_state_migration_documents_installed_single_file_upgrade(self) -> None:
        dockerfile = self.read("Dockerfile.gateway-go")
        full_compose = self.read("docker-compose.yml")
        lite_compose = self.read("docker-compose.lite.yml")
        migration = self.read("services/gateway-go/internal/gatewaystate/migration.go")
        runbook = self.read("docs/runtime/gateway-state-root.md")
        ledger = json.loads(self.read("docs/evals/v4.0.7-gateway-state-root.json"))

        self.assertIn("COPY services/gateway-go/internal ./internal", dockerfile)
        for compose in (full_compose, lite_compose):
            self.assertIn("GO_GATEWAY_SHUTDOWN_TIMEOUT_SECS: ${GO_GATEWAY_SHUTDOWN_TIMEOUT_SECS:-20}", compose)
            self.assertIn("stop_grace_period: 30s", compose)
        self.assertIn('LegacyKind            string             `json:"legacy_kind"`', migration)
        self.assertIn("--legacy-root /data/agent_sessions.json", runbook)
        self.assertIn("gateway-go shutdown complete", runbook)
        self.assertIn("single_regular_file_upgrade", ledger["holdout"]["migration_cases"])
        self.assertEqual(ledger["results"]["single_regular_file_apply_and_rollback"], "passed")

    def test_closeout_ledger_is_complete_and_excludes_growth_trackers(self) -> None:
        ledger = json.loads(self.read("docs/evals/v4.0.7-research-backlog-closeout.json"))
        tracker_ids = {row["tracker_id"] for row in ledger["decisions"]}

        self.assertEqual(ledger["schema_id"], "contextlattice_eval_ledger.v1")
        self.assertEqual(tracker_ids, {71, 72, 88, 210, 299, 362, 503})
        self.assertTrue(tracker_ids.isdisjoint({771, 772, 774, 775, 776, 782}))
        self.assertEqual(ledger["cost"]["external_provider_calls"], 0)
        self.assertEqual(ledger["cost"]["credential_mutations"], 0)


if __name__ == "__main__":
    unittest.main()
