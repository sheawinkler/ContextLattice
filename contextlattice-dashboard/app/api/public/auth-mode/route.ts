import { dashboardAuthRequired } from "@/lib/authMode";

export async function GET() {
  return Response.json(
    { ok: true, authRequired: dashboardAuthRequired() },
    { headers: { "cache-control": "no-store" } },
  );
}
