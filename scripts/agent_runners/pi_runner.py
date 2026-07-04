#!/usr/bin/env python3
"""ContextLattice adapter for optional Pi CLI runner execution."""

from __future__ import annotations

from runner_common import run_adapter


def main() -> int:
    return run_adapter(
        runner="pi",
        agent="pi",
        agent_id="pi_agent",
        install_hint="brew install pi-coding-agent",
        binary_env_names=("PI_CODING_AGENT_BIN", "PI_BIN"),
        binary_commands=("pi",),
        args_env_names=("PI_CODING_AGENT_ARGS", "PI_ARGS"),
        cwd_env_names=("PI_CODING_AGENT_CWD",),
        default_timeout=600,
        capability_metadata={
            "schema_id": "runner_capability.v1",
            "runner": "pi",
            "agent": "pi",
            "agent_id": "pi_agent",
            "roles": ["scout", "summarizer", "reviewer", "light_refactor"],
            "surfaces": ["cli", "json", "brew"],
            "install": {"brew": "pi-coding-agent", "brew_cask": "", "hint": "brew install pi-coding-agent"},
            "detection": {"commands": ["pi"], "env_overrides": ["PI_CODING_AGENT_BIN", "PI_BIN"]},
            "execution": {
                "adapter": "scripts/agent_runners/pi_runner.py",
                "supports_worktree": False,
                "supports_json_output": True,
                "requires_explicit_cwd": True,
            },
            "safety": {
                "default_write_access": False,
                "requires_sandbox": True,
                "no_auto_merge": True,
                "no_git_push": True,
                "redact_secrets": True,
            },
            "max_parallel": 3,
        },
    )


if __name__ == "__main__":
    raise SystemExit(main())
