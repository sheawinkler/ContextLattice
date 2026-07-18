package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	contextPackQualitySchemaID          = "contextlattice_context_pack_quality.v1"
	contextPackQualityOutcomeSchemaID   = "contextlattice_context_pack_outcome.v1"
	contextPackQualityTelemetrySchemaID = "contextlattice_context_pack_quality_telemetry.v1"
)

type contextPackQualityTelemetry struct {
	mu                            sync.Mutex
	limit                         int
	ledger                        *contextPackQualityLedger
	samples                       []map[string]any
	outcomes                      []map[string]any
	proofSamples                  proofTimelineMapRing
	proofOutcomes                 proofTimelineMapRing
	outcomeKeys                   map[string]struct{}
	sampleCount                   int64
	outcomeCount                  int64
	calibrationOutcomeCount       int64
	exactTokenSamples             int64
	totalQualityScore             int64
	totalExactPromptSaved         int64
	totalModeledInferenceAvoided  int64
	totalModeledExtraCallsMilli   int64
	firstPassSuccessCount         int64
	repairRequiredCount           int64
	totalRetryCount               int64
	totalObservedFollowupTokens   int64
	providerUsageCount            int64
	totalProviderPromptTokens     int64
	totalProviderCompletionTokens int64
	totalProviderTotalTokens      int64
	lastSampleAt                  string
	lastOutcomeAt                 string
}

type contextPackQualityLedger struct {
	mu          sync.Mutex
	enabled     bool
	path        string
	maxBytes    int64
	maxSamples  int
	parseErrors int
	writeErrors int
	loadedRows  int
	lastWriteAt string
	lastError   string
}

func newContextPackQualityTelemetry(limit int) *contextPackQualityTelemetry {
	if limit <= 0 {
		limit = 100
	}
	t := &contextPackQualityTelemetry{
		limit:       limit,
		ledger:      newContextPackQualityLedgerFromEnv(),
		samples:     make([]map[string]any, 0, limit),
		outcomes:    make([]map[string]any, 0, limit),
		outcomeKeys: make(map[string]struct{}),
	}
	t.loadPersistedRows()
	return t
}

func newContextPackQualityLedgerFromEnv() *contextPackQualityLedger {
	enabled := envBool("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", true)
	path := contextPackQualityLedgerPath()
	if strings.TrimSpace(path) == "" {
		enabled = false
	}
	maxBytes := int64(clampInt(envInt("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_BYTES", 2*1024*1024), 64*1024, 64*1024*1024))
	maxSamples := clampInt(envInt("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_SAMPLES", 1000), 20, 20000)
	ledger := &contextPackQualityLedger{enabled: enabled, path: path, maxBytes: maxBytes, maxSamples: maxSamples}
	if enabled {
		dedicatedParent := strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")) == ""
		if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
			ledger.enabled = false
			ledger.lastError = err.Error()
		}
	}
	return ledger
}

func contextPackQualityLedgerPath() string {
	if explicit := strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")); explicit != "" {
		return filepath.Clean(explicit)
	}
	root := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ROOT"))
	if root == "" {
		root = strings.TrimSpace(os.Getenv("MEMORY_BANK_ROOT"))
	}
	if root != "" {
		return filepath.Clean(filepath.Join(root, "_contextlattice", "context_pack_quality_ledger.ndjson"))
	}
	return filepath.Clean(filepath.Join(".data", "orchestrator", "context_pack_quality_ledger.ndjson"))
}

func (s *server) recordContextPackQuality(sample map[string]any) {
	if s == nil || s.contextPackQuality == nil || len(sample) == 0 {
		return
	}
	s.contextPackQuality.recordQuality(sample)
}

func (s *server) recordContextPackQualityOutcome(sample map[string]any) bool {
	if s == nil || s.contextPackQuality == nil || len(sample) == 0 {
		return false
	}
	return s.contextPackQuality.recordOutcome(sample)
}

func (s *server) contextPackQualityTelemetrySnapshot() map[string]any {
	if s == nil || s.contextPackQuality == nil {
		return defaultContextPackQualityTelemetrySnapshot(nil)
	}
	return s.contextPackQuality.snapshot()
}

func defaultContextPackQualityTelemetrySnapshot(ledger *contextPackQualityLedger) map[string]any {
	return map[string]any{
		"schema_id":                              contextPackQualityTelemetrySchemaID,
		"version":                                1,
		"updatedAt":                              nowUTCISO(),
		"sample_count":                           0,
		"outcome_sample_count":                   0,
		"calibration_outcome_sample_count":       0,
		"confidence":                             "low",
		"calibration_grade":                      "modeled_counterfactual",
		"average_quality_score":                  0,
		"exact_prompt_tokens_saved":              0,
		"modeled_inference_tokens_avoided":       0,
		"modeled_extra_calls_avoided":            0,
		"observed_first_pass_success_rate":       nil,
		"observed_repair_rate":                   nil,
		"observed_average_retry_count":           nil,
		"observed_followup_tokens":               0,
		"observed_provider_usage_count":          0,
		"observed_provider_prompt_tokens":        0,
		"observed_provider_completion_tokens":    0,
		"observed_provider_total_tokens":         0,
		"observed_average_provider_total_tokens": nil,
		"measurement_limit":                      contextPackQualityMeasurementLimit(false),
		"source":                                 "/telemetry/context-pack-quality",
		"storage":                                contextPackQualityLedgerPublicStatus(ledger),
		"samples":                                []any{},
		"outcomes":                               []any{},
	}
}

func contextPackQualityMeasurementLimit(hasOutcomes bool) string {
	if hasOutcomes {
		return "Exact prompt-token savings are measured from context-pack token counts; modeled inference avoidance is confidence-banded and calibrated by bounded outcome rows."
	}
	return "Exact prompt-token savings are measured from context-pack token counts; inference avoidance is a confidence-banded counterfactual model until outcome rows are posted."
}

func (t *contextPackQualityTelemetry) loadPersistedRows() {
	if t == nil || t.ledger == nil || !t.ledger.enabled || t.ledger.path == "" {
		return
	}
	rows, parseErrors, err := t.ledger.readRows()
	if err != nil {
		t.ledger.setError(err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, row := range rows {
		switch anyToString(row["schema_id"]) {
		case contextPackQualitySchemaID:
			t.applyQualityEntryLocked(row)
		case contextPackQualityOutcomeSchemaID:
			t.applyOutcomeEntryLocked(row)
		}
	}
	t.ledger.loadedRows = len(rows)
	t.ledger.parseErrors = parseErrors
}

func (t *contextPackQualityTelemetry) recordQuality(sample map[string]any) {
	if t == nil {
		return
	}
	entry := contextPackQualityEntryFromSample(sample)
	if len(entry) == 0 {
		return
	}
	t.mu.Lock()
	t.applyQualityEntryLocked(entry)
	t.mu.Unlock()
	if t.ledger != nil && t.ledger.enabled {
		if err := t.ledger.append(entry); err != nil {
			t.ledger.setError(err)
		}
	}
}

func (t *contextPackQualityTelemetry) recordOutcome(sample map[string]any) bool {
	if t == nil {
		return false
	}
	entry := contextPackQualityOutcomeFromSample(sample)
	if len(entry) == 0 {
		return false
	}
	t.mu.Lock()
	recorded := t.applyOutcomeEntryLocked(entry)
	t.mu.Unlock()
	if !recorded {
		return false
	}
	if t.ledger != nil && t.ledger.enabled {
		if err := t.ledger.append(entry); err != nil {
			t.ledger.setError(err)
		}
	}
	return true
}

func contextPackQualityEntryFromSample(sample map[string]any) map[string]any {
	sampleID := strings.TrimSpace(anyToString(sample["sample_id"]))
	if sampleID == "" {
		return nil
	}
	qualityScore := clampInt(anyToInt(sample["quality_score"], 0), 0, 100)
	exactSaved := anyToInt(sample["exact_prompt_tokens_saved"], 0)
	if exactSaved < 0 {
		exactSaved = 0
	}
	modeledAvoided := anyToInt(sample["modeled_inference_tokens_avoided"], 0)
	if modeledAvoided < 0 {
		modeledAvoided = 0
	}
	modeledCalls := anyToFloat(sample["modeled_extra_calls_avoided"])
	if modeledCalls < 0 {
		modeledCalls = 0
	}
	entry := map[string]any{
		"schema_id":                          contextPackQualitySchemaID,
		"version":                            1,
		"capturedAt":                         firstNonEmptyStrings(anyToString(sample["capturedAt"]), nowUTCISO()),
		"sample_id":                          sampleID,
		"query_hash":                         anyToString(sample["query_hash"]),
		"project":                            anyToString(sample["project"]),
		"topic_path":                         anyToString(sample["topic_path"]),
		"quality_score":                      qualityScore,
		"confidence":                         firstNonEmptyStrings(anyToString(sample["confidence"]), "low"),
		"calibration_grade":                  firstNonEmptyStrings(anyToString(sample["calibration_grade"]), "modeled_counterfactual"),
		"exact_prompt_tokens_saved":          exactSaved,
		"modeled_inference_tokens_avoided":   modeledAvoided,
		"modeled_extra_calls_avoided":        roundFloat(modeledCalls, 3),
		"counterfactual_baseline":            firstNonEmptyStrings(anyToString(sample["counterfactual_baseline"]), "raw_candidate_replay"),
		"ranked_evidence_count":              anyToInt(sample["ranked_evidence_count"], 0),
		"high_impact_evidence_count":         anyToInt(sample["high_impact_evidence_count"], 0),
		"omitted_high_value_count":           anyToInt(sample["omitted_high_value_count"], 0),
		"returned_source_count":              anyToInt(sample["returned_source_count"], 0),
		"warning_count":                      anyToInt(sample["warning_count"], 0),
		"tokenizer_exact":                    anyToBool(sample["tokenizer_exact"]),
		"wire_tokens_exact":                  anyToInt(firstPresentAny(sample["wire_tokens_exact"], sample["transport_tokens_exact"]), 0),
		"model_visible_context_tokens_exact": anyToInt(sample["model_visible_context_tokens_exact"], 0),
		"token_budget_active":                anyToBool(sample["token_budget_active"]),
		"source_coverage_complete":           anyToBool(sample["source_coverage_complete"]),
		"graph_context_used":                 anyToBool(sample["graph_context_used"]),
		"model_call_token_basis":             anyToInt(sample["model_call_token_basis"], 0),
		"raw_retry_probability_estimate":     roundFloat(anyToFloat(sample["raw_retry_probability_estimate"]), 3),
		"packed_retry_probability_estimate":  roundFloat(anyToFloat(sample["packed_retry_probability_estimate"]), 3),
	}
	copyProofTimelineIdentity(entry, sample)
	if encoding := anyToString(sample["tokenizer_encoding"]); encoding != "" {
		entry["tokenizer_encoding"] = encoding
	}
	return entry
}

func contextPackQualityOutcomeFromSample(sample map[string]any) map[string]any {
	sampleID := strings.TrimSpace(firstNonEmptyStrings(anyToString(sample["sample_id"]), anyToString(sample["context_pack_quality_sample_id"])))
	taskID := clipText(anyToString(sample["task_id"]), 160)
	firstPassRaw, firstPassPresent := contextPackOutcomeFirstPresent(sample, "first_pass_success", "succeeded_first_pass", "success_first_pass")
	repairRaw, repairPresent := contextPackOutcomeFirstPresent(sample, "repair_required", "needed_repair", "repair")
	retryCount := clampInt(anyToInt(firstPresentAny(sample["retry_count"], sample["retries"]), 0), 0, 50)
	followupTokens := anyToInt(firstPresentAny(sample["followup_tokens"], sample["actual_followup_tokens"], sample["repair_tokens"], sample["observed_followup_tokens"]), 0)
	if followupTokens < 0 {
		followupTokens = 0
	}
	providerUsage := anyMap(firstPresentAny(sample["provider_usage"], sample["usage"], sample["token_usage"]))
	providerPromptTokens := anyToInt(firstPresentAny(
		sample["provider_prompt_tokens"],
		sample["prompt_tokens"],
		sample["input_tokens"],
		providerUsage["prompt_tokens"],
		providerUsage["input_tokens"],
	), 0)
	providerCompletionTokens := anyToInt(firstPresentAny(
		sample["provider_completion_tokens"],
		sample["completion_tokens"],
		sample["output_tokens"],
		providerUsage["completion_tokens"],
		providerUsage["output_tokens"],
	), 0)
	providerTotalTokens := anyToInt(firstPresentAny(
		sample["provider_total_tokens"],
		sample["total_tokens"],
		providerUsage["total_tokens"],
	), 0)
	if providerPromptTokens < 0 {
		providerPromptTokens = 0
	}
	if providerCompletionTokens < 0 {
		providerCompletionTokens = 0
	}
	if providerTotalTokens < 0 {
		providerTotalTokens = 0
	}
	if providerTotalTokens == 0 && providerPromptTokens+providerCompletionTokens > 0 {
		providerTotalTokens = providerPromptTokens + providerCompletionTokens
	}
	outcomeSource := firstNonEmptyStrings(clipText(anyToString(sample["outcome_source"]), 80), "agent_report")
	calibrationRaw, calibrationPresent := contextPackOutcomeFirstPresent(sample, "calibration_eligible")
	calibrationEligible := true
	if calibrationPresent {
		calibrationEligible = anyToBool(calibrationRaw)
	}
	outcomeClass := clipText(anyToString(sample["outcome_class"]), 80)
	if outcomeClass == "" {
		if anyToBool(firstPassRaw) {
			outcomeClass = "success"
		} else if anyToBool(repairRaw) || retryCount > 0 {
			outcomeClass = "repair_required"
		} else {
			outcomeClass = "unspecified"
		}
	}
	attribution := firstNonEmptyStrings(clipText(anyToString(sample["context_attribution"]), 80), "unknown")
	outcomeID := clipText(anyToString(sample["outcome_id"]), 200)
	if outcomeID == "" {
		seed := strings.Join([]string{
			sampleID,
			taskID,
			outcomeSource,
			outcomeClass,
			anyToString(anyToBool(firstPassRaw)),
			anyToString(anyToBool(repairRaw)),
			anyToString(retryCount),
			anyToString(followupTokens),
		}, "\x00")
		outcomeID = "cpo_" + sha256Hex(seed)[:24]
	}
	entry := map[string]any{
		"schema_id":                contextPackQualityOutcomeSchemaID,
		"version":                  1,
		"capturedAt":               firstNonEmptyStrings(anyToString(sample["capturedAt"]), anyToString(sample["captured_at"]), nowUTCISO()),
		"outcome_id":               outcomeID,
		"sample_id":                sampleID,
		"task_id":                  taskID,
		"project":                  clipText(anyToString(sample["project"]), 160),
		"task_class":               clipText(anyToString(sample["task_class"]), 80),
		"first_pass_success":       anyToBool(firstPassRaw),
		"repair_required":          anyToBool(repairRaw),
		"retry_count":              retryCount,
		"observed_followup_tokens": followupTokens,
		"outcome_source":           outcomeSource,
		"outcome_class":            outcomeClass,
		"context_attribution":      attribution,
		"calibration_eligible":     calibrationEligible,
	}
	copyProofTimelineIdentity(entry, sample)
	if policyID := clipText(strings.TrimSpace(anyToString(sample["policy_id"])), 160); policyID != "" {
		entry["policy_id"] = policyID
	}
	if policyArm := strings.TrimSpace(strings.ToLower(anyToString(sample["policy_arm"]))); policyArm != "" {
		switch policyArm {
		case "control", "candidate", "shadow", "canary":
			entry["policy_arm"] = policyArm
		}
	}
	if policyPhase := strings.TrimSpace(strings.ToLower(anyToString(sample["policy_phase"]))); policyPhase != "" {
		if _, ok := contextPolicyPhases[policyPhase]; ok {
			entry["policy_phase"] = policyPhase
		}
	}
	if providerPromptTokens > 0 {
		entry["provider_prompt_tokens"] = providerPromptTokens
	}
	if providerCompletionTokens > 0 {
		entry["provider_completion_tokens"] = providerCompletionTokens
	}
	if providerTotalTokens > 0 {
		entry["provider_total_tokens"] = providerTotalTokens
	}
	for _, key := range []string{"utility", "verified_utility", "economics", "pairing", "matched_control"} {
		if value := anyMap(sample[key]); len(value) > 0 {
			entry[key] = compactAgentSessionValue(value, 4)
		}
	}
	for _, key := range []string{
		"utility_value", "verified_utility_value", "utility_unit", "verification_event_id",
		"verification_evidence_digest", "evidence_digest", "verification_passed", "verifier_kind",
		"verifier_id", "latency_ms", "duration_ms", "cost_microusd", "tool_calls",
		"tool_call_count", "failures", "failure_count", "pair_id", "pair_arm", "arm",
		"matched_control_outcome_id", "task_match_digest", "matching_method", "leakage_free",
	} {
		if value, present := sample[key]; present {
			entry[key] = compactAgentSessionValue(value, 2)
		}
	}
	utilityPresent := len(anyMap(entry["utility"])) > 0 || len(anyMap(entry["verified_utility"])) > 0
	if !utilityPresent {
		_, utilityPresent = firstPresentValue(entry["utility_value"], entry["verified_utility_value"])
	}
	if retryCount == 0 && followupTokens == 0 && providerTotalTokens == 0 && !firstPassPresent && !repairPresent && !utilityPresent {
		return nil
	}
	return entry
}

func (t *contextPackQualityTelemetry) sampleForUtility(sampleID string) (map[string]any, bool) {
	if t == nil || strings.TrimSpace(sampleID) == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := t.proofSamples.ordered()
	if len(rows) == 0 {
		rows = t.samples
	}
	for index := len(rows) - 1; index >= 0; index-- {
		if anyToString(rows[index]["sample_id"]) == sampleID {
			return cloneAnyMap(rows[index]), true
		}
	}
	return nil, false
}

func contextPackOutcomeFirstPresent(sample map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		value, ok := sample[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		return value, true
	}
	return nil, false
}

func (t *contextPackQualityTelemetry) applyQualityEntryLocked(entry map[string]any) {
	qualityScore := clampInt(anyToInt(entry["quality_score"], 0), 0, 100)
	t.sampleCount++
	t.totalQualityScore += int64(qualityScore)
	t.totalExactPromptSaved += int64(anyToInt(entry["exact_prompt_tokens_saved"], 0))
	t.totalModeledInferenceAvoided += int64(anyToInt(entry["modeled_inference_tokens_avoided"], 0))
	t.totalModeledExtraCallsMilli += int64(math.Round(anyToFloat(entry["modeled_extra_calls_avoided"]) * 1000))
	if anyToBool(entry["tokenizer_exact"]) {
		t.exactTokenSamples++
	}
	t.lastSampleAt = firstNonEmptyStrings(anyToString(entry["capturedAt"]), nowUTCISO())
	entry["capturedAt"] = t.lastSampleAt
	stored := cloneMap(entry)
	t.samples = append(t.samples, stored)
	t.proofSamples.add(stored)
	if len(t.samples) > t.limit {
		t.samples = append([]map[string]any{}, t.samples[len(t.samples)-t.limit:]...)
	}
}

func (t *contextPackQualityTelemetry) applyOutcomeEntryLocked(entry map[string]any) bool {
	if t.outcomeKeys == nil {
		t.outcomeKeys = make(map[string]struct{})
	}
	outcomeKey := strings.TrimSpace(anyToString(entry["outcome_id"]))
	if outcomeKey == "" {
		outcomeKey = "cpo_" + sha256Hex(anyToString(entry["sample_id"]) + "\x00" + anyToString(entry["capturedAt"]))[:24]
		entry["outcome_id"] = outcomeKey
	}
	if _, exists := t.outcomeKeys[outcomeKey]; exists {
		return false
	}
	t.outcomeKeys[outcomeKey] = struct{}{}
	t.outcomeCount++
	calibrationEligible := true
	if raw, present := entry["calibration_eligible"]; present {
		calibrationEligible = anyToBool(raw)
	}
	entry["calibration_eligible"] = calibrationEligible
	if calibrationEligible {
		t.calibrationOutcomeCount++
		if anyToBool(entry["first_pass_success"]) {
			t.firstPassSuccessCount++
		}
		if anyToBool(entry["repair_required"]) {
			t.repairRequiredCount++
		}
		t.totalRetryCount += int64(anyToInt(entry["retry_count"], 0))
		t.totalObservedFollowupTokens += int64(anyToInt(entry["observed_followup_tokens"], 0))
	}
	providerPromptTokens := anyToInt(entry["provider_prompt_tokens"], 0)
	providerCompletionTokens := anyToInt(entry["provider_completion_tokens"], 0)
	providerTotalTokens := anyToInt(entry["provider_total_tokens"], 0)
	if providerPromptTokens > 0 || providerCompletionTokens > 0 || providerTotalTokens > 0 {
		t.providerUsageCount++
		t.totalProviderPromptTokens += int64(providerPromptTokens)
		t.totalProviderCompletionTokens += int64(providerCompletionTokens)
		t.totalProviderTotalTokens += int64(providerTotalTokens)
	}
	t.lastOutcomeAt = firstNonEmptyStrings(anyToString(entry["capturedAt"]), nowUTCISO())
	entry["capturedAt"] = t.lastOutcomeAt
	stored := cloneMap(entry)
	t.outcomes = append(t.outcomes, stored)
	t.proofOutcomes.add(stored)
	if len(t.outcomes) > t.limit {
		t.outcomes = append([]map[string]any{}, t.outcomes[len(t.outcomes)-t.limit:]...)
	}
	return true
}

func (t *contextPackQualityTelemetry) snapshot() map[string]any {
	if t == nil {
		return defaultContextPackQualityTelemetrySnapshot(nil)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sampleCount == 0 && t.outcomeCount == 0 {
		return defaultContextPackQualityTelemetrySnapshot(t.ledger)
	}
	samples := make([]any, 0, minInt(len(t.samples), 20))
	start := maxInt(0, len(t.samples)-20)
	for _, sample := range t.samples[start:] {
		samples = append(samples, cloneMap(sample))
	}
	outcomes := make([]any, 0, minInt(len(t.outcomes), 20))
	outcomeStart := maxInt(0, len(t.outcomes)-20)
	for _, outcome := range t.outcomes[outcomeStart:] {
		outcomes = append(outcomes, cloneMap(outcome))
	}

	avgQuality := int64(0)
	if t.sampleCount > 0 {
		avgQuality = t.totalQualityScore / t.sampleCount
	}
	modeledCalls := 0.0
	if t.sampleCount > 0 {
		modeledCalls = roundFloat(float64(t.totalModeledExtraCallsMilli)/1000, 3)
	}
	confidence := "low"
	calibration := "modeled_counterfactual"
	if t.calibrationOutcomeCount > 0 {
		calibration = "outcome_seeded"
	}
	if t.calibrationOutcomeCount >= 20 {
		confidence = "high"
		calibration = "outcome_adjusted"
	} else if t.calibrationOutcomeCount >= 5 || t.sampleCount >= 10 {
		confidence = "medium"
	}
	var firstPassRate any
	var repairRate any
	var avgRetries any
	var avgProviderTotal any
	if t.calibrationOutcomeCount > 0 {
		firstPassRate = roundFloat(float64(t.firstPassSuccessCount)/float64(t.calibrationOutcomeCount), 3)
		repairRate = roundFloat(float64(t.repairRequiredCount)/float64(t.calibrationOutcomeCount), 3)
		avgRetries = roundFloat(float64(t.totalRetryCount)/float64(t.calibrationOutcomeCount), 3)
	}
	if t.providerUsageCount > 0 {
		avgProviderTotal = roundFloat(float64(t.totalProviderTotalTokens)/float64(t.providerUsageCount), 3)
	}

	return map[string]any{
		"schema_id":                              contextPackQualityTelemetrySchemaID,
		"version":                                1,
		"updatedAt":                              nowUTCISO(),
		"sample_count":                           t.sampleCount,
		"outcome_sample_count":                   t.outcomeCount,
		"calibration_outcome_sample_count":       t.calibrationOutcomeCount,
		"exact_token_sample_count":               t.exactTokenSamples,
		"confidence":                             confidence,
		"calibration_grade":                      calibration,
		"average_quality_score":                  avgQuality,
		"exact_prompt_tokens_saved":              t.totalExactPromptSaved,
		"modeled_inference_tokens_avoided":       t.totalModeledInferenceAvoided,
		"modeled_extra_calls_avoided":            modeledCalls,
		"observed_first_pass_success_rate":       firstPassRate,
		"observed_repair_rate":                   repairRate,
		"observed_average_retry_count":           avgRetries,
		"observed_followup_tokens":               t.totalObservedFollowupTokens,
		"observed_provider_usage_count":          t.providerUsageCount,
		"observed_provider_prompt_tokens":        t.totalProviderPromptTokens,
		"observed_provider_completion_tokens":    t.totalProviderCompletionTokens,
		"observed_provider_total_tokens":         t.totalProviderTotalTokens,
		"observed_average_provider_total_tokens": avgProviderTotal,
		"last_sample_at":                         t.lastSampleAt,
		"last_outcome_at":                        t.lastOutcomeAt,
		"measurement_limit":                      contextPackQualityMeasurementLimit(t.calibrationOutcomeCount > 0),
		"source":                                 "/telemetry/context-pack-quality",
		"basis":                                  contextPackQualityBasis(),
		"storage":                                contextPackQualityLedgerPublicStatus(t.ledger),
		"samples":                                samples,
		"outcomes":                               outcomes,
	}
}

func contextPackQualityBasis() []any {
	return []any{
		"exact context_pack.token_impact prompt-token delta",
		"ranked evidence count and high-impact evidence mix",
		"source coverage completeness and warning pressure",
		"bounded counterfactual retry probability model",
		"optional posted outcome rows and provider usage counters for calibration",
	}
}

func contextPackQualityLedgerPublicStatus(ledger *contextPackQualityLedger) map[string]any {
	status := map[string]any{"enabled": false, "durability": "memory_only"}
	if ledger == nil {
		return status
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	status["enabled"] = ledger.enabled
	if ledger.enabled {
		status["durability"] = "bounded_ndjson"
	}
	status["max_bytes"] = ledger.maxBytes
	status["max_samples"] = ledger.maxSamples
	status["loaded_rows"] = ledger.loadedRows
	status["parse_errors"] = ledger.parseErrors
	status["write_errors"] = ledger.writeErrors
	status["last_write_at"] = ledger.lastWriteAt
	status["last_error"] = ledger.lastError
	return status
}

func (l *contextPackQualityLedger) readRows() ([]map[string]any, int, error) {
	if l == nil || !l.enabled || l.path == "" {
		return nil, 0, nil
	}
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()
	rows := make([]map[string]any, 0, l.maxSamples)
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		schemaID := anyToString(row["schema_id"])
		if schemaID != contextPackQualitySchemaID && schemaID != contextPackQualityOutcomeSchemaID {
			continue
		}
		rows = append(rows, row)
		if len(rows) > l.maxSamples {
			rows = append([]map[string]any{}, rows[len(rows)-l.maxSamples:]...)
		}
	}
	if err := scanner.Err(); err != nil {
		return rows, parseErrors, err
	}
	return rows, parseErrors, nil
}

func (l *contextPackQualityLedger) append(entry map[string]any) error {
	if l == nil || !l.enabled || l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	file, err := openOwnerOnlyAppend(l.path, false)
	if err != nil {
		l.writeErrors++
		return err
	}
	encoded, err := json.Marshal(entry)
	if err == nil {
		_, err = file.Write(append(encoded, '\n'))
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		l.writeErrors++
		return err
	}
	l.lastWriteAt = nowUTCISO()
	l.lastError = ""
	if stat, statErr := os.Stat(l.path); statErr == nil && stat.Size() > l.maxBytes {
		if pruneErr := l.pruneLocked(); pruneErr != nil {
			l.writeErrors++
			return pruneErr
		}
	}
	return nil
}

func (l *contextPackQualityLedger) pruneLocked() error {
	rows, _, err := l.readRowsUnlocked()
	if err != nil {
		return err
	}
	if len(rows) > l.maxSamples {
		rows = rows[len(rows)-l.maxSamples:]
	}
	encodedRows := make([][]byte, 0, len(rows))
	total := int64(0)
	for i := len(rows) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(rows[i])
		if err != nil {
			continue
		}
		lineBytes := int64(len(encoded) + 1)
		if len(encodedRows) > 0 && total+lineBytes > l.maxBytes {
			break
		}
		encodedRows = append(encodedRows, encoded)
		total += lineBytes
	}
	for i, j := 0, len(encodedRows)-1; i < j; i, j = i+1, j-1 {
		encodedRows[i], encodedRows[j] = encodedRows[j], encodedRows[i]
	}
	tmp := l.path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, ownerOnlyFileMode)
	if err != nil {
		return err
	}
	for _, row := range encodedRows {
		if _, err := file.Write(append(row, '\n')); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	return ensureOwnerOnlyFile(l.path)
}

func (l *contextPackQualityLedger) readRowsUnlocked() ([]map[string]any, int, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()
	rows := []map[string]any{}
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		schemaID := anyToString(row["schema_id"])
		if schemaID == contextPackQualitySchemaID || schemaID == contextPackQualityOutcomeSchemaID {
			rows = append(rows, row)
		}
	}
	return rows, parseErrors, scanner.Err()
}

func (l *contextPackQualityLedger) setError(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastError = tokenImpactLedgerErrorCode(err)
}

func buildContextPackQualitySample(input contextPackQualitySampleInput) map[string]any {
	queryHash := sha256Hex(input.Query)
	sampleSeed := strings.Join([]string{
		nowUTCISO(),
		queryHash,
		anyToString(input.TokenImpact["saved_tokens_estimate"]),
		anyToString(input.TokenImpact["packed_tokens_estimate"]),
	}, "\x00")
	ranked := contextPackAnyList(input.RankedEvidence)
	omitted := contextPackAnyList(input.OmittedHighValueRefs)
	coverage := input.SourceCoverage
	highImpactCount := 0
	for _, raw := range ranked {
		switch anyToString(anyMap(raw)["kind"]) {
		case "decision", "risk", "check", "runbook":
			highImpactCount++
		}
	}
	warningCount := len(input.Warnings)
	returnedSources := len(anyToStringList(coverage["returned"], 64))
	tokenizerExact := anyToBool(input.TokenImpact["tokenizer_exact"])
	tokenBudgetActive := anyToBool(anyMap(input.Compiled["token_budget"])["active"])
	graphUsed := anyToBool(input.GraphQuality["used"])
	coverageComplete := anyToBool(coverage["complete"])
	exactPromptSaved := anyToInt(input.TokenImpact["saved_tokens_estimate"], 0)
	packedTokens := anyToInt(input.TokenImpact["packed_tokens_estimate"], 0)
	qualityScore := contextPackQualityScore(contextPackQualitySignals{
		RankedEvidenceCount:    len(ranked),
		HighImpactCount:        highImpactCount,
		OmittedHighValueCount:  len(omitted),
		ReturnedSourceCount:    returnedSources,
		WarningCount:           warningCount,
		TokenizerExact:         tokenizerExact,
		TokenBudgetActive:      tokenBudgetActive,
		SourceCoverageComplete: coverageComplete,
		GraphUsed:              graphUsed,
		ExactPromptSaved:       exactPromptSaved,
	})
	retryModel := contextPackQualityRetryModel(qualityScore, highImpactCount, returnedSources, warningCount, packedTokens)
	confidence := "low"
	if qualityScore >= 80 && tokenizerExact && warningCount == 0 {
		confidence = "medium"
	}
	sample := map[string]any{
		"schema_id":                          contextPackQualitySchemaID,
		"version":                            1,
		"capturedAt":                         nowUTCISO(),
		"sample_id":                          "cpq_" + sha256Hex(sampleSeed)[:24],
		"query_hash":                         queryHash[:16],
		"project":                            strings.TrimSpace(input.Project),
		"topic_path":                         strings.TrimSpace(input.TopicPath),
		"quality_score":                      qualityScore,
		"confidence":                         confidence,
		"calibration_grade":                  "modeled_counterfactual",
		"exact_prompt_tokens_saved":          exactPromptSaved,
		"modeled_inference_tokens_avoided":   retryModel.ModeledInferenceTokensAvoided,
		"modeled_extra_calls_avoided":        retryModel.ExtraCallsAvoided,
		"counterfactual_baseline":            "raw_candidate_replay",
		"ranked_evidence_count":              len(ranked),
		"high_impact_evidence_count":         highImpactCount,
		"omitted_high_value_count":           len(omitted),
		"returned_source_count":              returnedSources,
		"warning_count":                      warningCount,
		"tokenizer_exact":                    tokenizerExact,
		"tokenizer_encoding":                 anyToString(input.TokenImpact["tokenizer_encoding"]),
		"model_visible_context_tokens_exact": anyToInt(input.TokenImpact["model_visible_context_tokens_exact"], 0),
		"token_budget_active":                tokenBudgetActive,
		"source_coverage_complete":           coverageComplete,
		"graph_context_used":                 graphUsed,
		"model_call_token_basis":             retryModel.ModelCallTokenBasis,
		"raw_retry_probability_estimate":     retryModel.RawRetryProbability,
		"packed_retry_probability_estimate":  retryModel.PackedRetryProbability,
		"measurement_limit":                  contextPackQualityMeasurementLimit(false),
	}
	copyProofTimelineIdentity(sample, map[string]any{
		"session_id":        input.SessionID,
		"task_id":           input.TaskID,
		"task_identity_id":  input.TaskIdentityID,
		"execution_lane_id": input.ExecutionLaneID,
		"agent_id":          input.AgentID,
	})
	return sample
}

type contextPackQualitySampleInput struct {
	Query                string
	Project              string
	TopicPath            string
	SessionID            string
	TaskID               string
	TaskIdentityID       string
	ExecutionLaneID      string
	AgentID              string
	TokenImpact          map[string]any
	Compiled             map[string]any
	SourceCoverage       map[string]any
	GraphQuality         map[string]any
	RankedEvidence       any
	OmittedHighValueRefs any
	Warnings             []string
}

type contextPackQualitySignals struct {
	RankedEvidenceCount    int
	HighImpactCount        int
	OmittedHighValueCount  int
	ReturnedSourceCount    int
	WarningCount           int
	TokenizerExact         bool
	TokenBudgetActive      bool
	SourceCoverageComplete bool
	GraphUsed              bool
	ExactPromptSaved       int
}

func contextPackQualityScore(signals contextPackQualitySignals) int {
	score := 35
	if signals.ExactPromptSaved > 0 {
		score += 10
	}
	if signals.TokenizerExact {
		score += 8
	}
	if signals.TokenBudgetActive {
		score += 8
	}
	if signals.SourceCoverageComplete {
		score += 7
	}
	if signals.GraphUsed {
		score += 4
	}
	score += minInt(signals.RankedEvidenceCount*3, 18)
	score += minInt(signals.HighImpactCount*5, 15)
	score += minInt(signals.ReturnedSourceCount*2, 8)
	score -= minInt(signals.OmittedHighValueCount*2, 8)
	score -= minInt(signals.WarningCount*4, 16)
	return clampInt(score, 0, 100)
}

type contextPackQualityRetryEstimate struct {
	RawRetryProbability           float64
	PackedRetryProbability        float64
	ExtraCallsAvoided             float64
	ModelCallTokenBasis           int
	ModeledInferenceTokensAvoided int
}

func contextPackQualityRetryModel(qualityScore int, highImpactCount int, returnedSources int, warningCount int, packedTokens int) contextPackQualityRetryEstimate {
	rawProb := 0.34
	packedProb := rawProb -
		float64(qualityScore)*0.0022 -
		float64(minInt(highImpactCount, 5))*0.012 -
		float64(minInt(returnedSources, 5))*0.004 +
		float64(minInt(warningCount, 6))*0.025
	packedProb = clampFloat(packedProb, 0.05, rawProb)
	extraCalls := roundFloat(math.Max(0, rawProb-packedProb), 3)
	tokenBasis := maxInt(packedTokens+1200, 4000)
	return contextPackQualityRetryEstimate{
		RawRetryProbability:           roundFloat(rawProb, 3),
		PackedRetryProbability:        roundFloat(packedProb, 3),
		ExtraCallsAvoided:             extraCalls,
		ModelCallTokenBasis:           tokenBasis,
		ModeledInferenceTokensAvoided: int(math.Round(extraCalls * float64(tokenBasis))),
	}
}

func (s *server) telemetryContextPackQualityRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.contextPackQualityTelemetrySnapshot())
}

func (s *server) telemetryContextPackQualityOutcomeRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	entry := contextPackQualityOutcomeFromSample(payload)
	if len(entry) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "empty outcome payload"})
		return
	}
	recorded := s.recordContextPackQualityOutcome(entry)
	utilityObservation, utilityRecorded, utilityErr := s.recordUtilityOutcome(entry)
	var utilityStore *utilityLedgerStore
	if s != nil && s.utility != nil {
		utilityStore = s.utility.store
	}
	if errors.Is(utilityErr, errUtilityOutcomeConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "utility_outcome_conflict",
			"detail":   "outcome_id is already bound to a different utility source claim",
			"recorded": recorded, "utility_recorded": false, "utility_observation": utilityObservation,
		})
		return
	}
	if errors.Is(utilityErr, errUtilityPersistenceUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "utility_persistence_unavailable",
			"detail":   "the authoritative outcome was accepted, but the derived Utility Ledger did not acknowledge durable persistence",
			"recorded": recorded, "utility_recorded": false,
			"utility_storage": utilityStorageStatus(utilityStore),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "recorded": recorded, "duplicate": !recorded, "outcome": entry,
		"utility_recorded": utilityRecorded, "utility_observation": utilityObservation,
		"utility_storage": utilityStorageStatus(utilityStore),
		"telemetry":       s.contextPackQualityTelemetrySnapshot(),
	})
}
