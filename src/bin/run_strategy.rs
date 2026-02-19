use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use algotraderv2::app::runtime::{
    build_unified_runtime, AppMode, AppRunConfig, RuntimeOverrides,
};
use algotraderv2::backtest::{providers::provider_for, Backtester, SimMode};
use algotraderv2::blockchain::solana_client::{SolanaClient, SolanaClientConfig};
use algotraderv2::config::Config;
use algotraderv2::monitoring::{PositionSnapshot, TradingMetricSnapshot};
use algotraderv2::risk::{StopLossRule, TakeProfitRule};
use algotraderv2::strategies::{
    MeanReversionStrategy, MomentumStrategy, TimeFrame, TradingStrategy, TrendFollowingConfig,
    TrendFollowingStrategy,
};
use anyhow::{anyhow, Context, Result};
use clap::{Args, Parser, Subcommand};
use chrono::Utc;
use log::{error, info, warn};
use reqwest::Client;
use solana_sdk::signature::Keypair;
use tokio::signal;
use tokio::time::interval;

#[derive(Parser)]
#[command(author, version, about = "Backtest runner + live smoke harness")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Backtest a CSV dataset
    Historical(HistoricalArgs),
    /// Launch the live runtime (used by devnet smoke)
    Live(LiveArgs),
}

#[derive(Args)]
struct HistoricalArgs {
    data_path: PathBuf,
    timeframe: String,
    strategy: String,
}

#[derive(Args)]
struct LiveArgs {
    #[arg(long, default_value = "config.toml")]
    config: PathBuf,
    #[arg(long, default_value = "wallet_mainnet.json")]
    wallet: PathBuf,
    #[arg(long)]
    dry_run: bool,
    #[arg(long, default_value_t = 0.0)]
    min_balance: f64,
    #[arg(long)]
    max_position_pct: Option<f64>,
    #[arg(long)]
    max_positions: Option<usize>,
    #[arg(long)]
    profit_targets: Option<String>,
    #[arg(long)]
    stop_loss_pct: Option<f64>,
    #[arg(long)]
    kelly_multiplier: Option<f64>,
    #[arg(long)]
    min_confidence: Option<f64>,
    #[arg(long, default_value_t = 10)]
    status_interval_secs: u64,
    #[arg(long, default_value_t = false)]
    enable_sidecar: bool,
    #[arg(long)]
    sidecar_url: Option<String>,
    #[arg(long)]
    override_max_multiplier: Option<f64>,
    #[arg(long)]
    override_max_confidence_delta: Option<f64>,
    #[arg(long)]
    override_min_guidance_score: Option<f64>,
    #[arg(long)]
    override_bypass_priorities: Option<String>,
    #[arg(long)]
    low_balance_priority_cap_lamports: Option<u64>,
    #[arg(long)]
    low_balance_priority_threshold_sol: Option<f64>,
    #[arg(long)]
    reference_sol_price_usd: Option<f64>,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Command::Historical(args) => run_historical(args).await,
        Command::Live(args) => run_live(args).await,
    }
}

async fn run_historical(args: HistoricalArgs) -> Result<()> {
    let timeframe = match args.timeframe.as_str() {
        "1m" => TimeFrame::OneMinute,
        "5m" => TimeFrame::FiveMinutes,
        "15m" => TimeFrame::FifteenMinutes,
        "1h" => TimeFrame::OneHour,
        _ => TimeFrame::OneHour,
    };

    let data_path = args.data_path;
    let strategy_str = args.strategy.to_lowercase();

    let symbol = data_path
        .file_stem()
        .and_then(|s| s.to_str())
        .map(|s| s.split('_').take(2).collect::<Vec<_>>().join("/"))
        .unwrap_or_else(|| "UNK/UNK".to_string());

    let strategy: Box<dyn TradingStrategy> = match strategy_str.as_str() {
        "momentum" => Box::new(MomentumStrategy::new(&symbol)),
        "mean_reversion" => Box::new(MeanReversionStrategy::new(
            &symbol, timeframe, 50, 1.4, 1.5, 1.0,
        )),
        "trend" | "trend_following" => {
            let cfg = TrendFollowingConfig::new(
                &symbol, timeframe, 9, 21, 55, 12, 26, 9, 14, 14, 2.5, 5.0, 10.0,
            );
            Box::new(TrendFollowingStrategy::new(cfg))
        }
        other => {
            return Err(anyhow!("Unknown strategy: {other}"));
        }
    };

    let provider = provider_for(&data_path, SimMode::Bar);
    let mut bt = Backtester {
        risk_rules: vec![
            Box::new(StopLossRule::new(0.05)),
            Box::new(TakeProfitRule::new(0.10)),
        ],
        data_provider: provider,
        timeframe: args.timeframe,
        starting_balance: 10_000.0,
        strategies: vec![strategy],
        cache: None,
        persistence: None,
        sim_mode: SimMode::Bar,
        slippage_bps: 5,
        fee_bps: 10,
        #[cfg(feature = "sidecar")]
        sidecar: None,
    };

    let report = bt.run(&data_path).await?;
    println!(
        "RESULT symbol={} strategy={} trades={} PnL={:.2} Sharpe={:.2} MaxDD={:.2}% EndBalance={:.2}",
        symbol,
        strategy_str,
        report.total_trades,
        report.realized_pnl,
        report.sharpe,
        report.max_drawdown * 100.0,
        report.ending_balance
    );
    Ok(())
}

async fn run_live(args: LiveArgs) -> Result<()> {
    let mut config = Config::from_file(&args.config)
        .with_context(|| format!("failed to load config at {}
