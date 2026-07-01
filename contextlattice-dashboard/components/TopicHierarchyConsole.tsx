"use client";

import { useEffect, useMemo, useState } from "react";
import { asArray, asRecord, flattenMemoryTopics, formatCompact, formatTimestamp, toInt, toText } from "@/lib/dashboardMetrics";

type TopicTree = {
  id: string;
  label: string;
  path: string;
  eventCount: number;
  recentEventCount: number;
  uniqueAgents: number;
  uniqueSessions: number;
  children: TopicTree[];
};

async function getJson(path: string): Promise<any | null> {
  try {
    const response = await fetch(path, { cache: "no-store" });
    return await response.json();
  } catch {
    return null;
  }
}

function buildProjectTree(nodes: any[], project: string): TopicTree[] {
  const map = new Map<string, TopicTree>();
  const roots: TopicTree[] = [];
  const rows = nodes
    .map(asRecord)
    .filter((node) => toText(node.project) === project && toText(node.path))
    .sort((a, b) => toText(a.path).split("/").length - toText(b.path).split("/").length || toText(a.path).localeCompare(toText(b.path)));

  for (const row of rows) {
    const path = toText(row.path);
    const label = toText(row.label) || path.split("/").at(-1) || path;
    map.set(path, {
      id: toText(row.id) || `${project}:${path}`,
      label,
      path,
      eventCount: toInt(row.eventCount),
      recentEventCount: toInt(row.recentEventCount),
      uniqueAgents: toInt(row.uniqueAgentCount),
      uniqueSessions: toInt(row.uniqueSessionCount),
      children: [],
    });
  }

  for (const item of map.values()) {
    const parentPath = item.path.includes("/") ? item.path.slice(0, item.path.lastIndexOf("/")) : "";
    const parent = parentPath ? map.get(parentPath) : null;
    if (parent) parent.children.push(item);
    else roots.push(item);
  }

  const sortTree = (items: TopicTree[]) => {
    items.sort((a, b) => b.eventCount - a.eventCount || a.label.localeCompare(b.label));
    items.forEach((item) => sortTree(item.children));
  };
  sortTree(roots);
  return roots;
}

function TreeBranch({ node, depth = 1 }: { node: TopicTree; depth?: number }) {
  const open = depth <= 2;
  return (
    <details className="cl-tree-branch" open={open}>
      <summary>
        <span className="cl-tree-label">{node.label}</span>
        <span className="cl-tree-path">{node.path}</span>
        <span className="cl-tree-count">{formatCompact(node.eventCount)}</span>
      </summary>
      <div className="cl-tree-meta">
        <span>{formatCompact(node.recentEventCount)} recent</span>
        <span>{formatCompact(node.uniqueAgents)} agents</span>
        <span>{formatCompact(node.uniqueSessions)} sessions</span>
      </div>
      {node.children.length ? (
        <div className="cl-tree-children">
          {node.children.map((child) => <TreeBranch key={child.id} node={child} depth={depth + 1} />)}
        </div>
      ) : null}
    </details>
  );
}

export function TopicHierarchyConsole() {
  const [mindmap, setMindmap] = useState<any | null>(null);
  const [topics, setTopics] = useState<any | null>(null);
  const [selectedProject, setSelectedProject] = useState<string>("");

  useEffect(() => {
    let mounted = true;
    Promise.all([
      getJson("/api/telemetry/mindmap?project=__all__&depth=5&limit=1200"),
      getJson("/api/memory/topics?depth=5"),
    ]).then(([mindmapData, topicsData]) => {
      if (!mounted) return;
      setMindmap(mindmapData);
      setTopics(topicsData);
    });
    return () => {
      mounted = false;
    };
  }, []);

  const projects = asArray(asRecord(mindmap).projects).map(asRecord);
  const activeProject = selectedProject || toText(projects[0]?.project) || "";
  const tree = useMemo(() => buildProjectTree(asArray(asRecord(mindmap).nodes), activeProject), [mindmap, activeProject]);
  const simpleTopics = useMemo(() => flattenMemoryTopics(topics).slice(0, 18), [topics]);
  const summary = asRecord(asRecord(mindmap).summary);
  const topPaths = asArray(asRecord(mindmap).topPaths).slice(0, 8).map(asRecord);
  const stalePaths = asArray(asRecord(mindmap).stalePaths).slice(0, 6).map(asRecord);

  return (
    <div className="cl-page cl-topic-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Topics // memory shape</p>
          <h2>Projects, branches, and the places your agents keep returning to.</h2>
          <p>
            This is the lattice as a hierarchy: project roots, topic trunks, subtopics, active leaves,
            and the heat they carry across agents and sessions.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>rollup nodes</span>
          <strong>{formatCompact(toInt(summary.totalNodes))}</strong>
          <small>{formatTimestamp(asRecord(mindmap).capturedAt)}</small>
        </div>
      </section>

      <section className="cl-topic-layout">
        <aside className="cl-panel cl-project-rail">
          <p className="cl-kicker">projects</p>
          <div className="cl-project-list">
            {projects.map((project) => {
              const name = toText(project.project);
              return (
                <button
                  key={name}
                  className={`cl-project-button ${name === activeProject ? "cl-project-button--active" : ""}`}
                  type="button"
                  onClick={() => setSelectedProject(name)}
                >
                  <span>{name || "workspace"}</span>
                  <strong>{formatCompact(toInt(project.events))}</strong>
                </button>
              );
            })}
            {!projects.length ? <p className="cl-empty">No project rollups found.</p> : null}
          </div>
        </aside>

        <section className="cl-panel cl-topic-tree-panel">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">selected hierarchy</p>
              <h3>{activeProject || "workspace"}</h3>
            </div>
            <span className="cl-badge">{tree.length} roots</span>
          </div>
          <div className="cl-tree">
            {tree.length ? tree.map((node) => <TreeBranch key={node.id} node={node} />) : (
              <div className="cl-fallback-topics">
                {simpleTopics.length ? simpleTopics.map((topic) => (
                  <div key={topic.path} className="cl-topic-row">
                    <span>{topic.path}</span>
                    <strong>{formatCompact(topic.count)}</strong>
                  </div>
                )) : <p className="cl-empty">Topic tree is still warming.</p>}
              </div>
            )}
          </div>
        </section>

        <aside className="cl-panel cl-topic-sidecar">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">hot branches</p>
              <h3>Most written</h3>
            </div>
          </div>
          <div className="cl-topic-list">
            {topPaths.map((row) => (
              <div className="cl-topic-row" key={`${toText(row.project)}:${toText(row.path)}`}>
                <span>{toText(row.path) || "root"}</span>
                <strong>{formatCompact(toInt(row.eventCount))}</strong>
              </div>
            ))}
          </div>
          <div className="cl-divider" />
          <p className="cl-kicker">cold branches</p>
          <div className="cl-topic-list">
            {stalePaths.map((row) => (
              <div className="cl-topic-row" key={`${toText(row.project)}:${toText(row.path)}:stale`}>
                <span>{toText(row.path) || "root"}</span>
                <strong>{formatTimestamp(row.latestTimestamp)}</strong>
              </div>
            ))}
            {!stalePaths.length ? <p className="cl-empty">No stale branch data yet.</p> : null}
          </div>
        </aside>
      </section>
    </div>
  );
}
