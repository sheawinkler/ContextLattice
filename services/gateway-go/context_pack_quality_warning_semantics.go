package main

import (
	"regexp"
	"sort"
	"strings"
)

// Warning text remains part of the agent-facing response. These categories are
// an audit-only projection used by context-pack quality telemetry; they must
// never be used to remove, rewrite, or reorder response warnings.
const (
	contextPackQualityWarningCategoryAmbiguous                    = "ambiguous"
	contextPackQualityWarningCategoryTimeout                      = "timeout"
	contextPackQualityWarningCategoryBudgetExceeded               = "budget_exceeded"
	contextPackQualityWarningCategoryContinuationUnavailable      = "continuation_unavailable"
	contextPackQualityWarningCategoryOutputClipped                = "output_clipped"
	contextPackQualityWarningCategoryCoverageLoss                 = "coverage_loss"
	contextPackQualityWarningCategoryError                        = "error"
	contextPackQualityWarningCategoryDisabledSourceExcluded       = "disabled_source_excluded_effective_coverage"
	contextPackQualityWarningCategoryAuthoritativeStaleSuppressed = "authoritative_stale_fallback_suppressed"
	contextPackQualityWarningCategoryStagedOptionalDeferral       = "staged_optional_slow_source_deferral"
	contextPackQualityWarningCategorySourcesReturned              = "sources_returned_now"
	contextPackQualityWarningCategorySafetyFilteredEvidence       = "safety_filtered_evidence"
	contextPackQualityWarningCategoryRuntimeLaneNotice            = "runtime_lane_notice"
	contextPackQualityWarningCategoryTopicPrefilterNotice         = "topic_prefilter_notice"
	contextPackQualityWarningCategoryCoverageRescueSuccess        = "coverage_rescue_success"
	contextPackQualityWarningCategoryAuthoritativeCurrentState    = "authoritative_current_state_fast_path"
)

const contextPackQualityWarningCategoryLimit = 64

type contextPackQualityWarningAssessment struct {
	TotalCount       int
	ImpactingCount   int
	NoticeCount      int
	ImpactCategories map[string]int
	NoticeCategories map[string]int
}

var contextPackQualityAuthoritativeStaleSuppressionPattern = regexp.MustCompile(
	`^([a-z0-9][a-z0-9_.-]*: )?[a-z0-9][a-z0-9_.-]* authoritative memory state suppressed ([0-9]+) fallback result\(s\) \(stale_event=([0-9]+) hash_mismatch=([0-9]+) missing_authority=([0-9]+) lifecycle_hidden=([0-9]+) duplicate_path=([0-9]+)\); accepted current_event=([0-9]+) current_hash=([0-9]+) legacy_path_only=([0-9]+)$`,
)

var contextPackQualitySafetyFilteredEvidencePattern = regexp.MustCompile(
	`^[a-z0-9][a-z0-9_.-]*: suppressed ([1-9][0-9]*) (exact-state rows from semantic retrieval; use direct memory-file read for current state|ephemeral/test memory rows; set include_ephemeral=true to include)$`,
)

func contextPackQualityWarningAssessmentFor(warnings []string) contextPackQualityWarningAssessment {
	assessment := contextPackQualityWarningAssessment{
		ImpactCategories: make(map[string]int),
		NoticeCategories: make(map[string]int),
	}
	for _, warning := range warnings {
		assessment.TotalCount++
		category, notice := classifyContextPackQualityWarning(warning)
		if notice {
			assessment.NoticeCount++
			assessment.NoticeCategories[category]++
			continue
		}
		assessment.ImpactingCount++
		assessment.ImpactCategories[category]++
	}
	return assessment
}

func classifyContextPackQualityWarning(warning string) (category string, notice bool) {
	normalized := normalizeContextPackQualityWarning(warning)
	if contextPackQualityWarningIsDisabledSourceNotice(normalized) {
		return contextPackQualityWarningCategoryDisabledSourceExcluded, true
	}
	if contextPackQualityWarningIsAuthoritativeStaleSuppression(normalized) {
		return contextPackQualityWarningCategoryAuthoritativeStaleSuppressed, true
	}
	if contextPackQualityWarningIsStagedOptionalDeferral(normalized) {
		return contextPackQualityWarningCategoryStagedOptionalDeferral, true
	}
	if contextPackQualityWarningIsSourcesReturnedNotice(normalized) {
		return contextPackQualityWarningCategorySourcesReturned, true
	}
	if contextPackQualitySafetyFilteredEvidencePattern.MatchString(normalized) {
		return contextPackQualityWarningCategorySafetyFilteredEvidence, true
	}
	if normalized == "rust strict backend lane was promoted to qdrant_remote/non-strict for non-benchmark traffic to preserve recall quality and reduce empty-result risk." {
		return contextPackQualityWarningCategoryRuntimeLaneNotice, true
	}
	if contextPackQualityWarningHasSafeTopicPrefilterShape(normalized) {
		return contextPackQualityWarningCategoryTopicPrefilterNotice, true
	}
	if contextPackQualityWarningHasCoverageRescueSuccessShape(normalized) {
		return contextPackQualityWarningCategoryCoverageRescueSuccess, true
	}
	if normalized == "authoritative current-state fast path satisfied scoped retrieval; redundant sources were not queried." {
		return contextPackQualityWarningCategoryAuthoritativeCurrentState, true
	}

	// Impact classifications are deliberately checked before the ambiguous
	// fallback. A durable continuation does not erase the fact that a timeout
	// or error occurred, and an unavailable continuation is a direct recall
	// gap. Any warning not covered by this closed list fails closed below.
	switch {
	case contextPackQualityWarningContainsAny(normalized, "continuation unavailable", "continuation was unavailable"):
		return contextPackQualityWarningCategoryContinuationUnavailable, false
	case contextPackQualityWarningContainsAny(normalized, "budget exceeded", "budget_exceeded", "budget-exceeded"):
		return contextPackQualityWarningCategoryBudgetExceeded, false
	case contextPackQualityWarningContainsAny(normalized, "timed out", "timed-out", "timeout", "deadline exceeded"):
		return contextPackQualityWarningCategoryTimeout, false
	case contextPackQualityWarningContainsAny(normalized, "clipped", "output boundary"):
		return contextPackQualityWarningCategoryOutputClipped, false
	case contextPackQualityWarningContainsAny(normalized, "coverage loss", "coverage lost", "coverage incomplete", "incomplete coverage", "did not return additional results"):
		return contextPackQualityWarningCategoryCoverageLoss, false
	case contextPackQualityWarningContainsAny(normalized, " retrieval failed", " error", "failed:", "failure", "unavailable", "unable to", "could not"):
		return contextPackQualityWarningCategoryError, false
	default:
		return contextPackQualityWarningCategoryAmbiguous, false
	}
}

func contextPackQualityWarningIsAuthoritativeStaleSuppression(warning string) bool {
	matches := contextPackQualityAuthoritativeStaleSuppressionPattern.FindStringSubmatch(warning)
	if len(matches) != 11 {
		return false
	}
	// The producer emits this warning only when at least one fallback row was
	// suppressed or was legacy-path-only. Requiring a nonzero suppression
	// reason keeps an all-zero near-match from becoming a free notice.
	for _, raw := range []string{matches[2], matches[3], matches[4], matches[5], matches[6], matches[7], matches[10]} {
		if strings.Trim(raw, "0") != "" {
			return true
		}
	}
	return false
}

func normalizeContextPackQualityWarning(warning string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(warning)), " "))
}

func contextPackQualityWarningContainsAny(warning string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(warning, marker) {
			return true
		}
	}
	return false
}

func contextPackQualityWarningIsDisabledSourceNotice(warning string) bool {
	const prefix = "configured retrieval sources disabled by runtime policy and excluded from effective coverage:"
	return contextPackQualityWarningSourceListNotice(warning, prefix, "")
}

func contextPackQualityWarningIsSourcesReturnedNotice(warning string) bool {
	return contextPackQualityWarningSourceListNotice(warning, "sources returned now:", "")
}

func contextPackQualityWarningIsStagedOptionalDeferral(warning string) bool {
	if contextPackQualityWarningSourceListNotice(warning, "staged fetch deferred slow sources:", "") {
		return true
	}
	if contextPackQualityWarningSourceListNotice(warning, "additional context may be available later from:", ". re-run after cache warm or use deep mode / longer timeout budgets for blocking retrieval.") {
		return true
	}
	for _, exact := range []string{
		"explicit sources requested in staged fail-open mode; slow sources deferred asynchronously. set blocking=true (or sync_slow_sources=true) to wait for blocking completion.",
		"lexical backend policy deferred sync slow-source fallback; continuing asynchronously for cache warm.",
		"rust-first quality fallback satisfied minimum recall coverage; slow-source sync fallback skipped.",
	} {
		if warning == exact {
			return true
		}
	}
	return false
}

func contextPackQualityWarningHasSafeTopicPrefilterShape(warning string) bool {
	const prefix = "applied topic prefilter hint from query for deep retrieval: "
	if !strings.HasPrefix(warning, prefix) || !strings.HasSuffix(warning, ".") {
		return false
	}
	hint := strings.TrimSuffix(strings.TrimPrefix(warning, prefix), ".")
	if hint == "" {
		return false
	}
	for _, r := range hint {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '/' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func contextPackQualityWarningHasCoverageRescueSuccessShape(warning string) bool {
	const prefix = "coverage rescue query variant returned results from fast sources. variant="
	if !strings.HasPrefix(warning, prefix) {
		return false
	}
	variant := strings.TrimSpace(strings.TrimPrefix(warning, prefix))
	return variant != "" && !contextPackQualityWarningContainsAny(variant, "timed out", "timeout", "error", "failed", "unavailable")
}

// contextPackQualityWarningSourceListNotice accepts only the exact producer
// shape used by retrieval warnings. Strict source-token validation makes a
// near-match (for example, one that appends an error or timeout) ambiguous and
// therefore impacting.
func contextPackQualityWarningSourceListNotice(warning, prefix, suffix string) bool {
	if !strings.HasPrefix(warning, prefix) || !strings.HasSuffix(warning, suffix) {
		return false
	}
	value := strings.TrimPrefix(warning, prefix)
	if suffix != "" {
		value = strings.TrimSuffix(value, suffix)
	} else {
		if !strings.HasSuffix(value, ".") {
			return false
		}
		value = strings.TrimSuffix(value, ".")
	}
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, rawSource := range strings.Split(value, ",") {
		source := strings.TrimSpace(rawSource)
		if source == "" {
			return false
		}
		for _, r := range source {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
				continue
			}
			return false
		}
	}
	return true
}

func contextPackQualityWarningCategoryCounts(value any) map[string]any {
	counts := map[string]any{}
	raw := anyMap(value)
	known := []string{
		contextPackQualityWarningCategoryAmbiguous,
		contextPackQualityWarningCategoryTimeout,
		contextPackQualityWarningCategoryBudgetExceeded,
		contextPackQualityWarningCategoryContinuationUnavailable,
		contextPackQualityWarningCategoryOutputClipped,
		contextPackQualityWarningCategoryCoverageLoss,
		contextPackQualityWarningCategoryError,
		contextPackQualityWarningCategoryDisabledSourceExcluded,
		contextPackQualityWarningCategoryAuthoritativeStaleSuppressed,
		contextPackQualityWarningCategoryStagedOptionalDeferral,
		contextPackQualityWarningCategorySourcesReturned,
		contextPackQualityWarningCategorySafetyFilteredEvidence,
		contextPackQualityWarningCategoryRuntimeLaneNotice,
		contextPackQualityWarningCategoryTopicPrefilterNotice,
		contextPackQualityWarningCategoryCoverageRescueSuccess,
		contextPackQualityWarningCategoryAuthoritativeCurrentState,
	}
	for _, category := range known {
		count := clampInt(anyToInt(raw[category], 0), 0, contextPackQualityWarningCategoryLimit)
		if count > 0 {
			counts[category] = count
		}
	}
	return counts
}

func contextPackQualityWarningCategoryCountsSorted(counts map[string]int) map[string]any {
	if len(counts) == 0 {
		return map[string]any{}
	}
	keys := make([]string, 0, len(counts))
	for category := range counts {
		keys = append(keys, category)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(keys))
	for _, category := range keys {
		out[category] = clampInt(counts[category], 0, contextPackQualityWarningCategoryLimit)
	}
	return out
}
