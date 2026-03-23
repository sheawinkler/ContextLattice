#!/usr/bin/env python3
"""Backward-compatible helper exports for legacy scripts."""

try:
    from scripts.contextlattice_client import (  # noqa: F401
        ContextLatticeClient,
        build_orchestrator_headers,
        create_orchestrator_client,
        create_worker_client,
        resolve_orchestrator_api_key,
    )
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from contextlattice_client import (  # type: ignore[no-redef]  # noqa: F401
        ContextLatticeClient,
        build_orchestrator_headers,
        create_orchestrator_client,
        create_worker_client,
        resolve_orchestrator_api_key,
    )
