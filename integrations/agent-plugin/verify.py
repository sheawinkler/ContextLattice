#!/usr/bin/env python3
"""Check native discovery and artifact identity without sending a model turn."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

from manage import Conflict, SKILL, SOURCE, directory, payload


NATIVE = {"codex": "CODEX_HOME", "claude": "CLAUDE_CONFIG_DIR", "prime-agent": "PRIME_AGENT_CODING_AGENT_DIR",
          "mercury": "MERCURY_HOME", "hermes-ultra": "HERMES_HOME"}
NAME = "contextlattice-recall"
MARKET = "contextlattice-local"
ID = "contextlattice@" + MARKET


def require(ok, message):
    if not ok:
        raise Conflict(message)


def one(rows):
    require(len(rows) == 1, "expected exactly one matching resource")
    return rows[0]


def verify(host, profile, dist, binary):
    guide = (SOURCE / SKILL).read_bytes()
    env = {**os.environ, NATIVE[host]: str(profile)}
    def native(*args):
        p = subprocess.run([binary or host, *args], capture_output=True, env=env, timeout=30)
        require(p.returncode == 0, "native " + " ".join(args[:2]) + " returned " + str(p.returncode))
        return p.stdout
    if host == "hermes-ultra":
        require(native("--version").startswith(b"hermes-agent-ultra "), "not the Rust Ultra runtime")
        body = native("skills", "inspect", NAME)
        require(body in (guide, guide + b"\n"), "named native selection did not return the shared guide")
        return "named-skill-inspect"

    require(dist is not None, "explicit --dist is required to verify the selected source")
    kit = dist / MARKET
    skill = Path("skills") / NAME / "SKILL.md"
    if host == "codex":
        row = one([r for r in json.loads(native("plugin", "list", "--marketplace", MARKET, "--json"))["installed"]
                   if r["pluginId"] == ID])
        require(row["installed"] and row["enabled"] and row["marketplaceSource"]["source"] == str(kit),
                "native registration disabled or points to another marketplace")
        version = row["version"]
        require(re.fullmatch(r"[0-9A-Za-z.+-]+", version), "invalid host version")
        file = profile / "plugins/cache" / MARKET / "contextlattice" / version / skill
    elif host == "claude":
        row = one([r for r in json.loads(native("plugin", "list", "--json")) if r["id"] == ID])
        require(row["enabled"] and row["scope"] == "user", "not an enabled user plugin")
        known = json.loads((profile / "plugins/known_marketplaces.json").read_bytes())[MARKET]
        require(known["source"].get("path") == str(kit), "marketplace points elsewhere")
        require(b"Skills (1)  contextlattice-recall" in native("plugin", "details", ID),
                "native component inventory does not contain exactly the expected skill")
        file = Path(row["installPath"]) / skill
    elif host == "prime-agent":
        row = one([r for r in json.loads(native("skills", "--json"))["skills"] if r["name"] == NAME])
        file = Path(row["path"])
        require(row["state"] == "active" and file == kit / "plugins/contextlattice" / skill,
                "inactive, shadowed, or unexpected capability package")
    else:
        info = native("plugins", "info", "contextlattice")
        require(b"contextlattice__recall_guide" in info and b"/contextlattice" in info,
                "native inventory lacks the tool or command")
        row = json.loads((profile / "plugins/state.json").read_bytes())["plugins"]["contextlattice"]
        require(row["enabled"] and row["kind"] == "in-process" and row["source"]["ref"] == str(dist / "contextlattice"),
                "disabled, non-native, or different source")
        version = row["version"]
        require(re.fullmatch(r"[0-9A-Za-z.+-]+", version), "invalid host version")
        installed = profile / "plugins/contextlattice/versions" / version
        require(all((installed / key).read_bytes() == data for key, data in payload("mercury").items()),
                "installed Mercury adapter differs from this distribution")
        return "native-command-and-tool-inventory"
    require(file.read_bytes() == guide, "installed skill digest differs from this distribution")
    return "native-discovery-and-installed-bytes"


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", choices=NATIVE)
    parser.add_argument("--profile", required=True, help="explicit host config directory")
    parser.add_argument("--dist", help="parent of the staged marketplace or Mercury plugin")
    parser.add_argument("--binary", help="explicit installed host executable, if not on PATH")
    args = parser.parse_args()
    try:
        grade = verify(args.host, directory(args.profile), directory(args.dist) if args.dist else None, args.binary)
    except (OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as exc:
        reason = str(exc) if isinstance(exc, Conflict) else type(exc).__name__
        parser.exit(1, json.dumps({"verified": False, "host": args.host, "reason": reason}) + "\n")
    print(json.dumps({"host": args.host, "verified": True, "grade": grade, "model_turns": 0,
                      "guide_sha256": hashlib.sha256((SOURCE / SKILL).read_bytes()).hexdigest()}))
