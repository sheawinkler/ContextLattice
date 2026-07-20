import type { ReactNode } from "react";
import "./globals.css";
import { DashboardShell } from "@/components/DashboardShell";
import { COMMERCIAL_TRUTH } from "@/lib/billing/commercial.generated";

export const metadata = {
  title: "ContextLattice",
  description: COMMERCIAL_TRUTH.product.canonical_description,
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body className="shell-body">
        <DashboardShell>{children}</DashboardShell>
      </body>
    </html>
  );
}
