interface QueueMetrics {
  updatedAt?: string | null;
  queueDepth?: number;
  batchSize?: number;
  totals?: {
    enqueued?: number;
    dropped?: number;
    batches?: number;
    flushedEvents?: number;
  };
}

interface TradingMetrics {
  updatedAt?: string | null;
  openPositions?: number;
  totalValueUsd?: number;
  unrealizedPnl?: number;
  realizedPnl?: number;
  dailyPnl?: number;
  priceCacheEntries?: number;
  priceCacheMaxAge?: number;
  priceCacheTtl?: number;
  priceCacheFreshness?: number;
  priceCachePenalty?: number;
  positions?: Array<{
    symbol?: string;
    quantity?: number;
    current_price?: number;
    entry_price?: number;
    unrealized_pnl?: number;
  }>;
  history?: TradingHistoryEntry[];
}

interface TradingHistoryEntry {
  timestamp?: string;
  daily_pnl?: number;
  total_value_usd?: number;
  open_positions?: number;
}

export function TelemetryPanel({
  queueMetrics,
  tradingMetrics,
}: {
  queueMetrics: QueueMetrics | null;
  tradingMetrics: TradingMetrics | null;
}) {
  if (!queueMetrics && !tradingMetrics) {
    return (
      <section className="card">
        <h2 className="text-xl font-semibold">Telemetry</h2>
        <p className="text-sm text-rose-300">No telemetry data available.</p>
      </section>
    );
  }

  const queue = queueMetrics ?? {};
  const totals = queue.totals ?? {};
  const trading = tradingMetrics ?? {};
  const positions = trading.positions ?? [];
  const recentHistory = Array.isArray(trading.history)
    ? trading.history.slice(-5).reverse()
    : [];

  return (
    <section className="card">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Telemetry</h2>
        {queue.updatedAt && (
          <span className="text-xs text-slate-400">
            Queue Updated {new Date(queue.updatedAt).toLocaleTimeString()}
          </span>
        )}
      </div>
      <div className="grid md:grid-cols-4 gap-4 mt-4 text-sm">
        <MetricCard label="Queue Depth" value={queue.queueDepth ?? 0} />
        <MetricCard label="Last Batch" value={queue.batchSize ?? 0} suffix="events" />
        <MetricCard label="Total Batches" value={totals.batches ?? 0} />
        <MetricCard label="Events Flushed" value={totals.flushedEvents ?? 0} />
      </div>
      <div className="grid md:grid-cols-2 gap-4 mt-4 text-sm">
        <MetricCard label="Enqueued" value={totals.enqueued ?? 0} />
        <MetricCard label="Dropped" value={totals.dropped ?? 0} highlight={Boolean(totals.dropped)} />
      </div>
      {tradingMetrics && (
        <div className="mt-6 space-y-4">
          <div className="flex items-center justify-between">
            <h3 className="text-lg font-semibold">Trading Snapshot</h3>
            {trading.updatedAt && (
              <span className="text-xs text-slate-400">
                Updated {new Date(trading.updatedAt).toLocaleTimeString()}
              </span>
            )}
          </div>
          <div className="grid md:grid-cols-4 gap-4 text-sm">
            <MetricCard label="Open Positions" value={trading.openPositions ?? 0} />
            <MetricCard
              label="Portfolio Value"
              value={Number(trading.totalValueUsd ?? 0)}
              prefix="$"
            />
            <MetricCard
              label="Unrealized PnL"
              value={Number(trading.unrealizedPnl ?? 0)}
              prefix="$"
              highlight={Number(trading.unrealizedPnl ?? 0) < 0}
            />
            <MetricCard
              label="Daily PnL"
              value={Number(trading.dailyPnl ?? 0)}
              prefix="$"
              highlight={Number(trading.dailyPnl ?? 0) < 0}
            />
          </div>
          <div className="grid md:grid-cols-3 gap-4 text-sm">
            <MetricCard
              label="Cache Entries"
              value={Number(trading.priceCacheEntries ?? 0)}
            />
            <MetricCard
              label="Cache Max Age"
              value={Number(trading.priceCacheMaxAge ?? 0)}
              suffix="s"
              highlight={Number(trading.priceCacheMaxAge ?? 0) > 15}
            />
            <MetricCard
              label="Cache Penalty"
              value={Number(trading.priceCachePenalty ?? 1)}
              highlight={Number(trading.priceCachePenalty ?? 1) < 0.8}
            />
          </div>
          {positions.length > 0 && (
            <div className="overflow-x-auto text-xs">
              <table className="w-full border border-slate-700/60 rounded">
                <thead className="bg-slate-900/40 text-slate-300">
                  <tr>
                    <th className="px-3 py-2 text-left">Symbol</th>
                    <th className="px-3 py-2 text-right">Qty</th>
                    <th className="px-3 py-2 text-right">Entry</th>
                    <th className="px-3 py-2 text-right">Current</th>
                    <th className="px-3 py-2 text-right">uPnL</th>
                  </tr>
                </thead>
                <tbody>
                  {positions.map((pos, idx) => (
                    <tr key={`${pos.symbol}-${idx}`} className="border-t border-slate-800">
                      <td className="px-3 py-2">{pos.symbol}</td>
                      <td className="px-3 py-2 text-right">
                        {(pos.quantity ?? 0).toLocaleString(undefined, { maximumFractionDigits: 4 })}
                      </td>
                      <td className="px-3 py-2 text-right">
                        ${(pos.entry_price ?? 0).toFixed(6)}
                      </td>
                      <td className="px-3 py-2 text-right">
                        ${(pos.current_price ?? 0).toFixed(6)}
                      </td>
                      <td
                        className={`px-3 py-2 text-right ${
                          (pos.unrealized_pnl ?? 0) >= 0 ? "text-emerald-300" : "text-rose-300"
                        }`}
                      >
                        ${(pos.unrealized_pnl ?? 0).toFixed(4)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {recentHistory.length > 0 && (
            <div className="text-xs mt-4">
              <h4 className="font-semibold text-sm text-slate-200">Recent PnL</h4>
              <ul className="divide-y divide-slate-800/80 border border-slate-800/80 rounded mt-2">
                {recentHistory.map((entry, idx) => {
                  const pnl = entry.daily_pnl ?? 0;
                  const color = pnl >= 0 ? "text-emerald-300" : "text-rose-300";
                  return (
                    <li key={`history-${idx}`} className="flex items-center justify-between px-3 py-2">
                      <span className="text-slate-400">
                        {entry.timestamp ? new Date(entry.timestamp).toLocaleTimeString() : ""}
                      </span>
                      <span className={`font-semibold ${color}`}>
                        ${pnl.toFixed(3)} {" · "}
                        {(entry.total_value_usd ?? 0).toLocaleString(undefined, {
                          maximumFractionDigits: 0,
                        })}
                        $ total
                      </span>
                    </li>
                  );
                })}
              </ul>
            </div>
          )}
        </div>
      )}
    </section>
  );
}

function MetricCard({
  label,
  value,
  suffix,
  highlight,
  prefix,
}: {
  label: string;
  value: number;
  suffix?: string;
  highlight?: boolean;
  prefix?: string;
}) {
  return (
    <div
      className={`rounded border px-3 py-2 ${
        highlight ? "border-amber-500 text-amber-200" : "border-slate-600 text-slate-200"
      }`}
    >
      <div className="text-xs uppercase tracking-wide text-slate-400">{label}</div>
      <div className="text-lg font-semibold">
        {prefix ?? ""}
        {value.toLocaleString()} {suffix ?? ""}
      </div>
    </div>
  );
}
