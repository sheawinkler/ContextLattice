import os, httpx
from typing import Optional, Dict, Any, List
from pydantic import BaseModel, Field
from fastmcp import FastMCP
from fastembed import TextEmbedding

# Config
QDRANT_URL   = os.getenv("QDRANT_URL", "http://qdrant:6333")
COLLECTION   = os.getenv("COLLECTION_NAME", "memmcp_notes")
ADV_PORT     = int(os.getenv("QDRANT_ADV_PORT", "8022"))
EMB_MODEL    = os.getenv("EMBEDDING_MODEL_QDRANT", "sentence-transformers/all-MiniLM-L6-v2")
EF_DEFAULT   = int(os.getenv("QDRANT_RUNTIME_EF_DEFAULT", "128"))
EXACT_DFLT   = os.getenv("QDRANT_SEARCH_EXACT_DEFAULT", "false").lower() == "true"

# Embedder (FastEmbed is light & CPU-friendly)
# Docs: https://qdrant.github.io/fastembed/Getting%20Started/
embedder = TextEmbedding(model_name=EMB_MODEL)

mcp = FastMCP("Qdrant ADV")

class FindArgs(BaseModel):
    query: str = Field(..., description="Natural language query to embed & search")
    limit: int = Field(10, ge=1, le=200, description="Top-K results")
    hnsw_ef: Optional[int] = Field(None, ge=1, description="Override ef at query-time")
    exact: Optional[bool] = Field(None, description="Exact KNN (full scan) if true")
    filter: Optional[Dict[str, Any]] = Field(None, description="Qdrant filter JSON")
    with_payload: bool = True
    with_vector: bool = False

@mcp.tool(name="qdrant-find-adv")
def qdrant_find_adv(args: FindArgs) -> Dict[str, Any]:
    # Embed the query
    vec = list(embedder.embed([args.query]))[0].tolist()

    # Build search body with runtime params (ef/exact)
    params = {
        "hnsw_ef": args.hnsw_ef if args.hnsw_ef is not None else EF_DEFAULT,
        "exact": args.exact if args.exact is not None else EXACT_DFLT,
    }
    body: Dict[str, Any] = {
        "vector": vec,
        "limit": args.limit,
        "params": params,             # Qdrant supports hnsw_ef & exact here
        "with_payload": args.with_payload,
        "with_vector": args.with_vector,
    }
    if args.filter:
        body["filter"] = args.filter

    # POST /collections/{collection}/points/search
    # Qdrant search params doc: https://qdrant.tech/documentation/concepts/search/
    r = httpx.post(f"{QDRANT_URL}/collections/{COLLECTION}/points/search",
                   json=body, timeout=90)
    r.raise_for_status()
    return r.json()

if __name__ == "__main__":
    # Streamable HTTP serves the MCP endpoint at /mcp
    # Doc shows the HTTP transport mounts at /mcp by default
    # https://gofastmcp.com/deployment/running-server
    mcp.run(transport="streamable-http", host="0.0.0.0", port=ADV_PORT)
