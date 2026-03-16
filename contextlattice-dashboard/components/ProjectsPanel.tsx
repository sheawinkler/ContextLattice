"use client";

import { useEffect, useState } from "react";

type Project = {
  name: string;
  files: Array<{ name: string }>;
};

export function ProjectsPanel() {
  const [projects, setProjects] = useState<Project[]>([]);
  const [selectedProject, setSelectedProject] = useState<Project | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    async function load() {
      try {
        setLoading(true);
        const res = await fetch("/api/memory/projects");
        if (!res.ok) throw new Error(`Failed to load projects: ${res.status}`);
        const data = await res.json();
        setProjects(data.projects ?? []);
        setSelectedProject(data.projects?.[0] ?? null);
      } catch (err) {
        setError((err as Error).message);
      } finally {
        setLoading(false);
      }
    }
    load();
  }, []);

  return (
    <section className="card">
      <header className="flex items-center justify-between">
        <div>
          <h2 className="text-xl font-semibold">Projects</h2>
          <p className="text-sm text-slate-400">
            Directory listing pulled from the memory-bank MCP
          </p>
        </div>
        {loading && <span className="text-xs text-amber-300">refreshing…</span>}
      </header>
      {error && <p className="text-rose-400 text-sm">{error}</p>}
      <div className="grid gap-4 md:grid-cols-3 mt-4">
        <aside className="space-y-2">
          {projects.map(project => (
            <button
              key={project.name}
              className={`w-full text-left px-3 py-2 rounded border transition ${
                selectedProject?.name === project.name
                  ? "border-cyan-400 bg-cyan-500/10"
                  : "border-slate-700 hover:border-cyan-700"
              }`}
              onClick={() => setSelectedProject(project)}
            >
              <div className="font-medium">{project.name}</div>
              <div className="text-xs text-slate-400">
                {project.files?.length ?? 0} files
              </div>
            </button>
          ))}
          {!projects.length && !loading && (
            <p className="text-sm text-slate-400">No projects yet.</p>
          )}
        </aside>
        <div className="md:col-span-2 border border-slate-800 rounded-lg p-4 min-h-[200px]">
          {selectedProject ? (
            <div>
              <h3 className="font-semibold text-lg">{selectedProject.name}</h3>
              <ul className="mt-2 space-y-1">
                {selectedProject.files?.map(file => (
                  <li
                    key={file.name}
                    className="border border-slate-800 rounded px-3 py-2 text-sm"
                  >
                    {file.name}
                  </li>
                )) ?? <li>No files</li>}
              </ul>
            </div>
          ) : (
            <p className="text-sm text-slate-400">
              Select a project to view its files.
            </p>
          )}
        </div>
      </div>
    </section>
  );
}
