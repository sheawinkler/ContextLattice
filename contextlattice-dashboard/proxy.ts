import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";
import { getToken } from "next-auth/jwt";
import { sanitizeCallbackUrl } from "@/lib/callback-url";

const PUBLIC_ROUTES = [
  "/auth/login",
  "/auth/register",
  "/pricing",
  "/api/auth",
  "/api/public",
  "/api/billing/stripe/webhook",
  "/api/billing/paypal/webhook",
  "/api/billing/coinbase/webhook",
];

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

function isPublicRoute(rawPathname: string): boolean {
  const pathname = normalizePathForMatch(rawPathname);
  if (!pathname) {
    return false;
  }

  return PUBLIC_ROUTES.some((route) => pathname === route || pathname.startsWith(`${route}/`));
}

export async function proxy(req: NextRequest) {
  const requestId = req.headers.get("x-request-id") || crypto.randomUUID();
  const requestHeaders = new Headers(req.headers);
  requestHeaders.set("x-request-id", requestId);

  if (process.env.AUTH_REQUIRED !== "true") {
    const res = NextResponse.next({ request: { headers: requestHeaders } });
    res.headers.set("x-request-id", requestId);
    return res;
  }

  const pathname = req.nextUrl.pathname;
  if (isPublicRoute(pathname)) {
    const res = NextResponse.next({ request: { headers: requestHeaders } });
    res.headers.set("x-request-id", requestId);
    return res;
  }

  const token = await getToken({ req, secret: process.env.NEXTAUTH_SECRET });
  if (!token) {
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
