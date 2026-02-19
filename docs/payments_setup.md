# Payments setup (ContextLattice)

This repo now includes auth + billing scaffolding in `memmcp-dashboard/`.

## 1) Dashboard env
Copy the sample env file and adjust:

```bash
cp memmcp-dashboard/.env.example memmcp-dashboard/.env
```

Then generate Prisma client:

```bash
cd memmcp-dashboard
npm run db:generate
npm run db:push
```

## 2) Stripe (cards)
Required env:
- `STRIPE_SECRET_KEY`
- `STRIPE_WEBHOOK_SECRET`
- `STRIPE_PRICE_*`

Checkout endpoint:
- `POST /api/billing/stripe/checkout`

Portal endpoint:
- `POST /api/billing/stripe/portal`

Webhook endpoint:
- `POST /api/billing/stripe/webhook`

## 3) PayPal
Required env:
- `PAYPAL_CLIENT_ID`
- `PAYPAL_CLIENT_SECRET`

Order endpoint:
- `POST /api/billing/paypal/order`

Capture endpoint:
- `POST /api/billing/paypal/capture`

## 4) Solana Pay (crypto)
Required env:
- `SOLPAY_RECIPIENT`

Optional:
- `SOLPAY_SPL_TOKEN` (USDC mint)

Endpoint:
- `POST /api/billing/solana-pay`

## 5) Kraken / Krak (manual until merchant API)
Required env:
- `KRAKEN_PAY_ADDRESS`

Endpoint:
- `POST /api/billing/kraken`

## 6) Coinbase Commerce
Required env:
- `COINBASE_COMMERCE_API_KEY`

Endpoint:
- `POST /api/billing/coinbase/charge`

## Notes
- `AUTH_REQUIRED=false` keeps the dashboard open for local usage.
- In production, set `AUTH_REQUIRED=true` to enforce authentication.
- Providers auto-disable in the UI if required env keys are missing.
- Providers can also be explicitly disabled via `STRIPE_ENABLED`, `PAYPAL_ENABLED`,
  `SOLANA_PAY_ENABLED`, `KRAKEN_PAY_ENABLED`, `COINBASE_COMMERCE_ENABLED`.
- All webhooks now write to the BillingEvent audit log for traceability.
- Reconciliation helper: `npm run billing:reconcile` (marks stale intents if enabled).
- Provider reconciliations: `npm run billing:reconcile:stripe`, `:paypal`, `:coinbase`
  (set `BILLING_RECONCILE_APPLY=true` to apply changes).

## Webhooks
- PayPal webhook endpoint: `/api/billing/paypal/webhook` (placeholder signature check)
- Coinbase webhook endpoint: `/api/billing/coinbase/webhook` (HMAC signature)
- Solana/Kraken verification: `/api/billing/solana-pay/verify`, `/api/billing/kraken/verify`

## Reconciliation status
- `GET /api/billing/reconcile/status` shows recent intent counts and failed webhook totals.

## Entitlements + subscription enforcement

Env flags (dashboard):
- `DEFAULT_PLAN_ID=starter`
- `REQUIRE_ACTIVE_SUBSCRIPTION=false` (set true to hard-stop memory writes/usage events without an active subscription)

Plan entitlements currently enforce API key limits, max projects, max write size, and SCIM gating (Enterprise-only).
