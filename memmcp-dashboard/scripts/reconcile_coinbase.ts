import { prisma } from "@/lib/db";

const applyChanges = process.env.BILLING_RECONCILE_APPLY === "true";

async function reconcileCoinbase() {
  const apiKey = process.env.COINBASE_COMMERCE_API_KEY;
  if (!apiKey) {
    throw new Error("Missing COINBASE_COMMERCE_API_KEY");
  }

  const intents = await prisma.paymentIntent.findMany({
    where: { provider: "coinbase" },
    orderBy: { createdAt: "desc" },
    take: 300,
  });

  let updated = 0;
  for (const intent of intents) {
    if (!intent.reference) continue;
    const res = await fetch(
      `https://api.commerce.coinbase.com/charges/${intent.reference}`,
      {
        headers: {
          "Content-Type": "application/json",
          "X-CC-Api-Key": apiKey,
          "X-CC-Version": "2018-03-22",
        },
      },
    );
    const data = await res.json();
    if (!res.ok) {
      console.warn("[coinbase] failed to fetch charge", intent.reference, data?.error);
      continue;
    }
    const timeline = data?.data?.timeline || [];
    const last = timeline.length > 0 ? timeline[timeline.length - 1] : null;
    const nextStatus = (last?.status || intent.status).toLowerCase();

    if (nextStatus !== intent.status) {
      console.log(
        `[coinbase] intent ${intent.reference} ${intent.status} -> ${nextStatus}`,
      );
      if (applyChanges) {
        await prisma.paymentIntent.update({
          where: { id: intent.id },
          data: { status: nextStatus },
        });
      }
      updated += 1;
    }
  }

  console.log(`[coinbase] intents reconciled: ${updated} updated`);
}

reconcileCoinbase()
  .catch((err) => {
    console.error("[coinbase] reconcile error", err);
    process.exitCode = 1;
  })
  .finally(async () => {
    await prisma.$disconnect();
  });
