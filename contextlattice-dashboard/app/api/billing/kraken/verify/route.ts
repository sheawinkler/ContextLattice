import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { updatePaymentIntentStatus } from "@/lib/billing/reconcile";

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }
  const role = String(session.user.workspaceRole || "").toLowerCase();
  if (role !== "owner" && role !== "admin") {
    return Response.json({ ok: false, error: "Forbidden" }, { status: 403 });
  }
  const { reference, status } = await request.json();
  if (!reference) {
    return Response.json({ ok: false, error: "Missing reference" }, { status: 400 });
  }
  await updatePaymentIntentStatus("kraken", reference, status || "confirmed");
  return Response.json({ ok: true });
}
