package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func derivedRegressionTestRow(partition, taskID, query, outcomeClass string) map[string]any {
	digest := "sha256:" + sha256Hex("result:"+taskID)
	return map[string]any{
		"outcome_id":                   "outcome-" + taskID,
		"sample_id":                    "sample-" + taskID,
		"task_id":                      taskID,
		"outcome_class":                outcomeClass,
		"first_pass_success":           outcomeClass == "success",
		"repair_required":              outcomeClass != "success",
		"verification_passed":          true,
		"verification_evidence_digest": "sha256:" + sha256Hex("verification:"+taskID),
		"calibration_eligible":         true,
		"leakage_free":                 true,
		"traffic_class":                "user",
		"regression_partition":         partition,
		"stability": map[string]any{
			"stable":         true,
			"run_count":      2,
			"result_digests": []string{digest, digest},
			"external_state": false,
		},
		"regression_case": map[string]any{
			"query":               query,
			"project":             "contextlattice",
			"topic_path":          "recall/regression",
			"limit":               8,
			"expected_files":      []string{"notes/expected.md"},
			"expected_substrings": []string{"expected evidence"},
			"negative_files":      []string{"notes/irrelevant.md"},
			"negative_substrings": []string{"known wrong answer"},
			"sources":             []string{"qdrant", "memory_bank"},
		},
	}
}

func TestDerivedRegressionSuiteStableRedactedDeduplicatedAndReviewGated(t *testing.T) {
	train := derivedRegressionTestRow(
		"train",
		"train-1",
		"Find Bearer abcdefghijklmnopqrstuvwxyz at /Users/alice/private/task",
		"success",
	)
	anyMap(train["regression_case"])["expected_files"] = []string{"/Users/alice/private/notes/expected.md"}
	duplicate := derivedRegressionTestRow(
		"train",
		"train-2",
		"Find Bearer abcdefghijklmnopqrstuvwxyz at /Users/alice/private/task",
		"failure",
	)
	anyMap(duplicate["regression_case"])["expected_files"] = []string{"/Users/alice/private/notes/expected.md"}
	holdout := derivedRegressionTestRow("holdout", "holdout-1", "Recover independent holdout evidence", "failure")

	first := buildDerivedRegressionSuite([]map[string]any{train, duplicate, holdout}, derivedRegressionSuiteOptions{})
	second := buildDerivedRegressionSuite([]map[string]any{holdout, duplicate, train}, derivedRegressionSuiteOptions{})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("suite output changed with input order:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if anyToString(first["schema_id"]) != derivedRegressionSuiteSchemaID || !anyToBool(first["immutable"]) {
		t.Fatalf("unexpected suite contract: %#v", first)
	}
	if !anyToBool(first["review_required"]) || anyToBool(first["admission_eligible"]) || anyToBool(first["admitted"]) {
		t.Fatalf("suite bypassed review-before-admission: %#v", first)
	}
	proposals, ok := first["proposals"].([]map[string]any)
	if !ok || len(proposals) != 2 {
		t.Fatalf("expected deduped train plus holdout proposals, got %#v", first["proposals"])
	}
	for _, proposal := range proposals {
		if !strings.HasPrefix(anyToString(proposal["proposal_id"]), "drp_") || !utilitySHA256DigestValid(anyToString(proposal["content_id"])) {
			t.Fatalf("proposal lacks stable content identity: %#v", proposal)
		}
		review := anyMap(proposal["review"])
		if anyToString(review["status"]) != "pending_review" || anyToBool(review["admission_eligible"]) {
			t.Fatalf("proposal is not review gated: %#v", proposal)
		}
	}
	trainProposal := proposals[0]
	if anyToString(trainProposal["partition"]) != "train" {
		trainProposal = proposals[1]
	}
	if got := len(anyToStringList(trainProposal["source_digests"], 8)); got != 2 {
		t.Fatalf("expected two deduplicated source digests, got %#v", trainProposal)
	}
	if !anyToBool(anyMap(trainProposal["redaction"])["applied"]) {
		t.Fatalf("expected redaction before hashing, got %#v", trainProposal)
	}
	summary := anyMap(first["summary"])
	if anyToInt(summary["deduplicated_source_count"], 0) != 1 || !anyToBool(summary["partition_complete"]) {
		t.Fatalf("unexpected suite summary: %#v", summary)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal suite: %v", err)
	}
	if strings.Contains(string(raw), "abcdefghijklmnopqrstuvwxyz") || strings.Contains(string(raw), "/Users/alice") {
		t.Fatalf("proposal leaked pre-redaction task material: %s", raw)
	}
}

func TestDerivedRegressionSuiteRejectsLeakageInstabilitySyntheticAndMissingNegatives(t *testing.T) {
	unstable := derivedRegressionTestRow("train", "unstable", "unstable fixture", "success")
	anyMap(unstable["stability"])["stable"] = false
	synthetic := derivedRegressionTestRow("train", "synthetic", "synthetic fixture", "success")
	synthetic["traffic_class"] = "synthetic"
	unknownTraffic := derivedRegressionTestRow("train", "unknown-traffic", "unknown traffic fixture", "success")
	delete(unknownTraffic, "traffic_class")
	missingNegative := derivedRegressionTestRow("train", "no-negative", "fixture without negative", "success")
	delete(anyMap(missingNegative["regression_case"]), "negative_files")
	delete(anyMap(missingNegative["regression_case"]), "negative_substrings")
	leakyTrain := derivedRegressionTestRow("train", "shared-task", "identical leaked query", "success")
	leakyHoldout := derivedRegressionTestRow("holdout", "shared-task", "identical leaked query", "failure")

	suite := buildDerivedRegressionSuite(
		[]map[string]any{unstable, synthetic, unknownTraffic, missingNegative, leakyTrain, leakyHoldout},
		derivedRegressionSuiteOptions{},
	)
	if proposals := parseRows(suite["proposals"]); len(proposals) != 0 {
		t.Fatalf("rejected rows produced proposals: %#v", proposals)
	}
	reasons := map[string]struct{}{}
	for _, rejection := range parseRows(suite["rejections"]) {
		for _, reason := range anyToStringList(rejection["reasons"], 16) {
			reasons[reason] = struct{}{}
		}
	}
	for _, expected := range []string{"instability_rejected", "synthetic_source", "traffic_class_unverified", "negative_expectation_missing", "train_holdout_leakage"} {
		if _, exists := reasons[expected]; !exists {
			t.Fatalf("missing rejection %q in %#v", expected, suite["rejections"])
		}
	}
}

func TestDerivedRegressionLedgerFieldsRedactsExplicitFixture(t *testing.T) {
	row := derivedRegressionTestRow("train", "ledger", "Bearer abcdefghijklmnopqrstuvwxyz /Users/alice/private", "success")
	delete(row, "regression_partition")
	anyMap(row["regression_case"])["partition"] = "train"
	entry := contextPackQualityOutcomeFromSample(row)
	if _, present := entry["regression_case"]; present {
		t.Fatalf("quality outcome must not retain an evaluable regression fixture: %#v", entry)
	}
	if !utilitySHA256DigestValid(anyToString(entry["regression_case_ref"])) {
		t.Fatalf("expected opaque regression fixture ref in outcome row: %#v", entry)
	}
	if anyToString(entry["regression_partition"]) != "train" {
		t.Fatalf("nested fixture partition did not survive ledger normalization: %#v", entry)
	}
	raw, _ := json.Marshal(entry)
	if strings.Contains(string(raw), "abcdefghijklmnopqrstuvwxyz") || strings.Contains(string(raw), "/Users/alice") || strings.Contains(string(raw), "topic_path") {
		t.Fatalf("quality outcome leaked raw regression fixture: %s", raw)
	}
}
