"use client";

import { useEffect, useMemo, useState } from "react";
import { API_ENDPOINTS, PAGE_ENDPOINTS, type EndpointTarget } from "@/lib/endpointCatalog";
import { asArray, asRecord, formatCompact, formatMs, serviceHealthLabel, toInt, toNumber, toText } from "@/lib/dashboardMetrics";

type EndpointCheck = EndpointTarget & {
  status: number | null;
  latencyMs: number;
  ok: boolean;
  skipped?: boolean;
};

async function getJson(path: string): Promise<any | null> {
  try {
    const response = await fetch(path, { cache: "no-store" });
    return await response.json();
  } catch {
    return null;
  }
}

async function checkEndpoint(target: EndpointTarget): Promise<EndpointCheck> {
  const authOrHostedProbe =
    target.allowedStatuses.includes(401) ||
    target.allowedStatuses.includes(503) ||
    target.path.startsWith("/auth/");
  if (authOrHostedProbe) {
    return {
      ...target,
      status: null,
      latencyMs: 0,
      ok: true,
      skipped: true,
    };
  }

  const started = performance.now();
  try {
    const response = await fetch(target.path, { method: "GET", cache: "no-store", redirect: "follow" });
    const latencyMs = performance.now() - started;
    return {
      ...target,
      status: response.status,
      latencyMs,
      ok: target.allowedStatuses.includes(response.status),
    };
  } catch {
    return {
      ...target,
      status: null,
      latencyMs: performance.now() - started,
      ok: false,
    };
  }
}

function ServiceTile({ service }: { service: any }) {
  const row = asRecord(service);
  const healthy = Boolean(row.healthy);
  return (
    <article className={`cl-service-tile ${healthy ? "cl-service-tile--good" : "cl-service-tile--bad"}`}>
      <div>
        <strong>{toText(row.name) || "service"}</strong>
        <p>{toText(row.detail) || toText(row.status) || "--"}</p>
      </div>
      <span>{healthy ? "up" : "down"}</span>
    </article>
  );
}

function EndpointTable({ title, rows }: { title: string; rows: EndpointCheck[] }) {
  return (
    <section className="cl-panel">
      <div className="cl-section-head">
        <div>
          <p className="cl-kicker">route checks</p>
          <h3>{title}</h3>
        </div>
        <span className="cl-badge">{rows.filter((row) => row.ok).length}/{rows.length}</span>
      </div>
      <div className="cl-table-wrap">
        <table className="cl-table">
          <thead>
            <tr>
              <th>path</th>
              <th>status</th>
              <th>latency</th>
              <th>purpose</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.path}>
                <td>{row.path}</td>
                <td><span className={`cl-status-pill ${row.ok ? "cl-status-pill--good" : "cl-status-pill--bad"}`}>{row.skipped ? "POLICY" : row.status ?? "ERR"}</span></td>
                <td>{row.skipped ? "--" : formatMs(row.latencyMs)}</td>
                <td>{row.note}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

export function RuntimeStatusBoard() {
  const [overview, setOverview] = useState<any | null>(null);
  const [health, setHealth] = useState<any | null>(null);
  const [checks, setChecks] = useState<EndpointCheck[]>([]);
  const [updatedAt, setUpdatedAt] = useState("");

  useEffect(() => {
    let mounted = true;
    async function load() {
      const [overviewData, healthData, endpointChecks] = await Promise.all([
        getJson("/api/telemetry/overview?recallHistory=24&toolLimit=16"),
        getJson("/api/health"),
        Promise.all([...PAGE_ENDPOINTS, ...API_ENDPOINTS].map(checkEndpoint)),
      ]);
      if (!mounted) return;
      setOverview(overviewData);
      setHealth(healthData);
      setChecks(endpointChecks);
      setUpdatedAt(new Date().toLocaleTimeString());
    }
    void load();
    const handle = window.setInterval(() => void load(), 20000);
    return () => {
      mounted = false;
      window.clearInterval(handle);
    };
  }, []);

  const status = asRecord(asRecord(overview).status);
  const services = asArray(status.services);
  const healthLabel = serviceHealthLabel(overview);
  const queue = asRecord(status.queue);
  const memoryTelemetry = asRecord(asRecord(overview).memoryTelemetry);
  const fanout = asRecord(memoryTelemetry.fanout);
  const fanoutTargets = Object.entries(asRecord(fanout.targets));
  const storageGovernance = asRecord(status.storageGovernance);
  const disk = asRecord(storageGovernance.disk);
  const runtimePolicy = asRecord(status.runtimeBackendPolicy);
  const pageRows = useMemo(() => checks.filter((row) => row.kind === "page"), [checks]);
  const apiRows = useMemo(() => checks.filter((row) => row.kind === "api"), [checks]);

  return (
    <div className="cl-page cl-status-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Status // runtime truth</p>
          <h2>Not the mind map. The machine room.</h2>
          <p>
            Service health, route checks, queue pressure, storage governance, and native runtime ownership in one place.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>services</span>
          <strong>{healthLabel.label}</strong>
          <small>{toText(health?.status) || "dashboard probe"} · {updatedAt || "--"}</small>
        </div>
      </section>

      <section className="cl-metric-grid cl-metric-grid--status">
        <article className="cl-metric cl-metric--good">
          <div className="cl-label">strict hot path</div>
          <div className="cl-metric-value">{status.strictNoPythonRuntime ? "no python" : "mixed"}</div>
          <div className="cl-metric-detail">route owner: {toText(status.routeOwnerClass) || "--"}</div>
        </article>
        <article className="cl-metric">
          <div className="cl-label">vector backend</div>
          <div className="cl-metric-value">{toText(runtimePolicy.vector_backend) || "--"}</div>
          <div className="cl-metric-detail">memory bank: {toText(runtimePolicy.memory_bank_backend) || "--"}</div>
        </article>
        <article className={`cl-metric ${toNumber(disk.usedRatio) > 0.9 ? "cl-metric--warn" : ""}`}>
          <div className="cl-label">storage</div>
          <div className="cl-metric-value">{toText(storageGovernance.pressureBand) || "tracked"}</div>
          <div className="cl-metric-detail">{toText(disk.usedHuman) || "--"} used · {toText(disk.freeHuman) || "--"} free</div>
        </article>
        <article className="cl-metric">
          <div className="cl-label">continuations</div>
          <div className="cl-metric-value">{formatCompact(toInt(queue.pendingTotal))}</div>
          <div className="cl-metric-detail">{formatCompact(toInt(queue.cooldownActive))} cooldowns · {formatCompact(toInt(queue.retrying))} retrying</div>
        </article>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">services</p>
            <h3>Live dependency health</h3>
          </div>
          <span className="cl-badge">{healthLabel.label}</span>
        </div>
        <div className="cl-service-grid">
          {services.map((service) => <ServiceTile key={toText(asRecord(service).name)} service={service} />)}
          {!services.length ? <p className="cl-empty">Service payload is not available.</p> : null}
        </div>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">fanout</p>
            <h3>Write propagation targets</h3>
          </div>
          <span className="cl-badge">queue {formatCompact(toInt(fanout.queueDepth))}/{formatCompact(toInt(fanout.queueMax))}</span>
        </div>
        <div className="cl-fanout-grid">
          {fanoutTargets.map(([name, target]) => {
            const row = asRecord(target);
            return (
              <article className="cl-fanout-card" key={name}>
                <strong>{name}</strong>
                <span>{toText(row.status) || "--"}</span>
                <small>{toText(row.runtimeOwner) || "--"} · {toText(row.mode) || "--"}</small>
              </article>
            );
          })}
          {!fanoutTargets.length ? <p className="cl-empty">No fanout target telemetry yet.</p> : null}
        </div>
      </section>

      <EndpointTable title="Dashboard Pages" rows={pageRows} />
      <EndpointTable title="APIs" rows={apiRows} />
    </div>
  );
}
