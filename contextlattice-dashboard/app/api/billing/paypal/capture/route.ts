import crypto from "crypto";
import { getDashboardSession } from "@/lib/dashboardSession";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";
import { fetchWithRetry } from "@/lib/http/retry";
import { getPayPalAccessToken, getPayPalBaseUrl } from "@/lib/billing/paypal";

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const session = await getDashboardSession();
  if (!session?.user?.email || !session.user.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const { orderId } = await request.json();
  if (!orderId) {
    return Response.json({ ok: false, error: "orderId required" }, { status: 400 });
  }

  const token = await getPayPalAccessToken();
  const res = await fetchWithRetry(
    `${getPayPalBaseUrl()}/v2/checkout/orders/${orderId}/capture`,
    {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
      "PayPal-Request-Id": requestId,
    },
    },
  );
  const data = await res.json();
  if (!res.ok) {
    return Response.json({ ok: false, error: data?.message || "PayPal capture failed" }, { status: 400 });
  }

  await updatePaymentIntentStatus("paypal", orderId, "captured");

  return Response.json({ ok: true, data });
}
