#!/usr/bin/env python3
import os, json, sys, argparse
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError

def gateway_url():
    # Prefer host URL when running from your Mac shell; fall back to in-network
    return (os.getenv("GATEWAY_URL_HOST") or os.getenv("GATEWAY_URL") or "http://127.0.0.1:8010").rstrip("/")

def post_json(path, payload):
    url = gateway_url() + path
    data = json.dumps(payload).encode("utf-8")
    req = Request(url, data=data, headers={"Content-Type":"application/json"}, method="POST")
    try:
        with urlopen(req, timeout=30) as resp:
            body = resp.read()
            ctype = resp.headers.get("Content-Type","")
            if "application/json" in ctype:
                return json.loads(body.decode("utf-8"))
            return {"status": resp.status, "text": body.decode("utf-8", "ignore")[:500]}
    except HTTPError as e:
        text = e.read().decode("utf-8", "ignore")
        sys.stderr.write(f"Gateway returned {e.code}: {text}\n")
        sys.exit(2)
    except URLError as e:
        sys.stderr.write(f"Failed to reach gateway at {url}: {e}\n")
        sys.exit(2)

def deploy(payload):
    # LobeHub Gateway merges servers posted to /deploy under "mcpServers": {...}
    return post_json("/deploy", payload)

def add_url(name, url, env=None):
    server = {"url": url}
    if env: server["env"] = env
    return deploy({"mcpServers": {name: server}})

def add_stdio(name, command, args_list=None, env=None):
    server = {"command": command}
    if args_list: server["args"] = args_list
    if env: server["env"] = env
    return deploy({"mcpServers": {name: server}})

def add_bulk(path):
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    if "mcpServers" not in data or not isinstance(data["mcpServers"], dict):
        raise SystemExit('Bulk file must look like: {"mcpServers": {"name": {"url": "..." } | {"command": "...", "args":[...]}}}')
    return deploy(data)

def main():
    ap = argparse.ArgumentParser(description="Auto-register MCP servers into the gateway /deploy API")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p1 = sub.add_parser("add-url", help="Add a Streamable HTTP/SSE server by URL")
    p1.add_argument("name")
    p1.add_argument("url")
    p1.add_argument("--env", help="JSON of env overrides", default=None)

    p2 = sub.add_parser("add-stdio", help="Wrap a stdio server with gateway (command + args)")
    p2.add_argument("name")
    p2.add_argument("command")
    p2.add_argument("--args", help='JSON array, e.g. ["mcp-server-time","--tz","America/Denver"]', default=None)
    p2.add_argument("--env", help="JSON of env overrides", default=None)

    p3 = sub.add_parser("bulk", help='Deploy from a JSON file: {"mcpServers": {...}}')
    p3.add_argument("file")

    args = ap.parse_args()
    if args.cmd == "add-url":
        env = json.loads(args.env) if args.env else None
        print(json.dumps(add_url(args.name, args.url, env=env), indent=2))
    elif args.cmd == "add-stdio":
        env = json.loads(args.env) if args.env else None
        argv = json.loads(args.args) if args.args else None
        print(json.dumps(add_stdio(args.name, args.command, argv, env), indent=2))
    elif args.cmd == "bulk":
        print(json.dumps(add_bulk(args.file), indent=2))

if __name__ == "__main__":
    main()
