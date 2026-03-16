import { PLANS } from "@/lib/billing/plans";

export default function PricingPage() {
  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Pricing</h2>
        <p className="text-sm text-slate-400 mt-1">
          Plans scale from solo builders to enterprise teams. Stripe handles card
          billing; PayPal and crypto options are available when configured. Annual plans include a discount.
        </p>
      </section>

      <section className="grid md:grid-cols-3 gap-4">
        {PLANS.map((plan) => (
          <div key={plan.id} className="card space-y-3">
            <h3 className="text-lg font-semibold">{plan.name}</h3>
            <p className="text-sm text-slate-400">{plan.description}</p>
            <div>
              <div className="text-2xl font-semibold">${plan.monthly}/mo</div>
              <div className="text-sm text-slate-400">${plan.annual}/yr</div>
            </div>
            <p className="text-sm text-slate-400">{plan.seats}</p>
            <ul className="text-sm text-slate-300 space-y-1">
              {plan.features.map((feature) => (
                <li key={feature}>• {feature}</li>
              ))}
            </ul>
            <a
              className="inline-flex items-center justify-center rounded bg-emerald-500 text-emerald-950 px-4 py-2 font-semibold"
              href="/billing"
            >
              Choose {plan.name}
            </a>
          </div>
        ))}
      </section>
    </div>
  );
}
