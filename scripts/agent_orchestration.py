#!/usr/bin/env python3
"""
Agent orchestration helper for memMCP.
Enables multi-agent coordination through shared memory + task tracking.
"""

import json
import os
import sys
from datetime import datetime
from typing import Any, Dict, List, Optional
from urllib.parse import quote

import httpx

MEMMCP_ORCHESTRATOR_URL = os.getenv(
    "MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075"
)


class MemMCPOrchestrator:
    """Helper for agent coordination via memMCP."""

    def __init__(self, orchestrator_url: str = MEMMCP_ORCHESTRATOR_URL):
        self.base_url = orchestrator_url.rstrip("/")
        self.client = httpx.Client(timeout=30.0)

    def _encode_project_path(self, project: str, file_name: str | None = None) -> str:
        encoded_project = quote(project, safe="")
        if not file_name:
            return encoded_project
        cleaned = file_name.lstrip("/")
        parts = [quote(part, safe="") for part in cleaned.split("/") if part]
        return f"{encoded_project}/{'/'.join(parts)}" if parts else encoded_project

    def write(self, project: str, file_name: str, content: str) -> Dict[str, Any]:
        """Write a file to memMCP."""
        resp = self.client.post(
            f"{self.base_url}/memory/write",
            json={
                "projectName": project,
                "fileName": file_name,
                "content": content,
            },
        )
        resp.raise_for_status()
        return resp.json()

    def read(self, project: str, file_name: str) -> str:
        """Read a file from memMCP."""
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
        resp = self.client.post(
            f"{self.base_url}/memory/search",
            json={
                "query": query,
                "project": project,
                "limit": limit,
                "fetch_content": fetch_content,
            },
        )
        resp.raise_for_status()
        data = resp.json()
        return data.get("results", [])

    def status(self) -> Dict[str, Any]:
        """Get orchestrator + service status."""
        resp = self.client.get(f"{self.base_url}/status")
        resp.raise_for_status()
        return resp.json()


class TaskCoordinator:
    """Coordinates tasks across multiple agents via memMCP."""

    def __init__(self, orchestrator: MemMCPOrchestrator, project: str):
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
        print("  status")
        print("  create-tasks <project> <task_id> <tasks_json>")
        sys.exit(1)

    orch = MemMCPOrchestrator()
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
