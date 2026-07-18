package main

import (
	"sort"
	"strings"
)

const (
	retrievalAblationSchemaID       = "retrieval_ablation.v1"
	retrievalAblationReportSchemaID = "retrieval_ablation_report.v1"
	retrievalAblationDefaultMaxRows = 128
	retrievalAblationMaxRows        = 256
	retrievalAblationMaxResults     = 100
)

type retrievalAblationInput struct {
	CaseID                               string
	Results                              []map[string]any
	ExpectedFiles                        []string
	K                                    int
	TrafficClass                         string
	SnapshotStable                       bool
	BaselineOutcomeLabel                 string
	BaselineOutcomeEvidenceDigest        string
	CounterfactualOutcomeLabels          map[string]string
	CounterfactualOutcomeEvidenceDigests map[string]string
	MaxRows                              int
}

type retrievalAblationSnapshotResult struct {
	row       map[string]any
	resultID  string
	memoryKey string
	memoryRef string
	sources   []string
}

// buildRetrievalAblation computes every counterfactual from one captured result
// slice. It does not execute retrieval and never infers utility from rank deltas.
func buildRetrievalAblation(input retrievalAblationInput) map[string]any {
	maxRows := clampInt(input.MaxRows, 1, retrievalAblationMaxRows)
	if input.MaxRows <= 0 {
		maxRows = retrievalAblationDefaultMaxRows
	}
	k := clampInt(input.K, 1, retrievalAblationMaxResults)
	if input.K <= 0 {
		k = minInt(10, maxInt(1, len(input.Results)))
	}
	input.K = k
	results := input.Results
	resultLimitOmitted := 0
	if len(results) > retrievalAblationMaxResults {
		resultLimitOmitted = len(results) - retrievalAblationMaxResults
		results = results[:retrievalAblationMaxResults]
	}
	snapshot := retrievalAblationSnapshot(results)
	snapshotDigest := retrievalAblationSnapshotDigest(input.CaseID, k, snapshot)
	expectedFiles := sortedKeys(normalizeExpectedFileTokens(input.ExpectedFiles))

	sourceSet := map[string]struct{}{}
	memoryTargets := map[string]string{}
	for _, result := range snapshot {
		for _, source := range result.sources {
			sourceSet[source] = struct{}{}
		}
		memoryTargets[result.memoryKey] = result.memoryRef
	}
	sources := derivedRegressionSortedSet(sourceSet)
	memoryKeys := make([]string, 0, len(memoryTargets))
	for memoryKey := range memoryTargets {
		memoryKeys = append(memoryKeys, memoryKey)
	}
	sort.Strings(memoryKeys)

	rows := make([]map[string]any, 0, minInt(maxRows, len(sources)+len(memoryKeys)))
	for _, source := range sources {
		if len(rows) >= maxRows {
			break
		}
		counterfactual := retrievalAblationWithoutSource(snapshot, source)
		rows = append(rows, retrievalAblationRow(input, snapshotDigest, snapshot, counterfactual, expectedFiles, "leave_one_source", source, source))
	}
	for _, memoryKey := range memoryKeys {
		if len(rows) >= maxRows {
			break
		}
		counterfactual := retrievalAblationWithoutMemory(snapshot, memoryKey)
		targetID := "memory_" + sha256Hex(memoryKey)[:24]
		rows = append(rows, retrievalAblationRow(input, snapshotDigest, snapshot, counterfactual, expectedFiles, "leave_one_memory", targetID, memoryTargets[memoryKey]))
	}

	changedResults := 0
	citationLosses := 0
	promotionEligible := 0
	for _, row := range rows {
		if anyToString(row["result_change_label"]) == "changed" {
			changedResults++
		}
		if anyToString(row["citation_change_label"]) == "lost" {
			citationLosses++
		}
		if anyToBool(row["promotion_eligible"]) {
			promotionEligible++
		}
	}
	totalTargets := len(sources) + len(memoryKeys)
	return map[string]any{
		"schema_id":       retrievalAblationSchemaID,
		"version":         1,
		"case_id":         clipText(strings.TrimSpace(input.CaseID), 200),
		"snapshot_id":     "ras_" + strings.TrimPrefix(snapshotDigest, "sha256:")[:24],
		"snapshot_digest": snapshotDigest,
		"same_case":       true,
		"same_snapshot":   true,
		"snapshot_stable": input.SnapshotStable,
		"traffic_class":   retrievalAblationTrafficClass(input.TrafficClass),
		"synthetic":       retrievalAblationTrafficClass(input.TrafficClass) == "synthetic",
		"rows":            rows,
		"summary": map[string]any{
			"baseline_result_count":        len(snapshot),
			"source_ablation_target_count": len(sources),
			"memory_ablation_target_count": len(memoryKeys),
			"total_target_count":           totalTargets,
			"evaluated_target_count":       len(rows),
			"row_limit_omitted_count":      maxInt(0, totalTargets-len(rows)),
			"result_limit_omitted_count":   resultLimitOmitted,
			"changed_result_row_count":     changedResults,
			"citation_loss_row_count":      citationLosses,
			"promotion_eligible_row_count": promotionEligible,
		},
		"computation": map[string]any{
			"path":                       "offline_snapshot",
			"bounded":                    true,
			"retrieval_hot_path":         false,
			"additional_retrieval_calls": 0,
		},
		"measurement_limit": "Result and citation deltas are exact for the captured snapshot. Outcome labels require an explicit paired observation, and utility is never inferred.",
	}
}

func retrievalAblationSnapshot(results []map[string]any) []retrievalAblationSnapshotResult {
	out := make([]retrievalAblationSnapshotResult, 0, len(results))
	for index, row := range results {
		copyRow := cloneJSONMap(row)
		memoryRef := strings.TrimSpace(recallResultMemoryID(copyRow))
		memoryKey := memoryRef
		if memoryKey == "" {
			memoryKey = "result:" + retrievalAblationResultID(copyRow, index)
			memoryRef = memoryKey
		}
		stats := &portableRedactionStats{}
		memoryRef = clipText(portableString(memoryRef, stats), 360)
		out = append(out, retrievalAblationSnapshotResult{
			row:       copyRow,
			resultID:  retrievalAblationResultID(copyRow, index),
			memoryKey: memoryKey,
			memoryRef: memoryRef,
			sources:   sourcesForRecallResult(copyRow),
		})
	}
	return out
}

func retrievalAblationResultID(row map[string]any, index int) string {
	basis := map[string]any{
		"rank":      index + 1,
		"memory_id": recallResultMemoryID(row),
		"project":   strings.TrimSpace(anyToString(row["project"])),
		"file":      strings.TrimSpace(anyToString(row["file"])),
		"sources":   sourcesForRecallResult(row),
		"score":     roundFloat(anyToFloat(row["score"]), 8),
	}
	return "result_" + strings.TrimPrefix(derivedRegressionDigest(basis), "sha256:")[:24]
}

func retrievalAblationSnapshotDigest(caseID string, k int, snapshot []retrievalAblationSnapshotResult) string {
	rows := make([]map[string]any, 0, len(snapshot))
	for index, result := range snapshot {
		rows = append(rows, map[string]any{
			"rank":       index + 1,
			"result_id":  result.resultID,
			"memory_key": "sha256:" + sha256Hex(result.memoryKey),
			"sources":    result.sources,
		})
	}
	return derivedRegressionDigest(map[string]any{
		"case_id": strings.TrimSpace(caseID),
		"k":       k,
		"results": rows,
	})
}

func retrievalAblationWithoutSource(snapshot []retrievalAblationSnapshotResult, omitted string) []retrievalAblationSnapshotResult {
	out := make([]retrievalAblationSnapshotResult, 0, len(snapshot))
	for _, result := range snapshot {
		if !retrievalAblationContains(result.sources, omitted) {
			out = append(out, result)
			continue
		}
		remaining := make([]string, 0, len(result.sources)-1)
		for _, source := range result.sources {
			if source != omitted {
				remaining = append(remaining, source)
			}
		}
		if len(remaining) == 0 {
			continue
		}
		copyResult := result
		copyResult.row = cloneJSONMap(result.row)
		copyResult.sources = remaining
		copyResult.row["sources"] = remaining
		for _, key := range []string{"source", "retrieval_source"} {
			if strings.EqualFold(strings.TrimSpace(anyToString(copyResult.row[key])), omitted) {
				copyResult.row[key] = remaining[0]
			}
		}
		out = append(out, copyResult)
	}
	return out
}

func retrievalAblationWithoutMemory(snapshot []retrievalAblationSnapshotResult, memoryKey string) []retrievalAblationSnapshotResult {
	out := make([]retrievalAblationSnapshotResult, 0, len(snapshot))
	for _, result := range snapshot {
		if result.memoryKey != memoryKey {
			out = append(out, result)
		}
	}
	return out
}

func retrievalAblationRow(
	input retrievalAblationInput,
	snapshotDigest string,
	baseline []retrievalAblationSnapshotResult,
	counterfactual []retrievalAblationSnapshotResult,
	expectedFiles []string,
	kind string,
	targetID string,
	targetRef string,
) map[string]any {
	baselineRows := retrievalAblationRows(baseline)
	counterfactualRows := retrievalAblationRows(counterfactual)
	resultChanges := retrievalAblationResultChanges(baseline, counterfactual)
	resultChangeLabel := "unchanged"
	if len(resultChanges) > 0 {
		resultChangeLabel = "changed"
	}
	baselineCitations := matchedExpectedFilesWithinK(baselineRows, normalizeExpectedFileTokens(expectedFiles), input.K)
	counterfactualCitations := matchedExpectedFilesWithinK(counterfactualRows, normalizeExpectedFileTokens(expectedFiles), input.K)
	citationChanges := retrievalAblationCitationChanges(baselineCitations, counterfactualCitations)
	citationChangeLabel := "unchanged"
	if len(citationChanges) > 0 {
		citationChangeLabel = "lost"
	}

	outcomeKey := kind + ":" + targetID
	if kind == "leave_one_memory" {
		outcomeKey = kind + ":" + targetRef
	}
	baselineOutcome := strings.TrimSpace(input.BaselineOutcomeLabel)
	counterfactualOutcome := strings.TrimSpace(input.CounterfactualOutcomeLabels[outcomeKey])
	baselineOutcomeDigest := strings.TrimSpace(input.BaselineOutcomeEvidenceDigest)
	counterfactualOutcomeDigest := strings.TrimSpace(input.CounterfactualOutcomeEvidenceDigests[outcomeKey])
	outcomeLabelsPresent := baselineOutcome != "" && counterfactualOutcome != ""
	outcomeObserved := outcomeLabelsPresent && utilitySHA256DigestValid(baselineOutcomeDigest) && utilitySHA256DigestValid(counterfactualOutcomeDigest)
	outcomeChangeLabel := "unobserved"
	if outcomeLabelsPresent && !outcomeObserved {
		outcomeChangeLabel = "unverified"
	} else if outcomeObserved && baselineOutcome == counterfactualOutcome {
		outcomeChangeLabel = "unchanged"
	} else if outcomeObserved {
		outcomeChangeLabel = "changed"
	}
	promotionEligible, promotionReasons := retrievalAblationPromotionEligibility(input, outcomeObserved, outcomeLabelsPresent)
	targetStats := &portableRedactionStats{}
	targetRef = clipText(portableString(targetRef, targetStats), 360)
	ablationIDSeed := strings.Join([]string{snapshotDigest, kind, targetID}, "\x00")
	return map[string]any{
		"ablation_id":                            "rab_" + sha256Hex(ablationIDSeed)[:24],
		"case_id":                                clipText(strings.TrimSpace(input.CaseID), 200),
		"snapshot_id":                            "ras_" + strings.TrimPrefix(snapshotDigest, "sha256:")[:24],
		"kind":                                   kind,
		"target_id":                              targetID,
		"target_ref":                             targetRef,
		"same_case":                              true,
		"same_snapshot":                          true,
		"result_change_label":                    resultChangeLabel,
		"changed_results":                        resultChanges,
		"citation_change_label":                  citationChangeLabel,
		"changed_citations":                      citationChanges,
		"baseline_citations":                     baselineCitations,
		"counterfactual_citations":               counterfactualCitations,
		"outcome_change_label":                   outcomeChangeLabel,
		"baseline_outcome_label":                 retrievalAblationNullableLabel(baselineOutcome),
		"counterfactual_outcome_label":           retrievalAblationNullableLabel(counterfactualOutcome),
		"baseline_outcome_evidence_digest":       retrievalAblationNullableDigest(baselineOutcomeDigest),
		"counterfactual_outcome_evidence_digest": retrievalAblationNullableDigest(counterfactualOutcomeDigest),
		"outcome_observed":                       outcomeObserved,
		"utility":                                nil,
		"utility_inferred":                       false,
		"promotion_eligible":                     promotionEligible,
		"promotion_ineligible_reasons":           promotionReasons,
	}
}

func retrievalAblationRows(snapshot []retrievalAblationSnapshotResult) []map[string]any {
	rows := make([]map[string]any, 0, len(snapshot))
	for _, result := range snapshot {
		rows = append(rows, result.row)
	}
	return rows
}

func retrievalAblationResultChanges(baseline, counterfactual []retrievalAblationSnapshotResult) []map[string]any {
	afterRank := map[string]int{}
	for index, result := range counterfactual {
		afterRank[result.resultID] = index + 1
	}
	changes := make([]map[string]any, 0)
	for index, result := range baseline {
		before := index + 1
		after, retained := afterRank[result.resultID]
		if !retained {
			changes = append(changes, map[string]any{
				"result_id":   result.resultID,
				"label":       "removed",
				"before_rank": before,
				"after_rank":  nil,
			})
			continue
		}
		if before != after {
			changes = append(changes, map[string]any{
				"result_id":   result.resultID,
				"label":       "rank_changed",
				"before_rank": before,
				"after_rank":  after,
			})
		}
	}
	return changes
}

func retrievalAblationCitationChanges(baseline, counterfactual []string) []map[string]any {
	after := map[string]struct{}{}
	for _, citation := range counterfactual {
		after[citation] = struct{}{}
	}
	changes := make([]map[string]any, 0)
	for _, citation := range baseline {
		if _, retained := after[citation]; retained {
			continue
		}
		stats := &portableRedactionStats{}
		changes = append(changes, map[string]any{
			"citation": portableString(citation, stats),
			"label":    "removed",
		})
	}
	return changes
}

func retrievalAblationPromotionEligibility(input retrievalAblationInput, outcomeObserved bool, outcomeLabelsPresent bool) (bool, []string) {
	reasons := make([]string, 0, 3)
	trafficClass := retrievalAblationTrafficClass(input.TrafficClass)
	if trafficClass == "synthetic" {
		reasons = append(reasons, "synthetic_row")
	} else if trafficClass == "unknown" {
		reasons = append(reasons, "traffic_class_unverified")
	}
	if !outcomeObserved {
		if outcomeLabelsPresent {
			reasons = append(reasons, "outcome_pair_unverified")
		} else {
			reasons = append(reasons, "outcome_unobserved")
		}
	}
	if !input.SnapshotStable {
		reasons = append(reasons, "snapshot_unstable")
	}
	return len(reasons) == 0, reasons
}

func retrievalAblationTrafficClass(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	return clipText(value, 40)
}

func retrievalAblationNullableLabel(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return clipText(strings.TrimSpace(value), 120)
}

func retrievalAblationNullableDigest(value string) any {
	if !utilitySHA256DigestValid(value) {
		return nil
	}
	return value
}

func retrievalAblationContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func summarizeRetrievalAblations(reports []map[string]any) map[string]any {
	rowCount := 0
	changedResults := 0
	citationLosses := 0
	promotionEligible := 0
	for _, report := range reports {
		summary := anyMap(report["summary"])
		rowCount += anyToInt(summary["evaluated_target_count"], 0)
		changedResults += anyToInt(summary["changed_result_row_count"], 0)
		citationLosses += anyToInt(summary["citation_loss_row_count"], 0)
		promotionEligible += anyToInt(summary["promotion_eligible_row_count"], 0)
	}
	return map[string]any{
		"schema_id":                    retrievalAblationReportSchemaID,
		"version":                      1,
		"case_count":                   len(reports),
		"row_count":                    rowCount,
		"changed_result_row_count":     changedResults,
		"citation_loss_row_count":      citationLosses,
		"promotion_eligible_row_count": promotionEligible,
		"cases":                        reports,
		"utility_inferred":             false,
	}
}
