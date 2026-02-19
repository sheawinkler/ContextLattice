#!/usr/bin/env python3
"""
Monitor Opus (or any executor agent) progress via memMCP.
Polls task status and recent decisions to track implementation.
"""

import json
import sys
import time
from datetime import datetime
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from agent_orchestration import MemMCPOrchestrator, TaskCoordinator


def print_status(status_data: dict) -> None:
    """Pretty print task status."""
    print(f"\n{'='*80}")
    print(f"Task: {status_data['task_id']}")
    print(f"Status: {status_data['status']}")
    print(f"Created: {status_data['created_at']}")
    print(f"{'='*80}\n")

    tasks = status_data.get("tasks", [])
    total = len(tasks)
    done = sum(1 for t in tasks if t.get("status") == "done")
    in_progress = sum(1 for t in tasks if t.get("status") == "in_progress")
    blocked = sum(1 for t in tasks if t.get("status") == "blocked")
    pending = total - done - in_progress - blocked

    print(f"Progress: {done}/{total} done, {in_progress} in progress, {blocked} blocked, {pending} pending\n")

    for task in tasks:
        status_icon = {
            "done": "✅",
            "in_progress": "🔄",
            "blocked": "🚫",
            "pending": "⏳",
        }.get(task.get("status", "pending"), "❓")

        print(f"{status_icon} [{task['id']}] {task['title']}")
        if task.get("status") in ("done", "in_progress"):
            updated = task.get("updated_at", "")
            print(f"   Updated: {updated}")
        if task.get("result"):
            result = task["result"][:100] + "..." if len(task["result"]) > 100 else task["result"]
            print(f"   Result: {result}")
        print()


def monitor_loop(
    project: str,
    task_file: str,
    interval: int = 30,
    search_recent: bool = True
) -> None:
    """
    Continuously monitor task progress.
    
    Args:
        project: Project name
        task_file: Path to task JSON file
        interval: Poll interval in seconds
        search_recent: Whether to search for recent decisions
    """
    orch = MemMCPOrchestrator()
    coord = TaskCoordinator(orch, project)

    print(f"Monitoring {project}/{task_file}")
    print(f"Poll interval: {interval}s")
    print(f"Press Ctrl+C to stop\n")

    try:
        while True:
            try:
                # Read current task status
                status_data = coord.read_task_list(task_file)
                print_status(status_data)

                # Search for recent decisions if enabled
                if search_recent:
                    print("Recent decisions:")
                    results = orch.search(
                        "metadata standardization implementation progress",
                        project=project,
                        limit=3
                    )
                    for i, result in enumerate(results, 1):
                        print(f"{i}. {result['file']} (score: {result['score']:.3f})")
                        summary = result.get("summary", "")[:150]
                        print(f"   {summary}...\n")

                # Check if complete
                if status_data["status"] in ("completed", "failed"):
                    print(f"\n🎉 Task {status_data['status'].upper()}!")
                    break

                print(f"\nNext update in {interval}s...")
                time.sleep(interval)

            except KeyboardInterrupt:
                print("\n\nMonitoring stopped by user.")
                break
            except Exception as exc:
                print(f"\n⚠️  Error: {exc}")
                print(f"Retrying in {interval}s...")
                time.sleep(interval)

    except KeyboardInterrupt:
        print("\n\nMonitoring stopped by user.")


def main():
    """CLI entry point."""
    if len(sys.argv) < 3:
        print("Usage: monitor_opus.py <project> <task_file> [interval_seconds]")
        print("\nExample:")
        print("  python3 monitor_opus.py algotraderv2_rust tasks/metadata_standardization_20251217_004057.json 30")
        sys.exit(1)

    project = sys.argv[1]
    task_file = sys.argv[2]
    interval = int(sys.argv[3]) if len(sys.argv) > 3 else 30

    monitor_loop(project, task_file, interval)


if __name__ == "__main__":
    main()
