import type { ReactNode } from "react";
import { buildApiUrl } from "@/lib/serverFetch";

async function fetchRecent() {
  const res = await fetch(buildApiUrl("/api/memory/recent"), {
    cache: "no-store",
  });
  if (!res.ok) {
    return { ok: false, items: [] };
  }
  return res.json();
}

function Row({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs uppercase tracking-widest text-slate-500">
        {label}
      </span>
      <span className="text-sm text-slate-200">{value}</span>
    </div>
  );
}

export async function RecentMemoryPanel() {
  const data = await fetchRecent();
  const items = data?.items || [];
  return (
    <section className="card">
      <h3 className="text-lg font-semibold">Recent memory writes</h3>
      {items.length === 0 ? (
        <p className="text-sm text-slate-400 mt-2">No recent entries yet.</p>
      ) : (
        <div className="mt-3 space-y-3">
          {items.map((item: any) => (
            <div key={item.path} className="border border-slate-800 rounded p-3">
              <Row label="Project" value={item.project} />
              <Row label="File" value={item.path} />
              <Row label="Timestamp" value={item.timestamp} />
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
