import type { ReactNode } from "react";
import "./globals.css";
import { AuthProvider } from "@/components/SessionProvider";
import { ShellNav } from "@/components/ShellNav";
import { dashboardAuthRequired } from "@/lib/authMode";

export const metadata = {
  title: "ContextLattice",
  description: "Local-first memory infrastructure for agent systems",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  const authEnabled = dashboardAuthRequired();

  return (
    <html lang="en">
      <body className="shell-body">
        <AuthProvider enabled={authEnabled}>
          <header className="shell-header">
            <div className="shell-header-row">
              <div className="shell-title-wrap">
                <h1 className="shell-title">ContextLattice</h1>
                <p className="shell-subtitle">
                  Durable memory, context allocation, and runtime proof for agent systems.
                </p>
              </div>
              <ShellNav authEnabled={authEnabled} />
            </div>
          </header>
          <main className="shell-main">{children}</main>
        </AuthProvider>
      </body>
    </html>
  );
}
