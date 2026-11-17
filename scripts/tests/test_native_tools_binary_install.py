"""Failure injection for the source function; not a whole installed-upgrade proof."""
import hashlib
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]


class NativeToolsBinaryInstallTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='native-cli-publication-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.global_bin = self.root / 'global'
        self.global_bin.mkdir()
        (self.root / 'services/gateway-go').mkdir(parents=True)
        self.target = self.global_bin / 'contextlattice-agent-tools'
        self.original = b'#!/bin/zsh -f\nprint old-cli\n'
        self.target.write_bytes(self.original)
        self.target.chmod(0o755)
        source = (ROOT / 'scripts/install_global_agent_tools.sh').read_text()
        match = re.search(r'(?ms)^build_go_agent_tools\(\) \{\n.*?^\}\n', source)
        self.assertIsNotNone(match)
        self.function = match.group()
        self.function_digest = hashlib.sha256(self.function.encode()).hexdigest()
        self.source = source

    def run_build(self, body, *, publish_commands=False):
        fake = self.bin / 'go'
        fake.write_text('#!/bin/zsh -f\nset -eu\n' + body + '\n')
        fake.chmod(0o755)
        script = self.root / 'invoke.zsh'
        invocation = self.function + '\nbuild_go_agent_tools\n'
        if publish_commands:
            link = re.search(r'(?ms)^install_go_native_link\(\) \{\n.*?^\}\n', self.source)
            publication = re.search(r'(?ms)^install_agent_cli_commands\(\) \(\n.*?^\)\n(?=install_agent_cli_commands\n)', self.source)
            self.assertIsNotNone(link)
            self.assertIsNotNone(publication)
            invocation = self.function + link.group() + publication.group() + '\ninstall_agent_cli_commands\n'
        script.write_text('set -eu\n' + invocation)
        return subprocess.run(['/bin/zsh', '-f', str(script)], env={
            'PATH': str(self.bin) + ':/usr/bin:/bin', 'ROOT_DIR': str(self.root),
            'GLOBAL_BIN_DIR': str(self.global_bin), 'EXPECTED_OLD': str(self.target),
        }, capture_output=True, text=True, timeout=20)

    def test_failed_compiler_preserves_working_binary(self):
        result = self.run_build('exit 9')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.target.read_bytes(), self.original)
        self.assertTrue(os.access(self.target, os.X_OK))
        self.assertFalse(list(self.global_bin.glob('.contextlattice-agent-tools.*')))

    def test_partial_failed_compile_never_publishes(self):
        result = self.run_build('print partial > "$3"\nexit 11')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.target.read_bytes(), self.original)
        self.assertFalse(list(self.global_bin.glob('.contextlattice-agent-tools.*')))

    def test_success_preserves_old_until_candidate_is_complete(self):
        result = self.run_build('grep -q old-cli "$EXPECTED_OLD"\nprint new-cli > "$3"')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.target.read_text(), 'new-cli\n')
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o755)
        self.assertFalse(list(self.global_bin.glob('.contextlattice-agent-tools.*')))

    def test_empty_success_is_not_a_publishable_binary(self):
        result = self.run_build('exit 0')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.target.read_bytes(), self.original)
        self.assertFalse(list(self.global_bin.glob('.contextlattice-agent-tools.*')))

    def test_symlink_target_is_not_followed(self):
        elsewhere = self.root / 'elsewhere'
        elsewhere.write_bytes(self.original)
        self.target.unlink()
        self.target.symlink_to(elsewhere)
        result = self.run_build('print must-not-run > "$3"')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.target.is_symlink())
        self.assertEqual(elsewhere.read_bytes(), self.original)

    def test_wrapper_generation_and_failed_build_preserve_existing_native_alias(self):
        alias = self.global_bin / 'contextlattice_search'
        alias.symlink_to(self.target.name)
        result = self.run_build('grep -q old-cli "$EXPECTED_OLD"\nexit 9', publish_commands=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Native tools build failed; the previous CLI was preserved.', result.stderr)
        self.assertEqual(self.target.read_bytes(), self.original)
        self.assertTrue(alias.is_symlink())
        self.assertEqual(alias.read_bytes(), self.original)
        self.assertEqual(sorted(p.name for p in self.global_bin.iterdir()),
                         ['contextlattice-agent-tools', 'contextlattice_search'])

    def test_successful_wrapper_publication_does_not_overwrite_native_binary(self):
        alias = self.global_bin / 'contextlattice_search'
        alias.symlink_to(self.target.name)
        result = self.run_build('grep -q old-cli "$EXPECTED_OLD"\nprint new-cli > "$3"',
                                publish_commands=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.target.read_text(), 'new-cli\n')
        self.assertTrue(alias.is_symlink())
        self.assertEqual(alias.read_text(), 'new-cli\n')
        legacy = self.global_bin / 'contextlattice_recall_quality_eval'
        self.assertFalse(legacy.is_symlink())
        self.assertIn('PYTHON_BIN=', legacy.read_text())
        self.assertTrue(os.access(legacy, os.X_OK))
        self.assertFalse(list(self.global_bin.glob('.contextlattice-*')))

    def test_windows_compiler_publication_is_guarded_and_atomic_static(self):
        source = (ROOT / 'scripts/install_global_agent_tools.ps1').read_text()
        start = source.index('$GoTool = Join-Path')
        end = source.index('$searchCmd =', start)
        publication = source[start:end]
        self.assertNotIn('Remove-Item $GoTool', publication)
        self.assertIn('& go build -o $GoCandidate', publication)
        self.assertIn('$LASTEXITCODE -ne 0', publication)
        self.assertIn('[IO.File]::Replace($GoCandidate, $GoTool, $null)', publication)
        self.assertIn('[IO.File]::Move($GoCandidate, $GoTool)', publication)
        self.assertIn('[IO.FileAttributes]::ReparsePoint', publication)
        self.assertLess(publication.index('$LASTEXITCODE -ne 0'), publication.index('[IO.File]::Replace'))


if __name__ == '__main__':
    unittest.main()
