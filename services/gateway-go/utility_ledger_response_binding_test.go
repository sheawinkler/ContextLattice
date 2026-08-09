package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func utilityResponseBindingFixture(t *testing.T, outcomeID, sampleID, sessionID string) (map[string]any, map[string]any, map[string]any, []map[string]any, map[string]any) {
	t.Helper()
	outcome, quality, impact, events := utilityTestFixture(outcomeID, sampleID, sessionID, "coding", "contextlattice", 8, 400, nil)
	response := composeRecallResponse(recallResponseTestInput(true))
	binding, ok := recallResponseBindingFromResponse(response)
	if !ok {
		t.Fatalf("response fixture did not produce a canonical binding: %#v", response)
	}
	if !recallResponseCopyBinding(outcome, binding) || !recallResponseCopyBinding(quality, binding) {
		t.Fatal("failed to attach canonical response binding to outcome and quality fixtures")
	}
	return outcome, quality, impact, events, binding
}

func utilityResponseBindingReasons(row map[string]any) []string {
	return anyToStringList(anyMap(row["eligibility"])["exclusion_reasons"], 32)
}

func TestUtilityObservationCopiesCanonicalResponseBindingAndKeepsExactEligibility(t *testing.T) {
	outcome, quality, impact, events, binding := utilityResponseBindingFixture(t, "utility_bound_observation", "utility_bound_sample", "utility_bound_session")
	row := buildUtilityObservation(outcome, quality, impact, events)
	if !recallResponseBindingsEqual(row, binding) {
		t.Fatalf("utility observation did not copy complete canonical binding: row=%#v binding=%#v", row, binding)
	}
	if !reflect.DeepEqual(row["response_component_refs"], binding["response_component_refs"]) {
		t.Fatalf("utility observation changed response component refs: got=%#v want=%#v", row["response_component_refs"], binding["response_component_refs"])
	}
	if !anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) || anyToBool(anyMap(row["eligibility"])["causal_gain_eligible"]) {
		t.Fatalf("exact matching bound rows did not retain baseline observed/causal posture: %#v", row["eligibility"])
	}
	if anyToString(row["workspace_ref"]) != "" {
		t.Fatalf("fixture unexpectedly inferred a workspace from recall response scope: %#v", row["workspace_ref"])
	}
	for _, leaked := range []string{"query", "prompt", "content", "request_scope", "answer"} {
		if _, present := row[leaked]; present {
			t.Fatalf("raw/internal response field leaked into Utility observation: %s", leaked)
		}
	}
}

func TestUtilityBoundExactPairRetainsCausalEligibility(t *testing.T) {
	matchDigest := utilityTestDigest("utility-bound-pair")
	controlOutcome, controlQuality, controlImpact, controlEvents := utilityTestFixture(
		"utility_bound_control", "utility_bound_control_sample", "utility_bound_control_session", "coding", "contextlattice", 4, 400,
		map[string]any{"pair_id": "utility-bound-pair", "arm": "control", "task_match_digest": matchDigest, "matching_method": "exact_holdout", "leakage_free": true},
	)
	treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents := utilityTestFixture(
		"utility_bound_treatment", "utility_bound_treatment_sample", "utility_bound_treatment_session", "coding", "contextlattice", 8, 400,
		map[string]any{"pair_id": "utility-bound-pair", "arm": "treatment", "matched_control_outcome_id": "utility_bound_control", "task_match_digest": matchDigest, "matching_method": "exact_holdout", "leakage_free": true},
	)
	controlResponse := composeRecallResponse(recallResponseTestInput(true))
	controlBinding, controlOK := recallResponseBindingFromResponse(controlResponse)
	treatmentResponse := composeRecallResponse(recallResponseTestInput(false))
	treatmentBinding, treatmentOK := recallResponseBindingFromResponse(treatmentResponse)
	if !controlOK || !treatmentOK || recallResponseBindingKey(controlBinding) == recallResponseBindingKey(treatmentBinding) {
		t.Fatal("bound pair fixtures did not produce distinct canonical bindings")
	}
	for _, fixture := range []struct {
		sample  map[string]any
		binding map[string]any
	}{
		{controlOutcome, controlBinding}, {controlQuality, controlBinding},
		{treatmentOutcome, treatmentBinding}, {treatmentQuality, treatmentBinding},
	} {
		if !recallResponseCopyBinding(fixture.sample, fixture.binding) {
			t.Fatal("failed to attach bound pair response binding")
		}
	}
	rows, pairs, exclusions := utilityPairProjection([]map[string]any{
		buildUtilityObservation(controlOutcome, controlQuality, controlImpact, controlEvents),
		buildUtilityObservation(treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents),
	})
	if len(pairs) != 1 || len(exclusions) != 0 || !anyToBool(anyMap(rows[1]["eligibility"])["causal_gain_eligible"]) {
		t.Fatalf("exactly bound pair did not retain causal eligibility: rows=%#v pairs=%#v exclusions=%#v", rows, pairs, exclusions)
	}

	if !recallResponseCopyBinding(treatmentOutcome, controlBinding) || !recallResponseCopyBinding(treatmentQuality, controlBinding) {
		t.Fatal("failed to construct reused response binding pair")
	}
	rows, pairs, exclusions = utilityPairProjection([]map[string]any{
		buildUtilityObservation(controlOutcome, controlQuality, controlImpact, controlEvents),
		buildUtilityObservation(treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents),
	})
	if len(pairs) != 0 || exclusions["response_binding_reused_across_pair"] != 1 || anyToBool(anyMap(rows[1]["eligibility"])["causal_gain_eligible"]) {
		t.Fatalf("reused response identity became a causal pair: rows=%#v pairs=%#v exclusions=%#v", rows, pairs, exclusions)
	}
}

func TestUtilityObservationResponseBindingEligibilityIsExact(t *testing.T) {
	tests := []struct {
		name          string
		mutateOutcome func(map[string]any, map[string]any)
		wantReason    string
	}{
		{
			name: "outcome_bound_quality_legacy",
			mutateOutcome: func(outcome, quality map[string]any) {
				delete(quality, "recall_response_id")
				delete(quality, "recall_response_digest")
				delete(quality, "response_component_refs")
			},
			wantReason: "response_binding_missing",
		},
		{
			name: "quality_bound_outcome_legacy",
			mutateOutcome: func(outcome, quality map[string]any) {
				delete(outcome, "recall_response_id")
				delete(outcome, "recall_response_digest")
				delete(outcome, "response_component_refs")
			},
			wantReason: "response_binding_missing",
		},
		{
			name: "outcome_partial",
			mutateOutcome: func(outcome, quality map[string]any) {
				delete(outcome, "recall_response_digest")
			},
			wantReason: "response_binding_invalid",
		},
		{
			name: "quality_malformed",
			mutateOutcome: func(outcome, quality map[string]any) {
				quality["recall_response_digest"] = "not-a-digest"
			},
			wantReason: "response_binding_invalid",
		},
		{
			name: "valid_mismatch",
			mutateOutcome: func(outcome, quality map[string]any) {
				otherResponse := composeRecallResponse(recallResponseTestInput(false))
				otherBinding, ok := recallResponseBindingFromResponse(otherResponse)
				if !ok || !recallResponseCopyBinding(quality, otherBinding) {
					t.Fatalf("failed to construct mismatch binding")
				}
			},
			wantReason: "response_binding_mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, quality, impact, events, _ := utilityResponseBindingFixture(t, "utility_binding_"+tt.name, "utility_binding_sample_"+tt.name, "utility_binding_session_"+tt.name)
			tt.mutateOutcome(outcome, quality)
			row := buildUtilityObservation(outcome, quality, impact, events)
			if anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) ||
				anyToBool(anyMap(row["eligibility"])["causal_gain_eligible"]) ||
				anyToString(row["status"]) != "excluded" {
				t.Fatalf("inexact response binding became eligible: %#v", row)
			}
			if !containsString(utilityResponseBindingReasons(row), tt.wantReason) {
				t.Fatalf("missing bounded response binding exclusion %q: %#v", tt.wantReason, row["eligibility"])
			}
		})
	}
}

func TestUtilityObservationDoesNotRequireTokenImpactResponseBinding(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("utility_impact_unbound", "utility_impact_sample", "utility_impact_session", "coding", "contextlattice", 8, 400, nil)
	impact["recall_response_id"] = "rr_invalid_impact_only"
	impact["recall_response_digest"] = "not-a-digest"
	row := buildUtilityObservation(outcome, quality, impact, events)
	if !anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) {
		t.Fatalf("token-impact response fields must not gate legacy Utility eligibility: %#v", row)
	}
	if recallResponseBindingHasAnyFields(row) {
		t.Fatalf("token-impact response fields were copied into Utility observation: %#v", row)
	}
}

func TestUtilityWorkspaceComesFromCanonicalOutcomeQualityChain(t *testing.T) {
	outcome, quality, impact, events := utilityTestFixture("utility_workspace_chain", "utility_workspace_sample", "utility_workspace_session", "coding", "contextlattice", 8, 400, nil)
	workspaceRef := contextPackLearnedScopeRef("workspace", "utility-canonical-workspace")
	quality["workspace_ref"] = workspaceRef
	delete(outcome, "workspace_ref")
	outcome["request_scope"] = map[string]any{"workspace_ref": contextPackLearnedScopeRef("workspace", "utility-request-workspace")}
	row := buildUtilityObservation(outcome, quality, impact, events)
	if anyToString(row["workspace_ref"]) != workspaceRef {
		t.Fatalf("Utility workspace was not sourced from the canonical quality chain: got=%q want=%q row=%#v", row["workspace_ref"], workspaceRef, row)
	}
	if anyToString(row["workspace_ref"]) == anyToString(anyMap(outcome["request_scope"])["workspace_ref"]) {
		t.Fatal("Utility inferred workspace from recall request scope")
	}
}

func TestUtilitySourceClaimDigestIncludesBindingButIgnoresTimestampRetry(t *testing.T) {
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", filepath.Join(t.TempDir(), "utility.ndjson"))
	outcome, quality, impact, events, _ := utilityResponseBindingFixture(t, "utility_digest_binding", "utility_digest_sample", "utility_digest_session")
	changedBinding := cloneAnyMap(outcome)
	otherResponse := composeRecallResponse(recallResponseTestInput(false))
	otherBinding, ok := recallResponseBindingFromResponse(otherResponse)
	if !ok || !recallResponseCopyBinding(changedBinding, otherBinding) {
		t.Fatal("failed to construct changed binding outcome")
	}
	if utilitySourceClaimDigest(outcome) == utilitySourceClaimDigest(changedBinding) {
		t.Fatal("source claim digest ignored changed response binding")
	}
	timestampRetry := cloneAnyMap(outcome)
	timestampRetry["capturedAt"] = "2026-08-08T23:59:59Z"
	timestampRetry["gateway_received_at"] = "2026-08-08T23:59:59Z"
	if utilitySourceClaimDigest(outcome) != utilitySourceClaimDigest(timestampRetry) {
		t.Fatal("source claim digest changed for timestamp-only retry")
	}

	telemetry := newUtilityTestTelemetry(t, 20)
	first := buildUtilityObservation(outcome, quality, impact, events)
	if _, recorded, err := telemetry.record(first); err != nil || !recorded {
		t.Fatalf("record bound source claim: recorded=%t err=%v", recorded, err)
	}
	retry := buildUtilityObservation(timestampRetry, quality, impact, events)
	if _, recorded, err := telemetry.record(retry); err != nil || recorded {
		t.Fatalf("timestamp-only binding retry was not replay: recorded=%t err=%v", recorded, err)
	}
	conflict := buildUtilityObservation(changedBinding, quality, impact, events)
	if _, recorded, err := telemetry.record(conflict); !errors.Is(err, errUtilityOutcomeConflict) || recorded {
		t.Fatalf("changed binding did not conflict durably: recorded=%t err=%v", recorded, err)
	}
	unchangedDigestBindingConflict := cloneAnyMap(first)
	if !recallResponseCopyBinding(unchangedDigestBindingConflict, otherBinding) {
		t.Fatal("failed to mutate Utility row binding without changing its source digest")
	}
	if _, recorded, err := telemetry.record(unchangedDigestBindingConflict); !errors.Is(err, errUtilityOutcomeConflict) || recorded {
		t.Fatalf("durable Utility store accepted changed binding with a stale source digest: recorded=%t err=%v", recorded, err)
	}
}

func TestUtilityBoundResponseBindingSurvivesRestartCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", path)
	outcome, quality, impact, events, binding := utilityResponseBindingFixture(t, "utility_restart_bound", "utility_restart_sample", "utility_restart_session")
	row := buildUtilityObservation(outcome, quality, impact, events)
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal bound Utility row: %v", err)
	}
	if err := os.WriteFile(path, append(append([]byte{}, raw...), '\n', 'n', 'o', 't', '-', 'j', 's', 'o', 'n', '\n'), 0o600); err != nil {
		t.Fatalf("seed bound row and malformed physical line: %v", err)
	}
	reloaded := newUtilityTestTelemetry(t, 20)
	persisted, ok := reloaded.observation("utility_restart_bound")
	if !ok || !recallResponseBindingsEqual(persisted, binding) {
		t.Fatalf("restart/compaction lost valid response binding: row=%#v", persisted)
	}
	if anyToBool(anyMap(persisted["eligibility"])["observed_yield_eligible"]) != anyToBool(anyMap(row["eligibility"])["observed_yield_eligible"]) {
		t.Fatalf("restart changed bound eligibility: before=%#v after=%#v", row["eligibility"], persisted["eligibility"])
	}
	storage := utilityStorageStatus(reloaded.store)
	if anyToInt(storage["parse_errors"], 0) != 1 || anyToInt(storage["physical_rows"], 0) != 1 {
		t.Fatalf("malformed physical row was not compacted with bound row retained: %#v", storage)
	}
}

func TestUtilityMalformedPersistedResponseBindingFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utility.ndjson")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_UTILITY_LEDGER_PATH", path)
	malformed := map[string]any{
		"schema_id": utilityObservationContractID, "version": 1, "revision": 1,
		"outcome_id": "utility_malformed_persisted", "source_claim_digest": utilityTestDigest("utility_malformed_persisted"),
		"recall_response_id": "rr_partial_only", "status": "verified_exact",
		"eligibility": map[string]any{"observed_yield_eligible": true, "causal_gain_eligible": true},
	}
	raw, err := json.Marshal(malformed)
	if err != nil {
		t.Fatalf("marshal malformed Utility row: %v", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("seed malformed Utility row: %v", err)
	}
	reloaded := newUtilityTestTelemetry(t, 20)
	if _, ok := reloaded.observation("utility_malformed_persisted"); ok {
		t.Fatal("malformed response-bound Utility row became resident")
	}
	if _, _, err := reloaded.store.append(malformed); !errors.Is(err, errUtilityInvalidResponseBinding) {
		t.Fatalf("malformed response-bound Utility append was not rejected: %v", err)
	}
	if rows := reloaded.rows(utilityQuery{}); len(rows) != 0 {
		t.Fatalf("malformed response-bound Utility row became eligible legacy evidence: %#v", rows)
	}
	storage := utilityStorageStatus(reloaded.store)
	if anyToInt(storage["parse_errors"], 0) != 1 || anyToInt(storage["physical_rows"], 0) != 0 {
		t.Fatalf("malformed response-bound row was not fail-closed/compacted: %#v", storage)
	}
	if strings.Contains(string(raw), "raw query") {
		t.Fatal("test fixture unexpectedly contains raw response content")
	}
}
