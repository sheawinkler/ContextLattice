package main

import "strings"

// The fallback receipt is an internal diagnostic only. It is deliberately
// count-and-stage based: no query, evidence ref, module payload, or source
// content crosses the public response boundary.
const (
	recallResponseFallbackStageReceiptKey       = "_recall_response_fallback_stage_receipt"
	recallResponseFallbackStageSchema           = "recall_response.fallback_stage_receipt.v1"
	recallResponseFallbackStageCompression      = "compression"
	recallResponseFallbackStageProtectedWitness = "protected_witness"
	recallResponseFallbackStageModuleValidation = "module_validation"
	recallResponseFallbackStageFit              = "fit"
)

var recallResponseFallbackStages = map[string]bool{
	recallResponseFallbackStageCompression:      true,
	recallResponseFallbackStageProtectedWitness: true,
	recallResponseFallbackStageModuleValidation: true,
	recallResponseFallbackStageFit:              true,
}

func recallResponseFallbackStageReceipt(stage string, compression recallResponseProofCompression, candidate map[string]any) map[string]any {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = strings.TrimSpace(compression.FailureStage)
	}
	if !recallResponseFallbackStages[stage] {
		stage = recallResponseFallbackStageCompression
	}
	compactBytes, compactTokens := 0, 0
	optionalContext, optionalModules := 0, 0
	if candidate != nil {
		compactBytes, compactTokens = recallResponseCompactBudget(candidate)
		for _, raw := range contextPackAnyList(candidate["evidence"]) {
			if anyToString(anyMap(raw)["role"]) == "context" {
				optionalContext++
			}
		}
		for _, raw := range contextPackAnyList(anyMap(candidate["answer"])["components"]) {
			if !recallResponseModuleSafety[anyToString(anyMap(raw)["kind"])] {
				optionalModules++
			}
		}
	}
	// Keep diagnostic dimensions bounded even if a malformed candidate reaches
	// this helper. A max+1 value means "at or above the limit" without emitting
	// a potentially sensitive or unbounded measurement.
	bounded := func(value, limit int) int {
		if value < 0 {
			return 0
		}
		if value > limit {
			return limit + 1
		}
		return value
	}
	receipt := map[string]any{
		"schema_id":              recallResponseFallbackStageSchema,
		"stage":                  stage,
		"status":                 "fallback",
		"candidate_count":        bounded(len(compression.Candidates), recallResponseMaxProofCandidates),
		"selected_count":         bounded(len(compression.Selected), recallResponseMaxProofRefs),
		"protected_obligations":  bounded(compression.ProtectedObligations, recallResponseMaxProofCandidates),
		"protected_witnesses":    bounded(compression.ProtectedWitnesses, recallResponseMaxProofCandidates),
		"optional_context_count": bounded(optionalContext, recallResponseMaxEvidence),
		"optional_module_count":  bounded(optionalModules, recallResponseMaxModules),
		"compact_bytes":          bounded(compactBytes, recallResponseMaxCompactBytes),
		"compact_tokens":         bounded(compactTokens, recallResponseMaxCompactTokens),
		"max_compact_bytes":      recallResponseMaxCompactBytes,
		"max_compact_tokens":     recallResponseMaxCompactTokens,
	}
	receipt["receipt_digest"] = "sha256:" + sha256Hex(recallResponseCanonicalJSON(receipt))
	return receipt
}

func recallResponseAttachFallbackStageReceipt(response, receipt map[string]any) {
	if response == nil || receipt == nil {
		return
	}
	response[recallResponseFallbackStageReceiptKey] = cloneJSONMap(receipt)
}
