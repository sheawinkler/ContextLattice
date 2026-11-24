"""Verify a direct-load Gemini extension and Droid personal skill, without inference."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import uuid


KIT = Path(__file__).resolve().parents[1]
NAME = "contextlattice-recall"


def check(ok, label):
    if not ok:
        raise RuntimeError(label)


def probe(root):
    home, work, dist, backups, tmp = (root / k for k in ("home", "work", "dist", "backups", "tmp"))
    gemini, droid = home / ".gemini", home / ".factory"
    for p in (home, work, dist, backups, tmp, gemini, droid):
        p.mkdir()
    env = {"HOME": str(home), "PATH": os.environ["PATH"], "TMPDIR": str(tmp), "SHELL": "/bin/zsh",
           "XDG_CONFIG_HOME": str(home / ".config"), "XDG_CACHE_HOME": str(home / ".cache"),
           "XDG_DATA_HOME": str(home / ".data"), "XDG_STATE_HOME": str(home / ".state"),
           "GIT_CEILING_DIRECTORIES": str(root), "CI": "1", "NO_COLOR": "1", "DO_NOT_TRACK": "1"}
    policy = ('(version 1)(allow default)(deny network*)(deny file-write*)(deny process-exec (regex #"/bash$"))'
              f'(allow file-write* (subpath {json.dumps(str(root))}) (literal "/dev/null") (literal "/dev/tty"))')
    def run(*args, stdin=None, ok=True):
        p = subprocess.run(["/usr/bin/sandbox-exec", "-p", policy, *map(str, args)], env=env, cwd=work,
                           input=stdin, capture_output=True, text=True, timeout=15)
        check((p.returncode == 0) == ok, f"{Path(args[0]).name} exit {p.returncode}")
        return p.stdout + p.stderr

    def manage(action, host, profile, ok=True):
        opts = ["--backup-dir", backups] if action == "uninstall" else ["--dist", dist]
        return json.loads(run(sys.executable, "-B", KIT / "extra_profiles.py", action, host,
                              "--profile", profile, *opts, ok=ok))

    def droid_list():
        # Request only the local skill inventory. No session initialization, user prompt or daemon.
        key = str(uuid.uuid4())
        req = {"jsonrpc": "2.0", "type": "request", "factoryApiVersion": "1.0.0",
               "factoryProtocolVersion": "1.107.0", "id": key, "method": "droid.list_skills", "params": {}}
        output = run("droid", "exec", "--input-format", "stream-jsonrpc", "--output-format", "stream-jsonrpc",
                     stdin=json.dumps(req) + "\n")
        rows = [json.loads(s) for s in output.splitlines() if s.startswith("{")]
        response, = [r for r in rows if r.get("id") == key]
        return [r for r in response["result"]["skills"] if r["name"] == NAME]

    run(sys.executable, "-B", KIT / "manage.py", "install", "--kind", "marketplace", "--root", dist)
    expected = (dist / "contextlattice-local/plugins/contextlattice/skills" / NAME / "SKILL.md").read_bytes()
    for host, profile in (("droid", droid), ("gemini", gemini)):
        check(manage("install", host, profile)["state"] == "installed", "clean install")
        check(manage("install", host, profile)["state"] == "unchanged", "idempotence")
    ext = gemini / "extensions" / NAME
    inventory = run("gemini", "extensions", "list")
    check("Enabled (User): true" in inventory and "Agent skills:" in inventory and
          f"Path: {ext}" in inventory, "Gemini did not enable the extension")
    skills = run("gemini", "skills", "list", "--all")
    check(skills.count(NAME + " [Enabled]") == 1 and str(ext / "skills" / NAME / "SKILL.md") in skills,
          "Gemini did not resolve the extension skill exactly once")
    row, = droid_list()
    droid_file = droid / "skills" / NAME / "SKILL.md"
    check(row["filePath"] == str(droid_file) and row["enabled"] and row["userInvocable"] and
          row["location"] == "personal" and row["content"].strip() == expected.decode().split("---", 2)[2].strip(),
          "Droid did not return the expected skill body")
    check(manage("install", "gemini-skill", gemini, ok=False)["state"] == "refused", "double registration")
    settings = droid / "settings.json"
    settings.write_bytes(b'{"logoAnimation":false}\n')
    droid_file.write_bytes(b"operator edit")
    check(manage("uninstall", "droid", droid, ok=False)["state"] == "refused", "modified skill")
    droid_file.write_bytes(expected)
    for host, profile in (("droid", droid), ("gemini", gemini)):
        saved = backups / manage("uninstall", host, profile)["backup"]
        key = Path("skills") / NAME / "SKILL.md" if host == "gemini" else Path("SKILL.md")
        check((saved / key).read_bytes() == expected, "backup bytes")
    check(not droid_list() and NAME not in run("gemini", "extensions", "list") and
          NAME + " [Enabled]" not in run("gemini", "skills", "list", "--all"), "native absence after rollback")
    check(settings.read_bytes() == b'{"logoAnimation":false}\n', "unrelated settings changed")
    return {"gemini": "native-extension-and-bundled-skill", "droid": "native-personal-skill-body",
            "guide_sha256": hashlib.sha256(expected).hexdigest(), "rollback": "both-absent",
            "network": "denied", "model_turns": 0}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scratch-root", type=Path, required=True)
    args = parser.parse_args()
    if not all(shutil.which(x) for x in ("gemini", "droid", "sandbox-exec")):
        parser.exit(2, "requires installed Gemini, Droid and macOS sandbox-exec\n")
    with tempfile.TemporaryDirectory(prefix="extra-", dir=args.scratch_root.resolve(strict=True)) as d:
        print(json.dumps(probe(Path(d)), sort_keys=True))
