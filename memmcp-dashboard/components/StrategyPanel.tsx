interface StrategyEntry {
  name?: string;
  capital?: number;
  win_rate?: number | null;
  daily_pnl?: number | null;
  kelly_fraction?: number | null;
  risk_score?: number | null;
  price_cache_entries?: number | null;
  price_cache_max_age?: number | null;
  price_cache_freshness?: number | null;
  price_cache_penalty?: number | null;
  notes?: string | null;
  memory_ref?: string | null;
}

interface Props {
  strategies: {
    updatedAt?: string | null;
    strategies?: StrategyEntry[];
  } | null;
}

export function StrategyPanel({ strategies }: Props) {
  const orchestratorBase =
    process.env.NEXT_PUBLIC_MEMMCP_ORCHESTRATOR_URL ?? "http://127.0.0.1:8075";

  const buildMemoryHref = (ref?: string | null): string | null => {
    if (!ref) {
      return null;
    }
    const parts = ref.split("/");
    if (parts.length < 2) {
      return null;
    }
    const [project, ...fileParts] = parts;
    if (!project || fileParts.length === 0) {
      return null;
    }
    const encodedProject = encodeURIComponent(project);
    const encodedPath = fileParts.map((segment) => encodeURIComponent(segment)).join("/");
    return `${orchestratorBase}/memory/files/${encodedProject}/${encodedPath}`;
  };

  const rows = strategies?.strategies ?? [];
  if (!rows.length) {
    return (
      <section className="card">
        <h2 className="text-xl font-semibold">Strategy Heatmap</h2>
        <p className="text-sm text-slate-400">No strategy telemetry received yet.</p>
      </section>
    );
  }

  return (
    <section className="card space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Strategy Heatmap</h2>
        {strategies?.updatedAt && (
          <span className="text-xs text-slate-400">
            Updated {new Date(strategies.updatedAt).toLocaleTimeString()}
          </span>
        )}
      </div>
      <div className="overflow-x-auto text-sm">
        <table className="w-full border border-slate-700/60 rounded">
          <thead className="bg-slate-900/40 text-slate-300">
            <tr>
              <th className="px-3 py-2 text-left">Strategy</th>
              <th className="px-3 py-2 text-right">Capital</th>
              <th className="px-3 py-2 text-right">Win Rate</th>
              <th className="px-3 py-2 text-right">Daily PnL</th>
              <th className="px-3 py-2 text-right">Kelly</th>
              <th className="px-3 py-2 text-right">Risk</th>
              <th className="px-3 py-2 text-right">Cache / Penalty</th>
              <th className="px-3 py-2 text-left">Notes</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row, idx) => {
              const winRate = row.win_rate ?? null;
              const pnl = row.daily_pnl ?? null;
              const memoryHref = buildMemoryHref(row.memory_ref);
              const penalty = row.price_cache_penalty ?? null;
              return (
                <tr key={`${row.name}-${idx}`} className="border-t border-slate-800">
                  <td className="px-3 py-2 font-medium">
                    <div>{row.name ?? `Strategy ${idx + 1}`}</div>
                    {memoryHref && (
                      <a
                        href={memoryHref}
                        className="text-xs text-amber-300 hover:underline"
                        target="_blank"
                        rel="noreferrer"
                      >
                        memory trace
                      </a>
                    )}
                  </td>
                  <td className="px-3 py-2 text-right">
                    ${(row.capital ?? 0).toLocaleString(undefined, {
                      maximumFractionDigits: 0,
                    })}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {winRate !== null ? `${(winRate * 100).toFixed(1)}%` : "—"}
                  </td>
                  <td
                    className={`px-3 py-2 text-right ${
                      pnl !== null && pnl < 0 ? "text-rose-300" : "text-emerald-300"
                    }`}
                  >
                    {pnl !== null ? `$${pnl.toFixed(2)}` : "—"}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {row.kelly_fraction !== null && row.kelly_fraction !== undefined
                      ? `${(row.kelly_fraction * 100).toFixed(1)}%`
                      : "—"}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {row.risk_score !== null && row.risk_score !== undefined
                      ? `${(row.risk_score * 100).toFixed(1)}%`
                      : "—"}
                  </td>
                  <td className="px-3 py-2 text-right text-xs text-slate-400 space-y-1">
                    <div>
                      {row.price_cache_entries !== null && row.price_cache_entries !== undefined
                        ? `${row.price_cache_entries} entries`
                        : "—"}
                      {row.price_cache_max_age !== null && row.price_cache_max_age !== undefined && (
                        <span className="ml-1">({row.price_cache_max_age.toFixed(1)}s)</span>
                      )}
                    </div>
                    {penalty !== null && (
                      <span className={`px-1.5 py-0.5 rounded ${cacheBadgeClass(penalty)}`}>
                        penalty x{penalty.toFixed(2)}
                      </span>
                    )}
                    {row.price_cache_freshness !== null && row.price_cache_freshness !== undefined && (
                      <div>fresh {(row.price_cache_freshness * 100).toFixed(0)}%</div>
                    )}
                  </td>
                  <td className="px-3 py-2 text-left text-slate-300">
                    {row.notes ?? ""}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function cacheBadgeClass(penalty: number) {
  if (penalty < 0.5) {
    return "bg-rose-500/15 text-rose-200 border border-rose-500/40";
  }
  if (penalty < 0.8) {
    return "bg-amber-500/15 text-amber-200 border border-amber-500/40";
  }
  return "bg-emerald-500/10 text-emerald-200 border border-emerald-500/30";
}
