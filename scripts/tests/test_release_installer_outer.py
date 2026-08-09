from __future__ import annotations

import importlib.util
import binascii
import io
import json
import stat
import struct
import sys
import tarfile
import tempfile
import unittest
import zlib
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts/release_installer_outer.py"
SPEC = importlib.util.spec_from_file_location("release_installer_outer", MODULE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load outer installer contract module")
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)


class ReleaseInstallerOuterTests(unittest.TestCase):
    def write_linux_tar(self, stage: Path, tar_path: Path, members: list[Path]) -> None:
        with tarfile.open(tar_path, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for path in members:
                relative = path.relative_to(stage).as_posix()
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
                content = path.read_bytes()
                info.type = tarfile.REGTYPE
                info.mode = 0o755 if stat.S_IMODE(path.stat().st_mode) & 0o111 else 0o644
                info.size = len(content)
                archive.addfile(info, io.BytesIO(content))

    def add_dynamic_files(self, root: Path, kind: str, lane: str, tag: str) -> None:
        metadata = {
            "installer_outer_contract_schema_id": MODULE.SCHEMA_ID,
            "installer_outer_contract_sha256": MODULE.contract_sha256(ROOT, lane, tag),
        }
        for relative in MODULE._dynamic_paths(kind):
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            if path.name == "contextlattice-release.json":
                path.write_text(json.dumps(metadata), encoding="utf-8")
            else:
                path.write_bytes(b"bounded dynamic payload fixture\n")

    def staged_fixture(
        self, kind: str, lane: str = "paid"
    ) -> tuple[tempfile.TemporaryDirectory[str], Path]:
        tmp = tempfile.TemporaryDirectory(prefix=f"outer-{kind}-")
        stage = Path(tmp.name) / "stage"
        MODULE.stage(ROOT, stage, kind, lane, "v3.18.0-public-paid")
        self.add_dynamic_files(stage, kind, lane, "v3.18.0-public-paid")
        return tmp, stage

    def test_contract_is_deterministic_and_release_bound(self) -> None:
        first = MODULE.contract_payload(ROOT, "paid", "v3.18.0-public-paid")
        second = MODULE.contract_payload(ROOT, "paid", "v3.18.0-public-paid")
        self.assertEqual(first, second)
        self.assertEqual(first["schema_id"], MODULE.SCHEMA_ID)
        self.assertNotEqual(
            MODULE.contract_sha256(ROOT, "paid", "v3.18.0-public-paid"),
            MODULE.contract_sha256(ROOT, "public", "v3.18.0-public"),
        )
        self.assertEqual(
            first["artifacts"]["windows"]["package_control"]["allowed_msi_tables"],
            list(MODULE.ALLOWED_MSI_TABLES),
        )
        self.assertEqual(
            first["artifacts"]["windows"]["package_control"][
                "required_empty_msi_tables"
            ],
            list(MODULE.REQUIRED_EMPTY_MSI_TABLES),
        )

    def test_all_outer_installer_kinds_validate_exact_bytes(self) -> None:
        for kind in MODULE.KINDS:
            with self.subTest(kind=kind):
                tmp, stage = self.staged_fixture(kind)
                try:
                    result = MODULE.validate_tree(
                        ROOT, stage, kind, "paid", "v3.18.0-public-paid"
                    )
                    self.assertGreater(result["contracted_files"], 0)
                finally:
                    tmp.cleanup()

    def test_marker_preserving_wrapper_tamper_is_rejected(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        try:
            installer = stage / "ContextLattice-Install.sh"
            installer.write_bytes(installer.read_bytes() + b"\n# marker remains\n")
            with self.assertRaisesRegex(
                MODULE.OuterContractError,
                "file differs from reviewed source: ContextLattice-Install.sh",
            ):
                MODULE.validate_tree(
                    ROOT, stage, "linux", "paid", "v3.18.0-public-paid"
                )
        finally:
            tmp.cleanup()

    def test_extra_executable_or_control_file_is_rejected(self) -> None:
        for kind in MODULE.KINDS:
            with self.subTest(kind=kind):
                tmp, stage = self.staged_fixture(kind)
                try:
                    extra = stage / "unexpected-runner"
                    extra.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
                    extra.chmod(0o755)
                    with self.assertRaisesRegex(
                        MODULE.OuterContractError, "contains unbound files"
                    ):
                        MODULE.validate_tree(
                            ROOT, stage, kind, "paid", "v3.18.0-public-paid"
                        )
                finally:
                    tmp.cleanup()

    def test_wrapper_mode_tamper_is_rejected(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        try:
            installer = stage / "ContextLattice-Install.sh"
            installer.chmod(0o644)
            self.assertEqual(stat.S_IMODE(installer.stat().st_mode), 0o644)
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "mode differs from reviewed source"
            ):
                MODULE.validate_tree(
                    ROOT, stage, "linux", "paid", "v3.18.0-public-paid"
                )
        finally:
            tmp.cleanup()

    def test_linux_archive_binds_stored_wrapper_modes(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        try:
            valid = Path(tmp.name) / "valid.tar.gz"
            MODULE.build_linux_archive(stage, valid)
            result = MODULE.validate_linux_archive(
                ROOT, valid, "paid", "v3.18.0-public-paid"
            )
            self.assertEqual(result["contracted_files"], 3)

            wrong_mode = Path(tmp.name) / "wrong-mode.tar.gz"
            (stage / "ContextLattice-Install.sh").chmod(0o644)
            MODULE.build_linux_archive(stage, wrong_mode)
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "mode differs from reviewed source"
            ):
                MODULE.validate_linux_archive(
                    ROOT, wrong_mode, "paid", "v3.18.0-public-paid"
                )
        finally:
            tmp.cleanup()

    def test_linux_archive_is_deterministic_and_rejects_host_metadata(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        try:
            first = Path(tmp.name) / "first.tar.gz"
            second = Path(tmp.name) / "second.tar.gz"
            MODULE.build_linux_archive(stage, first)
            MODULE.build_linux_archive(stage, second)
            self.assertEqual(first.read_bytes(), second.read_bytes())

            host_owned = Path(tmp.name) / "host-owned.tar.gz"
            host_owned_tar = Path(tmp.name) / "host-owned.tar"
            with tarfile.open(
                host_owned_tar, mode="w", format=tarfile.USTAR_FORMAT
            ) as archive:

                def host_metadata(info: tarfile.TarInfo) -> tarfile.TarInfo:
                    info.uid = 501
                    info.gid = 20
                    info.uname = "local-user"
                    info.gname = "staff"
                    info.mtime = 0
                    info.pax_headers = {}
                    return info

                archive.add(
                    stage,
                    arcname="ContextLattice-linux-bootstrap",
                    filter=host_metadata,
                )
            MODULE._write_stored_gzip(host_owned_tar, host_owned)
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "non-canonical owner, time, or PAX"
            ):
                MODULE.validate_linux_archive(
                    ROOT, host_owned, "paid", "v3.18.0-public-paid"
                )

            extended = Path(tmp.name) / "extended-metadata.tar.gz"
            extended_tar = Path(tmp.name) / "extended-metadata.tar"
            with tarfile.open(
                extended_tar, mode="w", format=tarfile.PAX_FORMAT
            ) as archive:

                def extended_metadata(info: tarfile.TarInfo) -> tarfile.TarInfo:
                    info.uid = 0
                    info.gid = 0
                    info.uname = ""
                    info.gname = ""
                    info.mtime = 0
                    info.pax_headers = {}
                    if info.name.endswith("/ContextLattice-Install.sh"):
                        info.pax_headers["SCHILY.xattr.user.fixture"] = "present"
                    return info

                archive.add(
                    stage,
                    arcname="ContextLattice-linux-bootstrap",
                    filter=extended_metadata,
                )
            MODULE._write_stored_gzip(extended_tar, extended)
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "non-canonical owner, time, or PAX"
            ):
                MODULE.validate_linux_archive(
                    ROOT, extended, "paid", "v3.18.0-public-paid"
                )

            host_gzip = Path(tmp.name) / "host-gzip.tar.gz"
            with tarfile.open(host_gzip, "w:gz") as archive:
                archive.add(stage, arcname="ContextLattice-linux-bootstrap")
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "gzip header is not canonical"
            ):
                MODULE.validate_linux_archive(
                    ROOT, host_gzip, "paid", "v3.18.0-public-paid"
                )

            members = [
                stage,
                *sorted(
                    stage.rglob("*"),
                    key=lambda value: value.relative_to(stage).as_posix(),
                ),
            ]
            reordered_tar = Path(tmp.name) / "reordered.tar"
            reordered = Path(tmp.name) / "reordered.tar.gz"
            self.write_linux_tar(stage, reordered_tar, list(reversed(members)))
            MODULE._write_stored_gzip(reordered_tar, reordered)
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "canonical deterministic encoding"
            ):
                MODULE.validate_linux_archive(
                    ROOT, reordered, "paid", "v3.18.0-public-paid"
                )

            empty = Path(tmp.name) / "empty"
            empty.write_bytes(b"")
            extra_member = Path(tmp.name) / "empty.gz"
            MODULE._write_stored_gzip(empty, extra_member)
            trailing = Path(tmp.name) / "trailing.tar.gz"
            trailing.write_bytes(first.read_bytes() + extra_member.read_bytes())
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "trailing or concatenated gzip data"
            ):
                MODULE.validate_linux_archive(
                    ROOT, trailing, "paid", "v3.18.0-public-paid"
                )
        finally:
            tmp.cleanup()

    def test_linux_archive_rejects_resource_exhaustion_before_extraction(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        original_archive_limit = MODULE.MAX_LINUX_ARCHIVE_BYTES
        original_member_limit = MODULE.MAX_LINUX_MEMBER_BYTES
        original_member_count = MODULE.MAX_LINUX_ARCHIVE_MEMBERS
        try:
            archive = Path(tmp.name) / "bounded.tar.gz"
            MODULE.build_linux_archive(stage, archive)

            MODULE.MAX_LINUX_ARCHIVE_BYTES = archive.stat().st_size - 1
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "compressed-size limit"
            ):
                MODULE.validate_linux_archive(
                    ROOT, archive, "paid", "v3.18.0-public-paid"
                )
            MODULE.MAX_LINUX_ARCHIVE_BYTES = original_archive_limit

            MODULE.MAX_LINUX_MEMBER_BYTES = 1
            with self.assertRaisesRegex(MODULE.OuterContractError, "member exceeds"):
                MODULE.validate_linux_archive(
                    ROOT, archive, "paid", "v3.18.0-public-paid"
                )
            MODULE.MAX_LINUX_MEMBER_BYTES = original_member_limit

            MODULE.MAX_LINUX_ARCHIVE_MEMBERS = 1
            with self.assertRaisesRegex(MODULE.OuterContractError, "member-count"):
                MODULE.validate_linux_archive(
                    ROOT, archive, "paid", "v3.18.0-public-paid"
                )
        finally:
            MODULE.MAX_LINUX_ARCHIVE_BYTES = original_archive_limit
            MODULE.MAX_LINUX_MEMBER_BYTES = original_member_limit
            MODULE.MAX_LINUX_ARCHIVE_MEMBERS = original_member_count
            tmp.cleanup()

    def test_linux_archive_rejects_nonstored_deflate_before_tar_parsing(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        try:
            tar_path = Path(tmp.name) / "canonical.tar"
            MODULE._write_canonical_linux_tar(stage, tar_path)
            raw = tar_path.read_bytes()
            compressor = zlib.compressobj(level=9, wbits=-15)
            compressed = compressor.compress(raw) + compressor.flush()
            archive = Path(tmp.name) / "compressed.tar.gz"
            archive.write_bytes(
                MODULE.CANONICAL_LINUX_GZIP_HEADER
                + compressed
                + struct.pack(
                    "<II", binascii.crc32(raw) & 0xFFFFFFFF, len(raw) & 0xFFFFFFFF
                )
            )
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "canonical stored-block deflate"
            ):
                MODULE.validate_linux_archive(
                    ROOT, archive, "paid", "v3.18.0-public-paid"
                )
        finally:
            tmp.cleanup()

    def test_linux_archive_validation_and_tar_parse_share_one_file_handle(self) -> None:
        tmp, stage = self.staged_fixture("linux")
        try:
            archive = Path(tmp.name) / "same-handle.tar.gz"
            MODULE.build_linux_archive(stage, archive)
            source = MODULE._open_validated_canonical_linux_gzip(archive)
            try:
                replacement = Path(tmp.name) / "replacement.tar.gz"
                replacement.write_bytes(b"not a gzip archive")
                replacement.replace(archive)
                with tarfile.open(fileobj=source, mode="r:gz") as retained:
                    self.assertGreater(len(retained.getmembers()), 0)
            finally:
                source.close()
        finally:
            tmp.cleanup()

    def test_metadata_must_bind_the_outer_contract(self) -> None:
        tmp, stage = self.staged_fixture("windows")
        try:
            metadata = stage / "payload/contextlattice-release.json"
            payload = json.loads(metadata.read_text(encoding="utf-8"))
            payload["installer_outer_contract_sha256"] = "f" * 64
            metadata.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "metadata contract hash is invalid"
            ):
                MODULE.validate_tree(
                    ROOT, stage, "windows", "paid", "v3.18.0-public-paid"
                )
        finally:
            tmp.cleanup()

    def test_msi_custom_action_table_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory(prefix="outer-msiinfo-") as tmp:
            root = Path(tmp)
            msi = root / "fixture.msi"
            msi.write_bytes(b"fixture")
            msiinfo = root / "msiinfo"
            msiinfo.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = tables ]; then\n'
                "  printf '%s\\n' Property File CustomAction\n"
                "else\n"
                "  printf '%s\\n' 'Action Type Source Target' 's72 i2 S72 S255' "
                "'CustomAction Action' 'RunPayload 3073 BinaryData launch'\n"
                "fi\n",
                encoding="utf-8",
            )
            msiinfo.chmod(0o755)
            with self.assertRaisesRegex(MODULE.OuterContractError, "CustomAction"):
                MODULE.validate_msi_tables(msi, str(msiinfo))

    def test_empty_msi_side_effect_tables_are_allowed(self) -> None:
        with tempfile.TemporaryDirectory(prefix="outer-msiinfo-empty-") as tmp:
            root = Path(tmp)
            msi = root / "fixture.msi"
            msi.write_bytes(b"fixture")
            msiinfo = root / "msiinfo"
            msiinfo.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = tables ]; then\n'
                "  printf '%s\\n' File CustomAction Registry\n"
                "else\n"
                "  printf '%s\\n' 'Column' 's72' \"$4 Column\"\n"
                "fi\n",
                encoding="utf-8",
            )
            msiinfo.chmod(0o755)
            result = MODULE.validate_msi_tables(msi, str(msiinfo))
            self.assertEqual(result["populated_required_empty_tables"], 0)

    def test_unreviewed_msi_table_is_rejected_even_when_empty(self) -> None:
        with tempfile.TemporaryDirectory(prefix="outer-msiinfo-unreviewed-") as tmp:
            root = Path(tmp)
            msi = root / "fixture.msi"
            msi.write_bytes(b"fixture")
            msiinfo = root / "msiinfo"
            msiinfo.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = tables ]; then\n'
                "  printf '%s\\n' File RemoveRegistry\n"
                "else\n"
                "  printf '%s\\n' 'Column' 's72' \"$4 Column\"\n"
                "fi\n",
                encoding="utf-8",
            )
            msiinfo.chmod(0o755)
            with self.assertRaisesRegex(
                MODULE.OuterContractError, "unreviewed tables: RemoveRegistry"
            ):
                MODULE.validate_msi_tables(msi, str(msiinfo))


if __name__ == "__main__":
    unittest.main()
