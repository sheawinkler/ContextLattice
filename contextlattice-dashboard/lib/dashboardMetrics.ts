export type TopicFlatNode = {
  name: string;
  path: string;
  count: number;
  depth: number;
  children: TopicFlatNode[];
};

export type TokenImpactConfidence = "low" | "medium" | "high";
export type TokenImpactCalibrationGrade = "heuristic" | "sampled_pack_estimate" | "measured";
export type TokenImpactFactorRole = "baseline" | "packed" | "penalty";

export type TokenImpactFactor = {
  label: string;
  value: string;
  detail: string;
  tokens: number;
  role: TokenImpactFactorRole;
};

export type TokenImpactEstimate = {
  estimatedSaved: number;
  baselineTokens: number;
  packedTokens: number;
  riskPenaltyTokens: number;
  compressionRatio: number;
  perSession: number;
  confidence: TokenImpactConfidence;
  calibrationGrade: TokenImpactCalibrationGrade;
  qualityScore: number;
  requestEquivalent: number;
  windowLabel: string;
  measurementLimit: string;
  source: string;
  basis: string[];
  factors: TokenImpactFactor[];
  warnings: string[];
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

function clampInt(value: unknown, min: number, max: number): number {
  return Math.min(max, Math.max(min, toInt(value)));
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
  const measured = measuredTokenImpactEstimate(overviewRecord);
  if (measured) {
    return measured;
  }

  const mindmapRecord = asRecord(mindmap);
  const summary = asRecord(mindmapRecord.summary);
  const memoryTelemetry = asRecord(overviewRecord.memoryTelemetry);
  const memoryBank = asRecord(memoryTelemetry.memoryBank);
  const agentRuntime = asRecord(overviewRecord.agentRuntime);
  const sessions = asArray(agentRuntime.sessions);
  const retrieval = asRecord(overviewRecord.retrieval);
  const alerts = asArray(asRecord(retrieval.alerts).active);
  const sources = Object.values(asRecord(asRecord(retrieval.latency).sources)).map(asRecord);

  const totalWrites = Math.max(
    toInt(summary.totalWrites),
    toInt(memoryBank.processed),
    toInt(asRecord(asRecord(overviewRecord.status).metadataContract).totalWrites),
  );
  const totalEvents = Math.max(toInt(summary.totalEvents), totalWrites);
  const topicCount = Math.max(toInt(summary.totalNodes), topicCountFromTree(topics));
  const coveredTopics = toInt(summary.coveredTopics);
  const hotTopics = toInt(summary.hotTopics);
  const sessionCount = Math.max(toInt(agentRuntime.active), sessions.length);
  const sourceCount = sources.length;
  const retrievalRequests = sources.reduce((sum, source) => sum + toInt(source.requests), 0);
  const retrievalErrors = sources.reduce((sum, source) => sum + toInt(source.errors), 0);
  const retrievalTimeouts = sources.reduce((sum, source) => sum + toInt(source.timeouts), 0);
  const slowSourceCount = sources.filter((source) => toNumber(source.p95Ms) > 7000).length;
  const recentTopEvents = asArray(asRecord(mindmapRecord).topPaths).reduce(
    (sum, row) => sum + toInt(asRecord(row).recentEventCount),
    0,
  );
  const workingSetCeiling = totalWrites > 0 ? Math.min(totalWrites, 1200) : 1200;
  const workingSetWrites = clampInt(
    Math.max(recentTopEvents, hotTopics * 6, Math.ceil(Math.sqrt(Math.max(totalWrites, totalEvents)) * 4), sessionCount * 8),
    0,
    workingSetCeiling,
  );

  const rawWriteTokens = workingSetWrites * 720;
  const topicBaselineTokens = topicCount * 260;
  const coveredTopicTokens = coveredTopics * 180;
  const sessionBaselineTokens = sessionCount * 1400;
  const sourceRequestTokens = Math.min(retrievalRequests, 1500) * 38;
  const baselineTokens = roundTokenCount(
    rawWriteTokens + topicBaselineTokens + coveredTopicTokens + sessionBaselineTokens + sourceRequestTokens,
  );

  const hasSignal = totalWrites > 0 || topicCount > 0 || sessionCount > 0 || sourceCount > 0;
  const packCoreTokens = hasSignal ? 1800 : 0;
  const rankedEvidenceTokens = hasSignal
    ? clampInt(sourceCount * 420 + hotTopics * 120 + Math.min(coveredTopics, 80) * 45, 1200, 6800)
    : 0;
  const rollupIndexTokens = Math.min(32000, topicCount * 90);
  const sessionDigestTokens = Math.min(22000, sessionCount * 160);
  const packedTokens = roundTokenCount(packCoreTokens + rankedEvidenceTokens + rollupIndexTokens + sessionDigestTokens);
  const riskPenaltyTokens = roundTokenCount(alerts.length * 2400 + retrievalErrors * 80 + retrievalTimeouts * 120 + slowSourceCount * 300);
  const estimatedSaved = roundTokenCount(Math.max(0, baselineTokens - packedTokens - riskPenaltyTokens));
  const perSession = Math.max(0, Math.round(estimatedSaved / Math.max(1, sessionCount || sessions.length || 1)));
  const qualityScore = clampInt(
    20 +
      (totalWrites > 0 ? 18 : 0) +
      (topicCount > 0 ? 18 : 0) +
      (coveredTopics > 0 ? 12 : 0) +
      (sourceCount > 0 ? 14 : 0) +
      (sessionCount > 0 ? 10 : 0) -
      alerts.length * 8 -
      retrievalTimeouts * 2,
    0,
    100,
  );
  const confidence: TokenImpactConfidence = qualityScore >= 70 ? "high" : qualityScore >= 42 ? "medium" : "low";
  const compressionRatio = packedTokens > 0 ? roundRatio(baselineTokens / packedTokens) : 0;
  const factors: TokenImpactFactor[] = [
    {
      label: "raw working-set writes",
      value: `${formatCompact(workingSetWrites)} writes`,
      detail: "bounded from recent heat, active sessions, and lifetime depth",
      tokens: rawWriteTokens,
      role: "baseline",
    },
    {
      label: "topic rollup breadth",
      value: `${formatCompact(topicCount)} nodes`,
      detail: "candidate branches an agent no longer has to stuff whole",
      tokens: topicBaselineTokens + coveredTopicTokens,
      role: "baseline",
    },
    {
      label: "active session memory",
      value: `${formatCompact(sessionCount)} sessions`,
      detail: "handoff and objective state compressed into recallable state",
      tokens: sessionBaselineTokens,
      role: "baseline",
    },
    {
      label: "compiled context packet",
      value: `${formatCompact(packedTokens)} tokens`,
      detail: "estimated bounded packet: core guidance, ranked evidence, rollups, session digest",
      tokens: packedTokens,
      role: "packed",
    },
    {
      label: "retrieval risk drag",
      value: `${formatCompact(alerts.length + retrievalErrors + retrievalTimeouts)} signals`,
      detail: "alerts, source errors, timeouts, and slow lanes reduce usable savings",
      tokens: riskPenaltyTokens,
      role: "penalty",
    },
  ];
  const warnings = [
    "No measured token_impact sample history is present in dashboard telemetry; using bounded working-set estimate.",
    totalWrites > workingSetWrites ? `Lifetime writes are capped to a ${formatCompact(workingSetWrites)} write working set to avoid cumulative inflation.` : "",
    alerts.length > 0 || retrievalErrors > 0 || retrievalTimeouts > 0
      ? "Retrieval pressure is reducing confidence until lane errors/timeouts settle."
      : "",
  ].filter(Boolean);

  return {
    estimatedSaved,
    baselineTokens,
    packedTokens,
    riskPenaltyTokens,
    compressionRatio,
    perSession,
    confidence,
    calibrationGrade: "heuristic",
    qualityScore,
    requestEquivalent: roundRatio(estimatedSaved / 16000),
    windowLabel: "live working set",
    measurementLimit: "Fallback estimate uses bounded working-set telemetry until sampled context-pack token_impact events are available.",
    source: "dashboard_live_telemetry",
    basis: [
      `${formatCompact(workingSetWrites)} bounded working-set writes`,
      `${formatCompact(topicCount)} topic nodes`,
      `${formatCompact(sessionCount)} active sessions`,
      `${formatCompact(sourceCount)} retrieval lanes`,
    ],
    factors,
    warnings,
  };
}

function measuredTokenImpactEstimate(overviewRecord: Record<string, any>): TokenImpactEstimate | null {
  const candidates = [
    overviewRecord.tokenImpact,
    overviewRecord.token_impact,
    asRecord(overviewRecord.status).tokenImpact,
    asRecord(overviewRecord.status).token_impact,
    asRecord(overviewRecord.telemetryMetrics).tokenImpact,
    asRecord(overviewRecord.telemetryMetrics).token_impact,
  ];
  for (const candidate of candidates) {
    const normalized = normalizeMeasuredTokenImpact(candidate);
    if (normalized) {
      return normalized;
    }
  }
  return null;
}

function normalizeMeasuredTokenImpact(candidate: unknown): TokenImpactEstimate | null {
  const record = asRecord(candidate);
  if (Object.keys(record).length === 0) {
    return null;
  }
  const baselineTokens = firstInt(record, ["baselineTokens", "baseline_tokens", "baseline_tokens_estimate"]);
  const packedTokens = firstInt(record, ["packedTokens", "packed_tokens", "packed_tokens_estimate"]);
  const explicitSaved = firstInt(record, ["savedTokens", "saved_tokens", "saved_tokens_estimate", "estimatedSaved"]);
  if (baselineTokens <= 0 || packedTokens <= 0) {
    return null;
  }
  const estimatedSaved = explicitSaved > 0 ? explicitSaved : Math.max(0, baselineTokens - packedTokens);
  const riskPenaltyTokens = firstInt(record, ["riskPenaltyTokens", "risk_penalty_tokens"]);
  const sampleCount = Math.max(1, firstInt(record, ["sampleCount", "sample_count", "samples"]));
  const sessionCount = Math.max(1, firstInt(record, ["sessionCount", "session_count", "activeSessions"]));
  const calibrationGrade = normalizeCalibrationGrade(toText(record.calibrationGrade) || toText(record.calibration_grade));
  const confidence = normalizeConfidence(toText(record.confidence), calibrationGrade === "measured" ? "high" : "medium");
  const factors = normalizeMeasuredFactors(record.factors, baselineTokens, packedTokens, riskPenaltyTokens);
  const measurementLimit =
    toText(record.measurementLimit) ||
    toText(record.measurement_limit) ||
    "Measured sample uses the upstream estimator method advertised by the token_impact payload.";

  return {
    estimatedSaved: roundTokenCount(estimatedSaved),
    baselineTokens: roundTokenCount(baselineTokens),
    packedTokens: roundTokenCount(packedTokens),
    riskPenaltyTokens: roundTokenCount(riskPenaltyTokens),
    compressionRatio: roundRatio(toNumber(record.compressionRatio) || toNumber(record.compression_ratio) || baselineTokens / Math.max(1, packedTokens)),
    perSession: Math.max(0, Math.round(estimatedSaved / sessionCount)),
    confidence,
    calibrationGrade,
    qualityScore: calibrationGrade === "measured" ? 96 : confidence === "high" ? 86 : 68,
    requestEquivalent: roundRatio(estimatedSaved / 16000),
    windowLabel: toText(record.windowLabel) || toText(record.window_label) || `${sampleCount} sampled pack${sampleCount === 1 ? "" : "s"}`,
    measurementLimit,
    source: toText(record.source) || "token_impact_payload",
    basis: asArray(record.basis).map(String).filter(Boolean).slice(0, 8),
    factors,
    warnings: [
      measurementLimit,
      toText(record.measurement_warning),
    ].filter(Boolean),
  };
}

function normalizeMeasuredFactors(
  value: unknown,
  baselineTokens: number,
  packedTokens: number,
  riskPenaltyTokens: number,
): TokenImpactFactor[] {
  const parsed = asArray(value)
    .map((item) => {
      const row = asRecord(item);
      const role = normalizeFactorRole(toText(row.role));
      const tokens = firstInt(row, ["tokens", "token_count", "estimated_tokens"]);
      const label = toText(row.label);
      if (!label || (tokens <= 0 && role !== "penalty")) {
        return null;
      }
      return {
        label,
        value: toText(row.value) || formatCompact(tokens),
        detail: toText(row.detail),
        tokens,
        role,
      } satisfies TokenImpactFactor;
    })
    .filter(Boolean) as TokenImpactFactor[];
  if (parsed.length > 0) {
    return parsed.slice(0, 8);
  }
  return [
    {
      label: "raw candidate baseline",
      value: `${formatCompact(baselineTokens)} tokens`,
      detail: "prompt-stuffing counterfactual",
      tokens: baselineTokens,
      role: "baseline",
    },
    {
      label: "compiled prompt packet",
      value: `${formatCompact(packedTokens)} tokens`,
      detail: "bounded ContextLattice packet",
      tokens: packedTokens,
      role: "packed",
    },
    {
      label: "risk adjustment",
      value: `${formatCompact(riskPenaltyTokens)} tokens`,
      detail: "reliability drag from the upstream sample",
      tokens: riskPenaltyTokens,
      role: "penalty",
    },
  ];
}

function firstInt(record: Record<string, any>, keys: string[]): number {
  for (const key of keys) {
    if (record[key] !== undefined && record[key] !== null) {
      return toInt(record[key]);
    }
  }
  return 0;
}

function roundTokenCount(value: unknown): number {
  return Math.max(0, Math.round(toNumber(value) / 100) * 100);
}

function roundRatio(value: unknown): number {
  const parsed = toNumber(value);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    return 0;
  }
  return Math.round(parsed * 10) / 10;
}

function normalizeConfidence(value: string, fallback: TokenImpactConfidence): TokenImpactConfidence {
  if (value === "high" || value === "medium" || value === "low") {
    return value;
  }
  return fallback;
}

function normalizeCalibrationGrade(value: string): TokenImpactCalibrationGrade {
  if (value === "measured" || value === "sampled_pack_estimate" || value === "heuristic") {
    return value;
  }
  return value.includes("sample") ? "sampled_pack_estimate" : "heuristic";
}

function normalizeFactorRole(value: string): TokenImpactFactorRole {
  if (value === "packed" || value === "penalty" || value === "baseline") {
    return value;
  }
  return "baseline";
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
