"""Importable helpers shared by hook wiring/trust audits."""

from __future__ import annotations

import hashlib
import json
from typing import Any


EVENT_LABELS = {
    "SessionStart": "session_start",
    "PreCompact": "pre_compact",
    "PostCompact": "post_compact",
}


def hook_hash(event: str, matcher: str | None, hook: dict[str, Any]) -> str:
    normalized = {
        "type": "command",
        "command": str(hook.get("command") or ""),
        "timeout": max(1, int(hook.get("timeout") or 600)),
        "async": bool(hook.get("async", False)),
    }
    if hook.get("statusMessage") is not None:
        normalized["statusMessage"] = str(hook.get("statusMessage"))
    identity: dict[str, Any] = {"event_name": EVENT_LABELS[event], "hooks": [normalized]}
    if matcher is not None:
        identity["matcher"] = matcher
    raw = json.dumps(identity, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def command_hooks_for(payload: dict[str, Any], event: str) -> list[tuple[int, int, str | None, dict[str, Any]]]:
    out = []
    for group_index, group in enumerate((payload.get("hooks") or {}).get(event) or []):
        if not isinstance(group, dict):
            continue
        matcher = group.get("matcher")
        matcher = str(matcher) if matcher is not None else None
        for hook_index, hook in enumerate(group.get("hooks") or []):
            if isinstance(hook, dict) and hook.get("type") == "command":
                out.append((group_index, hook_index, matcher, hook))
    return out
