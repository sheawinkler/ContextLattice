#!/usr/bin/env python3
"""Prove lite-image replacement on disposable, persistent data, never a live stack."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


MAX_RESPONSE_BYTES = 262144
OWNER_LABEL = "io.contextlattice.lite-acceptance"
ROOTS = {
    "MEMORY_BANK_ROOT": "/data/memory",
    "CONTEXTLATTICE_GATEWAY_STATE_ROOT": "/data/state/gateway",
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def require_contract(payload: dict) -> None:
    contract = payload.get("format_contract", {})
    require(contract.get("contract_valid") is True, "response contract is not valid")
    require(contract.get("validation", {}).get("status") == "passed", "response contract was not validated")


def read_bounded(path: Path) -> bytes:
    with path.open("rb") as handle:
        raw = handle.read(MAX_RESPONSE_BYTES + 1)
    require(len(raw) <= MAX_RESPONSE_BYTES, "fixture artifact exceeds size bound")
    return raw


def digest(value) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def runtime_source_bound(image: dict, ready: dict) -> bool:
    build = ready.get("build", {})
    for key in ("source_commit", "source_tree", "version"):
        require(build.get(key) == image.get(key), f"runtime/image {key} mismatch")
    return all(re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", str(image.get(key))) for key in ("source_commit", "source_tree"))


def require_local_context(context: dict) -> None:
    endpoint = context.get("Endpoints", {}).get("docker", {}).get("Host", "")
    require(endpoint.startswith("unix://"), "acceptance requires a local Unix-socket Docker context")


def effective_memory_root(env: dict[str, str]) -> str:
    """Match native memory-store precedence, leaving absent roots invalid here."""
    return env.get("GO_MEMORY_STORE_ROOT", "").strip() or env.get("MEMORY_BANK_ROOT", "").strip()


def image_identity(payload: dict) -> dict:
    image_id = payload.get("Id", "")
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is not None, "image has no immutable identity")
    env = dict(item.split("=", 1) for item in payload["Config"].get("Env", []) if "=" in item)
    require(effective_memory_root(env) == ROOTS["MEMORY_BANK_ROOT"]
            and env.get("CONTEXTLATTICE_GATEWAY_STATE_ROOT", "").strip() == ROOTS["CONTEXTLATTICE_GATEWAY_STATE_ROOT"],
            "image does not declare durable memory/state roots")
    healthcheck = payload["Config"].get("Healthcheck", {}).get("Test", [])
    require(any("/readyz" in part for part in healthcheck), "image healthcheck does not use readiness")
    labels = payload["Config"].get("Labels") or {}
    return {
        "id": image_id,
        "platform": f"{payload.get('Os')}/{payload.get('Architecture')}",
        "source_commit": labels.get("org.opencontainers.image.revision"),
        "source_tree": labels.get("io.contextlattice.source-tree"),
        "version": labels.get("org.opencontainers.image.version"),
    }


class Acceptance:
    def __init__(self, context: str, root: Path, timeout: int) -> None:
        self.context = context
        self.root = root
        self.timeout = timeout
        self.owner = uuid.uuid4().hex
        self.api_key = secrets.token_hex(32)
        self.name = f"contextlattice-lite-proof-{self.owner}"
        self.base_url = ""
        self.started = False
        self.data = root / "data"
        self.data.mkdir()
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def docker(self, *args: str, timeout: int = 60) -> str:
        # Docker output is spooled, not accumulated without a bound in agent memory.
        with tempfile.TemporaryFile(dir=self.root) as output:
            result = subprocess.run(
                ["docker", "--context", self.context, *args],
                env={**os.environ, "CONTEXTLATTICE_ORCHESTRATOR_API_KEY": self.api_key},
                stdout=output, stderr=subprocess.STDOUT, timeout=timeout, check=False,
            )
            size = output.tell()
            output.seek(max(0, size - MAX_RESPONSE_BYTES))
            text = output.read(MAX_RESPONSE_BYTES).decode("utf-8", errors="replace")
        require(result.returncode == 0, f"Docker {args[0]} failed ({result.returncode}): {text[-4000:]}")
        require(size <= MAX_RESPONSE_BYTES, "Docker output exceeds bound")
        return text.strip()

    def request(self, path: str, payload: dict | None = None, *, text: bool = False):
        data = None if payload is None else json.dumps(payload).encode()
        request = urllib.request.Request(
            self.base_url + path, data=data,
            headers={"Content-Type": "application/json", "Authorization": f"Bearer {self.api_key}"},
        )
        try:
            with self.http.open(request, timeout=5) as response:
                raw = response.read(MAX_RESPONSE_BYTES + 1)
        except urllib.error.HTTPError as error:
            detail = error.read(4000).decode("utf-8", errors="replace").replace(self.api_key, "[redacted]")
            raise RuntimeError(f"HTTP {error.code} {path}: {detail}") from error
        require(len(raw) <= MAX_RESPONSE_BYTES, "HTTP response exceeds bound")
        return raw if text else json.loads(raw)

    def start(self, image: dict) -> dict:
        self.started = True
        self.docker(
            "run", "--detach", "--pull=never", "--name", self.name,
            "--label", f"{OWNER_LABEL}={self.owner}",
            "--memory", "1g", "--cpus", "1", "--pids-limit", "128",
            "--publish", "127.0.0.1::7860", "--env", "GOMAXPROCS=2",
            "--env", "CONTEXTLATTICE_ORCHESTRATOR_API_KEY",
            "--mount", f"type=bind,source={self.data},target=/data",
            image["id"],
        )
        ports = json.loads(self.docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", self.name))
        bindings = ports.get("7860/tcp", [])
        require(len(bindings) == 1 and bindings[0]["HostIp"] == "127.0.0.1", "test port is not loopback-only")
        port = int(bindings[0]["HostPort"])
        self.base_url = f"http://127.0.0.1:{port}"
        deadline = time.monotonic() + self.timeout
        last_error = "no readiness response"
        while time.monotonic() < deadline:
            try:
                ready = self.request("/readyz")
                require(ready.get("ready") is True and ready.get("ok") is True, "runtime is not ready")
                return ready
            except (OSError, ValueError, RuntimeError) as error:
                last_error = str(error)
                time.sleep(0.5)
        raise RuntimeError(f"readiness timed out: {last_error}")

    def stop(self) -> None:
        if not self.started:
            return
        owned = self.docker(
            "ps", "--all", "--quiet", "--filter", f"name=^{self.name}$",
            "--filter", f"label={OWNER_LABEL}={self.owner}",
        )
        if not owned:
            self.started = False
            return
        labels = json.loads(self.docker("inspect", "--format", "{{json .Config.Labels}}", self.name))
        require(labels.get(OWNER_LABEL) == self.owner, "refusing to stop a container not owned by this test")
        self.docker("stop", "--time", "15", self.name, timeout=30)
        self.docker("rm", self.name)
        self.started = False

    def history_events(self, project: str) -> dict[str, str]:
        raw = read_bounded(self.data / "memory/_contextlattice/memory_write_history.ndjson")
        rows = [json.loads(line) for line in raw.splitlines() if line.strip()]
        events = {}
        for row in rows:
            if row.get("project") != project:
                continue
            event_id = row["event_id"]
            require(event_id not in events, "duplicate fixture event ID in durable history")
            events[event_id] = digest(row)
        return events

    def verify(self, project: str, content: bytes, expected_history: dict[str, str]) -> dict:
        raw = self.request(f"/memory/files/{project}/proof.md", text=True)
        require(raw == content, "exact memory readback differs after replacement")
        history = self.history_events(project)
        require(bool(expected_history) and all(history.get(key) == value for key, value in expected_history.items()), "durable history lost or changed fixture events")
        response = self.request("/memory/search", {
            "query": "persistent amber lighthouse", "project": project,
            "retrieval_mode": "fast", "sources": ["topic_rollups"], "limit": 5,
        })
        # Raw search is the literal retrieval surface, not the agent boundary
        # envelope. Check its own terminal state without fabricating a contract.
        require(isinstance(response, dict), "search response is not an object")
        require(response.get("result_state") == "ready" and response.get("degraded") is False, "search is not ready and non-degraded")
        sources = response.get("source_summary")
        require(isinstance(sources, dict), "search source summary missing")
        for field in ("pending_sources", "failed_sources", "timed_out_sources", "continuation_unavailable_sources"):
            require(isinstance(sources.get(field), list) and not sources[field], f"search has unresolved {field}")
        rows = response.get("results")
        require(isinstance(rows, list) and all(isinstance(row, dict) for row in rows), "search results malformed")
        content_hash = hashlib.sha256(content).hexdigest()
        require(any(
            row.get("project") == project and row.get("file") == "proof.md"
            and row.get("event_id") in expected_history and row.get("content_hash") in (content_hash, f"sha256:{content_hash}")
            and "persistent amber lighthouse" in str(row.get("summary", "")).lower()
            for row in rows
        ), "exact fixture not found through search")
        return {"content_sha256": content_hash, "history_events": len(history), "history_sha256": digest(expected_history), "search_hits": len(rows), "search_result_state": response["result_state"], "search_results_sha256": digest(rows)}

    def create_state(self, project: str, content: bytes) -> dict:
        claim = self.request("/memory/claims", {
            "project": project, "subject": "lighthouse", "predicate": "fixture", "object": "amber",
            "statement": "The disposable lighthouse fixture is amber.",
            "support": [{"ref_id": f"{project}::proof.md", "kind": "memory", "excerpt": "Persistent amber lighthouse."}],
        })
        require_contract(claim)
        session = self.request("/v1/agents/sessions", {"project": project, "agent_id": "lite-proof", "objective": "Retain disposable state"})
        session_id = session["session"]["id"]
        task = self.request("/agents/tasks", {
            "schema_id": "agent_task_manifest.v1", "contract_version": 1,
            "task_id": "task-" + self.owner, "project": project, "objective": "Retain this queued fixture; do not execute it",
            "acceptance_criteria": ["Fixture survives replacement"], "task_class": "non_coding",
            "execution_profile": "local-default", "risk_level": "low", "approval_policy": {"required": True},
            "context_request": {"content_hash": "sha256:" + hashlib.sha256(content).hexdigest(), "session_id": session_id, "topic_path": "proof/persistence"},
            "recipients": [{"principal_id": "lite-proof", "role": "reviewer", "project": project, "observer": False, "session_id": session_id}],
            "review_owner": "lite-proof", "requesting_agent_id": "lite-proof", "idempotency_key": "lite-proof:" + self.owner,
        })
        return {"claim": claim["claim"], "task": task["task"]}

    def verify_state(self, project: str, expected: dict) -> dict:
        claims = self.request("/memory/claims/query", {"project": project, "query": "lighthouse", "limit": 5})
        require_contract(claims)
        claim_id = expected["claim"]["claim_id"]
        matches = [row for row in claims.get("claims", []) if row.get("claim_id") == claim_id]
        require(len(matches) == 1 and digest(matches[0]) == digest(expected["claim"]), "claim lost or changed")
        tasks = self.request(f"/agents/tasks?project={project}&limit=5")
        task_id = expected["task"]["task_id"]
        matches = [row for row in tasks.get("tasks", []) if row.get("task_id") == task_id]
        require(len(matches) == 1 and digest(matches[0]) == digest(expected["task"]), "task lost or changed")
        return {"claim_id": claim_id, "claim_sha256": digest(expected["claim"]), "task_id": task_id, "task_sha256": digest(expected["task"])}


def execute(args: argparse.Namespace, root: Path) -> dict:
    proof = Acceptance(args.docker_context, root, args.timeout)
    receipt: dict = {"schema_id": "hf_lite_acceptance.v1", "ok": False, "scope": "same_image_replacement", "coverage": ["memory", "history", "search", "claims", "tasks"], "source_bound": False, "phases": []}
    try:
        context = json.loads(proof.docker("context", "inspect", args.docker_context))[0]
        require_local_context(context)
        baseline = image_identity(json.loads(proof.docker("image", "inspect", args.image))[0])
        upgrade = None
        if args.upgrade_image:
            upgrade = image_identity(json.loads(proof.docker("image", "inspect", args.upgrade_image))[0])
            require(upgrade["id"] != baseline["id"], "upgrade proof requires two different images")
            require(upgrade["platform"] == baseline["platform"], "upgrade proof requires matching platforms")
            receipt["scope"] = "replacement_upgrade_rollback"
        receipt["images"] = {"baseline": baseline, "upgrade": upgrade}
        expected_commit = getattr(args, "expected_source_commit", None)
        expected_tree = getattr(args, "expected_source_tree", None)
        if expected_commit:
            candidate = upgrade or baseline
            require(candidate["source_commit"] == expected_commit and candidate["source_tree"] == expected_tree, "candidate image does not match expected source")
        project = "lite-proof-" + proof.owner
        content = b"Persistent amber lighthouse. This disposable fixture must survive replacement.\n"
        ready = proof.start(baseline)
        source_bound = runtime_source_bound(baseline, ready)
        response = proof.request("/memory/write", {
            "projectName": project, "fileName": "proof.md", "topicPath": "proof/persistence", "content": content.decode(),
        })
        require(response.get("ok") is True, "fixture write was not accepted")
        require_contract(response)
        expected_history = proof.history_events(project)
        expected_state = proof.create_state(project, content)
        first = proof.verify(project, content, expected_history)
        receipt["phases"].append({"phase": "initial", "build": ready.get("build"), **first, **proof.verify_state(project, expected_state)})
        proof.stop()
        sequence = [("replacement", baseline)]
        if upgrade:
            sequence += [("upgrade", upgrade), ("rollback", baseline)]
        for phase, image in sequence:
            ready = proof.start(image)
            source_bound = runtime_source_bound(image, ready) and source_bound
            checks = proof.verify(project, content, expected_history)
            receipt["phases"].append({"phase": phase, "build": ready.get("build"), **checks, **proof.verify_state(project, expected_state)})
            proof.stop()
        require(not expected_commit or source_bound, "source-bound proof was required")
        receipt["source_bound"] = source_bound
        receipt["ok"] = True
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        receipt["error"] = str(error).replace(proof.api_key, "[redacted]")[-4000:]
        if proof.started:
            try:
                receipt["container_log_tail"] = proof.docker("logs", "--tail", "30", proof.name).replace(proof.api_key, "[redacted]")[-4000:]
            except (OSError, RuntimeError, subprocess.SubprocessError):
                receipt["container_log_tail"] = "unavailable"
    finally:
        try:
            proof.stop()
        except (OSError, RuntimeError, subprocess.SubprocessError) as error:
            receipt["ok"] = False
            receipt["cleanup_error"] = str(error).replace(proof.api_key, "[redacted]")[-4000:]
        receipt["helper_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        (root / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--upgrade-image")
    parser.add_argument("--docker-context", required=True)
    parser.add_argument("--artifact-dir", type=Path, required=True)
    parser.add_argument("--timeout", type=int, default=90)
    parser.add_argument("--expected-source-commit")
    parser.add_argument("--expected-source-tree")
    args = parser.parse_args()
    if not 5 <= args.timeout <= 300:
        parser.error("--timeout must be between 5 and 300 seconds")
    if bool(args.expected_source_commit) != bool(args.expected_source_tree):
        parser.error("expected source commit and tree must be supplied together")
    for value in (args.expected_source_commit, args.expected_source_tree):
        if value and not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", value):
            parser.error("expected source identity must be a complete Git object ID")
    root = args.artifact_dir.expanduser().absolute()
    # Do not reuse evidence or bind an existing operator directory into a fixture.
    try:
        root.mkdir(mode=0o700, parents=True, exist_ok=False)
    except OSError:
        parser.error("--artifact-dir must be a new writable directory")
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    receipt = execute(args, root.resolve())
    print(json.dumps({"ok": receipt["ok"], "scope": receipt["scope"], "phases": len(receipt["phases"])}))
    return 0 if receipt["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
