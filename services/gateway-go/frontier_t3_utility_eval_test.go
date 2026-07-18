package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	frontierT3UtilityHoldoutID       = "frontier_t3_utility_adversarial_v1"
	frontierT3UtilityHoldoutSHA256   = "d541dff3d6091a2720bd5b4059b8467f8d198a39ea4ab62cdad519152a8a9cc7"
	frontierT3UtilityBaselineSHA256  = "be49cb2c2b06e5d62a51dbdb3467b288391c3e3fba0218171c391c7d96367f01"
	frontierT3UtilityExpectedCaseNum = 13
)

type frontierT3UtilityHoldoutCase struct {
	CaseID             string         `json:"case_id"`
	Kind               string         `json:"kind"`
	Utility            float64        `json:"utility"`
	ControlUtility     float64        `json:"control_utility"`
	TreatmentUtility   float64        `json:"treatment_utility"`
	ModelVisibleTokens int            `json:"model_visible_tokens"`
	Expected           map[string]any `json:"expected"`
}

type frontierT3UtilityHoldout struct {
	SchemaID                string                         `json:"schema_id"`
	HoldoutID               string                         `json:"holdout_id"`
	IndependentFromTraining bool                           `json:"independent_from_training"`
	Cases                   []frontierT3UtilityHoldoutCase `json:"cases"`
	AggregateExpectation    map[string]any                 `json:"aggregate_expectation"`
}

func frontierT3LoadUtilityHoldout(t *testing.T) frontierT3UtilityHoldout {
	t.Helper()
	baselinePath := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t3-utility-baseline.v1.json")
	baseline, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read frozen utility baseline: %v", err)
	}
	baselineDigest := sha256.Sum256(baseline)
	if got := hex.EncodeToString(baselineDigest[:]); got != frontierT3UtilityBaselineSHA256 {
		t.Fatalf("utility baseline sha256=%s want=%s", got, frontierT3UtilityBaselineSHA256)
	}
	path := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t3-utility-holdout.v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frozen utility holdout: %v", err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != frontierT3UtilityHoldoutSHA256 {
		t.Fatalf("utility holdout sha256=%s want=%s", got, frontierT3UtilityHoldoutSHA256)
	}
	fixture := frontierT3UtilityHoldout{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode frozen utility holdout: %v", err)
	}
	if fixture.SchemaID != "frontier_t3_utility_holdout.v1" || fixture.HoldoutID != frontierT3UtilityHoldoutID || !fixture.IndependentFromTraining || len(fixture.Cases) != frontierT3UtilityExpectedCaseNum {
		t.Fatalf("unexpected utility holdout identity: schema=%q holdout=%q independent=%t cases=%d", fixture.SchemaID, fixture.HoldoutID, fixture.IndependentFromTraining, len(fixture.Cases))
	}
	seen := map[string]struct{}{}
	for _, row := range fixture.Cases {
		if strings.TrimSpace(row.CaseID) == "" {
			t.Fatal("utility holdout contains an empty case_id")
		}
		if _, exists := seen[row.CaseID]; exists {
			t.Fatalf("utility holdout contains duplicate case_id=%q", row.CaseID)
		}
		seen[row.CaseID] = struct{}{}
	}
	return fixture
}

func frontierT3UtilityPairRows(row frontierT3UtilityHoldoutCase) []map[string]any {
	digest := utilityTestDigest("task:" + row.CaseID)
	treatmentDigest := digest
	leakageFree := true
	controlClass, treatmentClass := "coding", "coding"
	switch row.Kind {
	case "leaky_pair":
		leakageFree = false
	case "digest_mismatch_pair":
		treatmentDigest = utilityTestDigest("different-task:" + row.CaseID)
	case "task_class_mismatch_pair":
		treatmentClass = "review"
	}
	controlID := "control_" + row.CaseID
	controlOutcome, controlQuality, controlImpact, controlEvents := utilityTestFixture(
		controlID, "sample_control_"+row.CaseID, "session_control_"+row.CaseID, controlClass, "contextlattice", row.ControlUtility, row.ModelVisibleTokens,
		map[string]any{"pair_id": row.CaseID, "arm": "control", "task_match_digest": digest, "matching_method": "exact_holdout", "leakage_free": true},
	)
	treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents := utilityTestFixture(
		"treatment_"+row.CaseID, "sample_treatment_"+row.CaseID, "session_treatment_"+row.CaseID, treatmentClass, "contextlattice", row.TreatmentUtility, row.ModelVisibleTokens,
		map[string]any{"pair_id": row.CaseID, "arm": "treatment", "matched_control_outcome_id": controlID, "task_match_digest": treatmentDigest, "matching_method": "exact_holdout", "leakage_free": leakageFree},
	)
	if row.Kind == "model_mismatch_pair" {
		anyMap(treatmentOutcome["pairing"])["model"] = "different-model"
	}
	return []map[string]any{
		buildUtilityObservation(controlOutcome, controlQuality, controlImpact, controlEvents),
		buildUtilityObservation(treatmentOutcome, treatmentQuality, treatmentImpact, treatmentEvents),
	}
}

func frontierT3UtilitySingleRow(row frontierT3UtilityHoldoutCase) map[string]any {
	pairing := map[string]any(nil)
	if row.Kind == "no_control" {
		pairing = map[string]any{
			"pair_id": row.CaseID, "arm": "treatment", "task_match_digest": utilityTestDigest("task:" + row.CaseID),
			"matching_method": "exact_holdout", "leakage_free": true,
		}
	}
	outcome, quality, impact, events := utilityTestFixture("outcome_"+row.CaseID, "sample_"+row.CaseID, "session_"+row.CaseID, "coding", "contextlattice", row.Utility, row.ModelVisibleTokens, pairing)
	switch row.Kind {
	case "inexact_tokens":
		impact["tokenizer_exact"] = false
	case "missing_verification":
		events = nil
	case "failed_verification":
		anyMap(outcome["utility"])["verification_passed"] = false
		anyMap(anyMap(events[0]["metadata"])["utility_verification"])["verification_passed"] = false
	case "self_verification":
		anyMap(outcome["utility"])["verifier_id"] = "codex_test"
		anyMap(anyMap(events[0]["metadata"])["utility_verification"])["verifier_id"] = "codex_test"
		events[0]["agent_id"] = "codex_test"
	case "source_identity_mismatch":
		impact["agent_id"] = "foreign_agent"
	}
	return buildUtilityObservation(outcome, quality, impact, events)
}

func TestFrontierT3UtilityFrozenHoldout(t *testing.T) {
	fixture := frontierT3LoadUtilityHoldout(t)
	allRows := []map[string]any{}
	correct := 0
	for _, row := range fixture.Cases {
		row := row
		passed := t.Run(row.CaseID, func(t *testing.T) {
			var rows []map[string]any
			switch row.Kind {
			case "matched_pair", "leaky_pair", "digest_mismatch_pair", "task_class_mismatch_pair", "model_mismatch_pair":
				rows = frontierT3UtilityPairRows(row)
			default:
				rows = []map[string]any{frontierT3UtilitySingleRow(row)}
			}
			projected, pairs, pairExclusions := utilityPairProjection(rows)
			observedEligible := 0
			for _, observation := range projected {
				if anyToBool(anyMap(observation["eligibility"])["observed_yield_eligible"]) {
					observedEligible++
				}
			}
			if observedEligible != anyToInt(row.Expected["observed_eligible"], 0) || len(pairs) != anyToInt(row.Expected["causal_pairs"], 0) {
				t.Fatalf("classification mismatch: observed=%d pairs=%d expected=%#v rows=%#v", observedEligible, len(pairs), row.Expected, projected)
			}
			if len(pairs) == 1 && math.Abs(pairs[0].GainPer1K-anyToFloat(row.Expected["gain_per_1k"])) > 0.000001 {
				t.Fatalf("gain_per_1k=%f want=%v", pairs[0].GainPer1K, row.Expected["gain_per_1k"])
			}
			if reason := anyToString(row.Expected["causal_exclusion"]); reason != "" && pairExclusions[reason] != 1 {
				t.Fatalf("missing causal exclusion %q: %#v", reason, pairExclusions)
			}
			if reason := anyToString(row.Expected["observation_exclusion"]); reason != "" {
				found := false
				for _, observation := range projected {
					if containsString(anyToStringList(anyMap(observation["eligibility"])["exclusion_reasons"], 32), reason) {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing observation exclusion %q: %#v", reason, projected)
				}
			}
			allRows = append(allRows, rows...)
		})
		if passed {
			correct++
		}
	}
	if correct != len(fixture.Cases) {
		t.Fatalf("utility holdout classification=%d/%d", correct, len(fixture.Cases))
	}
	projected, pairs, exclusions := utilityPairProjection(allRows)
	summary := utilityAggregate(projected, pairs, exclusions)
	want := fixture.AggregateExpectation
	for _, field := range []string{"observation_count", "observed_yield_eligible_count", "causal_pair_count"} {
		if anyToInt(summary[field], 0) != anyToInt(want[field], 0) {
			t.Fatalf("aggregate %s=%v want=%v summary=%#v", field, summary[field], want[field], summary)
		}
	}
	if anyToFloat(summary["verified_utility_sum"]) != anyToFloat(want["verified_utility_sum"]) ||
		anyToInt(anyMap(summary["denominators"])["model_visible_context_tokens_exact"], 0) != anyToInt(want["model_visible_context_tokens_exact"], 0) ||
		math.Abs(anyToFloat(summary["observed_utility_per_1k_model_visible_tokens"])-anyToFloat(want["observed_utility_per_1k_model_visible_tokens"])) > 0.000001 ||
		anyToFloat(summary["causal_utility_gain_per_1k_model_visible_tokens"]) != anyToFloat(want["causal_utility_gain_per_1k_model_visible_tokens"]) ||
		anyToString(summary["claim_status"]) != anyToString(want["claim_status"]) {
		t.Fatalf("aggregate utility economics drifted: summary=%#v want=%#v", summary, want)
	}
	negative := 0
	for _, pair := range pairs {
		if pair.UtilityGain < 0 {
			negative++
		}
	}
	if negative != anyToInt(want["negative_pair_count"], 0) || anyToString(anyMap(summary["causal_interval"])["status"]) != "available" {
		t.Fatalf("negative gains or interval were censored: negative=%d interval=%#v", negative, summary["causal_interval"])
	}
	if anyToInt(anyMap(summary["denominators"])["provider_total_tokens_observed"], 0) != 0 {
		t.Fatalf("provider tokens were inferred without observations: %#v", summary)
	}
}
