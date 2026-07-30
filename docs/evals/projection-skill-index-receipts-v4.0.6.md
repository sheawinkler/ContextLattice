# Projection and Skills Receipt Eval Ledger

Date: 2026-07-30
Scope: vector projection identity, multi-harness Skills Index discovery, and
skill usage-to-outcome receipts

## Baseline

- Owner memory hashes newline-normalized file content, while Qdrant hashed the
  unnormalized write payload. Current Qdrant events could therefore be rejected
  as stale solely because of projection formatting.
- Qdrant retrieval omitted `event_id`; pgvector did not guarantee either
  `event_id` or `content_hash` columns.
- The container indexed Codex active/system roots only. A generic
  `skill index audit` query could rank unrelated skills, and root inventory
  counted matches rather than all scanned skills.
- Byte-identical skills in multiple harness roots were returned independently
  without a shared digest/provenance identity.
- The installed CLI predating this source did not expose `usage-record` or
  `efficacy-review`.

## Metrics and acceptance thresholds

| Metric | Threshold | Result |
| --- | ---: | ---: |
| Current event accepted despite legacy hash mismatch | 100% | PASS |
| Stale event suppressed despite matching content hash | 100% | PASS |
| Surfaced vector rows per logical owner path | At most 1 | PASS |
| Generic-stopword-only Skills Index matches | 0 | PASS |
| Minimum discriminating-term coverage | 0.5 | PASS |
| Exact-content duplicate results | 1 canonical result with all provenance | PASS |
| Cross-harness receipt chains | 3 of 3 reach verified outcome | PASS |
| Isolated retention schedule intervals | At least 2, exit 0 | PASS |
| Gateway container identity/restart drift during retention smoke | 0 | PASS |

## Holdouts

- Current event ID with a deliberately mismatched legacy projection hash.
- Stale event ID with the current owner content hash.
- Multiple unidentified legacy rows for one authoritative path.
- A skill matching only one of three discriminating query concepts.
- Byte-identical skills in Codex and Hermes roots.
- Three distinct skills sourced from Codex, Hermes, and shared-agent roots.
- A PID-scoped launchd label using `/usr/bin/true`, a two-second interval, and
  an isolated temporary plist/log directory.

## Cost, latency, calls, and failures

- Model/API cost: USD 0; all checks are deterministic and local.
- Full Go suite: 67.141 seconds for the gateway package and 3.948 seconds for
  the CLI package.
- Three-harness receipt canary: 0.182 seconds for the gateway package.
- Retention installer suite: 5 tests in 1.405 seconds.
- Tool surfaces: Go test runner, Python unittest, launchctl, Docker through the
  OrbStack context, and Compose config validation.
- Expected negative cases passed: unproven outcomes, skipped receipt stages,
  idempotency conflicts, stale projection identities, insufficient query
  coverage, installer replacement failure, unload failure, and rollback.
- During development, one canary fixture used an unsupported selection reason;
  the receipt endpoint rejected it with HTTP 422 and the fixture was corrected
  to the existing `top_match` contract.
- Compose validation without the runtime env failed on pre-existing required
  variables; validation with the consuming runtime env passed.
- The unrelated OrbStack self-heal failure-injection suite was not run because
  it explicitly invokes Bash while this maintenance session is Zsh-only; no
  supervisor, recovery, or self-heal source changed.

## Reproduction

```zsh
cd services/gateway-go
go test ./... -count=1
go test ./... -run TestSkillsIndexCrossHarnessUsageToVerifiedOutcomeCanary -count=1

cd ../..
python3 -m unittest scripts.tests.test_retention_runner_install
docker --context orbstack compose --env-file /path/to/runtime.env config --quiet
```

Live installation evidence is recorded separately against the exact merged
commit so source tests cannot be mistaken for installed-runtime proof.
