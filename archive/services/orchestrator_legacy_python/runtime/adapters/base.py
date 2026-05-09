from __future__ import annotations

from dataclasses import dataclass
from typing import Protocol, Sequence


@dataclass(slots=True)
class EmbeddingRequest:
    texts: Sequence[str]
    model: str | None = None


@dataclass(slots=True)
class EmbeddingResponse:
    vectors: list[list[float]]
    provider: str


@dataclass(slots=True)
class ANNQueryRequest:
    vector: Sequence[float]
    limit: int
    filter: dict | None = None


@dataclass(slots=True)
class ANNQueryResponse:
    rows: list[dict]
    provider: str


class EmbeddingProviderAdapter(Protocol):
    name: str

    async def embed(self, request: EmbeddingRequest) -> EmbeddingResponse:
        ...


class ANNQueryAdapter(Protocol):
    name: str

    async def query(self, request: ANNQueryRequest) -> ANNQueryResponse:
        ...
