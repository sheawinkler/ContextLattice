from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import unittest
from copy import deepcopy
from importlib.machinery import SourceFileLoader
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
AGENT_DIR = ROOT / "scripts" / "agent"
AUDIT = AGENT_DIR / "audit-agent-runtime-contract"
PACK = AGENT_DIR / "contextlattice-pack"


def load_audit_module():
    sys.path.insert(0, str(AGENT_DIR))
    name = "agent_runtime_contract_audit_test_module"
    loader = SourceFileLoader(name, str(AUDIT))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load agent runtime contract audit")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load_pack_module():
    sys.path.insert(0, str(AGENT_DIR))
    name = "agent_runtime_contract_pack_test_module"
    loader = SourceFileLoader(name, str(PACK))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load contextlattice-pack")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def contracts_for(module):
    return sys.modules[module.validate_agent_contract_payload.__module__]


def valid_objective_runtime(module, session_id: str, agent: str | None = None):
    contracts = contracts_for(module)
    agent = agent or module.AUDIT_REQUEST_AGENT
    return contracts.attach_format_contract(
        "objective_runtime_state.v1",
        {
            "version": "1",
            "agent": agent,
            "agent_id": module.AUDIT_AGENT_ID,
            "project": module.AUDIT_PROJECT,
            "session_id": session_id,
            "objective_state": "active",
            "mission": module.AUDIT_MISSION,
            "objective": module.AUDIT_OBJECTIVE,
            "goal": module.AUDIT_GOAL,
            "objective_hierarchy": {
                "schema_id": "contextlattice_objective_hierarchy.v1",
                "project": {"name": module.AUDIT_PROJECT},
                "topic": {"topic_path": module.AUDIT_TOPIC_PATH, "objective": module.AUDIT_OBJECTIVE},
                "session": {
                    "session_id": session_id,
                    "objective": module.AUDIT_OBJECTIVE,
                    "query": module.AUDIT_QUERY,
                },
                "current": {"level": "session", "objective": module.AUDIT_OBJECTIVE},
            },
            "objective_lineage": {
                "schema_id": "contextlattice_objective_lineage.v1",
                "source": "test_fixture",
                "precedence": ["session"],
                "drift": {"status": "aligned"},
                "handoff_rule": "preserve exact bounded runtime authority",
            },
            "scoreboard": {
                "primary_kpi": "verified task success",
                "guardrail_kpi": "no boundary violations",
                "cadence_kpi": "each lifecycle boundary",
            },
            "action_executed": "agent.preflight.completed",
            "evidence": {"required": ["request", "contract", "session"], "current": []},
            "objective_delta": {"before": "pending", "after": "active"},
            "risk_or_blocker": {"status": "none", "fastest_recovery_path": "repeat validated preflight"},
            "next_action": "continue with bounded context",
        },
    )


def valid_policy_context_package(module, runtime: dict):
    contracts = contracts_for(module)
    return contracts.attach_format_contract(
        "policy_context_package.v1",
        {
            "version": "1",
            "agent": runtime["agent"],
            "agent_id": module.AUDIT_AGENT_ID,
            "project": module.AUDIT_PROJECT,
            "topic_path": module.AUDIT_TOPIC_PATH,
            "query": module.AUDIT_QUERY,
            "retrieval_mode": module.AUDIT_RETRIEVAL_MODE,
            "mission": module.AUDIT_MISSION,
            "objective": module.AUDIT_OBJECTIVE,
            "goal": module.AUDIT_GOAL,
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


def valid_preflight_response(
    module,
    session_id: str = "sess_audit_internal_123",
    *,
    runtime_agent: str | None = None,
):
    contracts = contracts_for(module)
    request, reuse_key = module.audit_preflight_request()
    runtime = valid_objective_runtime(module, session_id, agent=runtime_agent)
    policy = valid_policy_context_package(module, runtime)
    response = {
        "ok": True,
        "service": "gateway-go",
        "agent": module.AUDIT_RESOLVED_AGENT,
        "agent_id": module.AUDIT_AGENT_ID,
        "project": module.AUDIT_PROJECT,
        "query": module.AUDIT_QUERY,
        "topic_path": module.AUDIT_TOPIC_PATH,
        "retrieval_mode": module.AUDIT_RETRIEVAL_MODE,
        "session_id": session_id,
        "context_pack": {"ok": True},
        "objective_runtime": runtime,
        "objective_hierarchy": runtime["objective_hierarchy"],
        "policy_context_package": policy,
        "agent_runtime": {
            "session": {
                "id": session_id,
                "status": "active",
                "project": module.AUDIT_PROJECT,
                "agent": module.AUDIT_RESOLVED_AGENT,
                "agent_id": module.AUDIT_AGENT_ID,
                "reuse_key": reuse_key,
                "mission": module.AUDIT_MISSION,
                "objective": module.AUDIT_OBJECTIVE,
                "goal": module.AUDIT_GOAL,
                "objective_hierarchy": runtime["objective_hierarchy"],
                "metadata": {
                    "topic_path": module.AUDIT_TOPIC_PATH,
                    "retrieval_mode": module.AUDIT_RETRIEVAL_MODE,
                },
            }
        },
    }
    return contracts.attach_preflight_contracts(response), request


def runtime_receipt(module, session_id: str):
    contracts = contracts_for(module)
    runtime = valid_objective_runtime(module, session_id, agent="context-pack")
    return contracts.build_objective_runtime_attestation(
        runtime,
        session_id=session_id,
        project=module.AUDIT_PROJECT,
        agent_id=module.AUDIT_AGENT_ID,
        runtime_agent="context-pack",
    )


def forge_passed_format_metadata(module, payload: dict) -> dict:
    forged = deepcopy(payload)
    metadata = forged.get("format_contract")
    if not isinstance(metadata, dict):
        raise AssertionError("fixture requires attached format metadata")
    metadata["contract_valid"] = True
    metadata["validation"] = {"status": "passed", "errors": []}
    for _ in range(8):
        actual = len(contracts_for(module).agent_contract_go_json(forged))
        if metadata.get("actual_json_bytes") == actual:
            break
        metadata["actual_json_bytes"] = actual
    return forged


def write_cli_state(module, state_path: Path, session_id: str, *, receipt: dict | None = None) -> None:
    reuse_key = module.agent_session_reuse_key(
        module.AUDIT_PROJECT,
        "agent-cli",
        module.AUDIT_AGENT_ID,
        module.AUDIT_CLI_QUERY,
        {"tool": "contextlattice-pack", "topic_path": module.AUDIT_TOPIC_PATH, "retrieval_mode": "balanced"},
    )
    state_path.write_text(
        json.dumps(
            {
                "session_id": session_id,
                "project": module.AUDIT_PROJECT,
                "agent": "agent-cli",
                "agent_id": module.AUDIT_AGENT_ID,
                "objective": module.AUDIT_CLI_QUERY,
                "reuse_key": reuse_key,
                **({"latest_objective_runtime_attestation": receipt} if receipt is not None else {}),
            }
        ),
        encoding="utf-8",
    )


class AgentRuntimeContractAuditTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_audit_module()
        cls.pack = load_pack_module()

    def test_preflight_requires_registered_attestation_and_exact_authority(self) -> None:
        response, _ = valid_preflight_response(self.module)
        findings = []
        with patch.object(self.module, "request_json_for_validation", return_value=response):
            actual, session_id = self.module.audit_preflight(findings, 1)
        self.assertIs(actual, response)
        self.assertEqual(session_id, "sess_audit_internal_123")
        self.assertEqual(findings, [])

    def test_preflight_accepts_strict_and_default_runtime_agent_modes(self) -> None:
        for mode, runtime_agent in (
            ("strict native", self.module.AUDIT_REQUEST_AGENT),
            ("default resolved profile", self.module.AUDIT_RESOLVED_AGENT),
        ):
            with self.subTest(mode=mode):
                response, _ = valid_preflight_response(self.module, runtime_agent=runtime_agent)
                findings = []
                with patch.object(self.module, "request_json_for_validation", return_value=response):
                    _, session_id = self.module.audit_preflight(findings, 1)
                self.assertEqual(session_id, "sess_audit_internal_123")
                self.assertEqual(findings, [])

        response, _ = valid_preflight_response(self.module, runtime_agent=self.module.AUDIT_REQUEST_AGENT)
        policy = deepcopy(response["policy_context_package"])
        policy["agent"] = self.module.AUDIT_RESOLVED_AGENT
        response["policy_context_package"] = contracts_for(self.module).attach_format_contract(
            "policy_context_package.v1", policy
        )
        response = contracts_for(self.module).attach_preflight_contracts(response)
        findings = []
        with patch.object(self.module, "request_json_for_validation", return_value=response):
            _, session_id = self.module.audit_preflight(findings, 1)
        self.assertEqual(session_id, "")
        self.assertIn("preflight_authority_mismatch", {item.get("reason") for item in findings})

    def test_registered_envelopes_reject_forged_passed_metadata(self) -> None:
        contracts = contracts_for(self.module)
        response, _ = valid_preflight_response(self.module)
        runtime = deepcopy(response["objective_runtime"])
        runtime.pop("scoreboard")
        policy = deepcopy(response["policy_context_package"])
        policy.pop("handoff")

        for contract_id, malformed in (
            ("objective_runtime_state.v1", forge_passed_format_metadata(self.module, runtime)),
            ("policy_context_package.v1", forge_passed_format_metadata(self.module, policy)),
        ):
            with self.subTest(contract_id=contract_id):
                self.assertTrue(contracts.validate_agent_contract_payload(contract_id, malformed))
                self.assertFalse(contracts.agent_contract_envelope_attestation_valid(contract_id, malformed))

    def test_preflight_rejects_forged_nested_runtime_and_policy_metadata(self) -> None:
        contracts = contracts_for(self.module)

        def malformed_root_runtime(response):
            runtime = deepcopy(response["objective_runtime"])
            runtime.pop("scoreboard")
            response["objective_runtime"] = forge_passed_format_metadata(self.module, runtime)

        def malformed_policy(response):
            policy = deepcopy(response["policy_context_package"])
            policy.pop("handoff")
            response["policy_context_package"] = forge_passed_format_metadata(self.module, policy)

        def malformed_policy_runtime(response):
            policy = deepcopy(response["policy_context_package"])
            runtime = deepcopy(policy["objective_runtime"])
            runtime.pop("scoreboard")
            policy["objective_runtime"] = forge_passed_format_metadata(self.module, runtime)
            response["policy_context_package"] = forge_passed_format_metadata(self.module, policy)

        for name, mutate, expected_reason in (
            ("root runtime", malformed_root_runtime, "preflight_objective_runtime_attestation_invalid"),
            ("policy", malformed_policy, "preflight_policy_attestation_invalid"),
            ("policy runtime", malformed_policy_runtime, "policy_objective_runtime_attestation_invalid"),
        ):
            with self.subTest(name=name):
                response, _ = valid_preflight_response(self.module)
                mutate(response)
                response = contracts.attach_preflight_contracts(response)
                findings = []
                with patch.object(self.module, "request_json_for_validation", return_value=response):
                    _, session_id = self.module.audit_preflight(findings, 1)
                self.assertEqual(session_id, "")
                self.assertIn(expected_reason, {item.get("reason") for item in findings})

    def test_preflight_rejects_mission_objective_goal_and_hierarchy_drift(self) -> None:
        contracts = contracts_for(self.module)

        def runtime_objective(response):
            runtime = deepcopy(response["objective_runtime"])
            runtime["objective"] = "foreign objective"
            response["objective_runtime"] = contracts.attach_format_contract("objective_runtime_state.v1", runtime)

        def runtime_mission(response):
            runtime = deepcopy(response["objective_runtime"])
            runtime["mission"] = "foreign mission"
            response["objective_runtime"] = contracts.attach_format_contract("objective_runtime_state.v1", runtime)

        def policy_objective(response):
            policy = deepcopy(response["policy_context_package"])
            policy["objective"] = "foreign objective"
            response["policy_context_package"] = contracts.attach_format_contract("policy_context_package.v1", policy)

        def policy_runtime_goal(response):
            policy = deepcopy(response["policy_context_package"])
            runtime = deepcopy(policy["objective_runtime"])
            runtime["goal"] = "foreign goal"
            policy["objective_runtime"] = contracts.attach_format_contract("objective_runtime_state.v1", runtime)
            response["policy_context_package"] = contracts.attach_format_contract("policy_context_package.v1", policy)

        def runtime_topic_hierarchy(response):
            runtime = deepcopy(response["objective_runtime"])
            runtime["objective_hierarchy"]["topic"]["objective"] = "foreign hierarchy objective"
            response["objective_runtime"] = contracts.attach_format_contract("objective_runtime_state.v1", runtime)

        def root_current_hierarchy(response):
            response["objective_hierarchy"]["current"]["objective"] = "foreign hierarchy objective"

        def policy_topic_hierarchy(response):
            policy = deepcopy(response["policy_context_package"])
            policy["objective_hierarchy"]["topic"]["topic_path"] = "foreign/topic"
            response["policy_context_package"] = contracts.attach_format_contract("policy_context_package.v1", policy)

        def session_project_hierarchy(response):
            response["agent_runtime"]["session"]["objective_hierarchy"]["project"]["name"] = "foreign-project"

        for name, mutate in (
            ("runtime mission", runtime_mission),
            ("runtime objective", runtime_objective),
            ("policy objective", policy_objective),
            ("policy runtime goal", policy_runtime_goal),
            ("root current hierarchy", root_current_hierarchy),
            ("runtime topic hierarchy", runtime_topic_hierarchy),
            ("policy topic hierarchy", policy_topic_hierarchy),
            ("session project hierarchy", session_project_hierarchy),
        ):
            with self.subTest(name=name):
                response, _ = valid_preflight_response(self.module)
                mutate(response)
                response = contracts.attach_preflight_contracts(response)
                findings = []
                with patch.object(self.module, "request_json_for_validation", return_value=response):
                    _, session_id = self.module.audit_preflight(findings, 1)
                self.assertEqual(session_id, "")
                self.assertIn("preflight_authority_mismatch", {item.get("reason") for item in findings})

    def test_preflight_rejects_root_nested_and_session_authority_mismatches(self) -> None:
        contracts = contracts_for(self.module)

        def foreign_root_project(response):
            response["project"] = "foreign-project"

        def foreign_root_agent(response):
            response["agent"] = "foreign-agent"

        def foreign_runtime_session(response):
            runtime = deepcopy(response["objective_runtime"])
            runtime["session_id"] = "sess_foreign_runtime"
            runtime["objective_hierarchy"]["session"]["session_id"] = "sess_foreign_runtime"
            response["objective_runtime"] = contracts.attach_format_contract("objective_runtime_state.v1", runtime)

        def foreign_policy_runtime_agent(response):
            policy = deepcopy(response["policy_context_package"])
            runtime = deepcopy(policy["objective_runtime"])
            runtime["agent"] = "foreign-agent"
            policy["objective_runtime"] = contracts.attach_format_contract("objective_runtime_state.v1", runtime)
            response["policy_context_package"] = contracts.attach_format_contract("policy_context_package.v1", policy)

        def foreign_session_authority(response):
            response["agent_runtime"]["session"]["id"] = "sess_foreign_authority"

        for name, mutate in (
            ("root project", foreign_root_project),
            ("root agent", foreign_root_agent),
            ("objective runtime session", foreign_runtime_session),
            ("policy runtime agent", foreign_policy_runtime_agent),
            ("agent runtime session", foreign_session_authority),
        ):
            with self.subTest(name=name):
                response, _ = valid_preflight_response(self.module)
                mutate(response)
                response = contracts.attach_preflight_contracts(response)
                findings = []
                with patch.object(self.module, "request_json_for_validation", return_value=response):
                    _, session_id = self.module.audit_preflight(findings, 1)
                self.assertEqual(session_id, "")
                self.assertIn("preflight_authority_mismatch", {item.get("reason") for item in findings})

    def test_cli_reads_private_runtime_receipt_while_requiring_public_omission(self) -> None:
        session_id = "sess_audit_cli_123"
        receipt = runtime_receipt(self.module, session_id)

        def fake_run(_args, **kwargs):
            state_path = Path(kwargs["env"]["CONTEXTLATTICE_SESSION_STATE_PATH"])
            write_cli_state(self.module, state_path, session_id, receipt=receipt)
            return subprocess.CompletedProcess(
                args=[],
                returncode=0,
                stdout=json.dumps({"ok": True, "identity_omitted": ["session_id"]}),
                stderr="",
            )

        findings = []
        with (
            patch.object(self.module.subprocess, "run", side_effect=fake_run),
            patch.object(self.module, "agent_contract_envelope_attestation_valid", return_value=True),
        ):
            payload, session_id = self.module.audit_cli_auto_session(findings, 1)
        self.assertTrue(payload["ok"])
        self.assertNotIn("session_id", payload)
        self.assertEqual(session_id, "sess_audit_cli_123")
        self.assertEqual(findings, [])
        self.assertEqual(self.module.audit_phase_summary(payload, session_id)["objective_runtime_status"], "passed")

    def test_pack_records_only_an_exact_authority_bound_runtime_receipt(self) -> None:
        session_id = "sess_audit_cli_receipt"
        runtime = valid_objective_runtime(self.module, session_id, agent="context-pack")
        state = {
            "session_id": session_id,
            "project": self.module.AUDIT_PROJECT,
            "agent": "agent-cli",
            "agent_id": self.module.AUDIT_AGENT_ID,
            "objective": self.module.AUDIT_CLI_QUERY,
        }
        written = []
        with (
            patch.object(self.pack, "read_agent_session_state", return_value=state),
            patch.object(self.pack, "write_agent_session_state", side_effect=lambda _project, value: written.append(value)),
        ):
            self.pack.record_objective_runtime_attestation(
                self.module.AUDIT_PROJECT,
                session_id,
                self.module.AUDIT_AGENT_ID,
                self.module.AUDIT_CLI_QUERY,
                {"objective_runtime": runtime},
            )
        self.assertEqual(len(written), 1)
        self.assertTrue(
            contracts_for(self.module).objective_runtime_attestation_valid(
                written[0]["latest_objective_runtime_attestation"],
                session_id=session_id,
                project=self.module.AUDIT_PROJECT,
                agent_id=self.module.AUDIT_AGENT_ID,
                runtime_agent="context-pack",
            )
        )

        written.clear()
        foreign = deepcopy(runtime)
        foreign["project"] = "foreign-project"
        foreign = contracts_for(self.module).attach_format_contract("objective_runtime_state.v1", foreign)
        with (
            patch.object(self.pack, "read_agent_session_state", return_value=state),
            patch.object(self.pack, "write_agent_session_state", side_effect=lambda _project, value: written.append(value)),
        ):
            self.pack.record_objective_runtime_attestation(
                self.module.AUDIT_PROJECT,
                session_id,
                self.module.AUDIT_AGENT_ID,
                self.module.AUDIT_CLI_QUERY,
                {"objective_runtime": foreign},
            )
        self.assertEqual(written, [])

    def test_pack_rejects_forged_passed_runtime_metadata(self) -> None:
        session_id = "sess_audit_cli_forged"
        runtime = valid_objective_runtime(self.module, session_id, agent="context-pack")
        runtime.pop("scoreboard")
        runtime = forge_passed_format_metadata(self.module, runtime)
        state = {
            "session_id": session_id,
            "project": self.module.AUDIT_PROJECT,
            "agent": "agent-cli",
            "agent_id": self.module.AUDIT_AGENT_ID,
            "objective": self.module.AUDIT_CLI_QUERY,
        }
        written = []
        with (
            patch.object(self.pack, "read_agent_session_state", return_value=state),
            patch.object(self.pack, "write_agent_session_state", side_effect=lambda _project, value: written.append(value)),
        ):
            self.pack.record_objective_runtime_attestation(
                self.module.AUDIT_PROJECT,
                session_id,
                self.module.AUDIT_AGENT_ID,
                self.module.AUDIT_CLI_QUERY,
                {"objective_runtime": runtime},
            )
        self.assertEqual(written, [])

    def test_cli_rejects_soft_ok_false_even_with_valid_private_binding(self) -> None:
        session_id = "sess_audit_cli_failed"
        receipt = runtime_receipt(self.module, session_id)

        def fake_run(_args, **kwargs):
            write_cli_state(
                self.module,
                Path(kwargs["env"]["CONTEXTLATTICE_SESSION_STATE_PATH"]),
                session_id,
                receipt=receipt,
            )
            return subprocess.CompletedProcess(
                args=[],
                returncode=0,
                stdout=json.dumps({"ok": False, "identity_omitted": ["session_id"]}),
                stderr="",
            )

        findings = []
        with (
            patch.object(self.module.subprocess, "run", side_effect=fake_run),
            patch.object(self.module, "agent_contract_envelope_attestation_valid", return_value=True),
        ):
            _, actual_session_id = self.module.audit_cli_auto_session(findings, 1)
        self.assertEqual(actual_session_id, "")
        self.assertIn("contextlattice_pack_response_not_successful", {item.get("reason") for item in findings})

    def test_cli_rejects_missing_or_malformed_objective_runtime_receipt(self) -> None:
        for name, receipt in (
            ("missing", None),
            ("malformed", {"kind": "objective_runtime_attestation"}),
        ):
            with self.subTest(name=name):
                session_id = f"sess_audit_cli_{name}"

                def fake_run(_args, **kwargs):
                    write_cli_state(
                        self.module,
                        Path(kwargs["env"]["CONTEXTLATTICE_SESSION_STATE_PATH"]),
                        session_id,
                        receipt=receipt,
                    )
                    return subprocess.CompletedProcess(
                        args=[],
                        returncode=0,
                        stdout=json.dumps({"ok": True, "identity_omitted": ["session_id"]}),
                        stderr="",
                    )

                findings = []
                with (
                    patch.object(self.module.subprocess, "run", side_effect=fake_run),
                    patch.object(self.module, "agent_contract_envelope_attestation_valid", return_value=True),
                ):
                    _, actual_session_id = self.module.audit_cli_auto_session(findings, 1)
                self.assertEqual(actual_session_id, "")
                self.assertIn(
                    "contextlattice_pack_objective_runtime_attestation_missing",
                    {item.get("reason") for item in findings},
                )

    def test_cli_requires_public_identity_omission(self) -> None:
        session_id = "sess_audit_cli_private"
        receipt = runtime_receipt(self.module, session_id)

        def fake_run(_args, **kwargs):
            write_cli_state(
                self.module,
                Path(kwargs["env"]["CONTEXTLATTICE_SESSION_STATE_PATH"]),
                session_id,
                receipt=receipt,
            )
            return subprocess.CompletedProcess(
                args=[],
                returncode=0,
                stdout=json.dumps({"ok": True, "session_id": session_id}),
                stderr="",
            )

        findings = []
        with (
            patch.object(self.module.subprocess, "run", side_effect=fake_run),
            patch.object(self.module, "agent_contract_envelope_attestation_valid", return_value=True),
        ):
            _, actual_session_id = self.module.audit_cli_auto_session(findings, 1)
        self.assertEqual(actual_session_id, "")
        self.assertIn("contextlattice_pack_public_session_identity_not_omitted", {item.get("reason") for item in findings})

    def test_cli_rejects_recursive_private_identity_and_stderr_output(self) -> None:
        session_id = "sess_audit_cli_private_nested"
        receipt = runtime_receipt(self.module, session_id)

        for name, stdout_payload, stderr, expected_reason in (
            (
                "nested identity",
                {"ok": True, "identity_omitted": ["session_id"], "context_pack": {"session_id": session_id}},
                "",
                "contextlattice_pack_private_session_identity_leaked",
            ),
            (
                "identity in command string",
                {
                    "ok": True,
                    "identity_omitted": ["session_id"],
                    "outcome_report": {"command": f"contextlattice outcome --session-id {session_id}"},
                },
                "",
                "contextlattice_pack_private_session_identity_leaked",
            ),
            (
                "identity on stderr",
                {"ok": True, "identity_omitted": ["session_id"]},
                f"internal session {session_id}",
                "contextlattice_pack_private_session_identity_leaked",
            ),
            (
                "arbitrary stderr",
                {"ok": True, "identity_omitted": ["session_id"]},
                "unexpected diagnostic",
                "contextlattice_pack_stderr_not_empty",
            ),
        ):
            with self.subTest(name=name):

                def fake_run(_args, **kwargs):
                    write_cli_state(
                        self.module,
                        Path(kwargs["env"]["CONTEXTLATTICE_SESSION_STATE_PATH"]),
                        session_id,
                        receipt=receipt,
                    )
                    return subprocess.CompletedProcess(
                        args=[],
                        returncode=0,
                        stdout=json.dumps(stdout_payload),
                        stderr=stderr,
                    )

                findings = []
                with (
                    patch.object(self.module.subprocess, "run", side_effect=fake_run),
                    patch.object(self.module, "agent_contract_envelope_attestation_valid", return_value=True),
                ):
                    _, actual_session_id = self.module.audit_cli_auto_session(findings, 1)
                self.assertEqual(actual_session_id, "")
                self.assertIn(expected_reason, {item.get("reason") for item in findings})

    def test_completion_requires_exact_session_acknowledgement(self) -> None:
        findings = []
        response = {
            "ok": True,
            "event": {"session_id": "sess_audit_internal_123", "type": "session.completed"},
        }
        with patch.object(self.module, "request_json_for_validation", return_value=response):
            self.module.complete_session("sess_audit_internal_123", "completed", 1, findings)
        self.assertEqual(findings, [])

        for name, rejected in (
            ("explicit rejection", {"ok": False}),
            ("foreign acknowledgement", {"ok": True, "event": {"session_id": "sess_foreign", "type": "session.completed"}}),
        ):
            with self.subTest(name=name):
                findings = []
                with patch.object(self.module, "request_json_for_validation", return_value=rejected):
                    self.module.complete_session("sess_audit_internal_123", "completed", 1, findings)
                self.assertEqual(findings, [{"severity": "error", "reason": "audit_session_complete_not_acknowledged"}])

    def test_completion_uses_internal_identity_and_catches_typed_transport_failure(self) -> None:
        findings = []
        with patch.object(
            self.module,
            "request_json_for_validation",
            side_effect=SystemExit('{"error":"gateway_request_failed","ok":false,"status":422}'),
        ) as request:
            self.module.complete_session("sess_audit_internal_123", "completed", 1, findings)
        request.assert_called_once()
        self.assertEqual(findings[0]["severity"], "error")
        self.assertEqual(findings[0]["reason"], "audit_session_complete_failed")
        self.assertNotIn("sess_audit_internal_123", json.dumps(findings, sort_keys=True))

    def test_completion_failure_makes_main_exit_nonzero(self) -> None:
        emitted = {}

        def fail_completion(_session_id, _summary, _timeout, findings):
            findings.append({"severity": "error", "reason": "audit_session_complete_failed"})

        def capture(payload, _pretty):
            emitted.update(payload)

        with (
            patch.object(self.module, "audit_registry"),
            patch.object(self.module, "audit_preflight", return_value=({}, "sess_preflight")),
            patch.object(self.module, "audit_cli_auto_session", return_value=({}, "sess_cli")),
            patch.object(self.module, "complete_session", side_effect=fail_completion),
            patch.object(self.module, "emit", side_effect=capture),
            patch.object(sys, "argv", [str(AUDIT)]),
        ):
            rc = self.module.main()
        self.assertEqual(rc, 1)
        self.assertIs(emitted["ok"], False)
        self.assertEqual({item["reason"] for item in emitted["findings"]}, {"audit_session_complete_failed"})

    def test_public_summary_preserves_boolean_without_emitting_identity(self) -> None:
        from _common import redact_public_value

        summary = self.module.audit_phase_summary(
            {"objective_runtime": {"format_contract": {"validation": {"status": "passed"}}}},
            "sess_audit_internal_123",
        )
        projected = redact_public_value(summary)
        self.assertEqual(projected, {"binding_established": True, "objective_runtime_status": "passed"})
        self.assertNotIn("sess_audit_internal_123", json.dumps(projected, sort_keys=True))


if __name__ == "__main__":
    unittest.main()
