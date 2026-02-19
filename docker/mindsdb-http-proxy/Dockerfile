FROM public.ecr.aws/docker/library/python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# deps, then clean
RUN apt-get update && apt-get install -y --no-install-recommends build-essential && \
    pip install --no-cache-dir fastmcp fastapi uvicorn httpx anyio && \
    apt-get purge -y build-essential && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY scripts/ scripts/

EXPOSE 8004
CMD ["python", "scripts/mindsdb_http_proxy.py"]
