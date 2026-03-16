import { prisma } from "@/lib/db";

const WINDOW_SECONDS = parseInt(
  process.env.AUTH_RATE_LIMIT_WINDOW_SECONDS || "600",
  10,
);
const MAX_ATTEMPTS = parseInt(
  process.env.AUTH_RATE_LIMIT_MAX_ATTEMPTS || "10",
  10,
);

export async function isRateLimited(email: string, action: string) {
  const since = new Date(Date.now() - WINDOW_SECONDS * 1000);
  const count = await prisma.authAttempt.count({
    where: {
      email,
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
    data: { email, action, ip },
  });
}
