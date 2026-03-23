#!/usr/bin/env python3
"""Generic task runner for ContextLattice agent tasks (used by external agent shims)."""

from __future__ import annotations

import json
import os
import sys
import time
import urllib.request
from typing import Any, Optional

try:
    from scripts.contextlattice_client import (
        build_orchestrator_headers,
        resolve_orchestrator_api_key,
    )
    from scripts.context_expansion_runtime import ContextExpansionRuntime
    from scripts.inference_router import (
        InferenceRoute,
        call_chat_completion,
        format_route_label,
        resolve_inference_route,
    )
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from contextlattice_client import (  # type: ignore[no-redef]
        build_orchestrator_headers,
        resolve_orchestrator_api_key,
    )
    from context_expansion_runtime import ContextExpansionRuntime
    from inference_router import (  # type: ignore[no-redef]
        InferenceRoute,
        call_chat_completion,
        format_route_label,
        resolve_inference_route,
    )


def _post_json(url: str, payload: dict[str, Any], headers: Optional[dict[str, str]] = None) -> dict[str, Any]:
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers or {}, method="POST")
    with urllib.request.urlopen(req, timeout=60) as resp:
        body = resp.read().decode("utf-8")
    return json.loads(body)


def _run_llm_task(
    route: InferenceRoute,
    model: str,
    task: dict[str, Any],
    context_prompt: str | None = None,
) -> str:
    prompt = task.get("title", "Task")
    payload = task.get("payload") or {}
    body = f"{prompt}\n\nPayload:\n{json.dumps(payload, indent=2)}"
    messages = [
        {
            "role": "system",
            "content": (
                "You are a task runner. Provide a concise plan and next actions. "
                "Use supplied factual context and copy numeric facts verbatim."
            ),
        },
    ]
    if context_prompt:
        messages.append({"role": "system", "content": context_prompt})
    messages.append({"role": "user", "content": body})
    return call_chat_completion(route, model, messages)


def _write_memory(orchestrator_url: str, project: str, file_name: str, content: str) -> None:
    url = f"{orchestrator_url.rstrip('/')}/memory/write"
    payload = {"projectName": project, "fileName": file_name, "content": content}
    api_key = resolve_orchestrator_api_key(role="worker")
    headers = {"content-type": "application/json", **build_orchestrator_headers(api_key)}
    _post_json(url, payload, headers=headers)


def _format_result(task: dict[str, Any], output: str, agent_label: str) -> str:
    payload = task.get("payload")
    payload_block = json.dumps(payload, indent=2) if payload else "{}"
    return (
        "# Task Result\n\n"
        f"## Task\n- id: {task.get('id')}\n- title: {task.get('title')}\n- project: {task.get('project')}\n"
        f"- agent: {agent_label}\n\n"
        "## Payload\n```json\n"
        f"{payload_block}\n```\n\n"
        f"## Output\n{output}\n"
    )


def main(agent_label: Optional[str] = None) -> int:
    orchestrator_url = os.getenv(
        "CONTEXTLATTICE_ORCHESTRATOR_URL",
        os.getenv("MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075"),
    )
    task_id = os.getenv("TASK_ID")
    task_title = os.getenv("TASK_TITLE", "Task")
    task_project = os.getenv("TASK_PROJECT", "_global")
    task_payload = os.getenv("TASK_PAYLOAD", "{}")
    agent = (agent_label or os.getenv("TASK_AGENT", "trae")).lower()
    provider = os.getenv("TASK_MODEL_PROVIDER", os.getenv("ORCH_INFER_PROVIDER", "auto"))
    model = os.getenv("TASK_MODEL", "qwen3.5:9b")
    base_url_override = os.getenv("TASK_BASE_URL")
    api_key = os.getenv("TASK_API_KEY")

    try:
        payload_data = json.loads(task_payload) if task_payload else {}
    except json.JSONDecodeError:
        payload_data = {"raw": task_payload}

    task = {
        "id": task_id or f"adhoc-{int(time.time())}",
        "title": task_title,
        "project": task_project,
        "agent": agent,
        "payload": payload_data,
    }

    context_runtime = ContextExpansionRuntime(
        orchestrator_url=orchestrator_url,
        agent_id=agent,
        caller_role="worker",
    )
    context_bundle = None
    context_prompt: str | None = None
    context_bundle_env = str(os.getenv("TASK_CONTEXT_BUNDLE") or "").strip()
    if context_bundle_env:
        try:
            parsed = json.loads(context_bundle_env)
            if isinstance(parsed, dict):
                context_bundle = parsed
                context_prompt = context_runtime.render_for_prompt(parsed)
        except json.JSONDecodeError:
            context_bundle = None
            context_prompt = None
    if context_bundle is None:
        try:
            context_bundle = context_runtime.prepare(task)
            context_prompt = context_runtime.render_for_prompt(context_bundle)
        except Exception:
            context_bundle = {
                "enabled": False,
                "query": task_title,
                "project": task_project,
                "topic_path": None,
                "warnings": ["context expansion failed-open"],
                "lifecycle": {"status": "failed_open", "result_state": "failed_open", "degraded": True},
                "layers": {"l0_facts": [], "l1_rollups": [], "l2_raw_refs": []},
                "numeric_facts": [],
                "tool_slices": {},
                "expansion": {"broadened_scope": False, "deep_escalated": False, "steps": ["failed_open"]},
            }
            context_prompt = "Context expansion unavailable; proceed fail-open."

    try:
        route = resolve_inference_route(
            provider,
            base_url_override=base_url_override,
            api_key=api_key,
        )
    except Exception as exc:
        print(f"[agent-runner] inference route failed: {exc}", file=sys.stderr)
        return 1

    try:
        output = _run_llm_task(
            route,
            model,
            task,
            context_prompt=context_prompt,
        )
        file_name = f"task_runs/{task['id']}.md"
        _write_memory(orchestrator_url, task_project or "_global", file_name, _format_result(task, output, agent))
        if isinstance(context_bundle, dict):
            context_runtime.write_checkpoint(
                task=task,
                bundle=context_bundle,
                output=output,
                provider=format_route_label(route),
                model=model,
                status="succeeded",
            )
    except Exception as exc:
        if isinstance(context_bundle, dict):
            context_runtime.write_checkpoint(
                task=task,
                bundle=context_bundle,
                output=f"[agent-runner] failed: {exc}",
                provider=format_route_label(route),
                model=model,
                status="failed",
            )
        print(f"[agent-runner] failed: {exc}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
