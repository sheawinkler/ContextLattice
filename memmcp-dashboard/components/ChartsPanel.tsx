interface TradingHistoryEntry {
  timestamp?: string;
  total_value_usd?: number;
  daily_pnl?: number;
}

interface SidecarHistoryEntry {
  timestamp?: string;
  healthy: boolean;
}

interface Props {
  trading?: {
    history?: TradingHistoryEntry[];
  } | null;
  sidecar?: {
    history?: SidecarHistoryEntry[];
  } | null;
}

interface ChartSeriesPoint {
  value: number;
  label?: string;
}

export function ChartsPanel({ trading, sidecar }: Props) {
  const tradingHistory = Array.isArray(trading?.history) ? trading!.history : [];
  const equitySeries = buildSeries(tradingHistory, (entry) => entry.total_value_usd);
  const pnlSeries = buildSeries(tradingHistory, (entry) => entry.daily_pnl);
  const uptimeSeries = buildSeries(sidecar?.history ?? [], (entry) =>
    entry.healthy ? 1 : 0,
  );

  if (!equitySeries.length && !pnlSeries.length && !uptimeSeries.length) {
    return null;
  }

  return (
    <section className="card space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Performance Charts</h2>
        <span className="text-xs text-slate-400">
          Rendering {equitySeries.length + pnlSeries.length + uptimeSeries.length} series
        </span>
      </div>
      <div className="grid gap-4 md:grid-cols-3 text-sm">
        {equitySeries.length > 1 && (
          <ChartCard
            title="Equity Curve"
            subtitle="Portfolio value"
            series={equitySeries}
            formatValue={(value) => `$${value.toLocaleString(undefined, { maximumFractionDigits: 0 })}`}
          />
        )}
        {pnlSeries.length > 1 && (
          <ChartCard
            title="Daily PnL"
            subtitle="24h realized"
            series={pnlSeries}
            formatValue={(value) => `$${value.toFixed(2)}`}
          />
        )}
        {uptimeSeries.length > 1 && (
          <ChartCard
            title="Sidecar Uptime"
            subtitle="Healthy vs unhealthy"
            series={uptimeSeries}
            formatValue={(value) => (value >= 1 ? "Healthy" : "Unhealthy")}
          />
        )}
      </div>
    </section>
  );
}

function ChartCard({
  title,
  subtitle,
  series,
  formatValue,
}: {
  title: string;
  subtitle: string;
  series: ChartSeriesPoint[];
  formatValue: (value: number) => string;
}) {
  const latest = series.at(-1)?.value ?? 0;
  const previous = series[0]?.value ?? latest;
  const delta = latest - previous;
  const deltaPercent = previous !== 0 ? (delta / Math.abs(previous)) * 100 : 0;
  const positive = delta >= 0;

  return (
    <div className="rounded border border-slate-700/60 p-3 space-y-2">
      <div className="flex items-center justify-between">
        <div className="text-sm font-semibold text-slate-200">{title}</div>
        <span
          className={`text-xs font-semibold ${
            positive ? "text-emerald-300" : "text-rose-300"
          }`}
        >
          {positive ? "▲" : "▼"} {deltaPercent.toFixed(2)}%
        </span>
      </div>
      <div className="flex items-center justify-between">
        <div>
          <div className="text-xs uppercase tracking-wide text-slate-400">{subtitle}</div>
          <div className="text-lg font-semibold">{formatValue(latest)}</div>
        </div>
      </div>
      <div className="mt-3 h-24">
        <HistoryChart points={series} positive={positive} />
      </div>
      <div className="mt-2 text-xs text-slate-400">
        Last {series.length} points · Change {formatValue(delta)}
      </div>
    </div>
  );
}

function HistoryChart({ points, positive }: { points: ChartSeriesPoint[]; positive: boolean }) {
  if (points.length < 2) {
    return <div className="h-full w-full bg-slate-900/60 rounded" />;
  }
  const values = points.map((point) => point.value);
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const normalized = points.map((point, idx) => {
    const x = (idx / (points.length - 1)) * 100;
    const y = 100 - ((point.value - min) / range) * 100;
    return `${x},${y}`;
  });
  const strokeColor = positive ? "#34d399" : "#fb7185";
  const fillColor = positive ? "rgba(52, 211, 153, 0.12)" : "rgba(251, 113, 133, 0.12)";

  const path = `M0,100 L${normalized.join(" ")} L100,100 Z`;

  return (
    <svg viewBox="0 0 100 100" preserveAspectRatio="none" className="w-full h-full">
      <path d={path} fill={fillColor} stroke="none" />
      <polyline points={normalized.join(" ")} fill="none" stroke={strokeColor} strokeWidth={2} />
    </svg>
  );
}

function buildSeries<T>(entries: T[], extractor: (entry: T) => number | null | undefined) {
  const result: ChartSeriesPoint[] = [];
  entries.forEach((entry) => {
    const valueRaw = extractor(entry);
    if (valueRaw === null || valueRaw === undefined) {
      return;
    }
    const value = Number(valueRaw);
    if (!Number.isFinite(value)) {
      return;
    }
    result.push({ value, label: (entry as any)?.timestamp });
  });
  return result;
}
