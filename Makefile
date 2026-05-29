default: mem-up

# === Unified launcher (GNU Make 4.4.1) ===============================
# Optional for legacy Docker clients; leave unset by default so
# docker compose negotiates the daemon-supported API version.
ifdef DOCKER_API_VERSION
export DOCKER_API_VERSION
endif

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.ONESHELL:
.RECIPEPREFIX := >
.DEFAULT_GOAL := launch
PROFILES ?=
COMMA := ,
PROFILE_LIST := $(strip $(subst $(COMMA), ,$(PROFILES)))
PROFILE_ARGS := $(strip $(foreach profile,$(PROFILE_LIST),--profile $(profile)))

# OS detect (for future use)
UNAME_S := $(shell uname -s)
BASE_OS := $(if $(filter $(UNAME_S),Darwin),mac,linux)

# Core compose invocation (env-driven)
ENV_FILE ?= .env
DC := docker compose -f docker-compose.yml
PYTEST_FOCUS ?= app
PYTEST_APP_TESTS := archive/services/orchestrator_legacy_python/tests/test_orchestrator_retrieval.py archive/services/orchestrator_legacy_python/tests/test_migration_runtime.py

.PHONY: help launch all up up-core down status ps logs build rebuild pull clean prune             mcp-proxy-up init qdrant-init mindsdb-seed letta-seed models-pull             proxy-status doctor mem-ping monitor-open monitor-check dmg-build msi-build linux-bundle-build            storage-audit qdrant-snapshot-prune qdrant-cutover cold-snapshot-pack cold-snapshot-tier cold-snapshot-restore telemetry-archive fanout-status fanout-deadletters fanout-rehydrate retention-install retention-uninstall retention-status retention-install-daily storage-ledger-capture storage-ledger-prune storage-ledger-install storage-ledger-uninstall storage-ledger-status memory-graph-quality memory-graph-quality-install memory-graph-quality-uninstall memory-graph-quality-status weekly-lineage-rollup weekly-lineage-install weekly-lineage-uninstall weekly-lineage-status            docker-fs-watchdog-run docker-fs-watchdog-install docker-fs-watchdog-uninstall docker-fs-watchdog-status            storage-migrate-hot-bindings disk-clean-safe            mem-mode-show mem-mode-core mem-mode-balanced mem-mode-full mem-up-core mem-up-balanced mem-up-full observability-up observability-down launch-readiness-gate launch-readiness-gate-schedule launch-readiness-gate-schedule-status launch-readiness-gate-schedule-cancel paid-launch-checklist backup-restore-drill mem-up-release mem-up-lite-release release-lock-verify qdrant-cloud-check quickstart submission-preflight launch-lock launch-lock-public test-py bench-shortlist bench-qdrant-tuning bench-backend-lanes env-lock-check env-lock-apply sentrux-check sentrux-gate sentrux-gate-save agent-context-gate

help:
> echo "Targets:"
> echo "  launch (default): compose up + proxy + memory init"
> echo "  up/down/status/logs/build/rebuild/pull/clean/prune/ps"
> echo "    (set PROFILES=core,llm to limit docker compose)"
> echo "  up-core: helper for PROFILES=core docker compose up"
> echo "  mem-mode-show|mem-mode-core|mem-mode-balanced|mem-mode-full: toggle persistent COMPOSE_PROFILES in .env"
> echo "  mem-up-lite: local lite core (topic_rollups + qdrant, no adapter lab)"
> echo "  mem-up-lite-advanced: local lite plus memory-bank spike/adapters"
> echo "  mem-up-balanced: bounded v4 launcher (single active spike lane, observability off by default)"
> echo "  observability-up|observability-down: on-demand Langfuse stack controls"
> echo "  models-pull: pull local Ollama models (optional)"
> echo "  mcp-proxy-up: configure & start mcp-proxy on :9090"
> echo "  init: qdrant-init + optional mindsdb/letta seeds"
> echo "  doctor: quick endpoint probes"
> echo "  mem-ping: MCP hub tools/list against memorymcp"
> echo "  monitor-open|monitor-check: less-technical monitoring helpers (dashboard + health/status)"
> echo "  dmg-build: build ContextLattice macOS bootstrap DMG in ./dist"
> echo "  msi-build: build ContextLattice Windows MSI bootstrap installer in ./dist"
> echo "  linux-bundle-build: build ContextLattice Linux bootstrap tarball in ./dist"
> echo "  fanout-status|fanout-deadletters|fanout-rehydrate: durability + replay ops"
> echo "  cold-snapshot-pack|cold-snapshot-tier|cold-snapshot-restore: compact + tier + restore cold snapshots"
> echo "  storage-ledger-capture|storage-ledger-prune: append/prune metadata-only storage growth ledger"
> echo "  storage-ledger-install|storage-ledger-status: install hourly ledger runner (launchd)"
> echo "  memory-graph-quality*: score graph coverage and install bounded repair runner"
> echo "  weekly-lineage-rollup: generate weekly per-project lineage + global synergy rollups"
> echo "  weekly-lineage-install|weekly-lineage-status: install weekly lineage runner (launchd)"
> echo "  qdrant-cutover: set QDRANT_COLLECTION and rehydrate vectors"
> echo "  service-version-audit|service-version-apply: check/apply stable image tag bumps"
> echo "  service-update-pipeline: audit -> validate -> redeploy -> tests -> health checks"
> echo "  service-update-install|service-update-status: schedule automated service update checks"
> echo "  storage-migrate-hot-bindings: copy hot bind-mount data into named volumes for fs stability"
> echo "  docker-fs-watchdog-* : install/status/run watchdog for Docker Desktop fs injector stalls"
> echo "  test-py: run Python tests (PYTEST_FOCUS=app|all; default app)"
> echo "  agent-context-gate: validate AGENTS.md/skill budgets and frontmatter"
> echo "  bench-shortlist: run shortlist candidate performance matrix harness"
> echo "  bench-qdrant-tuning: run qdrant tuning benchmark matrix harness"
> echo "  bench-backend-lanes: run baseline vs rust lane vs lexical spike benchmark matrix"
> echo "  launch-readiness-gate: accelerated soak + queue drain + backup drill + security preflight"
> echo "  launch-readiness-gate-schedule*: schedule/status/cancel one-shot 04:30 America/Denver gate run"
> echo "  qdrant-cloud-check: verify HTTP + gRPC to BYO Qdrant Cloud endpoint"
> echo "  mem-up-release|mem-up-lite-release: compose up with image digest lockfile"
> echo "  quickstart: create .env if needed, run secure bootstrap, and verify health"
> echo "  submission-preflight: verify repo collateral for launch directory submissions"
> echo "  launch-lock: hard local-track launch gate (blocks out-of-order publishing)"
> echo "  launch-lock-public: hard public-track gate (requires live public HTTPS /mcp)"

test-py:
> if [ "$(PYTEST_FOCUS)" = "all" ]; then \
>   pytest -q archive/services/orchestrator_legacy_python/tests; \
> else \
>   pytest -q $(PYTEST_APP_TESTS); \
> fi

agent-context-gate:
> scripts/agent/audit-agent-context --pretty

bench-shortlist:
> api_key="$${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}"; \
> python3 tooling/python/bench/perf_shortlist_matrix.py --api-key "$$api_key"

bench-qdrant-tuning:
> api_key="$${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}"; \
> python3 tooling/python/bench/qdrant_tuning_matrix.py --api-key "$$api_key"

bench-backend-lanes:
> api_key="$${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}"; \
> python3 tooling/python/bench/backend_lane_matrix.py --api-key "$$api_key"

# ---- One-shot launcher ----

# Back-compat alias
all: launch

# ---- Compose lifecycle ----
up:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply
> if [ -n "$(PROFILES)" ]; then echo ">> compose up (build) [profiles: $(PROFILES)] with $(ENV_FILE)"; else echo ">> compose up (build) with $(ENV_FILE)"; fi
> $(DC) $(PROFILE_ARGS) up -d --build
> PROFILES="$(PROFILES)" ENV_FILE="$(ENV_FILE)" scripts/ensure_langfuse_running.sh

up-core:
> $(MAKE) up PROFILES="core"

down:
> $(DC) down

status ps:
> $(DC) ps

logs:
> $(DC) logs -f --tail=200

build:
> $(DC) build

rebuild:
> $(DC) build --no-cache

pull:
> $(DC) pull

clean:
> $(DC) down -v --remove-orphans

prune:
> docker system prune -f

# ---- Proxy-only gateway (TBXark mcp-proxy) ----
mcp-proxy-up:
> [ -x scripts/mcp_proxy_bootstrap.sh ] || { echo "ERROR: scripts/mcp_proxy_bootstrap.sh missing or not executable"; exit 1; }
> bash scripts/mcp_proxy_bootstrap.sh

# ---- Memory init bundle (Qdrant tuning + optional seeds) ----
init: qdrant-init mindsdb-seed letta-seed

qdrant-init:
> if [ -x scripts/qdrant_init.sh ]; then bash scripts/qdrant_init.sh; else echo ">> skip qdrant init (no script)"; fi

mindsdb-seed:
> if [ -x scripts/mindsdb_seed_kb.sh ]; then echo ">> seeding MindsDB KB"; bash scripts/mindsdb_seed_kb.sh || true; else echo ">> skip mindsdb seed"; fi

letta-seed:
> if [ -x scripts/letta_seed_ollama.sh ]; then
>   echo ">> seeding Letta (ollama)"; bash scripts/letta_seed_ollama.sh || true
> elif [ -x scripts/letta_seed.sh ]; then
>   echo ">> seeding Letta (generic)"; bash scripts/letta_seed.sh || true
> else
>   echo ">> skip letta seed"
> fi
> if [ -x scripts/letta_autowire.sh ]; then echo ">> autowiring Letta"; bash scripts/letta_autowire.sh || true; fi

# ---- Models (Ollama) ----
models-pull:
> $(DC) exec -T ollama ollama pull qwen2.5-coder:7b || true
> $(DC) exec -T ollama ollama pull nomic-embed-text:latest || true

# ---- Diagnostics ----
proxy-status:
> curl -fsS http://127.0.0.1:9090/status || echo "WARN: proxy status endpoint unavailable"

doctor:
> echo "== compose ps ==" && $(DC) ps || true
> echo "== probe mcp-proxy :9090 ==" && (curl -fsSI http://127.0.0.1:9090 | head -n1 || true)
> echo "== probe qdrant :6333 ==" && (curl -fsSI http://127.0.0.1:6333 | head -n1 || true)
> echo "== probe qdrant-adv :8022/mcp ==" && (curl -fsSI http://127.0.0.1:8022/mcp | head -n1 || true)
> echo "== probe qdrant std :8000/mcp ==" && (curl -fsSI http://127.0.0.1:8000/mcp | head -n1 || true)
> echo "== probe mindsdb-proxy :8011/mcp ==" && (curl -fsSI http://127.0.0.1:8011/mcp | head -n1 || true)

mem-ping:
> bash scripts/mem_ping.sh

monitor-open:
> bash scripts/open_monitoring.sh

monitor-check:
> if [ -f .env ]; then source .env >/dev/null 2>&1 || true; fi
> ORCH_URL="$${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
> ORCH_KEY="$${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}"
> echo "== /health ==" && curl -fsS "$${ORCH_URL%/}/health" | jq .
> if [ -n "$$ORCH_KEY" ]; then \
>   echo "== /status ==" && curl -fsS -H "x-api-key: $$ORCH_KEY" "$${ORCH_URL%/}/status" | jq .; \
>   echo "== /telemetry/fanout ==" && curl -fsS -H "x-api-key: $$ORCH_KEY" "$${ORCH_URL%/}/telemetry/fanout" | jq '{updatedAt,summary,health}'; \
> else \
>   echo "INFO: no orchestrator API key found in env; skipped authenticated checks."; \
> fi

dmg-build:
> bash scripts/build_macos_dmg.sh

msi-build:
> bash scripts/build_windows_msi.sh

linux-bundle-build:
> bash scripts/build_linux_bundle.sh

# ---- Storage / retention helpers ----

storage-audit:
> python3 scripts/storage_audit.py \
>   --qdrant-url "$${QDRANT_URL_HOST:-http://localhost:6333}" \
>   --mongo-url "$${MONGO_URL:-mongodb://localhost:27017}" \
>   --mongo-db "$${MONGO_DB:-memorybank}"

qdrant-snapshot-prune:
> python3 scripts/qdrant_snapshot_prune.py \
>   --qdrant-url "$${QDRANT_URL_HOST:-http://localhost:6333}" \
>   --collection "$${ORCH_QDRANT_COLLECTION:-contextlattice_notes}" \
>   --retention-hours "$${QDRANT_RETENTION_HOURS:-0}" \
>   --retention-days "$${QDRANT_RETENTION_DAYS:-14}" \
>   --snapshot-dir "$${CONTEXTLATTICE_COLD_ROOT:-./.data/cold/qdrant}" \
>   --timeout-secs "$${QDRANT_HTTP_TIMEOUT_SECS:-300}" \
>   $$([ "$${QDRANT_SKIP_SNAPSHOT:-0}" = "1" ] && echo --skip-snapshot) \
>   $$([ "$${QDRANT_SKIP_PRUNE:-0}" = "1" ] && echo --skip-prune)

cold-snapshot-pack:
> scripts/context_storage_ops.sh cold-pack \
>   --cold-root "$${CONTEXTLATTICE_COLD_ROOT:-./.data/cold}" \
>   --level "$${COLD_SNAPSHOT_ZSTD_LEVEL:-3}" \
>   $$([ "$${COLD_SNAPSHOT_PACK_APPLY:-1}" = "1" ] && echo --apply) \
>   $$([ "$${COLD_SNAPSHOT_PACK_KEEP_ORIGINAL:-0}" = "1" ] && echo --keep-original) \
>   $$([ "$${COLD_SNAPSHOT_PACK_VERIFY:-1}" = "0" ] && echo --no-verify) \
>   $$([ -n "$${COLD_SNAPSHOT_PACK_MAX_FILES:-}" ] && echo --max-files "$${COLD_SNAPSHOT_PACK_MAX_FILES}")

cold-snapshot-tier:
> scripts/context_storage_ops.sh cold-tier \
>   --cold-root "$${CONTEXTLATTICE_COLD_ROOT:-./.data/cold}" \
>   --keep-latest "$${COLD_SNAPSHOT_KEEP_LATEST:-6}" \
>   --keep-daily "$${COLD_SNAPSHOT_KEEP_DAILY:-21}" \
>   --keep-weekly "$${COLD_SNAPSHOT_KEEP_WEEKLY:-12}" \
>   --keep-monthly "$${COLD_SNAPSHOT_KEEP_MONTHLY:-12}" \
>   $$([ "$${COLD_SNAPSHOT_TIER_APPLY:-1}" = "1" ] && echo --apply)

cold-snapshot-restore:
> COLLECTION="$${COLLECTION:?set COLLECTION=<bucket_name>}" \
> SNAPSHOT="$${SNAPSHOT:-}" \
> OUT_DIR="$${OUT_DIR:-}" \
> FORCE="$${FORCE:-0}" \
> python3 scripts/cold_snapshot_restore.py \
>   --cold-root "$${CONTEXTLATTICE_COLD_ROOT:-./.data/cold}" \
>   --collection "$$COLLECTION" \
>   $$([ -n "$$SNAPSHOT" ] && echo --snapshot "$$SNAPSHOT") \
>   $$([ -n "$$OUT_DIR" ] && echo --out-dir "$$OUT_DIR") \
>   $$([ "$$FORCE" = "1" ] && echo --force)

qdrant-cutover:
> TARGET_COLLECTION="$${TARGET_COLLECTION:?set TARGET_COLLECTION=<new_collection>}" LIMIT="$${LIMIT:-2000}" PROJECT="$${PROJECT:-}" scripts/qdrant_collection_cutover.sh --target "$$TARGET_COLLECTION" --limit "$$LIMIT" $${PROJECT:+--project "$$PROJECT"}

telemetry-archive:
> scripts/context_storage_ops.sh archive-ndjson \
>   --data-dir "$${ORCHESTRATOR_DATA_DIR:-./.data/orchestrator}" \
>   --retention-hours "$${TELEMETRY_HOT_HOURS:-48}" \
>   --cold-dir "$${CONTEXTLATTICE_COLD_ROOT:-./.data/cold/telemetry}"

fanout-status:
> curl -fsS "$${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}/telemetry/fanout" | jq

fanout-deadletters:
> curl -fsS "$${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}/telemetry/fanout/deadletters?limit=$${LIMIT:-50}" | jq

fanout-rehydrate:
> LIMIT="$${LIMIT:-2000}" PROJECT="$${PROJECT:-}" TARGETS="$${TARGETS:-}" QDRANT_COLLECTION="$${QDRANT_COLLECTION:-}" FORCE_REQUEUE="$${FORCE_REQUEUE:-0}" scripts/rehydrate_fanout.sh

retention-install:
> bash scripts/install_retention_runner.sh install

retention-install-daily:
> RETENTION_INTERVAL_SECONDS="$${RETENTION_INTERVAL_SECONDS:-86400}" RETENTION_RUN_AT_LOAD=1 bash scripts/install_retention_runner.sh install

retention-uninstall:
> bash scripts/install_retention_runner.sh uninstall

retention-status:
> bash scripts/install_retention_runner.sh status

storage-ledger-capture:
> scripts/context_storage_ops.sh ledger \
>   --orchestrator-url "$${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}" \
>   --keep-days "$${ORCH_STORAGE_LEDGER_KEEP_DAYS:-180}" \
>   --max-bytes "$${ORCH_STORAGE_LEDGER_MAX_BYTES:-134217728}" \
>   --tracked-top-limit "$${ORCH_STORAGE_LEDGER_TRACKED_TOP_LIMIT:-24}" \
>   --timeout-secs "$${ORCH_STORAGE_LEDGER_TIMEOUT_SECS:-20}"

storage-ledger-prune:
> scripts/context_storage_ops.sh ledger \
>   --keep-days "$${ORCH_STORAGE_LEDGER_KEEP_DAYS:-180}" \
>   --max-bytes "$${ORCH_STORAGE_LEDGER_MAX_BYTES:-134217728}" \
>   --prune-only

storage-ledger-install:
> bash scripts/install_storage_ledger_runner.sh install

storage-ledger-uninstall:
> bash scripts/install_storage_ledger_runner.sh uninstall

storage-ledger-status:
> bash scripts/install_storage_ledger_runner.sh status

memory-graph-quality:
> scripts/agent/memory-graph-quality --all-projects --pretty

memory-graph-quality-install:
> bash scripts/install_memory_graph_quality_runner.sh install

memory-graph-quality-uninstall:
> bash scripts/install_memory_graph_quality_runner.sh uninstall

memory-graph-quality-status:
> bash scripts/install_memory_graph_quality_runner.sh status

weekly-lineage-rollup:
> scripts/context_storage_ops.sh weekly-lineage \
>   --orchestrator-url "$${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}" \
>   --min-count "$${CONTEXTLATTICE_LINEAGE_MIN_COUNT:-1}" \
>   --top-topic-limit "$${CONTEXTLATTICE_LINEAGE_TOP_TOPIC_LIMIT:-60}" \
>   --synergy-min-projects "$${CONTEXTLATTICE_LINEAGE_SYNERGY_MIN_PROJECTS:-2}" \
>   --keep-weeks "$${CONTEXTLATTICE_LINEAGE_KEEP_WEEKS:-104}" \
>   --emit-synergy

weekly-lineage-install:
> bash scripts/install_weekly_lineage_runner.sh install

weekly-lineage-uninstall:
> bash scripts/install_weekly_lineage_runner.sh uninstall

weekly-lineage-status:
> bash scripts/install_weekly_lineage_runner.sh status

docker-fs-watchdog-run:
> bash scripts/docker_fs_watchdog.sh

docker-fs-watchdog-install:
> bash scripts/install_docker_fs_watchdog.sh install

docker-fs-watchdog-uninstall:
> bash scripts/install_docker_fs_watchdog.sh uninstall

docker-fs-watchdog-status:
> bash scripts/install_docker_fs_watchdog.sh status

storage-migrate-hot-bindings:
> bash scripts/migrate_hot_bind_mounts_to_named_volumes.sh

.PHONY: service-version-audit service-version-apply service-update-pipeline service-update-install service-update-uninstall service-update-status service-update-run
service-version-audit:
> python3 scripts/service_version_audit.py --report-file tmp/service-version-report.json

service-version-apply:
> python3 scripts/service_version_audit.py --apply --report-file tmp/service-version-report.json

service-update-pipeline:
> bash scripts/update_services_pipeline.sh

service-update-install:
> bash scripts/install_service_update_runner.sh install

service-update-uninstall:
> bash scripts/install_service_update_runner.sh uninstall

service-update-status:
> bash scripts/install_service_update_runner.sh status

service-update-run:
> bash scripts/service_update_runner.sh

submission-preflight:
> python3 scripts/submission_preflight.py

launch-lock:
> python3 scripts/launch_lock.py --mode local

launch-lock-public:
> python3 scripts/launch_lock.py --mode public

# ---- env wiring (append-only) ----

export OPENAI_API_BASE ?= $(if $(wildcard $(ENV_FILE)),$(shell grep -E '^OPENAI_API_BASE=' $(ENV_FILE) 2>/dev/null | tail -1 | cut -d= -f2),)
export OPENAI_API_KEY  ?= $(if $(wildcard $(ENV_FILE)),$(shell grep -E '^OPENAI_API_KEY='  $(ENV_FILE) 2>/dev/null | tail -1 | cut -d= -f2),)
export MLX_API_BASE    ?= $(if $(wildcard $(ENV_FILE)),$(shell grep -E '^MLX_API_BASE='    $(ENV_FILE) 2>/dev/null | tail -1 | cut -d= -f2),)
export MLX_MODEL_PATH  ?= $(if $(wildcard $(ENV_FILE)),$(shell grep -E '^MLX_MODEL_PATH='  $(ENV_FILE) 2>/dev/null | tail -1 | cut -d= -f2),)
export LETTA_PORT      ?= $(if $(wildcard $(ENV_FILE)),$(shell grep -E '^LETTA_PORT='      $(ENV_FILE) 2>/dev/null | tail -1 | cut -d= -f2),)

.PHONY: env-print env-check
env-print:
> @echo "OPENAI_API_BASE=$(OPENAI_API_BASE)"
> @echo "OPENAI_API_KEY=$(OPENAI_API_KEY)"
> @echo "MLX_API_BASE=$(MLX_API_BASE)"
> @echo "MLX_MODEL_PATH=$(MLX_MODEL_PATH)"
> @echo "LETTA_PORT=$(LETTA_PORT)"

env-check:
> @ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --check
> @$(DC) config >/dev/null && echo "compose syntax: OK"

env-lock-check:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --check

env-lock-apply:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply

# ===== Local sidecars: MLX/vLLM endpoints; Go gateway owns routing =====
ENV_FILE ?= .env

.PHONY: mlx-up mlx-down router-up router-down sidecars-up sidecars-down runtime-policy inference-backend-status inference-backend-assert-one

mlx-up:
> scripts/inference_backend_guard.sh prepare mlx
> test -d .venv-mlx || (uv venv .venv-mlx && . .venv-mlx/bin/activate && uv pip install -U mlx-lm)
> pgrep -f "mlx_lm.server" >/dev/null 2>&1 && echo "mlx already running" || \
> (MODEL_PATH="$(MLX_MODEL_PATH)"; \
>  [ -n "$$MODEL_PATH" ] || { echo "ERROR: set MLX_MODEL_PATH in $(ENV_FILE)"; exit 1; }; \
>  . .venv-mlx/bin/activate; \
>  python -m mlx_lm.server \
>    --model "$$MODEL_PATH" \
>    --host 127.0.0.1 --port 18087 >logs/mlx.log 2>&1 & echo $$! > .mlx.pid; \
>  echo "mlx: http://127.0.0.1:18087/v1 (pid $$(cat .mlx.pid))")

mlx-down:
> test -f .mlx.pid && kill "$$(cat .mlx.pid)" 2>/dev/null && rm -f .mlx.pid || true
> pkill -f "mlx_lm.server" 2>/dev/null || true

router-up:
> @echo "router-up is archived; gateway-go /v1/inference/* owns provider routing"

router-down:
> test -f .router.pid && kill "$$(cat .router.pid)" 2>/dev/null && rm -f .router.pid || true
> pkill -f "openai_router:app" 2>/dev/null || true

sidecars-up: mlx-up
sidecars-down: router-down mlx-down

runtime-policy:
> scripts/inference_runtime_policy.sh

inference-backend-status:
> scripts/inference_backend_guard.sh status

inference-backend-assert-one:
> scripts/inference_backend_guard.sh assert-one

# Wire sidecars into your one-shot launcher

# ---- OLLAMA bring-up & health (host install or Homebrew), plus wait ----
ENV_FILE ?= .env
.ONESHELL:

.PHONY: ollama-up ollama-down ollama-wait

ollama-up:
> scripts/inference_backend_guard.sh prepare ollama
> OAI_BASE="$$(grep -E '^OLLAMA_API_BASE=' $(ENV_FILE) | tail -1 | cut -d= -f2)"
> [ -z "$$OAI_BASE" ] && OAI_BASE="http://127.0.0.1:11434/v1"
> echo "Checking Ollama at $$OAI_BASE ..."
> if curl -sS --fail "$$OAI_BASE/models" >/dev/null 2>&1; then
>   echo "Ollama already running"
> else
>   if command -v brew >/dev/null 2>&1 && brew list --formula | grep -q '^ollama$$'; then
>     echo "Starting Ollama via Homebrew service..."
>     brew services start ollama >/dev/null 2>&1 || true
>   else
>     echo "Starting foreground 'ollama serve' (nohup)..."
>     nohup ollama serve >/tmp/ollama.log 2>&1 & echo $$! > .ollama.pid
>   fi
>   echo "Waiting for Ollama HTTP..."
>   scripts/wait_for_http.sh "$$OAI_BASE/models" 90
> fi

ollama-down:
> if [ -f .ollama.pid ]; then kill "$$(cat .ollama.pid)" 2>/dev/null && rm -f .ollama.pid; fi
> if command -v brew >/dev/null 2>&1 && brew list --formula | grep -q '^ollama$$'; then
>   brew services stop ollama >/dev/null 2>&1 || true
> fi

ollama-wait:
> OAI_BASE="$$(grep -E '^OLLAMA_API_BASE=' $(ENV_FILE) | tail -1 | cut -d= -f2)"
> [ -z "$$OAI_BASE" ] && OAI_BASE="http://127.0.0.1:11434/v1"
> scripts/wait_for_http.sh "$$OAI_BASE/models" 90

# Ollama is a compatibility fallback, not a required launch precondition.

# ---- Ordered launcher (strict sequence) ----
.PHONY: launch

.PHONY: router-status router-logs router-restart
router-status:
> scripts/inference_runtime_policy.sh

router-logs:
> [ -f logs/router.log ] && tail -n 200 logs/router.log || echo "no logs/router.log yet"

router-restart: router-down router-up router-status
.PHONY: launch
launch:
> $(MAKE) up
> $(MAKE) mcp-proxy-up
> scripts/inference_backend_guard.sh assert-one
> scripts/inference_runtime_policy.sh || true
> $(MAKE) init
> echo ">> launch complete - inference policy above, Ollama only starts when selected by profile/config"

.PHONY: router-wait
router-wait:
> scripts/wait_for_http.sh "$${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}/v1/inference/runtime-policy" 60

# ----- Trae (runs from local source checkout) -----
TRAE_DIR ?= $(HOME)/.trae_agent
TRAE_CFG ?= $(PWD)/trae_config.yaml

.PHONY: trae-install trae-config trae-run-small trae-run-big trae-shell
trae-install:
> test -d $(TRAE_DIR) || git clone https://github.com/bytedance/trae-agent.git $(TRAE_DIR)
> cd $(TRAE_DIR) && uv sync --all-extras

trae-config:
> envsubst < trae_config.template.yaml > trae_config.yaml
> echo "Rendered $(TRAE_CFG)"

trae-run-small: trae-install trae-config
> cd $(TRAE_DIR) && uv run trae-cli --config "$(TRAE_CFG)" run --agent fast_fix "Add logging to retry path"

trae-run-big: trae-install trae-config
> cd $(TRAE_DIR) && uv run trae-cli --config "$(TRAE_CFG)" run "Refactor the auth module to remove deprecated JWT path"

trae-shell: trae-install trae-config
> cd $(TRAE_DIR) && uv run trae-cli --config "$(TRAE_CFG)" interactive

# Lightweight memory stack shortcuts (avoid mk/memory.mk)
.PHONY: mem-up mem-down mem mem-ps quickstart
MEM_PROFILE_CORE := core
MEM_PROFILE_BALANCED := core,llm
MEM_PROFILE_FULL := core,analytics,llm,observability
MEM_PROFILES_DEFAULT := $(shell [ -f "$(ENV_FILE)" ] && awk -F= '/^COMPOSE_PROFILES=/{print substr($$0,index($$0,"=")+1)}' "$(ENV_FILE)" | tail -1)
MEM_PROFILES ?= $(if $(strip $(MEM_PROFILES_DEFAULT)),$(strip $(MEM_PROFILES_DEFAULT)),core)

mem-mode-show:
> if [ -f "$(ENV_FILE)" ]; then \
>   grep -E '^COMPOSE_PROFILES=' "$(ENV_FILE)" | tail -1 || echo "COMPOSE_PROFILES not set in $(ENV_FILE)"; \
> else \
>   echo "$(ENV_FILE) not found"; \
> fi
> echo "mem default profiles => $(MEM_PROFILES)"

mem-mode-core:
> bash scripts/set_compose_profiles.sh "$(ENV_FILE)" "$(MEM_PROFILE_CORE)"
> $(MAKE) mem-mode-show

mem-mode-balanced:
> bash scripts/set_compose_profiles.sh "$(ENV_FILE)" "$(MEM_PROFILE_BALANCED)"
> $(MAKE) mem-mode-show

mem-mode-full:
> bash scripts/set_compose_profiles.sh "$(ENV_FILE)" "$(MEM_PROFILE_FULL)"
> $(MAKE) mem-mode-show

mem-up:
> PROFILES="$(MEM_PROFILES)" $(MAKE) up

mem-down:
> PROFILES="$(MEM_PROFILES)" $(MAKE) down

mem-ps:
> PROFILES="$(MEM_PROFILES)" $(MAKE) ps

quickstart:
> if [ ! -f "$(ENV_FILE)" ]; then \
>   cp .env.example "$(ENV_FILE)"; \
>   echo ">> created $(ENV_FILE) from .env.example"; \
> fi
> mkdir -p infra/compose
> ln -svf "../../$(ENV_FILE)" infra/compose/.env >/dev/null
> BOOTSTRAP=1 scripts/first_run.sh
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply
> ORCH_KEY="$$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($$0,index($$0,"=")+1)}' "$(ENV_FILE)" | tail -1)"; \
> curl -fsS "http://127.0.0.1:8075/health" | jq . >/dev/null; \
> curl -fsS -H "x-api-key: $$ORCH_KEY" "http://127.0.0.1:8075/status" | jq . >/dev/null; \
> echo ">> quickstart complete (health + status checks passed)"

mem-up-core:
> PROFILES="$(MEM_PROFILE_CORE)" $(MAKE) up

mem-up-balanced:
> ENV_FILE="$(ENV_FILE)" scripts/compose_v4_balanced.sh --env-file "$(ENV_FILE)"

mem-guard-start:
> ENV_FILE="$(ENV_FILE)" scripts/docker_vm_rss_guard.sh start --env-file "$(ENV_FILE)"

mem-guard-stop:
> ENV_FILE="$(ENV_FILE)" scripts/docker_vm_rss_guard.sh stop --env-file "$(ENV_FILE)"

mem-guard-status:
> ENV_FILE="$(ENV_FILE)" scripts/docker_vm_rss_guard.sh status --env-file "$(ENV_FILE)"

mem-up-full:
> PROFILES="$(MEM_PROFILE_FULL)" $(MAKE) up

observability-up:
> ENV_FILE="$(ENV_FILE)" scripts/compose_v4_balanced.sh --env-file "$(ENV_FILE)" --with-observability

observability-down:
> docker compose --env-file "$(ENV_FILE)" stop langfuse langfuse-worker lf-postgres lf-clickhouse lf-minio || true

.PHONY: mem-up-lite mem-up-lite-advanced mem-down-lite mem-ps-lite mem-ps-lite-advanced
mem-up-lite:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply
> COMPOSE_PROFILES= \
> ORCH_LITE_RETRIEVAL_SOURCES=qdrant,mongo_raw,topic_rollups \
> ORCH_LITE_RETRIEVAL_SLOW_SOURCES=mongo_raw \
> ORCH_LITE_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES=qdrant,mongo_raw \
> ORCH_LITE_RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED=false \
> ORCH_LITE_MEMORY_BANK_SEARCH_BACKEND=disabled \
> docker compose -f docker-compose.lite.yml up -d --build --remove-orphans
> ENV_FILE="$(ENV_FILE)" COMPOSE_PROJECT_NAME="$${COMPOSE_PROJECT_NAME:-contextlattice}" scripts/verify_storage_mounts.sh

mem-up-lite-advanced:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply
> COMPOSE_PROFILES=advanced \
> ORCH_LITE_RETRIEVAL_SOURCES=qdrant,mongo_raw,topic_rollups,memory_bank \
> ORCH_LITE_RETRIEVAL_SLOW_SOURCES=mongo_raw,memory_bank \
> ORCH_LITE_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES=qdrant,memory_bank,mongo_raw \
> ORCH_LITE_RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED=true \
> ORCH_LITE_MEMORY_BANK_SEARCH_BACKEND=shodh_spike \
> docker compose -f docker-compose.lite.yml up -d --build --remove-orphans

mem-down-lite:
> docker compose -f docker-compose.lite.yml down --remove-orphans

mem-ps-lite:
> docker compose -f docker-compose.lite.yml ps

mem-ps-lite-advanced:
> COMPOSE_PROFILES=advanced docker compose -f docker-compose.lite.yml ps

mem-up-release:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply
> docker compose -f docker-compose.yml -f docker-compose.release.lock.yml up -d --build

mem-up-lite-release:
> ENV_FILE="$(ENV_FILE)" scripts/enforce_strict_env.sh --apply
> docker compose -f docker-compose.lite.yml -f docker-compose.lite.release.lock.yml up -d --build --remove-orphans

release-lock-verify:
> docker compose -f docker-compose.yml -f docker-compose.release.lock.yml config --services >/dev/null
> docker compose -f docker-compose.lite.yml -f docker-compose.lite.release.lock.yml config --services >/dev/null
> echo "release lock compose config: OK"

launch-readiness-gate:
> scripts/launch_readiness_gate.sh

launch-readiness-gate-schedule:
> scripts/schedule_launch_gate_0430_mt.sh schedule

launch-readiness-gate-schedule-status:
> scripts/schedule_launch_gate_0430_mt.sh status

launch-readiness-gate-schedule-cancel:
> scripts/schedule_launch_gate_0430_mt.sh cancel

backup-restore-drill:
> scripts/launch_backup_restore_drill.sh

qdrant-cloud-check:
> scripts/test_qdrant_cloud.sh

# Full "launch everything" under the mem alias
mem:
> PROFILES="$(MEM_PROFILES)" $(MAKE) launch
