set shell := ["bash", "-c"]

default:
	@just --list

orch-up:
    cd ~/.mcp-servers/mem_mcp_lobehub && ${DOCKER_API_VERSION:+DOCKER_API_VERSION=$DOCKER_API_VERSION }docker compose up -d memmcp-orchestrator

orch-down:
    cd ~/.mcp-servers/mem_mcp_lobehub && ${DOCKER_API_VERSION:+DOCKER_API_VERSION=$DOCKER_API_VERSION }docker compose down memmcp-orchestrator || true

sidecar-up:
    cd ~/Documents/Projects/crypto_trader_post_training_needs_godmode_and_finalization && nohup poetry run uvicorn project.src.api.fastapi_server:app --host 0.0.0.0 --port 8288 > /tmp/devnet_sidecar.log 2>&1 & echo $! > /tmp/devnet_sidecar.pid

sidecar-down:
    if [ -f /tmp/devnet_sidecar.pid ]; then kill $(cat /tmp/devnet_sidecar.pid) 2>/dev/null || true; rm -f /tmp/devnet_sidecar.pid; fi

devnet-up: orch-up sidecar-up

devnet-down: sidecar-down orch-down

devnet-smoke CONFIG=config.toml WALLET=wallet_devnet.json DURATION=90 SKIP_SIDECAR_CHECK=0 BIN=unified_trader:
	CONFIG={{CONFIG}} WALLET={{WALLET}} SMOKE_DURATION={{DURATION}} BOOTSTRAP_ORCH=1 BOOTSTRAP_SIDECAR=1 SKIP_SIDECAR_CHECK={{SKIP_SIDECAR_CHECK}} SMOKE_BIN={{BIN}} ./scripts/devnet_smoke.sh
