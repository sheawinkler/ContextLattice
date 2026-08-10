from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
from importlib.machinery import SourceFileLoader
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "agent" / "context-pack-quality-benchmark"


def load_module():
    name = "context_pack_quality_benchmark_test_module"
    loader = SourceFileLoader(name, str(SCRIPT))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load context-pack-quality-benchmark")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def frozen_artifact(module):
    cases = []
    for split, count in (("train", 2), ("holdout", 4)):
        for index in range(count):
            case_id = f"{split}-{index}"
            cases.append(
                {
                    "id": case_id,
                    "query": f"verified topic evidence {split} {index}",
                    "project": "project-alpha" if index % 2 == 0 else "project-beta",
                    "topic_path": f"runbooks/{'quality' if index % 2 == 0 else 'release'}",
                    "limit": 10,
                    "expected_files": [f"notes/{split}-{index}.md"],
                    "expected_substrings": ["verified topic evidence"],
                    "split": split,
                    "source_updated_at": f"2026-08-0{index + 1}T00:00:00Z",
                    "agent_id": f"agent-{index}",
                    "session_id": f"session-{index}",
                    "source_family": "memory_store",
                    "lifecycle": "durable",
                    "storage_tier": "hot",
                    "horizon_days": index + 1,
                    "query_intent": "research",
                    "difficulty": "medium",
                }
            )
    snapshot_digest = "sha256:" + "1" * 64
    source_stats = {
        "scanned_count": 12,
        "population_count": 12,
        "sample_count": len(cases),
        "index_mode": "current_state_bottom_k",
        "index_integrity": True,
        "bounded": True,
        "context_cancelled": False,
    }
    artifact = {
        "schema_id": module.SAVED_RECALL_SCHEMA_ID,
        "version": 3,
        "updatedAt": "2026-08-09T00:00:00Z",
        "source": "current_state_bottom_k",
        "case_set_digest": module.digest(cases),
        "snapshot": {
            "schema_id": module.SAVED_RECALL_SNAPSHOT_SCHEMA_ID,
            "captured_at": "2026-08-09T00:00:00Z",
            "source": "current_state_bottom_k",
            "selected_case_count": len(cases),
            "digest": snapshot_digest,
            "source_stats": copy.deepcopy(source_stats),
            "population": {"count": 12},
            "sample": {"count": len(cases)},
            "diversity": {"valid": True, "minimums": {"project": 2}},
        },
        "custody": {
            "schema_id": module.SAVED_RECALL_CUSTODY_SCHEMA_ID,
            "owner": "gateway-go",
            "mode": "frozen_live_index",
            "synthetic": False,
            "source_snapshot_digest": snapshot_digest,
            "case_set_digest": module.digest(cases),
            "derivation": "file_backed_memory_summary_with_filename_redaction",
            "oracle_leakage": "filename_removed_from_query; summary-derived labels retained",
            "population_count": 12,
            "sample_count": len(cases),
            "source_stats": copy.deepcopy(source_stats),
            "diversity_valid": True,
        },
        "split_counts": {"train": 2, "holdout": 4},
        "synthetic": False,
        "source_stats": source_stats,
        "cases": cases,
    }
    return artifact


class FakeTransport:
    def __init__(
        self,
        module,
        artifact,
        scores=None,
        mismatch=False,
        missing_quality=False,
        saved_passed=True,
        boundary_clipped=False,
        runtime_boot_nonce="boot-stable",
        runtime_source_bound=True,
    ):
        self.module = module
        self.artifact = artifact
        self.scores = scores or [90, 91, 92, 93]
        self.mismatch = mismatch
        self.missing_quality = missing_quality
        self.saved_passed = saved_passed
        self.boundary_clipped = boundary_clipped
        self.runtime_boot_nonce = runtime_boot_nonce
        self.runtime_source_bound = runtime_source_bound
        self.calls = []

    def request(self, method, path, payload, timeout_seconds):
        self.calls.append((method, path, copy.deepcopy(payload), timeout_seconds))
        if path == self.module.HEALTH_ROUTE:
            return self.module.TransportResponse(
                200,
                {
                    "ok": True,
                    "build": {
                        "schema_id": self.module.BUILD_IDENTITY_SCHEMA_ID,
                        "version": "4.0.11-test",
                        "channel": "test",
                        "source_bound": self.runtime_source_bound,
                        "source_commit": "1" * 40,
                        "source_tree": "2" * 40,
                        "boot_nonce": self.runtime_boot_nonce,
                    },
                    "memoryStore": {
                        "ready": True,
                        "phase": "ready",
                        "writer_policy": "owner_only_writer.v2",
                        "store_ref": "store_test",
                        "processed_entries": 12,
                        "batch_count": 1,
                    },
                },
                1.0,
            )
        if path == self.module.SAVED_RECALL_ROUTE:
            return self.module.TransportResponse(
                200,
                {
                    "ok": True,
                    "passed": self.saved_passed,
                    "metrics": {
                        "k": 5,
                        "casesTotal": 4,
                        "casesEvaluated": 4,
                        "recallAtK": 0.75,
                        "mrr": 0.625,
                        "numericExactness": 1.0,
                        "citationCoverage": 0.8,
                        "noHitRate": 0.0,
                        "lowConfidenceRate": 0.0,
                        "sourceDiversity": 2.0,
                        "avgLatencyMs": 12.0,
                        "p95LatencyMs": 18.0,
                        "directPassed": True,
                        "graphEvaluatedCases": 1,
                        "graphExplicitCases": 1,
                        "graphExpectedHits": 1,
                        "graphAddedExpectedHits": 1,
                        "graphHelpedCases": 1,
                        "graphLift": 0.25,
                        "graphPassed": True,
                        "graphRequired": True,
                        "graphEfficacyStatus": "passed",
                    },
                    "savedCaseSet": {"case_set_digest": self.artifact["case_set_digest"], "evaluation_split": "holdout"},
                },
                1.0,
            )
        case = next(item for item in self.artifact["cases"] if item["split"] == "holdout" and item["query"] == payload["query"])
        index = int(case["id"].split("-")[-1])
        query_hash = hashlib.sha256(payload["query"].encode("utf-8")).hexdigest()[:16]
        score = self.scores[index]
        quality = {
            "schema_id": self.module.QUALITY_SCHEMA_ID,
            "version": 1,
            "sample_id": f"cpq_{index:024d}",
            "query_hash": query_hash,
            "project": payload["project"],
            "quality_score": score,
            "ranked_evidence_count": 4,
            "high_impact_evidence_count": 2,
            "omitted_high_value_count": 0,
            "returned_source_count": 2,
            "warning_count": 0,
            "tokenizer_exact": True,
            "token_budget_active": True,
            "source_coverage_complete": True,
            "graph_context_used": False,
            "exact_prompt_tokens_saved": 100,
        }
        compiler = {
            "schema_id": "contextlattice_context_compiler.v1",
            "complete": not self.boundary_clipped,
            "boundary_compacted": self.boundary_clipped,
        }
        context_pack = {
            "query": payload["query"],
            "project": payload["project"],
            "topic_path": payload["topic_path"],
            "context_compiler": compiler,
            "contextPackQuality": quality,
        }
        if self.missing_quality:
            context_pack.pop("contextPackQuality")
            quality = {}
        response_query = "mismatched response" if self.mismatch else payload["query"]
        return self.module.TransportResponse(
            200,
            {
                "ok": True,
                "query": response_query,
                "context_pack": context_pack,
                "context_compiler": compiler,
                "context_pack_quality": quality,
                "format_contract": {
                    "schema_id": "context_pack_response.v1",
                    "contract_valid": True,
                    "truncated": self.boundary_clipped,
                    "actual_json_bytes": 1200,
                    "max_total_json_bytes": 120000,
                },
                "token_impact": {
                    "schema_id": "contextlattice_token_impact.v1",
                    "baseline_tokens_estimate": 300,
                    "packed_tokens_estimate": 200,
                    "compiled_prompt_tokens_estimate": 180,
                    "saved_tokens_estimate": 100,
                    "net_token_delta": 100,
                    "transport_tokens_exact": 200,
                    "transport_inclusive": True,
                    "tokenizer_exact": True,
                    "tokenizer_encoding": "cl100k_base",
                },
            },
            1.0,
        )


class ContextPackQualityBenchmarkTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.module = load_module()

    def test_artifact_validation_requires_complete_non_synthetic_custody(self):
        artifact = frozen_artifact(self.module)
        validation = self.module.validate_frozen_v3_artifact(artifact, b"artifact")
        self.assertTrue(validation["valid"], validation["issues"])
        self.assertEqual(validation["holdout_count"], 4)
        self.assertEqual(validation["digests"]["case_set_digest"], artifact["case_set_digest"])

        contaminated = copy.deepcopy(artifact)
        contaminated["custody"]["synthetic"] = True
        contaminated["case_set_digest"] = self.module.digest(contaminated["cases"])
        contaminated["custody"]["case_set_digest"] = contaminated["case_set_digest"]
        rejected = self.module.validate_frozen_v3_artifact(contaminated)
        self.assertFalse(rejected["valid"])
        self.assertIn("synthetic_custody", {row["code"] for row in rejected["issues"]})

    def test_artifact_validation_requires_current_state_source_stats_and_agreement(self):
        artifact = frozen_artifact(self.module)
        for source in ("recent_history_bottom_k", "memory_store_unavailable", "project_current_state_empty"):
            rejected = copy.deepcopy(artifact)
            rejected["source"] = source
            rejected["snapshot"]["source"] = source
            rejected["source_stats"]["index_mode"] = source
            rejected["snapshot"]["source_stats"]["index_mode"] = source
            rejected["custody"]["source_stats"]["index_mode"] = source
            validation = self.module.validate_frozen_v3_artifact(rejected)
            self.assertFalse(validation["valid"], source)
            self.assertIn("unsupported_index_source", {row["code"] for row in validation["issues"]})

        mismatched = copy.deepcopy(artifact)
        mismatched["snapshot"]["source"] = "project_current_state_bottom_k"
        mismatch_validation = self.module.validate_frozen_v3_artifact(mismatched)
        self.assertFalse(mismatch_validation["valid"])
        self.assertIn("snapshot_source_mismatch", {row["code"] for row in mismatch_validation["issues"]})

        invalid_stats = copy.deepcopy(artifact)
        invalid_stats["source_stats"]["index_integrity"] = False
        invalid_stats["snapshot"]["source_stats"]["bounded"] = False
        invalid_stats["custody"]["source_stats"]["context_cancelled"] = True
        stats_validation = self.module.validate_frozen_v3_artifact(invalid_stats)
        self.assertFalse(stats_validation["valid"])
        issue_codes = {row["code"] for row in stats_validation["issues"]}
        self.assertIn("artifact_source_index_integrity_invalid", issue_codes)
        self.assertIn("snapshot_source_not_bounded", issue_codes)
        self.assertIn("custody_source_context_cancelled", issue_codes)

    def test_aggregate_is_deterministic_and_does_not_recompute_server_score(self):
        aggregate = self.module.aggregate_quality_scores([93, 90, 91, 92], 4)
        self.assertEqual(aggregate["mean"], 91.5)
        self.assertEqual(aggregate["median"], 91.5)
        self.assertEqual(aggregate["p10"], 90)
        self.assertEqual(aggregate["pass_fraction"], 1.0)
        self.assertEqual(aggregate["percentile_method"], "nearest_rank")

    def test_benchmark_calls_context_pack_once_per_holdout_and_separates_saved_recall(self):
        artifact = frozen_artifact(self.module)
        transport = FakeTransport(self.module, artifact)
        report = self.module.run_benchmark(artifact, transport, artifact_bytes=b"artifact", concurrency=2, timeout_seconds=240)
        context_calls = [call for call in transport.calls if call[1] == self.module.CONTEXT_PACK_ROUTE]
        saved_calls = [call for call in transport.calls if call[1] == self.module.SAVED_RECALL_ROUTE]
        health_calls = [call for call in transport.calls if call[1] == self.module.HEALTH_ROUTE]
        self.assertEqual(len(context_calls), 4)
        self.assertEqual(len(saved_calls), 1)
        self.assertEqual(len(health_calls), 2)
        self.assertTrue(all(call[0] == "POST" for call in context_calls))
        self.assertEqual(saved_calls[0][0], "POST")
        self.assertEqual(report["aggregate"]["mean"], 91.5)
        self.assertTrue(report["promotion"]["promotion_eligible"], report["promotion"])
        self.assertEqual(
            report["promotion"]["score_fields"],
            ["aggregate.mean", "aggregate.median", "aggregate.p10"],
        )
        self.assertEqual(report["saved_recall"]["metrics"]["k"], 5)
        self.assertEqual(report["saved_recall"]["metrics"]["recallAtK"], 0.75)
        self.assertEqual(report["dataset"]["case_set_digest"], artifact["case_set_digest"])
        self.assertTrue(all(row["quality_signals"]["schema_id"] == self.module.QUALITY_SCHEMA_ID for row in report["cases"]))
        self.assertTrue(all(row["token_impact"]["available"] for row in report["cases"]))
        self.assertEqual(report["latency"]["p50_ms"], 1.0)
        self.assertEqual(report["latency"]["p95_ms"], 1.0)
        self.assertFalse(report["cost"]["available"])
        self.assertIsNone(report["cost"]["total"])
        self.assertFalse(report["tool_calls"]["available"])
        self.assertTrue(report["runtime_identity"]["stable_across_run"])
        self.assertEqual(report["runtime_identity"]["before"]["identity"]["source_commit"], "1" * 40)

    def test_runtime_identity_is_required_and_stable_across_run(self):
        artifact = frozen_artifact(self.module)
        unbound = FakeTransport(self.module, artifact, runtime_source_bound=False)
        unbound_report = self.module.run_benchmark(artifact, unbound, artifact_bytes=b"artifact", concurrency=2)
        self.assertFalse(unbound_report["promotion"]["promotion_eligible"])
        self.assertEqual(unbound_report["status"], "runtime_identity_rejected")
        self.assertIn("runtime_source_identity_unbound", unbound_report["promotion"]["blocked_reasons"])
        self.assertEqual(len([call for call in unbound.calls if call[1] == self.module.CONTEXT_PACK_ROUTE]), 0)

        changed = FakeTransport(self.module, artifact)
        original_request = changed.request
        health_count = 0

        def request_with_restart(method, path, payload, timeout_seconds):
            nonlocal health_count
            if path == self.module.HEALTH_ROUTE:
                health_count += 1
                changed.runtime_boot_nonce = "boot-before" if health_count == 1 else "boot-after"
            return original_request(method, path, payload, timeout_seconds)

        changed.request = request_with_restart
        changed_report = self.module.run_benchmark(artifact, changed, artifact_bytes=b"artifact", concurrency=2)
        self.assertFalse(changed_report["promotion"]["promotion_eligible"])
        self.assertFalse(changed_report["runtime_identity"]["stable_across_run"])
        self.assertIn("runtime_identity_changed_during_run", changed_report["promotion"]["blocked_reasons"])

    def test_saved_recall_failed_gate_blocks_even_when_metrics_are_available(self):
        artifact = frozen_artifact(self.module)
        transport = FakeTransport(self.module, artifact, saved_passed=False)
        report = self.module.run_benchmark(artifact, transport, artifact_bytes=b"artifact", concurrency=2)
        self.assertTrue(report["saved_recall"]["available"])
        self.assertFalse(report["saved_recall"]["passed"])
        self.assertFalse(report["promotion"]["promotion_eligible"])
        self.assertIn("saved_recall_gate_failed", report["promotion"]["blocked_reasons"])

    def test_refuses_partial_mismatch_and_score_below_threshold_without_cherry_picking(self):
        artifact = frozen_artifact(self.module)
        transport = FakeTransport(self.module, artifact, scores=[100, 90, 80, 80])
        report = self.module.run_benchmark(artifact, transport, artifact_bytes=b"artifact", concurrency=2)
        self.assertFalse(report["promotion"]["promotion_eligible"])
        self.assertIn("quality_p10_below_90", report["promotion"]["blocked_reasons"])
        self.assertEqual(report["aggregate"]["denominator"], 4)
        self.assertEqual(report["aggregate"]["evaluated"], 4)

        serialized = json.dumps(report, sort_keys=True)
        self.assertNotIn("verified topic evidence", serialized)
        self.assertNotIn("project-alpha", serialized)

        mismatch_transport = FakeTransport(self.module, artifact, mismatch=True)
        mismatch_report = self.module.run_benchmark(artifact, mismatch_transport, artifact_bytes=b"artifact", concurrency=2)
        self.assertFalse(mismatch_report["promotion"]["promotion_eligible"])
        self.assertIn("case_or_reconciliation_failures", mismatch_report["promotion"]["blocked_reasons"])
        self.assertTrue(any(row["failure"]["code"] == "request_result_mismatch" for row in mismatch_report["cases"]))

        missing_transport = FakeTransport(self.module, artifact, missing_quality=True)
        missing_report = self.module.run_benchmark(artifact, missing_transport, artifact_bytes=b"artifact", concurrency=2)
        self.assertFalse(missing_report["promotion"]["promotion_eligible"])
        self.assertTrue(any(row["failure"]["code"] == "quality_row_missing" for row in missing_report["cases"]))

        clipped_transport = FakeTransport(self.module, artifact, boundary_clipped=True)
        clipped_report = self.module.run_benchmark(artifact, clipped_transport, artifact_bytes=b"artifact", concurrency=2)
        self.assertFalse(clipped_report["promotion"]["promotion_eligible"])
        self.assertTrue(any(row["failure"]["code"] == "boundary_compaction_detected" for row in clipped_report["cases"]))

        no_denominator = copy.deepcopy(artifact)
        no_denominator.pop("split_counts")
        denominator_validation = self.module.validate_frozen_v3_artifact(no_denominator)
        self.assertFalse(denominator_validation["valid"])
        self.assertIn("missing_split_denominator", {row["code"] for row in denominator_validation["issues"]})

    def test_timeout_and_artifact_defaults_are_bounded_without_short_kill_guess(self):
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(self.module.configured_timeout_seconds(), 200.0)
        with patch.dict(os.environ, {"CONTEXTLATTICE_CLIENT_TIMEOUT_SECS": "17.5"}, clear=True):
            self.assertEqual(self.module.configured_timeout_seconds(), 17.5)
        with patch.dict(os.environ, {"CONTEXTLATTICE_CLIENT_TIMEOUT_SECS": "not-a-number"}, clear=True):
            self.assertEqual(self.module.configured_timeout_seconds(), 200.0)
        with patch.dict(os.environ, {"ORCH_RECALL_EVAL_CASES_PATH": "/tmp/frozen-v3.json"}, clear=True):
            self.assertEqual(self.module.default_artifact_path(), Path("/tmp/frozen-v3.json"))

    def test_content_words_about_fallback_are_not_case_provenance(self):
        artifact = frozen_artifact(self.module)
        artifact["cases"][0]["query"] = "how should the verified fallback path be checked"
        artifact["case_set_digest"] = self.module.digest(artifact["cases"])
        artifact["custody"]["case_set_digest"] = artifact["case_set_digest"]
        validation = self.module.validate_frozen_v3_artifact(artifact)
        self.assertTrue(validation["valid"], validation["issues"])

        tampered = copy.deepcopy(artifact)
        tampered["cases"][0]["query"] = "different query"
        tampered["custody"]["case_set_digest"] = tampered["case_set_digest"]
        validation = self.module.validate_frozen_v3_artifact(tampered)
        self.assertFalse(validation["valid"])
        self.assertIn("case_set_digest_mismatch", {row["code"] for row in validation["issues"]})

    def test_refuses_synthetic_compositional_fixture_as_promotion_evidence(self):
        fixture = {
            "schema_id": "recall_response.compositional_holdout_custody.v1",
            "contamination_state": "local_regression/contaminated",
        }
        self.assertEqual(fixture["contamination_state"], "local_regression/contaminated")
        # The v3 validator cannot reinterpret a different custody schema as a
        # live-index artifact; this proves the benchmark does not fall back to
        # the repository's synthetic compositional fixture.
        rejected = self.module.validate_frozen_v3_artifact({"schema_id": fixture["schema_id"], "version": 1, "cases": []})
        self.assertFalse(rejected["valid"])
        self.assertIn("schema_version_mismatch", {row["code"] for row in rejected["issues"]})


if __name__ == "__main__":
    unittest.main()
