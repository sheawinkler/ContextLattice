from __future__ import annotations

import importlib.machinery
import importlib.util
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import threading
import time
from typing import Any
import sys


SCRIPT = Path(__file__).with_name("recall-response-live-eval")
LOADER = importlib.machinery.SourceFileLoader("recall_response_live_eval", str(SCRIPT))
SPEC = importlib.util.spec_from_loader("recall_response_live_eval", LOADER)
assert SPEC and SPEC.loader
live_eval = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = live_eval
SPEC.loader.exec_module(live_eval)


COMMIT = "a" * 40
TREE = "b" * 40
SNAPSHOT = "sha256:" + "c" * 64
RECEIPT = "sha256:" + "d" * 64
TEMPORAL = "sha256:" + "e" * 64
SOURCE_REF = "sha256:" + "f" * 64
CONTENT = "sha256:" + "1" * 64


def make_artifact(*, graph_claimed: bool = False) -> tuple[dict[str, Any], bytes]:
    cases: list[dict[str, Any]] = [
        {
            "id": "case-development",
            "split": "train",
            "project": "contextlattice",
            "topic_path": "runbooks/response",
            "query": "deterministic response contract",
            "limit": 5,
            "expected_files": ["notes/development.md"],
            "source_updated_at": "2026-08-01T00:00:00Z",
        },
        {
            "id": "case-holdout",
            "split": "holdout",
            "project": "contextlattice",
            "topic_path": "runbooks/response",
            "query": "bounded response proof binding",
            "limit": 5,
            "expected_files": ["notes/holdout.md"],
            "expected_record_digests": [CONTENT],
            "source_updated_at": "2026-08-09T00:00:00Z",
        },
    ]
    if graph_claimed:
        cases[1]["case_kind"] = "graph_neighbor"
        cases[1]["graph_expected_files"] = ["notes/neighbor.md"]
        cases[1]["graph_expected_relations"] = ["same_topic"]
    source_stats = {
        "index_mode": "current_state_bottom_k",
        "bounded": True,
        "index_integrity": True,
        "context_cancelled": False,
    }
    snapshot = {
        "schema_id": live_eval.SNAPSHOT_SCHEMA_ID,
        "source": "current_state_bottom_k",
        "digest": SNAPSHOT,
        "selected_case_count": len(cases),
        "population": {"count": len(cases)},
        "sample": {"count": len(cases)},
        "diversity": {"valid": True},
        "source_stats": source_stats,
    }
    custody = {
        "schema_id": live_eval.CUSTODY_SCHEMA_ID,
        "owner": "gateway-go",
        "mode": "frozen_live_index",
        "synthetic": False,
        "source_snapshot_digest": SNAPSHOT,
        "case_set_digest": live_eval.digest(cases),
        "population_count": len(cases),
        "sample_count": len(cases),
        "diversity_valid": True,
        "oracle_leakage": "filename_removed_from_query; summary-derived labels retained",
        "source_stats": source_stats,
        "promotional_claims_allowed": True,
    }
    artifact: dict[str, Any] = {
        "schema_id": live_eval.CASE_SET_SCHEMA_ID,
        "version": 3,
        "source": "current_state_bottom_k",
        "synthetic": False,
        "case_set_digest": live_eval.digest(cases),
        "snapshot": snapshot,
        "custody": custody,
        "source_stats": source_stats,
        "split_counts": {"train": 1, "holdout": 1},
        "k": 5,
        "graphCasesEnabled": graph_claimed,
        "cases": cases,
    }
    raw = json.dumps(artifact, sort_keys=True, separators=(",", ":")).encode()
    return artifact, raw


def make_response(query: str, *, partial: bool = False, spoof_digest: bool = False, snapshot_digest: str = SNAPSHOT) -> dict[str, Any]:
    scope = {
        "query_digest": "sha256:" + __import__("hashlib").sha256(query.encode()).hexdigest(),
        "snapshot_digest": snapshot_digest,
        "receipt_digest": RECEIPT,
        "temporal_premise_digest": TEMPORAL,
        "as_of": "latest_available",
        "task_class": "decision",
        "retrieval_intent": "decision",
    }
    evidence = [{"ref_id": "evidence-ref", "source_ref": SOURCE_REF, "content_digest": CONTENT, "kind": "evidence", "role": "support", "status": "current", "confidence": 0.9}]
    gaps = [{"code": "pending_sources", "material": True, "reason": "one bounded source is pending", "required_for_action": True}] if partial else []
    response: dict[str, Any] = {
        "ok": True,
        "schema_id": live_eval.RESPONSE_SCHEMA_ID,
        "version": 1,
        "request_scope": scope,
        "classification": {"posture": "verify_before_action", "facets": {"jobs": ["decision"], "memory_objects": ["evidence"], "temporal_state": "current_or_unknown", "evidence_state": "degraded" if partial else "clean", "consequence": "medium"}},
        "answer": {"summary": "bounded proof", "answer_mode": "qualified_answer", "basis": ["ranked_evidence"], "claim_refs": ["evidence-ref"], "components": [], "proof_spine": {"primary_result": "evidence-ref", "as_of": "latest_available", "temporal_premise_digest": TEMPORAL, "proof_refs": ["evidence-ref"], "confidence_basis": ["bounded"], "conflict_refs": [], "gap_refs": [], "memory_boundary": "server_evidence_and_deterministic_inference_only", "next_move": "retrieve_or_verify", "receipt_refs": [], "disclosure": "opaque_refs_only", "coverage": []}, "composition": {"condition": "compositional_router", "ablation": "none", "primary_module": "v1_control", "ordered_modules": [], "proof_strategy": "shared_bounded_v1", "coverage_status": "unsatisfied" if partial else "satisfied", "fallback_reason": ""}},
        "state": {"status": "verify_before_action", "source_complete": not partial, "evidence_count": 1, "conflict_count": 0, "gap_count": len(gaps), "retrieval_mode": "balanced"},
        "evidence": evidence,
        "confidence": {"label": "medium", "score": 0.9, "basis": ["bounded"], "calibrated": False},
        "conflicts": [],
        "gaps": gaps,
        "inferences": [],
        "next_action": {"kind": "retrieve_or_verify" if partial else "inspect_proof", "label": "verify", "reason": "advisory", "requires_verification": True, "authority": "advisory_only", "execution_performed": False},
        "action_boundary": {"can_act": False, "requires_confirmation": True, "allowed": ["inspect_proof_refs"], "forbidden": ["external_mutation"], "reason": "advisory", "execution_performed": False},
        "disclosure": {"bounded": True, "raw_retrieval_included": False, "raw_prompt_included": False, "paths_included": False, "secrets_included": False, "inference_boundary": "deterministic", "omission_policy": "gaps remain disclosed"},
        "receipt_refs": [],
        "outcome": {"status": "not_attributable", "attributable": False, "receipt_id": "", "execution_performed": False},
        "writeback_required": True,
        "format_contract": {"schema_id": live_eval.RESPONSE_SCHEMA_ID, "contract_valid": True, "truncated": False},
    }
    response["response_id"] = live_eval._response_id(response)
    response["response_digest"] = live_eval._response_digest(response)
    if spoof_digest:
        response["response_digest"] = "sha256:" + "0" * 64
    return response


def make_health() -> dict[str, Any]:
    return {
        "ok": True,
        "build": {"schema_id": "contextlattice_build_identity.v1", "version": "test", "channel": "test", "source_bound": True, "source_commit": COMMIT, "source_tree": TREE, "boot_nonce": "boot-test"},
        "memoryStore": {"ready": True, "phase": "ready", "writer_policy": "owner_only_writer.v2", "store_ref": "memory://test", "processed_entries": 2, "batch_count": 1},
    }


class FakeRuntime:
    def __init__(self, *, partial: bool = False, spoof_digest: bool = False, snapshot_digest: str = SNAPSHOT, delay: float = 0.0, redirect: bool = False, headers: dict[str, str] | None = None) -> None:
        self.partial = partial
        self.spoof_digest = spoof_digest
        self.snapshot_digest = snapshot_digest
        self.delay = delay
        self.redirect = redirect
        self.headers = headers or {"X-ContextLattice-Cost-Microusd": "0", "X-ContextLattice-Provider-Calls": "0", "X-ContextLattice-Provider-Tokens": "0", "X-ContextLattice-External-Network-Calls": "0", "X-ContextLattice-Tool-Calls": "0"}
        self.requests = 0
        runtime = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args: Any) -> None:
                return

            def _write(self, payload: dict[str, Any], status: int = 200) -> None:
                raw = json.dumps(payload, separators=(",", ":")).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                for key, value in runtime.headers.items():
                    self.send_header(key, value)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                try:
                    self.wfile.write(raw)
                except BrokenPipeError:
                    # The evaluator already recorded the explicit per-case
                    # timeout; the delayed fake response is intentionally
                    # allowed to finish without polluting test output.
                    return

            def do_GET(self) -> None:  # noqa: N802
                if self.path == live_eval.HEALTH_ROUTE:
                    self._write(make_health())
                else:
                    self._write({}, 404)

            def do_POST(self) -> None:  # noqa: N802
                runtime.requests += 1
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length) or b"{}")
                if self.path not in {live_eval.RESPONSE_ROUTE, live_eval.TOOL_RESPONSE_ROUTE}:
                    self._write({}, 404)
                    return
                if runtime.redirect:
                    self.send_response(302)
                    self.send_header("Location", "http://example.com/escaped")
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                if runtime.delay:
                    time.sleep(runtime.delay)
                self._write(make_response(str(body.get("query") or ""), partial=runtime.partial, spoof_digest=runtime.spoof_digest, snapshot_digest=runtime.snapshot_digest))

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def run_with_server(runtime: FakeRuntime, *, replay: bool = True, labels: dict[str, Any] | None = None, timeout: float = 1.0) -> dict[str, Any]:
    try:
        artifact, raw = make_artifact()
        transport = live_eval.LocalhostTransport(runtime.base_url)
        return live_eval.run_evaluation(artifact, raw, transport, expected_source_commit=COMMIT, expected_source_tree=TREE, replay=replay, timeout_seconds=timeout, labels_payload=labels)
    finally:
        runtime.close()


def test_exact_replay_digest_and_provider_free_observability() -> None:
    report = run_with_server(FakeRuntime())
    assert report["ok"] is True
    assert report["reproducibility"]["status"] == "measured"
    assert report["reproducibility"]["mismatch_count"] == 0
    assert report["cases"][0]["replay"]["exact_digest_match"] is True
    assert report["metrics"]["observability"]["cost_microusd"]["status"] == "measured"
    assert report["provider_free"] is True
    assert report["provider_free_status"] == "proven_zero"
    assert report["metrics"]["expected_record_coverage"]["status"] == "measured"
    assert "missing_authored_labels" in report["promotion"]["blocked_reasons"]
    assert report["semantic_metrics"]["answer_correctness"]["status"] == "unmeasured"


def test_malformed_response_digest_is_fail_closed_but_case_is_preserved() -> None:
    report = run_with_server(FakeRuntime(spoof_digest=True), replay=False)
    row = report["cases"][0]
    assert len(report["cases"]) == 1
    assert row["attempted"] is True
    assert any(failure["code"] == "response_digest_mismatch" for failure in row["failures"])
    assert "case_failures_present" in report["promotion"]["blocked_reasons"]


def test_contradictory_synthetic_custody_is_rejected_before_runtime() -> None:
    artifact, raw = make_artifact()
    artifact["custody"]["synthetic"] = True
    transport = live_eval.LocalhostTransport("http://127.0.0.1:1")
    report = live_eval.run_evaluation(artifact, raw, transport, expected_source_commit=COMMIT, expected_source_tree=TREE)
    assert report["status"] == "custody_rejected"
    assert "synthetic_custody" in report["promotion"]["blocked_reasons"]


def test_spoofed_custody_digest_is_rejected_before_runtime() -> None:
    artifact, raw = make_artifact()
    artifact["custody"]["case_set_digest"] = "sha256:" + "9" * 64
    transport = live_eval.LocalhostTransport("http://127.0.0.1:1")
    report = live_eval.run_evaluation(artifact, raw, transport, expected_source_commit=COMMIT, expected_source_tree=TREE)
    assert report["status"] == "custody_rejected"
    assert "custody_case_set_mismatch" in report["promotion"]["blocked_reasons"]


def test_runtime_source_expectation_is_required_for_live_claim() -> None:
    runtime = FakeRuntime()
    try:
        artifact, raw = make_artifact()
        report = live_eval.run_evaluation(artifact, raw, live_eval.LocalhostTransport(runtime.base_url), replay=False)
    finally:
        runtime.close()
    assert report["status"] == "runtime_identity_rejected"
    assert "runtime_source_expectation_missing" in report["promotion"]["blocked_reasons"]


def test_partial_continuation_is_accounted_without_dropping_the_result() -> None:
    report = run_with_server(FakeRuntime(partial=True), replay=False)
    row = report["cases"][0]
    assert row["continuation"] == {"available": True, "observed": True}
    assert row["non_exclusion"]["preserved"] is True
    assert report["metrics"]["continuation"]["rows_with_continuation"] == 1


def test_timeout_is_an_explicit_case_failure_not_an_outer_test_timeout() -> None:
    report = run_with_server(FakeRuntime(delay=0.15), replay=False, timeout=0.02)
    row = report["cases"][0]
    assert any(failure["code"] == "timeout" for failure in row["failures"])
    assert row["attempted"] is True
    assert report["status"] == "completed_with_failures"


def test_graph_claim_requires_a_graph_holdout_cohort() -> None:
    artifact, raw = make_artifact(graph_claimed=True)
    artifact["cases"][1].pop("case_kind")
    artifact["cases"][1].pop("graph_expected_files")
    artifact["case_set_digest"] = live_eval.digest(artifact["cases"])
    artifact["custody"]["case_set_digest"] = artifact["case_set_digest"]
    report = live_eval.run_evaluation(artifact, raw, live_eval.LocalhostTransport("http://127.0.0.1:1"), expected_source_commit=COMMIT, expected_source_tree=TREE)
    assert report["status"] == "custody_rejected"
    assert "graph_cohort_missing" in report["promotion"]["blocked_reasons"]


def test_labels_are_optional_but_must_be_sealed_and_digest_bound() -> None:
    artifact, raw = make_artifact()
    labels_rows = [
        {"case_id": "case-development", "split": "development", "request_shape": {"retrieval_intent": "decision"}, "action": {"expected_next_action": "inspect_proof"}, "temporal": {"expected_temporal_state": "current_or_unknown"}, "abstention": {"expected": True}},
        {"case_id": "case-holdout", "split": "holdout", "request_shape": {"retrieval_intent": "decision"}, "action": {"expected_next_action": "inspect_proof"}, "temporal": {"expected_temporal_state": "current_or_unknown"}, "abstention": {"expected": True}},
    ]
    labels_digest = live_eval.digest(labels_rows)
    labels = {"schema_id": live_eval.LABEL_SCHEMA_ID, "version": 1, "synthetic": False, "authoring": "human_authored", "case_set_digest": artifact["case_set_digest"], "snapshot_digest": SNAPSHOT, "labels_digest": labels_digest, "split_counts": {"development": 1, "holdout": 1}, "custody": {"schema_id": live_eval.LABEL_CUSTODY_SCHEMA_ID, "owner": "human_authored", "mode": "sealed_holdout", "synthetic": False, "contamination_state": "sealed_holdout", "case_set_digest": artifact["case_set_digest"], "snapshot_digest": SNAPSHOT, "labels_digest": labels_digest, "promotional_claims_allowed": True}, "labels": labels_rows}
    report = run_with_server(FakeRuntime(), replay=False, labels=labels)
    assert report["labels"]["available"] is True
    assert report["semantic_metrics"]["status"] == "measured"


def _assert_raises(expected: type[BaseException], callback: Any) -> None:
    try:
        callback()
    except expected:
        return
    raise AssertionError(f"expected {expected.__name__}")


def _make_labels(artifact: dict[str, Any], *, expected_action: str = "inspect_proof", expected_abstention: bool = True, include_citation: bool = False) -> dict[str, Any]:
    holdout_label: dict[str, Any] = {"case_id": "case-holdout", "split": "holdout", "request_shape": {"retrieval_intent": "decision"}, "action": {"expected_next_action": expected_action}, "temporal": {"expected_temporal_state": "current_or_unknown"}, "abstention": {"expected": expected_abstention}}
    if include_citation:
        holdout_label["expected_content_digests"] = [CONTENT]
    labels_rows = [
        {"case_id": "case-development", "split": "development", "request_shape": {"retrieval_intent": "decision"}, "action": {"expected_next_action": expected_action}, "temporal": {"expected_temporal_state": "current_or_unknown"}, "abstention": {"expected": expected_abstention}},
        holdout_label,
    ]
    labels_digest = live_eval.digest(labels_rows)
    return {"schema_id": live_eval.LABEL_SCHEMA_ID, "version": 1, "synthetic": False, "authoring": "human_authored", "case_set_digest": artifact["case_set_digest"], "snapshot_digest": SNAPSHOT, "labels_digest": labels_digest, "split_counts": {"development": 1, "holdout": 1}, "custody": {"schema_id": live_eval.LABEL_CUSTODY_SCHEMA_ID, "owner": "human_authored", "mode": "sealed_holdout", "synthetic": False, "contamination_state": "sealed_holdout", "case_set_digest": artifact["case_set_digest"], "snapshot_digest": SNAPSHOT, "labels_digest": labels_digest, "promotional_claims_allowed": True}, "labels": labels_rows}


def test_transport_rejects_non_loopback_proxy_and_redirect_escape() -> None:
    _assert_raises(ValueError, lambda: live_eval.LocalhostTransport("http://localhost:8075"))
    _assert_raises(ValueError, lambda: live_eval.LocalhostTransport("http://example.com:8075"))
    _assert_raises(ValueError, lambda: live_eval.LocalhostTransport("https://127.0.0.1:8075"))
    runtime = FakeRuntime()
    old_proxy = {name: os.environ.get(name) for name in ("HTTP_PROXY", "http_proxy")}
    try:
        os.environ["HTTP_PROXY"] = "http://127.0.0.1:1"
        os.environ["http_proxy"] = "http://127.0.0.1:1"
        transport = live_eval.LocalhostTransport(runtime.base_url)
        proxy_handlers = [handler for handler in transport.opener.handlers if isinstance(handler, live_eval.urllib.request.ProxyHandler)]
        assert not proxy_handlers or all(handler.proxies == {} for handler in proxy_handlers)
        assert transport.request("GET", live_eval.HEALTH_ROUTE, None, 1.0).status == 200
    finally:
        for name, value in old_proxy.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value
        runtime.close()
    redirect_runtime = FakeRuntime(redirect=True)
    try:
        result = live_eval.LocalhostTransport(redirect_runtime.base_url).request("POST", live_eval.RESPONSE_ROUTE, {"query": "redirect"}, 1.0)
        assert result.error == "transport_error"
    finally:
        redirect_runtime.close()


def test_provider_free_requires_complete_zero_runtime_observability() -> None:
    missing = run_with_server(FakeRuntime(headers={"X-ContextLattice-Tool-Calls": "3"}), replay=False)
    assert missing["provider_free"] is False
    assert "missing_cost_observability" in missing["provider_free_blockers"]
    assert "missing_provider_call_observability" in missing["provider_free_blockers"]
    assert "missing_external_network_observability" in missing["provider_free_blockers"]
    assert missing["metrics"]["observability"]["tool_calls"]["status"] == "measured"
    nonzero_headers = {
        "X-ContextLattice-Cost-Microusd": "1",
        "X-ContextLattice-Provider-Calls": "2",
        "X-ContextLattice-Provider-Tokens": "3",
        "X-ContextLattice-External-Network-Calls": "4",
        "X-ContextLattice-Provider-Network-Calls": "5",
        "X-ContextLattice-Tool-Calls": "7",
    }
    nonzero = run_with_server(FakeRuntime(headers=nonzero_headers), replay=False)
    assert nonzero["provider_free"] is False
    assert {"nonzero_provider_cost", "provider_calls_observed", "provider_tokens_observed", "external_network_calls_observed", "provider_network_calls_observed"}.issubset(set(nonzero["provider_free_blockers"]))
    assert "tool_calls_observed" not in nonzero["promotion"]["blocked_reasons"]


def test_snapshot_drift_and_promotion_threshold_failures_are_blocked() -> None:
    report = run_with_server(FakeRuntime(snapshot_digest="sha256:" + "9" * 64), replay=False)
    assert "source_snapshot_digest_mismatch" in [failure["code"] for failure in report["cases"][0]["failures"]]
    assert "case_failures_present" in report["promotion"]["blocked_reasons"]
    artifact, _raw = make_artifact()
    labels = _make_labels(artifact, expected_action="none")
    report = run_with_server(FakeRuntime(), replay=False, labels=labels)
    assert report["semantic_metrics"]["action"]["rate"] == 0.0
    assert "semantic_threshold_failed:action" in report["promotion"]["blocked_reasons"]
    assert report["promotion"]["answer_quality_claim_allowed"] is False
    assert "answer_quality_unmeasured" in report["promotion"]["answer_quality_claim_blockers"]


def test_custody_claim_permission_and_replay_are_fail_closed() -> None:
    artifact, raw = make_artifact()
    artifact["custody"].pop("promotional_claims_allowed")
    report = live_eval.run_evaluation(artifact, raw, live_eval.LocalhostTransport("http://127.0.0.1:1"), expected_source_commit=COMMIT, expected_source_tree=TREE)
    assert report["status"] == "custody_rejected"
    assert "custody_promotional_claims_unproven" in report["promotion"]["blocked_reasons"]
    report = run_with_server(FakeRuntime(), replay=False)
    assert report["reproducibility"]["status"] == "unmeasured"
    assert "reproducibility_unmeasured" in report["promotion"]["blocked_reasons"]


def test_promotion_schema_declares_derived_provider_free_and_threshold_policy() -> None:
    schema = json.loads((Path(__file__).parents[2] / "docs/evals/schemas/recall-response-live-eval.v1.schema.json").read_text())
    assert schema["properties"]["provider_free"] == {"type": "boolean"}
    assert schema["properties"]["promotion_policy"]["properties"]["version"] == {"const": 1}
    assert {"activation_eligible", "answer_quality_claim_allowed", "answer_quality_claim_blockers"}.issubset(set(schema["properties"]["promotion"]["required"]))
    assert schema["$defs"]["digestValidation"]["properties"]["status"]["enum"] == ["provenance_bound", "canonical_validated", "invalid"]


def test_supported_gates_activate_without_answer_quality_claim_permission() -> None:
    artifact, _raw = make_artifact()
    labels = _make_labels(artifact, expected_abstention=False, include_citation=True)
    report = run_with_server(FakeRuntime(), replay=True, labels=labels)
    assert report["promotion"]["activation_eligible"] is True
    assert report["promotion"]["promotion_eligible"] is True
    assert report["promotion"]["blocked_reasons"] == []
    assert report["promotion"]["answer_quality_claim_allowed"] is False
    assert report["promotion"]["answer_quality_claim_blockers"] == ["answer_quality_unmeasured"]
    assert report["semantic_metrics"]["answer_correctness"]["status"] == "unmeasured"
    assert report["semantic_metrics"]["request_shape"]["rate"] >= 0.90
    assert report["semantic_metrics"]["action"]["rate"] >= 0.90
    assert report["semantic_metrics"]["temporal"]["rate"] >= 0.90
    assert report["semantic_metrics"]["abstention_appropriateness"]["rate"] >= 0.90
    assert report["semantic_metrics"]["citation_exactness"]["rate"] >= 0.90


def test_declared_snapshot_digest_material_is_canonicalized_or_rejected() -> None:
    artifact, raw = make_artifact()
    material = {"source": "current_state_bottom_k", "population": 2}
    expected = live_eval.digest(material)
    artifact["snapshot"]["digest_material"] = material
    artifact["snapshot"]["digest_rule"] = {"id": "sha256-canonical-json.v1", "algorithm": "sha256", "canonicalization": "sorted_compact_json"}
    artifact["snapshot"]["digest"] = expected
    artifact["custody"]["source_snapshot_digest"] = expected
    validation = live_eval.validate_frozen_artifact(artifact, raw)
    assert validation.valid is True
    assert validation.digest_validation["snapshot"]["status"] == "canonical_validated"
    artifact["snapshot"]["digest"] = "sha256:" + "0" * 64
    validation = live_eval.validate_frozen_artifact(artifact, raw)
    assert validation.valid is False
    assert any(issue["code"] == "snapshot_digest_canonical_mismatch" for issue in validation.issues)
