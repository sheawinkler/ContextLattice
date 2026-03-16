.PHONY: login up down restart logs

# Use the gh_pat environment variable to log in to GitHub's container registry
login:
	@echo "Logging in to ghcr.io..."
	echo "$$gh_pat" | docker login ghcr.io -u sheawinkler --password-stdin

# Bring up the MCP stack (Qdrant, MindsDB, SuperGateway)
up: login
	@echo "Starting Qdrant, MindsDB, and MemoryMCP gateway..."
	docker compose --env-file .env up -d

# Bring down the stack


down:
	@echo "Stopping all services..."
	docker compose down

# Restart the stack
restart: down up

# Tail logs for all services
logs:
	docker compose logs -f --tail=200
