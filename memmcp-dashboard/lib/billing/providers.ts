export type BillingProvider = "stripe" | "paypal" | "solana-pay" | "kraken" | "coinbase";

function enabledWithFlag(required: boolean, flagKey?: string) {
  if (!required) {
    return false;
  }
  if (!flagKey) {
    return true;
  }
  return process.env[flagKey] !== "false";
}

const stripeReady = Boolean(process.env.STRIPE_SECRET_KEY);
const paypalReady = Boolean(
  process.env.PAYPAL_CLIENT_ID && process.env.PAYPAL_CLIENT_SECRET,
);
const solanaReady = Boolean(process.env.SOLPAY_RECIPIENT);
const krakenReady = Boolean(process.env.KRAKEN_PAY_ADDRESS);
const coinbaseReady = Boolean(process.env.COINBASE_COMMERCE_API_KEY);

const stripeEnabled = enabledWithFlag(stripeReady, "STRIPE_ENABLED");
const paypalEnabled = enabledWithFlag(paypalReady, "PAYPAL_ENABLED");
const solanaEnabled = enabledWithFlag(solanaReady, "SOLANA_PAY_ENABLED");
const krakenEnabled = enabledWithFlag(krakenReady, "KRAKEN_PAY_ENABLED");
const coinbaseEnabled = enabledWithFlag(coinbaseReady, "COINBASE_COMMERCE_ENABLED");

export const billingProviders = {
  stripe: {
    name: "Card (Stripe)",
    enabled: stripeEnabled,
    reason: !stripeReady
      ? "Missing STRIPE_SECRET_KEY"
      : process.env.STRIPE_ENABLED === "false"
        ? "Disabled by configuration"
        : undefined,
  },
  paypal: {
    name: "PayPal",
    enabled: paypalEnabled,
    reason: !paypalReady
      ? "Missing PayPal credentials"
      : process.env.PAYPAL_ENABLED === "false"
        ? "Disabled by configuration"
        : undefined,
  },
  "solana-pay": {
    name: "Solana Pay",
    enabled: solanaEnabled,
    reason: !solanaReady
      ? "Missing SOLPAY_RECIPIENT"
      : process.env.SOLANA_PAY_ENABLED === "false"
        ? "Disabled by configuration"
        : undefined,
  },
  kraken: {
    name: "Krak / Kraken",
    enabled: krakenEnabled,
    reason: !krakenReady
      ? "Missing KRAKEN_PAY_ADDRESS"
      : process.env.KRAKEN_PAY_ENABLED === "false"
        ? "Disabled by configuration"
        : undefined,
  },
  coinbase: {
    name: "Coinbase Commerce",
    enabled: coinbaseEnabled,
    reason: !coinbaseReady
      ? "Missing COINBASE_COMMERCE_API_KEY"
      : process.env.COINBASE_COMMERCE_ENABLED === "false"
        ? "Disabled by configuration"
        : undefined,
  },
} as const;

export const priceIds = {
  starter: {
    monthly: process.env.STRIPE_PRICE_STARTER_MONTHLY,
    annual: process.env.STRIPE_PRICE_STARTER_ANNUAL,
  },
  team: {
    monthly: process.env.STRIPE_PRICE_TEAM_MONTHLY,
    annual: process.env.STRIPE_PRICE_TEAM_ANNUAL,
  },
  enterprise: {
    monthly: process.env.STRIPE_PRICE_ENTERPRISE_MONTHLY,
    annual: process.env.STRIPE_PRICE_ENTERPRISE_ANNUAL,
  },
} as const;

export function getStripePriceId(planId: string, interval: "monthly" | "annual") {
  const plan = priceIds[planId as keyof typeof priceIds];
  return plan?.[interval];
}
