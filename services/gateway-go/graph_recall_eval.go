package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func anyToMapSlice(value any) []map[string]any {
	if rows, ok := value.([]map[string]any); ok {
		return rows
	}
	rows := []map[string]any{}
	if raw, ok := value.([]any); ok {
		for _, item := range raw {
			if mapped := anyMap(item); len(mapped) > 0 {
				rows = append(rows, mapped)
			}
		}
	}
	return rows
}

type graphRecallEvaluationTotals struct {
	casesExpected               int
	casesAttempted              int
	casesEvaluated              int
	casesTerminal               int
	caseFailures                int
	positiveExpected            int
	positiveCases               int
	positiveFailures            int
	hardNegativeExpected        int
	graphHits                   int
	incrementalCases            int
	incrementalHelped           int
	hardNegativeCases           int
	hardNegativePassed          int
	hardNegativeFailures        int
	hardNegativeOracleAvailable int
	directCases                 int
	directHits                  int
	directReciprocalSum         float64
	numericExpected             int
	numericMatched              int
	citationExpected            int
	citationMatched             int
	sourceDiversitySum          float64
	latencyValues               []float64
	treatmentLatencyValues      []float64
	controlLatencyValues        []float64
	latencyDeltaValues          []float64
	pairedLatencySamples        []graphRecallPairedLatencySample
	pairedLatencyCases          int
	pairedLatencyFailures       int
	caseReports                 []map[string]any
	incrementalReports          []map[string]any
	providerCalls               int
	providerTokens              int
	providerCostMicros          int
	networkCalls                int
	providerCallsKnown          bool
	providerTokensKnown         bool
	providerCostKnown           bool
	networkCallsKnown           bool
	localBackendCalls           int
	localBackendCallsKnown      bool
	externalNetworkCalls        int
	externalNetworkCallsKnown   bool
	externalNetworkZeroProven   bool
	costObservationExpected     int
	costObservationObserved     int
	costObservationMissing      int
	sourcePolicyObserved        bool
	sourcePolicyConsistent      bool
	sourcePolicyDigests         map[string]struct{}
	costSeamSeen                bool
	sourcePolicySeen            bool
}

type graphRecallPairedLatencySample struct {
	CaseID           string
	GraphTreatmentMS float64
	DirectControlMS  float64
}

// graphRecallPairedLatencyScope is the only latency surface used for the
// graph treatment/direct-control comparison. Both sides include executeRetrieval
// and the production final-response composition seam; retrieval-only timings
// are not presented as comparable evidence.
const (
	graphRecallPairedLatencyScope              = "execute_retrieval_plus_production_final_response_composition"
	graphRecallPairedLatencySchemaID           = "saved_recall_graph_paired_latency.v1"
	graphRecallPairedLatencyVersion            = 1
	graphRecallPairedLatencyRegressionBudgetMS = 20.0
)

func graphRecallControlLatencyValid(control map[string]any) (float64, bool) {
	if len(control) == 0 || !anyToBool(control["control_latency_comparable"]) || anyToString(control["control_latency_scope"]) != graphRecallPairedLatencyScope {
		return 0, false
	}
	latency := anyToFloat(control["control_latency_ms"])
	if math.IsNaN(latency) || math.IsInf(latency, 0) || latency < 0 {
		return 0, false
	}
	return latency, true
}

func graphRecallRoundSigned(value float64, places int) float64 {
	if places < 0 {
		return value
	}
	pow := math.Pow10(places)
	return math.Round(value*pow) / pow
}

func graphRecallSignedLatencyStats(values []float64) (float64, float64) {
	clean := make([]float64, 0, len(values))
	sum := 0.0
	for _, value := range values {
		if !isFiniteGraphMetric(value) {
			continue
		}
		clean = append(clean, value)
		sum += value
	}
	if len(clean) == 0 {
		return 0, 0
	}
	sort.Float64s(clean)
	return sum / float64(len(clean)), percentileFloat(clean, 0.95)
}

func graphRecallPairedLatencySummary(totals graphRecallEvaluationTotals) map[string]any {
	pairedExpected := totals.positiveExpected
	samples := make([]map[string]any, 0, len(totals.pairedLatencySamples))
	seenCases := make(map[string]struct{}, len(totals.pairedLatencySamples))
	validSamples := true
	improvements := 0
	regressions := 0
	unchanged := 0
	overBudget := 0
	maxRegression := 0.0
	for _, sample := range totals.pairedLatencySamples {
		caseID := strings.TrimSpace(sample.CaseID)
		treatment := roundFloat(sample.GraphTreatmentMS, 3)
		control := roundFloat(sample.DirectControlMS, 3)
		delta := graphRecallRoundSigned(treatment-control, 3)
		if caseID == "" || !isFiniteGraphMetric(treatment) || !isFiniteGraphMetric(control) || treatment < 0 || control < 0 {
			validSamples = false
		}
		if _, duplicate := seenCases[caseID]; duplicate {
			validSamples = false
		}
		seenCases[caseID] = struct{}{}
		classification := "unchanged"
		switch {
		case delta < 0:
			classification = "improvement"
			improvements++
		case delta > 0:
			classification = "regression"
			regressions++
			if delta > maxRegression {
				maxRegression = delta
			}
			if delta > graphRecallPairedLatencyRegressionBudgetMS {
				overBudget++
			}
		default:
			unchanged++
		}
		samples = append(samples, map[string]any{
			"case_id": caseID, "graph_treatment_ms": treatment, "direct_control_ms": control,
			"delta_ms": delta, "classification": classification,
		})
	}
	comparable := pairedExpected > 0 && validSamples && totals.pairedLatencyCases == pairedExpected && len(samples) == pairedExpected && totals.pairedLatencyFailures == 0
	withinBudget := comparable && overBudget == 0
	reason := "paired_graph_and_direct_control_latency_observed"
	if !comparable {
		reason = "paired_graph_and_direct_control_latency_incomplete"
	} else if !withinBudget {
		reason = "paired_graph_latency_regression_budget_exceeded"
	}
	controlAverage, controlP95 := recallLatencyStats(totals.controlLatencyValues)
	deltaAverage, deltaP95 := graphRecallSignedLatencyStats(totals.latencyDeltaValues)
	treatmentAverage, treatmentP95 := recallLatencyStats(totals.treatmentLatencyValues)
	return map[string]any{
		"schema_id":                      graphRecallPairedLatencySchemaID,
		"version":                        graphRecallPairedLatencyVersion,
		"scope":                          graphRecallPairedLatencyScope,
		"comparable":                     comparable,
		"reason":                         reason,
		"expected_cases":                 pairedExpected,
		"paired_cases":                   totals.pairedLatencyCases,
		"failed_cases":                   totals.pairedLatencyFailures,
		"graph_treatment_count":          len(totals.treatmentLatencyValues),
		"direct_control_count":           len(totals.controlLatencyValues),
		"delta_count":                    len(totals.latencyDeltaValues),
		"graph_treatment_avg_ms":         roundFloat(treatmentAverage, 3),
		"graph_treatment_p95_ms":         roundFloat(treatmentP95, 3),
		"direct_control_avg_ms":          roundFloat(controlAverage, 3),
		"direct_control_p95_ms":          roundFloat(controlP95, 3),
		"latency_delta_avg_ms":           graphRecallRoundSigned(deltaAverage, 3),
		"latency_delta_p95_ms":           graphRecallRoundSigned(deltaP95, 3),
		"samples":                        samples,
		"sample_digest":                  "sha256:" + graphCorpusDigestMap(map[string]any{"samples": samples}),
		"improvement_count":              improvements,
		"regression_count":               regressions,
		"unchanged_count":                unchanged,
		"regression_over_budget_count":   overBudget,
		"max_regression_ms":              roundFloat(maxRegression, 3),
		"canonical_regression_budget_ms": graphRecallPairedLatencyRegressionBudgetMS,
		"regression_budget_kind":         "maximum_per_case_positive_delta_ms",
		"within_regression_budget":       withinBudget,
		"claims_allowed":                 comparable && withinBudget,
		"measurement_authority":          "execute_retrieval_server",
		"retrieval_only_comparison_used": false,
	}
}

func graphRecallPairedLatencyEvidenceValid(summary map[string]any, expected int) (bool, bool) {
	if expected <= 0 || anyToString(summary["schema_id"]) != graphRecallPairedLatencySchemaID || anyToInt(summary["version"], 0) != graphRecallPairedLatencyVersion || anyToString(summary["scope"]) != graphRecallPairedLatencyScope || anyToFloat(summary["canonical_regression_budget_ms"]) != graphRecallPairedLatencyRegressionBudgetMS || anyToString(summary["regression_budget_kind"]) != "maximum_per_case_positive_delta_ms" {
		return false, false
	}
	samples := anyToMapSlice(summary["samples"])
	if len(samples) != expected || anyToInt(summary["expected_cases"], 0) != expected || anyToInt(summary["paired_cases"], 0) != expected || anyToInt(summary["failed_cases"], 0) != 0 || !anyToBool(summary["comparable"]) {
		return false, false
	}
	seen := make(map[string]struct{}, len(samples))
	improvements, regressions, unchanged, overBudget := 0, 0, 0, 0
	maxRegression := 0.0
	treatmentValues := make([]float64, 0, len(samples))
	controlValues := make([]float64, 0, len(samples))
	deltaValues := make([]float64, 0, len(samples))
	for _, sample := range samples {
		caseID := strings.TrimSpace(anyToString(sample["case_id"]))
		treatment := anyToFloat(sample["graph_treatment_ms"])
		control := anyToFloat(sample["direct_control_ms"])
		delta := anyToFloat(sample["delta_ms"])
		if caseID == "" || !isFiniteGraphMetric(treatment) || !isFiniteGraphMetric(control) || !isFiniteGraphMetric(delta) || treatment < 0 || control < 0 || math.Abs(delta-graphRecallRoundSigned(treatment-control, 3)) > 0.0005 {
			return false, false
		}
		if _, duplicate := seen[caseID]; duplicate {
			return false, false
		}
		seen[caseID] = struct{}{}
		treatmentValues = append(treatmentValues, treatment)
		controlValues = append(controlValues, control)
		deltaValues = append(deltaValues, delta)
		expectedClass := "unchanged"
		switch {
		case delta < 0:
			expectedClass = "improvement"
			improvements++
		case delta > 0:
			expectedClass = "regression"
			regressions++
			maxRegression = math.Max(maxRegression, delta)
			if delta > graphRecallPairedLatencyRegressionBudgetMS {
				overBudget++
			}
		default:
			unchanged++
		}
		if anyToString(sample["classification"]) != expectedClass {
			return false, false
		}
	}
	if anyToString(summary["sample_digest"]) != "sha256:"+graphCorpusDigestMap(map[string]any{"samples": samples}) || anyToInt(summary["improvement_count"], -1) != improvements || anyToInt(summary["regression_count"], -1) != regressions || anyToInt(summary["unchanged_count"], -1) != unchanged || anyToInt(summary["regression_over_budget_count"], -1) != overBudget || math.Abs(anyToFloat(summary["max_regression_ms"])-roundFloat(maxRegression, 3)) > 0.0005 {
		return false, false
	}
	treatmentAverage, treatmentP95 := recallLatencyStats(treatmentValues)
	controlAverage, controlP95 := recallLatencyStats(controlValues)
	deltaAverage, deltaP95 := graphRecallSignedLatencyStats(deltaValues)
	if anyToInt(summary["graph_treatment_count"], 0) != expected || anyToInt(summary["direct_control_count"], 0) != expected || anyToInt(summary["delta_count"], 0) != expected || math.Abs(anyToFloat(summary["graph_treatment_avg_ms"])-roundFloat(treatmentAverage, 3)) > 0.0005 || math.Abs(anyToFloat(summary["graph_treatment_p95_ms"])-roundFloat(treatmentP95, 3)) > 0.0005 || math.Abs(anyToFloat(summary["direct_control_avg_ms"])-roundFloat(controlAverage, 3)) > 0.0005 || math.Abs(anyToFloat(summary["direct_control_p95_ms"])-roundFloat(controlP95, 3)) > 0.0005 || math.Abs(anyToFloat(summary["latency_delta_avg_ms"])-graphRecallRoundSigned(deltaAverage, 3)) > 0.0005 || math.Abs(anyToFloat(summary["latency_delta_p95_ms"])-graphRecallRoundSigned(deltaP95, 3)) > 0.0005 || anyToString(summary["measurement_authority"]) != "execute_retrieval_server" || anyToBool(summary["retrieval_only_comparison_used"]) {
		return false, false
	}
	withinBudget := overBudget == 0
	if anyToBool(summary["within_regression_budget"]) != withinBudget || anyToBool(summary["claims_allowed"]) != withinBudget {
		return false, false
	}
	return true, withinBudget
}

const graphRecallDirectBaselineSchemaID = "saved_recall_direct_baseline.v1"

const graphRecallDirectBaselineCustodySchemaID = "saved_recall_direct_baseline_custody.v1"

const graphRecallDirectControlSchemaID = "saved_recall_direct_control.v1"

const graphRecallDirectControlPolicyVersion = 1

func savedRecallDirectBaselineCases(cfg recallEvalSavedConfig) ([]map[string]any, string) {
	holdout := recallEvalCasesForSplit(cfg.Cases, "holdout")
	if len(holdout) > 0 {
		return holdout, "holdout"
	}
	return append([]map[string]any(nil), cfg.Cases...), "all"
}

func savedRecallDirectControlPolicy() map[string]any {
	identity := contextLatticeBuildIdentity()
	policy := map[string]any{
		"schema_id":                graphRecallDirectControlSchemaID,
		"version":                  graphRecallDirectControlPolicyVersion,
		"graph_influence_disabled": true,
		"graph_backend_consulted":  false,
		"graph_results_used":       false,
		"retrieval_path":           "executeRetrieval_native_without_graph_context",
		"evaluation_traffic_class": "evaluation_holdout",
		"source_commit":            identity["source_commit"],
		"source_tree":              identity["source_tree"],
		"source_bound":             identity["source_bound"],
	}
	policy["policy_digest"] = "sha256:" + graphCorpusDigestMap(policy)
	return policy
}

func savedRecallDirectControlReceipt(requestPayload map[string]any) map[string]any {
	receipt := cloneJSONMap(savedRecallDirectControlPolicy())
	receipt["case_id"] = strings.TrimSpace(anyToString(requestPayload["direct_baseline_case_id"]))
	receipt["case_set_digest"] = strings.TrimSpace(anyToString(requestPayload["direct_baseline_case_set_digest"]))
	receipt["snapshot_digest"] = strings.TrimSpace(anyToString(requestPayload["direct_baseline_snapshot_digest"]))
	receipt["k"] = anyToInt(requestPayload["direct_baseline_k"], 0)
	receipt["captured_at"] = nowUTCISO()
	receipt["phase"] = "pre_graph_treatment"
	receipt["traffic_class"] = strings.TrimSpace(anyToString(requestPayload["traffic_class"]))
	receipt["candidate_allocation_active"] = false
	receipt["treatment_active"] = false
	return receipt
}

func savedRecallDirectControlValid(receipt, expectedBinding map[string]any) bool {
	if len(receipt) == 0 || anyToString(receipt["schema_id"]) != graphRecallDirectControlSchemaID || anyToInt(receipt["version"], 0) != graphRecallDirectControlPolicyVersion {
		return false
	}
	policy := savedRecallDirectControlPolicy()
	for _, key := range []string{"policy_digest", "source_commit", "source_tree"} {
		if strings.TrimSpace(anyToString(receipt[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(receipt[key])), strings.TrimSpace(anyToString(policy[key]))) {
			return false
		}
	}
	if !anyToBool(receipt["source_bound"]) || !anyToBool(receipt["graph_influence_disabled"]) || anyToBool(receipt["graph_backend_consulted"]) || anyToBool(receipt["graph_results_used"]) || anyToString(receipt["evaluation_traffic_class"]) != "evaluation_holdout" || anyToString(receipt["phase"]) != "pre_graph_treatment" || strings.TrimSpace(anyToString(receipt["case_id"])) == "" || anyToBool(receipt["candidate_allocation_active"]) || anyToBool(receipt["treatment_active"]) || anyToString(receipt["traffic_class"]) != "evaluation_holdout" {
		return false
	}
	if _, present := receipt["candidate_allocation_active"]; !present {
		return false
	}
	if _, present := receipt["treatment_active"]; !present {
		return false
	}
	for _, key := range []string{"case_set_digest", "snapshot_digest"} {
		if strings.TrimSpace(anyToString(expectedBinding[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(receipt[key])), strings.TrimSpace(anyToString(expectedBinding[key]))) {
			return false
		}
	}
	if anyToInt(receipt["k"], 0) != anyToInt(expectedBinding["k"], 0) {
		return false
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(receipt["captured_at"]))); err != nil || capturedAt.IsZero() {
		return false
	}
	return true
}

func graphRecallDirectControlCohortReceipt(receipts []map[string]any, cases []map[string]any, binding map[string]any) map[string]any {
	result := map[string]any{
		"schema_id": graphRecallDirectControlSchemaID, "version": graphRecallDirectControlPolicyVersion,
		"available": false, "case_count": len(receipts), "expected_case_count": len(cases),
		"case_set_digest": binding["case_set_digest"], "snapshot_digest": binding["snapshot_digest"], "k": binding["k"],
		"evaluation_split": binding["evaluation_split"], "evaluation_case_set_digest": binding["evaluation_case_set_digest"], "evaluation_case_count": binding["evaluation_case_count"], "evaluation_traffic_class": binding["evaluation_traffic_class"],
	}
	expectedIDs := map[string]struct{}{}
	for _, rawCase := range cases {
		if caseID := strings.TrimSpace(anyToString(rawCase["id"])); caseID != "" {
			expectedIDs[caseID] = struct{}{}
		}
	}
	seenIDs := map[string]struct{}{}
	var first map[string]any
	for _, receipt := range receipts {
		if !savedRecallDirectControlValid(receipt, binding) {
			result["reason"] = "direct_control_receipt_invalid"
			return result
		}
		caseID := strings.TrimSpace(anyToString(receipt["case_id"]))
		if _, exists := seenIDs[caseID]; exists {
			result["reason"] = "direct_control_case_duplicate"
			return result
		}
		seenIDs[caseID] = struct{}{}
		if len(expectedIDs) > 0 {
			if _, exists := expectedIDs[caseID]; !exists {
				result["reason"] = "direct_control_case_binding_mismatch"
				return result
			}
		}
		if first == nil {
			first = receipt
		}
	}
	if len(receipts) != len(cases) || len(seenIDs) != len(expectedIDs) || first == nil {
		result["reason"] = "direct_control_cohort_incomplete"
		return result
	}
	result["available"] = true
	result["status"] = "validated"
	result["policy_digest"] = first["policy_digest"]
	result["source_commit"] = first["source_commit"]
	result["source_tree"] = first["source_tree"]
	result["source_bound"] = first["source_bound"]
	result["graph_influence_disabled"] = true
	result["graph_backend_consulted"] = false
	result["graph_results_used"] = false
	result["phase"] = "pre_graph_treatment"
	result["traffic_class"] = first["traffic_class"]
	result["evaluation_traffic_class"] = "evaluation_holdout"
	result["candidate_allocation_active"] = false
	result["treatment_active"] = false
	result["cohort_captured_at"] = nowUTCISO()
	return result
}

func savedRecallGraphIncrementalControlReceipt(requestPayload, searchResponse map[string]any) map[string]any {
	identity := contextLatticeBuildIdentity()
	seedID := strings.TrimSpace(anyToString(requestPayload["graph_control_seed_memory_id"]))
	targetID := strings.TrimSpace(anyToString(requestPayload["graph_control_target_memory_id"]))
	k := anyToInt(requestPayload["graph_control_k"], defaultRecallEvalK)
	results := parseRows(searchResponse["results"])
	targetDirect := graphRecallResultContainsMemoryID(results, targetID, k)
	if !targetDirect {
		targetDirect = matchRankWithinKProject(results, normalizeExpectedFileTokens(requestPayload["graph_control_target_files"]), nil, k, anyToString(requestPayload["project"])) != nil
	}
	receipt := map[string]any{
		"schema_id":                   savedRecallGraphIncrementalControlSchemaID,
		"version":                     savedRecallGraphIncrementalControlVersion,
		"authority":                   savedRecallGraphIncrementalControlAuthority,
		"graph_influence_disabled":    true,
		"graph_backend_consulted":     false,
		"graph_results_used":          false,
		"candidate_allocation_active": false,
		"treatment_active":            false,
		"traffic_class":               "evaluation_holdout",
		"seed_target_lineage":         strings.TrimSpace(anyToString(requestPayload["graph_control_seed_target_lineage"])),
		"seed_memory_id":              seedID,
		"target_memory_id":            targetID,
		"target_direct_hit":           targetDirect,
		"control_snapshot_digest":     strings.TrimSpace(anyToString(requestPayload["graph_control_snapshot_digest"])),
		"source_snapshot_digest":      strings.TrimSpace(anyToString(requestPayload["graph_control_source_snapshot_digest"])),
		"edge_snapshot_digest":        strings.TrimSpace(anyToString(requestPayload["graph_control_edge_snapshot_digest"])),
		"control_k":                   k,
		"control_request_digest":      graphRecallControlRequestDigest(requestPayload),
		"control_response_digest":     graphRecallControlResponseDigest(searchResponse),
		"source_runtime_identity":     identity,
		"captured_at":                 nowUTCISO(),
		"control_path":                "executeRetrieval_native_graph_disabled",
		"cost_observability":          cloneJSONMap(anyMap(searchResponse["cost_observability"])),
	}
	receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt)
	return receipt
}

func savedRecallDirectControlCohortValid(receipt, expectedBinding map[string]any) bool {
	if len(receipt) == 0 || !anyToBool(receipt["available"]) || anyToString(receipt["schema_id"]) != graphRecallDirectControlSchemaID || anyToInt(receipt["version"], 0) != graphRecallDirectControlPolicyVersion {
		return false
	}
	policy := savedRecallDirectControlPolicy()
	for _, key := range []string{"policy_digest", "source_commit", "source_tree"} {
		if strings.TrimSpace(anyToString(receipt[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(receipt[key])), strings.TrimSpace(anyToString(policy[key]))) {
			return false
		}
	}
	if !anyToBool(receipt["source_bound"]) || !anyToBool(receipt["graph_influence_disabled"]) || anyToBool(receipt["graph_backend_consulted"]) || anyToBool(receipt["graph_results_used"]) || anyToString(receipt["phase"]) != "pre_graph_treatment" || anyToBool(receipt["candidate_allocation_active"]) || anyToBool(receipt["treatment_active"]) || anyToString(receipt["traffic_class"]) != "evaluation_holdout" || anyToString(receipt["evaluation_traffic_class"]) != "evaluation_holdout" {
		return false
	}
	for _, key := range []string{"candidate_allocation_active", "treatment_active"} {
		if _, present := receipt[key]; !present {
			return false
		}
	}
	for _, key := range []string{"case_set_digest", "snapshot_digest"} {
		if strings.TrimSpace(anyToString(expectedBinding[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(receipt[key])), strings.TrimSpace(anyToString(expectedBinding[key]))) {
			return false
		}
	}
	if anyToInt(receipt["k"], 0) != anyToInt(expectedBinding["k"], 0) {
		return false
	}
	for _, key := range []string{"evaluation_split", "evaluation_case_set_digest", "evaluation_case_count", "evaluation_traffic_class"} {
		if expected, present := expectedBinding[key]; present {
			if key == "evaluation_case_count" {
				if anyToInt(receipt[key], 0) != anyToInt(expected, 0) {
					return false
				}
				continue
			}
			if strings.TrimSpace(anyToString(receipt[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(receipt[key])), strings.TrimSpace(anyToString(expected))) {
				return false
			}
		}
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(receipt["cohort_captured_at"]))); err != nil || capturedAt.IsZero() {
		return false
	}
	return true
}

func resolveSavedRecallDirectBaselinePath() string {
	return resolveStoragePath("ORCH_RECALL_DIRECT_BASELINE_PATH", ".data/orchestrator/recall_eval_direct_baseline.json")
}

func loadSavedRecallDirectBaseline(expectedBinding map[string]any) (map[string]any, map[string]any) {
	receipt := map[string]any{
		"schema_id": graphRecallDirectBaselineSchemaID, "available": false,
		"binding": cloneJSONMap(expectedBinding), "store_ref": ownerOnlyStoreRef("recall_direct_baseline"),
	}
	path := resolveSavedRecallDirectBaselinePath()
	info, statErr := os.Lstat(filepath.Clean(path))
	if errors.Is(statErr, os.ErrNotExist) {
		receipt["reason"] = "baseline_receipt_missing"
		return nil, receipt
	}
	if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		receipt["reason"] = "baseline_storage_access_error"
		return nil, receipt
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		receipt["reason"] = "baseline_receipt_missing"
		return nil, receipt
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		receipt["reason"] = "baseline_receipt_invalid_json"
		return nil, receipt
	}
	issues := make([]string, 0)
	if anyToString(payload["schema_id"]) != graphRecallDirectBaselineSchemaID || anyToInt(payload["version"], 0) != 1 {
		issues = append(issues, "schema_version_mismatch")
	}
	binding := anyMap(payload["binding"])
	for _, key := range []string{"schema_id", "version", "case_set_digest", "snapshot_digest", "k"} {
		if strings.TrimSpace(anyToString(expectedBinding[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(expectedBinding[key])), strings.TrimSpace(anyToString(binding[key]))) {
			issues = append(issues, "artifact_binding_mismatch")
			break
		}
	}
	for _, key := range []string{"evaluation_split", "evaluation_case_set_digest", "evaluation_case_count", "evaluation_traffic_class"} {
		if _, expected := expectedBinding[key]; !expected {
			continue
		}
		if key == "evaluation_case_count" {
			if anyToInt(expectedBinding[key], 0) != anyToInt(binding[key], 0) {
				issues = append(issues, "artifact_evaluation_cohort_mismatch")
			}
			continue
		}
		if strings.TrimSpace(anyToString(expectedBinding[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(expectedBinding[key])), strings.TrimSpace(anyToString(binding[key]))) {
			issues = append(issues, "artifact_evaluation_cohort_mismatch")
		}
	}
	custody := anyMap(payload["custody"])
	if anyToString(custody["schema_id"]) != graphRecallDirectBaselineCustodySchemaID || anyToString(custody["owner"]) != savedRecallGraphCorpusOwner || !anyToBool(custody["sealed"]) || anyToBool(custody["promotional_claims_allowed"]) || !strings.EqualFold(anyToString(custody["case_set_digest"]), anyToString(binding["case_set_digest"])) || !strings.EqualFold(anyToString(custody["snapshot_digest"]), anyToString(binding["snapshot_digest"])) {
		issues = append(issues, "invalid_baseline_custody")
	}
	if anyToBool(payload["file_names_disclosed"]) || anyToBool(binding["file_names_disclosed"]) {
		issues = append(issues, "baseline_filename_disclosure")
	}
	control := anyMap(payload["control"])
	if !savedRecallDirectControlCohortValid(control, binding) {
		issues = append(issues, "invalid_direct_control_receipt")
	}
	evaluation := anyMap(payload["evaluation"])
	if strings.TrimSpace(anyToString(evaluation["split"])) != strings.TrimSpace(anyToString(binding["evaluation_split"])) || anyToInt(evaluation["case_count"], 0) != anyToInt(binding["evaluation_case_count"], 0) || anyToInt(evaluation["k"], 0) != anyToInt(binding["k"], 0) || anyToBool(evaluation["graph_results_used"]) || anyToBool(evaluation["candidate_allocation_active"]) || anyToString(evaluation["traffic_class"]) != "evaluation_holdout" {
		issues = append(issues, "invalid_direct_evaluation_binding")
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(evaluation["captured_at"]))); err != nil || capturedAt.IsZero() {
		issues = append(issues, "invalid_direct_evaluation_binding")
	}
	digest := strings.TrimSpace(anyToString(payload["digest"]))
	if digest == "" || !strings.EqualFold(digest, "sha256:"+graphCorpusDigestMap(payload, "digest")) {
		issues = append(issues, "baseline_digest_mismatch")
	}
	metrics := anyMap(payload["metrics"])
	baseline := map[string]any{"binding": cloneJSONMap(binding)}
	for _, key := range []string{"recallAtK", "mrr", "numericExactness", "citationCoverage", "sourceDiversity"} {
		value, present := metrics[key]
		parsed := anyToFloat(value)
		if !present || !isFiniteGraphMetric(parsed) || parsed < 0 {
			issues = append(issues, "baseline_metric_missing_"+key)
			continue
		}
		baseline[key] = parsed
	}
	if len(issues) > 0 {
		receipt["reason"] = "baseline_receipt_failed_closed_validation"
		receipt["issues"] = graphCorpusSortedStrings(issues)
		return nil, receipt
	}
	receipt["available"] = true
	receipt["status"] = "validated"
	receipt["digest"] = digest
	receipt["binding"] = cloneJSONMap(binding)
	receipt["control_policy_digest"] = control["policy_digest"]
	receipt["control_source_commit"] = control["source_commit"]
	receipt["control_source_tree"] = control["source_tree"]
	receipt["control_captured_at"] = control["cohort_captured_at"]
	return baseline, receipt
}

// captureSavedRecallDirectBaseline is callable only from the direct saved
// recall evaluator.  The graph evaluator deliberately has no write path for
// this artifact, so a graph result cannot become its own pre-treatment
// baseline.  Repeating an identical capture is idempotent; replacing a
// receipt requires an explicit direct-evaluator capture after the frozen v3
// case/snapshot binding has changed.
func captureSavedRecallDirectBaseline(cfg recallEvalSavedConfig, metrics, control map[string]any) (map[string]any, error) {
	binding := graphCorpusDirectBaselineBinding(cfg)
	if strings.TrimSpace(anyToString(binding["case_set_digest"])) == "" || strings.TrimSpace(anyToString(binding["snapshot_digest"])) == "" || !anyToBool(binding["benchmark_eligible"]) {
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "direct_case_set_not_benchmark_eligible"}, errors.New("direct case set is not benchmark eligible")
	}
	if !anyToBool(control["available"]) || anyToString(control["schema_id"]) != graphRecallDirectControlSchemaID || anyToBool(metrics["graph_results_used"]) || anyToBool(metrics["candidate_allocation_active"]) || !savedRecallDirectControlCohortValid(control, binding) {
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "direct_control_receipt_invalid"}, errors.New("direct baseline requires a server-owned graph-disabled control receipt")
	}
	artifactMetrics := map[string]any{}
	for _, key := range []string{"recallAtK", "mrr", "numericExactness", "citationCoverage", "sourceDiversity"} {
		value, present := metrics[key]
		parsed := anyToFloat(value)
		if !present || !isFiniteGraphMetric(parsed) || parsed < 0 {
			return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "direct_metric_missing", "metric": key}, fmt.Errorf("direct metric %s is unavailable", key)
		}
		artifactMetrics[key] = roundFloat(parsed, 6)
	}
	artifact := map[string]any{
		"schema_id": graphRecallDirectBaselineSchemaID,
		"version":   1,
		"binding":   binding,
		"metrics":   artifactMetrics,
		"source":    "authoritative_saved_recall_evaluation_direct",
		"evaluation": map[string]any{
			"split":                       anyToString(binding["evaluation_split"]),
			"case_count":                  anyToInt(binding["evaluation_case_count"], len(cfg.Cases)),
			"k":                           cfg.K,
			"graph_results_used":          false,
			"candidate_allocation_active": false,
			"traffic_class":               "evaluation_holdout",
			"captured_at":                 nowUTCISO(),
		},
		"control": control,
		"custody": map[string]any{
			"schema_id":                  graphRecallDirectBaselineCustodySchemaID,
			"owner":                      savedRecallGraphCorpusOwner,
			"sealed":                     true,
			"promotional_claims_allowed": false,
			"case_set_digest":            binding["case_set_digest"],
			"snapshot_digest":            binding["snapshot_digest"],
			"source":                     "gateway-go-direct-evaluator",
			"file_names_disclosed":       false,
		},
		"file_names_disclosed": false,
	}
	artifact["digest"] = "sha256:" + graphCorpusDigestMap(artifact, "digest")
	path := resolveSavedRecallDirectBaselinePath()
	if err := prepareOwnerOnlyFile(path, strings.TrimSpace(os.Getenv("ORCH_RECALL_DIRECT_BASELINE_PATH")) == ""); err != nil {
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_storage_access_error"}, err
	}
	if existing, receipt := loadSavedRecallDirectBaseline(binding); len(existing) > 0 {
		if graphRecallMetricMapsEqual(existing, artifactMetrics) {
			receipt["capture"] = "idempotent"
			return receipt, nil
		}
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_immutable_existing", "binding": cloneJSONMap(binding)}, errors.New("direct baseline already sealed for this case and snapshot binding")
	}
	if info, statErr := os.Lstat(filepath.Clean(path)); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_immutable_existing", "binding": cloneJSONMap(binding)}, errors.New("direct baseline path already exists and is not a valid sealed artifact")
		}
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_immutable_existing", "binding": cloneJSONMap(binding)}, errors.New("direct baseline exists but failed closed validation")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_storage_access_error"}, statErr
	}
	raw, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_encode_error"}, err
	}
	if err := writeOwnerOnlyAtomicFile(path, raw, false); err != nil {
		return map[string]any{"schema_id": graphRecallDirectBaselineSchemaID, "available": false, "reason": "baseline_write_error"}, err
	}
	return map[string]any{
		"schema_id": graphRecallDirectBaselineSchemaID, "available": true, "status": "captured",
		"capture": "authoritative_direct_evaluation", "digest": artifact["digest"],
		"binding": cloneJSONMap(binding), "store_ref": ownerOnlyStoreRef("recall_direct_baseline"),
	}, nil
}

func graphRecallMetricMapsEqual(left, right map[string]any) bool {
	for _, key := range []string{"recallAtK", "mrr", "numericExactness", "citationCoverage", "sourceDiversity"} {
		l, lok := left[key]
		r, rok := right[key]
		if !lok || !rok || roundFloat(anyToFloat(l), 6) != roundFloat(anyToFloat(r), 6) {
			return false
		}
	}
	return true
}

func graphRecallObservedInt(payload map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if raw, ok := payload[key]; ok {
			return maxInt(anyToInt(raw, 0), 0), true
		}
	}
	return 0, false
}

func graphRecallObservedEconomics(response map[string]any, totals *graphRecallEvaluationTotals) {
	if totals == nil || response == nil {
		return
	}
	// Only the server-owned per-request seam is admissible. Mirrored response,
	// telemetry, or caller fields are data, not cost proof. Every attempted
	// request contributes to the run-wide observed denominator below.
	seam := anyMap(response["cost_observability"])
	if anyToString(seam["schema_id"]) != retrievalCostObservabilitySchemaID || anyToString(seam["authority"]) != retrievalCostObservabilityAuthority {
		return
	}
	if !totals.costSeamSeen {
		totals.costSeamSeen = true
		totals.networkCallsKnown = true
		totals.localBackendCallsKnown = true
		totals.externalNetworkCallsKnown = true
		totals.providerCallsKnown = true
		totals.providerTokensKnown = true
		totals.providerCostKnown = true
		totals.externalNetworkZeroProven = true
	}
	addObserved := func(value int, present bool, observed bool, total *int, known *bool) {
		if !present {
			*known = false
			return
		}
		*total += value
		*known = *known && observed
	}
	if value, present := graphRecallObservedInt(seam, "network_calls"); present {
		addObserved(value, true, anyToBool(seam["network_calls_observed"]), &totals.networkCalls, &totals.networkCallsKnown)
	} else {
		totals.networkCallsKnown = false
	}
	if value, present := graphRecallObservedInt(seam, "local_backend_calls"); present {
		addObserved(value, true, anyToBool(seam["local_backend_calls_observed"]), &totals.localBackendCalls, &totals.localBackendCallsKnown)
	} else {
		totals.localBackendCallsKnown = false
	}
	if value, present := graphRecallObservedInt(seam, "external_network_calls"); present {
		addObserved(value, true, anyToBool(seam["external_network_calls_observed"]), &totals.externalNetworkCalls, &totals.externalNetworkCallsKnown)
	} else {
		totals.externalNetworkCallsKnown = false
	}
	if value, present := graphRecallObservedInt(seam, "provider_calls", "model_calls"); present {
		addObserved(value, true, anyToBool(seam["provider_calls_observed"]), &totals.providerCalls, &totals.providerCallsKnown)
	} else {
		totals.providerCallsKnown = false
	}
	if value, present := graphRecallObservedInt(seam, "provider_tokens", "provider_total_tokens", "total_provider_tokens"); present {
		addObserved(value, true, anyToBool(seam["provider_tokens_observed"]), &totals.providerTokens, &totals.providerTokensKnown)
	} else {
		totals.providerTokensKnown = false
	}
	if value, present := graphRecallObservedInt(seam, "provider_cost_microusd", "provider_cost_micros", "cost_microusd"); present {
		addObserved(value, true, anyToBool(seam["provider_cost_observed"]), &totals.providerCostMicros, &totals.providerCostKnown)
	} else {
		totals.providerCostKnown = false
	}
	totals.externalNetworkZeroProven = totals.externalNetworkZeroProven && anyToBool(seam["external_network_zero_proven"])
	policy := anyMap(seam["source_policy"])
	if !totals.sourcePolicySeen {
		totals.sourcePolicySeen = true
		totals.sourcePolicyConsistent = graphRecallSourcePolicyReceiptValid(policy)
	} else if !graphRecallSourcePolicyReceiptValid(policy) {
		totals.sourcePolicyConsistent = false
	}
	if graphRecallSourcePolicyReceiptValid(policy) {
		totals.sourcePolicyObserved = true
		if totals.sourcePolicyDigests == nil {
			totals.sourcePolicyDigests = map[string]struct{}{}
		}
		if digest := strings.TrimSpace(anyToString(policy["digest"])); digest != "" {
			totals.sourcePolicyDigests[digest] = struct{}{}
		}
	}
}

func graphRecallSourcePolicyReceiptValid(policy map[string]any) bool {
	if anyToString(policy["schema_id"]) != retrievalEvaluationSourcePolicySchemaID || anyToInt(policy["version"], 0) != retrievalEvaluationSourcePolicyVersion || !anyToBool(policy["server_owned"]) || !anyToBool(policy["evaluation_holdout"]) || !anyToBool(policy["eligible"]) || anyToString(policy["allowed_transport"]) != "in_process" || anyToString(policy["provider_policy"]) != "provider_incapable_in_process_only" || !anyToBool(policy["redirect_escape_disabled"]) || anyToBool(policy["downstream_zero_receipt"]) {
		return false
	}
	if strings.TrimSpace(anyToString(policy["digest"])) == "" || !strings.EqualFold(anyToString(policy["digest"]), "sha256:"+graphCorpusDigestMap(policy, "digest")) {
		return false
	}
	transport := anyMap(policy["transport_classification"])
	if len(transport) == 0 {
		return false
	}
	localStores := map[string]struct{}{}
	for _, source := range anyToStringSlice(policy["approved_local_store_sources"]) {
		if !retrievalEvaluationApprovedLocalStore(source) {
			return false
		}
		localStores[strings.TrimSpace(strings.ToLower(source))] = struct{}{}
	}
	for source, raw := range transport {
		entry := anyMap(raw)
		class := strings.TrimSpace(strings.ToLower(anyToString(entry["class"])))
		if source == "embedding_provider" {
			if class != "in_process" {
				return false
			}
			continue
		}
		if anyToString(entry["owner"]) != sourceOwnerGoNative {
			return false
		}
		switch class {
		case "in_process":
			if _, listed := localStores[source]; listed {
				return false
			}
		case "approved_local_endpoint":
			if !retrievalEvaluationApprovedLocalStore(source) {
				return false
			}
			if _, listed := localStores[source]; !listed {
				return false
			}
		default:
			return false
		}
	}
	identity := anyMap(policy["source_runtime_identity"])
	currentIdentity := contextLatticeBuildIdentity()
	return anyToBool(identity["source_bound"]) && anyToBool(currentIdentity["source_bound"]) && strings.EqualFold(anyToString(identity["source_commit"]), anyToString(currentIdentity["source_commit"])) && strings.EqualFold(anyToString(identity["source_tree"]), anyToString(currentIdentity["source_tree"]))
}

func graphRecallSourcePolicyRunReceipt(totals graphRecallEvaluationTotals) map[string]any {
	digests := make([]string, 0, len(totals.sourcePolicyDigests))
	for digest := range totals.sourcePolicyDigests {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	receipt := map[string]any{
		"schema_id":                retrievalEvaluationSourcePolicySchemaID,
		"version":                  retrievalEvaluationSourcePolicyVersion,
		"receipt_kind":             "run",
		"authority":                retrievalCostObservabilityAuthority,
		"server_owned":             true,
		"evaluation_holdout":       true,
		"allowed_transport":        "in_process",
		"provider_policy":          "provider_incapable_in_process_only",
		"redirect_escape_disabled": true,
		"eligible":                 totals.sourcePolicyObserved && totals.sourcePolicyConsistent && totals.costObservationObserved == totals.costObservationExpected && totals.costObservationMissing == 0,
		"expected_case_count":      totals.costObservationExpected,
		"observed_case_count":      totals.costObservationObserved,
		"policy_digests":           digests,
		"source_runtime_identity":  contextLatticeBuildIdentity(),
	}
	receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt)
	return receipt
}

func graphRecallEconomicsObservationComplete(response map[string]any) bool {
	seam := anyMap(response["cost_observability"])
	if anyToString(seam["schema_id"]) != retrievalCostObservabilitySchemaID || anyToString(seam["authority"]) != retrievalCostObservabilityAuthority || !anyToBool(seam["proven_zero"]) {
		return false
	}
	if anyToString(seam["traffic_class"]) != "evaluation_holdout" {
		return false
	}
	for _, key := range []string{"network_calls", "network_calls_observed", "local_backend_calls", "local_backend_calls_observed", "external_network_calls", "external_network_calls_observed", "external_network_zero_proven", "provider_calls", "provider_calls_observed", "provider_tokens", "provider_tokens_observed", "provider_cost_microusd", "provider_cost_observed", "transport_observed", "transport_classification", "continuation_sources", "coverage_rescue_applied", "rust_quality_fallback_applied", "traffic_class", "source_runtime_identity", "source_policy_preflight", "pre_execution_policy_enforced", "captured_at"} {
		if _, present := seam[key]; !present {
			return false
		}
	}
	if !anyToBool(seam["network_calls_observed"]) || !anyToBool(seam["local_backend_calls_observed"]) || !anyToBool(seam["external_network_calls_observed"]) || !anyToBool(seam["external_network_zero_proven"]) || !anyToBool(seam["provider_calls_observed"]) || !anyToBool(seam["provider_tokens_observed"]) || !anyToBool(seam["provider_cost_observed"]) || !anyToBool(seam["transport_observed"]) || !anyToBool(seam["pre_execution_policy_enforced"]) || !retrievalEvaluationSourcePreflightValid(anyMap(seam["source_policy_preflight"])) || len(anyMap(seam["transport_classification"])) == 0 || len(anyToStringSlice(seam["continuation_sources"])) > 0 || anyToBool(seam["coverage_rescue_applied"]) || anyToBool(seam["rust_quality_fallback_applied"]) {
		return false
	}
	for _, raw := range anyMap(seam["transport_classification"]) {
		class := strings.TrimSpace(strings.ToLower(anyToString(anyMap(raw)["class"])))
		if class != "in_process" && class != "approved_local_endpoint" {
			return false
		}
	}
	if anyToInt(seam["external_network_calls"], 0) != 0 || anyToInt(seam["provider_calls"], 0) != 0 || anyToInt(seam["provider_tokens"], 0) != 0 || anyToInt(seam["provider_cost_microusd"], 0) != 0 {
		return false
	}
	if !graphRecallSourcePolicyReceiptValid(anyMap(seam["source_policy"])) {
		return false
	}
	identity := anyMap(seam["source_runtime_identity"])
	if !anyToBool(identity["source_bound"]) || strings.TrimSpace(anyToString(identity["source_commit"])) == "" || strings.TrimSpace(anyToString(identity["source_tree"])) == "" {
		return false
	}
	currentIdentity := contextLatticeBuildIdentity()
	if !anyToBool(currentIdentity["source_bound"]) || !strings.EqualFold(anyToString(identity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(identity["source_tree"]), anyToString(currentIdentity["source_tree"])) {
		return false
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(seam["captured_at"]))); err != nil || capturedAt.IsZero() {
		return false
	}
	return true
}

func graphRecallRecordEconomics(response map[string]any, totals *graphRecallEvaluationTotals) {
	if totals == nil {
		return
	}
	totals.costObservationExpected++
	graphRecallObservedEconomics(response, totals)
	if graphRecallEconomicsObservationComplete(response) {
		totals.costObservationObserved++
	} else {
		totals.costObservationMissing++
	}
}

func graphRecallQualityScoreFromSample(raw map[string]any) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	if anyToString(raw["authority"]) != "execute_retrieval_server" || anyToString(raw["scorer_schema_id"]) != contextPackQualitySchemaID || anyToInt(raw["scorer_version"], 0) != 1 || strings.TrimSpace(anyToString(raw["case_id"])) == "" || strings.TrimSpace(anyToString(raw["case_set_digest"])) == "" || strings.TrimSpace(anyToString(raw["snapshot_digest"])) == "" {
		return 0, false
	}
	identity := anyMap(raw["source_runtime_identity"])
	currentIdentity := contextLatticeBuildIdentity()
	if !anyToBool(identity["source_bound"]) || !anyToBool(currentIdentity["source_bound"]) || !strings.EqualFold(anyToString(identity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(identity["source_tree"]), anyToString(currentIdentity["source_tree"])) {
		return 0, false
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(raw["captured_at"]))); err != nil || capturedAt.IsZero() {
		return 0, false
	}
	if !graphRecallFinalModelVisibleProjectionValid(raw) {
		return 0, false
	}
	signals := anyMap(raw["signals"])
	if len(signals) == 0 {
		signals = anyMap(raw["quality_signals"])
	}
	if len(signals) == 0 {
		return 0, false
	}
	// Reuse the unchanged product scorer. A raw caller-supplied score is not
	// sufficient evidence for promotion because it cannot prove the formula.
	intSignal := func(names ...string) (int, bool) {
		for _, name := range names {
			if value, present := signals[name]; present {
				return anyToInt(value, 0), true
			}
		}
		return 0, false
	}
	boolSignal := func(names ...string) (bool, bool) {
		for _, name := range names {
			if value, present := signals[name]; present {
				return anyToBool(value), true
			}
		}
		return false, false
	}
	ranked, rankedOK := intSignal("rankedEvidenceCount", "ranked_evidence_count")
	highImpact, highImpactOK := intSignal("highImpactCount", "high_impact_count", "high_impact_evidence_count")
	omitted, omittedOK := intSignal("omittedHighValueCount", "omitted_high_value_count")
	returned, returnedOK := intSignal("returnedSourceCount", "returned_source_count")
	warnings, warningsOK := intSignal("warningCount", "warning_count")
	tokenizer, tokenizerOK := boolSignal("tokenizerExact", "tokenizer_exact")
	tokenBudget, tokenBudgetOK := boolSignal("tokenBudgetActive", "token_budget_active")
	coverage, coverageOK := boolSignal("sourceCoverageComplete", "source_coverage_complete")
	graphUsed, graphUsedOK := boolSignal("graphUsed", "graph_used", "graph_context_used")
	exactSaved, exactSavedOK := intSignal("exactPromptSaved", "exact_prompt_saved", "exact_prompt_tokens_saved")
	if !rankedOK || !highImpactOK || !omittedOK || !returnedOK || !warningsOK || !tokenizerOK || !tokenBudgetOK || !coverageOK || !graphUsedOK || !exactSavedOK {
		return 0, false
	}
	parsed := contextPackQualitySignals{
		RankedEvidenceCount:    ranked,
		HighImpactCount:        highImpact,
		OmittedHighValueCount:  omitted,
		ReturnedSourceCount:    returned,
		WarningCount:           warnings,
		TokenizerExact:         tokenizer,
		TokenBudgetActive:      tokenBudget,
		SourceCoverageComplete: coverage,
		GraphUsed:              graphUsed,
		ExactPromptSaved:       exactSaved,
	}
	return contextPackQualityScore(parsed), true
}

func graphRecallQualityCalibrationSnapshotForCohort(caseSetDigest, snapshotDigest string, cases, reports []map[string]any, _ map[string]any) map[string]any {
	result := map[string]any{
		"schema_id": "context_pack_quality_calibration.v1", "formula": "unchanged_contextPackQualityScore_0_to_100",
		"available": false, "same_snapshot": false, "case_set_digest": caseSetDigest, "snapshot_digest": snapshotDigest,
		"mean": nil, "p10": nil, "sample_count": 0, "calibration_grade": "unavailable", "cohort_complete": false,
	}
	if strings.TrimSpace(caseSetDigest) == "" || strings.TrimSpace(snapshotDigest) == "" {
		result["reason"] = "graph_snapshot_binding_missing"
		return result
	}
	expectedPositiveIDs := map[string]struct{}{}
	for _, rawCase := range cases {
		if anyToBool(rawCase["hard_negative"]) {
			continue
		}
		caseID := strings.TrimSpace(anyToString(rawCase["id"]))
		if caseID != "" {
			expectedPositiveIDs[caseID] = struct{}{}
		}
	}
	result["expected_case_count"] = len(expectedPositiveIDs)
	if len(expectedPositiveIDs) == 0 {
		result["reason"] = "same_snapshot_graph_quality_cohort_incomplete"
		return result
	}
	values := make([]float64, 0, len(expectedPositiveIDs))
	seenPositiveIDs := map[string]struct{}{}
	evaluated := 0
	for _, report := range reports {
		caseID := strings.TrimSpace(anyToString(report["id"]))
		if _, expected := expectedPositiveIDs[caseID]; !expected {
			continue
		}
		if _, duplicate := seenPositiveIDs[caseID]; duplicate {
			result["reason"] = "same_snapshot_graph_quality_duplicate_positive_case"
			result["cohort_count"] = evaluated
			return result
		}
		seenPositiveIDs[caseID] = struct{}{}
		evaluated++
		if !strings.EqualFold(anyToString(report["status"]), "evaluated") {
			continue
		}
		sample := anyMap(report["context_quality_sample"])
		if !strings.EqualFold(strings.TrimSpace(anyToString(sample["case_id"])), caseID) || !strings.EqualFold(strings.TrimSpace(anyToString(sample["case_set_digest"])), strings.TrimSpace(caseSetDigest)) || !strings.EqualFold(strings.TrimSpace(anyToString(sample["snapshot_digest"])), strings.TrimSpace(snapshotDigest)) {
			continue
		}
		if score, ok := graphRecallQualityScoreFromSample(sample); ok {
			values = append(values, float64(score))
		}
	}
	if evaluated != len(expectedPositiveIDs) || len(seenPositiveIDs) != len(expectedPositiveIDs) || len(values) != len(expectedPositiveIDs) {
		result["reason"] = "same_snapshot_graph_quality_cohort_incomplete"
		result["sample_count"] = len(values)
		result["cohort_count"] = evaluated
		return result
	}
	sort.Float64s(values)
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	result["available"] = true
	result["same_snapshot"] = true
	result["cohort_complete"] = true
	result["mean"] = roundFloat(sum/float64(len(values)), 6)
	result["p10"] = roundFloat(percentileFloat(values, 0.10), 6)
	result["sample_count"] = len(values)
	result["calibration_grade"] = "same_snapshot_scorer"
	return result
}

func graphRecallPromotionGate(graphActive bool, direct, quality, graph, corpus map[string]any) map[string]any {
	blocked := make([]string, 0)
	block := func(code string) { blocked = append(blocked, code) }
	if !graphActive {
		block("graph_operation_disabled")
	}
	directPassed := anyToBool(direct["passed"]) || anyToBool(direct["directPassed"])
	directNoRegression := directPassed
	baseline := anyMap(direct["baseline"])
	directMetricKeys := []string{"recallAtK", "mrr", "numericExactness", "citationCoverage", "sourceDiversity"}
	if len(baseline) == 0 {
		block("direct_baseline_unavailable")
		directNoRegression = false
	} else {
		binding := anyMap(corpus["direct_baseline_binding"])
		baselineBinding := anyMap(baseline["binding"])
		if len(baselineBinding) == 0 {
			baselineBinding = baseline
		}
		for _, key := range []string{"schema_id", "version", "case_set_digest", "snapshot_digest", "k"} {
			if strings.TrimSpace(anyToString(binding[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(binding[key])), strings.TrimSpace(anyToString(baselineBinding[key]))) {
				block("direct_baseline_binding_mismatch")
				break
			}
		}
		for _, key := range []string{"evaluation_split", "evaluation_case_set_digest", "evaluation_case_count", "evaluation_traffic_class"} {
			if expected, present := binding[key]; present {
				if key == "evaluation_case_count" {
					if anyToInt(expected, 0) != anyToInt(baselineBinding[key], 0) {
						block("direct_baseline_binding_mismatch")
					}
					continue
				}
				if strings.TrimSpace(anyToString(expected)) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(expected)), strings.TrimSpace(anyToString(baselineBinding[key]))) {
					block("direct_baseline_binding_mismatch")
				}
			}
		}
		for _, key := range directMetricKeys {
			current, currentPresent := graphRecallMetricValue(direct, key)
			prior, priorPresent := graphRecallMetricValue(baseline, key)
			if !currentPresent || !priorPresent || !isFiniteGraphMetric(current) || !isFiniteGraphMetric(prior) {
				block("direct_baseline_metric_missing")
				directNoRegression = false
				continue
			}
			if current < prior {
				directNoRegression = false
			}
		}
		baselineReceipt := anyMap(direct["baseline_receipt"])
		if !anyToBool(baselineReceipt["available"]) || strings.TrimSpace(anyToString(baselineReceipt["digest"])) == "" {
			block("direct_baseline_receipt_unavailable")
			directNoRegression = false
		}
	}
	if !directPassed {
		block("direct_metrics_failed")
	}
	if present, ok := direct["binding_valid"]; ok && !anyToBool(present) {
		block("direct_cohort_binding_mismatch")
	}
	if !directNoRegression {
		block("direct_metrics_regressed")
	}
	evaluationSplit := strings.ToLower(strings.TrimSpace(anyToString(corpus["evaluation_split"])))
	expectedCaseCount := anyToInt(corpus["evaluation_case_count"], 0)
	expectedPositive := anyToInt(corpus["evaluation_positive_cases"], 0)
	expectedHardNegatives := anyToInt(corpus["evaluation_hard_negative_cases"], 0)
	expectedIncremental := anyToInt(corpus["evaluation_incremental_cases"], 0)
	if evaluationSplit != "holdout" || expectedCaseCount <= 0 || expectedPositive <= 0 || expectedHardNegatives <= 0 || expectedIncremental <= 0 {
		block("graph_evaluation_denominator_unbound")
	}
	if graphActive && anyToInt(graph["explicit_cases"], anyToInt(graph["explicitCases"], 0)) == 0 {
		block("graph_active_zero_explicit_cases")
	}
	pairedLatencyExpected := expectedPositive
	pairedLatencyCases := anyToInt(graph["paired_latency_cases"], anyToInt(anyMap(graph["paired_latency"])["paired_cases"], 0))
	pairedLatencyFailures := anyToInt(graph["paired_latency_failures"], anyToInt(anyMap(graph["paired_latency"])["failed_cases"], 0))
	pairedLatencyValid, pairedLatencyWithinBudget := graphRecallPairedLatencyEvidenceValid(anyMap(graph["paired_latency"]), pairedLatencyExpected)
	if !anyToBool(graph["latency_comparable"]) || pairedLatencyExpected <= 0 || pairedLatencyCases != pairedLatencyExpected || pairedLatencyFailures != 0 || !pairedLatencyValid {
		block("graph_latency_incomparable")
	} else if !pairedLatencyWithinBudget {
		block("graph_latency_regression_budget_exceeded")
	}
	if !anyToBool(corpus["benchmark_eligible"]) || !anyToBool(corpus["valid"]) {
		block("graph_corpus_not_benchmark_eligible")
	}
	validation := anyMap(corpus["validation_receipt"])
	expectedCorpusCount := anyToInt(corpus["case_count"], 0)
	if anyToString(validation["schema_id"]) != "saved_recall_graph_validation.v1" || anyToInt(validation["version"], 0) != 1 || anyToString(validation["authority"]) != savedRecallGraphCorpusOwner || !anyToBool(validation["server_owned"]) || !anyToBool(validation["valid"]) || !anyToBool(validation["benchmark_eligible"]) || expectedCorpusCount <= 0 || anyToInt(validation["case_count"], 0) != expectedCorpusCount || !strings.EqualFold(anyToString(validation["case_set_digest"]), anyToString(corpus["case_set_digest"])) || !strings.EqualFold(anyToString(validation["manifest_digest"]), anyToString(corpus["manifest_digest"])) || !strings.EqualFold(anyToString(validation["custody_case_set_digest"]), strings.TrimSpace(anyToString(anyMap(corpus["custody"])["case_set_digest"]))) || strings.TrimSpace(anyToString(validation["digest"])) == "" || !strings.EqualFold(anyToString(validation["digest"]), "sha256:"+graphCorpusDigestMap(validation, "digest")) {
		block("graph_corpus_validation_receipt_unbound")
	}
	qualityMean := anyToFloat(quality["mean"])
	qualityP10 := anyToFloat(quality["p10"])
	qualityAvailable := anyToBool(quality["available"]) && anyToBool(quality["same_snapshot"]) && anyToBool(quality["cohort_complete"]) && anyToString(quality["formula"]) == "unchanged_contextPackQualityScore_0_to_100"
	if expectedPositive > 0 && (anyToInt(quality["expected_case_count"], 0) != expectedPositive || anyToInt(quality["sample_count"], 0) != expectedPositive) {
		qualityAvailable = false
		block("context_quality_cohort_incomplete")
	}
	if !qualityAvailable {
		block("context_quality_calibration_unavailable")
	} else {
		if qualityMean < 90 {
			block("context_quality_mean_below_90")
		}
		if qualityP10 < 90 {
			block("context_quality_p10_below_90")
		}
	}
	positive := anyToInt(graph["positive_cases"], anyToInt(graph["positiveCases"], 0))
	graphHits := anyToInt(graph["graph_hits"], anyToInt(graph["graphHits"], 0))
	graphAttributedHits := anyToInt(graph["graph_attributed_hits"], -1)
	graphAttributedDenominator := anyToInt(graph["graph_attributed_denominator"], -1)
	if anyToString(graph["graph_attribution_binding"]) != "finalized_visible_graph_provenance.v1" || graphAttributedHits < 0 || graphAttributedDenominator <= 0 || graphAttributedHits != graphHits || graphAttributedDenominator != positive || graphAttributedHits > graphAttributedDenominator {
		block("graph_attribution_binding_incomplete")
	} else {
		// The promotion numerator/denominator are the server-owned attribution
		// counters, never a caller-supplied ratio detached from final evidence.
		graphHits = graphAttributedHits
		positive = graphAttributedDenominator
	}
	graphRecall := anyToFloat(graph["graph_recall_at_5"])
	if expectedPositive > 0 && positive != expectedPositive {
		block("graph_case_denominator_incomplete")
	}
	if expectedCaseCount > 0 && (anyToInt(graph["cases_expected"], 0) != expectedCaseCount || anyToInt(graph["cases_attempted"], 0) != expectedCaseCount || anyToInt(graph["cases_evaluated"], 0) != expectedCaseCount || anyToInt(graph["cases_terminal"], 0) != expectedCaseCount || anyToInt(graph["case_failures"], 0) != 0) {
		block("graph_case_denominator_incomplete")
	}
	if expectedPositive > 0 && (anyToInt(graph["positive_expected"], 0) != expectedPositive || anyToInt(graph["positive_failures"], 0) != 0 || anyToInt(graph["explicit_cases"], anyToInt(graph["explicitCases"], 0)) != expectedPositive) {
		block("graph_positive_cohort_incomplete")
	}
	if expectedHardNegatives > 0 && (anyToInt(graph["hard_negative_expected"], 0) != expectedHardNegatives || anyToInt(graph["hard_negative_cases"], anyToInt(graph["hardNegativeCases"], 0)) != expectedHardNegatives || anyToInt(graph["hard_negative_failures"], 0) != 0 || anyToInt(graph["hard_negative_oracle_available"], 0) != expectedHardNegatives) {
		block("hard_negative_cohort_incomplete")
	}
	if !graphRecallHardNegativeCurrentIdentityReceiptValid(anyMap(graph["hard_negative_current_identity"]), expectedHardNegatives, anyToString(corpus["source_edge_snapshot_digest"])) {
		block("hard_negative_current_identity_unbound")
	}
	if expectedIncremental > 0 && anyToInt(graph["incremental_denominator"], anyToInt(graph["incrementalDenominator"], 0)) != expectedIncremental {
		block("incremental_denominator_binding_mismatch")
	}
	if positive <= 0 {
		block("graph_recall_denominator_missing")
	} else if graphRecall == 0 {
		graphRecall = float64(graphHits) / float64(positive)
	}
	if graphRecall < savedRecallGraphCorpusGraphRecallGate {
		block("graph_recall_at_5_below_90")
	}
	incrementalDenominator := anyToInt(graph["incremental_denominator"], anyToInt(graph["incrementalDenominator"], 0))
	incrementalHelp := anyToFloat(graph["incremental_help"])
	if incrementalDenominator < savedRecallGraphCorpusMinIncremental {
		block("incremental_denominator_below_30")
	} else if incrementalHelp < savedRecallGraphCorpusIncrementalGate {
		block("incremental_help_below_90")
	}
	negativeTotal := anyToInt(graph["hard_negative_cases"], anyToInt(graph["hardNegativeCases"], 0))
	negativePassed := anyToInt(graph["hard_negative_passed"], anyToInt(graph["hardNegativePassed"], 0))
	if negativeTotal != savedRecallGraphCorpusHardNegatives || (expectedHardNegatives > 0 && negativeTotal != expectedHardNegatives) || negativePassed != savedRecallGraphCorpusHardNegatives || (negativeTotal > 0 && float64(negativePassed)/float64(negativeTotal) < savedRecallGraphCorpusNegativeGate) {
		block("hard_negative_gate_failed")
	}
	cost := anyMap(corpus["cost"])
	if len(cost) == 0 {
		cost = anyMap(graph["cost"])
	}
	if anyToString(cost["schema_id"]) != retrievalCostObservabilitySchemaID || anyToString(cost["authority"]) != retrievalCostObservabilityAuthority || !anyToBool(cost["transport_observed"]) || !anyToBool(cost["proven_zero"]) {
		block("cost_observability_unknown")
	}
	policyRun := anyMap(cost["source_policy_run"])
	if anyToString(policyRun["schema_id"]) != retrievalEvaluationSourcePolicySchemaID || anyToInt(policyRun["version"], 0) != retrievalEvaluationSourcePolicyVersion || anyToString(policyRun["receipt_kind"]) != "run" || anyToString(policyRun["authority"]) != retrievalCostObservabilityAuthority || !anyToBool(policyRun["server_owned"]) || !anyToBool(policyRun["evaluation_holdout"]) || !anyToBool(policyRun["eligible"]) || anyToString(policyRun["allowed_transport"]) != "in_process" || anyToString(policyRun["provider_policy"]) != "provider_incapable_in_process_only" || !anyToBool(policyRun["redirect_escape_disabled"]) || len(anyToStringSlice(policyRun["policy_digests"])) == 0 {
		block("cost_source_policy_unbound")
	}
	if strings.TrimSpace(anyToString(policyRun["digest"])) == "" || !strings.EqualFold(anyToString(policyRun["digest"]), "sha256:"+graphCorpusDigestMap(policyRun, "digest")) || anyToInt(policyRun["expected_case_count"], 0) != anyToInt(cost["observation_expected"], 0) || anyToInt(policyRun["observed_case_count"], 0) != anyToInt(cost["observation_observed"], 0) || !anyToBool(cost["source_policy_observed"]) || !anyToBool(cost["source_policy_consistent"]) {
		block("cost_source_policy_unbound")
	}
	runIdentity := anyMap(policyRun["source_runtime_identity"])
	currentIdentity := contextLatticeBuildIdentity()
	if !anyToBool(runIdentity["source_bound"]) || !anyToBool(currentIdentity["source_bound"]) || !strings.EqualFold(anyToString(runIdentity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(runIdentity["source_tree"]), anyToString(currentIdentity["source_tree"])) {
		block("cost_source_policy_unbound")
	}
	for _, field := range []string{"network_calls_observed", "local_backend_calls_observed", "provider_calls_observed", "provider_tokens_observed", "provider_cost_observed", "external_network_calls_observed", "external_network_zero_proven"} {
		if !anyToBool(cost[field]) {
			block("cost_observability_unknown")
			break
		}
	}
	observationExpected := anyToInt(cost["observation_expected"], 0)
	observationRequired := anyToInt(cost["observation_expected_required"], 0)
	if observationRequired <= 0 || observationExpected != observationRequired || observationExpected <= 0 || anyToInt(cost["observation_observed"], 0) != observationExpected || anyToInt(cost["observation_missing"], 0) != 0 {
		block("cost_observability_incomplete")
	}
	if anyToInt(cost["external_network_calls"], 0) != 0 || !anyToBool(cost["external_network_zero_proven"]) {
		block("external_network_nonzero_or_unproven")
	}
	if anyToInt(cost["provider_calls"], 0) != 0 {
		block("provider_calls_nonzero")
	}
	if anyToInt(cost["provider_tokens"], 0) != 0 {
		block("provider_tokens_nonzero")
	}
	if anyToInt(cost["provider_cost_microusd"], 0) != 0 {
		block("provider_cost_nonzero")
	}
	if strings.TrimSpace(anyToString(cost["traffic_class"])) != "evaluation_holdout" {
		block("invalid_evaluation_traffic_class")
	}
	if anyToBool(corpus["runtime_identity_required"]) {
		currentIdentity := contextLatticeBuildIdentity()
		recordedIdentity := anyMap(corpus["runtime_identity"])
		if !anyToBool(recordedIdentity["source_bound"]) || !anyToBool(currentIdentity["source_bound"]) || !strings.EqualFold(anyToString(recordedIdentity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(recordedIdentity["source_tree"]), anyToString(currentIdentity["source_tree"])) {
			block("graph_runtime_identity_mismatch")
		}
		if strings.TrimSpace(anyToString(corpus["case_set_digest"])) == "" || strings.TrimSpace(anyToString(corpus["graph_snapshot_digest"])) == "" || strings.TrimSpace(anyToString(corpus["edge_snapshot_digest"])) == "" || strings.TrimSpace(anyToString(corpus["source_edge_snapshot_digest"])) == "" || strings.TrimSpace(anyToString(corpus["baseline_policy_digest"])) == "" || strings.TrimSpace(anyToString(corpus["evaluation_captured_at"])) == "" {
			block("graph_evidence_binding_incomplete")
		}
		if !anyToBool(corpus["evaluation_snapshot_stable"]) || !strings.EqualFold(anyToString(corpus["evaluation_snapshot_start_digest"]), anyToString(corpus["source_edge_snapshot_digest"])) || !strings.EqualFold(anyToString(corpus["evaluation_snapshot_end_digest"]), anyToString(corpus["source_edge_snapshot_digest"])) {
			block("graph_evaluation_snapshot_changed")
		}
		if trafficClass := strings.TrimSpace(anyToString(corpus["evaluation_traffic_class"])); trafficClass != "evaluation_holdout" {
			block("invalid_evaluation_traffic_class")
		}
	}
	blocked = graphCorpusSortedStrings(blocked)
	gates := map[string]any{
		"direct_no_regression": directNoRegression,
		"direct_metrics":       map[string]any{"current": direct, "baseline": baseline, "required": directMetricKeys},
		"context_quality":      map[string]any{"mean": quality["mean"], "p10": quality["p10"], "min": 90, "formula": "unchanged_contextPackQualityScore_0_to_100", "same_snapshot": quality["same_snapshot"]},
		"graph_recall_at_5":    map[string]any{"value": roundFloat(graphRecall, 6), "min": savedRecallGraphCorpusGraphRecallGate, "denominator": positive},
		"incremental_help":     map[string]any{"value": roundFloat(incrementalHelp, 6), "min": savedRecallGraphCorpusIncrementalGate, "denominator": incrementalDenominator},
		"hard_negatives":       map[string]any{"passed": negativePassed, "total": negativeTotal, "required": savedRecallGraphCorpusHardNegatives},
		"paired_latency":       cloneJSONMap(anyMap(graph["paired_latency"])),
	}
	return map[string]any{
		"promotion_eligible":   len(blocked) == 0,
		"passed":               len(blocked) == 0,
		"blocked_reasons":      blocked,
		"gates":                gates,
		"direct_no_regression": directNoRegression,
	}
}

func graphRecallMetricValue(metrics map[string]any, key string) (float64, bool) {
	if metrics == nil {
		return 0, false
	}
	if value, ok := metrics[key]; ok {
		return anyToFloat(value), true
	}
	snake := strings.ToLower(key)
	snake = strings.ReplaceAll(snake, "at", "_at_")
	snake = strings.ReplaceAll(snake, "exactness", "_exactness")
	snake = strings.ReplaceAll(snake, "coverage", "_coverage")
	snake = strings.ReplaceAll(snake, "diversity", "_diversity")
	if value, ok := metrics[snake]; ok {
		return anyToFloat(value), true
	}
	return 0, false
}

func isFiniteGraphMetric(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func graphRecallCaseReportBase(rawCase map[string]any, k int) map[string]any {
	return map[string]any{
		"id": anyToString(rawCase["id"]), "split": anyToString(rawCase["split"]),
		"relation": anyToString(rawCase["relation"]), "case_kind": anyToString(rawCase["case_kind"]),
		"project": anyToString(rawCase["project"]), "agent_family": anyToString(rawCase["agent_family"]),
		"session_ref": sha256Hex(anyToString(rawCase["session_id"])), "seed_memory_ref": graphRecallOpaqueMemoryRef(rawCase["seed_memory_id"]),
		"target_memory_ref": graphRecallOpaqueMemoryRef(rawCase["target_memory_id"]), "k": k,
		"incremental_needed_label": anyToBool(rawCase["incremental_needed"]),
	}
}

func graphRecallOpaqueMemoryRef(value any) string {
	trimmed := strings.TrimSpace(anyToString(value))
	if trimmed == "" {
		return ""
	}
	return "sha256:" + sha256Hex(trimmed)
}

func graphRecallPublicContribution(value map[string]any) map[string]any {
	if len(value) == 0 {
		return map[string]any{}
	}
	allowed := []string{"enabled", "reason", "seed_count", "candidate_count", "added_candidate_count", "expected_hit_count", "added_expected_hit_count", "edge_expected_match_count", "hydrated_expected_hit_count", "added_hydrated_expected_hit_count", "helped", "relations", "relation_counts"}
	result := map[string]any{}
	for _, key := range allowed {
		if raw, ok := value[key]; ok {
			result[key] = raw
		}
	}
	return result
}

func graphRecallResultContainsMemoryID(results []map[string]any, expected string, k int) bool {
	_, _, canonicalExpected, _, err := canonicalMemoryID(expected)
	if err != nil {
		canonicalExpected = strings.TrimSpace(expected)
	}
	for index, row := range results {
		if index >= clampInt(k, 1, 100) {
			break
		}
		if strings.EqualFold(strings.TrimSpace(recallResultMemoryID(row)), strings.TrimSpace(canonicalExpected)) {
			return true
		}
	}
	return false
}

func graphRecallResultContainsTarget(results []map[string]any, expectedMemoryID string, expectedFiles map[string]struct{}, k int) bool {
	return graphRecallResultContainsTargetForProject(results, expectedMemoryID, expectedFiles, k, "")
}

func graphRecallResultContainsTargetForProject(results []map[string]any, expectedMemoryID string, expectedFiles map[string]struct{}, k int, project string) bool {
	if strings.TrimSpace(expectedMemoryID) != "" && graphRecallResultContainsMemoryID(results, expectedMemoryID, k) {
		return true
	}
	return len(expectedFiles) > 0 && matchRankWithinKProject(results, expectedFiles, nil, k, project) != nil
}

func graphRecallMemoryIDEqual(left, right string) bool {
	_, _, leftCanonical, _, leftErr := canonicalMemoryID(strings.TrimSpace(left))
	_, _, rightCanonical, _, rightErr := canonicalMemoryID(strings.TrimSpace(right))
	if leftErr == nil && rightErr == nil {
		return strings.EqualFold(leftCanonical, rightCanonical)
	}
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

const (
	graphRecallHardNegativeCurrentIdentitySchemaID = "saved_recall_graph_hard_negative_current_identity.v1"
	graphRecallHardNegativeCurrentIdentityVersion  = 1
)

// graphRecallPublicFailure is the boundary-safe representation of an
// evaluation failure. Case identifiers are not public evidence: callers get a
// stable category and an opaque case reference, while the owner can still
// correlate the hashed reference with its private run receipt.
type graphRecallPublicFailure struct {
	Code    string
	CaseRef string
}

func (failure *graphRecallPublicFailure) Error() string {
	if failure == nil || strings.TrimSpace(failure.Code) == "" {
		return "graph_evaluation_failed"
	}
	return strings.TrimSpace(failure.Code)
}

func graphRecallOpaqueCaseRef(caseID string) string {
	caseID = strings.TrimSpace(caseID)
	if caseID == "" {
		caseID = "unknown"
	}
	return "sha256:" + sha256Hex(caseID)
}

func graphRecallPublicFailureForCase(code, caseID string) error {
	return &graphRecallPublicFailure{Code: strings.TrimSpace(code), CaseRef: graphRecallOpaqueCaseRef(caseID)}
}

func graphRecallPublicFailureDetails(err error) map[string]any {
	detail := map[string]any{"code": "graph_evaluation_failed", "case_ref": graphRecallOpaqueCaseRef("")}
	var failure *graphRecallPublicFailure
	if errors.As(err, &failure) && failure != nil {
		if strings.TrimSpace(failure.Code) != "" {
			detail["code"] = strings.TrimSpace(failure.Code)
		}
		if strings.TrimSpace(failure.CaseRef) != "" {
			detail["case_ref"] = strings.TrimSpace(failure.CaseRef)
		}
	}
	return detail
}

func graphRecallExecutionErrorCode(status int, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout {
		return "graph_retrieval_timeout"
	}
	if status >= http.StatusInternalServerError {
		return "graph_retrieval_server_error"
	}
	if status >= http.StatusBadRequest {
		return "graph_retrieval_rejected"
	}
	return "graph_retrieval_failed"
}

func graphRecallHardNegativeCanonicalIdentity(row map[string]any) (string, string, string, error) {
	project := strings.TrimSpace(anyToString(row["project"]))
	files := anyToStringSlice(row["forbidden_graph_files"])
	ids := anyToStringSlice(row["forbidden_memory_ids"])
	if project == "" || len(files) != 1 || len(ids) != 1 {
		return "", "", "", graphRecallPublicFailureForCase("hard_negative_identity_missing", anyToString(row["id"]))
	}
	fileName := strings.Trim(strings.TrimSpace(strings.ReplaceAll(files[0], "\\", "/")), "/")
	canonicalProject, canonicalFile, canonicalID, _, err := canonicalMemoryID(project + "::" + fileName)
	if err != nil {
		return "", "", "", graphRecallPublicFailureForCase("hard_negative_identity_invalid", anyToString(row["id"]))
	}
	rawID := strings.TrimSpace(ids[0])
	_, _, normalizedRawID, _, rawIDErr := canonicalMemoryID(rawID)
	oracle := anyMap(row["negative_oracle"])
	if rawIDErr != nil || rawID != normalizedRawID || canonicalID != rawID || canonicalProject != project || canonicalFile != strings.Trim(fileName, "/") || anyToString(row["target_memory_id"]) != canonicalID || anyToString(row["graph_target_memory_id"]) != canonicalID || anyToString(row["negative_target_memory_id"]) != canonicalID || anyToString(oracle["forbidden_target"]) != canonicalID || strings.Trim(anyToString(oracle["forbidden_file"]), "/") != canonicalFile {
		return "", "", "", graphRecallPublicFailureForCase("hard_negative_identity_mismatch", anyToString(row["id"]))
	}
	return canonicalProject, canonicalFile, canonicalID, nil
}

func (s *server) graphRecallHardNegativeCurrentIdentityReceipt(cases []map[string]any, sourceEdgeSnapshotDigest string) (map[string]any, error) {
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return nil, errors.New("current-state memory authority is unavailable")
	}
	identities := make([]string, 0)
	for _, row := range cases {
		if !anyToBool(row["hard_negative"]) {
			continue
		}
		project, fileName, memoryID, err := graphRecallHardNegativeCanonicalIdentity(row)
		if err != nil {
			return nil, err
		}
		entry, exists := s.memoryStore.currentEntry(project, fileName)
		if !exists || !shouldSurfaceMemoryLifecycle(entry.Lifecycle, false) || isEphemeralMemoryIdentity(entry.FileName, entry.TopicPath, entry.Summary, entry.Lifecycle) {
			return nil, graphRecallPublicFailureForCase("hard_negative_current_identity_stale", anyToString(row["id"]))
		}
		_, _, currentID, _, currentErr := canonicalMemoryID(entry.Project + "::" + entry.FileName)
		if currentErr != nil || currentID != memoryID || entry.Project != project || strings.Trim(entry.FileName, "/") != fileName {
			return nil, graphRecallPublicFailureForCase("hard_negative_current_identity_substituted", anyToString(row["id"]))
		}
		identities = append(identities, strings.TrimSpace(anyToString(row["id"]))+"|"+memoryID+"|"+strings.TrimSpace(entry.EventID))
	}
	sort.Strings(identities)
	receipt := map[string]any{
		"schema_id": graphRecallHardNegativeCurrentIdentitySchemaID, "version": graphRecallHardNegativeCurrentIdentityVersion,
		"authority": "gateway_current_state_index", "server_owned": true, "all_current": len(identities) > 0,
		"expected_cases": len(identities), "observed_cases": len(identities),
		"source_edge_snapshot_digest": strings.TrimSpace(sourceEdgeSnapshotDigest),
		"identity_digest":             "sha256:" + graphCorpusDigestMap(map[string]any{"identities": identities}),
		"captured_at":                 nowUTCISO(),
	}
	receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt, "digest")
	return receipt, nil
}

func graphRecallHardNegativeCurrentIdentityReceiptValid(receipt map[string]any, expected int, sourceEdgeSnapshotDigest string) bool {
	return expected > 0 && anyToString(receipt["schema_id"]) == graphRecallHardNegativeCurrentIdentitySchemaID && anyToInt(receipt["version"], 0) == graphRecallHardNegativeCurrentIdentityVersion && anyToString(receipt["authority"]) == "gateway_current_state_index" && anyToBool(receipt["server_owned"]) && anyToBool(receipt["all_current"]) && anyToInt(receipt["expected_cases"], 0) == expected && anyToInt(receipt["observed_cases"], 0) == expected && strings.TrimSpace(anyToString(receipt["identity_digest"])) != "" && strings.EqualFold(strings.TrimSpace(anyToString(receipt["source_edge_snapshot_digest"])), strings.TrimSpace(sourceEdgeSnapshotDigest)) && strings.TrimSpace(anyToString(receipt["digest"])) != "" && strings.EqualFold(anyToString(receipt["digest"]), "sha256:"+graphCorpusDigestMap(receipt, "digest"))
}

func ratioWithEmptyDenominator(matched, expected int) float64 {
	if expected <= 0 {
		return 1
	}
	return float64(matched) / float64(expected)
}

// graphRecallQualitySampleFromServer projects the exact context-pack scorer
// inputs from the already executed retrieval response. It performs no second
// retrieval and accepts no request-body quality receipt. The resulting sample
// is bound to the graph case and frozen graph snapshot before it enters the
// same-snapshot cohort scorer.
func (s *server) graphRecallProductionResponseProjection(ctx context.Context, requestPayload, searchResponse map[string]any, graphDisabled bool) (map[string]any, map[string]any, bool) {
	if s == nil || len(requestPayload) == 0 || len(searchResponse) == 0 || strings.TrimSpace(anyToString(requestPayload["query"])) == "" {
		return nil, nil, false
	}
	request := recallResponseRequestPayload(requestPayload)
	agentID := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["agent_id"]), anyToString(request["agentId"])))
	var finalResponse map[string]any
	var finalContextPack map[string]any
	var quality map[string]any
	competingInterventionsDisabled := false
	compositionHook := func(input contextPackCompilationInput, artifacts contextPackCompilationArtifacts, durable bool) contextPackCompilationArtifacts {
		competingInterventionsDisabled = len(input.ActiveContextPolicy) == 0 && !artifacts.Learned.Armed && !artifacts.Learned.Eligible && !artifacts.Learned.Performed
		composition := recallResponseCompositionInputFromCompilation(request, input, artifacts, durable)
		response := composeRecallResponse(composition)
		response = finalizeRecallResponseTransport(response, agentID, "recall_response", memoryRecallResponsePath)
		if !durable || !recallResponseTransportCandidateValid(response) {
			fallbackComposition := cloneJSONMap(composition)
			delete(fallbackComposition, "_durable_context_pack_quality")
			response = recallResponseProjectFallbackWithServerSilence(fallbackComposition, recallResponseProductionPolicyInput())
			response = finalizeRecallResponseTransport(response, agentID, "recall_response", memoryRecallResponsePath)
		}
		finalResponse = cloneJSONMap(response)
		finalContextPack = cloneJSONMap(artifacts.ContextPack)
		quality = cloneJSONMap(artifacts.Quality)
		// Preserve the production composer's durability decision and exact final
		// projection, but stop before quality, session, token, or outcome writes.
		artifacts.SideEffectsSuppressed = true
		return artifacts
	}
	response, status, err := s.buildContextPackResponseForSurfaceWithOptions(
		ctx,
		nil,
		request,
		"recall_response",
		contextPackResponseBuildOptions{
			compilationHook:            compositionHook,
			useProvidedSearchResponse:  true,
			providedSearchResponse:     searchResponse,
			providedSearchStatus:       http.StatusOK,
			suppressSideEffects:        true,
			graphDisabled:              graphDisabled,
			disableActiveContextPolicy: true,
			disableLearnedActivation:   true,
		},
	)
	if err != nil || status < http.StatusOK || status >= http.StatusMultipleChoices || len(response) == 0 || len(finalResponse) == 0 || len(finalContextPack) == 0 || len(quality) == 0 || !competingInterventionsDisabled {
		return nil, nil, false
	}
	// The production composer emits writeback_required=true by default. The
	// evaluation sample is admissible only when that same seam confirms that
	// its persistence/session/token side effects were suppressed.
	if anyToBool(response["writeback_required"]) {
		return nil, nil, false
	}
	responseDigest := strings.TrimSpace(anyToString(finalResponse["response_digest"]))
	responseScopeDigest := strings.TrimSpace(anyToString(anyMap(finalResponse["request_scope"])["scope_digest"]))
	if anyToString(finalResponse["schema_id"]) != recallResponseContractID || !utilitySHA256DigestValid(responseDigest) || !utilitySHA256DigestValid(responseScopeDigest) || !strings.EqualFold(responseDigest, recallResponseSemanticDigest(finalResponse)) || !recallResponseTransportCandidateValid(finalResponse) {
		return nil, nil, false
	}
	visibleEvidence, visibleMemoryRefs, visibleFileRefs, ordered := graphRecallFinalModelVisibleRefs(finalResponse, finalContextPack)
	projection := map[string]any{
		"production_response_seam":           true,
		"production_final_response_seam":     true,
		"production_response_schema_id":      recallResponseContractID,
		"production_response_digest":         responseDigest,
		"production_response_scope_digest":   responseScopeDigest,
		"production_response_contract_valid": true,
		"side_effects_suppressed":            true,
		"competing_interventions_disabled":   competingInterventionsDisabled,
		"final_model_visible_k":              defaultRecallEvalK,
		"final_model_visible_ordered":        ordered,
		"final_model_visible_evidence":       visibleEvidence,
		"final_model_visible_memory_refs":    visibleMemoryRefs,
		"final_model_visible_file_refs":      visibleFileRefs,
	}
	projection["final_model_visible_digest"] = graphRecallFinalModelVisibleDigest(projection)
	if !graphRecallFinalModelVisibleProjectionValid(projection) {
		return nil, nil, false
	}
	return projection, quality, true
}

func (s *server) graphRecallQualitySampleFromServer(ctx context.Context, requestPayload, searchResponse, graphContribution map[string]any, caseID, caseSetDigest, snapshotDigest string) map[string]any {
	_ = graphContribution
	if strings.TrimSpace(caseID) == "" || strings.TrimSpace(caseSetDigest) == "" || strings.TrimSpace(snapshotDigest) == "" {
		return nil
	}
	projection, quality, ok := s.graphRecallProductionResponseProjection(ctx, requestPayload, searchResponse, false)
	if !ok {
		return nil
	}
	projection["signals"] = quality
	projection["authority"] = "execute_retrieval_server"
	projection["case_id"] = caseID
	projection["case_set_digest"] = caseSetDigest
	projection["snapshot_digest"] = snapshotDigest
	projection["scorer_schema_id"] = contextPackQualitySchemaID
	projection["scorer_version"] = 1
	projection["source_runtime_identity"] = contextLatticeBuildIdentity()
	projection["captured_at"] = nowUTCISO()
	return projection
}

func graphRecallFinalResponseEvidenceRef(scopeDigest string, row map[string]any, index int) string {
	ref := recallResponseCanonicalSourceRef(row, "evidence")
	if ref == "" {
		// Match the response producer's deterministic malformed-row fallback.
		// The source index must not become part of a row's identity because
		// temporal filtering can remove earlier rows before presentation.
		ref = recallResponseScopedOpaqueRef(scopeDigest, "evidence", recallResponseCanonicalJSON(row))
	}
	_ = index
	return ref
}

func graphRecallCompiledCandidateRef(row map[string]any) string {
	if row == nil {
		return ""
	}
	if ref := contextPackOpaqueCandidateRef(firstPresentAny(
		row["candidate_id"],
		anyMap(row["memory_trust_assessment"])["candidate_id"],
	)); ref != "" {
		return ref
	}
	if !strings.EqualFold(strings.TrimSpace(anyToString(row["source"])), memoryEdgeSource) {
		return ""
	}
	assessment := memoryTrustAssessmentForCandidate(
		"graph_neighbor",
		contextPackEvidenceCandidateText(anyToString(row["summary"])),
		map[string]any{
			"project": row["project"], "file": row["file"], "source": row["source"],
			"topic_path": row["topic_path"], "source_owner": row["source_owner"], "memory_id": row["memory_id"],
		},
	)
	return contextPackOpaqueCandidateRef(assessment["candidate_id"])
}

func graphRecallFinalResponseEvidenceSourceForCompiled(scopeDigest string, row map[string]any, index int, compiledContextPack map[string]any) (string, map[string]any) {
	candidateRef := graphRecallCompiledCandidateRef(row)
	if candidateRef != "" {
		carrierRows := append(
			contextPackAnyList(compiledContextPack["_recall_response_source_rows"]),
			contextPackAnyList(compiledContextPack[recallResponseGraphRowsKey])...,
		)
		for _, raw := range carrierRows {
			carrier := anyMap(raw)
			if graphRecallCompiledCandidateRef(carrier) != candidateRef {
				continue
			}
			if ref := recallResponseCanonicalSourceRef(carrier, "evidence"); ref != "" {
				return ref, carrier
			}
		}
	}
	return graphRecallFinalResponseEvidenceRef(scopeDigest, row, index), row
}

func graphRecallFinalModelVisibleRefs(finalResponse, compiledContextPack map[string]any) ([]map[string]any, []string, []string, bool) {
	evidence := make([]map[string]any, 0, defaultRecallEvalK)
	memoryRefs := make([]string, 0, defaultRecallEvalK)
	fileRefs := make([]string, 0, defaultRecallEvalK)
	finalEvidence := contextPackAnyList(finalResponse["evidence"])
	rankedEvidence := contextPackAnyList(compiledContextPack["ranked_evidence"])
	if len(rankedEvidence) == 0 {
		rankedEvidence = contextPackAnyList(compiledContextPack["rankedEvidence"])
	}
	scopeDigest := strings.TrimSpace(anyToString(anyMap(finalResponse["request_scope"])["scope_digest"]))
	claimRefs := map[string]struct{}{}
	for _, raw := range contextPackAnyList(anyMap(finalResponse["answer"])["claim_refs"]) {
		if ref := strings.TrimSpace(anyToString(raw)); ref != "" {
			claimRefs[ref] = struct{}{}
		}
	}
	ordered := scopeDigest != "" && anyToInt(anyMap(finalResponse["state"])["evidence_count"], -1) == len(finalEvidence)
	type rankedVisibleRow struct {
		index int
		row   map[string]any
	}
	rankedByResponseRef := map[string]rankedVisibleRow{}
	for index := 0; index < len(rankedEvidence) && index < recallResponseMaxEvidence; index++ {
		row := anyMap(rankedEvidence[index])
		ref, sourceRow := graphRecallFinalResponseEvidenceSourceForCompiled(scopeDigest, row, index, compiledContextPack)
		if ref == "" {
			continue
		}
		// Duplicate candidate identities collapse to one response row. Bind that
		// row to its earliest compiler rank so a later duplicate cannot promote a
		// rank-six candidate into Recall@5.
		if _, exists := rankedByResponseRef[ref]; !exists {
			rankedByResponseRef[ref] = rankedVisibleRow{index: index, row: sourceRow}
		}
	}
	for finalIndex, raw := range finalEvidence {
		finalRef := strings.TrimSpace(anyToString(anyMap(raw)["ref_id"]))
		sourceIndex := -1
		var row map[string]any
		if ranked, exists := rankedByResponseRef[finalRef]; exists {
			sourceIndex = ranked.index
			row = ranked.row
		} else {
			ordered = false
		}
		if _, claimed := claimRefs[finalRef]; finalRef == "" || !claimed {
			ordered = false
		}
		// Recall@5 is the intersection of evidence that survives the final
		// response and the compiler's original top five. A late compiled row must
		// not be promoted merely because earlier evidence was pruned from the
		// public response.
		if finalIndex >= defaultRecallEvalK || sourceIndex < 0 || sourceIndex >= defaultRecallEvalK {
			continue
		}
		visible := map[string]any{
			"rank": sourceIndex + 1, "response_rank": finalIndex + 1, "response_ref": graphRecallOpaqueMemoryRef(finalRef),
		}
		if memoryID := recallResultMemoryID(row); memoryID != "" {
			ref := graphRecallOpaqueMemoryRef(memoryID)
			visible["memory_ref"] = ref
			memoryRefs = append(memoryRefs, ref)
		}
		if fileName := strings.TrimSpace(anyToString(row["file"])); fileName != "" {
			ref := graphRecallOpaqueMemoryRef(fileName)
			visible["file_ref"] = ref
			fileRefs = append(fileRefs, ref)
		}
		if project := strings.TrimSpace(anyToString(row["project"])); project != "" {
			visible["project_ref"] = graphRecallOpaqueMemoryRef(project)
		}
		if strings.EqualFold(strings.TrimSpace(anyToString(row["source"])), memoryEdgeSource) && strings.EqualFold(strings.TrimSpace(anyToString(row["source_owner"])), sourceOwnerGoNative) {
			seedID := strings.TrimSpace(anyToString(row["seed_memory_id"]))
			edgeID := strings.TrimSpace(anyToString(row["edge_id"]))
			relation := strings.TrimSpace(anyToString(row["relation"]))
			candidateID := recallResultMemoryID(row)
			if seedID != "" && edgeID != "" && relation != "" && candidateID != "" && anyToBool(row["hydrated"]) {
				visible["graph_provenance"] = map[string]any{
					"schema_id": "graph_provenance.v1", "source": memoryEdgeSource, "source_owner": sourceOwnerGoNative,
					"seed_memory_ref": graphRecallOpaqueMemoryRef(seedID), "target_memory_ref": graphRecallOpaqueMemoryRef(candidateID),
					"edge_ref": "sha256:" + sha256Hex(edgeID), "relation": relation,
				}
			}
		}
		evidence = append(evidence, visible)
	}
	return evidence, memoryRefs, fileRefs, ordered
}

func graphRecallFinalModelVisibleDigest(sample map[string]any) string {
	return "sha256:" + graphCorpusDigestMap(map[string]any{
		"k": defaultRecallEvalK, "ordered_evidence": anyToMapSlice(sample["final_model_visible_evidence"]),
		"production_response_schema_id":    anyToString(sample["production_response_schema_id"]),
		"production_response_scope_digest": anyToString(sample["production_response_scope_digest"]),
	})
}

func graphRecallStringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func graphRecallFinalModelVisibleProjectionValid(sample map[string]any) bool {
	if len(sample) == 0 || !anyToBool(sample["production_response_seam"]) || !anyToBool(sample["production_final_response_seam"]) || !anyToBool(sample["production_response_contract_valid"]) || anyToString(sample["production_response_schema_id"]) != recallResponseContractID || !utilitySHA256DigestValid(anyToString(sample["production_response_digest"])) || !utilitySHA256DigestValid(anyToString(sample["production_response_scope_digest"])) || !anyToBool(sample["side_effects_suppressed"]) || !anyToBool(sample["competing_interventions_disabled"]) || anyToInt(sample["final_model_visible_k"], 0) != defaultRecallEvalK || !anyToBool(sample["final_model_visible_ordered"]) {
		return false
	}
	visible := anyToMapSlice(sample["final_model_visible_evidence"])
	if len(visible) > defaultRecallEvalK {
		return false
	}
	lastResponseRank := 0
	seenSourceRanks := map[int]bool{}
	for _, row := range visible {
		sourceRank := anyToInt(row["rank"], 0)
		responseRank := anyToInt(row["response_rank"], 0)
		if sourceRank <= 0 || sourceRank > defaultRecallEvalK || seenSourceRanks[sourceRank] || responseRank <= lastResponseRank || responseRank > defaultRecallEvalK || strings.TrimSpace(anyToString(row["response_ref"])) == "" {
			return false
		}
		seenSourceRanks[sourceRank] = true
		lastResponseRank = responseRank
		if provenance, present := row["graph_provenance"]; present {
			graph := anyMap(provenance)
			if anyToString(graph["schema_id"]) != "graph_provenance.v1" || anyToString(graph["source"]) != memoryEdgeSource || anyToString(graph["source_owner"]) != sourceOwnerGoNative || !utilitySHA256DigestValid(anyToString(graph["seed_memory_ref"])) || !utilitySHA256DigestValid(anyToString(graph["target_memory_ref"])) || !utilitySHA256DigestValid(anyToString(graph["edge_ref"])) || strings.TrimSpace(anyToString(graph["relation"])) == "" || anyToString(graph["target_memory_ref"]) != anyToString(row["memory_ref"]) {
				return false
			}
		}
	}
	memoryRefs := make([]string, 0, len(visible))
	fileRefs := make([]string, 0, len(visible))
	for _, row := range visible {
		if ref := strings.TrimSpace(anyToString(row["memory_ref"])); ref != "" {
			memoryRefs = append(memoryRefs, ref)
		}
		if ref := strings.TrimSpace(anyToString(row["file_ref"])); ref != "" {
			fileRefs = append(fileRefs, ref)
		}
	}
	if !graphRecallStringSlicesEqual(memoryRefs, anyToStringSlice(sample["final_model_visible_memory_refs"])) || !graphRecallStringSlicesEqual(fileRefs, anyToStringSlice(sample["final_model_visible_file_refs"])) {
		return false
	}
	digest := strings.TrimSpace(anyToString(sample["final_model_visible_digest"]))
	return utilitySHA256DigestValid(digest) && strings.EqualFold(digest, graphRecallFinalModelVisibleDigest(sample))
}

func graphRecallFinalModelVisibleContains(sample map[string]any, memoryID string, expectedFiles map[string]struct{}) bool {
	return graphRecallFinalModelVisibleContainsForProject(sample, memoryID, expectedFiles, "")
}

func graphRecallFinalModelVisibleContainsForProject(sample map[string]any, memoryID string, expectedFiles map[string]struct{}, project string) bool {
	if !graphRecallFinalModelVisibleProjectionValid(sample) {
		return false
	}
	visible := anyToMapSlice(sample["final_model_visible_evidence"])
	for _, row := range visible {
		if strings.EqualFold(anyToString(row["memory_ref"]), graphRecallOpaqueMemoryRef(memoryID)) {
			return true
		}
		if strings.TrimSpace(anyToString(row["memory_ref"])) != "" && strings.TrimSpace(memoryID) != "" {
			continue
		}
		if strings.TrimSpace(project) != "" && !strings.EqualFold(anyToString(row["project_ref"]), graphRecallOpaqueMemoryRef(project)) {
			continue
		}
		for file := range expectedFiles {
			if file != "" && strings.EqualFold(anyToString(row["file_ref"]), graphRecallOpaqueMemoryRef(file)) {
				return true
			}
		}
	}
	return false
}

func graphRecallFinalModelVisibleGraphAttribution(sample map[string]any, targetMemoryID string, expectedFiles map[string]struct{}, project string, contribution map[string]any) bool {
	if !anyToBool(contribution["enabled"]) || anyToInt(contribution["added_hydrated_expected_hit_count"], 0) <= 0 || !graphRecallFinalModelVisibleProjectionValid(sample) {
		return false
	}
	targetProject, _, canonicalTarget, _, targetErr := canonicalMemoryID(strings.TrimSpace(targetMemoryID))
	if targetErr != nil || targetProject != strings.TrimSpace(project) {
		return false
	}
	matchedCandidate := false
	for _, candidate := range anyToStringSlice(contribution["added_matched_memory_ids"]) {
		if graphRecallMemoryIDEqual(candidate, canonicalTarget) {
			matchedCandidate = true
			break
		}
	}
	if !matchedCandidate {
		return false
	}
	for _, row := range anyToMapSlice(sample["final_model_visible_evidence"]) {
		if !strings.EqualFold(anyToString(row["memory_ref"]), graphRecallOpaqueMemoryRef(canonicalTarget)) {
			continue
		}
		provenance := anyMap(row["graph_provenance"])
		if anyToString(provenance["target_memory_ref"]) == graphRecallOpaqueMemoryRef(canonicalTarget) && strings.EqualFold(anyToString(row["project_ref"]), graphRecallOpaqueMemoryRef(project)) && anyToString(provenance["source"]) == memoryEdgeSource && anyToString(provenance["source_owner"]) == sourceOwnerGoNative {
			return true
		}
	}
	_ = expectedFiles
	return false
}

func graphRecallDirectBindingMatches(expected, actual map[string]any) bool {
	for _, key := range []string{"schema_id", "version", "case_set_digest", "snapshot_digest", "k"} {
		if strings.TrimSpace(anyToString(expected[key])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(expected[key])), strings.TrimSpace(anyToString(actual[key]))) {
			return false
		}
	}
	for _, key := range []string{"evaluation_split", "evaluation_case_set_digest", "evaluation_case_count", "evaluation_traffic_class"} {
		if expectedValue, present := expected[key]; present {
			if key == "evaluation_case_count" {
				if anyToInt(expectedValue, 0) != anyToInt(actual[key], 0) {
					return false
				}
				continue
			}
			if strings.TrimSpace(anyToString(expectedValue)) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(expectedValue)), strings.TrimSpace(anyToString(actual[key]))) {
				return false
			}
		}
	}
	return true
}

func graphRecallDirectMetricsFromResults(results []savedRecallEvalCaseResult, cfg recallEvalSavedConfig) map[string]any {
	baselineCases, baselineSplit := savedRecallDirectBaselineCases(cfg)
	directCases, directHits := 0, 0
	directReciprocalSum := 0.0
	numericExpected, numericMatched := 0, 0
	citationExpected, citationMatched := 0, 0
	sourceDiversitySum := 0.0
	for _, result := range results {
		if result.retrievalFailed || !result.hasExpectations {
			continue
		}
		directCases++
		if result.hit {
			directHits++
		}
		directReciprocalSum += result.reciprocalRank
		numericExpected += result.numericExpected
		numericMatched += result.numericMatches
		citationExpected += result.citationExpected
		citationMatched += result.citationMatched
		sourceDiversitySum += float64(result.sourceDiversity)
	}
	recallAtK, mrr, sourceDiversity := 0.0, 0.0, 0.0
	if directCases > 0 {
		recallAtK = float64(directHits) / float64(directCases)
		mrr = directReciprocalSum / float64(directCases)
		sourceDiversity = sourceDiversitySum / float64(directCases)
	}
	numericExactness := ratioWithEmptyDenominator(numericMatched, numericExpected)
	citationCoverage := ratioWithEmptyDenominator(citationMatched, citationExpected)
	expectedCases := len(baselineCases)
	passed := directCases == expectedCases && expectedCases > 0 && recallAtK >= cfg.Gate.MinRecallAtK && mrr >= cfg.Gate.MinMRR && numericExactness >= cfg.Gate.MinNumericExactly
	return map[string]any{
		"passed": passed, "directPassed": passed, "recallAtK": roundFloat(recallAtK, 6), "mrr": roundFloat(mrr, 6),
		"numericExactness": roundFloat(numericExactness, 6), "numericExpected": numericExpected, "numericMatched": numericMatched,
		"citationCoverage": roundFloat(citationCoverage, 6), "citationExpected": citationExpected, "citationMatched": citationMatched,
		"sourceDiversity": roundFloat(sourceDiversity, 6), "cases": directCases, "expected_cases": expectedCases,
		"case_set_digest": cfg.CaseSetDigest, "snapshot_digest": anyToString(anyMap(cfg.Snapshot)["digest"]), "k": cfg.K,
		"evaluation_split": baselineSplit, "evaluation_case_set_digest": "sha256:" + recallEvalCaseSetDigest(baselineCases), "evaluation_case_count": len(baselineCases), "evaluation_traffic_class": "evaluation_holdout",
		"cohort": "saved_recall_v3_frozen_direct", "metrics_source": "authoritative_direct_cohort_same_run",
	}
}

func (s *server) evaluateGraphDirectCohort(ctx context.Context, incomingHeaders http.Header, payload map[string]any, cfg recallEvalSavedConfig) (map[string]any, []savedRecallEvalCaseResult) {
	health := validateSavedRecallEvalCaseSet(cfg)
	if !anyToBool(health["valid"]) || !anyToBool(health["benchmark_eligible"]) {
		return map[string]any{"passed": false, "directPassed": false, "binding_valid": false, "reason": "direct_case_set_not_benchmark_eligible", "case_set_health": health}, nil
	}
	k := clampInt(cfg.K, 1, 20)
	baselineCases, _ := savedRecallDirectBaselineCases(cfg)
	binding := graphCorpusDirectBaselineBinding(cfg)
	// Re-run the exact server-owned pre-treatment profile used to seal the
	// baseline. Caller headers and diagnostic flags are not part of that fixed
	// cohort and cannot turn the same-run comparator into a learned treatment.
	results := evaluateSavedRecallCasesConcurrently(ctx, s, baselineCases, k, nil, false, false, "", true, binding, "evaluation_holdout")
	receipts := make([]map[string]any, 0, len(results))
	for _, result := range results {
		if receipt := cloneJSONMap(anyMap(result.searchResponse["direct_control_receipt"])); len(receipt) > 0 {
			receipts = append(receipts, receipt)
		}
	}
	control := graphRecallDirectControlCohortReceipt(receipts, baselineCases, binding)
	metrics := graphRecallDirectMetricsFromResults(results, cfg)
	metrics["control_receipt"] = control
	metrics["binding_valid"] = anyToBool(control["available"]) && savedRecallDirectControlCohortValid(control, binding)
	if !anyToBool(metrics["binding_valid"]) {
		metrics["passed"] = false
		metrics["directPassed"] = false
		metrics["reason"] = "same_run_direct_control_receipt_invalid"
	}
	return metrics, results
}

func (s *server) memoryRecallEvaluateGraphCorpusNative(w http.ResponseWriter, r *http.Request, payload map[string]any, incomingHeaders http.Header) {
	cfg, err := loadSavedRecallGraphCorpusConfig()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to load graph recall corpus", "code": "storage_access_error"})
		return
	}
	health := validateSavedRecallGraphCorpusConfig(cfg)
	if !anyToBool(health["valid"]) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "graph recall corpus failed closed validation", "code": "graph_corpus_invalid", "case_set_health": health})
		return
	}
	expectedSourceEdgeDigest := strings.TrimSpace(anyToString(anyMap(cfg.Snapshot)["source_edge_snapshot_digest"]))
	snapshotStartDigest, snapshotErr := s.currentSavedRecallGraphSourceEdgeDigest(r.Context(), cfg.Snapshot)
	if snapshotErr != nil || expectedSourceEdgeDigest == "" || !strings.EqualFold(snapshotStartDigest, expectedSourceEdgeDigest) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "graph evaluation source or edge snapshot no longer matches the sealed corpus",
			"code":  "graph_evaluation_snapshot_mismatch", "expected_snapshot_digest": expectedSourceEdgeDigest,
			"observed_snapshot_digest": snapshotStartDigest,
		})
		return
	}
	split := strings.ToLower(strings.TrimSpace(anyToString(payload["split"])))
	if split == "" {
		split = "holdout"
	}
	if split != "all" && split != "development" && split != "holdout" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported graph corpus split", "supported_splits": []string{"all", "development", "holdout"}})
		return
	}
	cases := cfg.Cases
	if split != "all" {
		cases = make([]map[string]any, 0, len(cfg.Cases))
		for _, row := range cfg.Cases {
			if strings.EqualFold(anyToString(row["split"]), split) {
				cases = append(cases, row)
			}
		}
	}
	hardNegativeCurrentIdentity, identityErr := s.graphRecallHardNegativeCurrentIdentityReceipt(cases, expectedSourceEdgeDigest)
	if identityErr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "graph hard-negative target no longer binds an exact canonical current memory identity",
			"code":  "graph_hard_negative_current_identity_mismatch", "detail": graphRecallPublicFailureDetails(identityErr),
		})
		return
	}
	k := defaultRecallEvalK
	if raw, exists := payload["k"]; exists {
		if anyToInt(raw, 0) != defaultRecallEvalK {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "graph recall promotion fixes k at 5", "code": "graph_recall_k_must_equal_5", "k": defaultRecallEvalK})
			return
		}
	}
	totals := graphRecallEvaluationTotals{
		latencyValues:          make([]float64, 0, len(cases)),
		treatmentLatencyValues: make([]float64, 0, len(cases)),
		controlLatencyValues:   make([]float64, 0, len(cases)),
		latencyDeltaValues:     make([]float64, 0, len(cases)),
		pairedLatencySamples:   make([]graphRecallPairedLatencySample, 0, len(cases)),
		caseReports:            make([]map[string]any, 0, len(cases)),
		incrementalReports:     make([]map[string]any, 0),
	}
	totals.casesExpected = len(cases)
	for _, rawCase := range cases {
		if anyToBool(rawCase["hard_negative"]) {
			totals.hardNegativeExpected++
		} else {
			totals.positiveExpected++
			if anyToBool(rawCase["incremental_needed"]) {
				totals.incrementalCases++
			}
		}
	}
	// The incremental denominator is sealed by the corpus labels before any
	// retrieval result is observed. The successful-help count is accumulated
	// separately below and can never shrink this denominator.
	expectedIncrementalCases := totals.incrementalCases
	totals.incrementalCases = 0
	started := time.Now()
	for _, rawCase := range cases {
		totals.casesAttempted++
		query := strings.TrimSpace(anyToString(rawCase["query"]))
		report := graphRecallCaseReportBase(rawCase, k)
		if query == "" {
			report["status"] = "missing_query"
			totals.casesTerminal++
			totals.caseFailures++
			if anyToBool(rawCase["hard_negative"]) {
				totals.hardNegativeFailures++
			} else {
				totals.positiveFailures++
				totals.pairedLatencyFailures++
			}
			graphRecallRecordEconomics(nil, &totals)
			totals.caseReports = append(totals.caseReports, report)
			continue
		}
		directLimit := defaultRecallEvalK
		reqPayload := map[string]any{
			"query": query, "limit": directLimit, "project": strings.TrimSpace(anyToString(rawCase["project"])),
			"topic_path": anyToString(rawCase["topic_path"]), "retrieval_mode": normalizeRetrievalMode(anyToString(rawCase["retrieval_mode"])),
			"retrieval_intent": "decision", "rerank_with_learning": false, "include_grounding": true,
			"include_retrieval_debug": false, "include_preferences": false, "sources": []string{sourceTopicRollup},
			"user_id": "", "agent_id": anyToString(rawCase["agent_id"]), "auto_escalate": false,
			"deep_async": false, "callback_url": "", "traffic_class": "evaluation_holdout",
		}
		positive := !anyToBool(rawCase["hard_negative"])
		control := anyMap(rawCase["incremental_control"])
		if positive && !strings.EqualFold(graphRecallControlRequestDigest(reqPayload), anyToString(control["control_request_digest"])) {
			report["status"] = "paired_control_request_mismatch"
			totals.casesTerminal++
			totals.caseFailures++
			totals.positiveFailures++
			totals.pairedLatencyFailures++
			graphRecallRecordEconomics(nil, &totals)
			totals.caseReports = append(totals.caseReports, report)
			continue
		}
		controlLatencyMs := 0.0
		controlLatencyValid := false
		if positive {
			controlLatencyMs, controlLatencyValid = graphRecallControlLatencyValid(control)
			if !controlLatencyValid {
				report["status"] = "paired_control_latency_unavailable"
				report["latency_comparable"] = false
				report["latency_comparability_reason"] = "sealed_direct_control_latency_missing_or_invalid"
				totals.casesTerminal++
				totals.caseFailures++
				totals.positiveFailures++
				totals.pairedLatencyFailures++
				graphRecallRecordEconomics(nil, &totals)
				totals.caseReports = append(totals.caseReports, report)
				continue
			}
		}
		caseStarted := time.Now()
		pairedTreatmentStarted := caseStarted
		caseLatencyMs := 0.0
		caseLatencyRecorded := false
		recordCaseLatency := func() float64 {
			if !caseLatencyRecorded {
				caseLatencyMs = float64(time.Since(caseStarted).Microseconds()) / 1000
				totals.latencyValues = append(totals.latencyValues, caseLatencyMs)
				caseLatencyRecorded = true
			}
			return caseLatencyMs
		}
		searchResponse, status, execErr := s.executeRetrieval(r.Context(), incomingHeaders, reqPayload, true)
		if execErr != nil {
			report["status"] = "retrieval_failed"
			report["status_code"] = status
			errorCode := graphRecallExecutionErrorCode(status, execErr)
			// Keep the historical field shape but expose only a stable category;
			// the underlying error remains an owner-side diagnostic.
			report["error"] = errorCode
			report["error_code"] = errorCode
			totals.casesTerminal++
			totals.caseFailures++
			if anyToBool(rawCase["hard_negative"]) {
				totals.hardNegativeFailures++
			} else {
				totals.positiveFailures++
				totals.pairedLatencyFailures++
			}
			graphRecallRecordEconomics(searchResponse, &totals)
			report["latency_ms"] = roundFloat(recordCaseLatency(), 3)
			totals.caseReports = append(totals.caseReports, report)
			continue
		}
		totals.casesTerminal++
		totals.casesEvaluated++
		if positive && !strings.EqualFold(graphRecallControlResponseDigest(searchResponse), anyToString(control["control_response_digest"])) {
			report["status"] = "paired_control_response_mismatch"
			totals.caseFailures++
			totals.positiveFailures++
			totals.pairedLatencyFailures++
			graphRecallRecordEconomics(searchResponse, &totals)
			report["latency_ms"] = roundFloat(recordCaseLatency(), 3)
			totals.caseReports = append(totals.caseReports, report)
			continue
		}
		// Measure the graph treatment at the same server seam as the sealed
		// direct control: executeRetrieval plus production final-response
		// composition. Keep this separate from the full case accounting latency,
		// which also includes scoring and receipt assembly.
		qualitySample := s.graphRecallQualitySampleFromServer(r.Context(), reqPayload, searchResponse, nil, anyToString(rawCase["id"]), anyToString(anyMap(cfg.Custody)["case_set_digest"]), anyToString(anyMap(cfg.Snapshot)["digest"]))
		treatmentLatencyMeasured := len(qualitySample) > 0
		treatmentLatencyMs := float64(time.Since(pairedTreatmentStarted).Microseconds()) / 1000
		controlFinalTargetVisible := false
		if positive {
			replayedControl, _, replayed := s.graphRecallProductionResponseProjection(r.Context(), reqPayload, searchResponse, true)
			sealedControl := anyMap(control["control_final_model_visible"])
			controlHit := replayed && graphRecallFinalModelVisibleContainsForProject(replayedControl, anyToString(rawCase["target_memory_id"]), normalizeExpectedFileTokens(rawCase["graph_expected_files"]), anyToString(rawCase["project"]))
			if !replayed || !strings.EqualFold(anyToString(replayedControl["final_model_visible_digest"]), anyToString(sealedControl["final_model_visible_digest"])) || controlHit != anyToBool(control["target_direct_hit"]) {
				report["status"] = "paired_control_composition_mismatch"
				totals.caseFailures++
				totals.positiveFailures++
				totals.pairedLatencyFailures++
				graphRecallRecordEconomics(searchResponse, &totals)
				report["latency_ms"] = roundFloat(recordCaseLatency(), 3)
				totals.caseReports = append(totals.caseReports, report)
				continue
			}
			controlFinalTargetVisible = controlHit
			if treatmentLatencyMeasured && controlLatencyValid {
				treatmentLatencyMs = roundFloat(treatmentLatencyMs, 3)
				controlLatencyMs = roundFloat(controlLatencyMs, 3)
				latencyDeltaMs := graphRecallRoundSigned(treatmentLatencyMs-controlLatencyMs, 3)
				totals.treatmentLatencyValues = append(totals.treatmentLatencyValues, treatmentLatencyMs)
				totals.controlLatencyValues = append(totals.controlLatencyValues, controlLatencyMs)
				totals.latencyDeltaValues = append(totals.latencyDeltaValues, latencyDeltaMs)
				totals.pairedLatencySamples = append(totals.pairedLatencySamples, graphRecallPairedLatencySample{
					CaseID: strings.TrimSpace(anyToString(rawCase["id"])), GraphTreatmentMS: treatmentLatencyMs, DirectControlMS: controlLatencyMs,
				})
				totals.pairedLatencyCases++
			} else {
				totals.pairedLatencyFailures++
			}
		}
		results := parseRows(searchResponse["results"])
		grounding := anyMap(searchResponse["grounding"])
		seedFiles := normalizeExpectedFileTokens(rawCase["expected_files"])
		targetFiles := map[string]struct{}{}
		if positive {
			targetFiles = normalizeExpectedFileTokens(rawCase["graph_expected_files"])
		}
		directRank := matchRankWithinKProject(results, seedFiles, nil, k, anyToString(rawCase["project"]))
		directHit := directRank != nil
		totals.directCases++
		if directHit {
			totals.directHits++
			totals.directReciprocalSum += 1 / float64(*directRank)
		}
		expectedNumeric := normalizeExpectedNumeric(rawCase["expected_numeric"])
		matchedNumeric := matchedNumericFacts(grounding, expectedNumeric)
		matchedFiles := matchedExpectedFilesWithinKProject(results, seedFiles, k, anyToString(rawCase["project"]))
		caseSources := uniqueSourcesWithinK(results, k)
		totals.numericExpected += len(expectedNumeric)
		totals.numericMatched += len(matchedNumeric)
		totals.citationExpected += len(seedFiles)
		totals.citationMatched += len(matchedFiles)
		totals.sourceDiversitySum += float64(len(caseSources))
		graphContribution := map[string]any{}
		if positive {
			graphContribution = s.evaluateRecallGraphContribution(r.Context(), results, targetFiles, nil, k, anyToString(rawCase["project"]))
		} else {
			// Hard negatives enumerate the complete bounded adjacency snapshot but
			// pass no forbidden target as an expected positive. The forbidden target
			// is checked separately against the returned candidate IDs.
			graphContribution = s.evaluateRecallGraphContributionForSeed(r.Context(), results, nil, nil, k, anyToString(rawCase["project"]), anyToString(rawCase["seed_memory_id"]), true)
		}
		forbiddenIDs := anyToStringSlice(rawCase["forbidden_memory_ids"])
		forbiddenID := ""
		if len(forbiddenIDs) > 0 {
			forbiddenID = strings.TrimSpace(forbiddenIDs[0])
		}
		forbiddenFiles := normalizeExpectedFileTokens(rawCase["forbidden_graph_files"])
		targetDirect := matchRankWithinKProject(results, targetFiles, nil, k, anyToString(rawCase["project"])) != nil
		if !positive {
			targetDirect = graphRecallResultContainsTargetForProject(results, forbiddenID, forbiddenFiles, k, anyToString(rawCase["project"]))
		}
		seedDirectPresent := graphRecallResultContainsMemoryID(results, anyToString(rawCase["seed_memory_id"]), k)
		finalTargetVisible := false
		finalForbiddenVisible := false
		finalSeedVisible := false
		if len(qualitySample) > 0 {
			if positive {
				finalTargetVisible = graphRecallFinalModelVisibleContainsForProject(qualitySample, anyToString(rawCase["target_memory_id"]), targetFiles, anyToString(rawCase["project"]))
			} else {
				finalForbiddenVisible = graphRecallFinalModelVisibleContainsForProject(qualitySample, forbiddenID, forbiddenFiles, anyToString(rawCase["project"]))
			}
			// The production compiler preserves direct result files as opaque
			// evidence refs even when the legacy rendered row does not carry a
			// memory_id. Bind the hard-negative oracle to that exact sealed seed
			// file as well as its memory ID; an unrelated clean seed cannot satisfy
			// the oracle.
			finalSeedVisible = graphRecallFinalModelVisibleContainsForProject(qualitySample, anyToString(rawCase["seed_memory_id"]), seedFiles, anyToString(rawCase["project"]))
		}
		// Graph efficacy is measured only on evidence that survives the actual
		// production context-pack enrichment and compilation seam. An off-path
		// graph lookup cannot satisfy Recall@5 or incremental help.
		graphAttributedHit := positive && graphRecallFinalModelVisibleGraphAttribution(qualitySample, anyToString(rawCase["target_memory_id"]), targetFiles, anyToString(rawCase["project"]), graphContribution)
		// A target visible in the direct control, or merely present in the
		// treatment without a hydrated graph-edge provenance row, is not a graph
		// hit. The paired control is the material-contribution counterfactual.
		graphHit := graphAttributedHit && !controlFinalTargetVisible
		addedHit := graphHit
		forbiddenAdded := false
		hardNegativeOracleAvailable := false
		candidateIDs := anyToStringSlice(graphContribution["added_candidate_memory_ids"])
		for _, candidateID := range candidateIDs {
			if forbiddenID != "" && graphRecallMemoryIDEqual(candidateID, forbiddenID) {
				forbiddenAdded = true
				break
			}
		}
		if finalForbiddenVisible {
			forbiddenAdded = true
		}
		if positive {
			totals.positiveCases++
			if graphHit {
				totals.graphHits++
			}
			if anyToBool(rawCase["incremental_needed"]) {
				totals.incrementalCases++
				// Incremental help is the paired change in the production-visible
				// packet, not presence in an off-path raw retrieval list.
				if addedHit {
					totals.incrementalHelped++
				}
			}
		} else {
			totals.hardNegativeCases++
			hardNegativeOracleAvailable = anyToBool(graphContribution["enabled"]) && seedDirectPresent && finalSeedVisible && len(qualitySample) > 0
			if hardNegativeOracleAvailable {
				totals.hardNegativeOracleAvailable++
			}
			if hardNegativeOracleAvailable && !graphHit && !targetDirect && !forbiddenAdded {
				totals.hardNegativePassed++
			} else {
				totals.hardNegativeFailures++
			}
		}
		report["status"] = "evaluated"
		report["direct_hit"] = directHit
		report["direct_rank"] = directRank
		report["graph_hit"] = graphHit
		report["graph_attributed_hit"] = graphAttributedHit
		report["graph_added_hit"] = addedHit
		report["target_in_direct_top_k"] = targetDirect
		report["target_in_final_context_pack"] = finalTargetVisible
		report["control_target_in_final_context_pack"] = controlFinalTargetVisible
		report["forbidden_target_in_graph_added"] = forbiddenAdded
		report["sealed_seed_in_direct_top_k"] = seedDirectPresent
		report["sealed_seed_in_final_context_pack"] = finalSeedVisible
		report["hard_negative_oracle_available"] = hardNegativeOracleAvailable
		report["graph_contribution"] = graphRecallPublicContribution(graphContribution)
		report["expected_numeric_count"] = len(expectedNumeric)
		report["matched_numeric_count"] = len(matchedNumeric)
		report["citation_coverage"] = ratioWithEmptyDenominator(len(matchedFiles), len(seedFiles))
		report["source_diversity"] = len(caseSources)
		if len(qualitySample) > 0 {
			report["context_quality_sample"] = qualitySample
		}
		if treatmentLatencyMeasured {
			report["graph_treatment_latency_ms"] = roundFloat(treatmentLatencyMs, 3)
		} else {
			report["graph_treatment_latency_ms"] = nil
		}
		if positive {
			report["direct_control_latency_ms"] = roundFloat(controlLatencyMs, 3)
			comparable := treatmentLatencyMeasured && controlLatencyValid
			report["latency_comparable"] = comparable
			if comparable {
				report["latency_delta_ms"] = graphRecallRoundSigned(treatmentLatencyMs-controlLatencyMs, 3)
				report["latency_comparability_reason"] = "same_request_and_production_response_seam"
			} else {
				report["latency_delta_ms"] = nil
				report["latency_comparability_reason"] = "graph_treatment_composition_receipt_missing"
			}
		} else {
			report["direct_control_latency_ms"] = nil
			report["latency_delta_ms"] = nil
			report["latency_comparable"] = false
			report["latency_comparability_reason"] = "hard_negative_has_no_paired_direct_control"
		}
		report["latency_ms"] = roundFloat(recordCaseLatency(), 3)
		report["result_count"] = len(results)
		graphRecallRecordEconomics(searchResponse, &totals)
		totals.caseReports = append(totals.caseReports, report)
		if positive && anyToBool(rawCase["incremental_needed"]) {
			totals.incrementalReports = append(totals.incrementalReports, cloneJSONMap(report))
		}
	}
	graphRecall := 0.0
	if totals.positiveCases > 0 {
		graphRecall = float64(totals.graphHits) / float64(totals.positiveCases)
	}
	incrementalHelp := 0.0
	if totals.incrementalCases > 0 {
		incrementalHelp = float64(totals.incrementalHelped) / float64(totals.incrementalCases)
	}
	directCfg, directCfgErr := loadSavedRecallEvalConfig()
	direct := map[string]any{"passed": false, "directPassed": false, "binding_valid": false, "reason": "direct_cohort_unavailable"}
	var directCohortResults []savedRecallEvalCaseResult
	directExpectedCases := 0
	if directCfgErr == nil {
		directBaselineCases, _ := savedRecallDirectBaselineCases(directCfg)
		directExpectedCases = len(directBaselineCases)
		direct, directCohortResults = s.evaluateGraphDirectCohort(r.Context(), incomingHeaders, payload, directCfg)
		directBinding := graphCorpusDirectBaselineBinding(directCfg)
		if anyToBool(direct["binding_valid"]) || direct["reason"] == nil {
			direct["binding_valid"] = graphRecallDirectBindingMatches(cfg.DirectBaseline, directBinding)
		}
		if !anyToBool(direct["binding_valid"]) {
			direct["passed"] = false
			direct["directPassed"] = false
			direct["reason"] = "direct_graph_corpus_binding_mismatch"
		}
	} else {
		direct["reason"] = "direct_case_set_storage_error"
	}
	for _, directResult := range directCohortResults {
		graphRecallRecordEconomics(directResult.searchResponse, &totals)
	}
	// Fence the complete run, including the fixed direct cohort. A source or
	// edge mutation during either the paired graph cohort or its same-run direct
	// quality/economics comparator invalidates the promotion receipt.
	snapshotEndDigest, snapshotEndErr := s.currentSavedRecallGraphSourceEdgeDigest(r.Context(), cfg.Snapshot)
	evaluationSnapshotStable := snapshotEndErr == nil && strings.EqualFold(snapshotStartDigest, snapshotEndDigest) && strings.EqualFold(snapshotEndDigest, expectedSourceEdgeDigest)
	baseline, baselineReceipt := loadSavedRecallDirectBaseline(cfg.DirectBaseline)
	if len(baseline) > 0 {
		direct["baseline"] = baseline
	}
	direct["baseline_receipt"] = baselineReceipt
	graph := map[string]any{
		"latency_scope":  "retrieval_plus_paired_production_composition",
		"cases_expected": totals.casesExpected, "cases_attempted": totals.casesAttempted, "cases_evaluated": totals.casesEvaluated, "cases_terminal": totals.casesTerminal, "case_failures": totals.caseFailures,
		"positive_expected": totals.positiveExpected, "positive_failures": totals.positiveFailures,
		"positive_cases": totals.positiveCases, "positiveCases": totals.positiveCases,
		"graph_hits": totals.graphHits, "graphHits": totals.graphHits,
		"graph_recall_at_5": roundFloat(graphRecall, 6), "incremental_denominator": totals.incrementalCases,
		"incrementalDenominator": totals.incrementalCases, "incremental_help": roundFloat(incrementalHelp, 6),
		"hard_negative_cases": totals.hardNegativeCases, "hardNegativeCases": totals.hardNegativeCases,
		"hard_negative_passed": totals.hardNegativePassed, "hardNegativePassed": totals.hardNegativePassed,
		"hard_negative_expected": totals.hardNegativeExpected, "hard_negative_failures": totals.hardNegativeFailures, "hard_negative_oracle_available": totals.hardNegativeOracleAvailable,
		"hard_negative_current_identity": hardNegativeCurrentIdentity,
		"incremental_expected":           expectedIncrementalCases,
		"explicit_cases":                 totals.positiveCases, "explicitCases": totals.positiveCases,
		"graph_attribution_binding":    "finalized_visible_graph_provenance.v1",
		"graph_attributed_hits":        totals.graphHits,
		"graph_attributed_denominator": totals.positiveCases,
	}
	pairedLatency := graphRecallPairedLatencySummary(totals)
	graph["latency_comparable"] = pairedLatency["comparable"]
	graph["paired_latency_cases"] = pairedLatency["paired_cases"]
	graph["paired_latency_expected"] = pairedLatency["expected_cases"]
	graph["paired_latency_failures"] = pairedLatency["failed_cases"]
	graph["paired_latency"] = pairedLatency
	quality := graphRecallQualityCalibrationSnapshotForCohort(anyToString(anyMap(cfg.Custody)["case_set_digest"]), anyToString(anyMap(cfg.Snapshot)["digest"]), cases, totals.caseReports, payload)
	costTransportObserved := totals.costObservationExpected > 0 && totals.costObservationObserved == totals.costObservationExpected && totals.costObservationMissing == 0
	policyRun := graphRecallSourcePolicyRunReceipt(totals)
	costProvenZero := costTransportObserved && totals.networkCallsKnown && totals.localBackendCallsKnown && totals.externalNetworkCallsKnown && totals.externalNetworkZeroProven && totals.providerCallsKnown && totals.providerTokensKnown && totals.providerCostKnown && totals.externalNetworkCalls == 0 && totals.providerCalls == 0 && totals.providerTokens == 0 && totals.providerCostMicros == 0 && anyToBool(policyRun["eligible"])
	cost := map[string]any{
		"schema_id": retrievalCostObservabilitySchemaID, "authority": retrievalCostObservabilityAuthority,
		"network_calls": totals.networkCalls, "network_calls_observed": totals.networkCallsKnown,
		"local_backend_calls": totals.localBackendCalls, "local_backend_calls_observed": totals.localBackendCallsKnown,
		"external_network_calls": totals.externalNetworkCalls, "external_network_calls_observed": totals.externalNetworkCallsKnown, "external_network_zero_proven": totals.externalNetworkZeroProven,
		"provider_calls": totals.providerCalls, "provider_calls_observed": totals.providerCallsKnown,
		"provider_tokens": totals.providerTokens, "provider_tokens_observed": totals.providerTokensKnown,
		"provider_cost_microusd": totals.providerCostMicros, "provider_cost_observed": totals.providerCostKnown,
		"observation_expected": totals.costObservationExpected, "observation_expected_required": len(cases) + directExpectedCases, "observation_observed": totals.costObservationObserved, "observation_missing": totals.costObservationMissing,
		"transport_observed": costTransportObserved, "proven_zero": costProvenZero,
		"traffic_class":     "evaluation_holdout",
		"source_policy_run": policyRun, "source_policy_observed": totals.sourcePolicyObserved, "source_policy_consistent": totals.sourcePolicyConsistent,
		"truth": "observed-only; omitted provider/network fields are not inferred",
	}
	corpusMap := cloneJSONMap(health)
	corpusMap["validation_receipt"] = graphRecallCorpusValidationReceipt(cfg, health)
	corpusMap["direct_baseline_binding"] = cfg.DirectBaseline
	corpusMap["cost"] = cost
	corpusMap["runtime_identity_required"] = true
	corpusMap["runtime_identity"] = contextLatticeBuildIdentity()
	corpusMap["case_set_digest"] = cfg.CaseSetDigest
	corpusMap["graph_snapshot_digest"] = anyToString(anyMap(cfg.Snapshot)["digest"])
	corpusMap["edge_snapshot_digest"] = anyToString(anyMap(cfg.Snapshot)["edge_snapshot_digest"])
	corpusMap["source_edge_snapshot_digest"] = expectedSourceEdgeDigest
	corpusMap["evaluation_snapshot_start_digest"] = snapshotStartDigest
	corpusMap["evaluation_snapshot_end_digest"] = snapshotEndDigest
	corpusMap["evaluation_snapshot_stable"] = evaluationSnapshotStable
	corpusMap["evaluation_split"] = split
	corpusMap["evaluation_case_count"] = len(cases)
	corpusMap["evaluation_positive_cases"] = totals.positiveExpected
	corpusMap["evaluation_hard_negative_cases"] = totals.hardNegativeExpected
	corpusMap["evaluation_incremental_cases"] = expectedIncrementalCases
	corpusMap["evaluation_traffic_class"] = "evaluation_holdout"
	corpusMap["evaluation_captured_at"] = nowUTCISO()
	corpusMap["baseline_policy_digest"] = anyToString(anyMap(baselineReceipt)["control_policy_digest"])
	promotion := graphRecallPromotionGate(s.memoryGraphBackend() != nil, direct, quality, graph, corpusMap)
	graph["status"] = map[bool]string{true: "passed", false: "failed"}[anyToBool(promotion["promotion_eligible"])]
	avgLatency, p95Latency := recallLatencyStats(totals.latencyValues)
	pairedLatencyMap := anyMap(graph["paired_latency"])
	metrics := map[string]any{
		"casesTotal": len(cases), "casesEvaluated": direct["cases"], "graphCasesEvaluated": totals.directCases, "evaluationSplit": split, "k": k,
		"recallAtK": direct["recallAtK"], "mrr": direct["mrr"], "directRecallAtK": direct["recallAtK"], "directMrr": direct["mrr"], "directPassed": direct["directPassed"],
		"numericExactness": direct["numericExactness"], "numericExpected": direct["numericExpected"], "numericMatched": direct["numericMatched"],
		"citationCoverage": direct["citationCoverage"], "citationExpected": direct["citationExpected"], "citationMatched": direct["citationMatched"],
		"sourceDiversity": direct["sourceDiversity"], "directCohort": map[string]any{"case_set_digest": direct["case_set_digest"], "snapshot_digest": direct["snapshot_digest"], "cases": direct["cases"], "binding_valid": direct["binding_valid"]},
		"graphRecallAt5": roundFloat(graphRecall, 6), "graphPositiveCases": totals.positiveCases, "graphHits": totals.graphHits,
		"incrementalHelp": roundFloat(incrementalHelp, 6), "incrementalDenominator": totals.incrementalCases,
		"incrementalHelpedCases": totals.incrementalHelped, "hardNegativeCases": totals.hardNegativeCases,
		"hardNegativePassed": totals.hardNegativePassed, "hardNegativeAccuracy": func() float64 {
			if totals.hardNegativeCases == 0 {
				return 0
			}
			return float64(totals.hardNegativePassed) / float64(totals.hardNegativeCases)
		}(),
		"graphExplicitCases": totals.positiveCases, "graphEfficacyStatus": map[bool]string{true: "passed", false: "failed"}[anyToBool(promotion["promotion_eligible"])],
		"memoryGraphStoreActive": s.memoryGraphBackend() != nil, "avgLatencyMs": roundFloat(avgLatency, 3), "p95LatencyMs": roundFloat(p95Latency, 3),
		"graphTreatmentLatencyMs": pairedLatencyMap["graph_treatment_avg_ms"], "graphTreatmentLatencyP95Ms": pairedLatencyMap["graph_treatment_p95_ms"],
		"directControlLatencyMs": pairedLatencyMap["direct_control_avg_ms"], "directControlLatencyP95Ms": pairedLatencyMap["direct_control_p95_ms"],
		"latencyDeltaMs": pairedLatencyMap["latency_delta_avg_ms"], "latencyDeltaP95Ms": pairedLatencyMap["latency_delta_p95_ms"],
		"latencyComparable": pairedLatencyMap["comparable"], "pairedLatency": pairedLatencyMap,
		"durationMs":   roundFloat(float64(time.Since(started).Microseconds())/1000, 3),
		"networkCalls": totals.networkCalls, "networkCallsObserved": totals.networkCallsKnown,
		"externalNetworkCalls": totals.externalNetworkCalls, "externalNetworkCallsObserved": totals.externalNetworkCallsKnown, "externalNetworkZeroProven": totals.externalNetworkZeroProven,
		"providerCalls": totals.providerCalls, "providerCallsObserved": totals.providerCallsKnown,
		"providerTokens": totals.providerTokens, "providerTokensObserved": totals.providerTokensKnown,
		"providerCostMicrousd": totals.providerCostMicros, "providerCostObserved": totals.providerCostKnown,
		"localBackendCalls": totals.localBackendCalls, "localBackendCallsObserved": totals.localBackendCallsKnown,
		"qualityCalibration": quality, "graphContribution": graph,
		"directBaseline": baselineReceipt,
	}
	response := map[string]any{
		"ok": anyToBool(promotion["promotion_eligible"]), "passed": anyToBool(promotion["promotion_eligible"]), "quality_status": map[bool]string{true: "passed", false: "blocked"}[anyToBool(promotion["promotion_eligible"])],
		"mode": "graph", "metrics": metrics, "promotion": promotion, "cases": totals.caseReports,
		"topology_cases": totals.caseReports, "incremental_needed_cases": totals.incrementalReports,
		"savedCaseSet":    map[string]any{"case_set_id": ownerOnlyStoreRef("recall_graph_corpus"), "schema_id": cfg.SchemaID, "version": cfg.Version, "case_set_digest": cfg.CaseSetDigest, "custody": cfg.Custody, "manifest": cfg.Manifest, "count": len(cfg.Cases), "evaluation_count": len(cases), "evaluation_split": split},
		"cost":            cost,
		"direct_baseline": baselineReceipt,
	}
	writeJSON(w, http.StatusOK, response)
}
