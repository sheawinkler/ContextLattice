#!/usr/bin/env python3
"""Hard launch gate so channel submissions stay in the correct order."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


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
    summary = "pass" if proc.returncode == 0 else "fail"
    if proc.returncode != 0:
        tail = "\n".join((proc.stdout + "\n" + proc.stderr).strip().splitlines()[-12:])
        return GateCheck(False, "submission_preflight", f"{summary}\n{tail}")
    return GateCheck(True, "submission_preflight", summary)


def _load_registry_remote_url() -> GateCheck:
    path = ROOT / "registry/contextlattice.server.json"
    if not path.exists():
        return GateCheck(False, "registry_manifest", "registry/contextlattice.server.json missing")
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        return GateCheck(False, "registry_manifest", f"invalid json: {exc}")
    remotes = payload.get("remotes") or []
    if not remotes:
        return GateCheck(False, "registry_manifest", "no remotes[] configured")
    url = str(remotes[0].get("url", "")).strip()
    if not url:
        return GateCheck(False, "registry_manifest", "remotes[0].url missing")
    return GateCheck(True, "registry_manifest", url)


def _is_private_host(host: str | None) -> bool:
    if not host:
        return True
    host_l = host.lower()
    return host_l in {"127.0.0.1", "localhost"} or host_l.startswith("192.168.") or host_l.startswith("10.")


def _probe_public_mcp(url: str, timeout: int) -> GateCheck:
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme != "https":
        return GateCheck(False, "public_mcp_url_scheme", f"{url} is not https")
    if _is_private_host(parsed.hostname):
        return GateCheck(False, "public_mcp_url_host", f"{parsed.hostname} is local/private")

    payload = json.dumps(
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": "launch-lock", "version": "1.0"},
            },
        }
    ).encode("utf-8")

    req = urllib.request.Request(
        url,
        data=payload,
        method="POST",
        headers={
            "Accept": "application/json, text/event-stream",
            "Content-Type": "application/json",
            "User-Agent": "ContextLattice-Launch-Lock/1.0",
        },
    )
    status = None
    final_url = url
    body = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = getattr(resp, "status", 200)
            final_url = getattr(resp, "geturl", lambda: url)()
            body = resp.read(220).decode("utf-8", errors="ignore")
    except urllib.error.HTTPError as exc:
        status = exc.code
        final_url = exc.geturl() or url
        body = exc.read(220).decode("utf-8", errors="ignore")
    except Exception as exc:  # noqa: BLE001
        return GateCheck(False, "public_mcp_probe", f"probe failed: {exc}")

    final = urllib.parse.urlparse(final_url)
    if final.scheme != "https":
        return GateCheck(False, "public_mcp_probe", f"redirected to non-https: {final_url}")

    # 200: initialize worked, 401/403: endpoint reachable with auth policy.
    if status not in {200, 401, 403}:
        short_body = body.replace("\n", " ").strip()[:120]
        return GateCheck(False, "public_mcp_probe", f"status={status} final={final_url} body={short_body}")

    return GateCheck(True, "public_mcp_probe", f"status={status} final={final_url}")


def _consistency_claim_checks(public_ready: bool) -> list[GateCheck]:
    out: list[GateCheck] = []
    tracker_path = ROOT / "docs/publish_execution_tracker.md"
    req_path = ROOT / "docs/submission_requirements.md"

    tracker = tracker_path.read_text(encoding="utf-8") if tracker_path.exists() else ""
    requirements = req_path.read_text(encoding="utf-8") if req_path.exists() else ""

    tracker_claims_live = bool(re.search(r"\|\s*P0\s*\|\s*MCP Registry \(official\).*?\|\s*Live", tracker))
    req_claims_live = "Live (published" in requirements

    if tracker_claims_live and not public_ready:
        out.append(
            GateCheck(
                False,
                "claims:publish_execution_tracker",
                "tracker marks MCP Registry as Live before public /mcp endpoint is ready",
            )
        )
    else:
        out.append(GateCheck(True, "claims:publish_execution_tracker", "consistent"))

    if req_claims_live and not public_ready:
        out.append(
            GateCheck(
                False,
                "claims:submission_requirements",
                "requirements doc marks MCP Registry as Live before public /mcp endpoint is ready",
            )
        )
    else:
        out.append(GateCheck(True, "claims:submission_requirements", "consistent"))
    return out


def _channel_gates(mode: str, public_ready: bool) -> list[GateCheck]:
    # Purpose: make explicit what is allowed to publish right now.
    out = [
        GateCheck(True, "channel:GitHub Release", "allowed"),
        GateCheck(True, "channel:Custom Domain Docs", "allowed"),
        GateCheck(True, "channel:MCP.so", "allowed (use local config unless public endpoint exists)"),
        GateCheck(True, "channel:Glama MCP", "allowed"),
        GateCheck(True, "channel:Awesome MCP Servers", "allowed (PR review queue)"),
    ]
    if mode == "local":
        registry_ok = True
        registry_details = "intentionally blocked in local mode (public publish deferred)"
        pulse_details = "intentionally blocked in local mode (depends on MCP Registry publish)"
    else:
        registry_ok = public_ready
        registry_details = "allowed" if registry_ok else "blocked (requires public HTTPS /mcp endpoint + auth)"
        pulse_details = "allowed" if registry_ok else "blocked (depends on official MCP Registry publication)"

    out.append(
        GateCheck(
            registry_ok,
            "channel:MCP Registry (official)",
            registry_details,
        )
    )
    out.append(
        GateCheck(
            registry_ok,
            "channel:PulseMCP",
            pulse_details,
        )
    )
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description="Enforce launch sequencing gates for ContextLattice submissions.")
    parser.add_argument("--mode", choices=("local", "public"), default="local")
    parser.add_argument("--timeout", type=int, default=10)
    args = parser.parse_args()

    checks: list[GateCheck] = []
    checks.append(_run_submission_preflight(online=(args.mode == "public")))

    remote = _load_registry_remote_url()
    checks.append(remote)

    public_ready = False
    if args.mode == "public":
        if remote.ok:
            probe = _probe_public_mcp(remote.details, timeout=args.timeout)
            checks.append(probe)
            public_ready = probe.ok
        else:
            checks.append(GateCheck(False, "public_mcp_probe", "skipped (manifest missing/invalid)"))
    else:
        # Local mode intentionally does not require public endpoint readiness.
        checks.append(GateCheck(True, "public_mcp_probe", "skipped in local mode"))

    checks.extend(_consistency_claim_checks(public_ready=public_ready))
    checks.extend(_channel_gates(mode=args.mode, public_ready=public_ready))

    failing = [c for c in checks if not c.ok]
    print(f"Launch lock mode: {args.mode}")
    for c in checks:
        state = "PASS" if c.ok else "FAIL"
        print(f"[{state}] {c.name} :: {c.details}")

    print(f"\nSummary: {len(checks) - len(failing)}/{len(checks)} checks passed.")
    if failing:
        print("Result: NO-GO")
        return 1
    print("Result: GO")
    return 0


if __name__ == "__main__":
    sys.exit(main())
