from __future__ import annotations

import importlib.util
import io
import json
import copy
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from importlib.machinery import SourceFileLoader
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "agent" / "recall-response-offline-eval"
BASELINE = ROOT / "docs" / "evals" / "fixtures" / "recall-response-advisory-baseline.v1.json"
HOLDOUT = ROOT / "docs" / "evals" / "fixtures" / "recall-response-advisory-holdout.v1.json"
LEDGER = ROOT / "docs" / "evals" / "recall-response-advisory.v1.json"


def load_module():
    name = "recall_response_offline_eval_test_module"
    loader = SourceFileLoader(name, str(SCRIPT))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load recall-response-offline-eval")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


class RecallResponseOfflineEvalTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_module()

    def test_public_synthetic_splits_are_versioned_and_disjoint(self) -> None:
        baseline = json.loads(BASELINE.read_text(encoding="utf-8"))
        holdout = json.loads(HOLDOUT.read_text(encoding="utf-8"))
        self.assertEqual(baseline["schema_id"], "recall_response_eval_fixture.v1")
        self.assertEqual(holdout["schema_id"], "recall_response_eval_fixture.v1")
        self.assertEqual(baseline["split"], "baseline")
        self.assertEqual(holdout["split"], "holdout")
        baseline_ids = {case["case_id"] for case in baseline["cases"]}
        holdout_ids = {case["case_id"] for case in holdout["cases"]}
        self.assertTrue(baseline_ids)
        self.assertTrue(holdout_ids)
        self.assertTrue(baseline_ids.isdisjoint(holdout_ids))
        self.assertTrue(holdout["independent_from_baseline"])
        self.assertFalse(holdout["frozen_before_response_tuning"])
        serialized = (BASELINE.read_text(encoding="utf-8") + HOLDOUT.read_text(encoding="utf-8")).lower()
        for forbidden in ("/users/", "/private/", "secret-token", "api_key", '"query"', '"path"', '"text"'):
            self.assertNotIn(forbidden, serialized)

    def test_evaluator_exposes_four_conditions_and_all_metric_families(self) -> None:
        result = self.module.evaluate_files(BASELINE, HOLDOUT, LEDGER)
        self.assertEqual(
            result["conditions"],
            ["no_recall", "retrieval_only", "static_recall_response", "continuous_cognition"],
        )
        required_metrics = {
            "contract_validity",
            "evidence_precision",
            "evidence_recall",
            "citation_proof_integrity",
            "correctness",
            "currentness",
            "conflict_handling",
            "abstention",
            "next_action",
            "decision_impact",
            "rank",
            "numeric",
            "diversity",
            "latency_ms",
            "tokens_in",
            "tokens_out",
            "tool_calls",
            "failures",
            "privacy",
            "determinism",
        }
        for condition in result["results"].values():
            self.assertTrue(required_metrics.issubset(condition["metrics"]))
        cognition = result["results"]["continuous_cognition"]
        self.assertEqual(cognition["availability"], "available")
        self.assertTrue(cognition["implemented"])
        self.assertEqual(cognition["contract_status"], "continuous_cognition.v1")
        self.assertEqual(cognition["metrics"]["contract_validity"], 1.0)
        self.assertEqual(cognition["metrics"]["privacy"], 1.0)
        operation_matrix = cognition["operation_contract_matrix"]
        self.assertEqual(
            operation_matrix["operations"],
            ["observe", "investigate", "status", "outcome", "evaluate", "rollback", "retire"],
        )
        self.assertTrue(operation_matrix["all_valid"])
        self.assertTrue(operation_matrix["all_deterministic"])
        self.assertFalse(operation_matrix["external_execution_claimed"])
        self.assertEqual(len(set(operation_matrix["operation_digests"].values())), 7)

    def test_static_response_is_bounded_and_retrieval_only_is_not_response_contract(self) -> None:
        result = self.module.evaluate_files(BASELINE, HOLDOUT, LEDGER)
        static = result["results"]["static_recall_response"]
        retrieval = result["results"]["retrieval_only"]
        self.assertEqual(static["metrics"]["contract_validity"], 1.0)
        self.assertEqual(static["metrics"]["privacy"], 1.0)
        self.assertIsNone(static["metrics"]["latency_ms"])
        self.assertIsNone(static["metrics"]["tokens_in"])
        self.assertIsNone(static["metrics"]["tokens_out"])
        self.assertEqual(static["metrics"]["tool_calls"], 0.0)
        self.assertEqual(static["metrics"]["failures"], 0.0)
        self.assertEqual(retrieval["metrics"]["contract_validity"], 0.0)
        self.assertEqual(retrieval["contract_status"], "not_a_response_contract")
        self.assertGreater(static["metrics"]["evidence_recall"], 0.0)
        self.assertIn(static["abstention_summary"]["posture"], {"advisory", "abstain"})
        self.assertNotIn("context_pack", json.dumps(static, sort_keys=True))
        self.assertNotIn("agent_packet", json.dumps(static, sort_keys=True))

        baseline = self.module.load_fixture(BASELINE)
        response = self.module._project_case(baseline["cases"][0], "static_recall_response")["response"]
        self.assertEqual(response["request_scope"]["condition"], "universal_template")
        self.assertEqual(response["answer"]["composition"]["proof_strategy"], "shared_bounded_v1")
        self.assertLessEqual(len(response["answer"]["proof_spine"]["proof_refs"]), 8)
        self.assertEqual(response["classification"]["facets"]["jobs"], ["look_up"])

        cognition = self.module._project_case(baseline["cases"][0], "continuous_cognition")["response"]
        self.assertEqual(cognition["next_action"], self.module._static_policy(baseline["cases"][0])["next_action"])
        self.assertEqual(cognition["silence"]["policy_version"], "continuous_cognition.offline_eval.v1")

    def test_retrieval_only_privacy_scans_rows_when_no_response_exists(self) -> None:
        baseline = self.module.load_fixture(BASELINE)
        case = copy.deepcopy(baseline["cases"][0])
        case["_split"] = baseline["split"]
        projection = self.module._project_case(case, "retrieval_only")
        self.assertTrue(projection["rows"])
        projection["rows"][0]["source_ref"] = "/Users/private/source"
        metrics = self.module._case_metrics(case, "retrieval_only", projection)
        self.assertEqual(metrics["privacy"], 0.0)

    def test_abstention_and_conflict_cases_are_measured_explicitly(self) -> None:
        result = self.module.evaluate_files(BASELINE, HOLDOUT, LEDGER)
        static = result["results"]["static_recall_response"]
        self.assertGreaterEqual(static["case_metrics"]["baseline-abstain"]["abstention"], 1.0)
        self.assertGreaterEqual(static["case_metrics"]["holdout-conflict"]["conflict_handling"], 1.0)
        no_recall = result["results"]["no_recall"]
        self.assertEqual(no_recall["metrics"]["abstention"], round(3 / 11, 6))
        self.assertEqual(no_recall["metrics"]["next_action"], 0.0)

    def test_expectation_perturbation_changes_scores_not_projections(self) -> None:
        baseline = self.module.load_fixture(BASELINE)
        holdout = self.module.load_fixture(HOLDOUT)
        original = self.module._evaluate_loaded(baseline, holdout)
        perturbed_baseline = copy.deepcopy(baseline)
        expected = perturbed_baseline["cases"][0]["expected"]
        expected["required_evidence_refs"] = []
        expected["acceptable_evidence_refs"] = []
        expected["status"] = "abstain"
        expected["abstain"] = True
        expected["next_action"] = "retrieve_or_verify"
        perturbed = self.module._evaluate_loaded(perturbed_baseline, holdout)
        original_static = original["results"]["static_recall_response"]
        perturbed_static = perturbed["results"]["static_recall_response"]
        self.assertEqual(original_static["projections"], perturbed_static["projections"])
        self.assertNotEqual(original_static["metrics"], perturbed_static["metrics"])
        original_cognition = original["results"]["continuous_cognition"]
        perturbed_cognition = perturbed["results"]["continuous_cognition"]
        self.assertEqual(original_cognition["projections"], perturbed_cognition["projections"])
        self.assertNotEqual(original_cognition["metrics"], perturbed_cognition["metrics"])

    def test_production_contract_and_digest_tamper_fail_closed(self) -> None:
        baseline = self.module.load_fixture(BASELINE)
        response = self.module._project_case(baseline["cases"][0], "static_recall_response")["response"]
        self.assertTrue(self.module.validate_recall_response(response))
        tampered = copy.deepcopy(response)
        tampered["response_digest"] = "sha256:" + ("0" * 64)
        self.assertFalse(self.module.validate_recall_response(tampered))
        registry_version = response["format_contract"]["registry_version"]
        registry = self.module.load_agent_contracts_registry()
        self.assertEqual(registry_version, registry["registry_version"])

        cognition = self.module._project_case(baseline["cases"][0], "continuous_cognition")["response"]
        self.assertTrue(self.module.validate_continuous_cognition_response(cognition))
        tampered_cognition = copy.deepcopy(cognition)
        tampered_cognition["cognition_digest"] = "sha256:" + ("0" * 64)
        self.assertFalse(self.module.validate_continuous_cognition_response(tampered_cognition))

        policy = self.module._static_policy(baseline["cases"][0])
        for operation in self.module.CONTINUOUS_COGNITION_OPERATIONS:
            response = self.module._build_continuous_cognition_response(baseline["cases"][0], policy, operation)
            self.assertEqual(response["operation"], operation)
            self.assertTrue(self.module.validate_continuous_cognition_response(response))

    def test_fixture_hash_overlap_and_ledger_drift_fail_closed(self) -> None:
        baseline = self.module.load_fixture(BASELINE)
        holdout = self.module.load_fixture(HOLDOUT)
        result = self.module.evaluate_files(BASELINE, HOLDOUT, None)
        ledger = json.loads(LEDGER.read_text(encoding="utf-8"))
        drifted = copy.deepcopy(ledger)
        drifted["evaluation"]["static_recall_response_metrics"]["rank"] = 0.25
        check = self.module.validate_ledger(ledger, baseline, holdout, result, result["fixture_metadata"]["fixture_hashes"])
        self.assertEqual(check["status"], "pass")
        drift_check = self.module.validate_ledger(drifted, baseline, holdout, result, result["fixture_metadata"]["fixture_hashes"])
        self.assertEqual(drift_check["status"], "fail")
        self.assertIn("evaluation.static_recall_response_metrics", drift_check["errors"])

        cognition_drift = copy.deepcopy(ledger)
        cognition_drift["evaluation"]["continuous_cognition_metrics"]["rank"] = 0.25
        cognition_drift_check = self.module.validate_ledger(cognition_drift, baseline, holdout, result, result["fixture_metadata"]["fixture_hashes"])
        self.assertEqual(cognition_drift_check["status"], "fail")
        self.assertIn("evaluation.continuous_cognition_metrics", cognition_drift_check["errors"])

        duplicate_holdout = copy.deepcopy(holdout)
        duplicate_holdout["cases"][0]["case_id"] = baseline["cases"][0]["case_id"]
        with self.assertRaises(ValueError):
            self.module._evaluate_loaded(baseline, duplicate_holdout)

        with tempfile.TemporaryDirectory() as temp_dir:
            tampered_baseline_path = Path(temp_dir) / "baseline.json"
            tampered_baseline = copy.deepcopy(baseline)
            tampered_baseline["cases"][0]["evidence"][0]["source_ref"] = "source_tampered_but_still_valid"
            tampered_baseline_path.write_text(json.dumps(tampered_baseline, sort_keys=True), encoding="utf-8")
            tampered_result = self.module.evaluate_files(tampered_baseline_path, HOLDOUT, LEDGER)
            self.assertEqual(tampered_result["ledger_check"]["status"], "fail")
            self.assertIn("fixture_hashes", tampered_result["ledger_check"]["errors"])

    def test_malformed_fixture_rows_fail_closed(self) -> None:
        baseline = json.loads(BASELINE.read_text(encoding="utf-8"))
        malformed_cases = []
        missing_expected = copy.deepcopy(baseline)
        missing_expected["cases"][0]["expected"] = []
        malformed_cases.append(missing_expected)
        invalid_rank = copy.deepcopy(baseline)
        invalid_rank["cases"][0]["evidence"][0]["rank"] = True
        malformed_cases.append(invalid_rank)
        unknown_evidence_field = copy.deepcopy(baseline)
        unknown_evidence_field["cases"][0]["evidence"][0]["raw"] = "opaque-looking-but-forbidden"
        malformed_cases.append(unknown_evidence_field)

        for index, malformed in enumerate(malformed_cases):
            with self.subTest(index=index), tempfile.TemporaryDirectory() as temp_dir:
                path = Path(temp_dir) / "malformed.json"
                path.write_text(json.dumps(malformed, sort_keys=True), encoding="utf-8")
                with self.assertRaises(ValueError):
                    self.module.load_fixture(path)

    def test_evaluation_and_ledger_are_deterministic_and_truthful(self) -> None:
        first = self.module.evaluate_files(BASELINE, HOLDOUT, LEDGER)
        second = self.module.evaluate_files(BASELINE, HOLDOUT, LEDGER)
        self.assertEqual(first, second)
        ledger = json.loads(LEDGER.read_text(encoding="utf-8"))
        self.assertEqual(ledger["schema_id"], "contextlattice_eval_ledger.v1")
        self.assertEqual(ledger["ledger_version"], "recall_response_advisory.v1")
        self.assertEqual(ledger["fixture_hashes"], first["fixture_metadata"]["fixture_hashes"])
        self.assertEqual(ledger["advisory_status"]["posture"], "abstention")
        utility = ledger["advisory_status"]["independent_utility"]
        self.assertEqual(utility["status"], "unproven")
        self.assertEqual(utility["independently_verified_observations"], 0)
        self.assertEqual(utility["causal_pairs"], 0)
        self.assertFalse(ledger["activation"]["enabled"])
        self.assertTrue(ledger["limitations"])

    def test_cli_check_is_offline_and_parseable(self) -> None:
        stdout = io.StringIO()
        argv = [
            str(SCRIPT),
            "--baseline",
            str(BASELINE),
            "--holdout",
            str(HOLDOUT),
            "--ledger",
            str(LEDGER),
            "--check",
            "--pretty",
        ]
        with patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(self.module.main(), 0)
        payload = json.loads(stdout.getvalue())
        self.assertEqual(payload["schema_id"], "recall_response_offline_eval.v1")
        self.assertTrue(payload["deterministic"])


if __name__ == "__main__":
    unittest.main()
