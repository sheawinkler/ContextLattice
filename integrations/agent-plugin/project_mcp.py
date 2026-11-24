#!/usr/bin/env python3
"""Print invocation-local native MCP configuration. Never edit a host profile."""

import argparse
import json
import re


def bound_args(project):
    if not project or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", project):
        raise ValueError("supply an explicit project identifier (letters, digits, dot, dash, underscore)")
    return ["mcp", "--project", project]


def config(host, project):
    args = bound_args(project)
    if host == "claude":
        return json.dumps({"mcpServers": {"contextlattice": {"type": "stdio", "command": "contextlattice",
                                                             "args": args}}})
    # Forward only the names needed by the trusted local CLI; no secret values
    # or endpoint appear in the invocation or in a project file.
    names = ["CONTEXTLATTICE_ORCHESTRATOR_URL", "CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "CONTEXTLATTICE_API_KEY"]
    return ('mcp_servers.contextlattice={command="contextlattice",args=' + json.dumps(args)
            + ",env_vars=" + json.dumps(names) + "}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", choices=("codex", "claude"))
    parser.add_argument("--project", required=True)
    args = parser.parse_args()
    try:
        print(config(args.host, args.project))
    except ValueError as exc:
        parser.exit(2, str(exc) + "\n")
