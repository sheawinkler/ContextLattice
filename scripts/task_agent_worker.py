#!/usr/bin/env python3
"""
Lightweight task worker for ContextLattice agent tasks.
Claims tasks from the orchestrator and routes them to a runner (Trae, Letta, etc.)
or a simple local model call when no runner is configured.
"""

from __future__ import annotations

import argparse
import contextlib
import contextvars
import errno
import fcntl
import hashlib
import http.client
import json
import os
import re
import secrets
import shlex
import socket
import stat
import struct
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional
from urllib.parse import urlencode, urljoin, urlsplit

RUNNERS_DIR = Path(__file__).resolve().parent / "agent_runners"
if str(RUNNERS_DIR) not in sys.path:
    sys.path.insert(0, str(RUNNERS_DIR))

try:
    from scripts.contextlattice_client import (
        ContextLatticeClient,
        build_orchestrator_headers,
        resolve_orchestrator_api_key,
    )
    from scripts.agent_contracts import attach_format_contract, validate_agent_contract_payload
    from scripts.context_expansion_runtime import ContextExpansionRuntime
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from contextlattice_client import (  # type: ignore[no-redef]
        ContextLatticeClient,
        build_orchestrator_headers,
        resolve_orchestrator_api_key,
    )
    from agent_contracts import attach_format_contract, validate_agent_contract_payload  # type: ignore[no-redef]
    from context_expansion_runtime import ContextExpansionRuntime

try:
    from runner_quality import record_runner_quality
except ModuleNotFoundError:  # pragma: no cover - runner metrics are fail-open
    record_runner_quality = None  # type: ignore[assignment]

try:
    from scripts.task_agent_execution import (
        ExecutionBlocked,
        PublicationNotAcknowledged,
        WorkerAuthSnapshot,
        start_lease_heartbeat,  # noqa: F401 - compatibility re-export
        claim_has_complete_fence,
        execute_claimed_task,
        extract_lease_fence,
        fenced_payload,
        reconcile_owned_workspaces,
        redact_public_value,
        redact_text,
    )
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from task_agent_execution import (  # type: ignore[no-redef]
        ExecutionBlocked,
        PublicationNotAcknowledged,
        WorkerAuthSnapshot,
        start_lease_heartbeat,  # noqa: F401 - compatibility re-export
        claim_has_complete_fence,
        execute_claimed_task,
        extract_lease_fence,
        fenced_payload,
        reconcile_owned_workspaces,
        redact_public_value,
        redact_text,
    )

DEFAULT_ORCH_URL = os.getenv(
    "CONTEXTLATTICE_ORCHESTRATOR_URL",
    os.getenv("CONTEXTLATTICE_ORCHESTRATOR_URL", "http://127.0.0.1:8075"),
)
DEFAULT_AGENT = os.getenv("TASK_AGENT", "trae")
DEFAULT_PROVIDER = os.getenv("TASK_MODEL_PROVIDER", os.getenv("ORCH_INFER_PROVIDER", "auto"))
DEFAULT_MODEL = os.getenv("TASK_MODEL", "qwen3.5:9b")
DEFAULT_INFERENCE_CONTROL_PLANE_URL = os.getenv(
    "TASK_INFERENCE_CONTROL_PLANE_URL",
    DEFAULT_ORCH_URL,
)
AGENT_FIT_SELECTION_ACTIVATION_PATH = "/memory/agent-fit/selection/activation"
AGENT_FIT_GOVERNANCE_SCHEMA_ID = "frontier_t6_agent_fit_governance.v1"
AGENT_FIT_GOVERNANCE_FEATURE_ID = "frontier_agent_fit_governance"

ADAPTER_AGENT_ALIASES = {
    "pi": "pi",
    "pi-coding-agent": "pi",
    "droid": "droid",
    "factory-droid": "droid",
}

def _env_bool(name: str, default: bool) -> bool:
    raw = str(os.getenv(name, "")).strip().lower()
    if not raw:
        return bool(default)
    return raw in {"1", "true", "yes", "on"}


def _gateway_inference_enabled() -> bool:
    return _env_bool("TASK_INFERENCE_GATEWAY_ENABLED", True)


def _gateway_inference_required() -> bool:
    return _env_bool("TASK_INFERENCE_GATEWAY_REQUIRED", True)


def _build_task_messages(task: dict[str, Any], context_prompt: str | None = None) -> list[dict[str, str]]:
    prompt = task.get("title", "Task")
    payload = task.get("payload") or {}
    body = f"{prompt}\n\nPayload:\n{json.dumps(payload, indent=2)}"
    messages: list[dict[str, str]] = [
        {
            "role": "system",
            "content": (
                "You are a task runner. Provide a concise plan and next actions. "
                "Use the supplied factual context pack when present and copy numeric facts verbatim."
            ),
        },
    ]
    if context_prompt:
        messages.append({"role": "system", "content": context_prompt})
    messages.append({"role": "user", "content": body})
    return messages


def _run_llm_task_via_gateway(
    control_plane_url: str,
    provider: str,
    model: str,
    task: dict[str, Any],
    context_prompt: str | None,
    *,
    base_url_override: str | None = None,
    api_key: str | None = None,
    timeout_secs: int | None = None,
    cancel_event: threading.Event | None = None,
) -> tuple[str, dict[str, Any]]:
    if cancel_event is not None and cancel_event.is_set():
        raise ExecutionBlocked(
            "lease_lost",
            "authoritative lease was lost before gateway inference started",
        )
    payload: dict[str, Any] = {
        "provider": provider,
        "model": model,
        "messages": _build_task_messages(task, context_prompt=context_prompt),
    }
    request_id = secrets.token_hex(32)
    payload["request_id"] = request_id
    if base_url_override:
        payload["base_url"] = base_url_override
    if api_key:
        payload["api_key"] = api_key
    if timeout_secs is None:
        response = _post(
            control_plane_url,
            "/v1/inference/chat",
            payload,
            cancel_event=cancel_event,
            cancellation_request_id=request_id,
        )
    else:
        if isinstance(timeout_secs, bool) or int(timeout_secs) <= 0:
            raise ValueError("gateway inference timeout must be a positive integer")
        exact_timeout = int(timeout_secs)
        payload["timeout_secs"] = exact_timeout
        response = _post(
            control_plane_url,
            "/v1/inference/chat",
            payload,
            timeout=exact_timeout,
            cancel_event=cancel_event,
            cancellation_request_id=request_id,
        )
    if cancel_event is not None and cancel_event.is_set():
        raise ExecutionBlocked(
            "lease_lost",
            "authoritative lease was lost during gateway inference",
            execution_observed=True,
        )
    content = _redact_runner_text(str(response.get("content") or ""))
    if not content.strip():
        raise RuntimeError("gateway-go returned empty inference content")
    raw_route = response.get("route") if isinstance(response.get("route"), dict) else {}
    sanitized_route = _redact_runner_value(raw_route)
    route_payload = dict(sanitized_route) if isinstance(sanitized_route, dict) else {}
    return content, route_payload


def _format_route_label_from_payload(
    route_payload: dict[str, Any],
) -> str:
    provider = str(route_payload.get("provider") or "").strip().lower()
    if provider == "ollama_coreml":
        return "ollama/coreml"
    if provider == "ane_sidecar":
        return "ane_sidecar"
    if provider:
        return provider
    return "unknown"


def _runner_cmd_for_agent(agent: str) -> Optional[str]:
    agent = _normalize_agent_alias(agent)
    if os.getenv("TASK_AGENT_CMD"):
        return os.getenv("TASK_AGENT_CMD")
    if agent == "trae":
        return os.getenv("TRAE_CMD")
    if agent == "letta":
        return os.getenv("LETTA_CMD")
    if agent == "autogen":
        return os.getenv("AUTOGEN_CMD")
    if agent == "crewai":
        return os.getenv("CREWAI_CMD")
    if agent == "langgraph":
        return os.getenv("LANGGRAPH_CMD")
    if agent == "openhands":
        return os.getenv("OPENHANDS_CMD")
    if agent in {"hermes", "hermes-agent"}:
        return os.getenv("HERMES_AGENT_CMD") or os.getenv("HERMES_CMD")
    if agent == "opencode":
        return os.getenv("OPENCODE_CMD")
    if agent == "goose":
        return os.getenv("GOOSE_CMD")
    if agent == "eliza":
        return os.getenv("ELIZA_CMD")
    return None


def _run_command(cmd: str, env: dict[str, str]) -> int:
    argv = shlex.split(str(cmd or ""))
    if not argv:
        return 127
    return subprocess.run(
        argv,
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    ).returncode


def _legacy_runner_env(values: dict[str, str]) -> dict[str, str]:
    """Build a closed legacy child environment without ambient credentials."""

    allowed = {
        "TASK_ID",
        "TASK_TITLE",
        "TASK_PROJECT",
        "TASK_AGENT",
        "TASK_PAYLOAD",
        "TASK_MODEL_PROVIDER",
        "TASK_MODEL",
        "TASK_BASE_URL",
        "CONTEXTLATTICE_ORCHESTRATOR_URL",
        "MEMMCP_ORCHESTRATOR_URL",
        "CONTEXTLATTICE_SESSION_ID",
        "CONTEXTLATTICE_AGENT_ID",
        "TASK_CONTEXT_BUNDLE",
        "TASK_CONTEXT_PROMPT",
        "TASK_TOOL_CONTEXT_SLICES",
    }
    baseline = {
        "PATH": "/usr/local/bin:/usr/bin:/bin",
        "HOME": "/var/empty",
        "LANG": "C",
        "LC_ALL": "C",
    }
    for key, value in values.items():
        if key in allowed:
            baseline[key] = str(value)
    return baseline


def _normalize_agent_alias(agent: str) -> str:
    normalized = str(agent or "").strip().lower().replace("_", "-")
    return ADAPTER_AGENT_ALIASES.get(normalized, normalized)


def _agent_fit_opaque_digest(label: str, value: str) -> str:
    raw = f"{label}\x00{str(value or '').strip()}".encode("utf-8")
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def _agent_fit_selection_request(
    task: dict[str, Any], agent_choice: str, model: str
) -> tuple[dict[str, Any] | None, str, str]:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    receipt = payload.get("agent_fit_selection_receipt")
    if receipt is None:
        return None, "", ""
    if not isinstance(receipt, dict):
        raise ValueError("agent_fit_selection_receipt must be an object")
    if payload.get("agent_fit_selection_authorize") is not True:
        raise ValueError("agent_fit_selection_authorize=true is required")
    if task.get("approved") is not True:
        raise ValueError("approved=true is required for governed selection")
    kind = str(receipt.get("kind") or "").strip().lower()
    selected_id = str(receipt.get("selected_id") or "").strip()
    if kind not in {"runner", "model"} or not selected_id:
        raise ValueError("governed selection requires a runner or model selected_id")
    task_id = str(task.get("id") or "").strip()
    if str(receipt.get("task_id") or "").strip() != task_id:
        raise ValueError("governed selection task_id does not match the claimed task")
    if kind == "runner" and _normalize_agent_alias(selected_id) != agent_choice:
        raise ValueError("governed runner selection does not match the explicit task agent")
    if kind == "model" and selected_id != str(model or "").strip():
        raise ValueError("governed model selection does not match the explicit task model")
    if os.getenv("TASK_AGENT_CMD"):
        return None, kind, "explicit_task_agent_cmd_override"
    try:
        expected_generation = int(payload.get("agent_fit_selection_expected_generation"))
    except (TypeError, ValueError) as exc:
        raise ValueError("agent_fit_selection_expected_generation is required") from exc
    if expected_generation < 1:
        raise ValueError("agent_fit_selection_expected_generation must be positive")
    receipt_id = str(receipt.get("receipt_id") or "").strip()
    if not receipt_id:
        raise ValueError("governed selection receipt_id is required")
    idempotency_key = str(
        payload.get("agent_fit_selection_idempotency_key")
        or f"task-worker-{hashlib.sha256((task_id + chr(0) + receipt_id).encode('utf-8')).hexdigest()[:24]}"
    ).strip()
    request = {
        "operation": "authorize",
        "project": str(task.get("project") or payload.get("project") or "").strip(),
        "approved": True,
        "reason": "authorize an explicitly selected external task worker target",
        "idempotency_key": idempotency_key,
        "expected_generation": expected_generation,
        "selection_receipt": dict(receipt),
    }
    if not request["project"]:
        raise ValueError("project is required for governed selection")
    return request, kind, selected_id


def _authorize_agent_fit_selection(
    orchestrator_url: str,
    task: dict[str, Any],
    agent_choice: str,
    model: str,
) -> dict[str, Any]:
    request, kind, selected_id = _agent_fit_selection_request(
        task, agent_choice, model
    )
    if request is None:
        return {
            "requested": bool(kind),
            "authorized": False,
            "reason": selected_id or "not_requested",
        }
    response = _post(
        orchestrator_url,
        AGENT_FIT_SELECTION_ACTIVATION_PATH,
        request,
        timeout=20.0,
    )
    result = response.get("result") if isinstance(response.get("result"), dict) else {}
    safety = response.get("safety") if isinstance(response.get("safety"), dict) else {}
    access = response.get("access") if isinstance(response.get("access"), dict) else {}
    receipt = response.get("receipt") if isinstance(response.get("receipt"), dict) else {}
    format_contract = (
        response.get("format_contract")
        if isinstance(response.get("format_contract"), dict)
        else {}
    )
    validation = (
        format_contract.get("validation")
        if isinstance(format_contract.get("validation"), dict)
        else {}
    )
    expected_selected_digest = _agent_fit_opaque_digest(
        "frontier-t6-governance-selected", selected_id
    )
    expected_task_digest = _agent_fit_opaque_digest(
        "frontier-t6-governance-task", str(task.get("id") or "")
    )
    execution_flags = (
        bool(result.get("execution_performed")),
        bool(safety.get("gateway_execution_performed")),
        bool(safety.get("model_execution_performed")),
        bool(safety.get("subprocess_execution_performed")),
        bool(safety.get("prompt_injection_performed")),
        bool(safety.get("ordinary_memory_mutated")),
    )
    valid = (
        response.get("ok") is True
        and response.get("schema_id") == AGENT_FIT_GOVERNANCE_SCHEMA_ID
        and response.get("feature_id") == AGENT_FIT_GOVERNANCE_FEATURE_ID
        and response.get("operation") == "authorize"
        and result.get("activation_owner") == "external_task_worker"
        and result.get("activation_delivery") == "explicit_pull"
        and result.get("kind") == kind
        and result.get("selected_id_digest") == expected_selected_digest
        and result.get("task_digest") == expected_task_digest
        and not any(execution_flags)
        and _int_value(safety.get("network_calls"), -1) == 0
        and safety.get("dispatch_owner") == "external_task_worker"
        and safety.get("dispatch_mode") == "explicit_pull"
        and access.get("workspace_project_binding_verified") is True
        and format_contract.get("schema_id") == AGENT_FIT_GOVERNANCE_SCHEMA_ID
        and validation.get("status") == "passed"
        and bool(receipt.get("receipt_id"))
        and bool(receipt.get("receipt_hash"))
        and _int_value(receipt.get("policy_generation"), -1)
        == _int_value(request.get("expected_generation"), -2)
    )
    if not valid:
        raise RuntimeError("governed Agent Fit selection receipt failed validation")
    authorized = {
        "requested": True,
        "authorized": True,
        "kind": kind,
        "selected_id": selected_id,
        "activation_id": str(result.get("activation_id") or ""),
        "activation_owner": "external_task_worker",
        "activation_delivery": "explicit_pull",
        "governance_receipt_id": str(receipt.get("receipt_id") or ""),
        "governance_receipt_hash": str(receipt.get("receipt_hash") or ""),
        "policy_generation": receipt.get("policy_generation"),
        "execution_performed": False,
    }
    sanitized = _redact_runner_value(authorized)
    return dict(sanitized) if isinstance(sanitized, dict) else {}


def _repo_root() -> Path:
    return Path(__file__).resolve().parents[1]


def _runner_adapter_for_agent(agent: str) -> list[str] | None:
    normalized = _normalize_agent_alias(agent)
    script_by_agent = {
        "pi": _repo_root() / "scripts" / "agent_runners" / "pi_runner.py",
        "droid": _repo_root() / "scripts" / "agent_runners" / "droid_runner.py",
    }
    script = script_by_agent.get(normalized)
    if not script or not script.is_file():
        return None
    return [sys.executable, str(script)]


def _runner_now_iso() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _redact_runner_text(value: Any) -> str:
    return redact_text(value)


def _redact_runner_value(value: Any) -> Any:
    return redact_public_value(value)


def _runner_tail(value: Any, limit: int = 4000) -> str:
    text = _redact_runner_text(value)
    if len(text) <= limit:
        return text
    return text[-limit:]


def _runner_timeout_secs(task_payload: dict[str, Any]) -> int | None:
    for key in ("timeout_secs", "max_runtime_secs"):
        try:
            value = int(task_payload.get(key) or 0)
        except Exception:
            value = 0
        if value > 0:
            return value + 30
    try:
        value = int(os.getenv("TASK_RUNNER_TIMEOUT_SECS", "0") or 0)
    except Exception:
        value = 0
    return value if value > 0 else None


def _fallback_runner_result(
    agent: str,
    status: str,
    exit_code: int,
    summary: str,
    stdout: str = "",
    stderr: str = "",
) -> dict[str, Any]:
    now = _runner_now_iso()
    result = attach_format_contract(
        "runner_result.v1",
        {
            "schema_id": "runner_result.v1",
            "ok": False,
            "runner": agent,
            "agent": agent,
            "agent_id": f"{agent}_agent",
            "task_id": "",
            "project": "",
            "status": status,
            "exit_code": exit_code,
            "started_at": now,
            "completed_at": now,
            "duration_secs": 0.0,
            "summary": _redact_runner_text(summary)[:3000],
            "stdout_tail": _runner_tail(stdout),
            "stderr_tail": _runner_tail(stderr),
            "artifacts": [],
            "warnings": [],
            "metadata": {"adapter": "task_agent_worker", "parse_fallback": True},
        },
    )
    sanitized = _redact_runner_value(result)
    return dict(sanitized) if isinstance(sanitized, dict) else {}


def _parse_adapter_result(agent: str, stdout: str, stderr: str, exit_code: int) -> dict[str, Any]:
    text = stdout.strip()
    candidates = [text]
    if "\n" in text:
        candidates.extend(line.strip() for line in reversed(text.splitlines()) if line.strip().startswith("{"))
    for candidate in candidates:
        if not candidate:
            continue
        try:
            parsed = json.loads(candidate)
        except json.JSONDecodeError:
            continue
        if isinstance(parsed, dict):
            sanitized = _redact_runner_value(parsed)
            return dict(sanitized) if isinstance(sanitized, dict) else {}
    return _fallback_runner_result(
        agent,
        "failed",
        exit_code,
        "Runner adapter did not emit valid runner_result.v1 JSON",
        stdout=stdout,
        stderr=stderr,
    )


def _run_adapter(argv: list[str], env: dict[str, str], cwd: Path, timeout: int | None, agent: str) -> dict[str, Any]:
    try:
        proc = subprocess.run(
            argv,
            cwd=str(cwd),
            env=env,
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
        )
        result = _parse_adapter_result(agent, proc.stdout, proc.stderr, proc.returncode)
        if "exit_code" not in result:
            result["exit_code"] = proc.returncode
        return result
    except subprocess.TimeoutExpired as exc:
        return _fallback_runner_result(
            agent,
            "timed_out",
            124,
            f"Runner adapter timed out after {timeout} seconds",
            stdout=exc.stdout or "",
            stderr=exc.stderr or "",
        )
    except OSError as exc:
        return _fallback_runner_result(
            agent,
            "failed",
            1,
            f"Runner adapter execution failed: {_redact_runner_text(str(exc))}",
        )


def _compact_runner_result(result: dict[str, Any]) -> dict[str, Any]:
    metadata = result.get("metadata") if isinstance(result.get("metadata"), dict) else {}
    compact = {
        "schema_id": str(result.get("schema_id") or "runner_result.v1"),
        "ok": bool(result.get("ok")),
        "runner": str(result.get("runner") or ""),
        "agent": str(result.get("agent") or ""),
        "agent_id": str(result.get("agent_id") or ""),
        "task_id": str(result.get("task_id") or ""),
        "project": str(result.get("project") or ""),
        "status": str(result.get("status") or ""),
        "exit_code": result.get("exit_code"),
        "started_at": result.get("started_at"),
        "completed_at": result.get("completed_at"),
        "duration_secs": result.get("duration_secs"),
        "summary": _redact_runner_text(str(result.get("summary") or ""))[:1500],
        "stdout_tail": _runner_tail(str(result.get("stdout_tail") or ""), 1200),
        "stderr_tail": _runner_tail(str(result.get("stderr_tail") or ""), 1200),
        "artifacts": result.get("artifacts") if isinstance(result.get("artifacts"), list) else [],
        "warnings": result.get("warnings") if isinstance(result.get("warnings"), list) else [],
        "metadata": {
            "adapter": metadata.get("adapter"),
            "lease": metadata.get("lease"),
            "agent_state": metadata.get("agent_state"),
            "flags_note": metadata.get("flags_note"),
            "install_hint": metadata.get("install_hint"),
        },
    }
    sanitized = _redact_runner_value(compact)
    return dict(sanitized) if isinstance(sanitized, dict) else {}


def _task_status_for_runner_result(result: dict[str, Any]) -> str:
    status = str(result.get("status") or "").lower()
    if status == "succeeded" and bool(result.get("ok")):
        return "succeeded"
    if status in {"blocked", "missing_binary", "invalid_task", "skipped"}:
        return "blocked"
    return "failed"


def _message_for_adapter_result(agent: str, result: dict[str, Any]) -> str:
    status = str(result.get("status") or "").lower()
    if status == "succeeded" and bool(result.get("ok")):
        return f"Completed by {agent} adapter"
    if status == "timed_out":
        return "Runner adapter timed out"
    if status == "missing_binary":
        return "Runner binary missing"
    return "Runner adapter failed"


def _agent_state_for_runner_status(result: dict[str, Any]) -> dict[str, Any]:
    status = str(result.get("status") or "").lower()
    if status == "succeeded" and bool(result.get("ok")):
        state = "done"
    elif status in {"blocked", "missing_binary", "invalid_task", "timed_out"}:
        state = "blocked"
    else:
        state = "blocked"
    return {"schema_id": "contextlattice_agent_lifecycle_state.v1", "state": state, "authority": "adapter", "source": "task_agent_worker"}


def _compact_runner_quality_sample(sample: dict[str, Any], storage: dict[str, Any]) -> dict[str, Any]:
    quality = sample.get("context_pack_quality") if isinstance(sample.get("context_pack_quality"), dict) else {}
    token_impact = sample.get("token_impact") if isinstance(sample.get("token_impact"), dict) else {}
    outcome = sample.get("outcome") if isinstance(sample.get("outcome"), dict) else {}
    compact = {
        "schema_id": str(sample.get("schema_id") or "runner_quality_sample.v1"),
        "sample_id": str(sample.get("sample_id") or ""),
        "runner": str(sample.get("runner") or ""),
        "agent": str(sample.get("agent") or ""),
        "agent_id": str(sample.get("agent_id") or ""),
        "task_id": str(sample.get("task_id") or ""),
        "project": str(sample.get("project") or ""),
        "task_class": str(sample.get("task_class") or "general"),
        "status": str(sample.get("status") or ""),
        "ok": bool(sample.get("ok")),
        "duration_secs": sample.get("duration_secs"),
        "context_pack_quality": {
            "sample_id": quality.get("sample_id"),
            "quality_score": quality.get("quality_score"),
            "confidence": quality.get("confidence"),
            "exact_prompt_tokens_saved": quality.get("exact_prompt_tokens_saved"),
            "modeled_inference_tokens_avoided": quality.get("modeled_inference_tokens_avoided"),
        },
        "token_impact": {
            "saved_tokens_estimate": token_impact.get("saved_tokens_estimate"),
            "provider_prompt_tokens": token_impact.get("provider_prompt_tokens"),
            "provider_completion_tokens": token_impact.get("provider_completion_tokens"),
            "provider_total_tokens": token_impact.get("provider_total_tokens"),
        },
        "outcome": {
            "first_pass_success": outcome.get("first_pass_success"),
            "repair_required": outcome.get("repair_required"),
            "blocked": outcome.get("blocked"),
            "failed": outcome.get("failed"),
            "retry_count": outcome.get("retry_count"),
            "observed_followup_tokens": outcome.get("observed_followup_tokens"),
        },
        "storage": {
            "enabled": bool(storage.get("enabled", False)),
            "sample_id": storage.get("sample_id"),
            "max_bytes": storage.get("max_bytes"),
            "max_samples": storage.get("max_samples"),
        },
    }
    sanitized = _redact_runner_value(compact)
    return dict(sanitized) if isinstance(sanitized, dict) else {}


def _record_runner_quality_sample(
    *,
    task: dict[str, Any],
    agent: str,
    result: dict[str, Any],
    context_bundle: dict[str, Any],
    task_status: str,
    message: str,
    route_payload: dict[str, Any],
) -> dict[str, Any]:
    if record_runner_quality is None:
        return {"ok": False, "reason": "runner_quality_module_unavailable"}
    try:
        safe_task = _redact_runner_value(task)
        safe_result = _redact_runner_value(result)
        safe_context_bundle = _redact_runner_value(context_bundle)
        safe_route_payload = _redact_runner_value(route_payload)
        sample, storage = record_runner_quality(
            task=dict(safe_task) if isinstance(safe_task, dict) else {},
            agent=_redact_runner_text(agent),
            result=dict(safe_result) if isinstance(safe_result, dict) else {},
            context_bundle=(
                dict(safe_context_bundle)
                if isinstance(safe_context_bundle, dict)
                else {}
            ),
            task_status=_redact_runner_text(task_status),
            message=_redact_runner_text(message),
            route_payload=(
                dict(safe_route_payload)
                if isinstance(safe_route_payload, dict)
                else {}
            ),
        )
        return _compact_runner_quality_sample(sample, storage)
    except Exception as exc:
        return {"ok": False, "reason": "runner_quality_record_failed", "error": _redact_runner_text(str(exc))[:240]}


def _context_pack_quality_sample_id(bundle: dict[str, Any]) -> str:
    quality = bundle.get("context_pack_quality") if isinstance(bundle.get("context_pack_quality"), dict) else {}
    if not quality:
        pack = bundle.get("context_pack") if isinstance(bundle.get("context_pack"), dict) else {}
        quality = pack.get("context_pack_quality") if isinstance(pack.get("context_pack_quality"), dict) else {}
    return str(quality.get("sample_id") or "").strip()


def _task_class(task: dict[str, Any]) -> str:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    for key in ("task_class", "taskClass", "runner_task_class", "runnerTaskClass", "role", "intent", "operation"):
        value = payload.get(key)
        if value is not None and str(value).strip():
            return re.sub(r"[^a-z0-9_.:/-]+", "-", str(value).strip().lower()).strip("-_.:/")[:80] or "general"
    return "general"


def _int_value(value: Any, default: int = 0) -> int:
    try:
        if value is None or value == "":
            return default
        return int(value)
    except (TypeError, ValueError):
        return default


def _post_context_pack_outcome(
    orchestrator_url: str,
    *,
    task: dict[str, Any],
    context_bundle: dict[str, Any],
    status: str,
    source: str,
    calibration_eligible: bool,
    outcome_class: str,
    result_metadata: dict[str, Any] | None = None,
) -> dict[str, Any]:
    sample_id = _context_pack_quality_sample_id(context_bundle)
    if not sample_id:
        return {"ok": False, "recorded": False, "reason": "context_pack_quality_sample_missing"}
    metadata = result_metadata if isinstance(result_metadata, dict) else {}
    retry_count = max(0, _int_value(metadata.get("retry_count"), 0))
    repair_required = _env_bool_value(metadata.get("repair_required")) or retry_count > 0
    normalized_status = str(status or "").strip().lower()
    explicit_first_pass = metadata.get("first_pass_success")
    if isinstance(explicit_first_pass, bool):
        first_pass_success = explicit_first_pass
    else:
        first_pass_success = normalized_status == "succeeded" and not repair_required
    provider_usage = metadata.get("provider_usage") if isinstance(metadata.get("provider_usage"), dict) else {}
    task_payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    payload: dict[str, Any] = {
        "sample_id": sample_id,
        "task_id": str(task.get("id") or ""),
        "task_class": _task_class(task),
        "first_pass_success": first_pass_success,
        "repair_required": repair_required,
        "retry_count": retry_count,
        "observed_followup_tokens": max(0, _int_value(metadata.get("observed_followup_tokens"), 0)),
        "outcome_source": f"task_agent_worker.{source}"[:80],
        "outcome_class": str(outcome_class or "unspecified")[:80],
        "context_attribution": str(metadata.get("context_attribution") or "observed_execution_result")[:80],
        "calibration_eligible": bool(calibration_eligible),
        "project": str(task.get("project") or task_payload.get("project") or "")[:160],
    }
    for field in ("policy_id", "policy_arm", "policy_phase"):
        value = metadata.get(field, task_payload.get(field))
        if value is not None and str(value).strip():
            payload[field] = str(value).strip()[:160]
    for field, keys in {
        "provider_prompt_tokens": ("prompt_tokens", "input_tokens"),
        "provider_completion_tokens": ("completion_tokens", "output_tokens"),
        "provider_total_tokens": ("total_tokens",),
    }.items():
        value = 0
        for key in keys:
            value = max(value, _int_value(provider_usage.get(key), 0))
        if value > 0:
            payload[field] = value
    try:
        safe_payload = _redact_runner_value(payload)
        response = _post(
            orchestrator_url,
            "/telemetry/context-pack-quality/outcome",
            dict(safe_payload) if isinstance(safe_payload, dict) else {},
        )
    except Exception as exc:
        return {
            "ok": False,
            "recorded": False,
            "reason": "outcome_post_failed",
            "error": _redact_runner_text(str(exc))[:240],
            "sample_id": sample_id,
        }
    outcome = response.get("outcome") if isinstance(response.get("outcome"), dict) else {}
    result = {
        "ok": bool(response.get("ok", False)),
        "recorded": bool(response.get("recorded", response.get("ok", False))),
        "duplicate": bool(response.get("duplicate", False)),
        "sample_id": sample_id,
        "outcome_id": str(outcome.get("outcome_id") or ""),
        "outcome_class": str(outcome.get("outcome_class") or payload["outcome_class"]),
        "calibration_eligible": bool(outcome.get("calibration_eligible", calibration_eligible)),
        "policy_id": str(outcome.get("policy_id") or payload.get("policy_id") or ""),
        "policy_arm": str(outcome.get("policy_arm") or payload.get("policy_arm") or ""),
        "policy_phase": str(outcome.get("policy_phase") or payload.get("policy_phase") or ""),
    }
    sanitized = _redact_runner_value(result)
    return dict(sanitized) if isinstance(sanitized, dict) else {}


def _env_bool_value(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    return str(value or "").strip().lower() in {"1", "true", "yes", "on"}


def _orchestrator_headers() -> dict[str, str]:
    return build_orchestrator_headers(resolve_orchestrator_api_key(role="worker"))


WORKER_STATE_FILE_NAME = "worker_identity.json"
WORKER_STATE_SLOT_LIMIT = 128
WORKER_STATE_ROOT_ENV_NAMES = (
    "TASK_AGENT_WORKER_STATE_ROOT",
    "TASK_WORKER_STATE_ROOT",
    "CONTEXTLATTICE_TASK_WORKER_STATE_ROOT",
)
WORKER_STATE_DISPATCHER_ID_ENV_NAMES = (
    "TASK_AGENT_DISPATCHER_ID",
    "TASK_WORKER_DISPATCHER_ID",
    "CONTEXTLATTICE_TASK_WORKER_DISPATCHER_ID",
)
WORKER_PRINCIPAL_ENV_NAMES = (
    "TASK_WORKER_PRINCIPAL",
    "CONTEXTLATTICE_WORKER_PRINCIPAL",
)
WORKER_WORKSPACE_ENV_NAMES = (
    "TASK_WORKER_WORKSPACE",
    "CONTEXTLATTICE_WORKER_WORKSPACE",
)
WORKER_INSTANCE_CREDENTIAL_HEADER = "X-Worker-Instance-Credential"
WORKER_INSTANCE_CREDENTIAL_MAX_BYTES = 256
WORKER_INSTANCE_CREDENTIAL_RE = re.compile(r"[0-9a-f]{64}\Z", re.ASCII)

_WORKER_STATE_LOCKS: dict[str, int] = {}
_WORKER_STATE_PATHS: dict[str, Path] = {}
_WORKER_AUTH_CONTEXT: contextvars.ContextVar[tuple[str, str] | None] = contextvars.ContextVar(
    "task_worker_auth_context", default=None
)
_LAST_RESPONSE_HEADERS: contextvars.ContextVar[dict[str, str] | None] = contextvars.ContextVar(
    "task_worker_last_response_headers", default=None
)
_WORKER_STATE_FIELDS = {
    "requested_worker_id",
    "worker_instance_id",
    "canonical_worker_id",
    "worker_identity_update_generation",
    "acknowledged_generation",
    "identity_id",
    "identity_digest",
    "retirement_receipt_digest",
    "principal_id",
    "workspace_id",
    "dispatcher_id",
    "worker_instance_credential",
    "retired",
    "pending_identity_update",
}
WORKER_IDENTITY_GENERATION_MAX = (1 << 63) - 1
PUBLIC_LEASE_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:@-]{0,255}\Z", re.ASCII)


def _normalise_worker_public_id(value: Any, field: str, *, lower: bool = False) -> str:
    if not isinstance(value, str):
        raise _state_failure()
    text = value.strip()
    if not PUBLIC_LEASE_ID_RE.fullmatch(text):
        raise RuntimeError(f"{field} is not a valid public lease identifier")
    return text.lower() if lower else text


def _worker_state_root() -> Path:
    configured = next((str(os.getenv(name) or "").strip() for name in WORKER_STATE_ROOT_ENV_NAMES if str(os.getenv(name) or "").strip()), "")
    if configured:
        return Path(configured).expanduser()
    return Path.home() / ".contextlattice" / "task-agent"


def _state_failure() -> RuntimeError:
    return RuntimeError("worker identity state persistence failed")


def _prepare_worker_state_root() -> Path:
    root = _worker_state_root()
    try:
        root.mkdir(parents=True, exist_ok=True, mode=0o700)
        root_stat = root.stat()
        if not stat.S_ISDIR(root_stat.st_mode) or root_stat.st_uid != os.getuid():
            raise OSError
        if root_stat.st_mode & 0o077:
            os.chmod(root, 0o700)
        root_stat = root.stat()
        if root_stat.st_mode & 0o077:
            raise OSError
    except (OSError, ValueError):
        raise _state_failure() from None
    return root


def _worker_state_path(root: Path) -> Path:
    return root / WORKER_STATE_FILE_NAME


def _worker_dispatcher_id(value: Any = None) -> str:
    if value is None:
        value = next((os.getenv(name) for name in WORKER_STATE_DISPATCHER_ID_ENV_NAMES if os.getenv(name)), "")
    if not isinstance(value, str):
        raise _state_failure()
    value = value.strip()
    if value and len(value.encode("utf-8")) > 160:
        raise _state_failure()
    return value


def _worker_dispatcher_state_path(root: Path, dispatcher_id: str) -> Path:
    digest = hashlib.sha256(dispatcher_id.encode("utf-8")).hexdigest()[:24]
    return root / f"worker_identity.dispatcher-{digest}.json"


def _worker_authority_config() -> tuple[str, str]:
    principal = next((str(os.getenv(name) or "").strip() for name in WORKER_PRINCIPAL_ENV_NAMES if str(os.getenv(name) or "").strip()), "")
    workspace = next((str(os.getenv(name) or "").strip() for name in WORKER_WORKSPACE_ENV_NAMES if str(os.getenv(name) or "").strip()), "")
    if bool(principal) != bool(workspace):
        raise RuntimeError("worker principal and workspace must be configured together")
    return principal, workspace


@contextlib.contextmanager
def _worker_auth_scope(state: dict[str, Any]):
    instance = str(state.get("worker_instance_id") or "").strip()
    credential = str(state.get("worker_instance_credential") or "").strip()
    token = _WORKER_AUTH_CONTEXT.set((instance, credential))
    try:
        yield
    finally:
        _WORKER_AUTH_CONTEXT.reset(token)


def _worker_auth_snapshot(state: dict[str, Any]) -> WorkerAuthSnapshot:
    instance = str(state.get("worker_instance_id") or "").strip()
    credential = str(state.get("worker_instance_credential") or "").strip()
    if not instance or not credential or not WORKER_INSTANCE_CREDENTIAL_RE.fullmatch(credential):
        raise RuntimeError("worker instance credential is required for task execution")
    return WorkerAuthSnapshot(instance, credential)


class _WorkerIdentityCredentialMigrationRequired(RuntimeError):
    """The Gateway durably migrated a legacy identity and requires rotation."""


def _worker_auth_path_requires_proof(path: str) -> bool:
    """Return true only for worker identity/lease proof routes.

    `/agents/` contains public task reads, reviewer/recipient operations, and
    service projections. A bearer credential is deliberately absent from those
    requests even when a worker context exists.
    """
    value = str(path or "").split("?", 1)[0].rstrip("/")
    identity_prefixes = (
        "/agents/workers/identity",
        "/agents/tasks/worker/identity",
        "/agents/tasks/workers/identity",
    )
    registration_paths = {
        "/agents/workers/register",
        "/agents/tasks/worker/register",
        "/agents/tasks/workers/register",
    }
    if value in registration_paths or any(
        value == prefix or (value.startswith(prefix + "/") and "/" not in value[len(prefix) + 1 :])
        for prefix in identity_prefixes
    ):
        return True
    if value == "/agents/tasks/next":
        return True
    if re.fullmatch(r"/agents/tasks/[^/]+/(heartbeat|observe|status|cancel|publish|publication|cleanup)", value):
        return True
    return bool(re.fullmatch(r"/agents/tasks/[^/]+/attempts/[^/]+/(cleanup|publication)", value))


def _worker_auth_headers(
    path: str,
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> dict[str, str]:
    if not _worker_auth_path_requires_proof(path):
        return {}
    context: tuple[str, str] | None
    if auth_snapshot is not None:
        context = (auth_snapshot.worker_instance_id, auth_snapshot.worker_instance_credential)
    else:
        context = _WORKER_AUTH_CONTEXT.get()
    if context is None:
        return {}
    instance, credential = context
    headers: dict[str, str] = {}
    if instance:
        headers["X-Worker-Instance-ID"] = instance
    if credential:
        headers[WORKER_INSTANCE_CREDENTIAL_HEADER] = credential
    return headers


def _worker_state_slot_path(root: Path, slot: int) -> Path:
    if slot == 0:
        return _worker_state_path(root)
    return root / f"worker_identity.{slot}.json"


def _worker_state_lock_path(path: Path) -> Path:
    return Path(f"{path}.lock")


def _release_worker_state_lock(path: Path) -> None:
    key = str(path)
    fd = _WORKER_STATE_LOCKS.pop(key, None)
    if fd is None:
        return
    try:
        fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        try:
            os.close(fd)
        except OSError:
            pass


def _acquire_worker_state_lock(path: Path) -> int | None:
    key = str(path)
    existing = _WORKER_STATE_LOCKS.get(key)
    if existing is not None:
        return existing
    lock_path = _worker_state_lock_path(path)
    flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(lock_path, flags, 0o600)
        lock_stat = os.fstat(fd)
        if (
            not stat.S_ISREG(lock_stat.st_mode)
            or lock_stat.st_uid != os.getuid()
            or lock_stat.st_nlink != 1
            or lock_stat.st_mode & 0o077
        ):
            raise OSError
        os.fchmod(fd, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as exc:
            if exc.errno in {errno.EACCES, errno.EAGAIN}:
                os.close(fd)
                return None
            raise
    except (OSError, ValueError):
        try:
            os.close(fd)  # type: ignore[possibly-undefined]
        except (NameError, OSError):
            pass
        raise _state_failure() from None
    _WORKER_STATE_LOCKS[key] = fd
    return fd


def _read_worker_state_file(path: Path) -> dict[str, Any]:
    try:
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(path, flags)
        try:
            file_stat = os.fstat(fd)
            if (
                not stat.S_ISREG(file_stat.st_mode)
                or file_stat.st_uid != os.getuid()
                or file_stat.st_mode & 0o077
                or file_stat.st_nlink != 1
                or file_stat.st_size > 16 * 1024
            ):
                raise OSError
            raw = b""
            while len(raw) <= 16 * 1024:
                chunk = os.read(fd, 4096)
                if not chunk:
                    break
                raw += chunk
            if len(raw) > 16 * 1024:
                raise OSError
        finally:
            os.close(fd)
        parsed = json.loads(raw.decode("utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError, TypeError, ValueError):
        raise _state_failure() from None
    if not isinstance(parsed, dict):
        raise _state_failure()
    return parsed


def _worker_state_json(state: dict[str, Any]) -> bytes:
    return json.dumps(state, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")


def _fsync_worker_state_directory(root: Path) -> None:
    directory_fd = os.open(root, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def _write_new_worker_state(path: Path, durable: dict[str, Any]) -> None:
    root = path.parent
    payload = _worker_state_json(durable)
    fd: int | None = None
    created = False
    try:
        fd = os.open(
            path,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0),
            0o600,
        )
        created = True
        os.fchmod(fd, 0o600)
        offset = 0
        while offset < len(payload):
            written = os.write(fd, payload[offset:])
            if written <= 0:
                raise OSError
            offset += written
        os.fsync(fd)
        os.close(fd)
        fd = None
        _fsync_worker_state_directory(root)
    except (OSError, ValueError, TypeError):
        if fd is not None:
            try:
                os.close(fd)
            except OSError:
                pass
        if created:
            try:
                os.unlink(path)
            except OSError:
                pass
        raise _state_failure() from None


def _normalise_worker_state(state: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(state, dict):
        raise _state_failure()
    if set(state) - _WORKER_STATE_FIELDS:
        raise _state_failure()
    for field in ("requested_worker_id", "worker_instance_id"):
        if not isinstance(state.get(field), str):
            raise _state_failure()
    for field in ("canonical_worker_id", "identity_id", "identity_digest", "retirement_receipt_digest", "principal_id", "workspace_id", "dispatcher_id", "worker_instance_credential"):
        if field in state and not isinstance(state.get(field), str):
            raise _state_failure()
    requested = _normalise_worker_public_id(state["requested_worker_id"], "requested_worker_id", lower=True)
    instance = _normalise_worker_public_id(state["worker_instance_id"], "worker_instance_id")
    canonical_raw = str(state.get("canonical_worker_id", "")).strip()
    canonical = _normalise_worker_public_id(canonical_raw, "canonical_worker_id", lower=True) if canonical_raw else ""
    principal = str(state.get("principal_id", "")).strip()
    workspace = str(state.get("workspace_id", "")).strip()
    dispatcher = str(state.get("dispatcher_id", "")).strip()
    credential = str(state.get("worker_instance_credential", "")).strip()
    if credential and (
        len(credential.encode("utf-8")) > WORKER_INSTANCE_CREDENTIAL_MAX_BYTES
        or not WORKER_INSTANCE_CREDENTIAL_RE.fullmatch(credential)
    ):
        raise _state_failure()
    retired = state.get("retired", False)
    if not isinstance(retired, bool):
        raise _state_failure()
    generation = state.get("worker_identity_update_generation", 0)
    if (
        isinstance(generation, bool)
        or not isinstance(generation, int)
        or generation < 0
        or generation > WORKER_IDENTITY_GENERATION_MAX
    ):
        raise _state_failure() from None
    acknowledged_generation = state.get("acknowledged_generation", 0)
    if (
        isinstance(acknowledged_generation, bool)
        or not isinstance(acknowledged_generation, int)
        or acknowledged_generation < 0
        or acknowledged_generation > WORKER_IDENTITY_GENERATION_MAX
        or acknowledged_generation > generation
    ):
        raise _state_failure()
    if (
        not requested
        or not instance
        or (principal and not workspace)
        or (workspace and not principal)
        or (dispatcher and len(dispatcher.encode("utf-8")) > 160)
        or (generation == 0 and canonical not in {"", requested})
        or (generation > 0 and not canonical)
    ):
        raise _state_failure()
    durable = {
        "requested_worker_id": requested,
        "worker_instance_id": instance,
        "canonical_worker_id": canonical,
        "worker_identity_update_generation": generation,
        "acknowledged_generation": acknowledged_generation,
    }
    for field in ("identity_id", "identity_digest", "retirement_receipt_digest"):
        value = str(state.get(field, "")).strip()
        if value:
            durable[field] = value
    if principal:
        durable["principal_id"] = principal
        durable["workspace_id"] = workspace
    if dispatcher:
        durable["dispatcher_id"] = dispatcher
    if credential:
        durable["worker_instance_credential"] = credential
    if retired:
        durable["retired"] = True
    pending = state.get("pending_identity_update")
    if pending is not None:
        if not isinstance(pending, dict):
            raise _state_failure()
        _validate_worker_identity_update(pending)
        if (
            str(pending.get("requested_worker_id") or "").strip().lower() != requested
            or str(pending.get("worker_instance_id") or "").strip() != instance
            or str(pending.get("canonical_worker_id") or "").strip().lower() != canonical
            or isinstance(pending.get("worker_identity_update_generation"), bool)
            or not isinstance(pending.get("worker_identity_update_generation"), int)
            or pending.get("worker_identity_update_generation") < 0
            or pending.get("worker_identity_update_generation") > WORKER_IDENTITY_GENERATION_MAX
            or pending.get("worker_identity_update_generation") != generation
        ):
            raise _state_failure()
        if principal and (
            str(pending.get("principal_id") or "").strip() != principal
            or str(pending.get("workspace_id") or "").strip() != workspace
        ):
            raise _state_failure()
        durable["pending_identity_update"] = dict(pending)
    return durable


def _state_path_for_worker_state(root: Path, instance: str) -> Path:
    path = _WORKER_STATE_PATHS.get(instance)
    if path is not None and path.parent == root:
        return path
    return _worker_state_path(root)


def _save_worker_state(state: dict[str, Any], path: Path | None = None) -> dict[str, Any]:
    durable = _normalise_worker_state(state)
    root = _prepare_worker_state_root()
    selected_path = path or _state_path_for_worker_state(root, durable["worker_instance_id"])
    if selected_path.parent != root:
        raise _state_failure()
    if _acquire_worker_state_lock(selected_path) is None:
        raise _state_failure()
    _WORKER_STATE_PATHS[durable["worker_instance_id"]] = selected_path
    payload = _worker_state_json(durable)
    temporary_path: str | None = None
    fd: int | None = None
    try:
        try:
            target_stat = os.lstat(selected_path)
            if (
                stat.S_ISLNK(target_stat.st_mode)
                or not stat.S_ISREG(target_stat.st_mode)
                or target_stat.st_uid != os.getuid()
                or target_stat.st_nlink != 1
            ):
                raise OSError
        except FileNotFoundError:
            _write_new_worker_state(selected_path, durable)
            return durable
        fd, temporary_path = tempfile.mkstemp(prefix=".worker-identity-", dir=str(root))
        os.fchmod(fd, 0o600)
        offset = 0
        while offset < len(payload):
            written = os.write(fd, payload[offset:])
            if written <= 0:
                raise OSError
            offset += written
        os.fsync(fd)
        os.close(fd)
        fd = None
        os.replace(temporary_path, selected_path)
        temporary_path = None
        _fsync_worker_state_directory(root)
    except (OSError, ValueError, TypeError):
        raise _state_failure() from None
    finally:
        if fd is not None:
            try:
                os.close(fd)
            except OSError:
                pass
        if temporary_path:
            try:
                os.unlink(temporary_path)
            except OSError:
                pass
    return durable


def _load_or_create_worker_state(
    requested_worker: str,
    dispatcher_id: str | None = None,
    worker_instance: str | None = None,
) -> dict[str, Any]:
    requested = str(requested_worker or "").strip().lower()
    if not requested:
        requested = "local-worker"
    requested = _normalise_worker_public_id(requested, "requested_worker_id", lower=True)
    dispatcher = _worker_dispatcher_id(dispatcher_id)
    explicit_instance = worker_instance.strip() if isinstance(worker_instance, str) else ""
    if worker_instance is not None and not isinstance(worker_instance, str):
        raise _state_failure()
    if explicit_instance:
        explicit_instance = _normalise_worker_public_id(explicit_instance, "worker_instance_id")
    root = _prepare_worker_state_root()
    paths = (
        [_worker_dispatcher_state_path(root, dispatcher)]
        if dispatcher
        else [_worker_state_slot_path(root, slot) for slot in range(WORKER_STATE_SLOT_LIMIT)]
    )
    matches: list[tuple[Path, dict[str, Any]]] = []
    available: list[Path] = []
    for path in paths:
        lock = _acquire_worker_state_lock(path)
        if lock is None:
            try:
                locked_raw = _read_worker_state_file(path)
            except RuntimeError:
                if path.exists():
                    raise _state_failure()
                locked_raw = None
            locked_state = _normalise_worker_state(locked_raw) if locked_raw is not None else None
            if locked_state is not None and explicit_instance and locked_state["worker_instance_id"] == explicit_instance:
                if locked_state["requested_worker_id"] != requested:
                    raise _state_failure()
                raise RuntimeError("worker dispatcher identity is already active")
            continue
        try:
            state = _read_worker_state_file(path)
        except RuntimeError:
            if path.exists():
                _release_worker_state_lock(path)
                raise
            state = None
        if state is not None:
            durable = _normalise_worker_state(state)
            if explicit_instance and durable["worker_instance_id"] == explicit_instance and durable["requested_worker_id"] != requested:
                _release_worker_state_lock(path)
                raise _state_failure()
            if durable["requested_worker_id"] != requested:
                _release_worker_state_lock(path)
                continue
            persisted_dispatcher = durable.get("dispatcher_id", "")
            if dispatcher and persisted_dispatcher != dispatcher:
                _release_worker_state_lock(path)
                raise _state_failure()
            if not dispatcher and persisted_dispatcher and not explicit_instance:
                _release_worker_state_lock(path)
                continue
            if explicit_instance and durable["worker_instance_id"] != explicit_instance:
                _release_worker_state_lock(path)
                raise _state_failure()
            if not dispatcher and not explicit_instance and durable.get("retired") is True:
                try:
                    path.unlink()
                    _fsync_worker_state_directory(root)
                except OSError:
                    _release_worker_state_lock(path)
                    raise _state_failure()
                available.append(path)
                _release_worker_state_lock(path)
                continue
            if durable.get("retired") is True:
                durable.pop("retired", None)
            if not dispatcher and not explicit_instance:
                # An unkeyed launcher has no durable owner identity. Never
                # resurrect an arbitrary prior process's slot on restart;
                # allocate a fresh slot and let Gateway establish a new
                # server-side identity binding.
                _release_worker_state_lock(path)
                continue
            matches.append((path, durable))
            continue
        available.append(path)
        _release_worker_state_lock(path)

    if len(matches) > 1:
        for path, _ in matches:
            _release_worker_state_lock(path)
        raise RuntimeError("worker identity state is ambiguous; dispatcher identity is required")
    if matches:
        path, durable = matches[0]
        _WORKER_STATE_PATHS[durable["worker_instance_id"]] = path
        return _save_worker_state(durable, path=path)
    if dispatcher and not available:
        raise RuntimeError("worker dispatcher identity is already active")
    if not available:
        raise _state_failure()
    # The scan above intentionally releases each probe lock. Two default
    # launchers can therefore observe the same empty slot; claim a candidate
    # again immediately before the exclusive create and fall through to the
    # next candidate when another process won the race.
    for path in available:
        if _acquire_worker_state_lock(path) is None:
            continue
        durable = _normalise_worker_state(
            {
                "requested_worker_id": requested,
                "worker_instance_id": explicit_instance or secrets.token_hex(16),
                "canonical_worker_id": "",
                "worker_identity_update_generation": 0,
                **({"dispatcher_id": dispatcher} if dispatcher else {}),
            }
        )
        claimed = False
        try:
            if path.exists():
                continue
            _write_new_worker_state(path, durable)
            claimed = True
        except RuntimeError:
            continue
        finally:
            if not claimed:
                _release_worker_state_lock(path)
        _WORKER_STATE_PATHS[durable["worker_instance_id"]] = path
        return durable
    raise _state_failure()


def _retire_unkeyed_worker_state(state: dict[str, Any]) -> bool:
    """Mark a clean, server-bound unkeyed state reusable without identity reuse."""
    if state.get("dispatcher_id") or state.get("pending_identity_update") is not None:
        return False
    try:
        durable = _normalise_worker_state(state)
    except RuntimeError:
        return False
    if not durable.get("principal_id") or not durable.get("workspace_id"):
        return False
    root = _prepare_worker_state_root()
    path = _state_path_for_worker_state(root, durable["worker_instance_id"])
    if _acquire_worker_state_lock(path) is None:
        return False
    try:
        current = _normalise_worker_state(_read_worker_state_file(path))
        if (
            current["worker_instance_id"] != durable["worker_instance_id"]
            or current["worker_identity_update_generation"] != durable["worker_identity_update_generation"]
            or current.get("principal_id") != durable.get("principal_id")
            or current.get("workspace_id") != durable.get("workspace_id")
            or current.get("pending_identity_update") is not None
        ):
            return False
        retired = dict(current)
        retired["retired"] = True
        _save_worker_state(retired, path=path)
        return True
    except RuntimeError:
        return False
    finally:
        _WORKER_STATE_PATHS.pop(durable["worker_instance_id"], None)
        _release_worker_state_lock(path)


def _canonical_worker_identity_json(value: dict[str, Any]) -> bytes:
    # Match encoding/json's UTF-8 output and its default HTML/U+2028/U+2029
    # escaping used by Gateway digests.
    rendered = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    rendered = (
        rendered.replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
        .replace("\u2028", "\\u2028")
        .replace("\u2029", "\\u2029")
    )
    return rendered.encode("utf-8")


WORKER_IDENTITY_UPDATE_STATES = {"pending", "delivering", "delivered", "acknowledged"}
WORKER_IDENTITY_UPDATE_REQUIRED_FIELDS = {
    "schema_id",
    "contract_version",
    "update_id",
    "identity_id",
    "principal_id",
    "workspace_id",
    "worker_instance_id",
    "old_worker_id",
    "requested_worker_id",
    "new_worker_id",
    "canonical_worker_id",
    "worker_identity_update_generation",
    "update_digest",
    "receipt_digest",
    "state",
    "delivery_attempts",
    "last_error",
    "created_at",
    "updated_at",
    "delivered_at",
    "acknowledged_at",
    "ack_receipt_digest",
    "expires_at",
    "ack_required",
    "format_contract",
}


def _validate_worker_contract(contract_id: str, payload: dict[str, Any]) -> None:
    try:
        findings = validate_agent_contract_payload(contract_id, payload)
    except Exception:
        raise RuntimeError("worker identity contract validation failed") from None
    if findings:
        raise RuntimeError("worker identity contract validation failed")


def _worker_identity_generation_value(value: Any, *, default: int | None = None) -> int:
    if value is None and default is not None:
        value = default
    if (
        isinstance(value, bool)
        or not isinstance(value, int)
        or value < 0
        or value > WORKER_IDENTITY_GENERATION_MAX
    ):
        raise RuntimeError("worker identity generation is invalid")
    return value


def _worker_identity_update_material(update: dict[str, Any]) -> dict[str, Any]:
    return {
        "update_id": str(update.get("update_id") or ""),
        "identity_id": str(update.get("identity_id") or ""),
        "principal_id": str(update.get("principal_id") or ""),
        "workspace_id": str(update.get("workspace_id") or ""),
        "worker_instance_id": str(update.get("worker_instance_id") or ""),
        "old_worker_id": str(update.get("old_worker_id") or ""),
        "requested_worker_id": str(update.get("requested_worker_id") or ""),
        "new_worker_id": str(update.get("new_worker_id") or ""),
        "canonical_worker_id": str(update.get("canonical_worker_id") or ""),
        "worker_identity_update_generation": _worker_identity_generation_value(
            update.get("worker_identity_update_generation"), default=0
        ),
    }


def _worker_identity_update_digest(update: dict[str, Any]) -> str:
    return "sha256:" + hashlib.sha256(_canonical_worker_identity_json(_worker_identity_update_material(update))).hexdigest()


def _worker_identity_receipt_digest(update: dict[str, Any]) -> str:
    material = _worker_identity_update_material(update)
    material["update_digest"] = str(update.get("update_digest") or "")
    return "sha256:" + hashlib.sha256(_canonical_worker_identity_json(material)).hexdigest()


def _worker_identity_identity_material(identity: dict[str, Any]) -> dict[str, Any]:
    return {
        "identity_id": str(identity.get("identity_id") or ""),
        "principal_id": str(identity.get("principal_id") or ""),
        "workspace_id": str(identity.get("workspace_id") or ""),
        "requested_worker_id": str(identity.get("requested_worker_id") or ""),
        "canonical_worker_id": str(identity.get("canonical_worker_id") or ""),
        "worker_instance_id": str(identity.get("worker_instance_id") or ""),
        "worker_identity_update_generation": _worker_identity_generation_value(
            identity.get("worker_identity_update_generation"), default=0
        ),
    }


def _worker_identity_identity_digest(identity: dict[str, Any]) -> str:
    return "sha256:" + hashlib.sha256(_canonical_worker_identity_json(_worker_identity_identity_material(identity))).hexdigest()


def _validate_worker_identity_update(update: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(update, dict):
        raise RuntimeError("worker identity update response is invalid")
    if set(update) != WORKER_IDENTITY_UPDATE_REQUIRED_FIELDS:
        raise RuntimeError("worker identity update response is invalid")
    _validate_worker_contract("agent_worker_identity_update.v1", update)
    text_fields = (
        "schema_id",
        "update_id",
        "identity_id",
        "principal_id",
        "workspace_id",
        "worker_instance_id",
        "old_worker_id",
        "requested_worker_id",
        "new_worker_id",
        "canonical_worker_id",
        "update_digest",
        "receipt_digest",
        "state",
        "last_error",
        "created_at",
        "updated_at",
        "delivered_at",
        "acknowledged_at",
        "ack_receipt_digest",
        "expires_at",
    )
    if any(not isinstance(update.get(field), str) for field in text_fields):
        raise RuntimeError("worker identity update response is invalid")
    if update.get("schema_id") != "agent_worker_identity_update.v1" or update.get("contract_version") != 1:
        raise RuntimeError("worker identity update response is invalid")
    if any(not str(update.get(field) or "").strip() for field in text_fields[:12]):
        raise RuntimeError("worker identity update response is invalid")
    if update["old_worker_id"] != update["requested_worker_id"] or update["new_worker_id"] != update["canonical_worker_id"]:
        raise RuntimeError("worker identity update response is invalid")
    try:
        requested_worker_id = _normalise_worker_public_id(update["requested_worker_id"], "requested_worker_id", lower=True)
        canonical_worker_id = _normalise_worker_public_id(update["canonical_worker_id"], "canonical_worker_id", lower=True)
        worker_instance_id = _normalise_worker_public_id(update["worker_instance_id"], "worker_instance_id")
    except RuntimeError:
        raise RuntimeError("worker identity update response is invalid") from None
    if update["old_worker_id"].lower() != update["requested_worker_id"] or update["new_worker_id"].lower() != update["canonical_worker_id"]:
        raise RuntimeError("worker identity update response is invalid")
    if requested_worker_id != update["requested_worker_id"] or canonical_worker_id != update["canonical_worker_id"] or worker_instance_id != update["worker_instance_id"]:
        raise RuntimeError("worker identity update response is invalid")
    if update["old_worker_id"].lower() == update["new_worker_id"].lower():
        raise RuntimeError("worker identity update response is invalid")
    generation = update.get("worker_identity_update_generation")
    attempts = update.get("delivery_attempts")
    try:
        generation = _worker_identity_generation_value(generation)
    except RuntimeError:
        generation = None
    if generation is None or generation <= 0:
        raise RuntimeError("worker identity update response is invalid")
    if isinstance(attempts, bool) or not isinstance(attempts, int) or attempts < 0:
        raise RuntimeError("worker identity update response is invalid")
    if update["state"] not in WORKER_IDENTITY_UPDATE_STATES or not isinstance(update.get("ack_required"), bool):
        raise RuntimeError("worker identity update response is invalid")
    if update["ack_required"] != (update["state"] != "acknowledged"):
        raise RuntimeError("worker identity update response is invalid")
    if update["update_digest"] != _worker_identity_update_digest(update):
        raise RuntimeError("worker identity update response is invalid")
    if update["receipt_digest"] != _worker_identity_receipt_digest(update):
        raise RuntimeError("worker identity update response is invalid")
    if update["ack_receipt_digest"] != _worker_identity_ack_receipt_digest(update):
        raise RuntimeError("worker identity update response is invalid")
    return dict(update)


def _validate_worker_identity_readback(identity: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(identity, dict):
        raise RuntimeError("worker identity registration response is invalid")
    _validate_worker_contract("agent_worker_identity_readback.v1", identity)
    required = (
        "schema_id",
        "identity_id",
        "principal_id",
        "workspace_id",
        "requested_worker_id",
        "canonical_worker_id",
        "worker_instance_id",
        "requested_id_digest",
        "identity_digest",
        "status",
        "created_at",
        "updated_at",
        "closed_at",
    )
    if any(not isinstance(identity.get(field), str) for field in required):
        raise RuntimeError("worker identity registration response is invalid")
    nonempty = tuple(field for field in required if field != "closed_at")
    if any(not str(identity.get(field) or "").strip() for field in nonempty):
        raise RuntimeError("worker identity registration response is invalid")
    if identity.get("schema_id") != "agent_worker_identity_readback.v1" or identity.get("contract_version") != 1:
        raise RuntimeError("worker identity registration response is invalid")
    try:
        generation = _worker_identity_generation_value(identity.get("worker_identity_update_generation"))
        acknowledged = _worker_identity_generation_value(identity.get("acknowledged_generation"))
    except RuntimeError:
        generation = None
        acknowledged = None
    if generation is None or acknowledged is None or acknowledged > generation:
        raise RuntimeError("worker identity registration response is invalid")
    if identity["status"] != "active" or identity["canonical_worker_id"].lower() == "":
        raise RuntimeError("worker identity registration response is invalid")
    try:
        requested_worker_id = _normalise_worker_public_id(identity["requested_worker_id"], "requested_worker_id", lower=True)
        canonical_worker_id = _normalise_worker_public_id(identity["canonical_worker_id"], "canonical_worker_id", lower=True)
        _normalise_worker_public_id(identity["worker_instance_id"], "worker_instance_id")
    except RuntimeError:
        raise RuntimeError("worker identity registration response is invalid") from None
    if requested_worker_id != identity["requested_worker_id"] or canonical_worker_id != identity["canonical_worker_id"]:
        raise RuntimeError("worker identity registration response is invalid")
    if generation == 0 and identity["canonical_worker_id"].lower() != identity["requested_worker_id"].lower():
        raise RuntimeError("worker identity registration response is invalid")
    if identity["requested_id_digest"] != "sha256:" + hashlib.sha256(
        _canonical_worker_identity_json({"requested_worker_id": identity["requested_worker_id"]})
    ).hexdigest():
        raise RuntimeError("worker identity registration response is invalid")
    if identity["identity_digest"] != _worker_identity_identity_digest(identity):
        raise RuntimeError("worker identity registration response is invalid")
    return dict(identity)


def _validate_worker_identity_update_matches(left: dict[str, Any], right: dict[str, Any]) -> bool:
    fields = (
        "update_id",
        "identity_id",
        "principal_id",
        "workspace_id",
        "worker_instance_id",
        "old_worker_id",
        "requested_worker_id",
        "new_worker_id",
        "canonical_worker_id",
        "worker_identity_update_generation",
        "update_digest",
        "receipt_digest",
        "ack_receipt_digest",
    )
    return all(left.get(field) == right.get(field) for field in fields)


def _worker_identity_ack_receipt_digest(update: dict[str, Any]) -> str:
    material = {
        "identity_update_receipt": str(update.get("receipt_digest") or ""),
        "update_digest": str(update.get("update_digest") or ""),
        "update_id": str(update.get("update_id") or ""),
        "identity_id": str(update.get("identity_id") or ""),
        "principal_id": str(update.get("principal_id") or ""),
        "workspace_id": str(update.get("workspace_id") or ""),
        "worker_instance_id": str(update.get("worker_instance_id") or ""),
        "old_worker_id": str(update.get("old_worker_id") or ""),
        "requested_worker_id": str(update.get("requested_worker_id") or ""),
        "canonical_worker_id": str(update.get("canonical_worker_id") or ""),
        "new_worker_id": str(update.get("new_worker_id") or ""),
        "worker_identity_update_generation": _worker_identity_generation_value(
            update.get("worker_identity_update_generation", update.get("generation")), default=0
        ),
        "acknowledged": True,
    }
    return "sha256:" + hashlib.sha256(_canonical_worker_identity_json(material)).hexdigest()


def _worker_identity_retirement_digest(state: dict[str, Any]) -> str:
    material = {
        "identity_id": str(state.get("identity_id") or ""),
        "principal_id": str(state.get("principal_id") or ""),
        "workspace_id": str(state.get("workspace_id") or ""),
        "requested_worker_id": str(state.get("requested_worker_id") or ""),
        "canonical_worker_id": str(state.get("canonical_worker_id") or ""),
        "worker_instance_id": str(state.get("worker_instance_id") or ""),
        "worker_identity_update_generation": _worker_identity_generation_value(
            state.get("worker_identity_update_generation"), default=0
        ),
        "acknowledged_generation": _worker_identity_generation_value(
            state.get("acknowledged_generation"), default=0
        ),
        "identity_digest": str(state.get("identity_digest") or ""),
        "retired": True,
    }
    return "sha256:" + hashlib.sha256(_canonical_worker_identity_json(material)).hexdigest()


def _worker_identity_retirement_receipt_digest(receipt: dict[str, Any]) -> str:
    material = {
        "retirement_id": str(receipt.get("retirement_id") or ""),
        "identity_id": str(receipt.get("identity_id") or ""),
        "principal_id": str(receipt.get("principal_id") or ""),
        "workspace_id": str(receipt.get("workspace_id") or ""),
        "requested_worker_id": str(receipt.get("requested_worker_id") or ""),
        "canonical_worker_id": str(receipt.get("canonical_worker_id") or ""),
        "tombstone_canonical_worker_id": str(receipt.get("tombstone_canonical_worker_id") or ""),
        "worker_instance_id": str(receipt.get("worker_instance_id") or ""),
        "worker_identity_update_generation": _worker_identity_generation_value(
            receipt.get("worker_identity_update_generation")
        ),
        "acknowledged_generation": _worker_identity_generation_value(receipt.get("acknowledged_generation")),
        "identity_digest": str(receipt.get("identity_digest") or ""),
        "closed_identity_digest": str(receipt.get("closed_identity_digest") or ""),
        "closed_status": str(receipt.get("closed_status") or ""),
        "retirement_digest": str(receipt.get("retirement_digest") or ""),
        "closed_at": str(receipt.get("closed_at") or ""),
        "retired": True,
        "canonical_reclaimed": True,
    }
    return "sha256:" + hashlib.sha256(_canonical_worker_identity_json(material)).hexdigest()


def _validate_worker_identity_retirement_receipt(receipt: dict[str, Any], state: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(receipt, dict):
        raise RuntimeError("worker identity retirement response is invalid")
    _validate_worker_contract("agent_worker_identity_retirement_receipt.v1", receipt)
    try:
        _normalise_worker_public_id(receipt.get("requested_worker_id"), "requested_worker_id", lower=True)
        _normalise_worker_public_id(receipt.get("canonical_worker_id"), "canonical_worker_id", lower=True)
        _normalise_worker_public_id(receipt.get("tombstone_canonical_worker_id"), "tombstone_canonical_worker_id", lower=True)
        _normalise_worker_public_id(receipt.get("worker_instance_id"), "worker_instance_id")
    except RuntimeError:
        raise RuntimeError("worker identity retirement response is invalid") from None
    if (
        receipt.get("schema_id") != "agent_worker_identity_retirement_receipt.v1"
        or receipt.get("contract_version") != 1
        or receipt.get("identity_id") != state.get("identity_id")
        or receipt.get("principal_id") != state.get("principal_id")
        or receipt.get("workspace_id") != state.get("workspace_id")
        or receipt.get("requested_worker_id") != state.get("requested_worker_id")
        or receipt.get("canonical_worker_id") != state.get("canonical_worker_id")
        or receipt.get("worker_instance_id") != state.get("worker_instance_id")
        or receipt.get("worker_identity_update_generation") != state.get("worker_identity_update_generation")
        or receipt.get("acknowledged_generation") != state.get("acknowledged_generation")
        or receipt.get("identity_digest") != state.get("identity_digest")
        or not isinstance(receipt.get("closed_identity_digest"), str)
        or not receipt.get("closed_identity_digest")
        or receipt.get("closed_status") != "closed"
        or receipt.get("retirement_digest") != _worker_identity_retirement_digest(state)
        or receipt.get("retirement_receipt_digest") != _worker_identity_retirement_receipt_digest(receipt)
        or receipt.get("retired") is not True
        or receipt.get("canonical_reclaimed") is not True
        or not isinstance(receipt.get("idempotent_replay"), bool)
        or not isinstance(receipt.get("tombstone_canonical_worker_id"), str)
        or not receipt.get("tombstone_canonical_worker_id")
        or receipt.get("tombstone_canonical_worker_id") == state.get("canonical_worker_id")
    ):
        raise RuntimeError("worker identity retirement response is invalid")
    return dict(receipt)


def _read_worker_identity_retirement(orchestrator_url: str, state: dict[str, Any]) -> dict[str, Any]:
    params = {
        "identity_id": str(state.get("identity_id") or "").strip(),
        "principal_id": str(state.get("principal_id") or "").strip(),
        "workspace_id": str(state.get("workspace_id") or "").strip(),
        "requested_worker_id": str(state.get("requested_worker_id") or "").strip(),
        "canonical_worker_id": str(state.get("canonical_worker_id") or "").strip(),
        "worker_instance_id": str(state.get("worker_instance_id") or "").strip(),
        "worker_identity_update_generation": str(_worker_identity_generation_value(state.get("worker_identity_update_generation"))),
        "acknowledged_generation": str(_worker_identity_generation_value(state.get("acknowledged_generation"))),
        "identity_digest": str(state.get("identity_digest") or "").strip(),
        "retirement_digest": _worker_identity_retirement_digest(state),
    }
    receipt_digest = str(state.get("retirement_receipt_digest") or "").strip()
    if receipt_digest:
        params["retirement_receipt_digest"] = receipt_digest
    with _worker_auth_scope(state):
        response = _get(orchestrator_url, "/agents/workers/identity/retire", params=params)
    return _validate_worker_identity_retirement_receipt(response, state)


def _retire_worker_identity(orchestrator_url: str, state: dict[str, Any]) -> dict[str, Any]:
    required = ("identity_id", "identity_digest", "principal_id", "workspace_id", "requested_worker_id", "canonical_worker_id", "worker_instance_id")
    if any(not isinstance(state.get(field), str) or not state.get(field).strip() for field in required):
        raise RuntimeError("worker identity retirement requires a server-bound durable identity")
    generation = _worker_identity_generation_value(state.get("worker_identity_update_generation"))
    acknowledged_generation = _worker_identity_generation_value(state.get("acknowledged_generation"))
    if generation != acknowledged_generation:
        raise RuntimeError("worker identity retirement requires an acknowledged identity generation")
    retirement_digest = _worker_identity_retirement_digest(state)
    payload = {
        "schema_id": "agent_worker_identity_retire.v1",
        "contract_version": 1,
        "identity_id": state["identity_id"],
        "principal_id": state["principal_id"],
        "workspace_id": state["workspace_id"],
        "requested_worker_id": state["requested_worker_id"],
        "canonical_worker_id": state["canonical_worker_id"],
        "worker_instance_id": state["worker_instance_id"],
        "worker_identity_update_generation": generation,
        "acknowledged_generation": acknowledged_generation,
        "identity_digest": state["identity_digest"],
        "retirement_digest": retirement_digest,
        "retired": True,
    }
    payload = attach_format_contract("agent_worker_identity_retire.v1", payload)
    # Readback first makes a restarted worker converge on a retirement that
    # already committed. If the POST fails after server commit (for example,
    # the process lost its response before saving locally), the second readback
    # is the only recovery authority; it never re-registers or rebinds a closed
    # identity.
    try:
        return _read_worker_identity_retirement(orchestrator_url, state)
    except Exception:
        pass
    try:
        with _worker_auth_scope(state):
            response = _post(orchestrator_url, "/agents/workers/identity/retire", payload)
        try:
            return _validate_worker_identity_retirement_receipt(response, state)
        except Exception:
            return _read_worker_identity_retirement(orchestrator_url, state)
    except Exception as post_error:
        try:
            return _read_worker_identity_retirement(orchestrator_url, state)
        except Exception:
            raise post_error


def _worker_identity_update_from_response(response: dict[str, Any]) -> dict[str, Any] | None:
    update = response.get("identity_update") if isinstance(response, dict) else None
    if update is None:
        return None
    return _validate_worker_identity_update(update)


def _ack_worker_identity_update(orchestrator_url: str, update: dict[str, Any]) -> dict[str, Any]:
    validated = _validate_worker_identity_update(update)
    ack_receipt = _worker_identity_ack_receipt_digest(validated)
    # The nested update is the immutable receipt delivered by Gateway.  The
    # acknowledgement is a separate assertion; mutating state/ack_required in
    # the receipt makes the producer send a record the consumer never issued.
    payload = {
        "schema_id": "agent_worker_identity_ack.v1",
        "contract_version": 1,
        "update_id": validated["update_id"],
        "identity_id": validated["identity_id"],
        "principal_id": validated["principal_id"],
        "workspace_id": validated["workspace_id"],
        "worker_instance_id": validated["worker_instance_id"],
        "old_worker_id": validated["old_worker_id"],
        "requested_worker_id": validated["requested_worker_id"],
        "canonical_worker_id": validated["canonical_worker_id"],
        "new_worker_id": validated["new_worker_id"],
        "worker_identity_update_generation": validated["worker_identity_update_generation"],
        "update_digest": validated["update_digest"],
        "receipt_digest": validated["receipt_digest"],
        "ack_receipt_digest": ack_receipt,
        "acknowledged": True,
        "idempotent_replay": False,
        "identity_update": validated,
    }
    payload = attach_format_contract("agent_worker_identity_ack.v1", payload)
    response = _post(orchestrator_url, "/agents/workers/identity/ack", payload)
    _validate_worker_contract("agent_worker_identity_ack.v1", response)
    if response.get("acknowledged") is not True or response.get("update_id") != validated["update_id"]:
        raise RuntimeError("worker identity acknowledgement was not accepted")
    if response.get("principal_id") != validated["principal_id"] or response.get("workspace_id") != validated["workspace_id"] or response.get("worker_instance_id") != validated["worker_instance_id"]:
        raise RuntimeError("worker identity acknowledgement response is invalid")
    if response.get("worker_identity_update_generation") != validated["worker_identity_update_generation"] or response.get("ack_receipt_digest") != ack_receipt:
        raise RuntimeError("worker identity acknowledgement response is invalid")
    response_update = response.get("identity_update")
    if not isinstance(response_update, dict):
        raise RuntimeError("worker identity acknowledgement response is invalid")
    response_update = _validate_worker_identity_update(response_update)
    if not _validate_worker_identity_update_matches(validated, response_update) or response_update["state"] != "acknowledged" or response_update["ack_required"]:
        raise RuntimeError("worker identity acknowledgement response is invalid")
    return response


def _apply_worker_identity_update(
    orchestrator_url: str,
    state: dict[str, Any],
    update: dict[str, Any],
) -> dict[str, Any]:
    validated = _validate_worker_identity_update(update)
    requested = validated["requested_worker_id"].strip().lower()
    instance = validated["worker_instance_id"].strip()
    if requested != str(state.get("requested_worker_id") or "").strip().lower() or instance != str(state.get("worker_instance_id") or "").strip():
        raise RuntimeError("worker identity update does not match the persisted instance")
    if state.get("principal_id") and validated["principal_id"] != state.get("principal_id"):
        raise RuntimeError("worker identity update authority does not match the persisted instance")
    if state.get("workspace_id") and validated["workspace_id"] != state.get("workspace_id"):
        raise RuntimeError("worker identity update authority does not match the persisted instance")
    current_generation = state.get("worker_identity_update_generation", 0)
    if isinstance(current_generation, bool) or not isinstance(current_generation, int):
        raise RuntimeError("worker identity state is invalid")
    pending = state.get("pending_identity_update")
    if pending is not None:
        pending = _validate_worker_identity_update(pending)
        if not _validate_worker_identity_update_matches(pending, validated):
            raise RuntimeError("worker identity update replay does not match the persisted update")
        if validated["worker_identity_update_generation"] != current_generation:
            raise RuntimeError("worker identity update generation is not monotonic")
    elif validated["worker_identity_update_generation"] <= current_generation:
        raise RuntimeError("worker identity update generation is not monotonic")
    canonical = validated["canonical_worker_id"].strip().lower()
    generation = validated["worker_identity_update_generation"]
    next_state = dict(state)
    next_state["principal_id"] = validated["principal_id"]
    next_state["workspace_id"] = validated["workspace_id"]
    next_state["acknowledged_generation"] = generation
    next_state["canonical_worker_id"] = canonical
    next_state["worker_identity_update_generation"] = generation
    next_state["pending_identity_update"] = validated
    durable = _save_worker_state(next_state)
    state.clear()
    state.update(durable)
    with _worker_auth_scope(state):
        _ack_worker_identity_update(orchestrator_url, validated)
    completed_state = dict(state)
    completed_state.pop("pending_identity_update", None)
    completed_state["acknowledged_generation"] = generation
    durable = _save_worker_state(completed_state)
    state.clear()
    state.update(durable)
    return durable


def _rotate_worker_instance_state(state: dict[str, Any]) -> dict[str, Any]:
    """Atomically replace a migrated/closed instance with a fresh owner state."""
    old_instance = str(state.get("worker_instance_id") or "").strip()
    if not old_instance:
        raise RuntimeError("worker instance is required for credential rotation")
    root = _prepare_worker_state_root()
    path = _WORKER_STATE_PATHS.get(old_instance)
    if path is None:
        dispatcher = str(state.get("dispatcher_id") or "").strip()
        path = _worker_dispatcher_state_path(root, dispatcher) if dispatcher else _worker_state_path(root)
    next_state: dict[str, Any] = {
        "requested_worker_id": str(state.get("requested_worker_id") or "").strip().lower(),
        "worker_instance_id": secrets.token_hex(32),
        "canonical_worker_id": "",
        "worker_identity_update_generation": 0,
        "acknowledged_generation": 0,
    }
    for field in ("principal_id", "workspace_id", "dispatcher_id"):
        value = str(state.get(field) or "").strip()
        if value:
            next_state[field] = value
    durable = _save_worker_state(next_state, path=path)
    _WORKER_STATE_PATHS.pop(old_instance, None)
    state.clear()
    state.update(durable)
    return durable


def _register_worker_identity(orchestrator_url: str, state: dict[str, Any], *, _legacy_retry: bool = False) -> dict[str, Any]:
    persisted_credential = str(state.get("worker_instance_credential") or "").strip()
    configured_principal, configured_workspace = _worker_authority_config()
    persisted_principal = state.get("principal_id", "")
    persisted_workspace = state.get("workspace_id", "")
    if not isinstance(persisted_principal, str) or not isinstance(persisted_workspace, str):
        raise RuntimeError("worker identity state authority is invalid")
    persisted_principal = persisted_principal.strip()
    persisted_workspace = persisted_workspace.strip()
    if bool(persisted_principal) != bool(persisted_workspace):
        raise RuntimeError("worker identity state authority is invalid")
    if persisted_credential and not WORKER_INSTANCE_CREDENTIAL_RE.fullmatch(persisted_credential):
        raise RuntimeError("worker instance credential is malformed")
    # The client owns the bearer secret. Persist it before the first request so
    # a committed registration whose response is lost can be retried exactly.
    # All authority and shape checks above are deliberately non-mutating so a
    # rejected rebind cannot create or rotate local credential state.
    if not persisted_credential:
        prepared = dict(state)
        prepared["worker_instance_credential"] = secrets.token_hex(32)
        durable = _save_worker_state(prepared)
        state.clear()
        state.update(durable)
    registration_payload = {
        "requested_worker_id": str(state.get("requested_worker_id") or "").strip(),
        "worker_instance_id": str(state.get("worker_instance_id") or "").strip(),
    }
    if configured_principal:
        registration_payload["principal_id"] = configured_principal
        registration_payload["workspace_id"] = configured_workspace
    try:
        with _worker_auth_scope(state):
            response = _post(
                orchestrator_url,
                "/agents/workers/register",
                registration_payload,
            )
    except _WorkerIdentityCredentialMigrationRequired:
        if _legacy_retry:
            raise
        _rotate_worker_instance_state(state)
        return _register_worker_identity(orchestrator_url, state, _legacy_retry=True)
    _validate_worker_contract("agent_worker_identity_registration.v1", response)
    identity = response.get("identity") if isinstance(response.get("identity"), dict) else {}
    if not identity:
        raise RuntimeError("worker identity registration response is invalid")
    identity = _validate_worker_identity_readback(identity)
    if persisted_principal and (
        identity["principal_id"] != persisted_principal
        or identity["workspace_id"] != persisted_workspace.strip().lower()
    ):
        raise RuntimeError("worker identity registration authority does not match the persisted instance")
    if (
        identity["requested_worker_id"].strip().lower() != str(state.get("requested_worker_id") or "").strip().lower()
        or identity["worker_instance_id"].strip() != str(state.get("worker_instance_id") or "").strip()
        or response.get("principal_id") != identity["principal_id"]
        or response.get("workspace_id") != identity["workspace_id"]
        or response.get("requested_worker_id") != identity["requested_worker_id"]
        or response.get("canonical_worker_id") != identity["canonical_worker_id"]
        or response.get("worker_instance_id") != identity["worker_instance_id"]
        or response.get("worker_identity_update_generation") != identity["worker_identity_update_generation"]
        or (configured_principal and identity["principal_id"] != configured_principal)
        or (configured_workspace and identity["workspace_id"] != configured_workspace.strip().lower())
    ):
        raise RuntimeError("worker identity registration response does not match the persisted instance")
    update = _worker_identity_update_from_response(response)
    if update is not None and not bool(response.get("identity_update_required")):
        raise RuntimeError("worker identity registration response is invalid")
    pending = state.get("pending_identity_update")
    if pending is not None:
        pending = _validate_worker_identity_update(pending)
        if update is not None and not _validate_worker_identity_update_matches(pending, update):
            raise RuntimeError("worker identity registration replay does not match the persisted update")
        update = update or pending
        if (
            update["principal_id"] != identity["principal_id"]
            or update["workspace_id"] != identity["workspace_id"]
            or update["requested_worker_id"].lower() != identity["requested_worker_id"].lower()
            or update["canonical_worker_id"].lower() != identity["canonical_worker_id"].lower()
            or update["worker_instance_id"] != identity["worker_instance_id"]
            or update["worker_identity_update_generation"] != identity["worker_identity_update_generation"]
        ):
            raise RuntimeError("worker identity registration response is invalid")
    if update is not None:
        base_state = dict(state)
        base_state["identity_id"] = identity["identity_id"]
        base_state["identity_digest"] = identity["identity_digest"]
        base_state["acknowledged_generation"] = identity["acknowledged_generation"]
        base_state["principal_id"] = identity["principal_id"]
        base_state["workspace_id"] = identity["workspace_id"]
        state.clear()
        state.update(_normalise_worker_state(base_state))
        return _apply_worker_identity_update(orchestrator_url, state, update)
    canonical = identity["canonical_worker_id"].strip().lower()
    generation = identity["worker_identity_update_generation"]
    acknowledged_generation = identity["acknowledged_generation"]
    current_generation = state.get("worker_identity_update_generation", 0)
    if generation < current_generation or acknowledged_generation != generation:
        raise RuntimeError("worker identity registration response is invalid")
    next_state = dict(state)
    next_state["identity_id"] = identity["identity_id"]
    next_state["identity_digest"] = identity["identity_digest"]
    next_state["acknowledged_generation"] = acknowledged_generation
    next_state["principal_id"] = identity["principal_id"]
    next_state["workspace_id"] = identity["workspace_id"]
    next_state["canonical_worker_id"] = canonical
    next_state["worker_identity_update_generation"] = generation
    durable = _save_worker_state(next_state)
    state.clear()
    state.update(durable)
    return durable


def _post(
    orchestrator_url: str,
    path: str,
    payload: dict[str, Any],
    params: dict[str, str] | None = None,
    *,
    timeout: float = 30.0,
    auth_snapshot: WorkerAuthSnapshot | None = None,
    cancel_event: threading.Event | None = None,
    cancellation_request_id: str | None = None,
) -> dict[str, Any]:
    _LAST_RESPONSE_HEADERS.set(None)
    if cancel_event is not None and cancel_event.is_set():
        raise ExecutionBlocked(
            "lease_lost",
            "authoritative lease was lost before the gateway request started",
        )
    if cancel_event is None:
        client = ContextLatticeClient(
            base_url=orchestrator_url,
            timeout=timeout,
            role="worker",
            extra_headers=_worker_auth_headers(path, auth_snapshot),
        )
        try:
            try:
                response = client.post_json(
                    path,
                    payload,
                    params=params,
                    timeout=max(1.0, float(timeout)),
                )
            except Exception as exc:
                if (
                    _worker_auth_path_requires_proof(path)
                    and str(path).split("?", 1)[0].rstrip("/") in {
                        "/agents/workers/register",
                        "/agents/tasks/worker/register",
                        "/agents/tasks/workers/register",
                    }
                    and client.last_response_status == 409
                    and client.last_error_code
                    == "worker_identity_credential_migration_required"
                ):
                    raise _WorkerIdentityCredentialMigrationRequired() from exc
                raise
            _LAST_RESPONSE_HEADERS.set(dict(client.last_response_headers))
            return response
        finally:
            client.close()

    absolute_url = (
        path
        if str(path).startswith(("http://", "https://"))
        else urljoin(str(orchestrator_url).rstrip("/") + "/", str(path).lstrip("/"))
    )
    parsed = urlsplit(absolute_url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise ValueError("gateway request URL must use http or https")
    query = parsed.query
    if params:
        encoded_params = urlencode(params)
        query = f"{query}&{encoded_params}" if query else encoded_params
    request_target = parsed.path or "/"
    if query:
        request_target = f"{request_target}?{query}"
    request_body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    request_headers = {
        **_orchestrator_headers(),
        **_worker_auth_headers(path, auth_snapshot),
        "content-type": "application/json",
        "content-length": str(len(request_body)),
    }
    connection_type = (
        http.client.HTTPSConnection
        if parsed.scheme == "https"
        else http.client.HTTPConnection
    )
    connection = connection_type(
        parsed.hostname,
        parsed.port,
        timeout=max(1.0, float(timeout)),
    )
    request_done = threading.Event()
    cancellation_observed = threading.Event()

    def notify_gateway_cancellation() -> None:
        request_id = str(cancellation_request_id or "").strip()
        if not request_id:
            return
        cancel_client = ContextLatticeClient(
            base_url=orchestrator_url,
            timeout=2.0,
            role="worker",
            extra_headers=_worker_auth_headers(
                "/v1/inference/cancel", auth_snapshot
            ),
        )
        try:
            cancel_client.post_json(
                "/v1/inference/cancel",
                {"request_id": request_id},
                timeout=2.0,
            )
        except Exception:
            pass
        finally:
            cancel_client.close()

    def abort_connection() -> None:
        active_socket = connection.sock
        if active_socket is not None:
            try:
                active_socket.setsockopt(
                    socket.SOL_SOCKET,
                    socket.SO_LINGER,
                    struct.pack("ii", 1, 0),
                )
            except OSError:
                pass
            try:
                active_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        connection.close()

    def watch_cancellation() -> None:
        while not request_done.wait(0.01):
            if cancel_event.is_set():
                cancellation_observed.set()
                abort_connection()
                notify_gateway_cancellation()

    watcher = threading.Thread(
        target=watch_cancellation,
        name="task-gateway-request-cancel",
        daemon=True,
    )
    watcher.start()
    try:
        connection.request(
            "POST",
            request_target,
            body=request_body,
            headers=request_headers,
        )
        response = connection.getresponse()
        response_body = response.read()
        _LAST_RESPONSE_HEADERS.set(dict(response.getheaders()))
        if cancel_event.is_set():
            raise ExecutionBlocked(
                "lease_lost",
                "authoritative lease was lost during the gateway request",
                execution_observed=True,
            )
        if response.status >= 400:
            safe_body = response_body.decode("utf-8", errors="replace")[:500]
            raise RuntimeError(
                f"ContextLattice request failed status={response.status}: {safe_body}"
            )
        decoded = json.loads(response_body.decode("utf-8") or "{}")
        return decoded if isinstance(decoded, dict) else {"data": decoded}
    except ExecutionBlocked:
        raise
    except Exception as exc:
        if cancel_event.is_set() or cancellation_observed.is_set():
            raise ExecutionBlocked(
                "lease_lost",
                "authoritative lease loss canceled the in-flight gateway request",
                execution_observed=True,
            ) from exc
        raise
    finally:
        request_done.set()
        abort_connection()
        watcher.join(timeout=0.5)


def _post_public(
    orchestrator_url: str,
    path: str,
    payload: dict[str, Any],
    *,
    timeout: float = 30.0,
) -> dict[str, Any]:
    sanitized = _redact_runner_value(payload)
    response = _post(
        orchestrator_url,
        path,
        dict(sanitized) if isinstance(sanitized, dict) else {},
        timeout=timeout,
    )
    safe_response = _redact_runner_value(response)
    return dict(safe_response) if isinstance(safe_response, dict) else {}


def _post_status(
    orchestrator_url: str,
    task_id: Any,
    payload: dict[str, Any],
    *,
    timeout: float = 30.0,
) -> dict[str, Any]:
    return _post_public(
        orchestrator_url,
        f"/agents/tasks/{str(task_id or '').strip()}/status",
        payload,
        timeout=timeout,
    )


def _claim_next_task(
    orchestrator_url: str,
    worker: str,
    worker_instance_id: str | None = None,
    canonical_worker_id: str | None = None,
    state: dict[str, Any] | None = None,
) -> dict[str, Any]:
    worker_identity = str(worker or "").strip() or "local-worker"
    if state is None and not canonical_worker_id:
        payload: dict[str, Any] = {"worker": worker_identity}
        if str(worker_instance_id or "").strip():
            payload["worker_id"] = worker_identity
            payload["worker_instance_id"] = str(worker_instance_id).strip()
        return _post(
            orchestrator_url,
            "/agents/tasks/next",
            payload,
            params={"worker": worker_identity},
        )
    instance = str(worker_instance_id or "").strip()
    if state is not None:
        worker_identity = str(state.get("requested_worker_id") or worker_identity).strip()
        instance = str(state.get("worker_instance_id") or "").strip()
        canonical_worker_id = str(state.get("canonical_worker_id") or "").strip()
    if not instance:
        raise RuntimeError("worker instance is required")
    if state is not None and state.get("pending_identity_update") is not None:
        pending = _validate_worker_identity_update(state["pending_identity_update"])
        _apply_worker_identity_update(orchestrator_url, state, pending)
        return {"task": None, "identity_update_acknowledged": True}
    canonical = str(canonical_worker_id or "").strip()
    if not canonical:
        raise RuntimeError("server-issued canonical worker ID is required")
    principal = str(state.get("principal_id") or "").strip() if state is not None else ""
    workspace = str(state.get("workspace_id") or "").strip() if state is not None else ""
    if not principal or not workspace:
        raise RuntimeError("registered worker authority is required")
    with _worker_auth_scope(state):
        response = _post(
            orchestrator_url,
            "/agents/tasks/next",
            {
                "requested_worker_id": worker_identity,
                "worker": canonical,
                "worker_instance_id": instance,
                "principal_id": principal,
                "workspace_id": workspace,
            },
            params={"worker": canonical},
        )
    update = _worker_identity_update_from_response(response)
    if update is not None and bool(response.get("identity_update_required")):
        if state is None:
            raise RuntimeError("worker identity update requires persisted state")
        _apply_worker_identity_update(orchestrator_url, state, update)
        return {"task": None, "identity_update_acknowledged": True}
    if response.get("task") is not None and state is not None:
        if response.get("identity_update_required") is True:
            raise RuntimeError("worker identity update was not acknowledged before task delivery")
    return response


def _get(
    orchestrator_url: str,
    path: str,
    params: dict[str, str] | None = None,
    *,
    timeout: float = 30.0,
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> dict[str, Any]:
    _LAST_RESPONSE_HEADERS.set(None)
    client = ContextLatticeClient(
        base_url=orchestrator_url,
        timeout=timeout,
        role="worker",
        extra_headers=_worker_auth_headers(path, auth_snapshot),
    )
    try:
        response = client.get_json(
            path,
            params=params,
            timeout=max(1.0, float(timeout)),
        )
        _LAST_RESPONSE_HEADERS.set(dict(client.last_response_headers))
        return response
    finally:
        client.close()


def _write_memory(
    orchestrator_url: str,
    project: str,
    file_name: str,
    content: str,
    topic_path: str | None = None,
) -> None:
    payload: dict[str, Any] = {
        "projectName": _redact_runner_text(project),
        "fileName": _redact_runner_text(file_name),
        "content": _redact_runner_text(content),
    }
    if topic_path:
        payload["topicPath"] = _redact_runner_text(topic_path)
    _post_public(orchestrator_url, "/memory/write", payload)


def _post_feedback(orchestrator_url: str, payload: dict[str, Any]) -> None:
    try:
        _post_public(orchestrator_url, "/feedback", payload)
    except Exception:
        return


def _write_checkpoint(
    context_runtime: Any,
    *,
    task: dict[str, Any],
    bundle: dict[str, Any],
    output: Any,
    provider: Any,
    model: Any,
    status: Any,
) -> Any:
    safe_task = _redact_runner_value(task)
    safe_bundle = _redact_runner_value(bundle)
    response = context_runtime.write_checkpoint(
        task=dict(safe_task) if isinstance(safe_task, dict) else {},
        bundle=dict(safe_bundle) if isinstance(safe_bundle, dict) else {},
        output=_redact_runner_text(output),
        provider=_redact_runner_text(provider),
        model=_redact_runner_text(model),
        status=_redact_runner_text(status),
    )
    return _redact_runner_value(response)


def _format_result(task: dict[str, Any], output: str) -> str:
    sanitized_task = _redact_runner_value(task)
    safe_task = dict(sanitized_task) if isinstance(sanitized_task, dict) else {}
    safe_output = _redact_runner_text(output)
    payload = safe_task.get("payload")
    payload_block = json.dumps(payload, indent=2) if payload else "{}"
    contract_payload = attach_format_contract(
        "agent_task_result.v1",
        {
            "ok": True,
            "task_id": str(safe_task.get("id") or ""),
            "project": str(safe_task.get("project") or "_global"),
            "agent": str(safe_task.get("agent") or DEFAULT_AGENT),
            "status": "succeeded",
            "output": safe_output[:119000],
        },
    )
    contract_block = json.dumps(contract_payload, indent=2, sort_keys=True)
    return f"""# Task Result\n\n```json contextlattice_contract\n{contract_block}\n```\n\n## Task\n- id: {safe_task.get('id')}\n- title: {safe_task.get('title')}\n- project: {safe_task.get('project')}\n- agent: {safe_task.get('agent')}\n\n## Payload\n```\n{payload_block}\n```\n\n## Output\n{safe_output}\n"""


def _serialize_env_json(payload: dict[str, Any], max_chars: int = 65000) -> str:
    rendered = json.dumps(_redact_runner_value(payload), separators=(",", ":"), ensure_ascii=True)
    if len(rendered) <= max_chars:
        return rendered
    return rendered[: max_chars - 1] + "}"


def _strict_claimed_task(
    orchestrator_url: str,
    task: dict[str, Any],
    claim: dict[str, Any],
    agent: str,
    provider: str,
    model: str,
    base_url_override: str | None,
    worker: str,
    worker_instance: str,
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> dict[str, Any]:
    """Run a Gateway-claimed task through the fenced U3 execution surface.

    The legacy compatibility path below intentionally has no claim argument;
    the production polling loop always supplies the complete Gateway claim.
    This keeps old compatibility tests readable while ensuring a claimed task
    can never silently fall back to Python inference, a shell string, or a
    second task store.
    """

    def gateway_inference(
        prepared: Any,
        cancel_event: threading.Event,
    ) -> tuple[str, dict[str, Any]]:
        try:
            safe_payload = json.loads(str(prepared.env.get("TASK_PAYLOAD") or "{}"))
        except (TypeError, json.JSONDecodeError):
            safe_payload = {}
        safe_task = {
            "id": str(prepared.fence.task_id),
            "title": _redact_runner_text(str(prepared.task.get("title") or "Task")),
            "payload": safe_payload if isinstance(safe_payload, dict) else {},
        }
        runtime_policy = (
            prepared.profile.get("_runtime_policy")
            if isinstance(prepared.profile, dict)
            else None
        )
        runtime_secs = (
            runtime_policy.get("effective_runtime_secs")
            if isinstance(runtime_policy, dict)
            else None
        )
        if (
            isinstance(runtime_secs, bool)
            or not isinstance(runtime_secs, int)
            or runtime_secs <= 0
        ):
            raise ExecutionBlocked(
                "runtime_limit_invalid",
                "prepared gateway inference has no exact positive runtime policy",
            )
        return _run_llm_task_via_gateway(
            orchestrator_url,
            provider,
            model,
            safe_task,
            prepared.prompt,
            base_url_override=base_url_override,
            api_key=None,
            timeout_secs=runtime_secs,
            cancel_event=cancel_event,
        )

    try:
        return execute_claimed_task(
            claim,
            worker=worker,
            worker_instance=worker_instance,
            orchestrator_url=orchestrator_url,
            get_json=_get,
            post_json=_post,
            source_repo=_repo_root(),
            auth_snapshot=auth_snapshot,
            gateway_inference=gateway_inference,
        )
    except PublicationNotAcknowledged as exc:
        # A generic or foreign acknowledgement is not success. Retain the
        # owned worktree for exact receipt reconciliation.
        fence_data: dict[str, Any] = {}
        try:
            fence_data = extract_lease_fence(claim, worker, worker_instance).as_dict()
        except ExecutionBlocked:
            pass
        return {
            "status": "quarantined",
            "execution_observed": True,
            "reason": "publication_receipt_invalid",
            "message": _redact_runner_text(str(exc))[:240],
            "fence": fence_data,
        }
    except ExecutionBlocked as exc:
        reason = _redact_runner_text(getattr(exc, "reason", "execution_blocked"))[:120]
        detail = _redact_runner_text(getattr(exc, "detail", ""))[:240]
        # A valid claim fence lets the authoritative ledger record a bounded
        # failed observation. If the Gateway itself is unavailable, preserve
        # the original failure and do not retry through another authority.
        try:
            fence = extract_lease_fence(claim, worker, worker_instance)
            evidence = getattr(exc, "evidence", {}) if isinstance(getattr(exc, "evidence", {}), dict) else {}
            sanitized_evidence = _redact_runner_value(evidence)
            safe_evidence = dict(sanitized_evidence) if isinstance(sanitized_evidence, dict) else {}
            fence_payload = fenced_payload(
                fence,
                {
                    "runner_status": "failed",
                    "exit_code": 125,
                    "metadata": {"execution_reason": reason, "detail": detail, "termination": safe_evidence},
                },
            )
            task_id = str(task.get("task_id") or task.get("id") or fence.task_id or "").strip()
            if task_id:
                _post(orchestrator_url, f"/agents/tasks/{task_id}/observe", fence_payload, timeout=30.0, auth_snapshot=auth_snapshot)
        except Exception:
            pass
        evidence = getattr(exc, "evidence", {}) if isinstance(getattr(exc, "evidence", {}), dict) else {}
        sanitized_evidence = _redact_runner_value(evidence)
        safe_evidence = dict(sanitized_evidence) if isinstance(sanitized_evidence, dict) else {}
        return {
            "status": "quarantined" if reason == "quarantined" else "execution_failed",
            "execution_observed": bool(getattr(exc, "execution_observed", False)),
            "reason": reason,
            "message": detail,
            "termination": safe_evidence,
        }


def _claim_argument_has_fence(
    claim: dict[str, Any] | None,
    task: dict[str, Any],
    worker: str = "",
    worker_instance: str = "",
) -> bool:
    if isinstance(claim, dict) and claim_has_complete_fence(claim, worker, worker_instance):
        return True
    embedded = task.get("_claim")
    return isinstance(embedded, dict) and claim_has_complete_fence(embedded, worker, worker_instance)


def _handle_task(
    orchestrator_url: str,
    task: dict[str, Any],
    agent: str,
    provider: str,
    model: str,
    base_url_override: str | None,
    api_key: Optional[str],
    claim: dict[str, Any] | None = None,
    worker: str = "",
    worker_instance: str = "",
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> None:
    strict_claim = claim if isinstance(claim, dict) else task.get("_claim")
    if isinstance(strict_claim, dict):
        if not _claim_argument_has_fence(claim, task, worker, worker_instance):
            extract_lease_fence(strict_claim, worker, worker_instance)
        return _strict_claimed_task(
            orchestrator_url,
            task,
            strict_claim,
            agent,
            provider,
            model,
            base_url_override,
            worker or str(task.get("worker_id") or os.getenv("TASK_WORKER") or "local-worker"),
            worker_instance or str(task.get("worker_instance_id") or os.getenv("TASK_WORKER_INSTANCE") or worker or "local-worker"),
            auth_snapshot,
        )
    if task.get("approval_required") and not task.get("approved"):
        _post_status(
            orchestrator_url,
            task["id"],
            {"status": "blocked", "message": "Awaiting approval"},
        )
        return

    control_plane_url = str(
        os.getenv("TASK_INFERENCE_CONTROL_PLANE_URL", DEFAULT_INFERENCE_CONTROL_PLANE_URL)
    ).strip() or orchestrator_url
    route_payload: dict[str, Any] = {}
    task_payload = task.get("payload") or {}
    agent_choice = _normalize_agent_alias(task.get("agent") or agent)
    try:
        agent_fit_selection = _authorize_agent_fit_selection(
            orchestrator_url, task, agent_choice, model
        )
    except Exception as exc:
        _post_status(
            orchestrator_url,
            task["id"],
            {
                "status": "blocked",
                "message": "Governed Agent Fit selection authorization failed",
                "metadata": {
                    "agent_fit_selection": {
                        "requested": True,
                        "authorized": False,
                        "reason": _redact_runner_text(str(exc))[:240],
                    }
                },
            },
        )
        return
    adapter_argv = None if os.getenv("TASK_AGENT_CMD") else _runner_adapter_for_agent(agent_choice)

    if not _gateway_inference_enabled() and adapter_argv is None:
        _post_status(
            orchestrator_url,
            task["id"],
            {"status": "failed", "message": "Go inference gateway is disabled; Python inference router is archived"},
        )
        return

    if _gateway_inference_enabled():
        try:
            route_response = _post(
                control_plane_url,
                "/v1/inference/route",
                {
                    "provider": provider,
                    "base_url": base_url_override,
                },
                timeout=20.0,
            )
            route_candidate = route_response.get("route")
            if isinstance(route_candidate, dict):
                sanitized_route = _redact_runner_value(route_candidate)
                route_payload = dict(sanitized_route) if isinstance(sanitized_route, dict) else {}
        except Exception as exc:
            if adapter_argv is None:
                _post_status(
                    orchestrator_url,
                    task["id"],
                    {
                        "status": "failed",
                        "message": f"Go inference route error: {_redact_runner_text(str(exc))}",
                    },
                )
                return
            fallback_route = {
                "provider": provider,
                "base_url": base_url_override or "",
                "reason": (
                    "inference route unavailable for optional adapter execution: "
                    + _redact_runner_text(str(exc))
                ),
            }
            sanitized_route = _redact_runner_value(fallback_route)
            route_payload = dict(sanitized_route) if isinstance(sanitized_route, dict) else {}

    if not route_payload and adapter_argv is None:
        _post_status(
            orchestrator_url,
            task["id"],
            {"status": "failed", "message": "Go inference route returned no route payload"},
        )
        return
    if not route_payload:
        fallback_route = {
            "provider": provider,
            "base_url": base_url_override or "",
            "reason": "inference route skipped for optional runner adapter",
        }
        sanitized_route = _redact_runner_value(fallback_route)
        route_payload = dict(sanitized_route) if isinstance(sanitized_route, dict) else {}

    topic_path = _redact_runner_text(
        task_payload.get("topic_path") or task_payload.get("topicPath") or ""
    )
    context_runtime = ContextExpansionRuntime(orchestrator_url=orchestrator_url, agent_id=agent_choice)
    context_bundle: dict[str, Any]
    context_prompt: str
    try:
        prepared_context = _redact_runner_value(context_runtime.prepare(task))
        context_bundle = dict(prepared_context) if isinstance(prepared_context, dict) else {}
        context_prompt = _redact_runner_text(context_runtime.render_for_prompt(context_bundle))
    except Exception as exc:
        context_bundle = {
            "enabled": False,
            "query": _redact_runner_text(task.get("title") or "task context"),
            "project": _redact_runner_text(task.get("project") or "_global"),
            "topic_path": topic_path,
            "warnings": [f"context expansion failed: {_redact_runner_text(str(exc))}"],
            "lifecycle": {"status": "failed_open", "result_state": "failed_open", "degraded": True},
            "layers": {"l0_facts": [], "l1_rollups": [], "l2_raw_refs": []},
            "numeric_facts": [],
            "tool_slices": {},
            "expansion": {"broadened_scope": False, "deep_escalated": False, "steps": ["failed_open"]},
        }
        context_prompt = "Context expansion unavailable; continue with fail-open execution."

    lifecycle = context_bundle.get("lifecycle") if isinstance(context_bundle.get("lifecycle"), dict) else {}
    pending_sources = lifecycle.get("pending_sources") if isinstance(lifecycle.get("pending_sources"), list) else []
    pending_label = ", ".join(_redact_runner_text(item) for item in pending_sources[:4])

    cmd = _runner_cmd_for_agent(agent_choice)
    route_provider = _redact_runner_text(route_payload.get("provider") or provider)
    route_base_url = _redact_runner_text(
        route_payload.get("base_url") or (base_url_override or "")
    )
    route_reason = _redact_runner_text(route_payload.get("reason") or "")
    route_label = _format_route_label_from_payload(route_payload)
    env = _legacy_runner_env(
        {
            "TASK_ID": _redact_runner_text(task["id"]),
            "TASK_TITLE": _redact_runner_text(task["title"]),
            "TASK_PROJECT": _redact_runner_text(task.get("project") or ""),
            "TASK_AGENT": _redact_runner_text(agent_choice),
            "TASK_PAYLOAD": _serialize_env_json(
                task.get("payload") if isinstance(task.get("payload"), dict) else {}
            ),
            "TASK_MODEL_PROVIDER": route_provider,
            "TASK_MODEL": _redact_runner_text(model),
            "TASK_BASE_URL": route_base_url,
            "CONTEXTLATTICE_ORCHESTRATOR_URL": _redact_runner_text(orchestrator_url),
            "MEMMCP_ORCHESTRATOR_URL": _redact_runner_text(orchestrator_url),
            "CONTEXTLATTICE_SESSION_ID": _redact_runner_text(
                task_payload.get("session_id") or task_payload.get("sessionId") or task.get("session_id") or os.getenv("CONTEXTLATTICE_SESSION_ID", "")
            ),
            "CONTEXTLATTICE_AGENT_ID": _redact_runner_text(task_payload.get("agent_id") or task_payload.get("agentId") or f"{agent_choice.replace('-', '_')}_agent"),
            "TASK_CONTEXT_BUNDLE": _serialize_env_json(context_bundle),
            "TASK_CONTEXT_PROMPT": context_prompt,
            "TASK_TOOL_CONTEXT_SLICES": _serialize_env_json(
                context_bundle.get("tool_slices")
                if isinstance(context_bundle.get("tool_slices"), dict)
                else {}
            ),
        }
    )

    if adapter_argv is not None:
        result = _run_adapter(
            adapter_argv,
            env,
            _repo_root(),
            _runner_timeout_secs(task_payload),
            agent_choice,
        )
        compact_result = _compact_runner_result(result)
        task_status = _task_status_for_runner_result(result)
        message = _message_for_adapter_result(agent_choice, result)
        if pending_sources:
            message += f" (pending async sources: {pending_label})"
        agent_state = _agent_state_for_runner_status(result)
        runner_quality = _record_runner_quality_sample(
            task=task,
            agent=agent_choice,
            result=result,
            context_bundle=context_bundle,
            task_status=task_status,
            message=message,
            route_payload=route_payload,
        )
        runner_status = str(result.get("status") or "").strip().lower()
        result_metadata = result.get("metadata") if isinstance(result.get("metadata"), dict) else {}
        outcome_class = "success" if task_status == "succeeded" else "task_failure"
        calibration_eligible = runner_status in {"succeeded", "failed"}
        if not calibration_eligible:
            outcome_class = "infrastructure_failure" if runner_status in {"missing_binary", "timed_out"} else "blocked"
        context_pack_outcome = _post_context_pack_outcome(
            orchestrator_url,
            task=task,
            context_bundle=context_bundle,
            status=task_status,
            source="runner_adapter",
            calibration_eligible=calibration_eligible,
            outcome_class=outcome_class,
            result_metadata=result_metadata,
        )
        _post_status(
            orchestrator_url,
            task["id"],
            {
                "status": task_status,
                "message": message,
                "metadata": {
                    "runner_result": compact_result,
                    "runner_quality": runner_quality,
                    "context_pack_outcome": context_pack_outcome,
                    "agent_fit_selection": agent_fit_selection,
                    "agent_state": agent_state,
                    "retrieval_lifecycle": lifecycle,
                },
            },
        )
        _write_checkpoint(
            context_runtime,
            task=task,
            bundle=context_bundle,
            output=json.dumps(
                {
                    "schema_id": "contextlattice_runner_checkpoint.v1",
                    "message": message,
                    "runner_result": compact_result,
                    "runner_quality": runner_quality,
                    "context_pack_outcome": context_pack_outcome,
                    "agent_fit_selection": agent_fit_selection,
                    "agent_state": agent_state,
                    "lease": compact_result.get("metadata", {}).get("lease"),
                    "retrieval_lifecycle": lifecycle,
                },
                indent=2,
                sort_keys=True,
            ),
            provider=f"{agent_choice}-adapter",
            model=model,
            status=task_status,
        )
        _post_feedback(
            orchestrator_url,
            {
                "project": task.get("project"),
                "task_id": task.get("id"),
                "source": "agent",
                "content": str(compact_result.get("summary") or message)[:1500],
                "topic_path": topic_path,
                "metadata": {
                    "agent": agent_choice,
                    "provider": route_provider,
                    "model": model,
                    "inference_route": {
                        "provider": route_provider,
                        "base_url": route_base_url,
                        "reason": route_reason,
                    },
                    "runner_result": compact_result,
                    "runner_quality": runner_quality,
                    "context_pack_outcome": context_pack_outcome,
                    "agent_fit_selection": agent_fit_selection,
                    "agent_state": agent_state,
                    "retrieval_lifecycle": lifecycle,
                    "context_expansion": context_bundle.get("expansion"),
                },
            },
        )
        return

    if cmd:
        exit_code = _run_command(cmd, env)
        status = "succeeded" if exit_code == 0 else "failed"
        message = "Task completed by runner command" if exit_code == 0 else "Runner command failed"
        if pending_sources:
            message += f" (pending async sources: {pending_label})"
        context_pack_outcome = _post_context_pack_outcome(
            orchestrator_url,
            task=task,
            context_bundle=context_bundle,
            status=status,
            source="legacy_command",
            calibration_eligible=True,
            outcome_class="success" if status == "succeeded" else "task_failure",
        )
        _post_status(
            orchestrator_url,
            task["id"],
            {"status": status, "message": message, "metadata": {"context_pack_outcome": context_pack_outcome, "agent_fit_selection": agent_fit_selection}},
        )
        _write_checkpoint(
            context_runtime,
            task=task,
            bundle=context_bundle,
            output=json.dumps({"message": message, "context_pack_outcome": context_pack_outcome, "agent_fit_selection": agent_fit_selection}, sort_keys=True),
            provider=route_label,
            model=model,
            status=status,
        )
        if exit_code == 0:
            _post_feedback(
                orchestrator_url,
                {
                    "project": task.get("project"),
                    "task_id": task.get("id"),
                    "source": "agent",
                    "content": message,
                    "topic_path": topic_path,
                    "metadata": {
                        "agent": agent_choice,
                        "provider": route_provider,
                        "model": model,
                        "inference_route": {
                            "provider": route_provider,
                            "base_url": route_base_url,
                            "reason": route_reason,
                        },
                        "retrieval_lifecycle": lifecycle,
                        "context_expansion": context_bundle.get("expansion"),
                        "context_pack_outcome": context_pack_outcome,
                        "agent_fit_selection": agent_fit_selection,
                    },
                },
            )
        return

    try:
        output: str | None = None
        active_route_payload = dict(route_payload)
        if _gateway_inference_enabled():
            try:
                output, gateway_route_payload = _run_llm_task_via_gateway(
                    control_plane_url,
                    provider,
                    model,
                    task,
                    context_prompt=context_prompt,
                    base_url_override=base_url_override,
                    # Legacy no-claim execution never receives provider
                    # credentials.  Claimed execution uses the strict
                    # gateway path above and does not reach this branch.
                    api_key=None,
                )
                if gateway_route_payload:
                    sanitized_route = _redact_runner_value(gateway_route_payload)
                    active_route_payload = dict(sanitized_route) if isinstance(sanitized_route, dict) else {}
            except Exception as exc:
                if _gateway_inference_required():
                    raise RuntimeError(
                        "go inference control plane required but unavailable: "
                        + _redact_runner_text(str(exc))
                    ) from exc
        if output is None:
            raise RuntimeError("Go inference control plane returned no output")
        run_provider = str(active_route_payload.get("provider") or route_provider)
        run_base_url = str(active_route_payload.get("base_url") or route_base_url)
        run_reason = str(active_route_payload.get("reason") or route_reason)
        run_route_label = _format_route_label_from_payload(active_route_payload)
        project = task.get("project") or "_global"
        file_name = f"task_runs/{task['id']}.md"
        _write_memory(orchestrator_url, project, file_name, _format_result(task, output), topic_path=topic_path)
        completion_message = f"Completed via {run_route_label} ({model})"
        if pending_sources:
            completion_message += f" | pending async sources: {pending_label}"
        context_pack_outcome = _post_context_pack_outcome(
            orchestrator_url,
            task=task,
            context_bundle=context_bundle,
            status="succeeded",
            source="gateway_inference",
            calibration_eligible=True,
            outcome_class="success",
            result_metadata=active_route_payload.get("metadata") if isinstance(active_route_payload.get("metadata"), dict) else {},
        )
        _post_status(
            orchestrator_url,
            task["id"],
            {"status": "succeeded", "message": completion_message, "metadata": {"context_pack_outcome": context_pack_outcome, "agent_fit_selection": agent_fit_selection}},
        )
        _write_checkpoint(
            context_runtime,
            task=task,
            bundle=context_bundle,
            output=output,
            provider=run_route_label,
            model=model,
            status="succeeded",
        )
        _post_feedback(
            orchestrator_url,
            {
                "project": project,
                "task_id": task.get("id"),
                "source": "agent",
                "content": output[:1500],
                "topic_path": topic_path,
                "metadata": {
                    "agent": agent_choice,
                    "provider": run_provider,
                    "model": model,
                    "inference_route": {
                        "provider": run_provider,
                        "base_url": run_base_url,
                        "reason": run_reason,
                    },
                    "retrieval_lifecycle": lifecycle,
                    "context_expansion": context_bundle.get("expansion"),
                    "context_pack_outcome": context_pack_outcome,
                    "agent_fit_selection": agent_fit_selection,
                },
            },
        )
    except Exception as exc:  # pragma: no cover
        context_pack_outcome = _post_context_pack_outcome(
            orchestrator_url,
            task=task,
            context_bundle=context_bundle,
            status="failed",
            source="gateway_inference",
            calibration_eligible=False,
            outcome_class="infrastructure_failure",
        )
        _write_checkpoint(
            context_runtime,
            task=task,
            bundle=context_bundle,
            output=f"Runner error: {_redact_runner_text(str(exc))}",
            provider=_format_route_label_from_payload(route_payload),
            model=model,
            status="failed",
        )
        _post_status(
            orchestrator_url,
            task["id"],
            {
                "status": "failed",
                "message": f"Runner error: {_redact_runner_text(str(exc))}",
                "metadata": {"context_pack_outcome": context_pack_outcome, "agent_fit_selection": agent_fit_selection},
            },
        )


def main() -> None:
    parser = argparse.ArgumentParser(description="ContextLattice task agent worker")
    parser.add_argument(
        "--task-agent",
        default=DEFAULT_AGENT,
        help="trae|letta|autogen|crewai|langgraph|openhands|hermes-agent|hermes|opencode|goose|eliza|pi|droid",
    )
    parser.add_argument("--orchestrator-url", default=DEFAULT_ORCH_URL)
    parser.add_argument("--model-provider", default=DEFAULT_PROVIDER)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--base-url", default=None)
    parser.add_argument("--poll-interval", type=float, default=3.0)
    parser.add_argument("--once", action="store_true", help="Process a single task then exit.")
    parser.add_argument("--worker-name", default=os.getenv("TASK_WORKER", "local-worker"))
    parser.add_argument("--worker-instance", default=os.getenv("TASK_WORKER_INSTANCE", ""))
    parser.add_argument(
        "--dispatcher-id",
        default=next((os.getenv(name) for name in WORKER_STATE_DISPATCHER_ID_ENV_NAMES if os.getenv(name)), ""),
        help="stable dispatcher identity used to bind durable worker state across restarts",
    )
    args = parser.parse_args()

    provider = args.model_provider
    model = args.model
    base_url_override = args.base_url
    agent = args.task_agent
    worker = args.worker_name
    worker_instance = args.worker_instance or worker

    if not model:
        model = DEFAULT_MODEL

    worker_state = _load_or_create_worker_state(worker, args.dispatcher_id or None, args.worker_instance or None)
    if str(args.worker_instance or "").strip() and str(args.worker_instance).strip() != worker_state["worker_instance_id"]:
        raise RuntimeError("configured worker instance does not match durable dispatcher identity")
    worker_state = _register_worker_identity(args.orchestrator_url, worker_state)
    worker_instance = worker_state["worker_instance_id"]
    worker_auth_snapshot = _worker_auth_snapshot(worker_state)
    _WORKER_AUTH_CONTEXT.set(
        (
            worker_instance,
            worker_auth_snapshot.worker_instance_credential,
        )
    )
    # Requested ID is registration metadata only. Every reconciliation,
    # claim-fence, execution, and publication call must carry the durable
    # server-issued canonical ID after registration/acknowledgement.
    worker = worker_state["canonical_worker_id"]

    def retire_on_clean_once_exit() -> None:
        nonlocal worker_state
        if not str(args.dispatcher_id or "").strip() and not str(args.worker_instance or "").strip():
            try:
                # Gateway is the authority for canonical-ID reclamation. A
                # local slot is not reusable until the exact server receipt
                # has durably closed the identity and released its fence.
                receipt = _retire_worker_identity(args.orchestrator_url, worker_state)
                worker_state["retirement_receipt_digest"] = receipt["retirement_receipt_digest"]
                worker_state = _save_worker_state(worker_state)
                if not _retire_unkeyed_worker_state(worker_state):
                    raise RuntimeError("server retirement succeeded but local worker state could not be retired")
            except Exception as exc:  # pragma: no cover - exercised by launcher integration tests
                print(f"[task-worker] identity retirement failed: {_redact_runner_text(str(exc))[:240]}", file=sys.stderr)

    def reconcile() -> None:
        try:
            reconcile_owned_workspaces(
                orchestrator_url=args.orchestrator_url,
                worker=worker,
                worker_instance=worker_instance,
                get_json=_get,
                post_json=_post,
            )
        except ExecutionBlocked as exc:
            print(f"[task-worker] reconciliation retained work: {_redact_runner_text(exc.reason)}", file=sys.stderr)

    reconcile()

    while True:
        try:
            data = _claim_next_task(args.orchestrator_url, worker, state=worker_state)
            task = data.get("task")
            if task:
                # The production polling loop is strict: a Gateway claim is
                # required to carry the complete task/attempt/lease fence.
                # Compatibility callers may still invoke _handle_task directly
                # without a claim, but the worker never executes such a task.
                if not claim_has_complete_fence(data, worker, worker_instance):
                    print("[task-worker] Gateway claim lacked a complete lease fence", file=sys.stderr)
                    if args.once:
                        return
                    time.sleep(args.poll_interval)
                    continue
                _handle_task(
                    args.orchestrator_url,
                    task,
                    agent,
                    provider,
                    model,
                    base_url_override,
                    None,
                    claim=data,
                    worker=worker,
                    worker_instance=worker_instance,
                    auth_snapshot=worker_auth_snapshot,
                )
                reconcile()
            else:
                if args.once:
                    retire_on_clean_once_exit()
                    return
                time.sleep(args.poll_interval)
        except KeyboardInterrupt:
            retire_on_clean_once_exit()
            return
        except Exception as exc:  # pragma: no cover
            print(f"[task-worker] error: {_redact_runner_text(str(exc))[:240]}", file=sys.stderr)
            reconcile()
            if args.once:
                retire_on_clean_once_exit()
                return
            time.sleep(args.poll_interval)


if __name__ == "__main__":
    main()
