use std::cmp::Ordering;
use std::collections::hash_map::DefaultHasher;
use std::collections::{HashMap, HashSet};
use std::env;
use std::fs;
use std::hash::{Hash, Hasher};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use futures_util::TryStreamExt;
use mongodb::bson::{Bson, Document};
use mongodb::Client as MongoClient;
use reqwest::Client;
use serde::{Deserialize, Serialize};
use tantivy::collector::TopDocs;
use tantivy::query::QueryParser;
use tantivy::schema::{Schema, Value, STORED, STRING, TEXT};
use tantivy::{doc, Index};
use tokio::net::TcpListener;
use tokio::sync::{Mutex, RwLock};
use tracing::{error, info, warn};
use walkdir::WalkDir;

#[cfg(not(target_env = "msvc"))]
#[global_allocator]
static GLOBAL_ALLOCATOR: mimalloc::MiMalloc = mimalloc::MiMalloc;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SnapshotSourceMode {
    File,
    Mongo,
    MongoFirst,
    Hybrid,
}

impl SnapshotSourceMode {
    fn from_env(raw: &str) -> Self {
        match raw.trim().to_lowercase().as_str() {
            "file" | "filesystem" => Self::File,
            "mongo" | "mongo_only" => Self::Mongo,
            "mongo_first" => Self::MongoFirst,
            "hybrid" | "file_fallback" => Self::Hybrid,
            _ => Self::Hybrid,
        }
    }

    fn use_mongo(self) -> bool {
        matches!(self, Self::Mongo | Self::MongoFirst | Self::Hybrid)
    }

    fn is_mongo_first(self) -> bool {
        matches!(self, Self::MongoFirst)
    }

    fn as_str(self) -> &'static str {
        match self {
            Self::File => "file",
            Self::Mongo => "mongo",
            Self::MongoFirst => "mongo_first",
            Self::Hybrid => "hybrid",
        }
    }
}

#[derive(Clone)]
struct Config {
    port: u16,
    data_root: PathBuf,
    source_mode: SnapshotSourceMode,
    mongo_uri: String,
    mongo_db: String,
    mongo_events_collection: String,
    mongo_query_timeout_secs: u64,
    mongo_scan_multiplier: usize,
    refresh_secs: u64,
    max_docs: usize,
    max_content_chars: usize,
    external_timeout_secs: u64,
    external_timeout_secs_icm: u64,
    meili_url: String,
    meili_api_key: String,
    meili_index_uid: String,
    meili_sync_secs: u64,
    meili_task_timeout_secs: u64,
    lancedb_url: String,
    lancedb_search_route: String,
    lancedb_api_key: String,
    trieve_url: String,
    trieve_search_route: String,
    trieve_api_key: String,
    helixdb_url: String,
    helixdb_search_route: String,
    helixdb_api_key: String,
    icm_url: String,
    icm_search_route: String,
    icm_api_key: String,
    shodh_url: String,
    shodh_search_route: String,
    shodh_api_key: String,
    memvid_url: String,
    memvid_search_route: String,
    memvid_api_key: String,
    surrealdb_url: String,
    surrealdb_search_route: String,
    surrealdb_api_key: String,
}

impl Config {
    fn from_env() -> Self {
        let port = env_u16("PORT", 8096);
        let data_root = PathBuf::from(env_string("MB_SPIKE_DATA_ROOT", "/data/memory-bank"));
        let source_mode =
            SnapshotSourceMode::from_env(&env_string("MB_SPIKE_SOURCE_MODE", "hybrid"));
        let mongo_uri = env::var("MB_SPIKE_MONGO_URI")
            .or_else(|_| env::var("MONGODB_URI"))
            .unwrap_or_else(|_| "mongodb://mongo:27017".to_string());
        let mongo_db = env::var("MB_SPIKE_MONGO_DB")
            .or_else(|_| env::var("GO_TELEMETRY_DB"))
            .or_else(|_| env::var("ORCH_TELEMETRY_DB"))
            .unwrap_or_else(|_| "contextlattice_raw".to_string());
        let mongo_events_collection = env::var("MB_SPIKE_MONGO_EVENTS_COLLECTION")
            .or_else(|_| env::var("GO_TELEMETRY_EVENTS_COLLECTION"))
            .unwrap_or_else(|_| "memory_write_telemetry".to_string());
        let mongo_query_timeout_secs = env_u64("MB_SPIKE_MONGO_QUERY_TIMEOUT_SECS", 6);
        let mongo_scan_multiplier = env_usize("MB_SPIKE_MONGO_SCAN_MULTIPLIER", 12).max(1);
        let refresh_secs = env_u64("MB_SPIKE_REFRESH_SECS", 120);
        let max_docs = env_usize("MB_SPIKE_MAX_DOCS", 50_000);
        let max_content_chars = env_usize("MB_SPIKE_MAX_CONTENT_CHARS", 4096);
        let external_timeout_secs = env_u64("MB_SPIKE_EXTERNAL_TIMEOUT_SECS", 12);
        let external_timeout_secs_icm = env_u64("MB_SPIKE_EXTERNAL_TIMEOUT_SECS_ICM", 0);
        let meili_url = env_string("MB_SPIKE_MEILI_URL", "http://meilisearch:7700");
        let meili_api_key = env::var("MB_SPIKE_MEILI_API_KEY")
            .or_else(|_| env::var("MEILI_MASTER_KEY"))
            .unwrap_or_default()
            .trim()
            .to_string();
        let meili_index_uid = env_string("MB_SPIKE_MEILI_INDEX", "contextlattice_memory");
        let meili_sync_secs = env_u64("MB_SPIKE_MEILI_SYNC_SECS", 300);
        let meili_task_timeout_secs = env_u64("MB_SPIKE_MEILI_TASK_TIMEOUT_SECS", 30);
        let lancedb_url = env_string("MB_SPIKE_LANCEDB_URL", "");
        let lancedb_search_route = env_string("MB_SPIKE_LANCEDB_SEARCH_ROUTE", "/search");
        let lancedb_api_key = env_string("MB_SPIKE_LANCEDB_API_KEY", "");
        let trieve_url = env_string("MB_SPIKE_TRIEVE_URL", "");
        let trieve_search_route = env_string("MB_SPIKE_TRIEVE_SEARCH_ROUTE", "/search");
        let trieve_api_key = env_string("MB_SPIKE_TRIEVE_API_KEY", "");
        let helixdb_url = env_string("MB_SPIKE_HELIXDB_URL", "");
        let helixdb_search_route = env_string("MB_SPIKE_HELIXDB_SEARCH_ROUTE", "/search");
        let helixdb_api_key = env_string("MB_SPIKE_HELIXDB_API_KEY", "");
        let icm_url = env_string("MB_SPIKE_ICM_URL", "");
        let icm_search_route = env_string("MB_SPIKE_ICM_SEARCH_ROUTE", "/search");
        let icm_api_key = env_string("MB_SPIKE_ICM_API_KEY", "");
        let shodh_url = env_string("MB_SPIKE_SHODH_URL", "");
        let shodh_search_route = env_string("MB_SPIKE_SHODH_SEARCH_ROUTE", "/search");
        let shodh_api_key = env_string("MB_SPIKE_SHODH_API_KEY", "");
        let memvid_url = env_string("MB_SPIKE_MEMVID_URL", "");
        let memvid_search_route = env_string("MB_SPIKE_MEMVID_SEARCH_ROUTE", "/search");
        let memvid_api_key = env_string("MB_SPIKE_MEMVID_API_KEY", "");
        let surrealdb_url = env_string("MB_SPIKE_SURREALDB_URL", "");
        let surrealdb_search_route = env_string("MB_SPIKE_SURREALDB_SEARCH_ROUTE", "/search");
        let surrealdb_api_key = env_string("MB_SPIKE_SURREALDB_API_KEY", "");
        Self {
            port,
            data_root,
            source_mode,
            mongo_uri: mongo_uri.trim().to_string(),
            mongo_db: mongo_db.trim().to_string(),
            mongo_events_collection: mongo_events_collection.trim().to_string(),
            mongo_query_timeout_secs: mongo_query_timeout_secs.max(1),
            mongo_scan_multiplier,
            refresh_secs,
            max_docs,
            max_content_chars,
            external_timeout_secs,
            external_timeout_secs_icm,
            meili_url,
            meili_api_key,
            meili_index_uid,
            meili_sync_secs,
            meili_task_timeout_secs,
            lancedb_url,
            lancedb_search_route,
            lancedb_api_key,
            trieve_url,
            trieve_search_route,
            trieve_api_key,
            helixdb_url,
            helixdb_search_route,
            helixdb_api_key,
            icm_url,
            icm_search_route,
            icm_api_key,
            shodh_url,
            shodh_search_route,
            shodh_api_key,
            memvid_url,
            memvid_search_route,
            memvid_api_key,
            surrealdb_url,
            surrealdb_search_route,
            surrealdb_api_key,
        }
    }

    fn external_timeout_for_backend(&self, backend: &str) -> u64 {
        let default_timeout = self.external_timeout_secs.max(1);
        match backend {
            "icm_spike" if self.external_timeout_secs_icm > 0 => {
                self.external_timeout_secs_icm.max(1)
            }
            _ => default_timeout,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct SearchRequest {
    query: String,
    #[serde(default = "default_limit")]
    limit: usize,
    #[serde(default)]
    project: Option<String>,
    #[serde(default)]
    topic_path: Option<String>,
    #[serde(default)]
    backend: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct SearchResult {
    project: String,
    file: String,
    summary: String,
    score: f32,
    topic_path: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct SearchResponse {
    backend: String,
    results: Vec<SearchResult>,
    meta: SearchMeta,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct SearchMeta {
    document_count: usize,
    fingerprint: u64,
    refreshed_at_unix_secs: u64,
}

#[derive(Clone, Debug)]
struct MemoryDoc {
    id: String,
    project: String,
    file: String,
    topic_path: String,
    summary: String,
}

#[derive(Clone, Debug)]
struct DocSnapshot {
    docs: Arc<Vec<MemoryDoc>>,
    fingerprint: u64,
    refreshed_at: Instant,
    refreshed_at_unix_secs: u64,
}

impl Default for DocSnapshot {
    fn default() -> Self {
        Self {
            docs: Arc::new(Vec::new()),
            fingerprint: 0,
            refreshed_at: Instant::now() - Duration::from_secs(3600),
            refreshed_at_unix_secs: 0,
        }
    }
}

#[derive(Clone, Debug, Default)]
struct TantivyCache {
    fingerprint: u64,
    index: Option<Index>,
    schema: Option<TantivySchema>,
}

#[derive(Clone, Debug)]
struct TantivySchema {
    project: tantivy::schema::Field,
    file: tantivy::schema::Field,
    topic_path: tantivy::schema::Field,
    summary: tantivy::schema::Field,
}

#[derive(Clone, Debug, Default)]
struct QuickwitCompatCache {
    fingerprint: u64,
    postings: HashMap<String, Vec<(usize, u16)>>,
}

#[derive(Clone, Debug, Default)]
struct MeiliCache {
    fingerprint: u64,
    last_sync: Option<Instant>,
    last_error: Option<String>,
}

#[derive(Clone)]
struct AppState {
    cfg: Config,
    docs: Arc<RwLock<DocSnapshot>>,
    snapshot_build_lock: Arc<Mutex<()>>,
    mongo_client: Option<MongoClient>,
    tantivy: Arc<RwLock<TantivyCache>>,
    tantivy_build_lock: Arc<Mutex<()>>,
    quickwit: Arc<RwLock<QuickwitCompatCache>>,
    quickwit_build_lock: Arc<Mutex<()>>,
    meili: Arc<RwLock<MeiliCache>>,
    meili_sync_lock: Arc<Mutex<()>>,
    client: Client,
}

#[derive(Serialize)]
struct HealthResponse {
    ok: bool,
    backend_modes: Vec<&'static str>,
    source_mode: String,
    docs_loaded: usize,
    fingerprint: u64,
    refreshed_at_unix_secs: u64,
    spike_data_root: String,
    meili_url: String,
    meili_index_uid: String,
    meili_task_timeout_secs: u64,
    external_timeout_secs: u64,
    external_timeout_secs_icm: u64,
    external_backends: HashMap<String, bool>,
}

#[derive(Serialize)]
struct ErrorResponse {
    error: String,
}

fn default_limit() -> usize {
    10
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,memory_bank_spike_rs=debug".into()),
        )
        .with_target(false)
        .compact()
        .init();

    let cfg = Config::from_env();
    let mongo_client = if cfg.source_mode.use_mongo() {
        match MongoClient::with_uri_str(cfg.mongo_uri.clone()).await {
            Ok(client) => Some(client),
            Err(err) => {
                if cfg.source_mode == SnapshotSourceMode::Mongo {
                    return Err(err).context("connect mongo snapshot source");
                }
                warn!(
                    error = %err,
                    "mongo snapshot source unavailable; continuing with file snapshot mode"
                );
                None
            }
        }
    } else {
        None
    };
    info!(
        port = cfg.port,
        data_root = %cfg.data_root.display(),
        source_mode = cfg.source_mode.as_str(),
        mongo_db = %cfg.mongo_db,
        mongo_collection = %cfg.mongo_events_collection,
        refresh_secs = cfg.refresh_secs,
        max_docs = cfg.max_docs,
        "starting memory-bank spike rust sidecar"
    );

    let state = AppState {
        cfg,
        docs: Arc::new(RwLock::new(DocSnapshot::default())),
        snapshot_build_lock: Arc::new(Mutex::new(())),
        mongo_client,
        tantivy: Arc::new(RwLock::new(TantivyCache::default())),
        tantivy_build_lock: Arc::new(Mutex::new(())),
        quickwit: Arc::new(RwLock::new(QuickwitCompatCache::default())),
        quickwit_build_lock: Arc::new(Mutex::new(())),
        meili: Arc::new(RwLock::new(MeiliCache::default())),
        meili_sync_lock: Arc::new(Mutex::new(())),
        client: Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .context("build reqwest client")?,
    };

    let app = Router::new()
        .route("/health", get(health))
        .route("/search", post(search))
        .with_state(state.clone());

    let listener = TcpListener::bind(("0.0.0.0", state.cfg.port))
        .await
        .context("bind listener")?;
    axum::serve(listener, app).await.context("serve http")?;
    Ok(())
}

async fn health(State(state): State<AppState>) -> impl IntoResponse {
    let snapshot = state.docs.read().await.clone();
    let mut external_backends: HashMap<String, bool> = HashMap::new();
    external_backends.insert(
        "lancedb_spike".to_string(),
        !state.cfg.lancedb_url.trim().is_empty(),
    );
    external_backends.insert(
        "trieve_spike".to_string(),
        !state.cfg.trieve_url.trim().is_empty(),
    );
    external_backends.insert(
        "helixdb_spike".to_string(),
        !state.cfg.helixdb_url.trim().is_empty(),
    );
    external_backends.insert(
        "icm_spike".to_string(),
        !state.cfg.icm_url.trim().is_empty(),
    );
    external_backends.insert(
        "shodh_spike".to_string(),
        !state.cfg.shodh_url.trim().is_empty(),
    );
    external_backends.insert(
        "memvid_spike".to_string(),
        !state.cfg.memvid_url.trim().is_empty(),
    );
    external_backends.insert(
        "surrealdb_spike".to_string(),
        !state.cfg.surrealdb_url.trim().is_empty(),
    );
    let payload = HealthResponse {
        ok: true,
        backend_modes: vec![
            "tantivy_spike",
            "quickwit_spike",
            "meilisearch_spike",
            "lancedb_spike",
            "trieve_spike",
            "helixdb_spike",
            "icm_spike",
            "shodh_spike",
            "memvid_spike",
            "surrealdb_spike",
        ],
        source_mode: state.cfg.source_mode.as_str().to_string(),
        docs_loaded: snapshot.docs.len(),
        fingerprint: snapshot.fingerprint,
        refreshed_at_unix_secs: snapshot.refreshed_at_unix_secs,
        spike_data_root: state.cfg.data_root.to_string_lossy().to_string(),
        meili_url: state.cfg.meili_url.clone(),
        meili_index_uid: state.cfg.meili_index_uid.clone(),
        meili_task_timeout_secs: state.cfg.meili_task_timeout_secs,
        external_timeout_secs: state.cfg.external_timeout_secs,
        external_timeout_secs_icm: state.cfg.external_timeout_secs_icm,
        external_backends,
    };
    (StatusCode::OK, Json(payload))
}

async fn search(
    State(state): State<AppState>,
    Json(req): Json<SearchRequest>,
) -> impl IntoResponse {
    let query = req.query.trim();
    if query.is_empty() {
        return (
            StatusCode::UNPROCESSABLE_ENTITY,
            Json(ErrorResponse {
                error: "query is required".to_string(),
            }),
        )
            .into_response();
    }
    let backend = normalize_backend(req.backend.as_deref().unwrap_or("tantivy_spike"));
    let limit = req.limit.clamp(1, 100);
    let project_filter = req
        .project
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty());
    let topic_filter = req
        .topic_path
        .as_deref()
        .map(str::trim)
        .map(normalize_topic)
        .filter(|s| !s.is_empty());

    let snapshot = match ensure_snapshot(&state).await {
        Ok(snapshot) => snapshot,
        Err(err) => {
            error!(error = %err, "failed to load document snapshot");
            return (
                StatusCode::INTERNAL_SERVER_ERROR,
                Json(ErrorResponse {
                    error: format!("failed to load memory-bank snapshot: {err}"),
                }),
            )
                .into_response();
        }
    };

    let search_result = match backend.as_str() {
        "quickwit_spike" => {
            quickwit_search(
                &state,
                &snapshot,
                query,
                limit,
                project_filter,
                topic_filter.as_deref(),
            )
            .await
        }
        "meilisearch_spike" => {
            meili_search(
                &state,
                &snapshot,
                query,
                limit,
                project_filter,
                topic_filter.as_deref(),
            )
            .await
        }
        "lancedb_spike" | "trieve_spike" | "helixdb_spike" => {
            external_adapter_search(
                &state,
                backend.as_str(),
                query,
                limit,
                project_filter,
                topic_filter.as_deref(),
            )
            .await
        }
        _ => {
            tantivy_search(
                &state,
                &snapshot,
                query,
                limit,
                project_filter,
                topic_filter.as_deref(),
            )
            .await
        }
    };

    match search_result {
        Ok(results) => {
            let response = SearchResponse {
                backend,
                results,
                meta: SearchMeta {
                    document_count: snapshot.docs.len(),
                    fingerprint: snapshot.fingerprint,
                    refreshed_at_unix_secs: snapshot.refreshed_at_unix_secs,
                },
            };
            (StatusCode::OK, Json(response)).into_response()
        }
        Err(err) => {
            warn!(error = %err, "search backend error");
            (
                StatusCode::BAD_GATEWAY,
                Json(ErrorResponse {
                    error: err.to_string(),
                }),
            )
                .into_response()
        }
    }
}

async fn ensure_snapshot(state: &AppState) -> Result<DocSnapshot> {
    {
        let current = state.docs.read().await;
        if current.refreshed_at.elapsed() < Duration::from_secs(state.cfg.refresh_secs) {
            return Ok(current.clone());
        }
    }

    let _guard = state.snapshot_build_lock.lock().await;
    {
        let current = state.docs.read().await;
        if current.refreshed_at.elapsed() < Duration::from_secs(state.cfg.refresh_secs) {
            return Ok(current.clone());
        }
    }

    let loaded = build_docs_snapshot(state).await?;

    {
        let mut current = state.docs.write().await;
        *current = loaded.clone();
    }
    Ok(loaded)
}

async fn build_docs_snapshot(state: &AppState) -> Result<DocSnapshot> {
    let cfg = state.cfg.clone();
    let mut docs: Vec<MemoryDoc> = Vec::new();
    let mut seen_ids: HashSet<String> = HashSet::new();
    let mut latest_mtime: u64 = 0;
    let mut total_bytes: u64 = 0;
    let mut scanned: usize = 0;
    let mut mongo_loaded = false;
    let mut mongo_doc_count = 0usize;

    if cfg.source_mode.use_mongo() {
        match load_docs_snapshot_mongo(state).await {
            Ok((mongo_docs, mongo_scanned, mongo_bytes, mongo_latest)) => {
                mongo_loaded = true;
                mongo_doc_count = mongo_docs.len();
                for row in mongo_docs {
                    seen_ids.insert(row.id.clone());
                    docs.push(row);
                }
                scanned = scanned.saturating_add(mongo_scanned);
                total_bytes = total_bytes.saturating_add(mongo_bytes);
                latest_mtime = latest_mtime.max(mongo_latest);
            }
            Err(err) => {
                if cfg.source_mode == SnapshotSourceMode::Mongo {
                    return Err(err).context("load docs from mongo snapshot source");
                }
                warn!(
                    error = %err,
                    "mongo snapshot load failed; falling back to file source"
                );
            }
        }
    }

    let should_load_files = if cfg.source_mode == SnapshotSourceMode::File {
        true
    } else if cfg.source_mode == SnapshotSourceMode::Hybrid {
        true
    } else if cfg.source_mode.is_mongo_first() {
        !mongo_loaded || mongo_doc_count == 0
    } else {
        false
    };

    if should_load_files {
        let cfg_for_files = cfg.clone();
        let (file_docs, file_scanned, file_total_bytes, file_latest_mtime) =
            tokio::task::spawn_blocking(move || load_docs_snapshot_files(&cfg_for_files))
                .await
                .context("join file document loader")??;
        for row in file_docs {
            if seen_ids.insert(row.id.clone()) {
                docs.push(row);
            }
        }
        scanned = scanned.saturating_add(file_scanned);
        total_bytes = total_bytes.saturating_add(file_total_bytes);
        latest_mtime = latest_mtime.max(file_latest_mtime);
    }

    if docs.len() > cfg.max_docs {
        docs.truncate(cfg.max_docs);
    }

    let mut fingerprint = 1469598103934665603u64; // FNV offset basis
    fingerprint ^= docs.len() as u64;
    fingerprint = fingerprint.wrapping_mul(1099511628211);
    fingerprint ^= latest_mtime;
    fingerprint = fingerprint.wrapping_mul(1099511628211);
    fingerprint ^= total_bytes;
    fingerprint = fingerprint.wrapping_mul(1099511628211);

    let refreshed_at_unix_secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);

    info!(
        docs = docs.len(),
        scanned,
        total_bytes,
        latest_mtime,
        fingerprint,
        source_mode = cfg.source_mode.as_str(),
        "memory-bank snapshot loaded"
    );

    Ok(DocSnapshot {
        docs: Arc::new(docs),
        fingerprint,
        refreshed_at: Instant::now(),
        refreshed_at_unix_secs,
    })
}

fn load_docs_snapshot_files(cfg: &Config) -> Result<(Vec<MemoryDoc>, usize, u64, u64)> {
    let root = cfg.data_root.clone();
    if !root.exists() {
        return Ok((Vec::new(), 0, 0, 0));
    }

    let mut docs: Vec<MemoryDoc> = Vec::new();
    let mut latest_mtime: u64 = 0;
    let mut total_bytes: u64 = 0;
    let mut scanned: usize = 0;

    for entry in WalkDir::new(&root)
        .follow_links(false)
        .into_iter()
        .filter_map(|row| row.ok())
    {
        if !entry.file_type().is_file() {
            continue;
        }
        let path = entry.into_path();
        scanned += 1;
        if docs.len() >= cfg.max_docs {
            break;
        }
        let rel = match path.strip_prefix(&root) {
            Ok(rel) => rel,
            Err(_) => continue,
        };
        let mut comps = rel.components();
        let project = match comps.next() {
            Some(c) => c.as_os_str().to_string_lossy().to_string(),
            None => continue,
        };
        if project.is_empty() {
            continue;
        }
        let file = rel
            .iter()
            .skip(1)
            .map(|p| p.to_string_lossy())
            .collect::<Vec<_>>()
            .join("/");
        if file.is_empty() {
            continue;
        }
        let bytes = match fs::read(&path) {
            Ok(bytes) => bytes,
            Err(_) => continue,
        };
        total_bytes = total_bytes.saturating_add(bytes.len() as u64);
        let mut summary = String::from_utf8_lossy(&bytes).to_string();
        if summary.len() > cfg.max_content_chars {
            summary.truncate(cfg.max_content_chars);
        }
        let summary = normalize_text(&summary);
        if summary.is_empty() {
            continue;
        }
        let topic_path = derive_topic_path(&file);
        let id = format!("{project}::{file}");
        if let Ok(meta) = fs::metadata(&path) {
            if let Ok(modified) = meta.modified() {
                if let Ok(secs) = modified.duration_since(UNIX_EPOCH) {
                    latest_mtime = latest_mtime.max(secs.as_secs());
                }
            }
        }
        docs.push(MemoryDoc {
            id,
            project,
            file,
            topic_path,
            summary,
        });
    }

    Ok((docs, scanned, total_bytes, latest_mtime))
}

async fn load_docs_snapshot_mongo(state: &AppState) -> Result<(Vec<MemoryDoc>, usize, u64, u64)> {
    let cfg = &state.cfg;
    let client = state
        .mongo_client
        .as_ref()
        .context("mongo client unavailable for snapshot source")?;
    let collection = client
        .database(cfg.mongo_db.trim())
        .collection::<Document>(cfg.mongo_events_collection.trim());
    let scan_limit = cfg
        .max_docs
        .saturating_mul(cfg.mongo_scan_multiplier)
        .max(cfg.max_docs)
        .min(500_000);
    let timeout = Duration::from_secs(cfg.mongo_query_timeout_secs.max(1));

    tokio::time::timeout(timeout, async {
        let mut cursor = collection
            .find(Document::new())
            .sort(mongodb::bson::doc! {"created_at": -1_i32})
            .limit(scan_limit as i64)
            .projection(mongodb::bson::doc! {
                "project": 1_i32,
                "file": 1_i32,
                "file_name": 1_i32,
                "topic_path": 1_i32,
                "summary": 1_i32,
                "content_inline": 1_i32,
                "created_at": 1_i32,
            })
            .await
            .context("mongo find for memory-bank snapshot")?;

        let mut docs: Vec<MemoryDoc> = Vec::new();
        let mut seen_ids: HashSet<String> = HashSet::new();
        let mut scanned: usize = 0;
        let mut total_bytes: u64 = 0;
        let mut latest_mtime: u64 = 0;

        while let Some(doc) = cursor.try_next().await.context("mongo cursor next")? {
            scanned = scanned.saturating_add(1);
            if docs.len() >= cfg.max_docs {
                break;
            }

            let project = first_doc_string(&doc, &["project"]);
            if project.is_empty() {
                continue;
            }

            let file = first_doc_string(&doc, &["file", "file_name"]);
            if file.is_empty() {
                continue;
            }

            let mut summary = first_doc_string(&doc, &["summary", "content_inline"]);
            if summary.is_empty() {
                continue;
            }
            if summary.len() > cfg.max_content_chars {
                summary.truncate(cfg.max_content_chars);
            }
            summary = normalize_text(&summary);
            if summary.is_empty() {
                continue;
            }
            total_bytes = total_bytes.saturating_add(summary.len() as u64);

            let mut topic_path = first_doc_string(&doc, &["topic_path"]);
            if topic_path.is_empty() {
                topic_path = derive_topic_path(&file);
            }
            let id = format!("{project}::{file}");
            if !seen_ids.insert(id.clone()) {
                continue;
            }
            latest_mtime = latest_mtime.max(doc_time_unix_secs(&doc, "created_at"));

            docs.push(MemoryDoc {
                id,
                project,
                file,
                topic_path,
                summary,
            });
        }

        info!(
            docs = docs.len(),
            scanned,
            total_bytes,
            latest_mtime,
            db = %cfg.mongo_db,
            collection = %cfg.mongo_events_collection,
            "mongo snapshot load complete"
        );

        Ok((docs, scanned, total_bytes, latest_mtime))
    })
    .await
    .context("mongo snapshot load timed out")?
}

fn first_doc_string(doc: &Document, keys: &[&str]) -> String {
    for key in keys {
        if let Some(value) = doc.get(*key) {
            let text = bson_to_string(value);
            if !text.is_empty() {
                return text;
            }
        }
    }
    String::new()
}

fn bson_to_string(value: &Bson) -> String {
    match value {
        Bson::String(v) => v.trim().to_string(),
        Bson::ObjectId(v) => v.to_hex(),
        Bson::Int32(v) => v.to_string(),
        Bson::Int64(v) => v.to_string(),
        Bson::Double(v) => v.to_string(),
        Bson::Boolean(v) => v.to_string(),
        Bson::DateTime(v) => v.timestamp_millis().to_string(),
        _ => String::new(),
    }
}

fn doc_time_unix_secs(doc: &Document, key: &str) -> u64 {
    let Some(value) = doc.get(key) else {
        return 0;
    };
    match value {
        Bson::DateTime(v) => {
            if v.timestamp_millis() < 0 {
                0
            } else {
                (v.timestamp_millis() as u64) / 1000
            }
        }
        Bson::Timestamp(v) => v.time as u64,
        Bson::Int64(v) => (*v).max(0) as u64,
        Bson::Int32(v) => (*v).max(0) as u64,
        Bson::String(v) => v.parse::<u64>().unwrap_or(0),
        _ => 0,
    }
}

async fn tantivy_search(
    state: &AppState,
    snapshot: &DocSnapshot,
    query: &str,
    limit: usize,
    project_filter: Option<&str>,
    topic_filter: Option<&str>,
) -> Result<Vec<SearchResult>> {
    {
        let cache = state.tantivy.read().await;
        if cache.fingerprint == snapshot.fingerprint
            && cache.index.is_some()
            && cache.schema.is_some()
        {
            return run_tantivy_query(
                cache.index.as_ref().expect("checked index"),
                cache.schema.as_ref().expect("checked schema"),
                query,
                limit,
                project_filter,
                topic_filter,
            );
        }
    }

    let _guard = state.tantivy_build_lock.lock().await;
    {
        let cache = state.tantivy.read().await;
        if cache.fingerprint == snapshot.fingerprint
            && cache.index.is_some()
            && cache.schema.is_some()
        {
            return run_tantivy_query(
                cache.index.as_ref().expect("checked index"),
                cache.schema.as_ref().expect("checked schema"),
                query,
                limit,
                project_filter,
                topic_filter,
            );
        }
    }

    let docs = snapshot.docs.clone();
    let built = tokio::task::spawn_blocking(move || build_tantivy_index(&docs))
        .await
        .context("join tantivy build")??;

    {
        let mut cache = state.tantivy.write().await;
        cache.fingerprint = snapshot.fingerprint;
        cache.index = Some(built.0.clone());
        cache.schema = Some(built.1.clone());
    }

    run_tantivy_query(
        &built.0,
        &built.1,
        query,
        limit,
        project_filter,
        topic_filter,
    )
}

fn build_tantivy_index(docs: &[MemoryDoc]) -> Result<(Index, TantivySchema)> {
    let mut schema_builder = Schema::builder();
    let id_field = schema_builder.add_text_field("id", STRING | STORED);
    let project_field = schema_builder.add_text_field("project", STRING | STORED);
    let file_field = schema_builder.add_text_field("file", TEXT | STORED);
    let topic_field = schema_builder.add_text_field("topic_path", TEXT | STORED);
    let summary_field = schema_builder.add_text_field("summary", TEXT | STORED);
    let schema = schema_builder.build();
    let index = Index::create_in_ram(schema.clone());
    let mut writer = index.writer(50_000_000).context("create tantivy writer")?;
    for row in docs {
        writer.add_document(doc!(
            id_field => row.id.clone(),
            project_field => row.project.clone(),
            file_field => row.file.clone(),
            topic_field => row.topic_path.clone(),
            summary_field => row.summary.clone(),
        ))?;
    }
    writer.commit().context("commit tantivy docs")?;
    Ok((
        index,
        TantivySchema {
            project: project_field,
            file: file_field,
            topic_path: topic_field,
            summary: summary_field,
        },
    ))
}

fn run_tantivy_query(
    index: &Index,
    schema: &TantivySchema,
    query: &str,
    limit: usize,
    project_filter: Option<&str>,
    topic_filter: Option<&str>,
) -> Result<Vec<SearchResult>> {
    let reader = index
        .reader_builder()
        .reload_policy(tantivy::ReloadPolicy::Manual)
        .try_into()
        .context("create tantivy reader")?;
    let searcher = reader.searcher();
    let parser =
        QueryParser::for_index(index, vec![schema.summary, schema.file, schema.topic_path]);
    let query_obj = match parser.parse_query(query) {
        Ok(parsed) => parsed,
        Err(_) => {
            // QueryParser is strict with reserved punctuation (for example cache-bust tokens like "::").
            // Fall back to tokenized text to keep the benchmark lane deterministic.
            let fallback_query = tokenize(query).join(" ");
            if fallback_query.is_empty() {
                return Ok(Vec::new());
            }
            parser
                .parse_query(&fallback_query)
                .context("parse tantivy query fallback")?
        }
    };
    let top_n = limit.saturating_mul(12).max(64);
    let docs = searcher
        .search(&query_obj, &TopDocs::with_limit(top_n).order_by_score())
        .context("run tantivy query")?;
    let mut out: Vec<SearchResult> = Vec::new();
    for (score, addr) in docs {
        let row: tantivy::schema::TantivyDocument =
            searcher.doc(addr).context("load tantivy doc")?;
        let project = row
            .get_first(schema.project)
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        let file = row
            .get_first(schema.file)
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        let topic_path = row
            .get_first(schema.topic_path)
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        let summary = row
            .get_first(schema.summary)
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        if !matches_project_topic(&project, &topic_path, project_filter, topic_filter) {
            continue;
        }
        out.push(SearchResult {
            project,
            file,
            summary,
            score,
            topic_path,
        });
        if out.len() >= limit {
            break;
        }
    }
    Ok(out)
}

async fn external_adapter_search(
    state: &AppState,
    backend: &str,
    query: &str,
    limit: usize,
    project_filter: Option<&str>,
    topic_filter: Option<&str>,
) -> Result<Vec<SearchResult>> {
    let (base_url, route, api_key) = match backend {
        "lancedb_spike" => (
            state.cfg.lancedb_url.trim(),
            state.cfg.lancedb_search_route.trim(),
            state.cfg.lancedb_api_key.trim(),
        ),
        "trieve_spike" => (
            state.cfg.trieve_url.trim(),
            state.cfg.trieve_search_route.trim(),
            state.cfg.trieve_api_key.trim(),
        ),
        "helixdb_spike" => (
            state.cfg.helixdb_url.trim(),
            state.cfg.helixdb_search_route.trim(),
            state.cfg.helixdb_api_key.trim(),
        ),
        "icm_spike" => (
            state.cfg.icm_url.trim(),
            state.cfg.icm_search_route.trim(),
            state.cfg.icm_api_key.trim(),
        ),
        "shodh_spike" => (
            state.cfg.shodh_url.trim(),
            state.cfg.shodh_search_route.trim(),
            state.cfg.shodh_api_key.trim(),
        ),
        "memvid_spike" => (
            state.cfg.memvid_url.trim(),
            state.cfg.memvid_search_route.trim(),
            state.cfg.memvid_api_key.trim(),
        ),
        "surrealdb_spike" => (
            state.cfg.surrealdb_url.trim(),
            state.cfg.surrealdb_search_route.trim(),
            state.cfg.surrealdb_api_key.trim(),
        ),
        _ => ("", "", ""),
    };
    if base_url.is_empty() {
        anyhow::bail!("{backend} endpoint is not configured");
    }
    let route = normalize_http_route(route);
    let url = format!("{}{}", base_url.trim_end_matches('/'), route);

    let mut payload = serde_json::Map::new();
    payload.insert(
        "query".to_string(),
        serde_json::Value::String(query.to_string()),
    );
    payload.insert(
        "q".to_string(),
        serde_json::Value::String(query.to_string()),
    );
    payload.insert("limit".to_string(), serde_json::Value::Number(limit.into()));
    payload.insert("k".to_string(), serde_json::Value::Number(limit.into()));
    payload.insert(
        "backend".to_string(),
        serde_json::Value::String(backend.to_string()),
    );
    if let Some(project) = project_filter {
        if !project.trim().is_empty() {
            payload.insert(
                "project".to_string(),
                serde_json::Value::String(project.trim().to_string()),
            );
        }
    }
    if let Some(topic) = topic_filter {
        if !topic.trim().is_empty() {
            payload.insert(
                "topic_path".to_string(),
                serde_json::Value::String(normalize_topic(topic)),
            );
        }
    }

    let mut request = state.client.post(url).json(&payload);
    if !api_key.is_empty() {
        request = request.header("x-api-key", api_key);
        request = request.header("Authorization", format!("Bearer {api_key}"));
    }
    let timeout_secs = state.cfg.external_timeout_for_backend(backend);
    let response = request
        .timeout(Duration::from_secs(timeout_secs))
        .send()
        .await
        .with_context(|| format!("{backend} request failed"))?;
    let status = response.status();
    let body = response
        .text()
        .await
        .unwrap_or_else(|_| String::from("{\"error\":\"unable to read body\"}"));
    if !status.is_success() {
        anyhow::bail!(
            "{backend} response failed: status={} body={}",
            status.as_u16(),
            body
        );
    }
    let payload: serde_json::Value =
        serde_json::from_str(&body).with_context(|| format!("parse {backend} response"))?;
    let rows = payload
        .get("results")
        .or_else(|| payload.get("rows"))
        .or_else(|| payload.get("hits"))
        .and_then(|value| value.as_array())
        .cloned()
        .unwrap_or_default();

    let mut out: Vec<SearchResult> = Vec::new();
    for row in rows {
        let project = row
            .get("project")
            .map(json_value_to_string)
            .filter(|value| !value.is_empty())
            .or_else(|| project_filter.map(|value| value.trim().to_string()))
            .unwrap_or_default();
        let file = row
            .get("file")
            .or_else(|| row.get("path"))
            .map(json_value_to_string)
            .unwrap_or_default();
        let summary = row
            .get("summary")
            .or_else(|| row.get("content"))
            .or_else(|| row.get("text"))
            .map(json_value_to_string)
            .unwrap_or_default();
        if project.is_empty() || file.is_empty() || summary.is_empty() {
            continue;
        }
        let score = row
            .get("score")
            .and_then(|value| value.as_f64())
            .map(|value| value as f32)
            .unwrap_or(0.0);
        let topic_path = row
            .get("topic_path")
            .or_else(|| row.get("topic"))
            .map(json_value_to_string)
            .map(|value| normalize_topic(&value))
            .filter(|value| !value.is_empty())
            .unwrap_or_else(|| derive_topic_path(&file));
        if !matches_project_topic(&project, &topic_path, project_filter, topic_filter) {
            continue;
        }
        out.push(SearchResult {
            project,
            file,
            summary,
            score,
            topic_path,
        });
    }
    out.sort_by(|a, b| b.score.partial_cmp(&a.score).unwrap_or(Ordering::Equal));
    if out.len() > limit {
        out.truncate(limit);
    }
    Ok(out)
}

async fn quickwit_search(
    state: &AppState,
    snapshot: &DocSnapshot,
    query: &str,
    limit: usize,
    project_filter: Option<&str>,
    topic_filter: Option<&str>,
) -> Result<Vec<SearchResult>> {
    {
        let cache = state.quickwit.read().await;
        if cache.fingerprint == snapshot.fingerprint {
            // cache already aligned with snapshot
        } else {
            drop(cache);
            let _guard = state.quickwit_build_lock.lock().await;
            let current = state.quickwit.read().await;
            let needs_build = current.fingerprint != snapshot.fingerprint;
            drop(current);
            if needs_build {
                let docs = snapshot.docs.clone();
                let built = tokio::task::spawn_blocking(move || build_quickwit_compat_index(&docs))
                    .await
                    .context("join quickwit-compat build")??;
                let mut cache_write = state.quickwit.write().await;
                cache_write.fingerprint = snapshot.fingerprint;
                cache_write.postings = built;
            }
        }
    }

    let cache = state.quickwit.read().await;
    let terms = tokenize(query);
    if terms.is_empty() {
        return Ok(Vec::new());
    }
    let docs = snapshot.docs.as_ref();
    let doc_count = docs.len().max(1) as f32;
    let mut scores: HashMap<usize, f32> = HashMap::new();
    for term in terms {
        if let Some(postings) = cache.postings.get(&term) {
            let df = postings.len().max(1) as f32;
            let idf = ((doc_count + 1.0) / (df + 1.0)).ln() + 1.0;
            for (doc_id, tf) in postings {
                let entry = scores.entry(*doc_id).or_insert(0.0);
                *entry += (*tf as f32) * idf;
            }
        }
    }

    let mut ranked: Vec<(usize, f32)> = scores.into_iter().collect();
    ranked.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(Ordering::Equal));
    let mut out = Vec::new();
    for (doc_id, score) in ranked {
        if let Some(row) = docs.get(doc_id) {
            if !matches_project_topic(&row.project, &row.topic_path, project_filter, topic_filter) {
                continue;
            }
            out.push(SearchResult {
                project: row.project.clone(),
                file: row.file.clone(),
                summary: row.summary.clone(),
                score,
                topic_path: row.topic_path.clone(),
            });
            if out.len() >= limit {
                break;
            }
        }
    }
    Ok(out)
}

fn build_quickwit_compat_index(docs: &[MemoryDoc]) -> Result<HashMap<String, Vec<(usize, u16)>>> {
    let mut postings: HashMap<String, Vec<(usize, u16)>> = HashMap::new();
    for (idx, row) in docs.iter().enumerate() {
        let mut term_freq: HashMap<String, u16> = HashMap::new();
        for term in tokenize(&format!("{} {} {}", row.file, row.topic_path, row.summary)) {
            let entry = term_freq.entry(term).or_insert(0);
            *entry = entry.saturating_add(1);
        }
        for (term, tf) in term_freq {
            postings.entry(term).or_default().push((idx, tf));
        }
    }
    Ok(postings)
}

async fn meili_search(
    state: &AppState,
    snapshot: &DocSnapshot,
    query: &str,
    limit: usize,
    project_filter: Option<&str>,
    topic_filter: Option<&str>,
) -> Result<Vec<SearchResult>> {
    ensure_meili_synced(state, snapshot).await?;
    let url = format!(
        "{}/indexes/{}/search",
        state.cfg.meili_url.trim_end_matches('/'),
        state.cfg.meili_index_uid
    );
    let mut payload = serde_json::json!({
        "q": query,
        "limit": limit.saturating_mul(12).max(64),
        "showRankingScore": true
    });
    if let Some(project) = project_filter {
        payload["filter"] =
            serde_json::json!(format!("project = \"{}\"", project.replace('"', "\\\"")));
    }
    let mut req = state.client.post(url).json(&payload);
    if !state.cfg.meili_api_key.is_empty() {
        req = req.header(
            "Authorization",
            format!("Bearer {}", state.cfg.meili_api_key),
        );
    }
    let resp = req.send().await.context("meili search request")?;
    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp
            .text()
            .await
            .unwrap_or_else(|_| "<unreadable>".to_string());
        anyhow::bail!("meili search failed: status={} body={}", status, body);
    }
    let payload: serde_json::Value = resp.json().await.context("parse meili search")?;
    let hits = payload
        .get("hits")
        .and_then(|v| v.as_array())
        .cloned()
        .unwrap_or_default();
    let mut out = Vec::new();
    for hit in hits {
        let project = hit
            .get("project")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        let file = hit
            .get("file")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        let summary = hit
            .get("summary")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        let topic_path = hit
            .get("topic_path")
            .and_then(|v| v.as_str())
            .unwrap_or_default()
            .to_string();
        if !matches_project_topic(&project, &topic_path, project_filter, topic_filter) {
            continue;
        }
        let score = hit
            .get("_rankingScore")
            .and_then(|v| v.as_f64())
            .unwrap_or(0.0) as f32;
        out.push(SearchResult {
            project,
            file,
            summary,
            score,
            topic_path,
        });
        if out.len() >= limit {
            break;
        }
    }
    Ok(out)
}

async fn ensure_meili_synced(state: &AppState, snapshot: &DocSnapshot) -> Result<()> {
    let should_sync = {
        let cache = state.meili.read().await;
        if cache.fingerprint != snapshot.fingerprint {
            true
        } else if let Some(last_sync) = cache.last_sync {
            last_sync.elapsed() > Duration::from_secs(state.cfg.meili_sync_secs)
        } else {
            true
        }
    };
    if !should_sync {
        return Ok(());
    }

    let _sync_guard = state.meili_sync_lock.lock().await;
    let should_sync = {
        let cache = state.meili.read().await;
        if cache.fingerprint != snapshot.fingerprint {
            true
        } else if let Some(last_sync) = cache.last_sync {
            last_sync.elapsed() > Duration::from_secs(state.cfg.meili_sync_secs)
        } else {
            true
        }
    };
    if !should_sync {
        return Ok(());
    }

    let index_url = format!("{}/indexes", state.cfg.meili_url.trim_end_matches('/'));
    let index_uid = state.cfg.meili_index_uid.clone();
    let mut create_req = state.client.post(index_url).json(&serde_json::json!({
        "uid": index_uid,
        "primaryKey": "id"
    }));
    if !state.cfg.meili_api_key.is_empty() {
        create_req = create_req.header(
            "Authorization",
            format!("Bearer {}", state.cfg.meili_api_key),
        );
    }
    let create_resp = create_req
        .send()
        .await
        .context("create meili index request")?;
    if create_resp.status().as_u16() != 409 {
        let create_task_uid = parse_meili_task_uid(create_resp, "create index").await?;
        if let Some(task_uid) = create_task_uid {
            wait_for_meili_task(state, task_uid, "create index").await?;
        }
    }

    let settings_url = format!(
        "{}/indexes/{}/settings/filterable-attributes",
        state.cfg.meili_url.trim_end_matches('/'),
        state.cfg.meili_index_uid
    );
    let mut settings_req = state
        .client
        .put(settings_url)
        .json(&serde_json::json!(["project", "topic_path"]));
    if !state.cfg.meili_api_key.is_empty() {
        settings_req = settings_req.header(
            "Authorization",
            format!("Bearer {}", state.cfg.meili_api_key),
        );
    }
    let settings_resp = settings_req
        .send()
        .await
        .context("set meili filterable attributes request")?;
    let settings_task_uid =
        parse_meili_task_uid(settings_resp, "set filterable attributes").await?;
    if let Some(task_uid) = settings_task_uid {
        wait_for_meili_task(state, task_uid, "set filterable attributes").await?;
    }

    let docs_url = format!(
        "{}/indexes/{}/documents",
        state.cfg.meili_url.trim_end_matches('/'),
        state.cfg.meili_index_uid
    );
    let docs_payload: Vec<serde_json::Value> = snapshot
        .docs
        .iter()
        .map(|row| {
            serde_json::json!({
                "id": meili_document_id(&row.id),
                "source_id": row.id,
                "project": row.project,
                "file": row.file,
                "topic_path": row.topic_path,
                "summary": row.summary,
            })
        })
        .collect();
    let mut docs_req = state.client.post(docs_url).json(&docs_payload);
    if !state.cfg.meili_api_key.is_empty() {
        docs_req = docs_req.header(
            "Authorization",
            format!("Bearer {}", state.cfg.meili_api_key),
        );
    }
    let docs_resp = docs_req.send().await.context("meili upsert docs request")?;
    let docs_task_uid = parse_meili_task_uid(docs_resp, "upsert documents").await?;
    if let Some(task_uid) = docs_task_uid {
        wait_for_meili_task(state, task_uid, "upsert documents").await?;
    }

    let mut cache = state.meili.write().await;
    cache.fingerprint = snapshot.fingerprint;
    cache.last_sync = Some(Instant::now());
    cache.last_error = None;
    Ok(())
}

async fn parse_meili_task_uid(resp: reqwest::Response, action: &str) -> Result<Option<u64>> {
    let status = resp.status();
    let body = resp
        .text()
        .await
        .unwrap_or_else(|_| "<unreadable>".to_string());
    if !status.is_success() {
        anyhow::bail!("meili {action} failed: status={status} body={body}");
    }
    if body.trim().is_empty() {
        return Ok(None);
    }
    let payload: serde_json::Value =
        serde_json::from_str(&body).context("parse meili task payload json")?;
    Ok(payload
        .get("taskUid")
        .and_then(|value| value.as_u64())
        .or_else(|| payload.get("uid").and_then(|value| value.as_u64())))
}

async fn wait_for_meili_task(state: &AppState, task_uid: u64, action: &str) -> Result<()> {
    let started = Instant::now();
    let timeout = Duration::from_secs(state.cfg.meili_task_timeout_secs.max(1));
    let task_url = format!(
        "{}/tasks/{}",
        state.cfg.meili_url.trim_end_matches('/'),
        task_uid
    );
    loop {
        let mut req = state.client.get(&task_url);
        if !state.cfg.meili_api_key.is_empty() {
            req = req.header(
                "Authorization",
                format!("Bearer {}", state.cfg.meili_api_key),
            );
        }
        let resp = req.send().await.context("meili task poll request")?;
        let status = resp.status();
        let body = resp
            .text()
            .await
            .unwrap_or_else(|_| "<unreadable>".to_string());
        if !status.is_success() {
            anyhow::bail!("meili task poll failed: status={status} body={body}");
        }
        let payload: serde_json::Value =
            serde_json::from_str(&body).context("parse meili task poll payload json")?;
        match payload
            .get("status")
            .and_then(|value| value.as_str())
            .unwrap_or_default()
        {
            "succeeded" => return Ok(()),
            "failed" | "canceled" => {
                let error_code = payload
                    .get("error")
                    .and_then(|value| value.get("code"))
                    .and_then(|value| value.as_str())
                    .unwrap_or_default();
                if action == "create index" && error_code == "index_already_exists" {
                    return Ok(());
                }
                anyhow::bail!("meili task failed: action={action} uid={task_uid} payload={payload}")
            }
            _ => {
                if started.elapsed() >= timeout {
                    anyhow::bail!(
                        "meili task timeout: action={action} uid={task_uid} timeout_secs={}",
                        timeout.as_secs()
                    );
                }
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }
    }
}

fn normalize_backend(input: &str) -> String {
    match input.trim().to_ascii_lowercase().as_str() {
        "helixdb" | "helixdb_spike" => "helixdb_spike".to_string(),
        "icm" | "icm_spike" => "icm_spike".to_string(),
        "lancedb" | "lancedb_spike" => "lancedb_spike".to_string(),
        "memvid" | "memvid_spike" => "memvid_spike".to_string(),
        "meilisearch_spike" => "meilisearch_spike".to_string(),
        "quickwit_spike" => "quickwit_spike".to_string(),
        "shodh" | "shodh_memory" | "shodh_spike" => "shodh_spike".to_string(),
        "surreal" | "surrealdb" | "surrealdb_spike" => "surrealdb_spike".to_string(),
        "tantivy_spike" => "tantivy_spike".to_string(),
        "trieve" | "trieve_spike" => "trieve_spike".to_string(),
        _ => "tantivy_spike".to_string(),
    }
}

fn normalize_http_route(raw: &str) -> String {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return "/search".to_string();
    }
    if trimmed.starts_with('/') {
        return trimmed.to_string();
    }
    format!("/{trimmed}")
}

fn json_value_to_string(value: &serde_json::Value) -> String {
    match value {
        serde_json::Value::Null => String::new(),
        serde_json::Value::String(s) => s.trim().to_string(),
        serde_json::Value::Number(n) => n.to_string(),
        serde_json::Value::Bool(flag) => flag.to_string(),
        other => other.to_string(),
    }
}

fn meili_document_id(raw: &str) -> String {
    let mut normalized = String::with_capacity(raw.len().min(511));
    let mut prev_underscore = false;
    for ch in raw.chars() {
        if ch.is_ascii_alphanumeric() || ch == '-' || ch == '_' {
            normalized.push(ch);
            prev_underscore = false;
        } else if !prev_underscore {
            normalized.push('_');
            prev_underscore = true;
        }
    }
    let mut base = normalized.trim_matches('_').to_string();
    if base.is_empty() {
        base.push_str("doc");
    }

    let mut hasher = DefaultHasher::new();
    raw.hash(&mut hasher);
    let suffix = format!("_{:x}", hasher.finish());

    let max_len = 511usize;
    if base.len() + suffix.len() > max_len {
        let keep_len = max_len.saturating_sub(suffix.len());
        base.truncate(keep_len);
        while base.ends_with('_') {
            base.pop();
        }
    }
    format!("{base}{suffix}")
}

fn normalize_text(input: &str) -> String {
    let mut out = String::with_capacity(input.len().min(2048));
    let mut prev_space = false;
    for ch in input.chars() {
        let c = if ch.is_control() { ' ' } else { ch };
        if c.is_whitespace() {
            if !prev_space {
                out.push(' ');
            }
            prev_space = true;
        } else {
            out.push(c);
            prev_space = false;
        }
    }
    out.trim().to_string()
}

fn tokenize(input: &str) -> Vec<String> {
    let mut cleaned = String::with_capacity(input.len());
    for ch in normalize_text(input).chars() {
        if ch.is_ascii_alphanumeric() || ch == '_' {
            cleaned.push(ch.to_ascii_lowercase());
        } else {
            cleaned.push(' ');
        }
    }
    cleaned
        .split_whitespace()
        .map(|token| token.to_string())
        .filter(|token| token.len() >= 2)
        .collect()
}

fn normalize_topic(topic: &str) -> String {
    topic
        .trim_matches('/')
        .replace('\\', "/")
        .split('/')
        .filter(|segment| !segment.is_empty())
        .collect::<Vec<_>>()
        .join("/")
}

fn derive_topic_path(file_path: &str) -> String {
    let normalized = file_path.replace('\\', "/");
    let parent = Path::new(&normalized)
        .parent()
        .map(|p| p.to_string_lossy().to_string())
        .unwrap_or_default();
    let topic = normalize_topic(&parent);
    if topic.is_empty() {
        "general".to_string()
    } else {
        topic
    }
}

fn matches_project_topic(
    project: &str,
    topic_path: &str,
    project_filter: Option<&str>,
    topic_filter: Option<&str>,
) -> bool {
    if let Some(project_filter) = project_filter {
        if !project.eq_ignore_ascii_case(project_filter) {
            return false;
        }
    }
    if let Some(topic_filter) = topic_filter {
        let normalized_topic = normalize_topic(topic_path);
        if !normalized_topic.starts_with(topic_filter) {
            return false;
        }
    }
    true
}

fn env_string(key: &str, default: &str) -> String {
    env::var(key).unwrap_or_else(|_| default.to_string())
}

fn env_u64(key: &str, default: u64) -> u64 {
    env::var(key)
        .ok()
        .and_then(|raw| raw.parse::<u64>().ok())
        .unwrap_or(default)
}

fn env_u16(key: &str, default: u16) -> u16 {
    env::var(key)
        .ok()
        .and_then(|raw| raw.parse::<u16>().ok())
        .unwrap_or(default)
}

fn env_usize(key: &str, default: usize) -> usize {
    env::var(key)
        .ok()
        .and_then(|raw| raw.parse::<usize>().ok())
        .unwrap_or(default)
}
