import crypto from "crypto";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";
import { recordBillingEvent } from "@/lib/billing/events";

// Note: Proper PayPal webhook verification requires API calls to validate signatures.
// This placeholder validates against a shared secret for now.
export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const secret = process.env.PAYPAL_WEBHOOK_SECRET;
  if (!secret) {
    return Response.json({ ok: false, error: "Missing webhook secret" }, { status: 500 });
  }

  const body = await request.text();
  const signature = request.headers.get("paypal-signature") || "";
  const expected = crypto
    .createHmac("sha256", secret)
    .update(body, "utf8")
    .digest("hex");

  if (signature !== expected) {
    return Response.json({ ok: false, error: "Invalid signature" }, { status: 400 });
  }

  const payload = JSON.parse(body);
  const eventType = payload?.event_type || "";
  const eventId = payload?.id || "paypal-unknown";
  const resource = payload?.resource || {};
  const reference = resource?.id;

  await recordBillingEvent({
    provider: "paypal",
    eventId,
    eventType,
    payload: body,
    status: "received",
    requestId,
  });

  try {
    if (reference) {
      const status = eventType.includes("COMPLETED") ? "captured" : "pending";
      await updatePaymentIntentStatus("paypal", reference, status);
    }
    await recordBillingEvent({
      provider: "paypal",
      eventId,
      eventType,
      payload: body,
      status: "processed",
      requestId,
    });
  } catch (err: any) {
    await recordBillingEvent({
      provider: "paypal",
      eventId,
      eventType,
      payload: body,
      status: "failed",
      error: err?.message || "paypal webhook error",
      requestId,
    });
    return Response.json({ ok: false, error: "Webhook processing failed" }, { status: 500 });
  }

  return Response.json({ ok: true });
}
