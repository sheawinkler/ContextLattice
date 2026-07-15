set shell := ["bash", "-c"]
repo_root := justfile_directory()

default:
	@just --list

orch-up:
    cd "{{repo_root}}" && ${DOCKER_API_VERSION:+DOCKER_API_VERSION=$DOCKER_API_VERSION }docker compose up -d gateway-go

orch-down:
    cd "{{repo_root}}" && ${DOCKER_API_VERSION:+DOCKER_API_VERSION=$DOCKER_API_VERSION }docker compose stop gateway-go || true

sidecar-config-check:
    test -n "${SIDECAR_START_CMD:-}" || { echo "SIDECAR_START_CMD is required" >&2; exit 2; }

sidecar-up: sidecar-config-check
    nohup bash -lc "$SIDECAR_START_CMD" > "${SIDECAR_LOG_PATH:-/tmp/contextlattice_sidecar.log}" 2>&1 & echo $! > "${SIDECAR_PID_PATH:-/tmp/contextlattice_sidecar.pid}"

sidecar-down:
    PID_PATH="${SIDECAR_PID_PATH:-/tmp/contextlattice_sidecar.pid}"; if [ -f "$PID_PATH" ]; then kill $(cat "$PID_PATH") 2>/dev/null || true; rm -f "$PID_PATH"; fi

devnet-up: sidecar-config-check orch-up sidecar-up

devnet-down: sidecar-down orch-down

devnet-smoke RUN_CARGO="0" CONFIG="config.toml" WALLET="wallet_devnet.json" DURATION="90" SKIP_SIDECAR_CHECK="1" BIN="":
    cd "{{repo_root}}" && test -z "$(git status --porcelain)" || { echo "devnet-smoke requires a clean source tree" >&2; exit 2; }; SOURCE_COMMIT="$(git rev-parse HEAD)"; SOURCE_TREE="$(git rev-parse 'HEAD^{tree}')"; export CONTEXTLATTICE_BUILD_VERSION=development CONTEXTLATTICE_BUILD_CHANNEL=local-smoke CONTEXTLATTICE_SOURCE_COMMIT="$SOURCE_COMMIT" CONTEXTLATTICE_SOURCE_TREE="$SOURCE_TREE" EXPECTED_GATEWAY_VERSION=development EXPECTED_GATEWAY_SOURCE_COMMIT="$SOURCE_COMMIT" EXPECTED_GATEWAY_SOURCE_TREE="$SOURCE_TREE" GATEWAY_IDENTITY_REQUIRED=1 ORCH_BUILD=1; CONFIG={{CONFIG}} WALLET={{WALLET}} SMOKE_DURATION={{DURATION}} RUN_CARGO_SMOKE={{RUN_CARGO}} BOOTSTRAP_ORCH=1 BOOTSTRAP_SIDECAR=0 SKIP_SIDECAR_CHECK={{SKIP_SIDECAR_CHECK}} MINDSDB_SMOKE=0 SMOKE_BINS={{BIN}} ./scripts/devnet_smoke.sh
