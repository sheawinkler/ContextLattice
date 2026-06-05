#!/usr/bin/env python3
"""Shared helpers for lightweight agent-context audit scripts."""

from __future__ import annotations

import json
import os
import re
import hashlib
import subprocess
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[2]
HOME = Path.home()

DEFAULT_SKILL_ROOTS = [
    HOME / ".agents" / "skills",
    HOME / ".codex" / "skills",
    HOME / ".codex" / "skills_quarantine",
    HOME / ".codex-pro-2" / "skills" / ".system",
]

DEFAULT_SCAN_FILES = [
    HOME / ".codex" / "AGENTS.md",
    REPO_ROOT / "AGENTS.md",
    REPO_ROOT / "private_docs" / "AGENTS.md",
    HOME / ".codex" / "hooks.json",
    REPO_ROOT / "config" / "codex" / "hooks.json",
]


def read_text(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="replace")


def emit(payload: dict[str, Any], pretty: bool = False) -> None:
    print(json.dumps(payload, indent=2 if pretty else None, sort_keys=pretty))


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def skill_files(roots: list[Path] | None = None) -> list[Path]:
    out: list[Path] = []
    for root in roots or DEFAULT_SKILL_ROOTS:
        if root.exists():
            out.extend(sorted(root.rglob("SKILL.md")))
    return out


def parse_frontmatter(text: str) -> tuple[dict[str, str], str]:
    if not text.startswith("---\n"):
        return {}, text
    end = text.find("\n---", 4)
    if end < 0:
        return {}, text
    body = text[end + 4 :].lstrip("\n")
    meta: dict[str, str] = {}
    for line in text[4:end].splitlines():
        if ":" not in line:
            continue
        key, value = line.split(":", 1)
        value = value.strip().strip('"').strip("'")
        meta[key.strip()] = value
    return meta, body


def approx_tokens(chars: int) -> int:
    return max(1, chars // 4)


def load_env_file(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    if not path.exists():
        return values
    for raw in read_text(path).splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip('"').strip("'")
    return values


def contextlattice_base_url() -> str:
    return (
        os.getenv("CONTEXTLATTICE_ORCHESTRATOR_URL")
        or os.getenv("MEMMCP_ORCHESTRATOR_URL")
        or "http://127.0.0.1:8075"
    ).rstrip("/")


def contextlattice_api_key() -> str:
    key = os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "").strip()
    if key:
        return key
    env = load_env_file(REPO_ROOT / ".env")
    return env.get("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "").strip()


def request_json(method: str, path: str, payload: dict[str, Any] | None, timeout: float) -> dict[str, Any]:
    url = path if path.startswith(("http://", "https://")) else contextlattice_base_url() + path
    data = json.dumps(payload or {}).encode("utf-8") if payload is not None else None
    headers = {"content-type": "application/json"}
    key = contextlattice_api_key()
    if key:
        headers["x-api-key"] = key
    req = urllib.request.Request(url, data=data, method=method.upper(), headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="replace")
        raise SystemExit(json.dumps({"ok": False, "status": exc.code, "error": raw[:2000]}))
    return json.loads(raw) if raw.strip() else {}


def git_value(args: list[str]) -> str:
    try:
        return subprocess.check_output(args, cwd=str(REPO_ROOT), stderr=subprocess.DEVNULL, text=True, timeout=2).strip()
    except Exception:
        return ""


def default_agent_session_state_path(project: str) -> Path:
    repo = git_value(["git", "config", "--get", "remote.origin.url"]) or str(REPO_ROOT)
    digest = hashlib.sha256(f"{project}|{repo}|{REPO_ROOT}".encode("utf-8")).hexdigest()[:16]
    return HOME / ".contextlattice" / "agent_runtime_sessions" / f"{digest}.json"


def agent_session_state_path(project: str) -> Path:
    override = os.getenv("CONTEXTLATTICE_SESSION_STATE_PATH", "").strip()
    if override:
        return Path(override).expanduser()
    return default_agent_session_state_path(project or "contextlattice")


def read_agent_session_state(project: str) -> dict[str, Any]:
    path = agent_session_state_path(project)
    try:
        parsed = json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return {}
    return parsed if isinstance(parsed, dict) else {}


def write_agent_session_state(project: str, payload: dict[str, Any]) -> None:
    path = agent_session_state_path(project)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    body = dict(payload)
    body["updated_at"] = now_iso()
    tmp.write_text(json.dumps(body, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    tmp.replace(path)


def auto_session_disabled() -> bool:
    return os.getenv("CONTEXTLATTICE_AUTO_SESSION_DISABLED", "").strip().lower() in {"1", "true", "yes", "on"}


def session_is_reusable(session: dict[str, Any]) -> bool:
    status = str(session.get("status") or "").strip().lower()
    return status in {"active", "paused", "running", "started", ""}


def ensure_agent_session(
    *,
    project: str,
    objective: str,
    agent_id: str = "",
    agent: str = "",
    mission: str = "",
    goal: str = "",
    metadata: dict[str, Any] | None = None,
    tags: list[str] | None = None,
    timeout: float = 5.0,
    soft: bool = True,
) -> dict[str, Any]:
    if auto_session_disabled():
        return {"ok": False, "disabled": True, "session_id": ""}
    project = project or os.getenv("CONTEXTLATTICE_PROJECT", "contextlattice")
    agent_id = agent_id or os.getenv("CONTEXTLATTICE_AGENT_ID", "") or os.getenv("MEMMCP_AGENT_ID", "")
    agent = agent or os.getenv("CONTEXTLATTICE_AGENT", "")
    env_session = os.getenv("CONTEXTLATTICE_SESSION_ID", "").strip()
    if env_session:
        return {"ok": True, "session_id": env_session, "source": "env"}

    state = read_agent_session_state(project)
    state_session_id = str(state.get("session_id") or "").strip()
    if state_session_id:
        try:
            raw = request_json("GET", f"/v1/agents/sessions/{state_session_id}", None, timeout)
            session = raw.get("session") if isinstance(raw.get("session"), dict) else {}
            if session and session_is_reusable(session):
                return {"ok": True, "session_id": state_session_id, "source": "state", "session": session}
        except Exception:
            if soft:
                return {"ok": True, "session_id": state_session_id, "source": "state_unverified"}
            raise

    payload: dict[str, Any] = {
        "agent": agent,
        "agent_id": agent_id,
        "project": project,
        "objective": objective or os.getenv("CONTEXTLATTICE_OBJECTIVE", "") or "ContextLattice agent runtime objective",
        "mission": mission or os.getenv("CONTEXTLATTICE_MISSION", ""),
        "goal": goal or os.getenv("CONTEXTLATTICE_GOAL", ""),
        "repo": git_value(["git", "config", "--get", "remote.origin.url"]),
        "branch": git_value(["git", "branch", "--show-current"]),
        "cwd": str(REPO_ROOT),
        "tags": tags or ["auto-session"],
        "metadata": metadata or {},
    }
    payload = {key: value for key, value in payload.items() if value not in ("", None, {}, [])}
    try:
        raw = request_json("POST", "/v1/agents/sessions/start", payload, timeout)
    except Exception as exc:
        if soft:
            return {"ok": False, "session_id": "", "error": str(exc)[:500]}
        raise
    session = raw.get("session") if isinstance(raw.get("session"), dict) else {}
    session_id = str(session.get("id") or "").strip()
    if session_id:
        write_agent_session_state(
            project,
            {
                "session_id": session_id,
                "project": project,
                "agent": agent,
                "agent_id": agent_id,
                "objective": payload.get("objective", ""),
                "source": "auto-session",
            },
        )
    return {"ok": bool(raw.get("ok")) and bool(session_id), "session_id": session_id, "source": "created", "session": session}


def normalize_rule(line: str) -> str:
    line = re.sub(r"`[^`]+`", "`x`", line.strip().lower())
    line = re.sub(r"\s+", " ", line)
    return line.strip(" -*#.")
