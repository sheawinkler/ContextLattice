from fastmcp import FastMCP
from fastmcp.server.proxy import ProxyClient
import os
upstream = os.environ.get("MINDSDB_SSE_URL", "http://mindsdb:47334/mcp/sse")
proxy = FastMCP.as_proxy(ProxyClient(upstream), name="MindsDB-HTTP-Proxy")
if __name__ == "__main__":
    proxy.run(transport="http", host="0.0.0.0", port=8004)
