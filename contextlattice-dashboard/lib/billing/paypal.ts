import { fetchWithRetry } from "@/lib/http/retry";

export function getPayPalBaseUrl() {
  return process.env.PAYPAL_ENV === "live"
    ? "https://api-m.paypal.com"
    : "https://api-m.sandbox.paypal.com";
}

export async function getPayPalAccessToken() {
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

function headerLookup(
  headers:
    | Headers
    | {
        get(name: string): string | null;
      },
  key: string,
) {
  return headers.get(key) || headers.get(key.toLowerCase()) || "";
}

export type PayPalWebhookVerificationInput = {
  headers: Headers;
  rawBody: string;
};

export async function verifyPayPalWebhookSignature({
  headers,
  rawBody,
}: PayPalWebhookVerificationInput) {
  const webhookId = process.env.PAYPAL_WEBHOOK_ID;
  if (!webhookId) {
    throw new Error("Missing PAYPAL_WEBHOOK_ID");
  }

  const transmissionId = headerLookup(headers, "paypal-transmission-id");
  const transmissionTime = headerLookup(headers, "paypal-transmission-time");
  const transmissionSig = headerLookup(headers, "paypal-transmission-sig");
  const certUrl = headerLookup(headers, "paypal-cert-url");
  const authAlgo = headerLookup(headers, "paypal-auth-algo");

  if (!transmissionId || !transmissionTime || !transmissionSig || !certUrl || !authAlgo) {
    throw new Error("Missing required PayPal webhook headers");
  }

  const event = JSON.parse(rawBody);
  const token = await getPayPalAccessToken();

  const verifyRes = await fetchWithRetry(
    `${getPayPalBaseUrl()}/v1/notifications/verify-webhook-signature`,
    {
      method: "POST",
      headers: {
        Authorization: `Bearer ${token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        auth_algo: authAlgo,
        cert_url: certUrl,
        transmission_id: transmissionId,
        transmission_sig: transmissionSig,
        transmission_time: transmissionTime,
        webhook_id: webhookId,
        webhook_event: event,
      }),
    },
  );

  const verifyPayload = await verifyRes.json();
  if (!verifyRes.ok) {
    throw new Error(
      verifyPayload?.message ||
        verifyPayload?.name ||
        "PayPal webhook signature verification request failed",
    );
  }

  if (verifyPayload?.verification_status !== "SUCCESS") {
    throw new Error("PayPal webhook signature verification failed");
  }

  return event;
}
