package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

type agentBoundaryLimits struct {
	MaxTotalJSONBytes int
	MaxStringBytes    int
	MaxListItems      int
}

type agentBoundaryStats struct {
	StringsClipped                int
	ListsClipped                  int
	OptionalFieldsCompacted       int
	TotalPasses                   int
	JSONBytesBefore               int
	JSONBytesAfter                int
	MaxTotalJSONBytes             int
	MaxStringBytes                int
	MaxListItems                  int
	ObjectiveGraphNodeLinksBefore int
}

func agentBoundaryLimitsFromContract(contract map[string]any) agentBoundaryLimits {
	if contract == nil {
		return agentBoundaryLimits{}
	}
	return agentBoundaryLimits{
		MaxTotalJSONBytes: anyToInt(contract["max_total_json_bytes"], 0),
		MaxStringBytes:    anyToInt(contract["max_string_bytes"], 0),
		MaxListItems:      anyToInt(contract["max_list_items"], 0),
	}
}

func agentBoundaryLimitsForContract(contractID string) agentBoundaryLimits {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		return agentBoundaryLimits{}
	}
	return agentBoundaryLimitsFromContract(agentContract(registry, contractID))
}

func enforceAgentBoundaryContract(contractID string, payload map[string]any) agentBoundaryStats {
	limits := agentBoundaryLimitsForContract(contractID)
	stats := agentBoundaryStats{
		JSONBytesBefore:   jsonByteLen(payload),
		MaxTotalJSONBytes: limits.MaxTotalJSONBytes,
		MaxStringBytes:    limits.MaxStringBytes,
		MaxListItems:      limits.MaxListItems,
	}
	if contractID == objectiveGraphContractID {
		stats.ObjectiveGraphNodeLinksBefore = objectiveGraphNodeLinkCount(payload)
	}
	if limits.MaxTotalJSONBytes <= 0 && limits.MaxStringBytes <= 0 && limits.MaxListItems <= 0 {
		stats.JSONBytesAfter = stats.JSONBytesBefore
		return stats
	}
	sanitizeOverflow := contractID != GeneratedAgentContractContextOverflowRecoveryV1
	applyAgentBoundaryLimits(payload, limits.MaxStringBytes, positiveListLimit(limits.MaxListItems), sanitizeOverflow, &stats)
	reconcileAgentBoundaryPayload(contractID, payload, &stats)
	if limits.MaxTotalJSONBytes > 0 {
		stats.TotalPasses += shrinkAgentBoundaryPayload(contractID, payload, limits, sanitizeOverflow, &stats)
	}
	stats.JSONBytesAfter = jsonByteLen(payload)
	return stats
}

func mergeAgentBoundaryStats(left agentBoundaryStats, right agentBoundaryStats) agentBoundaryStats {
	if left.JSONBytesBefore == 0 {
		left.JSONBytesBefore = right.JSONBytesBefore
	}
	if right.JSONBytesAfter > 0 {
		left.JSONBytesAfter = right.JSONBytesAfter
	}
	if left.MaxTotalJSONBytes == 0 {
		left.MaxTotalJSONBytes = right.MaxTotalJSONBytes
	}
	if left.MaxStringBytes == 0 {
		left.MaxStringBytes = right.MaxStringBytes
	}
	if left.MaxListItems == 0 {
		left.MaxListItems = right.MaxListItems
	}
	if left.ObjectiveGraphNodeLinksBefore == 0 {
		left.ObjectiveGraphNodeLinksBefore = right.ObjectiveGraphNodeLinksBefore
	}
	left.StringsClipped += right.StringsClipped
	left.ListsClipped += right.ListsClipped
	left.OptionalFieldsCompacted += right.OptionalFieldsCompacted
	left.TotalPasses += right.TotalPasses
	return left
}

func agentBoundaryStatsTruncated(stats agentBoundaryStats) bool {
	return stats.StringsClipped > 0 ||
		stats.ListsClipped > 0 ||
		stats.OptionalFieldsCompacted > 0 ||
		stats.TotalPasses > 0 ||
		(stats.JSONBytesBefore > 0 && stats.JSONBytesAfter > 0 && stats.JSONBytesAfter < stats.JSONBytesBefore)
}

func agentBoundaryOmittedCounts(stats agentBoundaryStats) map[string]any {
	reduced := 0
	if stats.JSONBytesBefore > 0 && stats.JSONBytesAfter > 0 && stats.JSONBytesBefore > stats.JSONBytesAfter {
		reduced = stats.JSONBytesBefore - stats.JSONBytesAfter
	}
	return map[string]any{
		"strings_clipped":           stats.StringsClipped,
		"lists_clipped":             stats.ListsClipped,
		"optional_fields_compacted": stats.OptionalFieldsCompacted,
		"boundary_passes":           stats.TotalPasses,
		"json_bytes_reduced":        reduced,
	}
}

func agentBoundaryStatsFromMetadata(value any) agentBoundaryStats {
	metadata, ok := value.(map[string]any)
	if !ok {
		return agentBoundaryStats{}
	}
	counts := anyMap(metadata["omitted_counts"])
	return agentBoundaryStats{
		StringsClipped:          anyToInt(counts["strings_clipped"], 0),
		ListsClipped:            anyToInt(counts["lists_clipped"], 0),
		OptionalFieldsCompacted: anyToInt(counts["optional_fields_compacted"], 0),
		TotalPasses:             anyToInt(counts["boundary_passes"], 0),
		JSONBytesBefore:         anyToInt(metadata["json_bytes_before_boundary"], 0),
		JSONBytesAfter:          anyToInt(metadata["json_bytes_after_boundary"], 0),
		MaxTotalJSONBytes:       anyToInt(metadata["max_total_json_bytes"], 0),
		MaxStringBytes:          anyToInt(metadata["max_string_bytes"], 0),
		MaxListItems:            anyToInt(metadata["max_list_items"], 0),
	}
}

func positiveListLimit(value int) int {
	if value <= 0 {
		return -1
	}
	return value
}

func applyAgentBoundaryLimits(value any, maxStringBytes int, maxListItems int, sanitizeOverflow bool, stats *agentBoundaryStats) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = applyAgentBoundaryLimits(item, maxStringBytes, maxListItems, sanitizeOverflow, stats)
		}
		return typed
	case []any:
		items := typed
		if maxListItems >= 0 && len(items) > maxListItems {
			items = items[:maxListItems]
			if stats != nil {
				stats.ListsClipped++
			}
		}
		for idx, item := range items {
			items[idx] = applyAgentBoundaryLimits(item, maxStringBytes, maxListItems, sanitizeOverflow, stats)
		}
		return items
	case []map[string]any:
		items := typed
		if maxListItems >= 0 && len(items) > maxListItems {
			items = items[:maxListItems]
			if stats != nil {
				stats.ListsClipped++
			}
		}
		for idx, item := range items {
			if next, ok := applyAgentBoundaryLimits(item, maxStringBytes, maxListItems, sanitizeOverflow, stats).(map[string]any); ok {
				items[idx] = next
			}
		}
		return items
	case map[string]string:
		out := map[string]any{}
		for key, item := range typed {
			out[key] = applyAgentBoundaryLimits(item, maxStringBytes, maxListItems, sanitizeOverflow, stats)
		}
		return out
	case []string:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return applyAgentBoundaryLimits(items, maxStringBytes, maxListItems, sanitizeOverflow, stats)
	case string:
		text := typed
		if sanitizeOverflow {
			text = sanitizeProviderOverflowText(text)
		}
		if maxStringBytes > 0 && len([]byte(text)) > maxStringBytes {
			text = clipUTF8Bytes(text, maxStringBytes)
			if stats != nil {
				stats.StringsClipped++
			}
		}
		return text
	default:
		return value
	}
}

func sanitizeProviderOverflowText(value string) string {
	lower := strings.ToLower(value)
	for _, pattern := range []string{
		"array_above_max_length",
		"context length exceeded",
		"maximum context length",
		"max context length",
		"input array is too long",
		"oversized input",
	} {
		if strings.Contains(lower, pattern) {
			return "ContextLattice boundary reduced an oversized provider input before returning this payload."
		}
	}
	return value
}

func clipUTF8Bytes(value string, maxBytes int) string {
	if maxBytes <= 0 || len([]byte(value)) <= maxBytes {
		return value
	}
	const suffix = "... [truncated]"
	if maxBytes <= len(suffix) {
		raw := []byte(value)
		limit := maxBytes
		for limit > 0 && !utf8.Valid(raw[:limit]) {
			limit--
		}
		return string(raw[:limit])
	}
	limit := maxBytes - len(suffix)
	raw := []byte(value)
	if limit > len(raw) {
		limit = len(raw)
	}
	for limit > 0 && !utf8.Valid(raw[:limit]) {
		limit--
	}
	return string(raw[:limit]) + suffix
}

func jsonByteLen(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

func validateAgentBoundaryStringBytes(value any, maxBytes int, path string, contractID string) []map[string]any {
	if maxBytes <= 0 {
		return nil
	}
	findings := []map[string]any{}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			nextPath := key
			if path != "" {
				nextPath = path + "." + key
			}
			findings = append(findings, validateAgentBoundaryStringBytes(typed[key], maxBytes, nextPath, contractID)...)
		}
	case []any:
		for idx, item := range typed {
			if idx >= 512 {
				break
			}
			nextPath := path + "[]"
			findings = append(findings, validateAgentBoundaryStringBytes(item, maxBytes, nextPath, contractID)...)
		}
	case string:
		actual := len([]byte(typed))
		if actual > maxBytes {
			findings = append(findings, map[string]any{
				"reason":      "string_bytes_exceed_contract",
				"path":        path,
				"bytes":       actual,
				"max_bytes":   maxBytes,
				"contract_id": contractID,
			})
		}
	}
	return findings
}

func validateAgentBoundaryListItems(value any, maxItems int, path string, contractID string) []map[string]any {
	if maxItems <= 0 {
		return nil
	}
	findings := []map[string]any{}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			nextPath := key
			if path != "" {
				nextPath = path + "." + key
			}
			findings = append(findings, validateAgentBoundaryListItems(typed[key], maxItems, nextPath, contractID)...)
		}
	case []any:
		if len(typed) > maxItems {
			findings = append(findings, map[string]any{
				"reason":      "list_items_exceed_contract",
				"path":        path,
				"items":       len(typed),
				"max_items":   maxItems,
				"contract_id": contractID,
			})
		}
		for idx, item := range typed {
			if idx >= 512 {
				break
			}
			nextPath := path + "[]"
			findings = append(findings, validateAgentBoundaryListItems(item, maxItems, nextPath, contractID)...)
		}
	}
	return findings
}

func shrinkAgentBoundaryPayload(
	contractID string,
	payload map[string]any,
	limits agentBoundaryLimits,
	sanitizeOverflow bool,
	stats *agentBoundaryStats,
) int {
	targetBytes := agentBoundaryTargetJSONBytes(limits)
	if jsonByteLen(payload) <= targetBytes {
		return 0
	}
	passes := 0
	shrink := func(maxStringBytes int, maxListItems int) bool {
		passes++
		applyAgentBoundaryLimits(payload, maxStringBytes, maxListItems, sanitizeOverflow, stats)
		reconcileAgentBoundaryPayload(contractID, payload, stats)
		return jsonByteLen(payload) <= targetBytes
	}

	switch contractID {
	case contextPackResponseContractID:
		compactContextPackResponseBoundary(payload, 16, stats)
	case dreamModeResponseContractID:
		compactDreamModeResponseBoundary(payload, 16, stats)
	case reviewModeResponseContractID:
		compactReviewModeResponseBoundary(payload, 16, stats)
	case agentPreflightResponseContractID:
		compactPreflightResponseBoundary(payload, 16, stats)
	case policyContextPackageContractID:
		compactPolicyContextPackageBoundary(payload, 8, stats)
	}
	if shrink(minPositive(limits.MaxStringBytes, 2048), minPositive(limits.MaxListItems, 16)) {
		return passes
	}

	switch contractID {
	case contextPackResponseContractID:
		dropContextPackDebugBoundary(payload, stats)
		compactContextPackResponseBoundary(payload, 8, stats)
	case dreamModeResponseContractID:
		dropDreamModeOptionalBoundary(payload, stats)
		compactDreamModeResponseBoundary(payload, 8, stats)
	case reviewModeResponseContractID:
		dropReviewModeOptionalBoundary(payload, stats)
		compactReviewModeResponseBoundary(payload, 8, stats)
	case agentPreflightResponseContractID:
		compactPreflightResponseBoundary(payload, 8, stats)
		dropPreflightOptionalBoundary(payload, stats)
	case policyContextPackageContractID:
		compactPolicyContextPackageBoundary(payload, 6, stats)
	}
	if shrink(minPositive(limits.MaxStringBytes, 1024), minPositive(limits.MaxListItems, 8)) {
		return passes
	}

	switch contractID {
	case contextPackResponseContractID:
		compactContextPackResponseBoundary(payload, 3, stats)
	case dreamModeResponseContractID:
		compactDreamModeResponseBoundary(payload, 4, stats)
	case reviewModeResponseContractID:
		compactReviewModeResponseBoundary(payload, 4, stats)
	case agentPreflightResponseContractID:
		compactPreflightResponseBoundary(payload, 4, stats)
	case policyContextPackageContractID:
		compactPolicyContextPackageBoundary(payload, 5, stats)
	}
	if shrink(minPositive(limits.MaxStringBytes, 512), minPositive(limits.MaxListItems, 8)) {
		return passes
	}

	switch contractID {
	case contextPackResponseContractID:
		forceMinimalContextPackResponseBoundary(payload, stats)
	case dreamModeResponseContractID:
		forceMinimalDreamModeResponseBoundary(payload, stats)
	case reviewModeResponseContractID:
		forceMinimalReviewModeResponseBoundary(payload, stats)
	case agentPreflightResponseContractID:
		forceMinimalPreflightResponseBoundary(payload, stats)
	case policyContextPackageContractID:
		compactPolicyContextPackageBoundary(payload, 5, stats)
	}
	shrink(minPositive(limits.MaxStringBytes, 384), minPositive(limits.MaxListItems, 8))
	return passes
}

func agentBoundaryTargetJSONBytes(limits agentBoundaryLimits) int {
	if limits.MaxTotalJSONBytes <= 0 {
		return 0
	}
	if limits.MaxTotalJSONBytes > 8192 {
		return limits.MaxTotalJSONBytes - 2048
	}
	if limits.MaxTotalJSONBytes > 1024 {
		return limits.MaxTotalJSONBytes - 256
	}
	return limits.MaxTotalJSONBytes
}

func minPositive(value int, capValue int) int {
	if value <= 0 {
		return capValue
	}
	if capValue <= 0 {
		return value
	}
	return minInt(value, capValue)
}

func trimBoundaryList(value any, keep int, stats *agentBoundaryStats) any {
	items, ok := value.([]any)
	if !ok {
		return value
	}
	if keep < 0 {
		keep = 0
	}
	if len(items) <= keep {
		return items
	}
	if stats != nil {
		stats.ListsClipped++
	}
	return items[:keep]
}

func compactContextPackLists(pack map[string]any, keep int, stats *agentBoundaryStats) {
	if pack == nil {
		return
	}
	for _, key := range []string{
		"facts",
		"numericFacts",
		"numeric_facts",
		"citations",
		"results",
		"relevantDecisions",
		"relevant_decisions",
		"filesToRead",
		"files_to_read",
		"filesToAvoid",
		"files_to_avoid",
		"capabilitiesToUse",
		"capabilities_to_use",
		"runbooks",
		"knownFailureModes",
		"known_failure_modes",
		"commands",
		"acceptanceCriteria",
		"acceptance_criteria",
	} {
		if _, ok := pack[key]; ok {
			pack[key] = trimBoundaryList(pack[key], keep, stats)
		}
	}
	for _, key := range []string{"query", "retrievalMode", "retrieval_mode", "retrievalIntent", "retrieval_intent"} {
		if text := strings.TrimSpace(anyToString(pack[key])); text != "" {
			pack[key] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 1000)
		}
	}
	for _, key := range []string{"sourceCoverage", "source_coverage"} {
		if coverage, ok := pack[key].(map[string]any); ok {
			compactSourceCoverageBoundary(coverage, keep, stats)
		}
	}
	for _, key := range []string{"agentGuidance", "agent_guidance"} {
		if guidance, ok := pack[key].(map[string]any); ok {
			pack[key] = compactAgentEvidenceGuidanceBoundary(guidance, minInt(keep, 8), stats)
		}
	}
}

func compactSourceCoverageBoundary(coverage map[string]any, keep int, stats *agentBoundaryStats) {
	if coverage == nil {
		return
	}
	for _, key := range []string{
		"configured",
		"queried",
		"returned",
		"pending",
		"warming",
		"timed_out",
		"failed",
		"budget_exceeded",
		"skipped",
		"continuation_unavailable",
		"sync_fallback_slow_sources",
		"async_warm_slow_sources",
		"fail_open_continuation_sources",
	} {
		if _, ok := coverage[key]; ok {
			coverage[key] = trimBoundaryList(coverage[key], keep, stats)
		}
	}
}

func compactContextPackResponseBoundary(payload map[string]any, keep int, stats *agentBoundaryStats) {
	compactContextPackLists(anyMap(payload["context_pack"]), keep, stats)
	if guidance, ok := payload["agent_guidance"].(map[string]any); ok {
		payload["agent_guidance"] = compactAgentEvidenceGuidanceBoundary(guidance, minInt(keep, 8), stats)
	}
	compactSourceCoverageBoundary(anyMap(payload["source_coverage"]), keep, stats)
	if warnings, ok := payload["warnings"]; ok {
		payload["warnings"] = trimBoundaryList(warnings, minInt(keep, 8), stats)
	}
}

func compactAgentEvidenceGuidanceBoundary(guidance map[string]any, keep int, stats *agentBoundaryStats) map[string]any {
	if guidance == nil {
		return guidance
	}
	for _, key := range []string{"themes", "risk_markers", "candidate_links", "missing_evidence", "prompt_hints"} {
		if _, ok := guidance[key]; ok {
			guidance[key] = trimBoundaryList(guidance[key], keep, stats)
		}
	}
	if text := strings.TrimSpace(anyToString(guidance["intended_use"])); text != "" {
		guidance["intended_use"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 240)
	}
	return guidance
}

func dropContextPackDebugBoundary(payload map[string]any, stats *agentBoundaryStats) {
	if _, ok := payload["retrieval"]; ok {
		payload["retrieval"] = map[string]any{"omitted_by_boundary": true}
		if stats != nil {
			stats.OptionalFieldsCompacted++
		}
	}
}

func compactDreamModeResponseBoundary(payload map[string]any, keep int, stats *agentBoundaryStats) {
	if _, ok := payload["hypotheses"]; ok {
		payload["hypotheses"] = trimBoundaryList(payload["hypotheses"], keep, stats)
	}
	if _, ok := payload["experiments"]; ok {
		payload["experiments"] = trimBoundaryList(payload["experiments"], keep, stats)
	}
	if evidence, ok := payload["evidence"].(map[string]any); ok {
		for _, key := range []string{"facts", "results", "citations", "combined"} {
			if _, ok := evidence[key]; ok {
				evidence[key] = trimBoundaryList(evidence[key], keep, stats)
			}
		}
	}
	compactSourceCoverageBoundary(anyMap(payload["source_coverage"]), keep, stats)
	if warnings, ok := payload["warnings"]; ok {
		payload["warnings"] = trimBoundaryList(warnings, minInt(keep, 8), stats)
	}
	if llm, ok := payload["llm"].(map[string]any); ok {
		if text := strings.TrimSpace(anyToString(llm["synthesis_text"])); text != "" {
			llm["synthesis_text"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), minPositive(6000, 6000))
		}
		if parsed, ok := llm["parsed"].(map[string]any); ok {
			if _, exists := parsed["hypotheses"]; exists {
				parsed["hypotheses"] = trimBoundaryList(parsed["hypotheses"], keep, stats)
			}
			if _, exists := parsed["experiments"]; exists {
				parsed["experiments"] = trimBoundaryList(parsed["experiments"], keep, stats)
			}
		}
	}
}

func dropDreamModeOptionalBoundary(payload map[string]any, stats *agentBoundaryStats) {
	for _, key := range []string{"retrieval", "writeback"} {
		if _, ok := payload[key]; ok {
			payload[key] = map[string]any{"omitted_by_boundary": true}
			if stats != nil {
				stats.OptionalFieldsCompacted++
			}
		}
	}
	if llm, ok := payload["llm"].(map[string]any); ok {
		if _, exists := llm["parsed"]; exists {
			delete(llm, "parsed")
			if stats != nil {
				stats.OptionalFieldsCompacted++
			}
		}
	}
}

func compactReviewModeResponseBoundary(payload map[string]any, keep int, stats *agentBoundaryStats) {
	if _, ok := payload["patterns"]; ok {
		payload["patterns"] = trimBoundaryList(payload["patterns"], keep, stats)
	}
	if _, ok := payload["agent_guidance"]; ok {
		payload["agent_guidance"] = trimBoundaryList(payload["agent_guidance"], minInt(keep, 8), stats)
	}
	if guidance, ok := payload["evidence_analysis"].(map[string]any); ok {
		payload["evidence_analysis"] = compactAgentEvidenceGuidanceBoundary(guidance, minInt(keep, 8), stats)
	}
	if _, ok := payload["warnings"]; ok {
		payload["warnings"] = trimBoundaryList(payload["warnings"], minInt(keep, 8), stats)
	}
	if sourceCoverage, ok := payload["source_coverage"].(map[string]any); ok {
		compactSourceCoverageBoundary(sourceCoverage, keep, stats)
	}
	if patterns, ok := payload["patterns"].([]any); ok {
		for _, raw := range patterns {
			pattern, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			pattern["evidence"] = trimBoundaryList(pattern["evidence"], minInt(keep, 6), stats)
			pattern["mitigation"] = trimBoundaryList(pattern["mitigation"], minInt(keep, 6), stats)
			if text := strings.TrimSpace(anyToString(pattern["agent_guidance"])); text != "" {
				pattern["agent_guidance"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 900)
			}
		}
	}
}

func dropReviewModeOptionalBoundary(payload map[string]any, stats *agentBoundaryStats) {
	for _, key := range []string{"review_context", "agent_runtime"} {
		if _, ok := payload[key]; ok {
			payload[key] = map[string]any{"omitted_by_boundary": true}
			if stats != nil {
				stats.OptionalFieldsCompacted++
			}
		}
	}
}

func compactContextPackValueBoundary(value any, keep int, stats *agentBoundaryStats) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	compactContextPackResponseBoundary(object, keep, stats)
	if nested, ok := object["payload"].(map[string]any); ok {
		compactContextPackResponseBoundary(nested, keep, stats)
	}
	dropContextPackDebugBoundary(object, stats)
	return object
}

func compactSearchBoundary(value any, stats *agentBoundaryStats) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := map[string]any{
		"omitted_by_boundary": true,
		"result_count":        resultCount(object),
	}
	for _, key := range []string{"ok", "degraded", "result_state", "retrieval_mode", "retrieval_intent", "traffic_class", "status", "error"} {
		if value, ok := object[key]; ok {
			out[key] = value
		}
	}
	if stats != nil {
		stats.OptionalFieldsCompacted++
	}
	return out
}

func sanitizePreflightSearchBoundary(payload map[string]any) {
	if payload == nil {
		return
	}
	for _, key := range []string{"scoped_search", "broadened_search"} {
		if value, ok := payload[key]; ok {
			payload[key] = sanitizeProviderOverflowValue(value)
		}
	}
}

func sanitizeProviderOverflowValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = sanitizeProviderOverflowValue(item)
		}
		return typed
	case []any:
		for idx, item := range typed {
			typed[idx] = sanitizeProviderOverflowValue(item)
		}
		return typed
	case []map[string]any:
		for idx, item := range typed {
			if next, ok := sanitizeProviderOverflowValue(item).(map[string]any); ok {
				typed[idx] = next
			}
		}
		return typed
	case map[string]string:
		out := map[string]any{}
		for key, item := range typed {
			out[key] = sanitizeProviderOverflowText(item)
		}
		return out
	case []string:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, sanitizeProviderOverflowText(item))
		}
		return out
	case string:
		return sanitizeProviderOverflowText(typed)
	default:
		return value
	}
}

func compactPreflightResponseBoundary(payload map[string]any, keep int, stats *agentBoundaryStats) {
	for _, key := range []string{"context_pack", "mission_context_pack", "mission_pack"} {
		if value, ok := payload[key]; ok {
			payload[key] = compactContextPackValueBoundary(value, keep, stats)
		}
	}
	if policy, ok := payload["policy_context_package"].(map[string]any); ok {
		compactPolicyContextPackageBoundary(policy, maxInt(keep, 5), stats)
	}
	if objectiveRuntime, ok := payload["objective_runtime"].(map[string]any); ok {
		compactObjectiveRuntimeBoundary(objectiveRuntime, stats)
	}
	for _, key := range []string{"scoped_search", "broadened_search"} {
		if value, ok := payload[key]; ok {
			payload[key] = compactSearchBoundary(value, stats)
		}
	}
}

func dropPreflightOptionalBoundary(payload map[string]any, stats *agentBoundaryStats) {
	if status, ok := payload["status"]; ok {
		payload["status"] = compactPreflightStatusBoundary(status)
		if stats != nil {
			stats.OptionalFieldsCompacted++
		}
	}
	for _, key := range []string{"health", "agent_profile"} {
		if _, ok := payload[key]; ok {
			payload[key] = map[string]any{"omitted_by_boundary": true}
			if stats != nil {
				stats.OptionalFieldsCompacted++
			}
		}
	}
}

func compactPreflightStatusBoundary(value any) map[string]any {
	out := map[string]any{"omitted_by_boundary": true}
	frontier := anyMap(anyMap(value)["frontierT1Governance"])
	if len(frontier) == 0 {
		return out
	}
	capsule := map[string]any{}
	for _, key := range []string{"schema_id", "authorization_mode", "entitlement_mode", "release_gate"} {
		if item, ok := frontier[key].(string); ok {
			capsule[key] = clipUTF8Bytes(sanitizeProviderOverflowText(strings.TrimSpace(item)), 128)
		}
	}
	for _, key := range []string{
		"configured", "enabled", "force_disabled", "ready", "store_ready", "authorization_ready",
		"release_gate_satisfied",
	} {
		if item, ok := frontier[key].(bool); ok {
			capsule[key] = item
		}
	}
	for _, key := range []string{
		"required_route_count", "protected_route_count", "eligible_route_count", "event_count",
		"last_sequence", "bytes",
	} {
		if item, ok := nonnegativePreflightStatusInteger(frontier[key]); ok {
			capsule[key] = item
		}
	}
	out["frontierT1Governance"] = capsule
	return out
}

func nonnegativePreflightStatusInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), typed >= 0
	case int8:
		return int64(typed), typed >= 0
	case int16:
		return int64(typed), typed >= 0
	case int32:
		return int64(typed), typed >= 0
	case int64:
		return typed, typed >= 0
	case uint:
		if uint64(typed) <= uint64(^uint64(0)>>1) {
			return int64(typed), true
		}
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed <= uint64(^uint64(0)>>1) {
			return int64(typed), true
		}
	case float64:
		const maxExactJSONInteger = float64(1<<53 - 1)
		if !math.IsNaN(typed) && !math.IsInf(typed, 0) && typed >= 0 && typed <= maxExactJSONInteger && typed == math.Trunc(typed) {
			return int64(typed), true
		}
	}
	return 0, false
}

func compactPolicyContextPackageBoundary(policy map[string]any, keep int, stats *agentBoundaryStats) {
	if policy == nil {
		return
	}
	for _, key := range []string{"query", "mission", "objective", "goal"} {
		if text := strings.TrimSpace(anyToString(policy[key])); text != "" {
			policy[key] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 2000)
		}
	}
	if evidence, ok := policy["evidence"].(map[string]any); ok {
		for _, key := range []string{"primary_facts", "mission_facts"} {
			if _, ok := evidence[key]; ok {
				evidence[key] = trimBoundaryList(evidence[key], keep, stats)
			}
		}
		if text := strings.TrimSpace(anyToString(evidence["mission_pack_error"])); text != "" {
			evidence["mission_pack_error"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 1000)
		}
	}
	if handoff, ok := policy["handoff"].(map[string]any); ok {
		if text := strings.TrimSpace(anyToString(handoff["handoff_prompt"])); text != "" {
			handoff["handoff_prompt"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 4000)
		}
	}
	if objectiveRuntime, ok := policy["objective_runtime"].(map[string]any); ok {
		compactObjectiveRuntimeBoundary(objectiveRuntime, stats)
	}
}

func compactObjectiveRuntimeBoundary(runtime map[string]any, stats *agentBoundaryStats) {
	if runtime == nil {
		return
	}
	for _, key := range []string{"mission", "objective", "goal"} {
		if text := strings.TrimSpace(anyToString(runtime[key])); text != "" {
			runtime[key] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 1600)
		}
	}
	if text := strings.TrimSpace(anyToString(runtime["next_action"])); text != "" {
		runtime["next_action"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 1200)
	}
	if risk, ok := runtime["risk_or_blocker"].(map[string]any); ok {
		if text := strings.TrimSpace(anyToString(risk["fastest_recovery_path"])); text != "" {
			risk["fastest_recovery_path"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 1200)
		}
	}
	if evidence, ok := runtime["evidence"].(map[string]any); ok {
		evidence["current"] = trimBoundaryList(evidence["current"], 8, stats)
		evidence["required"] = trimBoundaryList(evidence["required"], 8, stats)
	}
}

func forceMinimalContextPackResponseBoundary(payload map[string]any, stats *agentBoundaryStats) {
	sourceCoverage := anyMap(payload["source_coverage"])
	contextPack := minimalContextPackBoundary(anyMap(payload["context_pack"]))
	payload["context_pack"] = contextPack
	payload["context_compiler"] = contextPack["context_compiler"]
	if _, ok := payload["contextCompiler"]; ok {
		payload["contextCompiler"] = contextPack["context_compiler"]
	}
	payload["reference_prompt"] = contextPackReferencePrompt(anyMap(contextPack["prompt_sections"]))
	if tokenBudget := anyMap(contextPack["token_budget"]); len(tokenBudget) > 0 {
		payload["token_budget"] = tokenBudget
		if _, ok := payload["tokenBudget"]; ok {
			payload["tokenBudget"] = tokenBudget
		}
	}
	payload["source_coverage"] = minimalSourceCoverageBoundary(sourceCoverage)
	payload["warnings"] = []any{"ContextLattice context pack was clipped to the output boundary budget."}
	delete(payload, "retrieval")
	if stats != nil {
		stats.OptionalFieldsCompacted++
	}
}

func reconcileAgentBoundaryPayload(contractID string, payload map[string]any, stats *agentBoundaryStats) {
	switch contractID {
	case contextPackResponseContractID:
		reconcileContextPackBoundaryMetadata(payload, true, stats)
	case synthesisPackContractID:
		reconcileContextPackBoundaryMetadata(payload, false, stats)
	case objectiveGraphContractID:
		reconcileObjectiveGraphBoundaryMetadata(payload, stats)
	case decisionChangeQueryContractID:
		reconcileDecisionChangeQueryBoundaryMetadata(payload, stats)
	}
}

func reconcileDecisionChangeQueryBoundaryMetadata(payload map[string]any, stats *agentBoundaryStats) {
	changes := contextPackAnyList(payload["changes"])
	previousCount := maxInt(anyToInt(payload["change_count"], len(changes)), len(changes))
	totalCount := maxInt(anyToInt(payload["total_change_count"], previousCount), previousCount)
	previousOmitted := maxInt(anyToInt(payload["omitted_count"], totalCount-previousCount), 0)
	changesClipped := previousCount > len(changes)
	boundaryCompacted := anyToBool(payload["boundary_compacted"]) || changesClipped
	if stats != nil && (stats.StringsClipped > 0 || stats.ListsClipped > 0 || stats.OptionalFieldsCompacted > 0) {
		boundaryCompacted = true
	}

	payload["change_count"] = len(changes)
	payload["omitted_count"] = maxInt(previousOmitted, totalCount-len(changes))
	if len(changes) > 0 {
		if cursor := strings.TrimSpace(anyToString(anyMap(changes[len(changes)-1])["page_cursor"])); cursor != "" && totalCount > len(changes) {
			payload["next_cursor"] = cursor
		}
	}
	if boundaryCompacted {
		wasCompacted := anyToBool(payload["boundary_compacted"])
		payload["boundary_compacted"] = true
		payload["query_truncated"] = true
		payload["complete"] = false
		if stats != nil && !wasCompacted {
			stats.OptionalFieldsCompacted++
		}
	}
}

func reconcileObjectiveGraphBoundaryMetadata(payload map[string]any, stats *agentBoundaryStats) {
	nodes := contextPackAnyList(payload["nodes"])
	edges := contextPackAnyList(payload["edges"])
	transitions := contextPackAnyList(payload["transitions"])
	previousNodeCount := anyToInt(payload["node_count"], len(nodes))
	previousEdgeCount := anyToInt(payload["edge_count"], len(edges))
	transitionCount := maxInt(anyToInt(payload["transition_count"], len(transitions)), len(transitions))
	previousTransitionOmitted := maxInt(anyToInt(payload["transition_omitted_count"], 0), 0)
	transitionsIncluded := anyToBool(payload["transitions_included"])

	nodesClipped := previousNodeCount > len(nodes)
	edgesClipped := previousEdgeCount > len(edges)
	transitionsClipped := transitionsIncluded && transitionCount-previousTransitionOmitted > len(transitions)
	boundaryCompacted := anyToBool(payload["boundary_compacted"]) || nodesClipped || edgesClipped || transitionsClipped
	if stats != nil && (stats.StringsClipped > 0 || stats.ListsClipped > 0 || stats.OptionalFieldsCompacted > 0) {
		boundaryCompacted = true
	}

	payload["node_count"] = len(nodes)
	payload["edge_count"] = len(edges)
	if transitionsIncluded {
		payload["transition_omitted_count"] = maxInt(previousTransitionOmitted, transitionCount-len(transitions))
	} else {
		payload["transition_omitted_count"] = transitionCount
	}
	if nodesClipped {
		payload["traversal_truncated"] = true
	}
	if edgesClipped {
		payload["edge_truncated"] = true
	}
	if transitionsClipped {
		payload["transition_truncated"] = true
	}
	if stats != nil && stats.ObjectiveGraphNodeLinksBefore > objectiveGraphNodeLinkCount(payload) {
		payload["node_link_truncated"] = true
	}
	if boundaryCompacted {
		wasCompacted := anyToBool(payload["boundary_compacted"])
		payload["boundary_compacted"] = true
		payload["graph_truncated"] = true
		payload["complete"] = false
		if stats != nil && !wasCompacted {
			stats.OptionalFieldsCompacted++
		}
	}
}

func objectiveGraphNodeLinkCount(payload map[string]any) int {
	total := 0
	for _, rawNode := range contextPackAnyList(payload["nodes"]) {
		node := anyMap(rawNode)
		for _, key := range []string{
			"task_identity_ids", "execution_lane_ids", "session_ids", "decision_change_ids", "outcome_ids", "checkpoint_ids",
		} {
			total += len(contextPackAnyList(node[key]))
		}
	}
	return total
}

func reconcileContextPackBoundaryMetadata(payload map[string]any, rebuildReferencePrompt bool, stats *agentBoundaryStats) {
	contextPack := anyMap(payload["context_pack"])
	if len(contextPack) == 0 {
		return
	}
	transportProjection := anyMap(contextPack["transport_projection"])
	canonicalTransportProjection := anyToString(transportProjection["schema_id"]) == "contextlattice_context_pack_transport_projection.v1"
	_, rankedAliasExposed := contextPack["rankedEvidence"]
	_, compilerAliasExposed := contextPack["contextCompiler"]
	_, tokenBudgetAliasExposed := contextPack["tokenBudget"]
	_, promptSectionsAliasExposed := contextPack["promptSections"]
	rankedEvidence := contextPackAnyList(firstPresentAny(contextPack["ranked_evidence"], contextPack["rankedEvidence"]))
	compiler := cloneJSONMap(anyMap(firstPresentAny(contextPack["context_compiler"], contextPack["contextCompiler"], payload["context_compiler"], payload["contextCompiler"])))
	if len(compiler) == 0 {
		return
	}
	previousCount := anyToInt(compiler["ranked_evidence_count"], len(rankedEvidence))
	preBoundaryCount := anyToInt(compiler["pre_boundary_ranked_evidence_count"], previousCount)
	compacted := previousCount != len(rankedEvidence)
	if compacted {
		compiler["pre_boundary_ranked_evidence_count"] = maxInt(preBoundaryCount, previousCount)
		compiler["ranked_evidence_count"] = len(rankedEvidence)
		compiler["complete"] = false
		compiler["boundary_compacted"] = true
		if stats != nil {
			stats.OptionalFieldsCompacted++
		}
	}
	tokenBudget := cloneJSONMap(anyMap(compiler["token_budget"]))
	if len(tokenBudget) > 0 {
		previousSelected := anyToInt(tokenBudget["selected_count"], previousCount)
		if previousSelected != len(rankedEvidence) || compacted {
			tokenBudget["pre_boundary_selected_count"] = maxInt(anyToInt(tokenBudget["pre_boundary_selected_count"], previousSelected), previousSelected)
			tokenBudget["selected_count"] = len(rankedEvidence)
			tokenBudget["used_tokens_estimate"] = boundaryEvidenceTokenEstimate(rankedEvidence)
			tokenBudget["boundary_compacted"] = true
			compiler["token_budget"] = tokenBudget
		}
	}
	contextPack["ranked_evidence"] = rankedEvidence
	if rankedAliasExposed {
		contextPack["rankedEvidence"] = rankedEvidence
	}
	contextPack["context_compiler"] = compiler
	if compilerAliasExposed {
		contextPack["contextCompiler"] = compiler
	}
	if len(tokenBudget) > 0 {
		contextPack["token_budget"] = tokenBudget
		if tokenBudgetAliasExposed {
			contextPack["tokenBudget"] = tokenBudget
		}
		payload["token_budget"] = tokenBudget
		if _, ok := payload["tokenBudget"]; ok {
			payload["tokenBudget"] = tokenBudget
		}
	}
	promptSections := anyMap(firstPresentAny(contextPack["prompt_sections"], contextPack["promptSections"]))
	if len(promptSections) > 0 {
		if canonicalTransportProjection {
			promptSections["evidence"] = contextPackTransportEvidencePointers(rankedEvidence, contextPackTransportLegacyEvidenceLimit)
			promptSections["ranked_evidence_count"] = len(rankedEvidence)
			promptSections["ranked_evidence_path"] = "$.context_pack.ranked_evidence"
			transportProjection["ranked_evidence_count"] = len(rankedEvidence)
			transportProjection["prompt_evidence_preview_count"] = minInt(len(rankedEvidence), contextPackTransportLegacyEvidenceLimit)
			contextPack["transport_projection"] = transportProjection
		} else {
			promptSections["evidence"] = rankedEvidence
		}
		if len(tokenBudget) > 0 {
			promptSections["token_budget"] = tokenBudget
		}
		contextPack["prompt_sections"] = promptSections
		if promptSectionsAliasExposed {
			contextPack["promptSections"] = promptSections
		}
		if rebuildReferencePrompt && compacted {
			payload["reference_prompt"] = contextPackReferencePrompt(promptSections)
		}
	}
	payload["context_pack"] = contextPack
	payload["context_compiler"] = compiler
	if _, ok := payload["contextCompiler"]; ok {
		payload["contextCompiler"] = compiler
	}
	if compacted {
		appendBoundaryWarning(payload, "ContextLattice preserved a compact evidence kernel at the output boundary.")
	}
}

func boundaryEvidenceTokenEstimate(items []any) int {
	total := 0
	for _, raw := range items {
		total += anyToInt(anyMap(raw)["estimated_tokens"], 0)
	}
	return total
}

func appendBoundaryWarning(payload map[string]any, warning string) {
	warnings := contextPackAnyList(payload["warnings"])
	for _, raw := range warnings {
		if strings.TrimSpace(anyToString(raw)) == warning {
			return
		}
	}
	payload["warnings"] = append(warnings, warning)
}

func forceMinimalDreamModeResponseBoundary(payload map[string]any, stats *agentBoundaryStats) {
	dropDreamModeOptionalBoundary(payload, stats)
	sourceCoverage := anyMap(payload["source_coverage"])
	payload["hypotheses"] = trimBoundaryList(payload["hypotheses"], 2, stats)
	payload["experiments"] = trimBoundaryList(payload["experiments"], 2, stats)
	if evidence, ok := payload["evidence"].(map[string]any); ok {
		evidence["facts"] = trimBoundaryList(evidence["facts"], 2, stats)
		evidence["results"] = trimBoundaryList(evidence["results"], 2, stats)
		evidence["citations"] = trimBoundaryList(evidence["citations"], 2, stats)
		evidence["combined"] = []any{}
	}
	payload["source_coverage"] = minimalSourceCoverageBoundary(sourceCoverage)
	payload["warnings"] = []any{"ContextLattice Dream Mode was clipped to the output boundary budget."}
	if llm, ok := payload["llm"].(map[string]any); ok {
		if text := strings.TrimSpace(anyToString(llm["synthesis_text"])); text != "" {
			llm["synthesis_text"] = clipUTF8Bytes(sanitizeProviderOverflowText(text), 1000)
		}
	}
	if stats != nil {
		stats.OptionalFieldsCompacted++
	}
}

func forceMinimalReviewModeResponseBoundary(payload map[string]any, stats *agentBoundaryStats) {
	dropReviewModeOptionalBoundary(payload, stats)
	sourceCoverage := anyMap(payload["source_coverage"])
	payload["patterns"] = trimBoundaryList(payload["patterns"], 2, stats)
	payload["agent_guidance"] = trimBoundaryList(payload["agent_guidance"], 2, stats)
	payload["evidence_analysis"] = compactAgentEvidenceGuidanceBoundary(anyMap(payload["evidence_analysis"]), 2, stats)
	payload["source_coverage"] = minimalSourceCoverageBoundary(sourceCoverage)
	payload["warnings"] = []any{"ContextLattice Review Mode was clipped to the output boundary budget."}
	if stats != nil {
		stats.OptionalFieldsCompacted++
	}
}

func minimalContextPackBoundary(existing map[string]any) map[string]any {
	query := strings.TrimSpace(anyToString(existing["query"]))
	rankedEvidence := minimalRankedEvidenceBoundary(firstPresentAny(existing["ranked_evidence"], existing["rankedEvidence"]), 4)
	omittedRefs := minimalRankedEvidenceBoundary(firstPresentAny(existing["omitted_high_value_refs"], existing["omittedHighValueRefs"]), 3)
	compiler := minimalContextCompilerBoundary(anyMap(firstPresentAny(existing["context_compiler"], existing["contextCompiler"])), rankedEvidence)
	promptSections := minimalPromptSectionsBoundary(
		anyMap(firstPresentAny(existing["prompt_sections"], existing["promptSections"])),
		query,
		rankedEvidence,
		omittedRefs,
		compiler,
	)
	tokenBudget := anyMap(compiler["token_budget"])
	return map[string]any{
		"query":                   clipUTF8Bytes(sanitizeProviderOverflowText(query), 1000),
		"facts":                   []any{},
		"numericFacts":            []any{},
		"numeric_facts":           []any{},
		"citations":               []any{},
		"results":                 []any{},
		"rankedEvidence":          rankedEvidence,
		"ranked_evidence":         rankedEvidence,
		"promptSections":          promptSections,
		"prompt_sections":         promptSections,
		"contextCompiler":         compiler,
		"context_compiler":        compiler,
		"tokenBudget":             tokenBudget,
		"token_budget":            tokenBudget,
		"omittedHighValueRefs":    omittedRefs,
		"omitted_high_value_refs": omittedRefs,
		"relevantDecisions":       []any{},
		"relevant_decisions":      []any{},
		"filesToRead":             []any{},
		"files_to_read":           []any{},
		"filesToAvoid":            []any{},
		"files_to_avoid":          []any{},
		"capabilitiesToUse":       []any{},
		"capabilities_to_use":     []any{},
		"runbooks":                []any{},
		"knownFailureModes":       []any{},
		"known_failure_modes":     []any{},
		"commands":                []any{},
		"acceptanceCriteria":      []any{},
		"acceptance_criteria":     []any{},
		"agentGuidance":           map[string]any{"schema_id": "contextlattice_agent_guidance.v1", "source": "deterministic_evidence_analysis", "authoritative": false, "themes": []any{}, "risk_markers": []any{}, "candidate_links": []any{}, "missing_evidence": []any{}, "prompt_hints": []any{}},
		"agent_guidance":          map[string]any{"schema_id": "contextlattice_agent_guidance.v1", "source": "deterministic_evidence_analysis", "authoritative": false, "themes": []any{}, "risk_markers": []any{}, "candidate_links": []any{}, "missing_evidence": []any{}, "prompt_hints": []any{}},
	}
}

func minimalRankedEvidenceBoundary(value any, keep int) []any {
	items := contextPackAnyList(value)
	out := make([]any, 0, minInt(keep, len(items)))
	for _, raw := range items {
		if len(out) >= keep {
			break
		}
		item := anyMap(raw)
		text := firstNonEmptyStrings(anyToString(item["text"]), anyToString(item["summary"]))
		text = clipUTF8Bytes(sanitizeProviderOverflowText(strings.TrimSpace(text)), 420)
		if text == "" {
			continue
		}
		compact := map[string]any{
			"rank": anyToInt(item["rank"], len(out)+1),
			"text": text,
		}
		for _, key := range []string{"kind", "project", "file", "source", "topic_path", "timestamp", "relation", "edge_direction"} {
			if value := strings.TrimSpace(anyToString(item[key])); value != "" {
				compact[key] = clipUTF8Bytes(sanitizeProviderOverflowText(value), 240)
			}
		}
		for _, key := range []string{"score", "confidence", "estimated_tokens", "value_density"} {
			if value, ok := item[key]; ok {
				compact[key] = value
			}
		}
		if reason := strings.TrimSpace(anyToString(item["reason"])); reason != "" {
			compact["reason"] = clipUTF8Bytes(sanitizeProviderOverflowText(reason), 240)
		}
		if signals := boundaryStringList(item["why_selected"], 4, 160); len(signals) > 0 {
			compact["why_selected"] = signals
		}
		out = append(out, compact)
	}
	return out
}

func minimalContextCompilerBoundary(existing map[string]any, rankedEvidence []any) map[string]any {
	compiler := cloneJSONMap(existing)
	if len(compiler) == 0 {
		compiler = map[string]any{
			"schema_id":           "contextlattice_context_compiler.v1",
			"version":             1,
			"strategy":            "boundary_minimal",
			"intended_use":        "preserve a contract-valid context package after boundary compaction",
			"recommended_surface": "cli_for_local_agents",
		}
	}
	currentCount := anyToInt(compiler["ranked_evidence_count"], len(rankedEvidence))
	preBoundaryCount := maxInt(anyToInt(compiler["pre_boundary_ranked_evidence_count"], currentCount), currentCount)
	compiler["pre_boundary_ranked_evidence_count"] = preBoundaryCount
	compiler["ranked_evidence_count"] = len(rankedEvidence)
	compiler["complete"] = false
	compiler["boundary_compacted"] = true
	tokenBudget := cloneJSONMap(anyMap(compiler["token_budget"]))
	if len(tokenBudget) > 0 {
		currentSelected := anyToInt(tokenBudget["selected_count"], currentCount)
		preBoundarySelected := maxInt(anyToInt(tokenBudget["pre_boundary_selected_count"], currentSelected), currentSelected)
		usedTokens := 0
		for _, raw := range rankedEvidence {
			usedTokens += anyToInt(anyMap(raw)["estimated_tokens"], 0)
		}
		tokenBudget["pre_boundary_selected_count"] = preBoundarySelected
		tokenBudget["selected_count"] = len(rankedEvidence)
		tokenBudget["used_tokens_estimate"] = usedTokens
		tokenBudget["boundary_compacted"] = true
		compiler["token_budget"] = tokenBudget
	}
	return compiler
}

func minimalPromptSectionsBoundary(
	existing map[string]any,
	query string,
	rankedEvidence []any,
	omittedRefs []any,
	compiler map[string]any,
) map[string]any {
	objective := firstNonEmptyStrings(anyToString(existing["objective"]), query)
	task := firstNonEmptyStrings(anyToString(existing["task"]), query)
	nextAction := firstNonEmptyStrings(anyToString(existing["next_action"]), "Inspect the preserved evidence kernel, then broaden retrieval if required.")
	out := map[string]any{
		"objective":               clipUTF8Bytes(sanitizeProviderOverflowText(objective), 1000),
		"task":                    clipUTF8Bytes(sanitizeProviderOverflowText(task), 1000),
		"next_action":             clipUTF8Bytes(sanitizeProviderOverflowText(nextAction), 1000),
		"evidence":                rankedEvidence,
		"omitted_high_value_refs": omittedRefs,
	}
	if tokenBudget := anyMap(compiler["token_budget"]); len(tokenBudget) > 0 {
		out["token_budget"] = tokenBudget
	}
	for _, key := range []string{"constraints", "checks", "risks", "files_to_inspect", "commands", "capabilities"} {
		if values := boundaryStringList(existing[key], 5, 240); len(values) > 0 {
			out[key] = values
		}
	}
	return out
}

func boundaryStringList(value any, keep int, maxBytes int) []any {
	items := contextPackAnyList(value)
	out := make([]any, 0, minInt(keep, len(items)))
	for _, item := range items {
		text := strings.TrimSpace(anyToString(item))
		if text == "" {
			continue
		}
		out = append(out, clipUTF8Bytes(sanitizeProviderOverflowText(text), maxBytes))
		if len(out) >= keep {
			break
		}
	}
	return out
}

func minimalSourceCoverageBoundary(existing map[string]any) map[string]any {
	return map[string]any{
		"configured": anyToStringList(existing["configured"], 8),
		"returned":   anyToStringList(existing["returned"], 8),
		"complete":   anyToBool(existing["complete"]),
	}
}

func forceMinimalPreflightResponseBoundary(payload map[string]any, stats *agentBoundaryStats) {
	dropPreflightOptionalBoundary(payload, stats)
	payload["scoped_search"] = compactSearchBoundary(payload["scoped_search"], stats)
	payload["broadened_search"] = compactSearchBoundary(payload["broadened_search"], stats)
	payload["context_pack"] = compactContextPackValueBoundary(payload["context_pack"], 1, stats)
	payload["mission_context_pack"] = map[string]any{"omitted_by_boundary": true}
	payload["mission_pack"] = map[string]any{"omitted_by_boundary": true}
	if policy, ok := payload["policy_context_package"].(map[string]any); ok {
		compactPolicyContextPackageBoundary(policy, 5, stats)
	}
	if objectiveRuntime, ok := payload["objective_runtime"].(map[string]any); ok {
		compactObjectiveRuntimeBoundary(objectiveRuntime, stats)
	}
}
