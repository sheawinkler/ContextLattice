"""Bounded content scanning for public installer trees and embedded archives."""

from __future__ import annotations

import hashlib
import io
import errno
import json
import os
import stat
import struct
import subprocess
import tarfile
import tempfile
import zipfile
import zlib
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import BinaryIO

from public_release_material_scan import (
    DEFAULT_CHUNK_SIZE,
    MAX_FINDINGS,
    PublicReleaseMaterialError,
    _asset_label,
    _finding,
    _scan_stream,
)
from release_payload_policy import payload_exclusion_reason, portable_payload_path_key


PUBLIC_PACKAGE_SCAN_SCHEMA_ID = "contextlattice_public_release_package_scan.v1"
_SPOOL_MEMORY_BYTES = 8 * 1024 * 1024
_ZIP_EOCD_SIGNATURE = b"PK\x05\x06"
_ZIP_LOCAL_SIGNATURE = b"PK\x03\x04"
_SUPPORTED_ZIP_METHODS = {zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED}
_UNSUPPORTED_ARCHIVE_SUFFIXES = (
    ".7z",
    ".bz2",
    ".cab",
    ".cpio",
    ".gz",
    ".iso",
    ".rar",
    ".tar",
    ".tar.bz2",
    ".tar.xz",
    ".tbz2",
    ".txz",
    ".xz",
)


@dataclass(frozen=True)
class PackageScanLimits:
    """Hard resource bounds shared by the complete package scan."""

    max_archive_depth: int = 3
    max_archives: int = 64
    max_members: int = 50_000
    max_files: int = 40_000
    max_archive_bytes: int = 256 * 1024 * 1024
    max_member_bytes: int = 192 * 1024 * 1024
    max_expanded_bytes: int = 512 * 1024 * 1024
    max_compression_ratio: float = 200.0
    chunk_size: int = DEFAULT_CHUNK_SIZE

    def validate(self) -> None:
        integer_limits = (
            self.max_archive_depth,
            self.max_archives,
            self.max_members,
            self.max_files,
            self.max_archive_bytes,
            self.max_member_bytes,
            self.max_expanded_bytes,
            self.chunk_size,
        )
        if any(isinstance(value, bool) or not isinstance(value, int) or value <= 0 for value in integer_limits):
            raise ValueError("public package scan integer limits must be positive")
        if (
            isinstance(self.max_compression_ratio, bool)
            or not isinstance(self.max_compression_ratio, (int, float))
            or self.max_compression_ratio <= 0
        ):
            raise ValueError("public package scan compression ratio must be positive")


@dataclass(frozen=True)
class ExpectedArchiveMember:
    """Digest and normalized mode required for one contracted archive file."""

    size: int
    sha256: str
    mode: int
    reviewed_source: bool


@dataclass(frozen=True)
class ArchiveContentContract:
    """Closed archive member set produced by the reviewed release builder."""

    archive_format: str
    files: dict[str, ExpectedArchiveMember]
    directories: frozenset[str]


def archive_contract_sha256(contract: ArchiveContentContract) -> str:
    """Return the stable digest of the exact contracted archive member set."""
    payload = {
        "archive_format": contract.archive_format,
        "directories": sorted(contract.directories),
        "files": {
            name: {
                "mode": member.mode,
                "reviewed_source": member.reviewed_source,
                "sha256": member.sha256,
                "size": member.size,
            }
            for name, member in sorted(contract.files.items())
        },
    }
    canonical = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


@dataclass
class _PackageScanState:
    archive_count: int = 0
    member_count: int = 0
    file_count: int = 0
    raw_byte_count: int = 0
    expanded_byte_count: int = 0
    max_archive_depth: int = 0


def _member_asset(parent_asset: str, member_name: str) -> str:
    digest = hashlib.sha256(member_name.encode("utf-8", errors="surrogatepass")).hexdigest()
    return f"{parent_asset}!member-sha256:{digest}"


def build_public_payload_contract(
    source_root: Path,
    *,
    commit: str,
    metadata_bytes: bytes,
    archive_format: str,
) -> ArchiveContentContract:
    """Reconstruct the exact public payload member set without extraction."""

    if archive_format not in {"tar.gz", "zip"}:
        raise ValueError("unsupported public payload contract format")
    if not metadata_bytes or len(metadata_bytes) > 4096:
        raise ValueError("public payload metadata size is invalid")
    files: dict[str, ExpectedArchiveMember] = {}
    directories: set[str] = {"contextlattice"}
    exact_names: set[str] = set()
    portable_names: dict[str, str] = {}
    attributes_check = subprocess.run(
        [
            "git",
            "-C",
            str(source_root),
            "diff",
            "--no-ext-diff",
            "--quiet",
            commit,
            "--",
            ".gitattributes",
        ],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        check=False,
    )
    if attributes_check.returncode != 0:
        raise ValueError("reviewed source archive attributes differ from the commit")
    with tempfile.TemporaryFile(mode="w+b") as git_archive:
        result = subprocess.run(
            [
                "git",
                "-C",
                str(source_root),
                "archive",
                "--format=tar",
                "--worktree-attributes",
                commit,
            ],
            stdin=subprocess.DEVNULL,
            stdout=git_archive,
            stderr=subprocess.PIPE,
            check=False,
        )
        if result.returncode != 0:
            raise ValueError("reviewed source archive cannot be reconstructed")
        git_archive.seek(0)
        with tarfile.open(fileobj=git_archive, mode="r:") as archive:
            for member in archive:
                relative = member.name.rstrip("/")
                if (
                    not relative
                    or PurePosixPath(relative).is_absolute()
                    or "\\" in relative
                    or any(part in {"", ".", ".."} for part in PurePosixPath(relative).parts)
                ):
                    raise ValueError("reviewed source archive path is unsafe")
                if relative in exact_names:
                    raise ValueError("reviewed source archive has duplicate paths")
                exact_names.add(relative)
                for index in range(1, len(PurePosixPath(relative).parts) + 1):
                    prefix = PurePosixPath(*PurePosixPath(relative).parts[:index]).as_posix()
                    key = portable_payload_path_key(prefix)
                    prior = portable_names.get(key)
                    if prior is not None and prior != prefix:
                        raise ValueError("reviewed source archive paths are nonportable")
                    portable_names[key] = prefix
                if payload_exclusion_reason(relative) is not None:
                    raise ValueError("reviewed source archive contains an excluded path")
                contracted_name = f"contextlattice/{relative}"
                if member.isdir():
                    directories.add(contracted_name)
                    continue
                if not member.isfile():
                    raise ValueError("reviewed source archive contains a special entry")
                extracted = archive.extractfile(member)
                if extracted is None:
                    raise ValueError("reviewed source archive member cannot be read")
                digest = hashlib.sha256()
                observed_size = 0
                for chunk in iter(lambda: extracted.read(DEFAULT_CHUNK_SIZE), b""):
                    observed_size += len(chunk)
                    digest.update(chunk)
                if observed_size != member.size:
                    raise ValueError("reviewed source archive member size changed")
                files[contracted_name] = ExpectedArchiveMember(
                    size=observed_size,
                    sha256=digest.hexdigest(),
                    mode=0o755 if member.mode & 0o111 else 0o644,
                    reviewed_source=True,
                )
                for parent in PurePosixPath(contracted_name).parents:
                    if parent.as_posix() != ".":
                        directories.add(parent.as_posix())

    metadata_name = "contextlattice/.contextlattice-release.json"
    files[metadata_name] = ExpectedArchiveMember(
        size=len(metadata_bytes),
        sha256=hashlib.sha256(metadata_bytes).hexdigest(),
        mode=0o644,
        reviewed_source=False,
    )
    required_files = {
        "contextlattice/.env.example",
        "contextlattice/Makefile",
        "contextlattice/docker-compose.lite.yml",
    }
    if not required_files.issubset(files) or not any(
        name.startswith("contextlattice/services/gateway-go/") for name in files
    ):
        raise ValueError("reviewed source archive is missing required public runtime files")
    return ArchiveContentContract(
        archive_format=archive_format,
        files=files,
        directories=frozenset(directories),
    )


class PublicPackageScanner:
    """Scan a closed installer package without extracting tar/ZIP members."""

    def __init__(
        self,
        *,
        limits: PackageScanLimits | None = None,
        archive_contracts: dict[str, ArchiveContentContract] | None = None,
    ) -> None:
        self.limits = limits or PackageScanLimits()
        self.limits.validate()
        self.archive_contracts = dict(archive_contracts or {})
        self.state = _PackageScanState()
        self.findings: list[dict[str, str]] = []
        self._finding_keys: set[tuple[str, str, str]] = set()

    def _report(self, *, ok: bool) -> dict[str, object]:
        report: dict[str, object] = {
            "schema_id": PUBLIC_PACKAGE_SCAN_SCHEMA_ID,
            "ok": ok,
            "archive_count": self.state.archive_count,
            "member_count": self.state.member_count,
            "file_count": self.state.file_count,
            "raw_byte_count": self.state.raw_byte_count,
            "expanded_byte_count": self.state.expanded_byte_count,
            "max_archive_depth": self.state.max_archive_depth,
            "finding_count": len(self.findings),
            "truncated": len(self.findings) >= MAX_FINDINGS,
        }
        if self.findings:
            report["findings"] = self.findings
        return report

    def report(self) -> dict[str, object]:
        if self.findings:
            raise PublicReleaseMaterialError(self._report(ok=False))
        return self._report(ok=True)

    def _add_findings(self, findings: list[dict[str, str]]) -> None:
        for finding in findings:
            marker = (finding["asset"], finding["code"], finding["evidence_digest"])
            if marker in self._finding_keys:
                continue
            self._finding_keys.add(marker)
            self.findings.append(finding)
            if len(self.findings) >= MAX_FINDINGS:
                break
        if self.findings:
            raise PublicReleaseMaterialError(self._report(ok=False))

    def _fail(self, code: str, asset: str, evidence: bytes = b"") -> None:
        if len(self.findings) < MAX_FINDINGS:
            finding = _finding(code, asset, evidence or code.encode("ascii"))
            marker = (finding["asset"], finding["code"], finding["evidence_digest"])
            if marker not in self._finding_keys:
                self._finding_keys.add(marker)
                self.findings.append(finding)
        raise PublicReleaseMaterialError(self._report(ok=False))

    def _scan_metadata(self, value: bytes, *, asset: str) -> None:
        findings, _ = _scan_stream(
            io.BytesIO(value),
            asset=asset,
            chunk_size=self.limits.chunk_size,
        )
        self._add_findings(findings)

    def _register_path(
        self,
        name: str,
        *,
        asset: str,
        is_directory: bool,
        exact_names: set[str],
        portable_names: dict[str, str],
        path_kinds: dict[str, str],
    ) -> str:
        self._scan_metadata(name.encode("utf-8", errors="surrogatepass"), asset=asset)
        normalized = name.rstrip("/")
        path = PurePosixPath(normalized)
        if (
            not normalized
            or path.is_absolute()
            or "\\" in normalized
            or any(part in {"", ".", ".."} for part in path.parts)
        ):
            self._fail("archive.path_unsafe", asset, name.encode("utf-8", errors="surrogatepass"))
        if normalized in exact_names:
            self._fail("archive.path_duplicate", asset, normalized.encode("utf-8", errors="surrogatepass"))
        exact_names.add(normalized)
        for index in range(1, len(path.parts) + 1):
            prefix = PurePosixPath(*path.parts[:index]).as_posix()
            try:
                key = portable_payload_path_key(prefix)
            except ValueError:
                self._fail("archive.path_nonportable", asset, prefix.encode("utf-8", errors="surrogatepass"))
            prior = portable_names.get(key)
            if prior is not None and prior != prefix:
                self._fail(
                    "archive.path_case_collision",
                    asset,
                    (prior + "\0" + prefix).encode("utf-8", errors="surrogatepass"),
                )
            portable_names[key] = prefix
            prior_kind = path_kinds.get(prefix)
            if index < len(path.parts):
                if prior_kind == "file":
                    self._fail("archive.path_type_collision", asset, prefix.encode("utf-8"))
                path_kinds[prefix] = "directory"
            elif is_directory:
                if prior_kind == "file":
                    self._fail("archive.path_type_collision", asset, prefix.encode("utf-8"))
                path_kinds[prefix] = "directory"
            else:
                if prior_kind == "directory":
                    self._fail("archive.path_type_collision", asset, prefix.encode("utf-8"))
                path_kinds[prefix] = "file"
        return normalized

    def _register_member(self, *, size: int, compressed_size: int | None, asset: str, depth: int) -> None:
        self.state.member_count += 1
        if self.state.member_count > self.limits.max_members:
            self._fail("archive.limit_members", asset)
        if size < 0 or size > self.limits.max_member_bytes:
            self._fail("archive.limit_member_bytes", asset, str(size).encode("ascii"))
        self.state.file_count += 1
        if self.state.file_count > self.limits.max_files:
            self._fail("archive.limit_files", asset)
        self.state.expanded_byte_count += size
        if self.state.expanded_byte_count > self.limits.max_expanded_bytes:
            self._fail("archive.limit_expanded_bytes", asset)
        self.state.max_archive_depth = max(self.state.max_archive_depth, depth)
        if compressed_size is not None and size:
            ratio = size / max(compressed_size, 1)
            if ratio > self.limits.max_compression_ratio:
                self._fail("archive.limit_compression_ratio", asset)

    def scan_raw_path(self, path: Path, *, asset: str | None = None) -> None:
        label = asset or _asset_label(path)
        source = None
        try:
            descriptor = os.open(
                path,
                os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
            )
            source = os.fdopen(descriptor, "rb", closefd=True)
            identity = os.fstat(source.fileno())
            if not stat.S_ISREG(identity.st_mode):
                self._fail("scan.non_regular_file", label)
            findings, scanned = _scan_stream(
                source,
                asset=label,
                chunk_size=self.limits.chunk_size,
            )
            after = os.fstat(source.fileno())
            if (
                identity.st_dev,
                identity.st_ino,
                identity.st_uid,
                identity.st_gid,
                identity.st_mode,
                identity.st_size,
                identity.st_mtime_ns,
                identity.st_ctime_ns,
            ) != (
                after.st_dev,
                after.st_ino,
                after.st_uid,
                after.st_gid,
                after.st_mode,
                after.st_size,
                after.st_mtime_ns,
                after.st_ctime_ns,
            ):
                self._fail("scan.file_changed", label)
        except OSError as exc:
            if exc.errno == errno.ELOOP:
                self._fail("scan.path_link", label)
            self._fail("scan.io_error", label, type(exc).__name__.encode("ascii", errors="replace"))
        finally:
            if source is not None:
                source.close()
        self.state.raw_byte_count += scanned
        self._add_findings(findings)

    def scan_archive_path(
        self,
        path: Path,
        *,
        archive_format: str,
        asset: str | None = None,
        depth: int = 1,
        scan_raw: bool = True,
    ) -> None:
        label = asset or _asset_label(path)
        source = None
        try:
            descriptor = os.open(
                path,
                os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
            )
            source = os.fdopen(descriptor, "rb", closefd=True)
            identity = os.fstat(source.fileno())
            if not stat.S_ISREG(identity.st_mode):
                self._fail("scan.non_regular_file", label)
            size = identity.st_size
            if size > self.limits.max_archive_bytes:
                self._fail("archive.limit_compressed_bytes", label, str(size).encode("ascii"))
            if scan_raw:
                source.seek(0)
                findings, scanned = _scan_stream(
                    source,
                    asset=label,
                    chunk_size=self.limits.chunk_size,
                )
                self.state.raw_byte_count += scanned
                self._add_findings(findings)
            source.seek(0)
            with tempfile.SpooledTemporaryFile(
                max_size=_SPOOL_MEMORY_BYTES,
                mode="w+b",
            ) as buffered:
                while True:
                    chunk = source.read(self.limits.chunk_size)
                    if not chunk:
                        break
                    buffered.write(chunk)
                buffered.seek(0)
                self._scan_archive(
                    buffered,
                    archive_format=archive_format,
                    asset=label,
                    depth=depth,
                    contract=self.archive_contracts.get(path.name),
                )
            after = os.fstat(source.fileno())
            if (
                identity.st_dev,
                identity.st_ino,
                identity.st_uid,
                identity.st_gid,
                identity.st_mode,
                identity.st_size,
                identity.st_mtime_ns,
                identity.st_ctime_ns,
            ) != (
                after.st_dev,
                after.st_ino,
                after.st_uid,
                after.st_gid,
                after.st_mode,
                after.st_size,
                after.st_mtime_ns,
                after.st_ctime_ns,
            ):
                self._fail("scan.file_changed", label)
        except PublicReleaseMaterialError:
            raise
        except OSError as exc:
            if exc.errno == errno.ELOOP:
                self._fail("scan.path_link", label)
            self._fail("archive.malformed", label)
        except (tarfile.TarError, zipfile.BadZipFile, zlib.error, struct.error, UnicodeError):
            self._fail("archive.malformed", label)
        finally:
            if source is not None:
                source.close()

    def scan_tree(self, root: Path, *, asset: str) -> None:
        exact_names: set[str] = set()
        portable_names: dict[str, str] = {}
        path_kinds: dict[str, str] = {}

        def walk(directory_fd: int, prefix: str) -> None:
            try:
                names = sorted(os.listdir(directory_fd))
            except OSError:
                raise
            for name in names:
                relative = f"{prefix}/{name}" if prefix else name
                member_asset = _member_asset(asset, relative)
                try:
                    child_fd = os.open(
                        name,
                        os.O_RDONLY
                        | os.O_CLOEXEC
                        | os.O_NOFOLLOW
                        | os.O_NONBLOCK,
                        dir_fd=directory_fd,
                    )
                except OSError as exc:
                    if exc.errno == errno.ELOOP:
                        self._fail("archive.entry_link", member_asset, relative.encode("utf-8"))
                    raise
                try:
                    identity = os.fstat(child_fd)
                    is_directory = stat.S_ISDIR(identity.st_mode)
                    self._register_path(
                        relative,
                        asset=member_asset,
                        is_directory=is_directory,
                        exact_names=exact_names,
                        portable_names=portable_names,
                        path_kinds=path_kinds,
                    )
                    if is_directory:
                        self.state.member_count += 1
                        if self.state.member_count > self.limits.max_members:
                            self._fail("archive.limit_members", member_asset)
                        walk(child_fd, relative)
                        continue
                    if not stat.S_ISREG(identity.st_mode):
                        self._fail("archive.entry_special", member_asset, relative.encode("utf-8"))
                    if identity.st_nlink != 1:
                        self._fail("archive.entry_hardlink", member_asset, relative.encode("utf-8"))
                    size = identity.st_size
                    self._register_member(size=size, compressed_size=None, asset=member_asset, depth=1)
                    with os.fdopen(child_fd, "rb", closefd=False) as source:
                        self._scan_content_stream(
                            source,
                            size=size,
                            name=relative,
                            asset=member_asset,
                            depth=1,
                        )
                    after = os.fstat(child_fd)
                    if (
                        identity.st_dev,
                        identity.st_ino,
                        identity.st_uid,
                        identity.st_gid,
                        identity.st_mode,
                        identity.st_size,
                        identity.st_mtime_ns,
                        identity.st_ctime_ns,
                    ) != (
                        after.st_dev,
                        after.st_ino,
                        after.st_uid,
                        after.st_gid,
                        after.st_mode,
                        after.st_size,
                        after.st_mtime_ns,
                        after.st_ctime_ns,
                    ):
                        self._fail("scan.file_changed", member_asset)
                finally:
                    os.close(child_fd)

        try:
            root_fd = os.open(
                root,
                os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
            )
            try:
                if not stat.S_ISDIR(os.fstat(root_fd).st_mode):
                    self._fail("package.tree_unsafe", asset)
                walk(root_fd, "")
            finally:
                os.close(root_fd)
        except PublicReleaseMaterialError:
            raise
        except OSError as exc:
            if exc.errno == errno.ELOOP:
                self._fail("package.tree_unsafe", asset)
            self._fail("scan.io_error", asset, type(exc).__name__.encode("ascii", errors="replace"))

    def _scan_content_stream(
        self,
        source: BinaryIO,
        *,
        size: int,
        name: str,
        asset: str,
        depth: int,
        expected: ExpectedArchiveMember | None = None,
    ) -> None:
        with tempfile.SpooledTemporaryFile(max_size=_SPOOL_MEMORY_BYTES, mode="w+b") as buffered:
            findings, scanned = _scan_stream(
                source,
                asset=asset,
                chunk_size=self.limits.chunk_size,
                copy_to=buffered,
                stop_at_max_findings=False,
            )
            if scanned != size:
                self._fail("archive.member_size_mismatch", asset)
            buffered.seek(0)
            digest = hashlib.sha256()
            for chunk in iter(lambda: buffered.read(self.limits.chunk_size), b""):
                digest.update(chunk)
            exact_expected = (
                expected is not None
                and expected.size == size
                and expected.sha256 == digest.hexdigest()
            )
            # The tag's public leak guard already audits exact reviewed source
            # bytes. Baseline only a byte-identical source member so detector
            # literals and portable path examples do not become false positives;
            # changed or injected bytes remain fully scanned and fail closed.
            if not (exact_expected and expected is not None and expected.reviewed_source):
                self._add_findings(findings)
            if expected is not None and not exact_expected:
                self._fail("archive.contract_content_mismatch", asset)
            buffered.seek(0)
            prefix = buffered.read(512)
            buffered.seek(0)
            archive_format = self._detect_archive(name, prefix=prefix, asset=asset)
            if archive_format is not None:
                self._scan_archive(
                    buffered,
                    archive_format=archive_format,
                    asset=asset,
                    depth=depth + 1,
                    contract=self.archive_contracts.get(name),
                )

    def _detect_archive(self, name: str, *, prefix: bytes, asset: str) -> str | None:
        lowered = name.casefold()
        expected: str | None = None
        if lowered.endswith((".tar.gz", ".tgz")):
            expected = "tar.gz"
        elif lowered.endswith(".zip"):
            expected = "zip"

        magic: str | None = None
        if prefix.startswith(b"\x1f\x8b"):
            magic = "tar.gz"
        elif prefix.startswith((_ZIP_LOCAL_SIGNATURE, _ZIP_EOCD_SIGNATURE)):
            magic = "zip"
        elif len(prefix) >= 265 and prefix[257:265] in {b"ustar\x0000", b"ustar  \x00"}:
            self._fail("archive.unsupported_format", asset, b"tar")
        elif prefix.startswith((b"7z\xbc\xaf\x27\x1c", b"Rar!\x1a\x07", b"BZh", b"\xfd7zXZ\x00")):
            self._fail("archive.unsupported_format", asset, prefix[:8])

        unsupported_suffix = any(lowered.endswith(suffix) for suffix in _UNSUPPORTED_ARCHIVE_SUFFIXES)
        if expected is None and unsupported_suffix:
            self._fail("archive.unsupported_format", asset, lowered.rsplit(".", 1)[-1].encode("ascii", errors="replace"))
        if expected is not None and magic != expected:
            self._fail("archive.format_mismatch", asset, expected.encode("ascii"))
        if expected is None and magic is not None:
            self._fail("archive.format_mismatch", asset, magic.encode("ascii"))
        return expected

    def _scan_archive(
        self,
        source: BinaryIO,
        *,
        archive_format: str,
        asset: str,
        depth: int,
        contract: ArchiveContentContract | None = None,
    ) -> None:
        # Python 3.9's SpooledTemporaryFile lacks the seekable attribute that
        # zipfile.ZipFile expects, while its backing BytesIO/TemporaryFile has
        # the required method.  Bind that method without changing the bounded
        # spool behavior used for nested archives.
        if not hasattr(source, "seekable") and hasattr(source, "_file"):
            setattr(source, "seekable", source._file.seekable)
        if depth > self.limits.max_archive_depth:
            self._fail("archive.limit_depth", asset, str(depth).encode("ascii"))
        self.state.archive_count += 1
        if self.state.archive_count > self.limits.max_archives:
            self._fail("archive.limit_archives", asset)
        self.state.max_archive_depth = max(self.state.max_archive_depth, depth)
        source.seek(0, os.SEEK_END)
        archive_size = source.tell()
        source.seek(0)
        if archive_size > self.limits.max_archive_bytes:
            self._fail("archive.limit_compressed_bytes", asset, str(archive_size).encode("ascii"))
        if contract is not None and contract.archive_format != archive_format:
            self._fail("archive.contract_format_mismatch", asset)
        try:
            if archive_format == "tar.gz":
                self._scan_tar_gz(
                    source,
                    asset=asset,
                    depth=depth,
                    compressed_size=archive_size,
                    contract=contract,
                )
            elif archive_format == "zip":
                self._scan_zip(
                    source,
                    asset=asset,
                    depth=depth,
                    compressed_size=archive_size,
                    contract=contract,
                )
            else:
                self._fail("archive.unsupported_format", asset, archive_format.encode("ascii", errors="replace"))
        except PublicReleaseMaterialError:
            raise
        except (OSError, tarfile.TarError, zipfile.BadZipFile, zlib.error, struct.error, UnicodeError):
            self._fail("archive.malformed", asset)

    def _scan_tar_gz(
        self,
        source: BinaryIO,
        *,
        asset: str,
        depth: int,
        compressed_size: int,
        contract: ArchiveContentContract | None,
    ) -> None:
        header = source.read(10)
        if len(header) != 10 or header[:3] != b"\x1f\x8b\x08" or header[3] != 0:
            self._fail("archive.gzip_header_invalid", asset, header[:4])
        source.seek(0)
        decompressor = zlib.decompressobj(16 + zlib.MAX_WBITS)
        expanded_size = 0
        with tempfile.SpooledTemporaryFile(max_size=_SPOOL_MEMORY_BYTES, mode="w+b") as expanded:
            while True:
                chunk = source.read(self.limits.chunk_size)
                if not chunk:
                    break
                pending = chunk
                while pending:
                    output = decompressor.decompress(pending, 1024 * 1024)
                    pending = decompressor.unconsumed_tail
                    if output:
                        expanded.write(output)
                        expanded_size += len(output)
                        if expanded_size > self.limits.max_expanded_bytes:
                            self._fail("archive.limit_expanded_bytes", asset)
                    if decompressor.eof:
                        if decompressor.unused_data or pending or source.read(1):
                            self._fail("archive.trailing_data", asset)
                        pending = b""
                        break
            if not decompressor.eof:
                self._fail("archive.malformed", asset, b"gzip-eof")
            flushed = decompressor.flush()
            if flushed:
                expanded.write(flushed)
                expanded_size += len(flushed)
            if expanded_size > self.limits.max_expanded_bytes:
                self._fail("archive.limit_expanded_bytes", asset)
            if expanded_size and expanded_size / max(compressed_size, 1) > self.limits.max_compression_ratio:
                self._fail("archive.limit_compression_ratio", asset)
            expanded.seek(0)
            self._scan_tar(
                expanded,
                asset=asset,
                depth=depth,
                expanded_size=expanded_size,
                contract=contract,
            )

    def _scan_tar(
        self,
        source: BinaryIO,
        *,
        asset: str,
        depth: int,
        expanded_size: int,
        contract: ArchiveContentContract | None,
    ) -> None:
        exact_names: set[str] = set()
        portable_names: dict[str, str] = {}
        path_kinds: dict[str, str] = {}
        contracted_seen: set[str] = set()
        last_data_end = 0
        with tarfile.open(fileobj=source, mode="r:") as archive:
            if archive.pax_headers:
                self._fail("archive.metadata_unsupported", asset, b"global-pax")
            for member in archive:
                member_asset = _member_asset(asset, member.name)
                metadata = "\0".join(
                    (
                        member.name,
                        member.uname or "",
                        member.gname or "",
                        member.linkname or "",
                        *(
                            f"{key}={value}"
                            for key, value in sorted(member.pax_headers.items())
                        ),
                    )
                ).encode("utf-8", errors="surrogatepass")
                self._scan_metadata(metadata, asset=member_asset)
                normalized = self._register_path(
                    member.name,
                    asset=member_asset,
                    is_directory=member.isdir(),
                    exact_names=exact_names,
                    portable_names=portable_names,
                    path_kinds=path_kinds,
                )
                if member.pax_headers or member.sparse is not None:
                    self._fail("archive.metadata_unsupported", member_asset, metadata)
                self.state.member_count += 1
                if self.state.member_count > self.limits.max_members:
                    self._fail("archive.limit_members", member_asset)
                if member.isdir():
                    if member.size != 0:
                        self._fail("archive.member_size_mismatch", member_asset)
                    if contract is not None:
                        if normalized not in contract.directories:
                            self._fail("archive.contract_unexpected_member", member_asset)
                        if member.mode & 0o777 != 0o755:
                            self._fail("archive.contract_mode_mismatch", member_asset)
                        contracted_seen.add(normalized)
                    last_data_end = max(last_data_end, member.offset_data)
                    continue
                if not member.isfile():
                    self._fail("archive.entry_special", member_asset, bytes(member.type))
                expected = contract.files.get(normalized) if contract is not None else None
                if expected is not None and member.mode & 0o777 != expected.mode:
                    self._fail("archive.contract_mode_mismatch", member_asset)
                self.state.member_count -= 1
                self._register_member(
                    size=member.size,
                    compressed_size=None,
                    asset=member_asset,
                    depth=depth,
                )
                extracted = archive.extractfile(member)
                if extracted is None:
                    self._fail("archive.member_unreadable", member_asset)
                self._scan_content_stream(
                    extracted,
                    size=member.size,
                    name=member.name,
                    asset=member_asset,
                    depth=depth,
                    expected=expected,
                )
                if contract is not None:
                    if expected is None:
                        self._fail("archive.contract_unexpected_member", member_asset)
                    contracted_seen.add(normalized)
                last_data_end = max(last_data_end, member.offset_data + ((member.size + 511) // 512) * 512)
        if contract is not None:
            expected_names = set(contract.files) | set(contract.directories)
            if contracted_seen != expected_names:
                self._fail("archive.contract_missing_member", asset)
        # The reviewed builders emit two 512-byte zero records and tarfile's
        # canonical RECORDSIZE padding.  Checking only for a zero prefix lets
        # an attacker append arbitrary zero records (or bytes after them) and
        # still pass the scan.  Bind the trailer to the exact length the
        # canonical builder would have written for this final member.
        canonical_total = (
            (last_data_end + 2 * 512 + tarfile.RECORDSIZE - 1)
            // tarfile.RECORDSIZE
        ) * tarfile.RECORDSIZE
        trailer_size = expanded_size - last_data_end
        if expanded_size != canonical_total or trailer_size < 2 * 512:
            self._fail("archive.trailing_data", asset)
        source.seek(last_data_end)
        trailer = source.read(trailer_size)
        if len(trailer) != trailer_size or any(trailer) or source.read(1) != b"":
            self._fail("archive.trailing_data", asset)

    def _scan_zip(
        self,
        source: BinaryIO,
        *,
        asset: str,
        depth: int,
        compressed_size: int,
        contract: ArchiveContentContract | None,
    ) -> None:
        exact_names: set[str] = set()
        portable_names: dict[str, str] = {}
        path_kinds: dict[str, str] = {}
        contracted_seen: set[str] = set()
        with zipfile.ZipFile(source) as archive:
            infos = archive.infolist()
            self._validate_zip_envelope(source, archive, infos, asset=asset, compressed_size=compressed_size)
            for info in infos:
                member_asset = _member_asset(asset, info.filename)
                metadata = info.filename.encode("utf-8", errors="surrogatepass") + b"\0" + info.comment + b"\0" + info.extra
                self._scan_metadata(metadata, asset=member_asset)
                normalized = self._register_path(
                    info.filename,
                    asset=member_asset,
                    is_directory=info.is_dir(),
                    exact_names=exact_names,
                    portable_names=portable_names,
                    path_kinds=path_kinds,
                )
                if info.flag_bits & 0x1:
                    self._fail("archive.entry_encrypted", member_asset, info.filename.encode("utf-8"))
                if info.flag_bits & 0x8:
                    self._fail("archive.metadata_unsupported", member_asset, b"data-descriptor")
                if info.compress_type not in _SUPPORTED_ZIP_METHODS:
                    self._fail("archive.compression_unsupported", member_asset, str(info.compress_type).encode("ascii"))
                unix_type = (info.external_attr >> 16) & 0xF000
                if info.is_dir():
                    self.state.member_count += 1
                    if self.state.member_count > self.limits.max_members:
                        self._fail("archive.limit_members", member_asset)
                    if info.file_size != 0:
                        self._fail("archive.member_size_mismatch", member_asset)
                    if contract is not None:
                        if normalized not in contract.directories:
                            self._fail("archive.contract_unexpected_member", member_asset)
                        mode = (info.external_attr >> 16) & 0o777
                        if mode != 0o755:
                            self._fail("archive.contract_mode_mismatch", member_asset)
                        contracted_seen.add(normalized)
                    continue
                if unix_type not in {0, stat.S_IFREG}:
                    self._fail("archive.entry_special", member_asset, str(unix_type).encode("ascii"))
                expected = contract.files.get(normalized) if contract is not None else None
                mode = (info.external_attr >> 16) & 0o777
                if expected is not None and mode != expected.mode:
                    self._fail("archive.contract_mode_mismatch", member_asset)
                self._register_member(
                    size=info.file_size,
                    compressed_size=info.compress_size,
                    asset=member_asset,
                    depth=depth,
                )
                with archive.open(info, "r") as extracted:
                    self._scan_content_stream(
                        extracted,
                        size=info.file_size,
                        name=info.filename,
                        asset=member_asset,
                        depth=depth,
                        expected=expected,
                    )
                if contract is not None:
                    if expected is None:
                        self._fail("archive.contract_unexpected_member", member_asset)
                    contracted_seen.add(normalized)
            if contract is not None:
                expected_names = set(contract.files) | set(contract.directories)
                if contracted_seen != expected_names:
                    self._fail("archive.contract_missing_member", asset)

    def _validate_zip_envelope(
        self,
        source: BinaryIO,
        archive: zipfile.ZipFile,
        infos: list[zipfile.ZipInfo],
        *,
        asset: str,
        compressed_size: int,
    ) -> None:
        if archive.comment:
            self._scan_metadata(archive.comment, asset=asset)
            self._fail("archive.metadata_unsupported", asset, b"zip-comment")
        source.seek(max(0, compressed_size - (22 + 65_535)))
        tail_offset = source.tell()
        tail = source.read()
        eocd_relative = tail.rfind(_ZIP_EOCD_SIGNATURE)
        if eocd_relative < 0 or len(tail) - eocd_relative < 22:
            self._fail("archive.malformed", asset, b"zip-eocd")
        eocd_offset = tail_offset + eocd_relative
        (
            signature,
            disk_number,
            central_disk,
            disk_entries,
            total_entries,
            central_size,
            central_offset,
            comment_length,
        ) = struct.unpack("<4s4H2LH", tail[eocd_relative : eocd_relative + 22])
        if (
            signature != _ZIP_EOCD_SIGNATURE
            or disk_number != 0
            or central_disk != 0
            or disk_entries != total_entries
            or total_entries != len(infos)
            or comment_length != 0
            or eocd_offset + 22 != compressed_size
            or central_offset != archive.start_dir
            or central_offset + central_size != eocd_offset
        ):
            self._fail("archive.structure_unknown", asset, b"zip-eocd-fields")

        offsets = sorted(info.header_offset for info in infos)
        if offsets and offsets[0] != 0:
            self._fail("archive.trailing_data", asset, b"zip-prefix")
        for index, info in enumerate(sorted(infos, key=lambda value: value.header_offset)):
            if info.extra or info.comment:
                self._fail("archive.metadata_unsupported", _member_asset(asset, info.filename), b"zip-extra")
            if info.file_size >= 0xFFFFFFFF or info.compress_size >= 0xFFFFFFFF or info.header_offset >= 0xFFFFFFFF:
                self._fail("archive.structure_unknown", asset, b"zip64")
            source.seek(info.header_offset)
            local_header = source.read(30)
            if len(local_header) != 30:
                self._fail("archive.malformed", asset, b"zip-local-header")
            (
                local_signature,
                _version,
                local_flags,
                local_method,
                _time,
                _date,
                local_crc,
                local_compressed,
                local_size,
                name_length,
                extra_length,
            ) = struct.unpack("<L5H3L2H", local_header)
            local_name = source.read(name_length)
            if (
                local_signature != 0x04034B50
                or local_flags != info.flag_bits
                or local_method != info.compress_type
                or local_crc != info.CRC
                or local_compressed != info.compress_size
                or local_size != info.file_size
                or extra_length != 0
                or local_name != info.filename.encode("utf-8")
            ):
                self._fail("archive.structure_unknown", _member_asset(asset, info.filename), b"zip-local-fields")
            segment_end = info.header_offset + 30 + name_length + extra_length + info.compress_size
            expected_end = offsets[index + 1] if index + 1 < len(offsets) else archive.start_dir
            if segment_end != expected_end:
                self._fail("archive.trailing_data", _member_asset(asset, info.filename), b"zip-segment-gap")
        total_uncompressed = sum(info.file_size for info in infos)
        if total_uncompressed and total_uncompressed / max(compressed_size, 1) > self.limits.max_compression_ratio:
            self._fail("archive.limit_compression_ratio", asset)


def scan_public_release_archive(
    path: Path,
    *,
    archive_format: str,
    limits: PackageScanLimits | None = None,
    asset: str | None = None,
) -> dict[str, object]:
    scanner = PublicPackageScanner(limits=limits)
    scanner.scan_archive_path(path, archive_format=archive_format, asset=asset)
    return scanner.report()


def scan_public_release_tree(
    root: Path,
    *,
    asset: str,
    limits: PackageScanLimits | None = None,
) -> dict[str, object]:
    scanner = PublicPackageScanner(limits=limits)
    scanner.scan_tree(root, asset=asset)
    return scanner.report()
