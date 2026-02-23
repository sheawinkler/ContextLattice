# ROI Calculator (v0)

Use this to quantify savings from memory vs. long‑context prompts.

## Inputs
- Daily input tokens (baseline)
- Daily output tokens (baseline)
- % of requests using long context (>200k)
- Average tokens saved by ContextLattice (target: 30–80%)
- Model pricing (input/output) and any long‑context multipliers
- Memory cost per GB/month (or per 1M tokens stored)

## Calculations
1) **Baseline monthly cost** = (input tokens/day * input price + output tokens/day * output price) * 30
2) **Adjusted monthly cost** = baseline cost * (1 - savings%)
3) **Monthly savings** = baseline - adjusted
4) **Net savings** = monthly savings - memory layer cost
5) **ROI** = net savings / memory layer cost

## Example (placeholder numbers)
- Baseline input tokens/day: 1,000,000
- Baseline output tokens/day: 250,000
- Savings from memory: 50%
- Memory layer cost: $200/mo

Baseline = $X (use pricing for your model)\n
Adjusted = 0.5 * Baseline\n
Net savings = (Baseline - Adjusted) - 200\n
ROI = Net savings / 200

## Tips
- Use a conservative savings range (30–50%) in early pilots.
- Treat improved success rate as a separate benefit (higher task completion).
