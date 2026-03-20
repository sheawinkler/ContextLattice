#!/usr/bin/env python3
"""
Agent orchestration helper for ContextLattice.
Enables multi-agent coordination through shared memory + task tracking.
"""

import json
import os
import sys
import time
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional
from urllib.parse import quote, urljoin

import httpx

DEFAULT_ORCHESTRATOR_URL = os.getenv(
    "CONTEXTLATTICE_ORCHESTRATOR_URL",
    os.getenv("MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075"),
)
DEFAULT_AGENT_ID = (
    os.getenv("CONTEXTLATTICE_AGENT_ID", "").strip()
    or os.getenv("MEMMCP_AGENT_ID", "").strip()
    or "codex_gpt5"
)
DEFAULT_AGENT_PREFLIGHT_PROFILES: Dict[str, Dict[str, str]] = {
    "codex": {
        "agent_id": "codex_gpt5",
        "topic_path": "runbooks/codex-integration",
        "query": "codex preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "claude-code": {
        "agent_id": "claude_code_agent",
        "topic_path": "runbooks/claude-code-integration",
        "query": "claude code preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "opencode": {
        "agent_id": "opencode_agent",
        "topic_path": "runbooks/opencode-integration",
        "query": "opencode preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "hermes-agent": {
        "agent_id": "hermes_agent",
        "topic_path": "runbooks/hermes-agent-integration",
        "query": "hermes agent preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "chatgpt-web": {
        "agent_id": "chatgpt_web_agent",
        "topic_path": "runbooks/chatgpt-web-integration",
        "query": "chatgpt web session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "chatgpt-desktop": {
        "agent_id": "chatgpt_desktop_agent",
        "topic_path": "runbooks/chatgpt-desktop-integration",
        "query": "chatgpt desktop session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "claude-web": {
        "agent_id": "claude_web_agent",
        "topic_path": "runbooks/claude-web-integration",
        "query": "claude web session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "claude-desktop": {
        "agent_id": "claude_desktop_agent",
        "topic_path": "runbooks/claude-desktop-integration",
        "query": "claude desktop session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
}
AGENT_PREFLIGHT_ALIASES = {
    "codex_gpt5": "codex",
    "claude-code": "claude-code",
    "claude_code": "claude-code",
    "opencode": "opencode",
    "hermes": "hermes-agent",
    "hermes-agent": "hermes-agent",
    "chatgpt": "chatgpt-web",
    "chatgpt-web": "chatgpt-web",
    "chatgpt-desktop": "chatgpt-desktop",
    "claude": "claude-web",
    "claude-web": "claude-web",
    "claude-desktop": "claude-desktop",
}


def _load_agent_preflight_profiles() -> Dict[str, Dict[str, str]]:
    profiles: Dict[str, Dict[str, str]] = {
        key: dict(value) for key, value in DEFAULT_AGENT_PREFLIGHT_PROFILES.items()
    }
    profile_path = Path(__file__).resolve().parents[1] / "config" / "agents" / "agent_profiles.json"
    if not profile_path.exists():
        return profiles
    try:
        payload = json.loads(profile_path.read_text(encoding="utf-8"))
    except Exception:
        return profiles
    raw_profiles = payload.get("profiles") if isinstance(payload, dict) else None
    if not isinstance(raw_profiles, dict):
        return profiles
    for raw_key, raw_profile in raw_profiles.items():
        key = str(raw_key or "").strip().lower()
        if not key:
            key = "codex"
        key = AGENT_PREFLIGHT_ALIASES.get(key, key)
        if not isinstance(raw_profile, dict):
            continue
        current = dict(profiles.get(key) or {})
        for field in ("agent_id", "topic_path", "query", "retrieval_mode"):
            value = str(raw_profile.get(field, "")).strip()
            if value:
                current[field] = value
        if current:
            profiles[key] = current
    return profiles


AGENT_PREFLIGHT_PROFILES = _load_agent_preflight_profiles()


def _normalize_agent_profile_key(agent: Optional[str]) -> str:
    candidate = str(agent or "").strip().lower()
    if not candidate:
        return "codex"
    return AGENT_PREFLIGHT_ALIASES.get(candidate, candidate)


def _resolve_agent_preflight_profile(agent: Optional[str]) -> tuple[str, Dict[str, str]]:
    key = _normalize_agent_profile_key(agent)
    profile = AGENT_PREFLIGHT_PROFILES.get(key)
    if not profile:
        key = "codex"
        profile = AGENT_PREFLIGHT_PROFILES.get(key) or DEFAULT_AGENT_PREFLIGHT_PROFILES[key]
    return key, dict(profile)


class ContextLatticeOrchestrator:
    """Helper for agent coordination via ContextLattice."""

    def __init__(
        self,
        orchestrator_url: str = DEFAULT_ORCHESTRATOR_URL,
        agent_id: str = DEFAULT_AGENT_ID,
    ):
        self.base_url = orchestrator_url.rstrip("/")
        self.agent_id = str(agent_id or "").strip() or DEFAULT_AGENT_ID
        api_key = (
            os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "").strip()
            or os.getenv("MEMMCP_ORCHESTRATOR_API_KEY", "").strip()
        )
        headers = {"x-api-key": api_key} if api_key else None
        self.client = httpx.Client(timeout=30.0, headers=headers)

    def _encode_project_path(self, project: str, file_name: str | None = None) -> str:
        encoded_project = quote(project, safe="")
        if not file_name:
            return encoded_project
        cleaned = file_name.lstrip("/")
        parts = [quote(part, safe="") for part in cleaned.split("/") if part]
        return f"{encoded_project}/{'/'.join(parts)}" if parts else encoded_project

    def write(
        self,
        project: str,
        file_name: str,
        content: str,
        topic_path: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Write a file to ContextLattice."""
        payload: Dict[str, Any] = {
            "projectName": project,
            "fileName": file_name,
            "content": content,
        }
        if topic_path:
            payload["topicPath"] = topic_path
        resp = self.client.post(
            f"{self.base_url}/memory/write",
            json=payload,
        )
        resp.raise_for_status()
        return resp.json()

    def read(self, project: str, file_name: str) -> str:
        """Read a file from ContextLattice."""
        path = self._encode_project_path(project, file_name)
        resp = self.client.get(f"{self.base_url}/memory/files/{path}")
        resp.raise_for_status()
        if resp.headers.get("content-type", "").startswith("application/json"):
            return json.dumps(resp.json(), indent=2)
        return resp.text

    def list_files(self, project: str) -> List[str]:
        """List files in a project."""
        encoded_project = self._encode_project_path(project)
        resp = self.client.get(f"{self.base_url}/projects/{encoded_project}/files")
        resp.raise_for_status()
        data = resp.json()
        return data.get("files", [])

    def search(
        self,
        query: str,
        project: Optional[str] = None,
        limit: int = 10,
        fetch_content: bool = False,
    ) -> List[Dict[str, Any]]:
        """Semantic search across memory."""
        payload = self.search_with_lifecycle(
            query=query,
            project=project,
            limit=limit,
            fetch_content=fetch_content,
            wait_for_completion=False,
        )
        return payload.get("results", []) if isinstance(payload, dict) else []

    def _absolute_url(self, path_or_url: str) -> str:
        token = str(path_or_url or "").strip()
        if token.startswith("http://") or token.startswith("https://"):
            return token
        if not token.startswith("/"):
            token = "/" + token
        return urljoin(self.base_url + "/", token.lstrip("/"))

    @staticmethod
    def _lifecycle_summary(payload: Dict[str, Any]) -> Dict[str, Any]:
        lifecycle = payload.get("retrieval_lifecycle") if isinstance(payload, dict) else {}
        lifecycle = lifecycle if isinstance(lifecycle, dict) else {}
        status = str(lifecycle.get("status") or payload.get("status") or "").strip().lower()
        sources = lifecycle.get("sources") if isinstance(lifecycle.get("sources"), dict) else {}
        return {
            "status": status or "unknown",
            "result_state": str(lifecycle.get("result_state") or payload.get("result_state") or "").strip().lower() or None,
            "returned_now": list(sources.get("returned_now") or []),
            "pending": list(sources.get("pending") or payload.get("source_summary", {}).get("pending_sources", []) or []),
            "failed": list(sources.get("failed") or payload.get("source_summary", {}).get("failed_sources", []) or []),
            "timed_out": list(sources.get("timed_out") or payload.get("source_summary", {}).get("timed_out_sources", []) or []),
            "budget_exceeded": list(
                sources.get("budget_exceeded") or payload.get("source_summary", {}).get("budget_exceeded_sources", []) or []
            ),
            "next_actions": list(lifecycle.get("next_actions") or []),
        }

    def search_with_lifecycle(
        self,
        query: str,
        project: Optional[str] = None,
        topic_path: Optional[str] = None,
        limit: int = 10,
        fetch_content: bool = False,
        retrieval_mode: str = "balanced",
        include_grounding: bool = True,
        include_retrieval_debug: bool = False,
        agent_id: Optional[str] = None,
        deep_async: Optional[bool] = None,
        wait_for_completion: bool = False,
        poll_interval_secs: float = 1.5,
        max_wait_secs: float = 75.0,
    ) -> Dict[str, Any]:
        """
        Search memory and expose retrieval lifecycle details.

        For deep mode, async is enabled by default; callers can set
        `wait_for_completion=True` to block until final deep results arrive.
        """
        mode = str(retrieval_mode or "balanced").strip().lower() or "balanced"
        deep_async_value = deep_async
        if deep_async_value is None and mode == "deep":
            deep_async_value = True
        request_payload = {
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "limit": limit,
            "fetch_content": fetch_content,
            "retrieval_mode": mode,
            "include_grounding": include_grounding,
            "include_retrieval_debug": include_retrieval_debug,
            "agent_id": str(agent_id or self.agent_id).strip() or DEFAULT_AGENT_ID,
            "deep_async": bool(deep_async_value) if deep_async_value is not None else None,
        }
        if request_payload["deep_async"] is None:
            request_payload.pop("deep_async", None)

        resp = self.client.post(f"{self.base_url}/memory/search", json=request_payload)
        resp.raise_for_status()
        initial = resp.json()
        lifecycle = self._lifecycle_summary(initial)
        results = initial.get("results") if isinstance(initial.get("results"), list) else []
        output: Dict[str, Any] = {
            "ok": True,
            "query": query,
            "project": project,
            "retrieval_mode": mode,
            "results": results,
            "lifecycle": lifecycle,
            "async": bool(initial.get("async")),
            "token": initial.get("token") or initial.get("job_id"),
            "poll_url": initial.get("job_poll_url") or initial.get("poll_url"),
            "events_url": initial.get("events_url"),
            "warnings": initial.get("warnings") if isinstance(initial.get("warnings"), list) else [],
            "initial_response": initial,
            "final_response": None,
        }

        status = lifecycle.get("status")
        is_pending = status in {"queued", "running", "partial"} and bool(output.get("token"))
        if not wait_for_completion or not is_pending:
            return output

        poll_url = str(output.get("poll_url") or "").strip()
        if not poll_url:
            return output

        deadline = time.monotonic() + max(1.0, float(max_wait_secs))
        poll_target = self._absolute_url(poll_url)
        while time.monotonic() < deadline:
            poll_resp = self.client.get(
                poll_target,
                params={"include_result": "true"},
            )
            if poll_resp.status_code == 404:
                time.sleep(max(0.2, float(poll_interval_secs)))
                continue
            poll_resp.raise_for_status()
            polled = poll_resp.json()
            job_status = str(polled.get("status") or "").strip().lower()
            if job_status in {"completed", "failed"}:
                final_payload = polled.get("result") if isinstance(polled.get("result"), dict) else polled
                output["final_response"] = final_payload
                if isinstance(final_payload, dict):
                    output["results"] = final_payload.get("results") if isinstance(final_payload.get("results"), list) else output["results"]
                    output["warnings"] = (
                        final_payload.get("warnings")
                        if isinstance(final_payload.get("warnings"), list)
                        else output["warnings"]
                    )
                    output["lifecycle"] = self._lifecycle_summary(final_payload)
                return output
            time.sleep(max(0.2, float(poll_interval_secs)))
        return output

    def context_pack(
        self,
        query: str,
        project: Optional[str] = None,
        topic_path: Optional[str] = None,
        retrieval_mode: str = "balanced",
        limit: int = 10,
        max_facts: int = 24,
        include_retrieval_debug: bool = True,
        agent_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Retrieve factual context pack for pre-inference grounding."""
        payload = {
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "retrieval_mode": retrieval_mode,
            "limit": int(limit),
            "max_facts": int(max_facts),
            "include_retrieval_debug": bool(include_retrieval_debug),
            "agent_id": str(agent_id or self.agent_id).strip() or DEFAULT_AGENT_ID,
            "traffic_class": "user",
        }
        resp = self.client.post(f"{self.base_url}/memory/context-pack", json=payload)
        resp.raise_for_status()
        return resp.json()

    def codex_preflight(
        self,
        project: str,
        topic_path: str = "runbooks/codex-integration",
        query: str = "codex preflight connectivity and retrieval",
    ) -> Dict[str, Any]:
        """
        Codex-first preflight:
        - health
        - status
        - scoped search (with broadened fallback)
        - context-pack retrieval
        """
        return self.agent_preflight(
            agent="codex",
            project=project,
            topic_path=topic_path,
            query=query,
            retrieval_mode="balanced",
        )

    def agent_preflight(
        self,
        agent: str,
        project: str,
        topic_path: Optional[str] = None,
        query: Optional[str] = None,
        retrieval_mode: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Preflight for a named agent profile with scoped search + context pack."""
        profile_key, profile = _resolve_agent_preflight_profile(agent)
        effective_topic_path = str(topic_path or profile.get("topic_path") or "").strip()
        effective_query = str(query or profile.get("query") or "").strip()
        effective_mode = str(retrieval_mode or profile.get("retrieval_mode") or "balanced").strip().lower() or "balanced"
        effective_agent_id = str(profile.get("agent_id") or self.agent_id).strip() or DEFAULT_AGENT_ID

        health = self.client.get(f"{self.base_url}/health")
        health.raise_for_status()
        health_json = health.json()

        status = self.client.get(f"{self.base_url}/status")
        status.raise_for_status()
        status_json = status.json()

        scoped = self.search_with_lifecycle(
            query=effective_query,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=effective_mode,
            include_grounding=True,
            include_retrieval_debug=True,
            wait_for_completion=False,
            agent_id=effective_agent_id,
        )
        broadened = None
        scoped_results = scoped.get("results") if isinstance(scoped.get("results"), list) else []
        scoped_lifecycle = scoped.get("lifecycle") if isinstance(scoped.get("lifecycle"), dict) else {}
        if not scoped_results or str(scoped_lifecycle.get("status") or "").strip().lower() in {"partial", "failed"}:
            broadened = self.search_with_lifecycle(
                query=effective_query,
                project=project,
                topic_path=None,
                retrieval_mode=effective_mode,
                include_grounding=True,
                include_retrieval_debug=True,
                wait_for_completion=False,
                agent_id=effective_agent_id,
            )

        pack = self.context_pack(
            query=effective_query,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=effective_mode,
            include_retrieval_debug=True,
            agent_id=effective_agent_id,
        )

        return {
            "ok": True,
            "agent": profile_key,
            "agent_profile": profile,
            "agent_id": effective_agent_id,
            "orchestrator_url": self.base_url,
            "health": health_json,
            "status": status_json,
            "scoped_search": scoped,
            "broadened_search": broadened,
            "context_pack": pack,
        }

    def status(self) -> Dict[str, Any]:
        """Get orchestrator + service status."""
        resp = self.client.get(f"{self.base_url}/status")
        resp.raise_for_status()
        return resp.json()


# Backwards-compatible alias for external scripts importing the old symbol.
MemMCPOrchestrator = ContextLatticeOrchestrator


class TaskCoordinator:
    """Coordinates tasks across multiple agents via ContextLattice."""

    def __init__(self, orchestrator: ContextLatticeOrchestrator, project: str):
        self.orch = orchestrator
        self.project = project

    def create_task_list(
        self, task_id: str, tasks: List[Dict[str, Any]]
    ) -> Dict[str, Any]:
        """Create a task list for agent execution."""
        timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
        file_name = f"tasks/{task_id}_{timestamp}.json"

        payload = {
            "task_id": task_id,
            "created_at": datetime.utcnow().isoformat() + "Z",
            "status": "pending",
            "tasks": tasks,
        }

        self.orch.write(self.project, file_name, json.dumps(payload, indent=2))
        return {"file": file_name, "task_id": task_id}

    def update_task_status(
        self, file_name: str, task_index: int, status: str, result: Optional[str] = None
    ) -> None:
        """Update status of a specific task."""
        content = self.orch.read(self.project, file_name)
        data = json.loads(content)

        if task_index < len(data["tasks"]):
            data["tasks"][task_index]["status"] = status
            data["tasks"][task_index]["updated_at"] = (
                datetime.utcnow().isoformat() + "Z"
            )
            if result:
                data["tasks"][task_index]["result"] = result

        # Update overall status
        all_done = all(t.get("status") == "done" for t in data["tasks"])
        any_failed = any(t.get("status") == "failed" for t in data["tasks"])

        if all_done:
            data["status"] = "completed"
        elif any_failed:
            data["status"] = "failed"
        else:
            data["status"] = "in_progress"

        self.orch.write(self.project, file_name, json.dumps(data, indent=2))

    def read_task_list(self, file_name: str) -> Dict[str, Any]:
        """Read current task list state."""
        content = self.orch.read(self.project, file_name)
        return json.loads(content)

    def log_agent_handoff(
        self,
        from_agent: str,
        to_agent: str,
        context: str,
        task_file: Optional[str] = None,
    ) -> None:
        """Log agent handoff for traceability."""
        timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
        file_name = f"briefings/handoff_{from_agent}_to_{to_agent}_{timestamp}.txt"

        content = f"""# Agent Handoff
From: {from_agent}
To: {to_agent}
Timestamp: {datetime.utcnow().isoformat()}Z

## Context
{context}
"""
        if task_file:
            content += f"\n## Task File\n{task_file}\n"

        self.orch.write(self.project, file_name, content)


def main():
    """CLI for agent orchestration."""
    if len(sys.argv) < 2:
        print("Usage: agent_orchestration.py <command> [args...]")
        print("\nCommands:")
        print("  write <project> <file> <content>")
        print("  read <project> <file>")
        print("  list <project>")
        print("  search <query> [project]")
        print("  search-lifecycle <query> [project] [mode] [wait]")
        print("  context-pack <query> [project] [mode] [topic_path]")
        print("  preflight [project] [topic_path] [query]")
        print("  preflight-agent <agent> [project] [topic_path] [query] [mode]")
        print("  status")
        print("  create-tasks <project> <task_id> <tasks_json>")
        sys.exit(1)

    orch = ContextLatticeOrchestrator()
    cmd = sys.argv[1]

    if cmd == "write":
        project, file_name, content = sys.argv[2:5]
        result = orch.write(project, file_name, content)
        print(json.dumps(result, indent=2))

    elif cmd == "read":
        project, file_name = sys.argv[2:4]
        content = orch.read(project, file_name)
        print(content)

    elif cmd == "list":
        project = sys.argv[2]
        files = orch.list_files(project)
        print(json.dumps(files, indent=2))

    elif cmd == "search":
        query = sys.argv[2]
        project = sys.argv[3] if len(sys.argv) > 3 else None
        results = orch.search(query, project=project)
        print(json.dumps(results, indent=2))

    elif cmd == "search-lifecycle":
        query = sys.argv[2]
        project = sys.argv[3] if len(sys.argv) > 3 else None
        mode = sys.argv[4] if len(sys.argv) > 4 else "balanced"
        wait_flag = str(sys.argv[5]).strip().lower() if len(sys.argv) > 5 else ""
        wait_for_completion = wait_flag in {"1", "true", "yes", "wait", "on"}
        payload = orch.search_with_lifecycle(
            query=query,
            project=project,
            retrieval_mode=mode,
            wait_for_completion=wait_for_completion,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "context-pack":
        query = sys.argv[2]
        project = sys.argv[3] if len(sys.argv) > 3 else None
        mode = sys.argv[4] if len(sys.argv) > 4 else "balanced"
        topic_path = sys.argv[5] if len(sys.argv) > 5 else None
        payload = orch.context_pack(
            query=query,
            project=project,
            topic_path=topic_path,
            retrieval_mode=mode,
            include_retrieval_debug=True,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "preflight":
        project = sys.argv[2] if len(sys.argv) > 2 else os.getenv("MEMMCP_PROJECT", "contextlattice")
        topic_path = sys.argv[3] if len(sys.argv) > 3 else "runbooks/codex-integration"
        query = sys.argv[4] if len(sys.argv) > 4 else "codex preflight connectivity and retrieval"
        payload = orch.codex_preflight(project=project, topic_path=topic_path, query=query)
        print(json.dumps(payload, indent=2))

    elif cmd == "preflight-agent":
        agent = sys.argv[2] if len(sys.argv) > 2 else "codex"
        project = sys.argv[3] if len(sys.argv) > 3 else os.getenv("MEMMCP_PROJECT", "contextlattice")
        topic_path = sys.argv[4] if len(sys.argv) > 4 else None
        query = sys.argv[5] if len(sys.argv) > 5 else None
        retrieval_mode = sys.argv[6] if len(sys.argv) > 6 else None
        payload = orch.agent_preflight(
            agent=agent,
            project=project,
            topic_path=topic_path,
            query=query,
            retrieval_mode=retrieval_mode,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "status":
        status = orch.status()
        print(json.dumps(status, indent=2))

    elif cmd == "create-tasks":
        project = sys.argv[2]
        task_id = sys.argv[3]
        tasks_json = sys.argv[4]
        tasks = json.loads(tasks_json)
        coord = TaskCoordinator(orch, project)
        result = coord.create_task_list(task_id, tasks)
        print(json.dumps(result, indent=2))

    else:
        print(f"Unknown command: {cmd}")
        sys.exit(1)


if __name__ == "__main__":
    main()
