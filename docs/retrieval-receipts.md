# Retrieval Receipts

ContextLattice does not ask an agent to trust a pile of retrieved text. It
returns a bounded evidence packet and the receipts for every retrieval decision
that shaped it.

The CLI remains the prescribed interface. HTTP is available for application
integration, and MCP remains an optional host adapter.

## Inspect a receipt

```bash
contextlattice_pack \
  "prove the current release is ready" \
  --project contextlattice \
  --target-context-pack-tokens 2400 \
  --pretty
```

The response carries two canonical root contracts:

- `memory_trust_assessment.v1` labels observed provenance, instruction-shaped
  content, duplicate campaigns, consensus requirements, quarantine, and the
  exact influence boundary.
- `retrieval_decision_trace.v1` records selected, omitted, deduplicated,
  quarantined, and truncated candidates, plus `why_now` and the marginal stop.

`context_pack` and `context_compiler` contain compact references to those root
contracts rather than duplicating the full receipts. Raw candidate text is
never copied into the decision trace.

## The memory boundary

Retrieved memory is evidence. It is never instruction, policy, or behavior
authority.

- Prompt overrides, credential exfiltration, encoded instructions, and
  destructive single-source claims fail closed into quarantine.
- Self-awarded trust is ignored.
- Repeated risky paraphrases do not manufacture independent consensus.
- Superseded or retracted policy has zero current influence.
- Legitimate bounded runbooks remain usable when their observed provenance is
  safe.
- Quarantined content has zero ranking and prompt influence. Disabling a paid
  automation surface cannot disable this public defense.

## Prove what helped

```bash
# The normal saved recall gate.
contextlattice_recall_quality_eval --tuning

# Same-case, same-snapshot leave-one-source and leave-one-memory analysis.
contextlattice_recall_quality_eval --ablation --pretty

# Immutable review-only proposals from independently verified outcomes.
contextlattice_recall_quality_eval --derive --pretty

# Advisory calibration for evidence sources, files, agents, and memories.
contextlattice_recall_quality_eval \
  --reputation \
  --project contextlattice \
  --pretty
```

Ablation reports exact result, rank, and citation deltas for one captured
snapshot. They do not infer utility. Synthetic rows and rows without an
observed outcome pair cannot become promotion evidence.

Derived regressions remain immutable proposals until review. Train/holdout
leakage, missing negative expectations, unstable proof, and unredacted fixture
material are rejected instead of reconstructed from telemetry.

Evidence reputation is advisory in the public core. It requires independently
verified explicit attribution, minimum samples, independent issuers, and time
decay. It cannot self-award trust or override contradiction, opposition, or
quarantine.

## Proof-carrying synthesis

```bash
contextlattice_synthesis_pack_v2 \
  "explain why another project's evidence matters" \
  --project contextlattice \
  --pretty
```

`causal_bridge_explanation.v1` supports a causal conclusion only when a typed
causal-capable graph edge resolves at both ends and current structured claim
proof carries sufficient citations. Lexical, project, and associative
similarity alone always abstain.

## Paid governance boundary

The public core includes local receipts, quarantine, ablation, regression
proposals, advisory reputation, and causal explanations. Paid runtimes can add
workspace policy, bounded retention, scheduled evaluation, shared incident
review, and governed activation. Paid policy can reduce optional automation to
zero; it cannot bypass the public trust boundary or delete immutable evidence.

Use the primary Go-native CLI to inspect or change the paid governance layer:

```bash
contextlattice retrieval-governance status \
  --feature receipts \
  --project contextlattice \
  --pretty

contextlattice retrieval-governance configure \
  --feature counterfactual \
  --project contextlattice \
  --retention-days 30 \
  --schedule weekly \
  --incident-review required \
  --reason "govern the weekly retrieval holdout" \
  --pretty

contextlattice retrieval-governance deactivate \
  --feature defense \
  --project contextlattice \
  --reason "stop paid automation while preserving core quarantine" \
  --pretty
```

Feature values are `receipts`, `causal-bridges`, `counterfactual`,
`reputation`, `regressions`, and `defense`. ContextLattice accepts no CLI flag
for overriding workspace, plan, trust isolation, quarantine, or the public
fail-closed boundary.
