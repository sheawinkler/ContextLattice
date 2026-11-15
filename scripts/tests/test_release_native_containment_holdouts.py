from __future__ import annotations

import contextlib
import errno
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import types
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))
SPEC = importlib.util.spec_from_file_location(
    "release_native_containment_holdouts", ROOT / "scripts/release_native_containment_holdouts.py"
)
HOLDOUT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HOLDOUT)


def execute_helper_with_fakes(**overrides):
    """Execute the real helper with no access to real system-facing modules."""
    state = {
        "caps": {"CapEff": "0", "CapPrm": "0", "CapAmb": "0"},
        "label": "unpriv_bwrap (enforce)",
        "descendant": "unpriv_bwrap (enforce)",
        "interfaces": ["lo"],
        "net_exists": True,
        "routes": "header\n",
        "input_writable": False,
        "input_errno": errno.EROFS,
        "hostname_results": [(-1, errno.EPERM), (-1, errno.EPERM)],
        "unshare_result": (-1, errno.EPERM),
    }
    state.update(overrides)
    events, output = [], []
    current_errno = [0]

    class FakePath:
        def __init__(self, value):
            self.value = value

        def read_text(self):
            events.append(("read_text", self.value))
            if self.value == "/proc/self/status":
                return "\n".join(f"{key}: {value}" for key, value in state["caps"].items())
            if self.value == "/proc/self/attr/current":
                return state["label"]
            if self.value == "/proc/net/route":
                return state["routes"]
            raise ValueError("unexpected fake read")

        def exists(self):
            if self.value != "/sys/class/net":
                raise ValueError("unexpected fake exists")
            return state["net_exists"]

        def write_bytes(self, data):
            events.append(("write_bytes", self.value, data))
            if self.value != "/input/contextlattice.msi":
                raise ValueError("unexpected fake binary write")
            if not state["input_writable"]:
                raise OSError(state["input_errno"], "synthetic readonly")
            return len(data)

        def write_text(self, data):
            events.append(("write_text", self.value, data))
            if self.value != "/output/owned-scratch":
                raise ValueError("unexpected fake text write")
            return len(data)

    def listdir(path):
        events.append(("listdir", path))
        if path != "/sys/class/net":
            raise ValueError("unexpected fake listdir")
        return state["interfaces"]

    def readlink(path):
        events.append(("readlink", path))
        prefix = "/proc/self/ns/"
        if not path.startswith(prefix) or path[len(prefix):] not in {"user", "mnt", "net", "pid"}:
            raise ValueError("unexpected fake namespace")
        return path[len(prefix):] + ":[synthetic]"

    def sethostname(value, length):
        events.append(("sethostname", value, length))
        index = sum(event[0] == "sethostname" for event in events) - 1
        result, current_errno[0] = state["hostname_results"][index]
        return result

    def unshare(flags):
        events.append(("unshare", flags))
        result, current_errno[0] = state["unshare_result"]
        return result

    def cdll(name, *, use_errno):
        events.append(("CDLL", name, use_errno))
        if name is not None or use_errno is not True:
            raise ValueError("unexpected fake CDLL")
        return types.SimpleNamespace(sethostname=sethostname, unshare=unshare)

    def run(argv, **kwargs):
        events.append(("subprocess", argv, kwargs))
        return types.SimpleNamespace(stdout=state["descendant"].encode())

    def fake_module(name, **attributes):
        module = types.ModuleType(name)
        module.__dict__.update(attributes)
        return module

    modules = {
        "ctypes": fake_module("ctypes", CDLL=cdll, get_errno=lambda: current_errno[0]),
        "errno": fake_module("errno", EROFS=errno.EROFS, EACCES=errno.EACCES, EPERM=errno.EPERM),
        "json": fake_module("json", dumps=json.dumps),
        "os": fake_module("os", listdir=listdir, readlink=readlink),
        "pathlib": fake_module("pathlib", Path=FakePath),
        "subprocess": fake_module("subprocess", run=run),
        "sys": fake_module("sys", executable="/synthetic/python3"),
    }

    def import_fake(name, _globals=None, _locals=None, _fromlist=(), level=0):
        if level or name not in modules:
            raise ImportError("unexpected helper import")
        return modules[name]

    builtins = {"__import__": import_fake, "dict": dict, "all": all, "int": int, "set": set,
                "len": len, "OSError": OSError, "AssertionError": AssertionError,
                "print": lambda value: output.append(value)}
    code = compile(HOLDOUT.HELPER, "<containment-helper>", "exec", dont_inherit=True, optimize=0)
    error = None
    with mock.patch.dict(sys.modules, modules):
        try:
            exec(code, {"__builtins__": builtins})
        except BaseException as exc:
            error = exc
    return error, output, events


class NativeContainmentHoldoutTests(unittest.TestCase):
    def setUp(self):
        self.host = {name: f"{name}:[host]" for name in ("user", "mnt", "net", "pid")}
        self.child = {name: f"{name}:[child]" for name in self.host}
        self.scratch = None
        self.command = ["/fixed/mock-bwrap", "fixed-argument"]
        real_is_dir, real_readlink = Path.is_dir, os.readlink

        def is_dir(path):
            return True if path == Path("/proc/self/ns") else real_is_dir(path)

        def readlink(path, *args, **kwargs):
            prefix = "/proc/self/ns/"
            if str(path).startswith(prefix) and str(path)[len(prefix):] in self.host:
                return self.host[str(path)[len(prefix):]]
            return real_readlink(path, *args, **kwargs)

        self.patch(mock.patch.object(HOLDOUT.os, "geteuid", return_value=501))
        self.patch(mock.patch.object(Path, "is_dir", is_dir))
        self.patch(mock.patch.object(HOLDOUT.os, "readlink", side_effect=readlink))
        self.builder = mock.Mock(side_effect=self.build_command)
        self.audit = self.patch(mock.patch.object(HOLDOUT.runpy, "run_path", return_value={
            "_confined_bwrap_command": self.builder,
        }))
        self.process = self.patch(mock.patch.object(HOLDOUT.subprocess, "run"))
        self.stdout = io.StringIO()

    def patch(self, patcher):
        value = patcher.start()
        self.addCleanup(patcher.stop)
        return value

    def build_command(self, **kwargs):
        artifact = kwargs["artifact"]
        self.scratch = artifact.parent
        self.assertEqual(artifact.name, "sentinel")
        self.assertEqual(artifact.read_bytes(), b"controlled readonly sentinel, never an installer\n")
        self.assertEqual(kwargs["output_root"], self.scratch / "output")
        self.assertTrue(kwargs["output_root"].is_dir())
        self.assertEqual(kwargs["bwrap_path"], "/usr/bin/bwrap")
        self.assertEqual(kwargs["tool_path"], str(Path("/usr/bin/python3").resolve()))
        self.assertEqual(kwargs["tool_arguments"], ["-I", "-c", HOLDOUT.HELPER])
        return self.command

    def result(self, *, code=0, child=None, stdout=None, stderr=b""):
        if stdout is None:
            stdout = json.dumps(child if child is not None else {"ok": True, "namespaces": self.child}).encode()
        return subprocess.CompletedProcess(self.command, code, stdout, stderr)

    def invoke(self):
        with contextlib.redirect_stdout(self.stdout):
            code = HOLDOUT.main()
        return code, json.loads(self.stdout.getvalue())

    def assert_cleaned(self):
        self.assertIsNotNone(self.scratch)
        self.assertFalse(self.scratch.exists())

    def test_success_uses_fixed_helper_and_cleans_owned_scratch(self):
        self.process.return_value = self.result()
        code, report = self.invoke()
        self.assertEqual(code, 0)
        self.assertEqual(report["schema_id"], "contextlattice_native_containment_holdouts.v1")
        self.assertIs(report["ok"], True)
        self.assertIs(report["artifact_audit"], False)
        self.assertEqual(report["host_namespaces"], self.host)
        self.assertEqual(report["child"]["namespaces"], self.child)
        self.audit.assert_called_once_with(str(ROOT / "scripts/agent/audit-public-installer-package"),
                                           run_name="native_holdouts_import")
        self.builder.assert_called_once()
        self.process.assert_called_once_with(self.command, stdin=subprocess.DEVNULL, capture_output=True,
                                             timeout=20, check=False, env={"PATH": "/usr/bin:/bin"})
        self.assert_cleaned()

    def test_root_or_missing_proc_refuses_before_tool_import(self):
        for root, proc in ((True, True), (False, False)):
            with self.subTest(root=root, proc=proc), \
                    mock.patch.object(HOLDOUT.os, "geteuid", return_value=0 if root else 501), \
                    mock.patch.object(Path, "is_dir", return_value=proc), \
                    contextlib.redirect_stdout(io.StringIO()) as output:
                self.assertEqual(HOLDOUT.main(), 1)
                report = json.loads(output.getvalue())
                self.assertEqual(report["error"], "requires_unprivileged_linux")
                self.assertIs(report["ok"], False)
                self.assertIs(report["artifact_audit"], False)
        self.audit.assert_not_called()
        self.process.assert_not_called()

    def test_isolation_or_readonly_failure_is_not_success(self):
        for kind in ("child_false", "namespace", "sentinel"):
            with self.subTest(kind=kind):
                self.stdout = io.StringIO()
                def run(*_args, **_kwargs):
                    child = {"ok": kind != "child_false", "namespaces": dict(self.child)}
                    if kind == "namespace":
                        child["namespaces"]["net"] = self.host["net"]
                    if kind == "sentinel":
                        (self.scratch / "sentinel").write_bytes(b"changed")
                    return self.result(child=child)
                self.process.side_effect = run
                code, report = self.invoke()
                self.assertEqual(code, 1)
                self.assertIs(report["ok"], False)
                self.assertIs(report["artifact_audit"], False)
                self.assert_cleaned()

    def test_nonzero_or_oversized_output_fails_closed(self):
        for result in (self.result(code=2, stderr=b"denied" * 600), self.result(stdout=b"x" * 8193)):
            with self.subTest(returncode=result.returncode):
                self.stdout = io.StringIO()
                self.process.return_value = result
                code, report = self.invoke()
                self.assertEqual(code, 1)
                self.assertIs(report["ok"], False)
                self.assertNotIn("child", report)
                self.assertLessEqual(len(report["stderr"]), 2048)
                self.assert_cleaned()

    def test_missing_tool_and_timeout_propagate_after_cleanup(self):
        for error in (FileNotFoundError("mock tool missing"), subprocess.TimeoutExpired(self.command, 20)):
            with self.subTest(error=type(error).__name__):
                self.process.side_effect = error
                with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(type(error)):
                    HOLDOUT.main()
                self.assert_cleaned()

    def test_invalid_child_output_propagates_after_cleanup(self):
        for stdout, error in ((b"not json", json.JSONDecodeError), (b'{"ok": true}', KeyError)):
            with self.subTest(error=error.__name__):
                self.process.return_value = self.result(stdout=stdout)
                with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(error):
                    HOLDOUT.main()
                self.assert_cleaned()

    def test_helper_fake_success_with_denied_or_created_nested_namespace(self):
        for unshare_result in ((-1, errno.EPERM), (-1, errno.EACCES), (0, 0)):
            with self.subTest(unshare_result=unshare_result):
                error, output, events = execute_helper_with_fakes(unshare_result=unshare_result)
                self.assertIsNone(error)
                self.assertEqual(len(output), 1)
                report = json.loads(output[0])
                self.assertIs(report["ok"], True)
                self.assertIs(report["nested_namespace_created"], unshare_result[0] == 0)
                self.assertEqual(report["namespaces"], {
                    name: f"{name}:[synthetic]" for name in ("user", "mnt", "net", "pid")
                })
                self.assertIn(("write_bytes", "/input/contextlattice.msi", b"must not be writable"), events)
                self.assertIn(("write_text", "/output/owned-scratch", "writable only here"), events)
                self.assertIn(("CDLL", None, True), events)
                self.assertIn(("unshare", 0x10000000 | 0x04000000), events)
                self.assertEqual(sum(event[0] == "sethostname" for event in events),
                                 2 if unshare_result[0] == 0 else 1)
                calls = [event for event in events if event[0] == "subprocess"]
                self.assertEqual(len(calls), 1)
                self.assertEqual(calls[0][1], ["/synthetic/python3", "-I", "-c",
                                              "import pathlib; print(pathlib.Path('/proc/self/attr/current').read_text().strip())"])
                self.assertEqual(calls[0][2], {"capture_output": True, "check": True, "timeout": 5})

    def test_helper_fake_security_negative_controls(self):
        zeros = {"CapEff": "0", "CapPrm": "0", "CapAmb": "0"}
        cases = [(name, {"caps": {**zeros, name: "1"}}) for name in zeros]
        cases.extend((
            ("parent_profile", {"label": "other (enforce)"}),
            ("parent_mode", {"label": "unpriv_bwrap (complain)"}),
            ("descendant_profile", {"descendant": "other (enforce)"}),
            ("descendant_mode", {"descendant": "unpriv_bwrap (complain)"}),
            ("network_interface", {"interfaces": ["lo", "eth0"]}),
            ("host_route", {"routes": "header\nnon-loopback-route\n"}),
            ("input_writable", {"input_writable": True}),
            ("input_unexpected_errno", {"input_errno": errno.EINVAL}),
            ("hostname_succeeded", {"hostname_results": [(0, 0)]}),
            ("hostname_wrong_errno", {"hostname_results": [(-1, errno.EACCES)]}),
            ("nested_unshare_wrong_errno", {"unshare_result": (-1, errno.EINVAL)}),
            ("nested_hostname_succeeded", {"unshare_result": (0, 0),
                                           "hostname_results": [(-1, errno.EPERM), (0, 0)]}),
            ("nested_hostname_wrong_errno", {"unshare_result": (0, 0),
                                             "hostname_results": [(-1, errno.EPERM), (-1, errno.EACCES)]}),
        ))
        for name, overrides in cases:
            with self.subTest(case=name):
                error, output, _events = execute_helper_with_fakes(**overrides)
                self.assertIs(type(error), AssertionError)
                self.assertEqual(output, [])


if __name__ == "__main__":
    unittest.main()
