export type TopicFlatNode = {
  name: string;
  path: string;
  count: number;
  depth: number;
  children: TopicFlatNode[];
};

export type TokenImpactEstimate = {
  estimatedSaved: number;
  perSession: number;
  confidence: "low" | "medium" | "high";
  basis: string[];
};

export const COMPACT_NUMBER = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

export function toNumber(value: unknown, fallback = 0): number {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : fallback;
}

export function toInt(value: unknown, fallback = 0): number {
  return Math.max(0, Math.round(toNumber(value, fallback)));
}

export function toText(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

export function asArray<T = any>(value: unknown): T[] {
  return Array.isArray(value) ? (value as T[]) : [];
}

export function asRecord(value: unknown): Record<string, any> {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, any>)
    : {};
}

export function formatCompact(value: unknown): string {
  return COMPACT_NUMBER.format(toNumber(value));
}

export function formatMs(value: unknown): string {
  const ms = toNumber(value, 0);
  if (ms <= 0) return "0ms";
  if (ms >= 1000) return `${(ms / 1000).toFixed(ms >= 10000 ? 0 : 1)}s`;
  return `${ms.toFixed(ms >= 100 ? 0 : 1)}ms`;
}

export function formatTimestamp(value: unknown): string {
  const text = toText(value);
  if (!text) return "--";
  const parsed = Date.parse(text);
  if (!Number.isFinite(parsed)) return text;
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  }).format(new Date(parsed));
}

export function ageLabel(value: unknown): string {
  const text = toText(value);
  if (!text) return "--";
  const parsed = Date.parse(text);
  if (!Number.isFinite(parsed)) return "--";
  const ageSec = Math.max(0, Math.round((Date.now() - parsed) / 1000));
  if (ageSec < 60) return `${ageSec}s ago`;
  const mins = Math.round(ageSec / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

function normalizePath(path: string): string {
  return path
    .replace(/\\/g, "/")
    .split("/")
    .map((part) => part.trim())
    .filter(Boolean)
    .join("/");
}

function flattenTreeNode(name: string, raw: unknown, parent: string, depth: number): TopicFlatNode {
  const node = asRecord(raw);
  const path = normalizePath(parent ? `${parent}/${name}` : name);
  const childrenRecord = asRecord(node.children);
  const children = Object.entries(childrenRecord)
    .map(([childName, child]) => flattenTreeNode(childName, child, path, depth + 1))
    .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
  return {
    name,
    path,
    count: toInt(node.count),
    depth,
    children,
  };
}

export function flattenMemoryTopics(payload: unknown): TopicFlatNode[] {
  const root = asRecord(asRecord(payload).topics);
  const rootChildren = asRecord(root.children);
  const out: TopicFlatNode[] = [];
  const visit = (node: TopicFlatNode) => {
    out.push(node);
    node.children.forEach(visit);
  };
  Object.entries(rootChildren)
    .map(([name, raw]) => flattenTreeNode(name, raw, "", 1))
    .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name))
    .forEach(visit);
  return out;
}

export function topicCountFromTree(payload: unknown): number {
  return flattenMemoryTopics(payload).length;
}

export function estimateTokenImpact(overview: unknown, mindmap: unknown, topics: unknown): TokenImpactEstimate {
  const overviewRecord = asRecord(overview);
  const mindmapRecord = asRecord(mindmap);
  const summary = asRecord(mindmapRecord.summary);
  const memoryTelemetry = asRecord(overviewRecord.memoryTelemetry);
  const memoryBank = asRecord(memoryTelemetry.memoryBank);
  const agentRuntime = asRecord(overviewRecord.agentRuntime);
  const sessions = asArray(agentRuntime.sessions);
  const retrieval = asRecord(overviewRecord.retrieval);
  const alerts = asArray(asRecord(retrieval.alerts).active);

  const totalWrites = Math.max(
    toInt(summary.totalWrites),
    toInt(memoryBank.processed),
    toInt(asRecord(asRecord(overviewRecord.status).metadataContract).totalWrites),
  );
  const totalEvents = Math.max(toInt(summary.totalEvents), totalWrites);
  const topicCount = Math.max(toInt(summary.totalNodes), topicCountFromTree(topics));
  const coveredTopics = toInt(summary.coveredTopics);
  const sessionCount = Math.max(toInt(agentRuntime.active), sessions.length);
  const sourceCount = Object.keys(asRecord(asRecord(retrieval.latency).sources)).length;

  const writeAvoidance = totalWrites * 720;
  const topicAvoidance = topicCount * 1350;
  const coverageAvoidance = coveredTopics * 950;
  const sessionAvoidance = sessionCount * 1800;
  const sourceAvoidance = sourceCount * 550;
  const alertPenalty = alerts.length * 2400;
  const rawEstimate = writeAvoidance + topicAvoidance + coverageAvoidance + sessionAvoidance + sourceAvoidance - alertPenalty;
  const estimatedSaved = Math.max(0, Math.round(rawEstimate / 100) * 100);
  const perSession = Math.max(0, Math.round(estimatedSaved / Math.max(1, sessionCount || sessions.length || 1)));
  const confidence = totalWrites > 0 && topicCount > 0 ? (alerts.length > 0 ? "medium" : "high") : "low";

  return {
    estimatedSaved,
    perSession,
    confidence,
    basis: [
      `${formatCompact(totalWrites || totalEvents)} durable writes`,
      `${formatCompact(topicCount)} topic nodes`,
      `${formatCompact(sessionCount)} active sessions`,
      `${formatCompact(sourceCount)} retrieval lanes`,
    ],
  };
}

export function serviceHealthLabel(overview: unknown): { healthy: number; total: number; label: string } {
  const status = asRecord(asRecord(overview).status);
  const serviceHealth = asRecord(status.serviceHealth);
  const services = asArray(status.services);
  const healthy = Math.max(
    toInt(serviceHealth.healthy),
    services.filter((service) => Boolean(asRecord(service).healthy)).length,
  );
  const total = Math.max(toInt(serviceHealth.total), services.length);
  return { healthy, total, label: total ? `${healthy}/${total}` : "--" };
}
