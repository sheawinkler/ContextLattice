import NextAuth from "next-auth";
import { authOptions } from "@/lib/auth";
import { authDisabledResponse, dashboardAuthRequired } from "@/lib/authMode";

const handler = dashboardAuthRequired() ? NextAuth(authOptions) : null;

export async function GET(request: Request, context: any) {
  if (!handler) {
    return authDisabledResponse();
  }
  return handler(request, context);
}

export async function POST(request: Request, context: any) {
  if (!handler) {
    return authDisabledResponse();
  }
  return handler(request, context);
}
