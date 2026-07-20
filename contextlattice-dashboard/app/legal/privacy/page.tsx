export default function PrivacyPage() {
  return (
    <div className="cl-page cl-legal-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Privacy // local-first by design</p>
          <h2>Your memory is not inventory.</h2>
          <p>
            Self-hosted data stays under your control. Hosted services process only the account, billing,
            operational, and customer-provided data needed to deliver and secure the service. We do not sell personal data.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>effective</span>
          <strong>02.18.26</strong>
          <small>raw memory is never Aggregate Signal</small>
        </div>
      </section>

      <section className="cl-legal-grid">
        <article className="cl-panel">
          <p className="cl-kicker">Local lane</p>
          <h3>You hold the keys.</h3>
          <ul className="cl-legal-list">
            <li>You choose storage, retention, deletion, providers, and network exposure.</li>
            <li>No ContextLattice account is required for the public local runtime.</li>
            <li>Aggregate Signal is opt-in and excludes raw prompts and memory from its contribution boundary.</li>
          </ul>
        </article>
        <article className="cl-panel">
          <p className="cl-kicker">Hosted lane</p>
          <h3>Minimum necessary data.</h3>
          <ul className="cl-legal-list">
            <li>Account, billing, security, and support records serve explicit operational purposes.</li>
            <li>Retention follows plan, operations, and legal requirements.</li>
            <li>Access, correction, export, and deletion requests are honored where applicable.</li>
          </ul>
        </article>
      </section>

      <section className="cl-panel cl-legal-source">
        <div>
          <p className="cl-kicker">Full policy</p>
          <h3>Inspect every declared data flow.</h3>
        </div>
        <p>The full policy names collected data, uses, disclosures, retention, security posture, and user rights.</p>
        <a className="cl-button cl-button--secondary" href="https://github.com/sheawinkler/ContextLattice/blob/main/docs/legal/PRIVACY_POLICY.md">
          Open Privacy Policy
        </a>
      </section>
    </div>
  );
}
