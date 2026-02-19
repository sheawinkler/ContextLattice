import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";

export async function GET() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const cutoff = new Date(Date.now() - 30 * 24 * 60 * 60 * 1000);
  const intents = await prisma.paymentIntent.findMany({
    where: { userId: session.user.id, createdAt: { gte: cutoff } },
    select: { provider: true, status: true },
  });

  const counts: Record<string, Record<string, number>> = {};
  for (const intent of intents) {
    if (!counts[intent.provider]) {
      counts[intent.provider] = {};
    }
    counts[intent.provider][intent.status] =
      (counts[intent.provider][intent.status] || 0) + 1;
  }

  const failedWebhooks = await prisma.billingEvent.count({
    where: { status: "failed", createdAt: { gte: cutoff } },
  });

  return Response.json({
    ok: true,
    windowDays: 30,
    intents: counts,
    failedWebhooks,
  });
}
