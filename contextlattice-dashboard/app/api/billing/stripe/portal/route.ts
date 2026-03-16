import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { getStripeClient } from "@/lib/billing/stripe";

export async function POST() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const customer = await prisma.billingCustomer.findFirst({
    where: { userId: session.user.id, provider: "stripe" },
  });
  if (!customer) {
    return Response.json(
      { ok: false, error: "No Stripe customer found." },
      { status: 404 },
    );
  }

  const stripe = getStripeClient();
  const appUrl = process.env.APP_URL || "http://localhost:3000";
  const portal = await stripe.billingPortal.sessions.create({
    customer: customer.customerId,
    return_url: `${appUrl}/billing`,
  });

  return Response.json({ ok: true, url: portal.url });
}
