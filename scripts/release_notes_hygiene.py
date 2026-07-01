#!/usr/bin/env python3
"""Reject machine-local paths in public release notes."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path


BLOCKED_PATTERNS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("developer_home_path", re.compile(r"/Users/[A-Za-z0-9._-]+")),
    ("external_machine_volume", re.compile(r"/Volumes/[A-Za-z0-9._-]+")),
    ("temporary_machine_path", re.compile(r"/tmp/contextlattice[A-Za-z0-9._/-]*")),
    ("home_directory_shorthand", re.compile(r"(?<![A-Za-z0-9_])~/")),
)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--file", help="Release notes file to inspect.")
    parser.add_argument("--text", help="Release notes text to inspect.")
    args = parser.parse_args()

    if args.file:
        text = Path(args.file).read_text(encoding="utf-8")
    else:
        text = args.text or ""

    findings: list[dict[str, object]] = []
    for line_no, line in enumerate(text.splitlines(), 1):
        for kind, pattern in BLOCKED_PATTERNS:
            if pattern.search(line):
                findings.append({"kind": kind, "line": line_no})

    if findings:
        print("release note hygiene failed: remove machine-local paths before publishing", file=sys.stderr)
        for finding in findings:
            print(f"- {finding['kind']} at line {finding['line']}", file=sys.stderr)
        return 1
    print("release note hygiene ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
