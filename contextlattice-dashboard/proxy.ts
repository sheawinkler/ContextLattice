import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";
import { getToken } from "next-auth/jwt";
import { sanitizeCallbackUrl } from "@/lib/callback-url";
import { authDisabledPayload } from "@/lib/authMode";

const PUBLIC_ROUTES = [
  "/auth/login",
  "/auth/register",
  "/pricing",
  "/downloads",
  "/legal",
  "/api/auth",
  "/api/public",
];

// These APIs authenticate their own bearer/admin credentials and must remain
// reachable without a browser session.
const ROUTE_AUTHENTICATED_API_ROUTES = [
  "/api/billing/admin/metrics",
  "/api/memory/write",
  "/api/scim",
  "/api/usage/events",
];

const HOSTED_PRIVATE_API_ROUTES = [
  "/api/billing",
  "/api/support",
  "/api/telemetry/pro-analytics",
  "/api/workspace",
];

export type ProxyRouteDecision = "allow" | "authenticate" | "auth-disabled";

function sanitizeRoutePath(rawPathname: string): string {
  if (!rawPathname.startsWith("/")) {
    return "";
  }

  const unsafeChars = /\r|\n|\u0000|\\\\/;
  if (unsafeChars.test(rawPathname)) {
    return "";
  }

  if (rawPathname.includes("..")) {
    return "";
  }

  let pathname = rawPathname;
  if (/%(2e|2f|5c)/i.test(pathname)) {
    return "";
  }

  try {
    const decoded = decodeURIComponent(pathname);
    if (decoded.includes("..") || decoded.includes("\\") || /\r|\n|\u0000/.test(decoded)) {
      return "";
    }
    if (/(%(2e|2f|5c))/i.test(decoded)) {
      return "";
    }
    pathname = decoded;
  } catch {
    return "";
  }

  return pathname;
}

function normalizePathForMatch(rawPathname: string): string {
  try {
    const parsed = new URL(rawPathname, "http://127.0.0.1");
    const canonical = sanitizeRoutePath(parsed.pathname);
    if (!canonical) {
      return "";
    }
    return canonical;
  } catch {
    return "";
  }
}

function matchesRoute(pathname: string, route: string): boolean {
  return pathname === route || pathname.startsWith(`${route}/`);
}

function matchesAnyRoute(pathname: string, routes: readonly string[]): boolean {
  return routes.some((route) => matchesRoute(pathname, route));
}

export function proxyRouteDecision(
  rawPathname: string,
  authRequired: boolean,
): ProxyRouteDecision {
  const pathname = normalizePathForMatch(rawPathname);
  if (!pathname) {
    if (authRequired) return "authenticate";
    return rawPathname.startsWith("/api") ? "auth-disabled" : "allow";
  }

  if (
    matchesAnyRoute(pathname, PUBLIC_ROUTES) ||
    matchesAnyRoute(pathname, ROUTE_AUTHENTICATED_API_ROUTES)
  ) {
    return "allow";
  }
  if (!authRequired) {
    return matchesAnyRoute(pathname, HOSTED_PRIVATE_API_ROUTES)
      ? "auth-disabled"
      : "allow";
  }
  return "authenticate";
}

function isApiRoute(rawPathname: string): boolean {
  const pathname = normalizePathForMatch(rawPathname);
  return pathname ? matchesRoute(pathname, "/api") : rawPathname.startsWith("/api");
}

export async function proxy(req: NextRequest) {
  const requestId = req.headers.get("x-request-id") || crypto.randomUUID();
  const requestHeaders = new Headers(req.headers);
  requestHeaders.set("x-request-id", requestId);

  const pathname = req.nextUrl.pathname;
  const decision = proxyRouteDecision(pathname, process.env.AUTH_REQUIRED === "true");
  if (decision === "allow") {
    const res = NextResponse.next({ request: { headers: requestHeaders } });
    res.headers.set("x-request-id", requestId);
    return res;
  }

  if (decision === "auth-disabled") {
    const res = NextResponse.json(authDisabledPayload(), { status: 404 });
    res.headers.set("cache-control", "no-store");
    res.headers.set("x-request-id", requestId);
    return res;
  }

  const token = await getToken({ req, secret: process.env.NEXTAUTH_SECRET });
  if (!token) {
    if (isApiRoute(pathname)) {
      const res = NextResponse.json(
        { ok: false, error: "Unauthorized" },
        { status: 401 },
      );
      res.headers.set("cache-control", "no-store");
      res.headers.set("x-request-id", requestId);
      return res;
    }
    const url = req.nextUrl.clone();
    const redirectTarget = `${req.nextUrl.pathname}${req.nextUrl.search || ""}`;
    url.pathname = "/auth/login";
    url.searchParams.set("callbackUrl", sanitizeCallbackUrl(redirectTarget));
    const res = NextResponse.redirect(url);
    res.headers.set("x-request-id", requestId);
    return res;
  }

  const res = NextResponse.next({ request: { headers: requestHeaders } });
  res.headers.set("x-request-id", requestId);
  return res;
}

export const config = {
  matcher: ["/((?!_next|favicon.ico).*)"],
};
