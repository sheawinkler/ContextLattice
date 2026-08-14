#!/usr/bin/env python3
"""Focused tests for the graph recall CLI boundary contract."""

from __future__ import annotations

import contextlib
import importlib.machinery
import importlib.util
import io
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT_DIR = ROOT / "scripts" / "agent"
EVALUATOR = SCRIPT_DIR / "recall-quality-eval"


def graph_custody(case_digest: str = "sha256:graph-case", manifest_digest: str = "sha256:graph-manifest") -> dict[str, object]:
    return {
        "schema_id": "saved_recall_graph_custody.v1",
        "owner": "gateway-go",
        "mode": "frozen_live_index",
        "synthetic": False,
        "sealed_holdout": True,
        "promotional_claims_allowed": False,
        "oracle_separated": True,
        "case_set_digest": case_digest,
        "manifest_digest": manifest_digest,
    }


def graph_refresh_response(case_digest: str = "sha256:graph-case", manifest_digest: str = "sha256:graph-manifest") -> dict[str, object]:
    health_custody = graph_custody(case_digest, manifest_digest)
    saved_custody = graph_custody(case_digest, manifest_digest)
    return {
        "ok": True,
        "schema_id": "saved_recall_graph_corpus.v1",
        "graph_corpus": True,
        "case_set_health": {
            "valid": True,
            "benchmark_eligible": True,
            "status": "healthy",
            "schema_id": "saved_recall_graph_corpus.v1",
            "version": 1,
            "case_count": 300,
            "development_count": 200,
            "holdout_count": 100,
            "topology_cases": {"references": 90, "same_session": 90, "same_topic": 90, "hard_negative": 30},
            "holdout_topology": {"references": 30, "same_session": 30, "same_topic": 30, "hard_negative": 10},
            "incremental_needed": 90,
            "holdout_incremental_needed": 30,
            "population": {"projects": 5, "agent_families": 5, "sessions": 20},
            "case_set_digest": case_digest,
            "manifest_digest": manifest_digest,
            "custody": health_custody,
            "issues": [],
        },
        "validation_receipt": {
            "schema_id": "saved_recall_graph_validation.v1",
            "version": 1,
            "authority": "gateway-go",
            "server_owned": True,
            "valid": True,
            "benchmark_eligible": True,
            "case_count": 300,
            "case_set_digest": case_digest,
            "manifest_digest": manifest_digest,
            "custody_case_set_digest": case_digest,
            "captured_at": "2026-08-11T00:00:00Z",
            "digest": "sha256:graph-validation",
        },
        "savedCaseSet": {
            "case_set_id": "opaque:recall_graph_corpus",
            "schema_id": "saved_recall_graph_corpus.v1",
            "version": 1,
            "count": 300,
            "development_count": 200,
            "holdout_count": 100,
            "case_set_digest": case_digest,
            "manifest_digest": manifest_digest,
            "topology_counts": {"references": 90, "same_session": 90, "same_topic": 90, "hard_negative": 30},
            "incremental_needed": 90,
            "custody": saved_custody,
        },
    }


def graph_evaluation_response(case_digest: str = "sha256:graph-case", manifest_digest: str = "sha256:graph-manifest") -> dict[str, object]:
    return {
        "ok": True,
        "passed": True,
        "mode": "graph",
        "promotion": {"promotion_eligible": True},
        "metrics": {
            "directPassed": True,
            "graphEfficacyStatus": "passed",
            "graphContribution": {"graph_hits": 90, "status": "passed"},
        },
        "savedCaseSet": {
            "case_set_id": "opaque:recall_graph_corpus",
            "schema_id": "saved_recall_graph_corpus.v1",
            "version": 1,
            "count": 300,
            "evaluation_count": 100,
            "evaluation_split": "holdout",
            "case_set_digest": case_digest,
            "manifest": {"digest": manifest_digest},
            "custody": graph_custody(case_digest, manifest_digest),
        },
    }


def load_module():
    sys.path.insert(0, str(SCRIPT_DIR))
    loader = importlib.machinery.SourceFileLoader("recall_quality_eval", str(EVALUATOR))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    if spec is None:
        raise RuntimeError("unable to load recall quality evaluator")
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class RecallQualityEvalTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_module()

    def test_timeout_is_explicit_or_unlimited(self) -> None:
        self.assertIsNone(self.module.positive_or_unlimited_timeout("0"))
        self.assertEqual(self.module.positive_or_unlimited_timeout("3.5"), 3.5)
        for value in ("-1", "nan", "inf"):
            with self.assertRaises(Exception):
                self.module.positive_or_unlimited_timeout(value)

    def test_topic_prefix_is_strict_and_single_use(self) -> None:
        self.assertEqual(self.module.strict_topic_prefix("runbooks/cache"), "runbooks/cache")
        for value in ("/runbooks/cache", "runbooks//cache", "runbooks/../cache", " runbooks/cache"):
            with self.assertRaises(Exception):
                self.module.strict_topic_prefix(value)
        with self.assertRaises(SystemExit):
            self.module.reject_duplicate_option(["--topic-prefix", "a", "--topic-prefix=b"], "topic-prefix")

    def test_topic_prefix_abbreviation_and_duplicate_forms_fail_before_request(self) -> None:
        calls = []

        def forbidden_request(method, path, payload, timeout):
            calls.append((method, path, payload, timeout))
            raise AssertionError("malformed topic-prefix arguments reached the request seam")

        old_request = self.module.request_json
        old_argv = sys.argv
        self.module.request_json = forbidden_request
        try:
            for argv in (
                [str(EVALUATOR), "--graph", "--topic-pref", "runbooks/cache", "--json"],
                [str(EVALUATOR), "--graph", "--topic-pref", "runbooks/cache", "--topic-prefix", "runbooks/cache", "--json"],
                [str(EVALUATOR), "--graph", "--topic-prefix", "runbooks/cache", "--topic-prefix=runbooks/other", "--json"],
            ):
                sys.argv = argv
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    self.module.main()
        finally:
            self.module.request_json = old_request
            sys.argv = old_argv
        self.assertEqual(calls, [])

    def test_omitted_topic_prefix_parses_for_graph_and_non_graph(self) -> None:
        calls = []

        def fake_request(method, path, payload, timeout):
            calls.append((method, path, payload, timeout))
            if payload.get("mode") == "graph":
                return graph_evaluation_response()
            return {"ok": False, "passed": False}

        old_request = self.module.request_json
        old_argv = sys.argv
        self.module.request_json = fake_request
        try:
            for argv, expected_code in (
                ([str(EVALUATOR), "--graph", "--json"], 0),
                ([str(EVALUATOR), "--json"], 1),
            ):
                sys.argv = argv
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(self.module.main(), expected_code)
        finally:
            self.module.request_json = old_request
            sys.argv = old_argv
        self.assertEqual(calls[0][2]["topic_prefix"], "")

    def test_graph_mode_propagates_closed_contract_without_deadline(self) -> None:
        calls = []

        def fake_request(method, path, payload, timeout):
            calls.append((method, path, payload, timeout))
            if path.endswith("refresh"):
                return graph_refresh_response()
            return graph_evaluation_response()

        old_request = self.module.request_json
        old_argv = sys.argv
        self.module.request_json = fake_request
        sys.argv = [str(EVALUATOR), "--graph", "--refresh-cases", "--project", "alpha", "--topic-prefix", "runbooks/cache", "--timeout", "0", "--json"]
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(self.module.main(), 0)
        finally:
            self.module.request_json = old_request
            sys.argv = old_argv
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(call[3] is None for call in calls))
        refresh = calls[0][2]
        evaluation = calls[1][2]
        self.assertEqual(refresh["topic_prefix"], "runbooks/cache")
        self.assertTrue(refresh["graph_corpus"])
        self.assertEqual(evaluation["mode"], "graph")
        self.assertTrue(evaluation["graph_corpus"])
        self.assertEqual(evaluation["split"], "holdout")
        self.assertEqual(evaluation["project"], "alpha")
        self.assertEqual(evaluation["topic_prefix"], "runbooks/cache")
        self.assertEqual(evaluation["graph_corpus_binding"]["case_set_id"], "opaque:recall_graph_corpus")
        self.assertEqual(evaluation["graph_corpus_binding"]["case_set_digest"], "sha256:graph-case")
        self.assertEqual(evaluation["graph_corpus_binding"]["manifest_digest"], "sha256:graph-manifest")

    def test_graph_refresh_rejects_incomplete_receipt_before_evaluation(self) -> None:
        calls = []

        def fake_request(method, path, payload, timeout):
            calls.append((method, path, payload, timeout))
            return {"ok": True}

        old_request = self.module.request_json
        old_argv = sys.argv
        self.module.request_json = fake_request
        sys.argv = [str(EVALUATOR), "--graph", "--refresh-cases", "--project", "alpha", "--json"]
        try:
            with contextlib.redirect_stdout(io.StringIO()) as stdout:
                self.assertEqual(self.module.main(), 2)
        finally:
            self.module.request_json = old_request
            sys.argv = old_argv
        self.assertEqual(len(calls), 1)
        self.assertIn('"error":', stdout.getvalue())

    def test_graph_refresh_carries_and_compares_canonical_identity(self) -> None:
        calls = []

        def fake_request(method, path, payload, timeout):
            calls.append((method, path, payload, timeout))
            if path.endswith("refresh"):
                return graph_refresh_response()
            return graph_evaluation_response(case_digest="sha256:other-case")

        old_request = self.module.request_json
        old_argv = sys.argv
        self.module.request_json = fake_request
        sys.argv = [str(EVALUATOR), "--graph", "--refresh-cases", "--project", "alpha", "--json"]
        try:
            with contextlib.redirect_stdout(io.StringIO()) as stdout:
                self.assertEqual(self.module.main(), 1)
        finally:
            self.module.request_json = old_request
            sys.argv = old_argv
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[1][2]["graph_corpus_binding"]["case_set_digest"], "sha256:graph-case")
        self.assertIn("differs between refreshed and evaluated", stdout.getvalue())

    def test_graph_success_requires_authoritative_promotion_and_status_gates(self) -> None:
        for mutation in (
            lambda response: response["promotion"].update(promotion_eligible=False),
            lambda response: response["metrics"]["graphContribution"].update(status="failed"),
            lambda response: response["metrics"]["graphContribution"].pop("status"),
        ):
            calls = []

            def fake_request(method, path, payload, timeout):
                calls.append((method, path, payload, timeout))
                response = graph_evaluation_response()
                mutation(response)
                return response

            old_request = self.module.request_json
            old_argv = sys.argv
            self.module.request_json = fake_request
            sys.argv = [str(EVALUATOR), "--graph", "--json"]
            try:
                with contextlib.redirect_stdout(io.StringIO()) as stdout:
                    self.assertEqual(self.module.main(), 1)
            finally:
                self.module.request_json = old_request
                sys.argv = old_argv
            self.assertEqual(len(calls), 1)
            self.assertIn("graph_validation_error", stdout.getvalue())


if __name__ == "__main__":
    unittest.main()
