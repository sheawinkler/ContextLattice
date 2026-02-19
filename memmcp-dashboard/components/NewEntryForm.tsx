"use client";

import { useState } from "react";

export function NewEntryForm() {
  const [projectName, setProjectName] = useState("");
  const [fileName, setFileName] = useState("");
  const [content, setContent] = useState("");
  const [status, setStatus] = useState<string | null>(null);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setStatus("Saving…");
    try {
      const res = await fetch("/api/memory/write", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ projectName, fileName, content }),
      });
      if (!res.ok) throw new Error(await res.text());
      setStatus("Entry saved!");
      setContent("");
    } catch (err) {
      setStatus(`Error: ${(err as Error).message}`);
    }
  }

  return (
    <section className="card">
      <h2 className="text-xl font-semibold">Add Memory Entry</h2>
      <form onSubmit={handleSubmit} className="space-y-3 mt-3">
        <div>
          <label className="block text-sm text-slate-400">Project</label>
          <input
            required
            value={projectName}
            onChange={e => setProjectName(e.target.value)}
            className="w-full border border-slate-800 rounded px-3 py-2 bg-slate-900 text-slate-100"
          />
        </div>
        <div>
          <label className="block text-sm text-slate-400">File Name</label>
          <input
            required
            value={fileName}
            onChange={e => setFileName(e.target.value)}
            className="w-full border border-slate-800 rounded px-3 py-2 bg-slate-900 text-slate-100"
          />
        </div>
        <div>
          <label className="block text-sm text-slate-400">Content</label>
          <textarea
            required
            value={content}
            onChange={e => setContent(e.target.value)}
            rows={4}
            className="w-full border border-slate-800 rounded px-3 py-2 bg-slate-900 text-slate-100"
          />
        </div>
        <button
          type="submit"
          className="px-4 py-2 bg-cyan-600 hover:bg-cyan-500 rounded text-sm font-semibold"
        >
          Save to memory bank
        </button>
        {status && <p className="text-sm text-slate-400">{status}</p>}
      </form>
    </section>
  );
}
