import { ProjectsPanel } from "../components/ProjectsPanel";
import { NewEntryForm } from "../components/NewEntryForm";
import { TelemetryPanel } from "../components/TelemetryPanel";
import { CompoundingPanel } from "../components/CompoundingPanel";
import { StrategyPanel } from "../components/StrategyPanel";
import { SignalsPanel } from "../components/SignalsPanel";
import { OverridesPanel } from "../components/OverridesPanel";
import { SidecarHealthPanel } from "../components/SidecarHealthPanel";
import { ChartsPanel } from "../components/ChartsPanel";
import { AlertsPanel } from "../components/AlertsPanel";
import { buildApiUrl } from "@/lib/serverFetch";

async function fetchStatus() {
  const res = await fetch(buildApiUrl("/api/memory/status"), {
    cache: "no-store",
  });
  if (!res.ok) {
    return { ok: false, detail: await res.text() };
  }
  return res.json();
}

async function fetchTelemetryMetrics() {
  try {
    const res = await fetch(buildApiUrl("/api/telemetry/metrics"), {
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    return res.json();
  } catch (err) {
    console.warn("Telemetry metrics fetch failed", err);
    return null;
  }
}

async function fetchTradingMetrics() {
  try {
    const res = await fetch(buildApiUrl("/api/telemetry/trading"), {
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    return res.json();
  } catch (err) {
    console.warn("Trading metrics fetch failed", err);
    return null;
  }
}

async function fetchStrategyMetrics() {
  try {
    const res = await fetch(buildApiUrl("/api/telemetry/strategies"), {
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    return res.json();
  } catch (err) {
    console.warn("Strategy metrics fetch failed", err);
    return null;
  }
}

async function fetchSignals() {
  try {
    const res = await fetch(buildApiUrl("/api/signals/latest"), {
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    return res.json();
  } catch (err) {
    console.warn("Signals fetch failed", err);
    return null;
  }
}

async function fetchOverrides() {
  try {
    const res = await fetch(buildApiUrl("/api/overrides/latest"), {
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    return res.json();
  } catch (err) {
    console.warn("Overrides fetch failed", err);
    return null;
  }
}

async function fetchSidecarHealth() {
  try {
    const res = await fetch(buildApiUrl("/api/telemetry/sidecar-health"), {
      cache: "no-store",
    });
    if (!res.ok) {
      return null;
    }
    return res.json();
  } catch (err) {
    console.warn("Sidecar health fetch failed", err);
    return null;
  }
}

export default async function DashboardPage() {
  const [
    status,
    telemetry,
    trading,
    strategies,
    signals,
    overrides,
    sidecarHealth,
  ] = await Promise.all([
      fetchStatus(),
      fetchTelemetryMetrics(),
      fetchTradingMetrics(),
      fetchStrategyMetrics(),
      fetchSignals(),
      fetchOverrides(),
      fetchSidecarHealth(),
    ]);

  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Stack Health</h2>
        <div className="grid md:grid-cols-3 gap-4 mt-3 text-sm">
          {status.services?.map((svc: any) => (
            <div
              key={svc.name}
              className={`rounded border px-3 py-2 ${
                svc.healthy
                  ? "border-emerald-500 text-emerald-300"
                  : "border-rose-600 text-rose-300"
              }`}
            >
              <div className="font-semibold">{svc.name}</div>
              <div>{svc.detail}</div>
            </div>
          ))}
        </div>
      </section>
      <div className="grid gap-6 md:grid-cols-2">
        <CompoundingPanel trading={trading} />
        <SidecarHealthPanel data={sidecarHealth} />
      </div>
      <ChartsPanel trading={trading} sidecar={sidecarHealth} />
      <AlertsPanel
        queueMetrics={telemetry}
        tradingMetrics={trading}
        sidecar={sidecarHealth}
        signals={signals?.signals}
        strategies={strategies?.strategies}
      />
      <StrategyPanel strategies={strategies} />
      <SignalsPanel signals={signals?.signals} />
      <OverridesPanel overrides={overrides?.overrides} />
      <TelemetryPanel queueMetrics={telemetry} tradingMetrics={trading} />
      <ProjectsPanel />
      <NewEntryForm />
    </div>
  );
}
