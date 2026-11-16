"""Synthetic zsh installer fixtures; not native release acceptance."""
import hashlib
import io
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
COMMIT, TREE = 'a' * 40, 'b' * 40


@unittest.skipUnless(os.name == 'posix' and os.path.isfile('/bin/zsh') and os.access('/bin/zsh', os.X_OK),
                     'synthetic installer fixtures require POSIX and /bin/zsh')
class InstallerTransactionReadinessTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='synthetic-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        (self.bin / 'python3').symlink_to(sys.executable)
        self.install = self.root / 'install'
        self.install.mkdir()
        (self.install / '.env').write_text('SYNTHETIC_SECRET=keep-me\n')
        (self.install / 'previous-only').write_text('old tree')
        (self.install / 'data').mkdir()
        (self.install / 'data' / 'sentinel').write_text('before')
        self.log = self.root / 'calls'
        metadata = dict(schema_id='contextlattice_release_payload.v2', lane='public',
            tag='v3.18.0-public', commit=COMMIT, source_tree=TREE, runtime_version='3.18.0',
            build_channel='stable', release_ref='refs/tags/v3.18.0-public',
            source='approved_lane_tagged_checkout', approved_source_repository='sheawinkler/ContextLattice',
            approved_source_ref='refs/heads/main', installer_outer_contract_schema_id='contextlattice_installer_outer_contract.v1',
            installer_outer_contract_sha256='c'*64, frontier_t1_release_gate='not_applicable',
            frontier_t2_release_gate='not_applicable')
        metadata_raw = (json.dumps(metadata, indent=2, sort_keys=True)+'\n').encode()
        (self.install / '.contextlattice-release.json').write_bytes(metadata_raw)
        payload = self.root / 'payload'
        payload.mkdir()
        (payload / 'contextlattice-release.json').write_bytes(metadata_raw)
        archive = payload / 'contextlattice-payload.tar.gz'
        members = {'.contextlattice-release.json': metadata_raw, '.env.example': b'EXAMPLE=1\n',
                   'docker-compose.lite.yml': b'services: {}\n',
                   'scripts/install_global_agent_tools.sh': b'#!/bin/zsh -f\nprint global >> "$FIXTURE_LOG"\n'}
        with tarfile.open(archive, 'w:gz') as tar:
            for name, data in members.items():
                entry = tarfile.TarInfo('contextlattice/' + name)
                entry.mode = 0o755 if name.endswith('.sh') else 0o644
                entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data))
        (payload / (archive.name + '.sha256')).write_text(hashlib.sha256(archive.read_bytes()).hexdigest()+'  '+archive.name+'\n')
        self.script = self.root / 'installer'
        self.script.write_text((ROOT / 'packaging/linux/ContextLattice-Install.sh').read_text().replace('@RELEASE_LANE@', 'public'))
        self.fake('docker', '''
print -r -- "docker $*" >> "$FIXTURE_LOG"
if [[ "$1" == info ]]; then
  [[ "$FIXTURE_MODE" != daemon_fail ]] || exit 1
  print linux
elif [[ "$1" == compose && "$*" == *' up '* ]]; then
  print after > "$FIXTURE_INSTALL/data/runtime-write"
  [[ "$FIXTURE_MODE" != compose_fail ]] || exit 7
elif [[ "$1" == compose && "$*" == *' ps '* ]]; then
  print aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
elif [[ "$1" == inspect ]]; then
  if [[ "$FIXTURE_MODE" == stale ]]; then rev=ffffffffffffffffffffffffffffffffffffffff; else rev=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; fi
  health=healthy
  started=2026-01-01T00:00:00Z
  restarts=0
  if [[ -f "$FIXTURE_HTTP_DONE" ]]; then
    [[ "$FIXTURE_MODE" != restart ]] || { started=2026-01-01T00:00:01Z; restarts=1; }
    [[ "$FIXTURE_MODE" != counter_only ]] || restarts=1
    [[ "$FIXTURE_MODE" != started_only ]] || started=2026-01-01T00:00:01Z
    [[ "$FIXTURE_MODE" != health_regression ]] || health=starting
    /bin/rm -f "$FIXTURE_HTTP_DONE"
  fi
  if [[ "$*" == *'.State.StartedAt'* ]]; then
    print "true|$health|$rev|bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb|3.18.0|$started|$restarts"
  else
    print "true|$health|$rev|bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb|3.18.0"
  fi
elif [[ "$1" == exec && "$*" == *printenv* ]]; then
  print 8091
elif [[ "$1" == exec ]]; then
  print '{"ok":true,"ready":true,"service":"gateway-go","build":{"source_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_tree":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","boot_nonce":"cccccccccccccccccccccccccccccccc"}}'
elif [[ "$1" == port ]]; then
  print '127.0.0.1:18091'
fi
''')
        self.fake('curl', '''
print curl >> "$FIXTURE_LOG"
[[ "$FIXTURE_MODE" != health_fail ]] || exit 22
if [[ "$FIXTURE_MODE" == unrelated ]]; then nonce=dddddddddddddddddddddddddddddddd; else nonce=cccccccccccccccccccccccccccccccc; fi
if [[ "$FIXTURE_MODE" == restart || "$FIXTURE_MODE" == health_regression || "$FIXTURE_MODE" == counter_only || "$FIXTURE_MODE" == started_only ]]; then
  print changed > "$FIXTURE_HTTP_DONE"
fi
print '{"ok":true,"ready":true,"service":"gateway-go","build":{"source_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_tree":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","boot_nonce":"'"$nonce"'"}}'
''')
        self.fake('sleep', 'print sleep >> "$FIXTURE_LOG"')
        self.fake('uname', 'print -r -- "${FIXTURE_HOST_OS:-Linux}"')
        self.fake('open', 'print open >> "$FIXTURE_LOG"')
        self.fake('sha256sum', '''
python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest(),sys.argv[1])' "$1"
''')
        self.env = {'PATH': str(self.bin)+':/usr/bin:/bin:/usr/sbin:/sbin', 'HOME': str(self.root),
                    'TMPDIR': str(self.root), 'TMPPREFIX': str(self.root / 'zsh'),
                    'PAYLOAD_DIR': str(payload), 'FIXTURE_LOG': str(self.log),
                    'FIXTURE_INSTALL': str(self.install), 'FIXTURE_HTTP_DONE': str(self.root / 'http-done'), 'LC_ALL': 'C'}

    def fake(self, name, body):
        path = self.bin / name
        path.write_text('#!/bin/zsh -f\n' + body + '\n')
        path.chmod(0o755)

    def run_case(self, mode, *args):
        result = subprocess.run(['/bin/zsh', '-f', str(self.script), '--install-dir', str(self.install), *args],
                                env={**self.env, 'FIXTURE_MODE': mode}, text=True, capture_output=True, timeout=45)
        return result, self.log.read_text() if self.log.exists() else ''

    def assert_retained(self):
        self.assertEqual((self.install / 'data/runtime-write').read_text().strip(), 'after')
        self.assertIn('SYNTHETIC_SECRET=keep-me', (self.install / '.env').read_text())
        previous = list(self.root.glob('.install.install.*/previous'))
        self.assertEqual(len(previous), 1)
        self.assertEqual((previous[0] / 'previous-only').read_text(), 'old tree')
        self.assertEqual((previous[0] / 'data/sentinel').read_text(), 'before')

    def add_gateway_witness(self, name, content):
        archive = self.root / 'payload/contextlattice-payload.tar.gz'
        with tarfile.open(archive, 'r:gz') as source:
            rows = [(member, source.extractfile(member).read()) for member in source if member.isfile()]
        with tarfile.open(archive, 'w:gz') as target:
            for member, data in rows:
                target.addfile(member, io.BytesIO(data))
            member = tarfile.TarInfo('contextlattice/services/gateway-go/' + name)
            member.mode = 0o644
            member.size = len(content)
            target.addfile(member, io.BytesIO(content))
        archive.with_name(archive.name + '.sha256').write_text(
            hashlib.sha256(archive.read_bytes()).hexdigest() + '  ' + archive.name + '\n')

    def test_actual_public_disabled_compatibility_stub_is_installable(self):
        content = (ROOT / 'services/gateway-go/optional_paid_extensions.go').read_bytes()
        self.assertIn(b'"frontier_t1_governance_state.v1"', content)
        self.assertIn(b'"configured": false', content)
        self.add_gateway_witness('optional_paid_extensions.go', content)
        result, log = self.run_case('no_launch', '--no-launch')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('SYNTHETIC_SECRET=keep-me', (self.install / '.env').read_text())
        self.assertNotIn('docker ', log)

    def test_paid_marker_matrix_still_rejects_before_replacement(self):
        markers = (
            'context_policy_activation.v1', 'context_mesh_orchestration.v1',
            'frontier_delta_packet_automation.v1', 'frontier_shared_proof_timeline.v1',
            'frontier_t4_retrieval_governance_state.v1', 'frontier_t5_policy_laboratory_governance_state.v1',
            'frontier_t6_agent_fit_governance_state.v1', 'frontier_t7_portable_continuation_governance_state.v1',
            'frontier_t8_skill_evolution_governance_state.v1', 'frontier_t9_continuity_zero_governance_state.v1',
            'frontier_t10_aggregate_governance_state.v1', 'contextlattice_runtime_license_public_keys.v1',
            'CONTEXTLATTICE_FRONTIER_T2_PACKET_AUTOMATION', 'CONTEXTLATTICE_FRONTIER_T5_POLICY_GOVERNANCE',
            'CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_GOVERNANCE', 'CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_GOVERNANCE',
            'CONTEXTLATTICE_FRONTIER_T8_SKILL_EVOLUTION_GOVERNANCE', 'CONTEXTLATTICE_FRONTIER_T9_CONTINUITY_ZERO_GOVERNANCE',
            'CONTEXTLATTICE_FRONTIER_T10_AGGREGATE_GOVERNANCE', 'runtimeLicenseVerifier', 'runtimeLicenseSchemaID',
            'GO_V4_ENTITLEMENT', 'GO_V4_RUNTIME_LICENSE', 'GO_V4_MACHINE_BINDING',
        )
        for marker in markers:
            with self.subTest(marker=marker):
                case = type(self)('test_paid_marker_matrix_still_rejects_before_replacement')
                case.setUp()
                self.addCleanup(case.doCleanups)
                case.add_gateway_witness('witness.go', marker.encode())
                result, log = case.run_case('no_launch', '--no-launch')
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('paid/private gateway markers', result.stderr)
                self.assertTrue((case.install / 'previous-only').exists())
                self.assertNotIn('docker ', log)

    def test_unix_and_windows_marker_guards_preserve_shared_status_only(self):
        unix = (ROOT / 'packaging/linux/ContextLattice-Install.sh').read_text()
        windows = (ROOT / 'packaging/windows/Install-ContextLattice.ps1').read_text()
        unix_pattern = re.search(r"public_runtime_marker='([^']+)'", unix).group(1)
        windows_pattern = re.search(r'\$runtimePattern = "([^"]+)"', windows).group(1)
        self.assertEqual(unix_pattern, windows_pattern)
        self.assertNotIn('frontier_t1_governance_state', unix_pattern)
        for marker in ('runtimeLicenseVerifier', 'GO_V4_', 'frontier_delta_packet_automation',
                       'context_policy_activation', 'context_mesh_orchestration'):
            self.assertIn(marker, unix_pattern)

    def test_no_launch_does_not_require_tools(self):
        (self.bin / 'docker').unlink()
        result, log = self.run_case('no_launch', '--no-launch')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('docker ', log)
        self.assertFalse(list(self.root.glob('.install.install.*')))

    def test_public_unmanaged_existing_tree_is_not_replaced(self):
        (self.install / '.contextlattice-release.json').unlink()
        result, log = self.run_case('no_launch', '--no-launch')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('existing install is unmanaged', result.stderr)
        self.assertEqual((self.install / 'previous-only').read_text(), 'old tree')
        self.assertEqual((self.install / 'data/sentinel').read_text(), 'before')
        self.assertNotIn('docker ', log)

    def test_public_paid_downgrade_requires_explicit_consent(self):
        (self.install / '.contextlattice-release.json').write_text(json.dumps({'lane': 'paid'}, indent=2) + '\n')
        paid = self.install / 'config/runtime-license'
        paid.mkdir(parents=True)
        (paid / 'marker').write_text('paid')
        result, log = self.run_case('no_launch', '--no-launch')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('paid-to-public downgrade refused', result.stderr)
        self.assertTrue((paid / 'marker').exists())
        result, log = self.run_case('no_launch', '--no-launch', '--allow-paid-to-public-downgrade')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(paid.exists())
        self.assertEqual((self.install / 'data/sentinel').read_text(), 'before')
        self.assertIn('SYNTHETIC_SECRET=keep-me', (self.install / '.env').read_text())
        self.assertNotIn('docker ', log)

    def test_fresh_install_entropy_failure_cannot_publish_weak_key(self):
        self.fake('openssl', '''
case "$FIXTURE_MODE" in
  entropy_success) print -r -- aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  entropy_short) print -r -- aa ;;
  entropy_exit_output) print -r -- aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; exit 1 ;;
  entropy_invalid_fallback) print -r -- zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz ;;
  entropy_invalid) print -r -- zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz ;;
  *) exit 1 ;;
esac
''')
        self.fake('od', '''
case "$FIXTURE_MODE" in
  entropy_fallback|entropy_invalid_fallback) print -r -- 'aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa' ;;
  entropy_pipe_fail) print -r -- aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; exit 1 ;;
  entropy_short) print -r -- aa ;;
  entropy_invalid) print -r -- zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz ;;
  *) exit 1 ;;
esac
''')
        for mode in ('entropy_failed', 'entropy_short', 'entropy_invalid', 'entropy_exit_output',
                     'entropy_pipe_fail', 'entropy_fallback', 'entropy_invalid_fallback', 'entropy_success'):
            with self.subTest(mode=mode):
                (self.install / '.env').unlink(missing_ok=True)
                result, log = self.run_case(mode, '--no-launch')
                if mode in {'entropy_fallback', 'entropy_invalid_fallback', 'entropy_success'}:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    values = dict(line.split('=', 1) for line in (self.install / '.env').read_text().splitlines()
                                  if '=' in line and not line.startswith('#'))
                    self.assertRegex(values['CONTEXTLATTICE_ORCHESTRATOR_API_KEY'], r'^cl_[0-9a-f]{48}$')
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn('could not securely generate', result.stderr)
                    self.assertTrue((self.install / 'previous-only').is_file())
                    self.assertFalse((self.install / '.env').exists())
                self.assertNotIn('docker ', log)
                self.assertFalse(list(self.root.glob('.install.install.*')))
        original_env = (self.install / '.env').read_bytes()
        self.fake('openssl', 'print entropy-called >> "$FIXTURE_LOG"; exit 77')
        self.fake('od', 'print entropy-called >> "$FIXTURE_LOG"; exit 77')
        result, log = self.run_case('entropy_preserved', '--no-launch')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('CONTEXTLATTICE_ORCHESTRATOR_API_KEY=cl_' + 'a' * 48,
                      (self.install / '.env').read_text())
        self.assertNotIn('entropy-called', log)
        self.assertEqual((self.install / '.env').read_bytes(), original_env)

    def test_compose_failure_preserves_both_trees(self):
        result, log = self.run_case('compose_fail')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('Install complete.', result.stdout)
        self.assertNotIn('global', log)
        self.assertNotIn('open', log.splitlines())
        self.assert_retained()

    def test_health_timeout_preserves_both_trees(self):
        result, log = self.run_case('health_fail')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(log.count('sleep\n'), 30)
        self.assertNotIn('Install complete.', result.stdout)
        self.assertNotIn('global', log)
        self.assertNotIn('open', log.splitlines())
        self.assert_retained()

    def test_stale_gateway_identity_rejected(self):
        result, log = self.run_case('stale')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('global', log)
        self.assert_retained()

    def test_unrelated_http_nonce_rejected(self):
        result, log = self.run_case('unrelated')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('global', log)
        self.assert_retained()

    def test_optimized_python_cannot_accept_unrelated_gateway(self):
        self.env['PYTHONOPTIMIZE'] = '2'
        result, log = self.run_case('unrelated')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('global', log)
        self.assert_retained()

    def test_same_id_restart_or_health_regression_rejected(self):
        windows = (ROOT / 'packaging/windows/Install-ContextLattice.ps1').read_text()
        self.assertIn('{{json .State.StartedAt}},{{json .RestartCount}}', windows)
        self.assertIn('$finalState -cne $state', windows)
        self.assertGreater(windows.index('$finalState = Get-GatewayReadinessIdentity'), windows.index('$again = @(& docker compose'))
        for mode in ('restart', 'health_regression'):
            with self.subTest(mode=mode):
                case = type(self)('test_same_id_restart_or_health_regression_rejected')
                case.setUp()
                self.addCleanup(case.doCleanups)
                result, log = case.run_case(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('Install complete.', result.stdout)
                self.assertNotIn('global', log.splitlines())
                self.assertNotIn('open', log.splitlines())
                case.assert_retained()

    def test_success_runs_optional_actions_after_readiness(self):
        result, log = self.run_case('success')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Install complete. Gateway API: http://127.0.0.1:18091', result.stdout)
        self.assertLess(log.index('curl'), log.index('global'))
        self.assertLess(log.splitlines().index('global'), log.splitlines().index('open'))
        self.assertFalse(list(self.root.glob('.install.install.*')))

    def test_qualification_flags_skip_global_tools_and_both_browser_commands(self):
        self.fake('xdg-open', 'print xdg-open >> "$FIXTURE_LOG"')
        result, log = self.run_case('success', '--skip-agent-tools', '--no-browser')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('curl', log.splitlines())
        self.assertTrue({'global', 'open', 'xdg-open'}.isdisjoint(log.splitlines()))

    def test_no_browser_blocks_xdg_fallback_without_open(self):
        (self.bin / 'open').unlink()
        self.fake('xdg-open', 'print xdg-open >> "$FIXTURE_LOG"')
        result, log = self.run_case('success', '--no-browser')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('global', log.splitlines())
        self.assertNotIn('xdg-open', log.splitlines())

    def test_counter_and_started_at_fences_fail_independently(self):
        for mode in ('counter_only', 'started_only'):
            with self.subTest(mode=mode):
                case = type(self)('test_counter_and_started_at_fences_fail_independently')
                case.setUp()
                self.addCleanup(case.doCleanups)
                result, log = case.run_case(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('Install complete.', result.stdout)
                self.assertNotIn('global', log.splitlines())
                self.assertNotIn('open', log.splitlines())
                case.assert_retained()

    def test_extract_only_is_file_only(self):
        (self.bin / 'docker').unlink()
        result, log = self.run_case('no_launch', '--extract-only')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('docker ', log)
        self.assertFalse(list(self.root.glob('.install.install.*')))

    def test_missing_docker_fails_before_publication(self):
        (self.bin / 'docker').unlink()
        result, log = self.run_case('missing')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.install / 'previous-only').read_text(), 'old tree')
        self.assertFalse(list(self.root.glob('.install.install.*')))
        self.assertNotIn('open', log.splitlines())

    def test_unsupported_host_launch_is_rejected_before_publication_or_helpers(self):
        self.env['FIXTURE_HOST_OS'] = 'FreeBSD'
        result, log = self.run_case('wrong_host')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('requires Linux or macOS', result.stderr)
        self.assertEqual((self.install / 'previous-only').read_text(), 'old tree')
        self.assertFalse(list(self.root.glob('.install.install.*')))
        self.assertNotIn('docker ', log)
        self.assertNotIn('global', log)

    def test_non_linux_file_only_remains_supported(self):
        self.env['FIXTURE_HOST_OS'] = 'Darwin'
        (self.bin / 'docker').unlink()
        result, log = self.run_case('no_launch', '--extract-only')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('docker ', log)
        self.assertNotIn('global', log)

    def test_staged_macos_launcher_accepts_darwin_and_verifies_readiness(self):
        from scripts import release_installer_outer

        stage = self.root / 'macos-outer'
        release_installer_outer.stage(ROOT, stage, 'macos', 'public', 'v3.18.0-public')
        self.script = stage / 'ContextLattice.command'
        self.env['FIXTURE_HOST_OS'] = 'Darwin'
        result, log = self.run_case('success')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Install complete. Gateway API:', result.stdout)
        self.assertLess(log.index('curl'), log.index('global'))

    def test_unavailable_daemon_fails_before_publication(self):
        result, log = self.run_case('daemon_fail')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.install / 'previous-only').read_text(), 'old tree')
        self.assertFalse(list(self.root.glob('.install.install.*')))
        self.assertNotIn('global', log.splitlines())
        self.assertNotIn('open', log.splitlines())

    def test_readiness_parser_rejects_placeholder_nonce_and_coerced_types(self):
        source = self.script.read_text()
        parser = re.search(r"\| python3 -c '(.*?)' \"\$\{metadata_commit\}\"", source, re.S).group(1)
        valid = dict(ok=True, ready=True, service='gateway-go',
                     build=dict(source_commit=COMMIT, source_tree=TREE, boot_nonce='c' * 32))

        def execute(left, right):
            return subprocess.run([sys.executable, '-I', '-S', '-B', '-c', parser, COMMIT, TREE],
                input=json.dumps(left) + '\n' + json.dumps(right) + '\n', text=True,
                capture_output=True, timeout=5, env=self.env).returncode

        self.assertEqual(execute(valid, valid), 0)
        for nonce in (None, '', ' ', 'unknown', 'c' * 31, 'C' * 32, 'c' * 32 + '\n', 1, True, ['c' * 32]):
            with self.subTest(nonce=nonce):
                invalid = {**valid, 'build': {**valid['build'], 'boot_nonce': nonce}}
                self.assertNotEqual(execute(invalid, invalid), 0)
        for key in ('ok', 'ready'):
            for value in (False, 1, 'true'):
                with self.subTest(key=key, value=value):
                    self.assertNotEqual(execute({**valid, key: value}, valid), 0)
        for key in ('service', 'source_commit', 'source_tree'):
            expected = valid[key] if key == 'service' else valid['build'][key]
            for value in ([], [expected], None, True, 42, 'wrong'):
                for side in ('inside', 'outside'):
                    with self.subTest(key=key, value=value, side=side):
                        invalid = ({**valid, key: value} if key == 'service' else
                                   {**valid, 'build': {**valid['build'], key: value}})
                        self.assertNotEqual(execute(invalid if side == 'inside' else valid,
                                                    invalid if side == 'outside' else valid), 0)

    def test_windows_source_requires_guarded_readiness_and_recovery(self):
        source = (ROOT / 'packaging/windows/Install-ContextLattice.ps1').read_text()
        self.assertIn('function Test-GatewayReadiness', source)
        self.assertIn('boot_nonce', source)
        self.assertIn('$response.ok -isnot [bool]', source)
        self.assertIn('$response.ready -isnot [bool]', source)
        for field in ('service', 'build.source_commit', 'build.source_tree'):
            self.assertIn('$response.' + field + ' -isnot [string]', source)
        self.assertIn('$response -isnot [System.Management.Automation.PSCustomObject]', source)
        self.assertIn('$response.build -isnot [System.Management.Automation.PSCustomObject]', source)
        self.assertIn("-cnotmatch '\\A[0-9a-f]{32}\\z'", source)
        self.assertIn("docker info --format '{{.OSType}}'", source)
        self.assertIn('org.opencontainers.image.revision', source)
        self.assertIn('Install complete. Gateway API:', source)
        self.assertLess(source.index('Test-GatewayReadiness -'), source.index('scripts\\install_global_agent_tools.ps1'))
        self.assertIn('retaining both', source.lower())
        self.assertNotIn('{{json .Config.Env}}', source)

    def test_windows_inspect_template_avoids_legacy_embedded_quote_loss(self):
        source = (ROOT / 'packaging/windows/Install-ContextLattice.ps1').read_text()
        helper = source.split('function Get-GatewayReadinessIdentity {', 1)[1].split('function Test-GatewayReadiness', 1)[0]
        template = re.search(r"\$format = '([^']+)'", helper).group(1)
        self.assertNotIn('"', template)
        self.assertIn('{{json .Config.Labels}}', template)
        self.assertNotIn('.Config.Env', template)
        self.assertNotIn('.State.Health.Log', template)
        self.assertIn('$probe.Count -ne 5', helper)
        self.assertIn('$probe[0] -isnot [bool]', helper)
        self.assertIn('$probe[4] -lt 0', helper)
        for key in ('org.opencontainers.image.revision', 'io.contextlattice.source-tree', 'org.opencontainers.image.version'):
            self.assertIn("$probe[2].'" + key + "'", helper)
        self.assertIn('ConvertTo-Json -InputObject $identity -Compress', helper)


if __name__ == '__main__':
    unittest.main()
