use std::collections::{BTreeMap, BTreeSet, HashMap, HashSet};
use std::fs::{self, File, OpenOptions};
use std::io::{BufRead, BufReader, BufWriter, Read, Write};
use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::{anyhow, Result};
use chrono::{DateTime, Datelike, Duration as ChronoDuration, NaiveDate, NaiveDateTime, Utc};
use clap::{Args, Parser, Subcommand};
use flate2::write::GzEncoder;
use flate2::Compression;
use reqwest::blocking::Client;
use reqwest::header::{HeaderMap, HeaderValue, ACCEPT};
use rusqlite::Connection;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use walkdir::WalkDir;

#[derive(Parser)]
#[command(name = "context_storage_ops")]
#[command(about = "ContextLattice Rust storage operations", long_about = None)]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    Ledger(LedgerArgs),
    WeeklyLineage(WeeklyLineageArgs),
    ColdPack(ColdPackArgs),
    ColdTier(ColdTierArgs),
    ArchiveNdjson(ArchiveNdjsonArgs),
    FanoutGc(FanoutGcArgs),
}

#[derive(Args)]
struct LedgerArgs {
    #[arg(long, default_value_t = default_orchestrator_url())]
    orchestrator_url: String,
    #[arg(long, default_value_t = String::new())]
    api_key: String,
    #[arg(long, default_value_t = default_ledger_path())]
    out: String,
    #[arg(long, default_value_t = 20.0)]
    timeout_secs: f64,
    #[arg(long, default_value_t = 180)]
    keep_days: i64,
    #[arg(long, default_value_t = 128 * 1024 * 1024)]
    max_bytes: usize,
    #[arg(long, default_value_t = 24)]
    tracked_top_limit: usize,
    #[arg(long, default_value_t = false)]
    prune_only: bool,
    #[arg(long, default_value_t = false)]
    pretty: bool,
}

#[derive(Args)]
struct WeeklyLineageArgs {
    #[arg(long, default_value_t = default_orchestrator_url())]
    orchestrator_url: String,
    #[arg(long, default_value_t = String::new())]
    api_key: String,
    #[arg(long, default_value_t = default_memory_root())]
    memory_root: String,
    #[arg(long, default_value_t = default_lineage_root())]
    out_root: String,
    #[arg(long, default_value_t = current_week_id())]
    week_id: String,
    #[arg(long = "project")]
    projects: Vec<String>,
    #[arg(long, default_value_t = 1)]
    min_count: usize,
    #[arg(long, default_value_t = 2000)]
    page_limit: usize,
    #[arg(long, default_value_t = 60)]
    top_topic_limit: usize,
    #[arg(long, default_value_t = 2)]
    synergy_min_projects: usize,
    #[arg(long, default_value_t = 104)]
    keep_weeks: usize,
    #[arg(long, default_value_t = false)]
    emit_synergy: bool,
    #[arg(long, default_value_t = false)]
    dry_run: bool,
    #[arg(long, default_value_t = false)]
    pretty: bool,
    #[arg(long, default_value_t = 25.0)]
    timeout_secs: f64,
}

#[derive(Args)]
struct ColdPackArgs {
    #[arg(long, default_value_t = default_cold_root())]
    cold_root: String,
    #[arg(long, default_value_t = 3)]
    level: i32,
    #[arg(long, default_value_t = 0)]
    max_files: usize,
    #[arg(long, default_value_t = false)]
    keep_original: bool,
    #[arg(long, default_value_t = true)]
    verify: bool,
    #[arg(long, default_value_t = false)]
    no_verify: bool,
    #[arg(long, default_value_t = false)]
    apply: bool,
    #[arg(long, default_value_t = default_cold_catalog())]
    catalog: String,
}

#[derive(Args)]
struct ColdTierArgs {
    #[arg(long, default_value_t = default_cold_root())]
    cold_root: String,
    #[arg(long, default_value_t = 6)]
    keep_latest: usize,
    #[arg(long, default_value_t = 21)]
    keep_daily: i64,
    #[arg(long, default_value_t = 12)]
    keep_weekly: usize,
    #[arg(long, default_value_t = 12)]
    keep_monthly: usize,
    #[arg(long, default_value_t = false)]
    apply: bool,
}

#[derive(Args)]
struct ArchiveNdjsonArgs {
    #[arg(long = "file")]
    files: Vec<String>,
    #[arg(long, default_value_t = String::new())]
    data_dir: String,
    #[arg(long, default_value_t = 48)]
    retention_hours: i64,
    #[arg(long, default_value_t = default_cold_telemetry_root())]
    cold_dir: String,
    #[arg(long, default_value_t = String::from("timestamp"))]
    timestamp_field: String,
}

#[derive(Args)]
struct FanoutGcArgs {
    #[arg(long)]
    db_path: Option<String>,
    #[arg(long, default_value_t = 24)]
    succeeded_retention_hours: i64,
    #[arg(long, default_value_t = 168)]
    failed_retention_hours: i64,
    #[arg(long, default_value_t = 24)]
    stale_pending_hours: i64,
    #[arg(long, default_value_t = String::new())]
    stale_targets: String,
    #[arg(long, default_value_t = false)]
    vacuum: bool,
    #[arg(long, default_value_t = 500)]
    vacuum_min_deleted: i64,
    #[arg(long, default_value_t = 15.0)]
    timeout_secs: f64,
    #[arg(long, default_value_t = false)]
    dry_run: bool,
}

fn main() {
    if let Err(err) = run() {
        eprintln!(
            "{}",
            serde_json::to_string(&json!({"ok": false, "error": err.to_string()}))
                .unwrap_or_else(|_| "{\"ok\":false}".to_string())
        );
        std::process::exit(1);
    }
}

fn run() -> Result<()> {
    load_local_dotenv();
    let cli = Cli::parse();
    match cli.command {
        Commands::Ledger(args) => run_ledger(args),
        Commands::WeeklyLineage(args) => run_weekly_lineage(args),
        Commands::ColdPack(args) => run_cold_pack(args),
        Commands::ColdTier(args) => run_cold_tier(args),
        Commands::ArchiveNdjson(args) => run_archive_ndjson(args),
        Commands::FanoutGc(args) => run_fanout_gc(args),
    }
}

fn default_orchestrator_url() -> String {
    std::env::var("CONTEXTLATTICE_ORCHESTRATOR_URL")
        .ok()
        .or_else(|| std::env::var("MEMMCP_ORCHESTRATOR_URL").ok())
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| "http://127.0.0.1:8075".to_string())
}

fn default_orchestrator_api_key() -> String {
    std::env::var("CONTEXTLATTICE_ORCHESTRATOR_API_KEY")
        .ok()
        .or_else(|| std::env::var("MEMMCP_ORCHESTRATOR_API_KEY").ok())
        .unwrap_or_default()
}

fn default_memory_root() -> String {
    for candidate in [
        std::env::var("GO_MEMORY_STORE_ROOT").ok(),
        std::env::var("MEMORY_BANK_ROOT").ok(),
        std::env::var("CONTEXTLATTICE_MEMORY_ROOT").ok(),
    ] {
        if let Some(value) = candidate {
            let trimmed = value.trim();
            if !trimmed.is_empty() {
                return trimmed.to_string();
            }
        }
    }
    if let Ok(memory_bank_data) = std::env::var("MEMORY_BANK_DATA") {
        let trimmed = memory_bank_data.trim();
        if !trimmed.is_empty() {
            return Path::new(trimmed)
                .join("memory-bank")
                .to_string_lossy()
                .to_string();
        }
    }
    "/tmp/contextlattice-memory-bank".to_string()
}

fn default_lineage_root() -> String {
    if let Ok(explicit) = std::env::var("CONTEXTLATTICE_LINEAGE_ROOT") {
        let trimmed = explicit.trim();
        if !trimmed.is_empty() {
            return trimmed.to_string();
        }
    }
    if let Ok(cold_root) = std::env::var("CONTEXTLATTICE_COLD_ROOT") {
        let trimmed = cold_root.trim();
        if !trimmed.is_empty() {
            return Path::new(trimmed)
                .join("lineage")
                .to_string_lossy()
                .to_string();
        }
    }
    "./.data/cold/lineage".to_string()
}

fn default_cold_root() -> String {
    std::env::var("CONTEXTLATTICE_COLD_ROOT")
        .ok()
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| "./.data/cold".to_string())
}

fn default_cold_catalog() -> String {
    std::env::var("COLD_SNAPSHOT_CATALOG")
        .ok()
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| "_index/cold_snapshot_catalog.jsonl".to_string())
}

fn default_cold_telemetry_root() -> String {
    std::env::var("CONTEXTLATTICE_COLD_ROOT")
        .ok()
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| "./.data/cold/telemetry".to_string())
}

fn default_ledger_path() -> String {
    if let Ok(explicit) = std::env::var("ORCH_STORAGE_LEDGER_PATH") {
        let trimmed = explicit.trim();
        if !trimmed.is_empty() {
            return trimmed.to_string();
        }
    }
    if let Ok(go_root) = std::env::var("GO_MEMORY_STORE_ROOT") {
        let trimmed = go_root.trim();
        if !trimmed.is_empty() {
            return Path::new(trimmed)
                .join("_contextlattice")
                .join("storage_ledger.ndjson")
                .to_string_lossy()
                .to_string();
        }
    }
    if let Ok(memory_bank_data) = std::env::var("MEMORY_BANK_DATA") {
        let trimmed = memory_bank_data.trim();
        if !trimmed.is_empty() {
            return Path::new(trimmed)
                .join("memory-bank")
                .join("_contextlattice")
                .join("storage_ledger.ndjson")
                .to_string_lossy()
                .to_string();
        }
    }
    if let Ok(cold_root) = std::env::var("CONTEXTLATTICE_COLD_ROOT") {
        let trimmed = cold_root.trim();
        if !trimmed.is_empty() {
            return Path::new(trimmed)
                .join("storage")
                .join("storage_ledger.ndjson")
                .to_string_lossy()
                .to_string();
        }
    }
    "./.data/orchestrator/storage_ledger.ndjson".to_string()
}

fn load_local_dotenv() {
    let env_path = Path::new(".env");
    if !env_path.exists() {
        return;
    }
    if let Ok(contents) = fs::read_to_string(env_path) {
        for raw_line in contents.lines() {
            let line = raw_line.trim();
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            if let Some((key, value)) = line.split_once('=') {
                let key = key.trim();
                if key.is_empty() || std::env::var(key).is_ok() {
                    continue;
                }
                let mut value = value.trim().to_string();
                if value.len() >= 2 {
                    let bytes = value.as_bytes();
                    if (bytes[0] == b'"' && bytes[value.len() - 1] == b'"')
                        || (bytes[0] == b'\'' && bytes[value.len() - 1] == b'\'')
                    {
                        value = value[1..value.len() - 1].to_string();
                    }
                }
                std::env::set_var(key, value);
            }
        }
    }
}

fn parse_iso_utc(raw: &str) -> Option<DateTime<Utc>> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Ok(ts) = DateTime::parse_from_rfc3339(trimmed) {
        return Some(ts.with_timezone(&Utc));
    }
    if trimmed.ends_with('Z') {
        let replaced = format!("{}+00:00", &trimmed[..trimmed.len() - 1]);
        if let Ok(ts) = DateTime::parse_from_rfc3339(&replaced) {
            return Some(ts.with_timezone(&Utc));
        }
    }
    None
}

fn print_json(payload: &Value, pretty: bool) -> Result<()> {
    if pretty {
        println!("{}", serde_json::to_string_pretty(payload)?);
    } else {
        println!("{}", serde_json::to_string(payload)?);
    }
    Ok(())
}

fn atomic_write_lines(path: &Path, lines: &[String]) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let tmp_path = path.with_extension(format!(
        "{}.tmp",
        path.extension().and_then(|x| x.to_str()).unwrap_or("tmp")
    ));
    {
        let file = File::create(&tmp_path)?;
        let mut writer = BufWriter::new(file);
        for line in lines {
            writer.write_all(line.as_bytes())?;
            writer.write_all(b"\n")?;
        }
        writer.flush()?;
    }
    fs::rename(tmp_path, path)?;
    Ok(())
}

fn prune_ndjson(path: &Path, keep_days: i64, max_bytes: usize) -> Result<Value> {
    if !path.exists() {
        return Ok(json!({"pruned": false, "reason": "missing"}));
    }
    let file = File::open(path)?;
    let reader = BufReader::new(file);
    let cutoff = Utc::now() - ChronoDuration::days(std::cmp::max(1, keep_days));
    let mut kept: Vec<String> = Vec::new();

    for line in reader.lines() {
        let raw = line?;
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            continue;
        }
        let keep = match serde_json::from_str::<Value>(trimmed) {
            Ok(v) => {
                let ts = v
                    .get("captured_at")
                    .or_else(|| v.get("timestamp"))
                    .and_then(|x| x.as_str())
                    .and_then(parse_iso_utc);
                match ts {
                    Some(t) => t >= cutoff,
                    None => true,
                }
            }
            Err(_) => false,
        };
        if keep {
            kept.push(trimmed.to_string());
        }
    }

    if max_bytes > 0 {
        let mut size: usize = kept.iter().map(|s| s.as_bytes().len() + 1).sum();
        if size > max_bytes {
            let mut trimmed: Vec<String> = Vec::new();
            let mut running = 0usize;
            for item in kept.iter().rev() {
                let row_size = item.as_bytes().len() + 1;
                if running + row_size > max_bytes {
                    break;
                }
                trimmed.push(item.clone());
                running += row_size;
            }
            trimmed.reverse();
            kept = trimmed;
            size = running;
        }
        let _ = size;
    }

    atomic_write_lines(path, &kept)?;
    Ok(json!({
        "pruned": true,
        "lines": kept.len(),
    }))
}

fn http_client(api_key: &str, timeout_secs: f64) -> Result<Client> {
    let mut headers = HeaderMap::new();
    headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
    if !api_key.trim().is_empty() {
        headers.insert("x-api-key", HeaderValue::from_str(api_key.trim())?);
    }
    let client = Client::builder()
        .timeout(Duration::from_secs_f64(timeout_secs.max(2.0)))
        .default_headers(headers)
        .build()?;
    Ok(client)
}

fn run_ledger(args: LedgerArgs) -> Result<()> {
    let out_path = PathBuf::from(args.out.trim());
    let api_key = if args.api_key.trim().is_empty() {
        default_orchestrator_api_key()
    } else {
        args.api_key.clone()
    };
    let prune = prune_ndjson(&out_path, args.keep_days, args.max_bytes)?;
    if args.prune_only {
        return print_json(
            &json!({"ok": true, "path": out_path, "prune": prune}),
            args.pretty,
        );
    }

    let client = http_client(&api_key, args.timeout_secs)?;
    let base = args.orchestrator_url.trim_end_matches('/');
    let storage_payload: Value = client
        .get(format!("{}/telemetry/storage", base))
        .send()?
        .error_for_status()?
        .json()?;
    let status_payload: Value = client
        .get(format!("{}/status", base))
        .send()?
        .error_for_status()?
        .json()?;

    let disk = storage_payload
        .get("disk")
        .cloned()
        .unwrap_or_else(|| json!({}));
    let tracked = storage_payload
        .get("trackedArtifacts")
        .cloned()
        .unwrap_or_else(|| json!({}));
    let governance = storage_payload
        .get("storageGovernance")
        .cloned()
        .unwrap_or_else(|| json!({}));

    let services = status_payload
        .get("services")
        .and_then(|s| s.as_array())
        .cloned()
        .unwrap_or_default();
    let healthy_services = services
        .iter()
        .filter(|svc| {
            svc.get("healthy")
                .and_then(|v| v.as_bool())
                .unwrap_or(false)
        })
        .count();

    let mut top_artifacts = Vec::new();
    if let Some(obj) = tracked.as_object() {
        for (name, raw) in obj {
            if name == "_total" {
                continue;
            }
            if let Some(row) = raw.as_object() {
                top_artifacts.push(json!({
                    "name": name,
                    "bytes": row.get("bytes").and_then(|v| v.as_i64()).unwrap_or(0),
                    "exists": row.get("exists").and_then(|v| v.as_bool()).unwrap_or(false),
                    "truncated": row.get("truncated").and_then(|v| v.as_bool()).unwrap_or(false),
                    "error": row.get("error").and_then(|v| v.as_str()).unwrap_or("")
                }));
            }
        }
    }
    top_artifacts.sort_by(|a, b| {
        b.get("bytes")
            .and_then(|v| v.as_i64())
            .unwrap_or(0)
            .cmp(&a.get("bytes").and_then(|v| v.as_i64()).unwrap_or(0))
    });
    top_artifacts.truncate(args.tracked_top_limit);

    let captured_at = Utc::now().to_rfc3339().replace("+00:00", "Z");
    let snapshot = json!({
        "captured_at": captured_at,
        "schema_version": 1,
        "source": "telemetry/storage",
        "orchestrator_url": args.orchestrator_url,
        "service_health": {"healthy": healthy_services, "total": services.len()},
        "storage": {
            "pressure_band": governance.get("pressureBand").cloned().unwrap_or_else(|| json!("unknown")),
            "disk": {
                "root": disk.get("root").cloned().unwrap_or_else(|| json!("")),
                "used_ratio": disk.get("usedRatio").cloned().unwrap_or_else(|| json!(0.0)),
                "used_bytes": disk.get("usedBytes").cloned().unwrap_or_else(|| json!(0)),
                "free_bytes": disk.get("freeBytes").cloned().unwrap_or_else(|| json!(0)),
                "total_bytes": disk.get("totalBytes").cloned().unwrap_or_else(|| json!(0)),
            },
            "tracked": {
                "total_bytes": tracked.get("_total").and_then(|v| v.get("bytes")).cloned().unwrap_or_else(|| json!(0)),
                "top_artifacts": top_artifacts,
            }
        }
    });

    if let Some(parent) = out_path.parent() {
        fs::create_dir_all(parent)?;
    }
    {
        let mut file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&out_path)?;
        file.write_all(serde_json::to_string(&snapshot)?.as_bytes())?;
        file.write_all(b"\n")?;
    }

    let prune2 = prune_ndjson(&out_path, args.keep_days, args.max_bytes)?;
    let payload = json!({
        "ok": true,
        "path": out_path,
        "captured_at": captured_at,
        "pressure_band": snapshot["storage"]["pressure_band"],
        "disk_used_ratio": snapshot["storage"]["disk"]["used_ratio"],
        "tracked_total_bytes": snapshot["storage"]["tracked"]["total_bytes"],
        "service_health": snapshot["service_health"],
        "prune": prune2,
    });
    print_json(&payload, args.pretty)
}

fn parse_week_id(week_id: &str) -> Result<(i32, u32)> {
    let trimmed = week_id.trim();
    let (y, w) = trimmed
        .split_once("-W")
        .ok_or_else(|| anyhow!("invalid week id: {}", week_id))?;
    let year: i32 = y.parse()?;
    let week: u32 = w.parse()?;
    if !(1..=53).contains(&week) {
        return Err(anyhow!("invalid week number in week id: {}", week_id));
    }
    Ok((year, week))
}

fn week_start_from_id(week_id: &str) -> Result<NaiveDate> {
    let (year, week) = parse_week_id(week_id)?;
    NaiveDate::from_isoywd_opt(year, week, chrono::Weekday::Mon)
        .ok_or_else(|| anyhow!("invalid week id: {}", week_id))
}

fn current_week_id() -> String {
    let now = Utc::now();
    let iso = now.iso_week();
    format!("{:04}-W{:02}", iso.year(), iso.week())
}

#[derive(Clone)]
struct TopicRow {
    path: String,
    event_count: i64,
    unique_file_count: i64,
    depth: i64,
    latest_timestamp: Option<String>,
}

fn discover_projects_from_root(memory_root: &Path) -> Vec<String> {
    let mut projects = Vec::new();
    if !memory_root.exists() {
        return projects;
    }
    if let Ok(read) = fs::read_dir(memory_root) {
        for entry in read.flatten() {
            let p = entry.path();
            if !p.is_dir() {
                continue;
            }
            if let Some(name) = p.file_name().and_then(|n| n.to_str()) {
                if name.starts_with('.') || name.starts_with('_') || name.trim().is_empty() {
                    continue;
                }
                projects.push(name.to_string());
            }
        }
    }
    projects.sort();
    projects.dedup();
    projects
}

fn discover_projects_from_api(
    client: &Client,
    base_url: &str,
    limit: usize,
    max_pages: usize,
) -> Result<Vec<String>> {
    let mut projects = BTreeSet::new();
    let mut offset = 0usize;
    let mut page = 0usize;
    loop {
        if page >= max_pages {
            break;
        }
        let url = format!(
            "{}/memory/topics/list?limit={}&offset={}",
            base_url.trim_end_matches('/'),
            limit.clamp(1, 5000),
            offset
        );
        let payload: Value = client.get(url).send()?.error_for_status()?.json()?;
        let topics = payload
            .get("topics")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        if topics.is_empty() {
            break;
        }
        for row in &topics {
            if let Some(project) = row.get("project").and_then(|v| v.as_str()) {
                if !project.trim().is_empty() {
                    projects.insert(project.trim().to_string());
                }
            }
        }
        offset += topics.len();
        page += 1;
        let total = payload.get("total").and_then(|v| v.as_u64()).unwrap_or(0) as usize;
        if total > 0 && offset >= total {
            break;
        }
        if topics.len() < limit {
            break;
        }
    }
    Ok(projects.into_iter().collect())
}

fn fetch_topic_rollups(
    client: &Client,
    base_url: &str,
    project: &str,
    min_count: usize,
    limit: usize,
) -> Result<Vec<TopicRow>> {
    let mut rows = Vec::new();
    let mut offset = 0usize;
    loop {
        let url = format!(
            "{}/memory/topic-rollups?project={}&min_count={}&limit={}&offset={}",
            base_url.trim_end_matches('/'),
            urlencoding::encode(project),
            min_count.max(1),
            limit.clamp(1, 2000),
            offset
        );
        let payload: Value = client.get(url).send()?.error_for_status()?.json()?;
        let topics = payload
            .get("topics")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        if topics.is_empty() {
            break;
        }
        for row in &topics {
            let path = row
                .get("path")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .trim()
                .to_string();
            if path.is_empty() {
                continue;
            }
            rows.push(TopicRow {
                path,
                event_count: row.get("eventCount").and_then(|v| v.as_i64()).unwrap_or(0),
                unique_file_count: row
                    .get("uniqueFileCount")
                    .and_then(|v| v.as_i64())
                    .unwrap_or(0),
                depth: row.get("depth").and_then(|v| v.as_i64()).unwrap_or(0),
                latest_timestamp: row
                    .get("latestTimestamp")
                    .and_then(|v| v.as_str())
                    .map(|s| s.to_string()),
            });
        }
        let total = payload
            .get("total")
            .and_then(|v| v.as_u64())
            .unwrap_or(rows.len() as u64) as usize;
        offset += topics.len();
        if offset >= total || topics.len() < limit {
            break;
        }
    }
    rows.sort_by(|a, b| {
        b.event_count
            .cmp(&a.event_count)
            .then_with(|| a.path.cmp(&b.path))
    });
    Ok(rows)
}

fn hash_counts(counts: &BTreeMap<String, i64>) -> String {
    let compact: Vec<Value> = counts.iter().map(|(k, v)| json!([k, v])).collect();
    let encoded = serde_json::to_vec(&compact).unwrap_or_default();
    let mut hasher = Sha256::new();
    hasher.update(encoded);
    format!("{:x}", hasher.finalize())
}

fn read_json(path: &Path) -> Option<Value> {
    fs::read_to_string(path)
        .ok()
        .and_then(|raw| serde_json::from_str::<Value>(&raw).ok())
}

fn write_json_atomic(path: &Path, payload: &Value) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let tmp = path.with_extension("tmp");
    fs::write(
        &tmp,
        format!("{}\n", serde_json::to_string_pretty(payload)?),
    )?;
    fs::rename(tmp, path)?;
    Ok(())
}

fn read_counts_ref(path: &Path) -> BTreeMap<String, i64> {
    let mut out = BTreeMap::new();
    let Some(payload) = read_json(path) else {
        return out;
    };
    if let Some(rows) = payload.get("counts").and_then(|v| v.as_array()) {
        for row in rows {
            if let Some(arr) = row.as_array() {
                if arr.len() != 2 {
                    continue;
                }
                let topic = arr[0].as_str().unwrap_or("").trim().to_string();
                if topic.is_empty() {
                    continue;
                }
                let count = arr[1].as_i64().unwrap_or(0);
                out.insert(topic, count);
            }
        }
    }
    out
}

fn compute_delta(curr: &BTreeMap<String, i64>, prev: &BTreeMap<String, i64>) -> Value {
    let curr_keys: HashSet<_> = curr.keys().cloned().collect();
    let prev_keys: HashSet<_> = prev.keys().cloned().collect();
    let mut changes = Vec::new();
    for key in curr_keys.intersection(&prev_keys) {
        let c = *curr.get(key).unwrap_or(&0);
        let p = *prev.get(key).unwrap_or(&0);
        if c != p {
            changes.push((key.clone(), p, c));
        }
    }
    changes.sort_by(|a, b| (b.2 - b.1).abs().cmp(&(a.2 - a.1).abs()));

    json!({
        "topic_delta": curr.len() as i64 - prev.len() as i64,
        "event_count_delta": curr.values().sum::<i64>() - prev.values().sum::<i64>(),
        "added_topics": curr_keys.difference(&prev_keys).count(),
        "removed_topics": prev_keys.difference(&curr_keys).count(),
        "changed_topics": changes.len(),
        "top_changes": changes.iter().take(25).map(|(path, p, c)| json!({"path": path, "prev": p, "curr": c, "delta": c-p})).collect::<Vec<_>>()
    })
}

fn tokenize_topic(path: &str) -> HashSet<String> {
    const STOPWORDS: &[&str] = &[
        "root",
        "notes",
        "tasks",
        "task",
        "tmp",
        "state",
        "stats",
        "snapshot",
        "snapshots",
        "health",
        "system",
        "data",
        "project",
        "projects",
        "file",
        "files",
        "run",
        "runs",
        "log",
        "logs",
    ];
    let stop: HashSet<&str> = STOPWORDS.iter().copied().collect();
    let mut out = HashSet::new();
    for token in path
        .to_lowercase()
        .split(|c: char| !c.is_ascii_alphanumeric())
        .filter(|s| !s.trim().is_empty())
    {
        let t = token.trim();
        if t.len() < 3 || stop.contains(t) {
            continue;
        }
        out.insert(t.to_string());
    }
    out
}

fn skill_hint(token: &str) -> Option<&'static str> {
    match token {
        "rust" => Some("rust"),
        "go" | "golang" => Some("go"),
        "vector" => Some("vector-search"),
        "graph" => Some("graph-reasoning"),
        "retrieval" => Some("retrieval-policy"),
        "latency" => Some("performance-tuning"),
        "telemetry" => Some("observability"),
        "security" => Some("security-hardening"),
        "billing" => Some("paid-operations"),
        "auth" => Some("auth-identity"),
        "mcp" => Some("mcp-interoperability"),
        _ => None,
    }
}

fn build_synergy(week_id: &str, summaries: &[Value]) -> Value {
    let mut per_project: BTreeMap<String, BTreeMap<String, i64>> = BTreeMap::new();
    for summary in summaries {
        let project = summary
            .get("project")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim();
        let counts_ref = summary
            .get("counts_ref")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim();
        if project.is_empty() || counts_ref.is_empty() {
            continue;
        }
        let counts = read_counts_ref(Path::new(counts_ref));
        if !counts.is_empty() {
            per_project.insert(project.to_string(), counts);
        }
    }

    let mut token_weight: BTreeMap<String, BTreeMap<String, i64>> = BTreeMap::new();
    for (project, counts) in &per_project {
        let mut project_weights: BTreeMap<String, i64> = BTreeMap::new();
        for (topic, count) in counts {
            for token in tokenize_topic(topic) {
                *project_weights.entry(token).or_insert(0) += *count;
            }
        }
        for (token, weight) in project_weights {
            token_weight
                .entry(token)
                .or_default()
                .insert(project.clone(), weight);
        }
    }

    let mut overlaps: Vec<Value> = token_weight
        .iter()
        .filter(|(_, m)| m.len() >= 2)
        .map(|(token, by_project)| {
            let mut projects: Vec<_> = by_project
                .iter()
                .map(|(p, w)| json!({"project": p, "weight": w}))
                .collect();
            projects.sort_by(|a, b| {
                b["weight"]
                    .as_i64()
                    .unwrap_or(0)
                    .cmp(&a["weight"].as_i64().unwrap_or(0))
            });
            json!({
                "token": token,
                "project_count": by_project.len(),
                "total_weight": by_project.values().sum::<i64>(),
                "projects": projects,
            })
        })
        .collect();

    overlaps.sort_by(|a, b| {
        b["project_count"]
            .as_u64()
            .unwrap_or(0)
            .cmp(&a["project_count"].as_u64().unwrap_or(0))
            .then_with(|| {
                b["total_weight"]
                    .as_i64()
                    .unwrap_or(0)
                    .cmp(&a["total_weight"].as_i64().unwrap_or(0))
            })
    });

    let project_names: Vec<_> = per_project.keys().cloned().collect();
    let mut pairwise: Vec<Value> = Vec::new();
    for i in 0..project_names.len() {
        for j in (i + 1)..project_names.len() {
            let left = &project_names[i];
            let right = &project_names[j];
            let mut left_tokens = HashSet::new();
            let mut right_tokens = HashSet::new();
            for topic in per_project.get(left).into_iter().flat_map(|m| m.keys()) {
                left_tokens.extend(tokenize_topic(topic));
            }
            for topic in per_project.get(right).into_iter().flat_map(|m| m.keys()) {
                right_tokens.extend(tokenize_topic(topic));
            }
            let inter: HashSet<String> = left_tokens.intersection(&right_tokens).cloned().collect();
            let union_count = left_tokens.union(&right_tokens).count();
            if inter.is_empty() || union_count == 0 {
                continue;
            }
            let jaccard = inter.len() as f64 / union_count as f64;
            if jaccard < 0.08 {
                continue;
            }
            let mut shared: Vec<_> = inter.into_iter().collect();
            shared.sort();
            shared.truncate(24);
            pairwise.push(json!({
                "projects": [left, right],
                "jaccard": (jaccard * 10000.0).round() / 10000.0,
                "shared_tokens": shared,
            }));
        }
    }
    pairwise.sort_by(|a, b| {
        b["jaccard"]
            .as_f64()
            .unwrap_or(0.0)
            .partial_cmp(&a["jaccard"].as_f64().unwrap_or(0.0))
            .unwrap_or(std::cmp::Ordering::Equal)
    });

    let mut skill_candidates = Vec::new();
    for row in overlaps.iter().take(120) {
        let token = row["token"].as_str().unwrap_or("");
        if let Some(skill) = skill_hint(token) {
            let projects = row["projects"]
                .as_array()
                .cloned()
                .unwrap_or_default()
                .into_iter()
                .take(8)
                .filter_map(|r| {
                    r.get("project")
                        .and_then(|v| v.as_str())
                        .map(|s| s.to_string())
                })
                .collect::<Vec<_>>();
            skill_candidates.push(json!({
                "skill": skill,
                "trigger_token": token,
                "project_count": row["project_count"],
                "projects": projects,
            }));
        }
    }

    json!({
        "schema_version": 1,
        "generated_at": Utc::now().to_rfc3339().replace("+00:00", "Z"),
        "week_id": week_id,
        "project_count": per_project.len(),
        "project_refs": summaries.iter().map(|summary| json!({
            "project": summary.get("project").cloned().unwrap_or_else(|| json!("")),
            "summary_ref": summary.get("summary_path").cloned().unwrap_or_else(|| json!("")),
            "counts_ref": summary.get("counts_ref").cloned().unwrap_or_else(|| json!("")),
            "fingerprint": summary.get("fingerprint").cloned().unwrap_or_else(|| json!("")),
        })).collect::<Vec<_>>(),
        "synergy_tokens": overlaps.into_iter().take(200).collect::<Vec<_>>(),
        "project_pairwise": pairwise.into_iter().take(80).collect::<Vec<_>>(),
        "skill_candidates": skill_candidates,
    })
}

fn find_previous_summary(project_dir: &Path, week_id: &str) -> Option<PathBuf> {
    let mut candidates = Vec::new();
    if let Ok(read) = fs::read_dir(project_dir) {
        for entry in read.flatten() {
            let path = entry.path();
            let Some(name) = path.file_name().and_then(|v| v.to_str()) else {
                continue;
            };
            if !name.starts_with("week-") || !name.ends_with(".json") {
                continue;
            }
            let token = name.trim_start_matches("week-").trim_end_matches(".json");
            if parse_week_id(token).is_err() {
                continue;
            }
            if token < week_id {
                candidates.push(path);
            }
        }
    }
    candidates.sort();
    candidates.pop()
}

fn prune_old_weeks(root: &Path, keep_weeks: usize) {
    if keep_weeks == 0 || !root.exists() {
        return;
    }
    let mut by_parent: HashMap<PathBuf, Vec<PathBuf>> = HashMap::new();
    for entry in WalkDir::new(root).into_iter().flatten() {
        let path = entry.path();
        if !path.is_file() {
            continue;
        }
        let Some(name) = path.file_name().and_then(|v| v.to_str()) else {
            continue;
        };
        if name.starts_with("week-") && name.ends_with(".json") {
            by_parent
                .entry(path.parent().unwrap_or(root).to_path_buf())
                .or_default()
                .push(path.to_path_buf());
        }
    }
    for (_parent, mut files) in by_parent {
        files.sort();
        if files.len() <= keep_weeks {
            continue;
        }
        let stale_count = files.len() - keep_weeks;
        for stale in files.into_iter().take(stale_count) {
            let _ = fs::remove_file(stale);
        }
    }
}

fn run_weekly_lineage(args: WeeklyLineageArgs) -> Result<()> {
    parse_week_id(&args.week_id)?;
    let week_start = week_start_from_id(&args.week_id)?;
    let api_key = if args.api_key.trim().is_empty() {
        default_orchestrator_api_key()
    } else {
        args.api_key.clone()
    };

    let client = http_client(&api_key, args.timeout_secs)?;
    let base = args.orchestrator_url.trim_end_matches('/');
    let memory_root = PathBuf::from(args.memory_root.trim());
    let out_root = PathBuf::from(args.out_root.trim());

    let mut projects: Vec<String> = args
        .projects
        .iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    if projects.is_empty() {
        projects = discover_projects_from_root(&memory_root);
    }
    if projects.is_empty() {
        projects = discover_projects_from_api(&client, base, 1000, 20)?;
    }
    projects.sort();
    projects.dedup();

    if projects.is_empty() {
        return Err(anyhow!(
            "no projects discovered; pass --project or set memory root"
        ));
    }

    let mut weekly_summaries: Vec<Value> = Vec::new();
    for project in projects {
        let rows = fetch_topic_rollups(
            &client,
            base,
            &project,
            args.min_count.max(1),
            args.page_limit.max(1),
        )?;
        let mut counts = BTreeMap::new();
        for row in &rows {
            counts.insert(row.path.clone(), row.event_count);
        }
        let compact_counts: Vec<Value> = counts.iter().map(|(k, v)| json!([k, v])).collect();
        let fingerprint = hash_counts(&counts);

        let project_root = out_root.join("projects").join(&project);
        let summary_path = project_root.join(format!("week-{}.json", args.week_id));
        let counts_path = project_root
            .join("counts")
            .join(format!("week-{}.json", args.week_id));

        let prev_summary_path = find_previous_summary(&project_root, &args.week_id);
        let prev_summary = prev_summary_path.as_ref().and_then(|p| read_json(p));

        let prev_week_id = prev_summary
            .as_ref()
            .and_then(|v| v.get("week_id"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let prev_fingerprint = prev_summary
            .as_ref()
            .and_then(|v| v.get("fingerprint"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());
        let prev_counts_ref = prev_summary
            .as_ref()
            .and_then(|v| v.get("counts_ref"))
            .and_then(|v| v.as_str())
            .map(|s| s.to_string());

        let prev_counts = prev_counts_ref
            .as_ref()
            .map(|r| read_counts_ref(Path::new(r)))
            .unwrap_or_default();

        let mut counts_ref_to_use = counts_path.to_string_lossy().to_string();
        let mut counts_reused = false;
        if prev_fingerprint.as_deref() == Some(fingerprint.as_str()) {
            if let Some(prev_ref) = prev_counts_ref.clone() {
                counts_ref_to_use = prev_ref;
                counts_reused = true;
            }
        } else if !args.dry_run {
            write_json_atomic(
                &counts_path,
                &json!({
                    "schema_version": 1,
                    "project": project,
                    "week_id": args.week_id,
                    "generated_at": Utc::now().to_rfc3339().replace("+00:00", "Z"),
                    "fingerprint": fingerprint,
                    "counts": compact_counts,
                }),
            )?;
        }

        let delta = if prev_counts.is_empty() {
            json!({
                "topic_delta": counts.len(),
                "event_count_delta": counts.values().sum::<i64>(),
                "added_topics": counts.len(),
                "removed_topics": 0,
                "changed_topics": 0,
                "top_changes": [],
            })
        } else {
            compute_delta(&counts, &prev_counts)
        };

        let total_events = counts.values().sum::<i64>();
        let top_topics = rows
            .iter()
            .take(args.top_topic_limit.max(1))
            .map(|r| {
                json!({
                    "path": r.path,
                    "event_count": r.event_count,
                    "unique_file_count": r.unique_file_count,
                    "latest_timestamp": r.latest_timestamp,
                })
            })
            .collect::<Vec<_>>();

        let summary_payload = json!({
            "schema_version": 1,
            "generated_at": Utc::now().to_rfc3339().replace("+00:00", "Z"),
            "week_id": args.week_id,
            "week_start": week_start.to_string(),
            "project": project,
            "source": "/memory/topic-rollups",
            "fingerprint": fingerprint,
            "stats": {
                "topic_count": counts.len(),
                "total_event_count": total_events,
                "max_depth": rows.iter().map(|r| r.depth).max().unwrap_or(0),
            },
            "delta": {
                "previous_week_id": prev_week_id,
                "previous_fingerprint": prev_fingerprint,
                "topic_delta": delta.get("topic_delta").cloned().unwrap_or_else(|| json!(0)),
                "event_count_delta": delta.get("event_count_delta").cloned().unwrap_or_else(|| json!(0)),
                "added_topics": delta.get("added_topics").cloned().unwrap_or_else(|| json!(0)),
                "removed_topics": delta.get("removed_topics").cloned().unwrap_or_else(|| json!(0)),
                "changed_topics": delta.get("changed_topics").cloned().unwrap_or_else(|| json!(0)),
                "top_changes": delta.get("top_changes").cloned().unwrap_or_else(|| json!([])),
            },
            "top_topics": top_topics,
            "counts_ref": counts_ref_to_use,
            "counts_reused": counts_reused,
            "previous_summary_ref": prev_summary_path.as_ref().map(|p| p.to_string_lossy().to_string()),
        });

        if !args.dry_run {
            write_json_atomic(&summary_path, &summary_payload)?;
        }

        weekly_summaries.push(json!({
            "project": summary_payload["project"],
            "summary_path": summary_path,
            "counts_ref": counts_ref_to_use,
            "fingerprint": summary_payload["fingerprint"],
            "topic_count": counts.len(),
            "total_event_count": total_events,
        }));
    }

    let synergy_path = out_root
        .join("global")
        .join(format!("week-{}.json", args.week_id));
    if args.emit_synergy {
        let synergy = build_synergy(&args.week_id, &weekly_summaries);
        if !args.dry_run {
            write_json_atomic(&synergy_path, &synergy)?;
        }
    }

    if !args.dry_run {
        prune_old_weeks(&out_root, args.keep_weeks.max(1));
    }

    let result = json!({
        "ok": true,
        "week_id": args.week_id,
        "projects": weekly_summaries,
        "synergy_emitted": args.emit_synergy,
        "synergy_path": if args.emit_synergy { Some(synergy_path.to_string_lossy().to_string()) } else { None::<String> },
        "out_root": out_root,
        "dry_run": args.dry_run,
    });
    print_json(&result, args.pretty)
}

#[derive(Clone)]
struct SnapshotEntry {
    path: PathBuf,
    ts: Option<DateTime<Utc>>,
    size: u64,
    bucket: String,
}

fn parse_timestamp_from_name(name: &str) -> Option<DateTime<Utc>> {
    let digits = name.as_bytes();
    if digits.len() < 19 {
        return None;
    }
    for i in 0..=(name.len().saturating_sub(19)) {
        let slice = &name[i..i + 19];
        // yyyy-mm-dd-hh-mm-ss
        if slice.chars().nth(4) != Some('-')
            || slice.chars().nth(7) != Some('-')
            || slice.chars().nth(10) != Some('-')
            || slice.chars().nth(13) != Some('-')
            || slice.chars().nth(16) != Some('-')
        {
            continue;
        }
        if let Ok(ndt) = NaiveDateTime::parse_from_str(slice, "%Y-%m-%d-%H-%M-%S") {
            return Some(DateTime::<Utc>::from_naive_utc_and_offset(ndt, Utc));
        }
    }
    None
}

fn list_cold_entries(root: &Path) -> Vec<SnapshotEntry> {
    let mut out = Vec::new();
    for entry in WalkDir::new(root).into_iter().flatten() {
        let path = entry.path();
        if !path.is_file() {
            continue;
        }
        let Some(name) = path.file_name().and_then(|v| v.to_str()) else {
            continue;
        };
        if !(name.ends_with(".snapshot") || name.ends_with(".snapshot.zst")) {
            continue;
        }
        let size = path.metadata().map(|m| m.len()).unwrap_or(0);
        let ts = parse_timestamp_from_name(name);
        let bucket = path
            .parent()
            .and_then(|p| p.file_name())
            .and_then(|b| b.to_str())
            .unwrap_or("")
            .to_string();
        out.push(SnapshotEntry {
            path: path.to_path_buf(),
            ts,
            size,
            bucket,
        });
    }
    out.sort_by(|a, b| b.ts.cmp(&a.ts));
    out
}

fn safe_under(path: &Path, root: &Path) -> bool {
    match (path.canonicalize(), root.canonicalize()) {
        (Ok(p), Ok(r)) => p.starts_with(r),
        _ => false,
    }
}

fn sha256_file(path: &Path) -> Result<String> {
    let mut hasher = Sha256::new();
    let mut file = File::open(path)?;
    let mut buf = vec![0u8; 1024 * 1024];
    loop {
        let n = file.read(&mut buf)?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok(format!("{:x}", hasher.finalize()))
}

fn run_cold_pack(args: ColdPackArgs) -> Result<()> {
    let cold_root = PathBuf::from(args.cold_root.trim());
    if !cold_root.exists() {
        return Err(anyhow!("cold root missing: {}", cold_root.display()));
    }

    let mut snapshots: Vec<PathBuf> = WalkDir::new(&cold_root)
        .into_iter()
        .flatten()
        .filter(|e| e.path().is_file())
        .map(|e| e.path().to_path_buf())
        .filter(|p| {
            p.file_name()
                .and_then(|v| v.to_str())
                .map(|n| n.ends_with(".snapshot"))
                .unwrap_or(false)
        })
        .collect();
    snapshots.sort();
    if args.max_files > 0 && snapshots.len() > args.max_files {
        snapshots.truncate(args.max_files);
    }
    let scanned_count = snapshots.len();

    if snapshots.is_empty() {
        return print_json(
            &json!({"ok": true, "scanned": 0, "packed": 0, "skipped": 0, "deleted": 0}),
            false,
        );
    }

    let mut catalog_path = PathBuf::from(args.catalog.trim());
    if !catalog_path.is_absolute() {
        catalog_path = cold_root.join(catalog_path);
    }
    if let Some(parent) = catalog_path.parent() {
        fs::create_dir_all(parent)?;
    }

    let mut packed = 0usize;
    let mut skipped = 0usize;
    let mut deleted = 0usize;
    let mut source_total = 0u64;
    let mut target_total = 0u64;
    let verify_enabled = args.verify && !args.no_verify;

    let mut catalog = OpenOptions::new()
        .create(true)
        .append(true)
        .open(&catalog_path)?;

    for src in snapshots {
        let dst = PathBuf::from(format!("{}.zst", src.to_string_lossy()));
        let source_bytes = src.metadata().map(|m| m.len()).unwrap_or(0);
        source_total += source_bytes;

        if dst.exists() && dst.metadata().map(|m| m.len()).unwrap_or(0) > 0 {
            skipped += 1;
            let row = json!({
                "recorded_at": Utc::now().to_rfc3339().replace("+00:00", "Z"),
                "source": src,
                "target": dst,
                "source_bytes": source_bytes,
                "target_bytes": dst.metadata().map(|m| m.len()).unwrap_or(0),
                "verified": false,
                "removed_source": false,
                "skipped": true,
                "reason": "compressed_exists",
            });
            writeln!(catalog, "{}", serde_json::to_string(&row)?)?;
            continue;
        }

        let sha = sha256_file(&src)?;
        let mut target_bytes = 0u64;
        let mut verified = false;
        let mut removed = false;

        if args.apply {
            if let Some(parent) = dst.parent() {
                fs::create_dir_all(parent)?;
            }
            let mut in_file = File::open(&src)?;
            let mut out_file = File::create(&dst)?;
            zstd::stream::copy_encode(&mut in_file, &mut out_file, args.level)?;
            out_file.flush()?;
            target_bytes = dst.metadata().map(|m| m.len()).unwrap_or(0);
            if verify_enabled {
                let mut decoded_hasher = Sha256::new();
                let out = zstd::stream::decode_all(File::open(&dst)?)?;
                decoded_hasher.update(&out);
                verified = format!("{:x}", decoded_hasher.finalize()) == sha;
                if !verified {
                    return Err(anyhow!("hash verify failed for {}", src.display()));
                }
            } else {
                verified = true;
            }
            if !args.keep_original {
                if !safe_under(&src, &cold_root) {
                    return Err(anyhow!(
                        "refusing delete outside cold root: {}",
                        src.display()
                    ));
                }
                fs::remove_file(&src)?;
                removed = true;
                deleted += 1;
            }
            packed += 1;
        } else {
            skipped += 1;
        }

        target_total += target_bytes;
        let row = json!({
            "recorded_at": Utc::now().to_rfc3339().replace("+00:00", "Z"),
            "source": src,
            "target": dst,
            "source_bytes": source_bytes,
            "target_bytes": target_bytes,
            "savings_bytes": source_bytes.saturating_sub(target_bytes),
            "sha256": sha,
            "verified": verified,
            "removed_source": removed,
            "skipped": !args.apply,
            "reason": if args.apply { "" } else { "dry_run" },
        });
        writeln!(catalog, "{}", serde_json::to_string(&row)?)?;
    }

    let payload = json!({
        "ok": true,
        "apply": args.apply,
        "verify": verify_enabled,
        "keep_original": args.keep_original,
        "scanned": scanned_count,
        "packed": packed,
        "skipped": skipped,
        "deleted_originals": deleted,
        "source_bytes": source_total,
        "target_bytes": target_total,
        "savings_bytes": source_total.saturating_sub(target_total),
        "catalog": catalog_path,
    });
    print_json(&payload, false)
}

fn run_cold_tier(args: ColdTierArgs) -> Result<()> {
    let root = PathBuf::from(args.cold_root.trim());
    if !root.exists() {
        return Err(anyhow!("cold root missing: {}", root.display()));
    }
    let entries = list_cold_entries(&root);
    if entries.is_empty() {
        return print_json(
            &json!({"ok": true, "scanned": 0, "kept": 0, "deleted": 0, "reclaimed_bytes": 0}),
            false,
        );
    }

    let now = Utc::now();
    let daily_cutoff = now - ChronoDuration::days(args.keep_daily);
    let weekly_cutoff =
        now - ChronoDuration::weeks((args.keep_daily / 7).max(0) + args.keep_weekly as i64);

    let mut keep: HashSet<PathBuf> = HashSet::new();
    for item in entries.iter().take(args.keep_latest) {
        keep.insert(item.path.clone());
    }

    let mut daily_keys: HashSet<(String, String)> = HashSet::new();
    let mut weekly_keys: HashSet<(String, String)> = HashSet::new();
    let mut monthly_keys: HashSet<(String, String)> = HashSet::new();

    for item in &entries {
        if keep.contains(&item.path) {
            continue;
        }
        let Some(ts) = item.ts else {
            continue;
        };
        let day_key = (item.bucket.clone(), ts.format("%Y-%m-%d").to_string());
        let week = ts.iso_week();
        let week_key = (
            item.bucket.clone(),
            format!("{}-W{:02}", week.year(), week.week()),
        );
        let month_key = (item.bucket.clone(), ts.format("%Y-%m").to_string());

        if ts >= daily_cutoff {
            if !daily_keys.contains(&day_key) {
                daily_keys.insert(day_key);
                keep.insert(item.path.clone());
            }
            continue;
        }
        if ts >= weekly_cutoff {
            let per_bucket = weekly_keys
                .iter()
                .filter(|(b, _)| b == &item.bucket)
                .count();
            if per_bucket < args.keep_weekly && !weekly_keys.contains(&week_key) {
                weekly_keys.insert(week_key);
                keep.insert(item.path.clone());
            }
            continue;
        }
        let per_bucket = monthly_keys
            .iter()
            .filter(|(b, _)| b == &item.bucket)
            .count();
        if per_bucket < args.keep_monthly && !monthly_keys.contains(&month_key) {
            monthly_keys.insert(month_key);
            keep.insert(item.path.clone());
        }
    }

    let to_delete: Vec<_> = entries
        .iter()
        .filter(|e| !keep.contains(&e.path))
        .cloned()
        .collect();
    let mut reclaimed = 0u64;
    let mut deleted = 0usize;
    if args.apply {
        for entry in &to_delete {
            if !safe_under(&entry.path, &root) {
                return Err(anyhow!(
                    "refusing delete outside cold root: {}",
                    entry.path.display()
                ));
            }
            reclaimed += entry.size;
            let _ = fs::remove_file(&entry.path);
            deleted += 1;
        }
    } else {
        reclaimed = to_delete.iter().map(|e| e.size).sum();
    }

    let mut per_bucket: BTreeMap<String, Value> = BTreeMap::new();
    for entry in &entries {
        let row = per_bucket.entry(entry.bucket.clone()).or_insert_with(
            || json!({"kept": 0, "deleted": 0, "kept_bytes": 0, "deleted_bytes": 0}),
        );
        let (k_key, b_key) = if keep.contains(&entry.path) {
            ("kept", "kept_bytes")
        } else {
            ("deleted", "deleted_bytes")
        };
        row[k_key] = json!(row[k_key].as_u64().unwrap_or(0) + 1);
        row[b_key] = json!(row[b_key].as_u64().unwrap_or(0) + entry.size);
    }

    let payload = json!({
        "ok": true,
        "apply": args.apply,
        "scanned": entries.len(),
        "kept": keep.len(),
        "deleted": if args.apply { deleted } else { to_delete.len() },
        "reclaimed_bytes": reclaimed,
        "per_bucket": per_bucket,
    });
    print_json(&payload, false)
}

fn parse_iso_from_value(v: &Value) -> Option<DateTime<Utc>> {
    let s = v.as_str()?;
    parse_iso_utc(s)
}

fn archive_single_file(
    path: &Path,
    cold_dir: &Path,
    retention_hours: i64,
    timestamp_field: &str,
) -> Result<(usize, usize)> {
    let cutoff = Utc::now() - ChronoDuration::hours(retention_hours);
    let file = File::open(path)?;
    let reader = BufReader::new(file);
    let mut kept: Vec<String> = Vec::new();
    let mut buckets: BTreeMap<String, Vec<String>> = BTreeMap::new();
    let mut moved_count = 0usize;

    for line in reader.lines() {
        let raw = line?;
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            continue;
        }
        let parsed: Value = match serde_json::from_str(trimmed) {
            Ok(v) => v,
            Err(_) => {
                kept.push(raw);
                continue;
            }
        };
        let ts = parsed.get(timestamp_field).and_then(parse_iso_from_value);
        if let Some(ts) = ts {
            if ts < cutoff {
                let date_key = ts.format("%Y%m%d").to_string();
                buckets
                    .entry(date_key)
                    .or_default()
                    .push(format!("{}\n", trimmed));
                moved_count += 1;
                continue;
            }
        }
        kept.push(format!("{}\n", trimmed));
    }

    let base = path
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("metrics")
        .to_string();
    for (date_key, lines) in buckets {
        let out_dir = cold_dir.join(&base);
        fs::create_dir_all(&out_dir)?;
        let out_path = out_dir.join(format!("{}.{}.ndjson.gz", base, date_key));
        let file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(out_path)?;
        let mut encoder = GzEncoder::new(file, Compression::default());
        for line in lines {
            encoder.write_all(line.as_bytes())?;
        }
        let _ = encoder.finish()?;
    }

    atomic_write_lines(
        path,
        &kept
            .iter()
            .map(|s| s.trim_end_matches('\n').to_string())
            .collect::<Vec<_>>(),
    )?;
    Ok((moved_count, kept.len()))
}

fn run_archive_ndjson(args: ArchiveNdjsonArgs) -> Result<()> {
    let cold_dir = PathBuf::from(args.cold_dir.trim());
    let mut file_paths: Vec<PathBuf> = args.files.iter().map(PathBuf::from).collect();

    if !args.data_dir.trim().is_empty() {
        let data_dir = PathBuf::from(args.data_dir.trim());
        if data_dir.exists() {
            for name in [
                "trading_metrics.ndjson",
                "strategy_metrics.ndjson",
                "solana_signals.ndjson",
                "solana_overrides.ndjson",
            ] {
                let p = data_dir.join(name);
                if p.exists() {
                    file_paths.push(p);
                }
            }
        }
    }

    file_paths.retain(|p| p.exists());
    file_paths.sort();
    file_paths.dedup();
    if file_paths.is_empty() {
        return Err(anyhow!(
            "No files found to archive. Use --file or --data-dir."
        ));
    }

    for path in &file_paths {
        let (moved, kept) =
            archive_single_file(path, &cold_dir, args.retention_hours, &args.timestamp_field)?;
        println!("{}: moved={} kept={}", path.display(), moved, kept);
    }
    Ok(())
}

fn resolve_db_path(cli: Option<String>) -> PathBuf {
    let mut candidates = Vec::<PathBuf>::new();
    if let Some(cli) = cli {
        if !cli.trim().is_empty() {
            candidates.push(PathBuf::from(cli));
        }
    }
    if let Ok(task_db) = std::env::var("TASK_DB_PATH") {
        if !task_db.trim().is_empty() {
            candidates.push(PathBuf::from(task_db));
        }
    }
    if let Ok(orch_data) = std::env::var("ORCHESTRATOR_DATA_DIR") {
        if !orch_data.trim().is_empty() {
            candidates.push(Path::new(orch_data.trim()).join("agent_tasks.db"));
        }
    }
    if let Ok(home) = std::env::var("HOME") {
        candidates.push(Path::new(&home).join(".contextlattice/orchestrator/agent_tasks.db"));
    }
    candidates.push(PathBuf::from(".data/orchestrator/agent_tasks.db"));
    candidates.push(PathBuf::from("services/orchestrator/data/agent_tasks.db"));

    for path in &candidates {
        if path.exists() {
            return path.clone();
        }
    }
    candidates
        .into_iter()
        .next()
        .unwrap_or_else(|| PathBuf::from(".data/orchestrator/agent_tasks.db"))
}

fn count_rows(conn: &Connection, sql: &str, params: &[&dyn rusqlite::ToSql]) -> Result<i64> {
    let mut stmt = conn.prepare(sql)?;
    let mut rows = stmt.query(params)?;
    if let Some(row) = rows.next()? {
        Ok(row.get::<_, i64>(0).unwrap_or(0))
    } else {
        Ok(0)
    }
}

fn status_counts(conn: &Connection) -> Result<BTreeMap<String, i64>> {
    let mut out = BTreeMap::new();
    let mut stmt = conn.prepare("SELECT status, COUNT(*) FROM fanout_outbox NOT INDEXED GROUP BY status ORDER BY status ASC;")?;
    let mut rows = stmt.query([])?;
    while let Some(row) = rows.next()? {
        let status: String = row.get::<_, String>(0).unwrap_or_default();
        let count: i64 = row.get::<_, i64>(1).unwrap_or(0);
        out.insert(status, count);
    }
    Ok(out)
}

fn cutoff_iso(hours: i64) -> String {
    (Utc::now() - ChronoDuration::hours(hours.max(0)))
        .to_rfc3339()
        .replace("+00:00", "Z")
}

fn csv_list(raw: &str) -> Vec<String> {
    raw.split(',')
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect()
}

fn run_fanout_gc(args: FanoutGcArgs) -> Result<()> {
    let db_path = resolve_db_path(args.db_path);
    if !db_path.exists() {
        print_json(
            &json!({
                "ok": true,
                "message": "fanout outbox DB not found; skipping",
                "db_path": db_path,
                "timestamp": Utc::now().to_rfc3339().replace("+00:00", "Z"),
            }),
            false,
        )?;
        return Ok(());
    }

    let conn = Connection::open(&db_path)?;
    conn.busy_timeout(Duration::from_secs_f64(args.timeout_secs.max(1.0)))?;
    let _journal_mode: String = conn.query_row("PRAGMA journal_mode=WAL;", [], |row| row.get(0))?;

    let table_exists: i64 = conn.query_row(
        "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='fanout_outbox';",
        [],
        |row| row.get(0),
    )?;
    if table_exists == 0 {
        print_json(
            &json!({
                "ok": true,
                "message": "fanout_outbox table missing; skipping",
                "db_path": db_path,
                "timestamp": Utc::now().to_rfc3339().replace("+00:00", "Z"),
            }),
            false,
        )?;
        return Ok(());
    }

    let before_total = count_rows(
        &conn,
        "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED;",
        &[],
    )?;
    let before_status = status_counts(&conn)?;

    let succeeded_cutoff = cutoff_iso(args.succeeded_retention_hours);
    let failed_cutoff = cutoff_iso(args.failed_retention_hours);
    let pending_cutoff = cutoff_iso(args.stale_pending_hours);

    let (succeeded_deleted, failed_deleted);
    let mut stale_deleted = 0i64;

    if args.dry_run {
        succeeded_deleted = count_rows(
            &conn,
            "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED WHERE status='succeeded' AND COALESCE(completed_at, updated_at, created_at) < ?;",
            &[&succeeded_cutoff],
        )?;
        failed_deleted = count_rows(
            &conn,
            "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED WHERE status='failed' AND COALESCE(completed_at, updated_at, created_at) < ?;",
            &[&failed_cutoff],
        )?;
    } else {
        succeeded_deleted = conn.execute(
            "DELETE FROM fanout_outbox NOT INDEXED WHERE status='succeeded' AND COALESCE(completed_at, updated_at, created_at) < ?;",
            [&succeeded_cutoff],
        )? as i64;
        failed_deleted = conn.execute(
            "DELETE FROM fanout_outbox NOT INDEXED WHERE status='failed' AND COALESCE(completed_at, updated_at, created_at) < ?;",
            [&failed_cutoff],
        )? as i64;
    }

    let stale_targets = csv_list(&args.stale_targets);
    if !stale_targets.is_empty() {
        let placeholders = std::iter::repeat("?")
            .take(stale_targets.len())
            .collect::<Vec<_>>()
            .join(",");
        let stale_statuses = vec!["pending", "retrying", "running"];
        let status_placeholders = std::iter::repeat("?")
            .take(stale_statuses.len())
            .collect::<Vec<_>>()
            .join(",");
        let sql_tail = format!(
            "FROM fanout_outbox NOT INDEXED WHERE target IN ({}) AND status IN ({}) AND COALESCE(last_attempt_at, updated_at, created_at) < ?",
            placeholders, status_placeholders
        );

        let mut params: Vec<&dyn rusqlite::ToSql> = Vec::new();
        for t in &stale_targets {
            params.push(t as &dyn rusqlite::ToSql);
        }
        for s in &stale_statuses {
            params.push(s as &dyn rusqlite::ToSql);
        }
        params.push(&pending_cutoff as &dyn rusqlite::ToSql);

        if args.dry_run {
            let sql = format!("SELECT COUNT(*) {}", sql_tail);
            stale_deleted = count_rows(&conn, &sql, &params)?;
        } else {
            let sql = format!("DELETE {}", sql_tail);
            stale_deleted = conn.execute(&sql, params.as_slice())? as i64;
        }
    }

    let deleted_total = succeeded_deleted + failed_deleted + stale_deleted;
    let mut vacuum_ran = false;
    let mut checkpoint_ok = true;
    let mut checkpoint_error = String::new();
    let mut vacuum_error = String::new();

    if args.dry_run {
        conn.execute("ROLLBACK;", []).ok();
    } else {
        conn.execute("COMMIT;", []).ok();
        if let Err(exc) = conn.execute("PRAGMA wal_checkpoint(TRUNCATE);", []) {
            checkpoint_ok = false;
            checkpoint_error = exc.to_string();
        }
        if args.vacuum && deleted_total >= args.vacuum_min_deleted.max(0) {
            match conn.execute("VACUUM;", []) {
                Ok(_) => vacuum_ran = true,
                Err(exc) => vacuum_error = exc.to_string(),
            }
        }
    }

    let after_total = count_rows(
        &conn,
        "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED;",
        &[],
    )?;
    let after_status = status_counts(&conn)?;
    let db_size_bytes = db_path.metadata().map(|m| m.len()).unwrap_or(0);

    print_json(
        &json!({
            "ok": true,
            "dry_run": args.dry_run,
            "db_path": db_path,
            "db_size_bytes": db_size_bytes,
            "before_total": before_total,
            "after_total": after_total,
            "before_status": before_status,
            "after_status": after_status,
            "deleted": {
                "succeeded": succeeded_deleted,
                "failed": failed_deleted,
                "stale_pending_targets": stale_deleted,
                "total": deleted_total,
            },
            "retention_hours": {
                "succeeded": args.succeeded_retention_hours,
                "failed": args.failed_retention_hours,
                "stale_pending": args.stale_pending_hours,
            },
            "stale_targets": stale_targets,
            "checkpoint": {"ok": checkpoint_ok, "error": checkpoint_error},
            "vacuum": {
                "requested": args.vacuum,
                "ran": vacuum_ran,
                "min_deleted": args.vacuum_min_deleted,
                "error": vacuum_error,
            },
            "timestamp": Utc::now().to_rfc3339().replace("+00:00", "Z"),
        }),
        false,
    )?;

    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_week_roundtrip() {
        let parsed = parse_week_id("2026-W20").expect("parse");
        assert_eq!(parsed, (2026, 20));
    }

    #[test]
    fn tokenize_strips_stopwords() {
        let tokens = tokenize_topic("root/notes/rust/vector/alpha");
        assert!(tokens.contains("rust"));
        assert!(tokens.contains("vector"));
        assert!(!tokens.contains("root"));
        assert!(!tokens.contains("notes"));
    }
}
