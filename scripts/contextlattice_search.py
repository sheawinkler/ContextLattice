#!/usr/bin/env python3
"""CLI wrapper for ContextLattice lifecycle-aware memory search."""

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
        prog="contextlattice_search",
        description=(
            "Search ContextLattice memory with lifecycle metadata. "
            "Returns immediate results plus continuation details for slow sources."
        ),
    )
    parser.add_argument(
        "-q",
        "--query",
        help="Search query text. If omitted, positional query is used.",
    )
    parser.add_argument(
        "query_positional",
        nargs="?",
        help="Optional positional query string.",
    )
    parser.add_argument(
        "-p",
        "--project",
        default=os.getenv("CONTEXTLATTICE_PROJECT", "contextlattice"),
        help="Project scope (default: CONTEXTLATTICE_PROJECT or contextlattice).",
    )
    parser.add_argument(
        "-t",
        "--topic-path",
        default=None,
        help="Optional topic path for scoped retrieval.",
    )
    parser.add_argument(
        "-m",
        "--mode",
        choices=("fast", "balanced", "deep"),
        default=os.getenv("CONTEXTLATTICE_RETRIEVAL_MODE", "balanced"),
        help="Retrieval mode (default: balanced).",
    )
    parser.add_argument(
        "-l",
        "--limit",
        type=int,
        default=10,
        help="Maximum results requested from retrieval backend (default: 10).",
    )
    parser.add_argument(
        "--wait",
        action="store_true",
        help="Block for async completion when continuation is queued.",
    )
    parser.add_argument(
        "--raw",
        action="store_true",
        help="Emit compact JSON instead of pretty output.",
    )
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    query = (args.query or args.query_positional or "").strip()
    if not query:
        parser.error("query is required (use --query or positional query)")

    orch = ContextLatticeOrchestrator()
    payload = orch.search_with_lifecycle(
        query=query,
        project=args.project,
        topic_path=args.topic_path,
        limit=max(1, int(args.limit)),
        retrieval_mode=args.mode,
        wait_for_completion=bool(args.wait),
    )
    if args.raw:
        print(json.dumps(payload, separators=(",", ":"), ensure_ascii=False))
    else:
        print(json.dumps(payload, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
