#!/usr/bin/env python3
"""
Lightweight task worker for ContextLattice agent tasks.
Claims tasks from the orchestrator and routes them to a runner (Trae, Letta, etc.)
or a simple local model call when no runner is configured.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Optional

RUNNERS_DIR = Path(__file__).resolve().parent / "agent_runners"
if str(RUNNERS_DIR) not in sys.path:
    sys.path.insert(0, str(RUNNERS_DIR))

try:
    from scripts.contextlattice_client import (
        ContextLatticeClient,
        build_orchestrator_headers,
        resolve_orchestrator_api_key,
    )
    from scripts.agent_contracts import attach_format_contract
    from scripts.context_expansion_runtime import ContextExpansionRuntime
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from contextlattice_client import (  # type: ignore[no-redef]
        ContextLatticeClient,
        build_orchestrator_headers,
        resolve_orchestrator_api_key,
    )
    from agent_contracts import attach_format_contract  # type: ignore[no-redef]
    from context_expansion_runtime import ContextExpansionRuntime

try:
    from runner_quality import record_runner_quality
except ModuleNotFoundError:  # pragma: no cover - runner metrics are fail-open
    record_runner_quality = None  # type: ignore[assignment]

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

ADAPTER_AGENT_ALIASES = {
    "pi": "pi",
    "pi-coding-agent": "pi",
    "droid": "droid",
    "factory-droid": "droid",
}

RUNNER_SECRET_PATTERNS = (
    re.compile(r"Bearer\s+[A-Za-z0-9._~+/=-]{16,}", re.IGNORECASE),
    re.compile(r"sk-[A-Za-z0-9_-]{12,}"),
    re.compile(r"(?<![A-Za-z0-9])[A-Za-z0-9][A-Za-z0-9_-]{47,}(?![A-Za-z0-9])"),
)


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
) -> tuple[str, dict[str, Any]]:
    payload: dict[str, Any] = {
        "provider": provider,
        "model": model,
        "messages": _build_task_messages(task, context_prompt=context_prompt),
    }
    if base_url_override:
        payload["base_url"] = base_url_override
    if api_key:
        payload["api_key"] = api_key
    response = _post(control_plane_url, "/v1/inference/chat", payload, timeout=95.0)
    content = str(response.get("content") or "")
    if not content.strip():
        raise RuntimeError("gateway-go returned empty inference content")
    route_payload = response.get("route") if isinstance(response.get("route"), dict) else {}
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
    return subprocess.call(cmd, shell=True, env=env)


def _normalize_agent_alias(agent: str) -> str:
    normalized = str(agent or "").strip().lower().replace("_", "-")
    return ADAPTER_AGENT_ALIASES.get(normalized, normalized)


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


def _redact_runner_text(value: str) -> str:
    text = str(value or "")
    for pattern in RUNNER_SECRET_PATTERNS:
        text = pattern.sub("[REDACTED]", text)
    return text


def _runner_tail(value: str, limit: int = 4000) -> str:
    text = _redact_runner_text(value)
    if len(text) <= limit:
        return text
    return text[-limit:]


def _runner_timeout_secs(task_payload: dict[str, Any]) -> int:
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
    return value if value > 0 else 930


def _fallback_runner_result(
    agent: str,
    status: str,
    exit_code: int,
    summary: str,
    stdout: str = "",
    stderr: str = "",
) -> dict[str, Any]:
    now = _runner_now_iso()
    return attach_format_contract(
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
            "summary": summary[:3000],
            "stdout_tail": _runner_tail(stdout),
            "stderr_tail": _runner_tail(stderr),
            "artifacts": [],
            "warnings": [],
            "metadata": {"adapter": "task_agent_worker", "parse_fallback": True},
        },
    )


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
            return parsed
    return _fallback_runner_result(
        agent,
        "failed",
        exit_code,
        "Runner adapter did not emit valid runner_result.v1 JSON",
        stdout=stdout,
        stderr=stderr,
    )


def _run_adapter(argv: list[str], env: dict[str, str], cwd: Path, timeout: int, agent: str) -> dict[str, Any]:
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
        return _fallback_runner_result(agent, "failed", 1, f"Runner adapter execution failed: {exc}")


def _compact_runner_result(result: dict[str, Any]) -> dict[str, Any]:
    metadata = result.get("metadata") if isinstance(result.get("metadata"), dict) else {}
    return {
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
        "summary": str(result.get("summary") or "")[:1500],
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
    return {
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
        sample, storage = record_runner_quality(
            task=task,
            agent=agent,
            result=result,
            context_bundle=context_bundle,
            task_status=task_status,
            message=message,
            route_payload=route_payload,
        )
        return _compact_runner_quality_sample(sample, storage)
    except Exception as exc:
        return {"ok": False, "reason": "runner_quality_record_failed", "error": str(exc)[:240]}


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
    }
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
        response = _post(orchestrator_url, "/telemetry/context-pack-quality/outcome", payload)
    except Exception as exc:
        return {"ok": False, "recorded": False, "reason": "outcome_post_failed", "error": str(exc)[:240], "sample_id": sample_id}
    outcome = response.get("outcome") if isinstance(response.get("outcome"), dict) else {}
    return {
        "ok": bool(response.get("ok", False)),
        "recorded": bool(response.get("recorded", response.get("ok", False))),
        "duplicate": bool(response.get("duplicate", False)),
        "sample_id": sample_id,
        "outcome_id": str(outcome.get("outcome_id") or ""),
        "outcome_class": str(outcome.get("outcome_class") or payload["outcome_class"]),
        "calibration_eligible": bool(outcome.get("calibration_eligible", calibration_eligible)),
    }


def _env_bool_value(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    return str(value or "").strip().lower() in {"1", "true", "yes", "on"}


def _orchestrator_headers() -> dict[str, str]:
    return build_orchestrator_headers(resolve_orchestrator_api_key(role="worker"))


def _post(
    orchestrator_url: str,
    path: str,
    payload: dict[str, Any],
    params: dict[str, str] | None = None,
    *,
    timeout: float = 30.0,
) -> dict[str, Any]:
    client = ContextLatticeClient(
        base_url=orchestrator_url,
        timeout=timeout,
        role="worker",
    )
    try:
        return client.post_json(
            path,
            payload,
            params=params,
            timeout=max(1.0, float(timeout)),
        )
    finally:
        client.close()


def _get(
    orchestrator_url: str,
    path: str,
    params: dict[str, str] | None = None,
    *,
    timeout: float = 30.0,
) -> dict[str, Any]:
    client = ContextLatticeClient(
        base_url=orchestrator_url,
        timeout=timeout,
        role="worker",
    )
    try:
        return client.get_json(
            path,
            params=params,
            timeout=max(1.0, float(timeout)),
        )
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
        "projectName": project,
        "fileName": file_name,
        "content": content,
    }
    if topic_path:
        payload["topicPath"] = topic_path
    _post(orchestrator_url, "/memory/write", payload)


def _post_feedback(orchestrator_url: str, payload: dict[str, Any]) -> None:
    try:
        _post(orchestrator_url, "/feedback", payload)
    except Exception:
        return


def _format_result(task: dict[str, Any], output: str) -> str:
    payload = task.get("payload")
    payload_block = json.dumps(payload, indent=2) if payload else "{}"
    contract_payload = attach_format_contract(
        "agent_task_result.v1",
        {
            "ok": True,
            "task_id": str(task.get("id") or ""),
            "project": str(task.get("project") or "_global"),
            "agent": str(task.get("agent") or DEFAULT_AGENT),
            "status": "succeeded",
            "output": output[:119000],
        },
    )
    contract_block = json.dumps(contract_payload, indent=2, sort_keys=True)
    return f"""# Task Result\n\n```json contextlattice_contract\n{contract_block}\n```\n\n## Task\n- id: {task.get('id')}\n- title: {task.get('title')}\n- project: {task.get('project')}\n- agent: {task.get('agent')}\n\n## Payload\n```\n{payload_block}\n```\n\n## Output\n{output}\n"""


def _serialize_env_json(payload: dict[str, Any], max_chars: int = 65000) -> str:
    rendered = json.dumps(payload, separators=(",", ":"), ensure_ascii=True)
    if len(rendered) <= max_chars:
        return rendered
    return rendered[: max_chars - 1] + "}"


def _handle_task(
    orchestrator_url: str,
    task: dict[str, Any],
    agent: str,
    provider: str,
    model: str,
    base_url_override: str | None,
    api_key: Optional[str],
) -> None:
    control_plane_url = str(
        os.getenv("TASK_INFERENCE_CONTROL_PLANE_URL", DEFAULT_INFERENCE_CONTROL_PLANE_URL)
    ).strip() or orchestrator_url
    route_payload: dict[str, Any] = {}
    task_payload = task.get("payload") or {}
    agent_choice = _normalize_agent_alias(task.get("agent") or agent)
    adapter_argv = None if os.getenv("TASK_AGENT_CMD") else _runner_adapter_for_agent(agent_choice)

    if not _gateway_inference_enabled() and adapter_argv is None:
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
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
                    "api_key": api_key or "",
                },
                timeout=20.0,
            )
            route_candidate = route_response.get("route")
            if isinstance(route_candidate, dict):
                route_payload = dict(route_candidate)
        except Exception as exc:
            if adapter_argv is None:
                _post(
                    orchestrator_url,
                    f"/agents/tasks/{task['id']}/status",
                    {"status": "failed", "message": f"Go inference route error: {exc}"},
                )
                return
            route_payload = {
                "provider": provider,
                "base_url": base_url_override or "",
                "reason": f"inference route unavailable for optional adapter execution: {exc}",
            }

    if not route_payload and adapter_argv is None:
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "failed", "message": "Go inference route returned no route payload"},
        )
        return
    if not route_payload:
        route_payload = {
            "provider": provider,
            "base_url": base_url_override or "",
            "reason": "inference route skipped for optional runner adapter",
        }

    topic_path = task_payload.get("topic_path") or task_payload.get("topicPath")
    context_runtime = ContextExpansionRuntime(orchestrator_url=orchestrator_url, agent_id=agent_choice)
    context_bundle: dict[str, Any]
    context_prompt: str
    try:
        context_bundle = context_runtime.prepare(task)
        context_prompt = context_runtime.render_for_prompt(context_bundle)
    except Exception as exc:
        context_bundle = {
            "enabled": False,
            "query": str(task.get("title") or "task context"),
            "project": str(task.get("project") or "_global"),
            "topic_path": topic_path,
            "warnings": [f"context expansion failed: {exc}"],
            "lifecycle": {"status": "failed_open", "result_state": "failed_open", "degraded": True},
            "layers": {"l0_facts": [], "l1_rollups": [], "l2_raw_refs": []},
            "numeric_facts": [],
            "tool_slices": {},
            "expansion": {"broadened_scope": False, "deep_escalated": False, "steps": ["failed_open"]},
        }
        context_prompt = "Context expansion unavailable; continue with fail-open execution."

    lifecycle = context_bundle.get("lifecycle") if isinstance(context_bundle.get("lifecycle"), dict) else {}
    pending_sources = lifecycle.get("pending_sources") if isinstance(lifecycle.get("pending_sources"), list) else []

    if task.get("approval_required") and not task.get("approved"):
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "blocked", "message": "Awaiting approval"},
        )
        return
    cmd = _runner_cmd_for_agent(agent_choice)
    route_provider = str(route_payload.get("provider") or provider)
    route_base_url = str(route_payload.get("base_url") or (base_url_override or ""))
    route_reason = str(route_payload.get("reason") or "")
    route_label = _format_route_label_from_payload(route_payload)
    env = os.environ.copy()
    env.update(
        {
            "TASK_ID": task["id"],
            "TASK_TITLE": task["title"],
            "TASK_PROJECT": task.get("project") or "",
            "TASK_AGENT": agent_choice,
            "TASK_PAYLOAD": json.dumps(task.get("payload") or {}),
            "TASK_MODEL_PROVIDER": route_provider,
            "TASK_MODEL": model,
            "TASK_BASE_URL": route_base_url,
            "TASK_API_KEY": str(api_key or ""),
            "CONTEXTLATTICE_ORCHESTRATOR_URL": orchestrator_url,
            "MEMMCP_ORCHESTRATOR_URL": orchestrator_url,
            "CONTEXTLATTICE_SESSION_ID": str(
                task_payload.get("session_id") or task_payload.get("sessionId") or task.get("session_id") or os.getenv("CONTEXTLATTICE_SESSION_ID", "")
            ),
            "CONTEXTLATTICE_AGENT_ID": str(task_payload.get("agent_id") or task_payload.get("agentId") or f"{agent_choice.replace('-', '_')}_agent"),
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
            message += f" (pending async sources: {', '.join(str(item) for item in pending_sources[:4])})"
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
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {
                "status": task_status,
                "message": message,
                "metadata": {
                    "runner_result": compact_result,
                    "runner_quality": runner_quality,
                    "context_pack_outcome": context_pack_outcome,
                    "agent_state": agent_state,
                    "retrieval_lifecycle": lifecycle,
                },
            },
        )
        context_runtime.write_checkpoint(
            task=task,
            bundle=context_bundle,
            output=json.dumps(
                {
                    "schema_id": "contextlattice_runner_checkpoint.v1",
                    "message": message,
                    "runner_result": compact_result,
                    "runner_quality": runner_quality,
                    "context_pack_outcome": context_pack_outcome,
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
            message += f" (pending async sources: {', '.join(str(item) for item in pending_sources[:4])})"
        context_pack_outcome = _post_context_pack_outcome(
            orchestrator_url,
            task=task,
            context_bundle=context_bundle,
            status=status,
            source="legacy_command",
            calibration_eligible=True,
            outcome_class="success" if status == "succeeded" else "task_failure",
        )
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": status, "message": message, "metadata": {"context_pack_outcome": context_pack_outcome}},
        )
        context_runtime.write_checkpoint(
            task=task,
            bundle=context_bundle,
            output=json.dumps({"message": message, "context_pack_outcome": context_pack_outcome}, sort_keys=True),
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
                    api_key=api_key,
                )
                if gateway_route_payload:
                    active_route_payload = dict(gateway_route_payload)
            except Exception as exc:
                if _gateway_inference_required():
                    raise RuntimeError(f"go inference control plane required but unavailable: {exc}") from exc
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
            completion_message += f" | pending async sources: {', '.join(str(item) for item in pending_sources[:4])}"
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
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "succeeded", "message": completion_message, "metadata": {"context_pack_outcome": context_pack_outcome}},
        )
        context_runtime.write_checkpoint(
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
        context_runtime.write_checkpoint(
            task=task,
            bundle=context_bundle,
            output=f"Runner error: {exc}",
            provider=_format_route_label_from_payload(route_payload),
            model=model,
            status="failed",
        )
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {
                "status": "failed",
                "message": f"Runner error: {exc}",
                "metadata": {"context_pack_outcome": context_pack_outcome},
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
    parser.add_argument("--api-key", default=os.getenv("TASK_API_KEY") or os.getenv("OPENAI_API_KEY"))
    parser.add_argument("--poll-interval", type=float, default=3.0)
    parser.add_argument("--once", action="store_true", help="Process a single task then exit.")
    parser.add_argument("--worker-name", default=os.getenv("TASK_WORKER", "local-worker"))
    args = parser.parse_args()

    provider = args.model_provider
    model = args.model
    base_url_override = args.base_url
    agent = args.task_agent
    worker = args.worker_name

    if not model:
        model = DEFAULT_MODEL

    while True:
        try:
            data = _post(
                args.orchestrator_url,
                "/agents/tasks/next",
                {},
                params={"worker": worker},
            )
            task = data.get("task")
            if task:
                _handle_task(
                    args.orchestrator_url,
                    task,
                    agent,
                    provider,
                    model,
                    base_url_override,
                    args.api_key,
                )
            else:
                if args.once:
                    return
                time.sleep(args.poll_interval)
        except KeyboardInterrupt:
            return
        except Exception as exc:  # pragma: no cover
            print(f"[task-worker] error: {exc}", file=sys.stderr)
            time.sleep(args.poll_interval)


if __name__ == "__main__":
    main()
