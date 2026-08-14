#!/usr/bin/env python3
"""Shared helpers for lightweight agent-context audit scripts."""

from __future__ import annotations

import collections.abc
import json
import hashlib
import os
import re
import secrets
import subprocess
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import quote, unquote_plus


REPO_ROOT = Path(__file__).resolve().parents[2]
HOME = Path.home()

# This installed helper owns the canonical public-output boundary.  The task
# worker imports these functions too, so minimal installed CLIs and execution
# receipts cannot drift onto different redaction policies.

_SECRET_KEY_EXACT = {
    "api",
    "auth",
    "authorization",
    "bearer",
    "cookie",
    "credential",
    "credentials",
    "key",
    "password",
    "passwd",
    "pem",
    "proxyauthorization",
    "pwd",
    "secret",
    "session",
    "sessionid",
    "setcookie",
    "sig",
    "signature",
    "token",
    "userinfo",
}
_SECRET_KEY_SUFFIXES = (
    "api",
    "accesstoken",
    "auth",
    "authdata",
    "authheader",
    "authvalue",
    "apikey",
    "apitoken",
    "apisecret",
    "authtoken",
    "clientsecret",
    "credential",
    "credentials",
    "password",
    "passwd",
    "privatekey",
    "refreshtoken",
    "secretkey",
    "sessionid",
    "token",
    "userinfo",
)
_SECRET_KEY_FRAGMENTS = (
    "apikey",
    "apitoken",
    "apisecret",
    "authorization",
    "clientsecret",
    "cookie",
    "credential",
    "key",
    "password",
    "passwd",
    "pem",
    "privatekey",
    "refreshtoken",
    "secret",
    "secretkey",
    "session",
    "sig",
    "userinfo",
)
_SECRET_KEY_SEGMENTS = frozenset(
    {
        "api",
        "auth",
        "authorization",
        "bearer",
        "cookie",
        "credential",
        "credentials",
        "key",
        "password",
        "passwd",
        "pem",
        "proxyauthorization",
        "pwd",
        "secret",
        "session",
        "sessionid",
        "setcookie",
        "sig",
        "signature",
        "userinfo",
    }
)
_PUBLIC_REFERENCE_KEYS = {
    "artifactdigest",
    "artifactdigests",
    "authorizationdigest",
    "authorizationid",
    "canonicaldigest",
    "cleanupid",
    "containerref",
    "contentdigest",
    "contextpackhash",
    "diffdigest",
    "digest",
    "evidencedigest",
    "orphanref",
    "publicationid",
    "receiptdigest",
    "receipthash",
    "recorddigest",
    "redactionreceipt",
    "receiptid",
    "resultid",
    "sessionid",
    "snapshotid",
    "workspaceref",
}
_SECRET_VALUE_PATTERNS = (
    re.compile(r"\b(?:Bearer|Basic)\s+[^\s,;]+", re.IGNORECASE),
    re.compile(r"(?<![A-Za-z0-9])(?:sk|pk|rk)-[A-Za-z0-9_-]{4,}", re.IGNORECASE),
    re.compile(r"(?<![A-Za-z0-9])[A-Za-z0-9][A-Za-z0-9_-]{47,}(?![A-Za-z0-9])"),
    re.compile(r"(?i)(https?://[^\s/:@]+:)[^\s/@]+(@)"),
)
_PEM_BLOCK_RE = re.compile(
    r"-----BEGIN [A-Z0-9 ]*(?:PRIVATE KEY|OPENSSH PRIVATE KEY|CERTIFICATE)-----.*?"
    r"-----END [A-Z0-9 ]*(?:PRIVATE KEY|OPENSSH PRIVATE KEY|CERTIFICATE)-----",
    re.IGNORECASE | re.DOTALL,
)
_STRUCTURED_SECRET_RE = re.compile(
    r"(?im)(?P<prefix>(?:^|[,{;\s])(?:export\s+)?[\"']?"
    r"(?P<key>[A-Za-z][A-Za-z0-9_.-]{0,127})[\"']?\s*[:=]\s*)"
    r"(?P<value>(?:Bearer|Basic)\s+[^\s,;}\]\r\n]+|"
    r"\"(?:[^\"\\]|\\.)*\"|'(?:[^'\\]|\\.)*'|[^\s,;{}\[\]\r\n]+)"
)
_HEADER_SECRET_RE = re.compile(
    r"(?im)(?P<prefix>^\s*(?:authorization|proxy-authorization|x-api-key|cookie|set-cookie)\s*:\s*)"
    r"(?P<value>[^\r\n]*)"
)
_URL_QUERY_SECRET_RE = re.compile(
    r"(?i)(?P<delimiter>[?&])(?P<key>[^=&#\s\"'<>]{1,128})(?P<equals>=)"
    r"(?P<value>[^&#\s\"'<>]*)"
)
_URL_USERINFO_RE = re.compile(
    r"(?i)(?P<prefix>[A-Za-z][A-Za-z0-9+.-]*://)(?P<userinfo>[^/@\s]+)@"
)
_URL_ENCODED_RUN_RE = re.compile(
    r"(?P<encoded>(?:%[0-9a-f]{2}|[A-Za-z0-9._~_-]){4,})",
    re.IGNORECASE,
)
_FILE_URL_RE = re.compile(r"(?i)\bfile:(?://)?(?:/[^\r\n\"'<>]*|[A-Z]:[\\/][^\r\n\"'<>]*)")
_WINDOWS_ABSOLUTE_PATH_RE = re.compile(
    r"(?i)(?<![A-Za-z0-9])(?:[A-Z]:[\\/]|\\\\)[^\r\n\"'<>]*"
)
_LOCAL_ABSOLUTE_PATH_RE = re.compile(r"(?<![A-Za-z0-9/])/(?!/)[A-Za-z0-9._~+-][^\r\n\"'<>]*")
_OPAQUE_PUBLIC_REF_RE = re.compile(
    r"(?<![A-Za-z0-9._-])(?:sha256:[0-9a-f]{64}|[0-9a-f]{40,64}|"
    r"(?:artifact|authorization|cleanup|container|orphan|publication|receipt|result|snapshot|workspace)-"
    r"[A-Za-z0-9._:@-]{1,255})(?![A-Za-z0-9._-])"
)
_PUBLIC_PATH_SENTINEL_RE = re.compile(r"(?<![A-Za-z0-9/])/dev/null(?![A-Za-z0-9/])")
_PUBLIC_CONTINUATION_ROUTE_RE = re.compile(
    r"/memory/search/continuations/[A-Za-z0-9][A-Za-z0-9._:@-]{0,159}(?:/events)?"
)
_PUBLIC_PRODUCT_ROUTES = {
    "preflightroute": "/v1/agents/preflight",
    "eventroute": "/v1/agents/sessions/event",
    "contextpackroute": "/memory/context-pack",
    "contextpackoutcomeroute": "/telemetry/context-pack-quality/outcome",
    "checkpointroute": "/memory/write",
}
_CONTROL_RE = re.compile(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]")


def _is_secret_key(value: Any) -> bool:
    raw = str(value or "")
    normalized = re.sub(r"[^a-z0-9]", "", raw.lower())
    if not normalized:
        return False
    segmented = re.sub(r"(?<=[a-z0-9])(?=[A-Z])", "_", raw).lower()
    segments = {part for part in re.split(r"[^a-z0-9]+", segmented) if part}
    return (
        normalized in _SECRET_KEY_EXACT
        or bool(segments & _SECRET_KEY_SEGMENTS)
        or any(normalized.endswith(suffix) for suffix in _SECRET_KEY_SUFFIXES)
        or any(fragment in normalized for fragment in _SECRET_KEY_FRAGMENTS)
    )


def _is_safe_policy_marker(key: Any, value: Any) -> bool:
    """Keep non-secret deny-state markers in immutable boundary evidence."""

    normalized_key = re.sub(r"[^a-z0-9]", "", str(key or "").lower())
    raw = str(value or "").strip().strip("\"'").lower()
    return (
        normalized_key.startswith("host")
        and raw in {"blocked", "none", "disabled", "false", "true", "0", "[]", "{}"}
    )


def _is_public_reference_value(key: Any, value: Any) -> bool:
    raw_key = str(key or "")
    normalized = re.sub(r"[^a-z0-9]", "", raw_key.lower())
    # Only the canonical snake-case identity field may carry a public session
    # reference. Aliases such as sessionId are unbound duplicate authorities
    # and must flow through the sensitive-key boundary instead.
    if normalized == "sessionid" and raw_key != "session_id":
        return False
    if normalized == "artifactdigests":
        return (
            isinstance(value, collections.abc.Sequence)
            and not isinstance(value, (str, bytes, bytearray, memoryview))
            and len(value) <= 128
            and all(re.fullmatch(r"(?:sha256:)?[0-9a-f]{40,64}", str(item).strip()) for item in value)
        )
    raw = str(value or "").strip()
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "\"'":
        raw = raw[1:-1]
    if normalized in _PUBLIC_REFERENCE_KEYS and (
        normalized.endswith("digest")
        or normalized in {"contextpackhash", "receipthash", "redactionreceipt"}
    ):
        return bool(re.fullmatch(r"(?:sha256:)?[0-9a-f]{40,64}", raw))
    if normalized in {"basesha", "finalhead", "finaltree", "verifiedtree"}:
        return bool(re.fullmatch(r"[0-9a-f]{40,64}", raw))
    if normalized not in _PUBLIC_REFERENCE_KEYS:
        return False
    prefixes = {
        "authorizationid": "authorization-",
        "cleanupid": "cleanup-",
        "containerref": "container-",
        "orphanref": "orphan-",
        "publicationid": "publication-",
        "resultid": "result-",
        "sessionid": "session-",
        "snapshotid": "snapshot-",
        "workspaceref": "workspace-",
    }
    if normalized == "receiptid":
        return bool(
            re.fullmatch(r"(?:receipt|cleanup-receipt)-[A-Za-z0-9._:@-]{1,255}", raw)
        )
    prefix = prefixes.get(normalized)
    return bool(prefix and re.fullmatch(re.escape(prefix) + r"[A-Za-z0-9._:@-]{1,255}", raw))


def _is_public_route_value(key: Any, value: Any) -> bool:
    """Preserve only closed product routes in their structured fields."""

    if not isinstance(value, str):
        return False
    try:
        if len(value.encode("utf-8")) > 240:
            return False
    except UnicodeEncodeError:
        return False
    normalized = re.sub(r"[^a-z0-9]", "", str(key or "").lower())
    if normalized in _PUBLIC_PRODUCT_ROUTES:
        return value == _PUBLIC_PRODUCT_ROUTES[normalized]
    if normalized in {"pollurl", "eventsurl", "agentfollowupendpoint"}:
        return _PUBLIC_CONTINUATION_ROUTE_RE.fullmatch(value) is not None
    return normalized == "endpoint" and value == "/telemetry/context-pack-quality/outcome"


def _sanitize_public_text(value: Any, *, _depth: int = 0) -> tuple[str, int]:
    """Return canonical public-safe text and the number of redactions made."""

    if isinstance(value, (bytes, bytearray, memoryview)):
        text = bytes(value).decode("utf-8", errors="replace")
    else:
        text = str(value or "")
    text = _CONTROL_RE.sub("", text)
    findings = 0
    protected: dict[str, str] = {}

    def protect_value(original: str) -> str:
        placeholder = f"@@CL:{secrets.token_hex(16)}:{len(protected)}@@"
        while placeholder in text or placeholder in protected:
            placeholder = f"@@CL:{secrets.token_hex(16)}:{len(protected)}@@"
        protected[placeholder] = original
        return placeholder

    def protect_public_sentinel(match: re.Match[str]) -> str:
        return protect_value(match.group(0))

    text = _PUBLIC_PATH_SENTINEL_RE.sub(protect_public_sentinel, text)
    text, count = _PEM_BLOCK_RE.subn("[REDACTED_PEM]", text)
    findings += count

    def redact_header(_match: re.Match[str]) -> str:
        nonlocal findings
        findings += 1
        return "[REDACTED_HEADER]"

    def redact_structured(match: re.Match[str]) -> str:
        nonlocal findings
        if _is_public_reference_value(match.group("key"), match.group("value")):
            return match.group("prefix") + protect_value(match.group("value"))
        if not _is_secret_key(match.group("key")) or _is_safe_policy_marker(match.group("key"), match.group("value")):
            return match.group(0)
        findings += 1
        prefix = match.group("prefix")
        leading = prefix[0] if prefix and prefix[0] in " \t\r\n,{;" else ""
        return leading + "[REDACTED_FIELD]"

    def redact_query(match: re.Match[str]) -> str:
        nonlocal findings
        key = unquote_plus(match.group("key"))
        value = match.group("value")
        decoded = unquote_plus(value)
        if _is_public_reference_value(key, decoded):
            return match.group(0)
        if _is_secret_key(key):
            findings += 1
            return f"{match.group('delimiter')}{match.group('key')}{match.group('equals')}[REDACTED]"
        if decoded != value and _depth < 3:
            nested, nested_findings = _sanitize_public_text(decoded, _depth=_depth + 1)
            if nested_findings or nested != decoded:
                findings += max(1, nested_findings)
                return (
                    f"{match.group('delimiter')}{match.group('key')}{match.group('equals')}"
                    f"{quote(nested, safe='')}"
                )
        return match.group(0)

    def redact_url_userinfo(match: re.Match[str]) -> str:
        nonlocal findings
        findings += 1
        return f"{match.group('prefix')}[REDACTED_USERINFO]@"

    def redact_encoded(match: re.Match[str]) -> str:
        nonlocal findings
        if _depth >= 3:
            return match.group(0)
        encoded = match.group("encoded")
        decoded = unquote_plus(encoded)
        if decoded == encoded:
            return encoded
        nested, nested_findings = _sanitize_public_text(decoded, _depth=_depth + 1)
        if nested_findings or nested != decoded:
            findings += max(1, nested_findings)
            return quote(nested, safe="")
        return encoded

    text = _HEADER_SECRET_RE.sub(redact_header, text)
    text = _STRUCTURED_SECRET_RE.sub(redact_structured, text)
    text = _URL_USERINFO_RE.sub(redact_url_userinfo, text)
    text = _URL_QUERY_SECRET_RE.sub(redact_query, text)
    text = _URL_ENCODED_RUN_RE.sub(redact_encoded, text)
    # Opaque values in free text have no field-level provenance.  Preserve
    # them only when _redact_value has first validated a trusted structured
    # field; raw text is deny-by-default.
    text, count = _OPAQUE_PUBLIC_REF_RE.subn("[REDACTED_TOKEN]", text)
    findings += count
    for pattern in _SECRET_VALUE_PATTERNS:
        if pattern.pattern.startswith("(?i)(https?"):
            text, count = pattern.subn(r"\1[REDACTED]\2", text)
        else:
            text, count = pattern.subn("[REDACTED]", text)
        findings += count
    # Path expressions intentionally consume arbitrary trailing characters.
    # Run every content-aware secret scanner first so a path prefix cannot
    # erase the opening delimiter while leaving the secret tail public.
    configured_root = str(os.getenv("CONTEXTLATTICE_TASK_WORKTREE_ROOT") or "").strip()
    local_paths = [str(Path.home().resolve(strict=False)), configured_root]
    for local_path in sorted({item for item in local_paths if len(item) > 1}, key=len, reverse=True):
        count = text.count(local_path)
        if count:
            text = text.replace(local_path, "[LOCAL_PATH]")
            findings += count
    text, count = _FILE_URL_RE.subn("[LOCAL_PATH]", text)
    findings += count
    text, count = _WINDOWS_ABSOLUTE_PATH_RE.subn("[LOCAL_PATH]", text)
    findings += count
    text, count = _LOCAL_ABSOLUTE_PATH_RE.subn("[LOCAL_PATH]", text)
    findings += count
    for placeholder, original in protected.items():
        text = text.replace(placeholder, original)
    return text, findings


def _contains_sensitive_text(value: str | bytes) -> bool:
    if isinstance(value, (bytes, bytearray, memoryview)):
        text = bytes(value).decode("utf-8", errors="replace")
    else:
        text = str(value or "")
    stripped = text.strip()
    if stripped.startswith(("{", "[")):
        try:
            decoded = json.loads(stripped)
        except (TypeError, json.JSONDecodeError):
            decoded = None
        if isinstance(decoded, (Mapping, list)):
            return _redact_value(decoded) != decoded
    sanitized, _findings = _sanitize_public_text(text)
    # Redaction is intentionally idempotent.  A second scan may still count a
    # preserved query key as a finding (for example `?password=[REDACTED]`),
    # but the value is already replaced and must not make a safe artifact fail
    # closed merely because its public marker is present.
    return sanitized != _CONTROL_RE.sub("", text)


def _public_run_advisor_signals(value: Any, section: str) -> dict[str, Any]:
    """Return the closed public signal subset for a run-advisor section."""

    if not isinstance(value, Mapping):
        return {}
    objective_bools = {
        "mission_present",
        "objective_present",
        "goal_present",
        "project_primary_objective_present",
        "topic_objective_present",
        "session_objective_present",
    }
    objective_counts = {"subobjective_count", "query_token_count", "context_token_count"}
    graph_counts = {
        "edge_samples",
        "seed_count",
        "candidate_count",
        "added_evidence_count",
        "graph_touches",
        "handoffs",
        "checkpoints",
    }
    result: dict[str, Any] = {}
    for raw_key, raw_value in list(value.items())[:32]:
        key = str(raw_key or "").strip()
        if section == "objective_coherence" and key in objective_bools and isinstance(raw_value, bool):
            result[key] = raw_value
        elif section == "objective_coherence" and key in objective_counts and isinstance(raw_value, int) and not isinstance(raw_value, bool) and 0 <= raw_value <= (1 << 31) - 1:
            result[key] = raw_value
        elif section == "objective_coherence" and key == "shared_terms" and isinstance(raw_value, collections.abc.Sequence) and not isinstance(raw_value, (str, bytes, bytearray, memoryview)):
            terms: list[str] = []
            for item in list(raw_value)[:12]:
                if not isinstance(item, str):
                    continue
                safe = _sanitize_public_text(item)[0]
                if safe == item and len(safe.encode("utf-8")) <= 160:
                    terms.append(safe)
            result[key] = terms
        elif section == "graph_quality" and key in graph_counts and isinstance(raw_value, int) and not isinstance(raw_value, bool) and 0 <= raw_value <= (1 << 31) - 1:
            result[key] = raw_value
        elif section == "graph_quality" and key == "relations" and isinstance(raw_value, Mapping):
            relations: dict[str, int] = {}
            for relation_key, relation_value in list(raw_value.items())[:32]:
                relation = _sanitize_public_text(str(relation_key or ""))[0]
                if (
                    relation
                    and len(relation.encode("utf-8")) <= 120
                    and isinstance(relation_value, int)
                    and not isinstance(relation_value, bool)
                    and 0 <= relation_value <= (1 << 31) - 1
                ):
                    relations[relation] = relation_value
            result[key] = relations
    return result


def _redact_value(
    value: Any,
    *,
    _depth: int = 0,
    _run_advisor_scope: bool = False,
    _run_advisor_path: tuple[str, ...] = (),
) -> Any:
    if _depth > 32:
        return "[REDACTED_DEPTH]"
    if isinstance(value, Mapping):
        if value.get("schema_id") == "run_advisor.v1":
            _run_advisor_scope = True
            _run_advisor_path = ()
        result: dict[str, Any] = {}
        for key, item in value.items():
            raw_name = (
                bytes(key).decode("utf-8", errors="replace")
                if isinstance(key, (bytes, bytearray, memoryview))
                else str(key)
            )
            name = _sanitize_public_text(raw_name)[0] or "[REDACTED_KEY]"
            if name in result:
                name = "[REDACTED_KEY]"
            if _is_public_route_value(raw_name, item):
                result[name] = item
            elif _is_public_reference_value(raw_name, item):
                if isinstance(item, collections.abc.Sequence) and not isinstance(
                    item, (str, bytes, bytearray, memoryview)
                ):
                    result[name] = [str(value).strip() for value in item]
                else:
                    result[name] = str(item).strip()
            elif (
                _run_advisor_scope
                and raw_name == "signals"
                and _run_advisor_path in {("objective_coherence",), ("graph_quality",)}
            ):
                result[name] = _public_run_advisor_signals(item, _run_advisor_path[0])
            elif _is_secret_key(raw_name) and not _is_safe_policy_marker(raw_name, item):
                result[name] = "[REDACTED]"
            else:
                result[name] = _redact_value(
                    item,
                    _depth=_depth + 1,
                    _run_advisor_scope=_run_advisor_scope,
                    _run_advisor_path=_run_advisor_path + (raw_name,),
                )
        return result
    if isinstance(value, (bytes, bytearray, memoryview)):
        return redact_text(bytes(value).decode("utf-8", errors="replace"))
    if isinstance(value, collections.abc.Set):
        sanitized = [
            _redact_value(
                item,
                _depth=_depth + 1,
                _run_advisor_scope=_run_advisor_scope,
                _run_advisor_path=_run_advisor_path,
            )
            for item in list(value)[:128]
        ]
        return sorted(
            sanitized,
            key=lambda item: json.dumps(item, ensure_ascii=True, sort_keys=True, default=str),
        )
    if isinstance(value, collections.abc.Sequence) and not isinstance(value, str):
        return [
            _redact_value(
                item,
                _depth=_depth + 1,
                _run_advisor_scope=_run_advisor_scope,
                _run_advisor_path=_run_advisor_path,
            )
            for item in list(value)[:128]
        ]
    if isinstance(value, str):
        stripped = value.strip()
        if stripped.startswith(("{", "[")):
            try:
                decoded = json.loads(stripped)
            except (TypeError, json.JSONDecodeError):
                decoded = None
            if isinstance(decoded, (Mapping, list)):
                return json.dumps(
                    _redact_value(decoded, _depth=_depth + 1),
                    ensure_ascii=True,
                    sort_keys=True,
                    separators=(",", ":"),
                )
        return _sanitize_public_text(value)[0]
    if value is None or isinstance(value, (bool, int, float)):
        return value
    if isinstance(value, os.PathLike):
        return _sanitize_public_text(os.fspath(value))[0]
    return _sanitize_public_text(str(value))[0]


def redact_text(value: Any) -> str:
    if isinstance(value, (bytes, bytearray, memoryview)):
        text = bytes(value).decode("utf-8", errors="replace")
    else:
        text = str(value or "")
    stripped = text.strip()
    if stripped.startswith(("{", "[")):
        try:
            decoded = json.loads(stripped)
        except (TypeError, json.JSONDecodeError):
            decoded = None
        if isinstance(decoded, (Mapping, list)):
            return json.dumps(
                _redact_value(decoded, _depth=1),
                ensure_ascii=True,
                sort_keys=True,
                separators=(",", ":"),
            )
    return _sanitize_public_text(text)[0]


def redact_public_value(value: Any) -> Any:
    """Apply the canonical public-output boundary recursively."""

    return _redact_value(value)


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


def resolve_pretty(explicit: bool | None = None, *, output_path: str = "") -> bool:
    if explicit is not None:
        return explicit
    mode = os.getenv("CONTEXTLATTICE_CLI_OUTPUT", "auto").strip().lower()
    if mode == "pretty":
        return True
    if mode in {"compact", "raw"}:
        return False
    if output_path:
        return False
    return bool(sys.stdout.isatty())


def emit(payload: dict[str, Any], pretty: bool | None = None) -> None:
    resolved = resolve_pretty(pretty)
    safe_payload = redact_public_value(payload)
    print(json.dumps(safe_payload, indent=2 if resolved else None, sort_keys=resolved))


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def agent_contract_registry_identity() -> dict[str, Any]:
    path = REPO_ROOT / "config" / "agent_contracts" / "agent_output_contracts.json"
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
        registry_id = str(payload.get("registry_id") or "").strip()
        registry_version = int(payload.get("registry_version"))
        if not registry_id or registry_version <= 0:
            raise ValueError("registry identity is incomplete")
        return {"ok": True, "registry_id": registry_id, "registry_version": registry_version}
    except (OSError, TypeError, ValueError, json.JSONDecodeError):
        return {
            "ok": False,
            "registry_id": "",
            "registry_version": 0,
            "error": "agent_contract_registry_unavailable",
        }


def runtime_payload_freshness(
    generated_at: Any,
    *,
    maximum_age_seconds: float = 120.0,
    maximum_future_skew_seconds: float = 30.0,
    observed_at: datetime | None = None,
) -> dict[str, Any] | None:
    raw = str(generated_at or "").strip()
    try:
        parsed = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            raise ValueError("timestamp must include a timezone")
    except ValueError:
        return {"reason": "generated_at_invalid", "actual": raw}
    now = observed_at or datetime.now(timezone.utc)
    age_seconds = (now.astimezone(timezone.utc) - parsed.astimezone(timezone.utc)).total_seconds()
    if age_seconds > maximum_age_seconds:
        return {
            "reason": "generated_at_stale",
            "age_seconds": age_seconds,
            "maximum_age_seconds": maximum_age_seconds,
        }
    if age_seconds < -maximum_future_skew_seconds:
        return {
            "reason": "generated_at_future",
            "future_skew_seconds": -age_seconds,
            "maximum_future_skew_seconds": maximum_future_skew_seconds,
        }
    return None


def runtime_identity_expectation_findings(
    payload: dict[str, Any],
    *,
    expected_source_commit: str | None = None,
    expected_source_tree: str | None = None,
    expected_boot_nonce: str | None = None,
) -> tuple[dict[str, Any], dict[str, str], list[dict[str, Any]]]:
    expected = {
        field: value
        for field, value in (
            ("source_commit", expected_source_commit),
            ("source_tree", expected_source_tree),
            ("boot_nonce", expected_boot_nonce),
        )
        if value is not None
    }
    raw_build = payload.get("build")
    build = raw_build if isinstance(raw_build, dict) else {}
    findings: list[dict[str, Any]] = []
    if not expected:
        return build, expected, findings
    if not isinstance(raw_build, dict):
        findings.append({"reason": "build_identity_missing", "expected_fields": sorted(expected)})
        return build, expected, findings
    if build.get("schema_id") != "contextlattice_build_identity.v1":
        findings.append(
            {
                "reason": "build_identity_schema_mismatch",
                "expected": "contextlattice_build_identity.v1",
                "actual": build.get("schema_id"),
            }
        )
    required_fields = ("version", "channel", "source_commit", "source_tree", "boot_nonce")
    missing_fields = [field for field in required_fields if not isinstance(build.get(field), str) or not build[field]]
    if missing_fields:
        findings.append({"reason": "build_identity_fields_missing", "fields": missing_fields})
    for field, expected_value in expected.items():
        actual = build.get(field)
        if not isinstance(actual, str) or not actual:
            findings.append({"reason": "build_identity_field_missing", "field": field, "expected": expected_value})
        elif actual != expected_value:
            findings.append(
                {
                    "reason": "build_identity_mismatch",
                    "field": field,
                    "expected": expected_value,
                    "actual": actual,
                }
            )
    return build, expected, findings


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


def _request_json(
    method: str,
    path: str,
    payload: dict[str, Any] | None,
    timeout: float | None,
    *,
    redact_response: bool,
) -> dict[str, Any]:
    maximum_response_bytes = 8 * 1024 * 1024
    url = path if path.startswith(("http://", "https://")) else contextlattice_base_url() + path
    data = json.dumps(payload or {}).encode("utf-8") if payload is not None else None
    headers = {"content-type": "application/json"}
    key = contextlattice_api_key()
    if key:
        headers["x-api-key"] = key
    req = urllib.request.Request(url, data=data, method=method.upper(), headers=headers)
    try:
        open_kwargs = {} if timeout is None else {"timeout": timeout}
        with urllib.request.urlopen(req, **open_kwargs) as resp:
            body = resp.read(maximum_response_bytes + 1)
            if len(body) > maximum_response_bytes:
                raise SystemExit(
                    json.dumps({"ok": False, "error": "gateway_response_invalid"}, sort_keys=True)
                )
            try:
                raw = body.decode("utf-8", errors="strict")
            except UnicodeDecodeError as exc:
                raise SystemExit(
                    json.dumps({"ok": False, "error": "gateway_response_invalid"}, sort_keys=True)
                ) from exc
    except urllib.error.HTTPError as exc:
        try:
            body = exc.read(maximum_response_bytes + 1)
            if len(body) <= maximum_response_bytes:
                try:
                    body.decode("utf-8", errors="strict")
                except UnicodeDecodeError:
                    pass
        finally:
            exc.close()
        raise SystemExit(
            json.dumps({"ok": False, "status": exc.code, "error": "gateway_request_failed"}, sort_keys=True)
        )
    if not raw.strip():
        return {}
    try:
        decoded = json.loads(raw)
    except (UnicodeError, json.JSONDecodeError, RecursionError) as exc:
        # Do not place a raw gateway body in a parser exception or CLI error.
        raise SystemExit(json.dumps({"ok": False, "error": "gateway_response_invalid"}, sort_keys=True)) from exc
    if not isinstance(decoded, dict):
        return {}
    return redact_public_value(decoded) if redact_response else decoded


def request_json(method: str, path: str, payload: dict[str, Any] | None, timeout: float | None) -> dict[str, Any]:
    """Request a Gateway object through the canonical public redaction boundary."""

    return _request_json(method, path, payload, timeout, redact_response=True)


def request_json_for_validation(
    method: str,
    path: str,
    payload: dict[str, Any] | None,
    timeout: float | None,
) -> dict[str, Any]:
    """Request exact bounded data for internal validation before safe emission.

    Callers must validate the response and pass every emitted value through
    ``emit``.  This exists because pre-validation redaction destroys exact
    commit identities and native route names that the live boundary auditors
    are responsible for checking.
    """

    return _request_json(method, path, payload, timeout, redact_response=False)


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


def agent_session_reuse_key(
    project: str,
    agent: str,
    agent_id: str,
    objective: str,
    metadata: dict[str, Any] | None,
) -> str:
    try:
        metadata_digest = hashlib.sha256(
            json.dumps(metadata or {}, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()
    except (TypeError, ValueError, OverflowError, UnicodeEncodeError, RecursionError):
        metadata_digest = hashlib.sha256(b"invalid-metadata").hexdigest()
    material = "|".join((project, agent, agent_id, objective, metadata_digest)).encode("utf-8")
    return "reuse_" + hashlib.sha256(material).hexdigest()[:24]


def session_identity_valid(value: Any) -> bool:
    if not isinstance(value, str):
        return False
    value = value.strip()
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return bool(value) and len(encoded) <= 256 and re.fullmatch(r"[A-Za-z0-9._:@-]+", value) is not None


def agent_session_matches_authority(
    session: Any,
    *,
    expected_id: str,
    project: str,
    agent: str,
    agent_id: str,
    reuse_key: str,
) -> bool:
    if not isinstance(session, dict) or not session_is_reusable(session):
        return False
    actual_id = session.get("id")
    if not session_identity_valid(actual_id) or str(actual_id).strip() != expected_id:
        return False
    if str(session.get("project") or "").strip() != project:
        return False
    if agent and str(session.get("agent") or "").strip() != agent:
        return False
    if agent_id and str(session.get("agent_id") or "").strip() != agent_id:
        return False
    return bool(reuse_key) and str(session.get("reuse_key") or "").strip() == reuse_key


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
    objective = objective or os.getenv("CONTEXTLATTICE_OBJECTIVE", "") or "ContextLattice agent runtime objective"
    reuse_key = agent_session_reuse_key(project, agent, agent_id, objective, metadata)
    if (
        state_session_id
        and session_identity_valid(state_session_id)
        and str(state.get("reuse_key") or "").strip() == reuse_key
    ):
        try:
            raw = request_json_for_validation("GET", f"/v1/agents/sessions/{state_session_id}", None, timeout)
            session = raw.get("session") if isinstance(raw.get("session"), dict) else {}
            if raw.get("ok") is True and agent_session_matches_authority(
                session,
                expected_id=state_session_id,
                project=project,
                agent=agent,
                agent_id=agent_id,
                reuse_key=reuse_key,
            ):
                return {"ok": True, "session_id": state_session_id, "source": "state", "session": session}
        except (Exception, SystemExit):
            if not soft:
                raise

    payload: dict[str, Any] = {
        "agent": agent,
        "agent_id": agent_id,
        "project": project,
        "ensure": True,
        "reuse_key": reuse_key,
        "objective": objective,
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
        raw = request_json_for_validation("POST", "/v1/agents/sessions/start", payload, timeout)
    except (Exception, SystemExit):
        if soft:
            return {"ok": False, "session_id": "", "error": "agent_session_unavailable"}
        raise
    session = raw.get("session") if isinstance(raw.get("session"), dict) else {}
    session_id = str(session.get("id") or "").strip()
    if raw.get("ok") is not True or not agent_session_matches_authority(
        session,
        expected_id=session_id,
        project=project,
        agent=agent,
        agent_id=agent_id,
        reuse_key=reuse_key,
    ):
        return {"ok": False, "session_id": "", "error": "agent_session_authority_mismatch"}
    write_agent_session_state(
        project,
        {
            "session_id": session_id,
            "project": project,
            "agent": agent,
            "agent_id": agent_id,
            "objective": payload.get("objective", ""),
            "reuse_key": reuse_key,
            "source": "auto-session",
        },
    )
    return {"ok": True, "session_id": session_id, "source": "created", "session": session}


def normalize_rule(line: str) -> str:
    line = re.sub(r"`[^`]+`", "`x`", line.strip().lower())
    line = re.sub(r"\s+", " ", line)
    return line.strip(" -*#.")
