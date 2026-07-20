"use client";

import { signOut, useSession } from "next-auth/react";

const PRIVATE_NAV_LINKS = [
  { href: "/console", label: "Console" },
  { href: "/overview", label: "Overview" },
  { href: "/mindmap", label: "Topics" },
  { href: "/status", label: "Status" },
  { href: "/settings", label: "Settings" },
];

const PUBLIC_NAV_LINKS = [
  { href: "/pricing", label: "Pricing" },
  { href: "/downloads", label: "Install" },
  { href: "/legal/terms", label: "Legal" },
];

const BILLING_NAV_LINK = { href: "/billing", label: "Billing" };

export function shellNavLinks(authEnabled: boolean, isSignedIn: boolean) {
  if (!authEnabled) return [...PRIVATE_NAV_LINKS, ...PUBLIC_NAV_LINKS];
  if (!isSignedIn) return [...PUBLIC_NAV_LINKS];
  return [...PRIVATE_NAV_LINKS, ...PUBLIC_NAV_LINKS, BILLING_NAV_LINK];
}

function NavLinks({ links }: { links: Array<{ href: string; label: string }> }) {
  return links.map((link) => (
    <a key={link.href} className="shell-nav-link" href={link.href}>
      {link.label}
    </a>
  ));
}

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

function HostedNavigation() {
  const { data: session, status } = useSession();
  const isSignedIn = status === "authenticated" && !!session?.user;

  if (isSignedIn) {
    return (
      <>
        <NavLinks links={shellNavLinks(true, true)} />
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
      <NavLinks links={shellNavLinks(true, false)} />
      <a className="shell-nav-link" href="/auth/register">
        Create account
      </a>
      <a className="shell-nav-link shell-nav-link-primary" href="/auth/login">
        Sign in
      </a>
    </>
  );
}

export function ShellNav({
  authEnabled,
  authResolved = true,
}: {
  authEnabled: boolean;
  authResolved?: boolean;
}) {
  return (
    <nav className="shell-nav" aria-label="Primary">
      {!authResolved ? (
        <NavLinks links={PUBLIC_NAV_LINKS} />
      ) : authEnabled ? (
        <HostedNavigation />
      ) : (
        <NavLinks links={shellNavLinks(false, false)} />
      )}
    </nav>
  );
}
