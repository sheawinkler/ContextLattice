#!/usr/bin/env python3
import os, sys, json, threading, subprocess
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

app = FastAPI()

class Bridge:
    def __init__(self):
        # Allow override; default to memory-bank MCP on stdio
        self.cmd = os.environ.get("MEMORYBANK_CMD", "npx -y @allpepper/memory-bank-mcp")
        self.lock = threading.Lock()
        self.child = None
        self.started = False

    def start(self):
        if self.started and self.child and self.child.poll() is None:
            return
        os.makedirs("logs", exist_ok=True)
        env = os.environ.copy()
        # sensible defaults if not provided by .env
        env.setdefault("MONGODB_URI", "mongodb://127.0.0.1:27017")
        env.setdefault("MEMORY_BANK_ROOT", os.path.abspath("data/memory-bank"))

        # Start the child via shell so 'npx' resolves on user's PATH
        self.child = subprocess.Popen(
            self.cmd,
            shell=True,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            bufsize=0,
            env=env,
        )

        def _drain_stderr():
            try:
                with open("logs/memorybank-http.err", "ab", buffering=0) as f:
                    while True:
                        line = self.child.stderr.readline()
                        if not line:
                            break
                        f.write(line)
            except Exception:
                pass

        threading.Thread(target=_drain_stderr, daemon=True).start()
        self.started = True

    def _read_message(self):
        """Read Content-Length framed JSON-RPC response from child stdout."""
        headers = {}
        # Read header lines until blank line
        while True:
            line = self.child.stdout.readline()
            if not line:
                return None
            if line in (b"\r\n", b"\n"):
                break
            if b":" not in line:
                # ignore noise/non-header lines
                continue
            k, v = line.split(b":", 1)
            headers[k.strip().lower()] = v.strip()
        try:
            length = int(headers.get(b"content-length", b"0"))
        except Exception:
            length = 0
        body = self.child.stdout.read(length) if length > 0 else b""
        return body

    def call(self, payload: dict):
        data = json.dumps(payload).encode("utf-8")
        with self.lock:
            # Write request
            self.child.stdin.write(b"Content-Length: " + str(len(data)).encode("ascii") + b"\r\n\r\n")
            self.child.stdin.write(data)
            self.child.stdin.flush()
            # Read response
            body = self._read_message()
            if not body:
                return {"jsonrpc": "2.0", "error": {"code": -32603, "message": "no response from child"}, "id": payload.get("id")}
            try:
                return json.loads(body.decode("utf-8"))
            except Exception:
                return {"jsonrpc": "2.0", "error": {"code": -32700, "message": "invalid JSON from child"}, "id": payload.get("id")}

bridge = Bridge()

@app.on_event("startup")
async def _startup():
    bridge.start()

@app.get("/health")
async def health():
    alive = bridge.child is not None and bridge.child.poll() is None
    return {"status":"ok", "child_alive": alive}

@app.post("/mcp")
async def mcp(request: Request):
    try:
        payload = await request.json()
    except Exception:
        return JSONResponse({"jsonrpc":"2.0","error":{"code":-32700,"message":"invalid request"},"id":None}, status_code=400)
    # (Re)start if the child died
    if not bridge.started or bridge.child is None or bridge.child.poll() is not None:
        bridge.start()
    resp = bridge.call(payload)
    return JSONResponse(resp)
