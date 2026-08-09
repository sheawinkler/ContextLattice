package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func exactTokenImpactSample(sampleID, scope, packedKind string, baseline, packed, delta int) map[string]any {
	saved := baseline - packed
	if saved < 0 {
		saved = 0
	}
	return map[string]any{
		"sample_id":                          sampleID,
		"scope":                              scope,
		"packed_kind":                        packedKind,
		"baseline_tokens_estimate":           baseline,
		"packed_tokens_estimate":             packed,
		"saved_tokens_estimate":              saved,
		"tokenizer_exact":                    true,
		"transport_inclusive":                true,
		"wire_tokens_exact":                  packed,
		"transport_tokens_exact":             packed,
		"model_visible_context_tokens_exact": packed,
		"net_token_delta":                    delta,
	}
}

func searchImpactEligibleOutcome(id string, capturedAt time.Time, success, repair bool, verifier string) map[string]any {
	return map[string]any{
		"outcome_id":                         id,
		"captured_at":                        capturedAt.UTC().Format(time.RFC3339Nano),
		"gateway_received_at":                capturedAt.UTC().Format(time.RFC3339Nano),
		"first_pass_success":                 success,
		"repair_required":                    repair,
		"verifier_id":                        verifier,
		"observed_yield_eligible":            true,
		"wire_tokens_exact":                  90,
		"model_visible_context_tokens_exact": 72,
	}
}

func searchImpactCausalUtilityRows(controlOne, treatmentOne, controlTwo, treatmentTwo string) []map[string]any {
	rows := make([]map[string]any, 0, 4)
	addPair := func(pairID, controlID, treatmentID string, controlValue, treatmentValue float64) {
		taskDigest := "sha256:" + sha256Hex("search-impact-task-"+pairID)
		assignmentDigest := "sha256:" + sha256Hex("search-impact-assignment-"+pairID)
		commonPairing := map[string]any{
			"pair_id":                         pairID,
			"task_match_digest":               taskDigest,
			"leakage_free":                    true,
			"matching_method":                 "paired_replay",
			"assignment_digest":               assignmentDigest,
			"experiment_id":                   "search-impact-experiment",
			"model":                           "test-model",
			"runner":                          "test-runner",
			"harness":                         "test-harness",
			"context_reconstruction_contract": "exact-context-v1",
		}
		controlPairing := cloneJSONMap(commonPairing)
		controlPairing["arm"] = "control"
		treatmentPairing := cloneJSONMap(commonPairing)
		treatmentPairing["arm"] = "treatment"
		treatmentPairing["matched_control_outcome_id"] = controlID
		denominator := map[string]any{
			"model_visible_context_tokens_exact": true,
			"model_visible_context_tokens":       72,
			"tokenizer_encoding":                 "cl100k_base",
		}
		rows = append(rows,
			map[string]any{
				"outcome_id": controlID, "project": "contextlattice", "task_class": "coding", "session_id": "control-" + pairID,
				"captured_at": "2026-08-04T12:00:00Z", "utility": map[string]any{"value": controlValue, "unit": "verified_tasks", "independently_verified": true},
				"eligibility": map[string]any{"observed_yield_eligible": true}, "denominator": cloneJSONMap(denominator), "pairing": controlPairing,
			},
			map[string]any{
				"outcome_id": treatmentID, "project": "contextlattice", "task_class": "coding", "session_id": "treatment-" + pairID,
				"captured_at": "2026-08-04T12:01:00Z", "utility": map[string]any{"value": treatmentValue, "unit": "verified_tasks", "independently_verified": true},
				"eligibility": map[string]any{"observed_yield_eligible": true}, "denominator": cloneJSONMap(denominator), "pairing": treatmentPairing,
			},
		)
	}
	addPair("pair-one", controlOne, treatmentOne, 3, 5)
	addPair("pair-two", controlTwo, treatmentTwo, 4, 6)
	return rows
}

func searchImpactValidMetrics(recall, ndcg, mrr, numeric, coverage, exactness, safetyRate, latency float64) map[string]any {
	return map[string]any{
		"decision_impact_recall_at_5": recall,
		"decision_impact_ndcg_at_5":   ndcg,
		"mrr":                         mrr,
		"numeric_exactness":           numeric,
		"citation_coverage":           coverage,
		"citation_exactness":          exactness,
		"safety_case_count":           2,
		"safety_failure_count":        int(math.Round(safetyRate * 2)),
		"safety_failure_rate":         safetyRate,
		"p95_latency_ms":              latency,
		"effective_k_min":             5,
		"effective_k_max":             5,
		"sparse_candidate_case_count": 0,
	}
}

func searchImpactValidComparativeShadow() map[string]any {
	caseSetRef := "sha256:" + sha256Hex("impact-case-set")
	actuatorBaseline := searchImpactValidMetrics(0.55, 0.50, 0.45, 0.90, 0.88, 0.80, 0, 2)
	actuatorTreatment := searchImpactValidMetrics(0.70, 0.66, 0.45, 0.90, 0.88, 0.80, 0, 3)
	for _, metrics := range []map[string]any{actuatorBaseline, actuatorTreatment} {
		metrics["numeric_expected_count"] = 4
		metrics["citation_expected_count"] = 6
		metrics["citation_candidate_count"] = 20
	}
	return map[string]any{
		"schema_id":             savedRecallImpactShadowEvalSchemaID,
		"version":               1,
		"comparison_scope":      savedRecallImpactComparisonScope,
		"comparison_fixed_k":    savedRecallImpactK,
		"comparison_valid":      true,
		"comparison_reason":     "valid",
		"case_count":            10,
		"case_set_ref":          caseSetRef,
		"project_scope_refs":    []any{savedRecallImpactOpaqueScopeRef("project", "contextlattice")},
		"task_class_scope_refs": []any{savedRecallImpactOpaqueScopeRef("task_class", "coding")},
		"latency_basis":         "shared_synthetic_retrieval_replay_ms",
		"baseline":              searchImpactValidMetrics(0.55, 0.50, 0.45, 0.90, 0.88, 0.80, 0, 120),
		"shadow":                searchImpactValidMetrics(0.70, 0.66, 0.45, 0.90, 0.88, 0.80, 0, 120),
		"learned_actuator_comparator": map[string]any{
			"schema_id": contextPackLearnedActuatorComparatorContractID, "version": 1,
			"comparison_scope":   contextPackLearnedActuatorComparisonScope,
			"comparison_fixed_k": contextPackLearnedActuatorFixedK,
			"comparison_valid":   true, "comparison_reason": "valid", "case_count": 10,
			"influenced_case_count":        4,
			"ranking_contract_id":          contextPackLearnedActivationContractID,
			"allocation_contract_id":       "context_pack_evidence_allocation.v1",
			"latency_basis":                "measured_context_pack_rank_and_allocate_ms",
			"same_returned_candidate_pool": true, "same_token_budget": true,
			"protected_selection_preserved": true, "case_set_ref": caseSetRef,
			"candidate_pool_ref":      "sha256:" + strings.Repeat("1", 64),
			"token_budget_ref":        "sha256:" + strings.Repeat("2", 64),
			"protected_partition_ref": "sha256:" + strings.Repeat("3", 64),
			"ranking_vector_ref":      "sha256:" + strings.Repeat("4", 64),
			"control_selection_ref":   "sha256:" + strings.Repeat("5", 64),
			"treatment_selection_ref": "sha256:" + strings.Repeat("6", 64),
			"reputation_vector_ref":   "sha256:" + strings.Repeat("7", 64),
			"baseline":                actuatorBaseline,
			"treatment":               actuatorTreatment,
		},
	}
}

func searchImpactCanaryInput() searchImpactIntelligenceInput {
	baseTime := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	outcomes := make([]map[string]any, 0, 10)
	for index := 0; index < 10; index++ {
		outcomes = append(outcomes, searchImpactEligibleOutcome(
			"outcome-"+string(rune('a'+index)), baseTime.Add(time.Duration(index)*time.Minute), index != 1, index == 1, "verifier-"+string(rune('a'+(index%2))),
		))
	}
	return searchImpactIntelligenceInput{
		CandidateOutcomes: outcomes,
		UtilitySummary: map[string]any{
			"causal_interval": map[string]any{"available": true, "low": 0.04, "high": 0.16},
		},
		UtilityRows:       searchImpactCausalUtilityRows("outcome-a", "outcome-b", "outcome-c", "outcome-d"),
		ComparativeShadow: searchImpactValidComparativeShadow(),
		ReceiptLedger: map[string]any{
			"enabled": true, "durability": "bounded_ndjson", "last_error": "",
		},
		ReceiptBinding: map[string]any{"pass": true, "missing_receipt_outcome_count": 0},
	}
}

func TestSearchImpactCausalGateUsesOnlyExactCandidateOutcomePairs(t *testing.T) {
	input := searchImpactCanaryInput()
	// This scoped Utility Ledger evidence is individually valid and positive,
	// but none of its outcome IDs occurs in the receipt-bound candidate set.
	// It must not make the candidate impact gate pass.
	input.UtilityRows = searchImpactCausalUtilityRows(
		"unrelated-control-one", "unrelated-treatment-one",
		"unrelated-control-two", "unrelated-treatment-two",
	)

	snapshot := buildSearchImpactIntelligence(input)
	gate := anyMap(anyMap(snapshot["proof_gates"])["causal_interval"])
	if anyToBool(snapshot["canary_eligible"]) || anyToBool(gate["pass"]) {
		t.Fatalf("unrelated scoped causal pairs satisfied the receipt-bound candidate gate: %#v", gate)
	}
	if got := anyToInt(gate["causal_pair_count"], -1); got != 0 {
		t.Fatalf("unrelated causal pairs leaked into candidate gate: pair_count=%d gate=%#v", got, gate)
	}
	if got, want := anyToString(gate["source"]), "exact_receipt_bound_candidate_outcome_ids"; got != want {
		t.Fatalf("causal gate source = %q, want %q", got, want)
	}
	reconciliation := anyMap(anyMap(snapshot["impact_intelligence"])["utility_reconciliation"])
	if got, want := anyToString(reconciliation["scoped_summary_role"]), "contextual_non_gating"; got != want {
		t.Fatalf("scoped Utility summary role = %q, want %q", got, want)
	}
	if got, want := anyToString(reconciliation["candidate_causal_gate_basis"]), "exact_receipt_bound_candidate_outcome_ids"; got != want {
		t.Fatalf("candidate causal gate basis = %q, want %q", got, want)
	}
	if !strings.Contains(anyToString(snapshot["measurement_limit"]), "contextual and non-gating") {
		t.Fatalf("measurement limit does not distinguish scoped context from causal gate: %q", snapshot["measurement_limit"])
	}

	// The same pair construction passes once every control and treatment ID is
	// an exact receipt-bound candidate outcome ID used by this report.
	input.UtilityRows = searchImpactCausalUtilityRows("outcome-a", "outcome-b", "outcome-c", "outcome-d")
	snapshot = buildSearchImpactIntelligence(input)
	gate = anyMap(anyMap(snapshot["proof_gates"])["causal_interval"])
	if !anyToBool(gate["pass"]) || !anyToBool(snapshot["canary_eligible"]) {
		t.Fatalf("exact candidate causal pairs did not satisfy the gate: %#v", gate)
	}
	if got, want := anyToInt(gate["causal_pair_count"], 0), 2; got != want {
		t.Fatalf("exact candidate causal pair count = %d, want %d", got, want)
	}
}

func TestTokenImpactTelemetryKeepsLegacyRowsAndSeparatesExactArtifactCohorts(t *testing.T) {
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_PATH", filepath.Join(t.TempDir(), "token-impact.ndjson"))
	telemetry := newTokenImpactTelemetry(32)

	legacy := exactTokenImpactSample("legacy-sample", "", "", 100, 80, 20)
	delete(legacy, "scope")
	delete(legacy, "packed_kind")
	telemetry.record(legacy)
	telemetry.record(legacy)

	first := exactTokenImpactSample("shared-sample", "retrieval-primary", "ranked-evidence", 100, 75, 25)
	telemetry.record(first)
	telemetry.record(first)                                                                                        // Exact replays are one artifact.
	telemetry.record(exactTokenImpactSample("shared-sample", "retrieval-primary", "ranked-evidence", 100, 80, 20)) // A conflicting rewrite is not called a replay.
	telemetry.record(exactTokenImpactSample("shared-sample", "retrieval-secondary", "ranked-evidence", 100, 70, 30))
	telemetry.record(exactTokenImpactSample("negative-sample", "retrieval-negative", "ranked-evidence", 100, 150, -50))

	snapshot := telemetry.snapshot()
	if got, want := anyToInt(snapshot["sample_count"], 0), 5; got != want {
		t.Fatalf("sample_count = %d, want %d (legacy rows remain observable and only one exact replay dedupes)", got, want)
	}
	if got, want := anyToInt(snapshot["legacy_sample_count"], 0), 2; got != want {
		t.Fatalf("legacy_sample_count = %d, want %d", got, want)
	}
	if got, want := anyToInt(snapshot["exact_artifact_replay_count"], 0), 1; got != want {
		t.Fatalf("exact_artifact_replay_count = %d, want %d", got, want)
	}
	if got, want := anyToInt(snapshot["exact_artifact_conflict_count"], 0), 1; got != want {
		t.Fatalf("exact_artifact_conflict_count = %d, want %d", got, want)
	}

	cohorts, ok := snapshot["cohorts"].([]map[string]any)
	if !ok {
		t.Fatalf("cohorts = %#v, want []map[string]any", snapshot["cohorts"])
	}
	if got, want := len(cohorts), 3; got != want {
		t.Fatalf("cohort count = %d, want %d", got, want)
	}
	if findTokenImpactCohort(cohorts, "retrieval_primary", "ranked_evidence") == nil {
		t.Fatal("missing normalized primary-scope cohort")
	}
	if findTokenImpactCohort(cohorts, "retrieval_secondary", "ranked_evidence") == nil {
		t.Fatal("missing distinct-scope cohort for the same sample id")
	}
	negative := findTokenImpactCohort(cohorts, "retrieval_negative", "ranked_evidence")
	if negative == nil {
		t.Fatal("missing negative-delta cohort")
	}
	if got, want := anyToInt(negative["signed_net_token_delta"], 0), -50; got != want {
		t.Fatalf("signed_net_token_delta = %d, want %d", got, want)
	}
	if _, conflated := negative["saved_tokens_estimate"]; conflated {
		t.Fatal("cohort must not conflate clipped saved-token estimates with signed impact")
	}
}

func TestTokenImpactTelemetryRejectsNonpositiveWireCountWithoutDiscardingExactTransport(t *testing.T) {
	sample := exactTokenImpactSample("wire-sample", "retrieval-wire", "ranked-evidence", 100, 75, 25)
	sample["wire_tokens_exact"] = -5
	entry := tokenImpactEntryFromSample(sample)
	if got, want := anyToInt(entry["wire_tokens_exact"], 0), 75; got != want {
		t.Fatalf("wire_tokens_exact = %d, want transport fallback %d", got, want)
	}
}

func findTokenImpactCohort(cohorts []map[string]any, scope, packedKind string) map[string]any {
	for _, cohort := range cohorts {
		if anyToString(cohort["scope"]) == scope && anyToString(cohort["packed_kind"]) == packedKind {
			return cohort
		}
	}
	return nil
}

func TestSearchImpactIntelligenceZeroEvidenceAbstains(t *testing.T) {
	snapshot := buildSearchImpactIntelligence(searchImpactIntelligenceInput{})
	if got, want := anyToString(snapshot["status"]), "abstain"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	if anyToBool(snapshot["canary_eligible"]) {
		t.Fatal("zero evidence must not recommend a canary")
	}
	if !containsString(anyToStringSlice(snapshot["abstention_reasons"]), "shadow_eval_missing") {
		t.Fatalf("abstention_reasons = %#v, want shadow_eval_missing", snapshot["abstention_reasons"])
	}
}

func TestSearchImpactReconciledCandidateOutcomesRequiresSelectedEvidence(t *testing.T) {
	row := evidenceReputationTestOutcome(1, true, "verifier-a")
	ref := outcomeIntelligenceCandidateRef("impact-selected")
	digest := anyToString(row["verification_evidence_digest"])
	row["evidence_attribution"] = []any{map[string]any{
		"entity_type": "candidate", "entity_id": ref, "candidate_ref": ref,
		"result_level_credit": "selection_receipt_bound", "selection_state": "selected",
		"attribution_method": "counterfactual", "verifier_id": row["verifier_id"],
		"verification_evidence_digest": digest,
	}}
	row["candidate_utility_verification"] = map[string]any{
		"outcome_id": row["outcome_id"], "sample_id": row["sample_id"],
		"independently_verified": true, "verification_status": "verified",
		"evidence_digest": digest, "verifier_id": row["verifier_id"],
		"observed_yield_eligible": true, "wire_tokens_exact": 90, "model_visible_context_tokens_exact": 72,
	}
	row["gateway_received_at"] = time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if got := len(searchImpactReconciledCandidateOutcomes([]map[string]any{row}, "contextlattice", "coding")); got != 1 {
		t.Fatalf("selected verified candidate outcome count = %d, want 1", got)
	}
	omitted := cloneJSONMap(row)
	parseRows(omitted["evidence_attribution"])[0]["selection_state"] = "omitted"
	if got := len(searchImpactReconciledCandidateOutcomes([]map[string]any{omitted}, "contextlattice", "coding")); got != 0 {
		t.Fatalf("omitted candidate received causal outcome credit: count=%d", got)
	}
	legacy := cloneJSONMap(row)
	delete(legacy, "gateway_received_at")
	if got := len(searchImpactReconciledCandidateOutcomes([]map[string]any{legacy}, "contextlattice", "coding")); got != 0 {
		t.Fatalf("legacy candidate without a server-observed receipt time was eligible: count=%d", got)
	}
	reporterControlled := cloneJSONMap(row)
	reporterControlled["captured_at"] = "1999-01-01T00:00:00Z"
	reporterControlled["gateway_received_at"] = "2026-08-04T12:34:56Z"
	chronological := searchImpactReconciledCandidateOutcomes([]map[string]any{reporterControlled}, "contextlattice", "coding")
	if got, want := anyToString(chronological[0]["captured_at"]), "2026-08-04T12:34:56Z"; got != want {
		t.Fatalf("candidate chronology used reporter captured_at: got %q, want gateway receipt %q", got, want)
	}
}

func TestReconcileCandidateUtilityVerificationCarriesExactOutcomeProof(t *testing.T) {
	row := map[string]any{
		"outcome_id": "outcome-exact", "evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "selection_state": "selected",
		}},
	}
	utility := &utilityTelemetry{
		byOutcome: map[string]int{"outcome-exact": 0},
		observations: []map[string]any{{
			"outcome_id": "outcome-exact", "sample_id": "sample-exact",
			"utility": map[string]any{
				"independently_verified": true, "verification_status": "verified", "evidence_digest": "sha256:" + sha256Hex("exact"), "verifier_id": "verifier-a",
			},
			"eligibility": map[string]any{"observed_yield_eligible": true},
			"denominator": map[string]any{
				"wire_tokens_exact": true, "wire_tokens": 91,
				"model_visible_context_tokens_exact": true, "model_visible_context_tokens": 73,
			},
		}},
	}
	got := reconcileCandidateUtilityVerification([]map[string]any{row}, utility)
	proof := anyMap(got[0]["candidate_utility_verification"])
	if !anyToBool(proof["observed_yield_eligible"]) || anyToInt(proof["wire_tokens_exact"], 0) != 91 || anyToInt(proof["model_visible_context_tokens_exact"], 0) != 73 {
		t.Fatalf("reconciled outcome omitted its exact Utility Ledger proof: %#v", proof)
	}
}

func TestSearchImpactExactDenominatorGateUsesOnlyCreditedOutcomes(t *testing.T) {
	input := searchImpactCanaryInput()
	// These scoped aggregate values are intentionally much larger than the
	// candidate set and must not compensate for an ineligible credited outcome.
	input.UtilitySummary["observed_yield_eligible_count"] = 1000
	input.UtilitySummary["denominators"] = map[string]any{
		"wire_tokens_exact": 999999, "model_visible_context_tokens_exact": 888888,
	}
	input.CandidateOutcomes[9]["observed_yield_eligible"] = false
	input.CandidateOutcomes[9]["wire_tokens_exact"] = 0
	input.CandidateOutcomes[9]["model_visible_context_tokens_exact"] = 0

	snapshot := buildSearchImpactIntelligence(input)
	if anyToBool(snapshot["canary_eligible"]) {
		t.Fatalf("unrelated scoped Utility Ledger totals satisfied an ineligible candidate gate: %#v", snapshot["proof_gates"])
	}
	exactGate := anyMap(anyMap(snapshot["proof_gates"])["exact_denominators"])
	if got, want := anyToInt(exactGate["observed_yield_eligible_outcome_count"], 0), 9; got != want {
		t.Fatalf("eligible exact outcome count = %d, want %d", got, want)
	}
	if got, want := anyToInt(exactGate["wire_tokens_exact"], 0), 810; got != want {
		t.Fatalf("wire exact total = %d, want same-set total %d", got, want)
	}
	if got, want := anyToInt(exactGate["model_visible_context_tokens_exact"], 0), 648; got != want {
		t.Fatalf("model-visible exact total = %d, want same-set total %d", got, want)
	}
}

func TestSearchImpactOutcomeExactProofFieldsParticipateInReplayIdentity(t *testing.T) {
	input := searchImpactCanaryInput()
	conflict := cloneJSONMap(input.CandidateOutcomes[0])
	conflict["captured_at"] = time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	conflict["model_visible_context_tokens_exact"] = 73
	input.CandidateOutcomes = append(input.CandidateOutcomes, conflict)

	snapshot := buildSearchImpactIntelligence(input)
	if anyToBool(snapshot["canary_eligible"]) {
		t.Fatalf("conflicting exact denominator receipt was treated as a replay: %#v", snapshot["proof_gates"])
	}
	identityGate := anyMap(anyMap(snapshot["proof_gates"])["outcome_identity_consistency"])
	if got := anyToInt(identityGate["conflict_count"], 0); got != 1 {
		t.Fatalf("denominator conflict count = %d, want 1", got)
	}
}

func TestSearchImpactResponseBindingIdentityAndConflictRules(t *testing.T) {
	_, binding := contextPackOutcomeResponseBindingFixture(t, "search-impact-binding")
	otherBinding := cloneJSONMap(binding)
	otherBinding["recall_response_id"] = "rr_" + strings.Repeat("b", 24)
	otherBinding["recall_response_digest"] = "sha256:" + strings.Repeat("b", 64)
	if recallResponseBindingsEqual(binding, otherBinding) {
		t.Fatal("distinct test binding unexpectedly matched the fixture")
	}
	if _, ok := recallResponseBindingFromSample(otherBinding); !ok {
		t.Fatal("failed to construct a distinct valid canonical binding")
	}
	base := searchImpactEligibleOutcome("binding-outcome-a", time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC), true, false, "verifier-a")
	if !recallResponseCopyBinding(base, binding) {
		t.Fatal("failed to attach canonical binding")
	}

	t.Run("same outcome and binding replays once", func(t *testing.T) {
		rows := []map[string]any{base, cloneJSONMap(base)}
		outcomes, replays, conflicts := searchImpactDeduplicateOutcomes(rows)
		if len(outcomes) != 1 || replays != 1 || conflicts != 0 {
			t.Fatalf("same-bound replay identity = outcomes=%d replays=%d conflicts=%d", len(outcomes), replays, conflicts)
		}
	})

	t.Run("same outcome changed binding conflicts", func(t *testing.T) {
		changed := cloneJSONMap(base)
		if !recallResponseCopyBinding(changed, otherBinding) {
			t.Fatal("failed to attach changed binding")
		}
		outcomes, _, conflicts := searchImpactDeduplicateOutcomes([]map[string]any{base, changed})
		if len(outcomes) != 0 || conflicts != 1 {
			t.Fatalf("same-id binding conflict = outcomes=%d conflicts=%d", len(outcomes), conflicts)
		}
	})

	t.Run("different outcomes same binding discard both", func(t *testing.T) {
		otherID := cloneJSONMap(base)
		otherID["outcome_id"] = "binding-outcome-b"
		outcomes, _, conflicts := searchImpactDeduplicateOutcomes([]map[string]any{base, otherID})
		if len(outcomes) != 0 || conflicts != 1 {
			t.Fatalf("cross-id binding conflict = outcomes=%d conflicts=%d", len(outcomes), conflicts)
		}
	})

	t.Run("distinct valid bindings remain separate", func(t *testing.T) {
		otherID := cloneJSONMap(base)
		otherID["outcome_id"] = "binding-outcome-c"
		if !recallResponseCopyBinding(otherID, otherBinding) {
			t.Fatal("failed to attach distinct binding")
		}
		outcomes, _, conflicts := searchImpactDeduplicateOutcomes([]map[string]any{base, otherID})
		if len(outcomes) != 2 || conflicts != 0 {
			t.Fatalf("distinct binding identities = outcomes=%d conflicts=%d", len(outcomes), conflicts)
		}
	})

	t.Run("malformed binding is excluded and never wins", func(t *testing.T) {
		malformed := cloneJSONMap(base)
		delete(malformed, "recall_response_digest")
		outcomes, _, conflicts := searchImpactDeduplicateOutcomes([]map[string]any{malformed, base})
		if len(outcomes) != 0 || conflicts != 1 {
			t.Fatalf("malformed binding identity = outcomes=%d conflicts=%d", len(outcomes), conflicts)
		}
	})

	t.Run("legacy unbound identities retain prior behavior", func(t *testing.T) {
		legacyA := searchImpactEligibleOutcome("legacy-outcome-a", time.Date(2026, time.August, 4, 12, 2, 0, 0, time.UTC), true, false, "verifier-a")
		legacyB := searchImpactEligibleOutcome("legacy-outcome-b", time.Date(2026, time.August, 4, 12, 3, 0, 0, time.UTC), true, false, "verifier-b")
		outcomes, _, conflicts := searchImpactDeduplicateOutcomes([]map[string]any{legacyA, legacyB})
		if len(outcomes) != 2 || conflicts != 0 {
			t.Fatalf("legacy unbound identities changed = outcomes=%d conflicts=%d", len(outcomes), conflicts)
		}
	})
}

func TestSearchImpactReconciledOutcomesCarryOpaqueBindingAndIgnoreRequestScope(t *testing.T) {
	row := evidenceReputationTestOutcome(31, true, "verifier-a")
	row["outcome_id"] = "outcome-request-scope"
	row["sample_id"] = "sample-request-scope"
	row["gateway_received_at"] = "2026-08-04T12:00:00Z"
	row["request_scope"] = map[string]any{"workspace_ref": "sha256:" + strings.Repeat("f", 64), "query": "raw query must not cross"}
	ref := outcomeIntelligenceCandidateRef("request-scope-candidate")
	digest := anyToString(row["verification_evidence_digest"])
	row["evidence_attribution"] = []any{map[string]any{
		"entity_type": "candidate", "entity_id": ref, "candidate_ref": ref,
		"result_level_credit": "selection_receipt_bound", "selection_state": "selected",
		"attribution_method": "counterfactual", "verifier_id": row["verifier_id"],
		"verification_evidence_digest": digest,
	}}
	row["candidate_utility_verification"] = map[string]any{
		"outcome_id": row["outcome_id"], "sample_id": row["sample_id"], "independently_verified": true,
		"verification_status": "verified", "evidence_digest": digest, "verifier_id": row["verifier_id"],
		"observed_yield_eligible": true, "wire_tokens_exact": 90, "model_visible_context_tokens_exact": 72,
	}
	_, binding := contextPackOutcomeResponseBindingFixture(t, "search-impact-request-scope")
	if !recallResponseCopyBinding(row, binding) || !recallResponseCopyBinding(anyMap(row["candidate_utility_verification"]), binding) {
		t.Fatal("failed to attach exact candidate binding")
	}
	outcomes := searchImpactReconciledCandidateOutcomesForWorkspace([]map[string]any{row}, "contextlattice", "coding", "", "sha256:"+strings.Repeat("f", 64))
	if len(outcomes) != 0 {
		t.Fatalf("request_scope workspace decoration influenced exact workspace filtering: %#v", outcomes)
	}
	outcomes = searchImpactReconciledCandidateOutcomesForWorkspace([]map[string]any{row}, "contextlattice", "coding", "", "")
	if len(outcomes) != 1 {
		t.Fatalf("canonical row workspace was unexpectedly excluded: %#v", outcomes)
	}
	encoded, err := json.Marshal(outcomes[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"raw query must not cross", "request_scope", "sha256:" + strings.Repeat("f", 64)} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("search-impact outcome leaked non-canonical scope material %q: %s", forbidden, encoded)
		}
	}
	if got := anyToString(outcomes[0]["response_binding_key"]); got == "" || !utilitySHA256DigestValid(got) {
		t.Fatalf("opaque response binding key missing or malformed: %#v", outcomes[0])
	}
	keyOnly := cloneJSONMap(row)
	for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
		delete(keyOnly, key)
	}
	keyOnly["response_binding_key"] = anyToString(outcomes[0]["response_binding_key"])
	if got := searchImpactReconciledCandidateOutcomesForWorkspace([]map[string]any{keyOnly}, "contextlattice", "coding", "", ""); len(got) != 0 {
		t.Fatalf("digest-only outcome row stood in for a canonical binding: %#v", got)
	}
}

func TestSearchImpactCandidateCausalSummaryRequiresExactUtilityBinding(t *testing.T) {
	baseTime := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	candidateRows := []map[string]any{
		searchImpactEligibleOutcome("causal-control-a", baseTime, true, false, "verifier-a"),
		searchImpactEligibleOutcome("causal-treatment-a", baseTime.Add(time.Minute), true, false, "verifier-b"),
		searchImpactEligibleOutcome("causal-control-b", baseTime.Add(2*time.Minute), true, false, "verifier-a"),
		searchImpactEligibleOutcome("causal-treatment-b", baseTime.Add(3*time.Minute), true, false, "verifier-b"),
	}
	_, binding := contextPackOutcomeResponseBindingFixture(t, "search-impact-causal-binding")
	for _, row := range candidateRows {
		if !recallResponseCopyBinding(row, binding) {
			t.Fatal("failed to attach candidate binding")
		}
	}
	outcomes, _, conflicts := searchImpactDeduplicateOutcomes(candidateRows)
	if conflicts != 3 || len(outcomes) != 0 {
		t.Fatalf("same binding across distinct candidate IDs was not discarded: outcomes=%d conflicts=%d", len(outcomes), conflicts)
	}
	for index, row := range candidateRows {
		candidateBinding := cloneJSONMap(binding)
		marker := string(rune('a' + index))
		candidateBinding["recall_response_id"] = "rr_" + strings.Repeat(marker, 24)
		candidateBinding["recall_response_digest"] = "sha256:" + strings.Repeat(marker, 64)
		if !recallResponseCopyBinding(row, candidateBinding) {
			t.Fatal("failed to attach distinct candidate binding")
		}
	}
	outcomes, _, conflicts = searchImpactDeduplicateOutcomes(candidateRows)
	if conflicts != 0 || len(outcomes) != 4 {
		t.Fatalf("distinct candidate bindings did not remain attributable: outcomes=%d conflicts=%d", len(outcomes), conflicts)
	}
	utilityRows := searchImpactCausalUtilityRows("causal-control-a", "causal-treatment-a", "causal-control-b", "causal-treatment-b")
	for _, row := range utilityRows {
		for _, candidate := range candidateRows {
			if anyToString(row["outcome_id"]) == anyToString(candidate["outcome_id"]) {
				bindingCopy := cloneJSONMap(candidate)
				if !recallResponseCopyBinding(row, bindingCopy) {
					t.Fatal("failed to copy utility binding")
				}
			}
		}
	}
	summary := searchImpactCandidateCausalSummary(outcomes, utilityRows)
	if got := anyToInt(summary["causal_pair_count"], 0); got != 2 {
		t.Fatalf("exact candidate/Utility response bindings produced %d pairs, want 2", got)
	}
	for _, key := range []string{"recall_response_id", "recall_response_digest", "response_component_refs"} {
		delete(utilityRows[0], key)
	}
	utilityRows[0]["response_binding_key"] = outcomes[0].ResponseBindingKey
	summary = searchImpactCandidateCausalSummary(outcomes, utilityRows)
	if got := anyToInt(summary["causal_pair_count"], 0); got != 1 {
		t.Fatalf("digest-only Utility binding was not excluded: causal_pair_count=%d summary=%#v", got, summary)
	}
	delete(utilityRows[0], "response_binding_key")
	mismatchBinding := cloneJSONMap(candidateRows[0])
	mismatchBinding["recall_response_id"] = "rr_" + strings.Repeat("f", 24)
	mismatchBinding["recall_response_digest"] = "sha256:" + strings.Repeat("f", 64)
	if !recallResponseCopyBinding(utilityRows[0], mismatchBinding) {
		t.Fatal("failed to attach mismatched Utility binding")
	}
	summary = searchImpactCandidateCausalSummary(outcomes, utilityRows)
	if got := anyToInt(summary["causal_pair_count"], 0); got != 1 {
		t.Fatalf("mismatched Utility binding was not excluded: causal_pair_count=%d summary=%#v", got, summary)
	}
}

func TestSearchImpactGatewayReceiptTimeConflictAbstainsWithoutEarlierWinner(t *testing.T) {
	input := searchImpactCanaryInput()
	conflict := cloneJSONMap(input.CandidateOutcomes[0])
	conflict["gateway_received_at"] = time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	input.CandidateOutcomes = append(input.CandidateOutcomes, conflict)

	snapshot := buildSearchImpactIntelligence(input)
	if anyToBool(snapshot["canary_eligible"]) {
		t.Fatalf("gateway receipt timestamp conflict recommended a canary: %#v", snapshot["proof_gates"])
	}
	identity := anyMap(anyMap(snapshot["proof_gates"])["outcome_identity_consistency"])
	if got, want := anyToInt(identity["conflict_count"], 0), 1; got != want {
		t.Fatalf("gateway receipt timestamp conflict count = %d, want %d", got, want)
	}
	if got, want := anyToInt(anyMap(snapshot["outcome_intelligence"])["deduplicated_outcome_count"], 0), 9; got != want {
		t.Fatalf("conflicted outcome retained an arbitrary chronological winner: count=%d, want %d", got, want)
	}
}

func TestSearchImpactReceiptLedgerDurabilityGateFailsClosedAndRecovers(t *testing.T) {
	input := searchImpactCanaryInput()
	input.ReceiptLedger = map[string]any{"enabled": false, "durability": "disabled"}
	snapshot := buildSearchImpactIntelligence(input)
	gate := anyMap(anyMap(snapshot["proof_gates"])["receipt_ledger_durability"])
	if anyToBool(snapshot["canary_eligible"]) || anyToBool(gate["pass"]) || anyToString(gate["status"]) != "disabled_or_unavailable" {
		t.Fatalf("disabled receipt ledger did not hard-abstain: %#v", gate)
	}
	if !containsString(anyToStringSlice(snapshot["abstention_reasons"]), "receipt_ledger_durability_unavailable") {
		t.Fatalf("disabled receipt ledger omission was not explicit: %#v", snapshot["abstention_reasons"])
	}

	input.ReceiptLedger = map[string]any{"enabled": true, "durability": "bounded_ndjson", "last_error": "write_failed", "write_errors": 1}
	snapshot = buildSearchImpactIntelligence(input)
	gate = anyMap(anyMap(snapshot["proof_gates"])["receipt_ledger_durability"])
	if anyToBool(gate["pass"]) || anyToString(gate["status"]) != "latest_write_failed" {
		t.Fatalf("latest receipt ledger failure did not hard-abstain: %#v", gate)
	}

	// Historical write errors remain telemetry after a later durable append
	// clears the current failure state.
	input.ReceiptLedger = map[string]any{"enabled": true, "durability": "bounded_ndjson", "last_error": "", "write_errors": 1}
	snapshot = buildSearchImpactIntelligence(input)
	gate = anyMap(anyMap(snapshot["proof_gates"])["receipt_ledger_durability"])
	if !anyToBool(snapshot["canary_eligible"]) || !anyToBool(gate["pass"]) || anyToInt(gate["write_errors"], 0) != 1 {
		t.Fatalf("recovered receipt ledger did not clear the current durability gate: %#v", gate)
	}

	input.ReceiptBinding = map[string]any{"pass": false, "missing_receipt_outcome_count": 1}
	snapshot = buildSearchImpactIntelligence(input)
	gate = anyMap(anyMap(snapshot["proof_gates"])["receipt_ledger_durability"])
	if anyToBool(snapshot["canary_eligible"]) || anyToString(gate["status"]) != "referenced_receipt_missing" || anyToInt(gate["missing_receipt_outcome_count"], 0) != 1 {
		t.Fatalf("missing specific receipt escaped the durability gate: %#v", gate)
	}
}

func TestSearchImpactIntelligenceAllProofGatesCanRecommendButNeverActivate(t *testing.T) {
	input := searchImpactCanaryInput()
	outcomes := input.CandidateOutcomes
	for _, outcome := range outcomes {
		outcome["raw_text"] = "source body must never cross"
		outcome["source_path"] = "/private/nope"
	}
	// The repeated outcome must not change the chronological 80/20 split.
	outcomes = append(outcomes, searchImpactEligibleOutcome("outcome-a", time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC), true, false, "verifier-a"))
	input.CandidateOutcomes = outcomes
	snapshot := buildSearchImpactIntelligence(input)

	if !anyToBool(snapshot["canary_eligible"]) {
		t.Fatalf("all proof gates should recommend an advisory canary: %#v", snapshot["proof_gates"])
	}
	activation := anyMap(snapshot["activation"])
	if anyToBool(activation["performed"]) || anyToBool(activation["entitlement_changed"]) {
		t.Fatalf("impact intelligence may advise but never activate: %#v", activation)
	}
	if got, want := anyToString(snapshot["recommendation"]), "canary_recommended"; got != want {
		t.Fatalf("recommendation = %q, want %q", got, want)
	}
	if got, want := anyToInt(anyMap(snapshot["outcome_intelligence"])["deduplicated_outcome_count"], 0), 10; got != want {
		t.Fatalf("deduplicated_outcome_count = %d, want %d", got, want)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(encoded), "/private/nope") || strings.Contains(string(encoded), "source body must never cross") {
		t.Fatalf("raw text or paths leaked into impact telemetry: %s", encoded)
	}
}

func TestSearchImpactActivationEvidenceBindsExactIntentAndFreshInputs(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	input := searchImpactCanaryInput()
	impact := buildSearchImpactIntelligence(input)
	shadow := cloneJSONMap(input.ComparativeShadow)
	shadow["retrieval_intent_scope_refs"] = []any{savedRecallImpactOpaqueScopeRef("retrieval_intent", "decision")}
	shadow["evaluated_at"] = now.Add(-time.Hour).Format(time.RFC3339Nano)
	outcomes := []map[string]any{{"captured_at": now.Add(-2 * time.Hour).Format(time.RFC3339Nano)}}
	attachSearchImpactActivationEvidence(impact, "contextlattice", "coding", "decision", shadow, outcomes)
	evidence := anyMap(impact["activation_evidence"])
	if anyToString(evidence["retrieval_intent_scope_ref"]) != contextPackLearnedScopeRef("retrieval_intent", "decision") ||
		!isSearchIntelligenceFullSHA256Ref(anyToString(evidence["proof_digest"])) ||
		!isSearchIntelligenceFullSHA256Ref(anyToString(evidence["actuator_comparator_ref"])) {
		t.Fatalf("exact activation evidence was not attached: %#v", evidence)
	}

	missingActuator := cloneJSONMap(impact)
	delete(shadow, "learned_actuator_comparator")
	attachSearchImpactActivationEvidence(missingActuator, "contextlattice", "coding", "decision", shadow, outcomes)
	if _, present := missingActuator["activation_evidence"]; present {
		t.Fatalf("saved-recall comparator authorized the distinct context-pack actuator: %#v", missingActuator["activation_evidence"])
	}

	mixed := cloneJSONMap(impact)
	shadow = cloneJSONMap(input.ComparativeShadow)
	shadow["evaluated_at"] = now.Add(-time.Hour).Format(time.RFC3339Nano)
	shadow["retrieval_intent_scope_refs"] = []any{
		savedRecallImpactOpaqueScopeRef("retrieval_intent", "decision"),
		savedRecallImpactOpaqueScopeRef("retrieval_intent", "exploration"),
	}
	attachSearchImpactActivationEvidence(mixed, "contextlattice", "coding", "decision", shadow, outcomes)
	if _, present := mixed["activation_evidence"]; present {
		t.Fatalf("mixed-intent comparator produced activation evidence: %#v", mixed["activation_evidence"])
	}
}

func TestSearchImpactComparatorEnvelopeAndMetricRejectionsFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing_metric", func(shadow map[string]any) { delete(anyMap(shadow["baseline"]), "mrr") }},
		{"non_finite_metric", func(shadow map[string]any) { anyMap(shadow["shadow"])["mrr"] = math.Inf(1) }},
		{"string_metric", func(shadow map[string]any) { anyMap(shadow["shadow"])["mrr"] = "0.45" }},
		{"bool_metric", func(shadow map[string]any) { anyMap(shadow["shadow"])["mrr"] = true }},
		{"string_safety_rate", func(shadow map[string]any) { anyMap(shadow["shadow"])["safety_failure_rate"] = "0" }},
		{"bool_safety_rate", func(shadow map[string]any) { anyMap(shadow["shadow"])["safety_failure_rate"] = false }},
		{"safety_denominator_mismatch", func(shadow map[string]any) {
			candidate := anyMap(shadow["shadow"])
			candidate["safety_case_count"] = 1
			candidate["safety_failure_count"] = 0
			candidate["safety_failure_rate"] = 0.0
		}},
		{"decision_recall_not_improved", func(shadow map[string]any) { anyMap(shadow["shadow"])["decision_impact_recall_at_5"] = 0.55 }},
		{"decision_ndcg_not_improved", func(shadow map[string]any) { anyMap(shadow["shadow"])["decision_impact_ndcg_at_5"] = 0.50 }},
		{"mrr_regression", func(shadow map[string]any) { anyMap(shadow["shadow"])["mrr"] = 0.44 }},
		{"numeric_regression", func(shadow map[string]any) { anyMap(shadow["shadow"])["numeric_exactness"] = 0.89 }},
		{"citation_coverage_regression", func(shadow map[string]any) { anyMap(shadow["shadow"])["citation_coverage"] = 0.87 }},
		{"citation_exactness_regression", func(shadow map[string]any) { anyMap(shadow["shadow"])["citation_exactness"] = 0.79 }},
		{"safety_regression", func(shadow map[string]any) {
			candidate := anyMap(shadow["shadow"])
			candidate["safety_failure_count"] = 1
			candidate["safety_failure_rate"] = 0.5
		}},
		{"fractional_safety_case_count", func(shadow map[string]any) {
			candidate := anyMap(shadow["shadow"])
			candidate["safety_case_count"] = 2.5
			candidate["safety_failure_count"] = 0
			candidate["safety_failure_rate"] = 0.0
		}},
		{"fractional_safety_failure_count", func(shadow map[string]any) {
			candidate := anyMap(shadow["shadow"])
			candidate["safety_failure_count"] = 0.5
			candidate["safety_failure_rate"] = 0.25
		}},
		{"latency_regression", func(shadow map[string]any) { anyMap(shadow["shadow"])["p95_latency_ms"] = 121.0 }},
		{"version", func(shadow map[string]any) { shadow["version"] = 2 }},
		{"comparison_scope", func(shadow map[string]any) { shadow["comparison_scope"] = "different" }},
		{"fixed_k", func(shadow map[string]any) { shadow["comparison_fixed_k"] = 4 }},
		{"case_count", func(shadow map[string]any) { shadow["case_count"] = 0 }},
		{"case_set_ref", func(shadow map[string]any) { shadow["case_set_ref"] = "" }},
		{"case_set_ref_malformed", func(shadow map[string]any) { shadow["case_set_ref"] = "sha256:" + strings.Repeat("a", 63) }},
		{"latency_basis", func(shadow map[string]any) { shadow["latency_basis"] = "unshared" }},
		{"multiple_project_scope_refs", func(shadow map[string]any) {
			shadow["project_scope_refs"] = []any{savedRecallImpactOpaqueScopeRef("project", "contextlattice"), savedRecallImpactOpaqueScopeRef("project", "other")}
		}},
		{"pool_metadata_mismatch", func(shadow map[string]any) { anyMap(shadow["shadow"])["effective_k_min"] = 4 }},
		{"pool_metadata_insane", func(shadow map[string]any) { anyMap(shadow["baseline"])["effective_k_max"] = 6 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := searchImpactCanaryInput()
			test.mutate(input.ComparativeShadow)
			snapshot := buildSearchImpactIntelligence(input)
			if anyToBool(snapshot["canary_eligible"]) {
				t.Fatalf("%s unexpectedly recommended a canary: %#v", test.name, snapshot["proof_gates"])
			}
		})
	}
}

func TestTelemetrySearchImpactRouteRejectsInvalidAuthorization(t *testing.T) {
	s := &server{orchestratorAPIKey: "impact-test-key"}
	unauthorizedRequest := httptest.NewRequest(http.MethodGet, searchImpactIntelligencePath, nil)
	unauthorizedRequest.Header.Set("X-Api-Key", "wrong-impact-test-key")
	unauthorizedResponse := httptest.NewRecorder()
	s.telemetrySearchImpactRoute(unauthorizedResponse, unauthorizedRequest)
	if got, want := unauthorizedResponse.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("unauthorized status = %d, want %d", got, want)
	}
	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "missing_project", query: "?task_class=general"},
		{name: "missing_task_class", query: "?project=contextlattice"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, searchImpactIntelligencePath+test.query, nil)
			request.Header.Set("X-Api-Key", "impact-test-key")
			response := httptest.NewRecorder()
			s.telemetrySearchImpactRoute(response, request)
			if got, want := response.Code, http.StatusUnprocessableEntity; got != want {
				t.Fatalf("status = %d, want %d: %s", got, want, response.Body.String())
			}
		})
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, searchImpactIntelligencePath+"?project=contextlattice&task_class=general", nil)
	authorizedRequest.Header.Set("X-Api-Key", "impact-test-key")
	authorizedResponse := httptest.NewRecorder()
	s.telemetrySearchImpactRoute(authorizedResponse, authorizedRequest)
	if got, want := authorizedResponse.Code, http.StatusOK; got != want {
		t.Fatalf("authorized status = %d, want %d: %s", got, want, authorizedResponse.Body.String())
	}
	payload := map[string]any{}
	if err := json.Unmarshal(authorizedResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode authorized response: %v", err)
	}
	if got, want := anyToString(payload["schema_id"]), searchImpactIntelligenceContractID; got != want {
		t.Fatalf("schema_id = %q, want %q", got, want)
	}
	filters := anyMap(payload["filters"])
	exactScope := anyMap(filters["exact_scope"])
	if !anyToBool(exactScope["project_filter_applied"]) || !anyToBool(exactScope["task_class_filter_applied"]) {
		t.Fatalf("exact project/task scope was not marked as applied: %#v", filters)
	}
	if got, want := strings.Join(anyToStringSlice(exactScope["applies_to"]), ","), "receipt_bound_candidate_outcomes,candidate_causal_gate,comparative_shadow"; got != want {
		t.Fatalf("exact scope applies_to = %q, want %q", got, want)
	}
	if got, want := strings.Join(anyToStringSlice(filters["global_contextual_non_gating"]), ","), "token_impact"; got != want {
		t.Fatalf("global contextual cohorts = %q, want %q", got, want)
	}
	tokenImpact := anyMap(anyMap(payload["impact_intelligence"])["token_impact"])
	if got, want := anyToString(tokenImpact["scope"]), "global_contextual_non_gating"; got != want || !anyToBool(tokenImpact["non_gating"]) {
		t.Fatalf("token impact scope falsely claimed exact cohort membership: %#v", tokenImpact)
	}
}

func TestLatestSearchImpactShadowEvaluationUsesNewestExactScope(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	projectRef := savedRecallImpactOpaqueScopeRef("project", "contextlattice")
	taskClassRef := savedRecallImpactOpaqueScopeRef("task_class", "coding")
	otherProjectRef := savedRecallImpactOpaqueScopeRef("project", "other")
	rows := []map[string]any{
		{
			"schema_id": savedRecallImpactShadowEvalSchemaID, "comparison_valid": true,
			"project_scope_refs": []any{projectRef}, "task_class_scope_refs": []any{taskClassRef},
		},
		{
			"schema_id": savedRecallImpactShadowEvalSchemaID, "comparison_valid": false,
			"comparison_reason":  "safety_cases_missing",
			"project_scope_refs": []any{projectRef}, "task_class_scope_refs": []any{taskClassRef},
		},
		{
			"schema_id": savedRecallImpactShadowEvalSchemaID, "comparison_valid": true,
			"project_scope_refs": []any{otherProjectRef}, "task_class_scope_refs": []any{taskClassRef},
		},
		{
			"search_impact_shadow_evaluations": []any{map[string]any{
				"schema_id": savedRecallImpactShadowEvalSchemaID, "comparison_valid": false,
				"comparison_reason": "case_set_invalid", "project_scope_refs": []any{projectRef}, "task_class_scope_refs": []any{taskClassRef},
			}},
		},
	}
	encoded := make([]byte, 0)
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, raw...)
		encoded = append(encoded, '\n')
	}
	if err := os.WriteFile(monitorPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		t.Fatalf("load recall monitor shadow index: %v", err)
	}
	selected := s.latestSearchImpactShadowEvaluation("ContextLattice", "Coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "case_set_invalid" {
		t.Fatalf("newest exact-scope invalid artifact did not supersede older valid evidence: %#v", selected)
	}
	if status := anyToString(searchImpactShadowEvaluation(selected)["status"]); status != "shadow_eval_case_set_invalid" {
		t.Fatalf("invalid reason was not preserved: %q", status)
	}
	unfiltered := s.latestSearchImpactShadowEvaluation("", "")
	if anyToString(unfiltered["comparison_reason"]) != "scope_mismatch" {
		t.Fatalf("unfiltered scope should fail closed: %#v", unfiltered)
	}
}

func TestLatestSearchImpactShadowEvaluationRejectsPersistedPassAfterComparatorFailure(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	valid := searchImpactValidComparativeShadow()
	raw, err := json.Marshal(map[string]any{"search_impact_shadow_evaluations": []any{valid}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(monitorPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		t.Fatalf("load recall monitor shadow index: %v", err)
	}
	s.recordSearchImpactComparatorPersistence(false)
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_persistence_unavailable" {
		t.Fatalf("persisted comparator pass was reused after append failure: %#v", selected)
	}
	if anyToBool(searchImpactShadowEvaluation(selected)["pass"]) {
		t.Fatalf("persistence-unavailable comparator result passed the impact gate: %#v", selected)
	}
}

func TestRecallMonitorShadowIndexSeparatesExactWorkspacesAndRejectsLegacyLookup(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	workspaceA := contextPackLearnedScopeRef("workspace", "workspace-a")
	workspaceB := contextPackLearnedScopeRef("workspace", "workspace-b")
	artifactA := searchImpactValidComparativeShadow()
	artifactA["workspace_ref"] = workspaceA
	artifactA["case_set_ref"] = "sha256:" + sha256Hex("workspace-a-cases")
	artifactB := searchImpactValidComparativeShadow()
	artifactB["workspace_ref"] = workspaceB
	artifactB["case_set_ref"] = "sha256:" + sha256Hex("workspace-b-cases")
	raw, err := json.Marshal(map[string]any{"search_impact_shadow_evaluations": []any{artifactA, artifactB}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(monitorPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		t.Fatalf("load workspace-aware recall monitor shadow index: %v", err)
	}
	if selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding"); anyToString(selected["comparison_reason"]) != "scope_mismatch" {
		t.Fatalf("legacy unscoped lookup reused workspace evidence: %#v", selected)
	}
	if selected := s.latestSearchImpactShadowEvaluationForWorkspace("contextlattice", "coding", workspaceA); anyToString(selected["case_set_ref"]) != anyToString(artifactA["case_set_ref"]) {
		t.Fatalf("workspace A selected the wrong comparator: %#v", selected)
	}
	if selected := s.latestSearchImpactShadowEvaluationForWorkspace("contextlattice", "coding", workspaceB); anyToString(selected["case_set_ref"]) != anyToString(artifactB["case_set_ref"]) {
		t.Fatalf("workspace B selected the wrong comparator: %#v", selected)
	}
	if selected := s.latestSearchImpactShadowEvaluationForWorkspace("contextlattice", "coding", contextPackLearnedScopeRef("workspace", "workspace-c")); anyToString(selected["comparison_reason"]) != "scope_mismatch" {
		t.Fatalf("unknown workspace reused another comparator: %#v", selected)
	}
}

func TestReadRecallMonitorHistoryReadsNewestBoundedRowsAfterOversizedHistory(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	overlong := "{\"discard\":\"" + strings.Repeat("x", recallMonitorHistoryMaxLineBytes+1) + "\"}\n"
	content := overlong + "{\"sequence\":1}\n{\"sequence\":2}\n{\"unfinished\":true}"
	if err := os.WriteFile(monitorPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	rows := (&server{}).readRecallMonitorHistory(2)
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count = %d, want newest two rows", got)
	}
	if got, want := anyToInt(rows[0]["sequence"], 0), 1; got != want {
		t.Fatalf("first retained sequence = %d, want %d", got, want)
	}
	if got, want := anyToInt(rows[1]["sequence"], 0), 2; got != want {
		t.Fatalf("second retained sequence = %d, want %d", got, want)
	}
}

func TestRecallMonitorShadowIndexRefreshesAfterDurableAppend(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		t.Fatalf("load empty recall monitor shadow index: %v", err)
	}
	if selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding"); selected != nil {
		t.Fatalf("empty monitor selected comparator evidence: %#v", selected)
	}
	if err := s.appendRecallMonitorSample(map[string]any{
		"search_impact_shadow_evaluations": []any{searchImpactValidComparativeShadow()},
	}); err != nil {
		t.Fatalf("durable append and index refresh: %v", err)
	}
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if !anyToBool(selected["comparison_valid"]) {
		t.Fatalf("durable append did not refresh exact-scope index: %#v", selected)
	}
}

func TestRecallMonitorShadowIndexFailsClosedAfterExternalChange(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	raw, err := json.Marshal(map[string]any{
		"search_impact_shadow_evaluations": []any{searchImpactValidComparativeShadow()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(monitorPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		t.Fatalf("load recall monitor shadow index: %v", err)
	}
	if selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding"); !anyToBool(selected["comparison_valid"]) {
		t.Fatalf("startup index did not select comparator evidence: %#v", selected)
	}
	file, err := os.OpenFile(monitorPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"external_writer\":true}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_index_stale" {
		t.Fatalf("external change did not invalidate the startup index: %#v", selected)
	}
	if status := anyToString(searchImpactShadowEvaluation(selected)["status"]); status != "shadow_eval_comparator_index_stale" {
		t.Fatalf("stale index was not surfaced as fail-closed evidence: %q", status)
	}
}

func TestRecallMonitorShadowIndexFailsClosedOnMalformedCompleteRow(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	valid, err := json.Marshal(map[string]any{
		"search_impact_shadow_evaluations": []any{searchImpactValidComparativeShadow()},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := append(valid, '\n')
	content = append(content, []byte("{\"malformed\":}\n")...)
	if err := os.WriteFile(monitorPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err == nil {
		t.Fatal("malformed newest complete monitor row unexpectedly populated the activation index")
	}
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_index_unavailable" {
		t.Fatalf("malformed newest row left older comparator eligible: %#v", selected)
	}
}

func TestRecallMonitorShadowIndexFailsClosedOnTrailingPartialRow(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	valid, err := json.Marshal(map[string]any{
		"search_impact_shadow_evaluations": []any{searchImpactValidComparativeShadow()},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := append(valid, '\n')
	content = append(content, []byte("{\"crash_residue\":")...)
	if err := os.WriteFile(monitorPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err == nil {
		t.Fatal("trailing partial monitor row unexpectedly populated the activation index")
	}
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_index_unavailable" {
		t.Fatalf("trailing partial row left older comparator eligible: %#v", selected)
	}
}

func TestRecallMonitorShadowIndexFailsClosedOnOversizedCompleteRow(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	valid, err := json.Marshal(map[string]any{
		"search_impact_shadow_evaluations": []any{searchImpactValidComparativeShadow()},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := append(valid, '\n')
	content = append(content, []byte("{\"padding\":\""+strings.Repeat("x", recallMonitorHistoryMaxLineBytes+1)+"\"}\n")...)
	if err := os.WriteFile(monitorPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err == nil {
		t.Fatal("oversized newest complete monitor row unexpectedly populated the activation index")
	}
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_index_unavailable" {
		t.Fatalf("oversized newest row left older comparator eligible: %#v", selected)
	}
}

func BenchmarkLatestSearchImpactShadowEvaluationFromStartupIndex(b *testing.B) {
	monitorPath := filepath.Join(b.TempDir(), "recall-monitor.ndjson")
	b.Setenv("RECALL_MONITOR_PATH", monitorPath)
	ordinary, err := json.Marshal(map[string]any{"padding": strings.Repeat("x", 1100)})
	if err != nil {
		b.Fatal(err)
	}
	content := make([]byte, 0, (len(ordinary)+1)*1201)
	for index := 0; index < 1200; index++ {
		content = append(content, ordinary...)
		content = append(content, '\n')
	}
	artifact, err := json.Marshal(map[string]any{
		"search_impact_shadow_evaluations": []any{searchImpactValidComparativeShadow()},
	})
	if err != nil {
		b.Fatal(err)
	}
	content = append(content, artifact...)
	content = append(content, '\n')
	if err := os.WriteFile(monitorPath, content, 0o600); err != nil {
		b.Fatal(err)
	}
	s := &server{}
	if err := s.loadRecallMonitorShadowIndex(); err != nil {
		b.Fatalf("load recall monitor shadow index: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
		if !anyToBool(selected["comparison_valid"]) {
			b.Fatalf("startup index lost exact comparator evidence: %#v", selected)
		}
	}
}
