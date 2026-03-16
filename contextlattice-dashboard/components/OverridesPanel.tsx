interface OverrideEntry {
  symbol: string;
  priority: string;
  reason?: string;
  size_before: number;
  size_after: number;
  final_size?: number;
  confidence_before: number;
  confidence_after: number;
  override_strength: number;
  multiplier: number;
  timestamp?: string;
  kelly_fraction?: number;
  kelly_target?: number;
}

function formatUsd(value: number) {
  return value.toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
}

export function OverridesPanel({
  overrides,
}: {
  overrides?: OverrideEntry[];
}) {
  if (!overrides || overrides.length === 0) {
    return null;
  }
  return (
    <section className="card">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Strategy Overrides</h2>
        <span className="text-sm text-slate-400">
          {overrides.length} recent boosts
        </span>
      </div>
      <div className="overflow-x-auto mt-3">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-left text-slate-300 border-b border-slate-700">
              <th className="py-2 pr-3">Symbol</th>
              <th className="py-2 pr-3">Priority</th>
              <th className="py-2 pr-3">Size Δ</th>
              <th className="py-2 pr-3">Kelly / Risk</th>
              <th className="py-2 pr-3">Confidence</th>
              <th className="py-2 pr-3">Strength</th>
              <th className="py-2 pr-3">Reason</th>
            </tr>
          </thead>
          <tbody>
            {overrides.map((entry, idx) => (
              <tr key={`${entry.symbol}-${entry.timestamp ?? idx}`} className="border-b border-slate-800">
                <td className="py-2 pr-3 font-semibold">{entry.symbol}</td>
                <td className="py-2 pr-3">
                  <span
                    className={`px-2 py-0.5 rounded text-xs font-semibold ${
                      entry.priority === "HIGH"
                        ? "bg-rose-500/20 text-rose-200"
                        : entry.priority === "MEDIUM"
                        ? "bg-amber-500/20 text-amber-200"
                        : "bg-slate-600/30 text-slate-200"
                    }`}
                  >
                    {entry.priority}
                  </span>
                </td>
                <td className="py-2 pr-3">
                  <div className="font-semibold">
                    {formatUsd(entry.size_before)} → {formatUsd(entry.size_after)}
                  </div>
                  <div className="text-xs text-slate-400">
                    x{entry.multiplier.toFixed(2)} multiplier
                  </div>
                </td>
                <td className="py-2 pr-3">
                  <div className="font-semibold">
                    Final {formatUsd(entry.final_size ?? entry.size_after)}
                  </div>
                  <div className="text-xs text-slate-400">
                    {entry.kelly_fraction !== undefined
                      ? `Kelly ${(entry.kelly_fraction * 100).toFixed(1)}%`
                      : "Kelly —"}
                    {entry.kelly_target !== undefined && !Number.isNaN(entry.kelly_target)
                      ? ` · Target ${entry.kelly_target.toFixed(4)} SOL`
                      : ""}
                  </div>
                </td>
                <td className="py-2 pr-3">
                  <div className="font-semibold">
                    {(entry.confidence_before * 100).toFixed(1)}% →
                    {(entry.confidence_after * 100).toFixed(1)}%
                  </div>
                </td>
                <td className="py-2 pr-3">
                  <span
                    className={`font-semibold ${
                      entry.override_strength >= 0.75
                        ? "text-emerald-400"
                        : entry.override_strength <= 0.35
                        ? "text-slate-300"
                        : "text-amber-300"
                    }`}
                  >
                    {entry.override_strength.toFixed(2)}
                  </span>
                </td>
                <td className="py-2 pr-3 text-xs text-slate-300 max-w-sm">
                  {entry.reason || "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
