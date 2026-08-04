package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func savedRecallImpactTestCase() map[string]any {
	return map[string]any{
		"id":                    "impact-case",
		"query":                 "never persist this impact query",
		"project":               "private-impact-project",
		"task_class":            "coding",
		"expected_files":        []string{"private/expected.md"},
		"expected_numeric":      []string{"42"},
		"forbidden_files":       []string{"private/forbidden.md"},
		"decision_impact_grade": 3,
	}
}

func savedRecallImpactTestResults() []map[string]any {
	return []map[string]any{
		{"project": "private-impact-project", "file": "private/forbidden.md", "summary": "forbidden candidate count 7", "score": 10.0},
		{"project": "private-impact-project", "file": "private/other-1.md", "summary": "other candidate count 8", "score": 11.0},
		{"project": "private-impact-project", "file": "private/other-2.md", "summary": "other candidate count 9", "score": 12.0},
		{"project": "private-impact-project", "file": "private/other-3.md", "summary": "other candidate count 10", "score": 13.0},
		{"project": "private-impact-project", "file": "private/other-4.md", "summary": "other candidate count 11", "score": 14.0},
		{"project": "private-impact-project", "file": "private/expected.md", "summary": "expected candidate count 42", "score": 42.0},
	}
}

func savedRecallImpactTestFrontier(rows []map[string]any, order []int) map[string]any {
	candidates := make([]any, 0, len(order))
	for _, index := range order {
		identity := searchIntelligenceCandidateIdentity(rows[index])
		candidates = append(candidates, map[string]any{
			"refs": map[string]any{"candidate_ref": identity.CandidateRef},
		})
	}
	return map[string]any{
		"decision_frontier": map[string]any{
			"status":     "shadow_only",
			"candidates": candidates,
		},
	}
}

func TestSavedRecallImpactComparisonReordersOnlyMappedReturnedCandidates(t *testing.T) {
	rawCase := savedRecallImpactTestCase()
	cfg := recallEvalSavedConfig{Version: 1, Cases: []map[string]any{rawCase}}
	comparison := newSavedRecallImpactComparison(cfg)
	cohorts := comparison.sortedCohorts()
	if len(cohorts) != 1 || !cohorts[0].valid {
		t.Fatalf("comparison plan unexpectedly invalid: %#v", comparison)
	}
	results := savedRecallImpactTestResults()
	comparison.addCase(
		0,
		rawCase,
		results,
		savedRecallImpactTestFrontier(results, []int{5, 1, 2, 3, 4, 0}),
		normalizeExpectedNumeric(rawCase["expected_numeric"]),
		12.5,
	)
	fields := comparison.monitorFields(len(cfg.Cases))
	if !anyToBool(fields["comparison_valid"]) || anyToString(fields["comparison_reason"]) != "valid" {
		t.Fatalf("expected valid comparison receipt, got %#v", fields)
	}
	baseline := anyMap(fields["baseline"])
	shadow := anyMap(fields["shadow"])
	if anyToFloat64(baseline["decision_impact_recall_at_5"], -1) != 0 || anyToFloat64(shadow["decision_impact_recall_at_5"], -1) != 1 {
		t.Fatalf("expected shadow-only decision-impact recall gain, baseline=%#v shadow=%#v", baseline, shadow)
	}
	if anyToFloat64(baseline["decision_impact_ndcg_at_5"], -1) != 0 || anyToFloat64(shadow["decision_impact_ndcg_at_5"], -1) != 1 {
		t.Fatalf("expected shadow-only decision-impact nDCG gain, baseline=%#v shadow=%#v", baseline, shadow)
	}
	if anyToFloat64(baseline["numeric_exactness"], -1) != 0 || anyToFloat64(shadow["numeric_exactness"], -1) != 1 {
		t.Fatalf("numeric exactness did not follow mapped top-K candidates, baseline=%#v shadow=%#v", baseline, shadow)
	}
	if anyToInt(baseline["safety_failure_count"], -1) != 1 || anyToInt(shadow["safety_failure_count"], -1) != 0 {
		t.Fatalf("safety failures did not follow mapped top-K candidates, baseline=%#v shadow=%#v", baseline, shadow)
	}
	if anyToFloat64(baseline["p95_latency_ms"], -1) != anyToFloat64(shadow["p95_latency_ms"], -2) {
		t.Fatalf("shared synthetic replay latency diverged, baseline=%#v shadow=%#v", baseline, shadow)
	}
	if got := anyToString(fields["latency_basis"]); got != "shared_synthetic_retrieval_replay_ms" {
		t.Fatalf("latency basis = %q, want explicit shared synthetic basis", got)
	}
	caseSetRef := anyToString(fields["case_set_ref"])
	if want := savedRecallImpactCaseSetRef(cfg); caseSetRef != want || !isSearchIntelligenceFullSHA256Ref(caseSetRef) {
		t.Fatalf("emitted case set ref = %q, want deterministic full SHA-256 %q", caseSetRef, want)
	}
	if !searchImpactShadowEnvelopeValid(fields) {
		t.Fatalf("native emitted comparator artifact failed the strict envelope: %#v", fields)
	}
	malformed := cloneJSONMap(fields)
	malformed["case_set_ref"] = "sha256:deadbeef"
	if searchImpactShadowEnvelopeValid(malformed) {
		t.Fatalf("malformed case-set reference passed the strict envelope: %#v", malformed)
	}
	versionShifted := cfg
	versionShifted.Version = 2
	if savedRecallImpactCaseSetRef(versionShifted) == caseSetRef {
		t.Fatal("case set ref did not bind the saved-case version")
	}
	contentShifted := cfg
	contentShifted.Cases = []map[string]any{cloneJSONMap(rawCase)}
	contentShifted.Cases[0]["decision_impact_grade"] = 2
	if savedRecallImpactCaseSetRef(contentShifted) == caseSetRef {
		t.Fatal("case set ref did not bind saved-case content")
	}

	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal monitor fields: %v", err)
	}
	for _, forbidden := range []string{
		"never persist this impact query",
		"private-impact-project",
		"private/expected.md",
		"private/forbidden.md",
		"expected candidate",
		"forbidden candidate",
		searchIntelligenceCandidateIdentity(results[5]).CandidateRef,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("monitor artifact leaked %q: %s", forbidden, encoded)
		}
	}
	for _, ref := range append(anyToStringSlice(fields["project_scope_refs"]), anyToStringSlice(fields["task_class_scope_refs"])...) {
		if !isSearchIntelligenceFullSHA256Ref(ref) {
			t.Fatalf("scope ref is not opaque SHA-256: %q", ref)
		}
	}
}

func TestSavedRecallImpactComparisonFailsClosedForMissingPrerequisites(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"grade": func(rawCase map[string]any) {
			delete(rawCase, "decision_impact_grade")
		},
		"numeric": func(rawCase map[string]any) {
			delete(rawCase, "expected_numeric")
		},
		"safety": func(rawCase map[string]any) {
			delete(rawCase, "forbidden_files")
		},
	} {
		t.Run(name, func(t *testing.T) {
			rawCase := savedRecallImpactTestCase()
			mutate(rawCase)
			comparison := newSavedRecallImpactComparison(recallEvalSavedConfig{Cases: []map[string]any{rawCase}})
			fields := comparison.monitorFields(1)
			if anyToBool(fields["comparison_valid"]) || anyToString(fields["comparison_reason"]) == "valid" {
				t.Fatalf("%s prerequisite produced valid artifact: %#v", name, fields)
			}
		})
	}
}

func TestSavedRecallImpactComparisonNumericExactnessRequiresCandidateEvidence(t *testing.T) {
	for name, testCase := range map[string]struct {
		rows        []map[string]any
		wantMatched []string
		wantMapped  bool
	}{
		"score_coincidence_rejected": {
			rows:        []map[string]any{{"summary": "verified count 41", "score": 42.0}},
			wantMatched: []string{},
			wantMapped:  true,
		},
		"grounded_value_distinct_from_score": {
			rows:        []map[string]any{{"summary": "verified count 42", "score": 0.73}},
			wantMatched: []string{"42"},
			wantMapped:  true,
		},
		"strict_token_boundary": {
			rows:        []map[string]any{{"summary": "verified count 142", "score": 42.0}},
			wantMatched: []string{},
			wantMapped:  true,
		},
		"no_candidate_mapping_fails_closed": {
			rows:       []map[string]any{{"score": 42.0}},
			wantMapped: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidates := []savedRecallImpactCandidate{{rows: testCase.rows}}
			matched, mapped := savedRecallImpactMatchedNumericFacts(candidates, []string{"42"})
			if mapped != testCase.wantMapped || strings.Join(matched, ",") != strings.Join(testCase.wantMatched, ",") {
				t.Fatalf("numeric matching = matched=%#v mapped=%t, want matched=%#v mapped=%t", matched, mapped, testCase.wantMatched, testCase.wantMapped)
			}
		})
	}
	rawCase := savedRecallImpactTestCase()
	results := savedRecallImpactTestResults()
	for _, row := range results {
		delete(row, "summary")
	}
	comparison := newSavedRecallImpactComparison(recallEvalSavedConfig{Cases: []map[string]any{rawCase}})
	comparison.addCase(0, rawCase, results, savedRecallImpactTestFrontier(results, []int{5, 1, 2, 3, 4, 0}), normalizeExpectedNumeric(rawCase["expected_numeric"]), 3)
	fields := comparison.monitorFields(1)
	if anyToBool(fields["comparison_valid"]) || anyToString(fields["comparison_reason"]) != "candidate_bound_numeric_evidence_missing" {
		t.Fatalf("score-only numeric evidence did not invalidate comparator: %#v", fields)
	}
}

func TestSavedRecallImpactComparisonPartitionsMixedCohorts(t *testing.T) {
	firstCase := savedRecallImpactTestCase()
	secondCase := savedRecallImpactTestCase()
	secondCase["project"] = "private-impact-project-two"
	secondCase["task_class"] = "research"
	firstResults := savedRecallImpactTestResults()
	secondResults := savedRecallImpactTestResults()
	for _, row := range secondResults {
		row["project"] = secondCase["project"]
	}
	comparison := newSavedRecallImpactComparison(recallEvalSavedConfig{Cases: []map[string]any{firstCase, secondCase}})
	comparison.addCase(0, firstCase, firstResults, savedRecallImpactTestFrontier(firstResults, []int{5, 1, 2, 3, 4, 0}), normalizeExpectedNumeric(firstCase["expected_numeric"]), 4)
	comparison.addCase(1, secondCase, secondResults, savedRecallImpactTestFrontier(secondResults, []int{5, 1, 2, 3, 4, 0}), normalizeExpectedNumeric(secondCase["expected_numeric"]), 5)
	fields := comparison.monitorFields(2)
	if anyToBool(fields["comparison_valid"]) || anyToString(fields["comparison_reason"]) != "mixed_scope_requires_exact_cohort" {
		t.Fatalf("mixed top-level artifact was not fail-closed aggregate metadata: %#v", fields)
	}
	artifacts := contextPackAnyList(fields["search_impact_shadow_evaluations"])
	if len(artifacts) != 2 {
		t.Fatalf("exact cohort artifacts = %d, want 2: %#v", len(artifacts), fields)
	}
	wantScopes := map[string]struct{}{
		savedRecallImpactOpaqueScopeRef("project", "private-impact-project") + ":" + savedRecallImpactOpaqueScopeRef("task_class", "coding"):       {},
		savedRecallImpactOpaqueScopeRef("project", "private-impact-project-two") + ":" + savedRecallImpactOpaqueScopeRef("task_class", "research"): {},
	}
	for _, rawArtifact := range artifacts {
		artifact := anyMap(rawArtifact)
		projectRefs := anyToStringSlice(artifact["project_scope_refs"])
		taskClassRefs := anyToStringSlice(artifact["task_class_scope_refs"])
		if !anyToBool(artifact["comparison_valid"]) || len(projectRefs) != 1 || len(taskClassRefs) != 1 {
			t.Fatalf("cohort artifact is not independently valid and exact-scope: %#v", artifact)
		}
		scope := projectRefs[0] + ":" + taskClassRefs[0]
		if _, exists := wantScopes[scope]; !exists {
			t.Fatalf("unexpected cohort scope: %#v", artifact)
		}
		delete(wantScopes, scope)
	}
	if len(wantScopes) != 0 {
		t.Fatalf("missing exact cohort artifacts: %#v", wantScopes)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal mixed artifacts: %v", err)
	}
	for _, forbidden := range []string{"private-impact-project", "private-impact-project-two", "private/expected.md", "expected candidate count 42"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("mixed monitor artifact leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestSavedRecallImpactComparisonCohortPrerequisitesDoNotBorrow(t *testing.T) {
	validCase := savedRecallImpactTestCase()
	invalidCase := savedRecallImpactTestCase()
	invalidCase["project"] = "private-impact-project-two"
	invalidCase["task_class"] = "research"
	delete(invalidCase, "expected_numeric")
	comparison := newSavedRecallImpactComparison(recallEvalSavedConfig{Cases: []map[string]any{validCase, invalidCase}})
	fields := comparison.monitorFields(2)
	artifacts := contextPackAnyList(fields["search_impact_shadow_evaluations"])
	if len(artifacts) != 2 {
		t.Fatalf("cohort artifacts = %d, want 2: %#v", len(artifacts), fields)
	}
	validScope := savedRecallImpactOpaqueScopeRef("project", "private-impact-project")
	invalidScope := savedRecallImpactOpaqueScopeRef("project", "private-impact-project-two")
	for _, rawArtifact := range artifacts {
		artifact := anyMap(rawArtifact)
		projectRefs := anyToStringSlice(artifact["project_scope_refs"])
		if len(projectRefs) != 1 {
			t.Fatalf("missing exact project scope: %#v", artifact)
		}
		switch projectRefs[0] {
		case validScope:
			if anyToString(artifact["comparison_reason"]) == "numeric_expectations_missing" {
				t.Fatalf("valid cohort borrowed missing numeric prerequisite: %#v", artifact)
			}
		case invalidScope:
			if anyToBool(artifact["comparison_valid"]) || anyToString(artifact["comparison_reason"]) != "numeric_expectations_missing" {
				t.Fatalf("invalid cohort borrowed numeric prerequisite: %#v", artifact)
			}
		default:
			t.Fatalf("unexpected project scope: %#v", artifact)
		}
	}
}

func TestRecallComparatorPersistenceFailureIsFailClosed(t *testing.T) {
	s := &server{}
	if s.searchImpactComparatorPersistenceUnavailable() {
		t.Fatal("zero-value server unexpectedly reports comparator persistence unavailable")
	}
	notDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(notDirectory, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write append-failure fixture: %v", err)
	}
	t.Setenv("RECALL_MONITOR_PATH", filepath.Join(notDirectory, "recall-monitor.ndjson"))
	if err := s.appendRecallMonitorSample(map[string]any{"source": "saved_recall_eval"}); err == nil {
		t.Fatal("append unexpectedly succeeded for missing parent")
	}
	if !s.searchImpactComparatorPersistenceUnavailable() {
		t.Fatal("failed comparator persistence did not set fail-closed state")
	}
	recorder := httptest.NewRecorder()
	s.writeRecallEvalCaseSetInvalid(recorder, recallEvalSavedConfig{Cases: []map[string]any{savedRecallImpactTestCase()}}, map[string]any{"status": "invalid"})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-durable saved eval status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode non-durable response: %v", err)
	}
	if anyToBool(payload["durable"]) || anyToString(payload["code"]) != "recall_monitor_persistence_unavailable" {
		t.Fatalf("non-durable response missing explicit safe state: %#v", payload)
	}
	t.Setenv("RECALL_MONITOR_PATH", filepath.Join(t.TempDir(), "recall-monitor.ndjson"))
	if err := s.appendRecallMonitorSample(map[string]any{"source": "saved_recall_eval"}); err != nil {
		t.Fatalf("successful append: %v", err)
	}
	if s.searchImpactComparatorPersistenceUnavailable() {
		t.Fatal("successful comparator persistence did not clear fail-closed state")
	}
}

func TestRecallComparatorSyncFailureKeepsStalePassInactive(t *testing.T) {
	monitorPath := filepath.Join(t.TempDir(), "recall-monitor.ndjson")
	t.Setenv("RECALL_MONITOR_PATH", monitorPath)
	valid := searchImpactValidComparativeShadow()
	raw, err := json.Marshal(map[string]any{"search_impact_shadow_evaluations": []any{valid}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(monitorPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &server{}
	restore := setRecallMonitorPersistenceHooksForTest(s, recallMonitorPersistenceHooks{
		syncFile: func(*os.File) error { return errors.New("injected recall monitor fsync failure") },
	})
	t.Cleanup(restore)
	if err := s.appendRecallMonitorSample(map[string]any{
		"search_impact_shadow_evaluation": map[string]any{"comparison_valid": false, "comparison_reason": "case_set_invalid"},
	}); err == nil {
		t.Fatal("append unexpectedly succeeded after injected fsync failure")
	}
	if !s.searchImpactComparatorPersistenceUnavailable() {
		t.Fatal("fsync failure cleared comparator fail-closed state")
	}
	selected := s.latestSearchImpactShadowEvaluation("contextlattice", "coding")
	if anyToBool(selected["comparison_valid"]) || anyToString(selected["comparison_reason"]) != "comparator_persistence_unavailable" {
		t.Fatalf("stale persisted pass remained eligible after fsync failure: %#v", selected)
	}
	if anyToBool(searchImpactShadowEvaluation(selected)["pass"]) {
		t.Fatalf("stale persisted pass activated after fsync failure: %#v", selected)
	}
}

func TestSavedRecallImpactComparisonFailsClosedForIncompleteReturnedPoolAndInvalidNewestArtifact(t *testing.T) {
	rawCase := savedRecallImpactTestCase()
	cfg := recallEvalSavedConfig{Cases: []map[string]any{rawCase}}
	comparison := newSavedRecallImpactComparison(cfg)
	results := savedRecallImpactTestResults()
	frontier := savedRecallImpactTestFrontier(results, []int{5, 1, 2, 3, 4})
	frontierCandidates := contextPackAnyList(anyMap(frontier["decision_frontier"])["candidates"])
	anyMap(anyMap(frontierCandidates[0])["refs"])["candidate_ref"] = "sha256:" + strings.Repeat("0", 64)
	comparison.addCase(0, rawCase, results, frontier, normalizeExpectedNumeric(rawCase["expected_numeric"]), 4)
	fields := comparison.monitorFields(len(cfg.Cases))
	if anyToBool(fields["comparison_valid"]) || anyToString(fields["comparison_reason"]) != "shadow_returned_pool_incomplete" {
		t.Fatalf("incomplete returned-pool shadow mapping did not fail closed: %#v", fields)
	}

	newestInvalid := newSavedRecallImpactComparison(cfg)
	newestInvalid.invalidate("case_set_invalid")
	invalidFields := newestInvalid.monitorFields(len(cfg.Cases))
	if anyToString(invalidFields["schema_id"]) != savedRecallImpactShadowEvalSchemaID || anyToBool(invalidFields["comparison_valid"]) || anyToString(invalidFields["comparison_reason"]) != "case_set_invalid" {
		t.Fatalf("invalid newest evaluation did not generate an invalid artifact: %#v", invalidFields)
	}
}

func TestSavedRecallImpactComparisonPermitsSparseComparablePoolsAtFixedCeiling(t *testing.T) {
	rawCase := savedRecallImpactTestCase()
	cfg := recallEvalSavedConfig{Cases: []map[string]any{rawCase}}
	results := savedRecallImpactTestResults()[0:3]
	comparison := newSavedRecallImpactComparison(cfg)
	comparison.addCase(
		0,
		rawCase,
		results,
		savedRecallImpactTestFrontier(results, []int{2, 1, 0}),
		normalizeExpectedNumeric(rawCase["expected_numeric"]),
		3.25,
	)
	fields := comparison.monitorFields(len(cfg.Cases))
	if !anyToBool(fields["comparison_valid"]) {
		t.Fatalf("sparse but exactly mappable pool must remain comparable: %#v", fields)
	}
	for _, metrics := range []map[string]any{anyMap(fields["baseline"]), anyMap(fields["shadow"])} {
		if anyToInt(metrics["effective_k_min"], 0) != 3 || anyToInt(metrics["effective_k_max"], 0) != 3 || anyToInt(metrics["sparse_candidate_case_count"], 0) != 1 {
			t.Fatalf("sparse effective-K metadata missing: %#v", metrics)
		}
	}

	unexecuted := newSavedRecallImpactComparison(cfg).monitorFields(len(cfg.Cases))
	if anyToBool(unexecuted["comparison_valid"]) || anyToString(unexecuted["comparison_reason"]) != "comparison_metrics_unavailable" {
		t.Fatalf("unexecuted comparison must not remain valid: %#v", unexecuted)
	}
}
