#!/usr/bin/env python3
"""Materialize or roll back one local resource; never edit shared host config."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile


SOURCE = Path(__file__).resolve().parent
PLUGIN = Path("plugins/contextlattice")
SKILL = PLUGIN / "skills/contextlattice-recall/SKILL.md"
NAMES = {"marketplace": "contextlattice-local", "skill": "contextlattice-recall",
         "mercury": "contextlattice", "project-mcp": "contextlattice-project-mcp"}
RECEIPT = ".contextlattice-install.json"


class Conflict(ValueError):
    pass


def payload(kind, project=None):
    if kind == "project-mcp":
        from project_mcp import bound_args
        try:
            argv = ["contextlattice", *bound_args(project)]
        except ValueError as exc:
            raise Conflict(str(exc)) from None
        return {"launch": ("#!/usr/bin/env python3\nimport os\nos.execvp('contextlattice', "
                           + repr(argv) + ")\n").encode("ascii")}
    skill = (SOURCE / SKILL).read_bytes()
    if kind == "skill":
        return {"SKILL.md": skill}
    if kind == "mercury":
        # Mercury loads one self-contained ES module. No filesystem, subprocess,
        # network or credentials in its activation context.
        prefix = "const guide = " + json.dumps(skill.decode("utf-8")) + ";\n"
        return {
            "mercury-plugin.json": (SOURCE / "mercury/mercury-plugin.json").read_bytes(),
            "index.mjs": prefix.encode() + (SOURCE / "mercury/activate.mjs").read_bytes(),
        }
    paths = [Path(".agents/plugins/marketplace.json"), Path(".claude-plugin/marketplace.json"),
             PLUGIN / ".codex-plugin/plugin.json", PLUGIN / ".claude-plugin/plugin.json",
             PLUGIN / "package.json", SKILL]
    return {p.as_posix(): (SOURCE / p).read_bytes() for p in paths}


def hashes(files):
    return {key: hashlib.sha256(value).hexdigest() for key, value in files.items()}


def directory(arg):
    path = Path(arg).expanduser()
    if not path.is_absolute() or path != path.resolve() or not path.is_dir():
        raise Conflict("use an existing absolute physical directory (no symlinks)")
    return path


def stat_kind(path):
    try:
        return path.lstat().st_mode
    except FileNotFoundError:
        return 0


def parents(files):
    return {p for name in files for p in Path(name).parents if p != Path(".")}


def read_owned(target, kind, paths):
    """Enumerate before reading. Never follow an unexpected symlink or special file."""
    if not stat.S_ISDIR(stat_kind(target)):
        raise Conflict("target is not an owned directory")
    allowed = {*paths, RECEIPT}
    dirs = parents(allowed)
    files = {}
    stack = [target]
    while stack:
        for p in stack.pop().iterdir():
            rel = p.relative_to(target)
            mode = stat_kind(p)
            if rel in dirs and stat.S_ISDIR(mode):
                stack.append(p)
            elif rel.as_posix() in allowed and stat.S_ISREG(mode) and p.stat().st_size <= 65536:
                if kind == "project-mcp" and rel.name == "launch" and not mode & stat.S_IXUSR:
                    raise Conflict("project launcher is not executable")
                files[rel.as_posix()] = p.read_bytes()
            else:
                raise Conflict("unowned entry, symlink, or oversized file at target")
    if files.keys() != allowed:
        raise Conflict("unowned or incomplete target; no files changed")
    try:
        recorded = json.loads(files.pop(RECEIPT))
    except (ValueError, UnicodeDecodeError):
        raise Conflict("invalid ownership receipt") from None
    if recorded != receipt(kind, files):
        raise Conflict("modified payload or ownership receipt; no files changed")
    return files


def receipt(kind, files):
    return {"schema": "contextlattice.agent-resource.v1", "kind": kind, "sha256": hashes(files)}


def write_owned(target, kind, files):
    # The caller claims a new directory. Publish ownership last; an interrupted
    # install remains an explicit collision, never an implicitly adopted tree.
    for key, data in {**files, RECEIPT: json.dumps(receipt(kind, files), sort_keys=True).encode()}.items():
        path = target / key
        path.parent.mkdir(parents=True, exist_ok=True)
        if kind == "project-mcp" and key == "launch":
            out = os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o700), "wb")
        else:
            out = path.open("xb")
        with out:
            out.write(data)


def operate(args):
    root = directory(args.root)
    kind = args.kind
    name = NAMES[kind]
    target = root / name
    expected = payload(kind, args.project)

    def current():
        if not stat_kind(target):
            return None
        files = read_owned(target, kind, expected)
        if kind == "project-mcp" and files != expected:
            raise Conflict("launcher bound to another project; no files changed")
        return files

    if args.action == "status":
        installed = current()
        return {"state": "absent" if installed is None else
                "installed" if installed == expected else "different-version"}

    lock = root / ("." + name + ".install-lock")
    try:
        lock.mkdir(mode=0o700)
    except FileExistsError:
        raise Conflict("resource busy or interrupted; inspect the install lock") from None
    try:
        installed = current()
        if args.action == "install":
            if installed is not None:
                if installed != expected:
                    raise Conflict("different owned version; back up and uninstall it first")
                return {"state": "unchanged"}
            target.mkdir(mode=0o700)
            write_owned(target, kind, expected)
            if current() != expected:
                raise Conflict("installed bytes did not verify")
            return {"state": "installed"}
        if installed is None:
            return {"state": "absent"}
        if not args.backup_dir:
            raise Conflict("backup or uninstall requires --backup-dir")
        backups = directory(args.backup_dir)
        if target == backups or target in backups.parents:
            raise Conflict("backup directory must be outside the owned target")
        backup = Path(tempfile.mkdtemp(prefix=name + "-", dir=backups))
        write_owned(backup, kind, installed)
        if read_owned(backup, kind, expected) != installed or current() != installed:
            raise Conflict("backup or target changed; refusing removal")
        if args.action == "backup":
            return {"state": "backed-up", "backup": backup.name}
        for key in [*installed, RECEIPT]:
            (target / key).unlink()
        for rel in sorted(parents(installed), key=lambda p: len(p.parts), reverse=True):
            (target / rel).rmdir()
        target.rmdir()
        return {"state": "removed", "backup": backup.name}
    finally:
        lock.rmdir()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("status", "install", "backup", "uninstall"))
    parser.add_argument("--kind", required=True, choices=NAMES)
    parser.add_argument("--root", required=True, help="existing parent directory; never inferred from HOME")
    parser.add_argument("--backup-dir", help="existing private backup parent, outside the target")
    parser.add_argument("--project", help="required only for an opt-in project-mcp launcher")
    args = parser.parse_args()
    if args.project is not None and args.kind != "project-mcp":
        parser.error("--project applies only to project-mcp")
    os.umask(0o077)
    try:
        result = operate(args)
    except (Conflict, OSError) as exc:
        reason = exc.strerror if isinstance(exc, OSError) else str(exc)
        print(json.dumps({"state": "refused", "reason": reason}))
        return 1
    print(json.dumps({"kind": args.kind, **result}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
