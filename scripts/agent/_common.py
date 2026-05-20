#!/usr/bin/env python3
"""Shared helpers for lightweight agent-context audit scripts."""

from __future__ import annotations

import json
import os
import re
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


def normalize_rule(line: str) -> str:
    line = re.sub(r"`[^`]+`", "`x`", line.strip().lower())
    line = re.sub(r"\s+", " ", line)
    return line.strip(" -*#.")
