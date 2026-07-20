import { ALL_PLANS } from "@/lib/billing/plans";
import { COMMERCIAL_TRUTH } from "@/lib/billing/commercial.generated";

const PREMIUM_URL = "https://contextlattice.io/premium.html";

function priceLabel(monthly: number | null, custom: boolean): string {
  if (custom) return "Custom";
  if (monthly === null || monthly === 0) return "$0";
  return `$${monthly}`;
}

export default function PricingPage() {
  return (
    <div className="cl-page cl-pricing-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Pricing // keep the core useful</p>
          <h2>Local intelligence starts free.</h2>
          <p>
            Run the public Go/Rust core locally without an account. Pay when you need hosted artifacts,
            workspace governance, protected operations, or advanced analytics, not to recover your own memory.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>release</span>
          <strong>{COMMERCIAL_TRUTH.product.stable_tag}</strong>
          <small>CLI remains primary</small>
        </div>
      </section>

      <section className="cl-proof-strip">
        <div><span className="cl-label">public local</span><strong>account-free</strong></div>
        <div><span className="cl-label">memory posture</span><strong>local-first</strong></div>
        <div><span className="cl-label">paid boundary</span><strong>governance + distribution</strong></div>
        <div><span className="cl-label">learning posture</span><strong>proof before promotion</strong></div>
      </section>

      <section className="cl-pricing-grid" aria-label="ContextLattice plans">
        {ALL_PLANS.map((plan) => {
          const visibleFeatures = plan.features.slice(0, 6);
          const remaining = Math.max(0, plan.features.length - visibleFeatures.length);
          const recommended = plan.id === "operator";
          return (
            <article
              key={plan.id}
              className={`cl-panel cl-pricing-card${recommended ? " cl-pricing-card--recommended" : ""}`}
            >
              <div className="cl-pricing-head">
                <div>
                  <p className="cl-kicker">{plan.paid ? "hosted / governed" : "public local"}</p>
                  <h3>{plan.name}</h3>
                </div>
                {recommended ? <span className="cl-badge">recommended</span> : null}
              </div>
              <p className="cl-pricing-description">{plan.description}</p>
              <div className="cl-pricing-price">
                <strong>{priceLabel(plan.monthly, plan.customPricing)}</strong>
                <span>{plan.customPricing ? "contract" : plan.monthly ? "/ month" : "forever"}</span>
              </div>
              {plan.annual && !plan.customPricing ? (
                <p className="cl-pricing-annual">${plan.annual} billed annually</p>
              ) : null}
              <p className="cl-pricing-seats">{plan.seats}</p>
              <ul className="cl-pricing-features">
                {visibleFeatures.map((feature) => <li key={feature}>{feature}</li>)}
                {remaining ? <li className="cl-pricing-more">+ {remaining} governed capabilities</li> : null}
              </ul>
              <div className="cl-pricing-action">
                {!plan.paid ? (
                  <a className="cl-button cl-button--secondary" href="/downloads">Install free local</a>
                ) : plan.purchasable ? (
                  <a className="cl-button" href={`${PREMIUM_URL}#plans`}>
                    Choose {plan.name}
                  </a>
                ) : (
                  <a
                    className="cl-button cl-button--secondary"
                    href="mailto:hello@contextlattice.io?subject=ContextLattice%20Enterprise"
                  >
                    Design the contract
                  </a>
                )}
              </div>
            </article>
          );
        })}
      </section>

      <section className="cl-panel cl-pricing-boundary">
        <div>
          <p className="cl-kicker">Aggregate Signal // controlled activation preview</p>
          <h3>Your memory is not the training set.</h3>
        </div>
        <p>
          Raw prompts and memory remain local. Only explicitly opted-in, clipped sufficient statistics may queue;
          small cohorts are suppressed, opt-out is immediate, and production cohort learning remains hard-blocked
          until independent privacy and utility reviews pass.
        </p>
        <a className="cl-text-link" href="https://contextlattice.io/premium.html">Inspect the full capability boundary</a>
      </section>
    </div>
  );
}
