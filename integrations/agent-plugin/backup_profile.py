#!/usr/bin/env python3
"""Snapshot only the settings a local native package manager may update."""

import argparse
import hashlib
import json
import os
import stat
import tempfile
from pathlib import Path

from manage import Conflict, directory, stat_kind


SETTINGS = {
    "codex": ("config.toml",),
    "claude-code": ("settings.json", "plugins/known_marketplaces.json", "plugins/installed_plugins.json"),
    "prime-agent": ("settings.json",),
    "mercury": ("plugins/state.json",),
}


def snapshot(host, profile, backups):
    if profile == backups or profile in backups.parents:
        raise Conflict("choose a separate private backup directory outside the profile")
    contents = {}
    for key in SETTINGS[host]:
        p = profile / key
        if p != p.resolve() or (stat_kind(p) and not stat.S_ISREG(stat_kind(p))):
            raise Conflict("refusing linked or special settings file")
        contents[key] = p.read_bytes() if p.exists() else None
    dest = Path(tempfile.mkdtemp(prefix=host + "-before-", dir=backups))
    manifest = {}
    for key, data in contents.items():
        manifest[key] = None
        if data is not None:
            p = dest / key
            p.parent.mkdir(exist_ok=True, parents=True)
            p.write_bytes(data)
            if p.read_bytes() != data:
                raise Conflict("backup verification failed")
            manifest[key] = hashlib.sha256(data).hexdigest()
    (dest / "snapshot.json").write_text(json.dumps(manifest, sort_keys=True) + "\n")
    return {"backup": dest.name, "host": host, "files": len([v for v in contents.values() if v is not None])}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", choices=SETTINGS)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--backup-dir", required=True)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        result = snapshot(args.host, directory(args.profile), directory(args.backup_dir))
    except (Conflict, OSError) as exc:
        parser.exit(1, (exc.strerror if isinstance(exc, OSError) else str(exc)) + "\n")
    print(json.dumps(result, sort_keys=True))
