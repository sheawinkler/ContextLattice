package main

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	derivedRegressionSuiteSchemaID  = "derived_regression_suite.v1"
	derivedRegressionDefaultMaxRows = 128
	derivedRegressionMaxRows        = 256
	derivedRegressionMaxProposals   = 128
)

type derivedRegressionSuiteOptions struct {
	MaxRows      int
	MaxProposals int
}

type derivedRegressionCandidate struct {
	proposalID    string
	contentID     string
	partition     string
	fixture       map[string]any
	sourceDigest  string
	sourceOutcome string
	leakageKeys   []string
	redaction     map[string]any
}

// buildDerivedRegressionSuite is an offline proposal builder. It never admits
// cases or mutates the saved evaluation suite.
func buildDerivedRegressionSuite(rows []map[string]any, options derivedRegressionSuiteOptions) map[string]any {
	maxRows := clampInt(options.MaxRows, 1, derivedRegressionMaxRows)
	if options.MaxRows <= 0 {
		maxRows = derivedRegressionDefaultMaxRows
	}
	maxProposals := clampInt(options.MaxProposals, 1, derivedRegressionMaxProposals)
	if options.MaxProposals <= 0 {
		maxProposals = derivedRegressionMaxProposals
	}

	inputCount := len(rows)
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	candidates := make([]derivedRegressionCandidate, 0, len(rows))
	rejections := make([]map[string]any, 0)
	for _, row := range rows {
		candidate, rejection := derivedRegressionCandidateFromRow(row)
		if rejection != nil {
			rejections = append(rejections, rejection)
			continue
		}
		candidates = append(candidates, *candidate)
	}

	partitionsByLeakageKey := map[string]map[string]struct{}{}
	for _, candidate := range candidates {
		for _, key := range candidate.leakageKeys {
			if partitionsByLeakageKey[key] == nil {
				partitionsByLeakageKey[key] = map[string]struct{}{}
			}
			partitionsByLeakageKey[key][candidate.partition] = struct{}{}
		}
	}
	eligible := make([]derivedRegressionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		leaked := false
		for _, key := range candidate.leakageKeys {
			if len(partitionsByLeakageKey[key]) > 1 {
				leaked = true
				break
			}
		}
		if leaked {
			rejections = append(rejections, derivedRegressionRejection(candidate.sourceDigest, "train_holdout_leakage"))
			continue
		}
		eligible = append(eligible, candidate)
	}

	type proposalAccumulator struct {
		candidate      derivedRegressionCandidate
		sourceDigests  map[string]struct{}
		sourceOutcomes map[string]struct{}
		rowCount       int
	}
	grouped := map[string]*proposalAccumulator{}
	for _, candidate := range eligible {
		accumulator := grouped[candidate.proposalID]
		if accumulator == nil {
			accumulator = &proposalAccumulator{
				candidate:      candidate,
				sourceDigests:  map[string]struct{}{},
				sourceOutcomes: map[string]struct{}{},
			}
			grouped[candidate.proposalID] = accumulator
		}
		accumulator.sourceDigests[candidate.sourceDigest] = struct{}{}
		accumulator.sourceOutcomes[candidate.sourceOutcome] = struct{}{}
		accumulator.rowCount++
	}

	proposalIDs := make([]string, 0, len(grouped))
	for proposalID := range grouped {
		proposalIDs = append(proposalIDs, proposalID)
	}
	sort.Strings(proposalIDs)
	proposalLimitOmitted := maxInt(0, len(proposalIDs)-maxProposals)
	if len(proposalIDs) > maxProposals {
		proposalIDs = proposalIDs[:maxProposals]
	}
	proposals := make([]map[string]any, 0, len(proposalIDs))
	deduplicatedSources := 0
	trainCount := 0
	holdoutCount := 0
	for _, proposalID := range proposalIDs {
		accumulator := grouped[proposalID]
		sourceDigests := derivedRegressionSortedSet(accumulator.sourceDigests)
		sourceOutcomes := derivedRegressionSortedSet(accumulator.sourceOutcomes)
		if accumulator.rowCount > 1 {
			deduplicatedSources += accumulator.rowCount - 1
		}
		if accumulator.candidate.partition == "train" {
			trainCount++
		} else {
			holdoutCount++
		}
		proposals = append(proposals, map[string]any{
			"proposal_id":     proposalID,
			"content_id":      accumulator.candidate.contentID,
			"immutable":       true,
			"partition":       accumulator.candidate.partition,
			"case":            cloneJSONMap(accumulator.candidate.fixture),
			"source_digests":  sourceDigests,
			"source_outcomes": sourceOutcomes,
			"leakage_checks": map[string]any{
				"passed":                  true,
				"declared_leakage_free":   true,
				"train_holdout_separated": true,
				"checked_dimensions":      []string{"case_content", "query", "task_identity", "verification_evidence"},
			},
			"redaction": accumulator.candidate.redaction,
			"review": map[string]any{
				"status":             "pending_review",
				"review_required":    true,
				"admission_eligible": false,
				"admitted":           false,
			},
		})
	}

	sort.Slice(rejections, func(i, j int) bool {
		left := anyToString(rejections[i]["source_digest"]) + "\x00" + strings.Join(anyToStringList(rejections[i]["reasons"], 32), "\x00")
		right := anyToString(rejections[j]["source_digest"]) + "\x00" + strings.Join(anyToStringList(rejections[j]["reasons"], 32), "\x00")
		return left < right
	})
	suiteBasis := map[string]any{
		"schema_id":  derivedRegressionSuiteSchemaID,
		"proposals":  proposals,
		"rejections": rejections,
	}
	suiteDigest := derivedRegressionDigest(suiteBasis)
	return map[string]any{
		"schema_id":          derivedRegressionSuiteSchemaID,
		"version":            1,
		"suite_id":           "drs_" + strings.TrimPrefix(suiteDigest, "sha256:")[:24],
		"content_id":         suiteDigest,
		"immutable":          true,
		"status":             "pending_review",
		"review_required":    true,
		"admission_eligible": false,
		"admitted":           false,
		"proposals":          proposals,
		"rejections":         rejections,
		"summary": map[string]any{
			"input_row_count":              inputCount,
			"evaluated_row_count":          len(rows),
			"input_limit_omitted_count":    maxInt(0, inputCount-len(rows)),
			"proposal_count":               len(proposals),
			"proposal_limit_omitted_count": proposalLimitOmitted,
			"rejected_row_count":           len(rejections),
			"deduplicated_source_count":    deduplicatedSources,
			"train_proposal_count":         trainCount,
			"holdout_proposal_count":       holdoutCount,
			"partition_complete":           trainCount > 0 && holdoutCount > 0,
		},
		"computation": map[string]any{
			"path":                  "offline_saved_eval",
			"bounded":               true,
			"retrieval_hot_path":    false,
			"saved_suite_mutations": 0,
		},
		"measurement_limit": "Proposals require explicit verified fixture material and human review. Rejected or absent fixture data is never reconstructed from hashes or aggregate telemetry.",
	}
}

func derivedRegressionCandidateFromRow(row map[string]any) (*derivedRegressionCandidate, map[string]any) {
	fixture, redaction := normalizeDerivedRegressionFixture(anyMap(row["regression_case"]))
	sourceDigest := derivedRegressionSourceDigest(row, fixture)
	reasons := make([]string, 0)
	if len(fixture) == 0 {
		reasons = append(reasons, "explicit_regression_case_missing")
	} else {
		if strings.TrimSpace(anyToString(fixture["query"])) == "" {
			reasons = append(reasons, "query_missing_after_redaction")
		}
		if strings.TrimSpace(anyToString(fixture["project"])) == "" || strings.TrimSpace(anyToString(fixture["topic_path"])) == "" {
			reasons = append(reasons, "scope_missing")
		}
		positiveCount := len(anyToStringList(fixture["expected_files"], 32)) + len(anyToStringList(fixture["expected_substrings"], 32)) + len(anyToStringList(fixture["expected_numeric"], 32))
		if positiveCount == 0 {
			reasons = append(reasons, "positive_expectation_missing")
		}
		negativeCount := len(anyToStringList(fixture["negative_files"], 32)) + len(anyToStringList(fixture["negative_substrings"], 32))
		if negativeCount == 0 {
			reasons = append(reasons, "negative_expectation_missing")
		}
	}

	verificationPassed := anyToBool(row["verification_passed"])
	if utility := anyMap(firstPresentAny(row["verified_utility"], row["utility"])); !verificationPassed {
		verificationPassed = anyToBool(utility["verification_passed"])
	}
	if !verificationPassed {
		reasons = append(reasons, "verification_missing")
	}
	verificationDigest := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(row["verification_evidence_digest"]),
		anyToString(anyMap(firstPresentAny(row["verified_utility"], row["utility"]))["verification_evidence_digest"]),
	))
	if !utilitySHA256DigestValid(verificationDigest) {
		reasons = append(reasons, "verification_digest_invalid")
	}
	if raw, present := row["calibration_eligible"]; present && !anyToBool(raw) {
		reasons = append(reasons, "outcome_promotion_ineligible")
	}
	trafficClass := strings.ToLower(strings.TrimSpace(anyToString(row["traffic_class"])))
	if trafficClass == "" {
		reasons = append(reasons, "traffic_class_unverified")
	} else if trafficClass == "synthetic" || anyToBool(row["synthetic"]) {
		reasons = append(reasons, "synthetic_source")
	}

	partition := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(
		anyToString(row["regression_partition"]),
		anyToString(row["partition"]),
		anyToString(anyMap(row["regression_case"])["partition"]),
	)))
	if partition != "train" && partition != "holdout" {
		reasons = append(reasons, "partition_invalid")
	}
	leakageRaw, leakagePresent := contextPackOutcomeFirstPresent(row, "leakage_free")
	if !leakagePresent {
		leakageRaw, leakagePresent = contextPackOutcomeFirstPresent(anyMap(row["pairing"]), "leakage_free")
	}
	if !leakagePresent || !anyToBool(leakageRaw) {
		reasons = append(reasons, "leakage_check_missing_or_failed")
	}

	sourceOutcome := derivedRegressionOutcomePolarity(row)
	if sourceOutcome == "" {
		reasons = append(reasons, "verified_success_or_failure_missing")
	}
	if !derivedRegressionStabilityVerified(row) {
		reasons = append(reasons, "instability_rejected")
	}
	if len(reasons) > 0 {
		return nil, derivedRegressionRejection(sourceDigest, reasons...)
	}

	contentBasis := map[string]any{"partition": partition, "case": fixture}
	contentID := derivedRegressionDigest(contentBasis)
	contentHash := strings.TrimPrefix(contentID, "sha256:")
	leakageKeys := []string{
		"case:" + derivedRegressionDigest(fixture),
		"query:" + sha256Hex(strings.ToLower(strings.Join(strings.Fields(anyToString(fixture["query"])), " "))),
		"verification:" + verificationDigest,
	}
	for _, key := range []string{"task_id", "task_identity_id", "task_match_digest"} {
		if value := strings.ToLower(strings.TrimSpace(anyToString(row[key]))); value != "" {
			leakageKeys = append(leakageKeys, key+":"+sha256Hex(value))
		}
	}
	sort.Strings(leakageKeys)
	return &derivedRegressionCandidate{
		proposalID:    "drp_" + contentHash[:24],
		contentID:     contentID,
		partition:     partition,
		fixture:       fixture,
		sourceDigest:  sourceDigest,
		sourceOutcome: sourceOutcome,
		leakageKeys:   leakageKeys,
		redaction:     redaction,
	}, nil
}

func normalizeDerivedRegressionFixture(raw map[string]any) (map[string]any, map[string]any) {
	if len(raw) == 0 {
		return nil, map[string]any{"applied": false, "count": 0}
	}
	selected := map[string]any{
		"query":               raw["query"],
		"project":             raw["project"],
		"topic_path":          raw["topic_path"],
		"expected_files":      derivedRegressionPortableList(raw["expected_files"]),
		"expected_substrings": derivedRegressionPortableList(raw["expected_substrings"]),
		"expected_numeric":    derivedRegressionPortableList(raw["expected_numeric"]),
		"negative_files": derivedRegressionPortableList(firstPresentAny(
			raw["negative_files"], raw["negative_expected_files"], raw["forbidden_files"],
		)),
		"negative_substrings": derivedRegressionPortableList(firstPresentAny(
			raw["negative_substrings"], raw["forbidden_substrings"],
		)),
		"sources":          derivedRegressionPortableList(raw["sources"]),
		"retrieval_mode":   raw["retrieval_mode"],
		"retrieval_intent": raw["retrieval_intent"],
		"case_kind":        raw["case_kind"],
		"limit":            raw["limit"],
	}
	stats := &portableRedactionStats{}
	safe := portableMap(selected, stats)
	query := clipText(strings.Join(strings.Fields(anyToString(safe["query"])), " "), 600)
	project := clipText(strings.TrimSpace(anyToString(safe["project"])), 160)
	topicPath := clipText(recallEvalNormalizeCaseTopic(anyToString(safe["topic_path"])), 240)
	fixture := map[string]any{
		"query":               query,
		"project":             project,
		"topic_path":          topicPath,
		"limit":               clampInt(anyToInt(safe["limit"], 10), 1, 100),
		"expected_files":      derivedRegressionStrings(safe["expected_files"], 16, true),
		"expected_substrings": derivedRegressionStrings(safe["expected_substrings"], 16, true),
		"expected_numeric":    derivedRegressionStrings(safe["expected_numeric"], 16, false),
		"negative_files":      derivedRegressionStrings(safe["negative_files"], 16, true),
		"negative_substrings": derivedRegressionStrings(safe["negative_substrings"], 16, true),
		"sources":             derivedRegressionStrings(safe["sources"], 16, true),
		"retrieval_mode":      clipText(strings.ToLower(strings.TrimSpace(anyToString(safe["retrieval_mode"]))), 40),
		"retrieval_intent":    clipText(strings.ToLower(strings.TrimSpace(anyToString(safe["retrieval_intent"]))), 40),
		"case_kind":           clipText(strings.ToLower(strings.TrimSpace(anyToString(safe["case_kind"]))), 40),
	}
	redactionCount := stats.SecretKeys + stats.Tokens + stats.Paths + stats.Clipped + stats.Lists
	return fixture, map[string]any{
		"applied":               redactionCount > 0,
		"count":                 redactionCount,
		"secret_keys_removed":   stats.SecretKeys,
		"token_values_redacted": stats.Tokens,
		"local_paths_redacted":  stats.Paths,
		"values_clipped":        stats.Clipped,
		"list_items_omitted":    stats.Lists,
		"before_hashing":        true,
	}
}

func derivedRegressionPortableList(value any) any {
	if values, ok := value.([]string); ok {
		out := make([]any, 0, len(values))
		for _, item := range values {
			out = append(out, item)
		}
		return out
	}
	return value
}

func derivedRegressionLedgerFields(sample map[string]any) map[string]any {
	out := map[string]any{}
	if fixture, redaction := normalizeDerivedRegressionFixture(anyMap(sample["regression_case"])); len(fixture) > 0 {
		out["regression_case"] = fixture
		out["regression_redaction"] = redaction
	}
	if partition := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(
		anyToString(sample["regression_partition"]),
		anyToString(sample["partition"]),
		anyToString(anyMap(sample["regression_case"])["partition"]),
	))); partition != "" {
		out["regression_partition"] = clipText(partition, 20)
	}
	if stability := normalizeDerivedRegressionStability(anyMap(sample["stability"])); len(stability) > 0 {
		out["stability"] = stability
	}
	if trafficClass := strings.ToLower(strings.TrimSpace(anyToString(sample["traffic_class"]))); trafficClass != "" {
		out["traffic_class"] = clipText(trafficClass, 40)
	}
	if raw, present := sample["synthetic"]; present {
		out["synthetic"] = anyToBool(raw)
	}
	return out
}

func normalizeDerivedRegressionStability(raw map[string]any) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	return map[string]any{
		"stable":         anyToBool(raw["stable"]),
		"run_count":      clampInt(anyToInt(raw["run_count"], 0), 0, 20),
		"result_digests": derivedRegressionSequence(raw["result_digests"], 20, true),
		"external_state": anyToBool(raw["external_state"]),
	}
}

func derivedRegressionStabilityVerified(row map[string]any) bool {
	stability := normalizeDerivedRegressionStability(anyMap(row["stability"]))
	if !anyToBool(stability["stable"]) || anyToBool(stability["external_state"]) || anyToInt(stability["run_count"], 0) < 2 {
		return false
	}
	digests := anyToStringList(stability["result_digests"], 20)
	if len(digests) < 2 || !utilitySHA256DigestValid(digests[0]) {
		return false
	}
	for _, digest := range digests[1:] {
		if digest != digests[0] || !utilitySHA256DigestValid(digest) {
			return false
		}
	}
	return true
}

func derivedRegressionOutcomePolarity(row map[string]any) string {
	firstPassRaw, firstPassPresent := contextPackOutcomeFirstPresent(row, "first_pass_success", "succeeded_first_pass", "success_first_pass")
	repairRaw, repairPresent := contextPackOutcomeFirstPresent(row, "repair_required", "needed_repair", "repair")
	firstPass := firstPassPresent && anyToBool(firstPassRaw)
	repair := repairPresent && anyToBool(repairRaw)
	class := strings.ToLower(strings.TrimSpace(anyToString(row["outcome_class"])))
	if firstPass && repair {
		return ""
	}
	if firstPass || class == "success" {
		if repair || strings.Contains(class, "failure") {
			return ""
		}
		return "verified_success"
	}
	if repair || class == "failure" || class == "repair_required" || class == "task_failure" || class == "regression_failure" {
		return "verified_failure"
	}
	return ""
}

func derivedRegressionSourceDigest(row, fixture map[string]any) string {
	verification := anyMap(firstPresentAny(row["verified_utility"], row["utility"]))
	basis := map[string]any{
		"outcome_id":                   portableString(anyToString(row["outcome_id"]), &portableRedactionStats{}),
		"sample_id":                    portableString(anyToString(row["sample_id"]), &portableRedactionStats{}),
		"task_id":                      portableString(anyToString(row["task_id"]), &portableRedactionStats{}),
		"task_identity_id":             portableString(anyToString(row["task_identity_id"]), &portableRedactionStats{}),
		"verification_evidence_digest": firstNonEmptyStrings(anyToString(row["verification_evidence_digest"]), anyToString(verification["verification_evidence_digest"])),
		"outcome_class":                strings.ToLower(strings.TrimSpace(anyToString(row["outcome_class"]))),
		"regression_case":              fixture,
	}
	return derivedRegressionDigest(basis)
}

func derivedRegressionDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "sha256:" + sha256Hex("")
	}
	return "sha256:" + sha256Hex(string(raw))
}

func derivedRegressionRejection(sourceDigest string, reasons ...string) map[string]any {
	return map[string]any{
		"source_digest": sourceDigest,
		"status":        "rejected",
		"reasons":       derivedRegressionStrings(reasons, 16, true),
	}
}

func derivedRegressionStrings(value any, limit int, lower bool) []string {
	values := anyToStringList(value, limit*2)
	seen := map[string]struct{}{}
	out := make([]string, 0, minInt(len(values), limit))
	for _, item := range values {
		item = strings.TrimSpace(item)
		if lower {
			item = strings.ToLower(item)
		}
		item = clipText(item, 360)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

func derivedRegressionSequence(value any, limit int, lower bool) []string {
	values := anyToStringList(value, limit)
	out := make([]string, 0, minInt(len(values), limit))
	for _, item := range values {
		item = strings.TrimSpace(item)
		if lower {
			item = strings.ToLower(item)
		}
		item = clipText(item, 360)
		if item != "" {
			out = append(out, item)
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func derivedRegressionSortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
