#!/usr/bin/env python3
"""Shared deterministic inclusion policy for ContextLattice release payloads."""

from __future__ import annotations

from pathlib import PurePosixPath


DENIED_ROOTS = {
    ".backup",
    ".contextlattice.config",
    ".github",
    ".mcp-servers",
    ".ops",
    "archive",
    "artifacts",
    "bench",
    "data",
    "dev",
    "development",
    "dist",
    "docker_compose_backup",
    "logs",
    "packaging",
    "private",
    "private_docs",
    "promptfoo",
    "reports",
    "tmp",
    "trae_runs",
    "trajectories",
}
DENIED_PARTS = {
    "__pycache__",
    ".data",
    ".mypy_cache",
    ".pytest_cache",
    ".ruff_cache",
    "backups",
    "cache",
    "caches",
    "credentials",
    "evidence",
    "fixtures",
    "node_modules",
    "secrets",
    "target",
    "test",
    "test-evidence",
    "test-results",
    "test_evidence",
    "testdata",
    "tests",
}
DENIED_DOCS = {"audits", "evals", "perf", "perf-candidate-notes", "private"}
DENIED_ROOT_FILES = {
    ".env_dev",
    ".env_prod",
    ".envrc",
    ".gitattributes",
    ".gitignore",
    ".node-version",
    ".nvmrc",
    "AGENTS.md",
    "CODE_OF_CONDUCT.md",
    "CONTRIBUTING.md",
    "glama.json",
    "justfile",
    "launch.applescript",
    "pytest.ini",
    "trae_config.template.yaml",
}
DENIED_ROOT_FILES_CASEFOLDED = {value.casefold() for value in DENIED_ROOT_FILES}
SECRET_SUFFIXES = {".key", ".p12", ".pem", ".pfx"}
TEST_SUFFIXES = (
    ".test",
    ".test.exe",
    "_test.go",
    "_test.py",
    ".spec.js",
    ".spec.jsx",
    ".spec.ts",
    ".spec.tsx",
    ".test.js",
    ".test.jsx",
    ".test.ts",
    ".test.tsx",
)
WIN32_INVALID_COMPONENT_CHARACTERS = frozenset('<>:"|?*')
WIN32_RESERVED_COMPONENT_BASENAMES = {
    "aux",
    "clock$",
    "con",
    "conin$",
    "conout$",
    "nul",
    "prn",
    *(f"com{index}" for index in range(1, 10)),
    *(f"lpt{index}" for index in range(1, 10)),
}


def _path(relative: str | PurePosixPath) -> PurePosixPath:
    path = relative if isinstance(relative, PurePosixPath) else PurePosixPath(relative)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise ValueError(f"source path is not a safe repository-relative path: {relative}")
    return path


def portable_payload_path_key(relative: str | PurePosixPath) -> str:
    """Return a collision key safe for Unix, macOS, and Win32 extraction."""

    path = _path(relative)
    value = path.as_posix()
    if (
        not value.isascii()
        or "\\" in value
        or any(ord(character) < 0x20 or ord(character) == 0x7F for character in value)
    ):
        raise ValueError(f"release payload path is not portable ASCII: {relative}")
    for component in path.parts:
        if (
            component.endswith((" ", "."))
            or any(character in WIN32_INVALID_COMPONENT_CHARACTERS for character in component)
            or component.split(".", 1)[0].rstrip(" .").casefold()
            in WIN32_RESERVED_COMPONENT_BASENAMES
        ):
            raise ValueError(f"release payload path is not portable on Win32: {relative}")
    return value.casefold()


def payload_exclusion_reason(relative: str | PurePosixPath) -> str | None:
    """Return why a tracked path cannot enter a shipped payload, if excluded."""

    path = _path(relative)
    lowered_parts = tuple(part.lower() for part in path.parts)
    lowered_value = "/".join(lowered_parts)
    name = path.name.lower()
    if lowered_parts[0] in DENIED_ROOTS:
        return "developer/local-state root"
    if len(lowered_parts) >= 2 and lowered_parts[0] == "docs" and lowered_parts[1] in DENIED_DOCS:
        return "private or test-evidence documentation"
    if lowered_value == "services/orchestrator":
        return "legacy runtime symlink"
    if len(lowered_parts) >= 2 and lowered_parts[:2] == ("config", "runtime-license"):
        if "private" in name or "signing" in name:
            return "private runtime-license signing material"
    if lowered_value in DENIED_ROOT_FILES_CASEFOLDED:
        return "developer policy file"
    if any(part in DENIED_PARTS for part in lowered_parts):
        return "developer/test/cache path"
    if name == ".env" or name.startswith(".env.") or name.startswith(".env_"):
        if lowered_value != ".env.example":
            return "environment/local secret file"
    if name.endswith(".pid") or ".bak" in name or name.endswith(TEST_SUFFIXES):
        return "generated/test artifact"
    if path.suffix.lower() in SECRET_SUFFIXES:
        return "secret key material"
    return None


def normalized_payload_mode(git_mode: str) -> str:
    if git_mode == "100755":
        return "100755"
    if git_mode == "100644":
        return "100644"
    raise ValueError(f"unsupported payload source mode: {git_mode}")
