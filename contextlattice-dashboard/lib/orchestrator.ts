const ORCHESTRATOR_URL =
  process.env.CONTEXTLATTICE_ORCHESTRATOR_URL ??
  process.env.MEMMCP_ORCHESTRATOR_URL ??
  "http://127.0.0.1:8075";
const ORCHESTRATOR_API_KEY =
  process.env.CONTEXTLATTICE_ORCHESTRATOR_API_KEY ??
  process.env.MEMMCP_ORCHESTRATOR_API_KEY ??
  "";

export function orchestratorUrl(path: string): string {
  return `${ORCHESTRATOR_URL}${path}`;
}

export function buildOrchestratorHeaders(
  initialHeaders?: HeadersInit,
): Headers {
  const headers = new Headers(initialHeaders ?? {});
  if (!headers.has("content-type")) {
    headers.set("content-type", "application/json");
  }
  if (!headers.has("x-request-id")) {
    headers.set("x-request-id", crypto.randomUUID());
  }
  if (ORCHESTRATOR_API_KEY && !headers.has("x-api-key")) {
    headers.set("x-api-key", ORCHESTRATOR_API_KEY);
  }
  return headers;
}

export async function fetchOrchestrator(
  path: string,
  init?: RequestInit,
): Promise<Response> {
  return fetch(orchestratorUrl(path), {
    ...init,
    headers: buildOrchestratorHeaders(init?.headers),
    cache: "no-store",
  });
}

export async function callOrchestrator(
  path: string,
  init?: RequestInit,
): Promise<any> {
  const res = await fetchOrchestrator(path, init);
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Orchestrator ${path} failed: ${res.status} ${detail}`);
  }
  return res.json();
}
