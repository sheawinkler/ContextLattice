"use client";

import { signOut, useSession } from "next-auth/react";

const NAV_LINKS = [
  { href: "/console", label: "Console" },
  { href: "/overview", label: "Overview" },
  { href: "/mindmap", label: "Mindmap" },
  { href: "/status", label: "Status" },
  { href: "/setup", label: "Setup" },
  { href: "/pricing", label: "Pricing" },
  { href: "/billing", label: "Billing" },
  { href: "/downloads", label: "Downloads" },
  { href: "/settings", label: "Settings" },
];

function displayName(session: any): string {
  const name = session?.user?.name?.trim?.();
  if (name) {
    return name;
  }
  const email = session?.user?.email?.trim?.();
  if (email) {
    return email.split("@")[0] || email;
  }
  return "Account";
}

export function ShellNav() {
  const { data: session, status } = useSession();
  const isSignedIn = status === "authenticated" && !!session?.user;

  return (
    <nav className="shell-nav" aria-label="Primary">
      {NAV_LINKS.map((link) => (
        <a key={link.href} className="shell-nav-link" href={link.href}>
          {link.label}
        </a>
      ))}

      {isSignedIn ? (
        <>
          <span className="shell-account-chip" title={session?.user?.email || undefined}>
            {displayName(session)}
          </span>
          <button
            className="shell-nav-link shell-nav-link-action"
            type="button"
            onClick={() => signOut({ callbackUrl: "/auth/login" })}
          >
            Sign out
          </button>
        </>
      ) : (
        <>
          <a className="shell-nav-link" href="/auth/register">
            Create account
          </a>
          <a className="shell-nav-link shell-nav-link-primary" href="/auth/login">
            Sign in
          </a>
        </>
      )}
    </nav>
  );
}

