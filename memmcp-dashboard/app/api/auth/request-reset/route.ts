import crypto from "crypto";
import { prisma } from "@/lib/db";
import { sendEmail } from "@/lib/email";
import { recordAttempt, isRateLimited } from "@/lib/rateLimit";

export async function POST(request: Request) {
  const { email } = await request.json();
  const normalized = String(email || "").trim().toLowerCase();
  if (!normalized) {
    return Response.json({ ok: true, emailSent: true });
  }

  if (await isRateLimited(normalized, "reset")) {
    return Response.json({ ok: true });
  }

  const user = await prisma.user.findUnique({ where: { email: normalized } });
  await recordAttempt(normalized, "reset");
  if (!user) {
    return Response.json({ ok: true });
  }

  const token = crypto.randomBytes(32).toString("hex");
  const expires = new Date(Date.now() + 1000 * 60 * 60); // 1 hour

  await prisma.verificationToken.create({
    data: {
      identifier: normalized,
      token,
      expires,
    },
  });

  const appUrl = process.env.APP_URL || "http://localhost:3000";
  const link = `${appUrl}/auth/reset?token=${token}&email=${encodeURIComponent(
    normalized,
  )}`;

  await sendEmail({
    to: normalized,
    subject: "Reset your ContextLattice password",
    text: `Reset your password: ${link}`,
    html: `<p>Reset your password: <a href="${link}">${link}</a></p>`,
  });

  return Response.json({ ok: true });
}
