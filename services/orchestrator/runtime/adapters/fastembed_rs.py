from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import httpx

from .base import EmbeddingProviderAdapter, EmbeddingRequest, EmbeddingResponse


@dataclass(slots=True)
class FastembedRsConfig:
    base_url: str
    timeout_secs: float = 2.5
    model: str | None = None
    route: str = "/embed"


class FastembedRsEmbeddingAdapter(EmbeddingProviderAdapter):
    name = "fastembed-rs"

    def __init__(self, config: FastembedRsConfig):
        self._config = config

    @staticmethod
    def _parse_vectors(payload: Any) -> list[list[float]]:
        if not isinstance(payload, dict):
            raise ValueError("fastembed-rs response is not a JSON object")
        candidates = payload.get("vectors")
        if not isinstance(candidates, list):
            candidates = payload.get("embeddings")
        if not isinstance(candidates, list):
            data = payload.get("data")
            if isinstance(data, list):
                parsed: list[list[float]] = []
                for item in data:
                    if isinstance(item, dict) and isinstance(item.get("embedding"), list):
                        parsed.append([float(value) for value in item["embedding"]])
                if parsed:
                    return parsed
            raise ValueError("fastembed-rs response missing vectors/embeddings payload")
        vectors: list[list[float]] = []
        for row in candidates:
            if not isinstance(row, list):
                continue
            vectors.append([float(value) for value in row])
        if not vectors:
            raise ValueError("fastembed-rs returned zero vectors")
        return vectors

    async def embed(self, request: EmbeddingRequest) -> EmbeddingResponse:
        payload: dict[str, Any] = {"input": list(request.texts)}
        model = (request.model or self._config.model or "").strip()
        if model:
            payload["model"] = model
        route = self._config.route.strip() or "/embed"
        if not route.startswith("/"):
            route = "/" + route
        url = self._config.base_url.rstrip("/") + route
        async with httpx.AsyncClient(timeout=self._config.timeout_secs) as client:
            response = await client.post(url, json=payload)
            response.raise_for_status()
            parsed = response.json()
        vectors = self._parse_vectors(parsed)
        if len(vectors) < len(request.texts):
            raise ValueError(
                f"fastembed-rs returned {len(vectors)} vectors for {len(request.texts)} inputs"
            )
        return EmbeddingResponse(vectors=vectors[: len(request.texts)], provider=self.name)
