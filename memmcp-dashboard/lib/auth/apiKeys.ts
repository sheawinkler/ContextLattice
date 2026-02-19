import crypto from "crypto";
import { prisma } from "@/lib/db";

const API_KEY_PREFIX = "ctx_";

export function generateApiKey() {
  const secret = crypto.randomBytes(32).toString("hex");
  const apiKey = `${API_KEY_PREFIX}${secret}`;
  const prefix = apiKey.slice(0, 8);
  const keyHash = hashApiKey(apiKey);
  return { apiKey, prefix, keyHash };
}

export function hashApiKey(key: string) {
  return crypto.createHash("sha256").update(key).digest("hex");
}

export function parseScopes(scopes?: string | null) {
  if (!scopes) {
    return [];
  }
  return scopes
    .split(",")
    .map((scope) => scope.trim())
    .filter(Boolean);
}

export function hasScope(scopes: string | null | undefined, required: string) {
  const parsed = parseScopes(scopes);
  return parsed.includes(required);
}

export function extractApiKey(request: Request) {
  const headerKey = request.headers.get("x-api-key");
  if (headerKey) {
    return headerKey.trim();
  }
  const auth = request.headers.get("authorization") || "";
  const [scheme, token] = auth.split(" ");
  if (scheme?.toLowerCase() === "bearer" && token) {
    return token.trim();
  }
  return null;
}

export async function authenticateApiKey(rawKey: string) {
  const keyHash = hashApiKey(rawKey);
  const record = await prisma.apiKey.findFirst({
    where: { keyHash, revokedAt: null },
  });
  if (!record) {
    return null;
  }
  await prisma.apiKey.update({
    where: { id: record.id },
    data: { lastUsedAt: new Date() },
  });
  return record;
}
