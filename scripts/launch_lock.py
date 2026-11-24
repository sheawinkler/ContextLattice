#!/usr/bin/env python3
"""Separate local release readiness from hosted MCP publication evidence."""

from __future__ import annotations

import argparse
import http.client
import json
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[1]
# Independent, reviewed trust anchor. Never derive it from the listing or env.
TRUSTED_MCP_URLS: frozenset[str] = frozenset()
PROTOCOL = "2025-11-25"
MAX_BYTES = 16 * 1024
UNPUBLISHED = "hosted MCP unpublished"


@dataclass
class GateCheck:
    ok: bool
    name: str
    details: str


def _run_submission_preflight(online: bool) -> GateCheck:
    cmd = [sys.executable, str(ROOT / "scripts/submission_preflight.py")]
    if online:
        cmd.append("--online")
    proc = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, check=False)
    if proc.returncode:
        return GateCheck(False, "submission_preflight", "failed; inspect submission_preflight separately")
    return GateCheck(True, "submission_preflight", "pass")


def _json_object(raw: bytes) -> dict:
    def unique(pairs):
        obj = dict(pairs)
        if len(obj) != len(pairs):
            raise ValueError("duplicate member")
        return obj

    def invalid_number(_):
        raise ValueError("invalid number")

    if len(raw) > MAX_BYTES:
        raise ValueError("oversized object")
    obj = json.loads(raw.decode("utf-8"), object_pairs_hook=unique, parse_constant=invalid_number)
    if not isinstance(obj, dict):
        raise ValueError("not an object")
    return obj


def _trusted(url: object) -> bool:
    return (isinstance(url, str) and url in TRUSTED_MCP_URLS and
            re.fullmatch(r"https://(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)?contextlattice\.io/mcp", url) is not None)


def _registry_remote() -> str | None:
    path = ROOT / "registry/contextlattice.server.json"
    with path.open("rb") as manifest:
        obj = _json_object(manifest.read(MAX_BYTES + 1))
    if obj.get("name") != "io.github.sheawinkler/contextlattice":
        raise ValueError("unexpected registry identity")
    remotes = obj.get("remotes")
    if remotes == []:
        return None
    if not isinstance(remotes, list) or len(remotes) != 1 or not isinstance(remotes[0], dict):
        raise ValueError("expected one remote")
    remote = remotes[0]
    if (set(remote) != {"type", "url"} or remote["type"] != "streamable-http"
            or not _trusted(remote["url"])):
        raise ValueError("not a trusted credential-free remote")
    return remote["url"]


def _probe_public_mcp(url: str, timeout: int) -> GateCheck:
    def fail(reason: str) -> GateCheck:
        return GateCheck(False, "public_mcp_probe", f"{UNPUBLISHED}: {reason}")

    if not _trusted(url) or not 1 <= timeout <= 30:
        return fail("untrusted remote or invalid timeout")
    init = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
        "protocolVersion": PROTOCOL, "capabilities": {},
        "clientInfo": {"name": "launch-lock", "version": "1"},
    }}
    origin = urlsplit(url)
    # Direct, certificate-verified HTTPS: no ambient proxy, cookies, auth or redirect handler.
    conn = http.client.HTTPSConnection(origin.hostname, timeout=timeout)
    try:
        conn.request("POST", origin.path, json.dumps(init).encode("utf-8"), headers={
            "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
        })
        with conn.getresponse() as response:
            if response.status != 200:
                return fail(f"initialize returned HTTP {response.status}")
            if response.getheader("Content-Type", "").partition(";")[0].strip().lower() != "application/json":
                return fail("expected JSON initialize response")
            obj = _json_object(response.read(MAX_BYTES + 1))
    except (OSError, http.client.HTTPException, ValueError, RecursionError):
        return fail("invalid or unavailable initialize response")
    finally:
        conn.close()

    result = obj.get("result")
    if (obj.get("jsonrpc") != "2.0" or type(obj.get("id")) is not int or obj["id"] != 1
            or set(obj) != {"jsonrpc", "id", "result"} or not isinstance(result, dict)
            or result.get("protocolVersion") != PROTOCOL):
        return fail("initialize envelope or protocol mismatch")
    identity = result.get("serverInfo")
    caps = result.get("capabilities")
    if (not isinstance(identity, dict) or identity.get("name") != "contextlattice"
            or identity.get("version") != "1" or not isinstance(caps, dict)
            or not isinstance(caps.get("tools"), dict)
            or ("listChanged" in caps["tools"] and type(caps["tools"]["listChanged"]) is not bool)):
        return fail("server identity or tools capability mismatch")
    return GateCheck(True, "public_mcp_probe", "verified anonymous initialize only; not publication")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=("local", "public"), default="local")
    parser.add_argument("--timeout", type=int, choices=range(1, 31), default=10, metavar="1..30")
    args = parser.parse_args()
    local = args.mode == "local"
    remote = None
    try:
        remote = _registry_remote()
        manifest = GateCheck(True, "registry_manifest", "reviewed remote configured" if remote else
                             f"{UNPUBLISHED}: no remote configured")
    except (OSError, ValueError, RecursionError):
        manifest = GateCheck(False, "registry_manifest", f"{UNPUBLISHED}: invalid or untrusted manifest")

    if local:
        probe = GateCheck(True, "public_mcp_probe", "not required for local stdio; no request")
    elif remote is None:
        probe = GateCheck(False, "public_mcp_probe", f"{UNPUBLISHED}: no trusted configured remote")
    else:
        probe = _probe_public_mcp(remote, args.timeout)
    checks = [manifest, probe, _run_submission_preflight(online=not local and probe.ok)]
    for name in ("GitHub Release", "Custom Domain Docs"):
        checks.append(GateCheck(True, f"channel:{name}", "not evidence of hosted MCP publication"))
    for name in ("MCP.so", "Glama MCP", "Awesome MCP Servers", "MCP Registry (official)", "PulseMCP"):
        checks.append(GateCheck(local, f"channel:{name}", "hosted listing deferred" if local
                                else "hosted listing not qualified by this lock"))
    # This lock has no verifier for these independent publication prerequisites.
    # Never turn endpoint reachability (or ordinary docs preflight) into their proof.
    for name in ("public-site ownership", "public authentication policy", "matching registry registration"):
        checks.append(GateCheck(local, name, "deferred in local mode" if local else "not verified; hosted promotion blocked"))

    print(f"Launch lock mode: {args.mode}")
    for check in checks:
        print(f"[{'PASS' if check.ok else 'FAIL'}] {check.name} :: {check.details}")
    passed = sum(check.ok for check in checks)
    print(f"\nSummary: {passed}/{len(checks)} checks passed.")
    print("Result: GO" if passed == len(checks) else "Result: NO-GO")
    return int(passed != len(checks))


if __name__ == "__main__":
    sys.exit(main())
