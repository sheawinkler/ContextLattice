# $objective: hero_motion_smoothness_10

## Objective
Reach a consistent **10/10 perceived smoothness** for hero lane motion while preserving narrative clarity (multi-source flow converging into a living core).

## Scope
- `docs/public_overview/index.html`
- `docs/public_overview/index-orb-white.html`
- `docs/public_overview/styles-fracture.css`

## Acceptance Criteria
1. Lane morph animations use spline interpolation (`calcMode="spline"`), avoiding hard linear transitions.
2. Particle and orb travel uses spline motion with eased acceleration/deceleration (no step-like jumps).
3. Serpentine and surge lanes remain readable at desktop and mobile widths (no aliasing shimmer spikes).
4. Reduced-motion preference suppresses anomaly trigger behavior.
5. Warm and white variants remain behaviorally identical (only visual palette differs).

## Signal Injection Rule
- Red anomaly pulse appears on random cadence:
  - `delay = 5 + 1.5*x` seconds, `x = randint(0, 50)`
- Pulse converges to core, then deflects right and fades out.

## Why This Objective Exists
The hero is the first system-level explanation users see. Smooth motion quality directly affects perceived engineering quality and product trust.
