#!/usr/bin/env python3
"""Create and verify release metadata without leaking build-machine paths.

The release artifacts are the authority.  This module records only portable
asset names, sizes, and digests; it never records the local ``dist`` path.
Metadata writes are idempotent and refuse to replace a different existing
file.  That makes a rerun safe while preventing a proof or checksum from being
silently replaced after the bytes it describes have changed.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from pathlib import Path, PurePosixPath
from typing import Any, Iterable, Mapping


DEFAULT_ARTIFACTS = (
    "ContextLattice-macOS-universal.dmg",
    "ContextLattice-windows-x64.msi",
    "ContextLattice-linux-bootstrap.tar.gz",
)
PUBLIC_INSTALLER_ARTIFACTS = frozenset(DEFAULT_ARTIFACTS)
MANIFEST_SCHEMA_ID = "contextlattice_release_artifact_manifest.v1"
PROVENANCE_SCHEMA_ID = "contextlattice_release_provenance_input.v1"
INTEGRITY_RECEIPT_SCHEMA_ID = "contextlattice_release_artifact_integrity_receipt.v1"
FULL_SHA256 = re.compile(r"[0-9a-f]{64}\Z")
FULL_COMMIT = re.compile(r"[0-9a-f]{40}\Z")
SAFE_TAG = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,127}\Z")
SAFE_ASSET = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,191}\Z")
WINDOWS_DRIVE_PATH = re.compile(r"^[A-Za-z]:")
PRIVATE_FILESYSTEM_PATH = re.compile(
    r"(?:^|[/\\])(?:private|private_docs)(?:[/\\]|$)",
    re.IGNORECASE,
)
PUBLIC_FORBIDDEN_PATH = re.compile(
    r"(?:^|[/\\])(?:private|private_docs|public-paid)(?:[/\\]|$)",
    re.IGNORECASE,
)


class ReleaseMetadataError(RuntimeError):
    """The release metadata is unsafe, incomplete, or not reproducible."""


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _portable_asset_name(name: str) -> str:
    if not isinstance(name, str) or not name:
        raise ReleaseMetadataError("release asset name is empty")
    if "\x00" in name or "\\" in name:
        raise ReleaseMetadataError(f"release asset name is not portable: {name!r}")
    path = PurePosixPath(name)
    if (
        path.is_absolute()
        or len(path.parts) != 1
        or path.parts[0] in {"", ".", ".."}
        or not SAFE_ASSET.fullmatch(name)
    ):
        raise ReleaseMetadataError(f"release asset name is not a safe basename: {name!r}")
    return name


def _validate_tag_commit(*, tag: str, commit: str, release_ref: str | None = None) -> str:
    if not SAFE_TAG.fullmatch(tag):
        raise ReleaseMetadataError(f"release tag is invalid: {tag!r}")
    if not FULL_COMMIT.fullmatch(commit):
        raise ReleaseMetadataError("release commit must be a full lowercase SHA-1")
    expected_ref = f"refs/tags/{tag}"
    if release_ref is not None and release_ref != expected_ref:
        raise ReleaseMetadataError(
            f"release_ref must be {expected_ref!r}, got {release_ref!r}"
        )
    return expected_ref


def _assert_regular_file(path: Path, *, label: str) -> None:
    if path.is_symlink() or not path.is_file():
        raise ReleaseMetadataError(f"{label} is missing or is not a regular file: {path.name}")
    if path.stat().st_size <= 0:
        raise ReleaseMetadataError(f"{label} is empty: {path.name}")


def collect_artifacts(dist_dir: Path, names: Iterable[str]) -> list[dict[str, object]]:
    """Return deterministic, path-free rows for the requested artifact names."""

    root = dist_dir.resolve()
    if not root.is_dir():
        raise ReleaseMetadataError(f"artifact directory does not exist: {dist_dir}")
    requested = [_portable_asset_name(name) for name in names]
    if len(set(requested)) != len(requested):
        raise ReleaseMetadataError("release artifact set contains duplicate names")
    rows: list[dict[str, object]] = []
    for name in sorted(requested):
        path = root / name
        _assert_regular_file(path, label="release artifact")
        rows.append(
            {
                "name": name,
                "size_bytes": path.stat().st_size,
                "sha256": sha256_file(path),
            }
        )
    return rows


def _write_immutable(path: Path, content: bytes) -> None:
    """Write once, or accept the exact same bytes; never clobber a proof."""

    if path.exists() or path.is_symlink():
        if path.is_symlink() or not path.is_file():
            raise ReleaseMetadataError(f"refusing to replace non-file metadata: {path.name}")
        if path.read_bytes() != content:
            raise ReleaseMetadataError(f"refusing to overwrite existing metadata: {path.name}")
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    try:
        descriptor = os.open(path, flags, 0o600)
    except FileExistsError as exc:
        raise ReleaseMetadataError(f"metadata appeared during write: {path.name}") from exc
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
    except BaseException:
        try:
            path.unlink()
        except FileNotFoundError:
            pass
        raise


def _canonical_json(payload: Mapping[str, Any]) -> bytes:
    return (json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=True) + "\n").encode(
        "utf-8"
    )


def write_sha_sums(dist_dir: Path, rows: list[dict[str, object]], output_name: str) -> Path:
    output_name = _portable_asset_name(output_name)
    lines = [f"{row['sha256']}  {row['name']}" for row in rows]
    content = ("\n".join(lines) + "\n").encode("ascii")
    output = dist_dir / output_name
    _write_immutable(output, content)
    return output


def _metadata_path_is_unsafe(value: str) -> bool:
    normalized = value.strip().replace("\\", "/")
    normalized_lower = normalized.lower()
    return (
        normalized.startswith("/")
        or normalized.startswith("~")
        or WINDOWS_DRIVE_PATH.match(normalized) is not None
        or normalized_lower.startswith("file:")
        or "/volumes/" in normalized_lower
        or "/users/" in normalized_lower
        or PRIVATE_FILESYSTEM_PATH.search(normalized) is not None
    )


def _walk_metadata(payload: Any, *, public_safe: bool, location: str = "metadata") -> None:
    """Reject machine paths and null identity refs in JSON metadata."""

    if isinstance(payload, Mapping):
        for key, value in payload.items():
            child = f"{location}.{key}"
            if key.endswith("_ref") and value in (None, "", "null"):
                raise ReleaseMetadataError(f"metadata identity reference is null: {child}")
            if public_safe and isinstance(value, str) and PUBLIC_FORBIDDEN_PATH.search(value.replace("\\", "/")):
                raise ReleaseMetadataError(f"public metadata contains a private lane value: {child}")
            _walk_metadata(value, public_safe=public_safe, location=child)
        return
    if isinstance(payload, list):
        for index, value in enumerate(payload):
            _walk_metadata(value, public_safe=public_safe, location=f"{location}[{index}]")
        return
    if isinstance(payload, str) and _metadata_path_is_unsafe(payload):
        raise ReleaseMetadataError(f"metadata contains an absolute or private path: {location}")


def _validate_rows(rows: Any, dist_dir: Path, *, expected_names: set[str] | None = None) -> list[dict[str, object]]:
    if not isinstance(rows, list) or not rows:
        raise ReleaseMetadataError("manifest artifacts are missing or empty")
    normalized: list[dict[str, object]] = []
    seen: set[str] = set()
    for row in rows:
        if not isinstance(row, Mapping):
            raise ReleaseMetadataError("manifest artifact row is not an object")
        if set(row) != {"name", "size_bytes", "sha256"}:
            raise ReleaseMetadataError("manifest artifact row fields are invalid")
        name = _portable_asset_name(row.get("name"))
        if name in seen:
            raise ReleaseMetadataError(f"manifest contains duplicate artifact: {name}")
        seen.add(name)
        sha = row.get("sha256")
        size = row.get("size_bytes")
        if not isinstance(sha, str) or FULL_SHA256.fullmatch(sha) is None:
            raise ReleaseMetadataError(f"manifest artifact sha256 is invalid: {name}")
        if isinstance(size, bool) or not isinstance(size, int) or size <= 0:
            raise ReleaseMetadataError(f"manifest artifact size is invalid: {name}")
        path = dist_dir / name
        _assert_regular_file(path, label="manifest artifact")
        if path.stat().st_size != size or sha256_file(path) != sha:
            raise ReleaseMetadataError(f"manifest verification failed for artifact: {name}")
        normalized.append({"name": name, "size_bytes": size, "sha256": sha})
    if expected_names is not None and seen != expected_names:
        missing = sorted(expected_names - seen)
        extra = sorted(seen - expected_names)
        raise ReleaseMetadataError(f"manifest artifact set mismatch: missing={missing} extra={extra}")
    return sorted(normalized, key=lambda row: str(row["name"]))


def build_manifest(
    *,
    lane: str,
    channel: str,
    tag: str,
    commit: str,
    artifacts: list[dict[str, object]],
    public_safe: bool = False,
) -> dict[str, object]:
    if lane not in {"paid", "public"}:
        raise ReleaseMetadataError("manifest lane is missing or invalid")
    if channel not in {"stable", "canary"}:
        raise ReleaseMetadataError("manifest channel is missing or invalid")
    release_ref = _validate_tag_commit(tag=tag, commit=commit)
    payload: dict[str, object] = {
        "schema_id": MANIFEST_SCHEMA_ID,
        "lane": lane,
        "channel": channel,
        "tag": tag,
        "release_ref": release_ref,
        "commit": commit,
        "artifact_count": len(artifacts),
        "artifacts": sorted(artifacts, key=lambda row: str(row["name"])),
    }
    _walk_metadata(payload, public_safe=public_safe)
    return payload


def verify_manifest(
    manifest: dict[str, object],
    dist_dir: Path,
    *,
    expected_lane: str | None = None,
    expected_channel: str | None = None,
    expected_tag: str | None = None,
    expected_commit: str | None = None,
    expected_names: set[str] | None = None,
    public_safe: bool = False,
) -> list[dict[str, object]]:
    if not isinstance(manifest, Mapping):
        raise ReleaseMetadataError("release manifest is not an object")
    if manifest.get("schema_id") not in {MANIFEST_SCHEMA_ID, "contextlattice_release_manifest.v2"}:
        raise ReleaseMetadataError("release manifest schema is invalid")
    lane = manifest.get("lane")
    channel = manifest.get("channel")
    tag = manifest.get("tag")
    commit = manifest.get("commit")
    release_ref = manifest.get("release_ref")
    if not isinstance(lane, str) or lane not in {"paid", "public"}:
        raise ReleaseMetadataError("manifest lane is missing or invalid")
    if not isinstance(channel, str) or channel not in {"stable", "canary"}:
        raise ReleaseMetadataError("manifest channel is missing or invalid")
    if not isinstance(tag, str) or not isinstance(commit, str):
        raise ReleaseMetadataError("manifest tag or commit is missing")
    expected_ref = _validate_tag_commit(tag=tag, commit=commit, release_ref=release_ref)
    if release_ref != expected_ref:
        raise ReleaseMetadataError("manifest release_ref is missing or invalid")
    if expected_lane is not None and lane != expected_lane:
        raise ReleaseMetadataError(f"manifest lane mismatch: {lane!r}")
    if expected_channel is not None and channel != expected_channel:
        raise ReleaseMetadataError(f"manifest channel mismatch: {channel!r}")
    if expected_tag is not None and tag != expected_tag:
        raise ReleaseMetadataError(f"manifest tag mismatch: {tag!r}")
    if expected_commit is not None and commit != expected_commit:
        raise ReleaseMetadataError(f"manifest commit mismatch: {commit!r}")
    artifacts = _validate_rows(manifest.get("artifacts"), dist_dir, expected_names=expected_names)
    if manifest.get("artifact_count") != len(artifacts):
        raise ReleaseMetadataError("manifest artifact_count does not match artifacts")
    _walk_metadata(manifest, public_safe=public_safe)
    return artifacts


def build_provenance(
    *,
    lane: str,
    channel: str,
    tag: str,
    commit: str,
    manifest_name: str,
    manifest_sha256: str,
    artifacts: list[dict[str, object]],
    public_safe: bool = False,
) -> dict[str, object]:
    release_ref = _validate_tag_commit(tag=tag, commit=commit)
    if not FULL_SHA256.fullmatch(manifest_sha256):
        raise ReleaseMetadataError("manifest sha256 is invalid")
    payload: dict[str, object] = {
        "schema_id": PROVENANCE_SCHEMA_ID,
        "lane": lane,
        "channel": channel,
        "tag": tag,
        "release_ref": release_ref,
        "commit": commit,
        "manifest": {"name": _portable_asset_name(manifest_name), "sha256": manifest_sha256},
        "artifacts": sorted(artifacts, key=lambda row: str(row["name"])),
    }
    _walk_metadata(payload, public_safe=public_safe)
    return payload


def build_integrity_receipt(
    *,
    lane: str,
    tag: str,
    commit: str,
    artifact: Mapping[str, object],
    sbom: Mapping[str, object] | None = None,
) -> dict[str, object]:
    name = _portable_asset_name(artifact.get("name"))
    sha = artifact.get("sha256")
    size = artifact.get("size_bytes")
    if not isinstance(sha, str) or FULL_SHA256.fullmatch(sha) is None:
        raise ReleaseMetadataError(f"integrity receipt artifact sha256 is invalid: {name}")
    if isinstance(size, bool) or not isinstance(size, int) or size <= 0:
        raise ReleaseMetadataError(f"integrity receipt artifact size is invalid: {name}")
    release_ref = _validate_tag_commit(tag=tag, commit=commit)
    payload: dict[str, object] = {
        "schema_id": INTEGRITY_RECEIPT_SCHEMA_ID,
        "lane": lane,
        "tag": tag,
        "release_ref": release_ref,
        "commit": commit,
        "artifact": {"name": name, "size_bytes": size, "sha256": sha},
    }
    if sbom is not None:
        sbom_name = _portable_asset_name(sbom.get("name"))
        sbom_sha = sbom.get("sha256")
        sbom_size = sbom.get("size_bytes")
        if sbom_name != f"{name}.sbom.spdx.json":
            raise ReleaseMetadataError(f"integrity receipt SBOM name does not match artifact: {name}")
        if not isinstance(sbom_sha, str) or FULL_SHA256.fullmatch(sbom_sha) is None:
            raise ReleaseMetadataError(f"integrity receipt SBOM sha256 is invalid: {name}")
        if isinstance(sbom_size, bool) or not isinstance(sbom_size, int) or sbom_size <= 0:
            raise ReleaseMetadataError(f"integrity receipt SBOM size is invalid: {name}")
        payload["sbom"] = {
            "name": sbom_name,
            "size_bytes": sbom_size,
            "sha256": sbom_sha,
        }
    return payload


def _read_json(path: Path) -> dict[str, object]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ReleaseMetadataError(f"could not read JSON metadata: {path.name}") from exc
    if not isinstance(payload, dict):
        raise ReleaseMetadataError(f"JSON metadata is not an object: {path.name}")
    return payload


def verify_sha_sums(path: Path, dist_dir: Path, rows: list[dict[str, object]]) -> None:
    expected = "".join(f"{row['sha256']}  {row['name']}\n" for row in rows)
    try:
        actual = path.read_text(encoding="ascii")
    except (OSError, UnicodeDecodeError) as exc:
        raise ReleaseMetadataError(f"checksum file is unreadable: {path.name}") from exc
    if actual != expected:
        raise ReleaseMetadataError(f"checksum file does not match final artifacts: {path.name}")
    for row in rows:
        if sha256_file(dist_dir / str(row["name"])) != row["sha256"]:
            raise ReleaseMetadataError(f"checksum verification failed: {row['name']}")


def _manifest_name(channel: str) -> str:
    return f"ContextLattice-release-manifest-{channel}.json"


def _generate(args: argparse.Namespace, *, verify_only: bool) -> dict[str, object]:
    dist_dir = Path(args.dist_dir).resolve()
    names = tuple(args.artifacts or DEFAULT_ARTIFACTS)
    expected_names = set(_portable_asset_name(name) for name in names)
    integrity_names = tuple(args.integrity_artifacts or names)
    integrity_expected_names = set(_portable_asset_name(name) for name in integrity_names)
    if len(integrity_expected_names) != len(integrity_names):
        raise ReleaseMetadataError("integrity artifact set contains duplicate names")
    if not integrity_expected_names.issubset(expected_names):
        raise ReleaseMetadataError("integrity artifacts must be a subset of release artifacts")
    manifest_name = _portable_asset_name(args.manifest_name or _manifest_name(args.channel))
    sums_name = _portable_asset_name(args.sha_sums_name)
    provenance_name = _portable_asset_name(args.provenance_name) if args.provenance_name else ""
    sbom_dir = Path(args.sbom_dir).resolve() if args.sbom_dir else None
    if sbom_dir is not None and not args.integrity_dir:
        raise ReleaseMetadataError("--sbom-dir requires --integrity-dir")
    rows: list[dict[str, object]]

    if verify_only:
        manifest_path = dist_dir / manifest_name
        manifest = _read_json(manifest_path)
        rows = verify_manifest(
            manifest,
            dist_dir,
            expected_lane=args.lane,
            expected_channel=args.channel,
            expected_tag=args.tag,
            expected_commit=args.commit,
            expected_names=expected_names,
            public_safe=args.public_safe,
        )
    else:
        rows = collect_artifacts(dist_dir, names)
        manifest = build_manifest(
            lane=args.lane,
            channel=args.channel,
            tag=args.tag,
            commit=args.commit,
            artifacts=rows,
            public_safe=args.public_safe,
        )
        _write_immutable(dist_dir / manifest_name, _canonical_json(manifest))
        manifest = _read_json(dist_dir / manifest_name)
        rows = verify_manifest(
            manifest,
            dist_dir,
            expected_lane=args.lane,
            expected_channel=args.channel,
            expected_tag=args.tag,
            expected_commit=args.commit,
            expected_names=expected_names,
            public_safe=args.public_safe,
        )
        write_sha_sums(dist_dir, rows, sums_name)
        if provenance_name:
            provenance = build_provenance(
                lane=args.lane,
                channel=args.channel,
                tag=args.tag,
                commit=args.commit,
                manifest_name=manifest_name,
                manifest_sha256=sha256_file(dist_dir / manifest_name),
                artifacts=rows,
                public_safe=args.public_safe,
            )
            _write_immutable(dist_dir / provenance_name, _canonical_json(provenance))
    integrity_dir = Path(args.integrity_dir).resolve() if args.integrity_dir else None
    write_integrity = integrity_dir is not None and (not verify_only or args.write_integrity)
    if write_integrity:
        assert integrity_dir is not None
        for row in rows:
            if row["name"] not in integrity_expected_names:
                continue
            receipt_name = f"{row['name']}.integrity.json"
            sbom = None
            if sbom_dir is not None:
                sbom_path = sbom_dir / f"{row['name']}.sbom.spdx.json"
                _assert_regular_file(sbom_path, label="release SBOM")
                sbom = {
                    "name": sbom_path.name,
                    "size_bytes": sbom_path.stat().st_size,
                    "sha256": sha256_file(sbom_path),
                }
            receipt = build_integrity_receipt(
                lane=args.lane,
                tag=args.tag,
                commit=args.commit,
                artifact=row,
                sbom=sbom,
            )
            _walk_metadata(receipt, public_safe=args.public_safe)
            _write_immutable(integrity_dir / receipt_name, _canonical_json(receipt))

    verify_sha_sums(dist_dir / sums_name, dist_dir, rows)
    if provenance_name:
        provenance_path = dist_dir / provenance_name
        provenance = _read_json(provenance_path)
        _walk_metadata(provenance, public_safe=args.public_safe)
        if provenance.get("schema_id") != PROVENANCE_SCHEMA_ID:
            raise ReleaseMetadataError("release provenance input schema is invalid")
        if provenance.get("manifest", {}).get("sha256") != sha256_file(dist_dir / manifest_name):
            raise ReleaseMetadataError("release provenance does not bind the final manifest")
    if integrity_dir is not None:
        for row in rows:
            if row["name"] not in integrity_expected_names:
                continue
            receipt_path = integrity_dir / f"{row['name']}.integrity.json"
            receipt = _read_json(receipt_path)
            _walk_metadata(receipt, public_safe=args.public_safe)
            if receipt.get("schema_id") != INTEGRITY_RECEIPT_SCHEMA_ID:
                raise ReleaseMetadataError(
                    f"integrity receipt schema is invalid: {receipt_path.name}"
                )
            expected_receipt_keys = {
                "schema_id",
                "lane",
                "tag",
                "release_ref",
                "commit",
                "artifact",
            }
            if sbom_dir is not None:
                expected_receipt_keys.add("sbom")
            if set(receipt) != expected_receipt_keys:
                raise ReleaseMetadataError(
                    f"integrity receipt fields are invalid: {receipt_path.name}"
                )
            if receipt.get("lane") != args.lane or receipt.get("tag") != args.tag:
                raise ReleaseMetadataError(
                    f"integrity receipt identity mismatch: {receipt_path.name}"
                )
            if receipt.get("release_ref") != f"refs/tags/{args.tag}" or receipt.get("commit") != args.commit:
                raise ReleaseMetadataError(
                    f"integrity receipt release binding mismatch: {receipt_path.name}"
                )
            if receipt.get("artifact") != row:
                raise ReleaseMetadataError(
                    f"integrity receipt does not bind final artifact: {receipt_path.name}"
                )
            if sbom_dir is not None:
                sbom_path = sbom_dir / f"{row['name']}.sbom.spdx.json"
                _assert_regular_file(sbom_path, label="release SBOM")
                expected_sbom = {
                    "name": sbom_path.name,
                    "size_bytes": sbom_path.stat().st_size,
                    "sha256": sha256_file(sbom_path),
                }
                if receipt.get("sbom") != expected_sbom:
                    raise ReleaseMetadataError(
                        f"integrity receipt does not bind final SBOM: {receipt_path.name}"
                    )

    return {
        "ok": True,
        "lane": args.lane,
        "channel": args.channel,
        "tag": args.tag,
        "release_ref": f"refs/tags/{args.tag}",
        "manifest": manifest_name,
        "checksums": sums_name,
        "provenance": provenance_name or None,
        "artifact_count": len(rows),
        "integrity_receipt_count": len(integrity_expected_names) if integrity_dir is not None else 0,
        "verified": True,
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dist-dir", default="dist")
    parser.add_argument("--lane", required=True, choices=["paid", "public"])
    parser.add_argument("--channel", default="stable", choices=["stable", "canary"])
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--manifest-name", default="")
    parser.add_argument("--sha-sums-name", default="ContextLattice-SHA256SUMS.txt")
    parser.add_argument("--provenance-name", default="")
    parser.add_argument("--integrity-dir", default="")
    parser.add_argument("--sbom-dir", default="")
    parser.add_argument("--integrity-artifacts", nargs="*", default=None)
    parser.add_argument("--artifacts", nargs="*", default=list(DEFAULT_ARTIFACTS))
    parser.add_argument("--public-safe", action="store_true")
    parser.add_argument("--verify", action="store_true", help="verify generated metadata (the default behavior)")
    parser.add_argument("--verify-only", action="store_true")
    parser.add_argument(
        "--write-integrity",
        action="store_true",
        help="write immutable integrity receipts after verifying an existing manifest",
    )
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    try:
        result = _generate(args, verify_only=args.verify_only)
    except (OSError, ReleaseMetadataError, TypeError, ValueError) as exc:
        print(f"release artifact metadata: ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
