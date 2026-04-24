import { prisma } from "@/lib/db";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";
import { getPayPalAccessToken, getPayPalBaseUrl } from "@/lib/billing/paypal";

const applyChanges = process.env.BILLING_RECONCILE_APPLY === "true";

async function reconcilePayPal() {
  const token = await getPayPalAccessToken();
  const intents = await prisma.paymentIntent.findMany({
    where: { provider: "paypal" },
    orderBy: { createdAt: "desc" },
    take: 300,
  });

  let updated = 0;
  for (const intent of intents) {
    if (!intent.reference) continue;
    const res = await fetch(
      `${getPayPalBaseUrl()}/v2/checkout/orders/${intent.reference}`,
      {
        headers: { Authorization: `Bearer ${token}` },
      },
    );
    const data = await res.json();
    if (!res.ok) {
      console.warn("[paypal] failed to fetch order", intent.reference, data?.message);
      continue;
    }
    const status = String(data?.status || intent.status).toLowerCase();
    const nextStatus =
      status === "completed" ? "captured" : status === "approved" ? "approved" : status;

    if (nextStatus !== intent.status) {
      console.log(
        `[paypal] intent ${intent.reference} ${intent.status} -> ${nextStatus}`,
      );
      if (applyChanges) {
        await updatePaymentIntentStatus("paypal", intent.reference, nextStatus);
      }
      updated += 1;
    }
  }
  console.log(`[paypal] intents reconciled: ${updated} updated`);
}

reconcilePayPal()
  .catch((err) => {
    console.error("[paypal] reconcile error", err);
    process.exitCode = 1;
  })
  .finally(async () => {
    await prisma.$disconnect();
  });
