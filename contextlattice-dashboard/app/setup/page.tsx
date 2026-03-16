"use client";

import { useEffect, useState } from "react";

type Service = {
  name: string;
  healthy: boolean;
  detail?: string;
};

export default function SetupPage() {
  const [status, setStatus] = useState<Service[] | null>(null);
  const [projectName, setProjectName] = useState("_global");
  const [fileName, setFileName] = useState("");
  const [content, setContent] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [message, setMessage] = useState<string | null>(null);

  useEffect(() => {
    const ts = new Date().toISOString().replace(/[:.]/g, "-");
    setFileName(`setup/first_run_${ts}.txt`);
    setContent(`First run check at ${new Date().toISOString()}`);
  }, []);

  async function checkStatus() {
    setMessage(null);
    const res = await fetch("/api/memory/status", { cache: "no-store" });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Status check failed");
      return;
    }
    setStatus(data?.services || []);
  }

  async function runSampleWrite() {
    setMessage(null);
    const res = await fetch("/api/memory/write", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        ...(apiKey ? { "x-api-key": apiKey } : {}),
      },
      body: JSON.stringify({
        projectName,
        fileName,
        content,
      }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Memory write failed");
      return;
    }
    setMessage("Memory write succeeded. You can now query the file.");
  }

  return (
    <div className="space-y-6">
      <section className="card space-y-2">
        <h2 className="text-xl font-semibold">First-run setup</h2>
        <p className="text-sm text-slate-400">
          Use this page to verify the stack is healthy and write a sample memory
          file. For CLI automation, run <code>scripts/first_run.sh</code>.
        </p>
        <ol className="text-sm text-slate-300 list-decimal list-inside space-y-1 mt-2">
          <li>Start the stack with <code>gmake mem-up</code>.</li>
          <li>Check stack status and confirm every service is healthy.</li>
          <li>Write a sample memory file to validate the write pipeline.</li>
        </ol>
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Step 1: Status check</h3>
        <button
          className="rounded border border-slate-700 px-4 py-2 text-sm"
          onClick={checkStatus}
        >
          Run status check
        </button>
        {status ? (
          <div className="grid md:grid-cols-2 gap-3">
            {status.map((svc) => (
              <div key={svc.name} className="rounded border border-slate-800 px-3 py-2">
                <div className="flex items-center justify-between">
                  <span className="font-semibold capitalize">{svc.name}</span>
                  <span
                    className={`text-xs px-2 py-1 rounded ${
                      svc.healthy
                        ? "bg-emerald-500 text-emerald-950"
                        : "bg-rose-500 text-rose-950"
                    }`}
                  >
                    {svc.healthy ? "healthy" : "down"}
                  </span>
                </div>
                <div className="text-xs text-slate-400 mt-1">{svc.detail || "—"}</div>
              </div>
            ))}
          </div>
        ) : (
          <p className="text-sm text-slate-400">
            No status data yet. Click “Run status check.”
          </p>
        )}
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Step 2: Sample write</h3>
        <p className="text-xs text-slate-400">
          If you have an API key, paste it below so the request can authenticate.
        </p>
        <input
          className="w-full rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
          placeholder="Optional API key (x-api-key)"
          value={apiKey}
          onChange={(e) => setApiKey(e.target.value)}
        />
        <div className="grid md:grid-cols-2 gap-2">
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            value={projectName}
            onChange={(e) => setProjectName(e.target.value)}
            placeholder="projectName"
          />
          <input
            className="rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
            value={fileName}
            onChange={(e) => setFileName(e.target.value)}
            placeholder="fileName"
          />
        </div>
        <textarea
          className="min-h-[120px] rounded border border-slate-700 bg-slate-900 px-3 py-2 text-sm"
          value={content}
          onChange={(e) => setContent(e.target.value)}
        />
        <button
          className="rounded bg-emerald-500 text-emerald-950 px-4 py-2 text-sm font-semibold"
          onClick={runSampleWrite}
        >
          Run sample write
        </button>
      </section>

      {message ? (
        <section className="card">
          <h3 className="text-lg font-semibold">Status</h3>
          <p className="text-sm text-slate-300 mt-2">{message}</p>
        </section>
      ) : null}
    </div>
  );
}
