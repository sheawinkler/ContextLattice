from __future__ import annotations

import copy
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from release_sbom import INSTALLERS, generate, merge_documents


def document(files=None):
    value = {"SPDXID": "SPDXRef-DOCUMENT", "spdxVersion": "SPDX-2.3", "packages": [],
             "creationInfo": {"creators": ["Tool: syft-1.32.0"]}, "relationships": []}
    if files is not None:
        value["files"] = files
    return value


class ReleaseSBOMTests(unittest.TestCase):
    @unittest.skipUnless(os.environ.get("CONTEXTLATTICE_TEST_SYFT_BINARY"), "explicit pinned native Syft required")
    def test_real_scanner_output_has_no_ephemeral_scan_root(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            artifact = root / "ContextLattice-linux-bootstrap.tar.gz"
            with tarfile.open(artifact, "w:gz") as archive:
                content = b"actual scanner content\n"
                info = tarfile.TarInfo("notes.txt")
                info.size = len(content)
                archive.addfile(info, io.BytesIO(content))
            output = root / "result.json"
            generate(Path(os.environ["CONTEXTLATTICE_TEST_SYFT_BINARY"]), artifact, output, root)
            raw = output.read_text()
            self.assertNotIn("release-sbom-", raw)
            self.assertNotIn(str(root).lstrip("/"), raw)
            self.assertTrue(any(row["fileName"] == "notes.txt" for row in json.loads(raw)["files"]))

    def test_workflow_uses_reviewed_driver_without_modifying_payload_source(self):
        workflow = (Path(__file__).resolve().parents[2] / ".github/workflows/public-release-installers.yml").read_text()
        self.assertIn('RELEASE_SBOM_TOOL_COMMIT: ${{ github.workflow_sha }}', workflow)
        self.assertIn('git show "$RELEASE_SBOM_TOOL_COMMIT:scripts/release_sbom.py" > "$driver"', workflow)
        self.assertIn('python3 -I "$driver"', workflow)
        self.assertIn('--temp-root "$RUNNER_TEMP"', workflow)
        self.assertLess(workflow.index('python3 -I "$driver"'), workflow.index('name: Generate immutable final SBOM bindings'))
        self.assertIn('name: Audit final bytes, private-material scan, SPDX SBOMs, and public metadata', workflow)

    def test_preserves_real_components_and_binds_only_scanned_outer_file(self):
        for name in INSTALLERS:
            sha = hashlib.sha256(name.encode()).hexdigest()
            components = document()
            components["packages"] = [{"SPDXID": "SPDXRef-Package-a", "name": "component"}]
            row = {"SPDXID": "SPDXRef-File-a", "fileName": "/" + name, "checksums": [{"algorithm": "SHA256", "checksumValue": sha}]}
            outer = document([row])
            original = copy.deepcopy(components)
            result = merge_documents(components, outer, name, sha)
            self.assertEqual(result["packages"], original["packages"])
            self.assertEqual(components, original)
            self.assertEqual(result["files"][0]["checksums"], row["checksums"])
            self.assertIn("No package records are synthesized", result["creationInfo"]["comment"])

    def test_missing_wrong_duplicate_or_colliding_scanner_records_reject(self):
        name = sorted(INSTALLERS)[0]
        sha = "a" * 64
        row = {"SPDXID": "SPDXRef-File-a", "fileName": name, "checksums": [{"algorithm": "SHA256", "checksumValue": sha}]}
        for bad in [document(), document([]), document([row, row]), document([{**row, "fileName": "wrong"}]),
                    document([{**row, "checksums": [{"algorithm": "SHA256", "checksumValue": "b" * 64}]}]), document([{**row, "fileName": "../" + name}])]:
            with self.assertRaises(ValueError):
                merge_documents(document(), bad, name, sha)
        outer = document([row])
        for field, value in [("files", None), ("packages", None), ("spdxVersion", "SPDX-2.2"), ("creationInfo", {"creators": ["Tool: syft-unknown"]})]:
            bad = document()
            bad[field] = value
            with self.assertRaises(ValueError):
                merge_documents(bad, outer, name, sha)
        collision = document()
        collision["packages"] = [{"SPDXID": "SPDXRef-Installer-" + sha}]
        with self.assertRaises(ValueError):
            merge_documents(collision, outer, name, sha)

    @unittest.skipUnless(os.environ.get("CONTEXTLATTICE_TEST_SYFT_BINARY"), "explicit pinned native Syft required")
    def test_real_pinned_syft_archive_and_opaque_final_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for name in sorted(INSTALLERS):
                artifact = root / name
                if name.endswith(".tar.gz"):
                    with tarfile.open(artifact, "w:gz") as archive:
                        content = json.dumps({"name": "sbom-holdout", "version": "1.2.3"}).encode()
                        info = tarfile.TarInfo("package.json")
                        info.size = len(content)
                        archive.addfile(info, io.BytesIO(content))
                else:
                    artifact.write_bytes(b"opaque native-wrapper fixture; not a native package audit\n")
                output = root / (name + ".sbom.spdx.json")
                result = generate(Path(os.environ["CONTEXTLATTICE_TEST_SYFT_BINARY"]), artifact, output, root)
                self.assertTrue(result["ok"])
                self.assertFalse(result["artifact_audit"])
                sbom = json.loads(output.read_text())
                self.assertTrue(any(row["fileName"].endswith(name) for row in sbom["files"]))
                if name.endswith(".tar.gz"):
                    # Syft's generic tar file source reports the outer package,
                    # not an unpacked npm application. Do not invent that coverage.
                    self.assertTrue(any(package["name"] == name for package in sbom["packages"]))
                artifact.write_bytes(b"changed")
                original_output = output.read_bytes()
                with self.assertRaises((ValueError, subprocess.SubprocessError)):
                    generate(Path(os.environ["CONTEXTLATTICE_TEST_SYFT_BINARY"]), artifact, output, root)
                self.assertEqual(output.read_bytes(), original_output)


if __name__ == "__main__":
    unittest.main()
