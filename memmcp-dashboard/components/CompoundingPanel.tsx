interface HistoryEntry {
  timestamp?: string;
  total_value_usd?: number;
  daily_pnl?: number;
}

interface Props {
  trading: {
    totalValueUsd?: number;
    dailyPnl?: number;
    history?: HistoryEntry[];
  } | null;
}

const START_CAPITAL = Number(process.env.NEXT_PUBLIC_STARTING_CAPITAL_USD ?? 400);
const TARGET_CAPITAL = Number(process.env.NEXT_PUBLIC_TARGET_CAPITAL_USD ?? 4000000);

export function CompoundingPanel({ trading }: Props) {
  const history = Array.isArray(trading?.history) ? trading!.history : [];
  const sortedHistory = [...history].sort((a, b) => {
    const aTime = a.timestamp ? new Date(a.timestamp).getTime() : 0;
    const bTime = b.timestamp ? new Date(b.timestamp).getTime() : 0;
    return aTime - bTime;
  });
  const inferredStart = sortedHistory[0]?.total_value_usd ?? START_CAPITAL;
  const startValue = inferredStart || START_CAPITAL;
  const currentValue = Number(
    trading?.totalValueUsd ?? sortedHistory.at(-1)?.total_value_usd ?? startValue
  );
  const targetValue = TARGET_CAPITAL > startValue ? TARGET_CAPITAL : startValue * 1000;
  const dailyDelta = (() => {
    if (!sortedHistory.length) {
      return trading?.dailyPnl ?? 0;
    }
    const last = sortedHistory.at(-1);
    return last?.daily_pnl ?? trading?.dailyPnl ?? 0;
  })();

  const progressRaw = (currentValue - startValue) / (targetValue - startValue || 1);
  const progress = Number.isFinite(progressRaw)
    ? Math.min(1, Math.max(0, progressRaw))
    : 0;

  const progressPercent = (progress * 100).toFixed(2);

  return (
    <section className="card space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Compounding Trajectory</h2>
        <span className="text-xs text-slate-400">
          Target ${targetValue.toLocaleString()} (~20k SOL)
        </span>
      </div>
      <div>
        <div className="flex items-center justify-between text-sm text-slate-400">
          <span>Start ${startValue.toLocaleString()}</span>
          <span>{progressPercent}% to goal</span>
          <span>Current ${currentValue.toLocaleString()}</span>
        </div>
        <div className="mt-2 h-3 rounded bg-slate-800/80">
          <div
            className="h-3 rounded bg-gradient-to-r from-amber-400 to-emerald-400 transition-all"
            style={{ width: `${progress * 100}%` }}
          />
        </div>
      </div>
      <div className="grid md:grid-cols-3 gap-4 text-sm">
        <Stat label="Current Value" value={`$${currentValue.toLocaleString()}`} />
        <Stat
          label="Daily Change"
          value={`$${(dailyDelta ?? 0).toFixed(2)}`}
          highlight={(dailyDelta ?? 0) < 0}
        />
        <Stat label="Progress" value={`${progressPercent}%`} />
      </div>
      {sortedHistory.length > 0 && (
        <div className="text-xs text-slate-400">
          <p>
            Tracking {sortedHistory.length} entries. Latest update:
            {" "}
            {sortedHistory.at(-1)?.timestamp
              ? new Date(sortedHistory.at(-1)!.timestamp as string).toLocaleString()
              : "N/A"}
          </p>
        </div>
      )}
    </section>
  );
}

function Stat({
  label,
  value,
  highlight,
}: {
  label: string;
  value: string;
  highlight?: boolean;
}) {
  return (
    <div
      className={`rounded border px-3 py-2 ${
        highlight ? "border-rose-500 text-rose-200" : "border-slate-600 text-slate-200"
      }`}
    >
      <div className="text-xs uppercase tracking-wide text-slate-400">{label}</div>
      <div className="text-lg font-semibold">{value}</div>
    </div>
  );
}
