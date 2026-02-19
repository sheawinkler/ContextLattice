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

.PHONY: help launch all up up-core down status ps logs build rebuild pull clean prune             kalliste init qdrant-init mindsdb-seed letta-seed models-pull             proxy-status doctor mem-ping            storage-audit qdrant-snapshot-prune qdrant-cutover telemetry-archive fanout-status fanout-deadletters fanout-rehydrate retention-install retention-uninstall retention-status             mem-mode-show mem-mode-core mem-mode-full mem-up-core mem-up-full launch-readiness-gate launch-readiness-gate-schedule launch-readiness-gate-schedule-status launch-readiness-gate-schedule-cancel backup-restore-drill mem-up-release mem-up-lite-release release-lock-verify qdrant-cloud-check

help:
> echo "Targets:"
> echo "  launch (default): compose up + proxy + memory init"
> echo "  up/down/status/logs/build/rebuild/pull/clean/prune/ps"
> echo "    (set PROFILES=core,llm to limit docker compose)"
> echo "  up-core: helper for PROFILES=core docker compose up"
> echo "  mem-mode-show|mem-mode-core|mem-mode-full: toggle persistent COMPOSE_PROFILES in .env"
> echo "  models-pull: pull local Ollama models (optional)"
> echo "  kalliste: configure & start mcp-proxy on :9090"
> echo "  init: qdrant-init + optional mindsdb/letta seeds"
> echo "  doctor: quick endpoint probes"
> echo "  mem-ping: MCP hub tools/list against memorymcp"
> echo "  fanout-status|fanout-deadletters|fanout-rehydrate: durability + replay ops"
> echo "  qdrant-cutover: set QDRANT_COLLECTION and rehydrate vectors"
> echo "  service-version-audit|service-version-apply: check/apply stable image tag bumps"
> echo "  service-update-pipeline: audit -> validate -> redeploy -> tests -> health checks"
> echo "  service-update-install|service-update-status: schedule automated service update checks"
> echo "  launch-readiness-gate: accelerated soak + queue drain + backup drill + security preflight"
> echo "  launch-readiness-gate-schedule*: schedule/status/cancel one-shot 04:30 America/Denver gate run"
> echo "  qdrant-cloud-check: verify HTTP + gRPC to BYO Qdrant Cloud endpoint"
> echo "  mem-up-release|mem-up-lite-release: compose up with image digest lockfile"

# ---- One-shot launcher ----

# Back-compat alias
all: launch

# ---- Compose lifecycle ----
up:
> if [ -n "$(PROFILES)" ]; then echo ">> compose up (build) [profiles: $(PROFILES)] with $(ENV_FILE)"; else echo ">> compose up (build) with $(ENV_FILE)"; fi
> $(DC) $(PROFILE_ARGS) up -d --build

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
kalliste:
> [ -x scripts/kalliste.sh ] || { echo "ERROR: scripts/kalliste.sh missing or not executable"; exit 1; }
> bash scripts/kalliste.sh

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

# ---- Storage / retention helpers ----

storage-audit:
> python3 scripts/storage_audit.py \
>   --qdrant-url "$${QDRANT_URL_HOST:-http://localhost:6333}" \
>   --mongo-url "$${MONGO_URL:-mongodb://localhost:27017}" \
>   --mongo-db "$${MONGO_DB:-memorybank}"

qdrant-snapshot-prune:
> python3 scripts/qdrant_snapshot_prune.py \
>   --qdrant-url "$${QDRANT_URL_HOST:-http://localhost:6333}" \
>   --collection "$${ORCH_QDRANT_COLLECTION:-memmcp_notes}" \
>   --retention-hours "$${QDRANT_RETENTION_HOURS:-0}" \
>   --retention-days "$${QDRANT_RETENTION_DAYS:-14}" \
>   --snapshot-dir "$${MEMMCP_COLD_ROOT:-./.data/cold/qdrant}" \
>   --timeout-secs "$${QDRANT_HTTP_TIMEOUT_SECS:-300}" \
>   $$([ "$${QDRANT_SKIP_SNAPSHOT:-0}" = "1" ] && echo --skip-snapshot) \
>   $$([ "$${QDRANT_SKIP_PRUNE:-0}" = "1" ] && echo --skip-prune)

qdrant-cutover:
> TARGET_COLLECTION="$${TARGET_COLLECTION:?set TARGET_COLLECTION=<new_collection>}" LIMIT="$${LIMIT:-2000}" PROJECT="$${PROJECT:-}" scripts/qdrant_collection_cutover.sh --target "$$TARGET_COLLECTION" --limit "$$LIMIT" $${PROJECT:+--project "$$PROJECT"}

telemetry-archive:
> python3 scripts/archive_ndjson_by_time.py \
>   --data-dir "$${ORCHESTRATOR_DATA_DIR:-./.data/orchestrator}" \
>   --retention-hours "$${TELEMETRY_HOT_HOURS:-48}" \
>   --cold-dir "$${MEMMCP_COLD_ROOT:-./.data/cold/telemetry}"

fanout-status:
> curl -fsS "$${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}/telemetry/fanout" | jq

fanout-deadletters:
> curl -fsS "$${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}/telemetry/fanout/deadletters?limit=$${LIMIT:-50}" | jq

fanout-rehydrate:
> LIMIT="$${LIMIT:-2000}" PROJECT="$${PROJECT:-}" TARGETS="$${TARGETS:-}" QDRANT_COLLECTION="$${QDRANT_COLLECTION:-}" FORCE_REQUEUE="$${FORCE_REQUEUE:-0}" scripts/rehydrate_fanout.sh

retention-install:
> bash scripts/install_retention_runner.sh install

retention-uninstall:
> bash scripts/install_retention_runner.sh uninstall

retention-status:
> bash scripts/install_retention_runner.sh status

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

# ---- env wiring (append-only) ----

export OPENAI_API_BASE ?= $(shell grep -E '^OPENAI_API_BASE=' $(ENV_FILE) | tail -1 | cut -d= -f2)
export OPENAI_API_KEY  ?= $(shell grep -E '^OPENAI_API_KEY='  $(ENV_FILE) | tail -1 | cut -d= -f2)
export MLX_API_BASE    ?= $(shell grep -E '^MLX_API_BASE='    $(ENV_FILE) | tail -1 | cut -d= -f2)
export LETTA_PORT      ?= $(shell grep -E '^LETTA_PORT='      $(ENV_FILE) | tail -1 | cut -d= -f2)

.PHONY: env-print env-check
env-print:
> @echo "OPENAI_API_BASE=$(OPENAI_API_BASE)"
> @echo "OPENAI_API_KEY=$(OPENAI_API_KEY)"
> @echo "MLX_API_BASE=$(MLX_API_BASE)"
> @echo "LETTA_PORT=$(LETTA_PORT)"

env-check:
> @$(DC) config >/dev/null && echo "compose syntax: OK"

# ===== Local sidecars: MLX server + OpenAI router (pidfile-managed) =====
ENV_FILE ?= .env

.PHONY: mlx-up mlx-down router-up router-down sidecars-up sidecars-down

mlx-up:
> test -d .venv-mlx || (uv venv .venv-mlx && . .venv-mlx/bin/activate && uv pip install -U mlx-lm)
> pgrep -f "mlx_lm.server" >/dev/null 2>&1 && echo "mlx already running" || \
> (. .venv-mlx/bin/activate; \
>  python -m mlx_lm.server \
>    --model "/Volumes/wd_black/lmstudio_models/Eldadalbajob/Qwen3-42B-A3B-2507-Thinking-Abliterated-uncensored-TOTAL-RECALL-v2-Medium-MASTER-CODER-mlx-4Bit" \
>    --host 127.0.0.1 --port 18087 >logs/mlx.log 2>&1 & echo $$! > .mlx.pid; \
>  echo "mlx: http://127.0.0.1:18087/v1 (pid $$(cat .mlx.pid))")

mlx-down:
> test -f .mlx.pid && kill "$$(cat .mlx.pid)" 2>/dev/null && rm -f .mlx.pid || true
> pkill -f "mlx_lm.server" 2>/dev/null || true

router-up:

router-down:
> test -f .router.pid && kill "$$(cat .router.pid)" 2>/dev/null && rm -f .router.pid || true
> pkill -f "openai_router:app" 2>/dev/null || true

sidecars-up: mlx-up
sidecars-down: router-down mlx-down

# Wire sidecars into your one-shot launcher

# ---- OLLAMA bring-up & health (host install or Homebrew), plus wait ----
ENV_FILE ?= .env
.ONESHELL:

.PHONY: ollama-up ollama-down ollama-wait

ollama-up:
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

# Ensure launch verifies Ollama first

# ---- Ordered launcher (strict sequence) ----
.PHONY: launch

.PHONY: router-status router-logs router-restart
router-status:
> ROUTER_BASE="$$(grep -E '^ROUTER_API_BASE=' .env | tail -1 | cut -d= -f2)"; \
> [ -z "$$ROUTER_BASE" ] && ROUTER_BASE="http://127.0.0.1:18123/v1"; \
> echo "GET $$ROUTER_BASE/models"; \
> curl -fsS "$$ROUTER_BASE/models" | jq '.data[0:10]' || (echo "router NOT healthy"; exit 1)

router-logs:
> [ -f logs/router.log ] && tail -n 200 logs/router.log || echo "no logs/router.log yet"

router-restart: router-down router-up router-status
router-up:
> set -e
> test -d .venv-router || (uv venv .venv-router && . .venv-router/bin/activate && uv pip install fastapi uvicorn httpx anyio)
> OLLAMA_API_BASE="$$(grep '^OLLAMA_API_BASE=' .env | cut -d= -f2)"; \
> MLX_API_BASE="$$(grep '^MLX_API_BASE=' .env | cut -d= -f2)"; \
> : "$${OLLAMA_API_BASE:?OLLAMA_API_BASE missing in .env}"; \
> : "$${MLX_API_BASE:?MLX_API_BASE missing in .env}"; \
> if lsof -iTCP:18123 -sTCP:LISTEN >/dev/null 2>&1; then echo "router port 18123 already in use"; exit 1; fi
> pgrep -f "openai_router:app" >/dev/null 2>&1 && echo "router already running" || \
> (. .venv-router/bin/activate; \
>  OLLAMA_API_BASE="$$OLLAMA_API_BASE" MLX_API_BASE="$$MLX_API_BASE" \
>  python scripts/openai_router.py >logs/router.log 2>&1 & echo $$! > .router.pid; \
>  echo "router: http://127.0.0.1:18123/v1 (pid $$(cat .router.pid))")
.PHONY: launch
launch:
> $(MAKE) ollama-up
> $(MAKE) ollama-wait
> $(MAKE) up
> $(MAKE) kalliste
> $(MAKE) router-up
> $(MAKE) router-wait
> $(MAKE) sidecars-up       # (mlx only)
> $(MAKE) init
> echo ">> launch complete — router=$$(grep '^ROUTER_API_BASE=' .env | cut -d= -f2), mlx=$$(grep '^MLX_API_BASE=' .env | cut -d= -f2)"

.PHONY: router-wait
router-wait:
> ROUTER_BASE="$$(grep -E '^ROUTER_API_BASE=' .env | tail -1 | cut -d= -f2)"; \
> [ -z "$$ROUTER_BASE" ] && ROUTER_BASE="http://127.0.0.1:18123/v1"; \
> echo "Waiting for router at $$ROUTER_BASE ..."; \
> scripts/wait_for_http.sh "$$ROUTER_BASE/models" 60

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
.PHONY: mem-up mem-down mem mem-ps
MEM_PROFILE_CORE := core
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

mem-mode-full:
> bash scripts/set_compose_profiles.sh "$(ENV_FILE)" "$(MEM_PROFILE_FULL)"
> $(MAKE) mem-mode-show

mem-up:
> PROFILES="$(MEM_PROFILES)" $(MAKE) up

mem-down:
> PROFILES="$(MEM_PROFILES)" $(MAKE) down

mem-ps:
> PROFILES="$(MEM_PROFILES)" $(MAKE) ps

mem-up-core:
> PROFILES="$(MEM_PROFILE_CORE)" $(MAKE) up

mem-up-full:
> PROFILES="$(MEM_PROFILE_FULL)" $(MAKE) up

.PHONY: mem-up-lite mem-down-lite mem-ps-lite
mem-up-lite:
> docker compose -f docker-compose.lite.yml up -d --build

mem-down-lite:
> docker compose -f docker-compose.lite.yml down

mem-ps-lite:
> docker compose -f docker-compose.lite.yml ps

mem-up-release:
> docker compose -f docker-compose.yml -f docker-compose.release.lock.yml up -d --build

mem-up-lite-release:
> docker compose -f docker-compose.lite.yml -f docker-compose.release.lock.yml up -d --build

release-lock-verify:
> docker compose -f docker-compose.yml -f docker-compose.release.lock.yml config --services >/dev/null
> docker compose -f docker-compose.lite.yml -f docker-compose.release.lock.yml config --services >/dev/null
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
