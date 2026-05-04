import type { ReactNode } from "react";
import "./globals.css";
import { AuthProvider } from "@/components/SessionProvider";

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
              <nav className="shell-nav">
                <a className="shell-nav-link" href="/">
                  Console
                </a>
                <a className="shell-nav-link" href="/mindmap">
                  Mindmap
                </a>
                <a className="shell-nav-link" href="/status">
                  Status
                </a>
                <a className="shell-nav-link" href="/setup">
                  Setup
                </a>
                <a className="shell-nav-link" href="/pricing">
                  Pricing
                </a>
                <a className="shell-nav-link" href="/billing">
                  Billing
                </a>
                <a className="shell-nav-link" href="/downloads">
                  Downloads
                </a>
                <a className="shell-nav-link" href="/settings">
                  Settings
                </a>
                <a className="shell-nav-link" href="/auth/login">
                  Sign in
                </a>
              </nav>
            </div>
          </header>
          <main className="shell-main">{children}</main>
        </AuthProvider>
      </body>
    </html>
  );
}
