import { prisma } from "@/lib/db";

export async function recordPaymentIntent({
  userId,
  provider,
  status,
  planId,
  interval,
  amount,
  currency,
  reference,
  metadata,
}: {
  userId: string;
  provider: string;
  status: string;
  planId: string;
  interval: string;
  amount: number;
  currency: string;
  reference?: string;
  metadata?: string;
}) {
  return prisma.paymentIntent.create({
    data: {
      userId,
      provider,
      status,
      planId,
      interval,
      amount,
      currency,
      reference,
      metadata,
    },
  });
}

export async function updatePaymentIntentStatus(
  provider: string,
  reference: string,
  status: string,
) {
  await prisma.paymentIntent.updateMany({
    where: { provider, reference },
    data: { status },
  });
}
