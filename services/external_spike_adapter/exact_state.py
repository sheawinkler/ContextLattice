from __future__ import annotations

import json
from pathlib import Path
from typing import Any


EXACT_STATE_INDEX_SCHEMA_ID = "contextlattice_exact_state_index.v1"
EXACT_STATE_INDEX_MAX_PATHS = 100_000


class ExactStateRegistryError(ValueError):
    pass


def _normalized_key(value: Any) -> str:
    if not isinstance(value, str):
        raise ExactStateRegistryError("exact-state registry contains a non-string path key")
    normalized = value.strip().lower()
    if normalized.count("::") != 1:
        raise ExactStateRegistryError("exact-state registry contains an invalid path key")
    project, file_name = normalized.split("::", 1)
    canonical_file = _canonical_file_name(file_name)
    if (
        not project
        or project.startswith("_")
        or "/" in project
        or "\\" in project
        or project in {".", ".."}
        or canonical_file is None
    ):
        raise ExactStateRegistryError("exact-state registry contains an invalid path key")
    return f"{project}::{canonical_file}"


def parse_exact_state_paths(raw: bytes | str) -> set[str]:
    try:
        payload = json.loads(raw)
    except (TypeError, json.JSONDecodeError) as exc:
        raise ExactStateRegistryError("parse exact-state registry") from exc
    if not isinstance(payload, dict):
        raise ExactStateRegistryError("exact-state registry must be an object")
    if payload.get("schema_id") != EXACT_STATE_INDEX_SCHEMA_ID:
        raise ExactStateRegistryError("exact-state registry schema mismatch")
    paths = payload.get("paths")
    if not isinstance(paths, list):
        raise ExactStateRegistryError("exact-state registry paths must be an array")
    if len(paths) > EXACT_STATE_INDEX_MAX_PATHS:
        raise ExactStateRegistryError("exact-state registry exceeds bounded path limit")
    return {_normalized_key(value) for value in paths}


def load_exact_state_paths(path: Path) -> set[str]:
    if path.is_symlink():
        raise ExactStateRegistryError("exact-state registry must not be a symlink")
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise ExactStateRegistryError(f"read exact-state registry: {exc}") from exc
    return parse_exact_state_paths(raw)


def _canonical_file_name(file_name: str) -> str | None:
    normalized = str(file_name or "").strip().replace("\\", "/").lower()
    if not normalized or normalized.startswith("/") or "::" in normalized:
        return None
    segments: list[str] = []
    for segment in normalized.split("/"):
        token = segment.strip()
        if not token or token == ".":
            continue
        if token == "..":
            return None
        segments.append(token)
    return "/".join(segments) or None


def is_exact_state_path(paths: set[str], project: str, file_name: str) -> bool:
    normalized_project = str(project or "").strip().lower()
    normalized_file = _canonical_file_name(file_name)
    if (
        not normalized_project
        or normalized_project.startswith("_")
        or "/" in normalized_project
        or "\\" in normalized_project
        or "::" in normalized_project
        or normalized_project in {".", ".."}
        or normalized_file is None
    ):
        return True
    return f"{normalized_project}::{normalized_file}" in paths
