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
import subprocess
import sys
import time
from typing import Any, Dict, Optional

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
    agent = agent.lower()
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

    if not _gateway_inference_enabled():
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "failed", "message": "Go inference gateway is disabled; Python inference router is archived"},
        )
        return

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
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "failed", "message": f"Go inference route error: {exc}"},
        )
        return

    if not route_payload:
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "failed", "message": "Go inference route returned no route payload"},
        )
        return

    task_payload = task.get("payload") or {}
    topic_path = task_payload.get("topic_path") or task_payload.get("topicPath")
    context_runtime = ContextExpansionRuntime(orchestrator_url=orchestrator_url, agent_id=(task.get("agent") or agent))
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
    agent_choice = (task.get("agent") or agent).lower()
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
            "TASK_CONTEXT_BUNDLE": _serialize_env_json(context_bundle),
            "TASK_CONTEXT_PROMPT": context_prompt,
            "TASK_TOOL_CONTEXT_SLICES": _serialize_env_json(
                context_bundle.get("tool_slices")
                if isinstance(context_bundle.get("tool_slices"), dict)
                else {}
            ),
        }
    )

    if cmd:
        exit_code = _run_command(cmd, env)
        status = "succeeded" if exit_code == 0 else "failed"
        message = "Task completed by runner command" if exit_code == 0 else "Runner command failed"
        if pending_sources:
            message += f" (pending async sources: {', '.join(str(item) for item in pending_sources[:4])})"
        _post(orchestrator_url, f"/agents/tasks/{task['id']}/status", {"status": status, "message": message})
        context_runtime.write_checkpoint(
            task=task,
            bundle=context_bundle,
            output=message,
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
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "succeeded", "message": completion_message},
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
                },
            },
        )
    except Exception as exc:  # pragma: no cover
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
            {"status": "failed", "message": f"Runner error: {exc}"},
        )


def main() -> None:
    parser = argparse.ArgumentParser(description="ContextLattice task agent worker")
    parser.add_argument(
        "--task-agent",
        default=DEFAULT_AGENT,
        help="trae|letta|autogen|crewai|langgraph|openhands|hermes-agent|hermes|opencode|goose|eliza",
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
