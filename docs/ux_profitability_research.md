# UX/UI & profitability research (notes)

## Payments & conversion
- Stripe Checkout provides a **prebuilt, optimized** payment flow (supports subscriptions, promotion codes, and regional compliance). citeturn0open0
- PayPal Orders API supports a standard web checkout flow with explicit **order creation + capture** steps. citeturn1open0
- Solana Pay enables **wallet-based payments** via a URL/QR flow; good for crypto-native users who prefer on-chain settlement. citeturn0search1
- Kraken’s **Krak** app supports sending payments to merchants (manual until a full merchant API is available). citeturn1search0
- Coinbase Commerce provides a hosted crypto checkout flow and API for creating charges. citeturn2search0

## UX improvements to ship
- **Single CTA per plan** with a fallback (card → PayPal → crypto) keeps decision load low.
- **Trial/discount toggles**: keep a single pricing table with annual savings highlighted; avoid “choice overload”.
- **Trust badges**: mention private context, self-hosted option, and data ownership near checkout.
- **Billing portal**: link to Stripe portal so updates/cancellations are self‑serve.
- **Crypto clarity**: state “manual verification” if the provider doesn’t supply webhooks yet.

## Profitability levers
- **Baseline token audit**: quantify pre/post savings in the pilot to justify subscription pricing.
- **Usage-based overages**: use Stripe metered billing once usage tracking is stable.
- **Design partners**: secure a small cohort willing to share baseline usage metrics (used only to compute ROI and pricing).

## UX polish to prioritize next
- Add a branded pricing page in the public overview site with the same plan story.
- Add a “plan picker” modal that chooses a payment method based on location and wallet availability.
- Add a usage dashboard panel showing “tokens saved” or “context reuse rate.”
