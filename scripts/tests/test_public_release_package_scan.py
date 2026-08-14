from __future__ import annotations

import gzip
import hashlib
import importlib.machinery
import importlib.util
import io
import json
import os
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zipfile
from collections.abc import Callable
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

import release_installer_outer as OUTER  # noqa: E402
from public_release_material_scan import PublicReleaseMaterialError  # noqa: E402
from public_release_package_scan import (  # noqa: E402
    PackageScanLimits,
    scan_public_release_archive,
    scan_public_release_tree,
)


AUDIT_PATH = ROOT / "scripts/agent/audit-public-installer-package"
PUBLIC_WORKFLOW = ROOT / ".github/workflows/public-release-installers.yml"
AUDIT_LOADER = importlib.machinery.SourceFileLoader(
    "audit_public_installer_package",
    str(AUDIT_PATH),
)
AUDIT_SPEC = importlib.util.spec_from_loader(AUDIT_LOADER.name, AUDIT_LOADER)
if AUDIT_SPEC is None or AUDIT_SPEC.loader is None:
    raise RuntimeError("could not load public installer package auditor")
AUDIT = importlib.util.module_from_spec(AUDIT_SPEC)
sys.modules[AUDIT_SPEC.name] = AUDIT
AUDIT_SPEC.loader.exec_module(AUDIT)

TAG = "v5.0.0"


def _tar_gz_bytes(entries: list[tuple[str, bytes | None, bytes | None]]) -> bytes:
    raw = io.BytesIO()
    with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w", format=tarfile.GNU_FORMAT) as archive:
            for name, content, member_type in entries:
                info = tarfile.TarInfo(name)
                info.uid = 0
                info.gid = 0
                info.uname = "root"
                info.gname = "root"
                info.mtime = 0
                info.mode = 0o755 if content is None else 0o644
                info.pax_headers = {}
                if member_type is not None:
                    info.type = member_type
                    info.size = 0
                    if member_type in {tarfile.SYMTYPE, tarfile.LNKTYPE}:
                        info.linkname = "target"
                    archive.addfile(info)
                elif content is None:
                    info.type = tarfile.DIRTYPE
                    info.size = 0
                    archive.addfile(info)
                else:
                    info.type = tarfile.REGTYPE
                    info.size = len(content)
                    archive.addfile(info, io.BytesIO(content))
    return raw.getvalue()


def _zip_bytes(entries: list[tuple[str, bytes]]) -> bytes:
    raw = io.BytesIO()
    with zipfile.ZipFile(
        raw,
        mode="w",
        compression=zipfile.ZIP_DEFLATED,
        compresslevel=9,
        strict_timestamps=True,
    ) as archive:
        for name, content in entries:
            info = zipfile.ZipInfo(name, date_time=(2026, 8, 11, 0, 0, 0))
            info.create_system = 3
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = ((0o755 if name.endswith("/") else 0o644) & 0xFFFF) << 16
            if name.endswith("/"):
                info.external_attr |= 0x10
            archive.writestr(info, content)
    return raw.getvalue()


def _write(path: Path, content: bytes) -> Path:
    path.write_bytes(content)
    return path


class PublicReleasePackageScanTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="public-package-scan-")
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def assert_sanitized_failure(
        self,
        callable_value: Callable[[], object],
        expected_code: str,
        forbidden: bytes | str = b"",
    ) -> PublicReleaseMaterialError:
        with self.assertRaises(PublicReleaseMaterialError) as caught:
            callable_value()
        rendered = str(caught.exception)
        self.assertIn(expected_code, rendered)
        if forbidden:
            value = forbidden.decode("ascii") if isinstance(forbidden, bytes) else forbidden
            self.assertNotIn(value, rendered)
        findings = caught.exception.report["findings"]
        self.assertTrue(
            all(set(finding) == {"asset", "code", "evidence_digest"} for finding in findings)
        )
        return caught.exception

    def archive_path(self, name: str, content: bytes) -> Path:
        return _write(self.root / name, content)

    def source_repo(self) -> Path:
        source = self.root / "reviewed-source"
        if source.exists():
            return source
        source.mkdir()
        shutil.copytree(ROOT / "packaging", source / "packaging")
        for relative in (
            "scripts/agent/audit-public-installer-package",
            "scripts/public_release_package_scan.py",
            "scripts/release_installer_outer.py",
        ):
            destination = source / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / relative, destination)
        (source / ".gitattributes").write_text(
            "/.gitattributes export-ignore\n/packaging export-ignore\n"
            "/scripts export-ignore\n"
            "/scripts/agent/audit-public-installer-package export-ignore\n"
            "/scripts/public_release_package_scan.py export-ignore\n"
            "/scripts/release_installer_outer.py export-ignore\n",
            encoding="utf-8",
        )
        (source / ".env.example").write_text(
            "# STORAGE_ROOT=/Volumes/ExternalSSD/contextlattice\n",
            encoding="utf-8",
        )
        (source / "Makefile").write_text("all:\n\t@true\n", encoding="utf-8")
        (source / "docker-compose.lite.yml").write_text(
            "services: {}\n",
            encoding="utf-8",
        )
        runtime = source / "services/gateway-go/runtime.txt"
        runtime.parent.mkdir(parents=True)
        runtime.write_text("bounded public runtime\n", encoding="utf-8")
        commands = (
            ("init", "-q"),
            ("add", "."),
            (
                "-c",
                "user.name=Release Test",
                "-c",
                "user.email=release-test@example.invalid",
                "commit",
                "-q",
                "-m",
                "reviewed source",
            ),
            ("tag", TAG),
        )
        for command in commands:
            subprocess.run(
                ["git", "-C", str(source), *command],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=True,
            )
        return source

    def payload_metadata(self) -> bytes:
        source = self.source_repo()
        commit = subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD"],
            text=True,
        ).strip()
        tree = subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD^{tree}"],
            text=True,
        ).strip()
        metadata = {
            "approved_source_ref": "refs/heads/main",
            "approved_source_repository": "sheawinkler/ContextLattice",
            "build_channel": "stable",
            "commit": commit,
            "forbidden_paid_marker_paths": ["services/gateway-go/private-entitled.go"],
            "forbidden_paid_runtime_paths": ["config/runtime-license/public_keys.json"],
            "frontier_t1_eval_ledger_sha256": "",
            "frontier_t1_private_proof_commit": "",
            "frontier_t1_provenance_blob_sha1": "",
            "frontier_t1_release_gate": "not_applicable",
            "frontier_t1_reviewed_source_commit": "",
            "frontier_t1_reviewed_source_tree": "",
            "frontier_t1_tested_source_manifest_sha256": "",
            "frontier_t2_release_gate": "not_applicable",
            "installer_outer_contract_schema_id": OUTER.SCHEMA_ID,
            "installer_outer_contract_sha256": OUTER.contract_sha256(source, "public", TAG),
            "lane": "public",
            "payload_root": "contextlattice",
            "release_ref": f"refs/tags/{TAG}",
            "required_paid_markers": [],
            "required_paid_runtime_files": [],
            "runtime_version": TAG.removeprefix("v"),
            "schema_id": "contextlattice_release_payload.v2",
            "source": "approved_lane_tagged_checkout",
            "source_tree": tree,
            "tag": TAG,
        }
        return (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode("utf-8")

    def scan_tar(self, content: bytes, *, limits: PackageScanLimits | None = None) -> dict[str, object]:
        return scan_public_release_archive(
            self.archive_path("fixture.tar.gz", content),
            archive_format="tar.gz",
            limits=limits,
        )

    def scan_zip(self, content: bytes, *, limits: PackageScanLimits | None = None) -> dict[str, object]:
        return scan_public_release_archive(
            self.archive_path("fixture.zip", content),
            archive_format="zip",
            limits=limits,
        )

    def test_clean_deterministic_tar_and_zip_fixtures_pass(self) -> None:
        entries = [
            ("contextlattice/", b""),
            ("contextlattice/runtime.txt", b"bounded public runtime\n"),
            (
                "contextlattice/digest.txt",
                hashlib.sha256(b"bounded public runtime").hexdigest().encode("ascii"),
            ),
        ]
        tar_entries = [(name, None if name.endswith("/") else content, None) for name, content in entries]
        tar_report = self.scan_tar(_tar_gz_bytes(tar_entries))
        zip_report = self.scan_zip(_zip_bytes(entries))
        for report in (tar_report, zip_report):
            self.assertTrue(report["ok"])
            self.assertEqual(report["finding_count"], 0)
            self.assertGreater(report["expanded_byte_count"], 0)

    def test_secret_and_private_path_inside_compressed_member_are_detected(self) -> None:
        token = b"Authorization: Bearer " + b"S" * 40
        windows_path = rb"C:\Users\operator\private-release"
        for archive_format in ("tar.gz", "zip"):
            with self.subTest(archive_format=archive_format):
                if archive_format == "tar.gz":
                    content = _tar_gz_bytes(
                        [
                            ("contextlattice/", None, None),
                            ("contextlattice/runtime.txt", token + b"\n" + windows_path, None),
                        ]
                    )
                else:
                    content = _zip_bytes(
                        [
                            ("contextlattice/", b""),
                            ("contextlattice/runtime.txt", token + b"\n" + windows_path),
                        ]
                    )

                def run() -> dict[str, object]:
                    if archive_format == "tar.gz":
                        return self.scan_tar(content)
                    return self.scan_zip(content)

                self.assert_sanitized_failure(run, "credential.authorization_header", token)

    def test_member_content_chunk_boundary_is_scanned(self) -> None:
        material = b"sk_" + b"live_" + b"K" * 32
        content = _tar_gz_bytes(
            [
                ("contextlattice/", None, None),
                ("contextlattice/runtime.txt", b"x" * 14 + b"\n" + material + b"\n", None),
            ]
        )
        limits = PackageScanLimits(chunk_size=16)
        self.assert_sanitized_failure(
            lambda: self.scan_tar(content, limits=limits),
            "token.stripe_live",
            material,
        )

    def test_member_findings_are_bounded_without_truncating_content_read(self) -> None:
        materials = [
            b"sk_" + b"live_" + f"{index:02d}".encode("ascii") + b"K" * 30
            for index in range(40)
        ]
        content = _tar_gz_bytes(
            [
                ("contextlattice/", None, None),
                ("contextlattice/runtime.txt", b"\n".join(materials) + b"\n", None),
            ]
        )
        error = self.assert_sanitized_failure(
            lambda: self.scan_tar(content),
            "token.stripe_live",
            materials[0],
        )
        self.assertEqual(error.report["finding_count"], 32)
        self.assertTrue(error.report["truncated"])

    def test_recursive_supported_archive_is_scanned(self) -> None:
        material = b"client_" + b"secret=" + b"Q" * 32
        embedded = _zip_bytes(
            [
                ("payload/", b""),
                ("payload/config.txt", material),
            ]
        )
        outer = _tar_gz_bytes(
            [
                ("contextlattice/", None, None),
                ("contextlattice/embedded.zip", embedded, None),
            ]
        )
        self.assert_sanitized_failure(
            lambda: self.scan_tar(outer),
            "credential.named_value",
            material,
        )

    def test_renamed_and_unsupported_embedded_archives_fail_closed(self) -> None:
        embedded = _zip_bytes([("payload.txt", b"safe\n")])
        renamed = _tar_gz_bytes(
            [
                ("contextlattice/", None, None),
                ("contextlattice/embedded.bin", embedded, None),
            ]
        )
        self.assert_sanitized_failure(
            lambda: self.scan_tar(renamed),
            "archive.format_mismatch",
        )
        unsupported = _tar_gz_bytes(
            [
                ("contextlattice/", None, None),
                ("contextlattice/embedded.7z", b"not-an-archive", None),
            ]
        )
        self.assert_sanitized_failure(
            lambda: self.scan_tar(unsupported),
            "archive.unsupported_format",
        )

    def test_unsafe_duplicate_case_collision_and_special_tar_entries_fail(self) -> None:
        fixtures = (
            (
                "archive.path_unsafe",
                [("contextlattice/", None, None), ("../private.txt", b"safe", None)],
            ),
            (
                "archive.path_duplicate",
                [
                    ("contextlattice/", None, None),
                    ("contextlattice/value.txt", b"one", None),
                    ("contextlattice/value.txt", b"two", None),
                ],
            ),
            (
                "archive.path_case_collision",
                [
                    ("contextlattice/", None, None),
                    ("contextlattice/Value.txt", b"one", None),
                    ("contextlattice/value.txt", b"two", None),
                ],
            ),
            (
                "archive.path_type_collision",
                [
                    ("contextlattice/", None, None),
                    ("contextlattice/conflict", b"one", None),
                    ("contextlattice/conflict/value.txt", b"two", None),
                ],
            ),
            (
                "archive.entry_special",
                [("contextlattice/", None, None), ("contextlattice/link", None, tarfile.SYMTYPE)],
            ),
            (
                "archive.entry_special",
                [("contextlattice/", None, None), ("contextlattice/device", None, tarfile.CHRTYPE)],
            ),
        )
        for expected_code, entries in fixtures:
            with self.subTest(code=expected_code):
                content = _tar_gz_bytes(entries)
                self.assert_sanitized_failure(lambda: self.scan_tar(content), expected_code)

    def test_tree_scan_rejects_symlink_and_hardlink_members(self) -> None:
        hardlink_root = self.root / "hardlink-tree"
        hardlink_root.mkdir()
        original = hardlink_root / "original.txt"
        original.write_bytes(b"safe\n")
        os.link(original, hardlink_root / "alias.txt")
        self.assert_sanitized_failure(
            lambda: scan_public_release_tree(hardlink_root, asset="hardlink-tree"),
            "archive.entry_hardlink",
        )

        symlink_root = self.root / "symlink-tree"
        symlink_root.mkdir()
        (symlink_root / "target.txt").write_bytes(b"safe\n")
        (symlink_root / "link.txt").symlink_to("target.txt")
        self.assert_sanitized_failure(
            lambda: scan_public_release_tree(symlink_root, asset="symlink-tree"),
            "archive.entry_link",
        )

    def test_member_and_compression_bounds_fail_before_unbounded_read(self) -> None:
        content = _zip_bytes(
            [
                ("contextlattice/", b""),
                ("contextlattice/repeated.txt", b"A" * 4096),
            ]
        )
        member_limits = PackageScanLimits(max_member_bytes=64)
        self.assert_sanitized_failure(
            lambda: self.scan_zip(content, limits=member_limits),
            "archive.limit_member_bytes",
        )
        ratio_limits = PackageScanLimits(max_compression_ratio=2.0)
        self.assert_sanitized_failure(
            lambda: self.scan_zip(content, limits=ratio_limits),
            "archive.limit_compression_ratio",
        )

    def test_malformed_trailing_and_concatenated_archives_fail(self) -> None:
        valid_tar = _tar_gz_bytes(
            [("contextlattice/", None, None), ("contextlattice/value.txt", b"safe", None)]
        )
        valid_zip = _zip_bytes([("contextlattice/", b""), ("contextlattice/value.txt", b"safe")])
        fixtures = (
            ("tar.gz", valid_tar[:-8], "archive.malformed"),
            ("tar.gz", valid_tar + b"trailing", "archive.trailing_data"),
            ("tar.gz", valid_tar + valid_tar, "archive.trailing_data"),
            ("zip", valid_zip[:-4], "archive.malformed"),
            ("zip", valid_zip + b"trailing", "archive.structure_unknown"),
        )
        for archive_format, content, expected_code in fixtures:
            with self.subTest(archive_format=archive_format, expected_code=expected_code):
                run = (
                    (lambda: self.scan_tar(content))
                    if archive_format == "tar.gz"
                    else (lambda: self.scan_zip(content))
                )
                self.assert_sanitized_failure(run, expected_code)

    def test_tar_archive_binds_canonical_zero_padding_length(self) -> None:
        valid = _tar_gz_bytes(
            [("contextlattice/", None, None), ("contextlattice/value.txt", b"safe", None)]
        )
        raw = gzip.decompress(valid)
        for suffix in (b"\0" * 512, b"\0" * 1024, b"x"):
            with self.subTest(suffix_length=len(suffix)):
                tampered = gzip.compress(raw + suffix, compresslevel=9, mtime=0)
                self.assert_sanitized_failure(
                    lambda: self.scan_tar(tampered),
                    "archive.trailing_data",
                )

    def test_encrypted_and_unsupported_zip_entries_fail(self) -> None:
        base = bytearray(_zip_bytes([("contextlattice/value.txt", b"safe")]))
        central = base.index(b"PK\x01\x02")

        encrypted = bytearray(base)
        local_flags = struct.unpack_from("<H", encrypted, 6)[0] | 0x1
        central_flags = struct.unpack_from("<H", encrypted, central + 8)[0] | 0x1
        struct.pack_into("<H", encrypted, 6, local_flags)
        struct.pack_into("<H", encrypted, central + 8, central_flags)
        self.assert_sanitized_failure(
            lambda: self.scan_zip(bytes(encrypted)),
            "archive.entry_encrypted",
        )

        unsupported = bytearray(base)
        struct.pack_into("<H", unsupported, 8, 99)
        struct.pack_into("<H", unsupported, central + 10, 99)
        self.assert_sanitized_failure(
            lambda: self.scan_zip(bytes(unsupported)),
            "archive.compression_unsupported",
        )

    def _write_payload_tree(
        self,
        stage: Path,
        *,
        kind: str,
        secret: bytes | None = None,
        mutation: str = "",
    ) -> None:
        metadata_bytes = self.payload_metadata()
        source_files = {
            ".env.example": b"# STORAGE_ROOT=/Volumes/ExternalSSD/contextlattice\n",
            "Makefile": b"all:\n\t@true\n",
            "docker-compose.lite.yml": b"services: {}\n",
            "services/gateway-go/runtime.txt": (
                b"bounded public runtime\n" if secret is None else secret
            ),
            ".contextlattice-release.json": metadata_bytes,
        }
        if mutation == "safe-change":
            source_files["services/gateway-go/runtime.txt"] = b"changed but not secret material\n"
        elif mutation == "extra":
            source_files["injected.txt"] = b"safe but not source-bound\n"
        elif mutation == "missing":
            del source_files["Makefile"]
        directories = ("contextlattice/", "contextlattice/services/", "contextlattice/services/gateway-go/")
        if kind == "windows":
            archive_name = "contextlattice-payload.zip"
            archive_bytes = _zip_bytes(
                [(name, b"") for name in directories]
                + [(f"contextlattice/{name}", content) for name, content in source_files.items()]
            )
            metadata_path = stage / "payload/contextlattice-release.json"
            payload_path = stage / f"payload/{archive_name}"
        else:
            archive_name = "contextlattice-payload.tar.gz"
            archive_bytes = _tar_gz_bytes(
                [(name, None, None) for name in directories]
                + [(f"contextlattice/{name}", content, None) for name, content in source_files.items()]
            )
            if kind == "macos":
                metadata_path = stage / "contextlattice-release.json"
                payload_path = (
                    stage
                    / "ContextLattice.app/Contents/Resources/payload"
                    / archive_name
                )
            else:
                metadata_path = stage / "payload/contextlattice-release.json"
                payload_path = stage / f"payload/{archive_name}"
        payload_path.parent.mkdir(parents=True, exist_ok=True)
        payload_path.write_bytes(archive_bytes)
        payload_path.with_name(payload_path.name + ".sha256").write_text(
            f"{hashlib.sha256(archive_bytes).hexdigest()}  {archive_name}\n",
            encoding="ascii",
        )
        metadata_path.parent.mkdir(parents=True, exist_ok=True)
        metadata_path.write_bytes(metadata_bytes)
        if kind == "macos":
            embedded_metadata = payload_path.parent / "contextlattice-release.json"
            embedded_metadata.write_bytes(metadata_path.read_bytes())

    def _linux_artifact(self, *, secret: bytes | None = None, mutation: str = "") -> Path:
        stage = self.root / "linux-stage"
        OUTER.stage(self.source_repo(), stage, "linux", "public", TAG)
        self._write_payload_tree(stage, kind="linux", secret=secret, mutation=mutation)
        artifact = self.root / AUDIT.EXPECTED_INSTALLER_NAMES["linux"]
        OUTER.build_linux_archive(stage, artifact)
        return artifact

    def test_linux_canonical_outer_scans_embedded_payload(self) -> None:
        clean = self._linux_artifact()
        clean_report = AUDIT.audit_linux(clean, source_root=self.source_repo(), tag=TAG)
        self.assertTrue(clean_report["ok"])

        shutil.rmtree(self.root / "linux-stage")
        material = b"Authorization: Bearer " + b"L" * 40
        unsafe = self._linux_artifact(secret=material)
        self.assert_sanitized_failure(
            lambda: AUDIT.audit_linux(unsafe, source_root=self.source_repo(), tag=TAG),
            "credential.authorization_header",
            material,
        )

    def test_payload_contract_rejects_changed_extra_and_missing_safe_members(self) -> None:
        cases = (
            ("safe-change", "archive.contract_content_mismatch"),
            ("extra", "archive.contract_unexpected_member"),
            ("missing", "archive.contract_missing_member"),
        )
        for mutation, expected_code in cases:
            with self.subTest(mutation=mutation):
                stage = self.root / "linux-stage"
                if stage.exists():
                    shutil.rmtree(stage)
                artifact = self._linux_artifact(mutation=mutation)
                self.assert_sanitized_failure(
                    lambda: AUDIT.audit_linux(
                        artifact,
                        source_root=self.source_repo(),
                        tag=TAG,
                    ),
                    expected_code,
                )

    def _windows_stage(self, *, secret: bytes | None = None) -> Path:
        stage = self.root / "windows-template"
        OUTER.stage(self.source_repo(), stage, "windows", "public", TAG)
        self._write_payload_tree(stage, kind="windows", secret=secret)
        return stage

    def _fake_msitools(self, stage: Path) -> tuple[Path, Path]:
        msiinfo = self.root / "msiinfo"
        directory_table = (
            "Directory\tDirectory_Parent\tDefaultDir\n"
            "s72\ts72\tl255\n"
            "Directory\tDirectory_Parent\tDefaultDir\n"
            "TARGETDIR\t\tSourceDir\n"
            "ProgramFilesFolder\tTARGETDIR\tPFiles\n"
            "INSTALLDIR\tProgramFilesFolder\tContextLattice\n"
            "PAYLOADDIR\tINSTALLDIR\tpayload\n"
        )
        components = (
            ("cmpInstallCmd", "filInstallCmd", "ContextLattice-Install.cmd", "ContextLattice-Install.cmd"),
            ("cmpMonitorCmd", "filMonitorCmd", "ContextLattice-Monitor.cmd", "ContextLattice-Monitor.cmd"),
            ("cmpInstallPs1", "filInstallPs1", "Install-ContextLattice.ps1", "Install-ContextLattice.ps1"),
            ("cmpMonitorPs1", "filMonitorPs1", "Monitor-ContextLattice.ps1", "Monitor-ContextLattice.ps1"),
            ("cmpReadme", "filReadme", "README.txt", "README.txt"),
            ("cmpPayloadZip", "filPayloadZip", "contextlattice-payload.zip", "payload/contextlattice-payload.zip"),
            ("cmpPayloadChecksum", "filPayloadChecksum", "contextlattice-payload.zip.sha256", "payload/contextlattice-payload.zip.sha256"),
            ("cmpPayloadMetadata", "filPayloadMetadata", "contextlattice-release.json", "payload/contextlattice-release.json"),
        )
        component_table = (
            "Component\tComponentId\tDirectory_\tAttributes\tCondition\tKeyPath\n"
            "s72\tS38\ts72\ti2\tS255\tS72\n"
            "Component\tComponentId\tDirectory_\tAttributes\tCondition\tKeyPath\n"
            + "".join(
                f"{component}\tguid-{index}\t{'PAYLOADDIR' if source.startswith('payload/') else 'INSTALLDIR'}\t0\t\t{file_id}\n"
                for index, (component, file_id, _name, source) in enumerate(components)
            )
        )
        file_table = (
            "File\tComponent_\tFileName\tFileSize\n"
            "s72\ts72\tl255\ti4\n"
            "File\tComponent_\tFileName\tFileSize\n"
            + "".join(
                f"{file_id}\t{component}\t{name}\t{(stage / source).stat().st_size}\n"
                for component, file_id, name, source in components
            )
        )
        msiinfo.write_text(
            "#!/bin/sh\n"
            "if [ \"$1\" = tables ]; then printf '%s\\n' Directory Component File; exit 0; fi\n"
            "if [ \"$1\" = export ] && [ \"$3\" = Directory ]; then\n"
            "cat <<'EOF'\n"
            f"{directory_table}"
            "EOF\n"
            "exit 0\n"
            "fi\n"
            "if [ \"$1\" = export ] && [ \"$3\" = Component ]; then\n"
            "cat <<'EOF'\n"
            f"{component_table}"
            "EOF\n"
            "exit 0\n"
            "fi\n"
            "if [ \"$1\" = export ] && [ \"$3\" = File ]; then\n"
            "cat <<'EOF'\n"
            f"{file_table}"
            "EOF\n"
            "exit 0\n"
            "fi\n"
            "exit 1\n",
            encoding="utf-8",
        )
        msiinfo.chmod(0o755)
        msiextract = self.root / "msiextract"
        msiextract.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            "[ \"$1\" = -C ]\n"
            f"mkdir -p \"$2/Program Files/ContextLattice\"\n"
            f"cp -R {json.dumps(str(stage))}/. \"$2/Program Files/ContextLattice/\"\n",
            encoding="utf-8",
        )
        msiextract.chmod(0o755)
        return msiinfo, msiextract

    def _deterministic_msi_fixture(self) -> Path:
        artifact = self.root / AUDIT.EXPECTED_INSTALLER_NAMES["windows"]
        payload = b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" + b"".join(
            hashlib.sha256(f"msi-block-{index}".encode("ascii")).digest()
            for index in range(64)
        )
        artifact.write_bytes(payload)
        return artifact

    def test_windows_native_tool_contract_scans_extracted_payload(self) -> None:
        stage = self._windows_stage()
        msiinfo, msiextract = self._fake_msitools(stage)
        artifact = self._deterministic_msi_fixture()
        report = AUDIT.audit_windows(
            artifact,
            source_root=self.source_repo(),
            tag=TAG,
            temp_root=self.root,
            msiinfo=str(msiinfo),
            msiextract=str(msiextract),
        )
        self.assertTrue(report["ok"])

        shutil.rmtree(stage)
        material = b"sk_" + b"live_" + b"W" * 32
        stage = self._windows_stage(secret=material)
        _, msiextract = self._fake_msitools(stage)
        self.assert_sanitized_failure(
            lambda: AUDIT.audit_windows(
                artifact,
                source_root=self.source_repo(),
                tag=TAG,
                temp_root=self.root,
                msiinfo=str(msiinfo),
                msiextract=str(msiextract),
            ),
            "token.stripe_live",
            material,
        )

    def test_msi_directory_component_file_closure_rejects_foreign_rows(self) -> None:
        stage = self._windows_stage()
        valid_msiinfo, msiextract = self._fake_msitools(stage)
        artifact = self._deterministic_msi_fixture()
        exports: dict[str, bytes] = {}
        for table in ("Directory", "Component", "File"):
            result = subprocess.run(
                [str(valid_msiinfo), "export", str(artifact), table],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=True,
            )
            exports[table] = result.stdout
        self.assertEqual(
            OUTER.validate_msi_table_closure(
                self.source_repo(), exports["Directory"], exports["Component"], exports["File"]
            ),
            {"directories": 4, "components": 8, "files": 8},
        )
        invalid_tables = (
            exports["Directory"].replace(
                b"INSTALLDIR\tProgramFilesFolder\tContextLattice",
                b"INSTALLDIR\tTARGETDIR\tContextLattice",
            ),
            exports["Component"].replace(
                b"cmpInstallCmd\tguid-0\tINSTALLDIR\t",
                b"cmpInstallCmd\tguid-0\tFOREIGNDIR\t",
            ),
            exports["Component"].replace(
                b"cmpInstallCmd\tguid-0\tINSTALLDIR\t0\t\tfilInstallCmd",
                b"cmpInstallCmd\tguid-0\tINSTALLDIR\t0\t\tFOREIGNFILE",
            ),
            exports["File"].replace(
                b"filInstallCmd\tcmpInstallCmd\t",
                b"filInstallCmd\tFOREIGNCOMP\t",
            ),
            exports["Component"] + b"cmpInstallCmd\tguid-duplicate\tINSTALLDIR\t0\t\tfilInstallCmd\n",
        )
        for invalid in invalid_tables:
            with self.subTest(table=invalid[:32]):
                with self.assertRaises(OUTER.OuterContractError):
                    OUTER.validate_msi_table_closure(
                        self.source_repo(),
                        invalid if invalid is invalid_tables[0] else exports["Directory"],
                        invalid
                        if invalid is invalid_tables[1]
                        or invalid is invalid_tables[2]
                        or invalid is invalid_tables[4]
                        else exports["Component"],
                        invalid if invalid is invalid_tables[3] else exports["File"],
                    )

        for label, replacement in (
            (
                "directory",
                (
                    b"INSTALLDIR\tProgramFilesFolder\tContextLattice",
                    b"INSTALLDIR\tTARGETDIR\tContextLattice",
                ),
            ),
            (
                "component",
                (b"cmpInstallCmd\tguid-0\tINSTALLDIR\t", b"cmpInstallCmd\tguid-0\tFOREIGNDIR\t"),
            ),
            (
                "file",
                (b"filInstallCmd\tcmpInstallCmd\t", b"filInstallCmd\tFOREIGNCOMP\t"),
            ),
        ):
            altered = self.root / f"msiinfo-{label}"
            altered.write_text(
                "#!/bin/sh\n"
                "set -eu\n"
                f"if [ \"$1\" = export ] && [ \"$3\" = {label.title()} ]; then\n"
                f"  {json.dumps(str(valid_msiinfo))} \"$@\" | sed 's/{replacement[0].decode()}/{replacement[1].decode()}/'\n"
                "  exit 0\n"
                "fi\n"
                f"exec {json.dumps(str(valid_msiinfo))} \"$@\"\n",
                encoding="utf-8",
            )
            altered.chmod(0o755)
            with self.subTest(auditor=label):
                with mock.patch.object(AUDIT.sys, "platform", "darwin"):
                    self.assert_sanitized_failure(
                        lambda: AUDIT.audit_windows(
                            artifact,
                            source_root=self.source_repo(),
                            tag=TAG,
                            temp_root=self.root,
                            msiinfo=str(altered),
                            msiextract=str(msiextract),
                        ),
                        "package.contract_invalid",
                    )

    def test_msi_file_name_override_controls_destination_and_rejects_unsafe_collisions(self) -> None:
        stage = self._windows_stage()
        valid_msiinfo, _ = self._fake_msitools(stage)
        artifact = self._deterministic_msi_fixture()
        exports: dict[str, bytes] = {}
        for table in ("Directory", "Component", "File"):
            result = subprocess.run(
                [str(valid_msiinfo), "export", str(artifact), table],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=True,
            )
            exports[table] = result.stdout

        source = self.source_repo()
        wxs_path = source / "packaging/windows/contextlattice.wxs"
        original_wxs = wxs_path.read_text(encoding="utf-8")
        needle = 'File Id="filInstallCmd" Source="ContextLattice-Install.cmd"'
        try:
            wxs_path.write_text(
                original_wxs.replace(needle, 'File Id="filInstallCmd" Name="renamed.cmd" Source="ContextLattice-Install.cmd"'),
                encoding="utf-8",
            )
            renamed_files = exports["File"].replace(
                b"filInstallCmd\tcmpInstallCmd\tContextLattice-Install.cmd\t",
                b"filInstallCmd\tcmpInstallCmd\trenamed.cmd\t",
            )
            self.assertEqual(
                OUTER.validate_msi_table_closure(
                    source, exports["Directory"], exports["Component"], renamed_files
                ),
                {"directories": 4, "components": 8, "files": 8},
            )

            for override in (
                "../escape.cmd",
                "payload/escape.cmd",
                r"payload\escape.cmd",
                r"C:\escape.cmd",
                "ContextLattice-Monitor.cmd",
                "contextlattice-monitor.cmd",
            ):
                with self.subTest(override=override):
                    wxs_path.write_text(
                        original_wxs.replace(
                            needle,
                            f'File Id="filInstallCmd" Name="{override}" Source="ContextLattice-Install.cmd"',
                        ),
                        encoding="utf-8",
                    )
                    with self.assertRaises(OUTER.OuterContractError):
                        OUTER.validate_msi_table_closure(
                            source, exports["Directory"], exports["Component"], exports["File"]
                        )

            wxs_path.write_text(
                original_wxs.replace(
                    needle,
                    'File Id="filInstallCmd" Name="café.cmd" Source="ContextLattice-Install.cmd"',
                ).replace(
                    'File Id="filMonitorCmd" Source="ContextLattice-Monitor.cmd"',
                    'File Id="filMonitorCmd" Name="cafe\u0301.cmd" Source="ContextLattice-Monitor.cmd"',
                ),
                encoding="utf-8",
            )
            with self.assertRaises(OUTER.OuterContractError):
                OUTER.validate_msi_table_closure(
                    source, exports["Directory"], exports["Component"], exports["File"]
                )
        finally:
            wxs_path.write_text(original_wxs, encoding="utf-8")

    def test_msi_extractor_escape_is_detected_outside_containment_root(self) -> None:
        stage = self._windows_stage()
        msiinfo, _ = self._fake_msitools(stage)
        artifact = self._deterministic_msi_fixture()
        escape = self.root.parent / "public-msi-escape-sentinel"
        msiextract = self.root / "malicious-msiextract"
        msiextract.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            f"printf 'escape' > {json.dumps(str(escape))}\n"
            "exit 0\n",
            encoding="utf-8",
        )
        msiextract.chmod(0o755)
        try:
            with mock.patch.object(AUDIT.sys, "platform", "darwin"):
                self.assert_sanitized_failure(
                    lambda: AUDIT.audit_windows(
                        artifact,
                        source_root=self.source_repo(),
                        tag=TAG,
                        temp_root=self.root,
                        msiinfo=str(msiinfo),
                        msiextract=str(msiextract),
                    ),
                    "package.msi_extraction_escape",
                )
        finally:
            escape.unlink(missing_ok=True)

    def test_msiinfo_escape_is_detected_before_extraction(self) -> None:
        stage = self._windows_stage()
        msiinfo, msiextract = self._fake_msitools(stage)
        valid_msiinfo = self.root / "valid-msiinfo"
        shutil.copy2(msiinfo, valid_msiinfo)
        escape = self.root.parent / "public-msiinfo-escape-sentinel"
        msiinfo.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            f"printf 'escape' > {json.dumps(str(escape))}\n"
            f"exec {json.dumps(str(valid_msiinfo))} \"$@\"\n",
            encoding="utf-8",
        )
        msiinfo.chmod(0o755)
        artifact = self._deterministic_msi_fixture()
        try:
            with mock.patch.object(AUDIT.sys, "platform", "darwin"):
                self.assert_sanitized_failure(
                    lambda: AUDIT.audit_windows(
                        artifact,
                        source_root=self.source_repo(),
                        tag=TAG,
                        temp_root=self.root,
                        msiinfo=str(msiinfo),
                        msiextract=str(msiextract),
                    ),
                    "package.msi_parser_escape",
                )
        finally:
            escape.unlink(missing_ok=True)

    def test_msi_extraction_rejects_unexpected_root_path(self) -> None:
        stage = self._windows_stage()
        msiinfo, _ = self._fake_msitools(stage)
        artifact = self._deterministic_msi_fixture()
        extra = self.root / "extra-root-msiextract"
        extra.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            f"mkdir -p \"$2/Program Files/ContextLattice\"\n"
            f"cp -R {json.dumps(str(stage))}/. \"$2/Program Files/ContextLattice/\"\n"
            "printf 'unexpected' > \"$2/extra-root.txt\"\n",
            encoding="utf-8",
        )
        extra.chmod(0o755)
        try:
            with mock.patch.object(AUDIT.sys, "platform", "darwin"):
                self.assert_sanitized_failure(
                    lambda: AUDIT.audit_windows(
                        artifact,
                        source_root=self.source_repo(),
                        tag=TAG,
                        temp_root=self.root,
                        msiinfo=str(msiinfo),
                        msiextract=str(extra),
                    ),
                    "package.msi_unexpected_path",
                )
        finally:
            extra.unlink(missing_ok=True)

    def test_msi_parser_injected_harness_has_no_secret_environment_and_detects_file_escape(self) -> None:
        stage = self._windows_stage()
        msiinfo, msiextract = self._fake_msitools(stage)
        valid_msiinfo = self.root / "valid-msiinfo"
        shutil.copy2(msiinfo, valid_msiinfo)
        host_secret = self.root.parent / "public-msi-host-secret"
        host_secret.write_text("host-only", encoding="utf-8")
        escape = self.root.parent / "public-msiinfo-host-escape"
        msiinfo.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            f"if [ -n \"${{RELEASE_INTEGRITY_SECRET:-}}\" ]; then exit 42; fi\n"
            f"if [ -e /proc/1/environ ] || [ -e {json.dumps(str(host_secret))} ]; then\n"
            f"  printf 'escape' > {json.dumps(str(escape))}\n"
            "fi\n"
            f"exec {json.dumps(str(valid_msiinfo))} \"$@\"\n",
            encoding="utf-8",
        )
        msiinfo.chmod(0o755)
        artifact = self._deterministic_msi_fixture()
        try:
            with mock.patch.dict(os.environ, {"RELEASE_INTEGRITY_SECRET": "do-not-pass"}, clear=False):
                with mock.patch.object(AUDIT.sys, "platform", "darwin"):
                    self.assert_sanitized_failure(
                        lambda: AUDIT.audit_windows(
                            artifact,
                            source_root=self.source_repo(),
                            tag=TAG,
                            temp_root=self.root,
                            msiinfo=str(msiinfo),
                            msiextract=str(msiextract),
                        ),
                        "package.msi_parser_escape",
                    )
        finally:
            host_secret.unlink(missing_ok=True)
            escape.unlink(missing_ok=True)

    def test_windows_and_macos_verifiers_fail_closed_when_unavailable(self) -> None:
        windows = self._deterministic_msi_fixture()
        self.assert_sanitized_failure(
            lambda: AUDIT.audit_windows(
                windows,
                source_root=self.source_repo(),
                tag=TAG,
                temp_root=self.root,
                msiinfo="missing-msiinfo",
                msiextract="missing-msiextract",
            ),
            "package.verifier_unavailable",
        )
        dmg = self.root / AUDIT.EXPECTED_INSTALLER_NAMES["macos"]
        dmg.write_bytes(b"koly" + b"\x00" * 1024)
        with mock.patch.object(AUDIT.sys, "platform", "linux"):
            self.assert_sanitized_failure(
                lambda: AUDIT.audit_macos(
                    dmg,
                    source_root=self.source_repo(),
                    tag=TAG,
                    temp_root=self.root,
                ),
                "package.verifier_unavailable",
            )

    def test_linux_msi_boundary_has_no_host_root_or_inherited_environment(self) -> None:
        command = AUDIT._confined_bwrap_command(
            bwrap_path="/usr/bin/bwrap",
            tool_path="/usr/bin/msiinfo",
            artifact=Path("/input-artifact.msi"),
            output_root=Path("/output-root"),
            tool_arguments=["tables", "/input/contextlattice.msi"],
        )
        self.assertIn("--unshare-all", command)
        self.assertIn("--clearenv", command)
        self.assertIn("--proc", command)
        self.assertNotIn("--unshare-net", command)
        self.assertNotIn(
            ["--ro-bind", "/", "/"],
            [command[index : index + 3] for index in range(len(command) - 2)],
        )

    @unittest.skipUnless(sys.platform == "darwin" and Path("/usr/bin/hdiutil").is_file(), "requires hdiutil")
    def test_native_dmg_clean_and_embedded_secret_proof(self) -> None:
        def build_dmg(name: str, material: bytes | None) -> Path:
            stage = self.root / f"{name}-stage"
            OUTER.stage(self.source_repo(), stage, "macos", "public", TAG)
            self._write_payload_tree(stage, kind="macos", secret=material)
            artifact = self.root / name / AUDIT.EXPECTED_INSTALLER_NAMES["macos"]
            artifact.parent.mkdir()
            result = subprocess.run(
                [
                    "/usr/bin/hdiutil",
                    "create",
                    "-volname",
                    "ContextLattice",
                    "-srcfolder",
                    str(stage),
                    "-format",
                    "UDZO",
                    str(artifact),
                ],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr.decode("utf-8", errors="replace"))
            return artifact

        clean = build_dmg("clean", None)
        report = AUDIT.audit_macos(
            clean,
            source_root=self.source_repo(),
            tag=TAG,
            temp_root=self.root,
        )
        self.assertTrue(report["ok"])

        material = b"client_" + b"secret=" + b"D" * 32
        unsafe = build_dmg("unsafe", material)
        self.assert_sanitized_failure(
            lambda: AUDIT.audit_macos(
                unsafe,
                source_root=self.source_repo(),
                tag=TAG,
                temp_root=self.root,
            ),
            "credential.named_value",
            material,
        )

    @unittest.skipUnless(sys.platform == "darwin" and Path("/usr/bin/hdiutil").is_file(), "requires hdiutil")
    def test_hdiutil_identity_command_is_supported_and_bound(self) -> None:
        result = subprocess.run(
            ["/usr/bin/hdiutil", "help"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            check=False,
        )
        self.assertEqual(result.returncode, 0)
        self.assertIn(b"Usage:", result.stdout)
        identity = AUDIT._tool_identity(
            "/usr/bin/hdiutil",
            label="hdiutil",
            version_args=("help",),
        )
        self.assertEqual(identity["id"], "hdiutil")
        self.assertRegex(identity["sha256"], r"^[0-9a-f]{64}$")
        self.assertTrue(identity["version"].startswith("Usage:"))

    def test_receipt_is_closed_and_binds_exact_artifact_bytes(self) -> None:
        artifact = self._linux_artifact()
        source = self.source_repo()
        scan = AUDIT.audit_linux(artifact, source_root=source, tag=TAG)
        receipt = AUDIT.build_receipt(
            artifact,
            kind="linux",
            tag=TAG,
            scan=scan,
            source_root=source,
        )
        receipt_path = self.root / "receipt.json"
        AUDIT.write_receipt(receipt_path, receipt)
        verified = AUDIT.verify_receipt(
            receipt_path,
            artifact,
            kind="linux",
            tag=TAG,
            source_root=source,
            temp_root=self.root,
        )
        self.assertEqual(verified, receipt)
        with self.assertRaises(ValueError):
            AUDIT.write_receipt(receipt_path, receipt)

        tampered = json.loads(receipt_path.read_text(encoding="utf-8"))
        tampered["scan"]["unknown"] = "not allowed"
        receipt_path.write_text(json.dumps(tampered) + "\n", encoding="utf-8")
        self.assert_sanitized_failure(
            lambda: AUDIT.verify_receipt(
                receipt_path,
                artifact,
                kind="linux",
                tag=TAG,
                source_root=source,
                temp_root=self.root,
            ),
            "package.receipt_invalid",
        )
        receipt_path.write_text(json.dumps(receipt, sort_keys=True) + "\n", encoding="utf-8")

        noncanonical_tools = json.loads(receipt_path.read_text(encoding="utf-8"))
        noncanonical_tools["tools"]["package_audit"]["sha256"] = "A" * 64
        receipt_path.write_text(json.dumps(noncanonical_tools) + "\n", encoding="utf-8")
        self.assert_sanitized_failure(
            lambda: AUDIT.verify_receipt(
                receipt_path,
                artifact,
                kind="linux",
                tag=TAG,
                source_root=source,
                temp_root=self.root,
            ),
            "package.receipt_invalid",
        )
        receipt_path.write_text(json.dumps(receipt, sort_keys=True) + "\n", encoding="utf-8")

        artifact.write_bytes(artifact.read_bytes() + b"changed")
        self.assert_sanitized_failure(
            lambda: AUDIT.verify_receipt(
                receipt_path,
                artifact,
                kind="linux",
                tag=TAG,
                source_root=source,
                temp_root=self.root,
            ),
            "package.receipt_invalid",
        )

    def test_portable_receipt_binding_never_invokes_native_package_tools(self) -> None:
        linux_artifact = self._linux_artifact()
        source = self.source_repo()
        scan = AUDIT.audit_linux(linux_artifact, source_root=source, tag=TAG)
        base_receipt = AUDIT.build_receipt(
            linux_artifact,
            kind="linux",
            tag=TAG,
            scan=scan,
            source_root=source,
        )
        native_tool_names = {
            "macos": ("hdiutil",),
            "windows": ("msiinfo", "msiextract", "containment"),
        }
        with mock.patch.object(
            AUDIT,
            "_native_tool_identities",
            side_effect=AssertionError("portable receipt verification invoked a native tool"),
        ):
            for kind, tool_names in native_tool_names.items():
                with self.subTest(kind=kind):
                    artifact = self.root / AUDIT.EXPECTED_INSTALLER_NAMES[kind]
                    artifact.write_bytes(linux_artifact.read_bytes())
                    binding = AUDIT._artifact_binding(
                        artifact,
                        artifact_name=AUDIT.EXPECTED_INSTALLER_NAMES[kind],
                    )
                    receipt = json.loads(json.dumps(base_receipt))
                    receipt["kind"] = kind
                    receipt["artifact"] = {
                        "name": AUDIT.EXPECTED_INSTALLER_NAMES[kind],
                        "kind": kind,
                        "size_bytes": binding["size_bytes"],
                        "sha256": binding["sha256"],
                    }
                    for name in tool_names:
                        receipt["tools"][name] = {
                            "id": name,
                            "version": "native-stage-attested",
                            "sha256": "a" * 64,
                        }
                    receipt_path = self.root / f"{kind}-binding-receipt.json"
                    receipt_path.write_text(
                        json.dumps(receipt, sort_keys=True) + "\n",
                        encoding="utf-8",
                    )
                    self.assertEqual(
                        AUDIT.verify_receipt_binding(
                            receipt_path,
                            artifact,
                            kind=kind,
                            tag=TAG,
                            source_root=source,
                        ),
                        receipt,
                    )

                    tampered = json.loads(json.dumps(receipt))
                    tampered["tools"][tool_names[0]]["sha256"] = "A" * 64
                    receipt_path.write_text(json.dumps(tampered) + "\n", encoding="utf-8")
                    self.assert_sanitized_failure(
                        lambda: AUDIT.verify_receipt_binding(
                            receipt_path,
                            artifact,
                            kind=kind,
                            tag=TAG,
                            source_root=source,
                        ),
                        "package.receipt_invalid",
                    )

    def test_dirty_source_and_wrong_release_commit_fail_closed(self) -> None:
        artifact = self._linux_artifact()
        source = self.source_repo()
        injected = source / "untracked-release-injection.txt"
        injected.write_text("injected\n", encoding="utf-8")
        try:
            self.assert_sanitized_failure(
                lambda: AUDIT.audit_linux(
                    artifact,
                    source_root=source,
                    tag=TAG,
                    temp_root=self.root,
                ),
                "package.payload_metadata_invalid",
            )
        finally:
            injected.unlink(missing_ok=True)

        head = subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD"],
            text=True,
        ).strip()
        with mock.patch.dict(
            os.environ,
            {"GITHUB_SHA": "0" * 40, "RELEASE_COMMIT": head},
        ):
            self.assertTrue(
                AUDIT.audit_linux(
                    artifact,
                    source_root=source,
                    tag=TAG,
                    temp_root=self.root,
                )["ok"]
            )

        with mock.patch.dict(os.environ, {"RELEASE_COMMIT": "0" * 40}):
            self.assert_sanitized_failure(
                lambda: AUDIT.audit_linux(
                    artifact,
                    source_root=source,
                    tag=TAG,
                    temp_root=self.root,
                ),
                "package.payload_metadata_invalid",
            )

    def test_workflow_gates_metadata_and_promotion_on_exact_native_audits(self) -> None:
        workflow = PUBLIC_WORKFLOW.read_text(encoding="utf-8")
        publish_start = workflow.index("  publish-assets:")
        remote_macos_start = workflow.index("  audit-remote-macos:")
        remote_linux_start = workflow.index("  audit-remote-linux-msi:")
        promotion_start = workflow.index("  promote-release:")
        publish = workflow[publish_start:remote_macos_start]
        remote_macos = workflow[remote_macos_start:remote_linux_start]
        remote_linux = workflow[remote_linux_start:promotion_start]
        promotion = workflow[promotion_start:]

        self.assertLess(
            workflow.index("Audit final DMG package contents"),
            workflow.index("Upload DMG build artifact"),
        )
        self.assertLess(
            workflow.index("Audit final Linux and MSI package contents"),
            workflow.index("Upload Linux build artifact"),
        )
        self.assertLess(
            publish.index("Verify exact build package audits before metadata generation"),
            publish.index("Generate final-byte checksums, manifest, and provenance"),
        )
        self.assertNotIn("gh release edit", publish)
        self.assertIn("gh release download", remote_macos)
        self.assertIn("--kind macos", remote_macos)
        self.assertIn("--receipt-out", remote_macos)
        self.assertIn("msitools_version='0.103-3build1'", remote_linux)
        self.assertIn("bubblewrap_version='0.9.0-1ubuntu0.1'", remote_linux)
        self.assertIn("--kind linux", remote_linux)
        self.assertIn("--kind windows", remote_linux)
        self.assertIn("--receipt-out", remote_linux)
        self.assertIn("- audit-remote-macos", promotion)
        self.assertIn("- audit-remote-linux-msi", promotion)
        self.assertLess(
            promotion.index("Download and audit the complete remote draft asset set"),
            promotion.index("Promote audited draft release"),
        )
        final_audit = promotion[: promotion.index("Promote audited draft release")]
        self.assertIn("--verify-receipt-binding-only", publish)
        self.assertIn("--verify-receipt-binding-only", final_audit)
        self.assertNotIn("--verify-receipt-only", publish)
        self.assertNotIn("--verify-receipt-only", final_audit)
        self.assertNotIn("--containment-tool bwrap", publish)
        self.assertNotIn("--containment-tool bwrap", final_audit)
        self.assertIn("scripts/agent/audit-public-release-assets", final_audit)
        promote_step = promotion[promotion.index("Promote audited draft release") :]
        self.assertIn("public-release-promotion-recheck", promote_step)
        self.assertLess(promote_step.index("cmp -s"), promote_step.index("gh release edit"))
        self.assertIn("--latest=false", promote_step)
        self.assertNotIn("--clobber", workflow)


if __name__ == "__main__":
    unittest.main()
