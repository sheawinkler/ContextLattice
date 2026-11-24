#!/usr/bin/env python3
"""Check the native Pi or OMP skill inventory from a finite RPC JSONL capture."""

import argparse
import json
import sys


NAME = "skill:contextlattice-recall"
MAX_FRAME = 4 * 1024 * 1024
MAX_BYTES = 16 * 1024 * 1024


def verify(stream, host, request_id):
    matches = None
    total = 0
    for _ in range(8192):
        line = stream.readline(MAX_FRAME + 1)
        if not line:
            break
        total += len(line)
        if len(line) > MAX_FRAME or total > MAX_BYTES:
            raise ValueError("RPC capture exceeds its size limit")
        if not line.strip():
            continue
        frame = json.loads(line)
        if not isinstance(frame, dict):
            raise ValueError("invalid RPC frame")
        if host == "omp":
            if frame.get("type") != "available_commands_update":
                continue
            rows = frame.get("commands")
        else:
            if frame.get("type") != "response" or frame.get("id") != request_id:
                continue
            if matches is not None or frame.get("command") != "get_commands" or frame.get("success") is not True:
                raise ValueError("unsuccessful or ambiguous Pi reply")
            data = frame.get("data")
            rows = data.get("commands") if isinstance(data, dict) else None
        if not isinstance(rows, list) or any(not isinstance(row, dict) for row in rows):
            raise ValueError("invalid command inventory")
        # A later OMP inventory replaces the old one, including after removal.
        matches = sum(row.get("name") == NAME for row in rows)
    else:
        raise ValueError("too many RPC frames")
    if matches != 1:
        raise ValueError("expected exactly one recall command in the native inventory")
    return {"host": host, "verified": True, "grade": "command-discovery", "matches": matches}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", choices=("pi", "omp"))
    parser.add_argument("--request-id", default="cl-inventory", help="ID of the Pi get_commands request")
    args = parser.parse_args()
    try:
        print(json.dumps(verify(sys.stdin.buffer, args.host, args.request_id)))
    except (ValueError, RecursionError):
        parser.exit(1, json.dumps({"host": args.host, "verified": False}) + "\n")
