#!/usr/bin/env python3
"""ContextLattice adapter for optional Droid CLI runner execution."""

from __future__ import annotations

from runner_common import run_adapter


def main() -> int:
    return run_adapter(
        runner="droid",
        agent="droid",
        agent_id="droid_agent",
        install_hint="brew install --cask droid",
        binary_env_names=("DROID_BIN",),
        binary_commands=("droid",),
        args_env_names=("DROID_ARGS",),
        cwd_env_names=("DROID_CWD", "DROID_WORKTREE"),
        default_timeout=900,
        capability_metadata={
            "schema_id": "runner_capability.v1",
            "runner": "droid",
            "agent": "droid",
            "agent_id": "droid_agent",
            "roles": ["scout", "reviewer", "implementer"],
            "surfaces": ["cli", "json", "brew-cask"],
            "install": {"brew": "", "brew_cask": "droid", "hint": "brew install --cask droid"},
            "detection": {"commands": ["droid"], "env_overrides": ["DROID_BIN"]},
            "execution": {
                "adapter": "scripts/agent_runners/droid_runner.py",
                "supports_worktree": True,
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
            "max_parallel": 1,
        },
    )


if __name__ == "__main__":
    raise SystemExit(main())
