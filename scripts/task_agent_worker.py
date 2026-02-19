#!/usr/bin/env python3
"""
Lightweight task worker for memMCP agent tasks.
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

import httpx

DEFAULT_ORCH_URL = os.getenv("MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075")
DEFAULT_AGENT = os.getenv("TASK_AGENT", "trae")
DEFAULT_PROVIDER = os.getenv("TASK_MODEL_PROVIDER", "ollama")
DEFAULT_MODEL = os.getenv("TASK_MODEL", "qwen2.5-coder:7b")


def _base_url_for_provider(provider: str, override: Optional[str]) -> str:
    if override:
        return override.rstrip("/")
    provider = provider.lower()
    if provider == "ollama":
        return "http://127.0.0.1:11434"
    if provider == "lmstudio":
        return "http://127.0.0.1:1234"
    if provider in {"openai-compatible", "vllm"}:
        return "http://127.0.0.1:8000"
    if provider == "llama-cpp":
        return "http://127.0.0.1:8080"
    return "http://127.0.0.1:8000"


def _call_openai_compatible(
    base_url: str,
    model: str,
    messages: list[dict[str, str]],
    api_key: Optional[str] = None,
) -> str:
    url = f"{base_url}/v1/chat/completions"
    headers = {"content-type": "application/json"}
    if api_key:
        headers["authorization"] = f"Bearer {api_key}"
    payload = {
        "model": model,
        "messages": messages,
        "temperature": 0.2,
        "stream": False,
    }
    resp = httpx.post(url, json=payload, headers=headers, timeout=60.0)
    resp.raise_for_status()
    data = resp.json()
    return data["choices"][0]["message"]["content"]


def _call_ollama(
    base_url: str,
    model: str,
    messages: list[dict[str, str]],
) -> str:
    url = f"{base_url}/api/chat"
    payload = {"model": model, "messages": messages, "stream": False}
    resp = httpx.post(url, json=payload, timeout=60.0)
    resp.raise_for_status()
    data = resp.json()
    message = data.get("message") or {}
    return message.get("content", "")


def _run_llm_task(
    provider: str,
    model: str,
    base_url: str,
    api_key: Optional[str],
    task: dict[str, Any],
) -> str:
    prompt = task.get("title", "Task")
    payload = task.get("payload") or {}
    body = f"{prompt}\n\nPayload:\n{json.dumps(payload, indent=2)}"
    messages = [
        {
            "role": "system",
            "content": "You are a task runner. Provide a concise plan and next actions.",
        },
        {"role": "user", "content": body},
    ]
    provider = provider.lower()
    if provider == "ollama":
        return _call_ollama(base_url, model, messages)
    return _call_openai_compatible(base_url, model, messages, api_key)


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
    return None


def _run_command(cmd: str, env: dict[str, str]) -> int:
    return subprocess.call(cmd, shell=True, env=env)


def _post(orchestrator_url: str, path: str, payload: dict[str, Any], params: dict[str, str] | None = None) -> dict[str, Any]:
    url = f"{orchestrator_url.rstrip('/')}{path}"
    resp = httpx.post(url, json=payload, params=params, timeout=30.0)
    resp.raise_for_status()
    return resp.json()


def _get(orchestrator_url: str, path: str, params: dict[str, str] | None = None) -> dict[str, Any]:
    url = f"{orchestrator_url.rstrip('/')}{path}"
    resp = httpx.get(url, params=params, timeout=30.0)
    resp.raise_for_status()
    return resp.json()


def _write_memory(orchestrator_url: str, project: str, file_name: str, content: str) -> None:
    _post(orchestrator_url, "/memory/write", {"projectName": project, "fileName": file_name, "content": content})


def _post_feedback(orchestrator_url: str, payload: dict[str, Any]) -> None:
    try:
        _post(orchestrator_url, "/feedback", payload)
    except Exception:
        return


def _format_result(task: dict[str, Any], output: str) -> str:
    payload = task.get("payload")
    payload_block = json.dumps(payload, indent=2) if payload else "{}"
    return f"""# Task Result\n\n## Task\n- id: {task.get('id')}\n- title: {task.get('title')}\n- project: {task.get('project')}\n- agent: {task.get('agent')}\n\n## Payload\n```\n{payload_block}\n```\n\n## Output\n{output}\n"""


def _handle_task(
    orchestrator_url: str,
    task: dict[str, Any],
    agent: str,
    provider: str,
    model: str,
    base_url: str,
    api_key: Optional[str],
) -> None:
    task_payload = task.get("payload") or {}
    topic_path = task_payload.get("topic_path") or task_payload.get("topicPath")
    if task.get("approval_required") and not task.get("approved"):
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "blocked", "message": "Awaiting approval"},
        )
        return
    agent_choice = (task.get("agent") or agent).lower()
    cmd = _runner_cmd_for_agent(agent_choice)
    env = os.environ.copy()
    env.update(
        {
            "TASK_ID": task["id"],
            "TASK_TITLE": task["title"],
            "TASK_PROJECT": task.get("project") or "",
            "TASK_AGENT": agent_choice,
            "TASK_PAYLOAD": json.dumps(task.get("payload") or {}),
            "TASK_MODEL_PROVIDER": provider,
            "TASK_MODEL": model,
            "TASK_BASE_URL": base_url,
            "TASK_API_KEY": api_key or "",
            "MEMMCP_ORCHESTRATOR_URL": orchestrator_url,
        }
    )

    if cmd:
        exit_code = _run_command(cmd, env)
        status = "succeeded" if exit_code == 0 else "failed"
        message = "Task completed by runner command" if exit_code == 0 else "Runner command failed"
        _post(orchestrator_url, f"/agents/tasks/{task['id']}/status", {"status": status, "message": message})
        if exit_code == 0:
            _post_feedback(
                orchestrator_url,
            {
                "project": task.get("project"),
                "task_id": task.get("id"),
                "source": "agent",
                "content": message,
                "topic_path": topic_path,
                "metadata": {"agent": agent_choice, "provider": provider, "model": model},
            },
        )
        return

    try:
        output = _run_llm_task(provider, model, base_url, api_key, task)
        project = task.get("project") or "_global"
        file_name = f"task_runs/{task['id']}.md"
        _write_memory(orchestrator_url, project, file_name, _format_result(task, output))
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "succeeded", "message": f"Completed via {provider} ({model})"},
        )
        _post_feedback(
            orchestrator_url,
            {
                "project": project,
                "task_id": task.get("id"),
                "source": "agent",
                "content": output[:1500],
                "topic_path": topic_path,
                "metadata": {"agent": agent_choice, "provider": provider, "model": model},
            },
        )
    except Exception as exc:  # pragma: no cover
        _post(
            orchestrator_url,
            f"/agents/tasks/{task['id']}/status",
            {"status": "failed", "message": f"Runner error: {exc}"},
        )


def main() -> None:
    parser = argparse.ArgumentParser(description="memMCP task agent worker")
    parser.add_argument("--task-agent", default=DEFAULT_AGENT, help="trae|letta|autogen|crewai|langgraph|openhands")
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
    base_url = _base_url_for_provider(provider, args.base_url)
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
                    base_url,
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
