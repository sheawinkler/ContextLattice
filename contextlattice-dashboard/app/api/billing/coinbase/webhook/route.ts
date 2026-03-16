import crypto from "crypto";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";
import { recordBillingEvent } from "@/lib/billing/events";

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const secret = process.env.COINBASE_COMMERCE_WEBHOOK_SECRET;
  if (!secret) {
    return Response.json({ ok: false, error: "Missing webhook secret" }, { status: 500 });
  }

  const body = await request.text();
  const signature = request.headers.get("x-cc-webhook-signature") || "";
  const expected = crypto
    .createHmac("sha256", secret)
    .update(body, "utf8")
    .digest("hex");

  if (signature !== expected) {
    return Response.json({ ok: false, error: "Invalid signature" }, { status: 400 });
  }

  const payload = JSON.parse(body);
  const eventType = payload?.event?.type || "";
  const eventId = payload?.event?.id || "coinbase-unknown";
  const charge = payload?.event?.data || {};
  const reference = charge?.id;

  await recordBillingEvent({
    provider: "coinbase",
    eventId,
    eventType,
    payload: body,
    status: "received",
    requestId,
  });

  try {
    if (reference) {
      const status = eventType.includes("confirmed") ? "confirmed" : "pending";
      await updatePaymentIntentStatus("coinbase", reference, status);
    }
    await recordBillingEvent({
      provider: "coinbase",
      eventId,
      eventType,
      payload: body,
      status: "processed",
      requestId,
    });
  } catch (err: any) {
    await recordBillingEvent({
      provider: "coinbase",
      eventId,
      eventType,
      payload: body,
      status: "failed",
      error: err?.message || "coinbase webhook error",
      requestId,
    });
    return Response.json({ ok: false, error: "Webhook processing failed" }, { status: 500 });
  }

  return Response.json({ ok: true });
}
