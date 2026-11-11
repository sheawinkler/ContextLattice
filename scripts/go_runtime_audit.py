#!/usr/bin/env python3
"""Check reviewed Go/Alpine packaging pins without fetching or executing images."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import re

GO_VERSION = "1.26.11"
ALPINE_VERSION = "3.24.4"
EXPECTED_BASES = {
    "Dockerfile.gateway-go": [f"golang:{GO_VERSION}-alpine3.24", f"alpine:{ALPINE_VERSION}"],
    "Dockerfile.orchestrator-go": [f"golang:{GO_VERSION}-alpine3.24", f"alpine:{ALPINE_VERSION}"],
    "Dockerfile.hf-lite": [f"golang:{GO_VERSION}-bookworm"],
    "Dockerfile.orchestrator": [f"golang:{GO_VERSION}-bookworm"],
}
MODULES = ("services/gateway-go/go.mod", "services/orchestrator-go/go.mod")
BASE_RE = re.compile(
    r"^\s*FROM\s+(?:--\S+\s+)*(?:docker\.io/(?:library/)?)?((?:golang|alpine)(?::\S+|@\S+)?)(?=\s|$)",
    re.MULTILINE | re.IGNORECASE,
)


def audit(root: Path) -> dict:
    findings = []
    for name, expected in EXPECTED_BASES.items():
        path = root / name
        if not path.is_file():
            findings.append({"path": name, "kind": "missing_file"})
        elif BASE_RE.findall(path.read_text()) != expected:
            findings.append({"path": name, "kind": "runtime_base_mismatch", "expected": expected})
    allowed = {base for bases in EXPECTED_BASES.values() for base in bases}
    for path in sorted(root.glob("Dockerfile*")):
        if path.is_file() and path.name not in EXPECTED_BASES:
            if any(base not in allowed for base in BASE_RE.findall(path.read_text())):
                findings.append({"path": path.name, "kind": "unreviewed_runtime_base"})
    for name in MODULES:
        path = root / name
        if not path.is_file():
            findings.append({"path": name, "kind": "missing_file"})
            continue
        text = path.read_text()
        if re.findall(r"(?m)^go\s+(\S+)\s*$", text) != ["1.26"]:
            findings.append({"path": name, "kind": "go_language_floor_mismatch"})
        if re.findall(r"(?m)^toolchain\s+(\S+)\s*$", text) != [f"go{GO_VERSION}"]:
            findings.append({"path": name, "kind": "go_toolchain_mismatch"})
    return {
        "schema_id": "contextlattice_go_runtime_audit.v1", "ok": not findings,
        "target": {"go": GO_VERSION, "alpine": ALPINE_VERSION}, "findings": findings,
        "scope": "source pins only; installed compatibility and advisory freshness require separate evidence",
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--pretty", action="store_true")
    args = parser.parse_args()
    result = audit(args.root.resolve())
    print(json.dumps(result, indent=2 if args.pretty else None, sort_keys=True))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
