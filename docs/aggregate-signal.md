# Aggregate Signal

Memory can improve the system without becoming the product being exported.

Aggregate Signal accepts only an allowlisted, clipped numerical or categorical sufficient statistic. It never accepts raw memory, prompts, embeddings, file paths, project names, exact timestamps, or stable installation identifiers. The default path is local-only and performs no external network call.

## Primary CLI

Preview first. Preview does not persist:

```bash
contextlattice_aggregate_signal preview \
  --metric repair_rate \
  --value 0.1 \
  --pretty
```

Queueing requires explicit consent and a fresh nonce:

```bash
contextlattice_aggregate_signal queue \
  --metric repair_rate \
  --value 0.1 \
  --opt-in \
  --nonce "$(openssl rand -hex 16)" \
  --pretty
```

Inspect the owner-only local queue and rolling composition:

```bash
contextlattice_aggregate_signal status --pretty
```

Generate a local report from an explicit contribution payload:

```bash
contextlattice_aggregate_signal report \
  --payload-file aggregate-report-request.json \
  --output aggregate-report.json \
  --pretty
```

Stop future contribution and delete unreleased queued rows:

```bash
contextlattice_aggregate_signal opt-out --confirm --pretty
```

Opt-out never claims that an already released aggregate can be subtracted.

## Hard Bounds

- Cohorts smaller than 20 are suppressed.
- One installation can contribute once per metric and week.
- Each release is capped at epsilon 0.25.
- Rolling 90-day composition is capped at epsilon 2.0.
- Delta is capped at 0.000001.
- State is owner-only, atomically persisted, and bounded to 64 MiB and 100,000 records.
- Repeated report requests are idempotent; changed replay parameters are rejected as differencing attempts.
- Raw memory and prompt export remain forbidden recursively.

## Evidence Boundary

Aggregate Signal is a controlled preview, not a formal privacy certification. Its clipping, suppression, noise, accounting, expiry, replay, and opt-out controls are enforced contracts. Production promotion remains blocked until independent membership-inference, attribute-inference, reconstruction, malicious-client, accountant-exhaustion, and utility-loss reviews pass.

Operator and Enterprise artifacts add credential-derived workspace governance, revocation, workspace isolation, and hash-linked audit receipts. The gateway still performs no model or runner execution and enables no external transport.

Disable the local surface with `CONTEXTLATTICE_AGGREGATE_SIGNAL_ENABLED=false`.
