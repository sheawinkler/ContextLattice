#!/usr/bin/env python3
"""Preserve real Syft components and bind the unchanged outer installer file."""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

INSTALLERS = frozenset(("ContextLattice-macOS-universal.dmg", "ContextLattice-windows-x64.msi", "ContextLattice-linux-bootstrap.tar.gz"))
SYFT_VERSION = "1.32.0"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def syft_document(document):
    require(isinstance(document, dict), "scanner document is not an object")
    require(document.get("spdxVersion") == "SPDX-2.3", "unsupported scanner SPDX version")
    require(document.get("SPDXID") == "SPDXRef-DOCUMENT", "invalid scanner document identity")
    require(isinstance(document.get("packages"), list), "scanner package list missing")
    require(isinstance(document.get("creationInfo"), dict), "scanner creation information missing")
    creators = document["creationInfo"].get("creators", [])
    require(f"Tool: syft-{SYFT_VERSION}" in creators, "scanner version does not match the pin")


def outer_file(document, name, sha):
    syft_document(document)
    files = document.get("files")
    require(isinstance(files, list), "scanner file list missing")
    rows = [row for row in files if isinstance(row, dict) and row.get("fileName") in (name, "./" + name, "/" + name)]
    require(len(rows) == 1, "outer installer file must have exactly one scanner record")
    row = rows[0]
    checksums = row.get("checksums")
    require(isinstance(checksums, list), "outer installer checksums missing")
    hashes = [item.get("checksumValue") for item in checksums if isinstance(item, dict) and item.get("algorithm") == "SHA256"]
    require(hashes == [sha], "outer installer scanner digest mismatch")
    require(isinstance(row.get("SPDXID"), str) and row["SPDXID"].startswith("SPDXRef-"), "outer installer scanner identity missing")
    require("LicenseRef-" not in json.dumps(row), "outer file has unsupported external license references")
    return row


def merge_documents(components, outer, name, sha):
    require(name in INSTALLERS and re.fullmatch(r"[0-9a-f]{64}", sha), "invalid final installer identity")
    syft_document(components)
    row = outer_file(outer, name, sha)
    result = copy.deepcopy(components)
    files = result.get("files", [])
    require(isinstance(files, list), "component file list is malformed")
    existing = [item for item in files if isinstance(item, dict) and item.get("fileName") in (name, "./" + name, "/" + name)]
    if existing:
        outer_file(result, name, sha)
        return result
    row = copy.deepcopy(row)
    row["SPDXID"] = "SPDXRef-Installer-" + sha
    row["fileName"] = "./" + name
    all_ids = []

    def collect(value):
        if isinstance(value, dict):
            if "SPDXID" in value:
                require(isinstance(value["SPDXID"], str), "malformed SPDX identity")
                all_ids.append(value["SPDXID"])
            for child in value.values():
                collect(child)
        elif isinstance(value, list):
            for child in value:
                collect(child)

    collect(result)
    require(len(all_ids) == len(set(all_ids)) and row["SPDXID"] not in all_ids, "colliding SPDX identity")
    result["files"] = files + [row]
    relationships = result.setdefault("relationships", [])
    require(isinstance(relationships, list), "component relationships malformed")
    relationships.append({"spdxElementId": "SPDXRef-DOCUMENT", "relatedSpdxElement": row["SPDXID"], "relationshipType": "DESCRIBES"})
    comment = result["creationInfo"].get("comment", "")
    require(isinstance(comment, str), "component creation comment malformed")
    result["creationInfo"]["comment"] = (comment + "\n" if comment else "") + (
        "ContextLattice release_sbom: component inventory preserved; outer installer record copied from a separate "
        "Syft 1.32.0 directory scan with all file metadata and SHA256. No package records are synthesized."
    )
    outer_file(result, name, sha)
    return result


def generate(syft, artifact, output, temp_root):
    require(artifact.name in INSTALLERS and artifact.is_file() and not artifact.is_symlink(), "installer must be a regular canonical file")
    require(syft.is_file() and not syft.is_symlink(), "scanner must be an explicit regular executable")
    sha = digest(artifact)
    tool_sha = digest(syft)
    with tempfile.TemporaryDirectory(prefix="release-sbom-", dir=temp_root) as temp:
        root = Path(temp)
        (root / "input").mkdir()
        staged = root / "input" / artifact.name
        shutil.copyfile(artifact, staged)
        require(digest(staged) == sha, "installer changed while staging")
        config = root / "syft.yaml"
        config.write_text("check-for-app-update: false\nfile:\n  metadata:\n    selection: all\n    digests: [sha256]\n", encoding="ascii")
        env = {"PATH": "/usr/bin:/bin", "HOME": str(root), "TMPDIR": str(root), "GOMAXPROCS": "2", "SYFT_CHECK_FOR_APP_UPDATE": "false"}
        documents = []
        for source in ("file:input/" + artifact.name, "dir:input"):
            completed = subprocess.run([str(syft), "scan", source, "--config", str(config), "--parallelism", "2", "-o", "spdx-json"],
                                       cwd=root, env=env, capture_output=True, timeout=600, check=True)
            documents.append(json.loads(completed.stdout))
        result = merge_documents(*documents, artifact.name, sha)
        require(digest(artifact) == sha and digest(staged) == sha and digest(syft) == tool_sha, "installer or scanner changed during generation")
        raw = (json.dumps(result, indent=2, sort_keys=True) + "\n").encode()
        if output.exists():
            require(output.read_bytes() == raw, "refusing to replace an existing different SBOM")
        else:
            with output.open("xb") as stream:
                stream.write(raw)
        return {"ok": True, "artifact": artifact.name, "sha256": sha, "syft_version": SYFT_VERSION, "syft_sha256": tool_sha,
                "component_scan": True, "outer_file_scan": True, "artifact_audit": False,
                "component_scope": "syft_file_source_not_complete_payload_inventory"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--syft", required=True)
    parser.add_argument("--artifact", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--temp-root", required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(generate(Path(args.syft).absolute(), Path(args.artifact).absolute(), Path(args.output).absolute(), args.temp_root), sort_keys=True))
    except (ValueError, OSError, subprocess.SubprocessError, json.JSONDecodeError) as exc:
        print(json.dumps({"ok": False, "error": type(exc).__name__, "artifact_audit": False}))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
