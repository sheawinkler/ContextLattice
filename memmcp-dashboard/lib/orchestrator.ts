const ORCHESTRATOR_URL =
  process.env.MEMMCP_ORCHESTRATOR_URL ?? "http://127.0.0.1:8075";
const ORCHESTRATOR_API_KEY = process.env.MEMMCP_ORCHESTRATOR_API_KEY ?? "";

export async function callOrchestrator(
  path: string,
  init?: RequestInit,
): Promise<any> {
  const target = `${ORCHESTRATOR_URL}${path}`;
  const headers = new Headers(init?.headers ?? {});
  if (!headers.has("content-type")) {
    headers.set("content-type", "application/json");
  }
  if (!headers.has("x-request-id")) {
    headers.set("x-request-id", crypto.randomUUID());
  }
  if (ORCHESTRATOR_API_KEY && !headers.has("x-api-key")) {
    headers.set("x-api-key", ORCHESTRATOR_API_KEY);
  }
  const res = await fetch(target, {
    ...init,
    headers,
    cache: "no-store",
  });
  if (!res.ok) {
    const detail = await res.text();
    throw new Error(`Orchestrator ${path} failed: ${res.status} ${detail}`);
  }
  return res.json();
}
