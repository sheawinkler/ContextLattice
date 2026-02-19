interface HealthEntry {
  timestamp?: string;
  healthy: boolean;
  detail: string;
}

interface SidecarHealthData {
  updatedAt?: string | null;
  healthy?: boolean | null;
  detail?: string;
  history?: HealthEntry[];
}

export function SidecarHealthPanel({ data }: { data?: SidecarHealthData }) {
  if (!data) {
    return null;
  }
  const history = data.history ?? [];
  const latestState = data.healthy;
  const latestDetail = data.detail ?? "unknown";
  const latestTime = data.updatedAt
    ? new Date(data.updatedAt).toLocaleString()
    : "never";

  return (
    <section className="card">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Sidecar Health</h2>
        <span
          className={`text-sm px-2 py-0.5 rounded ${
            latestState === true
              ? "bg-emerald-500/20 text-emerald-200"
              : latestState === false
              ? "bg-rose-500/20 text-rose-200"
              : "bg-slate-600/30 text-slate-200"
          }`}
        >
          {latestState === true
            ? "Healthy"
            : latestState === false
            ? "Unhealthy"
            : "Unknown"}
        </span>
      </div>
      <div className="mt-2 text-sm text-slate-300">
        <div>Detail: {latestDetail}</div>
        <div className="text-slate-400 text-xs">Updated: {latestTime}</div>
      </div>
      {history.length > 0 && (
        <div className="mt-3">
          <h3 className="text-sm font-semibold text-slate-300">Recent events</h3>
          <ul className="text-xs text-slate-400 divide-y divide-slate-800">
            {history.slice(0, 8).map((entry, idx) => (
              <li key={`${entry.timestamp ?? idx}`} className="py-1">
                <span
                  className={`font-semibold ${
                    entry.healthy ? "text-emerald-400" : "text-rose-400"
                  }`}
                >
                  {entry.healthy ? "Healthy" : "Unhealthy"}
                </span>
                <span className="ml-2 text-slate-300">{entry.detail}</span>
                <span className="ml-2 text-slate-500">
                  {entry.timestamp
                    ? new Date(entry.timestamp).toLocaleTimeString()
                    : ""}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}
