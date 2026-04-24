import crypto from "crypto";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";
import { recordBillingEvent } from "@/lib/billing/events";
import { verifyPayPalWebhookSignature } from "@/lib/billing/paypal";

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const body = await request.text();

  let payload: any;
  try {
    payload = await verifyPayPalWebhookSignature({
      headers: request.headers,
      rawBody: body,
    });
  } catch (err: any) {
    return Response.json(
      { ok: false, error: err?.message || "PayPal webhook verification failed" },
      { status: 400 },
    );
  }

  const eventType = payload?.event_type || "";
  const normalizedEventType = String(eventType).toUpperCase();
  const eventId = payload?.id || "paypal-unknown";
  const resource = payload?.resource || {};
  const related = resource?.supplementary_data?.related_ids || {};
  const references = Array.from(
    new Set(
      [resource?.id, related?.order_id, related?.capture_id]
        .map((value) => String(value || "").trim())
        .filter(Boolean),
    ),
  );

  await recordBillingEvent({
    provider: "paypal",
    eventId,
    eventType,
    payload: body,
    status: "received",
    requestId,
  });

  try {
    let status = "pending";
    if (normalizedEventType.includes("COMPLETED")) {
      status = "captured";
    } else if (
      normalizedEventType.includes("DENIED") ||
      normalizedEventType.includes("FAILED") ||
      normalizedEventType.includes("REFUNDED") ||
      normalizedEventType.includes("REVERSED") ||
      normalizedEventType.includes("CANCEL")
    ) {
      status = "canceled";
    }
    for (const reference of references) {
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
