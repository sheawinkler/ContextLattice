#!/usr/bin/env python3
"""CLI wrapper for writing scoped memory records into ContextLattice."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

THIS_DIR = Path(__file__).resolve().parent
if str(THIS_DIR) not in sys.path:
    sys.path.insert(0, str(THIS_DIR))

from agent_orchestration import ContextLatticeOrchestrator  # noqa: E402


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="contextlattice_write",
        description="Write memory content to ContextLattice with project/file/topic metadata.",
    )
    parser.add_argument(
        "-p",
        "--project",
        default=os.getenv("CONTEXTLATTICE_PROJECT", "contextlattice"),
        help="Target project (default: CONTEXTLATTICE_PROJECT or contextlattice).",
    )
    parser.add_argument(
        "-f",
        "--file",
        required=True,
        help="Logical file path in memory store (for example notes/designs/foo.md).",
    )
    parser.add_argument(
        "-c",
        "--content",
        default=None,
        help="Inline content payload.",
    )
    parser.add_argument(
        "--content-file",
        default=None,
        help="Read content from a local file path.",
    )
    parser.add_argument(
        "--stdin",
        action="store_true",
        help="Read content from standard input.",
    )
    parser.add_argument(
        "-t",
        "--topic-path",
        default=None,
        help="Optional topic path (for example runbooks/codex-integration).",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Emit compact JSON instead of pretty output.",
    )
    return parser


def resolve_content(args: argparse.Namespace, parser: argparse.ArgumentParser) -> str:
    content = args.content
    if args.content_file:
        content = Path(args.content_file).read_text(encoding="utf-8")
    if args.stdin:
        content = sys.stdin.read()
    if content is None:
        parser.error("content is required (use --content, --content-file, or --stdin)")
    return str(content)


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    content = resolve_content(args, parser)

    orch = ContextLatticeOrchestrator()
    payload = orch.write(
        project=args.project,
        file_name=args.file,
        content=content,
        topic_path=args.topic_path,
    )
    if args.raw:
        print(json.dumps(payload, separators=(",", ":"), ensure_ascii=False))
    else:
        print(json.dumps(payload, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
