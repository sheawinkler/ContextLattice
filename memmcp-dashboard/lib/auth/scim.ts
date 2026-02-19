import crypto from "crypto";
import { prisma } from "@/lib/db";

const SCIM_PREFIX = "scim_";

export function generateScimToken() {
  const secret = crypto.randomBytes(32).toString("hex");
  const token = `${SCIM_PREFIX}${secret}`;
  const prefix = token.slice(0, 8);
  const tokenHash = hashScimToken(token);
  return { token, prefix, tokenHash };
}

export function hashScimToken(token: string) {
  return crypto.createHash("sha256").update(token).digest("hex");
}

export function extractScimToken(request: Request) {
  const auth = request.headers.get("authorization") || "";
  const [scheme, token] = auth.split(" ");
  if (scheme?.toLowerCase() === "bearer" && token) {
    return token.trim();
  }
  return null;
}

export async function authenticateScimToken(rawToken: string) {
  const tokenHash = hashScimToken(rawToken);
  const record = await prisma.scimToken.findFirst({
    where: { tokenHash, revokedAt: null },
  });
  if (!record) {
    return null;
  }
  await prisma.scimToken.update({
    where: { id: record.id },
    data: { lastUsedAt: new Date() },
  });
  return record;
}
