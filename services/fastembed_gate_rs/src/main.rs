use std::collections::BTreeMap;
use std::env;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use chrono::Utc;
use reqwest::StatusCode;
use serde::Serialize;
use tokio::fs;
use tokio::time::sleep;

#[derive(Clone)]
struct Config {
    base_url: String,
    api_key: String,
    interval_secs: f64,
    min_pause_secs: f64,
    failure_retry_secs: f64,
    timeout_secs: f64,
    output_path: PathBuf,
    gate_output_path: PathBuf,
    max_latency_ms: f64,
}

impl Config {
    fn from_env() -> Self {
        Self {
            base_url: env_string("GATE_REFRESH_BASE_URL", "http://gateway-go:8091"),
            api_key: env_string("GATE_REFRESH_API_KEY", ""),
            interval_secs: env_f64("GATE_REFRESH_INTERVAL_SECS", 1800.0).max(15.0),
            min_pause_secs: env_f64("GATE_REFRESH_MIN_PAUSE_SECS", 30.0).max(5.0),
            failure_retry_secs: env_f64("GATE_REFRESH_FAILURE_RETRY_SECS", 45.0).max(5.0),
            timeout_secs: env_f64("GATE_REFRESH_TIMEOUT_SECS", 45.0).max(2.0),
            output_path: PathBuf::from(env_string(
                "GATE_REFRESH_OUTPUT",
                "/app/data/bench/perf_shortlist_matrix_latest.json",
            )),
            gate_output_path: PathBuf::from(env_string(
                "GATE_REFRESH_GATE_OUTPUT",
                "/app/data/gates/fastembed_gate_latest.json",
            )),
            max_latency_ms: env_f64("GATE_REFRESH_MAX_HEALTH_LATENCY_MS", 1200.0).max(50.0),
        }
    }
}

#[derive(Serialize)]
struct GatePayload {
    #[serde(rename = "generatedAt")]
    generated_at: String,
    passed: bool,
    reason: String,
    metrics: BTreeMap<String, serde_json::Value>,
    thresholds: BTreeMap<String, serde_json::Value>,
}

#[derive(Serialize)]
struct SnapshotPayload {
    #[serde(rename = "generatedAt")]
    generated_at: String,
    base_url: String,
    passed: bool,
    reason: String,
    latency_ms: f64,
    status_code: u16,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            std::env::var("RUST_LOG").unwrap_or_else(|_| "fastembed_gate_rs=info,info".into()),
        )
        .with_target(false)
        .compact()
        .init();

    let cfg = Config::from_env();
    let timeout = Duration::from_secs_f64(cfg.timeout_secs);
    let client = reqwest::Client::builder()
        .timeout(timeout)
        .build()
        .context("build reqwest client")?;

    loop {
        let started = Instant::now();
        let run = run_once(&cfg, &client).await;
        let elapsed = started.elapsed().as_secs_f64();
        let sleep_for = if run.is_ok() {
            (cfg.interval_secs - elapsed).max(cfg.min_pause_secs)
        } else {
            cfg.failure_retry_secs.max(cfg.min_pause_secs)
        };
        sleep(Duration::from_secs_f64(sleep_for)).await;
    }
}

async fn run_once(cfg: &Config, client: &reqwest::Client) -> Result<()> {
    let health_url = format!("{}/health", cfg.base_url.trim_end_matches('/'));
    let started = Instant::now();
    let mut req = client.get(&health_url);
    if !cfg.api_key.is_empty() {
        req = req.header("x-api-key", &cfg.api_key);
        req = req.header("authorization", format!("Bearer {}", cfg.api_key));
    }

    let resp = req.send().await;
    let latency_ms = started.elapsed().as_secs_f64() * 1000.0;
    let now = Utc::now().to_rfc3339();

    let (status, passed, reason) = match resp {
        Ok(response) => {
            let status = response.status();
            if status == StatusCode::OK && latency_ms <= cfg.max_latency_ms {
                (status, true, "ok".to_string())
            } else if status == StatusCode::OK {
                (
                    status,
                    false,
                    format!("health_latency_exceeded:{:.2}ms", latency_ms),
                )
            } else {
                (
                    status,
                    false,
                    format!("health_status_{}", status.as_u16()),
                )
            }
        }
        Err(err) => (
            StatusCode::BAD_GATEWAY,
            false,
            format!("health_request_failed:{err}"),
        ),
    };

    let mut metrics = BTreeMap::new();
    metrics.insert(
        "healthLatencyMs".to_string(),
        serde_json::Value::from((latency_ms * 100.0).round() / 100.0),
    );
    metrics.insert(
        "healthStatusCode".to_string(),
        serde_json::Value::from(status.as_u16()),
    );

    let mut thresholds = BTreeMap::new();
    thresholds.insert(
        "maxHealthLatencyMs".to_string(),
        serde_json::Value::from(cfg.max_latency_ms),
    );
    thresholds.insert("requiredHealthStatus".to_string(), serde_json::Value::from(200));

    let gate_payload = GatePayload {
        generated_at: now.clone(),
        passed,
        reason: reason.clone(),
        metrics,
        thresholds,
    };
    write_json_atomic(&cfg.gate_output_path, &gate_payload).await?;

    let snapshot = SnapshotPayload {
        generated_at: now,
        base_url: cfg.base_url.clone(),
        passed,
        reason,
        latency_ms,
        status_code: status.as_u16(),
    };
    write_json_atomic(&cfg.output_path, &snapshot).await?;

    tracing::info!(
        passed,
        status = status.as_u16(),
        latency_ms = format!("{latency_ms:.2}"),
        gate_output = %cfg.gate_output_path.display(),
        "fastembed gate refresh"
    );

    if passed {
        Ok(())
    } else {
        anyhow::bail!("gate probe failed")
    }
}

async fn write_json_atomic<T: Serialize>(path: &Path, payload: &T) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .await
            .with_context(|| format!("create parent dir {}", parent.display()))?;
    }
    let raw = serde_json::to_vec_pretty(payload).context("serialize payload")?;
    let tmp = path.with_extension("tmp");
    fs::write(&tmp, raw)
        .await
        .with_context(|| format!("write temp {}", tmp.display()))?;
    fs::rename(&tmp, path)
        .await
        .with_context(|| format!("rename {} -> {}", tmp.display(), path.display()))?;
    Ok(())
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

fn env_f64(name: &str, default: f64) -> f64 {
    env::var(name)
        .ok()
        .and_then(|raw| raw.trim().parse::<f64>().ok())
        .unwrap_or(default)
}
