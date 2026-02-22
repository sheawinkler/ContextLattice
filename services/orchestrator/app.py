from __future__ import annotations

import asyncio
import contextlib
import hashlib
import hmac
import json
import logging
import math
import time
import os
import re
import random
import sqlite3
import uuid
import pathlib
from collections import OrderedDict, deque
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Dict
from urllib.parse import unquote

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import HTMLResponse, JSONResponse, ORJSONResponse, PlainTextResponse
from pydantic import BaseModel, Field

try:
    from pymongo import MongoClient, ReturnDocument, UpdateOne  # type: ignore
except Exception:  # pragma: no cover - optional dependency
    MongoClient = None  # type: ignore
    ReturnDocument = None  # type: ignore
    UpdateOne = None  # type: ignore

try:
    import orjson  # type: ignore
except Exception:  # pragma: no cover - optional dependency
    orjson = None  # type: ignore

try:
    from prometheus_fastapi_instrumentator import Instrumentator  # type: ignore
except Exception:  # pragma: no cover - optional dependency
    Instrumentator = None  # type: ignore

try:
    from aiolimiter import AsyncLimiter  # type: ignore
except Exception:  # pragma: no cover - optional dependency
    AsyncLimiter = None  # type: ignore

try:
    from qdrant_client import AsyncQdrantClient  # type: ignore
    from qdrant_client.http import models as qdrant_models  # type: ignore
except Exception:  # pragma: no cover - optional dependency
    AsyncQdrantClient = None  # type: ignore
    qdrant_models = None  # type: ignore

MEMMCP_HTTP_URL = os.getenv("MEMMCP_HTTP_URL", "http://memorymcp-http:59081/mcp")
MEMMCP_HTTP_TIMEOUT_SECS = float(os.getenv("MEMMCP_HTTP_TIMEOUT_SECS", "30"))
MEMMCP_HTTP_RETRIES = int(os.getenv("MEMMCP_HTTP_RETRIES", "1"))
MEMMCP_HTTP_RETRY_DELAY_SECS = float(os.getenv("MEMMCP_HTTP_RETRY_DELAY_SECS", "0.5"))
MEMMCP_LIST_TIMEOUT_SECS = float(os.getenv("MEMMCP_LIST_TIMEOUT_SECS", "4"))
MEMMCP_READ_TIMEOUT_SECS = float(os.getenv("MEMMCP_READ_TIMEOUT_SECS", "15"))
ORCH_LOG_LEVEL = os.getenv("ORCH_LOG_LEVEL", "INFO").upper()
ORCH_LOG_FILE = os.getenv("ORCH_LOG_FILE", "").strip()
LANGFUSE_URL = os.getenv("LANGFUSE_URL", "http://langfuse:3000")
LANGFUSE_API_KEY = os.getenv("LANGFUSE_API_KEY")
QDRANT_LOCAL_URL = os.getenv("QDRANT_LOCAL_URL", os.getenv("QDRANT_URL", "http://qdrant:6333")).strip()
QDRANT_CLUSTER_ENDPOINT = os.getenv("QDRANT_CLUSTER_ENDPOINT", os.getenv("QDRANT-CLUSTER-ENDPOINT", "")).strip()
QDRANT_API_KEY = os.getenv("QDRANT_API_KEY", os.getenv("QDRANT-API-KEY", "")).strip()
QDRANT_USE_CLOUD = os.getenv("QDRANT_USE_CLOUD", "false").lower() in ("1", "true", "yes", "on")
QDRANT_CLOUD_FALLBACK = os.getenv("QDRANT_CLOUD_FALLBACK", "false").lower() in ("1", "true", "yes", "on")
QDRANT_GRPC_PREFER = os.getenv("QDRANT_GRPC_PREFER", "true").lower() in ("1", "true", "yes", "on")
QDRANT_GRPC_PORT = int(os.getenv("QDRANT_GRPC_PORT", "6334"))
QDRANT_CLOUD_GRPC_PORT = int(os.getenv("QDRANT_CLOUD_GRPC_PORT", str(QDRANT_GRPC_PORT)))
QDRANT_CLIENT_TIMEOUT_SECS = float(os.getenv("QDRANT_CLIENT_TIMEOUT_SECS", "30"))
QDRANT_URL = QDRANT_CLUSTER_ENDPOINT if QDRANT_USE_CLOUD and QDRANT_CLUSTER_ENDPOINT else QDRANT_LOCAL_URL
QDRANT_COLLECTION = os.getenv("ORCH_QDRANT_COLLECTION", "memmcp_notes")
MINDSDB_URL = os.getenv("MINDSDB_URL", "http://mindsdb:47334")
MINDSDB_USER = os.getenv("MINDSDB_USER", "mindsdb")
MINDSDB_PASSWORD = os.getenv("MINDSDB_PASSWORD", "")
MINDSDB_SQL_URL = f"{MINDSDB_URL.rstrip('/')}/api/sql/query"
MINDSDB_ENABLED = os.getenv("MINDSDB_ENABLED", "true").lower() in ("1", "true", "yes", "on")
MINDSDB_AUTOSYNC = os.getenv("MINDSDB_AUTOSYNC", "true").lower() in ("1", "true", "yes", "on")
MINDSDB_AUTOSYNC_DB = os.getenv("MINDSDB_AUTOSYNC_DB", "files")
MINDSDB_AUTOSYNC_TABLE = os.getenv("MINDSDB_AUTOSYNC_TABLE", "memory_events")
MINDSDB_AUTOSYNC_BOOTSTRAP = os.getenv("MINDSDB_AUTOSYNC_BOOTSTRAP", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
MINDSDB_AUTOSYNC_RETRIES = int(os.getenv("MINDSDB_AUTOSYNC_RETRIES", "5"))
MINDSDB_AUTOSYNC_BACKOFF_SECS = float(os.getenv("MINDSDB_AUTOSYNC_BACKOFF_SECS", "1.5"))
MINDSDB_AUTOSYNC_QUEUE_MAX = int(os.getenv("MINDSDB_AUTOSYNC_QUEUE_MAX", "500"))
MINDSDB_AUTOSYNC_BATCH_SIZE = max(1, int(os.getenv("MINDSDB_AUTOSYNC_BATCH_SIZE", "8")))
MINDSDB_FAIL_OPEN_ON_PERMANENT_ERROR = os.getenv(
    "MINDSDB_FAIL_OPEN_ON_PERMANENT_ERROR",
    "true",
).lower() in ("1", "true", "yes", "on")
MINDSDB_TRADING_AUTOSYNC = os.getenv("MINDSDB_TRADING_AUTOSYNC", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
MINDSDB_TRADING_DB = os.getenv("MINDSDB_TRADING_DB", MINDSDB_AUTOSYNC_DB)
MINDSDB_TRADING_TABLE = os.getenv("MINDSDB_TRADING_TABLE", "trading_metrics")
ORCH_API_KEY = os.getenv("MEMMCP_ORCHESTRATOR_API_KEY", "").strip()
LETTA_URL = os.getenv("LETTA_URL", "http://letta:8283")
LETTA_API_KEY = os.getenv("LETTA_API_KEY", "")
LETTA_REQUIRE_API_KEY = os.getenv("LETTA_REQUIRE_API_KEY", "false").lower() in ("1", "true", "yes", "on")
LETTA_AUTO_SESSION_ID = os.getenv("LETTA_AUTO_SESSION_ID", "memmcp-default").strip()
LETTA_AGENT_MODEL = os.getenv("LETTA_AGENT_MODEL", "ollama/qwen2.5-coder:7b").strip()
LETTA_AGENT_EMBEDDING = os.getenv("LETTA_AGENT_EMBEDDING", "").strip()
LETTA_REQUEST_TIMEOUT_SECS = float(os.getenv("LETTA_REQUEST_TIMEOUT_SECS", "240"))
LETTA_AGENT_VERIFY_INTERVAL_SECS = float(os.getenv("LETTA_AGENT_VERIFY_INTERVAL_SECS", "300"))
LETTA_ARCHIVAL_MAX_CHARS = int(os.getenv("LETTA_ARCHIVAL_MAX_CHARS", "400"))
LETTA_DISABLE_ON_TRANSIENT_ERRORS = os.getenv(
    "LETTA_DISABLE_ON_TRANSIENT_ERRORS",
    "true",
).lower() in ("1", "true", "yes", "on")
LETTA_TRANSIENT_ERROR_THRESHOLD = int(os.getenv("LETTA_TRANSIENT_ERROR_THRESHOLD", "60"))
LETTA_ARCHIVAL_INCLUDE_CONTENT = os.getenv("LETTA_ARCHIVAL_INCLUDE_CONTENT", "false").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
MONGO_RAW_URI = os.getenv("MONGODB_URI", "mongodb://mongo:27017")
MONGO_RAW_DB = os.getenv("MONGO_RAW_DB", "memmcp_raw")
MONGO_RAW_COLLECTION = os.getenv("MONGO_RAW_COLLECTION", "memory_write_events")
MONGO_RAW_ENABLED = os.getenv("MONGO_RAW_ENABLED", "true").lower() in ("1", "true", "yes", "on")
MONGO_RAW_CONNECT_TIMEOUT_MS = int(os.getenv("MONGO_RAW_CONNECT_TIMEOUT_MS", "5000"))
MONGO_RAW_SERVER_SELECTION_TIMEOUT_MS = int(os.getenv("MONGO_RAW_SERVER_SELECTION_TIMEOUT_MS", "5000"))
MONGO_RAW_SOCKET_TIMEOUT_MS = int(os.getenv("MONGO_RAW_SOCKET_TIMEOUT_MS", "15000"))
MONGO_RAW_WAIT_QUEUE_TIMEOUT_MS = int(os.getenv("MONGO_RAW_WAIT_QUEUE_TIMEOUT_MS", "5000"))
MONGO_RAW_MAX_POOL_SIZE = max(10, int(os.getenv("MONGO_RAW_MAX_POOL_SIZE", "200")))
MONGO_RAW_MIN_POOL_SIZE = max(0, int(os.getenv("MONGO_RAW_MIN_POOL_SIZE", "0")))
PILOT_CONTACT_EMAIL = os.getenv("PILOT_CONTACT_EMAIL", "").strip()
PILOT_CONTACT_URL = os.getenv("PILOT_CONTACT_URL", "").strip()
LEARNING_LOOP_ENABLED = os.getenv("LEARNING_LOOP_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
HIGH_RISK_APPROVAL_REQUIRED = os.getenv("HIGH_RISK_APPROVAL_REQUIRED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
HIGH_RISK_ACTIONS = [
    item.strip()
    for item in os.getenv(
        "HIGH_RISK_ACTIONS",
        "payment,transfer_funds,delete_data,infra_change,prod_deploy,send_external_message,credential_change,purchase",
    ).split(",")
    if item.strip()
]
PREFERENCE_MAX_ENTRIES = int(os.getenv("PREFERENCE_MAX_ENTRIES", "25"))
FEEDBACK_MAX_CONTENT = int(os.getenv("FEEDBACK_MAX_CONTENT", "2000"))
DEFAULT_TOPIC_ROOT = os.getenv("DEFAULT_TOPIC_ROOT", "root")

EMBEDDING_PROVIDER = os.getenv("ORCH_EMBED_PROVIDER", os.getenv("EMBEDDING_PROVIDER", "cheap")).lower()
EMBEDDING_MODEL = os.getenv("ORCH_EMBED_MODEL", os.getenv("EMBEDDING_MODEL", "cheap-embed-v1"))
EMBEDDING_BASE_URL = os.getenv("EMBEDDING_BASE_URL", os.getenv("OPENAI_API_BASE"))
EMBEDDING_API_KEY = os.getenv("EMBEDDING_API_KEY", os.getenv("OPENAI_API_KEY"))
OLLAMA_BASE_URL = os.getenv("OLLAMA_BASE_URL", "http://ollama:11434")
FALLBACK_EMBED_DIM = int(os.getenv("ORCH_EMBED_DIM", os.getenv("EMBEDDING_DIM", "32")))
EMBEDDING_FAIL_OPEN = os.getenv("ORCH_EMBED_FAIL_OPEN", "true").lower() in ("1", "true", "yes", "on")
EMBEDDING_CACHE_ENABLED = os.getenv("EMBEDDING_CACHE_ENABLED", "true").lower() in ("1", "true", "yes", "on")
EMBEDDING_CACHE_MAX_KEYS = int(os.getenv("EMBEDDING_CACHE_MAX_KEYS", "50000"))
RETRIEVAL_SOURCES_ENV = os.getenv(
    "ORCH_RETRIEVAL_SOURCES",
    "qdrant,mongo_raw,mindsdb,letta,memory_bank",
)
RETRIEVAL_MONGO_SCAN_LIMIT = int(os.getenv("ORCH_RETRIEVAL_MONGO_SCAN_LIMIT", "400"))
RETRIEVAL_MINDSDB_SCAN_LIMIT = int(os.getenv("ORCH_RETRIEVAL_MINDSDB_SCAN_LIMIT", "300"))
RETRIEVAL_MEMORY_SCAN_LIMIT = int(os.getenv("ORCH_RETRIEVAL_MEMORY_SCAN_LIMIT", "36"))
RETRIEVAL_MEMORY_PROJECT_LIMIT = int(os.getenv("ORCH_RETRIEVAL_MEMORY_PROJECT_LIMIT", "12"))
RETRIEVAL_MEMORY_FILES_PER_PROJECT = int(os.getenv("ORCH_RETRIEVAL_MEMORY_FILES_PER_PROJECT", "40"))
RETRIEVAL_LETTA_TOP_K_FACTOR = float(os.getenv("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR", "2.0"))
QDRANT_EMBED_TIMEOUT_SECS = float(os.getenv("ORCH_QDRANT_EMBED_TIMEOUT_SECS", "2.0"))
RETRIEVAL_QDRANT_TIMEOUT_SECS = float(os.getenv("ORCH_RETRIEVAL_QDRANT_TIMEOUT_SECS", "8"))
RETRIEVAL_MONGO_TIMEOUT_SECS = float(os.getenv("ORCH_RETRIEVAL_MONGO_TIMEOUT_SECS", "6"))
RETRIEVAL_MINDSDB_TIMEOUT_SECS = float(os.getenv("ORCH_RETRIEVAL_MINDSDB_TIMEOUT_SECS", "8"))
RETRIEVAL_LETTA_TIMEOUT_SECS = float(os.getenv("ORCH_RETRIEVAL_LETTA_TIMEOUT_SECS", "4"))
RETRIEVAL_MEMORY_TIMEOUT_SECS = float(os.getenv("ORCH_RETRIEVAL_MEMORY_TIMEOUT_SECS", "3"))
RETRIEVAL_ENABLE_STAGED_FETCH = os.getenv(
    "ORCH_RETRIEVAL_ENABLE_STAGED_FETCH",
    "true",
).lower() in ("1", "true", "yes", "on")
RETRIEVAL_FAST_SOURCES_ENV = os.getenv(
    "ORCH_RETRIEVAL_FAST_SOURCES",
    "qdrant,mongo_raw,mindsdb",
)
RETRIEVAL_SLOW_SOURCES_ENV = os.getenv(
    "ORCH_RETRIEVAL_SLOW_SOURCES",
    "letta,memory_bank",
)
RETRIEVAL_SLOW_SOURCE_MIN_RESULTS = int(
    os.getenv("ORCH_RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", "6")
)
RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE = float(
    os.getenv("ORCH_RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", "0.6")
)
RETRIEVAL_ENABLE_LEARNING_RERANK = os.getenv(
    "ORCH_RETRIEVAL_ENABLE_LEARNING_RERANK",
    "true",
).lower() in ("1", "true", "yes", "on")
RETRIEVAL_LEARNING_POSITIVE_BOOST = float(os.getenv("ORCH_RETRIEVAL_LEARNING_POSITIVE_BOOST", "0.06"))
RETRIEVAL_LEARNING_NEGATIVE_PENALTY = float(os.getenv("ORCH_RETRIEVAL_LEARNING_NEGATIVE_PENALTY", "0.08"))
TRADING_HISTORY_LIMIT = int(os.getenv("TRADING_HISTORY_LIMIT", "256"))
TRADING_HISTORY_PATH = Path(
    os.getenv(
        "TRADING_HISTORY_PATH",
        str(Path(__file__).resolve().parent / "data" / "trading_metrics.ndjson"),
    )
)
STRATEGY_HISTORY_LIMIT = int(os.getenv("STRATEGY_HISTORY_LIMIT", "256"))
STRATEGY_HISTORY_PATH = Path(
    os.getenv(
        "STRATEGY_HISTORY_PATH",
        str(Path(__file__).resolve().parent / "data" / "strategy_metrics.ndjson"),
    )
)
SIGNAL_PROJECT = os.getenv("SIGNAL_PROJECT", "sol_scaler_signals")
SIGNAL_HISTORY_LIMIT = int(os.getenv("SIGNAL_HISTORY_LIMIT", "256"))
SIGNAL_FETCH_LIMIT = int(os.getenv("SIGNAL_FETCH_LIMIT", "64"))
SIGNAL_REFRESH_SECONDS = int(os.getenv("SIGNAL_REFRESH_SECONDS", "120"))
SIGNAL_HISTORY_PATH = Path(
    os.getenv(
        "SIGNAL_HISTORY_PATH",
        str(Path(__file__).resolve().parent / "data" / "solana_signals.ndjson"),
    )
)
OVERRIDE_PROJECT = os.getenv("OVERRIDE_PROJECT", "sol_scaler_overrides")
OVERRIDE_HISTORY_LIMIT = int(os.getenv("OVERRIDE_HISTORY_LIMIT", "256"))
OVERRIDE_FETCH_LIMIT = int(os.getenv("OVERRIDE_FETCH_LIMIT", "64"))
OVERRIDE_REFRESH_SECONDS = int(os.getenv("OVERRIDE_REFRESH_SECONDS", "120"))
OVERRIDE_HISTORY_PATH = Path(
    os.getenv(
        "OVERRIDE_HISTORY_PATH",
        str(Path(__file__).resolve().parent / "data" / "solana_overrides.ndjson"),
    )
)
MEMORY_WRITE_HISTORY_LIMIT = int(os.getenv("MEMORY_WRITE_HISTORY_LIMIT", "200"))
MEMORY_WRITE_HISTORY_PATH = Path(
    os.getenv(
        "MEMORY_WRITE_HISTORY_PATH",
        str(Path(__file__).resolve().parent / "data" / "memory_write_history.ndjson"),
    )
)
MEMORY_WRITE_ASYNC = os.getenv("MEMORY_WRITE_ASYNC", "true").lower() in ("1", "true", "yes", "on")
MEMORY_BANK_QUEUE_MAX = int(os.getenv("MEMORY_BANK_QUEUE_MAX", "2000"))
MEMORY_BANK_WORKERS = int(os.getenv("MEMORY_BANK_WORKERS", "4"))
MEMORY_WRITE_QUEUE_MAX = int(os.getenv("MEMORY_WRITE_QUEUE_MAX", "2000"))
MEMORY_WRITE_WORKERS = int(os.getenv("MEMORY_WRITE_WORKERS", "4"))
MEMORY_WRITE_DEDUP_ENABLED = os.getenv("MEMORY_WRITE_DEDUP_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
MEMORY_WRITE_DEDUP_WINDOW_SECS = float(os.getenv("MEMORY_WRITE_DEDUP_WINDOW_SECS", "120"))
MEMORY_WRITE_DEDUP_MAX_KEYS = int(os.getenv("MEMORY_WRITE_DEDUP_MAX_KEYS", "10000"))
MEMORY_WRITE_LATEST_HASH_DEDUP_ENABLED = os.getenv(
    "MEMORY_WRITE_LATEST_HASH_DEDUP_ENABLED",
    "true",
).lower() in ("1", "true", "yes", "on")
MEMORY_WRITE_LATEST_HASH_DEDUP_MAX_KEYS = int(os.getenv("MEMORY_WRITE_LATEST_HASH_DEDUP_MAX_KEYS", "50000"))
HOT_MEMORY_FILE_SUFFIXES = [
    suffix.strip().lower()
    for suffix in os.getenv("HOT_MEMORY_FILE_SUFFIXES", "__latest.json").split(",")
    if suffix.strip()
]
HOT_MEMORY_ROLLUP_ENABLED = os.getenv("HOT_MEMORY_ROLLUP_ENABLED", "true").lower() in ("1", "true", "yes", "on")
HOT_MEMORY_ROLLUP_FLUSH_SECS = float(os.getenv("HOT_MEMORY_ROLLUP_FLUSH_SECS", "20"))
HOT_MEMORY_ROLLUP_SUFFIX = os.getenv("HOT_MEMORY_ROLLUP_SUFFIX", "__rollup.json").strip() or "__rollup.json"
MINDSDB_FANOUT_WORKERS = int(os.getenv("MINDSDB_FANOUT_WORKERS", "1"))
LETTA_FANOUT_WORKERS = int(os.getenv("LETTA_FANOUT_WORKERS", "1"))
FANOUT_MAX_ATTEMPTS = int(os.getenv("FANOUT_MAX_ATTEMPTS", "40"))
FANOUT_RETRY_BASE_SECS = float(os.getenv("FANOUT_RETRY_BASE_SECS", "2"))
FANOUT_RETRY_MAX_SECS = float(os.getenv("FANOUT_RETRY_MAX_SECS", "900"))
FANOUT_BATCH_SIZE = int(os.getenv("FANOUT_BATCH_SIZE", "24"))
FANOUT_POLL_SECS = float(os.getenv("FANOUT_POLL_SECS", "2.0"))
FANOUT_RUNNING_STALE_SECS = int(os.getenv("FANOUT_RUNNING_STALE_SECS", "120"))
FANOUT_SUMMARY_TIMEOUT_SECS = float(os.getenv("FANOUT_SUMMARY_TIMEOUT_SECS", "20.0"))
FANOUT_SUMMARY_CACHE_TTL_SECS = float(os.getenv("FANOUT_SUMMARY_CACHE_TTL_SECS", "6.0"))
FANOUT_QDRANT_RATE_LIMIT_PER_SEC = float(os.getenv("FANOUT_QDRANT_RATE_LIMIT_PER_SEC", "40"))
FANOUT_MINDSDB_RATE_LIMIT_PER_SEC = float(os.getenv("FANOUT_MINDSDB_RATE_LIMIT_PER_SEC", "15"))
FANOUT_LETTA_RATE_LIMIT_PER_SEC = float(os.getenv("FANOUT_LETTA_RATE_LIMIT_PER_SEC", "6"))
FANOUT_LANGFUSE_RATE_LIMIT_PER_SEC = float(os.getenv("FANOUT_LANGFUSE_RATE_LIMIT_PER_SEC", "20"))
FANOUT_QDRANT_BULK_SIZE = max(1, int(os.getenv("FANOUT_QDRANT_BULK_SIZE", "16")))
FANOUT_MINDSDB_BULK_SIZE = max(1, int(os.getenv("FANOUT_MINDSDB_BULK_SIZE", "12")))
FANOUT_MONGO_BULK_SIZE = max(1, int(os.getenv("FANOUT_MONGO_BULK_SIZE", "24")))
FANOUT_LANGFUSE_BULK_SIZE = max(1, int(os.getenv("FANOUT_LANGFUSE_BULK_SIZE", "24")))
FANOUT_LETTA_BULK_SIZE = max(1, int(os.getenv("FANOUT_LETTA_BULK_SIZE", "8")))
FANOUT_LETTA_BATCH_CONCURRENCY = max(1, int(os.getenv("FANOUT_LETTA_BATCH_CONCURRENCY", "2")))
FANOUT_COALESCE_ENABLED = os.getenv("FANOUT_COALESCE_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
FANOUT_COALESCE_WINDOW_SECS = float(os.getenv("FANOUT_COALESCE_WINDOW_SECS", "6"))
FANOUT_COALESCE_TARGETS_ENV = os.getenv(
    "FANOUT_COALESCE_TARGETS",
    "qdrant,mindsdb,letta,langfuse",
)
FANOUT_BACKPRESSURE_ENABLED = os.getenv("FANOUT_BACKPRESSURE_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
FANOUT_BACKPRESSURE_QUEUE_HIGH_WATERMARK = float(os.getenv("FANOUT_BACKPRESSURE_QUEUE_HIGH_WATERMARK", "0.65"))
FANOUT_BACKPRESSURE_MAX_SLEEP_SECS = float(os.getenv("FANOUT_BACKPRESSURE_MAX_SLEEP_SECS", "1.25"))
FANOUT_BACKPRESSURE_TARGETS_ENV = os.getenv("FANOUT_BACKPRESSURE_TARGETS", "letta,langfuse")
FANOUT_BACKPRESSURE_LOG_COOLDOWN_SECS = float(os.getenv("FANOUT_BACKPRESSURE_LOG_COOLDOWN_SECS", "30"))
FANOUT_OUTBOX_BACKEND = os.getenv("FANOUT_OUTBOX_BACKEND", "sqlite").strip().lower()
FANOUT_OUTBOX_MONGO_URI = os.getenv("FANOUT_OUTBOX_MONGO_URI", MONGO_RAW_URI).strip()
FANOUT_OUTBOX_MONGO_DB = os.getenv("FANOUT_OUTBOX_MONGO_DB", MONGO_RAW_DB).strip()
FANOUT_OUTBOX_MONGO_COLLECTION = os.getenv(
    "FANOUT_OUTBOX_MONGO_COLLECTION",
    "fanout_outbox",
).strip()
FANOUT_OUTBOX_AUTO_PROMOTE_MONGO_ON_SQLITE_IO_ERROR = os.getenv(
    "FANOUT_OUTBOX_AUTO_PROMOTE_MONGO_ON_SQLITE_IO_ERROR",
    "true",
).lower() in ("1", "true", "yes", "on")
FANOUT_OUTBOX_FALLBACK_TO_SQLITE = os.getenv(
    "FANOUT_OUTBOX_FALLBACK_TO_SQLITE",
    "true",
).lower() in ("1", "true", "yes", "on")
FANOUT_OUTBOX_GC_ENABLED = os.getenv("FANOUT_OUTBOX_GC_ENABLED", "1").lower() in ("1", "true", "yes", "on")
FANOUT_OUTBOX_GC_INTERVAL_SECS = float(os.getenv("FANOUT_OUTBOX_GC_INTERVAL_SECS", "900"))
FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS = int(os.getenv("FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS", "24"))
FANOUT_OUTBOX_FAILED_RETENTION_HOURS = int(os.getenv("FANOUT_OUTBOX_FAILED_RETENTION_HOURS", "168"))
FANOUT_OUTBOX_STALE_PENDING_HOURS = int(os.getenv("FANOUT_OUTBOX_STALE_PENDING_HOURS", "24"))
FANOUT_OUTBOX_STALE_TARGETS_ENV = os.getenv("FANOUT_OUTBOX_STALE_TARGETS", "")
FANOUT_OUTBOX_GC_VACUUM = os.getenv("FANOUT_OUTBOX_GC_VACUUM", "1").lower() in ("1", "true", "yes", "on")
FANOUT_OUTBOX_GC_VACUUM_MIN_DELETED = int(os.getenv("FANOUT_OUTBOX_GC_VACUUM_MIN_DELETED", "500"))
FANOUT_OUTBOX_GC_VACUUM_MIN_INTERVAL_SECS = float(
    os.getenv("FANOUT_OUTBOX_GC_VACUUM_MIN_INTERVAL_SECS", "3600")
)
FANOUT_OUTBOX_GC_TIMEOUT_SECS = float(os.getenv("FANOUT_OUTBOX_GC_TIMEOUT_SECS", "45"))
LOW_VALUE_FILE_SUFFIXES_ENV = os.getenv("LOW_VALUE_FILE_SUFFIXES", "__latest.json,__rollup.json")
LOW_VALUE_TOPIC_PREFIXES_ENV = os.getenv(
    "LOW_VALUE_TOPIC_PREFIXES",
    "telemetry,metrics,signals,overrides,perf,tmp",
)
LETTA_ADMISSION_ENABLED = os.getenv("LETTA_ADMISSION_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
LETTA_ADMISSION_BACKLOG_SOFT_LIMIT = int(os.getenv("LETTA_ADMISSION_BACKLOG_SOFT_LIMIT", "800"))
LETTA_ADMISSION_BACKLOG_HARD_LIMIT = int(os.getenv("LETTA_ADMISSION_BACKLOG_HARD_LIMIT", "2500"))
LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS = int(
    os.getenv("LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS", "72")
)
LETTA_ADMISSION_LOG_COOLDOWN_SECS = float(os.getenv("LETTA_ADMISSION_LOG_COOLDOWN_SECS", "30"))
SINK_RETENTION_ENABLED = os.getenv("SINK_RETENTION_ENABLED", "true").lower() in ("1", "true", "yes", "on")
SINK_RETENTION_INTERVAL_SECS = float(os.getenv("SINK_RETENTION_INTERVAL_SECS", "2100"))
SINK_RETENTION_TIMEOUT_SECS = float(os.getenv("SINK_RETENTION_TIMEOUT_SECS", "240"))
SINK_RETENTION_SCAN_LIMIT = max(100, int(os.getenv("SINK_RETENTION_SCAN_LIMIT", "5000")))
SINK_RETENTION_DELETE_BATCH = max(1, int(os.getenv("SINK_RETENTION_DELETE_BATCH", "128")))
SINK_RETENTION_MAX_DELETES_PER_RUN = max(1, int(os.getenv("SINK_RETENTION_MAX_DELETES_PER_RUN", "5000")))
QDRANT_LOW_VALUE_RETENTION_HOURS = float(os.getenv("QDRANT_LOW_VALUE_RETENTION_HOURS", "72"))
LETTA_LOW_VALUE_RETENTION_HOURS = float(os.getenv("LETTA_LOW_VALUE_RETENTION_HOURS", "72"))
MONGO_RAW_LOW_VALUE_RETENTION_HOURS = float(os.getenv("MONGO_RAW_LOW_VALUE_RETENTION_HOURS", "0"))
LETTA_RETENTION_PAGE_LIMIT = max(10, int(os.getenv("LETTA_RETENTION_PAGE_LIMIT", "100")))
LETTA_RETENTION_MAX_DELETES_PER_RUN = max(1, int(os.getenv("LETTA_RETENTION_MAX_DELETES_PER_RUN", "500")))
MINDSDB_AUTOSYNC_TABLE_FALLBACK_SUFFIX = os.getenv("MINDSDB_AUTOSYNC_TABLE_FALLBACK_SUFFIX", "_v2")
TOPIC_INDEX_PATH = Path(
    os.getenv(
        "TOPIC_INDEX_PATH",
        str(Path(__file__).resolve().parent / "data" / "topic_index.json"),
    )
)
TASK_DB_PATH = Path(
    os.getenv(
        "TASK_DB_PATH",
        str(Path(__file__).resolve().parent / "data" / "agent_tasks.db"),
    )
)
TASK_DB_TIMEOUT = float(os.getenv("TASK_DB_TIMEOUT", "5.0"))
TASK_DB_LOCK_RETRIES = int(os.getenv("TASK_DB_LOCK_RETRIES", "8"))
TASK_DB_LOCK_BACKOFF_SECS = float(os.getenv("TASK_DB_LOCK_BACKOFF_SECS", "0.15"))
TASK_SCHEDULER_ENABLED = os.getenv("TASK_SCHEDULER_ENABLED", "true").lower() in ("1", "true", "yes", "on")
TASK_INTERNAL_WORKERS_ENABLED = os.getenv("TASK_INTERNAL_WORKERS_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
AGENT_TASK_WORKERS = max(0, int(os.getenv("AGENT_TASK_WORKERS", "2")))
TASK_WORKER_POLL_SECS = float(os.getenv("TASK_WORKER_POLL_SECS", "1.2"))
TASK_LEASE_SECS = max(15, int(os.getenv("TASK_LEASE_SECS", "120")))
TASK_DEFAULT_MAX_ATTEMPTS = max(1, int(os.getenv("TASK_DEFAULT_MAX_ATTEMPTS", "4")))
TASK_RETRY_BASE_SECS = max(0.2, float(os.getenv("TASK_RETRY_BASE_SECS", "2.0")))
TASK_RETRY_MAX_SECS = max(TASK_RETRY_BASE_SECS, float(os.getenv("TASK_RETRY_MAX_SECS", "300")))
TASK_CALLBACK_TIMEOUT_SECS = max(1.0, float(os.getenv("TASK_CALLBACK_TIMEOUT_SECS", "15")))
TASK_CALLBACK_ALLOWED_HOSTS_ENV = os.getenv("TASK_CALLBACK_ALLOWED_HOSTS", "localhost,127.0.0.1")
TASK_RESULT_WRITEBACK_ENABLED = os.getenv("TASK_RESULT_WRITEBACK_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
TASK_PROVIDER_CHAT_ENABLED = os.getenv("TASK_PROVIDER_CHAT_ENABLED", "false").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
TASK_PROVIDER_CHAT_MODEL = os.getenv("TASK_PROVIDER_CHAT_MODEL", "").strip()
TASK_PROVIDER_CHAT_URL = os.getenv("TASK_PROVIDER_CHAT_URL", EMBEDDING_BASE_URL or "").strip()
TASK_PROVIDER_CHAT_API_KEY = os.getenv("TASK_PROVIDER_CHAT_API_KEY", EMBEDDING_API_KEY or "").strip()
TASK_ALLOWED_ACTIONS_ENV = os.getenv(
    "TASK_ALLOWED_ACTIONS",
    "memory_write,memory_search,messaging_command,http_callback,provider_chat",
)
SIDECAR_HEALTH_HISTORY_LIMIT = int(os.getenv("SIDECAR_HEALTH_HISTORY_LIMIT", "200"))
MEMMCP_ENV = os.getenv("MEMMCP_ENV", "development").strip().lower()
ORCH_SECURITY_STRICT = os.getenv("ORCH_SECURITY_STRICT", "true").lower() in ("1", "true", "yes", "on")
ORCH_PUBLIC_STATUS = os.getenv(
    "ORCH_PUBLIC_STATUS",
    "false" if MEMMCP_ENV in ("production", "prod") else "true",
).lower() in ("1", "true", "yes", "on")
ORCH_PUBLIC_DOCS = os.getenv(
    "ORCH_PUBLIC_DOCS",
    "false" if MEMMCP_ENV in ("production", "prod") else "true",
).lower() in ("1", "true", "yes", "on")
ORCH_HTTP_REQUEST_LOG_SUCCESS_SAMPLE_RATE = float(
    os.getenv("ORCH_HTTP_REQUEST_LOG_SUCCESS_SAMPLE_RATE", "0.1")
)
ORCH_HTTP_REQUEST_LOG_SLOW_MS = float(os.getenv("ORCH_HTTP_REQUEST_LOG_SLOW_MS", "1500"))
ORCH_PROMETHEUS_ENABLED = os.getenv("ORCH_PROMETHEUS_ENABLED", "true").lower() in ("1", "true", "yes", "on")
ORCH_PROMETHEUS_ENDPOINT = os.getenv("ORCH_PROMETHEUS_ENDPOINT", "/metrics").strip() or "/metrics"
if not ORCH_PROMETHEUS_ENDPOINT.startswith("/"):
    ORCH_PROMETHEUS_ENDPOINT = f"/{ORCH_PROMETHEUS_ENDPOINT}"
ORCH_PROMETHEUS_PUBLIC = os.getenv("ORCH_PROMETHEUS_PUBLIC", "false").lower() in ("1", "true", "yes", "on")
MESSAGING_INTEGRATIONS_ENABLED = os.getenv("MESSAGING_INTEGRATIONS_ENABLED", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
MESSAGING_WEBHOOK_PUBLIC = os.getenv("MESSAGING_WEBHOOK_PUBLIC", "true").lower() in ("1", "true", "yes", "on")
MESSAGING_COMMAND_HANDLE = os.getenv("MESSAGING_COMMAND_HANDLE", "@ContextLattice").strip() or "@ContextLattice"
MESSAGING_DEFAULT_PROJECT = os.getenv("MESSAGING_DEFAULT_PROJECT", "messaging").strip() or "messaging"
MESSAGING_SEARCH_LIMIT = max(1, int(os.getenv("MESSAGING_SEARCH_LIMIT", "4")))
MESSAGING_MAX_RESPONSE_CHARS = max(240, int(os.getenv("MESSAGING_MAX_RESPONSE_CHARS", "1200")))
MESSAGING_ORCH_SELF_URL = os.getenv("MESSAGING_ORCH_SELF_URL", "http://127.0.0.1:8075").strip()
TELEGRAM_BOT_TOKEN = os.getenv("TELEGRAM_BOT_TOKEN", "").strip()
TELEGRAM_BOT_USERNAME = os.getenv("TELEGRAM_BOT_USERNAME", "ContextLattice").strip() or "ContextLattice"
TELEGRAM_DEFAULT_PROJECT = os.getenv("TELEGRAM_DEFAULT_PROJECT", MESSAGING_DEFAULT_PROJECT).strip()
TELEGRAM_TOPIC_ROOT = os.getenv("TELEGRAM_TOPIC_ROOT", "channels/telegram").strip() or "channels/telegram"
TELEGRAM_WEBHOOK_SECRET = os.getenv("TELEGRAM_WEBHOOK_SECRET", "").strip()
SLACK_BOT_TOKEN = os.getenv("SLACK_BOT_TOKEN", "").strip()
SLACK_SIGNING_SECRET = os.getenv("SLACK_SIGNING_SECRET", "").strip()
SLACK_DEFAULT_PROJECT = os.getenv("SLACK_DEFAULT_PROJECT", MESSAGING_DEFAULT_PROJECT).strip()
SLACK_TOPIC_ROOT = os.getenv("SLACK_TOPIC_ROOT", "channels/slack").strip() or "channels/slack"
OPENCLAW_DEFAULT_PROJECT = os.getenv("OPENCLAW_DEFAULT_PROJECT", MESSAGING_DEFAULT_PROJECT).strip()
IRONCLAW_DEFAULT_PROJECT = os.getenv(
    "IRONCLAW_DEFAULT_PROJECT",
    OPENCLAW_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
).strip()
IRONCLAW_INTEGRATION_ENABLED = os.getenv("IRONCLAW_INTEGRATION_ENABLED", "false").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
MESSAGING_OPENCLAW_STRICT_SECURITY = os.getenv("MESSAGING_OPENCLAW_STRICT_SECURITY", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)
SECRETS_STORAGE_MODE = os.getenv("SECRETS_STORAGE_MODE", "redact").strip().lower()
if SECRETS_STORAGE_MODE not in {"allow", "redact", "block"}:
    SECRETS_STORAGE_MODE = "redact"
ORCH_MISSING_FILE_AUTOSTUB = os.getenv("ORCH_MISSING_FILE_AUTOSTUB", "true").lower() in (
    "1",
    "true",
    "yes",
    "on",
)

MCP_HEADERS = {
    "content-type": "application/json",
    "accept": "application/json, text/event-stream",
    "MCP-Protocol-Version": os.getenv("MCP_PROTOCOL_VERSION", "2024-11-05").strip() or "2024-11-05",
    "MCP-Transport": "streamable-http",
}
MCP_CLIENT_LIMITS = httpx.Limits(max_connections=50, max_keepalive_connections=20)
MCP_CLIENT_TIMEOUT = httpx.Timeout(MEMMCP_HTTP_TIMEOUT_SECS)
MCP_CLIENT: httpx.AsyncClient | None = None
ORCH_SHARED_HTTP_MAX_CONNECTIONS = max(10, int(os.getenv("ORCH_SHARED_HTTP_MAX_CONNECTIONS", "120")))
ORCH_SHARED_HTTP_MAX_KEEPALIVE_CONNECTIONS = max(
    10,
    int(os.getenv("ORCH_SHARED_HTTP_MAX_KEEPALIVE_CONNECTIONS", "60")),
)
SHARED_SERVICE_CLIENT_LIMITS = httpx.Limits(
    max_connections=ORCH_SHARED_HTTP_MAX_CONNECTIONS,
    max_keepalive_connections=ORCH_SHARED_HTTP_MAX_KEEPALIVE_CONNECTIONS,
)
MINDSDB_CLIENT_TIMEOUT = httpx.Timeout(30.0)
LETTA_CLIENT_TIMEOUT = httpx.Timeout(max(30.0, LETTA_REQUEST_TIMEOUT_SECS))
LANGFUSE_CLIENT_TIMEOUT = httpx.Timeout(10.0)
QDRANT_CLIENT: AsyncQdrantClient | None = None
QDRANT_CLOUD_CLIENT: AsyncQdrantClient | None = None
MINDSDB_CLIENT: httpx.AsyncClient | None = None
LETTA_CLIENT: httpx.AsyncClient | None = None
LANGFUSE_CLIENT: httpx.AsyncClient | None = None
MCP_SESSION_HEADER = "mcp-session-id"
MCP_CLIENT_NAME = os.getenv("MCP_CLIENT_NAME", "memmcp-orchestrator").strip() or "memmcp-orchestrator"
MCP_CLIENT_VERSION = os.getenv("MCP_CLIENT_VERSION", "0.1.0").strip() or "0.1.0"

FANOUT_TARGET_MONGO_RAW = "mongo_raw"
FANOUT_TARGET_QDRANT = "qdrant"
FANOUT_TARGET_LANGFUSE = "langfuse"
FANOUT_TARGET_MINDSDB = "mindsdb"
FANOUT_TARGET_LETTA = "letta"
FANOUT_TARGETS = (
    FANOUT_TARGET_MONGO_RAW,
    FANOUT_TARGET_QDRANT,
    FANOUT_TARGET_LANGFUSE,
    FANOUT_TARGET_MINDSDB,
    FANOUT_TARGET_LETTA,
)


def _normalize_fanout_target_csv(raw: str | None) -> list[str]:
    requested = [item.strip().lower() for item in str(raw or "").split(",") if item.strip()]
    normalized: list[str] = []
    seen: set[str] = set()
    for target in requested:
        if target not in FANOUT_TARGETS or target in seen:
            continue
        seen.add(target)
        normalized.append(target)
    return normalized


def _normalize_lower_csv(raw: str | None) -> list[str]:
    return [item.strip().lower() for item in str(raw or "").split(",") if item.strip()]


def _normalize_task_action_csv(raw: str | None) -> tuple[str, ...]:
    allowed_defaults = ("memory_write", "memory_search", "messaging_command", "http_callback", "provider_chat")
    requested = [item.strip().lower() for item in str(raw or "").split(",") if item.strip()]
    normalized: list[str] = []
    seen: set[str] = set()
    for action in requested:
        if action in seen:
            continue
        seen.add(action)
        normalized.append(action)
    if not normalized:
        return allowed_defaults
    return tuple(normalized)


def _normalize_host_allowlist(raw: str | None) -> tuple[str, ...]:
    hosts = [item.strip().lower() for item in str(raw or "").split(",") if item.strip()]
    unique: list[str] = []
    seen: set[str] = set()
    for host in hosts:
        if host in seen:
            continue
        seen.add(host)
        unique.append(host)
    return tuple(unique)


FANOUT_OUTBOX_STALE_TARGETS = _normalize_fanout_target_csv(FANOUT_OUTBOX_STALE_TARGETS_ENV)
FANOUT_BACKPRESSURE_TARGETS = _normalize_fanout_target_csv(FANOUT_BACKPRESSURE_TARGETS_ENV)
if not FANOUT_BACKPRESSURE_TARGETS:
    FANOUT_BACKPRESSURE_TARGETS = [FANOUT_TARGET_LETTA, FANOUT_TARGET_LANGFUSE]
FANOUT_COALESCE_TARGETS = _normalize_fanout_target_csv(FANOUT_COALESCE_TARGETS_ENV)
TASK_ALLOWED_ACTIONS = _normalize_task_action_csv(TASK_ALLOWED_ACTIONS_ENV)
TASK_CALLBACK_ALLOWED_HOSTS = _normalize_host_allowlist(TASK_CALLBACK_ALLOWED_HOSTS_ENV)

if not FANOUT_COALESCE_TARGETS:
    FANOUT_COALESCE_TARGETS = [FANOUT_TARGET_QDRANT, FANOUT_TARGET_MINDSDB, FANOUT_TARGET_LETTA, FANOUT_TARGET_LANGFUSE]
LOW_VALUE_FILE_SUFFIXES = _normalize_lower_csv(LOW_VALUE_FILE_SUFFIXES_ENV)
if not LOW_VALUE_FILE_SUFFIXES:
    LOW_VALUE_FILE_SUFFIXES = ["__latest.json", "__rollup.json"]
LOW_VALUE_TOPIC_PREFIXES = _normalize_lower_csv(LOW_VALUE_TOPIC_PREFIXES_ENV)
if not LOW_VALUE_TOPIC_PREFIXES:
    LOW_VALUE_TOPIC_PREFIXES = ["telemetry", "metrics", "signals", "overrides", "perf", "tmp"]

RETRIEVAL_SOURCE_QDRANT = "qdrant"
RETRIEVAL_SOURCE_MEMORY_BANK = "memory_bank"
RETRIEVAL_SOURCE_MONGO_RAW = FANOUT_TARGET_MONGO_RAW
RETRIEVAL_SOURCE_MINDSDB = FANOUT_TARGET_MINDSDB
RETRIEVAL_SOURCE_LETTA = FANOUT_TARGET_LETTA
RETRIEVAL_SOURCES = (
    RETRIEVAL_SOURCE_QDRANT,
    RETRIEVAL_SOURCE_MONGO_RAW,
    RETRIEVAL_SOURCE_MINDSDB,
    RETRIEVAL_SOURCE_LETTA,
    RETRIEVAL_SOURCE_MEMORY_BANK,
)


def _normalize_retrieval_source_csv(raw: str | None) -> list[str]:
    requested = [item.strip().lower() for item in str(raw or "").split(",") if item.strip()]
    normalized: list[str] = []
    seen: set[str] = set()
    for source in requested:
        if source not in RETRIEVAL_SOURCES or source in seen:
            continue
        seen.add(source)
        normalized.append(source)
    return normalized


DEFAULT_RETRIEVAL_FAST_SOURCES = _normalize_retrieval_source_csv(RETRIEVAL_FAST_SOURCES_ENV)
if not DEFAULT_RETRIEVAL_FAST_SOURCES:
    DEFAULT_RETRIEVAL_FAST_SOURCES = [
        RETRIEVAL_SOURCE_QDRANT,
        RETRIEVAL_SOURCE_MONGO_RAW,
        RETRIEVAL_SOURCE_MINDSDB,
    ]
DEFAULT_RETRIEVAL_SLOW_SOURCES = _normalize_retrieval_source_csv(RETRIEVAL_SLOW_SOURCES_ENV)
if not DEFAULT_RETRIEVAL_SLOW_SOURCES:
    DEFAULT_RETRIEVAL_SLOW_SOURCES = [
        RETRIEVAL_SOURCE_LETTA,
        RETRIEVAL_SOURCE_MEMORY_BANK,
    ]
OPTIONAL_OVERRIDE_FILENAMES = {"override-smoke-test.json"}
INDEX_FILE_LATEST_HINTS = {
    "index__arena_health.json": "arena__health__latest.json",
    "index__arena_weights.json": "arena__weights__latest.json",
    "index__api_health.json": "api_health__snapshots__latest.json",
    "index__exits.json": "exit_manager__state__latest.json",
    "index__position_outcomes.json": "position_outcomes__latest.json",
    "index__trade_outcomes_exits.json": "attribution__trade_outcomes_exits__latest.json",
}

DEFAULT_RETRIEVAL_SOURCE_WEIGHTS: dict[str, float] = {
    RETRIEVAL_SOURCE_QDRANT: 1.0,
    RETRIEVAL_SOURCE_LETTA: 0.9,
    RETRIEVAL_SOURCE_MINDSDB: 0.8,
    RETRIEVAL_SOURCE_MONGO_RAW: 0.75,
    RETRIEVAL_SOURCE_MEMORY_BANK: 0.65,
}


class OrchestratorError(RuntimeError):
    """Intentional failure we can bubble up with a helpful hint."""


STATUS_PAGE_HTML = """<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>memMCP Status</title>
  <style>
    :root { color-scheme: light; }
    body { font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, sans-serif; margin: 24px; }
    h1 { font-size: 20px; margin: 0 0 8px; }
    .sub { color: #475569; margin: 0 0 16px; }
    .grid { display: grid; gap: 12px; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); }
    .card { border: 1px solid #e2e8f0; border-radius: 8px; padding: 12px; }
    .ok { color: #16a34a; font-weight: 600; }
    .bad { color: #dc2626; font-weight: 600; }
    .row { display: flex; justify-content: space-between; }
    button { padding: 6px 10px; border: 1px solid #cbd5f5; border-radius: 6px; background: #f8fafc; cursor: pointer; }
    pre { background: #f8fafc; border: 1px solid #e2e8f0; padding: 10px; border-radius: 8px; overflow: auto; }
  </style>
</head>
<body>
  <h1>memMCP Status</h1>
  <p class="sub">Lightweight status page for the orchestrator + attached services.</p>
  <div class="row" style="margin-bottom:12px;">
    <div id="last-update">Last update: --</div>
    <button onclick="loadStatus()">Refresh</button>
  </div>
  <div class="grid" id="cards"></div>
  <h2 style="font-size:16px;margin:18px 0 8px;">Raw JSON</h2>
  <pre id="raw">Loading...</pre>
  <script>
    async function loadStatus() {
      try {
        const res = await fetch('/status');
        const data = await res.json();
        document.getElementById('raw').textContent = JSON.stringify(data, null, 2);
        const cards = document.getElementById('cards');
        cards.innerHTML = '';
        (data.services || []).forEach(svc => {
          const card = document.createElement('div');
          card.className = 'card';
          const status = svc.healthy ? 'ok' : 'bad';
          card.innerHTML = `
            <div class="row"><strong>${svc.name}</strong><span class="${status}">${svc.healthy ? 'OK' : 'DOWN'}</span></div>
            <div style="color:#64748b;margin-top:6px;">${svc.detail || ''}</div>
          `;
          cards.appendChild(card);
        });
        const now = new Date();
        document.getElementById('last-update').textContent = 'Last update: ' + now.toLocaleString();
      } catch (err) {
        document.getElementById('raw').textContent = 'Failed to load /status';
      }
    }
    loadStatus();
    setInterval(loadStatus, 10000);
  </script>
</body>
</html>
"""


def build_pilot_html() -> str:
    contact_line = ""
    cta_href = "#"
    if PILOT_CONTACT_URL:
        cta_href = PILOT_CONTACT_URL
        contact_line = f"Book a call: {PILOT_CONTACT_URL}"
    elif PILOT_CONTACT_EMAIL:
        cta_href = f"mailto:{PILOT_CONTACT_EMAIL}"
        contact_line = f"Email: {PILOT_CONTACT_EMAIL}"
    else:
        contact_line = "Set PILOT_CONTACT_EMAIL or PILOT_CONTACT_URL to enable the CTA."

    return f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Context Lattice Pilot</title>
  <style>
    @import url('https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@500;700&family=IBM+Plex+Sans:wght@400;500;600&display=swap');
    :root {{
      color-scheme: light;
      --ink: #101417;
      --muted: #3d4b56;
      --paper: #f7f8f5;
      --line: #d8ddd8;
      --accent: #154734;
      --accent-2: #ffe9bf;
    }}
    * {{ box-sizing: border-box; }}
    body {{
      margin: 0;
      font-family: "IBM Plex Sans", "Segoe UI", sans-serif;
      color: var(--ink);
      background:
        radial-gradient(circle at 12% 8%, #ffe6a6 0, rgba(255, 230, 166, 0.22) 30%, transparent 48%),
        radial-gradient(circle at 90% 16%, #c7f0db 0, rgba(199, 240, 219, 0.24) 26%, transparent 46%),
        linear-gradient(180deg, #fdfcf8 0%, var(--paper) 60%, #f1f3ef 100%);
      min-height: 100vh;
    }}
    .wrap {{
      max-width: 1080px;
      margin: 0 auto;
      padding: 40px 20px 56px;
    }}
    .hero {{
      border: 1px solid rgba(16, 20, 23, 0.12);
      border-radius: 24px;
      padding: 28px;
      background: linear-gradient(140deg, rgba(255, 255, 255, 0.84), rgba(255, 255, 255, 0.62));
      box-shadow: 0 20px 55px rgba(17, 27, 34, 0.08);
    }}
    .brand {{
      font-family: "Space Grotesk", "Segoe UI", sans-serif;
      font-size: 13px;
      letter-spacing: 0.11em;
      text-transform: uppercase;
      color: var(--accent);
      margin: 0 0 10px;
    }}
    h1 {{
      font-family: "Space Grotesk", "Segoe UI", sans-serif;
      font-size: clamp(30px, 4.8vw, 54px);
      line-height: 1.04;
      margin: 0 0 14px;
      max-width: 840px;
    }}
    .sub {{
      color: var(--muted);
      margin: 0 0 18px;
      font-size: 18px;
      line-height: 1.45;
      max-width: 780px;
    }}
    .chips {{
      display: flex;
      flex-wrap: wrap;
      gap: 10px;
      margin: 12px 0 0;
      padding: 0;
      list-style: none;
    }}
    .chips li {{
      border: 1px solid var(--line);
      background: rgba(255, 255, 255, 0.8);
      color: #263540;
      border-radius: 999px;
      padding: 7px 12px;
      font-size: 12px;
      font-weight: 600;
      letter-spacing: 0.01em;
    }}
    .grid {{
      margin-top: 20px;
      display: grid;
      gap: 14px;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    }}
    .stat {{
      border: 1px solid var(--line);
      border-radius: 14px;
      padding: 14px;
      background: rgba(255, 255, 255, 0.78);
    }}
    .stat strong {{
      display: block;
      font-family: "Space Grotesk", "Segoe UI", sans-serif;
      font-size: 30px;
      line-height: 1;
      margin-bottom: 5px;
      color: var(--accent);
    }}
    .stat span {{
      color: var(--muted);
      font-size: 14px;
    }}
    .section {{
      margin-top: 18px;
      border: 1px solid var(--line);
      border-radius: 16px;
      padding: 16px;
      background: rgba(255, 255, 255, 0.82);
    }}
    .section h2 {{
      margin: 0 0 8px;
      font-family: "Space Grotesk", "Segoe UI", sans-serif;
      font-size: 20px;
      letter-spacing: 0.01em;
    }}
    .section ul {{
      margin: 0;
      padding-left: 18px;
      color: #2f3e48;
      line-height: 1.5;
    }}
    .timeline {{
      list-style: none;
      margin: 0;
      padding: 0;
      display: grid;
      gap: 10px;
    }}
    .timeline li {{
      border: 1px solid var(--line);
      border-radius: 12px;
      padding: 12px;
      background: rgba(255, 255, 255, 0.86);
    }}
    .timeline b {{
      font-family: "Space Grotesk", "Segoe UI", sans-serif;
      display: inline-block;
      margin-right: 8px;
      color: var(--accent);
    }}
    .actions {{
      margin-top: 22px;
      display: flex;
      flex-wrap: wrap;
      gap: 10px;
      align-items: center;
    }}
    .cta {{
      display: inline-block;
      border-radius: 10px;
      padding: 11px 16px;
      text-decoration: none;
      font-weight: 700;
      letter-spacing: 0.01em;
      border: 1px solid var(--accent);
      color: #fff;
      background: linear-gradient(135deg, #0f3c2d, #1a5e46);
      box-shadow: 0 7px 20px rgba(15, 60, 45, 0.22);
    }}
    .ghost {{
      display: inline-block;
      border-radius: 10px;
      padding: 11px 16px;
      text-decoration: none;
      font-weight: 600;
      border: 1px solid #9eb2a3;
      color: #1a3328;
      background: rgba(255, 255, 255, 0.76);
    }}
    .muted {{
      color: #63727f;
      font-size: 13px;
      margin-top: 10px;
    }}
    .footer {{
      margin-top: 18px;
      border-top: 1px dashed #c8d0c8;
      padding-top: 10px;
      font-size: 13px;
      color: #5c6b76;
    }}
    @media (max-width: 640px) {{
      .wrap {{ padding: 24px 14px 32px; }}
      .hero {{ padding: 18px; border-radius: 18px; }}
      .sub {{ font-size: 16px; }}
    }}
  </style>
</head>
<body>
  <main class="wrap">
    <section class="hero">
      <p class="brand">Context Lattice · memMCP</p>
      <h1>Fix context drift before it burns your token budget.</h1>
      <p class="sub">Local-first memory fabric for AI agents with HTTP MCP, federated retrieval, durable fanout, and automatic queue protection.</p>
      <ul class="chips">
        <li>HTTP-preferred MCP</li>
        <li>Federated retrieval (5 sources)</li>
        <li>Outbox durability + retries</li>
        <li>Learning rerank</li>
        <li>Storage retention controls</li>
      </ul>
      <div class="grid">
        <article class="stat">
          <strong>2-4 wks</strong>
          <span>Pilot timeline from baseline to ROI readout.</span>
        </article>
        <article class="stat">
          <strong>5 sinks</strong>
          <span>Qdrant, Mongo raw, MindsDB, Letta, memory-bank fallback.</span>
        </article>
        <article class="stat">
          <strong>Auto guardrails</strong>
          <span>Fanout coalescer, Letta backlog admission, sink retention sweeps.</span>
        </article>
      </div>
      <section class="section">
        <h2>What is new in this build</h2>
        <ul>
          <li>Coalesced fanout window to collapse duplicate hot writes before they amplify queue pressure.</li>
          <li>Backlog-aware Letta admission control to preserve system throughput under bursts.</li>
          <li>Low-value sink retention worker with explicit telemetry and on-demand run endpoint.</li>
          <li>Shared HTTP client pools and batched fanout paths for lower latency and less overhead.</li>
        </ul>
      </section>
      <section class="section">
        <h2>Pilot execution</h2>
        <ol class="timeline">
          <li><b>Week 1</b> baseline measurements: token spend, long-context rate, failure profile.</li>
          <li><b>Week 2-3</b> deploy + tune memory retrieval quality and fanout durability.</li>
          <li><b>Week 4</b> ROI summary with rollout and operating model recommendation.</li>
        </ol>
      </section>
      <div class="actions">
        <a class="cta" href="{cta_href}">Book a 20-minute call</a>
        <a class="ghost" href="/status/ui">Live system status</a>
      </div>
      <p class="muted">{contact_line}</p>
      <p class="footer">Private by default. Bring your own keys. Designed for local dev and enterprise rollout paths.</p>
    </section>
  </main>
</body>
</html>"""
def normalize_memory_path(value: str) -> str:
    if not value:
        return ""
    decoded = unquote(value).replace("\\", "/").lstrip("/")
    decoded = re.sub(r"/+", "/", decoded)
    parts = [part for part in decoded.split("/") if part and part != "."]
    if any(part == ".." for part in parts):
        raise HTTPException(400, "invalid file path")
    return "/".join(parts)


def normalize_topic_path(value: str) -> str:
    if not value:
        return ""
    return normalize_memory_path(value)


def derive_topic_path(file_name: str, explicit: str | None) -> str:
    if explicit:
        normalized = normalize_topic_path(explicit)
        return normalized or DEFAULT_TOPIC_ROOT
    if not file_name:
        return DEFAULT_TOPIC_ROOT
    if "/" not in file_name:
        return DEFAULT_TOPIC_ROOT
    parts = [part for part in file_name.split("/") if part]
    if len(parts) <= 1:
        return DEFAULT_TOPIC_ROOT
    return "/".join(parts[:-1]) or DEFAULT_TOPIC_ROOT


def topic_tags_for_path(topic_path: str) -> list[str]:
    normalized = normalize_topic_path(topic_path)
    if not normalized or normalized == DEFAULT_TOPIC_ROOT:
        return [DEFAULT_TOPIC_ROOT]
    parts = [part for part in normalized.split("/") if part]
    tags: list[str] = []
    current: list[str] = []
    for part in parts:
        current.append(part)
        tags.append("/".join(current))
    return tags or [DEFAULT_TOPIC_ROOT]


def _sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _mindsdb_insert_query(
    project: str,
    file_name: str,
    summary: str,
    created_at: str,
    table_name: str,
) -> str:
    return (
        f"INSERT INTO {MINDSDB_AUTOSYNC_DB}.{table_name} "
        "(project, file, summary, created_at) VALUES "
        f"({_sql_literal(project)}, {_sql_literal(file_name)}, {_sql_literal(summary)}, {_sql_literal(created_at)});"
    )


def _mindsdb_insert_many_query(
    rows: list[dict[str, Any]],
    table_name: str,
) -> str:
    values = ", ".join(
        (
            f"({_sql_literal(str(row.get('project') or ''))}, "
            f"{_sql_literal(str(row.get('file') or ''))}, "
            f"{_sql_literal(str(row.get('summary') or ''))}, "
            f"{_sql_literal(str(row.get('created_at') or _utc_now()))})"
        )
        for row in rows
    )
    return (
        f"INSERT INTO {MINDSDB_AUTOSYNC_DB}.{table_name} "
        "(project, file, summary, created_at) VALUES "
        f"{values};"
    )


def _looks_like_mindsdb_table_corruption(message: str) -> bool:
    text = message.lower()
    signatures = (
        "file is smaller than indicated metadata size",
        "can't create table",
        "[file/files]",
    )
    return any(signature in text for signature in signatures)


def _looks_like_mindsdb_table_exists(message: str) -> bool:
    text = message.lower()
    return "already exists" in text and "table" in text


def _looks_like_mindsdb_database_exists(message: str) -> bool:
    text = message.lower()
    return "already exists" in text and "database" in text


def _looks_like_mindsdb_database_missing(message: str) -> bool:
    text = message.lower()
    signatures = (
        "database not found",
        "unknown database",
        "can't find database",
    )
    return any(signature in text for signature in signatures)


def _mindsdb_fallback_table_name(base_table: str | None = None) -> str:
    table_name = base_table or MINDSDB_AUTOSYNC_TABLE
    if table_name.endswith(MINDSDB_AUTOSYNC_TABLE_FALLBACK_SUFFIX):
        return table_name
    return f"{table_name}{MINDSDB_AUTOSYNC_TABLE_FALLBACK_SUFFIX}"


def _mindsdb_next_fallback_table(current_table: str | None = None) -> str:
    first = _mindsdb_fallback_table_name()
    if not current_table or current_table == MINDSDB_AUTOSYNC_TABLE:
        return first
    if current_table == first:
        return f"{first}_r1"
    match = re.match(rf"^{re.escape(first)}_r(\d+)$", current_table)
    if match:
        revision = int(match.group(1))
        return f"{first}_r{revision + 1}"
    return f"{first}_r{int(time.time())}"


mindsdb_table_lock = asyncio.Lock()
mindsdb_table_ready = False
mindsdb_target_table = MINDSDB_AUTOSYNC_TABLE
mindsdb_queue_task: asyncio.Task | None = None
mindsdb_trading_table_lock = asyncio.Lock()
mindsdb_trading_table_ready = False


async def _mindsdb_execute(query: str) -> dict[str, Any]:
    client = await _get_mindsdb_client()
    resp = await client.post(MINDSDB_SQL_URL, json={"query": query}, timeout=10.0)
    data = resp.json() if resp.content else {}
    if resp.status_code != 200 or (isinstance(data, dict) and data.get("type") == "error"):
        error_message = ""
        if isinstance(data, dict):
            error_message = str(data.get("error_message", ""))
        raise OrchestratorError(error_message or resp.text)
    return data if isinstance(data, dict) else {}


async def _ensure_mindsdb_database_exists(database_name: str) -> None:
    db_name = str(database_name or "").strip()
    if not db_name:
        raise OrchestratorError("MindsDB database name is required")
    queries = [
        f"CREATE DATABASE IF NOT EXISTS {db_name};",
        f"CREATE DATABASE {db_name};",
    ]
    last_error: Exception | None = None
    for idx, query in enumerate(queries):
        try:
            await _mindsdb_execute(query)
            return
        except Exception as exc:
            last_error = exc
            message = str(exc)
            if _looks_like_mindsdb_database_exists(message):
                return
            # Older engines may reject IF NOT EXISTS; retry plain CREATE once.
            if idx == 0 and ("if not exists" in message.lower() or "syntax" in message.lower()):
                continue
            raise
    if last_error is not None:
        raise last_error


async def _ensure_mindsdb_table_exists(table_name: str) -> None:
    await _ensure_mindsdb_database_exists(MINDSDB_AUTOSYNC_DB)
    create_query = (
        f"CREATE TABLE IF NOT EXISTS {MINDSDB_AUTOSYNC_DB}.{table_name} "
        "(project TEXT, file TEXT, summary TEXT, created_at TEXT);"
    )
    check_query = f"SELECT COUNT(*) AS c FROM {MINDSDB_AUTOSYNC_DB}.{table_name};"
    try:
        await _mindsdb_execute(create_query)
    except Exception as exc:
        if not _looks_like_mindsdb_table_exists(str(exc)):
            raise
    await _mindsdb_execute(check_query)


def _safe_float(value: Any, default: float = 0.0) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return default
    if not math.isfinite(number):
        return default
    return number


def _safe_int(value: Any, default: int = 0) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _sql_float_literal(value: Any, default: float = 0.0) -> str:
    number = _safe_float(value, default=default)
    rendered = f"{number:.12f}".rstrip("0").rstrip(".")
    if not rendered or rendered == "-0":
        return "0"
    return rendered


async def ensure_mindsdb_trading_table() -> None:
    global mindsdb_trading_table_ready
    if not MINDSDB_ENABLED or not MINDSDB_TRADING_AUTOSYNC:
        return
    if mindsdb_trading_table_ready:
        return
    async with mindsdb_trading_table_lock:
        if mindsdb_trading_table_ready:
            return
        create_query = (
            f"CREATE TABLE IF NOT EXISTS {MINDSDB_TRADING_DB}.{MINDSDB_TRADING_TABLE} "
            "("
            "timestamp TEXT, "
            "open_positions DOUBLE, "
            "total_value_usd DOUBLE, "
            "unrealized_pnl DOUBLE, "
            "realized_pnl DOUBLE, "
            "daily_pnl DOUBLE, "
            "price_cache_entries DOUBLE, "
            "price_cache_max_age DOUBLE, "
            "price_cache_ttl DOUBLE, "
            "price_cache_freshness DOUBLE, "
            "price_cache_penalty DOUBLE"
            ");"
        )
        check_query = f"SELECT COUNT(*) AS c FROM {MINDSDB_TRADING_DB}.{MINDSDB_TRADING_TABLE};"
        await _ensure_mindsdb_database_exists(MINDSDB_TRADING_DB)
        try:
            await _mindsdb_execute(create_query)
        except Exception as exc:
            if not _looks_like_mindsdb_table_exists(str(exc)):
                raise
        await _mindsdb_execute(check_query)
        mindsdb_trading_table_ready = True


async def push_trading_snapshot_to_mindsdb(snapshot: dict[str, Any]) -> None:
    if not MINDSDB_ENABLED or not MINDSDB_TRADING_AUTOSYNC:
        return
    await ensure_mindsdb_trading_table()
    timestamp = str(snapshot.get("timestamp") or _utc_now())
    query = (
        f"INSERT INTO {MINDSDB_TRADING_DB}.{MINDSDB_TRADING_TABLE} "
        "(timestamp, open_positions, total_value_usd, unrealized_pnl, realized_pnl, daily_pnl, "
        "price_cache_entries, price_cache_max_age, price_cache_ttl, price_cache_freshness, price_cache_penalty) "
        "VALUES ("
        f"'{_escape_sql_literal(timestamp)}', "
        f"{_safe_int(snapshot.get('open_positions'))}, "
        f"{_sql_float_literal(snapshot.get('total_value_usd'))}, "
        f"{_sql_float_literal(snapshot.get('unrealized_pnl'))}, "
        f"{_sql_float_literal(snapshot.get('realized_pnl'))}, "
        f"{_sql_float_literal(snapshot.get('daily_pnl'))}, "
        f"{_sql_float_literal(snapshot.get('price_cache_entries'))}, "
        f"{_sql_float_literal(snapshot.get('price_cache_max_age'))}, "
        f"{_sql_float_literal(snapshot.get('price_cache_ttl'))}, "
        f"{_sql_float_literal(snapshot.get('price_cache_freshness'))}, "
        f"{_sql_float_literal(snapshot.get('price_cache_penalty'), default=1.0)}"
        ");"
    )
    await _mindsdb_execute(query)


async def _mindsdb_queue_worker() -> None:
    while True:
        first = await mindsdb_queue.get()
        batch = [first]
        while len(batch) < MINDSDB_AUTOSYNC_BATCH_SIZE:
            try:
                batch.append(mindsdb_queue.get_nowait())
            except asyncio.QueueEmpty:
                break
        if not MINDSDB_AUTOSYNC:
            for _ in batch:
                mindsdb_queue.task_done()
            continue
        retries = max(1, MINDSDB_AUTOSYNC_RETRIES)
        try:
            for attempt in range(1, retries + 1):
                try:
                    await _insert_many_into_mindsdb(batch)
                    _json_log(
                        "mindsdb.autosync.success",
                        {
                            "count": len(batch),
                            "attempt": attempt,
                        },
                    )
                    break
                except Exception as exc:  # pragma: no cover
                    _json_log(
                        "mindsdb.autosync.retry",
                        {
                            "count": len(batch),
                            "attempt": attempt,
                            "error": str(exc),
                        },
                    )
                    if attempt >= retries:
                        # Salvage on terminal batch failure so one bad row does not poison the entire queue window.
                        for row in batch:
                            try:
                                await _insert_into_mindsdb(row)
                            except Exception as row_exc:  # pragma: no cover
                                _json_log(
                                    "mindsdb.autosync.drop",
                                    {
                                        "project": row.get("project"),
                                        "file": row.get("file"),
                                        "error": str(row_exc),
                                    },
                                )
                        break
                    await asyncio.sleep(MINDSDB_AUTOSYNC_BACKOFF_SECS * attempt)
        finally:
            for _ in batch:
                mindsdb_queue.task_done()


async def _enqueue_mindsdb(item: dict[str, Any]) -> None:
    if mindsdb_queue.full():
        _json_log(
            "mindsdb.autosync.queue_full",
            {"project": item.get("project"), "file": item.get("file")},
        )
        return
    await mindsdb_queue.put(item)
    _json_log(
        "mindsdb.autosync.queued",
        {"project": item.get("project"), "file": item.get("file")},
    )


async def _mindsdb_bootstrap() -> None:
    if not MINDSDB_AUTOSYNC:
        return
    retries = max(1, MINDSDB_AUTOSYNC_RETRIES)
    for attempt in range(1, retries + 1):
        try:
            await ensure_mindsdb_table()
            if MINDSDB_TRADING_AUTOSYNC:
                await ensure_mindsdb_trading_table()
            return
        except Exception as exc:  # pragma: no cover
            _json_log(
                "mindsdb.bootstrap.retry",
                {"attempt": attempt, "error": str(exc)},
            )
            await asyncio.sleep(MINDSDB_AUTOSYNC_BACKOFF_SECS * attempt)


async def _memory_bank_worker(worker_id: int) -> None:
    global memory_bank_queue_processed, memory_write_last_at, memory_write_last_latency_ms
    while True:
        item = await memory_bank_queue.get()
        try:
            await call_memory_tool("memory_bank_write", item["payload"])
            entry = {
                "timestamp": datetime.utcnow().isoformat() + "Z",
                "project": item["project"],
                "file": item["file"],
                "topic_path": item.get("topic_path"),
                "summary": item["summary"],
                "contentLength": item.get("content_length"),
            }
            async with memory_write_history_lock:
                memory_write_history.append(entry)
            await _persist_memory_write(entry)
            await _update_topic_tree(item["project"], item.get("topic_path") or DEFAULT_TOPIC_ROOT)
            await _enqueue_memory_write_fanout(
                {
                    "event_id": item.get("event_id"),
                    "project": item["project"],
                    "file": item["file"],
                    "summary": item["summary"],
                    "payload": item["payload"],
                    "topic_path": item.get("topic_path"),
                    "topic_tags": item.get("topic_tags"),
                    "letta_session": item.get("letta_session"),
                    "letta_context": item.get("letta_context"),
                    "letta_admit": item.get("letta_admit", True),
                    "mongo_persisted": item.get("mongo_persisted"),
                    "qdrant_collection": item.get("qdrant_collection"),
                    "raw_event": item.get("raw_event"),
                }
            )
            start_time = item.get("start_time")
            if start_time is not None:
                latency_ms = (asyncio.get_event_loop().time() - start_time) * 1000
                memory_write_last_latency_ms = round(latency_ms, 2)
                memory_write_last_at = datetime.utcnow().isoformat() + "Z"
                asyncio.create_task(
                    trace_to_langfuse(
                        "write",
                        latency_ms,
                        {"project": item["project"], "file": item["file"]},
                    )
                )
            if item.get("waiter"):
                item["waiter"].set_result(True)
            _json_log(
                "memory.write.persisted",
                {
                    "project": item["project"],
                    "file": item["file"],
                    "worker": worker_id,
                },
            )
        except Exception as exc:  # pragma: no cover
            _json_log(
                "memory.write.error",
                {
                    "project": item.get("project"),
                    "file": item.get("file"),
                    "worker": worker_id,
                    "error": str(exc),
                },
            )
            if item.get("waiter"):
                item["waiter"].set_exception(exc)
        finally:
            memory_bank_queue_processed += 1
            memory_bank_queue.task_done()


async def _enqueue_memory_bank_write(item: dict[str, Any]) -> None:
    global memory_bank_queue_dropped
    if memory_bank_queue.full():
        memory_bank_queue_dropped += 1
        _json_log(
            "memory.write.queue_full",
            {
                "project": item.get("project"),
                "file": item.get("file"),
                "dropped": memory_bank_queue_dropped,
            },
        )
        raise HTTPException(503, "memory write queue full")
    await memory_bank_queue.put(item)


def _utc_iso_from_unix(unix_seconds: float) -> str:
    return datetime.utcfromtimestamp(float(unix_seconds)).isoformat() + "Z"


def _parse_timestamp_to_datetime(value: Any) -> datetime | None:
    if isinstance(value, datetime):
        if value.tzinfo is None:
            return value.replace(tzinfo=timezone.utc)
        return value.astimezone(timezone.utc)
    text = str(value or "").strip()
    if not text:
        return None
    normalized = text.replace("Z", "+00:00")
    try:
        parsed = datetime.fromisoformat(normalized)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _fanout_coalescer_active_for_target(target_name: str) -> bool:
    if not FANOUT_COALESCE_ENABLED:
        return False
    if target_name not in FANOUT_COALESCE_TARGETS:
        return False
    return max(0.0, FANOUT_COALESCE_WINDOW_SECS) > 0.0


def _record_fanout_coalesce_result(result: dict[str, Any] | None) -> None:
    global fanout_coalesce_total
    if not isinstance(result, dict):
        return
    total = int(result.get("coalesced") or 0)
    if total <= 0:
        return
    fanout_coalesce_total += total
    by_target = result.get("coalesced_by_target")
    if not isinstance(by_target, dict):
        return
    for target_name, value in by_target.items():
        delta = int(value or 0)
        if delta <= 0:
            continue
        fanout_coalesce_by_target[target_name] = int(fanout_coalesce_by_target.get(target_name, 0) or 0) + delta


def _fanout_target_outstanding_count(summary: dict[str, Any], target_name: str) -> int:
    by_target = summary.get("by_target") if isinstance(summary, dict) else {}
    if not isinstance(by_target, dict):
        return 0
    target_counts = by_target.get(target_name)
    if not isinstance(target_counts, dict):
        return 0
    pending = int(target_counts.get("pending", 0) or 0)
    retrying = int(target_counts.get("retrying", 0) or 0)
    running = int(target_counts.get("running", 0) or 0)
    return pending + retrying + running


def _looks_low_value_topic_path(topic_path: str | None) -> bool:
    normalized = str(topic_path or "").strip().lower().strip("/")
    if not normalized:
        return False
    for prefix in LOW_VALUE_TOPIC_PREFIXES:
        if normalized == prefix or normalized.startswith(f"{prefix}/"):
            return True
    return False


def _looks_low_value_file(file_name: str | None) -> bool:
    lowered = str(file_name or "").strip().lower()
    if not lowered:
        return False
    if "/_rollups/" in lowered:
        return True
    return any(lowered.endswith(suffix) for suffix in LOW_VALUE_FILE_SUFFIXES)


def _is_low_value_memory_record(
    file_name: str | None,
    topic_path: str | None,
    summary: str | None = None,
    *,
    source_kind: str | None = None,
    include_short_summary: bool = False,
) -> bool:
    if str(source_kind or "").strip().lower() == "high_frequency_rollup":
        return True
    if _looks_low_value_file(file_name):
        return True
    if _looks_low_value_topic_path(topic_path):
        return True
    if include_short_summary:
        summary_len = len(str(summary or "").strip())
        threshold = max(1, LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS)
        lowered_file = str(file_name or "").strip().lower()
        is_quick_churn_file = lowered_file.endswith(".json") or lowered_file.endswith(".log")
        if is_quick_churn_file and 0 < summary_len <= threshold:
            return True
    return False


async def _letta_admission_should_enqueue(
    file_name: str,
    topic_path: str,
    summary: str,
    source_kind: str,
) -> tuple[bool, str | None, int]:
    if not LETTA_ADMISSION_ENABLED:
        return True, None, 0
    soft_limit = max(1, LETTA_ADMISSION_BACKLOG_SOFT_LIMIT)
    hard_limit = max(soft_limit, LETTA_ADMISSION_BACKLOG_HARD_LIMIT)
    cached = _get_fanout_summary_cache()
    if not _fanout_cache_fresh(cached):
        _schedule_fanout_summary_refresh()
    backlog = _fanout_target_outstanding_count(cached, FANOUT_TARGET_LETTA)
    low_value = _is_low_value_memory_record(
        file_name,
        topic_path,
        summary,
        source_kind=source_kind,
        include_short_summary=True,
    )
    if backlog >= hard_limit and low_value:
        return False, "hard_backlog_low_value", backlog
    if backlog >= soft_limit and low_value:
        return False, "soft_backlog_low_value", backlog
    return True, None, backlog


def _record_letta_admission_drop(
    *,
    reason: str,
    backlog: int,
    project: str,
    file_name: str,
    topic_path: str,
) -> None:
    global letta_admission_dropped, letta_admission_last_reason, letta_admission_last_backlog
    global letta_admission_last_logged_at
    letta_admission_dropped += 1
    letta_admission_last_reason = reason
    letta_admission_last_backlog = max(0, int(backlog))
    now = time.monotonic()
    cooldown = max(1.0, LETTA_ADMISSION_LOG_COOLDOWN_SECS)
    if now - float(letta_admission_last_logged_at) < cooldown:
        return
    letta_admission_last_logged_at = now
    _json_log(
        "memory.write.letta_admission_drop",
        {
            "reason": reason,
            "backlog": max(0, int(backlog)),
            "project": project,
            "file": file_name,
            "topic_path": topic_path,
            "dropped_total": letta_admission_dropped,
        },
    )


def _chunk_rows(items: list[dict[str, Any]], chunk_size: int) -> list[list[dict[str, Any]]]:
    if not items:
        return []
    size = max(1, int(chunk_size))
    return [items[idx : idx + size] for idx in range(0, len(items), size)]


def _fanout_backpressure_seconds(target_name: str) -> float:
    if not FANOUT_BACKPRESSURE_ENABLED:
        return 0.0
    if target_name not in FANOUT_BACKPRESSURE_TARGETS:
        return 0.0
    queue_max = max(1, MEMORY_WRITE_QUEUE_MAX)
    queue_ratio = memory_write_queue.qsize() / queue_max
    high_watermark = _normalize_watermark(FANOUT_BACKPRESSURE_QUEUE_HIGH_WATERMARK)
    if queue_ratio <= high_watermark:
        return 0.0
    pressure = min(1.0, (queue_ratio - high_watermark) / max(1e-6, 1.0 - high_watermark))
    max_sleep = max(0.0, FANOUT_BACKPRESSURE_MAX_SLEEP_SECS)
    return pressure * max_sleep


async def _apply_fanout_backpressure(target_name: str, worker_id: int, batch_size: int) -> None:
    delay_secs = _fanout_backpressure_seconds(target_name)
    if delay_secs <= 0:
        return
    now = time.monotonic()
    cooldown = max(1.0, FANOUT_BACKPRESSURE_LOG_COOLDOWN_SECS)
    last_logged = fanout_backpressure_last_logged_at.get(target_name, 0.0)
    if now - last_logged >= cooldown:
        fanout_backpressure_last_logged_at[target_name] = now
        _json_log(
            "memory.write.fanout_backpressure",
            {
                "target": target_name,
                "worker": worker_id,
                "delay_secs": round(delay_secs, 4),
                "queue_depth": memory_write_queue.qsize(),
                "queue_max": MEMORY_WRITE_QUEUE_MAX,
                "batch_size": batch_size,
            },
        )
    await asyncio.sleep(delay_secs)


async def _mark_fanout_job_success(job: dict[str, Any], target_name: str) -> None:
    global memory_write_queue_processed
    if target_name == FANOUT_TARGET_LETTA:
        _reset_letta_transient_error_streak()
    await mark_fanout_success(job["id"])
    memory_write_queue_processed += 1


async def _handle_fanout_job_error(job: dict[str, Any], worker_id: int, exc: Exception) -> None:
    global memory_write_queue_processed
    error_text = str(exc).strip() or exc.__class__.__name__
    outbox_health["lastError"] = error_text
    target_name = str(job.get("target") or "")
    if target_name == FANOUT_TARGET_LETTA and _is_letta_permanent_error(error_text):
        _set_letta_runtime_disabled(error_text)
        await fail_letta_backlog(error_text)
        _reset_letta_transient_error_streak()
        next_status = "failed"
    elif target_name == FANOUT_TARGET_LETTA and _record_letta_transient_failure(error_text):
        disable_reason = (
            f"persistent Letta server errors ({letta_transient_error_streak} consecutive): "
            f"{error_text[:180]}"
        )
        _set_letta_runtime_disabled(disable_reason)
        await fail_letta_backlog(disable_reason)
        next_status = "failed"
    elif target_name == FANOUT_TARGET_MINDSDB and _is_mindsdb_permanent_error(error_text):
        if MINDSDB_FAIL_OPEN_ON_PERMANENT_ERROR:
            await mark_fanout_success(job["id"])
            memory_write_queue_processed += 1
            next_status = "degraded_success"
            _json_log(
                "memory.write.fanout_fail_open",
                {
                    "target": FANOUT_TARGET_MINDSDB,
                    "worker": worker_id,
                    "event_id": job.get("event_id"),
                    "error": error_text[:220],
                },
            )
        else:
            await mark_fanout_failed(job["id"], error_text)
            next_status = "failed"
    else:
        next_status = await mark_fanout_retry(job, error_text)
    _json_log(
        "memory.write.fanout_error",
        {
            "error": error_text,
            "worker": worker_id,
            "target": target_name,
            "event_id": job.get("event_id"),
            "next_status": next_status,
        },
    )


async def _memory_write_worker(
    worker_id: int,
    target: str | None = None,
    exclude_target: str | None = None,
) -> None:
    global memory_write_queue_processed
    while True:
        got_signal = False
        try:
            await asyncio.wait_for(memory_write_queue.get(), timeout=FANOUT_POLL_SECS)
            got_signal = True
        except asyncio.TimeoutError:
            got_signal = False
        except Exception:  # pragma: no cover
            got_signal = False

        try:
            jobs = await claim_fanout_batch(
                FANOUT_BATCH_SIZE,
                target=target,
                exclude_target=exclude_target,
            )
            outbox_health["lastBatchSize"] = len(jobs)
            if jobs:
                outbox_health["lastProcessedAt"] = _utc_now()
            if not jobs:
                continue

            jobs_by_target: dict[str, list[dict[str, Any]]] = {}
            for job in jobs:
                job_target = str(job.get("target") or "")
                jobs_by_target.setdefault(job_target, []).append(job)

            qdrant_jobs = jobs_by_target.pop(FANOUT_TARGET_QDRANT, [])
            for qdrant_batch in _chunk_rows(qdrant_jobs, FANOUT_QDRANT_BULK_SIZE):
                try:
                    await _apply_fanout_backpressure(FANOUT_TARGET_QDRANT, worker_id, len(qdrant_batch))
                    payload_rows: list[dict[str, Any]] = []
                    for job in qdrant_batch:
                        payload = job.get("payload") or {}
                        payload_rows.append(
                            {
                                "project": payload["project"],
                                "file": payload["file"],
                                "content": payload.get("summary") or "",
                                "topic_path": payload.get("topic_path"),
                                "topic_tags": payload.get("topic_tags") or [],
                                "collection_name": payload.get("qdrant_collection"),
                            }
                        )
                    await push_batch_to_qdrant(payload_rows)
                    for job in qdrant_batch:
                        await _mark_fanout_job_success(job, FANOUT_TARGET_QDRANT)
                except Exception as exc:  # pragma: no cover
                    for job in qdrant_batch:
                        await _handle_fanout_job_error(job, worker_id, exc)

            mindsdb_jobs = jobs_by_target.pop(FANOUT_TARGET_MINDSDB, [])
            for mindsdb_batch in _chunk_rows(mindsdb_jobs, FANOUT_MINDSDB_BULK_SIZE):
                try:
                    await _apply_fanout_backpressure(FANOUT_TARGET_MINDSDB, worker_id, len(mindsdb_batch))
                    payload_rows = []
                    for job in mindsdb_batch:
                        payload = job.get("payload") or {}
                        payload_rows.append(
                            {
                                "project": payload["project"],
                                "file": payload["file"],
                                "summary": payload.get("summary") or "",
                                "created_at": _utc_now(),
                            }
                        )
                    await push_batch_to_mindsdb(payload_rows, allow_fallback_queue=False)
                    for job in mindsdb_batch:
                        await _mark_fanout_job_success(job, FANOUT_TARGET_MINDSDB)
                except Exception as exc:  # pragma: no cover
                    for job in mindsdb_batch:
                        await _handle_fanout_job_error(job, worker_id, exc)

            mongo_jobs = jobs_by_target.pop(FANOUT_TARGET_MONGO_RAW, [])
            for mongo_batch in _chunk_rows(mongo_jobs, FANOUT_MONGO_BULK_SIZE):
                try:
                    await _apply_fanout_backpressure(FANOUT_TARGET_MONGO_RAW, worker_id, len(mongo_batch))
                    raw_events: list[dict[str, Any]] = []
                    for job in mongo_batch:
                        payload = job.get("payload") or {}
                        raw_event = payload.get("raw_event")
                        if not isinstance(raw_event, dict):
                            raise OrchestratorError("raw_event payload missing for mongo fanout")
                        raw_events.append(raw_event)
                    ok, error = await persist_raw_events_to_mongo(raw_events)
                    if not ok:
                        raise OrchestratorError(error or "mongo raw batch write failed")
                    for job in mongo_batch:
                        await _mark_fanout_job_success(job, FANOUT_TARGET_MONGO_RAW)
                except Exception as exc:  # pragma: no cover
                    for job in mongo_batch:
                        await _handle_fanout_job_error(job, worker_id, exc)

            langfuse_jobs = jobs_by_target.pop(FANOUT_TARGET_LANGFUSE, [])
            for langfuse_batch in _chunk_rows(langfuse_jobs, FANOUT_LANGFUSE_BULK_SIZE):
                try:
                    await _apply_fanout_backpressure(FANOUT_TARGET_LANGFUSE, worker_id, len(langfuse_batch))
                    payload_rows = []
                    for job in langfuse_batch:
                        payload = job.get("payload") or {}
                        payload_rows.append(
                            {
                                "project": payload.get("project") or "",
                                "summary": payload.get("summary") or "",
                                "payload": payload.get("payload") if isinstance(payload.get("payload"), dict) else {},
                            }
                        )
                    await push_batch_to_langfuse(payload_rows)
                    for job in langfuse_batch:
                        await _mark_fanout_job_success(job, FANOUT_TARGET_LANGFUSE)
                except Exception as exc:  # pragma: no cover
                    for job in langfuse_batch:
                        await _handle_fanout_job_error(job, worker_id, exc)

            letta_jobs = jobs_by_target.pop(FANOUT_TARGET_LETTA, [])
            for letta_batch in _chunk_rows(letta_jobs, FANOUT_LETTA_BULK_SIZE):
                if not _letta_target_enabled():
                    reason = letta_runtime_disabled_reason or "letta fanout disabled"
                    await fail_letta_backlog(reason)
                    continue
                try:
                    await _apply_fanout_backpressure(FANOUT_TARGET_LETTA, worker_id, len(letta_batch))
                    limiter = asyncio.Semaphore(FANOUT_LETTA_BATCH_CONCURRENCY)

                    async def _sync_letta(job: dict[str, Any]) -> None:
                        payload = job.get("payload") or {}
                        session_id = payload.get("letta_session")
                        if not session_id:
                            raise OrchestratorError("letta_session missing in fanout payload")
                        async with limiter:
                            await push_to_letta(
                                session_id,
                                payload.get("summary") or "",
                                payload.get("letta_context") or {},
                            )

                    results = await asyncio.gather(
                        *[_sync_letta(job) for job in letta_batch],
                        return_exceptions=True,
                    )
                    for job, result in zip(letta_batch, results):
                        if isinstance(result, Exception):
                            await _handle_fanout_job_error(job, worker_id, result)
                            continue
                        await _mark_fanout_job_success(job, FANOUT_TARGET_LETTA)
                except Exception as exc:  # pragma: no cover
                    for job in letta_batch:
                        await _handle_fanout_job_error(job, worker_id, exc)

            for target_name, target_jobs in jobs_by_target.items():
                unknown_error = OrchestratorError(f"unknown outbox target: {target_name}")
                for job in target_jobs:
                    await _handle_fanout_job_error(job, worker_id, unknown_error)
        except Exception as exc:  # pragma: no cover - runtime resilience
            error_text = str(exc).strip() or exc.__class__.__name__
            outbox_health["lastError"] = error_text
            promoted = await _promote_outbox_backend_to_mongo_if_sqlite_error(error_text)
            _json_log(
                "memory.write.fanout_worker_error",
                {
                    "worker": worker_id,
                    "error": error_text,
                    "promoted_to_mongo": promoted,
                },
            )
            await asyncio.sleep(max(0.25, min(5.0, FANOUT_POLL_SECS)))
        finally:
            if got_signal:
                try:
                    memory_write_queue.task_done()
                except ValueError:  # pragma: no cover
                    pass


async def _enqueue_memory_write_fanout(item: dict[str, Any]) -> None:
    global memory_write_queue_dropped
    letta_admit = bool(item.get("letta_admit", True))
    fanout_targets = [FANOUT_TARGET_QDRANT, FANOUT_TARGET_MINDSDB]
    if not item.get("mongo_persisted"):
        fanout_targets.insert(0, FANOUT_TARGET_MONGO_RAW)
    if item.get("letta_session") and _letta_target_enabled() and letta_admit:
        fanout_targets.append(FANOUT_TARGET_LETTA)
    elif item.get("letta_session") and not letta_admit:
        _json_log(
            "memory.write.letta_admission_skipped",
            {
                "project": item.get("project"),
                "file": item.get("file"),
            },
        )
    if LANGFUSE_API_KEY:
        fanout_targets.append(FANOUT_TARGET_LANGFUSE)
    payload = {
        "event_id": item.get("event_id") or uuid.uuid4().hex,
        "project": item["project"],
        "file": item["file"],
        "summary": item.get("summary") or "",
        "payload": item.get("payload") or {},
        "topic_path": item.get("topic_path"),
        "topic_tags": item.get("topic_tags") or [],
        "letta_session": item.get("letta_session"),
        "letta_context": item.get("letta_context") or {},
        "letta_admit": letta_admit,
        "qdrant_collection": item.get("qdrant_collection"),
        "raw_event": item.get("raw_event"),
    }
    outbox_result = await enqueue_fanout_outbox(payload, fanout_targets)
    if memory_write_queue.full():
        memory_write_queue_dropped += 1
        _json_log(
            "memory.write.fanout_signal_drop",
            {
                "project": item.get("project"),
                "file": item.get("file"),
                "dropped": memory_write_queue_dropped,
            },
        )
    else:
        await memory_write_queue.put(payload["event_id"])
    _json_log(
        "memory.write.fanout_queued",
        {
            "project": item.get("project"),
            "file": item.get("file"),
            "targets": fanout_targets,
            **outbox_result,
        },
    )


def _cheap_embedding(text: str, vector_size: int) -> list[float]:
    """Cheap deterministic embedding used when no provider is configured."""

    base = [0.0] * vector_size
    encoded = text.encode("utf-8")
    if not encoded:
        return base
    for idx, char in enumerate(encoded):
        base[idx % vector_size] += char / 255.0
    norm = max(sum(base), 1e-6)
    return [round(val / norm, 6) for val in base]


async def _openai_like_embedding(text: str) -> list[float]:
    if not EMBEDDING_BASE_URL:
        raise OrchestratorError("EMBEDDING_BASE_URL is not set for openai provider")
    url = EMBEDDING_BASE_URL.rstrip("/") + "/v1/embeddings"
    headers = {"content-type": "application/json"}
    if EMBEDDING_API_KEY:
        headers["authorization"] = f"Bearer {EMBEDDING_API_KEY}"
    payload = {"model": EMBEDDING_MODEL, "input": text}
    async with httpx.AsyncClient(timeout=30.0) as client:
        resp = await client.post(url, json=payload, headers=headers)
    if resp.status_code != 200:
        raise OrchestratorError(f"Embedding request failed: {resp.text}")
    data = resp.json()
    payloads = data.get("data") or []
    if not payloads:
        raise OrchestratorError("Embedding provider returned no data")
    return payloads[0]["embedding"]


async def _ollama_embedding(text: str) -> list[float]:
    url = OLLAMA_BASE_URL.rstrip("/") + "/api/embeddings"
    payload = {"model": EMBEDDING_MODEL, "prompt": text}
    async with httpx.AsyncClient(timeout=30.0) as client:
        resp = await client.post(url, json=payload)
    if resp.status_code != 200:
        raise OrchestratorError(f"Ollama embedding failed: {resp.text}")
    data = resp.json()
    vector = data.get("embedding")
    if vector is None and data.get("data"):
        vector = data["data"][0].get("embedding")
    if vector is None:
        raise OrchestratorError("Ollama response missing embedding field")
    return vector


async def embed_text(text: str) -> list[float]:
    def _cheap_fallback(provider: str, error: Exception) -> list[float]:
        if not EMBEDDING_FAIL_OPEN:
            raise OrchestratorError(str(error)) from error
        logger.warning(
            "Embedding provider '%s' failed (%s); using deterministic cheap fallback",
            provider,
            str(error)[:300],
        )
        return _cheap_embedding(text, FALLBACK_EMBED_DIM)

    cache_key = _embedding_cache_key(text)
    cached = await _embedding_cache_get(cache_key)
    if cached is not None:
        return cached

    provider = EMBEDDING_PROVIDER
    if provider in ("openai", "lmstudio", "openai-compatible"):
        try:
            vector = await _openai_like_embedding(text)
            await _embedding_cache_set(cache_key, vector)
            return vector
        except OrchestratorError as exc:
            return _cheap_fallback(provider, exc)
        except Exception as exc:  # pragma: no cover - network failure
            return _cheap_fallback(provider, exc)
    if provider == "ollama":
        try:
            vector = await _ollama_embedding(text)
            await _embedding_cache_set(cache_key, vector)
            return vector
        except OrchestratorError as exc:
            return _cheap_fallback(provider, exc)
        except Exception as exc:  # pragma: no cover
            return _cheap_fallback(provider, exc)
    # default fallback
    vector = _cheap_embedding(text, FALLBACK_EMBED_DIM)
    await _embedding_cache_set(cache_key, vector)
    return vector


DEFAULT_RESPONSE_CLASS = ORJSONResponse if orjson is not None else JSONResponse


def _normalize_rate_limit(value: float) -> float:
    try:
        rate = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not math.isfinite(rate):
        return 0.0
    return max(0.0, rate)


def _normalize_sample_rate(value: float) -> float:
    try:
        rate = float(value)
    except (TypeError, ValueError):
        return 0.0
    if not math.isfinite(rate):
        return 0.0
    return min(1.0, max(0.0, rate))


def _normalize_watermark(value: float) -> float:
    try:
        watermark = float(value)
    except (TypeError, ValueError):
        return 0.65
    if not math.isfinite(watermark):
        return 0.65
    return min(0.99, max(0.05, watermark))


def _build_fanout_rate_limiter(rate_per_sec: float) -> Any | None:
    normalized = _normalize_rate_limit(rate_per_sec)
    if normalized <= 0:
        return None
    if AsyncLimiter is None:
        return None
    return AsyncLimiter(normalized, time_period=1.0)


@contextlib.asynccontextmanager
async def _fanout_rate_limit(limiter: Any | None):
    if limiter is None:
        yield
        return
    async with limiter:
        yield


app = FastAPI(
    title="memMCP orchestrator",
    version="0.1.0",
    default_response_class=DEFAULT_RESPONSE_CLASS,
)
logger = logging.getLogger("memmcp.orchestrator")
qdrant_fanout_rate_limiter = _build_fanout_rate_limiter(FANOUT_QDRANT_RATE_LIMIT_PER_SEC)
mindsdb_fanout_rate_limiter = _build_fanout_rate_limiter(FANOUT_MINDSDB_RATE_LIMIT_PER_SEC)
letta_fanout_rate_limiter = _build_fanout_rate_limiter(FANOUT_LETTA_RATE_LIMIT_PER_SEC)
langfuse_fanout_rate_limiter = _build_fanout_rate_limiter(FANOUT_LANGFUSE_RATE_LIMIT_PER_SEC)
if AsyncLimiter is None and any(
    _normalize_rate_limit(rate) > 0
    for rate in (
        FANOUT_QDRANT_RATE_LIMIT_PER_SEC,
        FANOUT_MINDSDB_RATE_LIMIT_PER_SEC,
        FANOUT_LETTA_RATE_LIMIT_PER_SEC,
        FANOUT_LANGFUSE_RATE_LIMIT_PER_SEC,
    )
):
    logger.warning("aiolimiter unavailable; fanout rate limits are disabled")

mindsdb_queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue(maxsize=MINDSDB_AUTOSYNC_QUEUE_MAX)
mindsdb_queue_lock = asyncio.Lock()
mongo_client_lock = asyncio.Lock()
letta_agent_lock = asyncio.Lock()
mcp_session_lock = asyncio.Lock()
service_client_lock = asyncio.Lock()
embedding_cache_lock = asyncio.Lock()
embedding_cache: OrderedDict[str, list[float]] = OrderedDict()
embedding_cache_hits = 0
embedding_cache_misses = 0
embedding_cache_evictions = 0
qdrant_collection_dim_cache: dict[str, int] = {}
MCP_SESSION_ID: str | None = None
MONGO_CLIENT = None
FANOUT_OUTBOX_MONGO_CLIENT = None
fanout_outbox_mongo_lock = asyncio.Lock()
fanout_outbox_backend_active = FANOUT_OUTBOX_BACKEND if FANOUT_OUTBOX_BACKEND in ("sqlite", "mongo") else "sqlite"
LETTA_AGENT_CACHE: dict[str, str] = {}
LETTA_AGENT_VERIFIED_AT: dict[str, float] = {}
outbox_health = {
    "lastProcessedAt": None,
    "lastError": None,
    "lastBatchSize": 0,
    "gc": {
        "lastRunAt": None,
        "lastDurationMs": None,
        "lastDeleted": 0,
        "lastError": None,
        "runs": 0,
        "vacuumedAt": None,
    },
}
fanout_summary_cache: dict[str, Any] = {
    "by_status": {},
    "by_target": {},
    "updated_at": None,
    "updated_monotonic": None,
}
fanout_summary_refresh_task: asyncio.Task[Any] | None = None
outbox_gc_task: asyncio.Task[Any] | None = None
outbox_gc_last_vacuum_monotonic = 0.0
fanout_coalesce_total = 0
fanout_coalesce_by_target: dict[str, int] = {}
fanout_backpressure_last_logged_at: dict[str, float] = {}
letta_runtime_enabled = True
letta_runtime_disabled_reason = ""
letta_transient_error_streak = 0
letta_admission_last_logged_at = 0.0
letta_admission_dropped = 0
letta_admission_last_reason = ""
letta_admission_last_backlog = 0
sink_retention_task: asyncio.Task[Any] | None = None
sink_retention_state: dict[str, Any] = {
    "lastRunAt": None,
    "lastDurationMs": None,
    "lastError": None,
    "runs": 0,
    "lastResult": {},
}


def _letta_config_enabled() -> bool:
    if not LETTA_AUTO_SESSION_ID:
        return False
    if LETTA_REQUIRE_API_KEY and not LETTA_API_KEY:
        return False
    return True


def _letta_target_enabled() -> bool:
    return _letta_config_enabled() and letta_runtime_enabled


def _set_letta_runtime_disabled(reason: str) -> None:
    global letta_runtime_enabled, letta_runtime_disabled_reason, letta_transient_error_streak
    if not letta_runtime_enabled:
        return
    letta_runtime_enabled = False
    letta_transient_error_streak = 0
    letta_runtime_disabled_reason = reason[:240]
    logger.warning("Disabling Letta fanout due to permanent error: %s", letta_runtime_disabled_reason)


def _is_letta_permanent_error(message: str) -> bool:
    text = message.lower()
    status_match = re.search(r"status=(\d{3})", text)
    if status_match:
        status = int(status_match.group(1))
        if status in (400, 401, 403, 404, 405, 410, 422):
            return True
    return "not found" in text or "invalid_argument" in text


def _is_sqlite_disk_io_error(message: str) -> bool:
    text = (message or "").lower()
    return "disk i/o error" in text or "database is locked" in text or "unable to open database file" in text


def _is_letta_transient_server_error(message: str) -> bool:
    text = (message or "").lower()
    status_match = re.search(r"status=(\d{3})", text)
    if status_match:
        status = int(status_match.group(1))
        if 500 <= status <= 599:
            return True
    return "unknown error occurred" in text or "internal server error" in text


def _reset_letta_transient_error_streak() -> None:
    global letta_transient_error_streak
    letta_transient_error_streak = 0


def _record_letta_transient_failure(error_text: str) -> bool:
    global letta_transient_error_streak
    if not LETTA_DISABLE_ON_TRANSIENT_ERRORS:
        return False
    if not _is_letta_transient_server_error(error_text):
        letta_transient_error_streak = 0
        return False
    letta_transient_error_streak += 1
    threshold = max(1, LETTA_TRANSIENT_ERROR_THRESHOLD)
    return letta_transient_error_streak >= threshold


def _is_mindsdb_permanent_error(message: str) -> bool:
    text = message.lower()
    return (
        "file is too small to be a well-formed file" in text
        or "verification of flatbuffer-encoded footer failed" in text
        or "lz4 compressed input contains more than one frame" in text
        or ("expected to read" in text and "metadata bytes but got" in text)
        or ("unable to read" in text and "from end of file" in text)
    )


def _json_log(event: str, payload: dict[str, Any]) -> None:
    try:
        logger.info(json.dumps({"event": event, **payload}))
    except Exception:  # pragma: no cover - logging fallback
        logger.info("%s %s", event, payload)


def _configure_logging() -> None:
    logger.setLevel(ORCH_LOG_LEVEL)
    if ORCH_LOG_FILE:
        path = pathlib.Path(ORCH_LOG_FILE)
        path.parent.mkdir(parents=True, exist_ok=True)
        handler = logging.FileHandler(path)
        handler.setFormatter(logging.Formatter("%(message)s"))
        logger.addHandler(handler)


_configure_logging()


def _embedding_cache_key(text: str) -> str:
    identity = f"{EMBEDDING_PROVIDER}|{EMBEDDING_MODEL}|{text}".encode("utf-8")
    return hashlib.sha1(identity).hexdigest()


async def _embedding_cache_get(key: str) -> list[float] | None:
    global embedding_cache_hits, embedding_cache_misses
    if not EMBEDDING_CACHE_ENABLED or EMBEDDING_CACHE_MAX_KEYS <= 0:
        return None
    async with embedding_cache_lock:
        vector = embedding_cache.get(key)
        if vector is None:
            embedding_cache_misses += 1
            return None
        embedding_cache.move_to_end(key)
        embedding_cache_hits += 1
        return list(vector)


async def _embedding_cache_set(key: str, vector: list[float]) -> None:
    global embedding_cache_evictions
    if not EMBEDDING_CACHE_ENABLED or EMBEDDING_CACHE_MAX_KEYS <= 0:
        return
    async with embedding_cache_lock:
        embedding_cache[key] = list(vector)
        embedding_cache.move_to_end(key)
        while len(embedding_cache) > EMBEDDING_CACHE_MAX_KEYS:
            embedding_cache.popitem(last=False)
            embedding_cache_evictions += 1


def _should_log_http_request(status: int, duration_ms: float) -> bool:
    if status >= 400:
        return True
    if duration_ms >= max(0.0, ORCH_HTTP_REQUEST_LOG_SLOW_MS):
        return True
    sample_rate = _normalize_sample_rate(ORCH_HTTP_REQUEST_LOG_SUCCESS_SAMPLE_RATE)
    if sample_rate <= 0.0:
        return False
    if sample_rate >= 1.0:
        return True
    return random.random() < sample_rate


async def _ensure_shared_service_clients() -> None:
    global QDRANT_CLIENT, QDRANT_CLOUD_CLIENT, MINDSDB_CLIENT, LETTA_CLIENT, LANGFUSE_CLIENT
    cloud_needed = bool(QDRANT_CLUSTER_ENDPOINT)
    if (
        QDRANT_CLIENT is not None
        and MINDSDB_CLIENT is not None
        and LETTA_CLIENT is not None
        and (not LANGFUSE_API_KEY or LANGFUSE_CLIENT is not None)
        and (not cloud_needed or QDRANT_CLOUD_CLIENT is not None)
    ):
        return
    async with service_client_lock:
        if QDRANT_CLIENT is None:
            if AsyncQdrantClient is None or qdrant_models is None:
                raise RuntimeError("qdrant-client dependency is required (install qdrant-client)")
            QDRANT_CLIENT = AsyncQdrantClient(
                url=QDRANT_LOCAL_URL,
                prefer_grpc=QDRANT_GRPC_PREFER,
                grpc_port=QDRANT_GRPC_PORT,
                timeout=QDRANT_CLIENT_TIMEOUT_SECS,
            )
        if QDRANT_CLOUD_CLIENT is None and QDRANT_CLUSTER_ENDPOINT:
            if AsyncQdrantClient is None or qdrant_models is None:
                raise RuntimeError("qdrant-client dependency is required (install qdrant-client)")
            cloud_kwargs: dict[str, Any] = {
                "url": QDRANT_CLUSTER_ENDPOINT,
                "prefer_grpc": QDRANT_GRPC_PREFER,
                "grpc_port": QDRANT_CLOUD_GRPC_PORT,
                "timeout": QDRANT_CLIENT_TIMEOUT_SECS,
            }
            if QDRANT_API_KEY:
                cloud_kwargs["api_key"] = QDRANT_API_KEY
            QDRANT_CLOUD_CLIENT = AsyncQdrantClient(**cloud_kwargs)
        if MINDSDB_CLIENT is None:
            MINDSDB_CLIENT = httpx.AsyncClient(timeout=MINDSDB_CLIENT_TIMEOUT, limits=SHARED_SERVICE_CLIENT_LIMITS)
        if LETTA_CLIENT is None:
            LETTA_CLIENT = httpx.AsyncClient(timeout=LETTA_CLIENT_TIMEOUT, limits=SHARED_SERVICE_CLIENT_LIMITS)
        if LANGFUSE_CLIENT is None and LANGFUSE_API_KEY:
            LANGFUSE_CLIENT = httpx.AsyncClient(
                timeout=LANGFUSE_CLIENT_TIMEOUT,
                limits=SHARED_SERVICE_CLIENT_LIMITS,
            )


def _qdrant_operation_targets() -> list[str]:
    cloud_available = bool(QDRANT_CLUSTER_ENDPOINT)
    if QDRANT_USE_CLOUD and cloud_available:
        targets = ["cloud"]
        if QDRANT_CLOUD_FALLBACK:
            targets.append("local")
        return targets
    targets = ["local"]
    if QDRANT_CLOUD_FALLBACK and cloud_available:
        targets.append("cloud")
    return targets


async def _get_qdrant_client(target: str = "local") -> AsyncQdrantClient:
    if QDRANT_CLIENT is None or (target == "cloud" and QDRANT_CLOUD_CLIENT is None):
        await _ensure_shared_service_clients()
    if target == "cloud":
        if QDRANT_CLOUD_CLIENT is None:
            raise RuntimeError("Qdrant cloud requested but QDRANT_CLUSTER_ENDPOINT is not configured")
        return QDRANT_CLOUD_CLIENT
    assert QDRANT_CLIENT is not None
    return QDRANT_CLIENT


async def _qdrant_call(
    operation: str,
    fn: Any,
) -> Any:
    targets = _qdrant_operation_targets()
    errors: list[str] = []
    for idx, target in enumerate(targets):
        client = await _get_qdrant_client(target)
        try:
            return await fn(client, target)
        except Exception as exc:
            errors.append(f"{target}:{exc}")
            if idx < (len(targets) - 1):
                logger.warning(
                    "Qdrant %s failed on %s backend; attempting fallback (%s)",
                    operation,
                    target,
                    str(exc)[:220],
                )
                continue
            raise
    raise RuntimeError(f"Qdrant {operation} failed ({'; '.join(errors)})")


async def _get_mindsdb_client() -> httpx.AsyncClient:
    if MINDSDB_CLIENT is None:
        await _ensure_shared_service_clients()
    assert MINDSDB_CLIENT is not None
    return MINDSDB_CLIENT


async def _get_letta_client() -> httpx.AsyncClient:
    if LETTA_CLIENT is None:
        await _ensure_shared_service_clients()
    assert LETTA_CLIENT is not None
    return LETTA_CLIENT


async def _get_langfuse_client() -> httpx.AsyncClient:
    if not LANGFUSE_API_KEY:
        raise OrchestratorError("LANGFUSE_API_KEY is not configured")
    if LANGFUSE_CLIENT is None:
        await _ensure_shared_service_clients()
    assert LANGFUSE_CLIENT is not None
    return LANGFUSE_CLIENT


def validate_orchestrator_security_posture() -> None:
    is_production = MEMMCP_ENV in ("production", "prod")
    if not is_production:
        return
    issues: list[str] = []
    warnings: list[str] = []
    if not ORCH_API_KEY:
        issues.append("MEMMCP_ORCHESTRATOR_API_KEY is required in production")
    if ORCH_PUBLIC_STATUS:
        warnings.append("ORCH_PUBLIC_STATUS=true exposes status endpoints without auth in production")
    if ORCH_PUBLIC_DOCS:
        warnings.append("ORCH_PUBLIC_DOCS=true exposes OpenAPI/docs endpoints without auth in production")
    if ORCH_PROMETHEUS_ENABLED and ORCH_PROMETHEUS_PUBLIC:
        warnings.append(
            f"ORCH_PROMETHEUS_PUBLIC=true exposes {ORCH_PROMETHEUS_ENDPOINT} without auth in production"
        )
    for item in warnings:
        logger.warning("Security warning: %s", item)
    if issues and ORCH_SECURITY_STRICT:
        raise RuntimeError("; ".join(issues))
    for item in issues:
        logger.error("Security issue: %s", item)


def _extract_api_key(request: Request) -> str:
    header = request.headers.get("x-api-key") or request.headers.get("authorization") or ""
    if header.lower().startswith("bearer "):
        return header.split(" ", 1)[1].strip()
    return header.strip()


def _configure_prometheus_metrics(app_instance: FastAPI) -> None:
    if not ORCH_PROMETHEUS_ENABLED:
        return
    if Instrumentator is None:
        logger.warning("prometheus-fastapi-instrumentator unavailable; metrics endpoint disabled")
        return
    try:
        Instrumentator().instrument(app_instance).expose(
            app_instance,
            endpoint=ORCH_PROMETHEUS_ENDPOINT,
            include_in_schema=False,
        )
    except Exception as exc:  # pragma: no cover - optional observability path
        logger.warning("Prometheus instrumentation setup failed: %s", exc)


_configure_prometheus_metrics(app)


@app.middleware("http")
async def request_context_middleware(request: Request, call_next):
    request_id = request.headers.get("x-request-id") or str(uuid.uuid4())
    request.state.request_id = request_id
    public_prefixes = ["/health", "/pilot"]
    if MESSAGING_INTEGRATIONS_ENABLED and MESSAGING_WEBHOOK_PUBLIC:
        public_prefixes.extend(
            [
                "/integrations/telegram/webhook",
                "/integrations/slack/events",
            ]
        )
    if ORCH_PUBLIC_STATUS:
        public_prefixes.extend(["/status", "/status/ui"])
    if ORCH_PUBLIC_DOCS:
        public_prefixes.extend(["/openapi", "/docs", "/redoc"])
    if ORCH_PROMETHEUS_ENABLED and ORCH_PROMETHEUS_PUBLIC:
        public_prefixes.append(ORCH_PROMETHEUS_ENDPOINT)
    public_prefixes_tuple = tuple(public_prefixes)
    if ORCH_API_KEY and not request.url.path.startswith(public_prefixes_tuple):
        if _extract_api_key(request) != ORCH_API_KEY:
            return JSONResponse(
                status_code=401,
                content={"ok": False, "error": "Invalid API key"},
                headers={"x-request-id": request_id},
            )
    start = time.monotonic()
    response = await call_next(request)
    response.headers["x-request-id"] = request_id
    duration_ms = (time.monotonic() - start) * 1000
    if _should_log_http_request(response.status_code, duration_ms):
        _json_log(
            "http.request",
            {
                "request_id": request_id,
                "method": request.method,
                "path": request.url.path,
                "status": response.status_code,
                "duration_ms": round(duration_ms, 2),
                "client": request.client.host if request.client else None,
            },
        )
    return response


telemetry_state: Dict[str, Any] = {
    "updatedAt": None,
    "queueDepth": 0,
    "batchSize": 0,
    "totals": {
        "enqueued": 0,
        "dropped": 0,
        "batches": 0,
        "flushedEvents": 0,
    },
}
trading_metrics_state: Dict[str, Any] = {
    "updatedAt": None,
    "openPositions": 0,
    "totalValueUsd": 0.0,
    "unrealizedPnl": 0.0,
    "realizedPnl": 0.0,
    "dailyPnl": 0.0,
    "positions": [],
    "priceCacheEntries": 0,
    "priceCacheMaxAge": 0.0,
    "priceCacheTtl": 0.0,
    "priceCacheFreshness": 0.0,
    "priceCachePenalty": 1.0,
}
trading_history = deque(maxlen=TRADING_HISTORY_LIMIT)
trading_history_lock = asyncio.Lock()
strategy_metrics_state: Dict[str, Any] = {
    "updatedAt": None,
    "strategies": [],
}
strategy_history = deque(maxlen=STRATEGY_HISTORY_LIMIT)
strategy_history_lock = asyncio.Lock()
signal_cache = deque(maxlen=SIGNAL_HISTORY_LIMIT)
signal_cache_lock = asyncio.Lock()
signal_seen_files: set[str] = set()
override_cache = deque(maxlen=OVERRIDE_HISTORY_LIMIT)
override_cache_lock = asyncio.Lock()


@app.on_event("startup")
async def start_background_tasks() -> None:
    global mindsdb_queue_task, memory_write_queue_tasks, mindsdb_write_queue_tasks
    global memory_bank_queue_tasks, letta_write_queue_tasks, outbox_gc_task, hot_memory_rollup_task
    global sink_retention_task
    if MONGO_RAW_ENABLED:
        await init_mongo_client()
    if _use_mongo_outbox():
        await init_fanout_outbox_mongo_client()
    recovered = await recover_stale_running_jobs()
    if recovered:
        logger.warning("Recovered %d stale fanout jobs that were left in running state", recovered)
    if MINDSDB_AUTOSYNC and mindsdb_queue_task is None:
        mindsdb_queue_task = asyncio.create_task(_mindsdb_queue_worker())
    if MINDSDB_AUTOSYNC and MINDSDB_AUTOSYNC_BOOTSTRAP:
        asyncio.create_task(_mindsdb_bootstrap())
    if not memory_bank_queue_tasks:
        worker_count = max(1, MEMORY_BANK_WORKERS)
        for idx in range(worker_count):
            memory_bank_queue_tasks.append(asyncio.create_task(_memory_bank_worker(idx)))
    if not memory_write_queue_tasks:
        worker_count = max(1, MEMORY_WRITE_WORKERS)
        for idx in range(worker_count):
            memory_write_queue_tasks.append(
                asyncio.create_task(_memory_write_worker(idx, exclude_target=FANOUT_TARGET_LETTA))
            )
    if not mindsdb_write_queue_tasks:
        worker_count = max(0, MINDSDB_FANOUT_WORKERS)
        for idx in range(worker_count):
            mindsdb_write_queue_tasks.append(
                asyncio.create_task(
                    _memory_write_worker(
                        2000 + idx,
                        target=FANOUT_TARGET_MINDSDB,
                    )
                )
            )
    if not letta_write_queue_tasks:
        worker_count = max(0, LETTA_FANOUT_WORKERS)
        for idx in range(worker_count):
            letta_write_queue_tasks.append(
                asyncio.create_task(
                    _memory_write_worker(
                        1000 + idx,
                        target=FANOUT_TARGET_LETTA,
                    )
                )
            )
    if FANOUT_OUTBOX_GC_ENABLED and outbox_gc_task is None:
        outbox_gc_task = asyncio.create_task(_fanout_outbox_gc_worker())
    if SINK_RETENTION_ENABLED and sink_retention_task is None:
        sink_retention_task = asyncio.create_task(_sink_retention_worker())
    if HOT_MEMORY_ROLLUP_ENABLED and hot_memory_rollup_task is None:
        hot_memory_rollup_task = asyncio.create_task(_hot_memory_rollup_worker())


@app.on_event("startup")
async def init_mcp_client() -> None:
    global MCP_CLIENT
    if MCP_CLIENT is None:
        MCP_CLIENT = httpx.AsyncClient(timeout=MCP_CLIENT_TIMEOUT, limits=MCP_CLIENT_LIMITS)
    await _ensure_shared_service_clients()
    logger.info(
        "Qdrant backend=%s grpc_prefer=%s cloud_configured=%s fallback=%s",
        _qdrant_operation_targets()[0],
        QDRANT_GRPC_PREFER,
        bool(QDRANT_CLUSTER_ENDPOINT),
        QDRANT_CLOUD_FALLBACK,
    )


@app.on_event("shutdown")
async def close_mcp_client() -> None:
    global MCP_CLIENT, MCP_SESSION_ID, MONGO_CLIENT, FANOUT_OUTBOX_MONGO_CLIENT, outbox_gc_task, hot_memory_rollup_task
    global sink_retention_task, task_scheduler_task, agent_task_worker_tasks
    global QDRANT_CLIENT, QDRANT_CLOUD_CLIENT, MINDSDB_CLIENT, LETTA_CLIENT, LANGFUSE_CLIENT
    if task_scheduler_task is not None:
        task_scheduler_task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task_scheduler_task
        task_scheduler_task = None
    if agent_task_worker_tasks:
        for worker_task in list(agent_task_worker_tasks):
            worker_task.cancel()
        for worker_task in list(agent_task_worker_tasks):
            with contextlib.suppress(asyncio.CancelledError):
                await worker_task
        agent_task_worker_tasks.clear()
    task_runtime_health["workersRunning"] = 0
    task_runtime_health["schedulerRunning"] = False
    task_runtime_health["lastSchedulerTickAt"] = _task_iso_now()
    if hot_memory_rollup_task is not None:
        hot_memory_rollup_task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await hot_memory_rollup_task
        hot_memory_rollup_task = None
    if HOT_MEMORY_ROLLUP_ENABLED:
        with contextlib.suppress(Exception):
            await flush_hot_memory_rollups(force=True)
    if outbox_gc_task is not None:
        outbox_gc_task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await outbox_gc_task
        outbox_gc_task = None
    if sink_retention_task is not None:
        sink_retention_task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await sink_retention_task
        sink_retention_task = None
    if MCP_CLIENT is not None:
        await MCP_CLIENT.aclose()
        MCP_CLIENT = None
    if QDRANT_CLIENT is not None:
        close_result = QDRANT_CLIENT.close()
        if asyncio.iscoroutine(close_result):
            await close_result
        QDRANT_CLIENT = None
    if QDRANT_CLOUD_CLIENT is not None:
        close_result = QDRANT_CLOUD_CLIENT.close()
        if asyncio.iscoroutine(close_result):
            await close_result
        QDRANT_CLOUD_CLIENT = None
    if MINDSDB_CLIENT is not None:
        await MINDSDB_CLIENT.aclose()
        MINDSDB_CLIENT = None
    if LETTA_CLIENT is not None:
        await LETTA_CLIENT.aclose()
        LETTA_CLIENT = None
    if LANGFUSE_CLIENT is not None:
        await LANGFUSE_CLIENT.aclose()
        LANGFUSE_CLIENT = None
    qdrant_collection_dim_cache.clear()
    MCP_SESSION_ID = None
    if FANOUT_OUTBOX_MONGO_CLIENT is not None and FANOUT_OUTBOX_MONGO_CLIENT is not MONGO_CLIENT:
        try:
            await asyncio.to_thread(FANOUT_OUTBOX_MONGO_CLIENT.close)
        except Exception:  # pragma: no cover
            pass
        FANOUT_OUTBOX_MONGO_CLIENT = None
    if MONGO_CLIENT is not None:
        try:
            await asyncio.to_thread(MONGO_CLIENT.close)
        except Exception:  # pragma: no cover
            pass
        MONGO_CLIENT = None
    FANOUT_OUTBOX_MONGO_CLIENT = None
override_seen_files: set[str] = set()
memory_write_history = deque(maxlen=MEMORY_WRITE_HISTORY_LIMIT)
memory_write_history_lock = asyncio.Lock()
memory_write_dedupe_lock = asyncio.Lock()
memory_write_dedupe_seen: dict[str, float] = {}
memory_write_latest_hash_lock = asyncio.Lock()
memory_write_latest_hashes: dict[str, str] = {}
memory_bank_queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue(maxsize=MEMORY_BANK_QUEUE_MAX)
memory_bank_queue_tasks: list[asyncio.Task] = []
memory_bank_queue_dropped = 0
memory_bank_queue_processed = 0
memory_write_last_at: str | None = None
memory_write_last_latency_ms: float | None = None
memory_write_queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue(maxsize=MEMORY_WRITE_QUEUE_MAX)
memory_write_queue_tasks: list[asyncio.Task] = []
mindsdb_write_queue_tasks: list[asyncio.Task] = []
letta_write_queue_tasks: list[asyncio.Task] = []
memory_write_queue_dropped = 0
memory_write_queue_processed = 0
hot_memory_rollup_lock = asyncio.Lock()
hot_memory_rollup_entries: dict[str, dict[str, Any]] = {}
hot_memory_rollup_task: asyncio.Task[Any] | None = None
hot_memory_rollup_health: dict[str, Any] = {
    "pendingKeys": 0,
    "totalBuffered": 0,
    "totalFlushed": 0,
    "totalSkippedUnchanged": 0,
    "lastFlushAt": None,
    "lastFlushCount": 0,
    "lastError": None,
}
topic_tree: Dict[str, Any] = {}
topic_tree_lock = asyncio.Lock()
task_db_lock = asyncio.Lock()
task_db_ready = False
task_scheduler_task: asyncio.Task[Any] | None = None
agent_task_worker_tasks: list[asyncio.Task[Any]] = []
task_runtime_health: dict[str, Any] = {
    "schedulerEnabled": TASK_SCHEDULER_ENABLED,
    "schedulerRunning": False,
    "internalWorkersEnabled": TASK_INTERNAL_WORKERS_ENABLED,
    "workersConfigured": AGENT_TASK_WORKERS,
    "workersRunning": 0,
    "lastSchedulerTickAt": None,
    "lastSchedulerRecovered": 0,
    "lastWorkerError": None,
    "claimed": 0,
    "succeeded": 0,
    "failed": 0,
    "retried": 0,
}
sidecar_health_state: Dict[str, Any] = {
    "updatedAt": None,
    "healthy": None,
    "detail": "unknown",
}
sidecar_health_history = deque(maxlen=SIDECAR_HEALTH_HISTORY_LIMIT)
sidecar_health_lock = asyncio.Lock()


def _apply_trading_snapshot(snapshot: Dict[str, Any]) -> None:
    timestamp = snapshot.get("timestamp")
    if isinstance(timestamp, datetime):
        trading_metrics_state["updatedAt"] = timestamp.isoformat()
    else:
        trading_metrics_state["updatedAt"] = timestamp
    trading_metrics_state["openPositions"] = snapshot.get("open_positions", 0)
    trading_metrics_state["totalValueUsd"] = snapshot.get("total_value_usd", 0.0)
    trading_metrics_state["unrealizedPnl"] = snapshot.get("unrealized_pnl", 0.0)
    trading_metrics_state["realizedPnl"] = snapshot.get("realized_pnl", 0.0)
    trading_metrics_state["dailyPnl"] = snapshot.get("daily_pnl", 0.0)
    trading_metrics_state["positions"] = snapshot.get("positions", [])
    trading_metrics_state["priceCacheEntries"] = snapshot.get("price_cache_entries", 0)
    trading_metrics_state["priceCacheMaxAge"] = snapshot.get("price_cache_max_age", 0.0)
    trading_metrics_state["priceCacheTtl"] = snapshot.get("price_cache_ttl", 0.0)
    trading_metrics_state["priceCacheFreshness"] = snapshot.get("price_cache_freshness", 0.0)
    trading_metrics_state["priceCachePenalty"] = snapshot.get("price_cache_penalty", 1.0)


def _load_trading_history() -> None:
    if not TRADING_HISTORY_PATH.exists():
        return
    try:
        with TRADING_HISTORY_PATH.open("r", encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                snapshot = json.loads(line)
                trading_history.append(snapshot)
        if trading_history:
            _apply_trading_snapshot(trading_history[-1])
    except Exception as exc:  # pragma: no cover - best-effort load
        logger.warning("Failed to load trading history: %s", exc)


async def _persist_trading_snapshot(snapshot: Dict[str, Any]) -> None:
    def _append(path: Path, payload: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(payload)

    line = json.dumps(snapshot) + "\n"
    try:
        await asyncio.to_thread(_append, TRADING_HISTORY_PATH, line)
    except Exception as exc:  # pragma: no cover - disk full, etc.
        logger.warning("Failed to persist trading snapshot: %s", exc)


_load_trading_history()


def _apply_strategy_snapshot(snapshot: Dict[str, Any]) -> None:
    strategy_metrics_state["updatedAt"] = snapshot.get("timestamp")
    strategy_metrics_state["strategies"] = snapshot.get("strategies", [])


def _load_strategy_history() -> None:
    if not STRATEGY_HISTORY_PATH.exists():
        return
    try:
        with STRATEGY_HISTORY_PATH.open("r", encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                snapshot = json.loads(line)
                strategy_history.append(snapshot)
        if strategy_history:
            _apply_strategy_snapshot(strategy_history[-1])
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to load strategy history: %s", exc)


async def _persist_strategy_snapshot(snapshot: Dict[str, Any]) -> None:
    def _append(path: Path, payload: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(payload)

    line = json.dumps(snapshot) + "\n"
    try:
        await asyncio.to_thread(_append, STRATEGY_HISTORY_PATH, line)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to persist strategy snapshot: %s", exc)


_load_strategy_history()


def _load_signal_history() -> None:
    if not SIGNAL_HISTORY_PATH.exists():
        return
    try:
        with SIGNAL_HISTORY_PATH.open("r", encoding="utf-8") as handle:
            for line in handle:
                line = line.strip()
                if not line:
                    continue
                entry = json.loads(line)
                signal_cache.append(entry)
                file_name = entry.get("file")
                if file_name:
                    signal_seen_files.add(file_name)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to load signal history: %s", exc)


async def _persist_signal_entry(entry: Dict[str, Any]) -> None:
    def _append(path: Path, payload: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(payload)

    line = json.dumps(entry) + "\n"
    try:
        await asyncio.to_thread(_append, SIGNAL_HISTORY_PATH, line)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to persist signal entry: %s", exc)


async def _fetch_signals_from_memmcp(limit: int) -> list[Dict[str, Any]]:
    files = await list_files(SIGNAL_PROJECT)
    if not files:
        return []
    files = sorted(files, reverse=True)[:limit]
    entries: list[Dict[str, Any]] = []
    for file_name in files:
        try:
            content = await read_project_file(SIGNAL_PROJECT, file_name)
            if not content:
                continue
            data = json.loads(content)
            data["file"] = file_name
            entries.append(data)
        except Exception as exc:
            logger.warning("Failed to read signal file %s: %s", file_name, exc)
    return entries


def _normalize_signal(raw: Dict[str, Any]) -> Dict[str, Any]:
    def _to_float(value: Any) -> float:
        try:
            return float(value)
        except (TypeError, ValueError):
            return 0.0

    created = raw.get("created_at") or raw.get("createdAt")
    created_dt = None
    if isinstance(created, str):
        try:
            created_dt = datetime.fromisoformat(created)
        except ValueError:
            try:
                created_dt = datetime.fromisoformat(created.replace("Z", "+00:00"))
            except ValueError:
                created_dt = None

    return {
        "symbol": raw.get("symbol", "").upper(),
        "address": raw.get("address", ""),
        "price_usd": _to_float(raw.get("price_usd")),
        "volume_24h_usd": _to_float(raw.get("volume_24h_usd")),
        "liquidity_usd": _to_float(raw.get("liquidity_usd")),
        "momentum_score": _to_float(raw.get("momentum_score")),
        "risk_score": _to_float(raw.get("risk_score")),
        "verified": bool(raw.get("verified", False)),
        "created_at": created_dt.isoformat() if created_dt else raw.get("created_at"),
        "file": raw.get("file", ""),
    }


async def _refresh_signal_cache() -> None:
    try:
        raw_entries = await _fetch_signals_from_memmcp(SIGNAL_FETCH_LIMIT)
    except Exception as exc:
        logger.warning("Failed to refresh signals: %s", exc)
        return

    if not raw_entries:
        return

    new_entries: list[Dict[str, Any]] = []
    async with signal_cache_lock:
        for raw in reversed(raw_entries):
            file_name = raw.get("file")
            if not file_name or file_name in signal_seen_files:
                continue
            normalized = _normalize_signal(raw)
            signal_cache.append(normalized)
            signal_seen_files.add(file_name)
            new_entries.append(normalized)

    for entry in new_entries:
        await _persist_signal_entry(entry)


async def _signal_refresh_loop() -> None:
    while True:
        await _refresh_signal_cache()
        await asyncio.sleep(SIGNAL_REFRESH_SECONDS)
_load_signal_history()


def _load_override_history() -> None:
    if not OVERRIDE_HISTORY_PATH.exists():
        return
    try:
        with OVERRIDE_HISTORY_PATH.open("r", encoding="utf-8") as handle:
            for line in handle:
                payload = line.strip()
                if not payload:
                    continue
                entry = json.loads(payload)
                override_cache.append(entry)
                file_name = entry.get("file")
                if file_name:
                    override_seen_files.add(file_name)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to load override history: %s", exc)


async def _persist_override_entry(entry: Dict[str, Any]) -> None:
    def _append(path: Path, payload: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(payload)

    line = json.dumps(entry) + "\n"
    try:
        await asyncio.to_thread(_append, OVERRIDE_HISTORY_PATH, line)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to persist override entry: %s", exc)


async def _fetch_overrides_from_memmcp(limit: int) -> list[Dict[str, Any]]:
    files = await list_files(OVERRIDE_PROJECT)
    if not files:
        return []
    files = sorted(files, reverse=True)[:limit]
    entries: list[Dict[str, Any]] = []
    for file_name in files:
        normalized_file = normalize_memory_path(file_name)
        if normalized_file in OPTIONAL_OVERRIDE_FILENAMES:
            # Smoke-test placeholders are operational probes, not user overrides.
            continue
        try:
            content = await read_project_file(
                OVERRIDE_PROJECT,
                normalized_file,
                allow_missing=True,
                bootstrap_missing=True,
            )
            if not content:
                continue
            data = json.loads(content)
            data["file"] = normalized_file
            entries.append(data)
        except Exception as exc:
            logger.warning("Failed to read override file %s: %s", file_name, exc)
    return entries


def _normalize_override(raw: Dict[str, Any]) -> Dict[str, Any]:
    def _to_float(value: Any) -> float:
        try:
            return float(value)
        except (TypeError, ValueError):
            return 0.0

    created = raw.get("timestamp") or raw.get("created_at")
    created_dt = None
    if isinstance(created, str):
        try:
            created_dt = datetime.fromisoformat(created.replace("Z", "+00:00"))
        except ValueError:
            created_dt = None

    return {
        "symbol": (raw.get("symbol") or "").upper(),
        "priority": raw.get("priority", "MEDIUM"),
        "reason": raw.get("reason", ""),
        "size_before": _to_float(raw.get("size_before")),
        "size_after": _to_float(raw.get("size_after")),
        "final_size": _to_float(raw.get("final_size")),
        "confidence_before": _to_float(raw.get("confidence_before")),
        "confidence_after": _to_float(raw.get("confidence_after")),
        "override_strength": _to_float(raw.get("override_strength")),
        "multiplier": _to_float(raw.get("multiplier", 1.0)),
        "kelly_fraction": _to_float(raw.get("kelly_fraction")),
        "kelly_target": _to_float(raw.get("kelly_target")),
        "timestamp": created_dt.isoformat() if created_dt else created,
        "file": raw.get("file", ""),
    }


async def _refresh_override_cache() -> None:
    try:
        raw_entries = await _fetch_overrides_from_memmcp(OVERRIDE_FETCH_LIMIT)
    except Exception as exc:
        logger.warning("Failed to refresh overrides: %s", exc)
        return

    if not raw_entries:
        return

    new_entries: list[Dict[str, Any]] = []
    async with override_cache_lock:
        for raw in reversed(raw_entries):
            file_name = raw.get("file")
            if not file_name or file_name in override_seen_files:
                continue
            normalized = _normalize_override(raw)
            override_cache.append(normalized)
            override_seen_files.add(file_name)
            new_entries.append(normalized)

    for entry in new_entries:
        await _persist_override_entry(entry)


async def _override_refresh_loop() -> None:
    while True:
        await _refresh_override_cache()
        await asyncio.sleep(OVERRIDE_REFRESH_SECONDS)


def _load_memory_write_history() -> None:
    if not MEMORY_WRITE_HISTORY_PATH.exists():
        return
    try:
        with MEMORY_WRITE_HISTORY_PATH.open("r", encoding="utf-8") as handle:
            for line in handle:
                payload = line.strip()
                if not payload:
                    continue
                entry = json.loads(payload)
                memory_write_history.append(entry)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to load memory write history: %s", exc)


def _load_topic_tree() -> None:
    if not TOPIC_INDEX_PATH.exists():
        if memory_write_history:
            for entry in list(memory_write_history):
                project = entry.get("project")
                topic_path = entry.get("topic_path") or DEFAULT_TOPIC_ROOT
                if project:
                    segments = [seg for seg in topic_path.split("/") if seg]
                    project_node = topic_tree.setdefault(
                        project, {"count": 0, "children": {}}
                    )
                    project_node["count"] = int(project_node.get("count", 0)) + 1
                    current = project_node
                    for segment in segments:
                        children = current.setdefault("children", {})
                        node = children.setdefault(segment, {"count": 0, "children": {}})
                        node["count"] = int(node.get("count", 0)) + 1
                        current = node
        return
    try:
        with TOPIC_INDEX_PATH.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
        if isinstance(data, dict):
            topic_tree.update(data)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to load topic tree: %s", exc)


async def _persist_topic_tree() -> None:
    def _write(path: Path, payload: dict[str, Any]) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("w", encoding="utf-8") as handle:
            json.dump(payload, handle, indent=2, sort_keys=True)

    async with topic_tree_lock:
        snapshot = json.loads(json.dumps(topic_tree))
    try:
        await asyncio.to_thread(_write, TOPIC_INDEX_PATH, snapshot)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to persist topic tree: %s", exc)


async def _update_topic_tree(project: str, topic_path: str) -> None:
    segments = [seg for seg in topic_path.split("/") if seg]
    async with topic_tree_lock:
        project_node = topic_tree.setdefault(project, {"count": 0, "children": {}})
        project_node["count"] = int(project_node.get("count", 0)) + 1
        current = project_node
        for segment in segments:
            children = current.setdefault("children", {})
            node = children.setdefault(segment, {"count": 0, "children": {}})
            node["count"] = int(node.get("count", 0)) + 1
            current = node
    await _persist_topic_tree()


def _truncate_topic_tree(node: dict[str, Any], depth: int) -> dict[str, Any]:
    if depth <= 0:
        return {"count": node.get("count", 0)}
    children = node.get("children") or {}
    trimmed_children = {name: _truncate_topic_tree(child, depth - 1) for name, child in children.items()}
    return {"count": node.get("count", 0), "children": trimmed_children}


def _iter_topic_paths(
    node: dict[str, Any],
    *,
    prefix: str = "",
    depth: int = 8,
):
    if depth <= 0:
        return
    children = node.get("children") or {}
    if not isinstance(children, dict):
        return
    for segment, child in children.items():
        if not isinstance(segment, str) or not isinstance(child, dict):
            continue
        path = f"{prefix}/{segment}" if prefix else segment
        count = int(child.get("count", 0) or 0)
        yield path, max(0, count)
        yield from _iter_topic_paths(child, prefix=path, depth=depth - 1)


def _list_topics_snapshot(
    *,
    project: str | None = None,
    prefix: str | None = None,
    limit: int = 200,
    min_count: int = 1,
    depth: int = 8,
) -> dict[str, Any]:
    depth = max(1, min(depth, 16))
    limit = max(1, min(limit, 5000))
    min_count = max(1, min_count)
    normalized_prefix = normalize_topic_path(prefix) if prefix else None
    topics: list[dict[str, Any]] = []

    def _append_for_project(project_name: str, node: dict[str, Any]) -> None:
        for path, count in _iter_topic_paths(node, depth=depth):
            if count < min_count:
                continue
            if normalized_prefix and not path.startswith(normalized_prefix):
                continue
            topics.append({"project": project_name, "path": path, "count": count})

    if project:
        _append_for_project(project, topic_tree.get(project, {"count": 0, "children": {}}))
    else:
        for project_name, node in topic_tree.items():
            if not isinstance(project_name, str) or not isinstance(node, dict):
                continue
            _append_for_project(project_name, node)

    topics.sort(
        key=lambda item: (
            -int(item.get("count", 0)),
            str(item.get("project", "")),
            str(item.get("path", "")),
        )
    )
    total = len(topics)
    items = topics[:limit]
    payload: dict[str, Any] = {
        "topics": items,
        "total": total,
        "limit": limit,
        "min_count": min_count,
        "depth": depth,
    }
    if project:
        payload["project"] = project
    if normalized_prefix:
        payload["prefix"] = normalized_prefix
    return payload


async def _persist_memory_write(entry: Dict[str, Any]) -> None:
    def _append(path: Path, payload: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(payload)

    line = json.dumps(entry) + "\n"
    try:
        await asyncio.to_thread(_append, MEMORY_WRITE_HISTORY_PATH, line)
    except Exception as exc:  # pragma: no cover
        logger.warning("Failed to persist memory write entry: %s", exc)


def _task_db_connect() -> sqlite3.Connection:
    TASK_DB_PATH.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(TASK_DB_PATH, timeout=TASK_DB_TIMEOUT)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA busy_timeout = 5000")
    # Keep per-connection pragmas lightweight; WAL mode is set once during init.
    try:
        conn.execute("PRAGMA synchronous = NORMAL")
    except sqlite3.OperationalError:
        # External-volume hiccups can make pragma writes transiently fail.
        pass
    return conn


def _init_task_db() -> None:
    with _task_db_connect() as conn:
        try:
            conn.execute("PRAGMA journal_mode = WAL")
        except sqlite3.OperationalError as exc:
            logger.warning("Task DB WAL mode unavailable; continuing without WAL: %s", exc)
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS tasks (
                id TEXT PRIMARY KEY,
                title TEXT NOT NULL,
                status TEXT NOT NULL,
                project TEXT,
                agent TEXT,
                priority INTEGER DEFAULT 0,
                payload TEXT,
                run_after TEXT,
                attempts INTEGER NOT NULL DEFAULT 0,
                max_attempts INTEGER NOT NULL DEFAULT 3,
                lease_expires_at TEXT,
                claimed_by TEXT,
                last_error TEXT,
                result TEXT,
                completed_at TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            )
            """
        )
        columns = {row["name"] for row in conn.execute("PRAGMA table_info(tasks)")}
        if "approval_required" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN approval_required INTEGER DEFAULT 0")
        if "approved" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN approved INTEGER DEFAULT 0")
        if "risk_level" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN risk_level TEXT")
        if "action_type" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN action_type TEXT")
        if "run_after" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN run_after TEXT")
        if "attempts" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0")
        if "max_attempts" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 3")
        if "lease_expires_at" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN lease_expires_at TEXT")
        if "claimed_by" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN claimed_by TEXT")
        if "last_error" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN last_error TEXT")
        if "result" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN result TEXT")
        if "completed_at" not in columns:
            conn.execute("ALTER TABLE tasks ADD COLUMN completed_at TEXT")
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_tasks_status_due ON tasks(status, run_after, priority, created_at)"
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_tasks_running_lease ON tasks(status, lease_expires_at)"
        )
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS task_events (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                task_id TEXT NOT NULL,
                timestamp TEXT NOT NULL,
                status TEXT NOT NULL,
                message TEXT,
                metadata TEXT
            )
            """
        )
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS feedback (
                id TEXT PRIMARY KEY,
                created_at TEXT NOT NULL,
                project TEXT,
                user_id TEXT,
                source TEXT,
                task_id TEXT,
                rating INTEGER,
                sentiment TEXT,
                tags TEXT,
                content TEXT,
                topic_path TEXT,
                metadata TEXT
            )
            """
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_feedback_project_created ON feedback(project, created_at)"
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_feedback_user_created ON feedback(user_id, created_at)"
        )
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS fanout_outbox (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                event_id TEXT NOT NULL,
                target TEXT NOT NULL,
                project TEXT NOT NULL,
                file TEXT NOT NULL,
                summary TEXT,
                payload TEXT NOT NULL,
                topic_path TEXT,
                topic_tags TEXT,
                status TEXT NOT NULL,
                attempts INTEGER NOT NULL DEFAULT 0,
                max_attempts INTEGER NOT NULL DEFAULT 0,
                next_attempt_at TEXT NOT NULL,
                last_attempt_at TEXT,
                completed_at TEXT,
                last_error TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                dedupe_key TEXT NOT NULL UNIQUE
            )
            """
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_fanout_outbox_status_next ON fanout_outbox(status, next_attempt_at)"
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_fanout_outbox_event ON fanout_outbox(event_id)"
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_fanout_outbox_target_status ON fanout_outbox(target, status)"
        )


async def ensure_task_db() -> None:
    global task_db_ready
    if task_db_ready:
        return
    async with task_db_lock:
        if task_db_ready:
            return
        try:
            await asyncio.to_thread(_init_task_db)
        except Exception as exc:  # pragma: no cover
            logger.warning("Failed to init task DB: %s", exc)
            return
        task_db_ready = True


async def _task_db_exec(fn) -> Any:
    await ensure_task_db()

    def _run():
        retries = max(1, TASK_DB_LOCK_RETRIES)
        for attempt in range(1, retries + 1):
            try:
                with _task_db_connect() as conn:
                    return fn(conn)
            except sqlite3.OperationalError as exc:
                message = str(exc).lower()
                retryable = "locked" in message or "disk i/o error" in message
                if not retryable or attempt >= retries:
                    raise
                time.sleep(TASK_DB_LOCK_BACKOFF_SECS * attempt)

    return await asyncio.to_thread(_run)


def _utc_now() -> str:
    return datetime.utcnow().isoformat() + "Z"


def _backoff_seconds(attempt: int) -> float:
    exp = max(0, attempt - 1)
    base = FANOUT_RETRY_BASE_SECS * (2**exp)
    bounded = min(base, FANOUT_RETRY_MAX_SECS)
    jitter = random.uniform(0, min(1.0, bounded * 0.2))
    return bounded + jitter


def build_event_id(project: str, file_name: str, content: str) -> str:
    digest = hashlib.sha256(f"{project}:{file_name}:{content}".encode("utf-8")).hexdigest()
    return digest[:32]


def build_memory_write_dedupe_key(project: str, file_name: str, content: str) -> str:
    digest = hashlib.sha256(content.encode("utf-8")).hexdigest()
    return f"{project}:{file_name}:{digest[:32]}"


def memory_content_sha256(content: str) -> str:
    return hashlib.sha256(content.encode("utf-8")).hexdigest()


async def should_skip_duplicate_memory_write(dedupe_key: str, now_monotonic: float | None = None) -> bool:
    if not MEMORY_WRITE_DEDUP_ENABLED:
        return False
    window = max(0.0, MEMORY_WRITE_DEDUP_WINDOW_SECS)
    if window <= 0:
        return False
    now = float(now_monotonic if now_monotonic is not None else time.monotonic())
    cutoff = now - window
    async with memory_write_dedupe_lock:
        stale_keys = [key for key, seen_at in memory_write_dedupe_seen.items() if seen_at < cutoff]
        for key in stale_keys:
            memory_write_dedupe_seen.pop(key, None)
        previous_seen = memory_write_dedupe_seen.get(dedupe_key)
        memory_write_dedupe_seen[dedupe_key] = now
        max_keys = max(100, MEMORY_WRITE_DEDUP_MAX_KEYS)
        if len(memory_write_dedupe_seen) > max_keys:
            overflow = len(memory_write_dedupe_seen) - max_keys
            oldest = sorted(memory_write_dedupe_seen.items(), key=lambda item: item[1])[:overflow]
            for key, _ in oldest:
                memory_write_dedupe_seen.pop(key, None)
    return previous_seen is not None and (now - previous_seen) <= window


def is_hot_memory_file(file_name: str) -> bool:
    lowered = (file_name or "").lower()
    if not lowered:
        return False
    for suffix in HOT_MEMORY_FILE_SUFFIXES:
        if lowered.endswith(suffix):
            return True
    return False


def build_hot_memory_rollup_file(file_name: str) -> str:
    normalized = normalize_memory_path(file_name)
    if not normalized:
        return "_rollups/hot__rollup.json"
    directory = ""
    base = normalized
    if "/" in normalized:
        directory, base = normalized.rsplit("/", 1)
    if base.endswith(HOT_MEMORY_ROLLUP_SUFFIX):
        rollup_name = base
    else:
        stem = base[:-5] if base.endswith(".json") else base
        rollup_name = f"{stem}{HOT_MEMORY_ROLLUP_SUFFIX}"
    if directory:
        return f"{directory}/_rollups/{rollup_name}"
    return f"_rollups/{rollup_name}"


async def should_skip_unchanged_latest_hash(project: str, file_name: str, content_hash: str) -> bool:
    if not MEMORY_WRITE_LATEST_HASH_DEDUP_ENABLED:
        return False
    dedupe_key = f"{project}:{file_name}"
    max_keys = max(1000, MEMORY_WRITE_LATEST_HASH_DEDUP_MAX_KEYS)
    async with memory_write_latest_hash_lock:
        previous_hash = memory_write_latest_hashes.get(dedupe_key)
        memory_write_latest_hashes[dedupe_key] = content_hash
        while len(memory_write_latest_hashes) > max_keys:
            oldest_key = next(iter(memory_write_latest_hashes))
            if oldest_key == dedupe_key and len(memory_write_latest_hashes) > 1:
                second_key = list(memory_write_latest_hashes.keys())[1]
                memory_write_latest_hashes.pop(second_key, None)
                continue
            memory_write_latest_hashes.pop(oldest_key, None)
            if oldest_key == dedupe_key:
                memory_write_latest_hashes[dedupe_key] = content_hash
    return previous_hash is not None and previous_hash == content_hash


def build_hot_memory_rollup_content(entry: dict[str, Any]) -> str:
    payload = {
        "kind": "high_frequency_rollup",
        "project": entry.get("project"),
        "source_file": entry.get("file"),
        "source_content_sha256": entry.get("last_hash"),
        "source_size_bytes": int(entry.get("last_size") or 0),
        "topic_path": entry.get("topic_path"),
        "topic_tags": list(entry.get("topic_tags") or []),
        "summary": entry.get("summary") or "",
        "events_since_last_rollup": int(entry.get("events_since_flush") or 0),
        "bytes_since_last_rollup": int(entry.get("bytes_since_flush") or 0),
        "first_seen_at": entry.get("first_seen_at"),
        "last_seen_at": entry.get("last_seen_at"),
        "rollup_generated_at": _utc_now(),
    }
    return json.dumps(payload, ensure_ascii=True, sort_keys=True)


async def enqueue_hot_memory_rollup(item: dict[str, Any]) -> None:
    key = f"{item['project']}:{item['file']}"
    now_iso = _utc_now()
    now_monotonic = time.monotonic()
    async with hot_memory_rollup_lock:
        entry = hot_memory_rollup_entries.get(key)
        if entry is None:
            entry = {
                "project": item["project"],
                "file": item["file"],
                "topic_path": item.get("topic_path"),
                "topic_tags": list(item.get("topic_tags") or []),
                "summary": item.get("summary") or "",
                "last_hash": item.get("content_hash"),
                "last_size": int(item.get("content_length") or 0),
                "letta_session": item.get("letta_session"),
                "letta_admit": bool(item.get("letta_admit", True)),
                "letta_context": dict(item.get("letta_context") or {}),
                "qdrant_collection": item.get("qdrant_collection"),
                "events_since_flush": 0,
                "bytes_since_flush": 0,
                "first_seen_at": now_iso,
                "last_seen_at": now_iso,
                "last_flush_monotonic": 0.0,
                "last_seen_monotonic": now_monotonic,
            }
            hot_memory_rollup_entries[key] = entry
        entry["topic_path"] = item.get("topic_path")
        entry["topic_tags"] = list(item.get("topic_tags") or [])
        entry["summary"] = item.get("summary") or ""
        entry["last_hash"] = item.get("content_hash")
        entry["last_size"] = int(item.get("content_length") or 0)
        entry["letta_session"] = item.get("letta_session")
        entry["letta_admit"] = bool(item.get("letta_admit", True))
        entry["letta_context"] = dict(item.get("letta_context") or {})
        entry["qdrant_collection"] = item.get("qdrant_collection")
        entry["last_seen_at"] = now_iso
        entry["last_seen_monotonic"] = now_monotonic
        entry["events_since_flush"] = int(entry.get("events_since_flush") or 0) + 1
        entry["bytes_since_flush"] = int(entry.get("bytes_since_flush") or 0) + int(item.get("content_length") or 0)
        hot_memory_rollup_health["pendingKeys"] = len(hot_memory_rollup_entries)
        hot_memory_rollup_health["totalBuffered"] = int(hot_memory_rollup_health.get("totalBuffered") or 0) + 1


async def flush_hot_memory_rollups(force: bool = False) -> dict[str, Any]:
    interval_secs = max(1.0, HOT_MEMORY_ROLLUP_FLUSH_SECS)
    now_monotonic = time.monotonic()
    pending: list[tuple[str, dict[str, Any]]] = []
    async with hot_memory_rollup_lock:
        for key, entry in hot_memory_rollup_entries.items():
            events_since_flush = int(entry.get("events_since_flush") or 0)
            if events_since_flush <= 0:
                continue
            last_flush_monotonic = float(entry.get("last_flush_monotonic") or 0.0)
            is_due = force or last_flush_monotonic <= 0.0 or (now_monotonic - last_flush_monotonic) >= interval_secs
            if not is_due:
                continue
            snapshot = {
                "project": entry.get("project"),
                "file": entry.get("file"),
                "topic_path": entry.get("topic_path"),
                "topic_tags": list(entry.get("topic_tags") or []),
                "summary": entry.get("summary") or "",
                "last_hash": entry.get("last_hash"),
                "last_size": int(entry.get("last_size") or 0),
                "letta_session": entry.get("letta_session"),
                "letta_admit": bool(entry.get("letta_admit", True)),
                "letta_context": dict(entry.get("letta_context") or {}),
                "qdrant_collection": entry.get("qdrant_collection"),
                "first_seen_at": entry.get("first_seen_at"),
                "last_seen_at": entry.get("last_seen_at"),
                "events_since_flush": events_since_flush,
                "bytes_since_flush": int(entry.get("bytes_since_flush") or 0),
            }
            entry["events_since_flush"] = 0
            entry["bytes_since_flush"] = 0
            entry["last_flush_monotonic"] = now_monotonic
            pending.append((key, snapshot))

    flushed = 0
    failures: list[str] = []
    for key, entry in pending:
        try:
            rollup_file = build_hot_memory_rollup_file(str(entry.get("file") or ""))
            rollup_content = build_hot_memory_rollup_content(entry)
            rollup_summary = str(entry.get("summary") or "")
            if len(rollup_summary) > 400:
                rollup_summary = rollup_summary[:400]
            letta_context = dict(entry.get("letta_context") or {})
            letta_context.update(
                {
                    "project": entry.get("project"),
                    "file": entry.get("file"),
                    "topic_path": entry.get("topic_path"),
                    "rollup_file": rollup_file,
                    "source_kind": "high_frequency_rollup",
                }
            )
            await _enqueue_memory_bank_write(
                {
                    "project": entry.get("project"),
                    "file": entry.get("file"),
                    "payload": {
                        "projectName": entry.get("project"),
                        "fileName": rollup_file,
                        "content": rollup_content,
                    },
                    "summary": rollup_summary,
                    "topic_path": entry.get("topic_path"),
                    "topic_tags": list(entry.get("topic_tags") or []),
                    "content_length": int(entry.get("last_size") or 0),
                    "letta_session": entry.get("letta_session"),
                    "letta_admit": bool(entry.get("letta_admit", True)),
                    "letta_context": letta_context,
                    "waiter": None,
                    "start_time": None,
                    "request_id": "hot-rollup",
                    "event_id": uuid.uuid4().hex,
                    "raw_event": None,
                    "mongo_persisted": True,
                    "qdrant_collection": entry.get("qdrant_collection") or QDRANT_COLLECTION,
                }
            )
            flushed += 1
        except Exception as exc:  # pragma: no cover - queue/runtime specific
            failure = f"{key}: {exc}"
            failures.append(failure)
            async with hot_memory_rollup_lock:
                buffered = hot_memory_rollup_entries.get(key)
                if buffered is not None:
                    buffered["events_since_flush"] = int(buffered.get("events_since_flush") or 0) + int(
                        entry.get("events_since_flush") or 0
                    )
                    buffered["bytes_since_flush"] = int(buffered.get("bytes_since_flush") or 0) + int(
                        entry.get("bytes_since_flush") or 0
                    )

    async with hot_memory_rollup_lock:
        hot_memory_rollup_health["pendingKeys"] = len(hot_memory_rollup_entries)
        hot_memory_rollup_health["lastFlushAt"] = _utc_now()
        hot_memory_rollup_health["lastFlushCount"] = flushed
        hot_memory_rollup_health["totalFlushed"] = int(hot_memory_rollup_health.get("totalFlushed") or 0) + flushed
        hot_memory_rollup_health["lastError"] = "; ".join(failures[:3]) if failures else None
    return {"flushed": flushed, "attempted": len(pending), "errors": failures[:10]}


async def _hot_memory_rollup_worker() -> None:
    interval_secs = max(1.0, HOT_MEMORY_ROLLUP_FLUSH_SECS)
    while True:
        try:
            await asyncio.sleep(interval_secs)
            result = await flush_hot_memory_rollups(force=False)
            if result.get("flushed"):
                _json_log(
                    "memory.write.rollup_flushed",
                    {
                        "flushed": result.get("flushed"),
                        "attempted": result.get("attempted"),
                        "errors": len(result.get("errors") or []),
                    },
                )
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # pragma: no cover
            hot_memory_rollup_health["lastError"] = str(exc)[:300]
            logger.warning("Hot memory rollup worker failed: %s", exc)


def build_raw_memory_event(
    event_id: str,
    project: str,
    file_name: str,
    content: str,
    summary: str,
    topic_path: str,
    topic_tags: list[str],
    request_id: str | None,
    source: str = "memory.write",
) -> dict[str, Any]:
    created_at = _utc_now()
    return {
        "event_id": event_id,
        "source": source,
        "project": project,
        "file": file_name,
        "content_raw": content,
        "summary": summary,
        "topic_path": topic_path,
        "topic_tags": topic_tags,
        "request_id": request_id,
        "created_at": created_at,
        "updated_at": created_at,
    }


async def init_mongo_client() -> bool:
    global MONGO_CLIENT
    if not MONGO_RAW_ENABLED:
        return False
    if MongoClient is None:
        logger.warning("pymongo not available; mongo raw writes disabled")
        return False
    if MONGO_CLIENT is not None:
        return True
    async with mongo_client_lock:
        if MONGO_CLIENT is not None:
            return True

        def _connect():
            client = MongoClient(
                MONGO_RAW_URI,
                connectTimeoutMS=MONGO_RAW_CONNECT_TIMEOUT_MS,
                serverSelectionTimeoutMS=MONGO_RAW_SERVER_SELECTION_TIMEOUT_MS,
                socketTimeoutMS=MONGO_RAW_SOCKET_TIMEOUT_MS,
                waitQueueTimeoutMS=MONGO_RAW_WAIT_QUEUE_TIMEOUT_MS,
                maxPoolSize=MONGO_RAW_MAX_POOL_SIZE,
                minPoolSize=MONGO_RAW_MIN_POOL_SIZE,
            )
            coll = client[MONGO_RAW_DB][MONGO_RAW_COLLECTION]
            coll.create_index("event_id", unique=True)
            coll.create_index("created_at")
            # Force an initial round-trip so startup reports failures early.
            client.admin.command("ping")
            return client

        try:
            MONGO_CLIENT = await asyncio.to_thread(_connect)
            return True
        except Exception as exc:  # pragma: no cover - network/runtime specific
            logger.warning("Mongo raw store init failed: %s", exc)
            MONGO_CLIENT = None
            return False


async def persist_raw_event_to_mongo(event: dict[str, Any]) -> tuple[bool, str | None]:
    if not await init_mongo_client():
        return False, "mongo client unavailable"

    def _upsert() -> None:
        assert MONGO_CLIENT is not None  # guarded by init_mongo_client
        coll = MONGO_CLIENT[MONGO_RAW_DB][MONGO_RAW_COLLECTION]
        now = _utc_now()
        payload = dict(event)
        payload.pop("updated_at", None)
        if "created_at" not in payload:
            payload["created_at"] = now
        coll.update_one(
            {"event_id": event["event_id"]},
            {"$setOnInsert": payload, "$set": {"updated_at": now}},
            upsert=True,
        )

    try:
        await asyncio.to_thread(_upsert)
        return True, None
    except Exception as exc:  # pragma: no cover - driver/network specific
        return False, str(exc)


async def persist_raw_events_to_mongo(events: list[dict[str, Any]]) -> tuple[bool, str | None]:
    if not events:
        return True, None
    if not await init_mongo_client():
        return False, "mongo client unavailable"

    if UpdateOne is None:
        for event in events:
            ok, error = await persist_raw_event_to_mongo(event)
            if not ok:
                return False, error or "mongo raw write failed"
        return True, None

    def _bulk_upsert() -> None:
        assert MONGO_CLIENT is not None  # guarded by init_mongo_client
        coll = MONGO_CLIENT[MONGO_RAW_DB][MONGO_RAW_COLLECTION]
        now = _utc_now()
        operations = []
        for event in events:
            event_id = str(event.get("event_id") or "").strip()
            if not event_id:
                raise OrchestratorError("raw_event payload missing event_id for mongo batch fanout")
            payload = dict(event)
            payload.pop("updated_at", None)
            if "created_at" not in payload:
                payload["created_at"] = now
            operations.append(
                UpdateOne(
                    {"event_id": event_id},
                    {"$setOnInsert": payload, "$set": {"updated_at": now}},
                    upsert=True,
                )
            )
        if operations:
            coll.bulk_write(operations, ordered=False)

    try:
        await asyncio.to_thread(_bulk_upsert)
        return True, None
    except Exception as exc:  # pragma: no cover - driver/network specific
        return False, str(exc)


def _use_mongo_outbox() -> bool:
    return fanout_outbox_backend_active == "mongo"


def _demote_outbox_backend(reason: str) -> None:
    global fanout_outbox_backend_active
    if fanout_outbox_backend_active == "sqlite":
        return
    if not FANOUT_OUTBOX_FALLBACK_TO_SQLITE:
        return
    fanout_outbox_backend_active = "sqlite"
    logger.warning("Fanout outbox backend fell back to sqlite: %s", reason[:300])


async def _promote_outbox_backend_to_mongo_if_sqlite_error(reason: str) -> bool:
    global fanout_outbox_backend_active
    if fanout_outbox_backend_active != "sqlite":
        return False
    if not FANOUT_OUTBOX_AUTO_PROMOTE_MONGO_ON_SQLITE_IO_ERROR:
        return False
    if not _is_sqlite_disk_io_error(reason):
        return False
    previous = fanout_outbox_backend_active
    fanout_outbox_backend_active = "mongo"
    ok = await init_fanout_outbox_mongo_client()
    if ok:
        logger.warning("Fanout outbox backend promoted to mongo after sqlite error: %s", reason[:300])
        return True
    fanout_outbox_backend_active = previous
    return False


async def init_fanout_outbox_mongo_client() -> bool:
    global FANOUT_OUTBOX_MONGO_CLIENT
    if not _use_mongo_outbox():
        return False
    if MongoClient is None or ReturnDocument is None:
        logger.warning("pymongo not available; cannot use FANOUT_OUTBOX_BACKEND=mongo")
        _demote_outbox_backend("pymongo not available")
        return False
    if FANOUT_OUTBOX_MONGO_CLIENT is not None:
        return True
    async with fanout_outbox_mongo_lock:
        if FANOUT_OUTBOX_MONGO_CLIENT is not None:
            return True

        async def _connect_with_primary_mongo() -> bool:
            global FANOUT_OUTBOX_MONGO_CLIENT
            if FANOUT_OUTBOX_MONGO_URI != MONGO_RAW_URI:
                return False
            if not await init_mongo_client():
                return False
            FANOUT_OUTBOX_MONGO_CLIENT = MONGO_CLIENT
            return True

        def _create_indexes() -> None:
            assert FANOUT_OUTBOX_MONGO_CLIENT is not None
            coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
            coll.create_index("dedupe_key", unique=True)
            coll.create_index([("status", 1), ("next_attempt_at", 1), ("_id", 1)])
            coll.create_index([("target", 1), ("status", 1), ("next_attempt_at", 1), ("_id", 1)])
            coll.create_index("event_id")
            FANOUT_OUTBOX_MONGO_CLIENT.admin.command("ping")

        try:
            if not await _connect_with_primary_mongo():
                def _connect() -> Any:
                    return MongoClient(
                        FANOUT_OUTBOX_MONGO_URI,
                        connectTimeoutMS=MONGO_RAW_CONNECT_TIMEOUT_MS,
                        serverSelectionTimeoutMS=MONGO_RAW_SERVER_SELECTION_TIMEOUT_MS,
                        socketTimeoutMS=MONGO_RAW_SOCKET_TIMEOUT_MS,
                        waitQueueTimeoutMS=MONGO_RAW_WAIT_QUEUE_TIMEOUT_MS,
                        maxPoolSize=MONGO_RAW_MAX_POOL_SIZE,
                        minPoolSize=MONGO_RAW_MIN_POOL_SIZE,
                    )
                FANOUT_OUTBOX_MONGO_CLIENT = await asyncio.to_thread(_connect)
            await asyncio.to_thread(_create_indexes)
            return True
        except Exception as exc:
            logger.warning("Mongo fanout outbox init failed: %s", exc)
            FANOUT_OUTBOX_MONGO_CLIENT = None
            _demote_outbox_backend(str(exc))
            return False


def _fanout_doc_to_dict(doc: dict[str, Any]) -> dict[str, Any]:
    payload = doc.get("payload")
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except json.JSONDecodeError:
            payload = {}
    if not isinstance(payload, dict):
        payload = {}
    topic_tags = doc.get("topic_tags")
    if isinstance(topic_tags, str):
        try:
            topic_tags = json.loads(topic_tags)
        except json.JSONDecodeError:
            topic_tags = []
    if not isinstance(topic_tags, list):
        topic_tags = []
    return {
        "id": str(doc.get("_id")),
        "event_id": doc.get("event_id"),
        "target": doc.get("target"),
        "project": doc.get("project"),
        "file": doc.get("file"),
        "summary": doc.get("summary"),
        "payload": payload,
        "topic_path": doc.get("topic_path"),
        "topic_tags": topic_tags,
        "status": doc.get("status"),
        "attempts": _safe_int(doc.get("attempts")),
        "max_attempts": _safe_int(doc.get("max_attempts"), FANOUT_MAX_ATTEMPTS),
        "next_attempt_at": doc.get("next_attempt_at"),
        "last_attempt_at": doc.get("last_attempt_at"),
        "completed_at": doc.get("completed_at"),
        "last_error": doc.get("last_error"),
        "created_at": doc.get("created_at"),
        "updated_at": doc.get("updated_at"),
    }


async def _enqueue_fanout_outbox_mongo(
    event_payload: dict[str, Any],
    targets: list[str],
    force_requeue: bool = False,
) -> dict[str, Any]:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None
    now = _utc_now()
    event_id = str(event_payload.get("event_id") or uuid.uuid4().hex)
    summary = str(event_payload.get("summary") or "")
    project = str(event_payload.get("project") or "")
    file_name = str(event_payload.get("file") or "")
    topic_path = str(event_payload.get("topic_path") or "")
    topic_tags = event_payload.get("topic_tags") or []
    coalesce_cutoff = _utc_iso_from_unix(time.time() - max(0.0, FANOUT_COALESCE_WINDOW_SECS))

    def _enqueue() -> dict[str, Any]:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        inserted = 0
        requeued = 0
        existing = 0
        coalesced = 0
        coalesced_by_target: dict[str, int] = {}
        for target in targets:
            if (
                not force_requeue
                and project
                and file_name
                and _fanout_coalescer_active_for_target(target)
            ):
                candidate = coll.find_one(
                    {
                        "target": target,
                        "project": project,
                        "file": file_name,
                        "status": {"$in": ["pending", "retrying"]},
                        "updated_at": {"$gte": coalesce_cutoff},
                    },
                    {"_id": 1},
                    sort=[("updated_at", -1), ("_id", -1)],
                )
                if candidate:
                    update = {
                        "$set": {
                            "payload": event_payload,
                            "summary": summary,
                            "topic_path": topic_path,
                            "topic_tags": topic_tags,
                            "next_attempt_at": now,
                            "updated_at": now,
                        }
                    }
                    updated = coll.update_one(
                        {
                            "_id": candidate["_id"],
                            "status": {"$in": ["pending", "retrying"]},
                        },
                        update,
                    )
                    if int(updated.modified_count or 0) > 0:
                        coalesced += 1
                        coalesced_by_target[target] = int(coalesced_by_target.get(target, 0) or 0) + 1
                        continue
            dedupe_key = f"{event_id}:{target}"
            row = coll.find_one({"dedupe_key": dedupe_key}, {"status": 1})
            if row:
                existing += 1
                if force_requeue:
                    update = {
                        "$set": {
                            "status": "pending",
                            "attempts": 0,
                            "next_attempt_at": now,
                            "updated_at": now,
                            "last_error": None,
                            "completed_at": None,
                            "payload": event_payload,
                            "summary": summary,
                            "topic_path": topic_path,
                            "topic_tags": topic_tags,
                            "max_attempts": FANOUT_MAX_ATTEMPTS,
                        }
                    }
                    coll.update_one({"_id": row["_id"]}, update)
                    requeued += 1
                continue
            doc = {
                "_id": uuid.uuid4().hex,
                "event_id": event_id,
                "target": target,
                "project": project,
                "file": file_name,
                "summary": summary,
                "payload": event_payload,
                "topic_path": topic_path,
                "topic_tags": topic_tags,
                "status": "pending",
                "attempts": 0,
                "max_attempts": FANOUT_MAX_ATTEMPTS,
                "next_attempt_at": now,
                "last_attempt_at": None,
                "completed_at": None,
                "last_error": None,
                "created_at": now,
                "updated_at": now,
                "dedupe_key": dedupe_key,
            }
            try:
                coll.insert_one(doc)
                inserted += 1
            except Exception:
                # Duplicate races are expected under concurrent writers.
                existing += 1
        return {
            "inserted": inserted,
            "requeued": requeued,
            "existing": existing,
            "coalesced": coalesced,
            "coalesced_by_target": coalesced_by_target,
        }

    return await asyncio.to_thread(_enqueue)


async def _claim_fanout_batch_mongo(
    limit: int = FANOUT_BATCH_SIZE,
    target: str | None = None,
    exclude_target: str | None = None,
) -> list[dict[str, Any]]:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None
    now = _utc_now()

    def _claim() -> list[dict[str, Any]]:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        rows: list[dict[str, Any]] = []
        for _ in range(limit):
            query: dict[str, Any] = {
                "status": {"$in": ["pending", "retrying"]},
                "next_attempt_at": {"$lte": now},
            }
            if target:
                query["target"] = target
            elif exclude_target:
                query["target"] = {"$ne": exclude_target}
            row = coll.find_one_and_update(
                query,
                {
                    "$set": {
                        "status": "running",
                        "last_attempt_at": now,
                        "updated_at": now,
                    },
                    "$inc": {"attempts": 1},
                },
                sort=[("next_attempt_at", 1), ("_id", 1)],
                return_document=ReturnDocument.AFTER,
            )
            if not row:
                break
            rows.append(_fanout_doc_to_dict(row))
        return rows

    return await asyncio.to_thread(_claim)


async def _recover_stale_running_jobs_mongo(max_age_secs: int = FANOUT_RUNNING_STALE_SECS) -> int:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None
    now = _utc_now()
    cutoff = datetime.utcfromtimestamp(time.time() - max(0, max_age_secs)).isoformat() + "Z"

    def _recover() -> int:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        result = coll.update_many(
            {
                "status": "running",
                "$or": [
                    {"last_attempt_at": None},
                    {"last_attempt_at": {"$lte": cutoff}},
                ],
            },
            {
                "$set": {
                    "status": "retrying",
                    "next_attempt_at": now,
                    "updated_at": now,
                }
            },
        )
        # Ensure useful error context on recovered rows.
        coll.update_many(
            {
                "status": "retrying",
                "next_attempt_at": now,
                "$or": [{"last_error": None}, {"last_error": ""}],
            },
            {"$set": {"last_error": "Recovered stale running fanout job after worker restart"}},
        )
        return int(result.modified_count or 0)

    return await asyncio.to_thread(_recover)


async def _update_fanout_job_mongo(job_id: str, update: dict[str, Any]) -> None:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None

    def _update() -> None:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        coll.update_one({"_id": job_id}, {"$set": update})

    await asyncio.to_thread(_update)


async def _mark_fanout_success_mongo(job_id: str) -> None:
    now = _utc_now()
    await _update_fanout_job_mongo(
        job_id,
        {
            "status": "succeeded",
            "completed_at": now,
            "updated_at": now,
            "last_error": None,
        },
    )


async def _mark_fanout_failed_mongo(job_id: str, error: str) -> None:
    now = _utc_now()
    await _update_fanout_job_mongo(
        job_id,
        {
            "status": "failed",
            "next_attempt_at": now,
            "completed_at": now,
            "updated_at": now,
            "last_error": error[:2000],
        },
    )


async def _fail_letta_backlog_mongo(error: str) -> int:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None
    now = _utc_now()

    def _mark() -> int:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        result = coll.update_many(
            {
                "target": FANOUT_TARGET_LETTA,
                "status": {"$in": ["pending", "retrying", "running"]},
            },
            {
                "$set": {
                    "status": "failed",
                    "next_attempt_at": now,
                    "completed_at": now,
                    "updated_at": now,
                    "last_error": error[:2000],
                }
            },
        )
        return int(result.modified_count or 0)

    return await asyncio.to_thread(_mark)


async def _mark_fanout_retry_mongo(job: dict[str, Any], error: str) -> str:
    now = _utc_now()
    attempts = int(job.get("attempts") or 0)
    max_attempts = int(job.get("max_attempts") or FANOUT_MAX_ATTEMPTS)
    next_status = "retrying"
    next_attempt = now
    if attempts < max_attempts:
        delay = _backoff_seconds(attempts)
        next_attempt = datetime.utcfromtimestamp(time.time() + delay).isoformat() + "Z"
    else:
        next_status = "failed"
    update = {
        "status": next_status,
        "next_attempt_at": next_attempt,
        "updated_at": now,
        "last_error": error[:2000],
    }
    if next_status == "failed":
        update["completed_at"] = now
    await _update_fanout_job_mongo(str(job["id"]), update)
    return next_status


async def _get_fanout_summary_mongo() -> dict[str, Any]:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None

    def _summary() -> dict[str, Any]:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        by_status: dict[str, int] = {}
        for row in coll.aggregate([{"$group": {"_id": "$status", "c": {"$sum": 1}}}]):
            by_status[str(row.get("_id") or "")] = int(row.get("c") or 0)
        by_target: dict[str, dict[str, int]] = {}
        for row in coll.aggregate(
            [
                {"$group": {"_id": {"target": "$target", "status": "$status"}, "c": {"$sum": 1}}},
            ]
        ):
            ident = row.get("_id") or {}
            target = str(ident.get("target") or "")
            status = str(ident.get("status") or "")
            by_target.setdefault(target, {})[status] = int(row.get("c") or 0)
        return {"by_status": by_status, "by_target": by_target}

    return await asyncio.to_thread(_summary)


async def _list_fanout_jobs_mongo(
    statuses: list[str],
    limit: int = 100,
    target: str | None = None,
) -> list[dict[str, Any]]:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None

    def _list() -> list[dict[str, Any]]:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        query: dict[str, Any] = {"status": {"$in": statuses}}
        if target:
            query["target"] = target
        docs = list(coll.find(query).sort([("updated_at", -1), ("_id", -1)]).limit(limit))
        return [_fanout_doc_to_dict(doc) for doc in docs]

    return await asyncio.to_thread(_list)


def _fanout_row_to_dict(row: sqlite3.Row) -> dict[str, Any]:
    return {
        "id": row["id"],
        "event_id": row["event_id"],
        "target": row["target"],
        "project": row["project"],
        "file": row["file"],
        "summary": row["summary"],
        "payload": json.loads(row["payload"]) if row["payload"] else {},
        "topic_path": row["topic_path"],
        "topic_tags": json.loads(row["topic_tags"]) if row["topic_tags"] else [],
        "status": row["status"],
        "attempts": row["attempts"],
        "max_attempts": row["max_attempts"],
        "next_attempt_at": row["next_attempt_at"],
        "last_attempt_at": row["last_attempt_at"],
        "completed_at": row["completed_at"],
        "last_error": row["last_error"],
        "created_at": row["created_at"],
        "updated_at": row["updated_at"],
    }


async def enqueue_fanout_outbox(
    event_payload: dict[str, Any],
    targets: list[str],
    force_requeue: bool = False,
) -> dict[str, Any]:
    event_id = str(event_payload.get("event_id") or uuid.uuid4().hex)
    created_at = _utc_now()
    coalesce_cutoff = _utc_iso_from_unix(time.time() - max(0.0, FANOUT_COALESCE_WINDOW_SECS))
    payload_json = json.dumps(event_payload)
    topic_tags_json = json.dumps(event_payload.get("topic_tags") or [])
    summary = str(event_payload.get("summary") or "")
    project = str(event_payload.get("project") or "")
    file_name = str(event_payload.get("file") or "")
    topic_path = str(event_payload.get("topic_path") or "")
    targets = [target for target in targets if target in FANOUT_TARGETS]
    if _use_mongo_outbox():
        try:
            result = await _enqueue_fanout_outbox_mongo(
                event_payload,
                targets,
                force_requeue=force_requeue,
            )
            _record_fanout_coalesce_result(result)
            return result
        except Exception as exc:
            _demote_outbox_backend(str(exc))

    def _enqueue(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        inserted = 0
        requeued = 0
        existing = 0
        coalesced = 0
        coalesced_by_target: dict[str, int] = {}
        for target in targets:
            if (
                not force_requeue
                and project
                and file_name
                and _fanout_coalescer_active_for_target(target)
            ):
                row = conn.execute(
                    """
                    SELECT id FROM fanout_outbox
                    WHERE target = ? AND project = ? AND file = ?
                      AND status IN ('pending', 'retrying')
                      AND updated_at >= ?
                    ORDER BY updated_at DESC, id DESC
                    LIMIT 1
                    """,
                    (target, project, file_name, coalesce_cutoff),
                ).fetchone()
                if row:
                    updated = conn.execute(
                        """
                        UPDATE fanout_outbox
                        SET payload = ?, summary = ?, topic_path = ?, topic_tags = ?,
                            next_attempt_at = ?, updated_at = ?
                        WHERE id = ? AND status IN ('pending', 'retrying')
                        """,
                        (
                            payload_json,
                            summary,
                            topic_path,
                            topic_tags_json,
                            created_at,
                            created_at,
                            row["id"],
                        ),
                    )
                    if int(updated.rowcount or 0) > 0:
                        coalesced += 1
                        coalesced_by_target[target] = int(coalesced_by_target.get(target, 0) or 0) + 1
                        continue
            dedupe_key = f"{event_id}:{target}"
            row = conn.execute(
                "SELECT id, status FROM fanout_outbox WHERE dedupe_key = ?",
                (dedupe_key,),
            ).fetchone()
            if row:
                existing += 1
                if force_requeue:
                    conn.execute(
                        """
                        UPDATE fanout_outbox
                        SET status = ?, attempts = 0, next_attempt_at = ?, updated_at = ?,
                            last_error = NULL, completed_at = NULL, payload = ?, summary = ?,
                            topic_path = ?, topic_tags = ?, max_attempts = ?
                        WHERE id = ?
                        """,
                        (
                            "pending",
                            created_at,
                            created_at,
                            payload_json,
                            summary,
                            topic_path,
                            topic_tags_json,
                            FANOUT_MAX_ATTEMPTS,
                            row["id"],
                        ),
                    )
                    requeued += 1
                continue
            conn.execute(
                """
                INSERT INTO fanout_outbox (
                    event_id, target, project, file, summary, payload, topic_path, topic_tags,
                    status, attempts, max_attempts, next_attempt_at, created_at, updated_at, dedupe_key
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)
                """,
                (
                    event_id,
                    target,
                    project,
                    file_name,
                    summary,
                    payload_json,
                    topic_path,
                    topic_tags_json,
                    "pending",
                    FANOUT_MAX_ATTEMPTS,
                    created_at,
                    created_at,
                    created_at,
                    dedupe_key,
                ),
            )
            inserted += 1
        conn.commit()
        return {
            "inserted": inserted,
            "requeued": requeued,
            "existing": existing,
            "coalesced": coalesced,
            "coalesced_by_target": coalesced_by_target,
        }

    try:
        result = await _task_db_exec(_enqueue)
        _record_fanout_coalesce_result(result)
        return result
    except Exception as exc:
        if await _promote_outbox_backend_to_mongo_if_sqlite_error(str(exc)):
            result = await _enqueue_fanout_outbox_mongo(
                event_payload,
                targets,
                force_requeue=force_requeue,
            )
            _record_fanout_coalesce_result(result)
            return result
        raise


async def claim_fanout_batch(
    limit: int = FANOUT_BATCH_SIZE,
    target: str | None = None,
    exclude_target: str | None = None,
) -> list[dict[str, Any]]:
    now = _utc_now()
    limit = max(1, min(limit, 256))
    if _use_mongo_outbox():
        try:
            return await _claim_fanout_batch_mongo(
                limit=limit,
                target=target,
                exclude_target=exclude_target,
            )
        except Exception as exc:
            _demote_outbox_backend(str(exc))

    def _claim(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        query = """
            SELECT * FROM fanout_outbox
            WHERE status IN ('pending', 'retrying') AND next_attempt_at <= ?
        """
        params: list[Any] = [now]
        if target:
            query += " AND target = ?"
            params.append(target)
        elif exclude_target:
            query += " AND target != ?"
            params.append(exclude_target)
        query += " ORDER BY next_attempt_at ASC, id ASC LIMIT ?"
        params.append(limit)
        rows = conn.execute(query, params).fetchall()
        claimed: list[dict[str, Any]] = []
        for row in rows:
            attempts = int(row["attempts"]) + 1
            conn.execute(
                """
                UPDATE fanout_outbox
                SET status = ?, attempts = ?, last_attempt_at = ?, updated_at = ?
                WHERE id = ?
                """,
                ("running", attempts, now, now, row["id"]),
            )
            updated = conn.execute(
                "SELECT * FROM fanout_outbox WHERE id = ?",
                (row["id"],),
            ).fetchone()
            if updated:
                claimed.append(_fanout_row_to_dict(updated))
        conn.commit()
        return claimed

    try:
        return await _task_db_exec(_claim)
    except Exception as exc:
        if await _promote_outbox_backend_to_mongo_if_sqlite_error(str(exc)):
            return await _claim_fanout_batch_mongo(
                limit=limit,
                target=target,
                exclude_target=exclude_target,
            )
        raise


async def recover_stale_running_jobs(max_age_secs: int = FANOUT_RUNNING_STALE_SECS) -> int:
    if _use_mongo_outbox():
        try:
            return await _recover_stale_running_jobs_mongo(max_age_secs=max_age_secs)
        except Exception as exc:
            _demote_outbox_backend(str(exc))
    now = _utc_now()
    cutoff = datetime.utcfromtimestamp(time.time() - max(0, max_age_secs)).isoformat() + "Z"

    def _recover(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        cursor = conn.execute(
            """
            UPDATE fanout_outbox
            SET status = 'retrying',
                next_attempt_at = ?,
                updated_at = ?,
                last_error = CASE
                    WHEN last_error IS NULL OR last_error = '' THEN ?
                    ELSE last_error
                END
            WHERE status = 'running'
              AND (last_attempt_at IS NULL OR last_attempt_at <= ?)
            """,
            (now, now, "Recovered stale running fanout job after worker restart", cutoff),
        )
        changed = cursor.rowcount or 0
        conn.commit()
        return changed

    return await _task_db_exec(_recover)


async def mark_fanout_success(job_id: int | str) -> None:
    if _use_mongo_outbox():
        await _mark_fanout_success_mongo(str(job_id))
        return
    now = _utc_now()

    def _mark(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """
            UPDATE fanout_outbox
            SET status = ?, completed_at = ?, updated_at = ?, last_error = NULL
            WHERE id = ?
            """,
            ("succeeded", now, now, job_id),
        )
        conn.commit()

    await _task_db_exec(_mark)


async def mark_fanout_failed(job_id: int | str, error: str) -> None:
    if _use_mongo_outbox():
        await _mark_fanout_failed_mongo(str(job_id), error)
        return
    now = _utc_now()

    def _mark(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """
            UPDATE fanout_outbox
            SET status = ?, next_attempt_at = ?, completed_at = ?, updated_at = ?, last_error = ?
            WHERE id = ?
            """,
            ("failed", now, now, now, error[:2000], job_id),
        )
        conn.commit()

    await _task_db_exec(_mark)


async def fail_letta_backlog(error: str) -> int:
    if _use_mongo_outbox():
        return await _fail_letta_backlog_mongo(error)
    now = _utc_now()

    def _mark(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        cursor = conn.execute(
            """
            UPDATE fanout_outbox
            SET status = ?, next_attempt_at = ?, completed_at = ?, updated_at = ?, last_error = ?
            WHERE target = ? AND status IN ('pending', 'retrying', 'running')
            """,
            ("failed", now, now, now, error[:2000], FANOUT_TARGET_LETTA),
        )
        changed = cursor.rowcount or 0
        conn.commit()
        return changed

    return await _task_db_exec(_mark)


async def mark_fanout_retry(job: dict[str, Any], error: str) -> str:
    if _use_mongo_outbox():
        return await _mark_fanout_retry_mongo(job, error)
    now = _utc_now()
    attempts = int(job.get("attempts") or 0)
    max_attempts = int(job.get("max_attempts") or FANOUT_MAX_ATTEMPTS)
    next_status = "retrying"
    next_attempt = now
    if attempts < max_attempts:
        delay = _backoff_seconds(attempts)
        next_attempt = datetime.utcfromtimestamp(time.time() + delay).isoformat() + "Z"
    else:
        next_status = "failed"

    def _mark(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """
            UPDATE fanout_outbox
            SET status = ?, next_attempt_at = ?, updated_at = ?, last_error = ?,
                completed_at = CASE WHEN ? = 'failed' THEN ? ELSE completed_at END
            WHERE id = ?
            """,
            (next_status, next_attempt, now, error[:2000], next_status, now, job["id"]),
        )
        conn.commit()

    await _task_db_exec(_mark)
    return next_status


def _outbox_gc_cutoff_iso(hours: int) -> str:
    cutoff = datetime.now(timezone.utc) - timedelta(hours=max(0, int(hours)))
    return cutoff.isoformat().replace("+00:00", "Z")


def _should_run_outbox_gc_vacuum() -> bool:
    if not FANOUT_OUTBOX_GC_VACUUM:
        return False
    interval = max(0.0, FANOUT_OUTBOX_GC_VACUUM_MIN_INTERVAL_SECS)
    if interval <= 0:
        return True
    elapsed = max(0.0, time.monotonic() - float(outbox_gc_last_vacuum_monotonic))
    return elapsed >= interval


def _fanout_outbox_gc_sqlite(
    conn: sqlite3.Connection,
    *,
    succeeded_retention_hours: int,
    failed_retention_hours: int,
    stale_pending_hours: int,
    stale_targets: list[str],
    run_vacuum: bool,
    vacuum_min_deleted: int,
) -> dict[str, Any]:
    before_total = int(conn.execute("SELECT COUNT(*) FROM fanout_outbox").fetchone()[0])
    succeeded_cutoff = _outbox_gc_cutoff_iso(succeeded_retention_hours)
    failed_cutoff = _outbox_gc_cutoff_iso(failed_retention_hours)
    stale_cutoff = _outbox_gc_cutoff_iso(stale_pending_hours)
    conn.execute("BEGIN IMMEDIATE")
    succeeded_deleted = int(
        (
            conn.execute(
                "DELETE FROM fanout_outbox WHERE status = 'succeeded' AND COALESCE(completed_at, updated_at, created_at) < ?",
                (succeeded_cutoff,),
            ).rowcount
        )
        or 0
    )
    failed_deleted = int(
        (
            conn.execute(
                "DELETE FROM fanout_outbox WHERE status = 'failed' AND COALESCE(completed_at, updated_at, created_at) < ?",
                (failed_cutoff,),
            ).rowcount
        )
        or 0
    )
    stale_deleted = 0
    if stale_targets:
        status_values = ("pending", "retrying", "running")
        target_placeholders = ",".join("?" for _ in stale_targets)
        status_placeholders = ",".join("?" for _ in status_values)
        stale_query = (
            "DELETE FROM fanout_outbox WHERE "
            f"target IN ({target_placeholders}) "
            f"AND status IN ({status_placeholders}) "
            "AND COALESCE(last_attempt_at, updated_at, created_at) < ?"
        )
        stale_params = tuple(stale_targets) + status_values + (stale_cutoff,)
        stale_deleted = int((conn.execute(stale_query, stale_params).rowcount or 0))
    deleted_total = succeeded_deleted + failed_deleted + stale_deleted
    conn.commit()

    vacuum_ran = False
    vacuum_error = ""
    if run_vacuum and deleted_total >= max(0, int(vacuum_min_deleted)):
        try:
            conn.execute("VACUUM")
            vacuum_ran = True
        except Exception as exc:
            vacuum_error = str(exc)

    after_total = int(conn.execute("SELECT COUNT(*) FROM fanout_outbox").fetchone()[0])
    return {
        "backend": "sqlite",
        "before_total": before_total,
        "after_total": after_total,
        "deleted_total": deleted_total,
        "deleted": {
            "succeeded": succeeded_deleted,
            "failed": failed_deleted,
            "stale_pending_targets": stale_deleted,
        },
        "retention_hours": {
            "succeeded": int(succeeded_retention_hours),
            "failed": int(failed_retention_hours),
            "stale_pending": int(stale_pending_hours),
        },
        "stale_targets": stale_targets,
        "vacuum": {
            "requested": bool(run_vacuum),
            "ran": vacuum_ran,
            "min_deleted": int(vacuum_min_deleted),
            "error": vacuum_error,
        },
    }


async def _fanout_outbox_gc_mongo(
    *,
    succeeded_retention_hours: int,
    failed_retention_hours: int,
    stale_pending_hours: int,
    stale_targets: list[str],
) -> dict[str, Any]:
    if not await init_fanout_outbox_mongo_client():
        raise OrchestratorError("mongo outbox unavailable")
    assert FANOUT_OUTBOX_MONGO_CLIENT is not None
    succeeded_cutoff = _outbox_gc_cutoff_iso(succeeded_retention_hours)
    failed_cutoff = _outbox_gc_cutoff_iso(failed_retention_hours)
    stale_cutoff = _outbox_gc_cutoff_iso(stale_pending_hours)

    def _gc() -> dict[str, Any]:
        assert FANOUT_OUTBOX_MONGO_CLIENT is not None
        coll = FANOUT_OUTBOX_MONGO_CLIENT[FANOUT_OUTBOX_MONGO_DB][FANOUT_OUTBOX_MONGO_COLLECTION]
        before_total = int(coll.count_documents({}))
        succeeded_deleted = int(
            coll.delete_many(
                {
                    "status": "succeeded",
                    "updated_at": {"$lt": succeeded_cutoff},
                }
            ).deleted_count
        )
        failed_deleted = int(
            coll.delete_many(
                {
                    "status": "failed",
                    "updated_at": {"$lt": failed_cutoff},
                }
            ).deleted_count
        )
        stale_deleted = 0
        if stale_targets:
            stale_deleted = int(
                coll.delete_many(
                    {
                        "target": {"$in": stale_targets},
                        "status": {"$in": ["pending", "retrying", "running"]},
                        "updated_at": {"$lt": stale_cutoff},
                    }
                ).deleted_count
            )
        after_total = int(coll.count_documents({}))
        deleted_total = succeeded_deleted + failed_deleted + stale_deleted
        return {
            "backend": "mongo",
            "before_total": before_total,
            "after_total": after_total,
            "deleted_total": deleted_total,
            "deleted": {
                "succeeded": succeeded_deleted,
                "failed": failed_deleted,
                "stale_pending_targets": stale_deleted,
            },
            "retention_hours": {
                "succeeded": int(succeeded_retention_hours),
                "failed": int(failed_retention_hours),
                "stale_pending": int(stale_pending_hours),
            },
            "stale_targets": stale_targets,
            "vacuum": {
                "requested": False,
                "ran": False,
                "min_deleted": 0,
                "error": "",
            },
        }

    return await asyncio.to_thread(_gc)


def _record_outbox_gc_result(
    *,
    result: dict[str, Any] | None,
    error: str | None,
    duration_ms: float,
) -> None:
    gc_state = outbox_health.setdefault("gc", {})
    gc_state["lastRunAt"] = _utc_now()
    gc_state["lastDurationMs"] = round(duration_ms, 2)
    gc_state["runs"] = int(gc_state.get("runs", 0) or 0) + 1
    if error:
        gc_state["lastError"] = error[:400]
        return
    gc_state["lastError"] = None
    gc_state["lastDeleted"] = int((result or {}).get("deleted_total") or 0)
    vacuum = (result or {}).get("vacuum")
    if isinstance(vacuum, dict) and vacuum.get("ran"):
        gc_state["vacuumedAt"] = gc_state["lastRunAt"]


async def run_fanout_outbox_gc_once() -> dict[str, Any]:
    global outbox_gc_last_vacuum_monotonic
    stale_targets = list(FANOUT_OUTBOX_STALE_TARGETS)
    run_started = time.monotonic()
    run_vacuum = _should_run_outbox_gc_vacuum()
    try:
        if _use_mongo_outbox():
            try:
                result = await asyncio.wait_for(
                    _fanout_outbox_gc_mongo(
                        succeeded_retention_hours=FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS,
                        failed_retention_hours=FANOUT_OUTBOX_FAILED_RETENTION_HOURS,
                        stale_pending_hours=FANOUT_OUTBOX_STALE_PENDING_HOURS,
                        stale_targets=stale_targets,
                    ),
                    timeout=max(1.0, FANOUT_OUTBOX_GC_TIMEOUT_SECS),
                )
                _record_outbox_gc_result(
                    result=result,
                    error=None,
                    duration_ms=(time.monotonic() - run_started) * 1000,
                )
                return result
            except Exception as exc:
                err_text = str(exc).strip() or exc.__class__.__name__
                _demote_outbox_backend(err_text)
                logger.warning("Mongo outbox GC failed; retrying with sqlite: %s", err_text)

        def _gc(conn: sqlite3.Connection) -> dict[str, Any]:
            return _fanout_outbox_gc_sqlite(
                conn,
                succeeded_retention_hours=FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS,
                failed_retention_hours=FANOUT_OUTBOX_FAILED_RETENTION_HOURS,
                stale_pending_hours=FANOUT_OUTBOX_STALE_PENDING_HOURS,
                stale_targets=stale_targets,
                run_vacuum=run_vacuum,
                vacuum_min_deleted=FANOUT_OUTBOX_GC_VACUUM_MIN_DELETED,
            )

        result = await asyncio.wait_for(
            _task_db_exec(_gc),
            timeout=max(1.0, FANOUT_OUTBOX_GC_TIMEOUT_SECS),
        )
        vacuum_info = result.get("vacuum")
        if isinstance(vacuum_info, dict) and vacuum_info.get("ran"):
            outbox_gc_last_vacuum_monotonic = time.monotonic()
        _record_outbox_gc_result(
            result=result,
            error=None,
            duration_ms=(time.monotonic() - run_started) * 1000,
        )
        return result
    except Exception as exc:
        err_text = str(exc).strip() or exc.__class__.__name__
        _record_outbox_gc_result(
            result=None,
            error=err_text,
            duration_ms=(time.monotonic() - run_started) * 1000,
        )
        raise


async def _fanout_outbox_gc_worker() -> None:
    interval = max(30.0, FANOUT_OUTBOX_GC_INTERVAL_SECS)
    while True:
        try:
            result = await run_fanout_outbox_gc_once()
            logger.info(
                "fanout outbox gc: backend=%s deleted=%s before=%s after=%s",
                result.get("backend"),
                result.get("deleted_total"),
                result.get("before_total"),
                result.get("after_total"),
            )
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # pragma: no cover - runtime resilience
            err_text = str(exc).strip() or exc.__class__.__name__
            await _promote_outbox_backend_to_mongo_if_sqlite_error(err_text)
            logger.warning("Fanout outbox GC run failed: %s", err_text)
        await asyncio.sleep(interval)


def _chunk_values(values: list[Any], chunk_size: int) -> list[list[Any]]:
    if not values:
        return []
    size = max(1, int(chunk_size))
    return [values[idx : idx + size] for idx in range(0, len(values), size)]


def _retention_cutoff_datetime(hours: float) -> datetime:
    span_hours = max(0.0, float(hours))
    return datetime.now(timezone.utc) - timedelta(hours=span_hours)


def _retention_cutoff_iso(hours: float) -> str:
    return _retention_cutoff_datetime(hours).isoformat().replace("+00:00", "Z")


async def _run_qdrant_low_value_retention_once() -> dict[str, Any]:
    hours = max(0.0, QDRANT_LOW_VALUE_RETENTION_HOURS)
    if hours <= 0:
        return {"enabled": False, "reason": "QDRANT_LOW_VALUE_RETENTION_HOURS<=0", "scanned": 0, "deleted": 0}
    cutoff_epoch = int(time.time() - (hours * 3600.0))
    max_scan = max(100, SINK_RETENTION_SCAN_LIMIT)
    max_deletes = max(1, SINK_RETENTION_MAX_DELETES_PER_RUN)
    delete_batch = max(1, SINK_RETENTION_DELETE_BATCH)
    scanned = 0
    candidates: list[Any] = []
    offset: Any = None
    while scanned < max_scan and len(candidates) < max_deletes:
        page_limit = min(256, max_scan - scanned)
        try:
            points, next_offset = await _qdrant_call(
                "retention_scroll",
                lambda client, _: client.scroll(
                    collection_name=QDRANT_COLLECTION,
                    limit=page_limit,
                    offset=offset,
                    with_payload=True,
                    with_vectors=False,
                ),
            )
        except Exception as exc:
            raise OrchestratorError(f"Qdrant retention scroll failed: {exc}") from exc
        if not points:
            break
        for point in points:
            scanned += 1
            payload_row = getattr(point, "payload", None) or {}
            ts_raw = payload_row.get("ts")
            try:
                ts_value = int(ts_raw)
            except (TypeError, ValueError):
                continue
            if ts_value > cutoff_epoch:
                continue
            if not _is_low_value_memory_record(
                str(payload_row.get("file") or ""),
                str(payload_row.get("topic_path") or ""),
                str(payload_row.get("summary") or ""),
                include_short_summary=False,
            ):
                continue
            point_id = getattr(point, "id", None)
            if point_id is None:
                continue
            candidates.append(point_id)
            if len(candidates) >= max_deletes:
                break
        offset = next_offset
        if offset is None:
            break
    deleted = 0
    for id_batch in _chunk_values(candidates, delete_batch):
        if qdrant_models is None:
            raise OrchestratorError("qdrant-client dependency is required for Qdrant retention")
        try:
            await _qdrant_call(
                "retention_delete",
                lambda client, _: client.delete(
                    collection_name=QDRANT_COLLECTION,
                    points_selector=qdrant_models.PointIdsList(points=id_batch),
                    wait=True,
                ),
            )
        except Exception as exc:
            raise OrchestratorError(f"Qdrant retention delete failed: {exc}") from exc
        deleted += len(id_batch)
    return {
        "enabled": True,
        "cutoffEpoch": cutoff_epoch,
        "scanned": scanned,
        "deleteCandidates": len(candidates),
        "deleted": deleted,
    }


async def _run_mongo_low_value_retention_once() -> dict[str, Any]:
    hours = max(0.0, MONGO_RAW_LOW_VALUE_RETENTION_HOURS)
    if hours <= 0:
        return {"enabled": False, "reason": "MONGO_RAW_LOW_VALUE_RETENTION_HOURS<=0", "scanned": 0, "deleted": 0}
    if not await init_mongo_client():
        raise OrchestratorError("mongo raw store unavailable")
    assert MONGO_CLIENT is not None
    cutoff_iso = _retention_cutoff_iso(hours)
    max_scan = max(100, SINK_RETENTION_SCAN_LIMIT)
    max_deletes = max(1, SINK_RETENTION_MAX_DELETES_PER_RUN)

    def _cleanup() -> dict[str, Any]:
        assert MONGO_CLIENT is not None
        coll = MONGO_CLIENT[MONGO_RAW_DB][MONGO_RAW_COLLECTION]
        docs = list(
            coll.find(
                {"updated_at": {"$lt": cutoff_iso}},
                projection={
                    "_id": 1,
                    "file": 1,
                    "topic_path": 1,
                    "summary": 1,
                    "source": 1,
                },
            )
            .sort("updated_at", 1)
            .limit(max_scan)
        )
        delete_ids: list[Any] = []
        for doc in docs:
            if len(delete_ids) >= max_deletes:
                break
            if not _is_low_value_memory_record(
                str(doc.get("file") or ""),
                str(doc.get("topic_path") or ""),
                str(doc.get("summary") or ""),
                source_kind=str(doc.get("source") or ""),
                include_short_summary=False,
            ):
                continue
            delete_ids.append(doc.get("_id"))
        deleted = 0
        if delete_ids:
            result = coll.delete_many({"_id": {"$in": delete_ids}})
            deleted = int(result.deleted_count or 0)
        return {
            "enabled": True,
            "cutoffIso": cutoff_iso,
            "scanned": len(docs),
            "deleteCandidates": len(delete_ids),
            "deleted": deleted,
        }

    return await asyncio.to_thread(_cleanup)


async def _run_letta_low_value_retention_once() -> dict[str, Any]:
    hours = max(0.0, LETTA_LOW_VALUE_RETENTION_HOURS)
    if hours <= 0:
        return {"enabled": False, "reason": "LETTA_LOW_VALUE_RETENTION_HOURS<=0", "scanned": 0, "deleted": 0}
    if not _letta_config_enabled():
        return {"enabled": False, "reason": "letta not configured", "scanned": 0, "deleted": 0}
    headers: dict[str, str] = {}
    if LETTA_API_KEY:
        headers["Authorization"] = f"Bearer {LETTA_API_KEY}"
    cutoff_dt = _retention_cutoff_datetime(hours)
    scan_limit = max(100, SINK_RETENTION_SCAN_LIMIT)
    page_limit = max(10, min(LETTA_RETENTION_PAGE_LIMIT, 200))
    delete_cap = max(1, min(LETTA_RETENTION_MAX_DELETES_PER_RUN, SINK_RETENTION_MAX_DELETES_PER_RUN))
    agent_id = await _resolve_letta_agent_id(LETTA_AUTO_SESSION_ID, headers)
    client = await _get_letta_client()
    scanned = 0
    deleted = 0
    delete_candidates = 0
    after: str | None = None
    stop_on_recent = False
    while scanned < scan_limit and deleted < delete_cap and not stop_on_recent:
        params: dict[str, Any] = {"limit": page_limit, "ascending": True}
        if after:
            params["after"] = after
        resp = await client.get(
            f"{LETTA_URL}/v1/agents/{agent_id}/archival-memory",
            params=params,
            headers=headers,
            timeout=LETTA_REQUEST_TIMEOUT_SECS,
        )
        if resp.status_code >= 400:
            raise OrchestratorError(
                f"Letta retention list failed: status={resp.status_code} body={resp.text[:240]}"
            )
        rows = resp.json() if resp.content else []
        if not isinstance(rows, list) or not rows:
            break
        for row in rows:
            if not isinstance(row, dict):
                continue
            scanned += 1
            after = str(row.get("id") or "") or after
            row_created = _parse_timestamp_to_datetime(row.get("created_at") or row.get("updated_at"))
            if row_created and row_created > cutoff_dt:
                stop_on_recent = True
                break
            text = str(row.get("text") or "")
            parsed = _parse_letta_archival_content(text)
            if not _is_low_value_memory_record(
                parsed.get("file"),
                parsed.get("topic_path"),
                parsed.get("summary"),
                include_short_summary=False,
            ):
                continue
            memory_id = str(row.get("id") or "").strip()
            if not memory_id:
                continue
            delete_candidates += 1
            delete_resp = await client.delete(
                f"{LETTA_URL}/v1/agents/{agent_id}/archival-memory/{memory_id}",
                headers=headers,
                timeout=LETTA_REQUEST_TIMEOUT_SECS,
            )
            if delete_resp.status_code in (200, 202, 204, 404):
                deleted += 1
            elif delete_resp.status_code >= 400:
                raise OrchestratorError(
                    f"Letta retention delete failed: status={delete_resp.status_code} body={delete_resp.text[:240]}"
                )
            if deleted >= delete_cap:
                break
        if len(rows) < page_limit:
            break
    return {
        "enabled": True,
        "cutoffIso": cutoff_dt.isoformat().replace("+00:00", "Z"),
        "scanned": scanned,
        "deleteCandidates": delete_candidates,
        "deleted": deleted,
    }


def _record_sink_retention_run(
    *,
    result: dict[str, Any] | None,
    error: str | None,
    duration_ms: float,
) -> None:
    sink_retention_state["lastRunAt"] = _utc_now()
    sink_retention_state["lastDurationMs"] = round(duration_ms, 2)
    sink_retention_state["runs"] = int(sink_retention_state.get("runs", 0) or 0) + 1
    sink_retention_state["lastResult"] = result or {}
    sink_retention_state["lastError"] = (error or "")[:400] if error else None


async def run_sink_retention_once() -> dict[str, Any]:
    started = time.monotonic()
    sinks: dict[str, Any] = {}
    errors: dict[str, str] = {}
    try:
        try:
            sinks["qdrant"] = await asyncio.wait_for(
                _run_qdrant_low_value_retention_once(),
                timeout=max(5.0, SINK_RETENTION_TIMEOUT_SECS),
            )
        except Exception as exc:
            errors["qdrant"] = str(exc)
        try:
            sinks["mongo_raw"] = await asyncio.wait_for(
                _run_mongo_low_value_retention_once(),
                timeout=max(5.0, SINK_RETENTION_TIMEOUT_SECS),
            )
        except Exception as exc:
            errors["mongo_raw"] = str(exc)
        try:
            sinks["letta"] = await asyncio.wait_for(
                _run_letta_low_value_retention_once(),
                timeout=max(5.0, SINK_RETENTION_TIMEOUT_SECS),
            )
        except Exception as exc:
            errors["letta"] = str(exc)
        result = {
            "sinks": sinks,
            "errors": errors,
            "ok": not bool(errors),
        }
        _record_sink_retention_run(
            result=result,
            error=None if not errors else json.dumps(errors)[:400],
            duration_ms=(time.monotonic() - started) * 1000,
        )
        return result
    except Exception as exc:
        err_text = str(exc).strip() or exc.__class__.__name__
        _record_sink_retention_run(
            result={"sinks": sinks, "errors": errors, "ok": False},
            error=err_text,
            duration_ms=(time.monotonic() - started) * 1000,
        )
        raise


async def _sink_retention_worker() -> None:
    interval = max(60.0, SINK_RETENTION_INTERVAL_SECS)
    while True:
        try:
            result = await run_sink_retention_once()
            logger.info(
                "sink retention: qdrant_deleted=%s mongo_deleted=%s letta_deleted=%s errors=%s",
                ((result.get("sinks") or {}).get("qdrant") or {}).get("deleted"),
                ((result.get("sinks") or {}).get("mongo_raw") or {}).get("deleted"),
                ((result.get("sinks") or {}).get("letta") or {}).get("deleted"),
                result.get("errors"),
            )
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # pragma: no cover - runtime resilience
            logger.warning("Sink retention run failed: %s", exc)
        await asyncio.sleep(interval)


def _set_fanout_summary_cache(summary: dict[str, Any]) -> None:
    by_status = summary.get("by_status") if isinstance(summary, dict) else {}
    by_target = summary.get("by_target") if isinstance(summary, dict) else {}
    fanout_summary_cache["by_status"] = dict(by_status) if isinstance(by_status, dict) else {}
    fanout_summary_cache["by_target"] = dict(by_target) if isinstance(by_target, dict) else {}
    fanout_summary_cache["updated_at"] = _utc_now()
    fanout_summary_cache["updated_monotonic"] = time.monotonic()


def _get_fanout_summary_cache() -> dict[str, Any]:
    return {
        "by_status": dict(fanout_summary_cache.get("by_status") or {}),
        "by_target": dict(fanout_summary_cache.get("by_target") or {}),
    }


def _fanout_cache_fresh(cached: dict[str, Any]) -> bool:
    if not (cached.get("by_status") or cached.get("by_target")):
        return False
    cache_updated = fanout_summary_cache.get("updated_monotonic")
    if not isinstance(cache_updated, (int, float)):
        return False
    cache_age = max(0.0, time.monotonic() - float(cache_updated))
    return cache_age <= max(0.0, FANOUT_SUMMARY_CACHE_TTL_SECS)


def _fanout_sqlite_summary(conn: sqlite3.Connection) -> dict[str, Any]:
    statuses = ("pending", "retrying", "running", "succeeded", "failed")
    by_status: dict[str, int] = {}
    for status in statuses:
        row = conn.execute(
            "SELECT COUNT(*) AS c FROM fanout_outbox WHERE status = ?",
            (status,),
        ).fetchone()
        count = int(row["c"] if row else 0)
        if count > 0:
            by_status[status] = count
    by_target: dict[str, dict[str, int]] = {}
    for target in FANOUT_TARGETS:
        counts: dict[str, int] = {}
        for status in statuses:
            row = conn.execute(
                "SELECT COUNT(*) AS c FROM fanout_outbox WHERE target = ? AND status = ?",
                (target, status),
            ).fetchone()
            count = int(row["c"] if row else 0)
            if count > 0:
                counts[status] = count
        if counts:
            by_target[target] = counts
    return {"by_status": by_status, "by_target": by_target}


async def _query_fanout_summary_uncached() -> dict[str, Any]:
    if _use_mongo_outbox():
        try:
            return await asyncio.wait_for(
                _get_fanout_summary_mongo(),
                timeout=max(1.0, FANOUT_SUMMARY_TIMEOUT_SECS),
            )
        except Exception as exc:
            _demote_outbox_backend(str(exc))
    try:
        return await asyncio.wait_for(
            _task_db_exec(_fanout_sqlite_summary),
            timeout=max(1.0, FANOUT_SUMMARY_TIMEOUT_SECS),
        )
    except Exception as exc:
        if await _promote_outbox_backend_to_mongo_if_sqlite_error(str(exc)):
            return await asyncio.wait_for(
                _get_fanout_summary_mongo(),
                timeout=max(1.0, FANOUT_SUMMARY_TIMEOUT_SECS),
            )
        raise


async def _refresh_fanout_summary_in_background() -> None:
    try:
        summary = await _query_fanout_summary_uncached()
    except Exception as exc:
        logger.warning("Fanout summary background refresh failed: %s", exc)
        return
    _set_fanout_summary_cache(summary)


def _schedule_fanout_summary_refresh() -> None:
    global fanout_summary_refresh_task
    task = fanout_summary_refresh_task
    if task is not None and not task.done():
        return
    fanout_summary_refresh_task = asyncio.create_task(_refresh_fanout_summary_in_background())


async def get_fanout_summary() -> dict[str, Any]:
    cached = _get_fanout_summary_cache()
    if _fanout_cache_fresh(cached):
        return cached
    if cached.get("by_status") or cached.get("by_target"):
        _schedule_fanout_summary_refresh()
        return cached

    try:
        summary = await _query_fanout_summary_uncached()
    except Exception as exc:
        logger.warning("Fanout summary query failed; serving empty summary: %s", exc)
        return {"by_status": {}, "by_target": {}}
    _set_fanout_summary_cache(summary)
    return summary


async def list_fanout_jobs(
    statuses: list[str],
    limit: int = 100,
    target: str | None = None,
) -> list[dict[str, Any]]:
    statuses = [status for status in statuses if status]
    if not statuses:
        return []
    limit = max(1, min(limit, 500))
    if _use_mongo_outbox():
        try:
            return await _list_fanout_jobs_mongo(statuses=statuses, limit=limit, target=target)
        except Exception as exc:
            _demote_outbox_backend(str(exc))

    def _list(conn: sqlite3.Connection):
        placeholders = ",".join("?" for _ in statuses)
        params: list[Any] = list(statuses)
        query = f"SELECT * FROM fanout_outbox WHERE status IN ({placeholders})"
        if target:
            query += " AND target = ?"
            params.append(target)
        query += " ORDER BY updated_at DESC, id DESC LIMIT ?"
        params.append(limit)
        rows = conn.execute(query, params).fetchall()
        return [_fanout_row_to_dict(row) for row in rows]

    return await _task_db_exec(_list)


async def restore_letta_runtime_state_from_outbox() -> None:
    if not _letta_config_enabled():
        return
    failed_jobs = await list_fanout_jobs(["failed"], limit=50, target=FANOUT_TARGET_LETTA)
    for job in failed_jobs:
        error_text = str(job.get("last_error") or "")
        if _is_letta_permanent_error(error_text):
            _set_letta_runtime_disabled(error_text or "prior permanent Letta error")
            await fail_letta_backlog(letta_runtime_disabled_reason or "letta fanout disabled")
            return


def _task_payload_json(payload: dict[str, Any] | None) -> str | None:
    if payload is None:
        return None
    return json.dumps(payload)


def _task_metadata_json(metadata: dict[str, Any] | None) -> str | None:
    if metadata is None:
        return None
    return json.dumps(metadata)


def _feedback_tags_json(tags: list[str] | None) -> str | None:
    if tags is None:
        return None
    return json.dumps(tags)


def _feedback_metadata_json(metadata: dict[str, Any] | None) -> str | None:
    if metadata is None:
        return None
    return json.dumps(metadata)


def _detect_action_type(payload: dict[str, Any] | None) -> str | None:
    if not payload:
        return None
    action_type = payload.get("action_type") or payload.get("actionType")
    if isinstance(action_type, str) and action_type.strip():
        return action_type.strip().lower()
    actions = payload.get("actions")
    if isinstance(actions, list) and actions:
        first = actions[0]
        if isinstance(first, dict):
            value = first.get("type") or first.get("action") or first.get("category")
            if isinstance(value, str) and value.strip():
                return value.strip().lower()
        if isinstance(first, str) and first.strip():
            return first.strip().lower()
    return None


def _detect_risk_level(payload: dict[str, Any] | None) -> str:
    if not payload:
        return "low"
    raw = payload.get("risk_level") or payload.get("riskLevel") or payload.get("risk")
    if isinstance(raw, str) and raw.strip():
        return raw.strip().lower()
    action_type = _detect_action_type(payload)
    if action_type and action_type in HIGH_RISK_ACTIONS:
        return "high"
    return "low"


def _requires_approval(payload: dict[str, Any] | None) -> bool:
    if not HIGH_RISK_APPROVAL_REQUIRED:
        return False
    risk_level = _detect_risk_level(payload)
    return risk_level in ("high", "critical")


def _feedback_row_to_dict(row: sqlite3.Row) -> dict[str, Any]:
    return {
        "id": row["id"],
        "created_at": row["created_at"],
        "project": row["project"],
        "user_id": row["user_id"],
        "source": row["source"],
        "task_id": row["task_id"],
        "rating": row["rating"],
        "sentiment": row["sentiment"],
        "tags": json.loads(row["tags"]) if row["tags"] else None,
        "content": row["content"],
        "topic_path": row["topic_path"],
        "metadata": json.loads(row["metadata"]) if row["metadata"] else None,
    }


async def create_feedback_record(
    project: str | None,
    user_id: str | None,
    source: str | None,
    task_id: str | None,
    rating: int | None,
    sentiment: str | None,
    tags: list[str] | None,
    content: str | None,
    topic_path: str | None,
    metadata: dict[str, Any] | None,
) -> dict[str, Any]:
    feedback_id = uuid.uuid4().hex
    timestamp = datetime.utcnow().isoformat() + "Z"
    tags_json = _feedback_tags_json(tags)
    metadata_json = _feedback_metadata_json(metadata)
    safe_content = (content or "").strip()[:FEEDBACK_MAX_CONTENT]
    safe_source = (source or "user").lower()

    def _create(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """
            INSERT INTO feedback (
                id, created_at, project, user_id, source, task_id, rating, sentiment, tags, content, topic_path, metadata
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                feedback_id,
                timestamp,
                project,
                user_id,
                safe_source,
                task_id,
                rating,
                sentiment,
                tags_json,
                safe_content,
                topic_path,
                metadata_json,
            ),
        )
        conn.commit()
        row = conn.execute("SELECT * FROM feedback WHERE id = ?", (feedback_id,)).fetchone()
        return _feedback_row_to_dict(row)

    return await _task_db_exec(_create)


async def list_feedback_records(
    project: str | None,
    user_id: str | None,
    source: str | None,
    limit: int,
) -> list[dict[str, Any]]:
    limit = max(1, min(limit, 200))

    def _list(conn: sqlite3.Connection):
        clauses = []
        params: list[Any] = []
        if project:
            clauses.append("project = ?")
            params.append(project)
        if user_id:
            clauses.append("user_id = ?")
            params.append(user_id)
        if source:
            clauses.append("source = ?")
            params.append(source.lower())
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        params.append(limit)
        rows = conn.execute(
            f"SELECT * FROM feedback {where} ORDER BY created_at DESC LIMIT ?",
            params,
        ).fetchall()
        return [_feedback_row_to_dict(row) for row in rows]

    return await _task_db_exec(_list)


def build_preference_context(records: list[dict[str, Any]]) -> dict[str, Any]:
    positives: list[str] = []
    negatives: list[str] = []
    notes: list[str] = []
    for entry in records:
        rating = entry.get("rating")
        sentiment = (entry.get("sentiment") or "").lower()
        content = (entry.get("content") or "").strip()
        source = entry.get("source") or "user"
        topic_path = entry.get("topic_path") or ""
        tags = entry.get("tags") or []
        line = content or entry.get("metadata") or ""
        if not line:
            continue
        tag_str = f" [tags: {', '.join(tags)}]" if tags else ""
        topic_str = f" [topic: {topic_path}]" if topic_path else ""
        rendered = f"{line} (source: {source}){topic_str}{tag_str}"
        if rating is not None:
            if rating >= 4:
                positives.append(rendered)
                continue
            if rating <= 2:
                negatives.append(rendered)
                continue
        if sentiment in ("positive", "liked", "good"):
            positives.append(rendered)
        elif sentiment in ("negative", "disliked", "bad"):
            negatives.append(rendered)
        else:
            notes.append(rendered)
    context_lines: list[str] = []
    if positives:
        context_lines.append("Positive preferences:")
        context_lines.extend([f"- {item}" for item in positives])
    if negatives:
        context_lines.append("Avoid or dislike:")
        context_lines.extend([f"- {item}" for item in negatives])
    if notes:
        context_lines.append("Notes:")
        context_lines.extend([f"- {item}" for item in notes])
    return {
        "positive": positives,
        "negative": negatives,
        "notes": notes,
        "context": "\n".join(context_lines).strip(),
        "total": len(records),
        "updated_at": datetime.utcnow().isoformat() + "Z",
    }


TASK_TERMINAL_STATUSES = {"succeeded", "failed", "canceled"}
TASK_MUTABLE_STATUSES = {"queued", "approved", "running", "blocked"}


def _task_parse_datetime(value: str | None) -> datetime | None:
    raw = str(value or "").strip()
    if not raw:
        return None
    if raw.isdigit():
        return datetime.utcfromtimestamp(float(raw))
    candidate = raw
    if candidate.endswith("Z"):
        candidate = candidate[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(candidate)
    except ValueError:
        return None
    if parsed.tzinfo is not None:
        parsed = parsed.astimezone(timezone.utc).replace(tzinfo=None)
    return parsed


def _task_iso_now() -> str:
    return datetime.utcnow().isoformat() + "Z"


def _task_iso_after(seconds: float) -> str:
    when = datetime.utcnow() + timedelta(seconds=max(0.0, seconds))
    return when.isoformat() + "Z"


def _safe_json_load(value: str | None) -> Any:
    if value is None:
        return None
    try:
        return json.loads(value)
    except Exception:
        return value


def _task_row_to_dict(row: sqlite3.Row) -> dict[str, Any]:
    payload = _safe_json_load(row["payload"] if "payload" in row.keys() else None)
    result = _safe_json_load(row["result"] if "result" in row.keys() else None)
    return {
        "id": row["id"],
        "title": row["title"],
        "status": row["status"],
        "project": row["project"],
        "agent": row["agent"],
        "priority": row["priority"],
        "payload": payload if isinstance(payload, dict) else payload,
        "approval_required": bool(row["approval_required"]) if "approval_required" in row.keys() else False,
        "approved": bool(row["approved"]) if "approved" in row.keys() else False,
        "risk_level": row["risk_level"] if "risk_level" in row.keys() else None,
        "action_type": row["action_type"] if "action_type" in row.keys() else None,
        "run_after": row["run_after"] if "run_after" in row.keys() else None,
        "attempts": int(row["attempts"]) if "attempts" in row.keys() and row["attempts"] is not None else 0,
        "max_attempts": int(row["max_attempts"]) if "max_attempts" in row.keys() and row["max_attempts"] is not None else TASK_DEFAULT_MAX_ATTEMPTS,
        "lease_expires_at": row["lease_expires_at"] if "lease_expires_at" in row.keys() else None,
        "claimed_by": row["claimed_by"] if "claimed_by" in row.keys() else None,
        "last_error": row["last_error"] if "last_error" in row.keys() else None,
        "result": result,
        "completed_at": row["completed_at"] if "completed_at" in row.keys() else None,
        "created_at": row["created_at"],
        "updated_at": row["updated_at"],
    }


def _normalize_task_action(raw: Any) -> str | None:
    if raw is None:
        return None
    action = str(raw).strip().lower()
    if not action:
        return None
    alias = {
        "write": "memory_write",
        "remember": "memory_write",
        "search": "memory_search",
        "recall": "memory_search",
        "message": "messaging_command",
        "messaging": "messaging_command",
        "callback": "http_callback",
        "http": "http_callback",
        "provider": "provider_chat",
    }
    return alias.get(action, action)


def _task_validate_callback_url(url: str) -> None:
    from urllib.parse import urlparse

    parsed = urlparse(url)
    if parsed.scheme.lower() not in {"http", "https"}:
        raise HTTPException(422, "http_callback requires http/https URL")
    host = (parsed.hostname or "").lower()
    if not host:
        raise HTTPException(422, "http_callback URL host is required")
    if TASK_CALLBACK_ALLOWED_HOSTS:
        if host not in TASK_CALLBACK_ALLOWED_HOSTS:
            raise HTTPException(422, f"http_callback host '{host}' not allowlisted")


def _validate_task_payload_contract(payload: dict[str, Any] | None) -> dict[str, Any] | None:
    if payload is None:
        return None
    if not isinstance(payload, dict):
        raise HTTPException(422, "task payload must be an object")
    normalized = dict(payload)
    action = _normalize_task_action(
        normalized.get("action")
        or normalized.get("action_type")
        or _detect_action_type(normalized)
    )
    if not action:
        raise HTTPException(422, "task payload requires action")
    if action not in TASK_ALLOWED_ACTIONS:
        raise HTTPException(422, f"task action '{action}' is not allowed")
    normalized["action"] = action
    normalized["action_type"] = action

    if action == "memory_write":
        project_name = str(normalized.get("projectName") or normalized.get("project") or "").strip()
        file_name = str(normalized.get("fileName") or normalized.get("file") or "").strip()
        content = str(normalized.get("content") or "").strip()
        if not project_name or not file_name or not content:
            raise HTTPException(422, "memory_write requires projectName, fileName, and content")
        normalized["projectName"] = project_name
        normalized["fileName"] = file_name
        normalized["content"] = content
    elif action == "memory_search":
        query = str(normalized.get("query") or "").strip()
        if not query:
            raise HTTPException(422, "memory_search requires query")
        normalized["query"] = query
    elif action == "messaging_command":
        text = str(normalized.get("text") or "").strip()
        if not text:
            raise HTTPException(422, "messaging_command requires text")
        normalized["text"] = text
    elif action == "http_callback":
        callback_url = str(normalized.get("url") or "").strip()
        if not callback_url:
            raise HTTPException(422, "http_callback requires url")
        _task_validate_callback_url(callback_url)
        normalized["url"] = callback_url
        method = str(normalized.get("method") or "POST").strip().upper()
        if method not in {"GET", "POST", "PUT", "PATCH", "DELETE"}:
            raise HTTPException(422, f"http_callback method '{method}' not allowed")
        normalized["method"] = method
    elif action == "provider_chat":
        has_prompt = bool(str(normalized.get("prompt") or "").strip())
        has_messages = isinstance(normalized.get("messages"), list) and bool(normalized.get("messages"))
        if not has_prompt and not has_messages:
            raise HTTPException(422, "provider_chat requires prompt or messages")
    return normalized


def _task_retry_delay_secs(attempt: int) -> float:
    exp = max(0, int(attempt) - 1)
    delay = TASK_RETRY_BASE_SECS * (2**exp)
    return min(delay, TASK_RETRY_MAX_SECS)

async def create_task_record(
    title: str,
    project: str | None,
    agent: str | None,
    priority: int,
    payload: dict[str, Any] | None,
    *,
    run_after: str | None = None,
    max_attempts: int | None = None,
) -> dict[str, Any]:
    task_id = uuid.uuid4().hex
    timestamp = _task_iso_now()
    normalized_payload = _validate_task_payload_contract(payload)
    payload_json = _task_payload_json(normalized_payload)
    risk_level = _detect_risk_level(normalized_payload)
    action_type = _normalize_task_action(
        (normalized_payload or {}).get("action") if isinstance(normalized_payload, dict) else None
    ) or _detect_action_type(normalized_payload)
    approval_required = bool((normalized_payload or {}).get("approval_required")) if normalized_payload else False
    if not approval_required:
        approval_required = _requires_approval(normalized_payload)
    approved = bool((normalized_payload or {}).get("approved") or (normalized_payload or {}).get("approval")) if normalized_payload else False
    due_dt = _task_parse_datetime(run_after) if run_after else None
    if run_after and due_dt is None:
        raise HTTPException(422, "run_after must be ISO-8601 timestamp")
    due_at = due_dt.isoformat() + "Z" if due_dt else timestamp
    capped_max_attempts = max(1, min(int(max_attempts or TASK_DEFAULT_MAX_ATTEMPTS), 50))

    def _create(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """
            INSERT INTO tasks (
                id, title, status, project, agent, priority, payload, run_after, attempts, max_attempts,
                lease_expires_at, claimed_by, last_error, result, completed_at, created_at, updated_at,
                approval_required, approved, risk_level, action_type
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                task_id,
                title,
                "queued",
                project,
                agent,
                priority,
                payload_json,
                due_at,
                0,
                capped_max_attempts,
                None,
                None,
                None,
                None,
                None,
                timestamp,
                timestamp,
                int(approval_required),
                int(approved),
                risk_level,
                action_type,
            ),
        )
        event_message = f"Task created (due {due_at})"
        conn.execute(
            """
            INSERT INTO task_events (task_id, timestamp, status, message, metadata)
            VALUES (?, ?, ?, ?, ?)
            """,
            (task_id, timestamp, "queued", event_message, None),
        )
        conn.commit()
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        return _task_row_to_dict(row)

    return await _task_db_exec(_create)


async def list_task_records(
    status: str | None = None,
    project: str | None = None,
    agent: str | None = None,
    limit: int = 50,
) -> list[dict[str, Any]]:
    limit = max(1, min(limit, 200))

    def _list(conn: sqlite3.Connection):
        clauses = []
        params: list[Any] = []
        if status:
            clauses.append("status = ?")
            params.append(str(status).lower())
        if project:
            clauses.append("project = ?")
            params.append(project)
        if agent:
            normalized_agent = str(agent).strip().lower()
            if normalized_agent == "unassigned":
                clauses.append("(agent IS NULL OR trim(agent) = '')")
            else:
                clauses.append("lower(trim(coalesce(agent, ''))) = ?")
                params.append(normalized_agent)
        where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        params.append(limit)
        rows = conn.execute(
            f"SELECT * FROM tasks {where} ORDER BY priority DESC, run_after ASC, created_at DESC LIMIT ?",
            params,
        ).fetchall()
        return [_task_row_to_dict(row) for row in rows]

    return await _task_db_exec(_list)


async def get_task_record(task_id: str) -> dict[str, Any] | None:
    def _get(conn: sqlite3.Connection):
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        if not row:
            return None
        return _task_row_to_dict(row)

    return await _task_db_exec(_get)


async def get_task_events(task_id: str) -> list[dict[str, Any]]:
    def _events(conn: sqlite3.Connection):
        rows = conn.execute(
            "SELECT * FROM task_events WHERE task_id = ? ORDER BY id DESC", (task_id,)
        ).fetchall()
        events: list[dict[str, Any]] = []
        for row in rows:
            metadata = row["metadata"]
            events.append(
                {
                    "id": row["id"],
                    "task_id": row["task_id"],
                    "timestamp": row["timestamp"],
                    "status": row["status"],
                    "message": row["message"],
                    "metadata": json.loads(metadata) if metadata else None,
                }
            )
        return events

    return await _task_db_exec(_events)


async def list_deadletter_task_records(
    project: str | None = None,
    limit: int = 100,
) -> list[dict[str, Any]]:
    limit = max(1, min(limit, 500))

    def _list(conn: sqlite3.Connection):
        params: list[Any] = []
        where = "WHERE status = 'failed'"
        if project:
            where += " AND project = ?"
            params.append(project)
        params.append(limit)
        rows = conn.execute(
            f"SELECT * FROM tasks {where} ORDER BY updated_at DESC LIMIT ?",
            params,
        ).fetchall()
        return [_task_row_to_dict(row) for row in rows]

    return await _task_db_exec(_list)


async def replay_task_record(
    task_id: str,
    *,
    actor: str | None = None,
    note: str | None = None,
    reset_attempts: bool = True,
) -> dict[str, Any] | None:
    now = _task_iso_now()

    def _replay(conn: sqlite3.Connection):
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        if not row:
            return None
        conn.execute("BEGIN IMMEDIATE")
        attempts = 0 if reset_attempts else int(row["attempts"] or 0)
        conn.execute(
            """
            UPDATE tasks
            SET status = 'queued',
                run_after = ?,
                lease_expires_at = NULL,
                claimed_by = NULL,
                completed_at = NULL,
                attempts = ?,
                last_error = NULL,
                updated_at = ?
            WHERE id = ?
            """,
            (now, attempts, now, task_id),
        )
        event_metadata = {"actor": actor, "note": note, "reset_attempts": bool(reset_attempts)}
        conn.execute(
            """
            INSERT INTO task_events (task_id, timestamp, status, message, metadata)
            VALUES (?, ?, ?, ?, ?)
            """,
            (
                task_id,
                now,
                "queued",
                "Task replayed to queue",
                json.dumps(event_metadata),
            ),
        )
        conn.commit()
        updated = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        return _task_row_to_dict(updated)

    return await _task_db_exec(_replay)


async def get_task_runtime_snapshot() -> dict[str, Any]:
    now = _task_iso_now()

    def _snapshot(conn: sqlite3.Connection) -> dict[str, Any]:
        by_status_rows = conn.execute(
            "SELECT status, COUNT(*) AS total FROM tasks GROUP BY status"
        ).fetchall()
        by_status = {
            str(row["status"]): int(row["total"])
            for row in by_status_rows
            if row["status"] is not None
        }
        ready_row = conn.execute(
            """
            SELECT COUNT(*) AS total
            FROM tasks
            WHERE status IN ('queued', 'approved')
              AND (approval_required = 0 OR approved = 1)
              AND attempts < max_attempts
              AND (run_after IS NULL OR run_after <= ?)
            """,
            (now,),
        ).fetchone()
        oldest_row = conn.execute(
            """
            SELECT run_after
            FROM tasks
            WHERE status IN ('queued', 'approved')
            ORDER BY run_after ASC, created_at ASC
            LIMIT 1
            """
        ).fetchone()
        running_row = conn.execute(
            "SELECT COUNT(*) AS total FROM tasks WHERE status = 'running'"
        ).fetchone()
        deadletter_row = conn.execute(
            "SELECT COUNT(*) AS total FROM tasks WHERE status = 'failed'"
        ).fetchone()
        return {
            "queueReady": int((ready_row["total"] if ready_row else 0) or 0),
            "running": int((running_row["total"] if running_row else 0) or 0),
            "deadletter": int((deadletter_row["total"] if deadletter_row else 0) or 0),
            "oldestPendingRunAfter": oldest_row["run_after"] if oldest_row else None,
            "byStatus": by_status,
        }

    snapshot = await _task_db_exec(_snapshot)
    return {
        **task_runtime_health,
        **snapshot,
        "timestamp": now,
    }


async def update_task_status(
    task_id: str,
    status: str,
    message: str | None,
    metadata: dict[str, Any] | None,
) -> dict[str, Any] | None:
    normalized_status = str(status or "").strip().lower()
    if normalized_status not in TASK_TERMINAL_STATUSES.union(TASK_MUTABLE_STATUSES):
        raise HTTPException(422, f"invalid task status '{status}'")
    timestamp = _task_iso_now()
    metadata_json = _task_metadata_json(metadata)
    terminal = normalized_status in TASK_TERMINAL_STATUSES
    run_after_override = None
    if isinstance(metadata, dict) and metadata.get("run_after"):
        parsed = _task_parse_datetime(str(metadata.get("run_after")))
        if parsed is not None:
            run_after_override = parsed.isoformat() + "Z"
    result_payload = None
    if isinstance(metadata, dict):
        result_payload = metadata.get("result") if "result" in metadata else metadata
    result_json = json.dumps(result_payload) if result_payload is not None else None

    def _update(conn: sqlite3.Connection):
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        if not row:
            return None
        conn.execute("BEGIN IMMEDIATE")
        conn.execute(
            """
            UPDATE tasks
            SET status = ?, updated_at = ?, run_after = ?, lease_expires_at = ?, claimed_by = ?,
                completed_at = ?, last_error = ?, result = ?
            WHERE id = ?
            """,
            (
                normalized_status,
                timestamp,
                run_after_override if run_after_override is not None else (timestamp if normalized_status in {"queued", "approved"} else row["run_after"]),
                None if terminal or normalized_status in {"queued", "approved", "blocked"} else row["lease_expires_at"],
                None if terminal or normalized_status in {"queued", "approved", "blocked"} else row["claimed_by"],
                timestamp if terminal else row["completed_at"],
                (str(message or row["last_error"] or "")[:2000] if normalized_status == "failed" else row["last_error"]),
                result_json if result_json is not None else row["result"],
                task_id,
            ),
        )
        conn.execute(
            """
            INSERT INTO task_events (task_id, timestamp, status, message, metadata)
            VALUES (?, ?, ?, ?, ?)
            """,
            (task_id, timestamp, normalized_status, message, metadata_json),
        )
        conn.commit()
        updated = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        return _task_row_to_dict(updated)

    task = await _task_db_exec(_update)
    if task and terminal and TASK_RESULT_WRITEBACK_ENABLED:
        with contextlib.suppress(Exception):
            await _record_task_outcome_memory(task, message=message, metadata=metadata)
    return task


async def approve_task_record(
    task_id: str,
    approver: str | None,
    note: str | None,
) -> dict[str, Any] | None:
    timestamp = _task_iso_now()

    def _approve(conn: sqlite3.Connection):
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        if not row:
            return None
        conn.execute("BEGIN IMMEDIATE")
        status = row["status"]
        if status in ("blocked", "queued"):
            status = "approved"
        conn.execute(
            "UPDATE tasks SET approved = 1, status = ?, updated_at = ?, run_after = COALESCE(run_after, ?) WHERE id = ?",
            (status, timestamp, timestamp, task_id),
        )
        metadata = {"approver": approver, "note": note} if approver or note else None
        conn.execute(
            """
            INSERT INTO task_events (task_id, timestamp, status, message, metadata)
            VALUES (?, ?, ?, ?, ?)
            """,
            (task_id, timestamp, "approved", "Task approved", json.dumps(metadata) if metadata else None),
        )
        conn.commit()
        updated = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        return _task_row_to_dict(updated)

    return await _task_db_exec(_approve)


async def recover_expired_task_leases(limit: int = 100) -> int:
    if limit <= 0:
        return 0
    now = _task_iso_now()

    def _recover(conn: sqlite3.Connection) -> int:
        conn.execute("BEGIN IMMEDIATE")
        rows = conn.execute(
            """
            SELECT id FROM tasks
            WHERE status = 'running'
              AND lease_expires_at IS NOT NULL
              AND lease_expires_at <= ?
            ORDER BY lease_expires_at ASC
            LIMIT ?
            """,
            (now, int(limit)),
        ).fetchall()
        recovered = 0
        for row in rows:
            task_id = row["id"]
            conn.execute(
                """
                UPDATE tasks
                SET status = 'queued',
                    run_after = ?,
                    lease_expires_at = NULL,
                    claimed_by = NULL,
                    updated_at = ?,
                    last_error = COALESCE(last_error, 'lease expired; requeued')
                WHERE id = ?
                """,
                (now, now, task_id),
            )
            conn.execute(
                """
                INSERT INTO task_events (task_id, timestamp, status, message, metadata)
                VALUES (?, ?, ?, ?, ?)
                """,
                (task_id, now, "queued", "Lease expired; requeued by scheduler", None),
            )
            recovered += 1
        conn.commit()
        return recovered

    return await _task_db_exec(_recover)


async def requeue_task_for_retry(
    task_id: str,
    *,
    error: str,
    worker: str | None = None,
) -> dict[str, Any] | None:
    now = _task_iso_now()
    error_text = str(error or "task execution failed")[:2000]

    def _requeue(conn: sqlite3.Connection):
        row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        if not row:
            return None
        attempts = int(row["attempts"] or 0)
        max_attempts = int(row["max_attempts"] or TASK_DEFAULT_MAX_ATTEMPTS)
        terminal_failure = attempts >= max_attempts
        conn.execute("BEGIN IMMEDIATE")
        if terminal_failure:
            next_status = "failed"
            next_run_after = row["run_after"]
            completed_at = now
            message = f"Task failed permanently after {attempts}/{max_attempts} attempts"
        else:
            next_status = "queued"
            delay_secs = _task_retry_delay_secs(attempts)
            next_run_after = _task_iso_after(delay_secs)
            completed_at = None
            message = f"Task requeued after failure (attempt {attempts}/{max_attempts})"
        conn.execute(
            """
            UPDATE tasks
            SET status = ?, run_after = ?, lease_expires_at = NULL, claimed_by = NULL,
                updated_at = ?, completed_at = ?, last_error = ?
            WHERE id = ?
            """,
            (next_status, next_run_after, now, completed_at, error_text, task_id),
        )
        event_metadata = {"worker": worker, "error": error_text}
        conn.execute(
            """
            INSERT INTO task_events (task_id, timestamp, status, message, metadata)
            VALUES (?, ?, ?, ?, ?)
            """,
            (task_id, now, next_status, message, json.dumps(event_metadata)),
        )
        conn.commit()
        updated = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
        return _task_row_to_dict(updated)

    task = await _task_db_exec(_requeue)
    if task and task.get("status") == "failed" and TASK_RESULT_WRITEBACK_ENABLED:
        with contextlib.suppress(Exception):
            await _record_task_outcome_memory(task, message="Task failed after retries", metadata={"error": error_text})
    return task


async def claim_next_task(worker: str | None) -> dict[str, Any] | None:
    timestamp = _task_iso_now()
    lease_expires = _task_iso_after(TASK_LEASE_SECS)
    worker_name = str(worker or "external").strip() or "external"
    worker_key = worker_name.lower()
    worker_is_internal = worker_key.startswith("internal-worker")
    await recover_expired_task_leases(limit=25)

    def _claim(conn: sqlite3.Connection):
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute(
            """
            SELECT * FROM tasks
            WHERE status IN ('queued', 'approved')
              AND (approval_required = 0 OR approved = 1)
              AND attempts < max_attempts
              AND (run_after IS NULL OR run_after <= ?)
              AND (
                    agent IS NULL
                    OR trim(agent) = ''
                    OR lower(trim(agent)) = 'any'
                    OR lower(trim(agent)) = ?
                    OR (? = 1 AND lower(trim(agent)) = 'internal')
                    OR (? = 0 AND lower(trim(agent)) = 'external')
              )
            ORDER BY priority DESC, run_after ASC, created_at ASC
            LIMIT 1
            """,
            (timestamp, worker_key, int(worker_is_internal), int(worker_is_internal)),
        ).fetchone()
        if not row:
            conn.commit()
            return None
        conn.execute(
            """
            UPDATE tasks
            SET status = ?, updated_at = ?, lease_expires_at = ?, claimed_by = ?, attempts = attempts + 1
            WHERE id = ?
            """,
            ("running", timestamp, lease_expires, worker_name, row["id"]),
        )
        message = f"Claimed by {worker_name}"
        conn.execute(
            """
            INSERT INTO task_events (task_id, timestamp, status, message, metadata)
            VALUES (?, ?, ?, ?, ?)
            """,
            (row["id"], timestamp, "running", message, json.dumps({"worker": worker_name})),
        )
        conn.commit()
        updated = conn.execute("SELECT * FROM tasks WHERE id = ?", (row["id"],)).fetchone()
        return _task_row_to_dict(updated)

    return await _task_db_exec(_claim)


async def _execute_task_action(task: dict[str, Any], worker_name: str) -> dict[str, Any]:
    payload = task.get("payload")
    if not isinstance(payload, dict):
        raise OrchestratorError("task payload missing")
    action = _normalize_task_action(payload.get("action") or task.get("action_type"))
    if not action:
        raise OrchestratorError("task action missing")
    if action not in TASK_ALLOWED_ACTIONS:
        raise OrchestratorError(f"task action '{action}' is not allowed")

    if action == "memory_write":
        write_payload = MemoryWrite(
            projectName=str(payload.get("projectName") or payload.get("project") or task.get("project") or "").strip(),
            fileName=str(payload.get("fileName") or payload.get("file") or "").strip(),
            content=str(payload.get("content") or ""),
            topicPath=(str(payload.get("topic_path") or "").strip() or None),
        )
        result = await write_memory(write_payload, _synthetic_request("/agents/tasks/worker"))
        return {"action": action, "result": result}

    if action == "memory_search":
        search_payload = MemorySearch(
            query=str(payload.get("query") or "").strip(),
            limit=max(1, min(int(payload.get("limit") or 10), 50)),
            project=str(payload.get("project") or task.get("project") or "").strip() or None,
            topic_path=str(payload.get("topic_path") or "").strip() or None,
            fetch_content=bool(payload.get("fetch_content", False)),
            include_retrieval_debug=bool(payload.get("include_retrieval_debug", False)),
            user_id=str(payload.get("user_id") or "").strip() or None,
            include_preferences=bool(payload.get("include_preferences", True)),
        )
        result = await search_memory(search_payload)
        return {"action": action, "result": result}

    if action == "messaging_command":
        channel = str(payload.get("channel") or "custom").strip() or "custom"
        source_id = str(payload.get("source_id") or worker_name).strip() or worker_name
        text = str(payload.get("text") or "").strip()
        if not text:
            raise OrchestratorError("messaging_command text is required")
        parsed = _parse_messaging_command(text, require_prefix=bool(payload.get("require_prefix", False)))
        if parsed is None:
            parsed = {"action": "help", "content": "", "directives": {}, "raw": text}
        result = await _execute_messaging_command(
            parsed,
            channel=channel,
            source_id=source_id,
            default_project=str(payload.get("project") or task.get("project") or MESSAGING_DEFAULT_PROJECT),
            topic_root=str(payload.get("topic_path") or payload.get("topic_root") or f"channels/{channel}"),
            project_override=str(payload.get("project") or "").strip() or None,
            topic_override=str(payload.get("topic_path") or "").strip() or None,
            user_id=str(payload.get("user_id") or "").strip() or None,
        )
        return {"action": action, "result": result}

    if action == "http_callback":
        callback_url = str(payload.get("url") or "").strip()
        _task_validate_callback_url(callback_url)
        method = str(payload.get("method") or "POST").strip().upper()
        body = payload.get("body")
        headers_raw = payload.get("headers")
        headers = {str(k): str(v) for k, v in headers_raw.items()} if isinstance(headers_raw, dict) else {}
        async with httpx.AsyncClient(timeout=TASK_CALLBACK_TIMEOUT_SECS) as client:
            response = await client.request(method, callback_url, json=body, headers=headers)
        return {
            "action": action,
            "status_code": response.status_code,
            "response_text": response.text[:600],
        }

    if action == "provider_chat":
        if not TASK_PROVIDER_CHAT_ENABLED:
            raise OrchestratorError("provider_chat action is disabled")
        endpoint = str(payload.get("url") or TASK_PROVIDER_CHAT_URL).strip()
        if not endpoint:
            raise OrchestratorError("provider_chat endpoint is not configured")
        if endpoint.endswith("/"):
            endpoint = endpoint[:-1]
        if endpoint.endswith("/v1"):
            endpoint = f"{endpoint}/chat/completions"
        elif not endpoint.endswith("/chat/completions"):
            endpoint = f"{endpoint}/v1/chat/completions"
        model = str(payload.get("model") or TASK_PROVIDER_CHAT_MODEL or LETTA_AGENT_MODEL or "").strip()
        if not model:
            raise OrchestratorError("provider_chat model is not configured")
        messages = payload.get("messages")
        if not isinstance(messages, list) or not messages:
            prompt = str(payload.get("prompt") or "").strip()
            if not prompt:
                raise OrchestratorError("provider_chat requires prompt or messages")
            messages = [{"role": "user", "content": prompt}]
        body = {
            "model": model,
            "messages": messages,
            "temperature": float(payload.get("temperature", 0.2)),
        }
        headers = {"content-type": "application/json"}
        if TASK_PROVIDER_CHAT_API_KEY:
            headers["authorization"] = f"Bearer {TASK_PROVIDER_CHAT_API_KEY}"
        async with httpx.AsyncClient(timeout=max(10.0, TASK_CALLBACK_TIMEOUT_SECS)) as client:
            response = await client.post(endpoint, json=body, headers=headers)
        if response.status_code >= 400:
            raise OrchestratorError(f"provider_chat failed: status={response.status_code} body={response.text[:240]}")
        data = response.json() if response.content else {}
        choices = data.get("choices") if isinstance(data, dict) else None
        content = None
        if isinstance(choices, list) and choices:
            first = choices[0]
            if isinstance(first, dict):
                message = first.get("message")
                if isinstance(message, dict):
                    content = message.get("content")
        return {
            "action": action,
            "model": model,
            "content": str(content or "")[:1200],
            "raw": data,
        }

    raise OrchestratorError(f"unsupported task action '{action}'")


async def _record_task_outcome_memory(
    task: dict[str, Any],
    *,
    message: str | None = None,
    metadata: dict[str, Any] | None = None,
) -> None:
    if not TASK_RESULT_WRITEBACK_ENABLED:
        return
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    project = str(task.get("project") or payload.get("projectName") or payload.get("project") or "").strip()
    if not project:
        return
    task_id = str(task.get("id") or "").strip()
    if not task_id:
        return
    action = str(task.get("action_type") or payload.get("action") or "task").strip().lower()
    topic_path = normalize_topic_path(str(payload.get("topic_path") or f"tasks/{action}"))
    file_name = normalize_memory_path(f"tasks/{task_id}__latest.json")
    outcome_payload = {
        "task_id": task_id,
        "title": task.get("title"),
        "status": task.get("status"),
        "project": project,
        "action": action,
        "attempts": task.get("attempts"),
        "max_attempts": task.get("max_attempts"),
        "message": message,
        "metadata": metadata,
        "result": task.get("result"),
        "updated_at": task.get("updated_at") or _task_iso_now(),
    }
    write_payload = MemoryWrite(
        projectName=project,
        fileName=file_name,
        content=json.dumps(outcome_payload, ensure_ascii=True, sort_keys=True),
        topicPath=topic_path,
    )
    await write_memory(write_payload, _synthetic_request("/agents/tasks/outcome"))


async def _agent_task_worker(worker_index: int) -> None:
    worker_name = f"internal-worker-{worker_index}"
    task_runtime_health["workersRunning"] = int(task_runtime_health.get("workersRunning") or 0) + 1
    try:
        while True:
            task: dict[str, Any] | None = None
            try:
                task = await claim_next_task(worker_name)
                if not task:
                    await asyncio.sleep(max(0.2, TASK_WORKER_POLL_SECS))
                    continue
                task_runtime_health["claimed"] = int(task_runtime_health.get("claimed") or 0) + 1
                result = await _execute_task_action(task, worker_name=worker_name)
                await update_task_status(
                    str(task["id"]),
                    "succeeded",
                    "Task executed successfully",
                    {"worker": worker_name, "result": result},
                )
                task_runtime_health["succeeded"] = int(task_runtime_health.get("succeeded") or 0) + 1
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                task_runtime_health["lastWorkerError"] = str(exc)[:300]
                if task and task.get("id"):
                    retried = await requeue_task_for_retry(
                        str(task["id"]),
                        error=str(exc),
                        worker=worker_name,
                    )
                    if retried and retried.get("status") == "queued":
                        task_runtime_health["retried"] = int(task_runtime_health.get("retried") or 0) + 1
                    else:
                        task_runtime_health["failed"] = int(task_runtime_health.get("failed") or 0) + 1
                else:
                    await asyncio.sleep(max(0.3, TASK_WORKER_POLL_SECS))
    finally:
        task_runtime_health["workersRunning"] = max(0, int(task_runtime_health.get("workersRunning") or 0) - 1)


async def _task_scheduler_worker() -> None:
    task_runtime_health["schedulerRunning"] = True
    try:
        while True:
            try:
                recovered = await recover_expired_task_leases(limit=200)
                task_runtime_health["lastSchedulerTickAt"] = _task_iso_now()
                task_runtime_health["lastSchedulerRecovered"] = recovered
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                task_runtime_health["lastWorkerError"] = str(exc)[:300]
                logger.warning("Task scheduler loop failed: %s", exc)
            await asyncio.sleep(max(0.5, TASK_WORKER_POLL_SECS))
    finally:
        task_runtime_health["schedulerRunning"] = False


_load_override_history()
_load_memory_write_history()
_load_topic_tree()


@app.on_event("startup")
async def orchestrator_startup() -> None:
    global task_scheduler_task, agent_task_worker_tasks
    validate_orchestrator_security_posture()
    asyncio.create_task(_signal_refresh_loop())
    asyncio.create_task(_override_refresh_loop())
    await ensure_task_db()
    if TASK_SCHEDULER_ENABLED and task_scheduler_task is None:
        task_scheduler_task = asyncio.create_task(_task_scheduler_worker(), name="task-scheduler")
    if TASK_INTERNAL_WORKERS_ENABLED and AGENT_TASK_WORKERS > 0 and not agent_task_worker_tasks:
        for idx in range(AGENT_TASK_WORKERS):
            agent_task_worker_tasks.append(
                asyncio.create_task(_agent_task_worker(idx), name=f"task-worker-{idx}")
            )


class MemoryWrite(BaseModel):
    projectName: str = Field(..., description="Project identifier")
    fileName: str = Field(..., description="File name inside the project")
    content: str = Field(..., description="Payload to store")
    topicPath: str | None = Field(None, description="Optional topic path override")


class AgentTaskCreate(BaseModel):
    title: str = Field(..., description="Short task label")
    project: str | None = Field(None, description="Project identifier")
    agent: str | None = Field(None, description="Preferred agent/runner")
    priority: int = Field(0, description="Higher number = higher priority")
    payload: dict[str, Any] | None = Field(None, description="Arbitrary task payload")
    risk_level: str | None = Field(None, description="low|medium|high|critical")
    action_type: str | None = Field(None, description="Action category, e.g. payment")
    approval_required: bool | None = Field(None, description="Force approval gate")
    approved: bool | None = Field(None, description="Mark approved at creation")
    topic_path: str | None = Field(None, description="Topic path for task feedback")
    run_after: str | None = Field(None, description="Optional ISO-8601 schedule time")
    max_attempts: int | None = Field(None, ge=1, le=50, description="Max worker attempts before deadletter")


class AgentTaskStatus(BaseModel):
    status: str = Field(..., description="queued|running|succeeded|failed|canceled|blocked")
    message: str | None = Field(None, description="Optional status message")
    metadata: dict[str, Any] | None = Field(None, description="Optional metadata payload")


class AgentTaskApproval(BaseModel):
    approver: str | None = Field(None, description="Who approved the task")
    note: str | None = Field(None, description="Optional approval note")


class AgentTaskReplay(BaseModel):
    actor: str | None = Field(None, description="Who requested replay")
    note: str | None = Field(None, description="Optional replay note")
    reset_attempts: bool = Field(True, description="Reset attempts to zero before replay")


class TrajectoryIngest(BaseModel):
    project: str
    summary: str
    trajectory: dict[str, Any]


class FeedbackCreate(BaseModel):
    project: str | None = Field(None, description="Project identifier")
    user_id: str | None = Field(None, description="User identifier")
    source: str | None = Field("user", description="user|agent|system")
    task_id: str | None = Field(None, description="Related task id")
    rating: int | None = Field(None, ge=1, le=5)
    sentiment: str | None = Field(None, description="positive|neutral|negative")
    tags: list[str] | None = Field(None, description="Freeform tags")
    content: str | None = Field(None, description="Feedback content")
    topic_path: str | None = Field(None, description="Optional topic path")
    metadata: dict[str, Any] | None = Field(None, description="Optional metadata")


class TelemetryMetrics(BaseModel):
    timestamp: datetime
    queueDepth: int = Field(ge=0)
    batchSize: int = Field(ge=0)
    totals: Dict[str, int]


class TradingMetrics(BaseModel):
    timestamp: datetime
    open_positions: int
    total_value_usd: float
    unrealized_pnl: float
    realized_pnl: float
    daily_pnl: float
    positions: list[dict[str, Any]]
    price_cache_entries: int | None = None
    price_cache_max_age: float | None = None
    price_cache_ttl: float | None = None
    price_cache_freshness: float | None = None
    price_cache_penalty: float | None = None


class StrategyEntry(BaseModel):
    name: str
    capital: float
    win_rate: float | None = None
    daily_pnl: float | None = None
    kelly_fraction: float | None = None
    risk_score: float | None = None
    price_cache_entries: int | None = None
    price_cache_max_age: float | None = None
    price_cache_freshness: float | None = None
    price_cache_penalty: float | None = None
    notes: str | None = None
    memory_ref: str | None = None


class StrategyMetrics(BaseModel):
    timestamp: datetime
    strategies: list[StrategyEntry]


class SignalEntry(BaseModel):
    symbol: str
    address: str
    price_usd: float
    volume_24h_usd: float
    liquidity_usd: float
    momentum_score: float
    risk_score: float
    verified: bool = False
    created_at: datetime | None = None
    file: str


class OverrideEntry(BaseModel):
    symbol: str
    priority: str
    reason: str | None = None
    size_before: float
    size_after: float
    confidence_before: float
    confidence_after: float
    override_strength: float
    multiplier: float
    timestamp: datetime | None = None
    file: str | None = None


class SidecarHealthPayload(BaseModel):
    timestamp: datetime
    healthy: bool
    detail: str


def _build_mcp_headers(session_id: str | None = None) -> dict[str, str]:
    headers = dict(MCP_HEADERS)
    if session_id:
        headers[MCP_SESSION_HEADER] = session_id
    return headers


def _extract_mcp_session_id(headers: Any) -> str | None:
    if headers is None or not hasattr(headers, "get"):
        return None
    for key in ("mcp-session-id", "x-mcp-session-id", "Mcp-Session-Id", "X-Mcp-Session-Id"):
        value = headers.get(key)
        if not isinstance(value, str):
            continue
        value = value.strip()
        if value:
            return value
    return None


def _mcp_error_detail_from_response(resp: httpx.Response) -> str:
    text = (resp.text or "").strip()
    if text:
        try:
            payload = json.loads(text)
            if isinstance(payload, dict):
                error = payload.get("error")
                if isinstance(error, dict):
                    message = error.get("message")
                    if isinstance(message, str) and message.strip():
                        return message.strip()
        except json.JSONDecodeError:
            pass
    if text:
        return text[:500]
    return f"Memory MCP HTTP {resp.status_code}"


def _is_mcp_missing_session_error(status_code: int, detail: str) -> bool:
    if status_code not in (400, 401, 404, 422):
        return False
    lowered = (detail or "").lower()
    return (
        "no valid session id" in lowered
        or "no valid session-id" in lowered
        or "session id required" in lowered
        or "invalid session id" in lowered
        or "session not found" in lowered
    )


def _extract_mcp_payload(resp: httpx.Response) -> dict[str, Any]:
    event_payload: dict[str, Any] | None = None
    text = resp.text or ""
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("data:"):
            continue
        raw = line[5:].strip()
        if not raw:
            continue
        try:
            candidate = json.loads(raw)
        except json.JSONDecodeError:
            continue
        if isinstance(candidate, dict):
            event_payload = candidate
    if event_payload is not None:
        return event_payload
    try:
        payload = resp.json()
        if isinstance(payload, dict):
            return payload
    except Exception:
        pass
    if text:
        raise HTTPException(502, f"Unable to parse MCP response payload: {text[:300]}")
    raise HTTPException(502, "No MCP response payload")


async def _post_mcp_request(
    payload: dict[str, Any],
    session_id: str | None = None,
) -> httpx.Response:
    last_error: Exception | None = None
    for attempt in range(1, max(MEMMCP_HTTP_RETRIES, 1) + 1):
        try:
            headers = _build_mcp_headers(session_id=session_id)
            client = MCP_CLIENT
            if client is None:
                async with httpx.AsyncClient(timeout=MCP_CLIENT_TIMEOUT) as temp_client:
                    resp = await temp_client.post(
                        MEMMCP_HTTP_URL, json=payload, headers=headers
                    )
            else:
                resp = await client.post(MEMMCP_HTTP_URL, json=payload, headers=headers)
            return resp
        except httpx.ReadTimeout as err:
            last_error = err
            if attempt >= MEMMCP_HTTP_RETRIES:
                raise HTTPException(504, "Memory MCP timeout") from err
            await asyncio.sleep(MEMMCP_HTTP_RETRY_DELAY_SECS * attempt)
        except httpx.HTTPError as err:
            last_error = err
            if attempt >= MEMMCP_HTTP_RETRIES:
                raise HTTPException(502, f"Memory MCP error: {err}") from err
            await asyncio.sleep(MEMMCP_HTTP_RETRY_DELAY_SECS * attempt)
    raise HTTPException(502, f"Memory MCP error: {last_error}")


def _invalidate_mcp_session() -> None:
    global MCP_SESSION_ID
    MCP_SESSION_ID = None


async def _ensure_mcp_session(force_refresh: bool = False) -> str:
    global MCP_SESSION_ID
    if MCP_SESSION_ID and not force_refresh:
        return MCP_SESSION_ID
    async with mcp_session_lock:
        if MCP_SESSION_ID and not force_refresh:
            return MCP_SESSION_ID
        if force_refresh:
            MCP_SESSION_ID = None
        init_payload = {
            "jsonrpc": "2.0",
            "id": str(uuid.uuid4()),
            "method": "initialize",
            "params": {
                "protocolVersion": MCP_HEADERS.get("MCP-Protocol-Version", "2024-11-05"),
                "capabilities": {},
                "clientInfo": {"name": MCP_CLIENT_NAME, "version": MCP_CLIENT_VERSION},
            },
        }
        resp = await _post_mcp_request(init_payload, session_id=None)
        detail = _mcp_error_detail_from_response(resp)
        if resp.status_code != 200:
            raise HTTPException(resp.status_code, detail)
        session_id = _extract_mcp_session_id(resp.headers)
        if not session_id:
            init_response = _extract_mcp_payload(resp)
            maybe_result = init_response.get("result") if isinstance(init_response, dict) else None
            if isinstance(maybe_result, dict):
                for key in ("sessionId", "session_id"):
                    value = maybe_result.get(key)
                    if isinstance(value, str) and value.strip():
                        session_id = value.strip()
                        break
        if not session_id:
            raise HTTPException(502, "Memory MCP initialize did not return a session id")
        MCP_SESSION_ID = session_id
        initialized_payload = {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}}
        with contextlib.suppress(Exception):
            await _post_mcp_request(initialized_payload, session_id=session_id)
        return session_id


async def _call_mcp(payload: dict[str, Any]) -> dict[str, Any]:
    global MCP_SESSION_ID
    for attempt in range(2):
        session_id = await _ensure_mcp_session(force_refresh=attempt > 0)
        resp = await _post_mcp_request(payload, session_id=session_id)
        refreshed_session_id = _extract_mcp_session_id(resp.headers)
        if refreshed_session_id and refreshed_session_id != MCP_SESSION_ID:
            MCP_SESSION_ID = refreshed_session_id
        detail = _mcp_error_detail_from_response(resp)
        if resp.status_code != 200:
            if _is_mcp_missing_session_error(resp.status_code, detail):
                _invalidate_mcp_session()
                continue
            raise HTTPException(resp.status_code, detail)
        data = _extract_mcp_payload(resp)
        if "error" in data:
            error_payload = data.get("error")
            error_text = (
                error_payload
                if isinstance(error_payload, str)
                else json.dumps(error_payload, ensure_ascii=True)
            )
            if _is_mcp_missing_session_error(400, error_text):
                _invalidate_mcp_session()
                continue
            raise HTTPException(500, f"MCP error: {error_payload}")
        result = data.get("result")
        if result is None:
            raise HTTPException(500, "No MCP response data")
        return result
    raise HTTPException(502, "Memory MCP session invalid and refresh failed")


def _mcp_content_entries(content: Any) -> list[Any]:
    if isinstance(content, list):
        return content
    if content is None:
        return []
    return [content]


def _extract_mcp_text_chunks(content: Any) -> list[str]:
    chunks: list[str] = []
    if isinstance(content, str):
        if content:
            chunks.append(content)
        return chunks
    if isinstance(content, list):
        for item in content:
            chunks.extend(_extract_mcp_text_chunks(item))
        return chunks
    if isinstance(content, dict):
        text = content.get("text")
        if isinstance(text, str) and text:
            chunks.append(text)
        # Some adapters wrap tool output under nested content/data fields.
        nested_content = content.get("content")
        if nested_content is not None:
            chunks.extend(_extract_mcp_text_chunks(nested_content))
        nested_data = content.get("data")
        if nested_data is not None and nested_data is not nested_content:
            chunks.extend(_extract_mcp_text_chunks(nested_data))
    return chunks


def _mcp_result_error_message(result: dict[str, Any]) -> str | None:
    if not result.get("isError"):
        return None
    text_chunks = _extract_mcp_text_chunks(result.get("content"))
    for chunk in text_chunks:
        text = chunk.strip()
        if not text:
            continue
        try:
            parsed = json.loads(text)
        except json.JSONDecodeError:
            return text
        if isinstance(parsed, dict):
            detail = parsed.get("error") or parsed.get("message") or parsed.get("detail")
            name = parsed.get("name")
            if isinstance(detail, str) and detail:
                if isinstance(name, str) and name:
                    return f"{name}: {detail}"
                return detail
        return text
    return "MCP tool returned isError=true"


def _coerce_mcp_name_tokens(value: Any) -> list[str]:
    tokens: list[str] = []

    def _append(raw: str) -> None:
        token = raw.strip()
        if token:
            tokens.append(token)

    if isinstance(value, str):
        text = value.strip()
        if not text:
            return []
        # Tool adapters may serialize JSON into the text field.
        if text.startswith("{") or text.startswith("["):
            try:
                parsed = json.loads(text)
            except json.JSONDecodeError:
                parsed = None
            if parsed is not None and parsed is not value:
                return _coerce_mcp_name_tokens(parsed)
        if "," in text or "\n" in text:
            for part in re.split(r"[\n,]", text):
                _append(part)
            return tokens
        _append(text)
        return tokens

    if isinstance(value, list):
        for item in value:
            tokens.extend(_coerce_mcp_name_tokens(item))
        return tokens

    if isinstance(value, dict):
        if any(
            isinstance(value.get(key), str) and value.get(key).strip()
            for key in ("error", "message", "detail")
        ):
            return []
        for key in ("name", "file", "fileName", "path", "project", "projectName"):
            candidate = value.get(key)
            if isinstance(candidate, str):
                _append(candidate)
        for key in ("text", "value", "content"):
            if key in value:
                tokens.extend(_coerce_mcp_name_tokens(value.get(key)))
        return tokens

    return tokens


def _parse_mcp_name_list(result: dict[str, Any]) -> list[str]:
    names: list[str] = []
    for entry in _mcp_content_entries(result.get("content")):
        names.extend(_coerce_mcp_name_tokens(entry))
    # Preserve order while removing duplicates.
    deduped: list[str] = []
    seen: set[str] = set()
    for name in names:
        normalized = name.strip()
        if not normalized or normalized in seen:
            continue
        seen.add(normalized)
        deduped.append(normalized)
    return deduped


def _normalize_retrieval_sources(sources: list[str] | None) -> list[str]:
    if sources:
        requested = [str(item).strip().lower() for item in sources if str(item).strip()]
    else:
        requested = [item.strip().lower() for item in RETRIEVAL_SOURCES_ENV.split(",") if item.strip()]
    normalized: list[str] = []
    seen: set[str] = set()
    for source in requested:
        if source not in RETRIEVAL_SOURCES or source in seen:
            continue
        seen.add(source)
        normalized.append(source)
    if not normalized:
        normalized = [RETRIEVAL_SOURCE_QDRANT]
    return normalized


def _normalize_retrieval_weights(
    custom_weights: dict[str, float] | None,
) -> dict[str, float]:
    weights = dict(DEFAULT_RETRIEVAL_SOURCE_WEIGHTS)
    if not custom_weights:
        return weights
    for source, value in custom_weights.items():
        key = str(source).strip().lower()
        if key not in RETRIEVAL_SOURCES:
            continue
        try:
            score = float(value)
        except (TypeError, ValueError):
            continue
        weights[key] = max(0.0, min(score, 2.0))
    return weights


def _query_terms(query: str, max_terms: int = 8) -> list[str]:
    terms: list[str] = []
    seen: set[str] = set()
    for token in re.findall(r"[A-Za-z0-9_:/.-]{3,}", query.lower()):
        if token in seen:
            continue
        seen.add(token)
        terms.append(token)
        if len(terms) >= max_terms:
            break
    return terms


def _text_match_score(query: str, text: str) -> float:
    query_text = (query or "").strip().lower()
    body = (text or "").lower()
    if not query_text or not body:
        return 0.0
    if query_text in body:
        # Full phrase matches are strongly preferred.
        return 1.0
    terms = _query_terms(query_text, max_terms=10)
    if not terms:
        return 0.0
    hits = sum(1 for term in terms if term in body)
    if hits <= 0:
        return 0.0
    density = min(1.0, len(body) / 4000.0)
    return min(0.95, (hits / max(1, len(terms))) * (0.55 + 0.45 * density))


def _mindsdb_rows(data: dict[str, Any]) -> list[dict[str, Any]]:
    rows = data.get("data")
    if not isinstance(rows, list):
        return []
    if rows and isinstance(rows[0], dict):
        return [row for row in rows if isinstance(row, dict)]
    columns = data.get("column_names")
    if not isinstance(columns, list):
        return []
    result: list[dict[str, Any]] = []
    for row in rows:
        if not isinstance(row, list):
            continue
        values = list(row) + [None] * max(0, len(columns) - len(row))
        result.append({str(columns[idx]): values[idx] for idx in range(len(columns))})
    return result


def _escape_sql_literal(value: str) -> str:
    return value.replace("'", "''")


def _parse_letta_archival_content(text: str) -> dict[str, str]:
    project = ""
    file_name = ""
    topic = ""
    summary = ""
    lines = [line.strip() for line in (text or "").splitlines() if line.strip()]
    if lines:
        header = lines[0]
        match = re.search(r"project=([^\s]+)", header)
        if match:
            project = match.group(1).strip()
        match = re.search(r"file=([^\s]+)", header)
        if match:
            file_name = match.group(1).strip()
        match = re.search(r"topic=([^\s]+)", header)
        if match:
            topic = match.group(1).strip()
    for line in lines[1:]:
        if line.lower().startswith("summary:"):
            summary = line.split(":", 1)[1].strip()
            break
    if not summary:
        summary = (text or "")[:500]
    return {
        "project": project if project and project != "-" else "",
        "file": file_name if file_name and file_name != "-" else "",
        "topic_path": topic if topic and topic != "-" else "",
        "summary": summary,
    }


def _extract_learning_terms(preferences: dict[str, Any] | None) -> tuple[set[str], set[str]]:
    stop_words = {
        "about",
        "after",
        "again",
        "already",
        "avoid",
        "from",
        "into",
        "just",
        "like",
        "more",
        "only",
        "that",
        "this",
        "with",
        "would",
    }

    def _terms(items: Any) -> set[str]:
        extracted: set[str] = set()
        if not isinstance(items, list):
            return extracted
        for value in items:
            if not isinstance(value, str):
                continue
            for token in re.findall(r"[A-Za-z0-9_:/.-]{4,}", value.lower()):
                if token in stop_words:
                    continue
                extracted.add(token)
        return extracted

    if not preferences:
        return set(), set()
    return _terms(preferences.get("positive")), _terms(preferences.get("negative"))


def _result_identity(row: dict[str, Any]) -> str:
    project = str(row.get("project") or "").strip().lower()
    file_name = str(row.get("file") or "").strip().lower()
    if project or file_name:
        return f"{project}:{file_name}"
    summary = str(row.get("summary") or "").strip().lower()
    if not summary:
        return uuid.uuid4().hex
    return hashlib.sha1(summary.encode("utf-8")).hexdigest()


async def call_memory_tool(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    payload = {
        "jsonrpc": "2.0",
        "id": str(uuid.uuid4()),
        "method": "tools/call",
        "params": {"name": name, "arguments": arguments},
    }
    result = await _call_mcp(payload)
    error_message = _mcp_result_error_message(result)
    if error_message:
        raise HTTPException(500, f"{name} failed: {error_message}")
    return result


async def list_projects() -> list[str]:
    try:
        result = await asyncio.wait_for(
            call_memory_tool("list_projects", {}),
            timeout=MEMMCP_LIST_TIMEOUT_SECS,
        )
        projects = _parse_mcp_name_list(result)
        if projects:
            return projects
    except asyncio.TimeoutError:
        logger.warning("list_projects timed out; falling back to qdrant")
    except HTTPException as exc:
        logger.warning("list_projects failed (%s); falling back to qdrant", exc.detail)
    try:
        return await list_qdrant_projects()
    except Exception as exc:  # pragma: no cover - fallback only
        logger.warning("Qdrant fallback project list failed: %s", exc)
        return []


async def list_files(project: str) -> list[str]:
    try:
        result = await asyncio.wait_for(
            call_memory_tool("list_project_files", {"projectName": project}),
            timeout=MEMMCP_LIST_TIMEOUT_SECS,
        )
    except asyncio.TimeoutError:
        logger.warning("list_project_files timed out for %s; falling back to qdrant", project)
        try:
            return await list_qdrant_files(project)
        except Exception as fallback_exc:  # pragma: no cover - fallback only
            logger.warning("Qdrant fallback list failed for %s: %s", project, fallback_exc)
            return []
    except HTTPException as exc:
        detail = str(exc.detail)
        if "resource not found" in detail.lower() or "enoent" in detail.lower():
            logger.info("Project path missing in memory-bank for %s; falling back to qdrant", project)
        else:
            logger.warning("list_project_files failed for %s: %s", project, detail)
        try:
            return await list_qdrant_files(project)
        except Exception as fallback_exc:  # pragma: no cover - fallback only
            logger.warning("Qdrant fallback list failed for %s: %s", project, fallback_exc)
            return []
    filenames = _parse_mcp_name_list(result)
    if filenames:
        return filenames
    try:
        return await list_qdrant_files(project)
    except Exception as exc:  # pragma: no cover - fallback only
        logger.warning("Qdrant fallback list failed for %s: %s", project, exc)
        return filenames


async def list_qdrant_files(project: str, limit: int = 1000) -> list[str]:
    if qdrant_models is None:
        raise RuntimeError("qdrant-client dependency is required for Qdrant operations")
    files: set[str] = set()
    offset: Any = None
    remaining = max(0, limit)
    if remaining == 0:
        return []
    scroll_filter = qdrant_models.Filter(
        must=[
            qdrant_models.FieldCondition(
                key="project",
                match=qdrant_models.MatchValue(value=project),
            )
        ]
    )
    while remaining > 0:
        page_limit = min(remaining, 256)
        points, next_offset = await _qdrant_call(
            "list_files_scroll",
            lambda client, _: client.scroll(
                collection_name=QDRANT_COLLECTION,
                scroll_filter=scroll_filter,
                limit=page_limit,
                offset=offset,
                with_payload=True,
                with_vectors=False,
            ),
        )
        for point in points:
            payload = getattr(point, "payload", None) or {}
            file_name = payload.get("file")
            if isinstance(file_name, str) and file_name:
                files.add(file_name)
        remaining -= len(points)
        offset = next_offset
        if not points or offset is None:
            break
    return sorted(files)


async def list_qdrant_projects(limit: int = 5000) -> list[str]:
    projects: set[str] = set()
    offset: Any = None
    remaining = max(0, limit)
    if remaining == 0:
        return []
    while remaining > 0:
        page_limit = min(remaining, 256)
        points, next_offset = await _qdrant_call(
            "list_projects_scroll",
            lambda client, _: client.scroll(
                collection_name=QDRANT_COLLECTION,
                limit=page_limit,
                offset=offset,
                with_payload=True,
                with_vectors=False,
            ),
        )
        for point in points:
            payload_row = getattr(point, "payload", None) or {}
            project_name = payload_row.get("project")
            if isinstance(project_name, str) and project_name:
                projects.add(project_name)
        remaining -= len(points)
        offset = next_offset
        if not points or offset is None:
            break
    return sorted(projects)


def _memory_not_found_detail(detail: str) -> bool:
    detail_lower = detail.lower()
    return any(
        token in detail_lower
        for token in (
            "notfounderror",
            "resource not found",
            "file not found",
            "no such file",
            "enoent",
        )
    )


def _is_memory_not_found_http_error(exc: HTTPException) -> bool:
    return _memory_not_found_detail(str(exc.detail))


def _build_missing_memory_file_stub(project: str, file_name: str) -> dict[str, Any] | None:
    normalized_file = normalize_memory_path(file_name)
    now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")

    if project == OVERRIDE_PROJECT and normalized_file in OPTIONAL_OVERRIDE_FILENAMES:
        return {
            "kind": "override_smoke_test",
            "symbol": "SMOKE",
            "priority": "LOW",
            "reason": "Auto-created placeholder override file",
            "size_before": 0.0,
            "size_after": 0.0,
            "confidence_before": 0.0,
            "confidence_after": 0.0,
            "override_strength": 0.0,
            "multiplier": 1.0,
            "timestamp": now,
            "generated_by": "memmcp-orchestrator",
            "bootstrap": True,
        }

    if not (normalized_file.startswith("index__") and normalized_file.endswith(".json")):
        return None

    latest_hint = INDEX_FILE_LATEST_HINTS.get(normalized_file)
    if latest_hint is None:
        stem = normalized_file[len("index__") : -len(".json")]
        latest_hint = f"{stem}__latest.json" if stem else ""

    return {
        "kind": "memory_index",
        "project": project,
        "index_file": normalized_file,
        "latest": latest_hint,
        "latest_file": latest_hint,
        "files": [latest_hint] if latest_hint else [],
        "entries": [{"file": latest_hint}] if latest_hint else [],
        "generated_at": now,
        "generated_by": "memmcp-orchestrator",
        "bootstrap": True,
    }


async def _bootstrap_missing_memory_file(project: str, file_name: str) -> str | None:
    if not ORCH_MISSING_FILE_AUTOSTUB:
        return None
    stub_payload = _build_missing_memory_file_stub(project, file_name)
    if stub_payload is None:
        return None
    content = json.dumps(stub_payload, indent=2, sort_keys=True)
    try:
        await call_memory_tool(
            "memory_bank_write",
            {"projectName": project, "fileName": file_name, "content": content},
        )
    except Exception as exc:
        logger.warning(
            "Missing memory file %s/%s could not be auto-bootstrapped: %s",
            project,
            file_name,
            exc,
        )
        return content
    logger.warning("Auto-bootstrapped missing memory file %s/%s", project, file_name)
    return content


async def read_project_file(
    project: str,
    file_name: str,
    *,
    allow_missing: bool = False,
    bootstrap_missing: bool = False,
) -> str:
    try:
        result = await asyncio.wait_for(
            call_memory_tool(
                "memory_bank_read",
                {"projectName": project, "fileName": file_name},
            ),
            timeout=MEMMCP_READ_TIMEOUT_SECS,
        )
    except asyncio.TimeoutError as exc:
        raise HTTPException(504, f"memory_bank_read timeout for {project}/{file_name}") from exc
    except HTTPException as exc:
        if _is_memory_not_found_http_error(exc):
            if bootstrap_missing:
                fallback = await _bootstrap_missing_memory_file(project, file_name)
                if fallback:
                    return fallback
            if allow_missing:
                logger.warning("Missing memory file %s/%s", project, file_name)
                return ""
        raise
    chunks = _extract_mcp_text_chunks(result.get("content"))
    if chunks:
        return "\n".join(chunk for chunk in chunks if chunk)
    return ""


async def ensure_qdrant_collection(vector_size: int, collection_name: str | None = None) -> None:
    collection = collection_name or QDRANT_COLLECTION
    cached_size = qdrant_collection_dim_cache.get(collection)
    if cached_size is not None:
        if cached_size != vector_size:
            raise RuntimeError(
                "Qdrant collection dimension mismatch: "
                f"existing={cached_size}, required={vector_size}. "
                "Drop the collection or adjust the embedding model."
            )
        return
    if qdrant_models is None:
        raise RuntimeError("qdrant-client dependency is required for Qdrant operations")

    def _extract_vector_size(info: Any) -> int | None:
        config = getattr(info, "config", None)
        params = getattr(config, "params", None) if config is not None else None
        vectors = getattr(params, "vectors", None) if params is not None else None
        if vectors is None and isinstance(info, dict):
            vectors = ((((info.get("result") or {}).get("config") or {}).get("params") or {}).get("vectors"))
        size = getattr(vectors, "size", None)
        if size is None and isinstance(vectors, dict):
            size = vectors.get("size")
        if size is None:
            return None
        try:
            return int(size)
        except (TypeError, ValueError):
            return None

    async def _ensure_on_backend(client: AsyncQdrantClient, target: str) -> None:
        try:
            info = await client.get_collection(collection_name=collection)
            current_size = _extract_vector_size(info)
            if current_size and current_size != vector_size:
                raise RuntimeError(
                    "Qdrant collection dimension mismatch: "
                    f"existing={current_size}, required={vector_size}. "
                    "Drop the collection or adjust the embedding model."
                )
            if current_size:
                qdrant_collection_dim_cache[collection] = int(current_size)
            return
        except Exception as exc:
            error_text = str(exc).lower()
            if "404" not in error_text and "not found" not in error_text:
                raise

        await client.create_collection(
            collection_name=collection,
            vectors_config=qdrant_models.VectorParams(
                size=vector_size,
                distance=qdrant_models.Distance.COSINE,
            ),
        )
        qdrant_collection_dim_cache[collection] = vector_size
        logger.info("Created Qdrant collection %s on %s backend with dim=%s", collection, target, vector_size)

    await _qdrant_call("ensure_collection", _ensure_on_backend)


def _qdrant_expected_dim(error_text: str) -> int | None:
    patterns = [
        r"expected dim:\s*(\d+)",
        r"existing=(\d+),\s*required=\d+",
    ]
    for pattern in patterns:
        match = re.search(pattern, error_text, flags=re.IGNORECASE)
        if match:
            try:
                return int(match.group(1))
            except (TypeError, ValueError):
                return None
    return None


async def push_to_qdrant(
    project: str,
    file_name: str,
    content: str,
    topic_path: str | None = None,
    topic_tags: list[str] | None = None,
    collection_name: str | None = None,
) -> None:
    await push_batch_to_qdrant(
        [
            {
                "project": project,
                "file": file_name,
                "content": content,
                "topic_path": topic_path,
                "topic_tags": topic_tags or [],
                "collection_name": collection_name,
            }
        ]
    )


def _qdrant_point_payload(
    project: str,
    file_name: str,
    summary: str,
    vector: list[float],
    topic_path: str | None = None,
    topic_tags: list[str] | None = None,
) -> Any:
    payload_meta: dict[str, Any] = {
        "project": project,
        "file": file_name,
        "summary": summary[:500],
        # Epoch seconds for retention/cold-storage workflows
        "ts": int(datetime.utcnow().timestamp()),
    }
    if topic_path:
        payload_meta["topic_path"] = topic_path
    if topic_tags:
        payload_meta["topic_tags"] = topic_tags
    if qdrant_models is None:
        raise RuntimeError("qdrant-client dependency is required for Qdrant operations")
    return qdrant_models.PointStruct(
        id=str(uuid.uuid4()),
        vector=vector,
        payload=payload_meta,
    )


async def push_batch_to_qdrant(items: list[dict[str, Any]]) -> None:
    if not items:
        return
    grouped: dict[str, list[dict[str, Any]]] = {}
    for item in items:
        collection = str(item.get("collection_name") or QDRANT_COLLECTION).strip() or QDRANT_COLLECTION
        grouped.setdefault(collection, []).append(item)

    for collection, rows in grouped.items():
        vectors: list[list[float]] = []
        for row in rows:
            content = str(row.get("content") or "")
            vectors.append(await embed_text(content))

        vector_dim = len(vectors[0]) if vectors else 0
        try:
            await ensure_qdrant_collection(vector_dim, collection)
        except RuntimeError as exc:
            message = str(exc)
            expected_dim = _qdrant_expected_dim(message)
            if expected_dim and expected_dim > 0 and expected_dim != vector_dim:
                logger.warning(
                    "Qdrant dimension mismatch for writes (expected=%s got=%s); using deterministic fallback embedding",
                    expected_dim,
                    vector_dim,
                )
                vectors = [_cheap_embedding(str(row.get("content") or ""), expected_dim) for row in rows]
                vector_dim = expected_dim
            else:
                raise

        points = [
            _qdrant_point_payload(
                str(row.get("project") or ""),
                str(row.get("file") or ""),
                str(row.get("content") or ""),
                vectors[idx],
                row.get("topic_path"),
                row.get("topic_tags"),
            )
            for idx, row in enumerate(rows)
        ]

        async def _upsert(batch_points: list[Any]) -> Any:
            async with _fanout_rate_limit(qdrant_fanout_rate_limiter):
                return await _qdrant_call(
                    "upsert",
                    lambda client, _: client.upsert(
                        collection_name=collection,
                        points=batch_points,
                        wait=False,
                    ),
                )

        try:
            await _upsert(points)
            continue
        except Exception as exc:
            expected_dim = _qdrant_expected_dim(str(exc))
            if not expected_dim or expected_dim <= 0 or expected_dim == vector_dim:
                raise RuntimeError(f"Qdrant upsert failed: {exc}") from exc

        qdrant_collection_dim_cache[collection] = expected_dim
        fallback_points = [
            _qdrant_point_payload(
                str(row.get("project") or ""),
                str(row.get("file") or ""),
                str(row.get("content") or ""),
                _cheap_embedding(str(row.get("content") or ""), expected_dim),
                row.get("topic_path"),
                row.get("topic_tags"),
            )
            for row in rows
        ]
        try:
            await _upsert(fallback_points)
        except Exception as exc:
            raise RuntimeError(f"Qdrant upsert failed after fallback: {exc}") from exc


def _langfuse_trace_event(project: str, summary: str, payload: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": str(uuid.uuid4()),
        "type": "trace",
        "name": project,
        "metadata": {"summary": summary},
        "input": payload,
    }


async def _ingest_langfuse_events(
    events: list[dict[str, Any]],
    raise_on_error: bool = True,
) -> None:
    if not LANGFUSE_API_KEY or not events:
        return
    headers = {"x-langfuse-api-key": LANGFUSE_API_KEY}
    client = await _get_langfuse_client()
    try:
        resp = await client.post(f"{LANGFUSE_URL}/api/public/ingest", json=events, headers=headers)
    except Exception as exc:
        if raise_on_error:
            raise OrchestratorError(f"Langfuse ingest failed: {exc}") from exc
        logger.warning("Failed to push trace to Langfuse: %s", exc)
        return
    if resp.status_code >= 400:
        message = f"Langfuse ingest failed: status={resp.status_code} body={resp.text[:300]}"
        if raise_on_error:
            raise OrchestratorError(message)
        logger.warning(message)


async def push_to_langfuse(project: str, summary: str, payload: dict[str, Any]) -> None:
    await _ingest_langfuse_events([_langfuse_trace_event(project, summary, payload)], raise_on_error=True)


async def push_batch_to_langfuse(items: list[dict[str, Any]]) -> None:
    if not LANGFUSE_API_KEY or not items:
        return
    events = [
        _langfuse_trace_event(
            str(item.get("project") or ""),
            str(item.get("summary") or ""),
            item.get("payload") if isinstance(item.get("payload"), dict) else {},
        )
        for item in items
    ]
    async with _fanout_rate_limit(langfuse_fanout_rate_limiter):
        await _ingest_langfuse_events(events, raise_on_error=True)


async def trace_to_langfuse(
    operation: str,
    latency_ms: float,
    metadata: dict[str, Any] | None = None,
) -> None:
    """Enhanced Langfuse tracing for orchestrator operations."""
    if not LANGFUSE_API_KEY:
        return
    event = {
        "id": str(uuid.uuid4()),
        "type": "span",
        "name": f"orchestrator.{operation}",
        "startTime": datetime.utcnow().isoformat() + "Z",
        "endTime": (datetime.utcnow()).isoformat() + "Z",
        "metadata": {
            "latency_ms": latency_ms,
            **(metadata or {}),
        },
    }
    await _ingest_langfuse_events([event], raise_on_error=False)


async def summarize_content(content: str, max_length: int = 500) -> str:
    """Summarize long content for embedding. Simple truncation for now."""
    if len(content) <= max_length:
        return content
    # Take first and last portions to preserve context
    mid = max_length // 2
    return content[:mid] + "..." + content[-mid:]


async def search_qdrant(
    query: str,
    limit: int = 10,
    project_filter: str | None = None,
    topic_filter: str | None = None,
) -> list[dict[str, Any]]:
    """Search Qdrant for relevant notes."""
    start_time = asyncio.get_event_loop().time()
    try:
        query_vector = await asyncio.wait_for(
            embed_text(query),
            timeout=max(1.0, QDRANT_EMBED_TIMEOUT_SECS),
        )
    except Exception as exc:
        logger.warning(
            "Qdrant embedding fallback activated (%s); using deterministic vector",
            str(exc)[:200],
        )
        query_vector = _cheap_embedding(query, FALLBACK_EMBED_DIM)

    if qdrant_models is None:
        raise RuntimeError("qdrant-client dependency is required for Qdrant operations")

    def _build_filter() -> Any:
        must: list[Any] = []
        if project_filter:
            must.append(
                qdrant_models.FieldCondition(
                    key="project",
                    match=qdrant_models.MatchValue(value=project_filter),
                )
            )
        if topic_filter:
            must.append(
                qdrant_models.FieldCondition(
                    key="topic_tags",
                    match=qdrant_models.MatchValue(value=topic_filter),
                )
            )
        if not must:
            return None
        return qdrant_models.Filter(must=must)

    query_filter = _build_filter()

    def _extract_points_from_query_response(response: Any) -> list[Any]:
        if isinstance(response, list):
            return response
        points = getattr(response, "points", None)
        if isinstance(points, list):
            return points
        if isinstance(response, dict):
            candidate = response.get("points")
            if isinstance(candidate, list):
                return candidate
        return []

    async def _run_search(vector: list[float]) -> Any:
        async def _execute_search(client: Any, _: str) -> Any:
            if hasattr(client, "search"):
                return await client.search(
                    collection_name=QDRANT_COLLECTION,
                    query_vector=vector,
                    query_filter=query_filter,
                    limit=limit,
                    with_payload=True,
                )
            if hasattr(client, "query_points"):
                response = await client.query_points(
                    collection_name=QDRANT_COLLECTION,
                    query=vector,
                    query_filter=query_filter,
                    limit=limit,
                    with_payload=True,
                )
                return _extract_points_from_query_response(response)
            raise RuntimeError("Qdrant client does not expose search/query_points")

        return await _qdrant_call("search", _execute_search)

    try:
        hits = await _run_search(query_vector)
    except Exception as exc:
        expected_dim = _qdrant_expected_dim(str(exc))
        if not expected_dim or expected_dim <= 0 or expected_dim == len(query_vector):
            raise RuntimeError(f"Qdrant search failed: {exc}") from exc
        fallback_vector = _cheap_embedding(query, expected_dim)
        hits = await _run_search(fallback_vector)

    results = []
    for hit in hits:
        payload = getattr(hit, "payload", None) or {}
        results.append({
            "project": payload.get("project"),
            "file": payload.get("file"),
            "summary": payload.get("summary"),
            "score": getattr(hit, "score", None),
        })

    latency_ms = (asyncio.get_event_loop().time() - start_time) * 1000
    await trace_to_langfuse(
        "search",
        latency_ms,
        {"query_length": len(query), "results": len(results)},
    )
    return results


async def search_memory_bank_lexical(
    query: str,
    limit: int = 10,
    project_filter: str | None = None,
    topic_filter: str | None = None,
) -> list[dict[str, Any]]:
    terms = _query_terms(query)
    if not terms:
        return []
    if project_filter:
        projects = [project_filter]
    else:
        projects = await list_projects()
    projects = projects[: max(1, RETRIEVAL_MEMORY_PROJECT_LIMIT)]
    candidates: list[tuple[float, str, str]] = []
    for project in projects:
        try:
            files = await list_files(project)
        except Exception as exc:
            logger.warning("Memory-bank lexical search skipped %s: %s", project, exc)
            continue
        if topic_filter:
            files = [
                file_name
                for file_name in files
                if derive_topic_path(file_name, None).startswith(topic_filter)
            ]
        files = files[: max(1, RETRIEVAL_MEMORY_FILES_PER_PROJECT)]
        for file_name in files:
            name_score = _text_match_score(query, f"{project}\n{file_name}")
            if name_score <= 0:
                continue
            candidates.append((name_score, project, file_name))
    if not candidates:
        return []
    candidates.sort(key=lambda row: row[0], reverse=True)
    selected = candidates[: max(limit * 4, RETRIEVAL_MEMORY_SCAN_LIMIT)]
    semaphore = asyncio.Semaphore(8)
    rows: list[dict[str, Any]] = []

    async def _inspect(name_score: float, project: str, file_name: str) -> None:
        async with semaphore:
            try:
                content = await read_project_file(project, file_name)
            except Exception:
                return
            if not content:
                return
            summary = await summarize_content(content)
            score = max(name_score, _text_match_score(query, f"{file_name}\n{summary}"))
            if score <= 0:
                return
            rows.append(
                {
                    "project": project,
                    "file": file_name,
                    "summary": summary,
                    "score": score,
                    "source": RETRIEVAL_SOURCE_MEMORY_BANK,
                }
            )

    await asyncio.gather(*[_inspect(score, project, file_name) for score, project, file_name in selected])
    rows.sort(key=lambda row: float(row.get("score") or 0.0), reverse=True)
    return rows[:limit]


async def search_mongo_raw(
    query: str,
    limit: int = 10,
    project_filter: str | None = None,
    topic_filter: str | None = None,
) -> list[dict[str, Any]]:
    if not await init_mongo_client():
        return []
    assert MONGO_CLIENT is not None
    terms = _query_terms(query)
    max_scan = max(limit * 12, RETRIEVAL_MONGO_SCAN_LIMIT)

    def _scan() -> list[dict[str, Any]]:
        coll = MONGO_CLIENT[MONGO_RAW_DB][MONGO_RAW_COLLECTION]
        query_filter: dict[str, Any] = {}
        if project_filter:
            query_filter["project"] = project_filter
        if topic_filter:
            query_filter["topic_path"] = {"$regex": f"^{re.escape(topic_filter)}"}
        projection = {
            "_id": 0,
            "event_id": 1,
            "project": 1,
            "file": 1,
            "summary": 1,
            "content_raw": 1,
            "topic_path": 1,
            "topic_tags": 1,
            "created_at": 1,
            "updated_at": 1,
        }
        docs = list(
            coll.find(query_filter, projection=projection)
            .sort("updated_at", -1)
            .limit(max_scan)
        )
        return docs

    try:
        docs = await asyncio.to_thread(_scan)
    except Exception as exc:
        logger.warning("Mongo retrieval search failed: %s", exc)
        return []
    rows: list[dict[str, Any]] = []
    for doc in docs:
        content = str(doc.get("content_raw") or "")
        summary = str(doc.get("summary") or "")
        snippet = summary or (content[:500] if content else "")
        haystack = "\n".join(
            [
                str(doc.get("project") or ""),
                str(doc.get("file") or ""),
                str(doc.get("topic_path") or ""),
                snippet,
            ]
        )
        score = _text_match_score(query, haystack)
        if score <= 0 and terms:
            continue
        rows.append(
            {
                "project": doc.get("project"),
                "file": doc.get("file"),
                "summary": snippet,
                "score": score,
                "source": RETRIEVAL_SOURCE_MONGO_RAW,
                "event_id": doc.get("event_id"),
                "topic_path": doc.get("topic_path"),
                "created_at": doc.get("created_at"),
            }
        )
    rows.sort(key=lambda row: float(row.get("score") or 0.0), reverse=True)
    return rows[:limit]


async def search_mindsdb_memory(
    query: str,
    limit: int = 10,
    project_filter: str | None = None,
    topic_filter: str | None = None,
) -> list[dict[str, Any]]:
    if not MINDSDB_ENABLED or not MINDSDB_AUTOSYNC:
        return []
    try:
        await ensure_mindsdb_table()
    except Exception as exc:
        logger.warning("MindsDB retrieval bootstrap failed: %s", exc)
        return []
    table_name = mindsdb_target_table or MINDSDB_AUTOSYNC_TABLE
    clauses: list[str] = []
    if project_filter:
        clauses.append(f"project = '{_escape_sql_literal(project_filter)}'")
    if topic_filter:
        escaped_topic = _escape_sql_literal(topic_filter)
        clauses.append(f"(file LIKE '{escaped_topic}/%' OR file LIKE '{escaped_topic}%')")
    terms = _query_terms(query, max_terms=6)
    if terms:
        term_predicates = [
            f"LOWER(summary) LIKE '%{_escape_sql_literal(term.lower())}%'"
            for term in terms
        ]
        term_predicates.extend(
            [
                f"LOWER(file) LIKE '%{_escape_sql_literal(term.lower())}%'"
                for term in terms
            ]
        )
        clauses.append("(" + " OR ".join(term_predicates) + ")")
    where_clause = f"WHERE {' AND '.join(clauses)}" if clauses else ""
    scan_limit = max(limit * 12, RETRIEVAL_MINDSDB_SCAN_LIMIT)
    sql = (
        f"SELECT project, file, summary, created_at "
        f"FROM {MINDSDB_AUTOSYNC_DB}.{table_name} "
        f"{where_clause} ORDER BY created_at DESC LIMIT {scan_limit};"
    )
    try:
        raw = await _mindsdb_execute(sql)
    except Exception as exc:
        logger.warning("MindsDB retrieval query failed: %s", exc)
        return []
    rows: list[dict[str, Any]] = []
    for row in _mindsdb_rows(raw):
        project = str(row.get("project") or "")
        file_name = str(row.get("file") or "")
        summary = str(row.get("summary") or "")
        score = _text_match_score(query, f"{project}\n{file_name}\n{summary}")
        if score <= 0 and terms:
            continue
        rows.append(
            {
                "project": project,
                "file": file_name,
                "summary": summary,
                "score": score,
                "source": RETRIEVAL_SOURCE_MINDSDB,
                "created_at": row.get("created_at"),
            }
        )
    rows.sort(key=lambda row: float(row.get("score") or 0.0), reverse=True)
    return rows[:limit]


async def search_letta_archival(
    query: str,
    limit: int = 10,
    project_filter: str | None = None,
    topic_filter: str | None = None,
) -> list[dict[str, Any]]:
    if not _letta_config_enabled():
        return []
    headers: dict[str, str] = {}
    if LETTA_API_KEY:
        headers["Authorization"] = f"Bearer {LETTA_API_KEY}"
    try:
        agent_id = await _resolve_letta_agent_id(LETTA_AUTO_SESSION_ID, headers)
    except Exception as exc:
        logger.warning("Letta retrieval agent resolution failed: %s", exc)
        return []
    top_k = max(limit, int(limit * max(1.0, RETRIEVAL_LETTA_TOP_K_FACTOR)))
    params: dict[str, Any] = {"query": query, "top_k": top_k}
    if project_filter:
        params["tags"] = [f"project:{project_filter[:120]}"]
    try:
        client = await _get_letta_client()
        resp = await client.get(
            f"{LETTA_URL}/v1/agents/{agent_id}/archival-memory/search",
            params=params,
            headers=headers,
            timeout=LETTA_REQUEST_TIMEOUT_SECS,
        )
        if resp.status_code >= 400:
            raise OrchestratorError(
                f"Letta archival search failed: status={resp.status_code} body={resp.text[:300]}"
            )
        data = resp.json() if resp.content else {}
    except Exception as exc:
        logger.warning("Letta retrieval query failed: %s", exc)
        return []
    rows: list[dict[str, Any]] = []
    for item in data.get("results", []) if isinstance(data, dict) else []:
        if not isinstance(item, dict):
            continue
        content = str(item.get("content") or "")
        parsed = _parse_letta_archival_content(content)
        project = parsed.get("project") or project_filter or ""
        file_name = parsed.get("file") or ""
        topic_path = parsed.get("topic_path") or ""
        summary = parsed.get("summary") or content[:500]
        if project_filter and project and project != project_filter:
            continue
        if topic_filter and topic_path and not topic_path.startswith(topic_filter):
            continue
        score = _text_match_score(query, f"{project}\n{file_name}\n{summary}\n{content}")
        if score <= 0:
            continue
        rows.append(
            {
                "project": project or None,
                "file": file_name or None,
                "summary": summary,
                "score": score,
                "source": RETRIEVAL_SOURCE_LETTA,
                "topic_path": topic_path or None,
                "created_at": item.get("timestamp"),
                "letta_passage_id": item.get("id"),
            }
        )
    rows.sort(key=lambda row: float(row.get("score") or 0.0), reverse=True)
    return rows[:limit]


def _merge_federated_rows(
    results_by_source: dict[str, list[dict[str, Any]]],
    source_weights: dict[str, float],
    positive_terms: set[str],
    negative_terms: set[str],
    learning_enabled: bool,
) -> list[dict[str, Any]]:
    merged: dict[str, dict[str, Any]] = {}
    for source, rows in results_by_source.items():
        source_weight = float(source_weights.get(source, 1.0))
        for row in rows:
            key = _result_identity(row)
            summary = str(row.get("summary") or "")
            file_name = str(row.get("file") or "")
            project = str(row.get("project") or "")
            text_blob = f"{project}\n{file_name}\n{summary}".lower()
            base_score = float(row.get("score") or 0.0)
            positive_hits = sum(1 for term in positive_terms if term in text_blob)
            negative_hits = sum(1 for term in negative_terms if term in text_blob)
            learning_adjustment = 0.0
            if learning_enabled:
                learning_adjustment += positive_hits * RETRIEVAL_LEARNING_POSITIVE_BOOST
                learning_adjustment -= negative_hits * RETRIEVAL_LEARNING_NEGATIVE_PENALTY
            composite_score = (base_score * source_weight) + learning_adjustment
            normalized = dict(row)
            normalized["source"] = source
            normalized["sources"] = [source]
            normalized["base_score"] = round(base_score, 6)
            normalized["source_weight"] = round(source_weight, 4)
            normalized["learning_adjustment"] = round(learning_adjustment, 6)
            normalized["score"] = round(composite_score, 6)
            existing = merged.get(key)
            if existing is None:
                merged[key] = normalized
                continue
            sources = set(existing.get("sources") or [])
            sources.add(source)
            existing["sources"] = sorted(sources)
            if composite_score > float(existing.get("score") or 0.0):
                normalized["sources"] = sorted(sources)
                merged[key] = normalized
    return list(merged.values())


async def federated_search_memory(
    query: str,
    limit: int = 10,
    project_filter: str | None = None,
    topic_filter: str | None = None,
    sources: list[str] | None = None,
    source_weights: dict[str, float] | None = None,
    preferences: dict[str, Any] | None = None,
    rerank_with_learning: bool = True,
) -> tuple[list[dict[str, Any]], dict[str, Any], list[str]]:
    resolved_sources = _normalize_retrieval_sources(sources)
    resolved_weights = _normalize_retrieval_weights(source_weights)
    source_limit = max(limit, min(limit * 6, 200))
    warnings: list[str] = []
    source_timeouts: dict[str, float] = {
        RETRIEVAL_SOURCE_QDRANT: RETRIEVAL_QDRANT_TIMEOUT_SECS,
        RETRIEVAL_SOURCE_MONGO_RAW: RETRIEVAL_MONGO_TIMEOUT_SECS,
        RETRIEVAL_SOURCE_MINDSDB: RETRIEVAL_MINDSDB_TIMEOUT_SECS,
        RETRIEVAL_SOURCE_LETTA: RETRIEVAL_LETTA_TIMEOUT_SECS,
        RETRIEVAL_SOURCE_MEMORY_BANK: RETRIEVAL_MEMORY_TIMEOUT_SECS,
    }

    async def _timed_source(
        source_name: str,
        timeout_secs: float,
        coro: Any,
    ) -> list[dict[str, Any]]:
        try:
            return await asyncio.wait_for(coro, timeout=max(1.0, timeout_secs))
        except asyncio.TimeoutError as exc:
            raise OrchestratorError(
                f"{source_name} retrieval timed out after {timeout_secs:.1f}s"
            ) from exc

    def _build_source_coro(source: str) -> Any:
        if source == RETRIEVAL_SOURCE_QDRANT:
            return search_qdrant(
                query,
                limit=source_limit,
                project_filter=project_filter,
                topic_filter=topic_filter,
            )
        if source == RETRIEVAL_SOURCE_MONGO_RAW:
            return search_mongo_raw(
                query,
                limit=source_limit,
                project_filter=project_filter,
                topic_filter=topic_filter,
            )
        if source == RETRIEVAL_SOURCE_MINDSDB:
            return search_mindsdb_memory(
                query,
                limit=source_limit,
                project_filter=project_filter,
                topic_filter=topic_filter,
            )
        if source == RETRIEVAL_SOURCE_LETTA:
            return search_letta_archival(
                query,
                limit=source_limit,
                project_filter=project_filter,
                topic_filter=topic_filter,
            )
        if source == RETRIEVAL_SOURCE_MEMORY_BANK:
            return search_memory_bank_lexical(
                query,
                limit=source_limit,
                project_filter=project_filter,
                topic_filter=topic_filter,
            )
        raise OrchestratorError(f"Unknown retrieval source: {source}")

    async def _run_source_batch(
        source_batch: list[str],
    ) -> tuple[dict[str, list[dict[str, Any]]], dict[str, str], list[str]]:
        tasks: dict[str, asyncio.Task[list[dict[str, Any]]]] = {}
        for source in source_batch:
            timeout = float(source_timeouts.get(source, RETRIEVAL_QDRANT_TIMEOUT_SECS))
            tasks[source] = asyncio.create_task(
                _timed_source(
                    source,
                    timeout,
                    _build_source_coro(source),
                )
            )
        batch_rows: dict[str, list[dict[str, Any]]] = {}
        batch_errors: dict[str, str] = {}
        batch_warnings: list[str] = []
        if not tasks:
            return batch_rows, batch_errors, batch_warnings
        gathered = await asyncio.gather(*tasks.values(), return_exceptions=True)
        for source, outcome in zip(tasks.keys(), gathered):
            if isinstance(outcome, Exception):
                batch_errors[source] = str(outcome)
                batch_warnings.append(f"{source} retrieval failed: {outcome}")
                continue
            batch_rows[source] = outcome
        return batch_rows, batch_errors, batch_warnings

    results_by_source: dict[str, list[dict[str, Any]]] = {}
    source_errors: dict[str, str] = {}
    positive_terms, negative_terms = _extract_learning_terms(preferences)
    learning_enabled = bool(
        rerank_with_learning
        and RETRIEVAL_ENABLE_LEARNING_RERANK
        and (positive_terms or negative_terms)
    )
    configured_fast = [source for source in DEFAULT_RETRIEVAL_FAST_SOURCES if source in resolved_sources]
    configured_slow = [source for source in DEFAULT_RETRIEVAL_SLOW_SOURCES if source in resolved_sources]
    staged_fast_sources = list(configured_fast)
    for source in resolved_sources:
        if source in staged_fast_sources or source in configured_slow:
            continue
        staged_fast_sources.append(source)
    staged_slow_sources = [source for source in resolved_sources if source in configured_slow]
    slow_sources_skipped: list[str] = []
    staged_fetch_used = bool(
        RETRIEVAL_ENABLE_STAGED_FETCH
        and staged_fast_sources
        and staged_slow_sources
    )

    if staged_fetch_used:
        fast_rows, fast_errors, fast_warnings = await _run_source_batch(staged_fast_sources)
        results_by_source.update(fast_rows)
        source_errors.update(fast_errors)
        warnings.extend(fast_warnings)
        fast_merged = _merge_federated_rows(
            fast_rows,
            resolved_weights,
            positive_terms,
            negative_terms,
            learning_enabled=learning_enabled,
        )
        fast_merged.sort(
            key=lambda row: (
                float(row.get("score") or 0.0),
                float(row.get("base_score") or 0.0),
            ),
            reverse=True,
        )
        min_results_for_skip = max(1, min(limit, RETRIEVAL_SLOW_SOURCE_MIN_RESULTS))
        top_fast_score = float(fast_merged[0].get("score") or 0.0) if fast_merged else 0.0
        enough_fast_volume = len(fast_merged) >= max(min_results_for_skip, limit * 2)
        skip_slow = bool(
            len(fast_merged) >= min_results_for_skip
            and (
                top_fast_score >= RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE
                or enough_fast_volume
            )
        )
        if skip_slow:
            slow_sources_skipped = list(staged_slow_sources)
        else:
            slow_rows, slow_errors, slow_warnings = await _run_source_batch(staged_slow_sources)
            results_by_source.update(slow_rows)
            source_errors.update(slow_errors)
            warnings.extend(slow_warnings)
    else:
        batch_rows, batch_errors, batch_warnings = await _run_source_batch(resolved_sources)
        results_by_source.update(batch_rows)
        source_errors.update(batch_errors)
        warnings.extend(batch_warnings)

    merged = _merge_federated_rows(
        results_by_source,
        resolved_weights,
        positive_terms,
        negative_terms,
        learning_enabled=learning_enabled,
    )
    merged.sort(
        key=lambda row: (
            float(row.get("score") or 0.0),
            float(row.get("base_score") or 0.0),
        ),
        reverse=True,
    )
    return (
        merged[:limit],
        {
            "sources": resolved_sources,
            "source_weights": resolved_weights,
            "source_counts": {
                source: len(results_by_source.get(source, []))
                for source in resolved_sources
            },
            "source_errors": source_errors,
            "staged_fetch": {
                "enabled": staged_fetch_used,
                "fast_sources": staged_fast_sources,
                "slow_sources": staged_slow_sources,
                "slow_sources_skipped": slow_sources_skipped,
                "slow_source_min_results": RETRIEVAL_SLOW_SOURCE_MIN_RESULTS,
                "slow_source_min_top_score": RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE,
            },
            "learning_rerank": {
                "enabled": learning_enabled,
                "positive_terms": len(positive_terms),
                "negative_terms": len(negative_terms),
            },
        },
        warnings,
    )


def _build_letta_archival_text(summary: str, context: dict[str, Any]) -> str:
    project = str(context.get("project") or "")
    file_name = str(context.get("file") or "")
    topic_path = str(context.get("topic_path") or "")
    content = str(context.get("content") or "")
    metadata = {
        key: value
        for key, value in context.items()
        if key not in {"project", "file", "topic_path", "content", "summary"}
    }
    lines: list[str] = []
    if project or file_name or topic_path:
        lines.append(f"project={project or '-'} file={file_name or '-'} topic={topic_path or '-'}")
    if summary:
        summary_text = summary if len(summary) <= 240 else f"{summary[:240]}..."
        lines.append(f"summary: {summary_text}")
    if LETTA_ARCHIVAL_INCLUDE_CONTENT and content:
        snippet = content if len(content) <= 220 else f"{content[:220]}..."
        lines.append(f"content_snippet: {snippet}")
    if metadata:
        metadata_json = json.dumps(metadata, default=str)
        lines.append(f"metadata: {metadata_json[:180]}")
    text = "\n".join(lines).strip()
    if not text:
        text = summary or "memory update"
    if len(text) > LETTA_ARCHIVAL_MAX_CHARS:
        text = text[:LETTA_ARCHIVAL_MAX_CHARS]
    return text


def _normalize_ollama_openai_base(url: str) -> str:
    cleaned = (url or "").strip().rstrip("/")
    if not cleaned:
        return cleaned
    if "ollama:11434" not in cleaned:
        return cleaned
    if cleaned.endswith("/v1"):
        return cleaned
    return f"{cleaned}/v1"


async def _ensure_letta_agent_endpoint_compat(
    agent_id: str,
    agent_payload: dict[str, Any],
    headers: dict[str, str],
) -> None:
    llm_cfg = agent_payload.get("llm_config")
    emb_cfg = agent_payload.get("embedding_config")
    if not isinstance(llm_cfg, dict) or not isinstance(emb_cfg, dict):
        return

    llm_endpoint = str(llm_cfg.get("model_endpoint") or "")
    emb_endpoint = str(emb_cfg.get("embedding_endpoint") or "")
    normalized_llm = _normalize_ollama_openai_base(llm_endpoint)
    normalized_emb = _normalize_ollama_openai_base(emb_endpoint)
    if normalized_llm == llm_endpoint and normalized_emb == emb_endpoint:
        return

    llm_patch = dict(llm_cfg)
    emb_patch = dict(emb_cfg)
    llm_patch["model_endpoint"] = normalized_llm or llm_endpoint
    emb_patch["embedding_endpoint"] = normalized_emb or emb_endpoint
    patch_payload = {"llm_config": llm_patch, "embedding_config": emb_patch}
    client = await _get_letta_client()
    resp = await client.patch(
        f"{LETTA_URL}/v1/agents/{agent_id}",
        json=patch_payload,
        headers=headers,
        timeout=LETTA_REQUEST_TIMEOUT_SECS,
    )
    if resp.status_code >= 400:
        raise OrchestratorError(
            f"Letta agent endpoint patch failed: status={resp.status_code} body={resp.text[:300]}"
        )


async def _resolve_letta_agent_id(session_id: str, headers: dict[str, str]) -> str:
    if session_id.startswith("agent-"):
        return session_id
    cached = LETTA_AGENT_CACHE.get(session_id)
    verified_at = LETTA_AGENT_VERIFIED_AT.get(session_id, 0.0)
    if cached and (time.time() - verified_at) < LETTA_AGENT_VERIFY_INTERVAL_SECS:
        return cached

    async with letta_agent_lock:
        cached = LETTA_AGENT_CACHE.get(session_id)
        client = await _get_letta_client()
        if cached:
            details = await client.get(
                f"{LETTA_URL}/v1/agents/{cached}",
                headers=headers,
                timeout=LETTA_REQUEST_TIMEOUT_SECS,
            )
            if details.status_code >= 400:
                raise OrchestratorError(
                    f"Letta agent fetch failed: status={details.status_code} body={details.text[:300]}"
                )
            detail_payload = details.json() if details.content else {}
            if isinstance(detail_payload, dict):
                await _ensure_letta_agent_endpoint_compat(cached, detail_payload, headers)
            LETTA_AGENT_VERIFIED_AT[session_id] = time.time()
            return cached

        lookup = await client.get(
            f"{LETTA_URL}/v1/agents/",
            params={"name": session_id},
            headers=headers,
            timeout=LETTA_REQUEST_TIMEOUT_SECS,
        )
        if lookup.status_code >= 400:
            raise OrchestratorError(
                f"Letta agent lookup failed: status={lookup.status_code} body={lookup.text[:300]}"
            )
        rows = lookup.json() if lookup.content else []
        if isinstance(rows, list) and rows:
            agent_id = str(rows[0].get("id") or "").strip()
            if agent_id:
                details = await client.get(
                    f"{LETTA_URL}/v1/agents/{agent_id}",
                    headers=headers,
                    timeout=LETTA_REQUEST_TIMEOUT_SECS,
                )
                if details.status_code >= 400:
                    raise OrchestratorError(
                        f"Letta agent fetch failed: status={details.status_code} body={details.text[:300]}"
                    )
                detail_payload = details.json() if details.content else {}
                if isinstance(detail_payload, dict):
                    await _ensure_letta_agent_endpoint_compat(agent_id, detail_payload, headers)
                LETTA_AGENT_CACHE[session_id] = agent_id
                LETTA_AGENT_VERIFIED_AT[session_id] = time.time()
                return agent_id

        create_payload: dict[str, Any] = {"name": session_id}
        if LETTA_AGENT_MODEL:
            create_payload["model"] = LETTA_AGENT_MODEL
        if LETTA_AGENT_EMBEDDING:
            create_payload["embedding"] = LETTA_AGENT_EMBEDDING
        created = await client.post(
            f"{LETTA_URL}/v1/agents/",
            json=create_payload,
            headers=headers,
            timeout=LETTA_REQUEST_TIMEOUT_SECS,
        )
        if created.status_code >= 400:
            # Handle benign races by retrying lookup once.
            if created.status_code not in (409, 422):
                raise OrchestratorError(
                    f"Letta agent create failed: status={created.status_code} body={created.text[:300]}"
                )
            retry_lookup = await client.get(
                f"{LETTA_URL}/v1/agents/",
                params={"name": session_id},
                headers=headers,
                timeout=LETTA_REQUEST_TIMEOUT_SECS,
            )
            if retry_lookup.status_code >= 400:
                raise OrchestratorError(
                    f"Letta agent lookup retry failed: status={retry_lookup.status_code} body={retry_lookup.text[:300]}"
                )
            rows = retry_lookup.json() if retry_lookup.content else []
            if isinstance(rows, list) and rows:
                agent_id = str(rows[0].get("id") or "").strip()
                if agent_id:
                    details = await client.get(
                        f"{LETTA_URL}/v1/agents/{agent_id}",
                        headers=headers,
                        timeout=LETTA_REQUEST_TIMEOUT_SECS,
                    )
                    if details.status_code >= 400:
                        raise OrchestratorError(
                            f"Letta agent fetch failed: status={details.status_code} body={details.text[:300]}"
                        )
                    detail_payload = details.json() if details.content else {}
                    if isinstance(detail_payload, dict):
                        await _ensure_letta_agent_endpoint_compat(agent_id, detail_payload, headers)
                    LETTA_AGENT_CACHE[session_id] = agent_id
                    LETTA_AGENT_VERIFIED_AT[session_id] = time.time()
                    return agent_id
            raise OrchestratorError(
                f"Letta agent create failed: status={created.status_code} body={created.text[:300]}"
            )
        payload = created.json() if created.content else {}
        agent_id = str(payload.get("id") or "").strip() if isinstance(payload, dict) else ""
        if not agent_id:
            raise OrchestratorError("Letta agent create returned no id")
        if isinstance(payload, dict):
            await _ensure_letta_agent_endpoint_compat(agent_id, payload, headers)
        LETTA_AGENT_CACHE[session_id] = agent_id
        LETTA_AGENT_VERIFIED_AT[session_id] = time.time()
        return agent_id


async def push_to_letta(
    session_id: str,
    summary: str,
    context: dict[str, Any],
) -> None:
    """Persist context to Letta archival memory."""
    headers: dict[str, str] = {}
    if LETTA_API_KEY:
        headers["Authorization"] = f"Bearer {LETTA_API_KEY}"
    elif LETTA_REQUIRE_API_KEY:
        raise OrchestratorError("LETTA_REQUIRE_API_KEY is enabled but LETTA_API_KEY is not set")

    async with _fanout_rate_limit(letta_fanout_rate_limiter):
        agent_id = await _resolve_letta_agent_id(session_id, headers)
        tags = ["memmcp"]
        project = str(context.get("project") or "").strip()
        file_name = str(context.get("file") or "").strip()
        if project:
            tags.append(f"project:{project[:120]}")
        if file_name:
            tags.append(f"file:{file_name[:180]}")
        payload = {"text": _build_letta_archival_text(summary, context), "tags": tags[:8]}
        client = await _get_letta_client()

        async def _post_archival(agent_id: str) -> httpx.Response:
            return await client.post(
                f"{LETTA_URL}/v1/agents/{agent_id}/archival-memory",
                json=payload,
                headers=headers,
                timeout=LETTA_REQUEST_TIMEOUT_SECS,
            )

        resp = await client.post(
            f"{LETTA_URL}/v1/agents/{agent_id}/archival-memory",
            json=payload,
            headers=headers,
            timeout=LETTA_REQUEST_TIMEOUT_SECS,
        )
        if resp.status_code >= 500 and not session_id.startswith("agent-"):
            # Force agent revalidation in case endpoint config drifted.
            LETTA_AGENT_VERIFIED_AT[session_id] = 0.0
            agent_id = await _resolve_letta_agent_id(session_id, headers)
            resp = await _post_archival(agent_id)
        if resp.status_code >= 400:
            raise OrchestratorError(f"Letta sync failed: status={resp.status_code} body={resp.text[:300]}")


async def ensure_mindsdb_table() -> None:
    global mindsdb_table_ready, mindsdb_target_table
    if not MINDSDB_ENABLED or not MINDSDB_AUTOSYNC:
        return
    if mindsdb_table_ready:
        return
    async with mindsdb_table_lock:
        if mindsdb_table_ready:
            return
        primary_table = MINDSDB_AUTOSYNC_TABLE
        try:
            await _ensure_mindsdb_table_exists(primary_table)
            mindsdb_target_table = primary_table
            mindsdb_table_ready = True
            return
        except Exception as exc:  # pragma: no cover
            if not _looks_like_mindsdb_table_corruption(str(exc)):
                logger.warning("MindsDB table init error: %s", exc)
                return
    # Fallback activation requires a second lock pass.
    await _switch_mindsdb_to_fallback("primary table appears corrupted", current_table=MINDSDB_AUTOSYNC_TABLE)


async def _switch_mindsdb_to_fallback(reason: str, current_table: str | None = None) -> bool:
    global mindsdb_table_ready, mindsdb_target_table
    async with mindsdb_table_lock:
        seed_table = current_table or mindsdb_target_table or MINDSDB_AUTOSYNC_TABLE
        candidate = _mindsdb_next_fallback_table(seed_table)
        for _ in range(12):
            try:
                await _ensure_mindsdb_table_exists(candidate)
                mindsdb_target_table = candidate
                mindsdb_table_ready = True
                logger.warning(
                    "MindsDB autosync switched to fallback table '%s' after error: %s",
                    candidate,
                    reason[:240],
                )
                return True
            except Exception as exc:
                if _looks_like_mindsdb_table_corruption(str(exc)):
                    candidate = _mindsdb_next_fallback_table(candidate)
                    continue
                logger.warning("MindsDB fallback table activation failed: %s", exc)
                return False
    logger.warning("MindsDB fallback table activation exhausted candidate tables")
    return False


async def _insert_into_mindsdb(item: dict[str, Any]) -> None:
    await ensure_mindsdb_table()
    created_at = item.get("created_at") or _utc_now()
    table_name = mindsdb_target_table or MINDSDB_AUTOSYNC_TABLE
    query = _mindsdb_insert_query(
        item["project"],
        item["file"],
        item.get("summary") or "",
        created_at,
        table_name,
    )
    try:
        await _mindsdb_execute(query)
    except Exception as exc:
        if not _looks_like_mindsdb_table_corruption(str(exc)):
            raise
        if not await _switch_mindsdb_to_fallback(str(exc), current_table=table_name):
            raise
        fallback_table = mindsdb_target_table or _mindsdb_next_fallback_table(table_name)
        fallback_query = _mindsdb_insert_query(
            item["project"],
            item["file"],
            item.get("summary") or "",
            created_at,
            fallback_table,
        )
        await _mindsdb_execute(fallback_query)


async def _insert_many_into_mindsdb(items: list[dict[str, Any]]) -> None:
    if not items:
        return
    await ensure_mindsdb_table()
    table_name = mindsdb_target_table or MINDSDB_AUTOSYNC_TABLE
    query = _mindsdb_insert_many_query(items, table_name)
    try:
        await _mindsdb_execute(query)
    except Exception as exc:
        if not _looks_like_mindsdb_table_corruption(str(exc)):
            raise
        if not await _switch_mindsdb_to_fallback(str(exc), current_table=table_name):
            raise
        fallback_table = mindsdb_target_table or _mindsdb_next_fallback_table(table_name)
        fallback_query = _mindsdb_insert_many_query(items, fallback_table)
        await _mindsdb_execute(fallback_query)


async def push_to_mindsdb(
    project: str,
    file_name: str,
    summary: str,
    allow_fallback_queue: bool = False,
) -> None:
    await push_batch_to_mindsdb(
        [
            {
                "project": project,
                "file": file_name,
                "summary": summary,
                "created_at": _utc_now(),
            }
        ],
        allow_fallback_queue=allow_fallback_queue,
    )


async def push_batch_to_mindsdb(
    items: list[dict[str, Any]],
    allow_fallback_queue: bool = False,
) -> None:
    if not MINDSDB_ENABLED or not MINDSDB_AUTOSYNC:
        return
    if not items:
        return
    normalized_items = [
        {
            "project": str(item.get("project") or ""),
            "file": str(item.get("file") or ""),
            "summary": str(item.get("summary") or ""),
            "created_at": str(item.get("created_at") or _utc_now()),
        }
        for item in items
    ]
    try:
        async with _fanout_rate_limit(mindsdb_fanout_rate_limiter):
            await _insert_many_into_mindsdb(normalized_items)
    except Exception as exc:  # pragma: no cover
        if allow_fallback_queue:
            logger.warning("MindsDB autosync error (queued for retry): %s", exc)
            for row in normalized_items:
                await _enqueue_mindsdb(row)
            return
        raise OrchestratorError(f"MindsDB autosync error: {exc}") from exc


async def get_mindsdb_analytics() -> dict[str, Any] | None:
    """Fetch analytics from MindsDB if available."""
    if not MINDSDB_ENABLED:
        return None
    try:
        client = await _get_mindsdb_client()
        resp = await client.get(f"{MINDSDB_URL}/api/status", timeout=30.0)
        if resp.status_code == 200:
            return {"available": True, "data": resp.json()}
    except Exception as exc:  # pragma: no cover
        logger.debug("MindsDB not available: %s", exc)
    return None


async def compute_trading_analytics_mindsdb() -> dict[str, Any]:
    """Compute trading analytics directly from MindsDB SQL with local fallback."""

    async def _local_fallback(reason: str) -> dict[str, Any]:
        async with trading_history_lock:
            history = list(trading_history)
        if not history:
            return {
                "error": "No trading history available",
                "source": "local_history",
                "warning": reason,
            }
        total_trades = len(history)
        profitable_trades = sum(1 for h in history if _safe_float(h.get("realized_pnl")) > 0)
        win_rate = profitable_trades / total_trades if total_trades > 0 else 0.0
        total_pnl = sum(_safe_float(h.get("realized_pnl")) for h in history)
        avg_position_size = (
            sum(_safe_float(h.get("total_value_usd")) for h in history) / total_trades
            if total_trades > 0
            else 0.0
        )
        cache_freshness_values = [
            _safe_float(h.get("price_cache_freshness"))
            for h in history
            if h.get("price_cache_freshness") is not None
        ]
        avg_freshness = (
            sum(cache_freshness_values) / len(cache_freshness_values)
            if cache_freshness_values
            else 0.0
        )
        return {
            "total_trades": total_trades,
            "win_rate": round(win_rate, 4),
            "total_pnl": round(total_pnl, 2),
            "avg_position_size": round(avg_position_size, 2),
            "avg_cache_freshness": round(avg_freshness, 4),
            "computed_at": datetime.utcnow().isoformat() + "Z",
            "source": "local_history",
            "warning": reason,
        }

    if not MINDSDB_ENABLED or not MINDSDB_TRADING_AUTOSYNC:
        return await _local_fallback("MindsDB trading analytics disabled")

    try:
        await ensure_mindsdb_trading_table()
        sql = (
            "SELECT "
            "COUNT(*) AS total_trades, "
            "SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END) AS profitable_trades, "
            "COALESCE(SUM(realized_pnl), 0) AS total_pnl, "
            "COALESCE(AVG(total_value_usd), 0) AS avg_position_size, "
            "COALESCE(AVG(price_cache_freshness), 0) AS avg_cache_freshness "
            f"FROM {MINDSDB_TRADING_DB}.{MINDSDB_TRADING_TABLE};"
        )
        raw = await _mindsdb_execute(sql)
        rows = _mindsdb_rows(raw)
        if not rows:
            return await _local_fallback("MindsDB returned no rows")
        row = rows[0]
        total_trades = _safe_int(row.get("total_trades"))
        profitable_trades = _safe_int(row.get("profitable_trades"))
        total_pnl = _safe_float(row.get("total_pnl"))
        avg_position_size = _safe_float(row.get("avg_position_size"))
        avg_cache_freshness = _safe_float(row.get("avg_cache_freshness"))
        win_rate = profitable_trades / total_trades if total_trades > 0 else 0.0
        return {
            "total_trades": total_trades,
            "win_rate": round(win_rate, 4),
            "total_pnl": round(total_pnl, 2),
            "avg_position_size": round(avg_position_size, 2),
            "avg_cache_freshness": round(avg_cache_freshness, 4),
            "computed_at": datetime.utcnow().isoformat() + "Z",
            "source": "mindsdb_sql",
            "table": f"{MINDSDB_TRADING_DB}.{MINDSDB_TRADING_TABLE}",
        }
    except Exception as exc:  # pragma: no cover
        logger.error("Failed to compute MindsDB trading analytics: %s", exc)
        return await _local_fallback(str(exc))


@app.get("/projects")
async def get_projects():
    projects = await list_projects()
    semaphore = asyncio.Semaphore(8)

    async def _load(project: str) -> dict[str, Any]:
        async with semaphore:
            try:
                files = await list_files(project)
            except Exception as exc:  # pragma: no cover - defensive endpoint behavior
                logger.warning("Skipping project %s due to list failure: %s", project, exc)
                files = []
            return {"name": project, "files": [{"name": f} for f in files]}

    results = await asyncio.gather(*[_load(project) for project in projects])
    return {"projects": results}


@app.get("/projects/{project}/files")
async def get_files(project: str):
    files = await list_files(project)
    return {"files": files}


@app.get("/memory/recent")
async def get_recent_memory(limit: int = 20, project: str | None = None):
    limit = max(1, min(limit, MEMORY_WRITE_HISTORY_LIMIT))
    async with memory_write_history_lock:
        items = list(memory_write_history)
    if project:
        items = [item for item in items if item.get("project") == project]
    items = items[-limit:]
    return {"items": list(reversed(items))}


@app.post("/agents/tasks")
async def create_agent_task(payload: AgentTaskCreate):
    task_payload = dict(payload.payload or {})
    if payload.risk_level:
        task_payload["risk_level"] = payload.risk_level
    if payload.action_type:
        task_payload["action_type"] = payload.action_type
    if payload.approval_required is not None:
        task_payload["approval_required"] = payload.approval_required
    if payload.approved is not None:
        task_payload["approved"] = payload.approved
    if payload.topic_path:
        task_payload["topic_path"] = payload.topic_path
    task = await create_task_record(
        payload.title,
        payload.project,
        payload.agent,
        payload.priority,
        task_payload or None,
        run_after=payload.run_after,
        max_attempts=payload.max_attempts,
    )
    return {"task": task}


@app.get("/agents/tasks")
async def list_agent_tasks(
    status: str | None = None,
    project: str | None = None,
    agent: str | None = None,
    limit: int = 50,
):
    tasks = await list_task_records(status=status, project=project, agent=agent, limit=limit)
    return {"tasks": tasks}


@app.post("/agents/tasks/next")
async def claim_agent_task(worker: str | None = None):
    task = await claim_next_task(worker)
    if not task:
        return {"task": None}
    return {"task": task}


@app.get("/agents/tasks/{task_id}")
async def get_agent_task(task_id: str):
    task = await get_task_record(task_id)
    if not task:
        raise HTTPException(404, "task not found")
    events = await get_task_events(task_id)
    return {"task": task, "events": events}


@app.post("/agents/tasks/{task_id}/status")
async def update_agent_task_status(task_id: str, payload: AgentTaskStatus):
    task = await update_task_status(
        task_id,
        payload.status,
        payload.message,
        payload.metadata,
    )
    if not task:
        raise HTTPException(404, "task not found")
    return {"task": task}


@app.post("/agents/tasks/{task_id}/approve")
async def approve_agent_task(task_id: str, payload: AgentTaskApproval):
    task = await approve_task_record(task_id, payload.approver, payload.note)
    if not task:
        raise HTTPException(404, "task not found")
    return {"task": task}


@app.post("/agents/tasks/{task_id}/replay")
async def replay_agent_task(task_id: str, payload: AgentTaskReplay):
    task = await replay_task_record(
        task_id,
        actor=payload.actor,
        note=payload.note,
        reset_attempts=payload.reset_attempts,
    )
    if not task:
        raise HTTPException(404, "task not found")
    return {"task": task}


@app.post("/agents/tasks/recover-leases")
async def recover_agent_task_leases(limit: int = 200):
    recovered = await recover_expired_task_leases(limit=max(1, min(limit, 1000)))
    return {"ok": True, "recovered": recovered}


@app.get("/agents/tasks/deadletter")
async def list_deadletter_tasks(project: str | None = None, limit: int = 100):
    tasks = await list_deadletter_task_records(project=project, limit=limit)
    return {"tasks": tasks}


@app.get("/agents/tasks/runtime")
async def agent_task_runtime():
    return {"runtime": await get_task_runtime_snapshot()}


@app.get("/signals/latest")
async def get_latest_signals(limit: int = 50):
    limit = max(1, min(limit, SIGNAL_HISTORY_LIMIT))
    async with signal_cache_lock:
        entries = list(signal_cache)[-limit:]
    return {"signals": list(reversed(entries))}


@app.get("/overrides/latest")
async def get_latest_overrides(limit: int = 40):
    limit = max(1, min(limit, OVERRIDE_HISTORY_LIMIT))
    async with override_cache_lock:
        entries = list(override_cache)[-limit:]
    return {"overrides": list(reversed(entries))}


@app.post("/overrides/refresh")
async def refresh_overrides():
    await _refresh_override_cache()
    async with override_cache_lock:
        count = len(override_cache)
    return {"ok": True, "cached": count}


def _prepare_content_for_storage(content: str) -> tuple[str, str | None]:
    payload = str(content or "")
    mode = SECRETS_STORAGE_MODE
    if mode == "allow":
        return payload, None
    if not _contains_sensitive_value(payload):
        return payload, None
    if mode == "block":
        raise HTTPException(422, "potential secret detected; storage blocked by SECRETS_STORAGE_MODE=block")
    redacted = _redact_sensitive_values(payload)
    if redacted != payload:
        return redacted, "potential secrets were redacted before storage (SECRETS_STORAGE_MODE=redact)"
    return payload, None


@app.post("/memory/write")
async def write_memory(payload: MemoryWrite, request: Request):
    global memory_write_queue_dropped
    start_time = asyncio.get_event_loop().time()
    file_name = normalize_memory_path(payload.fileName)
    if not file_name:
        raise HTTPException(400, "fileName is required")
    content_to_store, storage_policy_warning = _prepare_content_for_storage(payload.content)
    request_id = getattr(request.state, "request_id", None)
    hot_file = is_hot_memory_file(file_name)
    content_hash = memory_content_sha256(content_to_store)
    hot_rollup_mode = hot_file and HOT_MEMORY_ROLLUP_ENABLED

    topic_path = derive_topic_path(file_name, payload.topicPath)
    topic_tags = topic_tags_for_path(topic_path)
    summary = await summarize_content(content_to_store)

    async def _seed_mongo_retry(
        event_id: str,
        raw_event: dict[str, Any],
        local_letta_context: dict[str, Any] | None,
    ) -> None:
        global memory_write_queue_dropped
        outbox_payload = {
            "event_id": event_id,
            "project": payload.projectName,
            "file": file_name,
            "summary": summary,
            "payload": {
                "projectName": payload.projectName,
                "fileName": file_name,
            },
            "topic_path": topic_path,
            "topic_tags": topic_tags,
            "letta_session": LETTA_AUTO_SESSION_ID if _letta_target_enabled() else None,
            "letta_context": local_letta_context or {},
            "qdrant_collection": QDRANT_COLLECTION,
            "raw_event": raw_event,
        }
        await enqueue_fanout_outbox(outbox_payload, [FANOUT_TARGET_MONGO_RAW], force_requeue=True)
        if memory_write_queue.full():
            memory_write_queue_dropped += 1
        else:
            await memory_write_queue.put(event_id)

    if hot_file and await should_skip_unchanged_latest_hash(payload.projectName, file_name, content_hash):
        event_id = uuid.uuid4().hex
        raw_event = build_raw_memory_event(
            event_id=event_id,
            project=payload.projectName,
            file_name=file_name,
            content=content_to_store,
            summary=summary,
            topic_path=topic_path,
            topic_tags=topic_tags,
            request_id=request_id,
        )
        warnings = ["latest snapshot unchanged (hash match); memory-bank and fanout writes suppressed"]
        if storage_policy_warning:
            warnings.append(storage_policy_warning)
        fanout_status: dict[str, str] = {
            "memory_bank": "skipped",
            FANOUT_TARGET_MONGO_RAW: "pending",
            FANOUT_TARGET_QDRANT: "skipped",
            FANOUT_TARGET_MINDSDB: "skipped",
            FANOUT_TARGET_LANGFUSE: "skipped",
            FANOUT_TARGET_LETTA: "skipped",
        }
        mongo_ok, mongo_error = await persist_raw_event_to_mongo(raw_event)
        if mongo_ok:
            fanout_status[FANOUT_TARGET_MONGO_RAW] = "succeeded"
        else:
            fanout_status[FANOUT_TARGET_MONGO_RAW] = "retrying"
            warnings.append(
                f"raw-event Mongo write deferred; queued for retry ({mongo_error or 'unknown error'})"
            )
            await _seed_mongo_retry(event_id, raw_event, None)
        hot_memory_rollup_health["totalSkippedUnchanged"] = int(
            hot_memory_rollup_health.get("totalSkippedUnchanged") or 0
        ) + 1
        _json_log(
            "memory.write.latest_hash_unchanged",
            {
                "request_id": request_id,
                "event_id": event_id,
                "project": payload.projectName,
                "file": file_name,
                "bytes": len(content_to_store),
                "hash": content_hash[:16],
            },
        )
        return {
            "ok": True,
            "event_id": event_id,
            "warnings": warnings,
            "fanout": fanout_status,
            "deduped": True,
            "latest_hash_unchanged": True,
        }

    dedupe_key = build_memory_write_dedupe_key(payload.projectName, file_name, content_to_store)
    if await should_skip_duplicate_memory_write(dedupe_key):
        skipped_event_id = build_event_id(payload.projectName, file_name, content_to_store)
        warnings = [
            f"duplicate memory.write suppressed within {int(max(0.0, MEMORY_WRITE_DEDUP_WINDOW_SECS))} seconds"
        ]
        if storage_policy_warning:
            warnings.append(storage_policy_warning)
        _json_log(
            "memory.write.deduped",
            {
                "request_id": request_id,
                "event_id": skipped_event_id,
                "project": payload.projectName,
                "file": file_name,
                "bytes": len(content_to_store),
            },
        )
        return {
            "ok": True,
            "event_id": skipped_event_id,
            "warnings": warnings,
            "fanout": {
                "memory_bank": "skipped",
                FANOUT_TARGET_MONGO_RAW: "skipped",
                FANOUT_TARGET_QDRANT: "skipped",
                FANOUT_TARGET_MINDSDB: "skipped",
                FANOUT_TARGET_LANGFUSE: "skipped",
                FANOUT_TARGET_LETTA: "skipped",
            },
            "deduped": True,
        }

    event_id = uuid.uuid4().hex
    raw_event = build_raw_memory_event(
        event_id=event_id,
        project=payload.projectName,
        file_name=file_name,
        content=content_to_store,
        summary=summary,
        topic_path=topic_path,
        topic_tags=topic_tags,
        request_id=request_id,
    )
    payload_data = {
        "projectName": payload.projectName,
        "fileName": file_name,
        "content": content_to_store,
    }
    warnings: list[str] = []
    if storage_policy_warning:
        warnings.append(storage_policy_warning)
    fanout_status: dict[str, str] = {
        "memory_bank": "queued_rollup" if hot_rollup_mode else "queued",
        FANOUT_TARGET_MONGO_RAW: "pending",
        FANOUT_TARGET_QDRANT: "deferred_rollup" if hot_rollup_mode else "pending",
        FANOUT_TARGET_MINDSDB: "deferred_rollup" if hot_rollup_mode else "pending",
        FANOUT_TARGET_LANGFUSE: "disabled" if not LANGFUSE_API_KEY else ("deferred_rollup" if hot_rollup_mode else "pending"),
        FANOUT_TARGET_LETTA: "disabled",
    }
    mongo_persisted = False
    mongo_ok, mongo_error = await persist_raw_event_to_mongo(raw_event)
    if mongo_ok:
        fanout_status[FANOUT_TARGET_MONGO_RAW] = "succeeded"
        mongo_persisted = True
    else:
        fanout_status[FANOUT_TARGET_MONGO_RAW] = "retrying"
        warnings.append(
            f"raw-event Mongo write deferred; queued for retry ({mongo_error or 'unknown error'})"
        )
    letta_admitted = False
    letta_context = None
    if LETTA_AUTO_SESSION_ID:
        source_kind = "high_frequency_rollup" if hot_rollup_mode else "memory_write"
        if LETTA_REQUIRE_API_KEY and not LETTA_API_KEY:
            fanout_status[FANOUT_TARGET_LETTA] = "disabled"
            warnings.append("Letta sync disabled because LETTA_REQUIRE_API_KEY is true and LETTA_API_KEY is empty")
        elif not letta_runtime_enabled:
            fanout_status[FANOUT_TARGET_LETTA] = "disabled"
            reason = letta_runtime_disabled_reason or "runtime disabled"
            warnings.append(f"Letta sync disabled due to permanent fanout error ({reason})")
        else:
            admitted, admission_reason, backlog = await _letta_admission_should_enqueue(
                file_name,
                topic_path,
                summary,
                source_kind,
            )
            if admitted:
                letta_admitted = True
                fanout_status[FANOUT_TARGET_LETTA] = "deferred_rollup" if hot_rollup_mode else "pending"
            else:
                fanout_status[FANOUT_TARGET_LETTA] = "throttled_backlog"
                backlog_msg = max(0, int(backlog))
                reason_text = admission_reason or "backlog"
                warnings.append(
                    f"Letta fanout deferred under backlog ({reason_text}; outstanding={backlog_msg})"
                )
                _record_letta_admission_drop(
                    reason=reason_text,
                    backlog=backlog_msg,
                    project=payload.projectName,
                    file_name=file_name,
                    topic_path=topic_path,
                )
        letta_context = {
            "project": payload.projectName,
            "file": file_name,
            "summary": summary,
            "topic_path": topic_path,
            "source_kind": source_kind,
            "content_hash": content_hash,
        }
        if not hot_rollup_mode:
            letta_context["content"] = content_to_store

    if hot_rollup_mode:
        rollup_file = build_hot_memory_rollup_file(file_name)
        await enqueue_hot_memory_rollup(
            {
                "project": payload.projectName,
                "file": file_name,
                "summary": summary,
                "topic_path": topic_path,
                "topic_tags": topic_tags,
                "content_hash": content_hash,
                "content_length": len(content_to_store),
                "letta_session": LETTA_AUTO_SESSION_ID if _letta_target_enabled() and letta_admitted else None,
                "letta_admit": letta_admitted,
                "letta_context": letta_context or {},
                "qdrant_collection": QDRANT_COLLECTION,
            }
        )
        warnings.append(
            f"high-frequency write buffered; rollup flush every {int(max(1.0, HOT_MEMORY_ROLLUP_FLUSH_SECS))}s to {rollup_file}"
        )
        if not mongo_persisted:
            await _seed_mongo_retry(event_id, raw_event, letta_context)
        latency_ms = (asyncio.get_event_loop().time() - start_time) * 1000
        _json_log(
            "memory.write.rollup_buffered",
            {
                "request_id": request_id,
                "event_id": event_id,
                "project": payload.projectName,
                "file": file_name,
                "rollup_file": rollup_file,
                "bytes": len(content_to_store),
                "latency_ms": round(latency_ms, 2),
            },
        )
        fanout_summary = await get_fanout_summary()
        retrying = int(fanout_summary.get("by_status", {}).get("retrying", 0))
        failed = int(fanout_summary.get("by_status", {}).get("failed", 0))
        if not _letta_target_enabled():
            letta_stats = fanout_summary.get("by_target", {}).get(FANOUT_TARGET_LETTA, {})
            retrying = max(0, retrying - int(letta_stats.get("retrying", 0)))
            failed = max(0, failed - int(letta_stats.get("failed", 0)))
        if failed > 0:
            warnings.append(
                f"fanout currently degraded: {failed} failed and {retrying} retrying jobs; durability outside memory-bank is lagging"
            )
        elif retrying > 0:
            warnings.append(
                f"fanout backlog detected: {retrying} jobs retrying; writes may be temporarily memory-bank-only"
            )
        return {
            "ok": True,
            "event_id": event_id,
            "warnings": warnings,
            "fanout": fanout_status,
            "rollup_buffered": True,
            "rollup_file": rollup_file,
        }

    waiter = None
    if not MEMORY_WRITE_ASYNC:
        waiter = asyncio.get_event_loop().create_future()
    await _enqueue_memory_bank_write(
        {
            "project": payload.projectName,
            "file": file_name,
            "payload": payload_data,
            "summary": summary,
            "topic_path": topic_path,
            "topic_tags": topic_tags,
            "content_length": len(content_to_store),
            "letta_session": LETTA_AUTO_SESSION_ID if _letta_target_enabled() and letta_admitted else None,
            "letta_admit": letta_admitted,
            "letta_context": letta_context,
            "waiter": waiter,
            "start_time": start_time,
            "request_id": request_id,
            "event_id": event_id,
            "raw_event": raw_event,
            "mongo_persisted": mongo_persisted,
            "qdrant_collection": QDRANT_COLLECTION,
        }
    )
    if not mongo_persisted:
        # Seed mongo retry immediately so we do not depend on memory-bank completion.
        await _seed_mongo_retry(event_id, raw_event, letta_context)
    if waiter is not None:
        try:
            await waiter
        except Exception as exc:  # pragma: no cover
            raise HTTPException(502, f"Memory write failed: {exc}") from exc
    latency_ms = (asyncio.get_event_loop().time() - start_time) * 1000
    _json_log(
        "memory.write" if waiter is not None else "memory.write.queued",
        {
            "request_id": request_id,
            "event_id": event_id,
            "project": payload.projectName,
            "file": file_name,
            "bytes": len(content_to_store),
            "latency_ms": round(latency_ms, 2),
        },
    )
    fanout_summary = await get_fanout_summary()
    retrying = int(fanout_summary.get("by_status", {}).get("retrying", 0))
    failed = int(fanout_summary.get("by_status", {}).get("failed", 0))
    if not _letta_target_enabled():
        letta_stats = fanout_summary.get("by_target", {}).get(FANOUT_TARGET_LETTA, {})
        retrying = max(0, retrying - int(letta_stats.get("retrying", 0)))
        failed = max(0, failed - int(letta_stats.get("failed", 0)))
    if failed > 0:
        warnings.append(
            f"fanout currently degraded: {failed} failed and {retrying} retrying jobs; durability outside memory-bank is lagging"
        )
    elif retrying > 0:
        warnings.append(
            f"fanout backlog detected: {retrying} jobs retrying; writes may be temporarily memory-bank-only"
        )
    return {
        "ok": True,
        "event_id": event_id,
        "warnings": warnings,
        "fanout": fanout_status,
    }


@app.post("/ingest/trajectory")
async def ingest_trajectory(body: TrajectoryIngest):
    summary = body.summary
    trajectory_content = json.dumps(body.trajectory, indent=2)
    content_to_store, storage_policy_warning = _prepare_content_for_storage(trajectory_content)
    langfuse_payload: dict[str, Any] = body.trajectory
    if storage_policy_warning:
        try:
            parsed = json.loads(content_to_store)
            if isinstance(parsed, dict):
                langfuse_payload = parsed
            else:
                langfuse_payload = {"trajectory": parsed}
        except Exception:
            langfuse_payload = {"trajectory": content_to_store}
    await call_memory_tool(
        "memory_bank_write",
        {
            "projectName": body.project,
            "fileName": f"trajectory-{uuid.uuid4().hex}.json",
            "content": content_to_store,
        },
    )
    await asyncio.gather(
        push_to_qdrant(body.project, "trajectory", summary),
        push_to_langfuse(body.project, summary, langfuse_payload),
    )
    response: dict[str, Any] = {"ok": True}
    if storage_policy_warning:
        response["warnings"] = [storage_policy_warning]
    return response


@app.get("/health")
async def health():
    """Coarse readiness endpoint used by smoke tests and compose checks."""

    return {
        "ok": True,
        "timestamp": datetime.utcnow().isoformat() + "Z",
        "telemetry": {
            "queueDepth": telemetry_state.get("queueDepth", 0),
            "batchSize": telemetry_state.get("batchSize", 0),
        },
        "trading": {
            "updatedAt": trading_metrics_state.get("updatedAt"),
            "openPositions": trading_metrics_state.get("openPositions", 0),
        },
        "sidecar": {
            "healthy": sidecar_health_state.get("healthy"),
            "detail": sidecar_health_state.get("detail", "unknown"),
        },
    }


@app.get("/status/ui")
async def status_ui():
    return HTMLResponse(STATUS_PAGE_HTML)

@app.get("/pilot")
async def pilot_landing():
    return HTMLResponse(build_pilot_html())
@app.get("/status")
async def status():
    services = []
    # memory bank check
    try:
        await list_projects()
        services.append({"name": "memory-bank", "healthy": True, "detail": "MCP reachable"})
    except Exception as exc:  # pragma: no cover - health fallback
        services.append({"name": "memory-bank", "healthy": False, "detail": str(exc)})

    # Langfuse
    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.get(LANGFUSE_URL)
        services.append({
            "name": "langfuse",
            "healthy": resp.status_code == 200,
            "detail": f"status {resp.status_code}",
        })
    except Exception as exc:  # pragma: no cover
        services.append({"name": "langfuse", "healthy": False, "detail": str(exc)})

    # Qdrant
    try:
        await _qdrant_call(
            "status_probe",
            lambda client, target: client.get_collections(),
        )
        backend = _qdrant_operation_targets()[0]
        transport = "grpc-preferred" if QDRANT_GRPC_PREFER else "http-preferred"
        services.append({
            "name": "qdrant",
            "healthy": True,
            "detail": f"{backend} {transport}",
        })
    except Exception as exc:  # pragma: no cover
        services.append({"name": "qdrant", "healthy": False, "detail": str(exc)})

    # MindsDB
    if not MINDSDB_ENABLED:
        services.append({"name": "mindsdb", "healthy": True, "detail": "disabled"})
    else:
        try:
            async with httpx.AsyncClient(timeout=5.0) as client:
                resp = await client.get(f"{MINDSDB_URL}/api/status")
            services.append({
                "name": "mindsdb",
                "healthy": resp.status_code == 200,
                "detail": f"status {resp.status_code}",
            })
        except Exception as exc:  # pragma: no cover
            services.append({"name": "mindsdb", "healthy": False, "detail": str(exc)})

    # Letta
    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.get(LETTA_URL)
        services.append({
            "name": "letta",
            "healthy": resp.status_code < 500,
            "detail": f"status {resp.status_code}",
        })
    except Exception as exc:  # pragma: no cover
        services.append({"name": "letta", "healthy": False, "detail": str(exc)})

    task_runtime: dict[str, Any] | None = None
    with contextlib.suppress(Exception):
        task_runtime = await get_task_runtime_snapshot()

    return {"services": services, "taskRuntime": task_runtime}


@app.post("/telemetry/metrics")
async def ingest_metrics(payload: TelemetryMetrics):
    telemetry_state["updatedAt"] = payload.timestamp.isoformat()
    telemetry_state["queueDepth"] = payload.queueDepth
    telemetry_state["batchSize"] = payload.batchSize
    totals = telemetry_state["totals"]
    totals.update({
        "enqueued": payload.totals.get("enqueued", totals.get("enqueued", 0)),
        "dropped": payload.totals.get("dropped", totals.get("dropped", 0)),
        "batches": payload.totals.get("batches", totals.get("batches", 0)),
        "flushedEvents": payload.totals.get("flushedEvents", totals.get("flushedEvents", 0)),
    })
    return {"ok": True}


@app.get("/telemetry/metrics")
async def get_metrics():
    return telemetry_state


@app.get("/telemetry/memory")
async def get_memory_metrics():
    outbox_summary = await get_fanout_summary()
    task_runtime = await get_task_runtime_snapshot()
    return {
        "updatedAt": datetime.utcnow().isoformat() + "Z",
        "lastWriteAt": memory_write_last_at,
        "lastWriteLatencyMs": memory_write_last_latency_ms,
        "memoryBank": {
            "queueDepth": memory_bank_queue.qsize(),
            "queueMax": MEMORY_BANK_QUEUE_MAX,
            "workers": MEMORY_BANK_WORKERS,
            "processed": memory_bank_queue_processed,
            "dropped": memory_bank_queue_dropped,
        },
        "fanout": {
            "queueDepth": memory_write_queue.qsize(),
            "queueMax": MEMORY_WRITE_QUEUE_MAX,
            "workers": MEMORY_WRITE_WORKERS,
            "mindsdbWorkers": MINDSDB_FANOUT_WORKERS,
            "lettaWorkers": LETTA_FANOUT_WORKERS,
            "outboxBackend": fanout_outbox_backend_active,
            "processed": memory_write_queue_processed,
            "dropped": memory_write_queue_dropped,
            "outbox": outbox_summary,
            "health": outbox_health,
            "letta": {
                "enabled": _letta_target_enabled(),
                "runtimeEnabled": letta_runtime_enabled,
                "disabledReason": letta_runtime_disabled_reason or None,
                "transientErrorStreak": letta_transient_error_streak,
                "transientErrorThreshold": LETTA_TRANSIENT_ERROR_THRESHOLD,
                "admission": {
                    "enabled": LETTA_ADMISSION_ENABLED,
                    "backlogSoftLimit": LETTA_ADMISSION_BACKLOG_SOFT_LIMIT,
                    "backlogHardLimit": LETTA_ADMISSION_BACKLOG_HARD_LIMIT,
                    "lowValueMinSummaryChars": LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS,
                    "dropped": letta_admission_dropped,
                    "lastReason": letta_admission_last_reason or None,
                    "lastBacklog": letta_admission_last_backlog,
                },
            },
            "rateLimitsPerSec": {
                "qdrant": FANOUT_QDRANT_RATE_LIMIT_PER_SEC,
                "mindsdb": FANOUT_MINDSDB_RATE_LIMIT_PER_SEC,
                "letta": FANOUT_LETTA_RATE_LIMIT_PER_SEC,
                "langfuse": FANOUT_LANGFUSE_RATE_LIMIT_PER_SEC,
            },
            "rateLimiterActive": {
                "qdrant": qdrant_fanout_rate_limiter is not None,
                "mindsdb": mindsdb_fanout_rate_limiter is not None,
                "letta": letta_fanout_rate_limiter is not None,
                "langfuse": langfuse_fanout_rate_limiter is not None,
            },
            "batchSizes": {
                "qdrant": FANOUT_QDRANT_BULK_SIZE,
                "mindsdb": FANOUT_MINDSDB_BULK_SIZE,
                "mongo_raw": FANOUT_MONGO_BULK_SIZE,
                "langfuse": FANOUT_LANGFUSE_BULK_SIZE,
                "letta": FANOUT_LETTA_BULK_SIZE,
            },
            "coalescer": {
                "enabled": FANOUT_COALESCE_ENABLED,
                "windowSecs": max(0.0, FANOUT_COALESCE_WINDOW_SECS),
                "targets": FANOUT_COALESCE_TARGETS,
                "coalescedTotal": fanout_coalesce_total,
                "coalescedByTarget": fanout_coalesce_by_target,
            },
            "backpressure": {
                "enabled": FANOUT_BACKPRESSURE_ENABLED,
                "targets": FANOUT_BACKPRESSURE_TARGETS,
                "queueHighWatermark": _normalize_watermark(FANOUT_BACKPRESSURE_QUEUE_HIGH_WATERMARK),
                "maxSleepSecs": max(0.0, FANOUT_BACKPRESSURE_MAX_SLEEP_SECS),
            },
        },
        "rollups": {
            "enabled": HOT_MEMORY_ROLLUP_ENABLED,
            "flushSecs": HOT_MEMORY_ROLLUP_FLUSH_SECS,
            "fileSuffixes": HOT_MEMORY_FILE_SUFFIXES,
            "health": hot_memory_rollup_health,
        },
        "taskRuntime": task_runtime,
        "embeddingCache": {
            "enabled": EMBEDDING_CACHE_ENABLED,
            "maxKeys": EMBEDDING_CACHE_MAX_KEYS,
            "currentKeys": len(embedding_cache),
            "hits": embedding_cache_hits,
            "misses": embedding_cache_misses,
            "evictions": embedding_cache_evictions,
        },
        "retention": {
            "enabled": SINK_RETENTION_ENABLED,
            "intervalSecs": max(60.0, SINK_RETENTION_INTERVAL_SECS),
            "state": sink_retention_state,
        },
    }


@app.get("/telemetry/fanout")
async def get_fanout_metrics():
    summary = await get_fanout_summary()
    return {
        "updatedAt": _utc_now(),
        "outboxBackend": fanout_outbox_backend_active,
        "summary": summary,
        "health": outbox_health,
        "letta": {
            "enabled": _letta_target_enabled(),
            "runtimeEnabled": letta_runtime_enabled,
            "disabledReason": letta_runtime_disabled_reason or None,
            "transientErrorStreak": letta_transient_error_streak,
            "transientErrorThreshold": LETTA_TRANSIENT_ERROR_THRESHOLD,
        },
        "rateLimitsPerSec": {
            "qdrant": FANOUT_QDRANT_RATE_LIMIT_PER_SEC,
            "mindsdb": FANOUT_MINDSDB_RATE_LIMIT_PER_SEC,
            "letta": FANOUT_LETTA_RATE_LIMIT_PER_SEC,
            "langfuse": FANOUT_LANGFUSE_RATE_LIMIT_PER_SEC,
        },
        "rateLimiterActive": {
            "qdrant": qdrant_fanout_rate_limiter is not None,
            "mindsdb": mindsdb_fanout_rate_limiter is not None,
            "letta": letta_fanout_rate_limiter is not None,
            "langfuse": langfuse_fanout_rate_limiter is not None,
        },
        "batchSizes": {
            "qdrant": FANOUT_QDRANT_BULK_SIZE,
            "mindsdb": FANOUT_MINDSDB_BULK_SIZE,
            "mongo_raw": FANOUT_MONGO_BULK_SIZE,
            "langfuse": FANOUT_LANGFUSE_BULK_SIZE,
            "letta": FANOUT_LETTA_BULK_SIZE,
        },
        "coalescer": {
            "enabled": FANOUT_COALESCE_ENABLED,
            "windowSecs": max(0.0, FANOUT_COALESCE_WINDOW_SECS),
            "targets": FANOUT_COALESCE_TARGETS,
            "coalescedTotal": fanout_coalesce_total,
            "coalescedByTarget": fanout_coalesce_by_target,
        },
        "backpressure": {
            "enabled": FANOUT_BACKPRESSURE_ENABLED,
            "targets": FANOUT_BACKPRESSURE_TARGETS,
            "queueHighWatermark": _normalize_watermark(FANOUT_BACKPRESSURE_QUEUE_HIGH_WATERMARK),
            "maxSleepSecs": max(0.0, FANOUT_BACKPRESSURE_MAX_SLEEP_SECS),
        },
        "lettaAdmission": {
            "enabled": LETTA_ADMISSION_ENABLED,
            "backlogSoftLimit": LETTA_ADMISSION_BACKLOG_SOFT_LIMIT,
            "backlogHardLimit": LETTA_ADMISSION_BACKLOG_HARD_LIMIT,
            "lowValueMinSummaryChars": LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS,
            "dropped": letta_admission_dropped,
            "lastReason": letta_admission_last_reason or None,
            "lastBacklog": letta_admission_last_backlog,
        },
    }


@app.post("/telemetry/memory/rollups/flush")
async def trigger_hot_rollup_flush(force: bool = True):
    result = await flush_hot_memory_rollups(force=force)
    return {"ok": True, "result": result}


@app.post("/telemetry/fanout/gc")
async def trigger_fanout_outbox_gc():
    result = await run_fanout_outbox_gc_once()
    return {"ok": True, "result": result}


@app.get("/telemetry/retention")
async def get_retention_metrics():
    return {
        "enabled": SINK_RETENTION_ENABLED,
        "intervalSecs": max(60.0, SINK_RETENTION_INTERVAL_SECS),
        "timeoutSecs": max(5.0, SINK_RETENTION_TIMEOUT_SECS),
        "scanLimit": SINK_RETENTION_SCAN_LIMIT,
        "deleteBatch": SINK_RETENTION_DELETE_BATCH,
        "maxDeletesPerRun": SINK_RETENTION_MAX_DELETES_PER_RUN,
        "thresholdHours": {
            "qdrant": QDRANT_LOW_VALUE_RETENTION_HOURS,
            "mongo_raw": MONGO_RAW_LOW_VALUE_RETENTION_HOURS,
            "letta": LETTA_LOW_VALUE_RETENTION_HOURS,
        },
        "state": sink_retention_state,
    }


@app.post("/telemetry/retention/run")
async def trigger_retention_run():
    result = await run_sink_retention_once()
    return {"ok": not bool(result.get("errors")), "result": result}


@app.get("/telemetry/fanout/deadletters")
async def get_fanout_deadletters(limit: int = 100, target: str | None = None):
    jobs = await list_fanout_jobs(["failed"], limit=limit, target=target)
    return {"items": jobs}


@app.post("/telemetry/trading")
async def ingest_trading(payload: TradingMetrics):
    snapshot = payload.model_dump()
    snapshot["timestamp"] = payload.timestamp.isoformat()
    _apply_trading_snapshot(snapshot)
    async with trading_history_lock:
        trading_history.append(snapshot)
        history_size = len(trading_history)
    await _persist_trading_snapshot(snapshot)
    mindsdb_synced = False
    mindsdb_error = None
    if MINDSDB_ENABLED and MINDSDB_TRADING_AUTOSYNC:
        try:
            await push_trading_snapshot_to_mindsdb(snapshot)
            mindsdb_synced = True
        except Exception as exc:  # pragma: no cover
            mindsdb_error = str(exc)
            logger.warning("Failed to sync trading telemetry to MindsDB: %s", exc)
    return {
        "ok": True,
        "historySize": history_size,
        "mindsdb_synced": mindsdb_synced,
        "warning": mindsdb_error,
    }


@app.get("/telemetry/trading")
async def get_trading_metrics():
    return trading_metrics_state


@app.get("/telemetry/trading/history")
async def get_trading_history(limit: int = 50):
    limit = max(1, min(limit, TRADING_HISTORY_LIMIT))
    async with trading_history_lock:
        items = list(trading_history)[-limit:]
    return {"history": items}


@app.post("/telemetry/strategies")
async def ingest_strategy_metrics(payload: StrategyMetrics):
    snapshot = payload.model_dump()
    snapshot["timestamp"] = payload.timestamp.isoformat()
    _apply_strategy_snapshot(snapshot)
    async with strategy_history_lock:
        strategy_history.append(snapshot)
        history_size = len(strategy_history)
    await _persist_strategy_snapshot(snapshot)
    return {"ok": True, "historySize": history_size}


@app.get("/telemetry/strategies")
async def get_strategy_metrics():
    return strategy_metrics_state


@app.get("/telemetry/strategies/history")
async def get_strategy_history(limit: int = 50):
    limit = max(1, min(limit, STRATEGY_HISTORY_LIMIT))
    async with strategy_history_lock:
        items = list(strategy_history)[-limit:]
    return {"history": items}


@app.post("/telemetry/sidecar-health")
async def ingest_sidecar_health(payload: SidecarHealthPayload):
    entry = {
        "timestamp": payload.timestamp.isoformat(),
        "healthy": payload.healthy,
        "detail": payload.detail,
    };
    async with sidecar_health_lock:
        sidecar_health_state.update(entry)
        sidecar_health_history.append(entry)
        history_len = len(sidecar_health_history)
    return {"ok": True, "historySize": history_len}


@app.get("/telemetry/sidecar-health")
async def get_sidecar_health(limit: int = 40):
    limit = max(1, min(limit, SIDECAR_HEALTH_HISTORY_LIMIT))
    async with sidecar_health_lock:
        history = list(sidecar_health_history)[-limit:]
        state = dict(sidecar_health_state)
    state["history"] = list(reversed(history))
    return state


@app.get("/memory/files/{project}/{file_path:path}")
async def get_memory_file(project: str, file_path: str):
    if not project or not file_path:
        raise HTTPException(400, "project and file path are required")
    file_path = normalize_memory_path(file_path)
    if not file_path:
        raise HTTPException(400, "file path is required")
    try:
        content = await read_project_file(
            project,
            file_path,
            allow_missing=True,
            bootstrap_missing=True,
        )
    except HTTPException:
        raise
    except Exception as exc:  # pragma: no cover
        raise HTTPException(500, f"Failed to read memory file: {exc}") from exc
    if not content:
        raise HTTPException(404, "memory file not found")
    try:
        data = json.loads(content)
    except json.JSONDecodeError:
        media_type = "application/json" if file_path.endswith(".json") else "text/plain"
        return PlainTextResponse(content, media_type=media_type)
    return JSONResponse(data)


class MemorySearch(BaseModel):
    query: str = Field(..., description="Search query")
    limit: int = Field(10, ge=1, le=100)
    project: str | None = Field(None, description="Filter by project")
    fetch_content: bool = Field(False, description="Fetch full file content")
    topic_path: str | None = Field(None, description="Filter by topic path")
    sources: list[str] | None = Field(
        None,
        description="Optional retrieval source override (qdrant,mongo_raw,mindsdb,letta,memory_bank)",
    )
    source_weights: dict[str, float] | None = Field(
        None,
        description="Optional source weighting override for final ranking",
    )
    rerank_with_learning: bool = Field(
        True,
        description="Apply preference-based reranking when learning data is available",
    )
    include_retrieval_debug: bool = Field(
        False,
        description="Include per-source retrieval diagnostics in the response",
    )
    user_id: str | None = Field(None, description="User identifier for preferences")
    include_preferences: bool = Field(True, description="Include preference context")


class TopicsListRequest(BaseModel):
    project: str | None = Field(None, description="Optional project scope")
    prefix: str | None = Field(None, description="Optional topic path prefix filter")
    limit: int = Field(200, ge=1, le=5000)
    min_count: int = Field(1, ge=1)
    depth: int = Field(8, ge=1, le=16)


class MessagingCommandIn(BaseModel):
    channel: str = Field(..., description="Channel adapter name (telegram/slack/openclaw/zeroclaw/ironclaw/custom)")
    source_id: str = Field(..., description="Conversation/user identifier from the channel")
    text: str = Field(..., description="Incoming message text")
    project: str | None = Field(None, description="Optional project override")
    topic_path: str | None = Field(None, description="Optional topic path override")
    user_id: str | None = Field(None, description="Optional user id for preference-aware retrieval")
    require_prefix: bool = Field(True, description="Require command prefix/mention in the message")


_MESSAGING_DIRECTIVE_RE = re.compile(
    r"\b(project|topic|file|limit|status|priority|max_attempts|run_after|agent|task|task_id|model)\s*=\s*([^\s]+)",
    flags=re.IGNORECASE,
)
_STRICT_MESSAGING_CHANNELS = {"openclaw", "zeroclaw", "ironclaw"}
_SENSITIVE_VALUE_PATTERNS = [
    re.compile(r"(?i)\b(?:api[_-]?key|token|secret|password|passwd|private[_-]?key|access[_-]?key)\s*[:=]\s*([^\s,;]{8,})"),
    re.compile(r"(?i)\bbearer\s+[A-Za-z0-9\-._~+/]+=*"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(r"\bsk-[A-Za-z0-9]{20,}\b"),
    re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}\b"),
    re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b"),
]


def _strict_messaging_channel(channel: str) -> bool:
    if not MESSAGING_OPENCLAW_STRICT_SECURITY:
        return False
    normalized = normalize_topic_path(channel)
    return normalized in _STRICT_MESSAGING_CHANNELS


def _contains_sensitive_value(text: str) -> bool:
    payload = str(text or "")
    return any(pattern.search(payload) for pattern in _SENSITIVE_VALUE_PATTERNS)


def _redact_sensitive_values(text: str) -> str:
    scrubbed = str(text or "")
    for pattern in _SENSITIVE_VALUE_PATTERNS:
        scrubbed = pattern.sub("[REDACTED]", scrubbed)
    return scrubbed


def _scrub_sensitive_payload(value: Any) -> Any:
    if isinstance(value, str):
        return _redact_sensitive_values(value)
    if isinstance(value, list):
        return [_scrub_sensitive_payload(item) for item in value]
    if isinstance(value, dict):
        scrubbed: dict[str, Any] = {}
        for key, nested in value.items():
            key_text = str(key)
            if re.search(r"(?i)(api[_-]?key|token|secret|password|private[_-]?key|access[_-]?key)", key_text):
                scrubbed[key_text] = "[REDACTED]"
                continue
            scrubbed[key_text] = _scrub_sensitive_payload(nested)
        return scrubbed
    return value


def _messaging_command_prefixes() -> list[str]:
    configured = MESSAGING_COMMAND_HANDLE.strip().lower()
    prefixes: list[str] = []
    if configured:
        prefixes.append(configured)
        if configured.startswith("@"):
            prefixes.append(configured[1:])
            prefixes.append("/" + configured[1:])
        elif configured.startswith("/"):
            prefixes.append(configured[1:])
            prefixes.append("@" + configured[1:])
        else:
            prefixes.append("@" + configured)
            prefixes.append("/" + configured)
    defaults = ["@contextlattice", "contextlattice", "/contextlattice", "/cl"]
    for prefix in defaults:
        if prefix not in prefixes:
            prefixes.append(prefix)
    if TELEGRAM_BOT_USERNAME:
        bot_suffix = TELEGRAM_BOT_USERNAME.strip().lower()
        if bot_suffix:
            for prefix in list(prefixes):
                if prefix.startswith("/"):
                    telegram_prefix = f"{prefix}@{bot_suffix}"
                    if telegram_prefix not in prefixes:
                        prefixes.append(telegram_prefix)
    return prefixes


def _strip_messaging_prefix(text: str, require_prefix: bool = True) -> str | None:
    raw = str(text or "").strip()
    if not raw:
        return None
    lower = raw.lower()
    for prefix in sorted(_messaging_command_prefixes(), key=len, reverse=True):
        if lower.startswith(prefix):
            remainder = raw[len(prefix) :].lstrip(" \t,:-")
            return remainder
    if require_prefix:
        return None
    return raw


def _parse_messaging_command(text: str, require_prefix: bool = True) -> dict[str, Any] | None:
    body = _strip_messaging_prefix(text, require_prefix=require_prefix)
    if body is None:
        return None
    if not body:
        return {"action": "help", "content": "", "directives": {}}
    parts = body.split(maxsplit=1)
    action_raw = parts[0].strip().lower()
    remainder = parts[1].strip() if len(parts) > 1 else ""
    directives: dict[str, str] = {}
    for match in _MESSAGING_DIRECTIVE_RE.finditer(remainder):
        directives[match.group(1).strip().lower()] = unquote(match.group(2).strip())
    content = _MESSAGING_DIRECTIVE_RE.sub("", remainder).strip(" \t,:-")
    action_aliases = {
        "remember": "remember",
        "write": "remember",
        "save": "remember",
        "recall": "recall",
        "search": "recall",
        "find": "recall",
        "status": "status",
        "health": "status",
        "task": "task",
        "tasks": "task",
        "help": "help",
    }
    action = action_aliases.get(action_raw)
    if not action:
        # Default to recall for plain text when prefix is present.
        return {"action": "recall", "content": body, "directives": {}, "raw": body}
    return {"action": action, "content": content, "directives": directives, "raw": body}


def _truncate_messaging_text(text: str, limit: int = MESSAGING_MAX_RESPONSE_CHARS) -> str:
    rendered = str(text or "").strip()
    if len(rendered) <= limit:
        return rendered
    return rendered[: max(1, limit - 1)].rstrip() + "…"


def _synthetic_request(path: str) -> Request:
    scope: dict[str, Any] = {
        "type": "http",
        "http_version": "1.1",
        "method": "POST",
        "path": path,
        "query_string": b"",
        "headers": [],
    }
    request = Request(scope)
    request.state.request_id = f"msg-{uuid.uuid4().hex}"
    return request


def _messaging_topic_path(default_root: str, source_id: str, explicit: str | None) -> str:
    if explicit:
        normalized = normalize_topic_path(explicit)
        if normalized:
            return normalized
    base = normalize_topic_path(default_root)
    source = normalize_topic_path(source_id)
    if source and source != DEFAULT_TOPIC_ROOT:
        if base == DEFAULT_TOPIC_ROOT:
            return source
        return normalize_topic_path(f"{base}/{source}")
    return base


def _messaging_file_name(topic_path: str, explicit: str | None) -> str:
    if explicit:
        normalized = normalize_memory_path(explicit)
        if normalized:
            return normalized
    folder = topic_path if topic_path != DEFAULT_TOPIC_ROOT else "channels/messaging"
    millis = int(time.time() * 1000)
    return normalize_memory_path(f"{folder}/msg_{millis}.md")


async def _telegram_send_message(chat_id: str, text: str, reply_to: int | None = None) -> None:
    if not TELEGRAM_BOT_TOKEN or not chat_id:
        return
    payload: dict[str, Any] = {
        "chat_id": chat_id,
        "text": _truncate_messaging_text(text, MESSAGING_MAX_RESPONSE_CHARS),
        "disable_web_page_preview": True,
    }
    if reply_to is not None:
        payload["reply_to_message_id"] = reply_to
    url = f"https://api.telegram.org/bot{TELEGRAM_BOT_TOKEN}/sendMessage"
    async with httpx.AsyncClient(timeout=10.0) as client:
        response = await client.post(url, json=payload)
    if response.status_code >= 400:
        logger.warning("Telegram sendMessage failed status=%s body=%s", response.status_code, response.text[:220])


def _verify_slack_signature(raw_body: bytes, request: Request) -> bool:
    if not SLACK_SIGNING_SECRET:
        return True
    provided = request.headers.get("x-slack-signature", "").strip()
    timestamp = request.headers.get("x-slack-request-timestamp", "").strip()
    if not provided or not timestamp:
        return False
    try:
        ts_value = int(timestamp)
    except ValueError:
        return False
    if abs(int(time.time()) - ts_value) > 300:
        return False
    base = f"v0:{timestamp}:{raw_body.decode('utf-8', errors='replace')}".encode("utf-8")
    expected = "v0=" + hmac.new(SLACK_SIGNING_SECRET.encode("utf-8"), base, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, provided)


async def _slack_send_message(channel_id: str, text: str, thread_ts: str | None = None) -> None:
    if not SLACK_BOT_TOKEN or not channel_id:
        return
    payload: dict[str, Any] = {
        "channel": channel_id,
        "text": _truncate_messaging_text(text, MESSAGING_MAX_RESPONSE_CHARS),
        "mrkdwn": False,
    }
    if thread_ts:
        payload["thread_ts"] = thread_ts
    headers = {"Authorization": f"Bearer {SLACK_BOT_TOKEN}", "Content-Type": "application/json; charset=utf-8"}
    async with httpx.AsyncClient(timeout=10.0) as client:
        response = await client.post("https://slack.com/api/chat.postMessage", headers=headers, json=payload)
    if response.status_code >= 400:
        logger.warning("Slack chat.postMessage failed status=%s body=%s", response.status_code, response.text[:220])
        return
    data = response.json() if response.content else {}
    if not data.get("ok"):
        logger.warning("Slack chat.postMessage rejected: %s", data)


def _format_recall_response(
    query: str,
    project: str,
    topic_path: str,
    result: dict[str, Any],
    limit: int = MESSAGING_SEARCH_LIMIT,
) -> str:
    results = result.get("results") or []
    warnings = result.get("warnings") or []
    if not isinstance(results, list) or not results:
        if warnings:
            return (
                f"No matching memory found for '{query}' in project '{project}' topic '{topic_path}'. "
                f"Warnings: {' | '.join(str(item) for item in warnings[:2])}"
            )
        return f"No matching memory found for '{query}' in project '{project}' topic '{topic_path}'."
    lines = [f"Top {min(len(results), limit)} memory hits for '{query}':"]
    for index, item in enumerate(results[:limit], start=1):
        summary = str(item.get("summary") or "").strip().replace("\n", " ")
        if not summary:
            summary = "(no summary)"
        if len(summary) > 180:
            summary = summary[:177].rstrip() + "..."
        file_name = str(item.get("file") or "-")
        source = str(item.get("source") or ",".join(item.get("sources") or []) or "memory")
        lines.append(f"{index}. [{source}] {file_name} -> {summary}")
    if warnings:
        lines.append("Warnings: " + " | ".join(str(item) for item in warnings[:2]))
    return "\n".join(lines)


def _messaging_directive_int(
    directives: dict[str, str],
    key: str,
    default: int,
    *,
    minimum: int,
    maximum: int,
) -> int:
    raw = directives.get(key)
    if raw is None:
        return default
    try:
        parsed = int(str(raw).strip())
    except ValueError:
        return default
    return max(minimum, min(maximum, parsed))


def _build_messaging_task_payload(
    create_spec: str,
    *,
    project: str,
    topic_path: str,
    channel: str,
    source_id: str,
    directives: dict[str, str],
) -> tuple[str, dict[str, Any]]:
    rendered = str(create_spec or "").strip()
    if not rendered:
        raise HTTPException(400, "task create requires a payload or subcommand")

    if rendered.startswith("{"):
        try:
            payload = json.loads(rendered)
        except json.JSONDecodeError as exc:
            raise HTTPException(400, "task create payload JSON is invalid") from exc
        if not isinstance(payload, dict):
            raise HTTPException(400, "task create JSON payload must be an object")
        title = str(payload.get("title") or payload.get("name") or "Queued task").strip() or "Queued task"
        return title, payload

    parts = rendered.split(maxsplit=1)
    sub_action = parts[0].strip().lower()
    remainder = parts[1].strip() if len(parts) > 1 else ""
    remember_aliases = {"remember", "write", "save"}
    recall_aliases = {"recall", "search", "find"}
    messaging_aliases = {"message", "command", "notify"}

    if sub_action in remember_aliases:
        if not remainder:
            raise HTTPException(400, "task create remember requires content")
        file_name = _messaging_file_name(topic_path, directives.get("file"))
        payload = {
            "action": "memory_write",
            "projectName": project,
            "fileName": file_name,
            "content": remainder,
            "topic_path": topic_path,
        }
        return f"Remember: {remainder[:72]}", payload

    if sub_action in recall_aliases:
        if not remainder:
            raise HTTPException(400, "task create recall requires query")
        payload = {
            "action": "memory_search",
            "query": remainder,
            "project": project,
            "topic_path": topic_path,
            "limit": _messaging_directive_int(
                directives,
                "limit",
                MESSAGING_SEARCH_LIMIT,
                minimum=1,
                maximum=50,
            ),
            "include_preferences": True,
        }
        return f"Recall: {remainder[:72]}", payload

    if sub_action in messaging_aliases:
        if not remainder:
            raise HTTPException(400, "task create message requires text")
        payload = {
            "action": "messaging_command",
            "text": remainder,
            "channel": channel,
            "source_id": source_id,
            "project": project,
            "topic_path": topic_path,
            "require_prefix": False,
        }
        return f"Message: {remainder[:72]}", payload

    if sub_action in {"callback", "http"}:
        if not remainder:
            raise HTTPException(400, "task create callback requires URL")
        method = "POST"
        url = remainder
        first_token = remainder.split(maxsplit=1)[0].upper()
        if first_token in {"GET", "POST", "PUT", "PATCH", "DELETE"}:
            method = first_token
            url = remainder.split(maxsplit=1)[1].strip() if len(remainder.split(maxsplit=1)) > 1 else ""
        if not url:
            raise HTTPException(400, "task create callback requires URL")
        payload = {
            "action": "http_callback",
            "url": url,
            "method": method,
            "body": {"source": "messaging_task", "channel": channel, "source_id": source_id},
        }
        return f"Callback: {method} {url[:60]}", payload

    if sub_action in {"provider", "chat"}:
        if not remainder:
            raise HTTPException(400, "task create provider requires prompt text")
        payload = {
            "action": "provider_chat",
            "prompt": remainder,
            "model": directives.get("model") or TASK_PROVIDER_CHAT_MODEL or None,
        }
        return f"Provider chat: {remainder[:60]}", payload

    payload = {
        "action": "messaging_command",
        "text": rendered,
        "channel": channel,
        "source_id": source_id,
        "project": project,
        "topic_path": topic_path,
        "require_prefix": False,
    }
    return f"Task: {rendered[:72]}", payload


async def _execute_messaging_command(
    parsed: dict[str, Any],
    *,
    channel: str,
    source_id: str,
    default_project: str,
    topic_root: str,
    project_override: str | None = None,
    topic_override: str | None = None,
    user_id: str | None = None,
) -> dict[str, Any]:
    action = str(parsed.get("action") or "help").strip().lower()
    directives = parsed.get("directives") if isinstance(parsed.get("directives"), dict) else {}
    content = str(parsed.get("content") or "").strip()
    strict_surface = _strict_messaging_channel(channel)

    def _secure_surface_response(payload: dict[str, Any]) -> dict[str, Any]:
        if not strict_surface:
            return payload
        secured = dict(payload)
        if "response_text" in secured:
            secured["response_text"] = _redact_sensitive_values(str(secured.get("response_text") or ""))
        if "file" in secured:
            secured["file"] = _redact_sensitive_values(str(secured.get("file") or ""))
        if "result" in secured:
            secured["result"] = _scrub_sensitive_payload(secured.get("result"))
        return secured

    project = str(project_override or directives.get("project") or default_project or MESSAGING_DEFAULT_PROJECT).strip()
    if not project:
        project = MESSAGING_DEFAULT_PROJECT
    source_key = normalize_topic_path(source_id) or "default"
    topic_path = _messaging_topic_path(topic_root, source_key, topic_override or directives.get("topic"))

    if action == "remember":
        if not content:
            raise HTTPException(400, "remember command requires content")
        if strict_surface and _contains_sensitive_value(content):
            raise HTTPException(422, "potential secret detected; remember command blocked on secure messaging surface")
        file_name = _messaging_file_name(topic_path, directives.get("file"))
        write_payload = MemoryWrite(
            projectName=project,
            fileName=file_name,
            content=content,
            topicPath=topic_path,
        )
        write_result = await write_memory(write_payload, _synthetic_request("/integrations/messaging/command"))
        warnings = write_result.get("warnings") if isinstance(write_result, dict) else []
        response_text = f"Stored memory in project '{project}' at '{file_name}'."
        if warnings:
            response_text += " Warnings: " + " | ".join(str(item) for item in warnings[:2])
        return _secure_surface_response(
            {
            "ok": True,
            "action": action,
            "project": project,
            "topic_path": topic_path,
            "file": file_name,
            "response_text": _truncate_messaging_text(response_text),
            "result": write_result,
        }
        )

    if action == "recall":
        query = content or str(parsed.get("raw") or "").strip()
        if not query:
            raise HTTPException(400, "recall command requires query text")
        if strict_surface and _contains_sensitive_value(query):
            raise HTTPException(422, "potential secret detected; recall command blocked on secure messaging surface")
        limit = MESSAGING_SEARCH_LIMIT
        if "limit" in directives:
            try:
                limit = max(1, min(10, int(str(directives.get("limit")))))
            except ValueError:
                limit = MESSAGING_SEARCH_LIMIT
        search_result = await search_memory(
            MemorySearch(
                query=query,
                limit=limit,
                project=project,
                topic_path=topic_path,
                fetch_content=False,
                include_retrieval_debug=False,
                user_id=user_id,
                include_preferences=True,
            )
        )
        response_text = _format_recall_response(query, project, topic_path, search_result, limit=limit)
        return _secure_surface_response(
            {
            "ok": True,
            "action": action,
            "project": project,
            "topic_path": topic_path,
            "response_text": _truncate_messaging_text(response_text),
            "result": search_result,
        }
        )

    if action == "task":
        raw = content or str(parsed.get("raw") or "").strip()
        parts = raw.split(maxsplit=1) if raw else []
        subcommand = parts[0].strip().lower() if parts else "help"
        remainder = parts[1].strip() if len(parts) > 1 else ""
        task_id = str(directives.get("task_id") or directives.get("task") or remainder).strip()

        if subcommand in {"create", "enqueue", "new"}:
            if strict_surface and _contains_sensitive_value(remainder):
                raise HTTPException(422, "potential secret detected; task create blocked on secure messaging surface")
            title, task_payload = _build_messaging_task_payload(
                remainder,
                project=project,
                topic_path=topic_path,
                channel=channel,
                source_id=source_id,
                directives=directives,
            )
            priority = _messaging_directive_int(directives, "priority", 0, minimum=-20, maximum=20)
            max_attempts = _messaging_directive_int(
                directives,
                "max_attempts",
                TASK_DEFAULT_MAX_ATTEMPTS,
                minimum=1,
                maximum=50,
            )
            run_after = str(directives.get("run_after") or "").strip() or None
            agent = str(directives.get("agent") or "").strip() or None
            task = await create_task_record(
                title=title,
                project=project,
                agent=agent,
                priority=priority,
                payload=task_payload,
                run_after=run_after,
                max_attempts=max_attempts,
            )
            response_text = (
                f"Queued task {task['id']} ({task.get('action_type') or 'custom'}) "
                f"status={task['status']} max_attempts={task.get('max_attempts')}."
            )
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "create",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"task": task},
                }
            )

        if subcommand in {"status", "get"}:
            if not task_id:
                raise HTTPException(400, "task status requires task id")
            task = await get_task_record(task_id)
            if not task:
                raise HTTPException(404, "task not found")
            events = await get_task_events(task_id)
            response_text = (
                f"Task {task_id}: status={task.get('status')} attempts={task.get('attempts')}/{task.get('max_attempts')} "
                f"project={task.get('project') or '-'} action={task.get('action_type') or '-'}."
            )
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "status",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"task": task, "events": events[:20]},
                }
            )

        if subcommand in {"approve"}:
            if not task_id:
                raise HTTPException(400, "task approve requires task id")
            task = await approve_task_record(task_id, approver=source_id, note="approved via messaging")
            if not task:
                raise HTTPException(404, "task not found")
            response_text = f"Approved task {task_id}; status={task.get('status')}."
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "approve",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"task": task},
                }
            )

        if subcommand in {"replay", "retry", "requeue"}:
            if not task_id:
                raise HTTPException(400, "task replay requires task id")
            task = await replay_task_record(task_id, actor=source_id, note="replayed via messaging", reset_attempts=True)
            if not task:
                raise HTTPException(404, "task not found")
            response_text = f"Replayed task {task_id}; status={task.get('status')} attempts reset."
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "replay",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"task": task},
                }
            )

        if subcommand in {"cancel"}:
            if not task_id:
                raise HTTPException(400, "task cancel requires task id")
            task = await update_task_status(
                task_id,
                "canceled",
                "Task canceled via messaging command",
                {"actor": source_id, "channel": channel},
            )
            if not task:
                raise HTTPException(404, "task not found")
            response_text = f"Canceled task {task_id}."
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "cancel",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"task": task},
                }
            )

        if subcommand in {"list", "ls"}:
            status_filter = str(directives.get("status") or "").strip().lower() or None
            agent_filter = str(directives.get("agent") or "").strip() or None
            limit = _messaging_directive_int(directives, "limit", 8, minimum=1, maximum=25)
            tasks = await list_task_records(
                status=status_filter,
                project=project,
                agent=agent_filter,
                limit=limit,
            )
            lines = [f"Tasks ({len(tasks)}):"]
            for item in tasks[:limit]:
                lines.append(
                    f"- {item.get('id')} | {item.get('status')} | p={item.get('priority')} | "
                    f"{item.get('action_type') or '-'} | {item.get('title') or '-'}"
                )
            response_text = "\n".join(lines) if tasks else "No tasks found."
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "list",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"tasks": tasks},
                }
            )

        if subcommand in {"deadletter", "dlq", "failed"}:
            limit = _messaging_directive_int(directives, "limit", 8, minimum=1, maximum=25)
            tasks = await list_deadletter_task_records(project=project, limit=limit)
            lines = [f"Deadletter ({len(tasks)}):"]
            for item in tasks[:limit]:
                lines.append(
                    f"- {item.get('id')} | {item.get('status')} | attempts={item.get('attempts')}/{item.get('max_attempts')} | "
                    f"error={str(item.get('last_error') or '-')[:70]}"
                )
            response_text = "\n".join(lines) if tasks else "No deadletter tasks."
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "deadletter",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"tasks": tasks},
                }
            )

        if subcommand in {"runtime", "health"}:
            runtime = await get_task_runtime_snapshot()
            response_text = (
                f"Task runtime: ready={runtime.get('queueReady')} running={runtime.get('running')} "
                f"deadletter={runtime.get('deadletter')} workers={runtime.get('workersRunning')}/"
                f"{runtime.get('workersConfigured')}."
            )
            return _secure_surface_response(
                {
                    "ok": True,
                    "action": "task",
                    "subcommand": "runtime",
                    "project": project,
                    "topic_path": topic_path,
                    "response_text": _truncate_messaging_text(response_text),
                    "result": {"runtime": runtime},
                }
            )

        task_help = (
            "Task commands: task create <remember|recall|message|callback|provider ...>; "
            "task status <id>; task list [status=queued limit=8]; task approve <id>; "
            "task replay <id>; task deadletter; task runtime."
        )
        return _secure_surface_response(
            {
                "ok": True,
                "action": "task",
                "subcommand": "help",
                "project": project,
                "topic_path": topic_path,
                "response_text": task_help,
                "result": {"help": True},
            }
        )

    if action == "status":
        status_result = await status()
        services = status_result.get("services") if isinstance(status_result, dict) else []
        healthy = sum(1 for svc in services if isinstance(svc, dict) and svc.get("healthy"))
        total = len(services) if isinstance(services, list) else 0
        response_text = f"Context Lattice status: {healthy}/{total} services healthy."
        return _secure_surface_response(
            {
            "ok": True,
            "action": action,
            "project": project,
            "topic_path": topic_path,
            "response_text": response_text,
            "result": status_result,
        }
        )

    help_handle = MESSAGING_COMMAND_HANDLE or "@ContextLattice"
    help_text = (
        f"Commands: {help_handle} remember <text>; "
        f"{help_handle} recall <query>; {help_handle} status; {help_handle} task <subcommand>. "
        "Optional directives: project=<name> topic=<path> limit=<n>."
    )
    return _secure_surface_response(
        {
        "ok": True,
        "action": "help",
        "project": project,
        "topic_path": topic_path,
        "response_text": help_text,
        "result": {"help": True},
    }
    )


@app.post("/memory/search")
async def search_memory(payload: MemorySearch):
    """Federated retrieval across memory services with preference-aware reranking."""
    topic_filter = None
    if payload.topic_path:
        topic_filter = normalize_topic_path(payload.topic_path)

    preferences = None
    pre_warnings: list[str] = []
    if LEARNING_LOOP_ENABLED and payload.include_preferences:
        try:
            feedback_records = await list_feedback_records(
                payload.project,
                payload.user_id,
                None,
                PREFERENCE_MAX_ENTRIES,
            )
            preferences = build_preference_context(feedback_records)
        except Exception as exc:
            logger.warning(
                "Preference context unavailable; continuing without rerank context: %s",
                exc,
            )
            pre_warnings.append("Preference context unavailable; results were not learning-reranked.")

    results, retrieval_debug, warnings = await federated_search_memory(
        payload.query,
        limit=payload.limit,
        project_filter=payload.project,
        topic_filter=topic_filter,
        sources=payload.sources,
        source_weights=payload.source_weights,
        preferences=preferences,
        rerank_with_learning=payload.rerank_with_learning,
    )
    if pre_warnings:
        warnings = pre_warnings + warnings

    if payload.fetch_content:
        # Fetch full content for each result
        for result in results:
            project = result.get("project")
            file_name = result.get("file")
            if not isinstance(project, str) or not isinstance(file_name, str) or not project or not file_name:
                result["content"] = None
                continue
            try:
                content = await read_project_file(
                    project,
                    file_name,
                )
                result["content"] = content
            except Exception as exc:
                logger.warning(
                    "Failed to fetch content for %s/%s: %s",
                    project,
                    file_name,
                    exc,
                )
                result["content"] = None

    response: dict[str, Any] = {
        "results": results,
        "preferences": preferences,
        "learning_enabled": LEARNING_LOOP_ENABLED,
        "warnings": warnings,
    }
    if payload.include_retrieval_debug:
        response["retrieval"] = retrieval_debug
    return response


@app.post("/integrations/messaging/command")
async def messaging_command(payload: MessagingCommandIn):
    if not MESSAGING_INTEGRATIONS_ENABLED:
        raise HTTPException(503, "messaging integrations disabled")
    parsed = _parse_messaging_command(payload.text, require_prefix=payload.require_prefix)
    if parsed is None:
        return {"ok": False, "ignored": True, "reason": "no_command_prefix"}
    channel = normalize_topic_path(payload.channel or "custom")
    source_id = normalize_topic_path(payload.source_id) or "source"
    topic_root = normalize_topic_path(payload.topic_path) if payload.topic_path else normalize_topic_path(f"channels/{channel}")
    channel_default_project = {
        "telegram": TELEGRAM_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
        "slack": SLACK_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
        "openclaw": OPENCLAW_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
        "zeroclaw": OPENCLAW_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
        "ironclaw": IRONCLAW_DEFAULT_PROJECT or OPENCLAW_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
    }.get(channel, MESSAGING_DEFAULT_PROJECT)
    default_project = payload.project or channel_default_project
    result = await _execute_messaging_command(
        parsed,
        channel=channel,
        source_id=source_id,
        default_project=default_project,
        topic_root=topic_root,
        project_override=payload.project,
        topic_override=payload.topic_path,
        user_id=payload.user_id,
    )
    return result


@app.post("/integrations/messaging/openclaw")
async def messaging_openclaw(payload: MessagingCommandIn):
    bridged = MessagingCommandIn(
        channel=payload.channel or "openclaw",
        source_id=payload.source_id,
        text=payload.text,
        project=payload.project or OPENCLAW_DEFAULT_PROJECT,
        topic_path=payload.topic_path,
        user_id=payload.user_id,
        require_prefix=payload.require_prefix,
    )
    return await messaging_command(bridged)


@app.post("/integrations/messaging/ironclaw")
async def messaging_ironclaw(payload: MessagingCommandIn):
    if not IRONCLAW_INTEGRATION_ENABLED:
        raise HTTPException(503, "ironclaw integration disabled")
    bridged = MessagingCommandIn(
        channel=payload.channel or "ironclaw",
        source_id=payload.source_id,
        text=payload.text,
        project=payload.project or IRONCLAW_DEFAULT_PROJECT or OPENCLAW_DEFAULT_PROJECT,
        topic_path=payload.topic_path,
        user_id=payload.user_id,
        require_prefix=payload.require_prefix,
    )
    return await messaging_command(bridged)


@app.post("/integrations/telegram/webhook")
async def telegram_webhook(update: dict[str, Any], request: Request):
    if not MESSAGING_INTEGRATIONS_ENABLED:
        return {"ok": False, "ignored": True, "reason": "messaging_disabled"}
    if TELEGRAM_WEBHOOK_SECRET:
        received = request.headers.get("x-telegram-bot-api-secret-token", "").strip()
        if received != TELEGRAM_WEBHOOK_SECRET:
            raise HTTPException(401, "invalid telegram webhook secret")
    message = update.get("message") or update.get("edited_message") or {}
    if not isinstance(message, dict):
        return {"ok": True, "ignored": True, "reason": "no_message"}
    text = str(message.get("text") or "").strip()
    if not text:
        return {"ok": True, "ignored": True, "reason": "non_text"}
    chat = message.get("chat") if isinstance(message.get("chat"), dict) else {}
    chat_id = str(chat.get("id") or "").strip()
    if not chat_id:
        return {"ok": True, "ignored": True, "reason": "missing_chat_id"}
    parsed = _parse_messaging_command(text, require_prefix=True)
    if parsed is None:
        return {"ok": True, "ignored": True, "reason": "no_command_prefix"}
    source_id = normalize_topic_path(f"chat-{chat_id}") or "chat"
    result = await _execute_messaging_command(
        parsed,
        channel="telegram",
        source_id=source_id,
        default_project=TELEGRAM_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
        topic_root=normalize_topic_path(TELEGRAM_TOPIC_ROOT),
        user_id=str(message.get("from", {}).get("id") or ""),
    )
    await _telegram_send_message(chat_id, result.get("response_text", ""), reply_to=message.get("message_id"))
    return {
        "ok": True,
        "action": result.get("action"),
        "project": result.get("project"),
        "topic_path": result.get("topic_path"),
    }


async def _process_slack_event(event: dict[str, Any]) -> None:
    text = str(event.get("text") or "").strip()
    if not text:
        return
    mention_present = bool(re.search(r"<@[^>]+>", text))
    normalized_text = re.sub(r"<@[^>]+>", "", text).strip()
    parsed = _parse_messaging_command(normalized_text, require_prefix=not mention_present)
    if parsed is None:
        return
    channel_id = str(event.get("channel") or "").strip()
    if not channel_id:
        return
    user_id = str(event.get("user") or "").strip()
    source_id = normalize_topic_path(f"channel-{channel_id}") or "channel"
    result = await _execute_messaging_command(
        parsed,
        channel="slack",
        source_id=source_id,
        default_project=SLACK_DEFAULT_PROJECT or MESSAGING_DEFAULT_PROJECT,
        topic_root=normalize_topic_path(SLACK_TOPIC_ROOT),
        user_id=user_id,
    )
    await _slack_send_message(channel_id, result.get("response_text", ""), thread_ts=event.get("thread_ts") or event.get("ts"))


@app.post("/integrations/slack/events")
async def slack_events(request: Request):
    if not MESSAGING_INTEGRATIONS_ENABLED:
        return {"ok": False, "ignored": True, "reason": "messaging_disabled"}
    raw_body = await request.body()
    if not _verify_slack_signature(raw_body, request):
        raise HTTPException(401, "invalid slack signature")
    try:
        payload = json.loads(raw_body.decode("utf-8"))
    except json.JSONDecodeError as exc:
        raise HTTPException(400, "invalid slack payload") from exc
    if payload.get("type") == "url_verification":
        return {"challenge": payload.get("challenge")}
    if payload.get("type") != "event_callback":
        return {"ok": True, "ignored": True, "reason": "unsupported_event_type"}
    event = payload.get("event") if isinstance(payload.get("event"), dict) else {}
    if event.get("bot_id"):
        return {"ok": True, "ignored": True, "reason": "bot_message"}
    asyncio.create_task(_process_slack_event(event))
    return {"ok": True}


@app.get("/memory/topics")
async def get_topic_tree(project: str | None = None, depth: int = 4):
    depth = max(1, min(depth, 8))
    async with topic_tree_lock:
        if project:
            node = topic_tree.get(project, {"count": 0, "children": {}})
            return {"project": project, "topics": _truncate_topic_tree(node, depth)}
        snapshot = {
            name: _truncate_topic_tree(node, depth)
            for name, node in topic_tree.items()
        }
    return {"topics": snapshot}


@app.get("/memory/topics/list")
async def list_topics(
    project: str | None = None,
    prefix: str | None = None,
    limit: int = 200,
    min_count: int = 1,
    depth: int = 8,
):
    async with topic_tree_lock:
        return _list_topics_snapshot(
            project=project,
            prefix=prefix,
            limit=limit,
            min_count=min_count,
            depth=depth,
        )


@app.post("/tools/topics_list")
async def tool_topics_list(payload: TopicsListRequest):
    async with topic_tree_lock:
        return _list_topics_snapshot(
            project=payload.project,
            prefix=payload.prefix,
            limit=payload.limit,
            min_count=payload.min_count,
            depth=payload.depth,
        )


@app.post("/feedback")
async def create_feedback(payload: FeedbackCreate):
    topic_path = normalize_topic_path(payload.topic_path) if payload.topic_path else None
    record = await create_feedback_record(
        payload.project,
        payload.user_id,
        payload.source,
        payload.task_id,
        payload.rating,
        payload.sentiment,
        payload.tags,
        payload.content,
        topic_path,
        payload.metadata,
    )
    if LEARNING_LOOP_ENABLED and record.get("project") and record.get("content"):
        file_name = f"feedback/{record['id']}.md"
        content = json.dumps(record, indent=2)
        await call_memory_tool(
            "memory_bank_write",
            {
                "projectName": record["project"],
                "fileName": file_name,
                "content": content,
            },
        )
    return {"feedback": record}


@app.get("/feedback")
async def list_feedback(
    project: str | None = None,
    user_id: str | None = None,
    source: str | None = None,
    limit: int = 50,
):
    records = await list_feedback_records(project, user_id, source, limit)
    return {"feedback": records}


@app.get("/preferences")
async def get_preferences(
    project: str | None = None,
    user_id: str | None = None,
    limit: int = PREFERENCE_MAX_ENTRIES,
):
    if not LEARNING_LOOP_ENABLED:
        return {"enabled": False, "preferences": None}
    records = await list_feedback_records(project, user_id, None, limit)
    return {"enabled": True, "preferences": build_preference_context(records)}


@app.get("/analytics/mindsdb")
async def get_mindsdb_status():
    """Check MindsDB availability and status."""
    if not MINDSDB_ENABLED:
        return {"available": False, "message": "MindsDB disabled"}
    data = await get_mindsdb_analytics()
    if data:
        return data
    return {"available": False, "message": "MindsDB not reachable"}


@app.get("/analytics/trading")
async def get_trading_analytics():
    """Compute trading analytics and optionally store as briefing."""
    analytics = await compute_trading_analytics_mindsdb()
    return analytics


@app.post("/analytics/trading/briefing")
async def save_trading_briefing(store_in_memory: bool = True):
    """Compute trading analytics and save as a briefing in memMCP."""
    analytics = await compute_trading_analytics_mindsdb()
    
    if store_in_memory and "error" not in analytics:
        timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
        file_name = f"briefings/trading_analytics_{timestamp}.json"
        content = json.dumps(analytics, indent=2)
        
        await call_memory_tool(
            "memory_bank_write",
            {
                "projectName": "algotraderv2_rust",
                "fileName": file_name,
                "content": content,
            },
        )
        
        # Also index in Qdrant for searchability
        summary = f"Trading analytics: {analytics['total_trades']} trades, {analytics['win_rate']:.2%} win rate, ${analytics['total_pnl']:.2f} PnL"
        await push_to_qdrant("algotraderv2_rust", file_name, summary)
        
        return {"ok": True, "analytics": analytics, "stored": file_name}
    
    return {"ok": True, "analytics": analytics, "stored": False}


class LettaSession(BaseModel):
    session_id: str = Field(..., description="Session identifier")
    summary: str = Field(..., description="Session summary")
    context: dict[str, Any] = Field(..., description="Session context")


class FanoutRehydrateRequest(BaseModel):
    project: str | None = Field(None, description="Optional single project to rehydrate")
    limit: int = Field(500, ge=1, le=5000)
    targets: list[str] | None = Field(None, description="Subset of fanout targets")
    qdrant_collection: str | None = Field(None, description="Optional target collection override")
    force_requeue: bool = Field(False, description="Reset already-succeeded jobs back to pending")


class QdrantBackfillRequest(BaseModel):
    project: str | None = Field(None, description="Optional single project scope")
    limit: int = Field(5000, ge=1, le=50000)
    targets: list[str] | None = Field(
        None,
        description="Subset of fanout targets (defaults to mongo_raw,mindsdb,letta)",
    )
    qdrant_collection: str | None = Field(None, description="Optional source collection override")
    force_requeue: bool = Field(False, description="Reset already-succeeded jobs back to pending")
    include_qdrant_target: bool = Field(
        False,
        description="Also enqueue qdrant target (normally skipped for qdrant-source backfill)",
    )


class MongoRawBackfillRequest(BaseModel):
    project: str | None = Field(None, description="Optional single project scope")
    limit: int = Field(50000, ge=1, le=200000)
    targets: list[str] | None = Field(
        None,
        description="Subset of fanout targets (defaults to qdrant,mindsdb,letta)",
    )
    qdrant_collection: str | None = Field(None, description="Optional qdrant target collection override")
    force_requeue: bool = Field(False, description="Reset already-succeeded jobs back to pending")
    max_pending_jobs: int = Field(
        6000,
        ge=0,
        le=200000,
        description="Stop enqueueing when pending+retrying+running fanout jobs reach this threshold (0 disables)",
    )
    check_interval: int = Field(
        200,
        ge=1,
        le=5000,
        description="How often to evaluate queue pressure while scanning mongo rows",
    )


@app.post("/memory/letta/session")
async def save_letta_session(payload: LettaSession):
    """Save session state to Letta for working memory."""
    try:
        await push_to_letta(
            payload.session_id,
            payload.summary,
            payload.context,
        )
    except Exception as exc:
        raise HTTPException(502, f"Letta write failed: {exc}") from exc
    return {"ok": True, "session_id": payload.session_id}


@app.post("/maintenance/fanout/rehydrate")
async def rehydrate_fanout(payload: FanoutRehydrateRequest):
    global memory_write_queue_dropped
    requested_targets = [t.lower() for t in (payload.targets or [])]
    if requested_targets:
        targets = [t for t in requested_targets if t in FANOUT_TARGETS]
    else:
        targets = [
            FANOUT_TARGET_MONGO_RAW,
            FANOUT_TARGET_QDRANT,
            FANOUT_TARGET_MINDSDB,
        ]
        if LANGFUSE_API_KEY:
            targets.append(FANOUT_TARGET_LANGFUSE)
        if _letta_target_enabled():
            targets.append(FANOUT_TARGET_LETTA)
    if not targets:
        raise HTTPException(400, "No valid targets selected for rehydrate")

    if payload.project:
        projects = [payload.project]
    else:
        projects = await list_projects()

    scanned = 0
    inserted = 0
    requeued = 0
    existing = 0
    errors: list[str] = []
    deferred_mongo = 0
    mongo_immediate_success = 0

    for project in projects:
        if scanned >= payload.limit:
            break
        try:
            files = await list_files(project)
        except Exception as exc:
            errors.append(f"{project}: list_files failed ({exc})")
            continue
        for file_name in files:
            if scanned >= payload.limit:
                break
            try:
                content = await read_project_file(project, file_name)
            except Exception as exc:
                errors.append(f"{project}/{file_name}: read failed ({exc})")
                continue
            if not content:
                continue
            summary = await summarize_content(content)
            topic_path = derive_topic_path(file_name, None)
            topic_tags = topic_tags_for_path(topic_path)
            event_id = build_event_id(project, file_name, content)
            raw_event = build_raw_memory_event(
                event_id=event_id,
                project=project,
                file_name=file_name,
                content=content,
                summary=summary,
                topic_path=topic_path,
                topic_tags=topic_tags,
                request_id="rehydrate",
                source="maintenance.rehydrate",
            )
            event_payload = {
                "event_id": event_id,
                "project": project,
                "file": file_name,
                "summary": summary,
                "payload": {"projectName": project, "fileName": file_name, "content": content},
                "topic_path": topic_path,
                "topic_tags": topic_tags,
                "letta_session": LETTA_AUTO_SESSION_ID if _letta_config_enabled() else None,
                "letta_context": {
                    "project": project,
                    "file": file_name,
                    "summary": summary,
                    "topic_path": topic_path,
                },
                "qdrant_collection": payload.qdrant_collection,
                "raw_event": raw_event,
            }
            event_targets = list(targets)
            if FANOUT_TARGET_MONGO_RAW in event_targets and not payload.force_requeue:
                ok, _ = await persist_raw_event_to_mongo(raw_event)
                if ok:
                    event_targets = [t for t in event_targets if t != FANOUT_TARGET_MONGO_RAW]
                    mongo_immediate_success += 1
                else:
                    deferred_mongo += 1
            result = await enqueue_fanout_outbox(event_payload, event_targets, force_requeue=payload.force_requeue)
            inserted += result["inserted"]
            requeued += result["requeued"]
            existing += result["existing"]
            scanned += 1

    if inserted > 0 or requeued > 0:
        if memory_write_queue.full():
            memory_write_queue_dropped += 1
        else:
            await memory_write_queue.put("rehydrate")

    return {
        "ok": True,
        "projects": projects,
        "targets": targets,
        "scanned_files": scanned,
        "outbox_inserted": inserted,
        "outbox_requeued": requeued,
        "outbox_existing": existing,
        "mongo_immediate_success": mongo_immediate_success,
        "mongo_deferred": deferred_mongo,
        "errors": errors[:50],
    }


@app.post("/maintenance/fanout/backfill/qdrant")
async def backfill_fanout_from_qdrant(payload: QdrantBackfillRequest):
    """
    Backfill fanout from existing Qdrant payloads.

    This is intended for onboarding/import scenarios where Qdrant already has
    project/file/summary entries but other sinks need hydration.
    """
    global memory_write_queue_dropped
    requested_targets = [t.lower() for t in (payload.targets or [])]
    if requested_targets:
        targets = [t for t in requested_targets if t in FANOUT_TARGETS]
    else:
        targets = [FANOUT_TARGET_MONGO_RAW, FANOUT_TARGET_MINDSDB]
        if _letta_target_enabled():
            targets.append(FANOUT_TARGET_LETTA)
    if payload.include_qdrant_target and FANOUT_TARGET_QDRANT not in targets:
        targets.append(FANOUT_TARGET_QDRANT)
    if LANGFUSE_API_KEY and FANOUT_TARGET_LANGFUSE not in targets:
        targets.append(FANOUT_TARGET_LANGFUSE)
    if not targets:
        raise HTTPException(400, "No valid targets selected for qdrant backfill")

    collection = payload.qdrant_collection or QDRANT_COLLECTION
    scanned = 0
    inserted = 0
    requeued = 0
    existing = 0
    skipped = 0
    errors: list[str] = []
    deferred_mongo = 0
    mongo_immediate_success = 0
    offset: Any = None
    seen_rows: set[str] = set()
    scroll_filter = None
    if payload.project and qdrant_models is not None:
        scroll_filter = qdrant_models.Filter(
            must=[
                qdrant_models.FieldCondition(
                    key="project",
                    match=qdrant_models.MatchValue(value=payload.project),
                )
            ]
        )

    while scanned < payload.limit:
        page_limit = min(256, payload.limit - scanned)
        try:
            points, next_offset = await _qdrant_call(
                "backfill_scroll",
                lambda client, _: client.scroll(
                    collection_name=collection,
                    scroll_filter=scroll_filter,
                    limit=page_limit,
                    offset=offset,
                    with_payload=True,
                    with_vectors=False,
                ),
            )
            offset = next_offset
        except Exception as exc:
            errors.append(f"scroll failed ({exc})")
            break

        if not points:
            break

        for point in points:
            if scanned >= payload.limit:
                break
            row_payload = getattr(point, "payload", None) or {}
            project = str(row_payload.get("project") or "").strip()
            file_name = str(row_payload.get("file") or "").strip()
            summary = str(row_payload.get("summary") or "").strip()
            if not project or not file_name or not summary:
                skipped += 1
                continue
            dedupe_row_key = f"{project}::{file_name}::{summary[:200]}"
            if dedupe_row_key in seen_rows:
                skipped += 1
                continue
            seen_rows.add(dedupe_row_key)

            topic_path = str(row_payload.get("topic_path") or derive_topic_path(file_name, None))
            raw_topic_tags = row_payload.get("topic_tags")
            if isinstance(raw_topic_tags, list):
                topic_tags = [str(tag).strip() for tag in raw_topic_tags if str(tag).strip()]
            else:
                topic_tags = topic_tags_for_path(topic_path)

            # For Qdrant-only imports we may not have original full content.
            synthetic_content = "\n".join(
                [
                    "[backfill:qdrant]",
                    f"project: {project}",
                    f"file: {file_name}",
                    f"summary: {summary}",
                ]
            )
            event_id = build_event_id(project, file_name, f"qdrant:{summary}")
            raw_event = build_raw_memory_event(
                event_id=event_id,
                project=project,
                file_name=file_name,
                content=synthetic_content,
                summary=summary,
                topic_path=topic_path,
                topic_tags=topic_tags,
                request_id="rehydrate-qdrant",
                source="maintenance.rehydrate_qdrant",
            )
            event_payload = {
                "event_id": event_id,
                "project": project,
                "file": file_name,
                "summary": summary,
                "payload": {
                    "projectName": project,
                    "fileName": file_name,
                    "content": synthetic_content,
                    "source": "qdrant_backfill",
                },
                "topic_path": topic_path,
                "topic_tags": topic_tags,
                "letta_session": LETTA_AUTO_SESSION_ID if _letta_config_enabled() else None,
                "letta_context": {
                    "project": project,
                    "file": file_name,
                    "summary": summary,
                    "topic_path": topic_path,
                    "source": "qdrant_backfill",
                },
                "qdrant_collection": collection,
                "raw_event": raw_event,
            }
            event_targets = list(targets)
            if FANOUT_TARGET_MONGO_RAW in event_targets and not payload.force_requeue:
                ok, _ = await persist_raw_event_to_mongo(raw_event)
                if ok:
                    event_targets = [t for t in event_targets if t != FANOUT_TARGET_MONGO_RAW]
                    mongo_immediate_success += 1
                else:
                    deferred_mongo += 1
            result = await enqueue_fanout_outbox(
                event_payload,
                event_targets,
                force_requeue=payload.force_requeue,
            )
            inserted += result["inserted"]
            requeued += result["requeued"]
            existing += result["existing"]
            scanned += 1

        if offset is None:
            break

    if inserted > 0 or requeued > 0:
        if memory_write_queue.full():
            memory_write_queue_dropped += 1
        else:
            await memory_write_queue.put("rehydrate-qdrant")

    return {
        "ok": True,
        "source_collection": collection,
        "project": payload.project,
        "targets": targets,
        "scanned_rows": scanned,
        "skipped_rows": skipped,
        "outbox_inserted": inserted,
        "outbox_requeued": requeued,
        "outbox_existing": existing,
        "mongo_immediate_success": mongo_immediate_success,
        "mongo_deferred": deferred_mongo,
        "errors": errors[:50],
    }


def _mongo_timestamp_iso(value: Any) -> str:
    if isinstance(value, datetime):
        if value.tzinfo is None:
            return value.isoformat() + "Z"
        return value.isoformat()
    text = str(value or "").strip()
    if not text:
        return _utc_now()
    return text


def _fanout_outstanding(summary: dict[str, Any]) -> int:
    by_status = summary.get("by_status") if isinstance(summary, dict) else {}
    if not isinstance(by_status, dict):
        return 0
    pending = int(by_status.get("pending", 0) or 0)
    retrying = int(by_status.get("retrying", 0) or 0)
    running = int(by_status.get("running", 0) or 0)
    return pending + retrying + running


@app.post("/maintenance/fanout/backfill/mongo")
async def backfill_fanout_from_mongo_raw(payload: MongoRawBackfillRequest):
    """
    Backfill fanout from the Mongo raw source-of-truth collection.

    This is intended for production repairs/migrations (for example, rebuilding
    Qdrant with a new vector dimension or rehydrating MindsDB after a table/db
    rotation) while preserving durable write history.
    """
    global memory_write_queue_dropped
    if not await init_mongo_client():
        raise HTTPException(503, "Mongo raw store is unavailable")
    assert MONGO_CLIENT is not None

    requested_targets = [t.lower() for t in (payload.targets or [])]
    if requested_targets:
        targets = [t for t in requested_targets if t in FANOUT_TARGETS]
    else:
        targets = [FANOUT_TARGET_QDRANT, FANOUT_TARGET_MINDSDB]
        if _letta_target_enabled():
            targets.append(FANOUT_TARGET_LETTA)
    if LANGFUSE_API_KEY and FANOUT_TARGET_LANGFUSE not in targets:
        targets.append(FANOUT_TARGET_LANGFUSE)
    if not targets:
        raise HTTPException(400, "No valid targets selected for mongo raw backfill")

    query_filter: dict[str, Any] = {}
    if payload.project:
        query_filter["project"] = payload.project
    projection = {
        "_id": 0,
        "event_id": 1,
        "source": 1,
        "project": 1,
        "file": 1,
        "content_raw": 1,
        "summary": 1,
        "topic_path": 1,
        "topic_tags": 1,
        "request_id": 1,
        "created_at": 1,
        "updated_at": 1,
    }

    def _scan_docs() -> list[dict[str, Any]]:
        coll = MONGO_CLIENT[MONGO_RAW_DB][MONGO_RAW_COLLECTION]
        return list(
            coll.find(query_filter, projection=projection)
            .sort("updated_at", -1)
            .limit(payload.limit)
        )

    try:
        docs = await asyncio.to_thread(_scan_docs)
    except Exception as exc:
        raise HTTPException(500, f"Mongo raw scan failed: {exc}") from exc

    collection = payload.qdrant_collection or QDRANT_COLLECTION
    scanned = 0
    inserted = 0
    requeued = 0
    existing = 0
    skipped = 0
    deferred_mongo = 0
    mongo_immediate_success = 0
    errors: list[str] = []
    seen_events: set[str] = set()
    throttled = False
    outstanding_at_stop = 0

    for doc in docs:
        if payload.max_pending_jobs > 0 and scanned > 0 and scanned % payload.check_interval == 0:
            try:
                summary = await _query_fanout_summary_uncached()
            except Exception:
                summary = await get_fanout_summary()
            outstanding_at_stop = _fanout_outstanding(summary)
            if outstanding_at_stop >= payload.max_pending_jobs:
                throttled = True
                break

        project = str(doc.get("project") or "").strip()
        file_name = str(doc.get("file") or "").strip()
        content = str(doc.get("content_raw") or "")
        summary = str(doc.get("summary") or "").strip()
        if not summary:
            summary = (content[:600] if content else f"{project}/{file_name}").strip()
        if not project or not file_name or not summary:
            skipped += 1
            continue

        topic_path = str(doc.get("topic_path") or derive_topic_path(file_name, None))
        raw_topic_tags = doc.get("topic_tags")
        if isinstance(raw_topic_tags, list):
            topic_tags = [str(tag).strip() for tag in raw_topic_tags if str(tag).strip()]
        else:
            topic_tags = topic_tags_for_path(topic_path)
        created_at = _mongo_timestamp_iso(doc.get("created_at"))
        updated_at = _mongo_timestamp_iso(doc.get("updated_at"))
        event_id = str(doc.get("event_id") or "").strip()
        if not event_id:
            event_id = build_event_id(project, file_name, content or summary)
        if event_id in seen_events:
            skipped += 1
            continue
        seen_events.add(event_id)

        raw_event = {
            "event_id": event_id,
            "source": str(doc.get("source") or "maintenance.rehydrate_mongo_raw"),
            "project": project,
            "file": file_name,
            "content_raw": content,
            "summary": summary,
            "topic_path": topic_path,
            "topic_tags": topic_tags,
            "request_id": doc.get("request_id"),
            "created_at": created_at,
            "updated_at": updated_at,
        }
        event_payload = {
            "event_id": event_id,
            "project": project,
            "file": file_name,
            "summary": summary,
            "payload": {
                "projectName": project,
                "fileName": file_name,
                "content": content,
                "source": "mongo_raw_backfill",
            },
            "topic_path": topic_path,
            "topic_tags": topic_tags,
            "letta_session": LETTA_AUTO_SESSION_ID if _letta_config_enabled() else None,
            "letta_context": {
                "project": project,
                "file": file_name,
                "summary": summary,
                "topic_path": topic_path,
                "source": "mongo_raw_backfill",
            },
            "qdrant_collection": collection,
            "raw_event": raw_event,
        }
        event_targets = list(targets)
        if FANOUT_TARGET_MONGO_RAW in event_targets and not payload.force_requeue:
            ok, _ = await persist_raw_event_to_mongo(raw_event)
            if ok:
                event_targets = [t for t in event_targets if t != FANOUT_TARGET_MONGO_RAW]
                mongo_immediate_success += 1
            else:
                deferred_mongo += 1
        try:
            result = await enqueue_fanout_outbox(
                event_payload,
                event_targets,
                force_requeue=payload.force_requeue,
            )
        except Exception as exc:
            errors.append(f"{project}/{file_name}: enqueue failed ({exc})")
            continue
        inserted += result["inserted"]
        requeued += result["requeued"]
        existing += result["existing"]
        scanned += 1

    if inserted > 0 or requeued > 0:
        if memory_write_queue.full():
            memory_write_queue_dropped += 1
        else:
            await memory_write_queue.put("rehydrate-mongo")

    if outstanding_at_stop == 0 and payload.max_pending_jobs > 0:
        try:
            summary = await get_fanout_summary()
            outstanding_at_stop = _fanout_outstanding(summary)
        except Exception:
            outstanding_at_stop = 0

    return {
        "ok": True,
        "source": "mongo_raw",
        "project": payload.project,
        "qdrant_collection": collection,
        "targets": targets,
        "scanned_rows": scanned,
        "skipped_rows": skipped,
        "outbox_inserted": inserted,
        "outbox_requeued": requeued,
        "outbox_existing": existing,
        "throttled": throttled,
        "outstanding_jobs": outstanding_at_stop,
        "max_pending_jobs": payload.max_pending_jobs,
        "mongo_immediate_success": mongo_immediate_success,
        "mongo_deferred": deferred_mongo,
        "errors": errors[:50],
    }
