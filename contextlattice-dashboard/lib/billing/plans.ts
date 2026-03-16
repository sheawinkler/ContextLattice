export type Plan = {
  id: string;
  name: string;
  description: string;
  monthly: number;
  annual: number;
  seats: string;
  features: string[];
};

export const PLANS: Plan[] = [
  {
    id: "starter",
    name: "Starter",
    description: "Solo builders and small agents.",
    monthly: 19,
    annual: 190,
    seats: "1-2 seats",
    features: [
      "HTTP-preferred MCP",
      "Private memory bank",
      "Qdrant recall",
      "Basic observability",
    ],
  },
  {
    id: "team",
    name: "Team",
    description: "Product teams shipping agent workflows.",
    monthly: 79,
    annual: 790,
    seats: "Up to 10 seats",
    features: [
      "Everything in Starter",
      "Shared workspaces",
      "Usage dashboards",
      "Priority support",
    ],
  },
  {
    id: "enterprise",
    name: "Enterprise",
    description: "Security, compliance, and scale.",
    monthly: 249,
    annual: 2490,
    seats: "Custom seats",
    features: [
      "SSO / SAML",
      "Private networking",
      "Custom retention",
      "Dedicated support",
    ],
  },
];
