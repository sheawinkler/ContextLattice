from __future__ import annotations

import os
import threading
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

try:
    from fastembed import TextEmbedding
except Exception as exc:  # pragma: no cover
    TextEmbedding = None  # type: ignore
    _IMPORT_ERROR = exc
else:
    _IMPORT_ERROR = None


DEFAULT_MODEL = os.getenv("FASTEMBED_DEFAULT_MODEL", "BAAI/bge-small-en-v1.5").strip() or "BAAI/bge-small-en-v1.5"
CACHE_DIR = os.getenv("FASTEMBED_CACHE_DIR", "/models").strip() or "/models"
MAX_BATCH = max(1, int(os.getenv("FASTEMBED_MAX_BATCH", "256") or "256"))


class EmbedRequest(BaseModel):
    input: list[str] | str = Field(..., description="Input text(s)")
    model: str | None = Field(default=None)


class EmbedResponse(BaseModel):
    vectors: list[list[float]]
    model: str


app = FastAPI(title="fastembed-sidecar", version="1.0.0")
_model_lock = threading.Lock()
_models: dict[str, Any] = {}


def _normalize_inputs(value: list[str] | str) -> list[str]:
    if isinstance(value, str):
        value = [value]
    rows: list[str] = []
    for item in value:
        text = str(item or "").strip()
        if not text:
            continue
        rows.append(text)
        if len(rows) >= MAX_BATCH:
            break
    if not rows:
        raise HTTPException(status_code=422, detail="input must include at least one non-empty string")
    return rows


def _resolve_model_name(requested: str | None) -> str:
    token = str(requested or "").strip()
    return token or DEFAULT_MODEL


def _get_model(model_name: str):
    if TextEmbedding is None:
        raise RuntimeError(f"fastembed import failed: {_IMPORT_ERROR}")
    with _model_lock:
        model = _models.get(model_name)
        if model is not None:
            return model
        model = TextEmbedding(model_name=model_name, cache_dir=CACHE_DIR)
        _models[model_name] = model
        return model


@app.get("/health")
def health() -> dict[str, Any]:
    return {
        "ok": True,
        "defaultModel": DEFAULT_MODEL,
        "cacheDir": CACHE_DIR,
        "loadedModels": sorted(_models.keys()),
    }


@app.post("/embed", response_model=EmbedResponse)
def embed(payload: EmbedRequest) -> EmbedResponse:
    model_name = _resolve_model_name(payload.model)
    inputs = _normalize_inputs(payload.input)
    try:
        model = _get_model(model_name)
        vectors: list[list[float]] = []
        for row in model.embed(inputs):
            if hasattr(row, "tolist"):
                values = row.tolist()
            else:
                values = list(row)
            vectors.append([float(item) for item in values])
    except HTTPException:
        raise
    except Exception as exc:
        raise HTTPException(status_code=500, detail=f"embedding failed: {exc}") from exc
    if len(vectors) < len(inputs):
        raise HTTPException(
            status_code=500,
            detail=f"embedding returned {len(vectors)} vectors for {len(inputs)} inputs",
        )
    return EmbedResponse(vectors=vectors[: len(inputs)], model=model_name)
