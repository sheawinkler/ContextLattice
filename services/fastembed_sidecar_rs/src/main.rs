use std::collections::HashMap;
use std::env;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use anyhow::{anyhow, Context, Result};
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use fastembed::{EmbeddingModel, InitOptions, TextEmbedding};
use serde::{Deserialize, Serialize};
use tokio::net::TcpListener;

#[cfg(not(target_env = "msvc"))]
#[global_allocator]
static GLOBAL_ALLOCATOR: mimalloc::MiMalloc = mimalloc::MiMalloc;

#[derive(Clone)]
struct Config {
    port: u16,
    default_model: String,
    cache_dir: PathBuf,
    max_batch: usize,
}

impl Config {
    fn from_env() -> Self {
        let port = env_u16("PORT", 8080);
        let default_model = env_string("FASTEMBED_DEFAULT_MODEL", "BAAI/bge-small-en-v1.5");
        let cache_dir = PathBuf::from(env_string("FASTEMBED_CACHE_DIR", "/models"));
        let max_batch = env_usize("FASTEMBED_MAX_BATCH", 256).max(1);
        Self {
            port,
            default_model,
            cache_dir,
            max_batch,
        }
    }
}

type SharedModel = Arc<Mutex<TextEmbedding>>;

#[derive(Clone)]
struct AppState {
    cfg: Config,
    models: Arc<Mutex<HashMap<String, SharedModel>>>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum InputValue {
    Single(String),
    Many(Vec<String>),
}

#[derive(Deserialize)]
struct EmbedRequest {
    input: InputValue,
    #[serde(default)]
    model: Option<String>,
}

#[derive(Serialize)]
struct EmbedResponse {
    vectors: Vec<Vec<f32>>,
    model: String,
}

#[derive(Serialize)]
struct HealthResponse {
    ok: bool,
    #[serde(rename = "defaultModel")]
    default_model: String,
    #[serde(rename = "cacheDir")]
    cache_dir: String,
    #[serde(rename = "loadedModels")]
    loaded_models: Vec<String>,
}

#[derive(Serialize)]
struct ErrorResponse {
    detail: String,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            std::env::var("RUST_LOG").unwrap_or_else(|_| "fastembed_sidecar_rs=info,info".into()),
        )
        .with_target(false)
        .compact()
        .init();

    let cfg = Config::from_env();
    let state = AppState {
        cfg,
        models: Arc::new(Mutex::new(HashMap::new())),
    };

    let app = Router::new()
        .route("/health", get(health))
        .route("/embed", post(embed))
        .with_state(state.clone());

    let bind = format!("0.0.0.0:{}", state.cfg.port);
    let listener = TcpListener::bind(&bind)
        .await
        .with_context(|| format!("bind {bind}"))?;
    tracing::info!(%bind, model=%state.cfg.default_model, "fastembed-sidecar-rs listening");
    axum::serve(listener, app).await.context("serve")
}

async fn health(State(state): State<AppState>) -> impl IntoResponse {
    let loaded_models = {
        let models = state.models.lock().expect("models lock");
        let mut keys: Vec<String> = models.keys().cloned().collect();
        keys.sort();
        keys
    };
    (
        StatusCode::OK,
        Json(HealthResponse {
            ok: true,
            default_model: state.cfg.default_model.clone(),
            cache_dir: state.cfg.cache_dir.to_string_lossy().to_string(),
            loaded_models,
        }),
    )
}

async fn embed(
    State(state): State<AppState>,
    Json(req): Json<EmbedRequest>,
) -> impl IntoResponse {
    match embed_impl(state, req).await {
        Ok(resp) => (StatusCode::OK, Json(resp)).into_response(),
        Err(err) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(ErrorResponse {
                detail: format!("embedding failed: {err}"),
            }),
        )
            .into_response(),
    }
}

async fn embed_impl(state: AppState, req: EmbedRequest) -> Result<EmbedResponse> {
    let model_name = resolve_model_name(&state.cfg.default_model, req.model.as_deref());
    let inputs = normalize_inputs(req.input, state.cfg.max_batch)?;

    let shared = get_or_load_model(&state, &model_name).await?;
    let docs = inputs.clone();
    let vectors = tokio::task::spawn_blocking(move || {
        let mut guard = shared.lock().expect("model lock");
        guard.embed(docs, None).map_err(|err| anyhow!(err.to_string()))
    })
    .await
    .context("join embedding worker")??;

    if vectors.len() < inputs.len() {
        return Err(anyhow!(
            "embedding returned {} vectors for {} inputs",
            vectors.len(),
            inputs.len()
        ));
    }

    let mut out: Vec<Vec<f32>> = Vec::with_capacity(inputs.len());
    for row in vectors.into_iter().take(inputs.len()) {
        out.push(row.into_iter().map(|v| v as f32).collect());
    }

    Ok(EmbedResponse {
        vectors: out,
        model: model_name,
    })
}

async fn get_or_load_model(state: &AppState, model_name: &str) -> Result<SharedModel> {
    {
        let models = state.models.lock().expect("models lock");
        if let Some(existing) = models.get(model_name) {
            return Ok(existing.clone());
        }
    }

    let model_name_owned = model_name.to_string();
    let cache_dir = state.cfg.cache_dir.clone();
    let loaded = tokio::task::spawn_blocking(move || load_model(&model_name_owned, cache_dir))
        .await
        .context("join model loader")??;
    let wrapped = Arc::new(Mutex::new(loaded));

    let mut models = state.models.lock().expect("models lock");
    let entry = models
        .entry(model_name.to_string())
        .or_insert_with(|| wrapped.clone());
    Ok(entry.clone())
}

fn load_model(model_name: &str, cache_dir: PathBuf) -> Result<TextEmbedding> {
    let model = parse_embedding_model(model_name)
        .ok_or_else(|| anyhow!("unsupported model {model_name} for fastembed-sidecar-rs"))?;
    let opts = InitOptions::new(model)
        .with_cache_dir(cache_dir)
        .with_show_download_progress(false);
    TextEmbedding::try_new(opts).context("fastembed init")
}

fn parse_embedding_model(name: &str) -> Option<EmbeddingModel> {
    match name.trim().to_ascii_lowercase().as_str() {
        "baai/bge-small-en-v1.5" | "bge-small-en-v1.5" => Some(EmbeddingModel::BGESmallENV15),
        "baai/bge-base-en-v1.5" | "bge-base-en-v1.5" => Some(EmbeddingModel::BGEBaseENV15),
        "baai/bge-large-en-v1.5" | "bge-large-en-v1.5" => Some(EmbeddingModel::BGELargeENV15),
        "sentence-transformers/all-minilm-l6-v2" | "all-minilm-l6-v2" => {
            Some(EmbeddingModel::AllMiniLML6V2)
        }
        "nomic-ai/nomic-embed-text-v1" | "nomic-embed-text-v1" => {
            Some(EmbeddingModel::NomicEmbedTextV1)
        }
        "nomic-ai/nomic-embed-text-v1.5" | "nomic-embed-text-v1.5" => {
            Some(EmbeddingModel::NomicEmbedTextV15)
        }
        _ => None,
    }
}

fn normalize_inputs(input: InputValue, max_batch: usize) -> Result<Vec<String>> {
    let iter: Box<dyn Iterator<Item = String>> = match input {
        InputValue::Single(s) => Box::new(std::iter::once(s)),
        InputValue::Many(v) => Box::new(v.into_iter()),
    };

    let mut rows: Vec<String> = Vec::new();
    for item in iter {
        let trimmed = item.trim();
        if trimmed.is_empty() {
            continue;
        }
        rows.push(trimmed.to_string());
        if rows.len() >= max_batch {
            break;
        }
    }
    if rows.is_empty() {
        return Err(anyhow!("input must include at least one non-empty string"));
    }
    Ok(rows)
}

fn resolve_model_name(default_model: &str, requested: Option<&str>) -> String {
    let requested = requested.unwrap_or_default().trim();
    if requested.is_empty() {
        return default_model.trim().to_string();
    }
    requested.to_string()
}

fn env_string(name: &str, default: &str) -> String {
    let value = env::var(name).unwrap_or_default();
    let value = value.trim();
    if value.is_empty() {
        default.to_string()
    } else {
        value.to_string()
    }
}

fn env_u16(name: &str, default: u16) -> u16 {
    env::var(name)
        .ok()
        .and_then(|raw| raw.trim().parse::<u16>().ok())
        .unwrap_or(default)
}

fn env_usize(name: &str, default: usize) -> usize {
    env::var(name)
        .ok()
        .and_then(|raw| raw.trim().parse::<usize>().ok())
        .unwrap_or(default)
}
