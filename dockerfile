FROM python:3.11-slim
WORKDIR /app
RUN pip install --no-cache-dir mcp-server-qdrant
# The server chooses HOST/PORT via env (PORT defaults to 8000). Compose injects PORT=8002.
CMD ["mcp-server-qdrant","--transport","streamable-http"]
