"""Offline native selection/dispatch probes. No model, keychain or live gateway required."""

import argparse
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import queue
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time


KIT = Path(__file__).resolve().parents[1]
GUIDE = KIT / "plugins/contextlattice/skills/contextlattice-recall/SKILL.md"
ID = "contextlattice@contextlattice-local"
MARKET = "contextlattice-local"
NAME = "contextlattice-recall"


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def proof(host, root, project, bridge=None, digest=None, success_fixture=False):
    home, cwd, tmp = [root / n for n in ("home", "work", "tmp")]
    for path in (home, cwd, tmp, home / ".codex"):
        path.mkdir()
    received, calls = [], []
    class Holdout(BaseHTTPRequestHandler):
        def do_POST(self):
            data = self.rfile.read(int(self.headers.get("Content-Length", "0")))
            route, query = self.path.split("?")[0], json.loads(data)
            received.append((route, query))
            if success_fixture and route == "/memory/search" and query.get("project") == project:
                hit = {"project": project, "file": "receipt.md", "text": "Scoped synthetic receipt.",
                       "summary": "Scoped synthetic receipt.", "score": 1.0}
                body = json.dumps({"ok": True, "project": project, "results": [hit]}).encode()
                self.send_response(200)
            else:
                body = b'{"type":"error","error":{"type":"invalid_request_error","message":"offline holdout"}}'
                self.send_response(400)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            pass

    with HTTPServer(("127.0.0.1", 0), Holdout) as server:
        threading.Thread(target=server.serve_forever, daemon=True).start()
        port = server.server_port
        address = f"http://127.0.0.1:{port}"
        env = {"PATH": os.environ["PATH"], "HOME": str(home), "SHELL": "/bin/zsh",
               "TMPDIR": str(tmp), "CLAUDE_CODE_TMPDIR": str(tmp), "XDG_CONFIG_HOME": str(home / ".config"),
               "XDG_CACHE_HOME": str(home / ".cache"), "XDG_STATE_HOME": str(home / ".state"),
               "XDG_DATA_HOME": str(home / ".data"), "GIT_CEILING_DIRECTORIES": str(root),
               "CODEX_HOME": str(home / ".codex"), "CLAUDE_CONFIG_DIR": str(home / ".claude"),
               "CONTEXTLATTICE_ORCHESTRATOR_URL": address, "MEMMCP_ORCHESTRATOR_URL": address,
               "CI": "1", "NO_COLOR": "1", "DO_NOT_TRACK": "1",
               "DISABLE_AUTOUPDATER": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}
        # Refuse external traffic at the OS boundary, not just by overriding a URL.
        # Only the new test directory is writable. No auth variables are inherited.
        policy = ('(version 1)(allow default)(deny file-write*)(deny network-outbound)'
                  '(deny process-exec (regex #"/bash$"))'
                  f'(allow network-outbound (remote tcp "localhost:{port}"))'
                  f'(allow file-write* (subpath {json.dumps(str(root))}) (literal "/dev/null") (literal "/dev/tty"))')
        sandbox = shutil.which("sandbox-exec")
        require(sandbox, "requires macOS sandbox-exec; not running unrestricted")
        prefix = [sandbox, "-p", policy]
        if host == "codex-mcp":
            require(bridge and digest and hashlib.sha256(bridge.read_bytes()).hexdigest() == digest,
                    "explicit bridge executable and matching SHA-256 required")
            bindir = root / "bin"
            bindir.mkdir()
            (bindir / "contextlattice").symlink_to(bridge.resolve(strict=True))
            env["PATH"] = str(bindir) + os.pathsep + env["PATH"]
            env["CONTEXTLATTICE_API_KEY"] = "offline-test-only"
        binary = shutil.which("codex" if host == "codex-mcp" else host)
        require(binary, "missing host binary")

        def run(*args, success=True):
            p = subprocess.run([*prefix, binary, *map(str, args)], cwd=cwd, env=env,
                               capture_output=True, text=True, timeout=40, check=False)
            calls.append({"action": " ".join(str(x) for x in args[:2]) if args else "", "exit": p.returncode})
            require(not success or p.returncode == 0, f"{host} {args[0]} failed (exit {p.returncode})")
            return p

        start = time.monotonic()
        try:
            if host == "codex-mcp":
                setting = subprocess.check_output([sys.executable, "-B", str(KIT / "project_mcp.py"),
                                                   "codex", "--project", project], text=True).strip()
                receipt = codex_dispatch(prefix, binary, env, cwd, received, project, setting, success_fixture)
                return {"host": host, "bridge_sha256": digest, "paid_calls": 0,
                        "elapsed_ms": round((time.monotonic() - start) * 1000), **receipt}
            run("plugin", "marketplace", "add", KIT)
            if host == "claude":
                run("plugin", "install", ID)
                env.update(ANTHROPIC_BASE_URL=address, ANTHROPIC_API_KEY="offline-test-only",
                           ANTHROPIC_MAX_RETRIES="0")
                # Slash selection is real. The HTTP fixture observes the expanded
                # request then refuses it; there is deliberately no model response.
                p = run("-p", f"/contextlattice:{NAME}", "--tools", "", "--strict-mcp-config",
                        "--no-session-persistence", success=False)
                requests = [d for route, d in received if route == "/v1/messages"]
                expanded = [d for d in requests if any(
                    "Do not guess a project" in str(m) and "--no-auto-session" in str(m)
                    for m in d.get("messages", []))]
                require(expanded and p.returncode != 0,
                        "skill expansion unproven; exit=%s messages=%s output=%s" %
                        (p.returncode, len(requests), (p.stdout + p.stderr)[:240].replace(str(root), "<fixture>")))
                result = {"selection": "native-slash-expanded-in-model-input", "provider": "fixture-rejected",
                          "cli_dispatch": False}
                run("plugin", "uninstall", ID)
            else:
                run("plugin", "add", ID)
                result = codex_dispatch(prefix, binary, env, cwd, received, project)
                run("plugin", "remove", ID)
            run("plugin", "marketplace", "remove", MARKET)
            return {"host": host, "elapsed_ms": round((time.monotonic() - start) * 1000),
                    "native_calls": calls, "guide_sha256": hashlib.sha256(GUIDE.read_bytes()).hexdigest(),
                    "requests": len(received), "paid_calls": 0, **result}
        finally:
            server.shutdown()


def codex_dispatch(prefix, binary, env, cwd, received, project, setting=None, success_fixture=False):
    overrides = ["-c", setting] if setting else []
    child = subprocess.Popen([*prefix, binary, *overrides, "app-server", "--listen", "stdio://"],
                             cwd=cwd, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, text=True)
    replies = queue.Queue()
    def read():
        for line in child.stdout:
            replies.put(json.loads(line))
        replies.put(None)
    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    counter = 0
    def rpc(method, params, *, accept_rejection=False):
        nonlocal counter
        counter += 1
        msg = {"id": counter, "method": method, "params": params}
        child.stdin.write(json.dumps(msg) + "\n")
        child.stdin.flush()
        while True:
            row = replies.get(timeout=15)
            require(row is not None, "native app-server closed early")
            if row.get("id") == counter:
                if accept_rejection and "error" in row:
                    return {"rpcRejection": row["error"]}
                require("result" in row, "native app-server rejected " + method)
                return row["result"]
    try:
        rpc("initialize", {"clientInfo": {"name": "local_qualification", "version": "1"},
                           "capabilities": {"experimentalApi": True}})
        if setting:
            thread = rpc("thread/start", {"cwd": str(cwd), "ephemeral": True, "approvalPolicy": "never"})
            status = rpc("mcpServerStatus/list", {})
            servers = [r for r in status["data"] if r["name"] == "contextlattice"]
            require(len(servers) == 1 and set(servers[0]["tools"]) == {"contextlattice_context"},
                    "native MCP inventory did not return the single recall tool")
            tool = servers[0]["tools"]["contextlattice_context"]
            require(set(tool["inputSchema"]["properties"]) == {"query"}, "caller can override more than query")
            args = {"threadId": thread["thread"]["id"], "server": "contextlattice", "tool": "contextlattice_context"}
            task = "offline scoped host bridge qualification"
            response = rpc("mcpServer/tool/call", {**args, "arguments": {"query": task}})
            require(len(received) == 1 and received[0][1].get("project") == project and
                    received[0][1].get("query") == task and bool(response.get("isError")) != success_fixture,
                    "scoped call unproven: requests=%s routes=%s tool_error=%s" %
                    (len(received), [r for r, _ in received], response.get("isError")))
            if success_fixture:
                require("Scoped synthetic receipt." in json.dumps(response["content"]),
                        "no synthetic witness in native result: " + str(response["content"])[:400])
                return {"surface": "host-native-mcp-client", "tools": ["contextlattice_context"],
                        "scoped_call": True, "gateway": "synthetic-success", "returned_witness": True,
                        "gateway_route": received[0][0], "gateway_requests": 1, "rpc_calls": counter, "model_turns": 0}
            override = rpc("mcpServer/tool/call", {**args, "arguments": {"query": task, "project": project + "-other"}},
                           accept_rejection=True)
            rejected = override.get("isError") is True or "rpcRejection" in override
            require(rejected and len(received) == 1, "project override did not fail closed")
            return {"surface": "host-native-mcp-client", "tools": ["contextlattice_context"],
                    "scoped_call": True, "gateway_route": received[0][0], "gateway": "fixture-rejected",
                    "project_override": "rejected-before-gateway", "gateway_requests": 1, "rpc_calls": counter,
                    "model_turns": 0}
        rows = rpc("skills/list", {"cwds": [str(cwd)], "forceReload": True})["data"][0]["skills"]
        selected = [r for r in rows if r["name"].split(":")[-1] == NAME and r["enabled"]]
        require(len(selected) == 1, "expected exactly one enabled plugin skill")
        raw = rpc("fs/readFile", {"path": selected[0]["path"]})
        guide = base64.b64decode(raw["dataBase64"])
        require(guide == GUIDE.read_bytes(), "native selection returned different bytes")
        line, = [s for s in guide.decode().splitlines() if s.startswith("contextlattice context ")]
        argv = shlex.split(line)
        require(argv == ["contextlattice", "context", "$task", "--project", "$project", "--no-auto-session"],
                "refusing unexpected selected command")
        task = "offline qualification; preserve as one argument"
        cli = shutil.which(argv[0])
        require(cli and project, "installed ContextLattice CLI and explicit --project required")
        argv[0], argv[2], argv[4] = cli, task, project
        result = rpc("command/exec", {"command": argv, "cwd": str(cwd), "timeoutMs": 10000,
                     "sandboxPolicy": {"type": "externalSandbox", "networkAccess": "restricted"},
                     "env": {k: env[k] for k in ("HOME", "CONTEXTLATTICE_ORCHESTRATOR_URL", "MEMMCP_ORCHESTRATOR_URL")}})
        require(len(received) == 1, "expected one scoped gateway request and no session creation")
        route, body = received[0]
        require(route == "/memory/synthesis-pack/v2" and body.get("project") == project and
                body.get("query") == task and result["exitCode"] != 0, "missing scoped CLI/refusal witness")
        return {"selection": "native-skills-list-and-read", "cli_dispatch": True,
                "dispatch": "explicit-app-server-argv", "gateway": "fixture-rejected",
                "rpc_calls": counter, "model_turns": 0}
    finally:
        child.stdin.close()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.terminate()
            child.wait(timeout=5)
        reader.join(timeout=2)
        child.stdout.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True, choices=("codex", "claude", "codex-mcp"))
    parser.add_argument("--scratch-root", required=True, type=Path)
    parser.add_argument("--project", help="caller-selected project for the loopback Codex fixture")
    parser.add_argument("--bridge-binary", type=Path, help="reviewed installed stdio bridge for codex-mcp")
    parser.add_argument("--bridge-sha256", help="expected SHA-256 of that exact executable")
    parser.add_argument("--success-fixture", action="store_true", help="return one synthetic project-scoped MCP result")
    args = parser.parse_args()
    if args.host.startswith("codex") and not args.project:
        parser.error("Codex CLI dispatch requires an explicit --project")
    if args.success_fixture and args.host != "codex-mcp":
        parser.error("the success fixture is only for the project MCP client")
    with tempfile.TemporaryDirectory(prefix="selection-", dir=args.scratch_root.resolve(strict=True)) as d:
        print(json.dumps(proof(args.host, Path(d), args.project, args.bridge_binary,
                               args.bridge_sha256, args.success_fixture), sort_keys=True))
