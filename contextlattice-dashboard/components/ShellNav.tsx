"use client";

import { signOut, useSession } from "next-auth/react";

const CORE_NAV_LINKS = [
  { href: "/console", label: "Console" },
  { href: "/overview", label: "Overview" },
  { href: "/mindmap", label: "Topics" },
  { href: "/status", label: "Status" },
  { href: "/downloads", label: "Install" },
  { href: "/settings", label: "Settings" },
];

const HOSTED_NAV_LINKS = [
  { href: "/pricing", label: "Pricing" },
  { href: "/billing", label: "Billing" },
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

function AuthControls() {
  const { data: session, status } = useSession();
  const isSignedIn = status === "authenticated" && !!session?.user;

  if (isSignedIn) {
    return (
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
    );
  }

  return (
    <>
      <a className="shell-nav-link" href="/auth/register">
        Create account
      </a>
      <a className="shell-nav-link shell-nav-link-primary" href="/auth/login">
        Sign in
      </a>
    </>
  );
}

export function ShellNav({ authEnabled }: { authEnabled: boolean }) {
  const navLinks = authEnabled ? [...CORE_NAV_LINKS, ...HOSTED_NAV_LINKS] : CORE_NAV_LINKS;

  return (
    <nav className="shell-nav" aria-label="Primary">
      {navLinks.map((link) => (
        <a key={link.href} className="shell-nav-link" href={link.href}>
          {link.label}
        </a>
      ))}

      {authEnabled ? <AuthControls /> : null}
    </nav>
  );
}
