package main

import (
	"reflect"
	"testing"
)

func TestContextPackQualityWarningClassificationClosedSet(t *testing.T) {
	tests := []struct {
		name     string
		warning  string
		category string
		notice   bool
	}{
		{
			name:     "disabled source excluded from effective coverage",
			warning:  "Configured retrieval sources disabled by runtime policy and excluded from effective coverage: mindsdb.",
			category: contextPackQualityWarningCategoryDisabledSourceExcluded,
			notice:   true,
		},
		{
			name:     "authoritative stale fallback suppression",
			warning:  "qdrant authoritative memory state suppressed 2 fallback result(s) (stale_event=1 hash_mismatch=1 missing_authority=0 lifecycle_hidden=0 duplicate_path=0); accepted current_event=3 current_hash=1 legacy_path_only=0",
			category: contextPackQualityWarningCategoryAuthoritativeStaleSuppressed,
			notice:   true,
		},
		{
			name:     "authoritative stale fallback suppression with retrieval source prefix",
			warning:  "qdrant: qdrant authoritative memory state suppressed 2 fallback result(s) (stale_event=1 hash_mismatch=1 missing_authority=0 lifecycle_hidden=0 duplicate_path=0); accepted current_event=3 current_hash=1 legacy_path_only=0",
			category: contextPackQualityWarningCategoryAuthoritativeStaleSuppressed,
			notice:   true,
		},
		{
			name:     "authoritative legacy path suppression",
			warning:  "qdrant authoritative memory state suppressed 0 fallback result(s) (stale_event=0 hash_mismatch=0 missing_authority=0 lifecycle_hidden=0 duplicate_path=0); accepted current_event=3 current_hash=1 legacy_path_only=1",
			category: contextPackQualityWarningCategoryAuthoritativeStaleSuppressed,
			notice:   true,
		},
		{
			name:     "staged slow source deferral",
			warning:  "Staged fetch deferred slow sources: mindsdb.",
			category: contextPackQualityWarningCategoryStagedOptionalDeferral,
			notice:   true,
		},
		{
			name:     "durable continuation follow-up notice",
			warning:  "Additional context may be available later from: mindsdb. Re-run after cache warm or use deep mode / longer timeout budgets for blocking retrieval.",
			category: contextPackQualityWarningCategoryStagedOptionalDeferral,
			notice:   true,
		},
		{
			name:     "sources returned notice",
			warning:  "Sources returned now: topic_rollups.",
			category: contextPackQualityWarningCategorySourcesReturned,
			notice:   true,
		},
		{
			name:     "safety filtered exact state evidence",
			warning:  "qdrant: suppressed 1 exact-state rows from semantic retrieval; use direct memory-file read for current state",
			category: contextPackQualityWarningCategorySafetyFilteredEvidence,
			notice:   true,
		},
		{
			name:     "safety filtered ephemeral evidence",
			warning:  "topic_rollups: suppressed 2 ephemeral/test memory rows; set include_ephemeral=true to include",
			category: contextPackQualityWarningCategorySafetyFilteredEvidence,
			notice:   true,
		},
		{
			name:     "positive rust lane notice",
			warning:  "Rust strict backend lane was promoted to qdrant_remote/non-strict for non-benchmark traffic to preserve recall quality and reduce empty-result risk.",
			category: contextPackQualityWarningCategoryRuntimeLaneNotice,
			notice:   true,
		},
		{
			name:     "positive coverage rescue notice",
			warning:  "Coverage rescue query variant returned results from fast sources. variant=release gate",
			category: contextPackQualityWarningCategoryCoverageRescueSuccess,
			notice:   true,
		},
		{
			name:     "authoritative current-state fast path notice",
			warning:  "Authoritative current-state fast path satisfied scoped retrieval; redundant sources were not queried.",
			category: contextPackQualityWarningCategoryAuthoritativeCurrentState,
			notice:   true,
		},
		{
			name:     "timeout remains impacting with durable continuation",
			warning:  "mindsdb timed out; async continuation deferred durably and will retry automatically.",
			category: contextPackQualityWarningCategoryTimeout,
		},
		{
			name:     "budget exceeded",
			warning:  "mindsdb retrieval sync budget exceeded after 0.2s",
			category: contextPackQualityWarningCategoryBudgetExceeded,
		},
		{
			name:     "continuation unavailable",
			warning:  "Async continuation was unavailable for: mindsdb. Re-run shortly to pick up warmed sources once queue pressure clears.",
			category: contextPackQualityWarningCategoryContinuationUnavailable,
		},
		{
			name:     "output clipping",
			warning:  "ContextLattice context pack was clipped to the output boundary budget.",
			category: contextPackQualityWarningCategoryOutputClipped,
		},
		{
			name:     "coverage rescue loss",
			warning:  "Coverage rescue query variant did not return additional results. variant=release gate",
			category: contextPackQualityWarningCategoryCoverageLoss,
		},
		{
			name:     "retrieval error",
			warning:  "mindsdb retrieval failed: connection refused",
			category: contextPackQualityWarningCategoryError,
		},
		{
			name:     "unknown warning fails closed",
			warning:  "Optional source policy changed for this request.",
			category: contextPackQualityWarningCategoryAmbiguous,
		},
		{
			name:     "disabled notice near match with error",
			warning:  "Configured retrieval sources disabled by runtime policy and excluded from effective coverage: mindsdb; retrieval failed.",
			category: contextPackQualityWarningCategoryError,
		},
		{
			name:     "stale suppression near match with malformed counter",
			warning:  "qdrant authoritative memory state suppressed 2 fallback result(s) (stale_event=1 hash_mismatch=1 missing_authority=none lifecycle_hidden=0 duplicate_path=0); accepted current_event=3 current_hash=1 legacy_path_only=0",
			category: contextPackQualityWarningCategoryAmbiguous,
		},
		{
			name:     "stale suppression near match with no suppressed reason",
			warning:  "qdrant authoritative memory state suppressed 0 fallback result(s) (stale_event=0 hash_mismatch=0 missing_authority=0 lifecycle_hidden=0 duplicate_path=0); accepted current_event=3 current_hash=1 legacy_path_only=0",
			category: contextPackQualityWarningCategoryAmbiguous,
		},
		{
			name:     "returned notice near match with timeout",
			warning:  "Sources returned now: topic_rollups; qdrant timed out.",
			category: contextPackQualityWarningCategoryTimeout,
		},
		{
			name:     "staged notice near match with unavailable continuation",
			warning:  "Staged fetch deferred slow sources: mindsdb; async continuation unavailable.",
			category: contextPackQualityWarningCategoryContinuationUnavailable,
		},
		{
			name:     "additional context near match with unavailable continuation",
			warning:  "Additional context may be available later from: mindsdb. Re-run after cache warm or use deep mode / longer timeout budgets for blocking retrieval; continuation unavailable.",
			category: contextPackQualityWarningCategoryContinuationUnavailable,
		},
		{
			name:     "safety filter near match with zero count",
			warning:  "qdrant: suppressed 0 exact-state rows from semantic retrieval; use direct memory-file read for current state",
			category: contextPackQualityWarningCategoryAmbiguous,
		},
		{
			name:     "positive rescue near match with error variant",
			warning:  "Coverage rescue query variant returned results from fast sources. variant=error handling",
			category: contextPackQualityWarningCategoryAmbiguous,
		},
		{
			name:     "authoritative current-state near match fails closed",
			warning:  "Authoritative current-state fast path satisfied scoped retrieval; one redundant source timed out.",
			category: contextPackQualityWarningCategoryTimeout,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			category, notice := classifyContextPackQualityWarning(test.warning)
			if category != test.category || notice != test.notice {
				t.Fatalf("classify warning=%q: category=%q notice=%t, want category=%q notice=%t", test.warning, category, notice, test.category, test.notice)
			}
		})
	}
}

func TestContextPackQualityWarningAssessmentSeparatesNoticesFromImpact(t *testing.T) {
	warnings := []string{
		"Configured retrieval sources disabled by runtime policy and excluded from effective coverage: mindsdb.",
		"qdrant authoritative memory state suppressed 1 fallback result(s) (stale_event=1 hash_mismatch=0 missing_authority=0 lifecycle_hidden=0 duplicate_path=0); accepted current_event=2 current_hash=1 legacy_path_only=0",
		"Staged fetch deferred slow sources: mindsdb.",
		"Sources returned now: topic_rollups.",
		"mindsdb timed out; async continuation deferred durably and will retry automatically.",
		"Optional source policy changed for this request.",
	}
	assessment := contextPackQualityWarningAssessmentFor(warnings)
	if assessment.TotalCount != 6 || assessment.ImpactingCount != 2 || assessment.NoticeCount != 4 {
		t.Fatalf("warning assessment counts=%+v, want total=6 impacting=2 notices=4", assessment)
	}
	if assessment.ImpactCategories[contextPackQualityWarningCategoryTimeout] != 1 || assessment.ImpactCategories[contextPackQualityWarningCategoryAmbiguous] != 1 {
		t.Fatalf("impact categories=%v", assessment.ImpactCategories)
	}
	if assessment.NoticeCategories[contextPackQualityWarningCategoryDisabledSourceExcluded] != 1 ||
		assessment.NoticeCategories[contextPackQualityWarningCategoryAuthoritativeStaleSuppressed] != 1 ||
		assessment.NoticeCategories[contextPackQualityWarningCategoryStagedOptionalDeferral] != 1 ||
		assessment.NoticeCategories[contextPackQualityWarningCategorySourcesReturned] != 1 {
		t.Fatalf("notice categories=%v", assessment.NoticeCategories)
	}
}

func TestContextPackQualitySampleUsesImpactWarningsWithoutDroppingResponseWarnings(t *testing.T) {
	warnings := []string{
		"Configured retrieval sources disabled by runtime policy and excluded from effective coverage: mindsdb.",
		"Staged fetch deferred slow sources: mindsdb.",
		"Sources returned now: topic_rollups.",
		"mindsdb retrieval timed out after 0.2s",
	}
	originalWarnings := append([]string(nil), warnings...)
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query:          "warning semantics",
		Project:        "contextlattice",
		TokenImpact:    map[string]any{"saved_tokens_estimate": 1},
		SourceCoverage: map[string]any{},
		GraphQuality:   map[string]any{},
		Warnings:       warnings,
	})
	if got := anyToInt(sample["warning_count"], -1); got != 1 {
		t.Fatalf("warning_count=%d, want one impacting timeout", got)
	}
	if got := anyToInt(sample["warning_total_count"], -1); got != 4 {
		t.Fatalf("warning_total_count=%d, want all four warnings", got)
	}
	if got := anyToInt(sample["warning_notice_count"], -1); got != 3 {
		t.Fatalf("warning_notice_count=%d, want three notices", got)
	}
	if got := anyToInt(sample["quality_score"], -1); got != 41 {
		t.Fatalf("quality_score=%d, want 41 (45 base with one four-point warning penalty)", got)
	}
	if !reflect.DeepEqual(warnings, originalWarnings) {
		t.Fatalf("quality scoring mutated response warnings: got=%v want=%v", warnings, originalWarnings)
	}
	if got := anyToInt(anyMap(sample["warning_impact_categories"])[contextPackQualityWarningCategoryTimeout], -1); got != 1 {
		t.Fatalf("impact categories=%v", sample["warning_impact_categories"])
	}
	if got := anyToInt(anyMap(sample["warning_notice_categories"])[contextPackQualityWarningCategorySourcesReturned], -1); got != 1 {
		t.Fatalf("notice categories=%v", sample["warning_notice_categories"])
	}
}

func TestContextPackQualityEntryCarriesWarningAuditProjection(t *testing.T) {
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "warning audit", Project: "contextlattice",
		Warnings: []string{
			"Sources returned now: topic_rollups.",
			"mindsdb retrieval failed: connection refused",
		},
	})
	entry := contextPackQualityEntryFromSample(sample)
	if anyToInt(entry["warning_count"], -1) != 1 || anyToInt(entry["warning_total_count"], -1) != 2 || anyToInt(entry["warning_notice_count"], -1) != 1 {
		t.Fatalf("entry warning counts=%#v", entry)
	}
	if got := anyToInt(anyMap(entry["warning_impact_categories"])[contextPackQualityWarningCategoryError], -1); got != 1 {
		t.Fatalf("entry impact categories=%#v", entry["warning_impact_categories"])
	}
	if got := anyToInt(anyMap(entry["warning_notice_categories"])[contextPackQualityWarningCategorySourcesReturned], -1); got != 1 {
		t.Fatalf("entry notice categories=%#v", entry["warning_notice_categories"])
	}

	legacy := contextPackQualityEntryFromSample(map[string]any{
		"schema_id": contextPackQualitySchemaID, "version": 1, "sample_id": "legacy-warning-sample",
		"project": "contextlattice", "quality_score": 50, "warning_count": 2,
	})
	if anyToInt(legacy["warning_count"], -1) != 2 || anyToInt(legacy["warning_total_count"], -1) != 2 || anyToInt(legacy["warning_notice_count"], -1) != 0 {
		t.Fatalf("legacy warning compatibility=%#v", legacy)
	}
}
