import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";
import { fetchWithRetry } from "@/lib/http/retry";

function getPayPalBaseUrl() {
  return process.env.PAYPAL_ENV === "live"
    ? "https://api-m.paypal.com"
    : "https://api-m.sandbox.paypal.com";
}

async function getAccessToken() {
  const clientId = process.env.PAYPAL_CLIENT_ID;
  const clientSecret = process.env.PAYPAL_CLIENT_SECRET;
  if (!clientId || !clientSecret) {
    throw new Error("PayPal credentials missing");
  }
  const creds = Buffer.from(`${clientId}:${clientSecret}`).toString("base64");
  const res = await fetchWithRetry(`${getPayPalBaseUrl()}/v1/oauth2/token`, {
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

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const session = await getServerSession(authOptions);
  if (!session?.user?.email || !session.user.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const { orderId } = await request.json();
  if (!orderId) {
    return Response.json({ ok: false, error: "orderId required" }, { status: 400 });
  }

  const token = await getAccessToken();
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
