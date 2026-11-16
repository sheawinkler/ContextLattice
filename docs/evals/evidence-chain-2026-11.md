# Evidence you can inspect

ContextLattice is built to remember the work, explain the context, and leave a
trail you can question. A receipt should tell you what happened, not ask you to
trust a green badge.

This report was recorded on **2026-11-16**. It separates product evidence,
engineering checks, and measured utility. It is not a new application release,
a universal compatibility certificate, or a claim that agents became smarter.
The [machine-readable snapshot](evidence-chain-2026-11.json) records the units
and limitations alongside the numbers.

## Follow the chain

```text
source + toolchain + inputs
           |
actual invocation + bounded observations
           |
receipt + hashes + explicit scope
           |
coverage + reviewed changes + exclusions
           |
source validation
           |
separate final-byte installation and release checks
```

Hash matching establishes identity. It does not establish that a test was good,
that a receipt was independently authenticated, or that a customer succeeded.
Source checks, installed acceptance, and agent outcomes are different evidence.

At the product level, [decision-evidence continuity](../decision-evidence-continuity.md)
binds decisions and context delivery to task/session identity and exposes missing
proof instead of filling the gap with a success claim. The CLI remains primary:

```zsh
contextlattice context "resume this project and show the evidence for the next move" --project example --pretty
contextlattice utility status --pretty
```

These are usage examples, not observations recorded by this report. A context
request may create its normal session/telemetry records; it does not authorize
paid automation or execute a proposed action.

## What the records actually show

### Public fixture evidence

The existing [compositional-response ledger](v5.0.0-compositional-response-intelligence.json)
records **1,169 exported fixture rows and 1,169 contract-valid rows**. Its dated
record is **2026-08-09**, not a fresh run performed for this report.

The harness exercised route code with synthetic fixtures. The ledger explicitly
records `production_runtime_used: false`, `latency_comparable: false`, estimated
transport-token counts, and `custody.state: local_regression/contaminated`.
Promotional claims are disabled. This is useful regression evidence; it is not
an untouched holdout, a live-deployment result, or independent task-success lift.
The existing [proof-boundary ledger](v5.0.0-context-pack-proof-boundary.json)
separately records adversarial identity, byte-budget, receipt and lifecycle checks
with their own historical source and environment scope.

### Maintainer-reported source qualification

A two-lane source qualification recorded the following snapshot on 2026-11-16:

| Observation | Recorded result | Unit and scope |
| --- | ---: | --- |
| Source/build cohorts | 62 | 31 per source lane; not 62 independent tasks |
| Retained passed cohorts | 61 | Original successful captures were not rerun |
| Additional contract cases | 15 | The one new cohort closed a specific source-coverage gap |
| Completed test entries | 2,109 | Python methods, Node tests, and Go test/subtest pass events |
| Cargo build-only cohorts | 8 | Included in 62 cohorts; not counted as test entries |

This is an explicitly **maintainer-reported aggregate** across two source lanes,
not a public-checkout or installed-binary qualification. The complete mixed-lane
capture is not reproducible from this public repository alone. The snapshot
includes a completion-record digest as an integrity reference, not as a
substitute for accessible raw evidence or independent validation. No private
fixtures, user memory, credentials, or paid implementation are published here.

The source intake produced time-bounded diagnostic decisions, not reusable
package, publication, promotion, or activation permission. Freshness at a past
check is not freshness at a later action. Qualification remains specific to the
recorded source, tools, inputs and platform; it cannot be transferred to changed
bytes just by retaining the same version label.

## Inspect and reproduce the public checks

Start from a pinned public commit, record its identity and use the tool versions
specified by that checkout. On supported platforms, these existing test families
exercise receipt/file identity and task-bound decision acknowledgement:

```zsh
git rev-parse HEAD
python3 -m unittest scripts.tests.test_release_artifact_stream_identity
cd services/gateway-go
go test . -run '^TestAgentDecisionAck' -count=1
```

The [file-identity tests](../../scripts/tests/test_release_artifact_stream_identity.py)
and [decision-acknowledgement tests](../../services/gateway-go/agent_session_decision_ack_test.go)
are source checks. Platform skips remain skips, a compilation is not a native
install, and passing them does not reproduce the aggregate above or prove a
production outcome. Configure build caches and temporary storage appropriately
for the host before compiling. No paid harness, model call, or GitHub Actions run
is required by these examples.

## What would establish utility

The next useful question is not "how many tests are green?" It is "did the agent
finish the right work more reliably, with less total effort?"

A credible efficacy comparison needs a frozen, uncontaminated task set; declared
no-memory and retrieval baselines; a pinned harness/model; independent outcome
verification; and failures, retries, latency, tokens, tool calls and costs counted
across the whole task. Selection effects and uncertainty belong in the result,
not outside the denominator. The [evaluation approach](../wiki/concepts.md#evidence-and-performance)
and [CLI guide](../wiki/cli.md) describe the measurement boundaries.

This report establishes no new independent task-success gain, causal inference
savings, customer purchase, or live automation promotion. Smaller context is not
a smaller bill unless the complete measured workflow supports that conclusion.
Native installer qualification and real paid-license acceptance also remain
separate from the source evidence reported here. Public local use stays
account-free; paid qualification is not a condition of using the open-source core.

**Keep the memory useful. Keep the evidence inspectable. Keep the claims earned.**
