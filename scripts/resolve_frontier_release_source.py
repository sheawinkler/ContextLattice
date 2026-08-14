#!/usr/bin/env python3
"""Resolve a fail-closed private source approval for immutable paid releases."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any


AMENDMENTS_SCHEMA_ID = "contextlattice_frontier_release_source_amendments.v1"
RESOLUTION_SCHEMA_ID = "contextlattice_frontier_release_source_resolution.v1"
COMMIT_RE = re.compile(r"[0-9a-f]{40}")
RELEASE_NOTE_RE = re.compile(r"docs/releases/v[0-9]+\.[0-9]+\.[0-9]+\.md")
QUALITY_EVIDENCE_RE = re.compile(
    r"docs/evals/v[0-9]+\.[0-9]+\.[0-9]+-context-pack-quality-(?:serial|concurrency4)\.json"
)
SAFE_DELTA_PATHS = {
    "config/frontier_t1_release_provenance.v1.json",
    "config/public_sync_blocklist.txt",
    "scripts/agent/audit-agent-global-install-smoke",
    "scripts/agent/audit-public-product-truth",
    "scripts/tests/test_context_pack_quality_benchmark.py",
    "scripts/tests/test_public_product_truth.py",
    "services/gateway-go/memory_recall_response_fallback_receipt_private_test.go",
    "services/gateway-go/memory_recall_response_fallback_receipt_test.go",
}
SAFE_ADDITIVE_DELTA_PATHS = {
    "services/gateway-go/memory_recall_response_fallback_receipt_private_test.go",
}


class ResolutionError(RuntimeError):
    """The release source amendment is missing, unsafe, or inconsistent."""


def git(root: Path, *args: str, check: bool = True) -> str:
    result = subprocess.run(
        ["git", *args],
        cwd=root,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        check=False,
    )
    if check and result.returncode != 0:
        detail = result.stderr.strip() or result.stdout.strip()
        raise ResolutionError(detail or f"git {' '.join(args)} failed")
    return result.stdout.strip()


def resolve_commit(root: Path, ref: str, label: str) -> str:
    commit = git(root, "rev-parse", "--verify", f"{ref}^{{commit}}")
    if COMMIT_RE.fullmatch(commit) is None:
        raise ResolutionError(f"{label} is not a full Git commit")
    return commit


def require_ancestor(root: Path, ancestor: str, descendant: str, label: str) -> None:
    result = subprocess.run(
        ["git", "merge-base", "--is-ancestor", ancestor, descendant],
        cwd=root,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise ResolutionError(f"{label}: {ancestor} is not an ancestor of {descendant}")


def safe_delta_path(path: str) -> bool:
    return (
        path in SAFE_DELTA_PATHS
        or RELEASE_NOTE_RE.fullmatch(path) is not None
        or QUALITY_EVIDENCE_RE.fullmatch(path) is not None
    )


def safe_delta_status(path: str, status: str) -> bool:
    return status == "M" or (status == "A" and path in SAFE_ADDITIVE_DELTA_PATHS)


def validate_registry(payload: Any) -> tuple[set[str], list[dict[str, Any]]]:
    if not isinstance(payload, dict) or payload.get("schema_id") != AMENDMENTS_SCHEMA_ID:
        raise ResolutionError("release source amendment registry schema is invalid")
    if payload.get("version") != 1:
        raise ResolutionError("release source amendment registry version is invalid")
    required = payload.get("required_release_tags", [])
    amendments = payload.get("amendments", [])
    if not isinstance(required, list) or not all(isinstance(tag, str) and tag for tag in required):
        raise ResolutionError("required_release_tags must be a string array")
    if len(required) != len(set(required)):
        raise ResolutionError("required_release_tags contains duplicates")
    if not isinstance(amendments, list) or not all(isinstance(row, dict) for row in amendments):
        raise ResolutionError("amendments must be an object array")
    tags = [str(row.get("release_tag", "")) for row in amendments]
    if any(not tag for tag in tags) or len(tags) != len(set(tags)):
        raise ResolutionError("amendment release tags must be non-empty and unique")
    return set(required), amendments


def observed_delta(root: Path, original: str, approved: str) -> list[dict[str, str]]:
    output = git(root, "diff", "--name-status", f"{original}..{approved}")
    rows: list[dict[str, str]] = []
    for line in output.splitlines():
        parts = line.split("\t")
        if len(parts) != 2:
            raise ResolutionError(f"private source amendment contains an invalid delta: {line}")
        status, path = parts
        if not safe_delta_path(path):
            raise ResolutionError(f"private source amendment touches a runtime-unsafe path: {path}")
        if not safe_delta_status(path, status):
            raise ResolutionError(f"private source amendment contains an unsafe delta status: {line}")
        blob_oid = git(root, "rev-parse", f"{approved}:{path}")
        rows.append({"path": path, "status": status, "blob_oid": blob_oid})
    return sorted(rows, key=lambda row: row["path"])


def resolve(
    *,
    root: Path,
    registry_path: Path,
    release_tag: str,
    source_commit_ref: str,
    original_private_proof_ref: str,
    private_main_ref: str,
    authorization_ref: str,
) -> dict[str, Any]:
    registry_bytes = registry_path.read_bytes()
    try:
        registry = json.loads(registry_bytes)
    except json.JSONDecodeError as exc:
        raise ResolutionError("release source amendment registry is not valid JSON") from exc
    required_tags, amendments = validate_registry(registry)

    source_commit = resolve_commit(root, source_commit_ref, "release source")
    original_private_proof = resolve_commit(
        root, original_private_proof_ref, "original private proof"
    )
    private_main_commit = resolve_commit(root, private_main_ref, "private main")
    authorization_commit = resolve_commit(root, authorization_ref, "authorization ref")
    require_ancestor(root, source_commit, authorization_commit, "release replay ancestry")
    require_ancestor(
        root, original_private_proof, private_main_commit, "private proof ancestry"
    )

    matches = [row for row in amendments if row.get("release_tag") == release_tag]
    if not matches:
        if release_tag in required_tags:
            raise ResolutionError(f"release {release_tag} requires an explicit source amendment")
        return {
            "schema_id": RESOLUTION_SCHEMA_ID,
            "mode": "original",
            "release_tag": release_tag,
            "source_commit": source_commit,
            "authorization_commit": authorization_commit,
            "private_main_commit": private_main_commit,
            "original_private_proof_commit": original_private_proof,
            "approved_private_source_commit": original_private_proof,
            "approved_private_source_tree": git(root, "rev-parse", f"{original_private_proof}^{{tree}}"),
            "changed_paths": [],
            "registry_sha256": hashlib.sha256(registry_bytes).hexdigest(),
        }

    amendment = matches[0]
    expected_source = str(amendment.get("source_commit", ""))
    expected_original = str(amendment.get("original_private_proof_commit", ""))
    approved = str(amendment.get("approved_private_source_commit", ""))
    approved_tree = str(amendment.get("approved_private_source_tree", ""))
    reason = str(amendment.get("reason", "")).strip()
    if expected_source != source_commit:
        raise ResolutionError("amendment source commit does not match the immutable release")
    if expected_original != original_private_proof:
        raise ResolutionError("amendment original private proof does not match release provenance")
    if COMMIT_RE.fullmatch(approved) is None or COMMIT_RE.fullmatch(approved_tree) is None:
        raise ResolutionError("amendment approved source commit or tree is invalid")
    if not reason:
        raise ResolutionError("amendment reason is required")
    approved = resolve_commit(root, approved, "approved private source")
    if git(root, "rev-parse", f"{approved}^{{tree}}") != approved_tree:
        raise ResolutionError("approved private source tree does not match the registry")
    require_ancestor(root, original_private_proof, approved, "amended proof ancestry")
    require_ancestor(root, approved, private_main_commit, "private main approval ancestry")

    expected_delta = amendment.get("changed_paths")
    if not isinstance(expected_delta, list) or not expected_delta:
        raise ResolutionError("amendment changed_paths must be a non-empty array")
    normalized: list[dict[str, str]] = []
    seen: set[str] = set()
    for row in expected_delta:
        if not isinstance(row, dict):
            raise ResolutionError("amendment changed_paths contains a non-object")
        normalized_row = {
            "path": str(row.get("path", "")),
            "status": str(row.get("status", "")),
            "blob_oid": str(row.get("blob_oid", "")),
        }
        path = normalized_row["path"]
        if not path or path in seen or not safe_delta_path(path):
            raise ResolutionError(f"amendment path is duplicate or runtime-unsafe: {path}")
        if (
            not safe_delta_status(path, normalized_row["status"])
            or COMMIT_RE.fullmatch(normalized_row["blob_oid"]) is None
        ):
            raise ResolutionError(f"amendment path identity is invalid: {path}")
        seen.add(path)
        normalized.append(normalized_row)
    normalized.sort(key=lambda row: row["path"])
    actual_delta = observed_delta(root, original_private_proof, approved)
    if actual_delta != normalized:
        raise ResolutionError("approved private source delta does not match the amendment registry")

    return {
        "schema_id": RESOLUTION_SCHEMA_ID,
        "mode": "amended",
        "release_tag": release_tag,
        "source_commit": source_commit,
        "authorization_commit": authorization_commit,
        "private_main_commit": private_main_commit,
        "original_private_proof_commit": original_private_proof,
        "approved_private_source_commit": approved,
        "approved_private_source_tree": approved_tree,
        "reason": reason,
        "changed_paths": actual_delta,
        "registry_sha256": hashlib.sha256(registry_bytes).hexdigest(),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--amendments", type=Path, required=True)
    parser.add_argument("--release-tag", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--original-private-proof", required=True)
    parser.add_argument("--private-main-ref", required=True)
    parser.add_argument("--authorization-ref", required=True)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        payload = resolve(
            root=args.root.resolve(),
            registry_path=args.amendments.resolve(),
            release_tag=args.release_tag,
            source_commit_ref=args.source_commit,
            original_private_proof_ref=args.original_private_proof,
            private_main_ref=args.private_main_ref,
            authorization_ref=args.authorization_ref,
        )
    except (OSError, ResolutionError) as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, sort_keys=True))
        return 1
    encoded = json.dumps(payload, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded, encoding="utf-8")
    else:
        sys.stdout.write(encoded)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
