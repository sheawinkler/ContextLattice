from __future__ import annotations

import os
from dataclasses import dataclass


def _env_bool(name: str, default: bool = False) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return str(raw).strip().lower() in {"1", "true", "yes", "on"}


@dataclass(slots=True)
class MigrationFlags:
    use_rust_codec: bool = False
    use_rust_memory: bool = False
    use_rust_retrieval: bool = False
    use_go_orchestrator: bool = False
    engine_mode: str = "embedded"
    shadow_dual_run: bool = False
    canary_enabled: bool = False



def load_migration_flags() -> MigrationFlags:
    engine_mode = (os.getenv("CONTEXTLATTICE_ENGINE_MODE", "embedded").strip().lower() or "embedded")
    if engine_mode not in {"embedded", "service"}:
        engine_mode = "embedded"
    return MigrationFlags(
        use_rust_codec=_env_bool("USE_RUST_CODEC", False),
        use_rust_memory=_env_bool("USE_RUST_MEMORY", False),
        use_rust_retrieval=_env_bool("USE_RUST_RETRIEVAL", False),
        use_go_orchestrator=_env_bool("USE_GO_ORCHESTRATOR", False),
        engine_mode=engine_mode,
        shadow_dual_run=_env_bool("MIGRATION_SHADOW_DUAL_RUN", False),
        canary_enabled=_env_bool("MIGRATION_CANARY_ENABLED", False),
    )
