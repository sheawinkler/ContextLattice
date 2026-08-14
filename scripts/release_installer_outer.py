#!/usr/bin/env python3
"""Generate and validate source-bound outer release installer files."""

from __future__ import annotations

import argparse
import binascii
import hashlib
import json
import re
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import unicodedata
from contextlib import ExitStack
from dataclasses import dataclass
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any
from xml.etree import ElementTree


SCHEMA_ID = "contextlattice_installer_outer_contract.v1"
CANONICAL_LINUX_GZIP_HEADER = bytes(
    (0x1F, 0x8B, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF)
)
MAX_LINUX_ARCHIVE_BYTES = 256 * 1024 * 1024
MAX_LINUX_EXPANDED_BYTES = 256 * 1024 * 1024
MAX_LINUX_MEMBER_BYTES = 192 * 1024 * 1024
MAX_LINUX_ARCHIVE_MEMBERS = 256
KINDS = ("linux", "macos", "windows")
LANES = ("paid", "public")
MIN_MACOS_VERSION = "13.0"
ALLOWED_MSI_TABLES = (
    "AdminExecuteSequence",
    "AdminUISequence",
    "AdvtExecuteSequence",
    "AppSearch",
    "Binary",
    "Component",
    "CreateFolder",
    "CustomAction",
    "Directory",
    "Error",
    "Feature",
    "FeatureComponents",
    "File",
    "Icon",
    "InstallExecuteSequence",
    "InstallUISequence",
    "LaunchCondition",
    "Media",
    "MsiFileHash",
    "Property",
    "RegLocator",
    "Registry",
    "RemoveFile",
    "ServiceControl",
    "ServiceInstall",
    "Shortcut",
    "Signature",
    "Upgrade",
    "_ForceCodepage",
    "_SummaryInformation",
)
REQUIRED_EMPTY_MSI_TABLES = (
    "AppSearch",
    "Binary",
    "CreateFolder",
    "CustomAction",
    "Icon",
    "RegLocator",
    "Registry",
    "RemoveFile",
    "ServiceControl",
    "ServiceInstall",
    "Shortcut",
    "Signature",
)


class OuterContractError(RuntimeError):
    """The outer installer differs from its reviewed source contract."""


@dataclass(frozen=True)
class OuterFile:
    content: bytes
    mode: int


def canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode("utf-8")


def _read(root: Path, relative: str) -> bytes:
    path = root / relative
    if not path.is_file() or path.is_symlink():
        raise OuterContractError(f"outer installer source is missing: {relative}")
    return path.read_bytes()


def _lane_template(root: Path, relative: str, lane: str) -> bytes:
    content = _read(root, relative)
    if b"@RELEASE_LANE@" not in content:
        raise OuterContractError(
            f"outer installer lane placeholder is absent: {relative}"
        )
    return content.replace(b"@RELEASE_LANE@", lane.encode("ascii"))


def _app_version(release_tag: str) -> str:
    match = re.match(r"^v([0-9]+(?:\.[0-9]+){0,2})(?:[-+].*)?$", release_tag)
    return match.group(1) if match else "0.0.0"


def _macos_info_plist(
    display_name: str, bundle_id: str, executable_name: str, version: str
) -> bytes:
    return f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleExecutable</key>
  <string>{executable_name}</string>
  <key>CFBundleIdentifier</key>
  <string>{bundle_id}</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>{display_name}</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>{version}</string>
  <key>CFBundleVersion</key>
  <string>{version}</string>
  <key>LSMinimumSystemVersion</key>
  <string>{MIN_MACOS_VERSION}</string>
  <key>LSUIElement</key>
  <false/>
  <key>NSHighResolutionCapable</key>
  <true/>
</dict>
</plist>
""".encode("utf-8")


def _macos_app_executable(launcher_name: str) -> bytes:
    return f"""#!/usr/bin/env bash
set -euo pipefail

APP_ROOT="$(cd "$(dirname "${{BASH_SOURCE[0]}}")/.." && pwd)"
LAUNCH_SCRIPT="${{APP_ROOT}}/Resources/{launcher_name}"

if [[ ! -x "${{LAUNCH_SCRIPT}}" ]]; then
  chmod +x "${{LAUNCH_SCRIPT}}" 2>/dev/null || true
fi

if [[ -t 1 ]]; then
  exec "${{LAUNCH_SCRIPT}}"
fi

if command -v osascript >/dev/null 2>&1; then
  /usr/bin/osascript \
    -e 'on run argv' \
    -e 'set launcherPath to item 1 of argv' \
    -e 'tell application "Terminal"' \
    -e 'activate' \
    -e 'do script quoted form of launcherPath' \
    -e 'end tell' \
    -e 'end run' \
    "${{LAUNCH_SCRIPT}}" >/dev/null
else
  exec "${{LAUNCH_SCRIPT}}"
fi
""".encode("utf-8")


def _macos_monitor_command() -> bytes:
    return b"""#!/usr/bin/env bash
set -euo pipefail

TARGET_DIR="${TARGET_DIR:-$HOME/ContextLattice}"
if [[ ! -d "${TARGET_DIR}" ]]; then
  echo "ERROR: ${TARGET_DIR} not found. Run ContextLattice first." >&2
  exit 1
fi

cd "${TARGET_DIR}"
if [[ -x ./scripts/open_monitoring.sh ]]; then
  ./scripts/open_monitoring.sh
else
  echo "ERROR: monitoring script is absent from the installed payload." >&2
  exit 1
fi
"""


def _macos_readme(lane: str, release_tag: str) -> bytes:
    return f"""ContextLattice macOS Release DMG
================================

This DMG contains a {lane} lane payload from {release_tag}.
No repository clone or pull is used during installation.

Included:
- ContextLattice.app and ContextLattice.command: verify, extract/update, and launch
- ContextLattice Monitoring.app and Monitoring.command: local health/status tools
- contextlattice-release.json: bounded lane/tag/commit identity

For deterministic extraction without Docker or network:
  ./ContextLattice.command --extract-only --install-dir /tmp/contextlattice

Existing .env and runtime data are preserved during updates.
Paid installs that create .env enable fail-closed entitlement/runtime-license
verification from the bundled public key registry; no private signing key is
included or required.
""".encode("utf-8")


def expected_files(
    root: Path, kind: str, lane: str, release_tag: str
) -> dict[str, OuterFile]:
    if kind not in KINDS:
        raise OuterContractError(f"unsupported outer installer kind: {kind}")
    if lane not in LANES:
        raise OuterContractError(f"unsupported release lane: {lane}")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,127}", release_tag):
        raise OuterContractError("release tag is invalid")

    if kind == "linux":
        return {
            "ContextLattice-Install.sh": OuterFile(
                _lane_template(root, "packaging/linux/ContextLattice-Install.sh", lane),
                0o755,
            ),
            "ContextLattice-Monitor.sh": OuterFile(
                _read(root, "packaging/linux/ContextLattice-Monitor.sh"), 0o755
            ),
            "README.txt": OuterFile(_read(root, "packaging/linux/README.txt"), 0o644),
        }
    if kind == "windows":
        return {
            "ContextLattice-Install.cmd": OuterFile(
                _read(root, "packaging/windows/ContextLattice-Install.cmd"), 0o644
            ),
            "ContextLattice-Monitor.cmd": OuterFile(
                _read(root, "packaging/windows/ContextLattice-Monitor.cmd"), 0o644
            ),
            "Install-ContextLattice.ps1": OuterFile(
                _lane_template(
                    root, "packaging/windows/Install-ContextLattice.ps1", lane
                ),
                0o644,
            ),
            "Monitor-ContextLattice.ps1": OuterFile(
                _read(root, "packaging/windows/Monitor-ContextLattice.ps1"), 0o644
            ),
            "README.txt": OuterFile(_read(root, "packaging/windows/README.txt"), 0o644),
        }

    install = _lane_template(root, "packaging/linux/ContextLattice-Install.sh", lane)
    monitor = _macos_monitor_command()
    version = _app_version(release_tag)
    return {
        "ContextLattice.command": OuterFile(install, 0o755),
        "Monitoring.command": OuterFile(monitor, 0o755),
        "README.txt": OuterFile(_macos_readme(lane, release_tag), 0o644),
        "ContextLattice.app/Contents/Info.plist": OuterFile(
            _macos_info_plist(
                "ContextLattice",
                "io.contextlattice.ContextLattice",
                "ContextLattice",
                version,
            ),
            0o644,
        ),
        "ContextLattice.app/Contents/MacOS/ContextLattice": OuterFile(
            _macos_app_executable("ContextLattice.command"), 0o755
        ),
        "ContextLattice.app/Contents/Resources/ContextLattice.command": OuterFile(
            install, 0o755
        ),
        "ContextLattice Monitoring.app/Contents/Info.plist": OuterFile(
            _macos_info_plist(
                "ContextLattice Monitoring",
                "io.contextlattice.ContextLatticeMonitoring",
                "ContextLatticeMonitoring",
                version,
            ),
            0o644,
        ),
        "ContextLattice Monitoring.app/Contents/MacOS/ContextLatticeMonitoring": OuterFile(
            _macos_app_executable("Monitoring.command"), 0o755
        ),
        "ContextLattice Monitoring.app/Contents/Resources/Monitoring.command": OuterFile(
            monitor, 0o755
        ),
    }


def contract_payload(root: Path, lane: str, release_tag: str) -> dict[str, Any]:
    artifacts: dict[str, Any] = {}
    for kind in KINDS:
        rows = []
        for relative, outer_file in sorted(
            expected_files(root, kind, lane, release_tag).items()
        ):
            rows.append(
                {
                    "path": relative,
                    "mode": f"{outer_file.mode:04o}",
                    "bytes": len(outer_file.content),
                    "sha256": hashlib.sha256(outer_file.content).hexdigest(),
                }
            )
        artifacts[kind] = {"files": rows}
    wix = _read(root, "packaging/windows/contextlattice.wxs")
    artifacts["windows"]["package_control"] = {
        "wix_source_sha256": hashlib.sha256(wix).hexdigest(),
        "allowed_msi_tables": list(ALLOWED_MSI_TABLES),
        "required_empty_msi_tables": list(REQUIRED_EMPTY_MSI_TABLES),
    }
    return {
        "schema_id": SCHEMA_ID,
        "lane": lane,
        "release_tag": release_tag,
        "minimum_macos_version": MIN_MACOS_VERSION,
        "artifacts": artifacts,
    }


def contract_sha256(root: Path, lane: str, release_tag: str) -> str:
    return hashlib.sha256(
        canonical_bytes(contract_payload(root, lane, release_tag))
    ).hexdigest()


def stage(root: Path, output: Path, kind: str, lane: str, release_tag: str) -> None:
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise OuterContractError(f"outer installer stage is not empty: {output}")
    for relative, outer_file in expected_files(root, kind, lane, release_tag).items():
        target = output / PurePosixPath(relative)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(outer_file.content)
        target.chmod(outer_file.mode)


def _dynamic_paths(kind: str) -> set[str]:
    if kind == "linux":
        return {
            "payload/contextlattice-payload.tar.gz",
            "payload/contextlattice-payload.tar.gz.sha256",
            "payload/contextlattice-release.json",
        }
    if kind == "windows":
        return {
            "payload/contextlattice-payload.zip",
            "payload/contextlattice-payload.zip.sha256",
            "payload/contextlattice-release.json",
        }
    return {
        "contextlattice-release.json",
        "ContextLattice.app/Contents/Resources/payload/contextlattice-payload.tar.gz",
        "ContextLattice.app/Contents/Resources/payload/contextlattice-payload.tar.gz.sha256",
        "ContextLattice.app/Contents/Resources/payload/contextlattice-release.json",
    }


def _signature_paths(kind: str) -> set[str]:
    if kind != "macos":
        return set()
    return {
        "ContextLattice.app/Contents/_CodeSignature/CodeResources",
        "ContextLattice Monitoring.app/Contents/_CodeSignature/CodeResources",
    }


def _parent_dirs(paths: set[str]) -> set[str]:
    directories: set[str] = set()
    for value in paths:
        path = PurePosixPath(value)
        for parent in path.parents:
            if parent.as_posix() != ".":
                directories.add(parent.as_posix())
    return directories


def validate_tree(
    root: Path,
    actual_root: Path,
    kind: str,
    lane: str,
    release_tag: str,
) -> dict[str, int | str]:
    file_records: dict[str, OuterFile] = {}
    actual_dirs: set[str] = set()
    if not actual_root.is_dir() or actual_root.is_symlink():
        raise OuterContractError("outer installer root is missing or unsafe")
    for path in actual_root.rglob("*"):
        relative = path.relative_to(actual_root).as_posix()
        if path.is_symlink():
            raise OuterContractError(f"outer installer contains a symlink: {relative}")
        if path.is_dir():
            actual_dirs.add(relative)
        elif path.is_file():
            file_records[relative] = OuterFile(
                path.read_bytes(), stat.S_IMODE(path.stat().st_mode)
            )
        else:
            raise OuterContractError(
                f"outer installer contains a special file: {relative}"
            )
    return _validate_records(
        root, kind, lane, release_tag, file_records, actual_dirs, check_modes=True
    )


def _write_canonical_linux_tar(stage_root: Path, tar_path: Path) -> None:
    if not stage_root.is_dir() or stage_root.is_symlink():
        raise OuterContractError("outer Linux archive stage is missing or unsafe")
    members = [
        stage_root,
        *sorted(
            stage_root.rglob("*"),
            key=lambda value: value.relative_to(stage_root).as_posix(),
        ),
    ]
    with tarfile.open(tar_path, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for path in members:
            if path.is_symlink():
                raise OuterContractError(
                    f"outer Linux archive stage contains a symlink: {path}"
                )
            relative = path.relative_to(stage_root).as_posix()
            name = "ContextLattice-linux-bootstrap"
            if relative != ".":
                name += "/" + relative
            info = tarfile.TarInfo(name)
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            info.mtime = 0
            info.pax_headers = {}
            if path.is_dir():
                info.type = tarfile.DIRTYPE
                info.mode = 0o755
                info.size = 0
                archive.addfile(info)
                continue
            if not path.is_file():
                raise OuterContractError(
                    f"outer Linux archive stage contains a special file: {path}"
                )
            info.type = tarfile.REGTYPE
            info.mode = 0o755 if stat.S_IMODE(path.stat().st_mode) & 0o111 else 0o644
            info.size = path.stat().st_size
            with path.open("rb") as content:
                archive.addfile(info, content)


def _write_stored_gzip(source_path: Path, archive_path: Path) -> None:
    """Write deterministic gzip bytes without host zlib dependencies."""
    crc = 0
    size = 0
    with source_path.open("rb") as source, archive_path.open("wb") as output:
        output.write(CANONICAL_LINUX_GZIP_HEADER)
        current = source.read(65535)
        if not current:
            output.write(b"\x01\x00\x00\xff\xff")
        while current:
            following = source.read(65535)
            output.write(b"\x01" if not following else b"\x00")
            output.write(struct.pack("<HH", len(current), 0xFFFF - len(current)))
            output.write(current)
            crc = binascii.crc32(current, crc)
            size = (size + len(current)) & 0xFFFFFFFF
            current = following
        output.write(struct.pack("<II", crc & 0xFFFFFFFF, size))


def build_linux_archive(stage_root: Path, archive_path: Path) -> None:
    """Write a byte-reproducible, host-independent Linux bootstrap archive."""
    archive_path.parent.mkdir(parents=True, exist_ok=True)
    tar_handle = tempfile.NamedTemporaryFile(
        prefix=".contextlattice-linux-", suffix=".tar", dir=archive_path.parent, delete=False
    )
    gzip_handle = tempfile.NamedTemporaryFile(
        prefix="." + archive_path.name + ".", dir=archive_path.parent, delete=False
    )
    tar_path, gzip_path = Path(tar_handle.name), Path(gzip_handle.name)
    tar_handle.close()
    gzip_handle.close()
    try:
        _write_canonical_linux_tar(stage_root, tar_path)
        _write_stored_gzip(tar_path, gzip_path)
        gzip_path.chmod(0o644)
        gzip_path.replace(archive_path)
    finally:
        tar_path.unlink(missing_ok=True)
        gzip_path.unlink(missing_ok=True)


def _read_exact(source: Any, length: int, description: str) -> bytes:
    raw = source.read(length)
    if len(raw) != length:
        raise OuterContractError(f"outer Linux archive {description} is truncated")
    return raw


def _open_validated_canonical_linux_gzip(archive_path: Path) -> Any:
    archive = None
    try:
        archive = archive_path.open("rb")
        archive.seek(0, 2)
        compressed_size = archive.tell()
        archive.seek(0)
        if compressed_size > MAX_LINUX_ARCHIVE_BYTES:
            raise OuterContractError(
                "outer Linux archive exceeds the compressed-size limit"
            )
        header = _read_exact(
            archive, len(CANONICAL_LINUX_GZIP_HEADER), "gzip header"
        )
        if header != CANONICAL_LINUX_GZIP_HEADER:
            raise OuterContractError(
                "outer Linux archive gzip header is not canonical (zero timestamp, no host fields)"
            )
        crc = 0
        expanded_size = 0
        block_count = 0
        while True:
            marker = _read_exact(archive, 1, "stored-block marker")[0]
            if marker not in (0, 1):
                raise OuterContractError(
                    "outer Linux archive gzip stream is not canonical stored-block deflate"
                )
            length, complement = struct.unpack(
                "<HH", _read_exact(archive, 4, "stored-block length")
            )
            if complement != 0xFFFF - length:
                raise OuterContractError(
                    "outer Linux archive stored-block length is invalid"
                )
            if marker == 0 and length != 65535:
                raise OuterContractError(
                    "outer Linux archive non-final stored block is not canonical"
                )
            block_count += 1
            if block_count > (MAX_LINUX_EXPANDED_BYTES // 65535) + 2:
                raise OuterContractError(
                    "outer Linux archive has too many stored blocks"
                )
            expanded_size += length
            if expanded_size > MAX_LINUX_EXPANDED_BYTES:
                raise OuterContractError(
                    "outer Linux archive exceeds the expanded-size limit"
                )
            remaining = length
            while remaining:
                chunk = _read_exact(
                    archive, min(remaining, 1024 * 1024), "stored-block payload"
                )
                crc = binascii.crc32(chunk, crc)
                remaining -= len(chunk)
            if marker == 1:
                break
        expected_crc, expected_size = struct.unpack(
            "<II", _read_exact(archive, 8, "gzip trailer")
        )
        if expected_crc != crc & 0xFFFFFFFF or expected_size != expanded_size & 0xFFFFFFFF:
            raise OuterContractError(
                "outer Linux archive gzip trailer is invalid"
            )
        if archive.read(1) != b"":
            raise OuterContractError(
                "outer Linux archive contains trailing or concatenated gzip data"
            )
        archive.seek(0)
        return archive
    except OSError as exc:
        if archive is not None:
            archive.close()
        raise OuterContractError(f"outer Linux archive cannot be read: {exc}") from exc
    except Exception:
        if archive is not None:
            archive.close()
        raise


def _validate_records(
    root: Path,
    kind: str,
    lane: str,
    release_tag: str,
    file_records: dict[str, OuterFile],
    actual_dirs: set[str],
    *,
    check_modes: bool,
) -> dict[str, int | str]:
    expected = expected_files(root, kind, lane, release_tag)
    dynamic = _dynamic_paths(kind)
    signatures = _signature_paths(kind)
    allowed_files = set(expected) | dynamic | signatures
    allowed_dirs = _parent_dirs(allowed_files)
    actual_files = set(file_records)
    extra_files = sorted(actual_files - allowed_files)
    missing_files = sorted((set(expected) | dynamic) - actual_files)
    extra_dirs = sorted(actual_dirs - allowed_dirs)
    if extra_files:
        raise OuterContractError(
            "outer installer contains unbound files: " + ", ".join(extra_files)
        )
    if missing_files:
        raise OuterContractError(
            "outer installer is missing contracted files: " + ", ".join(missing_files)
        )
    if extra_dirs:
        raise OuterContractError(
            "outer installer contains unbound directories: " + ", ".join(extra_dirs)
        )
    for relative, outer_file in expected.items():
        actual = file_records[relative]
        if actual.content != outer_file.content:
            raise OuterContractError(
                f"outer installer file differs from reviewed source: {relative}"
            )
        if check_modes and kind != "windows" and actual.mode != outer_file.mode:
            raise OuterContractError(
                f"outer installer mode differs from reviewed source: {relative}"
            )

    metadata_relative = (
        "contextlattice-release.json"
        if kind == "macos"
        else "payload/contextlattice-release.json"
    )
    try:
        metadata = json.loads(file_records[metadata_relative].content)
    except (KeyError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise OuterContractError(f"outer installer metadata is invalid: {exc}") from exc
    if metadata.get("installer_outer_contract_schema_id") != SCHEMA_ID:
        raise OuterContractError("outer installer metadata contract schema is invalid")
    digest = contract_sha256(root, lane, release_tag)
    if metadata.get("installer_outer_contract_sha256") != digest:
        raise OuterContractError("outer installer metadata contract hash is invalid")
    return {
        "contracted_files": len(expected),
        "dynamic_files": len(dynamic),
        "signature_files": len(actual_files & signatures),
        "contract_sha256": digest,
    }


def validate_linux_archive(
    root: Path, archive_path: Path, lane: str, release_tag: str
) -> dict[str, int | str]:
    prefix = "ContextLattice-linux-bootstrap"
    file_records: dict[str, OuterFile] = {}
    directories: set[str] = set()
    with ExitStack() as stack:
        archive_source = stack.enter_context(
            _open_validated_canonical_linux_gzip(archive_path)
        )
        archive = stack.enter_context(tarfile.open(fileobj=archive_source, mode="r:gz"))
        if archive.pax_headers:
            raise OuterContractError("outer Linux archive contains global PAX metadata")
        seen: set[str] = set()
        member_count = 0
        aggregate_member_bytes = 0
        for member in archive:
            member_count += 1
            if member_count > MAX_LINUX_ARCHIVE_MEMBERS:
                raise OuterContractError(
                    "outer Linux archive exceeds the member-count limit"
                )
            if member.size < 0 or member.size > MAX_LINUX_MEMBER_BYTES:
                raise OuterContractError(
                    f"outer Linux archive member exceeds the size limit: {member.name}"
                )
            aggregate_member_bytes += member.size
            if aggregate_member_bytes > MAX_LINUX_EXPANDED_BYTES:
                raise OuterContractError(
                    "outer Linux archive members exceed the aggregate-size limit"
                )
            name = member.name.rstrip("/")
            if name in seen:
                raise OuterContractError(
                    f"outer Linux archive contains a duplicate path: {name}"
                )
            seen.add(name)
            path = PurePosixPath(name)
            if (
                path.is_absolute()
                or not path.parts
                or path.parts[0] != prefix
                or any(part in {"", ".", ".."} for part in path.parts)
            ):
                raise OuterContractError(
                    f"outer Linux archive contains an unsafe path: {name!r}"
                )
            relative_path = PurePosixPath(*path.parts[1:])
            if (
                member.uid != 0
                or member.gid != 0
                or member.uname != ""
                or member.gname != ""
                or member.mtime != 0
                or member.pax_headers
            ):
                raise OuterContractError(
                    f"outer Linux archive contains non-canonical owner, time, or PAX metadata: {name}"
                )
            if not relative_path.parts:
                if not member.isdir() or member.mode & 0o7777 != 0o755:
                    raise OuterContractError(
                        "outer Linux archive root metadata is not canonical"
                    )
                continue
            relative = relative_path.as_posix()
            if member.isdir():
                if member.mode & 0o7777 != 0o755:
                    raise OuterContractError(
                        f"outer Linux archive directory mode is not canonical: {relative}"
                    )
                directories.add(relative)
                continue
            if not member.isfile():
                raise OuterContractError(
                    f"outer Linux archive contains a link or special file: {relative}"
                )
            extracted = archive.extractfile(member)
            if extracted is None:
                raise OuterContractError(
                    f"outer Linux archive file cannot be read: {relative}"
                )
            content = extracted.read(member.size + 1)
            if len(content) != member.size:
                raise OuterContractError(
                    f"outer Linux archive file size is inconsistent: {relative}"
                )
            file_records[relative] = OuterFile(content, member.mode & 0o7777)
            if relative in _dynamic_paths("linux") and member.mode & 0o7777 != 0o644:
                raise OuterContractError(
                    f"outer Linux archive dynamic payload mode is not canonical: {relative}"
                )
    result = _validate_records(
        root,
        "linux",
        lane,
        release_tag,
        file_records,
        directories,
        check_modes=True,
    )
    with tempfile.TemporaryDirectory(
        prefix=".contextlattice-linux-canonical-", dir=archive_path.parent
    ) as temporary:
        stage_root = Path(temporary) / "stage"
        stage_root.mkdir(mode=0o755)
        for relative in sorted(directories, key=lambda value: (value.count("/"), value)):
            directory = stage_root / PurePosixPath(relative)
            directory.mkdir(parents=True, exist_ok=True)
            directory.chmod(0o755)
        for relative, record in sorted(file_records.items()):
            target = stage_root / PurePosixPath(relative)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(record.content)
            target.chmod(record.mode)
        canonical_path = Path(temporary) / "canonical.tar.gz"
        build_linux_archive(stage_root, canonical_path)
        if (
            canonical_path.stat().st_size != archive_path.stat().st_size
            or _file_sha256(canonical_path) != _file_sha256(archive_path)
        ):
            raise OuterContractError(
                "outer Linux archive bytes are not the canonical deterministic encoding"
            )
    return result


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_msi_tables(msi_path: Path, msiinfo: str = "msiinfo") -> dict[str, int]:
    result = subprocess.run(
        [msiinfo, "tables", str(msi_path)],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode != 0:
        raise OuterContractError(
            result.stderr.strip() or "could not enumerate final MSI tables"
        )
    tables = {value.strip() for value in result.stdout.splitlines() if value.strip()}
    unexpected = sorted(tables.difference(ALLOWED_MSI_TABLES))
    if unexpected:
        raise OuterContractError(
            "final MSI contains unreviewed tables: " + ", ".join(unexpected)
        )
    populated_required_empty: list[str] = []
    checked = 0
    for table in sorted(tables.intersection(REQUIRED_EMPTY_MSI_TABLES)):
        checked += 1
        exported = subprocess.run(
            [msiinfo, "export", str(msi_path), table],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        if exported.returncode != 0:
            raise OuterContractError(
                exported.stderr.strip() or f"could not inspect final MSI table: {table}"
            )
        rows = [value for value in exported.stdout.splitlines()[3:] if value.strip()]
        if rows:
            populated_required_empty.append(table)
    if populated_required_empty:
        raise OuterContractError(
            "final MSI contains forbidden execution/side-effect tables: "
            + ", ".join(populated_required_empty)
        )
    return {
        "tables": len(tables),
        "allowed_tables_checked": len(tables),
        "required_empty_tables_checked": checked,
        "populated_required_empty_tables": 0,
    }


def _wix_tag(element: ElementTree.Element) -> str:
    return element.tag.rsplit("}", 1)[-1]


def _wix_file_name(value: str, *, description: str) -> str:
    """Resolve a WiX file name (including an optional short|long pair)."""
    normalized = value.replace("\\", "/").strip()
    if not normalized or "/" in normalized:
        raise OuterContractError(f"reviewed MSI WXS {description} name is unsafe")
    names = normalized.split("|")
    if len(names) > 2 or any(not name for name in names):
        raise OuterContractError(f"reviewed MSI WXS {description} name is unsafe")
    for name in names:
        if (
            name in {".", ".."}
            or any(ord(character) < 32 for character in name)
            or any(character in '<>:"/\\?*' for character in name)
        ):
            raise OuterContractError(f"reviewed MSI WXS {description} name is unsafe")
    return names[-1]


def _wix_source_name(value: str, *, description: str) -> str:
    normalized = value.replace("\\", "/").strip()
    path = PurePosixPath(normalized)
    windows_path = PureWindowsPath(normalized)
    if (
        not normalized
        or path.is_absolute()
        or windows_path.drive
        or windows_path.root
        or any(":" in part for part in path.parts)
        or any(part in {"", ".", ".."} for part in path.parts)
    ):
        raise OuterContractError(f"reviewed MSI WXS {description} path is unsafe")
    return _wix_file_name(path.name, description=description)


def _windows_destination_key(value: str) -> str:
    """Return the collision key used by Windows path semantics."""
    normalized = unicodedata.normalize("NFKC", value.replace("\\", "/"))
    parts = normalized.split("/")
    if not normalized or any(part in {"", ".", ".."} for part in parts):
        raise OuterContractError("reviewed MSI WXS file destination is unsafe")
    key_parts: list[str] = []
    for part in parts:
        # Windows ignores trailing spaces/dots and compares names case-insensitively.
        component = part.rstrip(" .")
        if not component:
            raise OuterContractError("reviewed MSI WXS file destination is unsafe")
        key_parts.append(component.casefold())
    return "/".join(key_parts)


def _wix_expected_msi_closure(wxs_path: Path) -> dict[str, object]:
    try:
        root = ElementTree.parse(wxs_path).getroot()
    except (OSError, ElementTree.ParseError) as exc:
        raise OuterContractError("reviewed MSI WXS structure cannot be parsed") from exc

    directories: dict[str, dict[str, str | None]] = {}

    def visit(element: ElementTree.Element, parent_id: str | None = None) -> None:
        if _wix_tag(element) == "Directory":
            directory_id = (element.get("Id") or "").strip()
            name = (element.get("Name") or "").strip()
            if (
                not directory_id
                or directory_id in directories
                or (directory_id != "ProgramFilesFolder" and not name)
            ):
                raise OuterContractError("reviewed MSI WXS directory keys are ambiguous")
            directories[directory_id] = {"parent": parent_id, "name": name}
            for child in element:
                visit(child, directory_id)
            return
        for child in element:
            visit(child, parent_id)

    visit(root)
    if not {"TARGETDIR", "ProgramFilesFolder", "INSTALLDIR", "PAYLOADDIR"}.issubset(directories):
        raise OuterContractError("reviewed MSI WXS directory closure is incomplete")

    directory_paths: dict[str, str] = {}

    def resolve_directory(directory_id: str, active: set[str] | None = None) -> str:
        if directory_id in directory_paths:
            return directory_paths[directory_id]
        active = set() if active is None else active
        if directory_id in active or directory_id not in directories:
            raise OuterContractError("reviewed MSI WXS directory references are ambiguous")
        active.add(directory_id)
        row = directories[directory_id]
        parent = row["parent"]
        if directory_id == "TARGETDIR":
            path = ""
        elif directory_id == "ProgramFilesFolder":
            if parent != "TARGETDIR":
                raise OuterContractError("reviewed MSI WXS ProgramFilesFolder parent is invalid")
            path = "Program Files"
        else:
            if not isinstance(parent, str) or not parent:
                raise OuterContractError("reviewed MSI WXS directory parent is missing")
            raw_name = str(row["name"])
            if "/" in raw_name or "\\" in raw_name:
                raise OuterContractError("reviewed MSI WXS directory name is unsafe")
            name = _wix_source_name(raw_name, description="directory")
            parent_path = resolve_directory(parent, active)
            path = "/".join(part for part in (parent_path, name) if part)
        directory_paths[directory_id] = path
        return path

    for directory_id in directories:
        resolve_directory(directory_id)

    components: dict[str, dict[str, object]] = {}
    files: dict[str, dict[str, str]] = {}
    for element in root.iter():
        if _wix_tag(element) != "DirectoryRef":
            continue
        directory_id = (element.get("Id") or "").strip()
        if directory_id not in directory_paths:
            raise OuterContractError("reviewed MSI WXS DirectoryRef is missing")
        for component_element in element:
            if _wix_tag(component_element) != "Component":
                continue
            component_id = (component_element.get("Id") or "").strip()
            if not component_id or component_id in components:
                raise OuterContractError("reviewed MSI WXS component keys are ambiguous")
            component_files: set[str] = set()
            key_path: str | None = None
            for file_element in component_element:
                if _wix_tag(file_element) != "File":
                    continue
                file_id = (file_element.get("Id") or "").strip()
                source = file_element.get("Source") or ""
                if not file_id or file_id in files or file_id in component_files:
                    raise OuterContractError("reviewed MSI WXS file keys are ambiguous")
                source_name = _wix_source_name(source, description="file source")
                name_override = file_element.get("Name")
                destination_name = (
                    source_name
                    if name_override is None
                    else _wix_file_name(name_override, description="file override")
                )
                destination = "/".join(
                    part for part in (directory_paths[directory_id], destination_name) if part
                )
                files[file_id] = {
                    "component": component_id,
                    "directory": directory_id,
                    "name": destination_name,
                    "destination": destination,
                }
                component_files.add(file_id)
                if (file_element.get("KeyPath") or "").strip().lower() == "yes":
                    if key_path is not None:
                        raise OuterContractError("reviewed MSI WXS component key paths are ambiguous")
                    key_path = file_id
            if not component_files or key_path is None:
                raise OuterContractError("reviewed MSI WXS component has no files")
            components[component_id] = {
                "directory": directory_id,
                "files": component_files,
                "key_path": key_path,
            }

    if not components or not files:
        raise OuterContractError("reviewed MSI WXS component/file closure is empty")
    destinations = [_windows_destination_key(str(row["destination"])) for row in files.values()]
    if len(destinations) != len(set(destinations)):
        raise OuterContractError("reviewed MSI WXS file destinations collide on Windows")
    return {
        "directories": directories,
        "directory_paths": directory_paths,
        "components": components,
        "files": files,
    }


def _parse_msi_export(raw: bytes | str, *, table: str) -> list[dict[str, str]]:
    try:
        text = raw.decode("utf-8") if isinstance(raw, bytes) else raw
    except UnicodeError as exc:
        raise OuterContractError(f"MSI {table} table is not UTF-8") from exc
    lines = text.splitlines()
    if len(lines) < 3:
        raise OuterContractError(f"MSI {table} table header is incomplete")
    columns = lines[0].split("\t")
    if not columns or any(not column for column in columns) or len(columns) != len(set(columns)):
        raise OuterContractError(f"MSI {table} table columns are ambiguous")
    rows: list[dict[str, str]] = []
    seen: set[str] = set()
    for line in lines[3:]:
        if not line.strip():
            continue
        values = line.split("\t")
        if len(values) != len(columns) or not values[0].strip():
            raise OuterContractError(f"MSI {table} table row is malformed")
        key = values[0].strip()
        if key in seen:
            raise OuterContractError(f"MSI {table} table keys are ambiguous")
        seen.add(key)
        rows.append({column: value.strip() for column, value in zip(columns, values)})
    return rows


def _msi_long_name(value: str, *, description: str) -> str:
    value = value.strip()
    if "|" in value:
        value = value.split("|", 1)[1]
    if not value or "/" in value or "\\" in value or value in {".", ".."}:
        raise OuterContractError(f"MSI {description} destination is unsafe")
    return value


def validate_msi_table_closure(
    source_root: Path,
    directory_output: bytes | str,
    component_output: bytes | str,
    file_output: bytes | str,
) -> dict[str, int]:
    """Cross-reference MSI Directory/Component/File rows to reviewed WXS."""
    expected = _wix_expected_msi_closure(
        source_root / "packaging/windows/contextlattice.wxs"
    )
    expected_directories = expected["directories"]
    expected_paths = expected["directory_paths"]
    expected_components = expected["components"]
    expected_files = expected["files"]
    if not isinstance(expected_directories, dict) or not isinstance(expected_paths, dict):
        raise OuterContractError("reviewed MSI WXS directory closure is malformed")
    if not isinstance(expected_components, dict) or not isinstance(expected_files, dict):
        raise OuterContractError("reviewed MSI WXS component/file closure is malformed")

    directories = _parse_msi_export(directory_output, table="Directory")
    directory_rows = {row["Directory"]: row for row in directories}
    if set(directory_rows) != set(expected_directories):
        raise OuterContractError("MSI Directory table differs from reviewed WXS")
    actual_paths: dict[str, str] = {}
    for directory_id, expected_row in expected_directories.items():
        row = directory_rows[directory_id]
        parent = row.get("Directory_Parent", "")
        expected_parent = expected_row["parent"] or ""
        if parent != expected_parent:
            raise OuterContractError("MSI Directory parent differs from reviewed WXS")
        default_name = _msi_long_name(row.get("DefaultDir", ""), description="directory")
        if directory_id == "TARGETDIR":
            if default_name != "SourceDir":
                raise OuterContractError("MSI TARGETDIR name differs from reviewed WXS")
            actual_path = ""
        elif directory_id == "ProgramFilesFolder":
            if default_name not in {"PFiles", "Program Files"}:
                raise OuterContractError(
                    "MSI ProgramFilesFolder name differs from reviewed WXS"
                )
            actual_path = "Program Files"
        else:
            if parent not in actual_paths:
                raise OuterContractError("MSI Directory parent reference is missing")
            actual_path = "/".join(
                part for part in (actual_paths[parent], default_name) if part
            )
        if actual_path != expected_paths[directory_id]:
            raise OuterContractError("MSI Directory destination differs from reviewed WXS")
        actual_paths[directory_id] = actual_path

    components = _parse_msi_export(component_output, table="Component")
    component_rows = {row["Component"]: row for row in components}
    if set(component_rows) != set(expected_components):
        raise OuterContractError("MSI Component table differs from reviewed WXS")
    component_ids: set[str] = set()
    for component_id, expected_row in expected_components.items():
        row = component_rows[component_id]
        directory_id = expected_row["directory"]
        if row.get("Directory_") != directory_id:
            raise OuterContractError("MSI Component directory reference differs from reviewed WXS")
        if row.get("KeyPath") != expected_row["key_path"]:
            raise OuterContractError("MSI Component key path differs from reviewed WXS")
        component_guid = row.get("ComponentId", "")
        if not component_guid or component_guid in component_ids:
            raise OuterContractError("MSI Component identities are ambiguous")
        component_ids.add(component_guid)

    files = _parse_msi_export(file_output, table="File")
    file_rows = {row["File"]: row for row in files}
    if set(file_rows) != set(expected_files):
        raise OuterContractError("MSI File table differs from reviewed WXS")
    actual_destinations: set[str] = set()
    files_by_component: dict[str, set[str]] = {component_id: set() for component_id in expected_components}
    for file_id, expected_row in expected_files.items():
        row = file_rows[file_id]
        component_id = expected_row["component"]
        if row.get("Component_") != component_id:
            raise OuterContractError("MSI File component reference differs from reviewed WXS")
        actual_name = _msi_long_name(row.get("FileName", ""), description="file")
        if actual_name != expected_row["name"]:
            raise OuterContractError("MSI File name differs from reviewed WXS")
        directory_id = expected_components[component_id]["directory"]
        destination = "/".join(part for part in (actual_paths[directory_id], actual_name) if part)
        destination_key = _windows_destination_key(destination)
        if destination_key in actual_destinations:
            raise OuterContractError("MSI File destinations collide on Windows")
        actual_destinations.add(destination_key)
        files_by_component[component_id].add(file_id)
    for component_id, expected_row in expected_components.items():
        if files_by_component[component_id] != expected_row["files"]:
            raise OuterContractError("MSI Component file closure differs from reviewed WXS")
    expected_destinations = {
        _windows_destination_key(str(row["destination"]))
        for row in expected_files.values()
    }
    if actual_destinations != expected_destinations:
        raise OuterContractError("MSI file destinations differ from reviewed WXS")
    return {
        "directories": len(directory_rows),
        "components": len(component_rows),
        "files": len(file_rows),
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for command in ("contract", "stage", "validate"):
        current = subparsers.add_parser(command)
        current.add_argument("--root", default=".")
        current.add_argument("--lane", choices=LANES, required=True)
        current.add_argument("--release-tag", required=True)
        if command == "contract":
            current.add_argument("--output", default="")
        else:
            current.add_argument("--kind", choices=KINDS, required=True)
            current.add_argument(
                "--output" if command == "stage" else "--actual-root", required=True
            )
    linux_builder = subparsers.add_parser("build-linux-archive")
    linux_builder.add_argument("--stage", required=True)
    linux_builder.add_argument("--archive", required=True)
    msi = subparsers.add_parser("validate-msi")
    msi.add_argument("--msi", required=True)
    msi.add_argument("--msiinfo", default="msiinfo")
    msi_closure = subparsers.add_parser("validate-msi-closure")
    msi_closure.add_argument("--root", default=".")
    msi_closure.add_argument("--directory-export", required=True)
    msi_closure.add_argument("--component-export", required=True)
    msi_closure.add_argument("--file-export", required=True)
    linux_archive = subparsers.add_parser("validate-linux-archive")
    linux_archive.add_argument("--root", default=".")
    linux_archive.add_argument("--lane", choices=LANES, required=True)
    linux_archive.add_argument("--release-tag", required=True)
    linux_archive.add_argument("--archive", required=True)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        if args.command == "build-linux-archive":
            build_linux_archive(
                Path(args.stage).resolve(), Path(args.archive).resolve()
            )
            # CLI receipts must remain portable; the caller already knows the
            # task-scoped output location and no machine path belongs in a
            # release proof or log artifact.
            result = {"archive": Path(args.archive).name}
        elif args.command == "validate-msi":
            result: Any = validate_msi_tables(Path(args.msi).resolve(), args.msiinfo)
        elif args.command == "validate-msi-closure":
            result = validate_msi_table_closure(
                Path(args.root).resolve(),
                Path(args.directory_export).read_bytes(),
                Path(args.component_export).read_bytes(),
                Path(args.file_export).read_bytes(),
            )
        elif args.command == "validate-linux-archive":
            result = validate_linux_archive(
                Path(args.root).resolve(),
                Path(args.archive).resolve(),
                args.lane,
                args.release_tag,
            )
        else:
            root = Path(args.root).resolve()
            if args.command == "contract":
                payload = contract_payload(root, args.lane, args.release_tag)
                payload["contract_sha256"] = hashlib.sha256(
                    canonical_bytes(payload)
                ).hexdigest()
                rendered = json.dumps(payload, indent=2, sort_keys=True) + "\n"
                if args.output:
                    target = Path(args.output)
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text(rendered, encoding="utf-8")
                else:
                    sys.stdout.write(rendered)
                return 0
            if args.command == "stage":
                stage(
                    root,
                    Path(args.output).resolve(),
                    args.kind,
                    args.lane,
                    args.release_tag,
                )
                result = {
                    "staged_files": len(
                        expected_files(root, args.kind, args.lane, args.release_tag)
                    )
                }
            else:
                result = validate_tree(
                    root,
                    Path(args.actual_root).resolve(),
                    args.kind,
                    args.lane,
                    args.release_tag,
                )
        print(json.dumps({"ok": True, **result}, sort_keys=True))
    except (OSError, OuterContractError, UnicodeError, ValueError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
