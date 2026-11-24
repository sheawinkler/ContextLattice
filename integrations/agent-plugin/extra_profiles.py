#!/usr/bin/env python3
"""Install the durable marketplace skill in one additional, explicit host profile."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile

from manage import Conflict, RECEIPT, SKILL, directory, parents, read_owned, stat_kind, write_owned


NAME = "contextlattice-recall"
HOSTS = ("pi", "omp", "droid", "gemini", "gemini-skill")


def stable_files(dist, extension):
    market = directory(str(dist / "contextlattice-local"))
    leaf, record = market / SKILL, market / RECEIPT
    for path in (leaf, record):
        if path != path.resolve() or not stat.S_ISREG(stat_kind(path)) or path.stat().st_size > 65536:
            raise Conflict("expected a regular skill and ownership receipt in the stable marketplace")
    guide = leaf.read_bytes()
    receipt = json.loads(record.read_bytes())
    if (not isinstance(receipt, dict) or receipt.get("kind") != "marketplace" or
            receipt.get("schema") != "contextlattice.agent-resource.v1" or
            not isinstance(receipt.get("sha256"), dict) or
            receipt["sha256"].get(SKILL.as_posix()) != hashlib.sha256(guide).hexdigest()):
        raise Conflict("stable skill does not match the marketplace receipt")
    if not extension:
        return {"SKILL.md": guide}
    manifest = Path(__file__).with_name("gemini") / "gemini-extension.json"
    return {"gemini-extension.json": manifest.read_bytes(), f"skills/{NAME}/SKILL.md": guide}


def apply(action, host, profile, dist, backup_dir):
    extension = host == "gemini"
    container = profile / ("extensions" if extension else "skills")
    if container != container.resolve():
        raise Conflict("use a physical host resource directory, not a symlink")
    target = container / NAME
    kind = "gemini-extension" if extension else "skill"
    keys = ("gemini-extension.json", f"skills/{NAME}/SKILL.md") if extension else ("SKILL.md",)
    files = None if action == "uninstall" else stable_files(dist, extension)
    if action == "status":
        if not stat_kind(target):
            return {"state": "absent"}
        return {"state": "installed" if read_owned(target, kind, keys) == files else "different-version"}
    if action == "install":
        conflicts = [Path.home() / ".agents/skills" / NAME]
        if host.startswith("gemini"):
            conflicts.append(profile / ("skills" if extension else "extensions") / NAME)
        if any(stat_kind(p) for p in conflicts if p != target):
            raise Conflict("another personal skill or extension already occupies this name")
        container.mkdir(mode=0o700, exist_ok=True)
    if not stat_kind(container):
        return {"state": "absent"}
    if not stat.S_ISDIR(stat_kind(container)):
        raise Conflict("host resource root is not a directory")
    lock = container / ("." + NAME + ".install-lock")
    lock.mkdir(mode=0o700)
    try:
        if not stat_kind(target):
            if action != "install":
                return {"state": "absent"}
            target.mkdir(mode=0o700)
            write_owned(target, kind, files)
            if read_owned(target, kind, files) != files:
                raise Conflict("install verification failed")
            return {"state": "installed"}
        installed = read_owned(target, kind, keys)
        if action != "uninstall":
            if installed != files:
                raise Conflict("owned version differs; back up and uninstall before replacing")
            return {"state": "unchanged" if action == "install" else "installed"}
        if not backup_dir:
            raise Conflict("--backup-dir is required for removal")
        backups = directory(backup_dir)
        if any(backups == p or p in backups.parents for p in (profile, Path.home() / ".agents/skills")):
            raise Conflict("back up outside the profile and shared skill discovery directories")
        saved = Path(tempfile.mkdtemp(prefix=host + "-" + NAME + "-", dir=backups))
        write_owned(saved, kind, installed)
        if read_owned(saved, kind, keys) != installed or read_owned(target, kind, keys) != installed:
            raise Conflict("backup or target changed during verification")
        for key in [*installed, RECEIPT]:
            (target / key).unlink()
        for path in sorted(parents(installed), key=lambda p: len(p.parts), reverse=True):
            (target / path).rmdir()
        target.rmdir()
        return {"state": "removed", "backup": saved.name}
    finally:
        lock.rmdir()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("install", "status", "uninstall"))
    parser.add_argument("host", choices=HOSTS)
    parser.add_argument("--profile", required=True, help="explicit existing host profile directory")
    parser.add_argument("--dist", help="stable parent of contextlattice-local (not needed for rollback)")
    parser.add_argument("--backup-dir")
    args = parser.parse_args()
    if args.action != "uninstall" and not args.dist:
        parser.error("--dist is required for install and status")
    os.umask(0o077)
    try:
        result = apply(args.action, args.host, directory(args.profile),
                       directory(args.dist) if args.action != "uninstall" else None, args.backup_dir)
    except (ValueError, OSError) as exc:
        reason = str(exc) if isinstance(exc, Conflict) else type(exc).__name__
        parser.exit(1, json.dumps({"state": "refused", "reason": reason}) + "\n")
    print(json.dumps({"host": args.host, **result}, sort_keys=True))
