#!/usr/bin/env python3
"""Fail public-lane builds when private launch-doc markers enter tracked files."""

from __future__ import annotations

import argparse
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def _s(*parts: str) -> str:
    return "".join(parts)


BLOCKED_PATHS = {
    _s("docs/", "publish_", "execution_", "tracker.md"),
    _s("docs/", "launch_", "channel_", "copybook.md"),
    _s("docs/", "submission_", "requirements.md"),
    _s("docs/", "internal", "-planning-archive.md"),
}

BLOCKED_PATH_PREFIXES = (
    _s("archive/", "internal", "-planning/"),
)

BLOCKED_TEXT = (
    _s("Private", "/", "Public Sync Notes"),
    _s("publish_", "execution_", "tracker"),
    _s("launch_", "channel_", "copybook"),
    _s("submission_", "requirements"),
    _s("internal", "-planning"),
    _s("private ", "operator ", "docs"),
)

TEXT_EXTENSIONS = {
    "",
    ".bash",
    ".cjs",
    ".css",
    ".go",
    ".html",
    ".js",
    ".json",
    ".md",
    ".mjs",
    ".py",
    ".rs",
    ".sh",
    ".toml",
    ".txt",
    ".yaml",
    ".yml",
}


@dataclass
class Finding:
    kind: str
    path: str
    marker: str


def _tracked_files() -> list[str]:
    proc = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=ROOT,
        capture_output=True,
        check=False,
    )
    if proc.returncode == 0:
        return [p.decode("utf-8") for p in proc.stdout.split(b"\0") if p]

    out: list[str] = []
    for path in ROOT.rglob("*"):
        if ".git" in path.parts:
            continue
        if path.is_file():
            out.append(path.relative_to(ROOT).as_posix())
    return out


def _is_text_candidate(path: str) -> bool:
    return Path(path).suffix.lower() in TEXT_EXTENSIONS


def _scan_file(path: str) -> list[Finding]:
    if not _is_text_candidate(path):
        return []
    full_path = ROOT / path
    if not full_path.is_file():
        return []
    try:
        text = full_path.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        return []
    findings: list[Finding] = []
    for marker in BLOCKED_TEXT:
        if marker in text:
            findings.append(Finding("text", path, marker))
    return findings


def scan() -> list[Finding]:
    findings: list[Finding] = []
    for path in _tracked_files():
        if path in BLOCKED_PATHS or any(path.startswith(prefix) for prefix in BLOCKED_PATH_PREFIXES):
            findings.append(Finding("path", path, path))
        findings.extend(_scan_file(path))
    return findings


def main() -> int:
    parser = argparse.ArgumentParser(description="Guard the public repo boundary.")
    parser.add_argument("--summary", action="store_true", help="Print a one-line summary on success.")
    args = parser.parse_args()

    findings = scan()
    if findings:
        print("Public boundary guard failed:")
        for finding in findings:
            print(f"- {finding.kind}: {finding.path}: {finding.marker}")
        return 1
    if args.summary:
        print("public boundary clean")
    return 0


if __name__ == "__main__":
    sys.exit(main())
