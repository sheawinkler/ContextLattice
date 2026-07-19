from __future__ import annotations

import json
import os
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
            (gateway / "agent_packet_delta_test.go").write_text(
                """package releasegate

import (
    "os"
    "testing"
)

func TestFrontierT2AgentPacketProjectionLatencyGate(t *testing.T) {
    if os.Getenv("CONTEXTLATTICE_AGENT_PACKET_DELTA_PERFORMANCE_GATE") != "1" {
        t.Skip("isolated release gate only")
    }
}
""",
                encoding="utf-8",
            )
            (gateway / "frontier_t2_delta_eval_test.go").write_text(
                """package releasegate

import (
    "fmt"
    "os"
    "testing"
)

func TestFrontierT2DeltaPacketHoldout(t *testing.T) {
    if os.Getenv("CONTEXTLATTICE_AGENT_PACKET_DELTA_PERFORMANCE_GATE") != "1" {
        return
    }
    output := os.Getenv("FRONTIER_T2_ITEM2_EVAL_OUTPUT")
    strictLatency := os.Getenv("CONTEXTLATTICE_FRONTIER_STRICT_LATENCY_GATE") != "0"
    observed, batches, passing := 1, "[1,2,3]", 3
    if !strictLatency {
        observed, batches, passing = 30, "[30,31,32]", 0
    }
    payload := fmt.Sprintf(`{"schema_id":"frontier_t2_delta_packet_eval.v1","tested_commit":%q,"sample_count":1,"correct_count":1,"release_gates":{"corrupt_reconstruction_count":0,"unsafe_delta_on_invalid_base_count":0,"synchronous_projection_p95_ms_max":20,"synchronous_projection_p95_ms_observed":%d,"synchronous_projection_p95_estimator":"median_of_3_batch_p95","synchronous_projection_batch_p95_ms":%s,"synchronous_projection_passing_batches":%d,"synchronous_projection_latency_enforced":%t,"synchronous_projection_allocs_max":40000,"synchronous_projection_allocs_observed":39000}}`, os.Getenv("FRONTIER_T2_TESTED_COMMIT"), observed, batches, passing, strictLatency)
    if err := os.WriteFile(output, []byte(payload), 0o600); err != nil {
        t.Fatal(err)
    }
}
""",
                encoding="utf-8",
            )
            (gateway / "frontier_t2_proof_timeline_eval_test.go").write_text(
                """package releasegate

import (
    "fmt"
    "os"
    "testing"
)

func TestFrontierT2ProofTimelineHoldout(t *testing.T) {
    output := os.Getenv("FRONTIER_T2_PROOF_TIMELINE_EVIDENCE_PATH")
    strictLatency := os.Getenv("CONTEXTLATTICE_FRONTIER_STRICT_LATENCY_GATE") != "0"
    observed := 1
    if !strictLatency {
        observed = 30
    }
    payload := fmt.Sprintf(`{"schema_id":"frontier_t2_proof_timeline_eval.v1","tested_commit":%q,"sample_count":15,"correct_count":15,"release_gates":{"case_classification_accuracy":1,"eligible_exact_link_coverage":1,"ordering_fidelity":1,"cross_scope_rejection_rate":1,"redaction_failure_count":0,"silent_gap_count":0,"projection_p95_ms":%d,"projection_p95_ms_max":20,"projection_latency_gate_enforced":%t,"provider_calls":0,"external_network_calls":0,"authoritative_ledger_mutations":0}}`, os.Getenv("FRONTIER_T2_TESTED_COMMIT"), observed, strictLatency)
    if output == "" {
        return
    }
    if err := os.WriteFile(output, []byte(payload), 0o600); err != nil {
        t.Fatal(err)
    }
}
""",
                encoding="utf-8",
            )
            shared_retention = gateway / "frontier_t2_proof_timeline_entitled_test.go"

            def write_shared_retention_fixture(
                fsync: str,
                strict_batches: str = "[1,2,300]",
                strict_observed: int = 2,
                strict_passing: int = 2,
            ) -> None:
                shared_retention.write_text(
                    """package releasegate

import (
    "fmt"
    "os"
    "testing"
)

func TestFrontierT2SharedProofConcurrentRetainRead(t *testing.T) {}

func TestFrontierT2SharedProofRetentionLatencyHoldout(t *testing.T) {
    if os.Getenv("CONTEXTLATTICE_FRONTIER_T2_SHARED_PROOF_PERFORMANCE_GATE") != "1" {
        t.Skip("isolated release gate only")
    }
    output := os.Getenv("FRONTIER_T2_SHARED_PROOF_EVIDENCE_PATH")
    strictLatency := os.Getenv("CONTEXTLATTICE_FRONTIER_STRICT_LATENCY_GATE") != "0"
    observed := __STRICT_OBSERVED__
    batches := "__STRICT_BATCHES__"
    passing := __STRICT_PASSING__
    if !strictLatency {
        observed = 301
        batches = "[300,301,302]"
        passing = 0
    }
    payload := fmt.Sprintf(`{"schema_id":"frontier_t2_shared_proof_retention_eval.v1","tested_commit":%q,"sample_count":72,"success_count":72,"concurrent_read_count":72,"performance_batch_count":3,"samples_per_batch":24,"durability":{"fsync":__FSYNC__,"percentile_method":"nearest_rank_per_batch"},"release_gates":{"retention_p95_ms":%d,"retention_p95_ms_max":250,"retention_p95_estimator":"median_of_3_batch_p95","retention_batch_p95_ms":%s,"retention_passing_batches":%d,"retention_required_passes":2,"retention_latency_gate_enforced":%t,"provider_calls":0,"external_network_calls":0,"authoritative_ledger_mutations":0}}`, os.Getenv("FRONTIER_T2_TESTED_COMMIT"), observed, batches, passing, strictLatency)
    if err := os.WriteFile(output, []byte(payload), 0o600); err != nil {
        t.Fatal(err)
    }
}
"""
                    .replace("__FSYNC__", fsync)
                    .replace("__STRICT_BATCHES__", strict_batches)
                    .replace("__STRICT_OBSERVED__", str(strict_observed))
                    .replace("__STRICT_PASSING__", str(strict_passing)),
                    encoding="utf-8",
                )

            orchestrator = repo / "services/orchestrator-go"
            orchestrator.mkdir(parents=True)
            (orchestrator / "go.mod").write_text("module example.test/orchestrator\n\ngo 1.23\n", encoding="utf-8")
            (orchestrator / "main.go").write_text("package orchestrator\n", encoding="utf-8")
            shared = gateway / "shared.go"
            shared.write_text("package releasegate\n\nfunc Shared() { paidOnly() }\n", encoding="utf-8")
            broken = commit_all(repo, "broken public projection")
            headless_env = {
                "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                "TMPDIR": tmp,
            }

            failed = run(
                [str(fixture_audit), "--lane", "public", "--ref", broken, "--expected-commit", broken],
                repo,
                env=headless_env,
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
                env=headless_env,
            )
            self.assertEqual(passed.returncode, 0, passed.stdout + passed.stderr)
            payload = json.loads(proof.read_text(encoding="utf-8"))
            self.assertEqual(payload["schema_id"], "contextlattice_release_tree_proof.v1")
            self.assertEqual(payload["lane"], "public")
            self.assertEqual(payload["performance_mode"], "strict")
            self.assertEqual(payload["source_commit"], candidate)
            self.assertEqual(
                payload["source_tree"],
                subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], cwd=repo, text=True).strip(),
            )
            self.assertEqual(payload["gates"]["services_gateway_go_test"], "pass")
            self.assertEqual(payload["gates"]["public_leak_guard"], "pass")
            self.assertEqual(payload["gates"]["frontier_t2_item2_isolated_performance"], "pass")
            self.assertEqual(payload["gates"]["frontier_t2_item24_isolated_proof_timeline"], "pass")
            self.assertEqual(payload["gates"]["frontier_t2_item24_shared_retention"], "not_present")
            self.assertTrue(payload["required_blobs"])

            shared_runner_proof = Path(tmp) / "public-shared-runner-proof.json"
            shared_runner_env = {
                **headless_env,
                "CONTEXTLATTICE_RELEASE_PERFORMANCE_MODE": "shared_runner",
            }
            shared_runner_passed = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "public",
                    "--ref",
                    candidate,
                    "--expected-commit",
                    candidate,
                    "--proof-out",
                    str(shared_runner_proof),
                ],
                repo,
                env=shared_runner_env,
            )
            self.assertEqual(
                shared_runner_passed.returncode,
                0,
                shared_runner_passed.stdout + shared_runner_passed.stderr,
            )
            shared_runner_payload = json.loads(
                shared_runner_proof.read_text(encoding="utf-8")
            )
            self.assertEqual(shared_runner_payload["performance_mode"], "shared_runner")
            self.assertEqual(
                shared_runner_payload["gates"]["frontier_t2_item2_isolated_performance"],
                "correctness_pass_latency_observed",
            )
            self.assertEqual(
                shared_runner_payload["gates"]["frontier_t2_item24_isolated_proof_timeline"],
                "correctness_pass_latency_observed",
            )

            proof_timeline_gate = gateway / "frontier_t2_proof_timeline_eval_test.go"
            proof_timeline_gate_source = proof_timeline_gate.read_text(encoding="utf-8")
            proof_timeline_gate.unlink()
            missing_t2_gate = commit_all(repo, "public projection missing required T2 gate")
            missing_t2_result = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "public",
                    "--ref",
                    missing_t2_gate,
                    "--expected-commit",
                    missing_t2_gate,
                ],
                repo,
                env=headless_env,
            )
            self.assertNotEqual(missing_t2_result.returncode, 0, missing_t2_result.stdout + missing_t2_result.stderr)
            self.assertIn("frontier_t2_proof_timeline_eval_test.go", missing_t2_result.stderr)
            proof_timeline_gate.write_text(proof_timeline_gate_source, encoding="utf-8")
            commit_all(repo, "restore required T2 release gate")

            paid_files = {
                ".github/workflows/release-installers.yml": "name: fixture\n",
                "docs/evals/v3.18-frontier-t1-paid-activation.json": "{}\n",
                "docs/evals/v3.19-frontier-t2-paid-activation.json": "{}\n",
                "docs/evals/v3.21-frontier-t4-paid-activation.json": "{}\n",
                "docs/evals/v3.22-frontier-t5-paid-activation.json": "{}\n",
                "packaging/linux/ContextLattice-Install.sh": "#!/bin/sh\nexit 0\n",
                "packaging/windows/Install-ContextLattice.ps1": "exit 0\n",
                "scripts/agent/audit-paid-artifact-integrity": "#!/bin/sh\nexit 0\n",
                "services/gateway-go/frontier_t1_eval_test.go": "package releasegate\n",
                "services/gateway-go/frontier_t1_governance_entitled.go": "package releasegate\n",
                "services/gateway-go/frontier_t1_governance_entitled_test.go": "package releasegate\n",
                "services/gateway-go/frontier_t2_proof_timeline_entitled.go": "package releasegate\n",
                "services/gateway-go/frontier_t4_retrieval_entitled.go": "package releasegate\n",
                "services/gateway-go/frontier_t4_retrieval_entitled_test.go": "package releasegate\n",
                "services/gateway-go/frontier_t5_policy_lab_entitled.go": "package releasegate\n",
                "services/gateway-go/frontier_t5_policy_lab_entitled_test.go": "package releasegate\n",
            }
            for relative, content in paid_files.items():
                target = repo / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(content, encoding="utf-8")
            write_shared_retention_fixture("false")
            invalid_durability = commit_all(repo, "paid projection with weak durability proof")
            durability_failed = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "paid",
                    "--ref",
                    invalid_durability,
                    "--expected-commit",
                    invalid_durability,
                ],
                repo,
            )
            self.assertNotEqual(
                durability_failed.returncode,
                0,
                durability_failed.stdout + durability_failed.stderr,
            )
            self.assertIn(
                "durability evidence is invalid",
                durability_failed.stdout + durability_failed.stderr,
            )

            write_shared_retention_fixture("true", "[1,300,301]", 300, 1)
            insufficient_batches = commit_all(repo, "paid projection with insufficient passing batches")
            insufficient_batches_failed = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "paid",
                    "--ref",
                    insufficient_batches,
                    "--expected-commit",
                    insufficient_batches,
                ],
                repo,
            )
            self.assertNotEqual(
                insufficient_batches_failed.returncode,
                0,
                insufficient_batches_failed.stdout + insufficient_batches_failed.stderr,
            )
            self.assertIn(
                "250ms p95 gate failed",
                insufficient_batches_failed.stdout + insufficient_batches_failed.stderr,
            )

            write_shared_retention_fixture("true")
            paid_candidate = commit_all(repo, "paid projection with durable evidence")
            paid_proof = Path(tmp) / "paid-proof.json"
            paid_passed = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "paid",
                    "--ref",
                    paid_candidate,
                    "--expected-commit",
                    paid_candidate,
                    "--proof-out",
                    str(paid_proof),
                ],
                repo,
            )
            self.assertEqual(
                paid_passed.returncode, 0, paid_passed.stdout + paid_passed.stderr
            )
            paid_payload = json.loads(paid_proof.read_text(encoding="utf-8"))
            self.assertEqual(paid_payload["gates"]["frontier_t2_item2_isolated_performance"], "pass")
            self.assertEqual(paid_payload["gates"]["frontier_t2_item24_isolated_proof_timeline"], "pass")
            self.assertEqual(
                paid_payload["gates"]["frontier_t2_item24_shared_retention"],
                "pass",
            )

            paid_shared_runner_proof = Path(tmp) / "paid-shared-runner-proof.json"
            paid_shared_runner_passed = run(
                [
                    str(fixture_audit),
                    "--lane",
                    "paid",
                    "--ref",
                    paid_candidate,
                    "--expected-commit",
                    paid_candidate,
                    "--proof-out",
                    str(paid_shared_runner_proof),
                ],
                repo,
                env={
                    **headless_env,
                    "CONTEXTLATTICE_RELEASE_PERFORMANCE_MODE": "shared_runner",
                },
            )
            self.assertEqual(
                paid_shared_runner_passed.returncode,
                0,
                paid_shared_runner_passed.stdout + paid_shared_runner_passed.stderr,
            )
            paid_shared_runner_payload = json.loads(
                paid_shared_runner_proof.read_text(encoding="utf-8")
            )
            self.assertEqual(paid_shared_runner_payload["performance_mode"], "shared_runner")
            self.assertEqual(
                paid_shared_runner_payload["gates"]["frontier_t2_item24_shared_retention"],
                "correctness_pass_latency_observed",
            )

            mismatch = run(
                [str(fixture_audit), "--lane", "public", "--ref", candidate, "--expected-commit", "0" * 40],
                repo,
            )
            self.assertNotEqual(mismatch.returncode, 0)
            self.assertIn("candidate commit mismatch", mismatch.stderr)


if __name__ == "__main__":
    unittest.main()
