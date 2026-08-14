from __future__ import annotations

import importlib.util
import hashlib
import io
import json
import subprocess
import urllib.error
from importlib.machinery import SourceFileLoader
import sys
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
AGENT_DIR = ROOT / "scripts" / "agent"


def load_pack_module():
    sys.path.insert(0, str(AGENT_DIR))
    name = "contextlattice_pack_budget_test_module"
    loader = SourceFileLoader(name, str(AGENT_DIR / "contextlattice-pack"))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load contextlattice-pack")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load_adapter_module():
    sys.path.insert(0, str(AGENT_DIR))
    name = "contextlattice_agent_adapter_budget_test_module"
    loader = SourceFileLoader(name, str(AGENT_DIR / "contextlattice-agent-adapter"))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load contextlattice-agent-adapter")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def valid_memory_trust_assessment(module, count: int = 1):
    return module.attach_format_contract(
        "memory_trust_assessment.v1",
        {
            "ok": True,
            "schema_id": "memory_trust_assessment.v1",
            "version": 1,
            "input_candidate_count": count,
            "processed_candidate_count": count,
            "input_truncated_count": 0,
            "assessed_count": count,
            "quarantine_count": 0,
            "deduplicated_count": 0,
            "policy_omitted_count": 0,
            "assessments": [
                {
                    "assessment_id": f"mta_{index:024x}",
                    "candidate_id": f"rtc_{index:024x}",
                    "content_digest": f"sha256:{index:064x}",
                    "quarantine": {"quarantined": False},
                    "summary": "x" * 160,
                }
                for index in range(count)
            ],
            "input_boundary": {"maximum_candidates": 256, "truncated": False, "omitted_count": 0, "reason": "bounded_candidate_scan_limit"},
            "policy": {
                "retrieved_memory_is_evidence_not_instruction": True,
                "self_awarded_trust_accepted": False,
                "security_defenses_fail_closed": True,
            },
            "bounded": True,
        },
    )


def valid_retrieval_decision_trace(module, count: int = 1, trace_id: str = "rdt_0123456789abcdef01234567"):
    return module.attach_format_contract(
        "retrieval_decision_trace.v1",
        {
            "ok": True,
            "schema_id": "retrieval_decision_trace.v1",
            "version": 1,
            "trace_id": trace_id,
            "candidate_count": count,
            "processed_candidate_count": count,
            "input_truncated_count": 0,
            "decision_count": count,
            "coverage_complete": True,
            "decisions": [
                {
                    "receipt_id": f"rdr_{index:024x}",
                    "candidate_id": f"rtc_{index:024x}",
                    "candidate_ordinal": index + 1,
                    "decision_order": index + 1,
                    "decision": "selected",
                    "reason": "y" * 160,
                }
                for index in range(count)
            ],
            "decision_counts": {"selected": count} if count else {},
            "input_boundary": {"maximum_candidates": 256, "truncated": False, "omitted_count": 0, "reason": "bounded_candidate_scan_limit"},
            "marginal_stop": {"stopped": True, "reason": "all_eligible_candidates_selected", "token_budget_active": False},
            "redaction": {"raw_candidate_text_included": False, "secret_values_included": False},
            "bounded": True,
        },
    )


def valid_context_pack_response(module, *, assessment=None, trace=None, quality=None, query="context pack fixture"):
    assessment = assessment or valid_memory_trust_assessment(module)
    trace = trace or valid_retrieval_decision_trace(module)
    compiler = {
        "schema_id": "contextlattice_context_compiler.v1",
        "version": 1,
        "strategy": "test_fixture",
        "intended_use": "bounded test context",
        "recommended_surface": "cli_for_local_agents",
        "ranked_evidence_count": 0,
    }
    pack = {
        "query": query,
        "facts": [],
        "results": [],
        "citations": [],
        "ranked_evidence": [],
        "prompt_sections": {},
        "context_compiler": compiler,
        "relevant_decisions": [],
        "files_to_read": [],
        "files_to_avoid": [],
        "capabilities_to_use": [],
        "runbooks": [],
        "known_failure_modes": [],
        "commands": [],
        "acceptance_criteria": [],
    }
    payload = {
        "ok": True,
        "memory_trust_assessment": assessment,
        "retrieval_decision_trace": trace,
        "source_coverage": {"configured": [], "returned": [], "complete": True},
        "context_pack": pack,
        "context_compiler": compiler,
        "reference_prompt": "Use the bounded fixture context.",
        "writeback_required": True,
    }
    if quality is not None:
        payload["context_pack_quality"] = quality
    return payload


def valid_objective_runtime(module, *, agent: str, agent_id: str, project: str, session_id: str):
    return module.attach_format_contract(
        "objective_runtime_state.v1",
        {
            "version": "1",
            "agent": agent,
            "agent_id": agent_id,
            "project": project,
            "session_id": session_id,
            "objective_state": "active",
            "mission": "bounded mission",
            "objective": "bounded objective",
            "goal": "bounded goal",
            "objective_hierarchy": {
                "schema_id": "contextlattice_objective_hierarchy.v1",
                "project": {"id": project},
                "topic": {},
                "session": {"id": session_id},
                "current": {"scope": "session"},
            },
            "objective_lineage": {
                "schema_id": "contextlattice_objective_lineage.v1",
                "source": "test_fixture",
                "precedence": ["session"],
                "drift": {"detected": False},
                "handoff_rule": "preserve bounded objective authority",
            },
            "scoreboard": {
                "primary_kpi": "verified task success",
                "guardrail_kpi": "no boundary violations",
                "cadence_kpi": "each lifecycle boundary",
            },
            "action_executed": "preflight validated",
            "evidence": {"required": ["request", "contract", "session"], "current": []},
            "objective_delta": {"before": "pending", "after": "active"},
            "risk_or_blocker": {"status": "none", "fastest_recovery_path": "repeat validated preflight"},
            "next_action": "continue with bounded context",
        },
    )


def valid_policy_context_package(
    module,
    *,
    agent: str,
    agent_id: str,
    project: str,
    topic_path: str,
    query: str,
    retrieval_mode: str,
    runtime: dict,
):
    return module.attach_format_contract(
        "policy_context_package.v1",
        {
            "version": "1",
            "agent": agent,
            "agent_id": agent_id,
            "project": project,
            "topic_path": topic_path,
            "query": query,
            "retrieval_mode": retrieval_mode,
            "mission": "bounded mission",
            "objective": "bounded objective",
            "goal": "bounded goal",
            "objective_hierarchy": runtime["objective_hierarchy"],
            "objective_lineage": runtime["objective_lineage"],
            "skills": {"selected": []},
            "policy_contract": {
                "retrieve_before_inference": True,
                "anti_scheming_required": True,
                "objective_runtime_required": True,
                "checkpoint_during_execution": True,
                "final_recency_pass_required": True,
                "include_grounding": True,
                "include_retrieval_debug": True,
                "broaden_scope_on_zero_or_degraded": True,
                "format_validation_required": True,
                "contract_boundary_validated": True,
                "fail_closed_on_contract_violation": True,
            },
            "objective_runtime": runtime,
            "anti_scheming_protocol": {
                "version": "1",
                "law": "Change conclusions to match evidence",
                "required_steps": ["retrieve", "inspect", "verify", "conclude", "report"],
                "red_flags": ["unsupported claim", "hidden mutation", "identity drift", "boundary leak"],
                "delivery": ["evidence", "findings", "verification"],
            },
            "handoff": {"disperse_to_agents": True, "handoff_prompt": "change conclusions to match evidence"},
            "evidence": {"primary_facts": [], "mission_facts": [], "mission_pack_error": ""},
        },
    )


def valid_preflight_response(module, request: dict, session_id: str = "session-bootstrap"):
    runtime = valid_objective_runtime(
        module,
        agent=request["agent"],
        agent_id=request["agent_id"],
        project=request["project"],
        session_id=session_id,
    )
    policy = valid_policy_context_package(
        module,
        agent=request["agent"],
        agent_id=request["agent_id"],
        project=request["project"],
        topic_path=request["topic_path"],
        query=request["query"],
        retrieval_mode=request["retrieval_mode"],
        runtime=runtime,
    )
    contracts = sys.modules[module.validate_agent_contract_payload.__module__]
    response = contracts.attach_preflight_contracts(
        {
            "ok": True,
            "service": "gateway-go",
            "agent": request["agent"],
            "agent_id": request["agent_id"],
            "project": request["project"],
            "query": request["query"],
            "topic_path": request["topic_path"],
            "retrieval_mode": request["retrieval_mode"],
            "session_id": session_id,
            "context_pack": {"ok": True},
            "objective_runtime": runtime,
            "policy_context_package": policy,
            "agent_runtime": {
                "session": {
                    "id": session_id,
                    "status": "active",
                    "project": request["project"],
                    "agent": request["agent"],
                    "agent_id": request["agent_id"],
                    "reuse_key": request["reuse_key"],
                }
            },
            "skills_index": {"ok": True, "returned": 0, "results": []},
        }
    )
    return response


class ContextLatticePackBudgetTests(unittest.TestCase):
    def test_auto_session_binds_raw_response_authority_before_state_write(self) -> None:
        module = load_pack_module()
        common = sys.modules[module.request_json_for_validation.__module__]
        metadata = {"surface": "unit-test"}
        objective = "bind automatic session authority"
        reuse_key = common.agent_session_reuse_key("alpha", "codex", "agent-safe", objective, metadata)

        def response(session_id: str = "session-authority", **changes):
            session = {
                "id": session_id,
                "status": "active",
                "project": "alpha",
                "agent": "codex",
                "agent_id": "agent-safe",
                "reuse_key": reuse_key,
            }
            session.update(changes)
            return {"ok": True, "session": session}

        written: list[dict] = []
        with patch.dict(
            common.os.environ,
            {"CONTEXTLATTICE_SESSION_ID": "", "CONTEXTLATTICE_AUTO_SESSION_DISABLED": ""},
            clear=False,
        ), patch.object(common, "read_agent_session_state", return_value={}), patch.object(
            common, "request_json_for_validation", return_value=response()
        ), patch.object(
            common, "write_agent_session_state", side_effect=lambda project, state: written.append(state)
        ):
            result = common.ensure_agent_session(
                project="alpha",
                objective=objective,
                agent="codex",
                agent_id="agent-safe",
                metadata=metadata,
            )
        self.assertEqual(result["session_id"], "session-authority")
        self.assertEqual(len(written), 1)
        self.assertEqual(written[0]["reuse_key"], reuse_key)

        for label, raw in (
            ("rejected", {**response(), "ok": False}),
            ("foreign project", response(project="other")),
            ("foreign agent", response(agent="other")),
            ("foreign worker", response(agent_id="other")),
            ("foreign reuse", response(reuse_key="reuse_other")),
        ):
            with self.subTest(label=label):
                rejected_writes: list[dict] = []
                with patch.dict(
                    common.os.environ,
                    {"CONTEXTLATTICE_SESSION_ID": "", "CONTEXTLATTICE_AUTO_SESSION_DISABLED": ""},
                    clear=False,
                ), patch.object(common, "read_agent_session_state", return_value={}), patch.object(
                    common, "request_json_for_validation", return_value=raw
                ), patch.object(
                    common,
                    "write_agent_session_state",
                    side_effect=lambda project, state: rejected_writes.append(state),
                ):
                    result = common.ensure_agent_session(
                        project="alpha",
                        objective=objective,
                        agent="codex",
                        agent_id="agent-safe",
                        metadata=metadata,
                    )
                self.assertEqual(result["session_id"], "")
                self.assertEqual(rejected_writes, [])

    def test_auto_session_reuses_only_exact_cached_authority(self) -> None:
        module = load_pack_module()
        common = sys.modules[module.request_json_for_validation.__module__]
        metadata = {"surface": "cached-test"}
        objective = "reuse exact cached authority"
        reuse_key = common.agent_session_reuse_key("alpha", "codex", "agent-safe", objective, metadata)
        calls: list[tuple[str, str]] = []

        def fake_request(method, path, payload, timeout):
            calls.append((method, path))
            return {
                "ok": True,
                "session": {
                    "id": "session-cached",
                    "status": "active",
                    "project": "alpha",
                    "agent": "codex",
                    "agent_id": "agent-safe",
                    "reuse_key": reuse_key,
                },
            }

        with patch.dict(
            common.os.environ,
            {"CONTEXTLATTICE_SESSION_ID": "", "CONTEXTLATTICE_AUTO_SESSION_DISABLED": ""},
            clear=False,
        ), patch.object(
            common,
            "read_agent_session_state",
            return_value={"session_id": "session-cached", "reuse_key": reuse_key},
        ), patch.object(common, "request_json_for_validation", side_effect=fake_request), patch.object(
            common, "write_agent_session_state"
        ) as write_state:
            result = common.ensure_agent_session(
                project="alpha",
                objective=objective,
                agent="codex",
                agent_id="agent-safe",
                metadata=metadata,
            )
        self.assertEqual(result["source"], "state")
        self.assertEqual(calls, [("GET", "/v1/agents/sessions/session-cached")])
        write_state.assert_not_called()

    def test_adapter_bootstrap_validates_raw_contract_and_session_authority(self) -> None:
        module = load_adapter_module()
        profile = (
            "codex",
            {},
            "agent-safe",
            "runbooks/test",
            "bootstrap authority café <>&\u2028",
            "fast",
        )

        def run_with_mutation(mutate=None, *, restamp=True):
            stdout = io.StringIO()
            stderr = io.StringIO()

            def fake_request(method, path, payload, timeout):
                self.assertEqual((method, path), ("POST", "/v1/agents/preflight"))
                response = valid_preflight_response(module, payload)
                if mutate is not None:
                    response = json.loads(json.dumps(response))
                    mutate(response)
                    if restamp:
                        contracts = sys.modules[module.validate_agent_contract_payload.__module__]
                        response = contracts.attach_preflight_contracts(response)
                return response

            argv = [
                "contextlattice-agent-adapter",
                "bootstrap",
                "--project",
                "alpha",
                "--agent",
                "codex",
                "--agent-id",
                "agent-safe",
                "--query",
                "bootstrap authority café <>&\u2028",
                "--mode",
                "fast",
            ]
            with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(
                module, "request_json", side_effect=AssertionError("bootstrap used the public pre-validation transport")
            ), patch.object(module, "profile_payload", return_value=profile), patch.object(
                sys, "argv", argv
            ), redirect_stdout(stdout), redirect_stderr(stderr):
                rc = module.main()
            return rc, json.loads(stdout.getvalue()), stderr.getvalue()

        rc, output, stderr = run_with_mutation()
        self.assertEqual(rc, 0)
        self.assertIs(output["ok"], True)
        self.assertEqual(output["session_id"], "session-bootstrap")
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(stderr, "")

        mutations = {
            "foreign project": lambda response: response.__setitem__("project", "other"),
            "foreign session agent": lambda response: response["agent_runtime"]["session"].__setitem__(
                "agent", "other"
            ),
            "foreign reuse key": lambda response: response["agent_runtime"]["session"].__setitem__(
                "reuse_key", "reuse_other"
            ),
            "terminal session": lambda response: response["agent_runtime"]["session"].__setitem__(
                "status", "completed"
            ),
        }
        for label, mutate in mutations.items():
            with self.subTest(label=label):
                rc, output, stderr = run_with_mutation(mutate)
                self.assertEqual(rc, 1)
                self.assertIs(output["ok"], False)
                self.assertTrue(output["format_contract"]["contract_valid"])
                self.assertEqual(output["result"], {"status": "bounded_public_boundary_failure"})
                self.assertNotIn("objective_runtime", output["result"])
                self.assertNotIn("policy_context_package", output["result"])
                self.assertNotIn("preflight", output["result"])
                self.assertEqual(stderr, "")

        stale_attestations = {
            "outer byte accounting": lambda response: response["format_contracts"].__setitem__(
                "actual_json_bytes", 0
            ),
            "outer contract inventory": lambda response: response["format_contracts"].__setitem__(
                "contracts", []
            ),
            "objective contract valid": lambda response: response["objective_runtime"][
                "format_contract"
            ].__setitem__("contract_valid", False),
            "objective registry version": lambda response: response["objective_runtime"][
                "format_contract"
            ].__setitem__("registry_version", 1),
            "policy contract valid": lambda response: response["policy_context_package"][
                "format_contract"
            ].__setitem__("contract_valid", False),
        }
        for label, mutate in stale_attestations.items():
            with self.subTest(label=label):
                rc, output, stderr = run_with_mutation(mutate, restamp=False)
                self.assertEqual(rc, 1)
                self.assertIs(output["ok"], False)
                self.assertTrue(output["format_contract"]["contract_valid"])
                self.assertEqual(output["result"], {"status": "bounded_public_boundary_failure"})
                self.assertEqual(stderr, "")

    def test_shared_contract_byte_accounting_matches_go_utf8_json(self) -> None:
        module = load_adapter_module()
        contracts = sys.modules[module.validate_agent_contract_payload.__module__]
        value = {
            "float": 1e20,
            "negative_zero": -0.0,
            "text": "café<>&\u2028\u2029",
        }
        expected = (
            '{"float":100000000000000000000,"negative_zero":-0,'
            '"text":"café\\u003c\\u003e\\u0026\\u2028\\u2029"}'
        ).encode("utf-8")
        self.assertEqual(contracts.agent_contract_go_json(value), expected)

    def test_tiny_budget_preserves_session_and_agent_identity(self) -> None:
        module = load_pack_module()
        payload = {
            "ok": True,
            "session_id": "session-" + "1" * 24,
            "agent_id": "agent_budget_identity",
            "task_summary": "identity must survive compaction",
            "context_pack": {
                "query": "bounded identity regression",
                "retrieval_mode": "balanced",
                "retrieval_intent": "decision",
                "ranked_evidence": [{"summary": "x" * 20000}],
            },
        }

        compacted = module.finalize_context_pack_payload(payload, 1024)

        self.assertEqual(compacted.get("session_id"), "session-" + "1" * 24)
        self.assertEqual(compacted.get("agent_id"), "agent_budget_identity")
        self.assertEqual(compacted["requested_context_budget_chars"], 1024)
        self.assertEqual(compacted["context_budget_chars"], module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertTrue(compacted["budget_floor_applied"])
        self.assertLessEqual(module.encoded_len(compacted), compacted["context_budget_chars"])
        self.assertTrue(compacted["format_contract"]["contract_valid"])
        self.assertEqual(compacted["memory_trust_assessment"]["available"], False)
        self.assertEqual(compacted["retrieval_decision_trace"]["available"], False)
        self.assertEqual(compacted["context_pack"]["memory_trust_assessment"]["canonical_path"], "$.memory_trust_assessment")
        self.assertEqual(compacted["context_compiler"]["retrieval_decision_trace"]["canonical_path"], "$.retrieval_decision_trace")

    def test_ranked_evidence_keeps_only_bounded_selection_rationale_lists(self) -> None:
        module = load_pack_module()
        compacted = module.compact_items(
            [
                {
                    "memory_id": "memory-rationale",
                    "summary": "bounded rationale",
                    "why_selected": [f"reason-{index}-" + "x" * 200 for index in range(10)],
                    "reason": {"untrusted": "nested composite"},
                }
            ],
            1,
        )

        self.assertEqual(len(compacted), 1)
        self.assertEqual(len(compacted[0]["why_selected"]), 6)
        self.assertTrue(all(len(reason) <= 120 for reason in compacted[0]["why_selected"]))
        self.assertNotIn("reason", compacted[0])

    def test_token_budget_projection_preserves_the_canonical_report_field_set(self) -> None:
        module = load_pack_module()
        canonical = {
            "schema_id": "contextlattice_context_token_budget.v1",
            "active": True,
            "estimate_method": "tiktoken",
            "calibration_grade": "tokenizer_exact",
            "tokenizer_exact": True,
            "selection_strategy": "impact_per_estimated_token_with_provenance_diversity",
            "agent_context_budget_tokens": 8192,
            "model_context_window_tokens": 32768,
            "reserved_response_tokens": 4096,
            "already_loaded_tokens": 2048,
            "target_context_pack_tokens": 4096,
            "ranked_evidence_budget_tokens": 2048,
            "used_tokens_estimate": 1234,
            "selected_count": 12,
            "omitted_high_value_count": 3,
            "compression_level": "none",
            "tokenizer_encoding": "cl100k_base",
            "untrusted_extension": "must not escape",
        }

        projected = module.compact_token_budget(canonical)

        self.assertEqual(projected, {key: value for key, value in canonical.items() if key != "untrusted_extension"})

    def test_token_budget_provenance_tracks_the_final_ranked_evidence_projection(self) -> None:
        module = load_pack_module()
        forged_budget = {
            "schema_id": "forged_context_budget.v9",
            "active": True,
            "selected_count": 999,
            "used_tokens_estimate": 42,
        }
        raw = {
            "ok": True,
            "token_budget": forged_budget,
            "context_pack": {
                "token_budget": forged_budget,
                "prompt_sections": {"token_budget": forged_budget},
                "ranked_evidence": [
                    {"rank": index + 1, "text": f"bounded evidence {index}"}
                    for index in range(12)
                ],
            },
        }

        for requested_budget in (100_000, 1_024):
            with self.subTest(requested_budget=requested_budget):
                output = module.compact_context_pack(raw, "token budget custody", requested_budget)
                ranked_count = len(output["context_pack"]["ranked_evidence"])
                root_budget = output["token_budget"]
                nested_budget = output["context_pack"]["token_budget"]
                self.assertEqual(root_budget, nested_budget)
                self.assertEqual(root_budget["schema_id"], "contextlattice_context_token_budget.v1")
                self.assertEqual(root_budget["selected_count"], ranked_count)
                prompt_budget = output["context_pack"].get("prompt_sections", {}).get("token_budget")
                if prompt_budget is not None:
                    self.assertEqual(prompt_budget, root_budget)

    def test_minimum_budget_is_hard_under_maximal_optional_inputs(self) -> None:
        module = load_pack_module()
        long_text = "x" * 6000
        payload = {
            "ok": True,
            "session_id": long_text,
            "agent_id": long_text,
            "task_summary": long_text,
            "token_budget": {f"untrusted_{index}": long_text for index in range(5000)},
            "omitted_high_value_refs": [
                {"memory_id": f"memory-{index}", "summary": long_text}
                for index in range(2)
            ],
            "context_pack": {
                "query": long_text,
                "retrieval_mode": long_text,
                "retrieval_intent": long_text,
                "ranked_evidence": [{"summary": long_text}],
            },
        }

        compacted = module.finalize_context_pack_payload(payload, 1024)

        self.assertLessEqual(module.encoded_len(compacted), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(compacted["context_budget_chars"], module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(compacted["requested_context_budget_chars"], 1024)
        self.assertTrue(compacted["budget_floor_applied"])
        self.assertTrue(compacted["format_contract"]["contract_valid"])
        self.assertNotIn("session_id", compacted)
        self.assertNotIn("agent_id", compacted)
        self.assertEqual(compacted["identity_omitted"], ["session_id", "agent_id"])
        self.assertEqual(
            compacted["token_budget"],
            {"schema_id": "contextlattice_context_token_budget.v1", "selected_count": 0},
        )
        self.assertEqual(compacted["context_pack"]["token_budget"], compacted["token_budget"])
        self.assertNotIn("omitted_high_value_refs", compacted)
        self.assertNotIn("omitted_high_value_refs", compacted["context_pack"])

        roomy = module.compact_context_pack(
            {
                "ok": True,
                "session_id": long_text,
                "agent_id": long_text,
                "context_pack": {"facts": [], "results": []},
            },
            "roomy identity boundary",
            500000,
        )
        self.assertNotIn("session_id", roomy)
        self.assertNotIn("agent_id", roomy)
        self.assertEqual(roomy["identity_omitted"], ["session_id", "agent_id"])
        self.assertTrue(roomy["format_contract"]["contract_valid"])

    def test_out_of_range_cli_budget_returns_bounded_typed_failure(self) -> None:
        module = load_pack_module()
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "invalid budget boundary",
            "--no-auto-session",
            "--budget-chars",
            str(1 << 100),
        ]
        with patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 1)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertFalse(output["ok"])
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(output["requested_context_budget_chars"], 1)
        self.assertEqual(output["context_budget_chars"], module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertLessEqual(len(emitted.encode("utf-8")), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertNotIn(str(1 << 100), emitted + stderr.getvalue())
        self.assertEqual(stderr.getvalue(), "")

    def test_compaction_failure_truth_controls_exit_and_state(self) -> None:
        module = load_pack_module()
        raw = {
            "ok": True,
            "agent_runtime": {"counter": 1 << 100},
            "source_coverage": {"configured": [], "returned": [], "complete": True},
            "context_pack": {"query": "invalid optional integer", "facts": [], "results": []},
        }
        written: list[dict] = []
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "invalid optional integer",
            "--no-auto-session",
            "--session-id",
            "session-invalid-domain",
            "--budget-chars",
            "50000",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "read_agent_session_state", return_value={}
        ), patch.object(
            module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 1)

        output = json.loads(stdout.getvalue())
        self.assertFalse(output["ok"])
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(written, [])
        self.assertEqual(stderr.getvalue(), "")

    def test_gateway_rejection_or_missing_pack_never_upgrades_to_success(self) -> None:
        module = load_pack_module()
        for name, raw in (
            ("explicit rejection", {"ok": False, "context_pack": {}}),
            ("missing pack", {"ok": True}),
        ):
            with self.subTest(name=name):
                written: list[dict] = []
                stdout = io.StringIO()
                stderr = io.StringIO()
                argv = [
                    "contextlattice-pack",
                    "gateway rejection truth",
                    "--no-auto-session",
                    "--session-id",
                    "session-request-authority",
                    "--agent-id",
                    "agent-request-authority",
                    "--budget-chars",
                    "50000",
                ]
                with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
                    module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)
                ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
                    self.assertEqual(module.main(), 1)

                emitted = stdout.getvalue()
                output = json.loads(emitted)
                self.assertFalse(output["ok"])
                self.assertTrue(output["format_contract"]["contract_valid"])
                self.assertLessEqual(len(emitted.encode("utf-8")), 50000)
                self.assertEqual(written, [])
                self.assertEqual(stderr.getvalue(), "")

    def test_main_response_identities_are_bound_to_request_authority(self) -> None:
        module = load_pack_module()
        quality = {
            "schema_id": "contextlattice_context_pack_quality.v1",
            "version": 1,
            "capturedAt": "2026-08-12T12:00:00Z",
            "sample_id": "cpq_main_identity_authority",
            "query_hash": "0123456789abcdef",
            "quality_score": 90,
        }
        raw = valid_context_pack_response(module, quality=quality, query="main identity authority")
        raw["session_id"] = "session-response-other"
        raw["agent_id"] = "agent-response-other"
        written: list[dict] = []
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "main identity authority",
            "--no-auto-session",
            "--session-id",
            "session-request-authority",
            "--agent-id",
            "agent-request-authority",
            "--budget-chars",
            "50000",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "read_agent_session_state", return_value={}
        ), patch.object(
            module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertEqual(output["session_id"], "session-request-authority")
        self.assertEqual(output["agent_id"], "agent-request-authority")
        self.assertNotIn("session-response-other", emitted + stderr.getvalue())
        self.assertNotIn("agent-response-other", emitted + stderr.getvalue())
        self.assertEqual(written[0]["session_id"], "session-request-authority")
        self.assertEqual(written[0]["agent_id"], "agent-request-authority")

    def test_hard_envelope_uses_exact_public_emission_bytes_and_priority_reduction(self) -> None:
        module = load_pack_module()
        full_trace = valid_retrieval_decision_trace(module, trace_id="r" * module.MAX_CONTEXT_PACK_IDENTITY_CHARS)
        full_assessment = valid_memory_trust_assessment(module)
        unicode_text = "😀" * 6000
        raw = {
            "ok": True,
            "session_id": "s" * module.MAX_CONTEXT_PACK_IDENTITY_CHARS,
            "agent_id": "a" * module.MAX_CONTEXT_PACK_IDENTITY_CHARS,
            "memory_trust_assessment": full_assessment,
            "retrieval_decision_trace": full_trace,
            "source_coverage": {
                "configured": [unicode_text] * 8,
                "returned": [unicode_text] * 8,
                "complete": True,
            },
            "context_pack": {
                "query": unicode_text,
                "retrieval_mode": unicode_text,
                "retrieval_intent": unicode_text,
                "facts": [],
                "results": [],
            },
        }

        compacted = module.compact_context_pack(raw, "token=x " * 6000, 1024)
        emitted = json.dumps(module.redact_public_value(compacted)) + "\n"

        self.assertLessEqual(len(emitted.encode("utf-8")), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertTrue(compacted["format_contract"]["contract_valid"])
        self.assertEqual(compacted["context_budget_chars"], module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(compacted["requested_context_budget_chars"], 1024)
        self.assertTrue(compacted["budget_floor_applied"])
        self.assertEqual(compacted.get("session_id", ""), "")
        self.assertEqual(compacted.get("agent_id", ""), "")
        self.assertEqual(compacted["memory_trust_assessment"]["canonical_path"], "$.memory_trust_assessment")
        self.assertRegex(compacted["memory_trust_assessment"]["canonical_digest"], r"^sha256:[0-9a-f]{64}$")
        self.assertEqual(compacted["retrieval_decision_trace"]["canonical_path"], "$.retrieval_decision_trace")
        self.assertRegex(compacted["retrieval_decision_trace"]["canonical_digest"], r"^sha256:[0-9a-f]{64}$")

        roomy = module.compact_context_pack({"ok": True, "context_pack": {"facts": [], "results": []}}, "roomy", 50000)
        self.assertEqual(roomy["context_budget_chars"], 50000)
        self.assertEqual(roomy["requested_context_budget_chars"], 50000)
        self.assertFalse(roomy["budget_floor_applied"])

    def test_large_budget_still_reduces_until_the_attached_contract_is_valid(self) -> None:
        module = load_pack_module()
        incoming = module.compact_context_pack(
            {"ok": True, "context_pack": {"facts": [], "results": []}},
            "large valid incoming contract",
            500000,
        )
        incoming.pop("format_contract", None)
        large_lifecycle = {f"phase_{index:03d}": "v" * 96 for index in range(400)}
        source_coverage = dict(incoming["source_coverage"])
        source_coverage["retrieval_lifecycle"] = large_lifecycle
        incoming["source_coverage"] = source_coverage
        advisor = dict(incoming["run_advisor"])
        graph_quality = dict(advisor.get("graph_quality") or {})
        graph_quality["signals"] = {f"metric_{index:03d}": "v" * 96 for index in range(400)}
        advisor["graph_quality"] = graph_quality
        incoming["run_advisor"] = advisor
        incoming = module.attach_format_contract("context_pack_response.v1", incoming)
        self.assertTrue(incoming["format_contract"]["contract_valid"])

        compacted = module.compact_context_pack(incoming, "large valid incoming contract", 500000)

        self.assertTrue(compacted["format_contract"]["contract_valid"])
        self.assertLessEqual(module.emitted_len(compacted), 500000)

        redaction_expansion = module.compact_context_pack(
            {"ok": True, "context_pack": {"facts": [], "results": []}},
            "public size expansion",
            500000,
        )
        redaction_expansion.pop("format_contract", None)
        sensitive_key_prefix = "pass" + "word"
        redaction_expansion["agent_runtime"] = {
            f"{sensitive_key_prefix}_{index:04d}": "x" for index in range(4000)
        }
        redaction_expansion = module.attach_format_contract(
            "context_pack_response.v1", redaction_expansion
        )
        self.assertTrue(redaction_expansion["format_contract"]["contract_valid"])
        self.assertGreater(
            len(
                json.dumps(
                    module.redact_public_value(redaction_expansion),
                    sort_keys=True,
                    separators=(",", ":"),
                ).encode("utf-8")
            ),
            redaction_expansion["format_contract"]["max_total_json_bytes"],
        )

        bounded_expansion = module.compact_context_pack(
            redaction_expansion,
            "public size expansion",
            500000,
        )
        self.assertTrue(bounded_expansion["format_contract"]["contract_valid"])
        self.assertLessEqual(
            bounded_expansion["format_contract"]["actual_json_bytes"],
            bounded_expansion["format_contract"]["max_total_json_bytes"],
        )
        self.assertLessEqual(module.emitted_len(bounded_expansion), 500000)

    def test_oversized_proof_counts_are_rejected_or_digest_bound_without_envelope_growth(self) -> None:
        module = load_pack_module()
        huge_count = 10**999
        trust_ref = {
            "schema_id": "memory_trust_assessment.v1",
            "canonical_path": "$.memory_trust_assessment",
            "assessed_count": huge_count,
            "quarantine_count": huge_count,
            "deduplicated_count": huge_count,
            "policy_omitted_count": huge_count,
            "input_truncated_count": huge_count,
        }
        trace_ref = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "trace_id": "rdt_huge_counts",
            "candidate_count": huge_count,
            "decision_count": huge_count,
            "input_truncated_count": huge_count,
            "coverage_complete": False,
        }
        self.assertEqual(module.canonical_retrieval_proof(trust_ref, "memory_trust_assessment", allow_reference=True), {})
        self.assertEqual(module.canonical_retrieval_proof(trace_ref, "retrieval_decision_trace", allow_reference=True), {})

        full_trace = valid_retrieval_decision_trace(module)
        for field in ("candidate_count", "decision_count", "input_truncated_count"):
            full_trace[field] = huge_count
        full_trace = module.attach_format_contract("retrieval_decision_trace.v1", full_trace)
        output = module.compact_context_pack(
            {
                "ok": True,
                "retrieval_decision_trace": full_trace,
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "huge proof counts", "facts": [], "results": []},
            },
            "huge proof counts",
            1024,
        )
        emitted = json.dumps(module.redact_public_value(output)) + "\n"
        self.assertLessEqual(len(emitted.encode("utf-8")), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertIs(output["retrieval_decision_trace"]["available"], False)
        self.assertNotIn("canonical_digest", output["retrieval_decision_trace"])
        self.assertNotIn(str(huge_count), emitted)

    def test_overlong_trace_identity_is_omitted_without_breaking_the_hard_output_bound(self) -> None:
        module = load_pack_module()
        long_trace_id = "trace-" + "x" * 1000
        trace = valid_retrieval_decision_trace(module, trace_id=long_trace_id)
        expected_digest = "sha256:" + hashlib.sha256(
            json.dumps(trace, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()

        def fake_request(method, path, payload, timeout):
            return {
                "ok": True,
                "memory_trust_assessment": valid_memory_trust_assessment(module),
                "retrieval_decision_trace": trace,
                "source_coverage": {"configured": ["fixture"], "returned": ["fixture"], "complete": True},
                "context_pack": {"query": "overlong trace identity", "facts": [], "results": []},
            }

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "overlong trace identity", "--no-auto-session", "--budget-chars", "1024"]
        with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertLessEqual(len(emitted.encode("utf-8")), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(output["context_budget_chars"], module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(output["retrieval_decision_trace"]["trace_id"], "")
        self.assertIs(output["retrieval_decision_trace"]["trace_id_omitted"], True)
        self.assertEqual(output["retrieval_decision_trace"]["canonical_digest"], expected_digest)
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertNotIn(long_trace_id, emitted)

        unicode_root_reference = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "trace_id": "é" * 200,
            "candidate_count": 1,
            "decision_count": 1,
            "input_truncated_count": 0,
            "coverage_complete": True,
        }
        self.assertEqual(
            module.canonical_retrieval_proof(
                unicode_root_reference,
                "retrieval_decision_trace",
                allow_reference=True,
            ),
            {},
        )
        attached = module.attach_format_contract(
            "context_pack_response.v1",
            {
                "ok": True,
                "context_pack": {},
                "context_compiler": {},
                "memory_trust_assessment": {
                    "schema_id": "memory_trust_assessment.v1",
                    "canonical_path": "$.memory_trust_assessment",
                    "assessed_count": 0,
                    "quarantine_count": 0,
                    "deduplicated_count": 0,
                    "policy_omitted_count": 0,
                    "input_truncated_count": 0,
                },
                "retrieval_decision_trace": unicode_root_reference,
                "source_coverage": {},
                "reference_prompt": "unicode byte-bound parity",
                "writeback_required": True,
            },
        )
        self.assertIs(attached["retrieval_decision_trace"]["available"], False)

    def test_compaction_preserves_canonical_retrieval_proofs_and_uses_bounded_references(self) -> None:
        module = load_pack_module()
        assessment = valid_memory_trust_assessment(module, 140)
        trace = valid_retrieval_decision_trace(module, 140, "rdt_111111111111111111111111")
        raw = {
            "ok": True,
            "session_id": "sess_retrieval_proof",
            "memory_trust_assessment": assessment,
            "retrieval_decision_trace": trace,
            "source_coverage": {"configured": ["fixture"], "returned": ["fixture"], "complete": True},
            "context_pack": {
                "query": "preserve proof custody",
                "facts": [],
                "results": [],
                "ranked_evidence": [],
            },
        }

        compacted = module.compact_context_pack(raw, "preserve proof custody", 9000)

        self.assertTrue(compacted["format_contract"]["contract_valid"])
        root_assessment = compacted["memory_trust_assessment"]
        root_trace = compacted["retrieval_decision_trace"]
        self.assertEqual(root_assessment["canonical_path"], "$.memory_trust_assessment")
        self.assertEqual(root_trace["canonical_path"], "$.retrieval_decision_trace")
        self.assertEqual(root_assessment["assessed_count"], 140)
        self.assertEqual(root_trace["trace_id"], "rdt_111111111111111111111111")
        self.assertTrue(root_assessment["bounded_projection"])
        self.assertTrue(root_trace["bounded_projection"])
        self.assertRegex(root_assessment["canonical_digest"], r"^sha256:[0-9a-f]{64}$")
        self.assertRegex(root_trace["canonical_digest"], r"^sha256:[0-9a-f]{64}$")
        expected_assessment_digest = "sha256:" + hashlib.sha256(
            json.dumps(assessment, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        expected_trace_digest = "sha256:" + hashlib.sha256(
            json.dumps(trace, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        self.assertEqual(root_assessment["canonical_digest"], expected_assessment_digest)
        self.assertEqual(root_trace["canonical_digest"], expected_trace_digest)
        reordered_assessment = dict(reversed(list(assessment.items())))
        self.assertEqual(
            module.bounded_retrieval_proof(reordered_assessment, "memory_trust_assessment")["canonical_digest"],
            root_assessment["canonical_digest"],
        )
        changed_assessment = {**assessment, "assessed_count": 139}
        self.assertNotEqual(
            module.bounded_retrieval_proof(changed_assessment, "memory_trust_assessment")["canonical_digest"],
            root_assessment["canonical_digest"],
        )
        changed_tail_assessment = {**assessment, "assessments": [dict(row) for row in assessment["assessments"]]}
        changed_tail_assessment["assessments"][100]["summary"] = "changed beyond the contract list clipping boundary"
        self.assertNotEqual(
            module.bounded_retrieval_proof(changed_tail_assessment, "memory_trust_assessment")["canonical_digest"],
            root_assessment["canonical_digest"],
        )
        minimum_budget = module.compact_context_pack(raw, "preserve proof custody", 1024)
        self.assertLessEqual(module.encoded_len(minimum_budget), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(minimum_budget["memory_trust_assessment"]["canonical_digest"], expected_assessment_digest)
        self.assertEqual(minimum_budget["retrieval_decision_trace"]["canonical_digest"], expected_trace_digest)
        for owner in (compacted["context_pack"], compacted["context_compiler"]):
            self.assertEqual(owner["memory_trust_assessment"]["canonical_path"], "$.memory_trust_assessment")
            self.assertEqual(owner["retrieval_decision_trace"]["canonical_path"], "$.retrieval_decision_trace")
            self.assertNotIn("assessments", owner["memory_trust_assessment"])
            self.assertNotIn("decisions", owner["retrieval_decision_trace"])

        already_bounded = dict(root_assessment)
        already_bounded["summary"] = "private projection detail" * 400
        rebound = module.bounded_retrieval_proof(already_bounded, "memory_trust_assessment")
        self.assertEqual(rebound["canonical_digest"], expected_assessment_digest)
        self.assertNotIn("summary", rebound)

        small_rows = valid_memory_trust_assessment(module, module.MAX_CONTEXT_PACK_CONTRACT_LIST_ITEMS + 1)
        small_rows = module.attach_format_contract("memory_trust_assessment.v1", small_rows)
        self.assertFalse(module.validate_agent_contract_payload("memory_trust_assessment.v1", small_rows))
        expected_small_rows_digest = "sha256:" + hashlib.sha256(
            json.dumps(small_rows, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        large_budget = module.compact_context_pack(
            {
                "ok": True,
                "memory_trust_assessment": small_rows,
                "retrieval_decision_trace": valid_retrieval_decision_trace(
                    module, module.MAX_CONTEXT_PACK_CONTRACT_LIST_ITEMS + 1
                ),
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "65 item receipt", "facts": [], "results": []},
            },
            "65 item receipt",
            20000,
        )
        self.assertEqual(large_budget["memory_trust_assessment"]["canonical_digest"], expected_small_rows_digest)
        self.assertTrue(large_budget["memory_trust_assessment"]["bounded_projection"])
        self.assertNotIn("assessments", large_budget["memory_trust_assessment"])

        nested_rows = valid_memory_trust_assessment(module)
        nested_rows["assessments"][0]["reasons"] = [""] * (module.MAX_CONTEXT_PACK_CONTRACT_LIST_ITEMS + 1)
        nested_rows = module.attach_format_contract("memory_trust_assessment.v1", nested_rows)
        self.assertFalse(module.validate_agent_contract_payload("memory_trust_assessment.v1", nested_rows))
        expected_nested_rows_digest = "sha256:" + hashlib.sha256(
            json.dumps(nested_rows, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        nested_budget = module.compact_context_pack(
            {
                "ok": True,
                "memory_trust_assessment": nested_rows,
                "retrieval_decision_trace": valid_retrieval_decision_trace(module),
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "nested 65 item receipt", "facts": [], "results": []},
            },
            "nested 65 item receipt",
            50000,
        )
        self.assertEqual(nested_budget["memory_trust_assessment"]["canonical_digest"], expected_nested_rows_digest)
        self.assertTrue(nested_budget["memory_trust_assessment"]["bounded_projection"])
        self.assertTrue(nested_budget["format_contract"]["contract_valid"])

        private_rows = valid_memory_trust_assessment(module)
        private_marker = "sk-PRIVATE-PROOF-SUMMARY-123456"
        private_rows["assessments"][0]["summary"] = private_marker
        private_rows = module.attach_format_contract("memory_trust_assessment.v1", private_rows)
        self.assertFalse(module.validate_agent_contract_payload("memory_trust_assessment.v1", private_rows))
        expected_private_rows_digest = "sha256:" + hashlib.sha256(
            json.dumps(private_rows, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        private_budget = module.compact_context_pack(
            {
                "ok": True,
                "memory_trust_assessment": private_rows,
                "retrieval_decision_trace": valid_retrieval_decision_trace(module),
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "private proof summary", "facts": [], "results": []},
            },
            "private proof summary",
            50000,
        )
        private_emission = json.dumps(module.redact_public_value(private_budget))
        self.assertEqual(private_budget["memory_trust_assessment"]["canonical_digest"], expected_private_rows_digest)
        self.assertTrue(private_budget["memory_trust_assessment"]["bounded_projection"])
        self.assertNotIn(private_marker, private_emission)

        overflow_rows = valid_memory_trust_assessment(module)
        overflow_marker = "maximum context length"
        overflow_rows["assessments"][0]["summary"] = overflow_marker
        overflow_rows = module.attach_format_contract("memory_trust_assessment.v1", overflow_rows)
        self.assertFalse(module.validate_agent_contract_payload("memory_trust_assessment.v1", overflow_rows))
        expected_overflow_rows_digest = "sha256:" + hashlib.sha256(
            json.dumps(overflow_rows, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        overflow_budget = module.compact_context_pack(
            {
                "ok": True,
                "memory_trust_assessment": overflow_rows,
                "retrieval_decision_trace": valid_retrieval_decision_trace(module),
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "provider overflow phrase proof", "facts": [], "results": []},
            },
            "provider overflow phrase proof",
            50000,
        )
        self.assertEqual(overflow_budget["memory_trust_assessment"]["canonical_digest"], expected_overflow_rows_digest)
        self.assertTrue(overflow_budget["memory_trust_assessment"]["bounded_projection"])

    def test_response_mode_delegates_before_pack_request(self) -> None:
        module = load_pack_module()
        delegated: list[list[str]] = []

        class Delegated(Exception):
            pass

        def fake_delegate(argv: list[str]) -> None:
            delegated.append(argv)
            raise Delegated

        argv = ["contextlattice-pack", "response task", "--project", "alpha", "--response", "--retries", "0"]
        with patch.object(module, "delegate_recall_response", side_effect=fake_delegate), patch.object(sys, "argv", argv):
            with self.assertRaises(Delegated):
                module.main()

        self.assertEqual(delegated, [["response task", "--project", "alpha", "--response", "--retries", "0"]])

    def test_nested_retrieval_references_cannot_claim_canonical_root_custody(self) -> None:
        module = load_pack_module()
        trust_ref = {
            "schema_id": "memory_trust_assessment.v1",
            "canonical_path": "$.memory_trust_assessment",
            "assessed_count": 9,
        }
        trace_ref = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "trace_id": "rdt_reference_only",
            "decision_count": 9,
        }
        raw = {
            "ok": True,
            "source_coverage": {"configured": ["fixture"], "returned": [], "complete": False},
            "context_pack": {
                "query": "reference-only custody",
                "facts": [],
                "results": [],
                "memory_trust_assessment": trust_ref,
                "retrieval_decision_trace": trace_ref,
                "context_compiler": {
                    **module.minimal_context_compiler("reference_only", 0),
                    "memory_trust_assessment": trust_ref,
                    "retrieval_decision_trace": trace_ref,
                },
            },
        }

        compacted = module.compact_context_pack(raw, "reference-only custody", 9000)

        self.assertTrue(compacted["format_contract"]["contract_valid"])
        self.assertIs(compacted["memory_trust_assessment"]["available"], False)
        self.assertIs(compacted["retrieval_decision_trace"]["available"], False)
        self.assertNotEqual(compacted["retrieval_decision_trace"].get("trace_id"), "rdt_reference_only")

    def test_malformed_projected_and_unavailable_proofs_fail_closed_at_every_origin(self) -> None:
        module = load_pack_module()
        for owner in ("root", "nested", "compiler"):
            for shape in ("projected", "unavailable"):
                with self.subTest(owner=owner, shape=shape):
                    assessment = {
                        "schema_id": "memory_trust_assessment.v1",
                        "canonical_path": "$.memory_trust_assessment",
                    }
                    trace = {
                        "schema_id": "retrieval_decision_trace.v1",
                        "canonical_path": "$.retrieval_decision_trace",
                    }
                    if shape == "projected":
                        assessment.update({"bounded_projection": True, "canonical_digest": "sha256:" + "a" * 64})
                        trace.update({"bounded_projection": True, "canonical_digest": "sha256:" + "b" * 64})
                    else:
                        assessment.update({"available": False, "reason": "caller supplied", "note": "must not survive"})
                        trace.update({"available": False, "reason": "caller supplied", "note": "must not survive"})
                    raw = valid_context_pack_response(module)
                    raw.pop("format_contract", None)
                    raw.pop("memory_trust_assessment", None)
                    raw.pop("retrieval_decision_trace", None)
                    if owner == "root":
                        raw["memory_trust_assessment"] = assessment
                        raw["retrieval_decision_trace"] = trace
                    elif owner == "nested":
                        raw["context_pack"]["memory_trust_assessment"] = assessment
                        raw["context_pack"]["retrieval_decision_trace"] = trace
                    else:
                        raw["context_compiler"]["memory_trust_assessment"] = assessment
                        raw["context_compiler"]["retrieval_decision_trace"] = trace
                    attached = module.attach_format_contract("context_pack_response.v1", raw)
                    for field in ("memory_trust_assessment", "retrieval_decision_trace"):
                        proof = attached[field]
                        self.assertIs(proof["available"], False)
                        self.assertEqual(set(proof), {"schema_id", "canonical_path", "available", "reason"})
                        self.assertNotIn("note", proof)
                    self.assertTrue(attached["format_contract"]["contract_valid"])
        for owner in ("root", "nested", "compiler"):
            malformed = {
                "ok": True,
                "source_coverage": {"configured": [], "returned": [], "complete": False},
                "context_pack": {"query": "malformed receipt", "facts": [], "results": []},
            }
            trust = {"schema_id": "memory_trust_assessment.v1", "assessments": []}
            trace = {"schema_id": "retrieval_decision_trace.v1", "decisions": []}
            if owner == "root":
                malformed["memory_trust_assessment"] = trust
                malformed["retrieval_decision_trace"] = trace
            elif owner == "nested":
                malformed["context_pack"]["memory_trust_assessment"] = trust
                malformed["context_pack"]["retrieval_decision_trace"] = trace
            else:
                malformed["context_pack"]["context_compiler"] = {
                    **module.minimal_context_compiler("malformed_receipt", 0),
                    "memory_trust_assessment": trust,
                    "retrieval_decision_trace": trace,
                }
            rejected = module.compact_context_pack(malformed, "malformed receipt", 9000)
            self.assertIs(rejected["memory_trust_assessment"]["available"], False, owner)
            self.assertIs(rejected["retrieval_decision_trace"]["available"], False, owner)

    def test_root_retrieval_references_remain_authoritative_in_non_debug_shape(self) -> None:
        module = load_pack_module()
        trust_ref = {
            "schema_id": "memory_trust_assessment.v1",
            "canonical_path": "$.memory_trust_assessment",
            "assessed_count": 9,
            "quarantine_count": 2,
            "deduplicated_count": 1,
            "policy_omitted_count": 3,
            "input_truncated_count": 4,
        }
        trace_ref = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "trace_id": "rdt_222222222222222222222222",
            "candidate_count": 13,
            "decision_count": 9,
            "input_truncated_count": 4,
            "coverage_complete": False,
        }
        raw = {
            "ok": True,
            "memory_trust_assessment": trust_ref,
            "retrieval_decision_trace": trace_ref,
            "source_coverage": {"configured": ["fixture"], "returned": ["fixture"], "complete": True},
            "context_pack": {"query": "root reference custody", "facts": [], "results": []},
        }

        compacted = module.compact_context_pack(raw, "root reference custody", 9000)

        self.assertTrue(compacted["format_contract"]["contract_valid"])
        self.assertEqual(compacted["memory_trust_assessment"]["assessed_count"], 9)
        self.assertTrue(compacted["memory_trust_assessment"].get("available", True))
        self.assertEqual(compacted["retrieval_decision_trace"]["trace_id"], "rdt_222222222222222222222222")
        self.assertTrue(compacted["retrieval_decision_trace"].get("available", True))

    def test_cli_emission_preserves_canonical_digest_and_redacts_adjacent_secret(self) -> None:
        module = load_pack_module()
        adjacent_secret = "sk-" + "adjacent-secret-value"
        assessment = valid_memory_trust_assessment(module, 140)
        trace = valid_retrieval_decision_trace(module, 140)
        expected_digest = "sha256:" + hashlib.sha256(
            json.dumps(assessment, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()

        def fake_request(method, path, payload, timeout):
            return {
                "ok": True,
                "memory_trust_assessment": assessment,
                "retrieval_decision_trace": trace,
                "source_coverage": {"configured": ["fixture"], "returned": ["fixture"], "complete": True},
                "context_pack": {
                    "query": "public digest custody",
                    "facts": [{"summary": "api_" + "key=" + adjacent_secret}],
                    "results": [],
                },
            }

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "public digest custody", "--no-auto-session", "--budget-chars", "9000"]
        with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertEqual(output["memory_trust_assessment"]["canonical_digest"], expected_digest)
        canonical_public_bytes = len(
            json.dumps(output, sort_keys=True, separators=(",", ":")).encode("utf-8")
        )
        self.assertEqual(output["format_contract"]["actual_json_bytes"], canonical_public_bytes)
        self.assertLessEqual(len(emitted.encode("utf-8")), output["context_budget_chars"])
        self.assertNotIn(adjacent_secret, emitted)

    def test_exact_http_transport_hashes_proof_tail_before_public_redaction(self) -> None:
        module = load_pack_module()
        common_module = sys.modules[module.request_json_for_validation.__module__]

        class FakeHTTPResponse:
            def __init__(self, payload):
                self.body = json.dumps(payload, separators=(",", ":")).encode("utf-8")

            def __enter__(self):
                return self

            def __exit__(self, exc_type, exc, tb):
                return False

            def read(self, _limit=-1):
                return self.body

        digests: list[str] = []
        for tail in ("tail-a", "tail-b"):
            assessment = valid_memory_trust_assessment(module, 140)
            trace = valid_retrieval_decision_trace(module, 140)
            assessment["assessments"][139]["tail_marker"] = tail
            assessment = module.attach_format_contract("memory_trust_assessment.v1", assessment)
            expected_digest = "sha256:" + hashlib.sha256(
                json.dumps(assessment, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
            ).hexdigest()
            server_payload = {
                "ok": True,
                "memory_trust_assessment": assessment,
                "retrieval_decision_trace": trace,
                "source_coverage": {"configured": ["fixture"], "returned": ["fixture"], "complete": True},
                "context_pack": {"query": "exact HTTP proof custody", "facts": [], "results": []},
            }
            stdout = io.StringIO()
            argv = ["contextlattice-pack", "exact HTTP proof custody", "--no-auto-session", "--budget-chars", "9000"]
            with patch.object(common_module.urllib.request, "urlopen", return_value=FakeHTTPResponse(server_payload)), patch.object(sys, "argv", argv), redirect_stdout(stdout):
                self.assertEqual(module.main(), 0)
            emitted = json.loads(stdout.getvalue())
            digest = emitted["memory_trust_assessment"]["canonical_digest"]
            self.assertEqual(digest, expected_digest)
            digests.append(digest)

        self.assertNotEqual(digests[0], digests[1])

    def test_shared_context_contract_projects_full_proof_before_list_enforcement(self) -> None:
        module = load_pack_module()
        assessment = valid_memory_trust_assessment(module, 140)
        trace = valid_retrieval_decision_trace(module, 140)
        expected_digest = "sha256:" + hashlib.sha256(
            json.dumps(assessment, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        ).hexdigest()
        compiler = module.minimal_context_compiler("shared_contract_projection", 0)
        advisor = {
            "schema_id": "run_advisor.v1",
            "posture": "minimal_context",
            "prompt_quality": {},
            "retrieval_advice": {},
            "continuation": {},
            "objective_coherence": {},
        }
        payload = {
            "ok": True,
            "context_pack": {
                "facts": [],
                "results": [],
                "citations": [],
                "ranked_evidence": [],
                "prompt_sections": {},
                "context_compiler": compiler,
                "relevant_decisions": [],
                "files_to_read": [],
                "files_to_avoid": [],
                "capabilities_to_use": [],
                "runbooks": [],
                "known_failure_modes": [],
                "commands": [],
                "acceptance_criteria": [],
            },
            "source_coverage": {"configured": [], "returned": [], "complete": True},
            "context_compiler": compiler,
            "reference_prompt": "shared contract projection",
            "run_advisor": advisor,
            "writeback_required": True,
            "memory_trust_assessment": assessment,
            "retrieval_decision_trace": trace,
        }

        attached = module.attach_format_contract("context_pack_response.v1", payload)

        root = attached["memory_trust_assessment"]
        self.assertTrue(attached["format_contract"]["contract_valid"])
        self.assertTrue(root["bounded_projection"])
        self.assertEqual(root["canonical_digest"], expected_digest)
        self.assertNotIn("assessments", root)
        self.assertEqual(attached["context_pack"]["memory_trust_assessment"]["assessed_count"], 140)

    def test_fractional_receipt_integer_fields_fail_closed_at_every_proof_origin(self) -> None:
        module = load_pack_module()
        for owner in ("root", "nested", "compiler"):
            with self.subTest(owner=owner):
                assessment = valid_memory_trust_assessment(module)
                trace = valid_retrieval_decision_trace(module)
                assessment["version"] = 1.5
                trace["candidate_count"] = 1.5
                self.assertTrue(module.validate_agent_contract_payload("memory_trust_assessment.v1", assessment))
                self.assertTrue(module.validate_agent_contract_payload("retrieval_decision_trace.v1", trace))
                raw = {
                    "ok": True,
                    "source_coverage": {"configured": [], "returned": [], "complete": False},
                    "context_pack": {"query": "fractional proof custody", "facts": [], "results": []},
                }
                if owner == "root":
                    raw["memory_trust_assessment"] = assessment
                    raw["retrieval_decision_trace"] = trace
                elif owner == "nested":
                    raw["context_pack"]["memory_trust_assessment"] = assessment
                    raw["context_pack"]["retrieval_decision_trace"] = trace
                else:
                    raw["context_pack"]["context_compiler"] = {
                        **module.minimal_context_compiler("fractional_receipt", 0),
                        "memory_trust_assessment": assessment,
                        "retrieval_decision_trace": trace,
                    }

                compacted = module.compact_context_pack(raw, "fractional proof custody", 9000)
                self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

        integral_float_assessment = valid_memory_trust_assessment(module)
        integral_float_trace = valid_retrieval_decision_trace(module)
        integral_float_assessment["version"] = 1.0
        integral_float_trace["candidate_count"] = 1.0
        self.assertTrue(module.validate_agent_contract_payload("memory_trust_assessment.v1", integral_float_assessment))
        self.assertTrue(module.validate_agent_contract_payload("retrieval_decision_trace.v1", integral_float_trace))
        self.assertEqual(module.canonical_retrieval_proof(integral_float_assessment, "memory_trust_assessment"), {})
        self.assertEqual(module.canonical_retrieval_proof(integral_float_trace, "retrieval_decision_trace"), {})

    def test_impossible_retrieval_count_relationships_fail_closed_at_every_origin(self) -> None:
        module = load_pack_module()
        for owner in ("root", "nested", "compiler"):
            with self.subTest(owner=owner):
                assessment = valid_memory_trust_assessment(module, 1)
                trace = valid_retrieval_decision_trace(module, 1)
                assessment["quarantine_count"] = 1
                assessment["deduplicated_count"] = 1
                trace["processed_candidate_count"] = 0
                trace["coverage_complete"] = False
                self.assertTrue(
                    any(
                        finding.get("reason") == "retrieval_proof_count_invariant_mismatch"
                        for finding in module.validate_agent_contract_payload(
                            "memory_trust_assessment.v1", assessment
                        )
                    )
                )
                self.assertTrue(
                    any(
                        finding.get("reason") == "retrieval_proof_count_invariant_mismatch"
                        for finding in module.validate_agent_contract_payload(
                            "retrieval_decision_trace.v1", trace
                        )
                    )
                )

                raw = {
                    "ok": True,
                    "source_coverage": {"configured": [], "returned": [], "complete": True},
                    "context_pack": {"query": "impossible receipt counts", "facts": [], "results": []},
                }
                if owner == "root":
                    raw["memory_trust_assessment"] = assessment
                    raw["retrieval_decision_trace"] = trace
                elif owner == "nested":
                    raw["context_pack"]["memory_trust_assessment"] = assessment
                    raw["context_pack"]["retrieval_decision_trace"] = trace
                else:
                    raw["context_pack"]["context_compiler"] = {
                        **module.minimal_context_compiler("impossible_receipt_counts", 0),
                        "memory_trust_assessment": assessment,
                        "retrieval_decision_trace": trace,
                    }
                compacted = module.compact_context_pack(raw, "impossible receipt counts", 50000)
                self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

    def test_retrieval_receipt_cardinality_and_category_histogram_fail_closed_at_every_origin(self) -> None:
        module = load_pack_module()
        mutations = {
            "trust list cardinality": lambda assessment, trace: assessment.__setitem__("assessments", []),
            "trace list cardinality": lambda assessment, trace: trace.__setitem__("decisions", []),
            "trace category histogram": lambda assessment, trace: trace["decisions"][0].__setitem__(
                "decision", "omitted"
            ),
        }
        for owner in ("root", "nested", "compiler"):
            for name, mutate in mutations.items():
                with self.subTest(owner=owner, mutation=name):
                    assessment = valid_memory_trust_assessment(module, 1)
                    trace = valid_retrieval_decision_trace(module, 1)
                    mutate(assessment, trace)
                    assessment_findings = module.validate_agent_contract_payload(
                        "memory_trust_assessment.v1", assessment
                    )
                    trace_findings = module.validate_agent_contract_payload(
                        "retrieval_decision_trace.v1", trace
                    )
                    self.assertTrue(assessment_findings or trace_findings)

                    raw = {
                        "ok": True,
                        "source_coverage": {"configured": [], "returned": [], "complete": True},
                        "context_pack": {"query": "receipt semantic mismatch", "facts": [], "results": []},
                    }
                    if owner == "root":
                        raw["memory_trust_assessment"] = assessment
                        raw["retrieval_decision_trace"] = trace
                    elif owner == "nested":
                        raw["context_pack"]["memory_trust_assessment"] = assessment
                        raw["context_pack"]["retrieval_decision_trace"] = trace
                    else:
                        raw["context_pack"]["context_compiler"] = {
                            **module.minimal_context_compiler("receipt_semantic_mismatch", 0),
                            "memory_trust_assessment": assessment,
                            "retrieval_decision_trace": trace,
                        }
                    compacted = module.compact_context_pack(raw, "receipt semantic mismatch", 50000)
                    if assessment_findings:
                        self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                    if trace_findings:
                        self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

    def test_duplicate_retrieval_receipt_identities_fail_closed(self) -> None:
        module = load_pack_module()
        assessment = valid_memory_trust_assessment(module, 2)
        trace = valid_retrieval_decision_trace(module, 2)
        assessment["assessments"][1]["assessment_id"] = assessment["assessments"][0]["assessment_id"]
        assessment["assessments"][1]["candidate_id"] = assessment["assessments"][0]["candidate_id"]
        trace["decisions"][1]["receipt_id"] = trace["decisions"][0]["receipt_id"]
        trace["decisions"][1]["candidate_id"] = trace["decisions"][0]["candidate_id"]
        trace["decisions"][1]["candidate_ordinal"] = trace["decisions"][0]["candidate_ordinal"]
        self.assertTrue(module.validate_agent_contract_payload("memory_trust_assessment.v1", assessment))
        self.assertTrue(module.validate_agent_contract_payload("retrieval_decision_trace.v1", trace))
        self.assertEqual(module.canonical_retrieval_proof(assessment, "memory_trust_assessment"), {})
        self.assertEqual(module.canonical_retrieval_proof(trace, "retrieval_decision_trace"), {})

    def test_registered_contract_type_validation_distinguishes_null_from_missing(self) -> None:
        module = load_adapter_module()
        base = module.adapter_response(
            command="profiles",
            ok=True,
            project="alpha",
            session_id="",
            agent="codex",
            agent_id="",
            result={"status": "available"},
        )
        for field in ("ok", "command", "project", "findings"):
            with self.subTest(field=field):
                mutated = json.loads(json.dumps(base))
                mutated[field] = None
                findings = module.validate_agent_contract_payload(
                    "universal_agent_adapter_response.v1", mutated
                )
                self.assertTrue(
                    any(finding.get("reason") == "field_type_mismatch" for finding in findings),
                    findings,
                )

    def test_retrieval_proof_pair_mismatches_fail_closed_at_every_origin(self) -> None:
        module = load_pack_module()
        for owner in ("root", "nested", "compiler"):
            for mismatch in (
                "candidate counts",
                "candidate identity",
                "large candidate identity",
                "quarantine disposition",
            ):
                with self.subTest(owner=owner, mismatch=mismatch):
                    count = 65 if mismatch == "large candidate identity" else 1
                    assessment = valid_memory_trust_assessment(module, count)
                    trace = valid_retrieval_decision_trace(
                        module,
                        2 if mismatch == "candidate counts" else count,
                    )
                    if mismatch == "candidate identity":
                        trace["decisions"][0]["candidate_id"] = "rtc_ffffffffffffffffffffffff"
                    elif mismatch == "large candidate identity":
                        trace["decisions"][-1]["candidate_id"] = "rtc_ffffffffffffffffffffffff"
                    elif mismatch == "quarantine disposition":
                        assessment["assessments"][0]["quarantine"]["quarantined"] = True
                        assessment["quarantine_count"] = 1
                    assessment = module.attach_format_contract(
                        "memory_trust_assessment.v1",
                        {key: value for key, value in assessment.items() if key != "format_contract"},
                    )
                    trace = module.attach_format_contract(
                        "retrieval_decision_trace.v1",
                        {key: value for key, value in trace.items() if key != "format_contract"},
                    )
                    self.assertEqual(
                        module.validate_agent_contract_payload("memory_trust_assessment.v1", assessment),
                        [],
                    )
                    self.assertEqual(
                        module.validate_agent_contract_payload("retrieval_decision_trace.v1", trace),
                        [],
                    )

                    raw = {
                        "ok": True,
                        "source_coverage": {"configured": [], "returned": [], "complete": True},
                        "context_pack": {"query": "proof pair mismatch", "facts": [], "results": []},
                    }
                    if owner == "root":
                        raw["memory_trust_assessment"] = assessment
                        raw["retrieval_decision_trace"] = trace
                    elif owner == "nested":
                        raw["context_pack"]["memory_trust_assessment"] = assessment
                        raw["context_pack"]["retrieval_decision_trace"] = trace
                    else:
                        raw["context_pack"]["context_compiler"] = {
                            **module.minimal_context_compiler("proof_pair_mismatch", 0),
                            "memory_trust_assessment": assessment,
                            "retrieval_decision_trace": trace,
                        }
                    compacted = module.compact_context_pack(raw, "proof pair mismatch", 50000)
                    self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                    self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

    def test_retrieval_proof_pair_never_combines_different_origins(self) -> None:
        module = load_pack_module()
        nested_assessment = valid_memory_trust_assessment(module)
        nested_trace = valid_retrieval_decision_trace(module)
        root_assessment = {
            "schema_id": "memory_trust_assessment.v1",
            "canonical_path": "$.memory_trust_assessment",
            "assessed_count": 1,
            "quarantine_count": 0,
            "deduplicated_count": 0,
            "policy_omitted_count": 0,
            "input_truncated_count": 0,
        }
        root_trace = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "trace_id": "rdt_0123456789abcdef01234567",
            "candidate_count": 1,
            "decision_count": 1,
            "input_truncated_count": 0,
            "coverage_complete": True,
        }
        for label, root in (
            ("root trust only", {"memory_trust_assessment": root_assessment}),
            ("root trace only", {"retrieval_decision_trace": root_trace}),
        ):
            with self.subTest(label=label):
                raw = {
                    "ok": True,
                    **root,
                    "source_coverage": {"configured": [], "returned": [], "complete": True},
                    "context_pack": {
                        "query": "mixed proof origins",
                        "facts": [],
                        "results": [],
                        "memory_trust_assessment": nested_assessment,
                        "retrieval_decision_trace": nested_trace,
                    },
                }
                compacted = module.compact_context_pack(raw, "mixed proof origins", 50000)
                self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

    def test_retrieval_proof_projection_and_root_reference_pairs_reconcile(self) -> None:
        module = load_pack_module()
        full_assessment = valid_memory_trust_assessment(module, 1)
        full_trace = valid_retrieval_decision_trace(module, 1)
        projected_assessment = module.bounded_retrieval_proof(
            full_assessment,
            "memory_trust_assessment",
            0,
        )
        projected_trace = module.bounded_retrieval_proof(
            full_trace,
            "retrieval_decision_trace",
            0,
        )
        projected_trace.update(
            {
                "candidate_count": 2,
                "decision_count": 1,
                "input_truncated_count": 1,
                "coverage_complete": False,
            }
        )
        reference_assessment = {
            "schema_id": "memory_trust_assessment.v1",
            "canonical_path": "$.memory_trust_assessment",
            "assessed_count": 1,
            "quarantine_count": 0,
            "deduplicated_count": 0,
            "policy_omitted_count": 0,
            "input_truncated_count": 0,
        }
        reference_trace = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "trace_id": "rdt_0123456789abcdef01234567",
            "candidate_count": 3,
            "decision_count": 2,
            "input_truncated_count": 1,
            "coverage_complete": False,
        }
        for shape, assessment, trace in (
            ("projection", projected_assessment, projected_trace),
            ("reference", reference_assessment, reference_trace),
            (
                "available trust with unavailable trace",
                reference_assessment,
                {
                    "schema_id": "retrieval_decision_trace.v1",
                    "canonical_path": "$.retrieval_decision_trace",
                    "available": False,
                    "reason": "trace missing",
                },
            ),
            (
                "unavailable trust with available trace",
                {
                    "schema_id": "memory_trust_assessment.v1",
                    "canonical_path": "$.memory_trust_assessment",
                    "available": False,
                    "reason": "assessment missing",
                },
                reference_trace,
            ),
        ):
            with self.subTest(shape=shape):
                raw = {
                    "ok": True,
                    "memory_trust_assessment": assessment,
                    "retrieval_decision_trace": trace,
                    "source_coverage": {"configured": [], "returned": [], "complete": True},
                    "context_pack": {"query": "summary pair mismatch", "facts": [], "results": []},
                }
                compacted = module.compact_context_pack(raw, "summary pair mismatch", 50000)
                self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

    def test_retrieval_proof_forbidden_fields_scan_full_contract_bound(self) -> None:
        module = load_pack_module()
        assessment = valid_memory_trust_assessment(module, 129)
        assessment["assessments"][128]["Private-Key"] = "opaque-forbidden-value"
        findings = module.validate_agent_contract_payload(
            "memory_trust_assessment.v1",
            assessment,
        )
        self.assertTrue(
            any(
                finding.get("reason") == "forbidden_field_present"
                and "Private-Key" in str(finding.get("path"))
                for finding in findings
            ),
            findings,
        )

    def test_retrieval_policy_and_input_boundary_semantics_fail_closed(self) -> None:
        module = load_pack_module()
        mutations = {
            "retrieved memory remains evidence": lambda assessment, trace: assessment["policy"].__setitem__(
                "retrieved_memory_is_evidence_not_instruction", False
            ),
            "security defenses remain fail closed": lambda assessment, trace: assessment["policy"].__setitem__(
                "security_defenses_fail_closed", False
            ),
            "trust omitted count reconciles": lambda assessment, trace: assessment["input_boundary"].__setitem__(
                "omitted_count", 1
            ),
            "trace truncated flag reconciles": lambda assessment, trace: trace["input_boundary"].__setitem__(
                "truncated", True
            ),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                assessment = valid_memory_trust_assessment(module, 1)
                trace = valid_retrieval_decision_trace(module, 1)
                mutate(assessment, trace)
                assessment_findings = module.validate_agent_contract_payload(
                    "memory_trust_assessment.v1", assessment
                )
                trace_findings = module.validate_agent_contract_payload(
                    "retrieval_decision_trace.v1", trace
                )
                self.assertTrue(assessment_findings or trace_findings)
                assessment_canonical = module.canonical_retrieval_proof(
                    assessment, "memory_trust_assessment"
                )
                trace_canonical = module.canonical_retrieval_proof(
                    trace, "retrieval_decision_trace"
                )
                self.assertEqual(bool(assessment_canonical), not bool(assessment_findings))
                self.assertEqual(bool(trace_canonical), not bool(trace_findings))

    def test_full_retrieval_proof_requires_exact_format_contract_provenance(self) -> None:
        module = load_pack_module()
        mutations = {
            "registry id": ("registry_id", "other_registry"),
            "registry version": ("registry_version", 1),
            "contract version": ("contract_version", 2),
            "output mode": ("required_output_mode", "text"),
            "validator": ("validator", "other.validator"),
            "maximum total bytes": ("max_total_json_bytes", 1),
            "maximum string bytes": ("max_string_bytes", 1),
            "maximum list items": ("max_list_items", 1),
            "actual bytes": ("actual_json_bytes", 1),
        }
        for name, (field, value) in mutations.items():
            with self.subTest(name=name):
                assessment = valid_memory_trust_assessment(module)
                trace = valid_retrieval_decision_trace(module)
                assessment["format_contract"][field] = value
                trace["format_contract"][field] = value
                self.assertEqual(module.canonical_retrieval_proof(assessment, "memory_trust_assessment"), {})
                self.assertEqual(module.canonical_retrieval_proof(trace, "retrieval_decision_trace"), {})
                contracts_module = sys.modules[module.attach_format_contract.__module__]
                self.assertEqual(
                    contracts_module._canonical_retrieval_proof(assessment, "memory_trust_assessment"),
                    {},
                )
                self.assertEqual(
                    contracts_module._canonical_retrieval_proof(trace, "retrieval_decision_trace"),
                    {},
                )

    def test_public_identity_alias_cannot_bypass_canonical_identity_authority(self) -> None:
        module = load_adapter_module()
        private_alias = "session-" + "a" * 32
        projected = module.public_context_pack_identities(
            {
                "sessionId": private_alias,
                "agentId": "agent-other",
                "context_pack": {
                    "session_id": "session-response-owned",
                    "agent_id": "agent-response-owned",
                    "nested": {"sessionId": private_alias, "agentId": "agent-other"},
                },
            },
            authoritative_fields={"session_id": "session-safe", "agent_id": "agent-safe"},
        )
        self.assertNotIn("sessionId", projected)
        self.assertNotIn("agentId", projected)
        self.assertEqual(projected["session_id"], "session-safe")
        self.assertEqual(projected["agent_id"], "agent-safe")
        self.assertEqual(projected["context_pack"]["session_id"], "session-safe")
        self.assertEqual(projected["context_pack"]["agent_id"], "agent-safe")
        self.assertNotIn("sessionId", projected["context_pack"]["nested"])
        self.assertNotIn("agentId", projected["context_pack"]["nested"])
        public = module.redact_public_value({"sessionId": private_alias})
        self.assertNotEqual(public.get("sessionId"), private_alias)

    def test_adapter_transport_system_exit_is_wrapped_in_universal_v2_failure(self) -> None:
        module = load_adapter_module()
        stdout = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--project",
            "alpha",
            "--agent",
            "codex",
            "--agent-id",
            "agent-safe",
            "--session-id",
            "session-safe",
        ]
        with patch.object(module, "main", side_effect=SystemExit('{"ok":false,"error":"gateway_response_invalid"}')), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout):
            self.assertEqual(module._entrypoint(), 1)
        output = json.loads(stdout.getvalue())
        self.assertIs(output["ok"], False)
        self.assertEqual(output["format_contract"]["contract_version"], 2)
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(output["findings"], [{"reason": "gateway_response_invalid"}])

        stdout = io.StringIO()
        with patch.object(module, "main", side_effect=RecursionError("deep response")), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout):
            self.assertEqual(module._entrypoint(), 1)
        output = json.loads(stdout.getvalue())
        self.assertIs(output["ok"], False)
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(output["findings"], [{"reason": "adapter_command_failed"}])

    def test_generic_number_contract_rejects_nonfinite_python_values(self) -> None:
        module = load_pack_module()
        contracts_module = sys.modules[module.attach_format_contract.__module__]
        for value in (float("nan"), float("inf"), float("-inf")):
            with self.subTest(value=value):
                self.assertFalse(contracts_module._matches_type(value, "number"))
        self.assertTrue(contracts_module._matches_type(0.75, "number"))
        self.assertTrue(contracts_module._matches_type(1, "number"))
        self.assertFalse(contracts_module._matches_type(1 << 63, "number"))

    def test_out_of_range_receipt_integer_fields_fail_closed_in_pack_and_shared_contract(self) -> None:
        module = load_pack_module()
        for huge in (1 << 63, -(1 << 63) - 1):
            for owner in ("root", "nested", "compiler"):
                with self.subTest(owner=owner, huge=huge):
                    assessment = valid_memory_trust_assessment(module)
                    trace = valid_retrieval_decision_trace(module)
                    assessment["assessed_count"] = huge
                    trace["decision_count"] = huge
                    self.assertTrue(module.validate_agent_contract_payload("memory_trust_assessment.v1", assessment))
                    self.assertTrue(module.validate_agent_contract_payload("retrieval_decision_trace.v1", trace))
                    raw = {
                        "ok": True,
                        "source_coverage": {"configured": [], "returned": [], "complete": False},
                        "context_pack": {"query": "out-of-range proof custody", "facts": [], "results": []},
                    }
                    if owner == "root":
                        raw["memory_trust_assessment"] = assessment
                        raw["retrieval_decision_trace"] = trace
                    elif owner == "nested":
                        raw["context_pack"]["memory_trust_assessment"] = assessment
                        raw["context_pack"]["retrieval_decision_trace"] = trace
                    else:
                        raw["context_pack"]["context_compiler"] = {
                            **module.minimal_context_compiler("out_of_range_receipt", 0),
                            "memory_trust_assessment": assessment,
                            "retrieval_decision_trace": trace,
                        }

                    compacted = module.compact_context_pack(raw, "out-of-range proof custody", 9000)
                    self.assertIs(compacted["memory_trust_assessment"]["available"], False)
                    self.assertIs(compacted["retrieval_decision_trace"]["available"], False)

                direct = module.compact_context_pack(
                    {
                        "ok": True,
                        "source_coverage": {"configured": [], "returned": [], "complete": False},
                        "context_pack": {"query": "shared proof custody", "facts": [], "results": []},
                    },
                    "shared proof custody",
                    50000,
                )
                if owner == "root":
                    direct["memory_trust_assessment"] = assessment
                    direct["retrieval_decision_trace"] = trace
                elif owner == "nested":
                    direct["context_pack"]["memory_trust_assessment"] = assessment
                    direct["context_pack"]["retrieval_decision_trace"] = trace
                else:
                    direct["context_compiler"]["memory_trust_assessment"] = assessment
                    direct["context_compiler"]["retrieval_decision_trace"] = trace
                shared = module.attach_format_contract("context_pack_response.v1", direct)
                self.assertIs(shared["memory_trust_assessment"]["available"], False)
                self.assertIs(shared["retrieval_decision_trace"]["available"], False)

    def test_raw_success_advisor_and_quality_fields_are_safe_and_quality_truth_survives(self) -> None:
        module = load_pack_module()
        advisor_secret = "sk-RAW-SUCCESS-ADVISOR-SECRET-123456"
        quality_secret = "sk-RAW-QUALITY-SECRET-123456"
        quality = {
            "schema_id": "contextlattice_context_pack_quality.v1",
            "version": 1,
            "capturedAt": "2026-08-12T12:00:00Z",
            "sample_id": "cpq_safe_projection",
            "query_hash": "0123456789abcdef",
            "project": "contextlattice",
            "task_class": "coding",
            "retrieval_intent": "decision",
            "quality_score": 91,
            "confidence": "high",
            "calibration_grade": "tokenizer_exact",
            "exact_prompt_tokens_saved": 1234,
            "modeled_inference_tokens_avoided": 567,
            "modeled_extra_calls_avoided": 1.25,
            "tokenizer_exact": True,
            "selection_receipt": {
                "receipt_id": "receipt-safe",
                "receipt_digest": "sha256:" + "a" * 64,
            },
            "debug_secret": quality_secret,
        }

        def fake_request(method, path, payload, timeout):
            return {
                "ok": True,
                "run_advisor": {
                    "prompt_quality": {"score": advisor_secret},
                },
                "context_pack_quality": quality,
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "safe raw success", "facts": [], "results": []},
            }

        written: list[dict] = []
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "safe raw success",
            "--no-auto-session",
            "--session-id",
            "session-safe-quality",
            "--budget-chars",
            "50000",
        ]
        with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(
            module, "read_agent_session_state", return_value={}
        ), patch.object(module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertLessEqual(len(emitted.encode("utf-8")), 50000)
        self.assertNotIn(advisor_secret, emitted + stderr.getvalue())
        self.assertNotIn(quality_secret, emitted + stderr.getvalue())
        projected = output["context_pack_quality"]
        for field in (
            "confidence",
            "calibration_grade",
            "exact_prompt_tokens_saved",
            "modeled_inference_tokens_avoided",
            "modeled_extra_calls_avoided",
            "tokenizer_exact",
            "selection_receipt",
        ):
            self.assertEqual(projected[field], quality[field], field)
        self.assertEqual(projected["debug_secret"], "[REDACTED]")
        self.assertEqual(len(written), 1)
        pending = written[0]["latest_context_pack_quality"]
        self.assertEqual(
            pending,
            {
                "sample_id": "cpq_safe_projection",
                "query_hash": "0123456789abcdef",
                "quality_score": 91,
                "captured_at": "2026-08-12T12:00:00Z",
                "reported": False,
            },
        )
        advisor = output["run_advisor"]
        advisor_public_bytes = len(json.dumps(advisor, sort_keys=True, separators=(",", ":")).encode("utf-8"))
        self.assertEqual(advisor["format_contract"]["actual_json_bytes"], advisor_public_bytes)
        self.assertTrue(advisor["format_contract"]["contract_valid"])
        self.assertIsInstance(advisor["objective_coherence"]["signals"], dict)
        self.assertIsInstance(advisor["graph_quality"]["signals"], dict)

    def test_generated_session_identity_is_omitted_before_derived_output_or_state(self) -> None:
        module = load_pack_module()
        generated_session_id = "sess_" + "a" * 32
        quality = {
            "schema_id": "contextlattice_context_pack_quality.v1",
            "version": 1,
            "capturedAt": "2026-08-12T12:00:00Z",
            "sample_id": "cpq_generated_session",
            "query_hash": "0123456789abcdef",
            "quality_score": 90,
        }

        def fake_request(method, path, payload, timeout):
            return {
                "ok": True,
                "session_id": generated_session_id,
                "context_pack_quality": quality,
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "generated identity custody", "facts": [], "results": []},
            }

        written: list[dict] = []
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "generated identity custody",
            "--no-auto-session",
            "--session-id",
            generated_session_id,
            "--budget-chars",
            "50000",
        ]
        with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(
            module, "read_agent_session_state", return_value={}
        ), patch.object(module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertNotIn(generated_session_id, emitted + stderr.getvalue())
        self.assertNotIn("session_id", output)
        self.assertIn("session_id", output.get("identity_omitted", []))
        self.assertNotIn("outcome_report", output)
        self.assertEqual(written, [])
        self.assertTrue(output["format_contract"]["contract_valid"])

    def test_non_string_identities_are_omitted_instead_of_stringified(self) -> None:
        module = load_pack_module()
        for value in (7, True, {"id": "agent"}, ["session"]):
            with self.subTest(value=value):
                identity, omitted = module.bounded_identity(value, "session_id")
                self.assertEqual(identity, "")
                self.assertTrue(omitted)

    def test_adapter_hashes_raw_small_proof_and_persists_only_validated_quality(self) -> None:
        module = load_adapter_module()
        proof = valid_memory_trust_assessment(module)
        proof_secret = "sk-" + "adapter-proof-custody"
        proof["assessments"][0]["summary"] = proof_secret
        proof = module.attach_format_contract("memory_trust_assessment.v1", proof)
        expected_digest = "sha256:" + hashlib.sha256(
            json.dumps(proof, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()
        quality = {
            "schema_id": "contextlattice_context_pack_quality.v1",
            "version": 1,
            "capturedAt": "2026-08-12T12:00:00Z",
            "sample_id": "cpq_adapter_safe",
            "query_hash": "0123456789abcdef",
            "quality_score": 91,
            "debug_secret": "sk-" + "adapter-quality-private",
        }
        raw = valid_context_pack_response(module, assessment=proof, quality=quality, query="adapter proof custody")
        event_calls: list[dict] = []
        written: list[dict] = []

        def fake_event(method, path, payload, timeout):
            event_calls.append(payload)
            return {"ok": True}

        common_module = sys.modules[module.request_json_for_validation.__module__]
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--project",
            "contextlattice",
            "--session-id",
            "session-adapter-safe",
            "--query",
            "adapter proof custody",
            "--include-retrieval-debug",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "request_json", side_effect=fake_event
        ), patch.object(module, "profile_payload", return_value=("codex", {}, "agent-safe", "", "adapter proof custody", "balanced")), patch.object(
            common_module, "read_agent_session_state", return_value={}
        ), patch.object(common_module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        projected = output["result"]["context_pack"]["memory_trust_assessment"]
        self.assertEqual(projected["canonical_digest"], expected_digest)
        self.assertTrue(projected["bounded_projection"])
        self.assertNotIn(proof_secret, emitted + stderr.getvalue())
        self.assertNotIn(quality["debug_secret"], emitted + stderr.getvalue())
        self.assertEqual(len(written), 1)
        self.assertEqual(
            written[0]["latest_context_pack_quality"],
            {
                "sample_id": "cpq_adapter_safe",
                "query_hash": "0123456789abcdef",
                "quality_score": 91,
                "captured_at": "2026-08-12T12:00:00Z",
                "reported": False,
            },
        )
        self.assertEqual(event_calls[0]["metadata"]["context_pack_quality_sample_id"], "cpq_adapter_safe")
        self.assertEqual(output["result"]["outcome_report"]["sample_id"], "cpq_adapter_safe")
        nested_pack = output["result"]["context_pack"]
        nested_public_bytes = len(json.dumps(nested_pack, sort_keys=True, separators=(",", ":")).encode("utf-8"))
        self.assertEqual(nested_pack["format_contract"]["actual_json_bytes"], nested_public_bytes)
        nested_advisor = nested_pack["run_advisor"]
        nested_advisor_bytes = len(json.dumps(nested_advisor, sort_keys=True, separators=(",", ":")).encode("utf-8"))
        self.assertEqual(nested_advisor["format_contract"]["actual_json_bytes"], nested_advisor_bytes)
        self.assertTrue(nested_advisor["format_contract"]["contract_valid"])
        output_public_bytes = len(json.dumps(output, sort_keys=True, separators=(",", ":")).encode("utf-8"))
        self.assertEqual(output["format_contract"]["actual_json_bytes"], output_public_bytes)

    def test_adapter_rejects_raw_quality_from_state_event_and_derived_output(self) -> None:
        module = load_adapter_module()
        raw_secret = "sk-" + "adapter-malformed-quality"
        raw = valid_context_pack_response(
            module,
            quality={
                "sample_id": "cpq_adapter_malformed",
                "query_hash": raw_secret,
                "quality_score": raw_secret,
                "capturedAt": "2026-08-12T12:00:00Z",
            },
            query="adapter malformed quality",
        )
        event_calls: list[dict] = []
        written: list[dict] = []

        def fake_event(method, path, payload, timeout):
            event_calls.append(payload)
            return {"ok": True}

        common_module = sys.modules[module.request_json_for_validation.__module__]
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--session-id",
            "sess_" + "b" * 32,
            "--query",
            "adapter malformed quality",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "request_json", side_effect=fake_event
        ), patch.object(module, "profile_payload", return_value=("codex", {}, "agent-safe", "", "adapter malformed quality", "balanced")), patch.object(
            common_module, "read_agent_session_state", return_value={}
        ), patch.object(common_module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertNotIn(raw_secret, emitted + stderr.getvalue() + json.dumps(event_calls))
        self.assertEqual(written, [])
        self.assertEqual(event_calls[0]["metadata"]["context_pack_quality_sample_id"], "")
        self.assertEqual(output["result"].get("outcome_report"), {})
        self.assertIn("session_id", output.get("identity_omitted", []))
        self.assertTrue(output["format_contract"]["contract_valid"])

    def test_adapter_omits_unsafe_identity_from_outer_and_nested_contracts(self) -> None:
        module = load_adapter_module()
        generated_session_id = "sess_" + "c" * 32
        raw = valid_context_pack_response(module, query="adapter nested identity custody")
        raw["session_id"] = generated_session_id
        raw["agent_id"] = "agent-safe"

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--session-id",
            generated_session_id,
            "--query",
            "adapter nested identity custody",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "request_json", return_value={"ok": True}
        ), patch.object(
            module,
            "profile_payload",
            return_value=("codex", {}, "agent-safe", "", "adapter nested identity custody", "balanced"),
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        nested = output["result"]["context_pack"]
        self.assertNotIn(generated_session_id, emitted + stderr.getvalue())
        self.assertNotIn("session_id", output)
        self.assertNotIn("session_id", nested)
        self.assertIn("session_id", output.get("identity_omitted", []))
        self.assertIn("session_id", nested.get("identity_omitted", []))
        self.assertEqual(nested.get("agent_id"), "agent-safe")
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertTrue(nested["format_contract"]["contract_valid"])

    def test_adapter_response_identities_are_bound_to_request_authority(self) -> None:
        module = load_adapter_module()
        quality = {
            "schema_id": "contextlattice_context_pack_quality.v1",
            "version": 1,
            "capturedAt": "2026-08-12T12:00:00Z",
            "sample_id": "cpq_adapter_identity_authority",
            "query_hash": "0123456789abcdef",
            "quality_score": 90,
        }
        raw = valid_context_pack_response(module, quality=quality, query="adapter identity authority")
        raw["session_id"] = "session-response-other"
        raw["agent_id"] = "agent-response-other"
        event_calls: list[dict] = []
        written: list[dict] = []

        def fake_event(method, path, payload, timeout):
            event_calls.append(payload)
            return {"ok": True}

        common_module = sys.modules[module.request_json_for_validation.__module__]
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--session-id",
            "session-request-authority",
            "--agent-id",
            "agent-request-authority",
            "--query",
            "adapter identity authority",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "request_json", side_effect=fake_event
        ), patch.object(
            module,
            "profile_payload",
            return_value=("codex", {}, "agent-request-authority", "", "adapter identity authority", "balanced"),
        ), patch.object(common_module, "read_agent_session_state", return_value={}), patch.object(
            common_module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        nested = output["result"]["context_pack"]
        self.assertEqual(output["session_id"], "session-request-authority")
        self.assertEqual(output["agent_id"], "agent-request-authority")
        self.assertEqual(nested["session_id"], "session-request-authority")
        self.assertEqual(nested["agent_id"], "agent-request-authority")
        self.assertNotIn("session-response-other", emitted + stderr.getvalue())
        self.assertNotIn("agent-response-other", emitted + stderr.getvalue())
        self.assertEqual(event_calls[0]["session_id"], "session-request-authority")
        self.assertEqual(event_calls[0]["agent_id"], "agent-request-authority")
        self.assertEqual(written[0]["session_id"], "session-request-authority")
        self.assertEqual(written[0]["agent_id"], "agent-request-authority")

    def test_every_python_adapter_response_uses_the_post_redaction_v2_gate(self) -> None:
        module = load_adapter_module()
        commands = ("bootstrap", "event", "checkpoint", "handoff", "outcome", "complete", "profiles")
        for command in commands:
            with self.subTest(command=command, shape="ordinary"):
                output = module.adapter_response(
                    command=command,
                    ok=True,
                    project="alpha",
                    session_id="session-safe",
                    agent="codex",
                    agent_id="agent-safe",
                    result={"status": "accepted"},
                )
                self.assertIs(output.get("ok"), True)
                self.assertTrue(module.public_contract_fits(output))
                self.assertEqual(output["format_contract"]["contract_version"], 2)

            with self.subTest(command=command, shape="public expansion"):
                expanding_note = json.dumps(
                    {f"api_key_{index}": "" for index in range(500)},
                    separators=(",", ":"),
                )
                output = module.adapter_response(
                    command=command,
                    ok=True,
                    project="alpha",
                    session_id="session-safe",
                    agent="codex",
                    agent_id="agent-safe",
                    result={"note": expanding_note},
                )
                self.assertIs(output.get("ok"), False)
                self.assertTrue(module.public_contract_fits(output))
                self.assertNotIn(expanding_note, json.dumps(output))

    def test_adapter_checkpoint_rejection_never_posts_writeback_completed(self) -> None:
        module = load_adapter_module()
        calls: list[str] = []

        def fake_request(method, path, payload, timeout):
            calls.append(path)
            if path == "/memory/write":
                return {"ok": False}
            self.fail(f"unexpected request after rejected write: {path}")

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "checkpoint",
            "--session-id",
            "session-checkpoint-rejected",
            "--agent-id",
            "agent-safe",
            "--content",
            "durable checkpoint content",
        ]
        with patch.object(module, "request_json", side_effect=fake_request), patch.object(
            module,
            "profile_payload",
            return_value=("codex", {}, "agent-safe", "agent/checkpoints", "checkpoint", "balanced"),
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 1)

        output = json.loads(stdout.getvalue())
        self.assertEqual(calls, ["/memory/write"])
        self.assertIs(output["ok"], False)
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(stderr.getvalue(), "")

    def test_adapter_checkpoint_success_keeps_nested_public_writeback_contract_valid(self) -> None:
        module = load_adapter_module()
        calls: list[str] = []

        def fake_request(method, path, payload, timeout):
            calls.append(path)
            if path == "/memory/write":
                return {"ok": True, "id": "write-checkpoint-safe"}
            if path == "/v1/agents/sessions/event":
                return {"ok": True, "event": {"id": "event-checkpoint-safe", "type": "writeback.completed"}}
            self.fail(f"unexpected request: {path}")

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "checkpoint",
            "--session-id",
            "session-checkpoint-safe",
            "--agent-id",
            "agent-safe",
            "--file",
            "notes/agent/checkpoint.md",
            "--content",
            "durable checkpoint content",
        ]
        with patch.object(module, "request_json", side_effect=fake_request), patch.object(
            module,
            "profile_payload",
            return_value=("codex", {}, "agent-safe", "agent/checkpoints", "checkpoint", "balanced"),
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        output = json.loads(stdout.getvalue())
        writeback = output["result"]["writeback"]
        self.assertEqual(calls, ["/memory/write", "/v1/agents/sessions/event"])
        self.assertIs(output["ok"], True)
        self.assertTrue(module.public_contract_fits(output))
        self.assertTrue(module.public_contract_valid(writeback))
        self.assertEqual(module.validate_agent_contract_payload("writeback_result.v1", writeback), [])
        nested_bytes = len(json.dumps(writeback, sort_keys=True, separators=(",", ":")).encode("utf-8"))
        self.assertEqual(writeback["format_contract"]["actual_json_bytes"], nested_bytes)
        self.assertEqual(stderr.getvalue(), "")

    def test_adapter_handoff_rejects_empty_or_malformed_success_before_event(self) -> None:
        module = load_adapter_module()
        for label, stdout_value in (("empty", ""), ("malformed", "not-json"), ("incomplete", '{"schema_version":1}')):
            with self.subTest(label=label):
                event_calls: list[str] = []
                stdout = io.StringIO()
                argv = [
                    "contextlattice-agent-adapter",
                    "handoff",
                    "--project",
                    "alpha",
                    "--session-id",
                    "session-safe",
                    "--agent-id",
                    "agent-safe",
                    "--summary",
                    "bounded handoff",
                ]
                completed = subprocess.CompletedProcess([], 0, stdout=stdout_value, stderr="")
                with patch.object(module.subprocess, "run", return_value=completed), patch.object(
                    module, "request_json", side_effect=lambda method, path, payload, timeout: event_calls.append(path)
                ), patch.object(sys, "argv", argv), redirect_stdout(stdout):
                    self.assertEqual(module.main(), 1)
                output = json.loads(stdout.getvalue())
                self.assertIs(output["ok"], False)
                self.assertTrue(module.public_contract_fits(output))
                self.assertEqual(event_calls, [])

    def test_adapter_quality_state_retires_only_after_outcome_event_success(self) -> None:
        module = load_adapter_module()
        common_module = sys.modules[module.request_json.__module__]
        initial_state = {
            "session_id": "session-quality",
            "latest_context_pack_quality": {"sample_id": "cpq_quality", "reported": False},
        }
        written: list[dict] = []
        with patch.object(common_module, "read_agent_session_state", return_value=initial_state), patch.object(
            common_module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)
        ):
            module.mark_context_pack_quality_reported(
                "alpha",
                "session-quality",
                {"sample_id": "cpq_quality", "outcome_id": "outcome-quality"},
            )
        self.assertEqual(len(written), 1)
        retired = written[0]["latest_context_pack_quality"]
        self.assertIs(retired["reported"], True)
        self.assertEqual(retired["outcome_id"], "outcome-quality")

        args = module.argparse.Namespace(
            project="alpha",
            context_pack_quality_sample_id="",
            first_pass_success="true",
            repair_required="false",
            provider_usage_json="",
            task_class="agent_workflow",
            outcome_source="",
            retry_count=0,
            followup_tokens=0,
            provider_prompt_tokens=0,
            provider_completion_tokens=0,
            provider_total_tokens=0,
            timeout=1,
        )
        with patch.object(common_module, "read_agent_session_state", return_value=written[0]):
            self.assertEqual(module.pending_quality_sample_id(args), "")
        args.context_pack_quality_sample_id = "cpq_quality"

        responses = iter(
            [
                {
                    "ok": True,
                    "outcome": {
                        "schema_id": "contextlattice_context_pack_outcome.v1",
                        "sample_id": "cpq_quality",
                        "outcome_id": "outcome-quality",
                    },
                },
                {"ok": False},
            ]
        )
        with patch.object(module, "request_json", side_effect=lambda *unused: next(responses)), patch.object(
            module, "mark_context_pack_quality_reported"
        ) as mark_reported:
            _raw, _event, findings = module.post_context_pack_outcome(
                args, "session-quality", "codex", "agent-safe", "adapter_outcome"
            )
        self.assertEqual(findings, [{"reason": "outcome_event_failed"}])
        mark_reported.assert_not_called()

    def test_universal_and_standalone_context_pack_rebind_nested_identity_authority(self) -> None:
        adapter = load_adapter_module()
        output = adapter.adapter_response(
            command="bootstrap",
            ok=True,
            project="alpha",
            session_id="session-request",
            agent="codex",
            agent_id="agent-request",
            result={
                "objective_runtime": {
                    "session_id": "session-response",
                    "agent_id": "agent-response",
                    "sessionId": "session-alias-response",
                    "diagnostic": {
                        "schema_id": "memory_trust_assessment.v1",
                        "agent_id": "agent-spoof-response",
                    },
                },
            },
        )
        nested = output["result"]["objective_runtime"]
        self.assertEqual(nested["session_id"], "session-request")
        self.assertEqual(nested["agent_id"], "agent-request")
        self.assertNotIn("sessionId", nested)
        self.assertEqual(nested["diagnostic"]["agent_id"], "agent-request")

        pack = load_pack_module()
        compacted = pack.compact_context_pack(
            {
                "ok": True,
                "agent_runtime": {"session_id": "session-response", "agentId": "agent-response"},
                "objective_runtime": {"sessionId": "session-response", "agent_id": "agent-response"},
                "context_pack": {"query": "nested identity authority", "facts": [], "results": []},
            },
            "nested identity authority",
            50_000,
            identity_authority={"session_id": "session-request", "agent_id": "agent-request"},
        )
        self.assertEqual(compacted["agent_runtime"]["session_id"], "session-request")
        self.assertEqual(compacted["agent_runtime"]["agent_id"], "agent-request")
        self.assertEqual(compacted["objective_runtime"]["session_id"], "session-request")
        self.assertEqual(compacted["objective_runtime"]["agent_id"], "agent-request")

    def test_python_adapter_contract_preserves_only_exact_public_product_routes(self) -> None:
        adapter = load_adapter_module()
        output = adapter.adapter_response(
            command="profiles",
            ok=True,
            project="alpha",
            session_id="session-route",
            agent="codex",
            agent_id="agent-route",
            result={"status": "available"},
        )
        contract = output["adapter_contract"]
        self.assertEqual(contract["preflight_route"], "/v1/agents/preflight")
        self.assertEqual(contract["event_route"], "/v1/agents/sessions/event")
        self.assertEqual(contract["context_pack_route"], "/memory/context-pack")
        self.assertEqual(
            contract["context_pack_outcome_route"],
            "/telemetry/context-pack-quality/outcome",
        )
        self.assertEqual(contract["checkpoint_route"], "/memory/write")
        self.assertTrue(output["format_contract"]["contract_valid"])

        unsafe_contract = {
            "preflight_route": "/Users/example/private/preflight",
            "event_route": "/v1/agents/../private",
            "context_pack_route": "/other/context-pack",
            "context_pack_outcome_route": "/telemetry/context-pack-quality/other",
            "checkpoint_route": "file:///tmp/private",
        }
        rejected_contract = adapter.redact_public_value(unsafe_contract)
        for field, unsafe in unsafe_contract.items():
            self.assertNotEqual(rejected_contract.get(field), unsafe, field)

    def test_adapter_entrypoint_wraps_routine_handoff_timeout_without_traceback(self) -> None:
        module = load_adapter_module()
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "handoff",
            "--project",
            "alpha",
            "--session-id",
            "session-safe",
            "--agent-id",
            "agent-safe",
        ]
        timeout = subprocess.TimeoutExpired(["compaction-handoff-payload"], 5)
        with patch.object(module, "main", side_effect=timeout), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module._entrypoint(), 1)
        output = json.loads(stdout.getvalue())
        self.assertIs(output["ok"], False)
        self.assertTrue(module.public_contract_fits(output))
        self.assertEqual(stderr.getvalue(), "")

    def test_adapter_complete_requires_outcome_and_outcome_event_before_terminal_event(self) -> None:
        module = load_adapter_module()
        for label, outcome_response, outcome_event_response in (
            ("outcome rejected", {"ok": False}, None),
            (
                "outcome event rejected",
                {"ok": True, "outcome": {"schema_id": "contextlattice_context_pack_outcome.v1"}},
                {"ok": False},
            ),
        ):
            with self.subTest(label=label):
                event_types: list[str] = []

                def fake_request(method, path, payload, timeout):
                    if path == "/telemetry/context-pack-quality/outcome":
                        return outcome_response
                    if path == "/v1/agents/sessions/event":
                        event_types.append(str(payload.get("type") or ""))
                        if outcome_event_response is None:
                            self.fail("terminal event was posted after rejected outcome")
                        return outcome_event_response
                    self.fail(f"unexpected path: {path}")

                stdout = io.StringIO()
                stderr = io.StringIO()
                argv = [
                    "contextlattice-agent-adapter",
                    "complete",
                    "--session-id",
                    "session-complete-ordered",
                    "--agent-id",
                    "agent-safe",
                    "--summary",
                    "verified completion",
                    "--context-pack-quality-sample-id",
                    "cpq-complete-ordered",
                    "--first-pass-success",
                    "true",
                ]
                with patch.object(module, "request_json", side_effect=fake_request), patch.object(
                    module,
                    "profile_payload",
                    return_value=("codex", {}, "agent-safe", "", "complete", "balanced"),
                ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
                    self.assertEqual(module.main(), 1)

                output = json.loads(stdout.getvalue())
                self.assertNotIn("session.completed", event_types)
                self.assertIs(output["ok"], False)
                self.assertTrue(output["format_contract"]["contract_valid"])
                self.assertEqual(stderr.getvalue(), "")

    def test_adapter_gateway_rejection_has_no_completion_side_effects(self) -> None:
        module = load_adapter_module()
        raw = valid_context_pack_response(module, query="adapter gateway rejection")
        raw["ok"] = False
        event_calls: list[dict] = []
        written: list[dict] = []
        common_module = sys.modules[module.request_json_for_validation.__module__]
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--session-id",
            "session-adapter-rejection",
            "--query",
            "adapter gateway rejection",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "request_json", side_effect=lambda method, path, payload, timeout: event_calls.append(payload)
        ), patch.object(
            module,
            "profile_payload",
            return_value=("codex", {}, "agent-safe", "", "adapter gateway rejection", "balanced"),
        ), patch.object(common_module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 1)

        output = json.loads(stdout.getvalue())
        self.assertFalse(output["ok"])
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(event_calls, [])
        self.assertEqual(written, [])
        self.assertEqual(stderr.getvalue(), "")

    def test_adapter_reduces_public_expansion_to_bounded_typed_failure(self) -> None:
        module = load_adapter_module()
        raw = valid_context_pack_response(module, query="adapter public expansion")
        secret_prefix = "pass" + "word"
        raw["agent_runtime"] = {f"{secret_prefix}_{index:04d}": "x" for index in range(5000)}
        event_calls: list[dict] = []
        written: list[dict] = []

        def fake_event(method, path, payload, timeout):
            event_calls.append(payload)
            return {"ok": True}

        common_module = sys.modules[module.request_json_for_validation.__module__]

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-agent-adapter",
            "context-pack",
            "--session-id",
            "session-adapter-expansion",
            "--query",
            "adapter public expansion",
        ]
        with patch.object(module, "request_json_for_validation", return_value=raw), patch.object(
            module, "request_json", side_effect=fake_event
        ), patch.object(module, "profile_payload", return_value=("codex", {}, "agent-safe", "", "adapter public expansion", "balanced")), patch.object(
            common_module, "read_agent_session_state", return_value={}
        ), patch.object(
            common_module,
            "write_agent_session_state",
            side_effect=lambda project, state: written.append(state),
        ), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 1)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertEqual(stderr.getvalue(), "")
        self.assertFalse(output["ok"])
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertLessEqual(
            output["format_contract"]["actual_json_bytes"],
            output["format_contract"]["max_total_json_bytes"],
        )
        self.assertLessEqual(
            len(json.dumps(output, sort_keys=True, separators=(",", ":")).encode("utf-8")),
            output["format_contract"]["max_total_json_bytes"],
        )
        nested = output["result"]["context_pack"]
        self.assertFalse(nested["ok"])
        self.assertTrue(nested["format_contract"]["contract_valid"])
        self.assertLessEqual(
            nested["format_contract"]["actual_json_bytes"],
            nested["format_contract"]["max_total_json_bytes"],
        )
        self.assertEqual(event_calls, [])
        self.assertEqual(written, [])

    def test_low_budget_raw_advisor_numeric_secret_never_reaches_stderr(self) -> None:
        module = load_pack_module()
        raw_secret = "sk-RAW-SUCCESS-ADVISOR-SECRET-LOW-BUDGET"

        def fake_request(method, path, payload, timeout):
            return {
                "ok": True,
                "run_advisor": {
                    "prompt_quality": {"score": raw_secret},
                    "pad": "word " * 6000,
                },
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "safe low-budget advisor", "facts": [], "results": []},
            }

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = ["contextlattice-pack", "safe low-budget advisor", "--no-auto-session", "--budget-chars", "1024"]
        with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        self.assertLessEqual(len(emitted.encode("utf-8")), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertNotIn(raw_secret, emitted + stderr.getvalue())
        self.assertEqual(stderr.getvalue(), "")
        self.assertTrue(json.loads(emitted)["format_contract"]["contract_valid"])

    def test_run_advisor_and_coverage_do_not_coerce_false_like_strings(self) -> None:
        module = load_pack_module()
        advisor = module.slim_run_advisor(
            {
                "ok": "false",
                "prompt_quality": {"complete": "false"},
                "retrieval_advice": {"blocking_recommended": "false"},
                "continuation": {"continuation_available": "false"},
                "graph_quality": {"used": "false"},
            }
        )
        self.assertIs(advisor["ok"], False)
        self.assertIs(advisor["prompt_quality"]["complete"], False)
        self.assertIs(advisor["retrieval_advice"]["blocking_recommended"], False)
        self.assertIs(advisor["continuation"]["continuation_available"], False)
        self.assertIs(advisor["graph_quality"]["used"], False)
        self.assertIs(module.slim_source_coverage({"complete": "false"})["complete"], False)

    def test_public_product_routes_survive_only_in_closed_structured_fields(self) -> None:
        module = load_pack_module()
        poll_route = "/memory/search/continuations/cont-public_01"
        events_route = poll_route + "/events"
        advisor = module.public_run_advisor(
            {
                "continuation": {
                    "poll_url": poll_route,
                    "events_url": events_route,
                    "agent_followup_endpoint": poll_route,
                }
            }
        )
        self.assertEqual(advisor["continuation"]["poll_url"], poll_route)
        self.assertEqual(advisor["continuation"]["events_url"], events_route)
        self.assertEqual(advisor["continuation"]["agent_followup_endpoint"], poll_route)

        rejected = module.public_run_advisor(
            {
                "continuation": {
                    "poll_url": "/Users/example/private/continuation.json",
                    "events_url": "/memory/search/continuations/../private",
                    "agent_followup_endpoint": "file:///tmp/private",
                }
            }
        )
        self.assertEqual(rejected["continuation"]["poll_url"], "")
        self.assertEqual(rejected["continuation"]["events_url"], "")
        self.assertEqual(rejected["continuation"]["agent_followup_endpoint"], "")

        quality = {
            "schema_id": "contextlattice_context_pack_quality.v1",
            "version": 1,
            "capturedAt": "2026-08-13T12:00:00Z",
            "sample_id": "cpq_public_route",
            "query_hash": "0123456789abcdef",
            "quality_score": 90,
        }
        compacted = module.compact_context_pack(
            {
                "ok": True,
                "session_id": "session-public-route",
                "run_advisor": {
                    "continuation": {
                        "poll_url": poll_route,
                        "events_url": events_route,
                        "agent_followup_endpoint": poll_route,
                    }
                },
                "context_pack_quality": quality,
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "public route", "facts": [], "results": []},
            },
            "public route",
            50000,
            identity_authority={"session_id": "session-public-route", "agent_id": ""},
        )
        emitted = module.redact_public_value(compacted)
        self.assertEqual(emitted["run_advisor"]["continuation"]["poll_url"], poll_route)
        self.assertEqual(emitted["run_advisor"]["continuation"]["events_url"], events_route)
        self.assertEqual(
            emitted["outcome_report"]["endpoint"],
            "/telemetry/context-pack-quality/outcome",
        )
        self.assertTrue(emitted["format_contract"]["contract_valid"])

    def test_standalone_http_error_body_is_replaced_with_constant_data(self) -> None:
        module = load_pack_module()
        common = sys.modules[module.request_json_for_validation.__module__]
        marker = "upstream-benign-diagnostic-marker"
        error = urllib.error.HTTPError(
            "http://gateway.invalid/memory/context-pack",
            502,
            "bad gateway",
            None,
            io.BytesIO(json.dumps({"detail": marker}).encode("utf-8")),
        )
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "bounded transport failure",
            "--no-auto-session",
            "--soft",
            "--budget-chars",
            "1024",
        ]
        try:
            with patch.dict(module.os.environ, {"CONTEXTLATTICE_TRIGGER_SELF_HEAL": "0"}), patch.object(
                common.urllib.request, "urlopen", side_effect=error
            ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
                self.assertEqual(module.main(), 0)
        finally:
            error.close()
        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertNotIn(marker, emitted + stderr.getvalue())
        self.assertIn("gateway_request_failed", str(output.get("error") or ""))
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(stderr.getvalue(), "")

    def test_malformed_raw_quality_cannot_persist_secret_state(self) -> None:
        module = load_pack_module()
        raw_secret = "sk-MALFORMED-QUALITY-SECRET-123456"

        def fake_request(method, path, payload, timeout):
            return {
                "ok": True,
                "context_pack_quality": {
                    "schema_id": "contextlattice_context_pack_quality.v1",
                    "version": 1,
                    "capturedAt": "2026-08-12T12:00:00Z",
                    "sample_id": "cpq_malformed_quality",
                    "query_hash": raw_secret,
                    "quality_score": raw_secret,
                },
                "source_coverage": {"configured": [], "returned": [], "complete": True},
                "context_pack": {"query": "malformed quality", "facts": [], "results": []},
            }

        written: list[dict] = []
        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "malformed quality",
            "--no-auto-session",
            "--session-id",
            "session-malformed-quality",
            "--budget-chars",
            "50000",
        ]
        with patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(
            module, "read_agent_session_state", return_value={}
        ), patch.object(module, "write_agent_session_state", side_effect=lambda project, state: written.append(state)), patch.object(
            sys, "argv", argv
        ), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        self.assertEqual(written, [])
        self.assertNotIn(raw_secret, stdout.getvalue() + stderr.getvalue())
        self.assertEqual(json.loads(stdout.getvalue())["context_pack_quality"]["query_hash"], "[REDACTED]")

    def test_malformed_raw_success_fails_closed_without_traceback_or_payload_echo(self) -> None:
        module = load_pack_module()
        malformed = valid_memory_trust_assessment(module)
        malformed["deep_untrusted"] = {}
        cursor = malformed["deep_untrusted"]
        for _ in range(1100):
            nested = {}
            cursor["nested"] = nested
            cursor = nested

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "malformed raw response",
            "--no-auto-session",
            "--soft",
            "--budget-chars",
            "1024",
        ]
        with patch.object(
            module,
            "request_json_for_validation",
            return_value={
                "ok": True,
                "memory_trust_assessment": malformed,
                "context_pack": {"facts": [], "results": []},
            },
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        output = json.loads(stdout.getvalue())
        self.assertTrue(output["ok"])
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertLessEqual(len(stdout.getvalue().encode("utf-8")), output["context_budget_chars"])
        self.assertIs(output["memory_trust_assessment"]["available"], False)
        self.assertNotIn("deep_untrusted", stdout.getvalue())
        self.assertEqual(stderr.getvalue(), "")

    def test_default_mode_still_posts_context_pack(self) -> None:
        module = load_pack_module()
        calls: list[str] = []
        timeouts: list[float] = []

        def fake_request(method, path, payload, timeout):
            calls.append(path)
            timeouts.append(timeout)
            return {"ok": True, "context_pack": {"facts": [], "results": []}}

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "default task", "--no-auto-session", "--retries", "0"]
        with patch.dict(module.os.environ, {"CONTEXTLATTICE_CLIENT_TIMEOUT_SECS": ""}), patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)
        output = json.loads(stdout.getvalue())
        self.assertEqual(calls, ["/memory/context-pack"])
        self.assertEqual(timeouts, [200.0])
        self.assertIn("context_pack", output)
        self.assertEqual(output["format_contract"]["schema_id"], "context_pack_response.v1")
        self.assertTrue(output["format_contract"]["contract_valid"])
        self.assertEqual(output["memory_trust_assessment"]["available"], False)
        self.assertEqual(output["retrieval_decision_trace"]["available"], False)
        self.assertEqual(output["context_pack"]["memory_trust_assessment"]["canonical_path"], "$.memory_trust_assessment")
        self.assertEqual(output["context_compiler"]["retrieval_decision_trace"]["canonical_path"], "$.retrieval_decision_trace")

    def test_legacy_pack_timeout_uses_finite_env_or_explicit_override(self) -> None:
        module = load_pack_module()
        cases = (
            ("49", [], 49.0),
            ("49", ["--timeout", "7"], 7.0),
            ("not-a-number", [], 200.0),
        )
        for env_timeout, timeout_args, expected in cases:
            with self.subTest(env_timeout=env_timeout, timeout_args=timeout_args):
                calls: list[float] = []

                def fake_request(method, path, payload, timeout):
                    calls.append(timeout)
                    return {"ok": True, "context_pack": {"facts": [], "results": []}}

                stdout = io.StringIO()
                argv = ["contextlattice-pack", "timeout resolution", "--no-auto-session", *timeout_args]
                with patch.dict(module.os.environ, {"CONTEXTLATTICE_CLIENT_TIMEOUT_SECS": env_timeout}), patch.object(module, "request_json_for_validation", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
                    self.assertEqual(module.main(), 0)
                self.assertEqual(calls, [expected])

    def test_legacy_pack_retry_flag_cannot_replay_post(self) -> None:
        module = load_pack_module()
        calls: list[str] = []

        def failed_request(method, path, payload, timeout):
            calls.append(path)
            raise RuntimeError("simulated one-shot failure")

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "legacy retry budget", "--no-auto-session", "--soft", "--retries", "3", "--budget-chars", "100000"]
        with patch.dict(module.os.environ, {"CONTEXTLATTICE_TRIGGER_SELF_HEAL": "0"}), patch.object(module, "request_json_for_validation", side_effect=failed_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)

        output = json.loads(stdout.getvalue())
        self.assertEqual(calls, ["/memory/context-pack"])
        self.assertEqual(output["attempts"], 1)
        self.assertEqual(output["status"], "failed_without_retry")
        self.assertEqual(output["retry_policy"], "one_shot_no_replay")

    def test_pretty_failure_emission_stays_within_the_effective_budget(self) -> None:
        module = load_pack_module()

        def failed_request(method, path, payload, timeout):
            raise RuntimeError("bounded transport failure")

        stdout = io.StringIO()
        stderr = io.StringIO()
        argv = [
            "contextlattice-pack",
            "q" * 6000,
            "--no-auto-session",
            "--soft",
            "--pretty",
            "--budget-chars",
            "1024",
        ]
        with patch.dict(module.os.environ, {"CONTEXTLATTICE_TRIGGER_SELF_HEAL": "0"}), patch.object(
            module, "request_json_for_validation", side_effect=failed_request
        ), patch.object(sys, "argv", argv), redirect_stdout(stdout), redirect_stderr(stderr):
            self.assertEqual(module.main(), 0)

        emitted = stdout.getvalue()
        output = json.loads(emitted)
        self.assertLessEqual(len(emitted.encode("utf-8")), module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertFalse(output["ok"])
        self.assertEqual(output["context_budget_chars"], module.MIN_CONTEXT_PACK_CONTRACT_BUDGET_CHARS)
        self.assertEqual(stderr.getvalue(), "")


if __name__ == "__main__":
    unittest.main()
