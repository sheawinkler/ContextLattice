# Reading ContextLattice telemetry

## Mongo startup recovery

Telemetry is optional and has a separate initialization state from core gateway readiness. `/healthz` and `/readyz` expose `telemetrySinkInitialization` (`configured`, `ready`, `phase`, attempt count and a fixed nonsecret failure code). A ready gateway does not imply Mongo telemetry is ready.

If Mongo starts late, the gateway retains its configured client and retries Ping plus required event/blob indexes without a gateway restart. Each attempt is bounded by `GO_TELEMETRY_CONNECT_TIMEOUT_SECS` (1-30 seconds, default 5); retry delays increase from 1 to 30 seconds and stay capped while the gateway is running. Static invalid configuration is rejected rather than retried.

Before initialization succeeds, writes use the existing telemetry spool/error policy, reads can use the spool, and blob GC does not start. New writes use Mongo after initialization; this does not automatically drain earlier spool entries. Later database outages still use existing operation-time fallback. Shutdown cancels retries and GC. `GO_TELEMETRY_SINK_ENABLED=false` disables the client and retries entirely. Unobserved telemetry counts remain unknown, not measured zero.

The dashboard measures context accounting, not hypothetical billing savings.

- **Signed net context delta** retains expansion as a negative result. Compare the named cohort and window, not unrelated lifetime totals.
- **Positive-only compression** is a subtotal, not net savings or a causal reduction in inference.
- **Model-visible context**, serialized transport and compiled-prompt estimates have separate observation counts. Missing measurements stay unknown; a reported zero remains zero.
- **Modeled retry avoidance** is an estimator. An uncalibrated model is labeled uncalibrated, not verified.
- **Provider usage** comes from outcome reports. It does not imply independently verified work, a billing export, or causal savings.

## Provider coverage

`GET /telemetry/context-pack-quality` exposes `provider_usage_coverage_version: 1` and separate prompt, completion and complete-total observation counts. A total is complete only when explicitly reported, or when both prompt and completion counts were reported. A prompt-only report is partial, even if an older aggregate summed it as a total.

Historical positive aggregates remain available for compatibility. They do not establish complete coverage. Historical omitted zeros cannot be reconstructed as observations by replaying an outcome.

Presence metadata is an owner-only sidecar beside the configured quality ledger, with suffix `.provider-coverage.v1.json`. It stores only exact-record digests and field-presence masks, not prompts or credentials. It is capped at 512 KiB and at most 4,096 rows, further limited by the quality-ledger sample bound. Retention can reduce coverage; it never creates observations.

The canonical outcome is durably written first. The sidecar joins exact persisted record bytes, including gateway receipt time, rather than a lossy retry identity. Older binaries can still read the canonical v1 ledger. Missing or invalid metadata leaves coverage unknown, without discarding canonical outcomes. Invalid files are not silently overwritten; transient storage failures receive a bounded acknowledgement retry on a later write.

Inspect `storage.provider_coverage_sidecar` for enabled, loaded, healthy, row-count and error-code fields. Paths and record contents are not exposed by this status. Back up the sidecar with its quality ledger if preserving observation coverage matters; it is not a replacement for the ledger.

## Refresh behavior

Console and Overview fetch immediately while visible, then every five minutes, with a manual Refresh button. Concurrent refreshes coalesce. Hidden tabs cancel pending work; late responses cannot overwrite newer data. Failures back off to a maximum 30-minute interval and retain the last complete snapshot with a stale label.

Last attempt, last successful fetch and upstream source time are distinct. A successful fetch does not make old source data fresh. Each overview response has a snapshot identifier but explicitly declares `snapshotAtomic: false`: independently collected upstream values are not a transactional snapshot.

The memory overview fetches eight consumed aggregates. The full specialist view retains its other sources. Missing probes or failed requests are not successful health evidence.
