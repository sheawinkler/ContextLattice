export type DashboardRuntimeMode = "local" | "hosted";

export type EndpointTarget = {
  path: string;
  kind: "page" | "api";
  note: string;
  allowedStatuses: number[];
  runtimeModes?: DashboardRuntimeMode[];
};

export const ENDPOINT_CATALOG: EndpointTarget[] = [
  { path: "/", kind: "page", note: "Legacy root alias to /console", allowedStatuses: [200, 307] },
  { path: "/console", kind: "page", note: "Live operator proof cockpit", allowedStatuses: [200] },
  { path: "/overview", kind: "page", note: "Product and memory-system overview", allowedStatuses: [200] },
  { path: "/mindmap", kind: "page", note: "Project and topic hierarchy", allowedStatuses: [200] },
  { path: "/status", kind: "page", note: "Runtime health, queues, storage, and route checks", allowedStatuses: [200] },
  { path: "/downloads", kind: "page", note: "Local install and agent integration", allowedStatuses: [200] },
  { path: "/setup", kind: "page", note: "First-run checks and sample write flow", allowedStatuses: [200] },
  { path: "/pricing", kind: "page", note: "Plan matrix and paid feature lanes", allowedStatuses: [200] },
  { path: "/settings", kind: "page", note: "Workspace controls and API key management", allowedStatuses: [200] },
  { path: "/auth/login", kind: "page", note: "User authentication", allowedStatuses: [200] },
  { path: "/auth/register", kind: "page", note: "Account registration", allowedStatuses: [200] },
  { path: "/auth/request-reset", kind: "page", note: "Password reset request", allowedStatuses: [200] },
  { path: "/auth/reset", kind: "page", note: "Password reset completion", allowedStatuses: [200] },
  { path: "/legal/terms", kind: "page", note: "Terms of service", allowedStatuses: [200] },
  { path: "/legal/privacy", kind: "page", note: "Privacy policy", allowedStatuses: [200] },
  { path: "/legal/refunds", kind: "page", note: "Refund policy", allowedStatuses: [200] },

  { path: "/api/telemetry/overview", kind: "api", note: "Top-level telemetry aggregate", allowedStatuses: [200] },
  { path: "/api/health", kind: "api", note: "Dashboard runtime and orchestrator health probe", allowedStatuses: [200] },
  { path: "/api/memory/status", kind: "api", note: "Orchestrator service status proxy", allowedStatuses: [200] },
  { path: "/api/memory/topics", kind: "api", note: "Topic and rollup tree summary", allowedStatuses: [200] },
  { path: "/api/memory/preferences", kind: "api", note: "Preference learning status", allowedStatuses: [200] },
  {
    path: "/api/workspace/current",
    kind: "api",
    note: "Workspace session view (auth-gated)",
    allowedStatuses: [200, 401],
    runtimeModes: ["hosted"],
  },
];

export const PAGE_ENDPOINTS = ENDPOINT_CATALOG.filter((item) => item.kind === "page");
export const API_ENDPOINTS = ENDPOINT_CATALOG.filter((item) => item.kind === "api");

export function endpointRunsInMode(target: EndpointTarget, mode: DashboardRuntimeMode): boolean {
  return !target.runtimeModes || target.runtimeModes.includes(mode);
}

export function endpointTargetsForMode(mode: DashboardRuntimeMode): EndpointTarget[] {
  return ENDPOINT_CATALOG.filter((target) => endpointRunsInMode(target, mode));
}
