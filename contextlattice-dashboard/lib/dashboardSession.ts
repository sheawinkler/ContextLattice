import type { Session } from "next-auth";
import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { dashboardAuthRequired } from "@/lib/authMode";

export async function getDashboardSession(): Promise<Session | null> {
  if (!dashboardAuthRequired()) {
    return null;
  }
  return getServerSession(authOptions);
}
