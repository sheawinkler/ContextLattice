import type { ReactNode } from "react";
import "./globals.css";
import { AuthProvider } from "@/components/SessionProvider";
import { ShellNav } from "@/components/ShellNav";

export const metadata = {
  title: "ContextLattice Console",
  description: "Operator console for the memory and context stack",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body className="shell-body">
        <AuthProvider>
          <header className="shell-header">
            <div className="shell-header-row">
              <div className="shell-title-wrap">
                <h1 className="shell-title">ContextLattice Console</h1>
                <p className="shell-subtitle">
                  Live window into the memory bank, orchestrator, and MCP stack
                </p>
              </div>
              <ShellNav />
            </div>
          </header>
          <main className="shell-main">{children}</main>
        </AuthProvider>
      </body>
    </html>
  );
}
