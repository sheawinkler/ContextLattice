import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";

export async function POST(request: Request) {
  const { reference, status } = await request.json();
  if (!reference) {
    return Response.json({ ok: false, error: "Missing reference" }, { status: 400 });
  }
  await updatePaymentIntentStatus("kraken", reference, status || "confirmed");
  return Response.json({ ok: true });
}
