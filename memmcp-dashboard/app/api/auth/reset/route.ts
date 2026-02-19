import { hash } from "bcryptjs";
import { prisma } from "@/lib/db";
import { recordAttempt, isRateLimited } from "@/lib/rateLimit";

export async function POST(request: Request) {
  const { email, token, password } = await request.json();
  const normalized = String(email || "").trim().toLowerCase();
  const resetToken = String(token || "");
  const newPassword = String(password || "");

  if (!normalized || !resetToken || newPassword.length < 8) {
    return Response.json({ ok: false, error: "Invalid reset payload" }, { status: 400 });
  }

  if (await isRateLimited(normalized, "reset")) {
    return Response.json({ ok: false, error: "Too many attempts" }, { status: 429 });
  }

  const record = await prisma.verificationToken.findUnique({
    where: { token: resetToken },
  });

  await recordAttempt(normalized, "reset");

  if (!record || record.identifier !== normalized || record.expires < new Date()) {
    return Response.json({ ok: false, error: "Invalid or expired token" }, { status: 400 });
  }

  const passwordHash = await hash(newPassword, 12);
  await prisma.user.update({
    where: { email: normalized },
    data: { passwordHash },
  });
  await prisma.verificationToken.delete({ where: { token: resetToken } });

  return Response.json({ ok: true });
}
