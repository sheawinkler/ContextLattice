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
          "mercury": "MERCURY_HOME", "hermes-ultra": "HERMES_HOME", "droid": "HOME"}
NAME = "contextlattice-recall"
MARKET = "contextlattice-local"
ID = "contextlattice@" + MARKET


def require(ok, message):
    if not ok:
        raise Conflict(message)


def one(rows):
    require(len(rows) == 1, "expected exactly one matching resource")
    return rows[0]


def droid_guide(profile, market, inventory):
    def record(key):
        path = profile / key
        require(path == path.resolve() and path.is_file() and path.stat().st_size <= 4 * 1024 * 1024,
                "invalid Droid registration file")
        value = json.loads(path.read_bytes())
        require(isinstance(value, dict), "invalid Droid registration")
        return value

    lines = inventory.decode().splitlines()
    active = False
    matches = 0
    for line in lines:
        if not line.startswith((" ", "\t")) and line.rstrip().endswith(":"):
            active = line.strip() == "Active:"
        elif active and re.fullmatch(r"\s*" + re.escape(ID) + r"\s+\[user\]\s+\S+\s*", line):
            matches += 1
    require(matches == 1, "Droid did not list exactly one active user plugin")
    settings = record("settings.json")
    known = record("plugins/known_marketplaces.json")[MARKET]
    source = {"source": "local", "path": str(market)}
    require(known["source"] == source and known["installLocation"] == str(market)
            and settings["enabledPlugins"][ID] is True and settings["extraKnownMarketplaces"][MARKET]["source"] == source,
            "Droid marketplace source or enablement mismatch")
    row = one(record("plugins/installed_plugins.json")["plugins"][ID])
    version = row["version"]
    require(isinstance(version, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}", version),
            "invalid Droid cache version")
    parent = profile / "plugins/cache" / MARKET / "contextlattice" / version
    file = parent / "skills" / NAME / "SKILL.md"
    require(row["scope"] == "user" and row["source"] == MARKET and row["installPath"] == str(parent)
            and file == file.resolve() and file.is_file() and file.stat().st_size <= 65536,
            "Droid registration does not own the expected cached skill")
    return file


def verify(host, profile, dist, binary):
    guide = (SOURCE / SKILL).read_bytes()
    env = {**os.environ, NATIVE[host]: str(profile)}
    if host == "droid":
        require(profile.name == ".factory", "select the explicit native .factory profile")
        env["HOME"] = str(profile.parent)
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
    if host == "droid":
        file = droid_guide(profile, kit, native("plugin", "list", "--scope", "user"))
        staged = kit / SKILL
        require(staged == staged.resolve() and staged.is_file() and staged.stat().st_size == len(guide)
                and file.read_bytes() == guide == staged.read_bytes(), "Droid cached skill differs from the guide")
        return "native-active-user-plugin-and-cached-bytes"
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
