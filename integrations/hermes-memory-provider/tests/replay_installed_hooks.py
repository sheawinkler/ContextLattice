#!/usr/bin/env python3
"""Zero-model replay of installed Hermes hook APIs, with isolated state and fake HTTP.

This does not launch an agent, discover tools, use credentials, or prove model
consumption. Run with the installed harness's Python and --harness-root.
"""
from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
from unittest.mock import patch


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--harness-root", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    args = parser.parse_args()
    root = args.harness_root.resolve(strict=True)
    plugin_path = Path(__file__).resolve().parents[1] / "__init__.py"
    receipt = {"schema_id": "hermes_hook_replay.v1", "ok": False,
               "scope": "installed hook API replay; fake HTTP and lifecycle routing",
               "model_calls": 0, "independent_task_verification": False,
               "python": sys.version.split()[0], "source_sha256": {}}
    for path in (plugin_path, Path(__file__), root / "hermes_cli/plugins.py",
                 root / "model_tools.py", root / "agent/memory_provider.py"):
        receipt["source_sha256"][path.name if path.is_relative_to(plugin_path.parent) else
                                  str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    try:
        with tempfile.TemporaryDirectory(prefix="cl-hermes-hook-replay-") as home, \
                patch.dict(os.environ, {"HERMES_HOME": home, "HERMES_ENABLE_PROJECT_PLUGINS": "false"}), \
                patch.object(socket.socket, "connect", side_effect=RuntimeError("network_disabled")), \
                patch.object(socket.socket, "connect_ex", side_effect=RuntimeError("network_disabled")):
            sys.path.insert(0, str(root))
            from hermes_cli import lifecycle, plugins
            import tools.registry
            with patch.object(tools.registry, "discover_builtin_tools"):
                import model_tools
            spec = importlib.util.spec_from_file_location("cl_installed_hook_probe", plugin_path)
            plugin = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(plugin)
            with patch.object(plugin, "_load_config", return_value=dict(plugin._DEFAULTS)):
                provider = plugin.ContextLatticeProvider()
            provider._key = "isolated-fixture-not-a-credential"
            provider._session_id = "host-session"
            calls = []

            def http(path, body, **_):
                calls.append((path, body))
                if path.endswith("/start"):
                    return {"ok": True, "session": dict(body, id="sess_hook_fixture")}
                if path == "/memory/context-pack":
                    return {"reference_prompt": "fixture evidence", "context_pack": {"project": body["project"]},
                            "context_pack_quality": {"sample_id": body["task_id"]}}
                if path.endswith("/events"):
                    return {"ok": True}
                raise AssertionError("unexpected_http_surface")

            def invoke(name, **kw):
                handler = {"pre_tool_call": provider._on_pre_tool_call,
                           "post_tool_call": provider._on_post_tool_call}[name]
                return [handler(**kw)]

            with patch.object(provider, "_post", side_effect=http), \
                    patch.object(lifecycle, "invoke_hook", side_effect=invoke), \
                    patch.object(lifecycle, "has_hook", return_value=True):
                injected = {}
                for call_id in ("call-a", "call-b"):
                    blocked, updated = plugins._dispatch_pre_tool_call_hooks(
                        "delegate_task", {"goal": "identical goal", "context": "existing context"},
                        session_id="host-session", task_id="host-task", tool_call_id=call_id,
                        turn_id="turn-a", api_request_id="request-a")
                    assert blocked is None and updated["goal"] == "identical goal"
                    assert "fixture evidence" in updated["context"]
                    injected[call_id] = updated
                for call_id, status in (("call-b", "failed"), ("call-a", "completed")):
                    model_tools._emit_post_tool_call_hook(
                        function_name="delegate_task", function_args=injected[call_id],
                        result=json.dumps({"results": [{"task_index": 0, "status": status}]}),
                        session_id="host-session", task_id="host-task", tool_call_id=call_id,
                        turn_id="turn-a", api_request_id="request-a", status="ok")
                provider.shutdown()
            rows = [body["metadata"] for path, body in calls if path.endswith("/events")]
            packs = [body for path, body in calls if path == "/memory/context-pack"]
            assert len(rows) == 2 and len(packs) == 2
            assert [(row["task_id"], row["host_reported_status"]) for row in rows] == [
                (packs[1]["task_id"], "failed"), (packs[0]["task_id"], "completed")]
            assert all(row["host_status"] == "unknown" and not row["dispatch_observed"]
                       and not row["delivery_confirmed"] for row in rows)
            assert all(row["post_hook_context_match"] and not row["independent_verification"] for row in rows)
            assert all(row["session_id"] == "sess_hook_fixture" for row in rows)
            assert not any(path == "/telemetry/context-pack-quality/outcome" for path, _ in calls)
            receipt.update(ok=True, cases=["installed_pre_hook_shallow_merge", "installed_post_hook_ids",
                                          "reverse_identical_goals", "exact_context_digest",
                                          "no_inferred_first_pass_success"], observations=rows)
    except Exception as exc:
        receipt["error_class"] = type(exc).__name__
    with args.receipt.open("x", encoding="utf-8") as stream:
        json.dump(receipt, stream, indent=2)
        stream.write("\n")
    print(json.dumps({"ok": receipt["ok"], "error_class": receipt.get("error_class"),
                      "cases": receipt.get("cases", [])}))
    return 0 if receipt["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
