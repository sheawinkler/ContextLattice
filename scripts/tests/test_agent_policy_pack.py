#!/usr/bin/env python3

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).parents[1] / "agent_hooks" / "agent_policy_pack.sh"


class AgentPolicyPackTest(unittest.TestCase):
    def run_with_fake_curl(
        self,
        *,
        stdout: str = "",
        stdout_size: int = 0,
        exit_code: int = 0,
        timeout: str | None = "1",
    ) -> subprocess.CompletedProcess[str]:
        with tempfile.TemporaryDirectory() as temp_dir:
            fake_bin = Path(temp_dir)
            fake_curl = fake_bin / "curl"
            fake_curl.write_text(
                "#!/bin/zsh\n"
                "if [[ -n \"${FAKE_CURL_STDOUT_FILE:-}\" ]]; then\n"
                "  /bin/cat \"$FAKE_CURL_STDOUT_FILE\"\n"
                "else\n"
                "  print -rn -- \"${FAKE_CURL_STDOUT:-}\"\n"
                "fi\n"
                "exit \"${FAKE_CURL_EXIT:-0}\"\n",
                encoding="utf-8",
            )
            fake_curl.chmod(0o755)

            env = os.environ.copy()
            env["PATH"] = f"{fake_bin}:{env['PATH']}"
            env["FAKE_CURL_STDOUT"] = stdout
            env["FAKE_CURL_EXIT"] = str(exit_code)
            if stdout_size:
                response_file = fake_bin / "response.txt"
                response_file.write_text("x" * stdout_size, encoding="utf-8")
                env["FAKE_CURL_STDOUT_FILE"] = str(response_file)
            command = [str(SCRIPT)]
            if timeout is not None:
                command.extend(("--timeout", timeout))
            return subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

    def assert_degraded(self, result: subprocess.CompletedProcess[str], warning: str) -> None:
        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        self.assertIs(payload["ok"], False)
        self.assertIs(payload["retrieval"]["degraded"], True)
        warnings = " ".join(payload["retrieval"]["warnings"])
        self.assertIn(warning, warnings)
        self.assertIn("degraded-memory mode", warnings)
        for key in ("mission", "objective", "goal", "usage"):
            self.assertIn(key, payload)
        self.assertIn("search", payload["usage"])

    def test_transport_failure_is_reported_as_degraded(self) -> None:
        self.assert_degraded(
            self.run_with_fake_curl(exit_code=7),
            "request failed",
        )

    def test_watchdog_failure_is_typed_and_does_not_claim_partial_results(self) -> None:
        result = self.run_with_fake_curl(exit_code=28, stdout='{"results":[{"file":"must-not-claim.md"}]}', timeout="200")

        self.assert_degraded(result, "watchdog timeout")
        payload = json.loads(result.stdout)
        self.assertEqual(payload["status"], "degraded")
        self.assertEqual(payload["result_state"], "degraded")
        self.assertIs(payload["partial"], False)
        self.assertEqual(payload["retrieval"]["result_count"], 0)
        self.assertIs(payload["retrieval"]["partial"], False)
        self.assertEqual(payload["error"]["type"], "watchdog_timeout")
        self.assertEqual(payload["watchdog"]["status"], "failed")
        self.assertEqual(payload["watchdog"]["requested_timeout_secs"], 200.0)
        self.assertEqual(payload["watchdog"]["effective_timeout_secs"], 200.0)
        self.assertNotIn("max_timeout_secs", payload["watchdog"])
        self.assertTrue(payload["watchdog"]["effective_deadline"])
        self.assertIn("do not replay", payload["watchdog"]["repair_hint"])

    def test_invalid_timeout_falls_back_to_repo_default_with_typed_hint(self) -> None:
        result = self.run_with_fake_curl(timeout="nan", stdout='{"degraded":false,"result_state":"ready","results":[]}')

        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        watchdog = payload["watchdog"]
        self.assertEqual(watchdog["timeout_validation"], "invalid_fell_back_to_default")
        self.assertEqual(watchdog["requested_timeout_secs"], None)
        self.assertEqual(watchdog["effective_timeout_secs"], 200.0)
        self.assertEqual(watchdog["default_timeout_secs"], 200.0)
        self.assertIn("finite positive", watchdog["repair_hint"])

    def test_unusable_responses_are_reported_as_degraded(self) -> None:
        cases = (
            ("", "empty response"),
            ("not-json", "invalid JSON"),
            ("[]", "invalid response shape"),
            ("{}", "invalid response shape"),
        )
        for stdout, warning in cases:
            with self.subTest(stdout=stdout):
                self.assert_degraded(self.run_with_fake_curl(stdout=stdout), warning)

    def test_explicit_error_response_is_reported_as_degraded(self) -> None:
        result = self.run_with_fake_curl(stdout='{"ok":false,"warnings":["upstream unavailable"]}')

        self.assert_degraded(result, "upstream unavailable")

    def test_native_degraded_states_include_mode_directive(self) -> None:
        cases = (
            '{"degraded":true,"result_state":"degraded","results":[],"warnings":["source timeout"]}',
            '{"degraded":false,"result_state":"degraded","results":[],"warnings":[]}',
        )
        for stdout in cases:
            with self.subTest(stdout=stdout):
                self.assert_degraded(self.run_with_fake_curl(stdout=stdout), "degraded-memory mode")

    def test_malformed_contract_fields_are_reported_as_degraded(self) -> None:
        cases = (
            '{"degraded":0,"results":[]}',
            '{"ok":"false","results":[]}',
            '{"degraded":false,"result_state":0,"results":[]}',
            '{"degraded":true,"results":[],"warnings":"source timeout"}',
            '{"degraded":false,"results":[1]}',
        )
        for stdout in cases:
            with self.subTest(stdout=stdout):
                self.assert_degraded(self.run_with_fake_curl(stdout=stdout), "invalid response shape")

    def test_response_larger_than_arg_max_retains_static_pack(self) -> None:
        response_size = os.sysconf("SC_ARG_MAX") + 1024

        self.assert_degraded(
            self.run_with_fake_curl(stdout_size=response_size),
            "invalid JSON",
        )

    def test_healthy_response_remains_healthy(self) -> None:
        result = self.run_with_fake_curl(
            stdout='{"degraded":false,"result_state":"ready","results":[{"file":"proof.md"}],"warnings":[]}'
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout)
        self.assertIs(payload["ok"], True)
        self.assertIs(payload["retrieval"]["degraded"], False)
        self.assertEqual(payload["retrieval"]["result_count"], 1)
        self.assertEqual(payload["status"], "ready")
        self.assertEqual(payload["watchdog"]["status"], "completed")
        self.assertEqual(payload["watchdog"]["effective_timeout_secs"], 1.0)

    def test_repo_default_timeout_is_preserved_when_not_overridden(self) -> None:
        result = self.run_with_fake_curl(timeout=None, stdout='{"degraded":false,"result_state":"ready","results":[]}')

        self.assertEqual(result.returncode, 0, result.stderr)
        watchdog = json.loads(result.stdout)["watchdog"]
        self.assertEqual(watchdog["requested_timeout_secs"], 200.0)
        self.assertEqual(watchdog["effective_timeout_secs"], 200.0)
        self.assertEqual(watchdog["default_timeout_secs"], 200.0)


if __name__ == "__main__":
    unittest.main()
