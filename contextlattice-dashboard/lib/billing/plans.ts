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

const featureLabels = new Map(
  COMMERCIAL_TRUTH.features.map((feature) => [feature.id, feature.buyer_label]),
);

function seatLabel(plan: (typeof COMMERCIAL_PLANS)[number]): string {
  if (plan.limits.included_seats !== null) {
    return `${plan.limits.included_seats} included seats; custom contract`;
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
