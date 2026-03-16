import { RecentMemoryPanel } from "@/components/RecentMemoryPanel";

export default function OverviewPage() {
  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">ContextLattice Overview</h2>
        <p className="text-sm text-slate-400 mt-1">
          Snapshot of recent memory writes, queue health, and stack status.
        </p>
      </section>
      <RecentMemoryPanel />
    </div>
  );
}
