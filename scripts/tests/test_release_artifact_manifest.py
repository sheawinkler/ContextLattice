from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

from public_release_material_scan import (  # noqa: E402
    PublicReleaseMaterialError,
    scan_public_release_files,
)
from release_artifact_manifest import ReleaseMetadataError, _walk_metadata  # noqa: E402

MANIFEST = ROOT / "scripts/release_artifact_manifest.py"
AUDIT = ROOT / "scripts/agent/audit-public-release-assets"
BINDING_AUDIT = ROOT / "scripts/agent/verify-release-sbom-binding"
ARCHIVE_AUDIT = ROOT / "scripts/agent/verify-release-archive-sha256"
PUBLIC_WORKFLOW = ROOT / ".github/workflows/public-release-installers.yml"
PAID_WORKFLOW = ROOT / ".github/workflows/release-installers.yml"
PAID_INTEGRITY_AUDIT = ROOT / "scripts/agent/audit-paid-artifact-integrity"
PAID_RELEASE_SURFACE_AVAILABLE = PAID_WORKFLOW.is_file() and PAID_INTEGRITY_AUDIT.is_file()
ARTIFACTS = (
    "ContextLattice-macOS-universal.dmg",
    "ContextLattice-windows-x64.msi",
    "ContextLattice-linux-bootstrap.tar.gz",
)
TAG = "v5.0.0"
COMMIT = "0123456789abcdef0123456789abcdef01234567"
SYFT_ARCHIVE_SHA256 = "d6400b579fa84dd383573b1d1ff6f081a37fc64d3ffaafdfdda95c4325f204be"


class ReleaseArtifactManifestTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="release-integrity-")
        self.dist = Path(self.temp.name) / "dist"
        self.dist.mkdir()
        for index, name in enumerate(ARTIFACTS, start=1):
            (self.dist / name).write_bytes((f"final-installer-{index}\n").encode("ascii"))

    def tearDown(self) -> None:
        self.temp.cleanup()

    @unittest.skipUnless(PAID_RELEASE_SURFACE_AVAILABLE, "paid release surface is not part of the public lane")
    def test_paid_msi_audit_uses_confined_parser_and_closed_extraction_root(self) -> None:
        audit = PAID_INTEGRITY_AUDIT.read_text(encoding="utf-8")
        self.assertIn("run_confined_msiinfo", audit)
        self.assertIn("run_confined_msiextract", audit)
        self.assertIn("--unshare-all", audit)
        self.assertIn("--clearenv", audit)
        self.assertIn("ulimit -f 16384", audit)
        self.assertIn("final MSI extraction contains an unexpected root path", audit)
        self.assertIn("final MSI extraction contains an unexpected Program Files path", audit)
        self.assertIn("final Windows MSI extraction contains a path outside the reviewed closed world", audit)
        self.assertIn("validate_msiinfo_outputs", audit)
        self.assertIn("validate-msi-closure", audit)
        self.assertIn("Directory.txt", audit)
        self.assertIn("Component.txt", audit)
        self.assertIn("File.txt", audit)

    @unittest.skipUnless(PAID_RELEASE_SURFACE_AVAILABLE, "paid release surface is not part of the public lane")
    def test_paid_downloaded_native_receipts_bind_tool_identity(self) -> None:
        workflow = PAID_WORKFLOW.read_text(encoding="utf-8")
        self.assertIn('"tools": tools', workflow)
        self.assertIn('"script_sha256"', workflow)
        self.assertIn("import shutil", workflow)
        self.assertIn('git", "show", f"{commit}:scripts/agent/audit-paid-artifact-integrity"', workflow)
        self.assertIn("closed audit receipt has a noncanonical tool identity", workflow)
        self.assertIn("Install and attest native MSI containment and parser tools", workflow)

    @unittest.skipUnless(PAID_RELEASE_SURFACE_AVAILABLE, "paid release surface is not part of the public lane")
    def test_paid_final_promotion_rechecks_closed_set_and_exact_hdiutil_identity(self) -> None:
        workflow = PAID_WORKFLOW.read_text(encoding="utf-8")
        final_start = workflow.index("  release-integrity:")
        final = workflow[final_start:]
        promotion = final.index("      - name: Promote audited draft release")
        pre_promotion = final[:promotion]
        self.assertIn("paid-final-dmg-native-identity", pre_promotion)
        self.assertIn('subprocess.run([str(tool), "help"]', workflow)
        self.assertIn('row["tools"] != final_identity["tools"]', pre_promotion)
        self.assertIn('final_identity.get("artifact") != row["artifact"]', pre_promotion)
        self.assertNotIn('set(row["tools"]) != {"hdiutil"}', pre_promotion)
        self.assertIn("Re-download and semantically audit exact closed paid draft set immediately before promotion", pre_promotion)
        self.assertIn("scripts/agent/audit-paid-release-assets", pre_promotion)
        self.assertIn("scripts/agent/verify-release-sbom-binding", pre_promotion)
        self.assertIn("scripts/agent/audit-paid-artifact-integrity", pre_promotion)
        self.assertIn("--approved-source-ref-tip", pre_promotion)
        paid_release_audit = Path(ROOT / "scripts/agent/audit-paid-release-assets").read_text(encoding="utf-8")
        for marker in (
            "_regular_closed_set",
            "verify_manifest",
            "verify_sha_sums",
            "_validate_sbom",
            "_validate_signer_and_resolution",
            "_validate_tree_proof",
            '"semantic_audit"',
        ):
            self.assertIn(marker, paid_release_audit)
        self.assertNotIn("FINAL_AUDITED_REMOTE_DIST", pre_promotion)
        self.assertNotIn("paid-final-audited", pre_promotion)
        self.assertLess(
            pre_promotion.index("Re-download and semantically audit exact closed paid draft set immediately before promotion"),
            promotion,
        )

    @unittest.skipUnless(PAID_RELEASE_SURFACE_AVAILABLE, "paid release surface is not part of the public lane")
    def test_embedded_paid_linux_msi_receipt_script_executes(self) -> None:
        workflow = PAID_WORKFLOW.read_text(encoding="utf-8")
        start = workflow.index("Emit closed downloaded Linux/MSI audit receipts")
        section = workflow[start:]
        match = re.search(r"<<'PY'\n(.*?)^[ \t]*PY[ \t]*$", section, re.MULTILINE | re.DOTALL)
        self.assertIsNotNone(match)
        embedded = textwrap.dedent(match.group(1))
        repo = Path(self.temp.name) / "embedded-receipt-repo"
        (repo / "dist").mkdir(parents=True)
        (repo / "scripts/agent").mkdir(parents=True)
        shutil.copy2(PAID_INTEGRITY_AUDIT, repo / "scripts/agent/audit-paid-artifact-integrity")
        for name in ARTIFACTS:
            (repo / "dist" / name).write_bytes(name.encode("utf-8"))
        bin_dir = repo / "bin"
        bin_dir.mkdir()
        for name in ("bwrap", "msiinfo", "msiextract", "python3"):
            tool = bin_dir / name
            tool.write_text("#!/bin/sh\nprintf '%s version 1\\n' \"$0\"\n", encoding="utf-8")
            tool.chmod(0o700)
        git = lambda *args: subprocess.run(  # noqa: E731
            ["git", "-C", str(repo), *args],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
            text=True,
        )
        git("init", "-q")
        git("add", ".")
        git(
            "-c",
            "user.name=Release Test",
            "-c",
            "user.email=release-test@example.invalid",
            "commit",
            "-q",
            "-m",
            "embedded receipt",
        )
        commit = git("rev-parse", "HEAD").stdout.strip()
        output = Path(self.temp.name) / "embedded-receipts"
        output.mkdir()
        old_cwd = Path.cwd()
        old_path = os.environ.get("PATH")
        old_argv = sys.argv[:]
        old_executable = sys.executable
        try:
            os.chdir(repo)
            os.environ["PATH"] = f"{bin_dir}:{old_path or ''}"
            sys.executable = str(bin_dir / "python3")
            sys.argv = ["embedded-paid-receipts", "v5.0.0-public-paid", commit, str(output)]
            exec(compile(embedded, "<embedded-paid-receipts>", "exec"), {"__name__": "__main__"})
        finally:
            os.chdir(old_cwd)
            if old_path is None:
                os.environ.pop("PATH", None)
            else:
                os.environ["PATH"] = old_path
            sys.argv = old_argv
            sys.executable = old_executable
        linux_receipt = json.loads(
            (output / "ContextLattice-linux-bootstrap.tar.gz.receipt.json").read_text(encoding="utf-8")
        )
        windows_receipt = json.loads(
            (output / "ContextLattice-windows-x64.msi.receipt.json").read_text(encoding="utf-8")
        )
        self.assertEqual(linux_receipt["tools"]["audit_script"]["version"], "committed-tree")
        self.assertEqual(set(windows_receipt["tools"]), {"audit_script", "python3", "bwrap", "msiinfo", "msiextract"})

    def command(self, script: Path, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(script), *args],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )

    def generate(
        self,
        *,
        lane: str = "public",
        dist: Path | None = None,
    ) -> subprocess.CompletedProcess[str]:
        target = dist or self.dist
        public_safe = lane == "public"
        first_args = [
            "--dist-dir",
            str(target),
            "--lane",
            lane,
            "--channel",
            "stable",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
            "--provenance-name",
            "ContextLattice-release-provenance.json",
            "--verify",
        ]
        if public_safe:
            first_args.append("--public-safe")
        result = self.command(
            MANIFEST,
            *first_args,
        )
        if result.returncode == 0:
            self.write_sboms(target)
            second_args = [
                "--dist-dir",
                str(target),
                "--lane",
                lane,
                "--channel",
                "stable",
                "--tag",
                TAG,
                "--commit",
                COMMIT,
                "--provenance-name",
                "ContextLattice-release-provenance.json",
                "--integrity-dir",
                str(target),
                "--sbom-dir",
                str(target),
                "--verify",
            ]
            if public_safe:
                second_args.append("--public-safe")
            result = self.command(
                MANIFEST,
                *second_args,
            )
        return result

    def write_sboms(self, dist: Path | None = None) -> None:
        target = dist or self.dist
        for name in ARTIFACTS:
            digest = hashlib.sha256((target / name).read_bytes()).hexdigest()
            payload = {
                "spdxVersion": "SPDX-2.3",
                "dataLicense": "CC0-1.0",
                "SPDXID": "SPDXRef-DOCUMENT",
                "name": name,
                "documentNamespace": f"https://anchore.com/syft/file/{name}",
                "creationInfo": {
                    "created": "2026-08-11T00:00:00Z",
                    "creators": ["Tool: syft-1.32.0"],
                },
                "packages": [
                    {
                        "SPDXID": "SPDXRef-Package",
                        "name": name,
                        "downloadLocation": "NOASSERTION",
                        "filesAnalyzed": False,
                    }
                ],
                "files": [
                    {
                        "SPDXID": "SPDXRef-File",
                        "fileName": name,
                        "checksums": [{"algorithm": "SHA256", "checksumValue": digest}],
                    }
                ],
            }
            (target / f"{name}.sbom.spdx.json").write_text(
                json.dumps(payload, sort_keys=True) + "\n", encoding="utf-8"
            )

    def audit(self, dist: Path | None = None) -> subprocess.CompletedProcess[str]:
        return self.command(
            AUDIT,
            "--dist-dir",
            str(dist or self.dist),
            "--lane",
            "public",
            "--channel",
            "stable",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
        )

    def binding(
        self,
        index: int = 0,
        *,
        dist: Path | None = None,
        artifact: Path | None = None,
        sbom: Path | None = None,
        receipt: Path | None = None,
        lane: str = "public",
        write_receipt: bool = False,
        failure_report: Path | None = None,
    ) -> subprocess.CompletedProcess[str]:
        name = ARTIFACTS[index]
        target = dist or self.dist
        args = [
            "--artifact",
            str(artifact or target / name),
            "--sbom",
            str(sbom or target / f"{name}.sbom.spdx.json"),
            "--receipt",
            str(receipt or target / f"{name}.integrity.json"),
            "--lane",
            lane,
            "--tag",
            TAG,
            "--commit",
            COMMIT,
        ]
        if write_receipt:
            args.append("--write-receipt")
        if failure_report is not None:
            args.extend(("--failure-report", str(failure_report)))
        return self.command(BINDING_AUDIT, *args)

    def test_generation_is_path_free_sorted_and_idempotent(self) -> None:
        first = self.generate()
        self.assertEqual(first.returncode, 0, first.stderr)
        manifest_path = self.dist / "ContextLattice-release-manifest-stable.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(manifest["release_ref"], f"refs/tags/{TAG}")
        self.assertEqual([row["name"] for row in manifest["artifacts"]], sorted(ARTIFACTS))
        self.assertTrue(all(set(row) == {"name", "size_bytes", "sha256"} for row in manifest["artifacts"]))
        serialized = json.dumps(manifest)
        self.assertNotIn('"path"', serialized)
        self.assertNotIn("/Volumes/", serialized)
        for row in manifest["artifacts"]:
            receipt = json.loads(
                (self.dist / f"{row['name']}.integrity.json").read_text(encoding="utf-8")
            )
            self.assertEqual(
                receipt["schema_id"],
                "contextlattice_release_artifact_integrity_receipt.v1",
            )
            self.assertEqual(receipt["artifact"], row)
            self.assertEqual(receipt["sbom"]["name"], f"{row['name']}.sbom.spdx.json")
            self.assertEqual(
                receipt["sbom"]["sha256"],
                hashlib.sha256(
                    (self.dist / f"{row['name']}.sbom.spdx.json").read_bytes()
                ).hexdigest(),
            )
            self.assertNotIn("spdxVersion", receipt)
            self.assertNotIn("packageVerificationCode", receipt)
        second = self.generate()
        self.assertEqual(second.returncode, 0, second.stderr)
        bound = self.binding()
        self.assertEqual(bound.returncode, 0, bound.stderr)
        audited = self.audit()
        self.assertEqual(audited.returncode, 0, audited.stderr)
        self.assertIn('"closed_asset_set": true', audited.stdout)
        self.assertIn('"public_material_scan": {', audited.stdout)

    def test_public_material_scan_allows_digests_and_deterministic_binary(self) -> None:
        sample = Path(self.temp.name) / "safe-public-bytes.bin"
        digest = hashlib.sha256(b"safe-release-bytes").hexdigest().encode("ascii")
        deterministic_binary = bytes(range(256)) * 8
        sample.write_bytes(
            deterministic_binary
            + b"\n"
            + digest
            + b"\nsk_"
            + b"live_\npassword=false\n/usr/bin/contextlattice\n"
        )
        result = scan_public_release_files([sample], chunk_size=17)
        self.assertTrue(result["ok"])
        self.assertEqual(result["finding_count"], 0)
        self.assertEqual(result["file_count"], 1)

    def test_public_material_scan_detects_chunk_boundary_shapes_without_echo(self) -> None:
        cases = (
            ("token.stripe_live", b"sk_" + b"live_" + b"A" * 32),
            ("key.private_pem_header", b"-----BEGIN " + b"PRIVATE KEY-----"),
            ("path.private_posix", b"/Users/operator/private-release"),
            ("path.private_windows_drive", rb"C:\Users\operator\private-release"),
            ("path.private_unc", rb"\\server\share\private-release"),
        )
        for index, (expected_code, material) in enumerate(cases):
            with self.subTest(code=expected_code):
                sample = Path(self.temp.name) / f"chunk-boundary-{index}.bin"
                sample.write_bytes(b"x" * 14 + b"\n" + material + b"\n")
                with self.assertRaises(PublicReleaseMaterialError) as caught:
                    scan_public_release_files([sample], chunk_size=16)
                report = caught.exception.report
                self.assertIn(expected_code, {row["code"] for row in report["findings"]})
                self.assertTrue(
                    all(
                        set(row) == {"asset", "code", "evidence_digest"}
                        for row in report["findings"]
                    )
                )
                self.assertNotIn(material.decode("ascii"), str(caught.exception))

    def test_public_material_scan_reports_bounded_sanitized_findings(self) -> None:
        sample = Path(self.temp.name) / "bounded-findings.bin"
        materials = [
            b"sk_" + b"live_" + f"{index:02d}".encode("ascii") + b"A" * 30
            for index in range(40)
        ]
        sample.write_bytes(b"\n".join(materials) + b"\n")
        with self.assertRaises(PublicReleaseMaterialError) as caught:
            scan_public_release_files([sample], chunk_size=31)
        report = caught.exception.report
        self.assertEqual(report["finding_count"], 32)
        self.assertTrue(report["truncated"])
        for finding in report["findings"]:
            self.assertEqual(set(finding), {"asset", "code", "evidence_digest"})
            self.assertEqual(finding["code"], "token.stripe_live")
            self.assertRegex(finding["evidence_digest"], r"\Asha256:[0-9a-f]{64}\Z")
        rendered = str(caught.exception)
        self.assertTrue(all(material.decode("ascii") not in rendered for material in materials))

    def test_public_material_scan_rejects_binary_installer_secret_without_echo(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        material = b"sk_" + b"live_" + b"Z" * 32
        installer = self.dist / ARTIFACTS[0]
        installer.write_bytes(installer.read_bytes() + b"\x00" + material + b"\x00")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("token.stripe_live", result.stderr)
        self.assertNotIn(material.decode("ascii"), result.stderr)
        self.assertIn("evidence_digest", result.stderr)

    def test_public_material_scan_rejects_nested_metadata_credentials(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        material = "client_" + "secret=" + "Q" * 32
        metadata_paths = (
            self.dist / "ContextLattice-release-provenance.json",
            self.dist / f"{ARTIFACTS[0]}.sbom.spdx.json",
            self.dist / f"{ARTIFACTS[0]}.integrity.json",
        )
        for metadata_path in metadata_paths:
            with self.subTest(asset=metadata_path.name):
                original = metadata_path.read_bytes()
                payload = json.loads(original)
                payload["nested_scan_fixture"] = [{"values": [material]}]
                metadata_path.write_text(json.dumps(payload, sort_keys=True) + "\n", encoding="utf-8")
                result = self.audit()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("credential.named_value", result.stderr)
                self.assertNotIn(material, result.stderr)
                metadata_path.write_bytes(original)

    def test_remote_downloaded_public_set_is_material_scanned_before_promotion(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        remote = Path(self.temp.name) / "remote-downloaded-set"
        shutil.copytree(self.dist, remote)
        credential = b"Authorization: Bearer " + b"R" * 40
        installer = remote / ARTIFACTS[1]
        installer.write_bytes(installer.read_bytes() + b"\n" + credential + b"\n")
        result = self.audit(remote)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("credential.authorization_header", result.stderr)
        self.assertNotIn(credential.decode("ascii"), result.stderr)

        workflow = PUBLIC_WORKFLOW.read_text(encoding="utf-8")
        self.assertEqual(workflow.count("scripts/agent/audit-public-release-assets"), 2)
        remote_audit = workflow.index("Download and audit the complete remote draft asset set")
        promotion = workflow.index("Promote audited draft release", remote_audit)
        self.assertIn("scripts/agent/audit-public-release-assets", workflow[remote_audit:promotion])

    def test_manifest_artifact_rows_reject_unknown_fields(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        manifest_path = self.dist / "ContextLattice-release-manifest-stable.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["artifacts"][0]["unexpected_field"] = "not-authorized"
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("artifact row fields are invalid", result.stderr)

    def test_remote_sbom_pair_is_reused_across_regenerated_timestamps(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        remote = Path(self.temp.name) / "remote-published"
        shutil.copytree(self.dist, remote)
        original_pairs = {
            name: (
                (remote / f"{name}.sbom.spdx.json").read_bytes(),
                (remote / f"{name}.integrity.json").read_bytes(),
            )
            for name in ARTIFACTS
        }

        for index, name in enumerate(ARTIFACTS, start=1):
            sbom_path = self.dist / f"{name}.sbom.spdx.json"
            sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
            sbom["creationInfo"]["created"] = f"2026-08-11T00:00:{index:02d}Z"
            sbom_path.write_text(json.dumps(sbom, sort_keys=True) + "\n", encoding="utf-8")
            (self.dist / f"{name}.integrity.json").unlink()

        rerun = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "public",
            "--channel",
            "stable",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
            "--provenance-name",
            "ContextLattice-release-provenance.json",
            "--integrity-dir",
            str(self.dist),
            "--sbom-dir",
            str(self.dist),
            "--public-safe",
            "--verify-only",
            "--write-integrity",
        )
        self.assertEqual(rerun.returncode, 0, rerun.stderr)

        for index, name in enumerate(ARTIFACTS):
            remote_sbom, remote_receipt = original_pairs[name]
            self.assertEqual((remote / f"{name}.sbom.spdx.json").read_bytes(), remote_sbom)
            self.assertEqual((remote / f"{name}.integrity.json").read_bytes(), remote_receipt)
            self.assertNotEqual((self.dist / f"{name}.integrity.json").read_bytes(), remote_receipt)
            bound = self.binding(index, dist=remote)
            self.assertEqual(bound.returncode, 0, bound.stderr)

        audited = self.audit(remote)
        self.assertEqual(audited.returncode, 0, audited.stderr)

        for workflow in (path for path in (PUBLIC_WORKFLOW, PAID_WORKFLOW) if path.is_file()):
            text = workflow.read_text(encoding="utf-8")
            self.assertNotIn("--clobber", text)
            self.assertIn("RECOVERED_RELEASE_RECEIPT_DIR", text)
            self.assertIn("SBOM-only draft state", text)
            self.assertIn("receipt-only draft state", text)
            upload_start = text.index('recovered_dir="${RECOVERED_RELEASE_RECEIPT_DIR:-}"')
            upload_end = text.index("gh release upload", upload_start) + len("gh release upload")
            upload_block = text[upload_start:upload_end]
            self.assertIn("missing+=(\"$candidate\")", upload_block)
            self.assertIn("if (( ${#missing[@]} )); then", upload_block)
            self.assertEqual(upload_block.count("gh release upload"), 1)

    @unittest.skipUnless(PAID_RELEASE_SURFACE_AVAILABLE, "paid release surface is not part of the public lane")
    def test_paid_final_audit_reuses_semantically_bound_timestamp_drift_pairs(self) -> None:
        paid_local = Path(self.temp.name) / "paid-timestamp-local"
        paid_local.mkdir()
        for name in ARTIFACTS:
            shutil.copy2(self.dist / name, paid_local / name)
        generated = self.generate(lane="paid", dist=paid_local)
        self.assertEqual(generated.returncode, 0, generated.stderr)

        paid_remote = Path(self.temp.name) / "paid-timestamp-remote"
        shutil.copytree(paid_local, paid_remote)
        remote_pair_bytes = {
            proof_name: (paid_remote / proof_name).read_bytes()
            for name in ARTIFACTS
            for proof_name in (f"{name}.sbom.spdx.json", f"{name}.integrity.json")
        }

        for index, name in enumerate(ARTIFACTS, start=1):
            sbom_path = paid_local / f"{name}.sbom.spdx.json"
            sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
            sbom["creationInfo"]["created"] = f"2026-08-12T00:00:{index:02d}Z"
            sbom_path.write_text(json.dumps(sbom, sort_keys=True) + "\n", encoding="utf-8")
            (paid_local / f"{name}.integrity.json").unlink()

        regenerated = self.command(
            MANIFEST,
            "--dist-dir",
            str(paid_local),
            "--lane",
            "paid",
            "--channel",
            "stable",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
            "--provenance-name",
            "ContextLattice-release-provenance.json",
            "--integrity-dir",
            str(paid_local),
            "--sbom-dir",
            str(paid_local),
            "--verify-only",
            "--write-integrity",
        )
        self.assertEqual(regenerated.returncode, 0, regenerated.stderr)

        remote_bound_names: set[str] = set()
        for index, name in enumerate(ARTIFACTS):
            bound = self.binding(index, dist=paid_remote, lane="paid")
            self.assertEqual(bound.returncode, 0, bound.stderr)
            self.assertIn('"state": "bound_pair_verified"', bound.stdout)
            for proof_name in (f"{name}.sbom.spdx.json", f"{name}.integrity.json"):
                remote_bound_names.add(proof_name)
                self.assertEqual((paid_remote / proof_name).read_bytes(), remote_pair_bytes[proof_name])
                self.assertNotEqual((paid_local / proof_name).read_bytes(), remote_pair_bytes[proof_name])

        self.assertEqual(
            remote_bound_names,
            {
                proof_name
                for name in ARTIFACTS
                for proof_name in (f"{name}.sbom.spdx.json", f"{name}.integrity.json")
            },
        )
        self.assertEqual(
            {path.name for path in paid_local.iterdir()},
            {path.name for path in paid_remote.iterdir()},
        )
        for local_path in paid_local.iterdir():
            if local_path.name in remote_bound_names:
                continue
            self.assertEqual(local_path.read_bytes(), (paid_remote / local_path.name).read_bytes())

        workflow = PAID_WORKFLOW.read_text(encoding="utf-8")
        final_audit_start = workflow.index("Download, rehash, and audit the complete remote draft set")
        promotion = workflow.index("Promote audited draft release", final_audit_start)
        final_audit = workflow[final_audit_start:promotion]
        self.assertIn("remote_bound_names=()", final_audit)
        self.assertIn('mark_remote_bound "$installer.sbom.spdx.json"', final_audit)
        self.assertIn('mark_remote_bound "$installer.integrity.json"', final_audit)
        self.assertLess(
            final_audit.index('if is_remote_bound "$name"; then'),
            final_audit.index('cmp -s "$file" "$remote_dist/$name"'),
        )
        self.assertNotIn('[[ "$name" == *.sbom.spdx.json ]]', final_audit)

    def test_public_and_paid_draft_recovery_matrix_and_operator_report(self) -> None:
        for lane in ("public", "paid"):
            lane_dist = Path(self.temp.name) / f"{lane}-lane"
            lane_dist.mkdir()
            for name in ARTIFACTS:
                shutil.copy2(self.dist / name, lane_dist / name)
            generated = self.generate(lane=lane, dist=lane_dist)
            self.assertEqual(generated.returncode, 0, generated.stderr)

            both = self.binding(dist=lane_dist, lane=lane)
            self.assertEqual(both.returncode, 0, both.stderr)
            self.assertIn('"state": "bound_pair_verified"', both.stdout)

            invalid_pair = Path(self.temp.name) / f"{lane}-invalid-pair"
            shutil.copytree(lane_dist, invalid_pair)
            invalid_receipt_path = invalid_pair / f"{ARTIFACTS[0]}.integrity.json"
            invalid_receipt = json.loads(invalid_receipt_path.read_text(encoding="utf-8"))
            invalid_receipt["sbom"]["sha256"] = "0" * 64
            invalid_receipt_path.write_text(
                json.dumps(invalid_receipt, sort_keys=True) + "\n", encoding="utf-8"
            )
            rejected_pair = self.binding(dist=invalid_pair, lane=lane)
            self.assertNotEqual(rejected_pair.returncode, 0)
            self.assertIn("SBOM digest mismatch", rejected_pair.stderr)

            sbom_only = Path(self.temp.name) / f"{lane}-sbom-only"
            shutil.copytree(lane_dist, sbom_only)
            receipt_name = f"{ARTIFACTS[0]}.integrity.json"
            (sbom_only / receipt_name).unlink()
            recovery_dir = Path(self.temp.name) / f"{lane}-recovery"
            recovery_dir.mkdir()
            recovered_receipt = recovery_dir / receipt_name
            report = recovery_dir / "repair-required.json"
            recovered = self.binding(
                dist=sbom_only,
                lane=lane,
                receipt=recovered_receipt,
                write_receipt=True,
                failure_report=report,
            )
            self.assertEqual(recovered.returncode, 0, recovered.stderr)
            self.assertIn('"state": "sbom_only_recovered"', recovered.stdout)
            (sbom_only / receipt_name).write_bytes(recovered_receipt.read_bytes())
            converged = self.binding(dist=sbom_only, lane=lane)
            self.assertEqual(converged.returncode, 0, converged.stderr)
            self.assertIn('"state": "bound_pair_verified"', converged.stdout)
            if lane == "public":
                audited = self.audit(sbom_only)
                self.assertEqual(audited.returncode, 0, audited.stderr)

            receipt_only = Path(self.temp.name) / f"{lane}-receipt-only"
            shutil.copytree(lane_dist, receipt_only)
            (receipt_only / f"{ARTIFACTS[0]}.sbom.spdx.json").unlink()
            receipt_state = self.binding(
                artifact=receipt_only / ARTIFACTS[0],
                sbom=lane_dist / f"{ARTIFACTS[0]}.sbom.spdx.json",
                receipt=receipt_only / receipt_name,
                lane=lane,
            )
            self.assertEqual(receipt_state.returncode, 0, receipt_state.stderr)
            self.assertIn('"state": "bound_pair_verified"', receipt_state.stdout)

            neither = Path(self.temp.name) / f"{lane}-neither"
            shutil.copytree(lane_dist, neither)
            (neither / f"{ARTIFACTS[0]}.sbom.spdx.json").unlink()
            (neither / receipt_name).unlink()
            missing = self.binding(dist=neither, lane=lane)
            self.assertNotEqual(missing.returncode, 0)
            self.assertIn("SBOM", missing.stderr)

            invalid = Path(self.temp.name) / f"{lane}-invalid-sbom"
            shutil.copytree(lane_dist, invalid)
            (invalid / receipt_name).unlink()
            invalid_sbom_path = invalid / f"{ARTIFACTS[0]}.sbom.spdx.json"
            invalid_sbom = json.loads(invalid_sbom_path.read_text(encoding="utf-8"))
            invalid_sbom["files"][0]["checksums"][0]["checksumValue"] = "0" * 64
            invalid_sbom_path.write_text(json.dumps(invalid_sbom, sort_keys=True) + "\n", encoding="utf-8")
            invalid_recovery = Path(self.temp.name) / f"{lane}-invalid-recovery"
            invalid_recovery.mkdir()
            invalid_report = invalid_recovery / "repair-required.json"
            first_failure = self.binding(
                dist=invalid,
                lane=lane,
                receipt=invalid_recovery / receipt_name,
                write_receipt=True,
                failure_report=invalid_report,
            )
            self.assertNotEqual(first_failure.returncode, 0)
            self.assertTrue(invalid_report.is_file())
            report_bytes = invalid_report.read_bytes()
            report_payload = json.loads(report_bytes)
            self.assertEqual(report_payload["schema_id"], "contextlattice_release_sbom_repair_required.v1")
            second_failure = self.binding(
                dist=invalid,
                lane=lane,
                receipt=invalid_recovery / receipt_name,
                write_receipt=True,
                failure_report=invalid_report,
            )
            self.assertNotEqual(second_failure.returncode, 0)
            self.assertEqual(invalid_report.read_bytes(), report_bytes)
            self.assertFalse((invalid_recovery / receipt_name).exists())

    def test_metadata_paths_normalize_and_reject_all_machine_path_forms(self) -> None:
        unsafe_values = (
            r"\\server\share\secret",
            r"C:\Users\secret",
            r"\\?\C:\secret",
            r"\\.\PhysicalDrive0",
            r"\??\C:\secret",
            "file:///Users/secret",
            "FILE://localhost/Users/secret",
            "/private/var/secret",
            "/Users/secret",
            "/Volumes/secret",
            "/",
        )
        for lane in ("public", "paid"):
            with self.subTest(lane=lane):
                for value in unsafe_values:
                    with self.subTest(value=value):
                        payload = {"nested": [{"values": [value]}, [value]]}
                        with self.assertRaisesRegex(ReleaseMetadataError, "absolute or private path"):
                            _walk_metadata(payload, public_safe=lane == "public")

    def test_public_and_paid_sbom_fixtures_reject_nested_machine_paths(self) -> None:
        unsafe_values = (
            r"\\server\share\secret",
            r"C:\Users\secret",
            r"\\?\C:\secret",
            r"\\.\PhysicalDrive0",
            r"\??\C:\secret",
            "file:///Users/secret",
            "FILE://localhost/Users/secret",
            "/private/var/secret",
            "/Users/secret",
            "/Volumes/secret",
            "/",
        )
        for lane in ("public", "paid"):
            lane_dist = Path(self.temp.name) / f"{lane}-sbom-paths"
            lane_dist.mkdir()
            for name in ARTIFACTS:
                shutil.copy2(self.dist / name, lane_dist / name)
            generated = self.generate(lane=lane, dist=lane_dist)
            self.assertEqual(generated.returncode, 0, generated.stderr)
            sbom_path = lane_dist / f"{ARTIFACTS[0]}.sbom.spdx.json"
            original = sbom_path.read_bytes()
            for value in unsafe_values:
                with self.subTest(lane=lane, value=value):
                    sbom = json.loads(original)
                    sbom["annotations"] = [{"nested": [{"values": [value]}, [value]]}]
                    sbom_path.write_text(json.dumps(sbom, sort_keys=True) + "\n", encoding="utf-8")
                    result = self.binding(dist=lane_dist, lane=lane)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("absolute or private path", result.stderr)
                    sbom_path.write_bytes(original)

    def test_syft_archive_digest_pin_accepts_only_exact_bytes(self) -> None:
        archive = self.dist / "syft_1.32.0_linux_amd64.tar.gz"
        archive.write_bytes(b"deterministic syft archive bytes\n")
        expected = hashlib.sha256(archive.read_bytes()).hexdigest()
        accepted = self.command(
            ARCHIVE_AUDIT,
            "--archive",
            str(archive),
            "--sha256",
            expected,
        )
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        wrong_pin = self.command(
            ARCHIVE_AUDIT,
            "--archive",
            str(archive),
            "--sha256",
            "0" * 64,
        )
        self.assertNotEqual(wrong_pin.returncode, 0)
        self.assertIn("SHA256 mismatch", wrong_pin.stderr)
        archive.write_bytes(b"changed archive bytes\n")
        changed_bytes = self.command(
            ARCHIVE_AUDIT,
            "--archive",
            str(archive),
            "--sha256",
            expected,
        )
        self.assertNotEqual(changed_bytes.returncode, 0)
        self.assertIn("SHA256 mismatch", changed_bytes.stderr)

    def test_syft_workflows_verify_pin_before_extraction_or_execution(self) -> None:
        for workflow in (path for path in (PUBLIC_WORKFLOW, PAID_WORKFLOW) if path.is_file()):
            text = workflow.read_text(encoding="utf-8")
            self.assertIn(f'SYFT_ARCHIVE_SHA256: "{SYFT_ARCHIVE_SHA256}"', text)
            verify_index = text.index("scripts/agent/verify-release-archive-sha256")
            extract_index = text.index("tar -xzf", verify_index)
            execute_index = text.index("syft\" version", extract_index)
            self.assertLess(verify_index, extract_index)
            self.assertLess(extract_index, execute_index)
            self.assertIn('--sha256 "$SYFT_ARCHIVE_SHA256"', text[verify_index:extract_index])

    def test_final_byte_mutation_is_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        artifact = self.dist / ARTIFACTS[0]
        artifact.write_bytes(b"mutated-after-proof\n")
        result = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "public",
            "--channel",
            "stable",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
            "--provenance-name",
            "ContextLattice-release-provenance.json",
            "--integrity-dir",
            str(self.dist),
            "--public-safe",
            "--verify-only",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("manifest verification failed", result.stderr)

    def test_verify_only_can_write_a_selected_integrity_subset(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        for name in ARTIFACTS:
            (self.dist / f"{name}.integrity.json").unlink()
        result = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "public",
            "--channel",
            "stable",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
            "--provenance-name",
            "ContextLattice-release-provenance.json",
            "--integrity-dir",
            str(self.dist),
            "--sbom-dir",
            str(self.dist),
            "--integrity-artifacts",
            ARTIFACTS[0],
            "--public-safe",
            "--verify-only",
            "--write-integrity",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.dist / f"{ARTIFACTS[0]}.integrity.json").is_file())
        self.assertFalse((self.dist / f"{ARTIFACTS[1]}.integrity.json").exists())

    def test_closed_asset_set_rejects_extra_paid_or_private_file(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        (self.dist / "ContextLattice-paid-proof.json").write_text("private\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("closed", result.stderr)

    def test_path_and_null_identity_values_are_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        provenance_path = self.dist / "ContextLattice-release-provenance.json"
        provenance = json.loads(provenance_path.read_text(encoding="utf-8"))
        provenance["release_ref"] = None
        provenance_path.write_text(json.dumps(provenance) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("null", result.stderr)

        provenance["release_ref"] = f"refs/tags/{TAG}"
        provenance["machine_path"] = "/Users/test/private/build"
        provenance_path.write_text(json.dumps(provenance) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue("absolute" in result.stderr or "private" in result.stderr)

    def test_unsafe_artifact_name_is_rejected_before_filesystem_access(self) -> None:
        result = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "public",
            "--tag",
            TAG,
            "--commit",
            COMMIT,
            "--artifacts",
            "/Volumes/contextlattice-test/private-installer.dmg",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("safe basename", result.stderr)

    def test_changed_existing_metadata_is_never_clobbered(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        manifest_path = self.dist / "ContextLattice-release-manifest-stable.json"
        manifest_path.write_text("tampered\n", encoding="utf-8")
        result = self.generate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("refusing to overwrite", result.stderr)

    def test_sbom_digest_binding_is_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        sbom_path = self.dist / f"{ARTIFACTS[0]}.sbom.spdx.json"
        sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
        sbom["files"][0]["checksums"][0]["checksumValue"] = "0" * 64
        sbom_path.write_text(json.dumps(sbom) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not bind final SBOM", result.stderr)

    def test_sbom_without_file_digest_binding_is_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        sbom_path = self.dist / f"{ARTIFACTS[0]}.sbom.spdx.json"
        sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
        sbom.pop("files")
        sbom_path.write_text(json.dumps(sbom) + "\n", encoding="utf-8")
        receipt_path = self.dist / f"{ARTIFACTS[0]}.integrity.json"
        receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
        receipt["sbom"]["size_bytes"] = sbom_path.stat().st_size
        receipt["sbom"]["sha256"] = hashlib.sha256(sbom_path.read_bytes()).hexdigest()
        receipt_path.write_text(json.dumps(receipt) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SBOM file set is invalid", result.stderr)

    def test_stale_syntactically_valid_sbom_is_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        sbom_path = self.dist / f"{ARTIFACTS[0]}.sbom.spdx.json"
        sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
        sbom["name"] = "stale-but-valid-sbom"
        sbom_path.write_text(json.dumps(sbom) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not bind final SBOM", result.stderr)

    def test_binding_rejects_changed_sbom_bytes(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        sbom_path = self.dist / f"{ARTIFACTS[0]}.sbom.spdx.json"
        sbom = json.loads(sbom_path.read_text(encoding="utf-8"))
        sbom["name"] = "different-but-valid-document"
        sbom_path.write_text(json.dumps(sbom, sort_keys=True) + "\n", encoding="utf-8")
        result = self.binding()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SBOM digest mismatch", result.stderr)

    def test_missing_sbom_binding_is_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        receipt_path = self.dist / f"{ARTIFACTS[0]}.integrity.json"
        receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
        receipt.pop("sbom")
        receipt_path.write_text(json.dumps(receipt) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("receipt fields are invalid", result.stderr)

    def test_wrong_installer_sbom_binding_is_rejected(self) -> None:
        self.assertEqual(self.generate().returncode, 0)
        receipt_path = self.dist / f"{ARTIFACTS[0]}.integrity.json"
        receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
        manifest = json.loads(
            (self.dist / "ContextLattice-release-manifest-stable.json").read_text(encoding="utf-8")
        )
        receipt["artifact"] = manifest["artifacts"][0]
        receipt_path.write_text(json.dumps(receipt) + "\n", encoding="utf-8")
        result = self.audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not bind final bytes", result.stderr)

    def test_paid_provenance_can_bind_public_paid_ref_but_not_null_ref(self) -> None:
        extra = (
            "ContextLattice-release-tree-proof.json",
            "ContextLattice-public-paid-source-manifest.json",
            "ContextLattice-release-provenance-resolution.json",
            "ContextLattice-signer-registry-proof.json",
        )
        for index, name in enumerate(extra, start=10):
            (self.dist / name).write_bytes((f"paid-proof-{index}\n").encode("ascii"))
        result = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "paid",
            "--channel",
            "stable",
            "--tag",
            "v5.0.0-public-paid",
            "--commit",
            COMMIT,
            "--artifacts",
            *ARTIFACTS,
            *extra,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest_path = self.dist / "ContextLattice-release-manifest-stable.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["provenance"] = {"approved_source_ref": "refs/heads/public-paid/main"}
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        verify = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "paid",
            "--channel",
            "stable",
            "--tag",
            "v5.0.0-public-paid",
            "--commit",
            COMMIT,
            "--artifacts",
            *ARTIFACTS,
            *extra,
            "--verify-only",
        )
        self.assertEqual(verify.returncode, 0, verify.stderr)
        manifest["provenance"]["approved_source_ref"] = None
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        rejected = self.command(
            MANIFEST,
            "--dist-dir",
            str(self.dist),
            "--lane",
            "paid",
            "--channel",
            "stable",
            "--tag",
            "v5.0.0-public-paid",
            "--commit",
            COMMIT,
            "--artifacts",
            *ARTIFACTS,
            *extra,
            "--verify-only",
        )
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn("null", rejected.stderr)


if __name__ == "__main__":
    unittest.main()
