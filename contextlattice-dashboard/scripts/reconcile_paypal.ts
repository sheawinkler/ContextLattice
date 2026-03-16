import { prisma } from "@/lib/db";

const applyChanges = process.env.BILLING_RECONCILE_APPLY === "true";

function getPayPalBaseUrl() {
  return process.env.PAYPAL_ENV === "live"
    ? "https://api-m.paypal.com"
    : "https://api-m.sandbox.paypal.com";
}

async function getAccessToken() {
  const clientId = process.env.PAYPAL_CLIENT_ID;
  const clientSecret = process.env.PAYPAL_CLIENT_SECRET;
  if (!clientId || !clientSecret) {
    throw new Error("Missing PAYPAL_CLIENT_ID or PAYPAL_CLIENT_SECRET");
  }
  const creds = Buffer.from(`${clientId}:${clientSecret}`).toString("base64");
  const res = await fetch(`${getPayPalBaseUrl()}/v1/oauth2/token`, {
    method: "POST",
    headers: {
      Authorization: `Basic ${creds}`,
      "Content-Type": "application/x-www-form-urlencoded",
    },
    body: "grant_type=client_credentials",
  });
  const data = await res.json();
  if (!res.ok) {
    throw new Error(data?.error_description || "PayPal auth failed");
  }
  return data.access_token as string;
}

async function reconcilePayPal() {
  const token = await getAccessToken();
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
        await prisma.paymentIntent.update({
          where: { id: intent.id },
          data: { status: nextStatus },
        });
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
