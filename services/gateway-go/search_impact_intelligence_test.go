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
	return map[string]any{
		"schema_id":             savedRecallImpactShadowEvalSchemaID,
		"version":               1,
		"comparison_scope":      savedRecallImpactComparisonScope,
		"comparison_fixed_k":    savedRecallImpactK,
		"comparison_valid":      true,
		"comparison_reason":     "valid",
		"case_count":            10,
		"case_set_ref":          "sha256:" + sha256Hex("impact-case-set"),
		"project_scope_refs":    []any{savedRecallImpactOpaqueScopeRef("project", "contextlattice")},
		"task_class_scope_refs": []any{savedRecallImpactOpaqueScopeRef("task_class", "coding")},
		"latency_basis":         "shared_synthetic_retrieval_replay_ms",
		"baseline":              searchImpactValidMetrics(0.55, 0.50, 0.45, 0.90, 0.88, 0.80, 0, 120),
		"shadow":                searchImpactValidMetrics(0.70, 0.66, 0.45, 0.90, 0.88, 0.80, 0, 120),
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
	s.recordSearchImpactComparatorPersistence(false)
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_persistence_unavailable" {
		t.Fatalf("persisted comparator pass was reused after append failure: %#v", selected)
	}
	if anyToBool(searchImpactShadowEvaluation(selected)["pass"]) {
		t.Fatalf("persistence-unavailable comparator result passed the impact gate: %#v", selected)
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
