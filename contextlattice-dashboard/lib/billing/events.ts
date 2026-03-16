import { prisma } from "@/lib/db";

type BillingEventInput = {
  provider: string;
  eventId: string;
  eventType: string;
  payload: string;
  status?: "received" | "processed" | "failed";
  error?: string | null;
  requestId?: string | null;
};

export async function recordBillingEvent(input: BillingEventInput) {
  return prisma.billingEvent.upsert({
    where: { provider_eventId: { provider: input.provider, eventId: input.eventId } },
    update: {
      eventType: input.eventType,
      payload: input.payload,
      status: input.status ?? "received",
      error: input.error ?? null,
      requestId: input.requestId ?? null,
      processedAt:
        input.status && input.status !== "received" ? new Date() : undefined,
    },
    create: {
      provider: input.provider,
      eventId: input.eventId,
      eventType: input.eventType,
      payload: input.payload,
      status: input.status ?? "received",
      error: input.error ?? null,
      requestId: input.requestId ?? null,
      processedAt:
        input.status && input.status !== "received" ? new Date() : null,
    },
  });
}

export async function markBillingEventProcessed(
  provider: string,
  eventId: string,
) {
  return prisma.billingEvent.update({
    where: { provider_eventId: { provider, eventId } },
    data: { status: "processed", processedAt: new Date(), error: null },
  });
}
