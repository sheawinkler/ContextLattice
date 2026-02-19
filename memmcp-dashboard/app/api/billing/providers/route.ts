import { billingProviders } from "@/lib/billing/providers";

export async function GET() {
  return Response.json({ ok: true, providers: billingProviders });
}
