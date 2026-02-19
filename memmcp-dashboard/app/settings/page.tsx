"use client";

import { useEffect, useState } from "react";

type Workspace = {
  id: string;
  name: string;
  slug: string;
  role: string;
  status?: string;
};

type ApiKey = {
  id: string;
  name: string;
  prefix: string;
  scopes?: string | null;
  createdAt: string;
  lastUsedAt?: string | null;
};

type ScimToken = {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  lastUsedAt?: string | null;
};

type Budget = {
  tokenLimit?: number | null;
  costLimitUsd?: number | null;
};

type UsageSummary = {
  tokens: number;
  costUsd: number;
};

type AuditLog = {
  id: string;
  action: string;
  targetType?: string | null;
  targetId?: string | null;
  createdAt: string;
};

export default function SettingsPage() {
  const [workspace, setWorkspace] = useState<Workspace | null>(null);
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [newKeyName, setNewKeyName] = useState("Default key");
  const [newKeyScopes, setNewKeyScopes] = useState("memory:write,usage:write");
  const [newKeyValue, setNewKeyValue] = useState<string | null>(null);
  const [keyLimit, setKeyLimit] = useState<number | null>(null);
  const [planId, setPlanId] = useState<string | null>(null);
  const [scimTokens, setScimTokens] = useState<ScimToken[]>([]);
  const [newScimName, setNewScimName] = useState("SCIM token");
  const [newScimTokenValue, setNewScimTokenValue] = useState<string | null>(null);
  const [budget, setBudget] = useState<Budget | null>(null);
  const [usage, setUsage] = useState<UsageSummary | null>(null);
  const [tokenLimit, setTokenLimit] = useState<string>("");
  const [costLimit, setCostLimit] = useState<string>("");
  const [message, setMessage] = useState<string | null>(null);
  const [auditLogs, setAuditLogs] = useState<AuditLog[]>([]);

  async function loadWorkspace() {
    const res = await fetch("/api/workspace/current");
    if (!res.ok) return;
    const data = await res.json();
    setWorkspace(data.workspace);
  }

  async function loadKeys() {
    const res = await fetch("/api/workspace/api-keys");
    if (!res.ok) return;
    const data = await res.json();
    setKeys(data.keys || []);
    setKeyLimit(
      data.limit === null || typeof data.limit === "number" ? data.limit : null,
    );
    setPlanId(typeof data.planId === "string" ? data.planId : null);
  }

  async function loadScimTokens() {
    const res = await fetch("/api/workspace/scim-tokens");
    if (!res.ok) return;
    const data = await res.json();
    setScimTokens(data.tokens || []);
  }

  async function loadBudget() {
    const res = await fetch("/api/workspace/budget");
    if (!res.ok) return;
    const data = await res.json();
    setBudget(data.budget || null);
    setUsage(data.usage || null);
  }

  async function loadAuditLogs() {
    const res = await fetch("/api/workspace/audit?limit=20");
    if (!res.ok) return;
    const data = await res.json();
    setAuditLogs(data.logs || []);
  }

  useEffect(() => {
    loadWorkspace();
    loadKeys();
    loadBudget();
    loadAuditLogs();
    loadScimTokens();
  }, []);

  async function createKey() {
    setMessage(null);
    setNewKeyValue(null);
    const res = await fetch("/api/workspace/api-keys", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: newKeyName, scopes: newKeyScopes }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to create key");
      return;
    }
    setNewKeyValue(data.key?.apiKey || null);
    loadKeys();
  }

  async function revokeKey(id: string) {
    setMessage(null);
    const res = await fetch(`/api/workspace/api-keys/${id}`, { method: "DELETE" });
    if (!res.ok) {
      setMessage("Failed to revoke key");
      return;
    }
    loadKeys();
  }

  async function createScimToken() {
    setMessage(null);
    setNewScimTokenValue(null);
    const res = await fetch("/api/workspace/scim-tokens", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: newScimName }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to create SCIM token");
      return;
    }
    setNewScimTokenValue(data.token?.token || null);
    loadScimTokens();
  }

  async function revokeScimToken(id: string) {
    setMessage(null);
    const res = await fetch(`/api/workspace/scim-tokens/${id}`, {
      method: "DELETE",
    });
    if (!res.ok) {
      setMessage("Failed to revoke SCIM token");
      return;
    }
    loadScimTokens();
  }

  async function saveBudget() {
    setMessage(null);
    const tokenLimitNum = tokenLimit ? Number(tokenLimit) : null;
    const costLimitNum = costLimit ? Number(costLimit) : null;
    const res = await fetch("/api/workspace/budget", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        tokenLimit: Number.isFinite(tokenLimitNum) ? tokenLimitNum : null,
        costLimitUsd: Number.isFinite(costLimitNum) ? costLimitNum : null,
      }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to save budget");
      return;
    }
    setBudget(data.budget);
    setTokenLimit("");
    setCostLimit("");
  }

  async function exportWorkspace() {
    setMessage(null);
    const res = await fetch("/api/workspace/export");
    if (!res.ok) {
      setMessage("Failed to export workspace");
      return;
    }
    const data = await res.json();
    const blob = new Blob([JSON.stringify(data, null, 2)], {
      type: "application/json",
    });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `contextlattice-export-${Date.now()}.json`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  async function requestDeletion() {
    setMessage(null);
    if (!confirm("This will request workspace deletion and revoke API keys. Continue?")) {
      return;
    }
    const res = await fetch("/api/workspace/delete", { method: "POST" });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to request deletion");
      return;
    }
    setMessage("Deletion request submitted.");
    loadWorkspace();
    loadKeys();
    loadAuditLogs();
  }

  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Workspace</h2>
        {workspace ? (
          <div className="text-sm text-slate-300 mt-2 space-y-1">
            <div>Name: {workspace.name}</div>
            <div>Slug: {workspace.slug}</div>
            <div>Role: {workspace.role}</div>
            {workspace.status ? <div>Status: {workspace.status}</div> : null}
          </div>
        ) : (
          <p className="text-sm text-slate-400 mt-2">
            Sign in to manage workspace settings.
          </p>
        )}
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">API keys</h3>
        <p className="text-sm text-slate-400">
          Keys are shown once on creation. Store them securely.
        </p>
        <p className="text-xs text-slate-500">
          Tip: add <code>audit:write</code> if you want retention/export scripts to
          write audit logs.
        </p>
        {planId ? (
          <p className="text-xs text-slate-500">
            Plan: {planId} • Keys: {keys.length}/
            {keyLimit === null ? "unlimited" : keyLimit}
          </p>
        ) : null}
        <div className="flex flex-wrap gap-2">
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            value={newKeyName}
            onChange={(e) => setNewKeyName(e.target.value)}
          />
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            value={newKeyScopes}
            onChange={(e) => setNewKeyScopes(e.target.value)}
          />
          <button
            className="rounded bg-emerald-500 text-emerald-950 px-3 py-2 text-sm font-semibold"
            onClick={createKey}
          >
            Create key
          </button>
        </div>
        {newKeyValue ? (
          <div className="rounded border border-emerald-600/60 bg-emerald-900/20 p-3 text-sm">
            <div className="text-emerald-200 font-semibold mb-1">
              New API key (copy now)
            </div>
            <code className="break-all">{newKeyValue}</code>
          </div>
        ) : null}
        <div className="space-y-2 text-sm">
          {keys.length === 0 ? (
            <p className="text-slate-400">No keys yet.</p>
          ) : (
            keys.map((key) => (
              <div
                key={key.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded border border-slate-800 px-3 py-2"
              >
                <div>
                  <div className="font-semibold">{key.name}</div>
                  <div className="text-xs text-slate-400">
                    {key.prefix}… • {key.scopes || "default scopes"} • created{" "}
                    {new Date(key.createdAt).toLocaleDateString()}
                  </div>
                </div>
                <button
                  className="text-xs text-amber-300 hover:text-amber-200"
                  onClick={() => revokeKey(key.id)}
                >
                  Revoke
                </button>
              </div>
            ))
          )}
        </div>
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">SCIM provisioning</h3>
        <p className="text-sm text-slate-400">
          Generate a SCIM token to integrate with IdPs. The SCIM endpoints are
          scaffolded but provisioning is not yet active.
        </p>
        <div className="flex flex-wrap gap-2">
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            value={newScimName}
            onChange={(e) => setNewScimName(e.target.value)}
          />
          <button
            className="rounded border border-slate-700 px-3 py-2 text-sm"
            onClick={createScimToken}
          >
            Create SCIM token
          </button>
        </div>
        {newScimTokenValue ? (
          <div className="rounded border border-emerald-600/60 bg-emerald-900/20 p-3 text-sm">
            <div className="text-emerald-200 font-semibold mb-1">
              New SCIM token (copy now)
            </div>
            <code className="break-all">{newScimTokenValue}</code>
          </div>
        ) : null}
        <div className="space-y-2 text-sm">
          {scimTokens.length === 0 ? (
            <p className="text-slate-400">No SCIM tokens yet.</p>
          ) : (
            scimTokens.map((token) => (
              <div
                key={token.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded border border-slate-800 px-3 py-2"
              >
                <div>
                  <div className="font-semibold">{token.name}</div>
                  <div className="text-xs text-slate-400">
                    {token.prefix}… • created{" "}
                    {new Date(token.createdAt).toLocaleDateString()}
                  </div>
                </div>
                <button
                  className="text-xs text-amber-300 hover:text-amber-200"
                  onClick={() => revokeScimToken(token.id)}
                >
                  Revoke
                </button>
              </div>
            ))
          )}
        </div>
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Usage budgets</h3>
        <p className="text-sm text-slate-400">
          Budgets apply to the current month. Enforcement is on by default
          (set <code className="ml-1">ENFORCE_BUDGETS=false</code> to disable).
        </p>
        <div className="flex flex-wrap gap-2">
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            placeholder="Token limit"
            value={tokenLimit}
            onChange={(e) => setTokenLimit(e.target.value)}
          />
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            placeholder="Cost limit (USD)"
            value={costLimit}
            onChange={(e) => setCostLimit(e.target.value)}
          />
          <button
            className="rounded border border-slate-700 px-3 py-2 text-sm"
            onClick={saveBudget}
          >
            Save budget
          </button>
        </div>
        {budget ? (
          <div className="text-sm text-slate-300 space-y-1">
            <div>
              Active budget: {budget.tokenLimit ?? "no token limit"} tokens /{" "}
              {budget.costLimitUsd ?? "no cost limit"} USD
            </div>
            {usage ? (
              <div>
                Month-to-date: {usage.tokens} tokens / ${usage.costUsd.toFixed(2)}
              </div>
            ) : null}
          </div>
        ) : (
          <p className="text-sm text-slate-400">No active budget set.</p>
        )}
      </section>

      {message ? (
        <p className="text-sm text-amber-300">{message}</p>
      ) : null}

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Data export</h3>
        <p className="text-sm text-slate-400">
          Export workspace metadata, usage, and audit logs. Memory bank files are
          exported separately from the memorymcp store.
        </p>
        <button
          className="rounded border border-slate-700 px-3 py-2 text-sm"
          onClick={exportWorkspace}
        >
          Download export JSON
        </button>
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Deletion request</h3>
        <p className="text-sm text-slate-400">
          Mark the workspace for deletion. API keys will be revoked immediately.
        </p>
        <button
          className="rounded border border-rose-500/60 text-rose-200 px-3 py-2 text-sm"
          onClick={requestDeletion}
        >
          Request deletion
        </button>
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Audit log</h3>
        {auditLogs.length === 0 ? (
          <p className="text-sm text-slate-400">No audit events yet.</p>
        ) : (
          <div className="space-y-2 text-sm">
            {auditLogs.map((log) => (
              <div
                key={log.id}
                className="flex flex-wrap items-center justify-between gap-2 rounded border border-slate-800 px-3 py-2"
              >
                <div>
                  <div className="font-semibold">{log.action}</div>
                  <div className="text-xs text-slate-400">
                    {log.targetType || "system"} •{" "}
                    {new Date(log.createdAt).toLocaleString()}
                  </div>
                </div>
                <div className="text-xs text-slate-500">
                  {log.targetId ? log.targetId.slice(0, 8) : ""}
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}
