import { PLANS } from "@/lib/billing/plans";

const PREMIUM_URL = "https://contextlattice.io/premium.html";

export default function PricingPage() {
  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Pricing</h2>
        <p className="text-sm text-slate-400 mt-1">
          Compare paid ContextLattice plans for solo operators, teams, and enterprise deployments. Annual plans include a discount.
        </p>
        <div className="mt-4 text-sm text-slate-300 space-y-1">
          <p><strong>Step 1:</strong> compare the current catalog below.</p>
          <p><strong>Step 2:</strong> open the public premium page for availability and purchase guidance.</p>
          <p><strong>Step 3:</strong> follow the onboarding path listed for your plan.</p>
        </div>
      </section>

      <section className="grid md:grid-cols-2 xl:grid-cols-4 gap-4">
        {PLANS.map((plan) => (
          <div key={plan.id} className="card space-y-3">
            <h3 className="text-lg font-semibold">{plan.name}</h3>
            <p className="text-sm text-slate-400">{plan.description}</p>
            <div>
              {plan.customPricing ? (
                <div className="text-2xl font-semibold">Custom contract</div>
              ) : (
                <>
                  <div className="text-2xl font-semibold">${plan.monthly}/mo</div>
                  <div className="text-sm text-slate-400">${plan.annual}/yr</div>
                </>
              )}
            </div>
            <p className="text-sm text-slate-400">{plan.seats}</p>
            <ul className="text-sm text-slate-300 space-y-1">
              {plan.features.map((feature) => (
                <li key={feature}>• {feature}</li>
              ))}
            </ul>
            <a
              className={plan.customPricing
                ? "inline-flex items-center justify-center rounded border border-slate-600 px-4 py-2 font-semibold"
                : "inline-flex items-center justify-center rounded bg-emerald-500 text-emerald-950 px-4 py-2 font-semibold"}
              href={PREMIUM_URL}
              target="_blank"
              rel="noreferrer"
            >
              {plan.customPricing ? "Discuss an enterprise contract" : `Explore ${plan.name}`}
            </a>
          </div>
        ))}
      </section>
    </div>
  );
}
