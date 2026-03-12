from __future__ import annotations

import os
from dataclasses import dataclass


def _env_bool(name: str, default: bool = False) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return str(raw).strip().lower() in {"1", "true", "yes", "on"}


@dataclass(slots=True)
class AdapterFlags:
    fastembed_rs_enabled: bool = False
    embedanything_enabled: bool = False
    edgevec_enabled: bool = False


def load_adapter_flags() -> AdapterFlags:
    return AdapterFlags(
        fastembed_rs_enabled=_env_bool("ORCH_ADAPTER_FASTEMBED_RS_ENABLED", False),
        embedanything_enabled=_env_bool("ORCH_ADAPTER_EMBEDANYTHING_ENABLED", False),
        edgevec_enabled=_env_bool("ORCH_ADAPTER_EDGEVEC_ENABLED", False),
    )


def adapter_flags_snapshot() -> dict[str, bool]:
    flags = load_adapter_flags()
    return {
        "fastembedRsEnabled": bool(flags.fastembed_rs_enabled),
        "embedAnythingEnabled": bool(flags.embedanything_enabled),
        "edgevecEnabled": bool(flags.edgevec_enabled),
    }
