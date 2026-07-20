import { COMMERCIAL_TRUTH } from "@/lib/billing/commercial.generated";

export default function TermsPage() {
  return (
    <div className="cl-page cl-legal-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Legal // service boundary</p>
          <h2>Terms without the fog.</h2>
          <p>
            Hosted accounts and paid services follow the published Terms of Service, acceptable-use policy,
            and any signed order form. Local open-source use remains governed by the repository license.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>effective</span>
          <strong>02.18.26</strong>
          <small>{COMMERCIAL_TRUTH.product.stable_tag}</small>
        </div>
      </section>

      <section className="cl-legal-grid">
        <article className="cl-panel">
          <p className="cl-kicker">Your side</p>
          <h3>Use the service responsibly.</h3>
          <ul className="cl-legal-list">
            <li>Safeguard account credentials, runtime licenses, and API keys.</li>
            <li>Use the service and stored data only as permitted by law and policy.</li>
            <li>Pay published charges or the amount in a signed order form.</li>
          </ul>
        </article>
        <article className="cl-panel">
          <p className="cl-kicker">Our side</p>
          <h3>Your data stays yours.</h3>
          <ul className="cl-legal-list">
            <li>You retain ownership of customer data.</li>
            <li>We process hosted data only to operate, secure, and support the service.</li>
            <li>Abuse, material security risk, or non-payment may trigger bounded suspension.</li>
          </ul>
        </article>
      </section>

      <section className="cl-panel cl-legal-source">
        <div>
          <p className="cl-kicker">Full contract</p>
          <h3>Read the actual terms.</h3>
        </div>
        <p>The repository policy is the complete public baseline; a signed commercial agreement controls when it differs.</p>
        <a className="cl-button cl-button--secondary" href="https://github.com/sheawinkler/ContextLattice/blob/main/docs/legal/TERMS_OF_SERVICE.md">
          Open Terms of Service
        </a>
      </section>
    </div>
  );
}
