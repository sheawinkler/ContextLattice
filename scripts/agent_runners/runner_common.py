#!/usr/bin/env python3
"""Shared helpers for repo-local ContextLattice runner adapters."""

from __future__ import annotations

import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))

from agent_contracts import attach_format_contract  # noqa: E402

STDOUT_TAIL_CHARS = 4000
STDERR_TAIL_CHARS = 4000
PROMPT_CONTEXT_CHARS = 65000

SECRET_KEY_MARKERS = (
    "api_key",
    "apikey",
    "token",
    "secret",
    "password",
    "credential",
    "private_key",
    "authorization",
    "bearer",
    "signing_secret",
    "webhook_secret",
)

TOKEN_PATTERNS = (
    re.compile(r"Bearer\s+[A-Za-z0-9._~+/=-]{16,}", re.IGNORECASE),
    re.compile(r"(?<![A-Za-z0-9])sk-[A-Za-z0-9_-]{12,}"),
    re.compile(r"(?<![A-Za-z0-9])[A-Za-z0-9][A-Za-z0-9_-]{47,}(?![A-Za-z0-9])"),
)


def now_iso() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def redact_text(value: str) -> str:
    text = str(value)
    for pattern in TOKEN_PATTERNS:
        text = pattern.sub("[REDACTED]", text)
    return text


def redact_value(value: Any) -> Any:
    if isinstance(value, dict):
        out: dict[str, Any] = {}
        for key, item in value.items():
            key_text = str(key)
            if any(marker in key_text.lower() for marker in SECRET_KEY_MARKERS):
                out[key_text] = item if isinstance(item, bool) else "[REDACTED]"
            else:
                out[key_text] = redact_value(item)
        return out
    if isinstance(value, list):
        return [redact_value(item) for item in value[:128]]
    if isinstance(value, str):
        return redact_text(value)
    return value


def tail_text(value: str, limit: int) -> str:
    text = redact_text(str(value or ""))
    if len(text) <= limit:
        return text
    return text[-limit:]


def parse_task_payload() -> tuple[dict[str, Any] | None, str | None]:
    raw = os.getenv("TASK_PAYLOAD", "{}")
    try:
        parsed = json.loads(raw or "{}")
    except json.JSONDecodeError as exc:
        return None, f"invalid TASK_PAYLOAD JSON: {exc}"
    if not isinstance(parsed, dict):
        return None, "invalid TASK_PAYLOAD: expected JSON object"
    return parsed, None


def task_bool(payload: dict[str, Any], key: str, default: bool = False) -> bool:
    if key not in payload:
        return default
    value = payload.get(key)
    if isinstance(value, bool):
        return value
    return str(value or "").strip().lower() in {"1", "true", "yes", "on"}


def task_int(payload: dict[str, Any], key: str, default: int) -> int:
    try:
        value = int(payload.get(key, default))
    except Exception:
        return default
    return value if value > 0 else default


def explicit_workdir(payload: dict[str, Any], env_names: tuple[str, ...] = ()) -> tuple[str, bool]:
    for key in ("cwd", "worktree", "repo"):
        raw = str(payload.get(key) or "").strip()
        if raw:
            return str(Path(raw).expanduser().resolve(strict=False)), True
    for name in env_names:
        raw = str(os.getenv(name, "")).strip()
        if raw:
            return str(Path(raw).expanduser().resolve(strict=False)), True
    return str(Path.cwd().resolve(strict=False)), False


def mutating_requested(payload: dict[str, Any]) -> bool:
    if task_bool(payload, "write_access", False):
        return True
    fields = ("mode", "task_mode", "runner_mode", "role", "intent", "operation")
    text = " ".join(str(payload.get(field) or "") for field in fields).lower()
    markers = ("implement", "edit", "refactor", "mutate", "write", "patch", "apply")
    return any(marker in text for marker in markers)


def resolve_binary(env_names: tuple[str, ...], commands: tuple[str, ...]) -> tuple[str | None, list[str]]:
    attempted: list[str] = []
    for name in env_names:
        raw = str(os.getenv(name, "")).strip()
        if not raw:
            continue
        attempted.append(f"{name}={raw}")
        resolved = shutil.which(raw)
        if resolved:
            return resolved, attempted
        candidate = Path(raw).expanduser()
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate), attempted
    for command in commands:
        attempted.append(command)
        resolved = shutil.which(command)
        if resolved:
            return resolved, attempted
    return None, attempted


def split_args(raw: str) -> list[str]:
    raw = str(raw or "").strip()
    if not raw:
        return []
    return shlex.split(raw)


def build_argv(binary: str, raw_args: str, prompt_file: Path) -> list[str]:
    args = split_args(raw_args)
    prompt = str(prompt_file)
    if any("{prompt_file}" in item for item in args):
        return [binary] + [item.replace("{prompt_file}", prompt) for item in args]
    return [binary] + args + [prompt]


def _env_bool(name: str, default: bool) -> bool:
    raw = str(os.getenv(name, "")).strip().lower()
    if not raw:
        return default
    return raw in {"1", "true", "yes", "on"}


def build_droid_argv(binary: str, raw_args: str, prompt_file: Path, workdir: str, payload: dict[str, Any]) -> list[str]:
    args = split_args(raw_args)
    prompt = str(prompt_file)
    if any("{prompt_file}" in item or "{cwd}" in item for item in args):
        return [binary] + [item.replace("{prompt_file}", prompt).replace("{cwd}", workdir) for item in args]

    argv = [binary, "exec", "--file", prompt, "--cwd", workdir]
    output_format = str(os.getenv("DROID_OUTPUT_FORMAT", "") or payload.get("output_format") or "").strip()
    if output_format:
        argv.extend(["--output-format", output_format])
    auto_level = str(os.getenv("DROID_AUTO_LEVEL", "") or payload.get("auto_level") or "").strip()
    if auto_level:
        argv.extend(["--auto", auto_level])
    if _env_bool("DROID_USE_SPEC", bool(payload.get("use_spec") or False)):
        argv.append("--use-spec")
    argv.extend(args)
    return argv


def build_runner_argv(runner: str, binary: str, raw_args: str, prompt_file: Path, workdir: str, payload: dict[str, Any]) -> list[str]:
    if runner == "droid":
        return build_droid_argv(binary, raw_args, prompt_file, workdir, payload)
    return build_argv(binary, raw_args, prompt_file)


def safe_payload_for_prompt(payload: dict[str, Any]) -> dict[str, Any]:
    out = redact_value(payload)
    if isinstance(out, dict):
        for key in ("api_key", "apikey", "token", "secret", "password", "credential", "private_key"):
            out.pop(key, None)
    return out if isinstance(out, dict) else {}


def build_prompt_text(runner: str, payload: dict[str, Any], warnings: list[str]) -> str:
    context_prompt = redact_text(os.getenv("TASK_CONTEXT_PROMPT", ""))[:PROMPT_CONTEXT_CHARS]
    context_bundle = redact_text(os.getenv("TASK_CONTEXT_BUNDLE", ""))[:PROMPT_CONTEXT_CHARS]
    tool_slices = redact_text(os.getenv("TASK_TOOL_CONTEXT_SLICES", ""))[:PROMPT_CONTEXT_CHARS]
    prompt = {
        "runner": runner,
        "task_id": os.getenv("TASK_ID", ""),
        "title": os.getenv("TASK_TITLE", ""),
        "project": os.getenv("TASK_PROJECT", ""),
        "agent": os.getenv("TASK_AGENT", ""),
        "contextlattice_session_id": os.getenv("CONTEXTLATTICE_SESSION_ID", ""),
        "contextlattice_agent_id": os.getenv("CONTEXTLATTICE_AGENT_ID", ""),
        "payload": safe_payload_for_prompt(payload),
        "warnings": warnings,
        "safety": {
            "no_auto_merge": True,
            "no_git_push": True,
            "do_not_log_secrets": True,
            "write_access": task_bool(payload, "write_access", False),
        },
        "context_prompt": context_prompt,
        "context_bundle": context_bundle,
        "tool_context_slices": tool_slices,
    }
    return json.dumps(prompt, indent=2, sort_keys=True)


def result_payload(
    *,
    runner: str,
    agent: str,
    agent_id: str,
    status: str,
    ok: bool,
    exit_code: int,
    started_at: str,
    completed_at: str,
    duration_secs: float,
    summary: str,
    stdout_tail: str = "",
    stderr_tail: str = "",
    artifacts: list[Any] | None = None,
    warnings: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
) -> dict[str, Any]:
    payload = {
        "schema_id": "runner_result.v1",
        "ok": bool(ok),
        "runner": runner,
        "agent": agent,
        "agent_id": agent_id,
        "task_id": str(os.getenv("TASK_ID", "")),
        "project": str(os.getenv("TASK_PROJECT", "")),
        "status": status,
        "exit_code": int(exit_code),
        "started_at": started_at,
        "completed_at": completed_at,
        "duration_secs": round(float(duration_secs), 3),
        "summary": redact_text(summary)[:3000],
        "stdout_tail": tail_text(stdout_tail, STDOUT_TAIL_CHARS),
        "stderr_tail": tail_text(stderr_tail, STDERR_TAIL_CHARS),
        "artifacts": redact_value(artifacts or []),
        "warnings": [redact_text(str(item))[:1000] for item in (warnings or [])[:16]],
        "metadata": redact_value(metadata or {}),
    }
    return attach_format_contract("runner_result.v1", payload)


def classify_process_status(returncode: int, stdout: str, stderr: str) -> str:
    if returncode == 0:
        return "succeeded"
    combined = f"{stdout}\n{stderr}".lower()
    if "authentication failed" in combined or "please log in" in combined or "factory_api_key" in combined:
        return "blocked"
    return "failed"


def emit_result(payload: dict[str, Any], exit_code: int) -> int:
    print(json.dumps(payload, sort_keys=True))
    return int(exit_code)


def run_adapter(
    *,
    runner: str,
    agent: str,
    agent_id: str,
    install_hint: str,
    binary_env_names: tuple[str, ...],
    binary_commands: tuple[str, ...],
    args_env_names: tuple[str, ...],
    cwd_env_names: tuple[str, ...],
    default_timeout: int,
    capability_metadata: dict[str, Any],
) -> int:
    started = now_iso()
    start = time.monotonic()
    payload, payload_error = parse_task_payload()
    if payload is None:
        completed = now_iso()
        return emit_result(
            result_payload(
                runner=runner,
                agent=agent,
                agent_id=agent_id,
                status="invalid_task",
                ok=False,
                exit_code=2,
                started_at=started,
                completed_at=completed,
                duration_secs=time.monotonic() - start,
                summary=payload_error or "invalid TASK_PAYLOAD",
                warnings=[],
                metadata={"adapter": f"{runner}_runner", "install_hint": install_hint},
            ),
            2,
        )

    warnings: list[str] = []
    if not os.getenv("TASK_CONTEXT_PROMPT") and not os.getenv("TASK_CONTEXT_BUNDLE"):
        warnings.append("optional ContextLattice context prompt/bundle not supplied")

    workdir, workdir_explicit = explicit_workdir(payload, cwd_env_names)
    if task_bool(payload, "approval_required", False) and not task_bool(payload, "approved", False):
        completed = now_iso()
        return emit_result(
            result_payload(
                runner=runner,
                agent=agent,
                agent_id=agent_id,
                status="blocked",
                ok=False,
                exit_code=3,
                started_at=started,
                completed_at=completed,
                duration_secs=time.monotonic() - start,
                summary="approval_required is true and approved is not true",
                warnings=warnings,
                metadata={"adapter": f"{runner}_runner", "cwd": workdir, "explicit_cwd": workdir_explicit},
            ),
            3,
        )

    if mutating_requested(payload) and not workdir_explicit:
        completed = now_iso()
        return emit_result(
            result_payload(
                runner=runner,
                agent=agent,
                agent_id=agent_id,
                status="blocked",
                ok=False,
                exit_code=3,
                started_at=started,
                completed_at=completed,
                duration_secs=time.monotonic() - start,
                summary="mutating runner mode requires explicit cwd, worktree, or repo",
                warnings=warnings,
                metadata={"adapter": f"{runner}_runner", "cwd": workdir, "explicit_cwd": workdir_explicit},
            ),
            3,
        )

    binary, attempted = resolve_binary(binary_env_names, binary_commands)
    if not binary:
        completed = now_iso()
        return emit_result(
            result_payload(
                runner=runner,
                agent=agent,
                agent_id=agent_id,
                status="missing_binary",
                ok=False,
                exit_code=127,
                started_at=started,
                completed_at=completed,
                duration_secs=time.monotonic() - start,
                summary=f"{runner} binary not found; install with: {install_hint}",
                warnings=warnings,
                metadata={
                    "adapter": f"{runner}_runner",
                    "install_hint": install_hint,
                    "detection": {"attempted": attempted, "env_overrides": list(binary_env_names)},
                    "capability": capability_metadata,
                },
            ),
            127,
        )

    timeout = task_int(payload, "timeout_secs", task_int(payload, "max_runtime_secs", default_timeout))
    for env_name in args_env_names:
        raw_args = os.getenv(env_name, "")
        if raw_args:
            break
    else:
        raw_args = ""

    with tempfile.TemporaryDirectory(prefix=f"contextlattice-{runner}-") as tmp:
        prompt_file = Path(tmp) / "prompt.json"
        prompt_file.write_text(build_prompt_text(runner, payload, warnings), encoding="utf-8")
        argv = build_runner_argv(runner, binary, raw_args, prompt_file, workdir, payload)
        metadata = {
            "adapter": f"{runner}_runner",
            "argv": [Path(binary).name] + argv[1:],
            "cwd": workdir,
            "explicit_cwd": workdir_explicit,
            "timeout_secs": timeout,
            "lease": {
                "lease_id": str(payload.get("lease_id") or ""),
                "expires_at": str(payload.get("expires_at") or ""),
                "heartbeat_interval_secs": payload.get("heartbeat_interval_secs"),
                "max_runtime_secs": payload.get("max_runtime_secs"),
                "worktree": str(payload.get("worktree") or ""),
                "allowed_paths": payload.get("allowed_paths") if isinstance(payload.get("allowed_paths"), list) else [],
            },
            "agent_state": {"state": "working", "authority": "adapter", "source": f"{runner}_runner"},
            "capability": capability_metadata,
            "flags_note": f"{runner} CLI flags are operator-controlled through environment args",
        }
        env = os.environ.copy()
        env.pop("TASK_API_KEY", None)
        try:
            proc = subprocess.run(
                argv,
                cwd=workdir,
                env=env,
                text=True,
                capture_output=True,
                timeout=timeout,
                check=False,
            )
            completed = now_iso()
            status = classify_process_status(proc.returncode, proc.stdout, proc.stderr)
            metadata["agent_state"] = {
                "state": "done" if proc.returncode == 0 else "blocked",
                "authority": "adapter",
                "source": f"{runner}_runner",
            }
            if status == "blocked":
                summary = f"{runner} adapter blocked: authentication or operator action required"
            else:
                summary = f"{runner} adapter completed" if proc.returncode == 0 else f"{runner} adapter exited {proc.returncode}"
            return emit_result(
                result_payload(
                    runner=runner,
                    agent=agent,
                    agent_id=agent_id,
                    status=status,
                    ok=proc.returncode == 0,
                    exit_code=proc.returncode,
                    started_at=started,
                    completed_at=completed,
                    duration_secs=time.monotonic() - start,
                    summary=summary,
                    stdout_tail=proc.stdout,
                    stderr_tail=proc.stderr,
                    warnings=warnings,
                    metadata=metadata,
                ),
                proc.returncode,
            )
        except subprocess.TimeoutExpired as exc:
            completed = now_iso()
            metadata["agent_state"] = {"state": "blocked", "authority": "adapter", "source": f"{runner}_runner"}
            return emit_result(
                result_payload(
                    runner=runner,
                    agent=agent,
                    agent_id=agent_id,
                    status="timed_out",
                    ok=False,
                    exit_code=124,
                    started_at=started,
                    completed_at=completed,
                    duration_secs=time.monotonic() - start,
                    summary=f"{runner} adapter timed out after {timeout} seconds",
                    stdout_tail=exc.stdout or "",
                    stderr_tail=exc.stderr or "",
                    warnings=warnings,
                    metadata=metadata,
                ),
                124,
            )
        except OSError as exc:
            completed = now_iso()
            metadata["agent_state"] = {"state": "blocked", "authority": "adapter", "source": f"{runner}_runner"}
            return emit_result(
                result_payload(
                    runner=runner,
                    agent=agent,
                    agent_id=agent_id,
                    status="failed",
                    ok=False,
                    exit_code=1,
                    started_at=started,
                    completed_at=completed,
                    duration_secs=time.monotonic() - start,
                    summary=f"{runner} adapter failed to execute: {exc}",
                    warnings=warnings,
                    metadata=metadata,
                ),
                1,
            )
