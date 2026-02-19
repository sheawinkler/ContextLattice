export default function RefundsPage() {
  return (
    <div className="card space-y-4">
      <h1 className="text-2xl font-semibold">Refund Policy</h1>
      <p className="text-sm text-slate-300">
        Refunds are evaluated case-by-case until automated billing operations
        are fully self-serve. Contact support within 14 days of purchase with
        the payment reference for review.
      </p>
      <p className="text-sm text-slate-400">
        Crypto payments require manual verification and may have longer refund
        timelines depending on network settlement.
      </p>
    </div>
  );
}
