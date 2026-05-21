import os, re, uvicorn, httpx
from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from starlette.routing import Mount

OLLAMA = os.getenv("OLLAMA_API_BASE", "http://127.0.0.1:11434/v1").rstrip("/")
MLX    = os.getenv("MLX_API_BASE",    "http://127.0.0.1:18087/v1").rstrip("/")

app = FastAPI(title="OpenAI Router (Ollama+MLX)")

async def list_models(base: str):
    try:
        async with httpx.AsyncClient(timeout=30) as c:
            r = await c.get(f"{base}/models")
            r.raise_for_status()
            return r.json()
    except Exception:
        return {"data": []}

def merge_models(*payloads):
    seen, merged = set(), []
    for p in payloads:
        for m in p.get("data", []):
            mid = m.get("id") or m.get("model") or m.get("name")
            if mid and mid not in seen:
                seen.add(mid)
                merged.append({"id": mid})
    return {"object":"list","data":merged}

def choose_backend(model: str) -> str:
    # heuristic 1: file hints
    s = model.strip()
    if s.endswith(".gguf") or ".gguf" in s:   # GGUF => Ollama land
        return OLLAMA
    if s.endswith(".safetensors") or "mlx" in s.lower() or "safetensors" in s.lower():
        return MLX

    # heuristic 2: query both /models and see who knows it
    # cache cheaply by simple fetch each time (fast on localhost)
    # prefer exact match; fallback to prefix match
    import anyio
    async def _probe():
        om = await list_models(OLLAMA)
        mm = await list_models(MLX)
        oset = {x.get("id","") for x in om.get("data",[])}
        mset = {x.get("id","") for x in mm.get("data",[])}
        if model in mset: return MLX
        if model in oset: return OLLAMA
        # loose contains
        if any(model in x for x in mset): return MLX
        if any(model in x for x in oset): return OLLAMA
        # default to Ollama (more common)
        return OLLAMA
    return anyio.run(_probe)

@app.get("/v1/models")
async def models():
    om, mm = await list_models(OLLAMA), await list_models(MLX)
    return JSONResponse(merge_models(om, mm))

@app.post("/v1/chat/completions")
async def chat(req: Request):
    body = await req.body()
    jb   = await req.json()
    model = (jb.get("model") or "").strip()
    base  = choose_backend(model)
    async with httpx.AsyncClient(timeout=None) as c:
        r = await c.post(f"{base}/chat/completions", content=body, headers={"Content-Type":"application/json"})
        return Response(content=r.content, status_code=r.status_code, headers={"Content-Type": r.headers.get("content-type","application/json")})

# Basic pass-throughs for other common OpenAI endpoints
@app.post("/v1/completions")
async def completions(req: Request):
    body = await req.body()
    jb   = await req.json()
    model = (jb.get("model") or "").strip()
    base  = choose_backend(model)
    async with httpx.AsyncClient(timeout=None) as c:
        r = await c.post(f"{base}/completions", content=body, headers={"Content-Type":"application/json"})
        return Response(content=r.content, status_code=r.status_code, headers={"Content-Type": r.headers.get("content-type","application/json")})

@app.post("/v1/embeddings")
async def embeddings(req: Request):
    body = await req.body()
    jb   = await req.json()
    model = (jb.get("model") or "").strip()
    base  = choose_backend(model)
    async with httpx.AsyncClient(timeout=None) as c:
        r = await c.post(f"{base}/embeddings", content=body, headers={"Content-Type":"application/json"})
        return Response(content=r.content, status_code=r.status_code, headers={"Content-Type": r.headers.get("content-type","application/json")})

if __name__ == "__main__":
    # non-standard high port to avoid collisions
    uvicorn.run("openai_router:app", host="127.0.0.1", port=18123, reload=False)
