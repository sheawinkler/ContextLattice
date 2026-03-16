import "server-only";

const DEFAULT_BASE = "http://localhost:3000";

function resolveBaseUrl() {
  const envBase =
    process.env.APP_URL ||
    process.env.NEXTAUTH_URL ||
    process.env.DASHBOARD_URL ||
    process.env.NEXT_PUBLIC_APP_URL;

  return envBase ? envBase.replace(/\/+$/, "") : DEFAULT_BASE;
}

export function buildApiUrl(path: string) {
  if (path.startsWith("http://") || path.startsWith("https://")) {
    return path;
  }
  const normalizedPath = path.startsWith("/") ? path : `/${path}`;
  return `${resolveBaseUrl()}${normalizedPath}`;
}
