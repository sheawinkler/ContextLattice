package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func recallEvalV3TestCandidates(count int) []recallEvalSourceCandidate {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candidates := make([]recallEvalSourceCandidate, 0, count)
	for idx := 0; idx < count; idx++ {
		project := fmt.Sprintf("project-%02d", idx%5)
		topic := fmt.Sprintf("topic/%02d/subtopic/%02d", idx%7, idx%3)
		fileName := fmt.Sprintf("notes/%s-%04d.md", project, idx)
		updated := base.Add(time.Duration(idx) * 24 * time.Hour)
		candidates = append(candidates, recallEvalSourceCandidate{
			doc: memoryStoreDoc{
				Project: project, FileName: fileName, TopicPath: topic,
				Summary:   fmt.Sprintf("retrieval evidence sequence %d for %s topic %s", idx, fileName, topic),
				UpdatedAt: updated,
			},
			agentID:   fmt.Sprintf("agent-%02d", idx%4),
			sessionID: fmt.Sprintf("session-%02d", idx%6),
			createdAt: updated,
			stableKey: recallEvalCandidateStableKey(project, fileName, topic),
		})
	}
	return candidates
}

func TestRecallEvalV3SelectionIsDeterministicStratifiedAndFilenameRedacted(t *testing.T) {
	candidates := recallEvalV3TestCandidates(120)
	eligible := recallEvalEligibleCandidates(candidates, 1, "", "")
	first, firstTemporal := recallEvalSelectCandidates(eligible, 60)
	second, secondTemporal := recallEvalSelectCandidates(eligible, 60)
	firstCases := recallEvalCasesFromCandidates(first, "")
	secondCases := recallEvalCasesFromCandidates(second, "")
	firstRaw, _ := json.Marshal(firstCases)
	secondRaw, _ := json.Marshal(secondCases)
	if string(firstRaw) != string(secondRaw) {
		t.Fatalf("v3 case generation is not deterministic:\nfirst=%s\nsecond=%s", firstRaw, secondRaw)
	}
	firstTemporalRaw, _ := json.Marshal(firstTemporal)
	secondTemporalRaw, _ := json.Marshal(secondTemporal)
	if string(firstTemporalRaw) != string(secondTemporalRaw) {
		t.Fatalf("temporal metadata is not deterministic: first=%s second=%s", firstTemporalRaw, secondTemporalRaw)
	}
	if len(firstCases) != 60 {
		t.Fatalf("expected bounded 60-case selection, got %d", len(firstCases))
	}
	projects := map[string]struct{}{}
	topics := map[string]struct{}{}
	splits := map[string]int{}
	for _, rawCase := range firstCases {
		projects[anyToString(rawCase["project"])] = struct{}{}
		topics[anyToString(rawCase["topic_path"])] = struct{}{}
		splits[anyToString(rawCase["split"])]++
		fileName := anyToStringSlice(rawCase["expected_files"])[0]
		if recallEvalQueryContainsExpectedFile(anyToString(rawCase["query"]), fileName) {
			t.Fatalf("query leaked expected filename: %#v", rawCase)
		}
		if anyToString(rawCase["query_derivation"]) != "topic_plus_summary_filename_redacted" {
			t.Fatalf("missing honest query derivation metadata: %#v", rawCase)
		}
	}
	if len(projects) < 4 || len(topics) < 5 {
		t.Fatalf("selection did not stratify workspace dimensions: projects=%d topics=%d", len(projects), len(topics))
	}
	if splits["train"] == 0 || splits["holdout"] == 0 {
		t.Fatalf("expected train and temporal holdout cases, got %#v", splits)
	}
	if digest := recallEvalCaseSetDigest(firstCases); digest != recallEvalCaseSetDigest(secondCases) || digest == "" {
		t.Fatalf("case set digest is not stable: %q", digest)
	}
}

func TestRecallEvalV3SelectionIsBoundedForTwentyThousandUnrelatedDocs(t *testing.T) {
	candidates := recallEvalV3TestCandidates(20000)
	eligible := recallEvalEligibleCandidates(candidates, 1, "", "")
	selected, _ := recallEvalSelectCandidates(eligible, savedRecallEvalV3MaxCases)
	if len(selected) != savedRecallEvalV3MaxCases {
		t.Fatalf("expected hard cap of %d cases, got %d", savedRecallEvalV3MaxCases, len(selected))
	}
	for _, candidate := range selected {
		if strings.TrimSpace(candidate.doc.FileName) == "" || strings.TrimSpace(candidate.doc.Project) == "" {
			t.Fatalf("bounded selection emitted incomplete source candidate: %#v", candidate)
		}
	}
}

func TestRecallEvalV3EligibilityRejectsRootTopicsAndDuplicateDerivedQueries(t *testing.T) {
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	candidates := []recallEvalSourceCandidate{
		{
			doc:       memoryStoreDoc{Project: "alpha", FileName: "notes/root.md", TopicPath: "root", Summary: "shared benchmark summary", UpdatedAt: base},
			createdAt: base,
		},
		{
			doc:       memoryStoreDoc{Project: "alpha", FileName: "notes/first.md", TopicPath: "runbooks/quality", Summary: "shared benchmark summary", UpdatedAt: base.Add(time.Hour)},
			createdAt: base.Add(time.Hour),
		},
		{
			doc:       memoryStoreDoc{Project: "alpha", FileName: "notes/second.md", TopicPath: "runbooks/quality", Summary: "shared benchmark summary", UpdatedAt: base.Add(2 * time.Hour)},
			createdAt: base.Add(2 * time.Hour),
		},
		{
			doc:       memoryStoreDoc{Project: "beta", FileName: "notes/third.md", TopicPath: "runbooks/quality", Summary: "shared benchmark summary", UpdatedAt: base.Add(3 * time.Hour)},
			createdAt: base.Add(3 * time.Hour),
		},
	}
	eligible := recallEvalEligibleCandidates(candidates, 1, "", "")
	if len(eligible) != 2 {
		t.Fatalf("expected one unique query per project and no root topic, got %d: %#v", len(eligible), eligible)
	}
	for _, candidate := range eligible {
		if topic := normalizeTopicPathLoose(candidate.doc.TopicPath); topic == "root" || topic == "." {
			t.Fatalf("root topic survived eligibility: %#v", candidate)
		}
	}
}

func TestRecallEvalIndexedCandidatesFiltersRootDominanceBeforeBottomK(t *testing.T) {
	store := &memoryStore{
		policy:          memoryStorePolicy{enabled: true, maxSummaryChars: 400, maxRecent: 2048},
		currentState:    map[string]memoryCurrentState{},
		latestTopic:     map[string]string{},
		exactStatePaths: map[string]struct{}{},
	}
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for idx := 0; idx < 1000; idx++ {
		entry := memoryStoreEntry{
			EventID: fmt.Sprintf("root-%04d", idx), Project: "bulk", FileName: fmt.Sprintf("root-%04d.md", idx),
			TopicPath: "root", Summary: "bulk root summary", Lifecycle: "durable", CreatedAt: base.Add(time.Duration(idx) * time.Minute).Format(time.RFC3339Nano),
		}
		store.currentState[memoryStoreKey(entry.Project, entry.FileName)] = memoryCurrentStateFromEntry(entry)
	}
	for idx := 0; idx < 20; idx++ {
		entry := memoryStoreEntry{
			EventID: fmt.Sprintf("concrete-%02d", idx), Project: "quality", FileName: fmt.Sprintf("notes/concrete-%02d.md", idx),
			TopicPath: fmt.Sprintf("runbooks/quality/%02d", idx), Summary: fmt.Sprintf("concrete benchmark summary %02d", idx),
			Lifecycle: "durable", CreatedAt: base.Add(time.Duration(2000+idx) * time.Minute).Format(time.RFC3339Nano),
		}
		store.currentState[memoryStoreKey(entry.Project, entry.FileName)] = memoryCurrentStateFromEntry(entry)
	}
	store.ready.Store(true)
	candidates, source, stats := (&server{memoryStore: store}).recallEvalIndexedCandidates(context.Background(), "", "", 10)
	if source != "current_state_bottom_k" || len(candidates) != 10 {
		t.Fatalf("pre-sampling eligibility did not preserve the concrete corpus: source=%q count=%d stats=%#v", source, len(candidates), stats)
	}
	if anyToInt(stats["population_count"], 0) != 1020 || anyToInt(stats["scanned_count"], 0) != 1020 {
		t.Fatalf("source custody stopped reporting the full indexed denominator: %#v", stats)
	}
	for _, candidate := range candidates {
		if normalizeTopicPathLoose(candidate.doc.TopicPath) == "root" {
			t.Fatalf("root-dominant row consumed the bounded sample: %#v", candidate)
		}
	}
}

func recallEvalV3IndexedTestStore(total int) *memoryStore {
	store := &memoryStore{
		policy:       memoryStorePolicy{enabled: true, maxSummaryChars: 400, maxRecent: total + 32},
		currentState: map[string]memoryCurrentState{},
		currentKeysByProject: map[string]map[string]struct{}{
			"target": {},
		},
		currentKeyCountsByProject: map[string]int{"target": 0},
		exactStatePaths:           map[string]struct{}{},
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for idx := 0; idx < total; idx++ {
		project := fmt.Sprintf("unrelated-%04d", idx%37)
		fileName := fmt.Sprintf("notes/doc-%06d.md", idx)
		entry := memoryStoreEntry{
			EventID:   fmt.Sprintf("event-%06d", idx),
			Project:   project,
			FileName:  fileName,
			TopicPath: "recall/quality",
			Summary:   fmt.Sprintf("bounded source summary %d", idx),
			Lifecycle: "durable",
			CreatedAt: base.Add(time.Duration(idx) * time.Minute).Format(time.RFC3339Nano),
		}
		key := memoryStoreKey(project, fileName)
		store.currentState[key] = memoryCurrentStateFromEntry(entry)
	}
	for idx := 0; idx < 10; idx++ {
		fileName := fmt.Sprintf("notes/target-%02d.md", idx)
		entry := memoryStoreEntry{
			EventID:   fmt.Sprintf("target-event-%02d", idx),
			Project:   "target",
			FileName:  fileName,
			TopicPath: "recall/quality",
			Summary:   fmt.Sprintf("target source summary %d", idx),
			Lifecycle: "durable",
			CreatedAt: base.Add(time.Duration(total+idx) * time.Minute).Format(time.RFC3339Nano),
		}
		key := memoryStoreKey(entry.Project, entry.FileName)
		store.currentState[key] = memoryCurrentStateFromEntry(entry)
		store.currentKeysByProject["target"][key] = struct{}{}
		store.currentKeyCountsByProject["target"]++
	}
	store.ready.Store(true)
	return store
}

func TestRecallEvalIndexedCandidatesUsesBoundedBottomKAndScopedIndex(t *testing.T) {
	store := recallEvalV3IndexedTestStore(20000)
	s := &server{memoryStore: store}
	candidates, source, stats := s.recallEvalIndexedCandidates(context.Background(), "", "", 17)
	if source != "current_state_bottom_k" {
		t.Fatalf("unscoped refresh used unexpected source: %q", source)
	}
	if len(candidates) != 17 {
		t.Fatalf("unscoped refresh exceeded or missed bounded sample: %d", len(candidates))
	}
	if got := anyToInt(stats["population_count"], 0); got != 20010 {
		t.Fatalf("unscoped population count is not truthful: %d", got)
	}
	if got := anyToInt(stats["scanned_count"], 0); got != 20010 {
		t.Fatalf("unscoped scan count is not truthful: %d", got)
	}
	if got := anyToInt(stats["sample_count"], 0); got != 17 {
		t.Fatalf("unscoped sample count is not bounded/truthful: %d", got)
	}

	scoped, scopedSource, scopedStats := s.recallEvalIndexedCandidates(context.Background(), "target", "", 7)
	if scopedSource != "project_current_state_bottom_k" {
		t.Fatalf("scoped refresh did not use project index: %q", scopedSource)
	}
	if len(scoped) != 7 {
		t.Fatalf("scoped refresh sample mismatch: %d", len(scoped))
	}
	if got := anyToInt(scopedStats["population_count"], 0); got != 10 {
		t.Fatalf("scoped population should exclude unrelated projects: %d", got)
	}
	if got := anyToInt(scopedStats["scanned_count"], 0); got != 10 {
		t.Fatalf("scoped scan should use only indexed project keys: %d", got)
	}
	if !anyToBool(scopedStats["index_integrity"]) {
		t.Fatalf("scoped index integrity was not recorded: %#v", scopedStats)
	}
	for _, candidate := range scoped {
		if candidate.doc.Project != "target" {
			t.Fatalf("scoped refresh leaked unrelated project: %#v", candidate.doc)
		}
	}
}

func TestRecallEvalIndexedCandidatesConcurrentWriter(t *testing.T) {
	store := recallEvalV3IndexedTestStore(20000)
	s := &server{memoryStore: store}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for idx := 0; idx < 100; idx++ {
			entry := memoryStoreEntry{
				EventID: fmt.Sprintf("writer-event-%03d", idx), Project: "writer", FileName: fmt.Sprintf("notes/%03d.md", idx),
				TopicPath: "recall/quality", Summary: "concurrent writer", Lifecycle: "durable", CreatedAt: nowUTCISO(),
			}
			store.mu.Lock()
			store.currentState[memoryStoreKey(entry.Project, entry.FileName)] = memoryCurrentStateFromEntry(entry)
			store.mu.Unlock()
		}
	}()
	_, _, _ = s.recallEvalIndexedCandidates(context.Background(), "", "", 32)
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent writer did not complete while bounded refresh was running")
	}
}

func TestRecallEvalIndexedCandidatesFailsClosedOnCorruptProjectIndex(t *testing.T) {
	store := recallEvalV3IndexedTestStore(64)
	store.currentKeyCountsByProject["target"]++
	s := &server{memoryStore: store}
	candidates, source, stats := s.recallEvalIndexedCandidates(context.Background(), "target", "", 8)
	if len(candidates) != 0 || source != "project_index_integrity_invalid" {
		t.Fatalf("corrupt project index was not fail-closed: source=%q candidates=%d stats=%#v", source, len(candidates), stats)
	}
	if anyToBool(stats["index_integrity"]) || anyToInt(stats["scanned_count"], 0) != 0 {
		t.Fatalf("corrupt index reported a valid or scanned fallback: %#v", stats)
	}
}

func TestRecallEvalV3ValidationRejectsDuplicateLeakageAndSynthetic(t *testing.T) {
	cases := []map[string]any{
		{
			"id": "duplicate", "query": "notes/expected.md", "project": "alpha", "topic_path": "runbooks/testing",
			"expected_files": []string{"notes/expected.md"}, "split": "train",
		},
		{
			"id": "duplicate", "query": "notes/expected.md", "project": "alpha", "topic_path": "runbooks/testing",
			"expected_files": []string{"notes/expected.md"}, "split": "holdout", "source_updated_at": "2026-08-01T00:00:00Z",
		},
	}
	cfg := recallEvalSavedConfig{
		SchemaID: savedRecallEvalV3SchemaID, Version: savedRecallEvalV3Version,
		CaseSetDigest: "sha256:synthetic", Synthetic: true,
		Cases: cases,
	}
	health := validateSavedRecallEvalCaseSet(cfg)
	if anyToBool(health["valid"]) || anyToBool(health["benchmark_eligible"]) {
		t.Fatalf("synthetic malformed v3 set unexpectedly valid: %#v", health)
	}
	issues := anyToSliceOfMaps(health["issues"])
	want := map[string]bool{"synthetic_case_set": true, "case_set_digest_mismatch": true, "duplicate_case_id": true, "duplicate_expected_file": true, "query_contains_expected_file": true}
	found := map[string]bool{}
	for _, issue := range issues {
		found[anyToString(issue["code"])] = true
	}
	for code := range want {
		if !found[code] {
			t.Fatalf("validation omitted %s: %#v", code, health)
		}
	}
}

func TestSavedRecallEvalQueryExpansionPreservesThreeStates(t *testing.T) {
	without := map[string]any{}
	applySavedRecallEvalCaseOptionalRetrievalFlags(without, map[string]any{})
	if _, present := without["query_expansion"]; present {
		t.Fatalf("omitted query_expansion must preserve product default, got %#v", without)
	}
	explicitFalse := map[string]any{}
	applySavedRecallEvalCaseOptionalRetrievalFlags(explicitFalse, map[string]any{"query_expansion": false})
	if value, present := explicitFalse["query_expansion"]; !present || anyToBool(value) {
		t.Fatalf("explicit false query_expansion was not preserved: %#v", explicitFalse)
	}
	explicitTrue := map[string]any{}
	applySavedRecallEvalCaseOptionalRetrievalFlags(explicitTrue, map[string]any{"query_expansion": true})
	if value, present := explicitTrue["query_expansion"]; !present || !anyToBool(value) {
		t.Fatalf("explicit true query_expansion was not preserved: %#v", explicitTrue)
	}
}

func TestRecallEvalCasesForSplitIsExactAndDeterministic(t *testing.T) {
	cases := []map[string]any{
		{"id": "train-1", "split": "train"},
		{"id": "holdout-1", "split": "holdout"},
		{"id": "train-2", "split": "train"},
	}
	all := recallEvalCasesForSplit(cases, "all")
	if len(all) != 3 || anyToString(all[0]["id"]) != "train-1" || anyToString(all[2]["id"]) != "train-2" {
		t.Fatalf("all split changed case order or count: %#v", all)
	}
	train := recallEvalCasesForSplit(cases, "train")
	if len(train) != 2 || anyToString(train[0]["id"]) != "train-1" || anyToString(train[1]["id"]) != "train-2" {
		t.Fatalf("train split mismatch: %#v", train)
	}
	holdout := recallEvalCasesForSplit(cases, "holdout")
	if len(holdout) != 1 || anyToString(holdout[0]["id"]) != "holdout-1" {
		t.Fatalf("holdout split mismatch: %#v", holdout)
	}
	if got := recallEvalCasesForSplit(cases, "unknown"); len(got) != 0 {
		t.Fatalf("unknown split should be empty for caller validation, got %#v", got)
	}
}

func anyToSliceOfMaps(value any) []map[string]any {
	items, _ := value.([]map[string]any)
	if items != nil {
		return items
	}
	itemsAny, _ := value.([]any)
	result := make([]map[string]any, 0, len(itemsAny))
	for _, item := range itemsAny {
		if mapped, ok := item.(map[string]any); ok {
			result = append(result, mapped)
		}
	}
	return result
}
