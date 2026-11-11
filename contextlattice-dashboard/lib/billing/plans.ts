import {
  COMMERCIAL_PLANS,
  COMMERCIAL_TRUTH,
  type CommercialPlanId,
} from "@/lib/billing/commercial.generated";

export type Plan = {
  id: CommercialPlanId;
  name: string;
  description: string;
  monthly: number | null;
  annual: number | null;
  seats: string;
  features: string[];
  featureIds: readonly string[];
  paid: boolean;
  purchasable: boolean;
  customPricing: boolean;
};

const featureLabels = new Map<string, string>(
  COMMERCIAL_TRUTH.features.map((feature) => [feature.id, feature.buyer_label]),
);

function seatLabel(plan: (typeof COMMERCIAL_PLANS)[number]): string {
  if (plan.limits.included_seats !== null) {
    const count = plan.limits.included_seats;
    const label = `${count} included ${count === 1 ? "seat" : "seats"}`;
    return plan.pricing.custom ? `${label}; custom contract` : label;
  }
  if (plan.id === "team") return "Team workspace";
  if (plan.id === "starter") return "Single operator";
  if (plan.id === "operator") return "Advanced operator";
  return "Local use";
}

export const ALL_PLANS: Plan[] = COMMERCIAL_PLANS.map((plan) => ({
  id: plan.id,
  name: plan.buyer_label,
  description: plan.description,
  monthly: plan.pricing.monthly_usd,
  annual: plan.pricing.annual_usd,
  seats: seatLabel(plan),
  features: plan.feature_ids.map((featureId) => featureLabels.get(featureId) || featureId),
  featureIds: plan.feature_ids,
  paid: plan.paid,
  purchasable: plan.self_serve_purchasable,
  customPricing: plan.pricing.custom,
}));

export const PLANS = ALL_PLANS.filter((plan) => plan.paid);

export function planHighlights(plan: Plan): string[] {
  const free = new Set<string>(ALL_PLANS.find((entry) => !entry.paid)?.featureIds || []);
  const ids = plan.featureIds.filter((id) => !plan.paid || !free.has(id));
  // Put the workflow capabilities ahead of the distribution/access prerequisites.
  const ordered = plan.paid
    ? [...ids.filter((id) => id.startsWith("frontier_")), ...ids.filter((id) => !id.startsWith("frontier_"))]
    : ids;
  return ordered.slice(0, 6).map((id) => featureLabels.get(id) || id);
}

export function planSelectionHref(planId: string, localBillingEnabled: boolean): string {
  const plan = ALL_PLANS.find((entry) => entry.id === planId);
  if (!plan) return "/pricing";
  if (!plan.paid) return "/downloads";
  if (!plan.purchasable) return "mailto:hello@contextlattice.io?subject=ContextLattice%20Enterprise";
  return localBillingEnabled
    ? `/billing?plan=${encodeURIComponent(plan.id)}`
    : `https://contextlattice.io/premium.html#plan-${encodeURIComponent(plan.id)}`;
}
