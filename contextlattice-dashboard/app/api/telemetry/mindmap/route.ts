import { NextRequest, NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

type RollupRow = {
  project: string;
  path: string;
  eventCount: number;
  recentEventCount: number;
  writeCount: number;
  uniqueFileCount: number;
  uniqueFiles: string[];
  uniqueAgentCount: number;
  uniqueAgents: string[];
  uniqueSessionCount: number;
  uniqueSessions: string[];
  agentIntensityScore: number;
  summarySnippets: string[];
  latestTimestamp: string | null;
  synthetic?: boolean;
};

type GraphNode = {
  id: string;
  project: string;
  path: string;
  label: string;
  depth: number;
  eventCount: number;
  recentEventCount: number;
  writeCount: number;
  uniqueFileCount: number;
  uniqueFiles: string[];
  uniqueAgentCount: number;
  uniqueAgents: string[];
  uniqueSessionCount: number;
  uniqueSessions: string[];
  agentIntensityScore: number;
  summarySnippets: string[];
  latestTimestamp: string | null;
  synthetic: boolean;
};

type GraphEdge = {
  from: string;
  to: string;
  kind?: "hierarchy" | "bridge";
};

type ProjectSummary = {
  project: string;
  topics: number;
  events: number;
  recentEvents: number;
  writes: number;
  uniqueAgents: number;
  uniqueSessions: number;
  intensity: number;
};

const DEFAULT_LIMIT = 420;
const DEFAULT_DEPTH = 4;
const DEFAULT_EXCLUDED_PREFIXES = ["root", "telemetry", "metrics", "signals", "overrides", "perf", "tmp"];
const DEFAULT_MAX_ROWS_FOCUSED = 1200;
const DEFAULT_MAX_ROWS_FULL_HISTORY = 50000;

function toNumber(value: unknown): number {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function toInt(value: unknown): number {
  return Math.max(0, Math.round(toNumber(value)));
}

function toText(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function toBool(value: unknown): boolean {
  const text = toText(value).toLowerCase();
  return text === "1" || text === "true" || text === "yes" || text === "on";
}

function envInt(name: string, fallback: number, min: number, max: number): number {
  const raw = toText(process.env[name]);
  if (!raw) return fallback;
  const parsed = Number(raw);
  if (!Number.isFinite(parsed)) return fallback;
  return Math.min(max, Math.max(min, Math.round(parsed)));
}

function normalizePath(value: unknown): string {
  const base = toText(value).replace(/\\/g, "/");
  if (!base) return "";
  return base
    .split("/")
    .map((segment) => segment.trim())
    .filter(Boolean)
    .join("/");
}

function toStringList(value: unknown, max = 24): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  const out: string[] = [];
  for (const item of value) {
    const text = toText(item);
    if (!text || out.includes(text)) {
      continue;
    }
    out.push(text);
    if (out.length >= max) {
      break;
    }
  }
  return out;
}

function excludedPrefixes(): string[] {
  const configured = toText(process.env.DASHBOARD_MINDMAP_EXCLUDED_PREFIXES);
  const raw = configured
    ? configured.split(",").map((entry) => entry.trim()).filter(Boolean)
    : DEFAULT_EXCLUDED_PREFIXES;
  const normalized = raw
    .map((entry) => normalizePath(entry).toLowerCase())
    .filter(Boolean);
  return normalized.length > 0 ? normalized : DEFAULT_EXCLUDED_PREFIXES;
}

function isExcludedPath(path: string, prefixes: string[]): boolean {
  const normalized = normalizePath(path).toLowerCase();
  if (!normalized) return false;
  return prefixes.some((prefix) => normalized === prefix || normalized.startsWith(`${prefix}/`));
}

function parseRollupRows(rawTopics: unknown[], maxDepth: number, includeSystem: boolean): RollupRow[] {
  const map = new Map<string, RollupRow>();
  const prefixes = excludedPrefixes();
  for (const raw of rawTopics) {
    if (!raw || typeof raw !== "object") {
      continue;
    }
    const row = raw as Record<string, unknown>;
    const project = toText(row.project);
    const path = normalizePath(row.path);
    if (!project || !path) {
      continue;
    }
    if (!includeSystem && isExcludedPath(path, prefixes)) {
      continue;
    }
    const depth = path.split("/").length;
    if (depth > maxDepth) {
      continue;
    }
    const key = `${project}::${path}`;
    map.set(key, {
      project,
      path,
        eventCount: toInt(row.eventCount),
        recentEventCount: toInt(row.recentEventCount),
        writeCount: toInt(row.writeCount) || toInt(row.eventCount),
        uniqueFileCount: toInt(row.uniqueFileCount),
        uniqueFiles: toStringList(row.uniqueFiles, 24),
        uniqueAgentCount: toInt(row.uniqueAgentCount),
        uniqueAgents: toStringList(row.uniqueAgents, 24),
        uniqueSessionCount: toInt(row.uniqueSessionCount),
        uniqueSessions: toStringList(row.uniqueSessions, 24),
        agentIntensityScore: toInt(row.agentIntensityScore),
        summarySnippets: toStringList(row.summarySnippets, 10),
        latestTimestamp: toText(row.latestTimestamp) || null,
        synthetic: false,
    });
  }
  return Array.from(map.values());
}

function ensurePrefixNodes(rows: RollupRow[], maxDepth: number): RollupRow[] {
  const byKey = new Map<string, RollupRow>();
  for (const row of rows) {
    byKey.set(`${row.project}::${row.path}`, row);
  }

  for (const row of rows) {
    const segments = row.path.split("/");
    for (let depth = 1; depth <= Math.min(maxDepth, segments.length); depth += 1) {
      const prefix = segments.slice(0, depth).join("/");
      const key = `${row.project}::${prefix}`;
      if (!byKey.has(key)) {
        byKey.set(key, {
          project: row.project,
          path: prefix,
          eventCount: 0,
          recentEventCount: 0,
          writeCount: 0,
          uniqueFileCount: 0,
          uniqueFiles: [],
          uniqueAgentCount: 0,
          uniqueAgents: [],
          uniqueSessionCount: 0,
          uniqueSessions: [],
          agentIntensityScore: 0,
          summarySnippets: [],
          latestTimestamp: null,
          synthetic: true,
        });
      }
    }
  }

  const childrenByKey = new Map<string, RollupRow[]>();
  for (const node of byKey.values()) {
    const parts = node.path.split("/");
    if (parts.length <= 1) continue;
    const parentPath = parts.slice(0, -1).join("/");
    const parentKey = `${node.project}::${parentPath}`;
    const bucket = childrenByKey.get(parentKey) ?? [];
    bucket.push(node);
    childrenByKey.set(parentKey, bucket);
  }

  const nodes = Array.from(byKey.values()).sort(
    (a, b) => b.path.split("/").length - a.path.split("/").length,
  );

  for (const node of nodes) {
    if (!node.synthetic) {
      continue;
    }
    const key = `${node.project}::${node.path}`;
    const children = childrenByKey.get(key) ?? [];
    if (children.length === 0) {
      continue;
    }
    node.eventCount = children.reduce((sum, child) => sum + child.eventCount, 0);
    node.recentEventCount = children.reduce((sum, child) => sum + child.recentEventCount, 0);
    node.writeCount = children.reduce((sum, child) => sum + (child.writeCount || child.eventCount), 0);
    const uniqueFiles = new Set<string>();
    const uniqueAgents = new Set<string>();
    const uniqueSessions = new Set<string>();
    for (const child of children) {
      for (const file of child.uniqueFiles) {
        uniqueFiles.add(file);
        if (uniqueFiles.size >= 120) break;
      }
      for (const agent of child.uniqueAgents) {
        uniqueAgents.add(agent);
        if (uniqueAgents.size >= 120) break;
      }
      for (const session of child.uniqueSessions) {
        uniqueSessions.add(session);
        if (uniqueSessions.size >= 120) break;
      }
      if (uniqueFiles.size >= 120) break;
    }
    node.uniqueFiles = Array.from(uniqueFiles).slice(0, 120);
    node.uniqueFileCount =
      uniqueFiles.size > 0
        ? uniqueFiles.size
        : children.reduce((sum, child) => sum + child.uniqueFileCount, 0);
    node.uniqueAgents = Array.from(uniqueAgents).slice(0, 120);
    node.uniqueAgentCount =
      uniqueAgents.size > 0
        ? uniqueAgents.size
        : children.reduce((sum, child) => sum + child.uniqueAgentCount, 0);
    node.uniqueSessions = Array.from(uniqueSessions).slice(0, 120);
    node.uniqueSessionCount =
      uniqueSessions.size > 0
        ? uniqueSessions.size
        : children.reduce((sum, child) => sum + child.uniqueSessionCount, 0);
    node.agentIntensityScore = Math.min(
      100,
      Math.max(
        ...children.map((child) => toInt(child.agentIntensityScore)),
        Math.round(Math.sqrt(Math.max(0, node.writeCount)) * 7 + node.recentEventCount * 4 + node.uniqueAgentCount * 8 + node.uniqueSessionCount * 4),
      ),
    );

    const snippets: string[] = [];
    for (const child of children) {
      for (const snippet of child.summarySnippets) {
        if (!snippets.includes(snippet)) {
          snippets.push(snippet);
          if (snippets.length >= 6) break;
        }
      }
      if (snippets.length >= 6) break;
    }
    node.summarySnippets = snippets;

    let latest: string | null = null;
    for (const child of children) {
      const ts = toText(child.latestTimestamp);
      if (!ts) continue;
      if (!latest || ts > latest) {
        latest = ts;
      }
    }
    node.latestTimestamp = latest;
  }

  return Array.from(byKey.values());
}

function buildGraph(rows: RollupRow[], project: string): { nodes: GraphNode[]; edges: GraphEdge[]; rootId: string } {
  const rootId = `${project}::__root__`;
  const nodes: GraphNode[] = [
    {
      id: rootId,
      project,
      path: "",
      label: project,
      depth: 0,
      eventCount: rows.reduce((sum, row) => sum + row.eventCount, 0),
      recentEventCount: rows.reduce((sum, row) => sum + row.recentEventCount, 0),
      writeCount: rows.reduce((sum, row) => sum + (row.writeCount || row.eventCount), 0),
      uniqueFileCount: rows.reduce((sum, row) => sum + row.uniqueFileCount, 0),
      uniqueFiles: [],
      uniqueAgentCount: rows.reduce((sum, row) => sum + row.uniqueAgentCount, 0),
      uniqueAgents: [],
      uniqueSessionCount: rows.reduce((sum, row) => sum + row.uniqueSessionCount, 0),
      uniqueSessions: [],
      agentIntensityScore: Math.max(...rows.map((row) => toInt(row.agentIntensityScore)), 0),
      summarySnippets: [],
      latestTimestamp: rows.map((row) => row.latestTimestamp).filter(Boolean).sort().at(-1) ?? null,
      synthetic: false,
    },
  ];
  const edges: GraphEdge[] = [];

  const seen = new Set<string>();
  for (const row of rows) {
    const depth = row.path.split("/").length;
    const label = row.path.split("/").at(-1) ?? row.path;
    const nodeId = `${project}::${row.path}`;
    if (!seen.has(nodeId)) {
      nodes.push({
        id: nodeId,
        project,
        path: row.path,
        label,
        depth,
        eventCount: row.eventCount,
        recentEventCount: row.recentEventCount,
        writeCount: row.writeCount || row.eventCount,
        uniqueFileCount: row.uniqueFileCount,
        uniqueFiles: row.uniqueFiles,
        uniqueAgentCount: row.uniqueAgentCount,
        uniqueAgents: row.uniqueAgents,
        uniqueSessionCount: row.uniqueSessionCount,
        uniqueSessions: row.uniqueSessions,
        agentIntensityScore: row.agentIntensityScore,
        summarySnippets: row.summarySnippets,
        latestTimestamp: row.latestTimestamp,
        synthetic: Boolean(row.synthetic),
      });
      seen.add(nodeId);
    }
    const parentPath = row.path.includes("/") ? row.path.slice(0, row.path.lastIndexOf("/")) : "";
    edges.push({
      from: parentPath ? `${project}::${parentPath}` : rootId,
      to: nodeId,
      kind: "hierarchy",
    });
  }

  nodes.sort((a, b) => {
    if (a.depth !== b.depth) return a.depth - b.depth;
    if (a.eventCount !== b.eventCount) return b.eventCount - a.eventCount;
    return a.path.localeCompare(b.path);
  });

  return { nodes, edges, rootId };
}

function anchorsForPath(path: string): string[] {
  const stop = new Set([
    "root",
    "runbooks",
    "notes",
    "docs",
    "tmp",
    "telemetry",
    "metrics",
    "signals",
    "overrides",
    "sprint",
    "project",
    "projects",
  ]);
  const parts = normalizePath(path)
    .toLowerCase()
    .split("/")
    .flatMap((segment) => segment.split(/[_\-]/))
    .map((segment) => segment.replace(/[^a-z0-9]/g, "").trim())
    .filter(Boolean)
    .filter((segment) => segment.length >= 3 && !stop.has(segment));
  if (parts.length === 0) return [];
  const anchors: string[] = [];
  const push = (value: string) => {
    const text = toText(value).toLowerCase();
    if (!text || anchors.includes(text)) return;
    anchors.push(text);
  };
  const last = parts[parts.length - 1];
  push(last);
  if (parts.length >= 2) {
    push(`${parts[parts.length - 2]}/${last}`);
  }
  push(parts[0]);
  return anchors.slice(0, 3);
}

function buildWorkspaceGraph(rows: RollupRow[]): { nodes: GraphNode[]; edges: GraphEdge[]; rootId: string; bridgeCount: number } {
  const rootId = "__workspace__::__root__";
  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  const seen = new Set<string>();
  const projectRows = new Map<string, RollupRow[]>();

  for (const row of rows) {
    const bucket = projectRows.get(row.project) ?? [];
    bucket.push(row);
    projectRows.set(row.project, bucket);
  }

  nodes.push({
    id: rootId,
    project: "__workspace__",
    path: "",
    label: "workspace",
    depth: 0,
    eventCount: rows.reduce((sum, row) => sum + row.eventCount, 0),
    recentEventCount: rows.reduce((sum, row) => sum + row.recentEventCount, 0),
    writeCount: rows.reduce((sum, row) => sum + (row.writeCount || row.eventCount), 0),
    uniqueFileCount: rows.reduce((sum, row) => sum + row.uniqueFileCount, 0),
    uniqueFiles: [],
    uniqueAgentCount: rows.reduce((sum, row) => sum + row.uniqueAgentCount, 0),
    uniqueAgents: [],
    uniqueSessionCount: rows.reduce((sum, row) => sum + row.uniqueSessionCount, 0),
    uniqueSessions: [],
    agentIntensityScore: Math.max(...rows.map((row) => toInt(row.agentIntensityScore)), 0),
    summarySnippets: [],
    latestTimestamp: rows.map((row) => row.latestTimestamp).filter(Boolean).sort().at(-1) ?? null,
    synthetic: true,
  });

  for (const [project, projectItems] of projectRows.entries()) {
    const projectNodeId = `${project}::__project__`;
    if (!seen.has(projectNodeId)) {
      nodes.push({
        id: projectNodeId,
        project,
        path: "",
        label: project,
        depth: 1,
        eventCount: projectItems.reduce((sum, row) => sum + row.eventCount, 0),
        recentEventCount: projectItems.reduce((sum, row) => sum + row.recentEventCount, 0),
        writeCount: projectItems.reduce((sum, row) => sum + (row.writeCount || row.eventCount), 0),
        uniqueFileCount: projectItems.reduce((sum, row) => sum + row.uniqueFileCount, 0),
        uniqueFiles: [],
        uniqueAgentCount: projectItems.reduce((sum, row) => sum + row.uniqueAgentCount, 0),
        uniqueAgents: [],
        uniqueSessionCount: projectItems.reduce((sum, row) => sum + row.uniqueSessionCount, 0),
        uniqueSessions: [],
        agentIntensityScore: Math.max(...projectItems.map((row) => toInt(row.agentIntensityScore)), 0),
        summarySnippets: [],
        latestTimestamp: projectItems.map((row) => row.latestTimestamp).filter(Boolean).sort().at(-1) ?? null,
        synthetic: true,
      });
      seen.add(projectNodeId);
    }
    edges.push({ from: rootId, to: projectNodeId, kind: "hierarchy" });

    for (const row of projectItems) {
      const depth = row.path.split("/").length + 1;
      const label = row.path.split("/").at(-1) ?? row.path;
      const nodeId = `${project}::${row.path}`;
      if (!seen.has(nodeId)) {
        nodes.push({
          id: nodeId,
          project,
          path: row.path,
          label,
          depth,
          eventCount: row.eventCount,
          recentEventCount: row.recentEventCount,
          writeCount: row.writeCount || row.eventCount,
          uniqueFileCount: row.uniqueFileCount,
          uniqueFiles: row.uniqueFiles,
          uniqueAgentCount: row.uniqueAgentCount,
          uniqueAgents: row.uniqueAgents,
          uniqueSessionCount: row.uniqueSessionCount,
          uniqueSessions: row.uniqueSessions,
          agentIntensityScore: row.agentIntensityScore,
          summarySnippets: row.summarySnippets,
          latestTimestamp: row.latestTimestamp,
          synthetic: Boolean(row.synthetic),
        });
        seen.add(nodeId);
      }
      const parentPath = row.path.includes("/") ? row.path.slice(0, row.path.lastIndexOf("/")) : "";
      edges.push({
        from: parentPath ? `${project}::${parentPath}` : projectNodeId,
        to: nodeId,
        kind: "hierarchy",
      });
    }
  }

  const bridgeCandidates = new Map<string, Array<{ id: string; project: string; eventCount: number; recentEventCount: number }>>();
  for (const node of nodes) {
    if (!node.path) continue;
    const anchors = anchorsForPath(node.path);
    for (const anchor of anchors) {
      if (!anchor) continue;
      const list = bridgeCandidates.get(anchor) ?? [];
      list.push({
        id: node.id,
        project: node.project,
        eventCount: node.eventCount,
        recentEventCount: node.recentEventCount,
      });
      bridgeCandidates.set(anchor, list);
    }
  }

  const edgeSet = new Set(edges.map((edge) => `${edge.from}->${edge.to}`));
  let bridgeCount = 0;
  for (const list of bridgeCandidates.values()) {
    if (list.length < 2 || list.length > 56) continue;
    const byProject = new Map<string, { id: string; project: string; eventCount: number; recentEventCount: number }>();
    for (const item of list) {
      const prev = byProject.get(item.project);
      const itemScore = item.eventCount + item.recentEventCount * 4;
      const prevScore = prev ? prev.eventCount + prev.recentEventCount * 4 : -1;
      if (!prev || itemScore > prevScore) {
        byProject.set(item.project, item);
      }
    }
    const items = Array.from(byProject.values()).sort(
      (a, b) => b.eventCount + b.recentEventCount * 4 - (a.eventCount + a.recentEventCount * 4),
    );
    if (items.length < 2) continue;
    const base = items[0];
    for (const item of items.slice(1, 5)) {
      const key = `${base.id}->${item.id}`;
      const reverse = `${item.id}->${base.id}`;
      if (edgeSet.has(key) || edgeSet.has(reverse)) continue;
      edges.push({ from: base.id, to: item.id, kind: "bridge" });
      edgeSet.add(key);
      bridgeCount += 1;
      if (bridgeCount >= 160) break;
    }
    if (bridgeCount >= 160) break;
  }

  nodes.sort((a, b) => {
    if (a.depth !== b.depth) return a.depth - b.depth;
    if (a.eventCount !== b.eventCount) return b.eventCount - a.eventCount;
    return `${a.project}/${a.path}`.localeCompare(`${b.project}/${b.path}`);
  });
  return { nodes, edges, rootId, bridgeCount };
}

export async function GET(req: NextRequest) {
  const projectFilterRaw = toText(req.nextUrl.searchParams.get("project"));
  const allProjects = projectFilterRaw.toLowerCase() === "__all__" || projectFilterRaw.toLowerCase() === "all";
  const projectFilter = allProjects ? "" : projectFilterRaw;
  const prefixFilter = normalizePath(req.nextUrl.searchParams.get("prefix"));
  const includeSystem = toBool(req.nextUrl.searchParams.get("include_system"));
  const limit = Math.min(
    5000,
    Math.max(60, toInt(req.nextUrl.searchParams.get("limit") ?? DEFAULT_LIMIT)),
  );
  const maxDepth = Math.min(
    8,
    Math.max(1, toInt(req.nextUrl.searchParams.get("depth") ?? DEFAULT_DEPTH)),
  );
  const maxRowsFocused = envInt("DASHBOARD_MINDMAP_MAX_ROWS_FOCUSED", DEFAULT_MAX_ROWS_FOCUSED, 120, 10000);
  const maxRowsFullHistory = envInt("DASHBOARD_MINDMAP_MAX_ROWS_FULL_HISTORY", DEFAULT_MAX_ROWS_FULL_HISTORY, 1000, 200000);

  try {
    const pageLimit = includeSystem
      ? Math.min(Math.max(limit, 1000), 2500)
      : Math.min(limit, 1000);
    const maxRows = includeSystem ? maxRowsFullHistory : Math.min(limit, maxRowsFocused);
    let offset = 0;
    let fetchedTotal = 0;
    let fetchLoops = 0;
    let mergedTopics: unknown[] = [];
    let payload: Record<string, unknown> = {};

    const maxFetchLoops = includeSystem ? 96 : 24;
    while (fetchLoops < maxFetchLoops && mergedTopics.length < maxRows) {
      fetchLoops += 1;
      const params = new URLSearchParams();
      params.set("limit", String(pageLimit));
      params.set("offset", String(offset));
      params.set("min_count", "1");
      if (projectFilter) params.set("project", projectFilter);
      if (prefixFilter) params.set("prefix", prefixFilter);

      const response = await fetchOrchestrator(`/memory/topic-rollups?${params.toString()}`, {
        method: "GET",
      });

      if (!response.ok) {
        const detail = (await response.text()).slice(0, 260);
        return NextResponse.json(
          {
            capturedAt: new Date().toISOString(),
            project: projectFilter || null,
            projects: [],
            rootId: null,
            nodes: [],
            edges: [],
            summary: {
              totalNodes: 0,
              totalEdges: 0,
              totalEvents: 0,
              hotTopics: 0,
            },
            error: `/memory/topic-rollups -> ${response.status}${detail ? ` ${detail}` : ""}`,
          },
          { status: 200 },
        );
      }

      const pagePayload = (await response.json()) as Record<string, unknown>;
      if (fetchLoops === 1) {
        payload = pagePayload;
      } else {
        payload = { ...payload, generatedAt: pagePayload.generatedAt, historyEntriesScanned: pagePayload.historyEntriesScanned, historyEntriesDeduped: pagePayload.historyEntriesDeduped };
      }
      const pageTopics = Array.isArray(pagePayload?.topics) ? pagePayload.topics : [];
      fetchedTotal = Math.max(fetchedTotal, toInt(pagePayload?.total));
      if (pageTopics.length === 0) break;
      mergedTopics = mergedTopics.concat(pageTopics);
      if (mergedTopics.length >= fetchedTotal || mergedTopics.length >= maxRows) break;
      offset += pageTopics.length;
      if (offset >= fetchedTotal && fetchedTotal > 0) break;
    }

    const topics = mergedTopics;
    const parsedRows = parseRollupRows(topics, maxDepth, includeSystem);

    const projectStatsMap = new Map<string, ProjectSummary>();
    const projectAgents = new Map<string, Set<string>>();
    const projectSessions = new Map<string, Set<string>>();
    for (const row of parsedRows) {
      const existing = projectStatsMap.get(row.project) ?? {
        project: row.project,
        topics: 0,
        events: 0,
        recentEvents: 0,
        writes: 0,
        uniqueAgents: 0,
        uniqueSessions: 0,
        intensity: 0,
      };
      existing.topics += 1;
      existing.events += row.eventCount;
      existing.recentEvents += row.recentEventCount;
      existing.writes += row.writeCount || row.eventCount;
      existing.intensity = Math.max(existing.intensity, row.agentIntensityScore);
      projectStatsMap.set(row.project, existing);
      const agents = projectAgents.get(row.project) ?? new Set<string>();
      for (const agent of row.uniqueAgents) {
        agents.add(agent);
      }
      projectAgents.set(row.project, agents);
      const sessions = projectSessions.get(row.project) ?? new Set<string>();
      for (const session of row.uniqueSessions) {
        sessions.add(session);
      }
      projectSessions.set(row.project, sessions);
    }
    const projects = Array.from(projectStatsMap.values()).map((project) => ({
      ...project,
      uniqueAgents: projectAgents.get(project.project)?.size ?? project.uniqueAgents,
      uniqueSessions: projectSessions.get(project.project)?.size ?? project.uniqueSessions,
    })).sort(
      (a, b) => b.events - a.events || b.topics - a.topics || a.project.localeCompare(b.project),
    );

    const selectedProject =
      allProjects
        ? "__all__"
        : (projectFilter || projects[0]?.project || toText(payload?.project) || "");

    let rows = allProjects
      ? [...parsedRows]
      : parsedRows.filter((row) => row.project === selectedProject);
    if (prefixFilter) {
      rows = rows.filter((row) => row.path.startsWith(prefixFilter));
    }
    rows = ensurePrefixNodes(rows, maxDepth);

    const topRows = [...rows].sort((a, b) => {
      if (a.eventCount !== b.eventCount) return b.eventCount - a.eventCount;
      if (a.recentEventCount !== b.recentEventCount) return b.recentEventCount - a.recentEventCount;
      return a.path.localeCompare(b.path);
    });

    const staleRows = [...rows]
      .filter((row) => Boolean(row.latestTimestamp))
      .sort((a, b) => String(a.latestTimestamp).localeCompare(String(b.latestTimestamp)));

    const built = allProjects
      ? buildWorkspaceGraph(rows)
      : { ...buildGraph(rows, selectedProject), bridgeCount: 0 };
    const { nodes, edges, rootId } = built;
    const summary = {
      totalNodes: nodes.length,
      totalEdges: edges.length,
      totalEvents: rows.reduce((sum, row) => sum + row.eventCount, 0),
      totalWrites: rows.reduce((sum, row) => sum + (row.writeCount || row.eventCount), 0),
      hotTopics: rows.filter((row) => row.recentEventCount > 0).length,
      coveredTopics: rows.filter((row) => row.summarySnippets.length > 0).length,
      bridgeEdges: built.bridgeCount,
      agentPressure: Math.max(...rows.map((row) => toInt(row.agentIntensityScore)), 0),
      uniqueAgents: new Set(rows.flatMap((row) => row.uniqueAgents)).size,
      uniqueSessions: new Set(rows.flatMap((row) => row.uniqueSessions)).size,
    };

    return NextResponse.json({
      capturedAt: new Date().toISOString(),
      generatedAt: toText(payload?.generatedAt) || null,
      historyEntriesScanned: toInt(payload?.historyEntriesScanned),
      historyEntriesDeduped: toInt(payload?.historyEntriesDeduped),
      rollupTotalTopics: toInt(payload?.total),
      rollupReturnedTopics: topics.length,
      rollupLimitRequested: pageLimit,
      rollupFetchedPages: fetchLoops,
      rollupMaxRows: maxRows,
      rollupTruncated: topics.length >= maxRows && maxRows > 0,
      rollupHistoryComplete: topics.length >= Math.max(0, toInt(payload?.total)),
      project: selectedProject || null,
      projects,
      rootId,
      nodes,
      edges,
      summary,
      topPaths: topRows.slice(0, 8),
      stalePaths: staleRows.slice(0, 8),
    });
  } catch (error) {
    return NextResponse.json(
      {
        capturedAt: new Date().toISOString(),
        project: projectFilter || null,
        projects: [],
        rootId: null,
        nodes: [],
        edges: [],
        summary: {
          totalNodes: 0,
          totalEdges: 0,
          totalEvents: 0,
          hotTopics: 0,
        },
        error: error instanceof Error ? error.message : String(error),
      },
      { status: 200 },
    );
  }
}
