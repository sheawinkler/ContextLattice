interface SignalEntry {
  symbol: string;
  address: string;
  price_usd: number;
  volume_24h_usd: number;
  liquidity_usd: number;
  momentum_score: number;
  risk_score: number;
  verified?: boolean;
  created_at?: string;
}

export function SignalsPanel({ signals }: { signals?: SignalEntry[] }) {
  if (!signals || signals.length === 0) {
    return null;
  }
  return (
    <section className="card">
      <div className="flex items-center justify-between">
        <h2 className="text-xl font-semibold">Momentum Signals</h2>
        <span className="text-sm text-slate-400">
          {signals.length} recent discoveries
        </span>
      </div>
      <div className="overflow-x-auto mt-3">
        <table className="w-full text-sm">
          <thead>
            <tr className="text-left text-slate-300 border-b border-slate-700">
              <th className="py-2 pr-4">Token</th>
              <th className="py-2 pr-4">Price</th>
              <th className="py-2 pr-4">24h Vol</th>
              <th className="py-2 pr-4">Liquidity</th>
              <th className="py-2 pr-4">Momentum</th>
              <th className="py-2 pr-4">Risk</th>
            </tr>
          </thead>
          <tbody>
            {signals.map((signal) => (
              <tr
                key={`${signal.symbol}-${signal.address}`}
                className="border-b border-slate-800"
              >
                <td className="py-2 pr-4">
                  <div className="font-semibold flex items-center gap-2">
                    {signal.symbol}
                    {signal.verified && (
                      <span className="text-emerald-400 text-xs">✔</span>
                    )}
                  </div>
                  <div className="text-xs text-slate-400">
                    {signal.address.slice(0, 4)}…{signal.address.slice(-4)}
                  </div>
                </td>
                <td className="py-2 pr-4">${signal.price_usd.toFixed(6)}</td>
                <td className="py-2 pr-4">
                  ${signal.volume_24h_usd.toLocaleString(undefined, {
                    maximumFractionDigits: 0,
                  })}
                </td>
                <td className="py-2 pr-4">
                  ${signal.liquidity_usd.toLocaleString(undefined, {
                    maximumFractionDigits: 0,
                  })}
                </td>
                <td className="py-2 pr-4">
                  <span
                    className={`font-semibold ${
                      signal.momentum_score >= 1.2
                        ? "text-emerald-400"
                        : signal.momentum_score <= 0.6
                        ? "text-rose-400"
                        : "text-slate-200"
                    }`}
                  >
                    {signal.momentum_score.toFixed(2)}
                  </span>
                </td>
                <td className="py-2 pr-4">
                  <span
                    className={`font-semibold ${
                      signal.risk_score >= 0.65
                        ? "text-rose-400"
                        : signal.risk_score <= 0.3
                        ? "text-emerald-400"
                        : "text-amber-300"
                    }`}
                  >
                    {signal.risk_score.toFixed(2)}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
