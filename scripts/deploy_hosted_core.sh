#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CADDY_DIR="$ROOT_DIR/infra/caddy"
DOMAIN="${1:-}"
EMAIL="${2:-noemail@example.com}"

if [[ -z "$DOMAIN" ]]; then
  echo "Usage: $0 <domain> [tls-email]" >&2
  exit 1
fi

mkdir -p "$CADDY_DIR"
cat >"$CADDY_DIR/Caddyfile" <<EOF2
{
	servers {
		# HTTP/2 Bomb mitigation (2026-06): ContextLattice hosted ingress does not
		# require client-side h2/h3. Default to h1 until this Caddy build and edge
		# path have audited header-count and stalled-stream bounds.
		protocols {\$CADDY_INGRESS_PROTOCOLS:h1}
	}
}

$DOMAIN {
	encode gzip
	tls $EMAIL

	handle_path /mcp* {
		reverse_proxy mcp-hub:53130
	}

	handle_path /status* {
		reverse_proxy contextlattice-orchestrator:8075
	}

	handle {
		reverse_proxy contextlattice-orchestrator:8075
	}
}
EOF2

echo "[1/2] Bringing up core ContextLattice profile..."
(cd "$ROOT_DIR" && COMPOSE_PROFILES="core" docker compose up -d --build)

echo "[2/2] Launching Caddy reverse proxy for $DOMAIN ..."
(cd "$CADDY_DIR" && docker compose -f docker-compose.caddy.yml up -d)

echo "Deployment complete. Point DNS for $DOMAIN to this host's IP and you will have HTTPS ingress for /mcp and /status."
