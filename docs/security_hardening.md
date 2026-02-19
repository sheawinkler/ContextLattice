# Security & launch hardening checklist

## Auth
- ✅ Rate limiting for login and password reset (configurable via env)
- ✅ Password reset flow
- ☐ Email verification (toggle available via `AUTH_REQUIRE_EMAIL_VERIFICATION`)
- ☐ CAPTCHA / bot defense (recommended before public launch)
- ◑ Workspace-scoped API keys (hashing + revoke + last-used tracking)
- ◑ SCIM token scaffolding (disabled by default)

## Billing
- ✅ Stripe Checkout + webhook handler
- ✅ PayPal order + capture flow (webhook placeholder)
- ✅ Solana Pay + manual verification endpoint
- ✅ Kraken manual verification endpoint
- ✅ Coinbase Commerce webhook handler
- ✅ Webhook event audit log (billing events)
- ◑ Ledger reconciliation job (DB stale-intent scan; provider API sync pending)
- ☐ Invoice + receipt delivery

## Compliance
- ◑ Privacy policy, ToS, refund policy (link on billing page)
- ◑ GDPR/CCPA disclosures (export + deletion request scaffolding)
- ☐ PCI scope review (use Stripe-hosted checkout to reduce scope)

## Usage controls
- ◑ Usage budgets + monthly enforcement toggle
- ◑ Usage anomaly audit logging (threshold-based)

## Observability
- ☐ Audit logging for payment state changes
- ☐ Alerts for webhook failures
- ☐ Rate limit metrics
- ◑ Audit log retention/export scripts

## Dependencies
- NOTE: `bigint-buffer` vulnerability via `@solana/pay` -> `@solana/spl-token`.
  - No upstream fix available as of now; monitor advisories and consider disabling Solana Pay in production if risk tolerance is low.
