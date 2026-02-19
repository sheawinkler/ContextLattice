import { prisma } from "@/lib/db";

const lookbackHours = Number(process.env.BILLING_RECONCILE_LOOKBACK_HOURS || "24");
const markStale = process.env.BILLING_RECONCILE_MARK_STALE === "true";

async function main() {
  const cutoff = new Date(Date.now() - lookbackHours * 60 * 60 * 1000);
  const pending = await prisma.paymentIntent.findMany({
    where: {
      status: { in: ["created", "pending", "pending_manual"] },
      createdAt: { lt: cutoff },
    },
    orderBy: { createdAt: "asc" },
  });

  if (pending.length === 0) {
    console.log("[reconcile] no stale payment intents found");
    return;
  }

  console.log(`[reconcile] found ${pending.length} stale intents older than ${lookbackHours}h`);
  for (const intent of pending) {
    console.log(
      `- ${intent.provider} ${intent.reference || "n/a"} status=${intent.status} created=${intent.createdAt.toISOString()}`,
    );
  }

  if (markStale) {
    const ids = pending.map((intent) => intent.id);
    await prisma.paymentIntent.updateMany({
      where: { id: { in: ids } },
      data: { status: "stale" },
    });
    console.log(`[reconcile] marked ${ids.length} intents as stale`);
  }
}

main()
  .catch((err) => {
    console.error("[reconcile] error", err);
    process.exitCode = 1;
  })
  .finally(async () => {
    await prisma.$disconnect();
  });
