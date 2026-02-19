type Severity = "info" | "warn" | "crit";

interface QueueMetrics {
  updatedAt?: string | null;
  queueDepth?: number;
  totals?: {
    dropped?: number;
  };
}

interface TradingMetrics {
  updatedAt?: string | null;
  dailyPnl?: number;
  unrealizedPnl?: number;
  openPositions?: number;
  priceCacheEntries?: number;
  priceCacheMaxAge?: number;
  priceCachePenalty?: number;
}

interface SidecarHealthData {
  updatedAt?: string | null;
  healthy?: boolean | null;
  detail?: string | null;
}

interface SignalEntry {
  symbol: string;
  risk_score: number;
  momentum_score: number;
}

interface StrategyEntry {
  name?: string;
  price_cache_entries?: number | null;
  price_cache_max_age?: number | null;
  price_cache_penalty?: number | null;
}

interface AlertItem {
  severity: Severity;
  title: string;
  detail: string;
}

export function AlertsPanel({
  queueMetrics,
  tradingMetrics,
  sidecar,
  signals,
  strategies,
}: {
  queueMetrics: QueueMetrics | null;
  tradingMetrics: TradingMetrics | null;
  sidecar: SidecarHealthData | null;
  signals?: SignalEntry[];
  strategies?: StrategyEntry[];
}) {
  const alerts = buildAlerts(queueMetrics, tradingMetrics, sidecar, signals, strategies);
  const hasAlerts = alerts.length > 0;

  return (
    <section className="card">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Alerts</h2>
        <span className="text-xs text-slate-400">
          {hasAlerts ? `${alerts.length} active` : "All systems nominal"}
        </span>
      </div>
      {hasAlerts ? (
        <ul className="mt-3 space-y-2">
          {alerts.map((alert, idx) => (
            <li
              key={`${alert.title}-${idx}`}
              className={`rounded border px-3 py-2 text-sm ${severityStyles(alert.severity)}`}
            >
              <div className="font-semibold">{alert.title}</div>
              <div className="text-slate-300 text-xs">{alert.detail}</div>
            </li>
          ))}
        </ul>
      ) : (
        <div className="text-sm text-emerald-300 mt-3">
          ✅ No outstanding alerts · keep scaling
        </div>
      )}
    </section>
  );
}

function buildAlerts(
  queueMetrics: QueueMetrics | null,
  tradingMetrics: TradingMetrics | null,
  sidecar: SidecarHealthData | null,
  signals?: SignalEntry[],
  strategies?: StrategyEntry[],
): AlertItem[] {
  const alerts: AlertItem[] = [];

  if (sidecar?.healthy === false) {
    alerts.push({
      severity: "crit",
      title: "Sidecar unhealthy",
      detail: sidecar.detail ?? "Diagnostics unavailable",
    });
  }

  const queueDepth = queueMetrics?.queueDepth ?? 0;
  if (queueDepth > 500) {
    alerts.push({
      severity: "crit",
      title: "Telemetry queue saturation",
      detail: `Depth at ${queueDepth.toLocaleString()} events`,
    });
  } else if (queueDepth > 200) {
    alerts.push({
      severity: "warn",
      title: "Telemetry queue elevated",
      detail: `Depth at ${queueDepth.toLocaleString()} events`,
    });
  }

  const dropped = queueMetrics?.totals?.dropped ?? 0;
  if (dropped > 0) {
    alerts.push({
      severity: "warn",
      title: "Events dropped",
      detail: `${dropped.toLocaleString()} telemetry events were dropped`,
    });
  }

  const dailyPnl = tradingMetrics?.dailyPnl ?? 0;
  if (dailyPnl < -500) {
    alerts.push({
      severity: "crit",
      title: "Drawdown alert",
      detail: `Daily PnL ${dailyPnl.toFixed(2)}`,
    });
  } else if (dailyPnl < 0) {
    alerts.push({
      severity: "warn",
      title: "Negative drift",
      detail: `Daily PnL ${dailyPnl.toFixed(2)}`,
    });
  }

  const unrealized = tradingMetrics?.unrealizedPnl ?? 0;
  if (unrealized < -1000) {
    alerts.push({
      severity: "crit",
      title: "Heavy unrealized loss",
      detail: `uPnL ${unrealized.toFixed(2)}`,
    });
  }

  const openPositions = tradingMetrics?.openPositions ?? 0;
  if (openPositions > 25) {
    alerts.push({
      severity: "warn",
      title: "Position count high",
      detail: `${openPositions} concurrent positions`,
    });
  }

  const entries = tradingMetrics?.priceCacheEntries ?? strategies?.[0]?.price_cache_entries ?? null;
  const maxAge = tradingMetrics?.priceCacheMaxAge ?? strategies?.[0]?.price_cache_max_age ?? null;
  const penalty = tradingMetrics?.priceCachePenalty ?? strategies?.[0]?.price_cache_penalty ?? null;
  if (entries !== null) {
    if (entries === 0) {
      alerts.push({
        severity: "crit",
        title: "Price cache empty",
        detail: "No cached quotes – oracle stalled",
      });
    }
  }
  if (maxAge !== null) {
    if (maxAge > 30) {
      alerts.push({
        severity: "crit",
        title: "Price cache stalled",
        detail: `Max age ${maxAge.toFixed(1)}s${entries ? ` · ${entries} entries` : ""}`,
      });
    } else if (maxAge > 15) {
      alerts.push({
        severity: "warn",
        title: "Price cache stale",
        detail: `Max age ${maxAge.toFixed(1)}s`,
      });
    }
  }
  if (penalty !== null) {
    if (penalty < 0.5) {
      alerts.push({
        severity: "crit",
        title: "Cache penalty severe",
        detail: `Risk multiplier x${penalty.toFixed(2)} – sizing clamped`,
      });
    } else if (penalty < 0.8) {
      alerts.push({
        severity: "warn",
        title: "Cache penalty active",
        detail: `Risk multiplier x${penalty.toFixed(2)}`,
      });
    }
  }

  if (signals && signals.length) {
    const risky = signals
      .filter((signal) => signal.risk_score >= 0.65)
      .sort((a, b) => b.risk_score - a.risk_score);
    if (risky.length) {
      alerts.push({
        severity: "info",
        title: "Risky discoveries",
        detail: `${risky.length} tokens w/ risk >= 0.65. Highest: ${
          risky[0]!.symbol
        } (${risky[0]!.risk_score.toFixed(2)})`,
      });
    }
  }

  const staleMinutes = minutesSince(tradingMetrics?.updatedAt);
  if (staleMinutes !== null && staleMinutes > 10) {
    alerts.push({
      severity: "warn",
      title: "Trading updates stale",
      detail: `${staleMinutes.toFixed(1)} minutes since last telemetry`,
    });
  }

  return alerts;
}

function severityStyles(level: Severity) {
  switch (level) {
    case "crit":
      return "border-rose-500/70 bg-rose-500/10 text-rose-100";
    case "warn":
      return "border-amber-500/70 bg-amber-500/10 text-amber-100";
    default:
      return "border-sky-500/70 bg-sky-500/10 text-sky-100";
  }
}

function minutesSince(timestamp?: string | null) {
  if (!timestamp) {
    return null;
  }
  const time = new Date(timestamp).getTime();
  if (Number.isNaN(time)) {
    return null;
  }
  const diffMs = Date.now() - time;
  return diffMs / 1000 / 60;
}
