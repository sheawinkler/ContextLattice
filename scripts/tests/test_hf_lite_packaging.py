from __future__ import annotations

import argparse
import copy
import importlib.util
import json
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("hf_lite_acceptance", ROOT / "scripts/hf_lite_acceptance.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def docker_environment() -> dict[str, str]:
    env = {}
    for line in (ROOT / "Dockerfile.hf-lite").read_text().splitlines():
        if line.startswith("ENV "):
            env.update(item.split("=", 1) for item in shlex.split(line[4:]))
    return env


def fixture_image(env: dict[str, str] | None = None) -> dict:
    return {
        "Id": "sha256:" + "a" * 64, "Os": "linux", "Architecture": "arm64",
        "Config": {
            "Env": [f"{key}={value}" for key, value in (MODULE.ROOTS if env is None else env).items()],
            "Healthcheck": {"Test": ["CMD-SHELL", "curl --fail http://127.0.0.1:7860/readyz"]},
        },
    }


def fixture_search(rows) -> dict:
    return {"results": rows, "result_state": "ready", "degraded": False, "source_summary": {
        field: [] for field in ("pending_sources", "failed_sources", "timed_out_sources", "continuation_unavailable_sources")
    }}


class LitePackagingTests(unittest.TestCase):
    def test_image_persists_authoritative_roots_and_bundles_contracts(self):
        dockerfile = (ROOT / "Dockerfile.hf-lite").read_text()
        env = docker_environment()
        self.assertNotIn("GO_MEMORY_STORE_ROOT", env)
        for key, value in MODULE.ROOTS.items():
            self.assertEqual(env[key], value)
        self.assertEqual(MODULE.effective_memory_root(env), "/data/memory")
        self.assertEqual(MODULE.image_identity(fixture_image(env))["platform"], "linux/arm64")
        registry = "config/agent_contracts/agent_output_contracts.json"
        self.assertIn(f"COPY {registry} /app/{registry}", dockerfile)
        self.assertEqual(env["CONTEXTLATTICE_AGENT_CONTRACTS_PATH"], f"/app/{registry}")
        self.assertIn('"http://127.0.0.1:${PORT}/readyz"', dockerfile)
        self.assertNotIn("GOARCH=amd64", dockerfile)
        self.assertIn("GOARCH=${TARGETARCH}", dockerfile)
        self.assertIn("FROM --platform=$BUILDPLATFORM", dockerfile)

    def test_memory_root_override_precedence_keeps_state_root_separate(self):
        defaults = docker_environment()
        # Offline ENV contract only; native loadMemoryStorePolicy uses the same
        # Go-first precedence. This is not installed custom-root proof.
        for overrides, expected in (
            ({}, "/data/memory"),
            ({"MEMORY_BANK_ROOT": "/data/operator-memory"}, "/data/operator-memory"),
            ({"GO_MEMORY_STORE_ROOT": "/data/go-memory"}, "/data/go-memory"),
            ({"GO_MEMORY_STORE_ROOT": "/data/go-memory", "MEMORY_BANK_ROOT": "/data/operator-memory"}, "/data/go-memory"),
            ({"GO_MEMORY_STORE_ROOT": "  ", "MEMORY_BANK_ROOT": " /data/operator-memory "}, "/data/operator-memory"),
        ):
            with self.subTest(overrides=overrides):
                env = {**defaults, **overrides}
                self.assertEqual(MODULE.effective_memory_root(env), expected)
                self.assertEqual(env["CONTEXTLATTICE_GATEWAY_STATE_ROOT"], "/data/state/gateway")
        moved = {**defaults, "MEMORY_BANK_ROOT": "/data/operator-memory",
                 "CONTEXTLATTICE_GATEWAY_STATE_ROOT": "/data/operator-state"}
        self.assertEqual(MODULE.effective_memory_root(moved), "/data/operator-memory")
        self.assertEqual(moved["CONTEXTLATTICE_GATEWAY_STATE_ROOT"], "/data/operator-state")

    def test_image_identity_accepts_legacy_and_memory_only_default_roots(self):
        for env in (
            dict(MODULE.ROOTS),
            {**MODULE.ROOTS, "GO_MEMORY_STORE_ROOT": "/data/memory"},
            {**MODULE.ROOTS, "GO_MEMORY_STORE_ROOT": "/data/memory", "MEMORY_BANK_ROOT": "/unused/lower-priority"},
            {"GO_MEMORY_STORE_ROOT": "/data/memory", "CONTEXTLATTICE_GATEWAY_STATE_ROOT": "/data/state/gateway"},
            {**MODULE.ROOTS, "GO_MEMORY_STORE_ROOT": "  "},
        ):
            with self.subTest(env=env):
                self.assertEqual(MODULE.image_identity(fixture_image(env))["platform"], "linux/arm64")

    def test_contract_validation_is_required_not_just_write_ok(self):
        for payload in ({"ok": True}, {"format_contract": {"contract_valid": True}}):
            with self.subTest(payload=payload), self.assertRaises(RuntimeError):
                MODULE.require_contract(payload)
        MODULE.require_contract({"format_contract": {"contract_valid": True, "validation": {"status": "passed"}}})

    def test_remote_daemon_cannot_bind_a_different_machines_directory(self):
        for host in ("tcp://127.0.0.1:2375", "ssh://example", ""):
            with self.subTest(host=host), self.assertRaises(RuntimeError):
                MODULE.require_local_context({"Endpoints": {"docker": {"Host": host}}})
        MODULE.require_local_context({"Endpoints": {"docker": {"Host": "unix:///var/run/docker.sock"}}})

    def test_image_identity_rejects_ephemeral_roots_and_liveness_only(self):
        image = fixture_image()
        self.assertEqual(MODULE.image_identity(image)["platform"], "linux/arm64")
        bad_roots = [fixture_image(env) for env in (
            {**MODULE.ROOTS, "GO_MEMORY_STORE_ROOT": "/tmp/memory"},
            {**MODULE.ROOTS, "GO_MEMORY_STORE_ROOT": "/data/wrong-root"},
            {**MODULE.ROOTS, "MEMORY_BANK_ROOT": "/tmp/memory"},
            {**MODULE.ROOTS, "CONTEXTLATTICE_GATEWAY_STATE_ROOT": "/tmp/state"},
            {"MEMORY_BANK_ROOT": "/data/memory"},
            {"CONTEXTLATTICE_GATEWAY_STATE_ROOT": "/data/state/gateway"},
        )]
        bad_health = copy.deepcopy(image)
        bad_health["Config"]["Healthcheck"]["Test"] = ["CMD-SHELL", "curl /health"]
        bad_id = copy.deepcopy(image)
        bad_id["Id"] = "mutable:tag"
        for invalid in (*bad_roots, bad_health, bad_id):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                MODULE.image_identity(invalid)

    def test_cleanup_checks_ownership_before_stopping(self):
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            proof.started = True
            with patch.object(proof, "docker", side_effect=["container-id", '{"io.contextlattice.lite-acceptance":"another-owner"}']) as docker:
                with self.assertRaisesRegex(RuntimeError, "not owned"):
                    proof.stop()
                self.assertEqual(docker.call_count, 2)

    def test_cleanup_handles_run_failure_without_a_container(self):
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            proof.started = True
            with patch.object(proof, "docker", return_value="") as docker:
                proof.stop()
                self.assertFalse(proof.started)
                self.assertEqual(docker.call_count, 1)

    def test_failed_start_retains_receipt_and_attempts_scoped_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            args = argparse.Namespace(docker_context="default", timeout=5, image="fixture", upgrade_image=None)
            context = [{"Endpoints": {"docker": {"Host": "unix:///socket"}}}]
            with patch.object(MODULE.Acceptance, "docker", side_effect=[json.dumps(context), json.dumps([fixture_image()])]), \
                 patch.object(MODULE.Acceptance, "start", side_effect=RuntimeError("fixture startup failure")), \
                 patch.object(MODULE.Acceptance, "stop") as stop:
                receipt = MODULE.execute(args, Path(directory))
            self.assertFalse(receipt["ok"])
            self.assertEqual(receipt["scope"], "same_image_replacement")
            self.assertIn("fixture startup failure", receipt["error"])
            self.assertTrue((Path(directory) / "receipt.json").is_file())
            stop.assert_called_once()

    def test_same_image_cannot_masquerade_as_upgrade_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            args = argparse.Namespace(docker_context="default", timeout=5, image="fixture", upgrade_image="same-tag")
            context = [{"Endpoints": {"docker": {"Host": "unix:///socket"}}}]
            with patch.object(MODULE.Acceptance, "docker", side_effect=[json.dumps(context), json.dumps([fixture_image()]), json.dumps([fixture_image()])]):
                receipt = MODULE.execute(args, Path(directory))
            self.assertFalse(receipt["ok"])
            self.assertIn("two different images", receipt["error"])
            self.assertEqual(receipt["phases"], [])

    def test_readback_requires_content_history_and_search(self):
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            with patch.object(proof, "request", return_value=b"wrong"):
                with self.assertRaisesRegex(RuntimeError, "readback differs"):
                    proof.verify("project", b"expected", {"event": "hash"})
            with patch.object(proof, "request", return_value=b"expected"), patch.object(proof, "history_events", return_value={}):
                with self.assertRaisesRegex(RuntimeError, "history lost"):
                    proof.verify("project", b"expected", {"event": "hash"})
            empty = fixture_search([])
            with patch.object(proof, "request", side_effect=[b"expected", empty]), patch.object(proof, "history_events", return_value={"event": "hash"}):
                with self.assertRaisesRegex(RuntimeError, "not found"):
                    proof.verify("project", b"expected", {"event": "hash"})

    def test_search_must_bind_exact_project_file_event_content_and_summary(self):
        content = b"Persistent amber lighthouse."
        row = {"project": "project", "file": "proof.md", "event_id": "event", "content_hash": MODULE.hashlib.sha256(content).hexdigest(), "summary": content.decode()}
        mutations = [{"project": "different"}, {"file": "unrelated.md"}, {"event_id": "wrong"}, {"content_hash": "wrong"}, {"summary": "", "metadata": content.decode()}]
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            for mutation in mutations:
                response = fixture_search([{**row, **mutation}])
                with self.subTest(mutation=mutation), patch.object(proof, "request", side_effect=[content, response]), patch.object(proof, "history_events", return_value={"event": "hash"}):
                    with self.assertRaisesRegex(RuntimeError, "exact fixture not found"):
                        proof.verify("project", content, {"event": "hash"})
            response = fixture_search([row])
            with patch.object(proof, "request", side_effect=[content, response]), patch.object(proof, "history_events", return_value={"event": "hash"}):
                self.assertEqual(proof.verify("project", content, {"event": "hash"})["history_events"], 1)

    def test_raw_search_incomplete_or_malformed_is_not_a_pass(self):
        invalid = [None, [], {}, fixture_search(None), fixture_search([None]),
                   {**fixture_search([]), "degraded": True}, {**fixture_search([]), "result_state": "pending"}]
        for field in ("pending_sources", "failed_sources", "timed_out_sources", "continuation_unavailable_sources"):
            response = fixture_search([])
            response["source_summary"][field] = ["topic_rollups"]
            invalid.append(response)
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            for response in invalid:
                with self.subTest(response=response), patch.object(proof, "request", side_effect=[b"expected", response]), patch.object(proof, "history_events", return_value={"event": "hash"}):
                    with self.assertRaisesRegex(RuntimeError, "search"):
                        proof.verify("project", b"expected", {"event": "hash"})

    def test_duplicate_history_event_ids_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            raw = b'{"project":"project","event_id":"same","value":1}\n{"project":"project","event_id":"same","value":2}\n'
            with patch.object(MODULE, "read_bounded", return_value=raw), self.assertRaisesRegex(RuntimeError, "duplicate fixture event"):
                proof.history_events("project")

    def test_same_event_id_with_changed_payload_is_not_history_continuity(self):
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            with patch.object(proof, "request", return_value=b"expected"), patch.object(proof, "history_events", return_value={"event": "changed"}):
                with self.assertRaisesRegex(RuntimeError, "lost or changed"):
                    proof.verify("project", b"expected", {"event": "original"})

    def test_runtime_source_must_match_image_and_unknown_is_not_bound(self):
        image = {"source_commit": "a" * 40, "source_tree": "b" * 40, "version": "test"}
        self.assertTrue(MODULE.runtime_source_bound(image, {"build": image}))
        unknown = {"source_commit": "unknown", "source_tree": "unknown", "version": "development"}
        self.assertFalse(MODULE.runtime_source_bound(unknown, {"build": unknown}))
        for payload in ({}, {"build": {**image, "source_tree": "c" * 40}}, {"build": {**image, "version": "stale"}}):
            with self.subTest(payload=payload), self.assertRaisesRegex(RuntimeError, "mismatch"):
                MODULE.runtime_source_bound(image, payload)

    def test_claim_and_task_loss_or_changed_payload_fail_the_gate(self):
        expected = {"claim": {"claim_id": "claim", "object": "original"}, "task": {"task_id": "task", "objective": "original"}}
        contract = {"contract_valid": True, "validation": {"status": "passed"}}
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            for claims in ([], [{**expected["claim"], "object": "changed"}]):
                with self.subTest(claims=claims), patch.object(proof, "request", return_value={"claims": claims, "format_contract": contract}):
                    with self.assertRaisesRegex(RuntimeError, "claim lost or changed"):
                        proof.verify_state("project", expected)
            for tasks in ([], [{**expected["task"], "objective": "changed"}]):
                with self.subTest(tasks=tasks), patch.object(proof, "request", side_effect=[{"claims": [expected["claim"]], "format_contract": contract}, {"tasks": tasks}]):
                    with self.assertRaisesRegex(RuntimeError, "task lost or changed"):
                        proof.verify_state("project", expected)

    def test_task_fixture_uses_returned_session_id_and_requires_approval(self):
        contract = {"contract_valid": True, "validation": {"status": "passed"}}
        with tempfile.TemporaryDirectory() as directory:
            proof = MODULE.Acceptance("default", Path(directory), 5)
            with patch.object(proof, "request", side_effect=[
                {"claim": {"claim_id": "claim"}, "format_contract": contract},
                {"session": {"id": "sess_fixture"}}, {"task": {"task_id": "task"}},
            ]) as request:
                proof.create_state("project", b"fixture")
            task = request.call_args_list[2].args[1]
            self.assertEqual(task["context_request"]["session_id"], "sess_fixture")
            self.assertTrue(task["approval_policy"]["required"])

    def test_artifact_reads_are_bounded(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "history"
            path.write_bytes(b"a" * (MODULE.MAX_RESPONSE_BYTES + 1))
            with self.assertRaisesRegex(RuntimeError, "size bound"):
                MODULE.read_bounded(path)


if __name__ == "__main__":
    unittest.main()
