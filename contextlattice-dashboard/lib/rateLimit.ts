import { prisma } from "@/lib/db";

function positiveInteger(value: string | undefined, fallback: number): number {
  const parsed = Number.parseInt(value || "", 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

const WINDOW_SECONDS = positiveInteger(
  process.env.AUTH_RATE_LIMIT_WINDOW_SECONDS,
  600,
);
const MAX_ATTEMPTS = positiveInteger(
  process.env.AUTH_RATE_LIMIT_MAX_ATTEMPTS,
  10,
);

function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}

export async function isRateLimited(email: string, action: string) {
  const since = new Date(Date.now() - WINDOW_SECONDS * 1000);
  const count = await prisma.authAttempt.count({
    where: {
      email: normalizeEmail(email),
      action,
      createdAt: { gte: since },
    },
  });
  return count >= MAX_ATTEMPTS;
}

export async function recordAttempt(
  email: string,
  action: string,
  ip?: string,
) {
  await prisma.authAttempt.create({
    data: { email: normalizeEmail(email), action, ip },
  });
}
