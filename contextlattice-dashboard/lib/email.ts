import nodemailer from "nodemailer";

export async function sendEmail({
  to,
  subject,
  text,
  html,
}: {
  to: string;
  subject: string;
  text: string;
  html?: string;
}) {
  const smtp = process.env.SMTP_CONNECTION_URL;
  const from = process.env.EMAIL_FROM_ADDRESS || "no-reply@contextlattice.io";

  if (!smtp) {
    return { ok: false, error: "SMTP not configured" };
  }

  const transporter = nodemailer.createTransport(smtp);
  await transporter.sendMail({
    from,
    to,
    subject,
    text,
    html,
  });

  return { ok: true };
}
