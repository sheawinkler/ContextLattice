export default function RefundsPage() {
  return (
    <div className="cl-page cl-legal-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Refunds // human review</p>
          <h2>If the purchase failed you, say so.</h2>
          <p>
            Refund requests are reviewed case by case. Submit the request within 14 days with the payment
            reference and workspace identifier so the original transaction can be verified.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>request window</span>
          <strong>14 days</strong>
          <small>original payment rail</small>
        </div>
      </section>

      <section className="cl-legal-grid">
        <article className="cl-panel">
          <p className="cl-kicker">Card + PayPal</p>
          <h3>Back through the provider.</h3>
          <p className="cl-panel-note">Approved refunds return through the original processor and remain subject to its settlement timing.</p>
        </article>
        <article className="cl-panel">
          <p className="cl-kicker">Crypto</p>
          <h3>Verification before return.</h3>
          <p className="cl-panel-note">Network-settled payments require manual verification; abusive or fraudulent usage is not eligible.</p>
        </article>
      </section>

      <section className="cl-panel cl-legal-source">
        <div>
          <p className="cl-kicker">Start the review</p>
          <h3>Bring the receipt.</h3>
        </div>
        <p>Include the transaction reference, purchase date, plan, workspace identifier, and the reason for the request.</p>
        <a className="cl-button" href="mailto:hello@contextlattice.io?subject=ContextLattice%20refund%20request">
          Request review
        </a>
      </section>
    </div>
  );
}
