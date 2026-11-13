#!/usr/bin/env python3
"""Prepare one distribution bwrap profile on an explicitly owned hosted CI runner."""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tarfile
import tempfile
import time

from release_native_toolchain_preflight import ROOT, probe


VERSION = "4.0.1really4.0.1-0ubuntu0.24.04.9"
PACKAGES = {
    "apparmor-profiles": "90b02aa006eea7702cd4e851343e469e41365dda42145a3cb035de1d6c773b8c",
    "apparmor": "59af23f557d666039708ec176aa056d2fd85125dabd452accf6d86f5c608fb74",
}
PROFILE = "usr/share/apparmor/extra-profiles/bwrap-userns-restrict"
PROFILE_SHA = "11d39094f044f0cda0febb3ad517b830301da6b2ce929664af09ee9e4dd264f9"
PARSER = "/usr/sbin/apparmor_parser"


def require(condition: bool, code: str) -> None:
    if not condition:
        raise ValueError(code)


def run(argv: list[str], *, cwd: Path | None = None, data: bytes | None = None) -> bytes:
    result = subprocess.run(argv, cwd=cwd, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=60, check=False, env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"})
    require(result.returncode == 0 and len(result.stdout) <= 4 * 1024 * 1024, "fixed_command_failed:" + Path(argv[0]).name)
    return result.stdout


def trusted_runner(environment: dict[str, str], system: str, release: str, uid: int) -> bool:
    return (system == "linux" and uid != 0 and release == 'ubuntu:24.04' and
            environment.get("GITHUB_ACTIONS") == "true" and environment.get("RUNNER_ENVIRONMENT") == "github-hosted" and
            environment.get("GITHUB_REPOSITORY") in {"sheawinkler/ContextLattice", "sheawinkler/http-context-and-memory-orchestrator"} and
            environment.get("GITHUB_EVENT_NAME") in {"pull_request", "workflow_dispatch", "push"} and
            environment.get("CONTEXTLATTICE_OWNED_NATIVE_CI") == "true")


def attributed_denial(observation: dict, rows: list[dict]) -> bool:
    tools = observation.get("tools", [])
    if len(tools) != 2 or {item.get("tool") for item in tools} != {"msiinfo", "msiextract"}:
        return False
    for item in tools:
        if (item.get("returncode") != 1 or item.get("output_truncated") or
                item.get("stderr") != "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n"):
            return False
        if not any(float(item["started_epoch"]) <= int(row.get("__REALTIME_TIMESTAMP", "0")) / 1_000_000 <= float(item["completed_epoch"]) + 1 and
                   all(token in row.get("MESSAGE", "") for token in (
                       'apparmor="DENIED"', 'operation="capable"', 'profile="unprivileged_userns"', 'comm="bwrap"', 'capname="net_admin"'))
                   for row in rows):
            return False
    return True


def fixed_binary(path: str, package: str) -> dict:
    binary = Path(path)
    require(binary.resolve() == binary and binary.is_file(), "noncanonical_binary")
    for ancestor in [binary, *binary.parents]:
        info = ancestor.stat()
        require(info.st_uid == 0 and not info.st_mode & 0o022 and not info.st_mode & (stat.S_ISUID | stat.S_ISGID), "unsafe_binary_ownership")
    # Dpkg can retain /sbin paths while usr-merged systems resolve them under
    # /usr/sbin. Verify the listed package member's canonical file, not spelling.
    members = run(["/usr/bin/dpkg-query", "-L", package]).decode().splitlines()
    require(any(member.startswith("/") and Path(member).resolve() == binary for member in members), "wrong_binary_package")
    require(not run(["/usr/sbin/getcap", path]).strip(), "unexpected_file_capabilities")
    return {"path": path, "sha256": hashlib.sha256(binary.read_bytes()).hexdigest()}


def unpack_reviewed_data(package: Path, expected: str, destination: Path, *, profiles: bool) -> None:
    require(hashlib.sha256(package.read_bytes()).hexdigest() == expected, "distribution_package_hash_mismatch")
    raw = run(["/usr/bin/dpkg-deb", "--fsys-tarfile", str(package)])
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
        for member in archive:
            name = member.name.removeprefix("./")
            selected = name == PROFILE if profiles else name.startswith("etc/apparmor.d/")
            if not selected or member.isdir():
                continue
            require(member.isfile() and ".." not in Path(name).parts and not name.startswith("/"), "unsafe_distribution_member")
            require(member.size <= 512 * 1024, "oversized_distribution_member")
            target = destination / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(archive.extractfile(member).read())


def wait_attributed_denials(before: dict, report: dict) -> list[dict]:
    journal = ["/usr/bin/sudo", "-n", "/usr/bin/journalctl", "-k", "--since", "@" + str(int(min(item["started_epoch"] for item in before["tools"]))),
               "--no-pager", "--output=json", '--grep=apparmor="DENIED".*(bwrap|unprivileged_userns)']
    deadline = time.monotonic() + 2
    while True:
        result = subprocess.run(journal, capture_output=True, check=False, timeout=5)
        require(result.returncode in (0, 1) and len(result.stdout) <= 1024 * 1024, "kernel_denial_read_failed")
        rows = [json.loads(line) for line in result.stdout.splitlines()]
        report["kernel_denials"], report["kernel_read_attempts"] = rows, report.get("kernel_read_attempts", 0) + 1
        if attributed_denial(before, rows) or time.monotonic() >= deadline:
            return rows
        time.sleep(.05)


def prepare(report: dict) -> dict:
    release = dict(line.split("=", 1) for line in Path("/etc/os-release").read_text().splitlines() if "=" in line)
    require(trusted_runner(dict(os.environ), sys.platform, release.get("ID", "").strip('"') + ":" + release.get("VERSION_ID", "").strip('"'), os.geteuid()), "not_owned_hosted_ubuntu_ci")
    if os.environ["GITHUB_EVENT_NAME"] == "pull_request":
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
        require(event["pull_request"]["head"]["repo"]["full_name"] == os.environ["GITHUB_REPOSITORY"], "fork_profile_activation_refused")
    require(Path("/proc/self/attr/current").read_text().strip() == "unconfined", "stacked_parent_policy_unknown")
    require(Path("/proc/sys/kernel/apparmor_restrict_unprivileged_userns").read_text().strip() == "1", "unexpected_global_restriction")
    identities = [fixed_binary(path, package) for path, package in (
        ("/usr/bin/bwrap", "bubblewrap"), ("/usr/bin/msiinfo", "msitools"), ("/usr/bin/msiextract", "msitools"), (PARSER, "apparmor"))]
    require(run(["/usr/bin/dpkg-query", "-W", "-f=${Version}", "apparmor"]).decode() == VERSION, "unreviewed_parser_version")
    before = probe(ROOT)
    report.update({"before": before, "binary_identities": identities, "kernel_denials": [], "kernel_read_attempts": 0})
    # Journald ingestion is asynchronous. Wait only for the already-executed
    # probes' exact bounded events; never retry probes or loosen attribution.
    rows = wait_attributed_denials(before, report)
    require(attributed_denial(before, rows), "fresh_exact_denial_not_attributed")
    loaded = run(["/usr/bin/sudo", "-n", "/usr/bin/cat", "/sys/kernel/security/apparmor/profiles"]).decode()
    require(not re.search(r"(?m)^(bwrap|unpriv_bwrap)(?:\s|//|$)", loaded), "existing_profile_name_collision")
    # --add refuses profile-name replacement. Scan existing policy sources for
    # competing executable attachments; do not unload or replace unknown policy.
    for path in Path("/etc/apparmor.d").rglob("*"):
        if path.is_file():
            require(b"/usr/bin/bwrap" not in path.read_bytes(), "existing_bwrap_attachment_unknown")
    with tempfile.TemporaryDirectory(prefix="contextlattice-ci-profile-", dir=os.environ["RUNNER_TEMP"]) as directory:
        scratch = Path(directory)
        for name, digest in PACKAGES.items():
            run(["/usr/bin/apt-get", "download", name + "=" + VERSION], cwd=scratch)
            matches = list(scratch.glob(name + "_*.deb"))
            require(len(matches) == 1, "ambiguous_distribution_package")
            unpack_reviewed_data(matches[0], digest, scratch, profiles=name == "apparmor-profiles")
        profile = scratch / PROFILE
        require(hashlib.sha256(profile.read_bytes()).hexdigest() == PROFILE_SHA, "distribution_profile_hash_mismatch")
        options = [PARSER, "--config-file", "/dev/null", "--skip-cache", "--base", str(scratch / "etc/apparmor.d")]
        names = run([*options, "--names", str(profile)]).decode().splitlines()
        require(set(names) == {"bwrap", "unpriv_bwrap"}, "unexpected_distribution_profile_names")
        compiled = run([*options, "--stdout", str(profile)])
        require(0 < len(compiled) <= 2 * 1024 * 1024, "invalid_compiled_profile")
        # The only privileged mutation: add these two reviewed distribution
        # definitions from compiled stdin. No host include/cache/policy rewrite.
        run(["/usr/bin/sudo", "-n", PARSER, "--config-file", "/dev/null", "--skip-cache", "--binary", "--add"], data=compiled)
    after = probe(ROOT)
    require(after["ok"], "profile_did_not_restore_contained_version_execution")
    require(Path("/proc/sys/kernel/apparmor_restrict_unprivileged_userns").read_text().strip() == "1", "global_restriction_changed")
    return {"schema_id": "contextlattice_native_ci_profile.v1", "ok": True, "artifact_audit": False,
            "global_restriction_unchanged": True,
            "binary_identities": identities, "profile_sha256": PROFILE_SHA, "package_hashes": PACKAGES,
            "compiled_profile_sha256": hashlib.sha256(compiled).hexdigest(), "before": before, "kernel_denials": rows, "after": after}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prepare-github-hosted-ci", action="store_true")
    args = parser.parse_args()
    report = {"schema_id": "contextlattice_native_ci_profile.v1", "ok": False, "artifact_audit": False}
    try:
        require(args.prepare_github_hosted_ci, "explicit_ci_preparation_required")
        report.update(prepare(report))
    except (ValueError, OSError, subprocess.SubprocessError, KeyError) as exc:
        report["error"] = str(exc) if isinstance(exc, ValueError) else type(exc).__name__
    print(json.dumps(report, sort_keys=True))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
