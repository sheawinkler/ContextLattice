package main

import (
	"reflect"
	"testing"
)

func retrievalAblationTestRows(value any) []map[string]any {
	rows, _ := value.([]map[string]any)
	return rows
}

func retrievalAblationFindRow(t *testing.T, rows []map[string]any, kind, targetRef string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if anyToString(row["kind"]) == kind && anyToString(row["target_ref"]) == targetRef {
			return row
		}
	}
	t.Fatalf("missing %s ablation for %q in %#v", kind, targetRef, rows)
	return nil
}

func TestRetrievalAblationUsesSameSnapshotAndExactResultCitationLabels(t *testing.T) {
	input := retrievalAblationInput{
		CaseID: "paired-case",
		K:      3,
		Results: []map[string]any{
			{"memory_id": "alpha::notes/a.md", "project": "alpha", "file": "notes/a.md", "sources": []string{"qdrant", "mongo_raw"}, "source": "qdrant", "score": 0.95},
			{"memory_id": "alpha::notes/b.md", "project": "alpha", "file": "notes/b.md", "source": "memory_bank", "score": 0.90},
			{"memory_id": "alpha::notes/c.md", "project": "alpha", "file": "notes/c.md", "source": "qdrant", "score": 0.80},
		},
		ExpectedFiles:  []string{"notes/a.md", "notes/b.md"},
		TrafficClass:   "synthetic",
		SnapshotStable: true,
	}
	first := buildRetrievalAblation(input)
	second := buildRetrievalAblation(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("ablation changed for the same case/snapshot:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if !anyToBool(first["same_case"]) || !anyToBool(first["same_snapshot"]) || !utilitySHA256DigestValid(anyToString(first["snapshot_digest"])) {
		t.Fatalf("ablation lacks paired snapshot identity: %#v", first)
	}
	rows := retrievalAblationTestRows(first["rows"])
	qdrant := retrievalAblationFindRow(t, rows, "leave_one_source", "qdrant")
	if anyToString(qdrant["result_change_label"]) != "changed" || anyToString(qdrant["citation_change_label"]) != "unchanged" {
		t.Fatalf("correlated qdrant result should retain citations while removing its exclusive row: %#v", qdrant)
	}
	memoryBank := retrievalAblationFindRow(t, rows, "leave_one_source", "memory_bank")
	if anyToString(memoryBank["result_change_label"]) != "changed" || anyToString(memoryBank["citation_change_label"]) != "lost" {
		t.Fatalf("decisive memory_bank result did not label exact loss: %#v", memoryBank)
	}
	changedCitations := retrievalAblationTestRows(memoryBank["changed_citations"])
	if len(changedCitations) != 1 || anyToString(changedCitations[0]["citation"]) != "notes/b.md" || anyToString(changedCitations[0]["label"]) != "removed" {
		t.Fatalf("unexpected citation delta: %#v", changedCitations)
	}
	if anyToString(memoryBank["outcome_change_label"]) != "unobserved" || anyToBool(memoryBank["promotion_eligible"]) || anyToBool(memoryBank["utility_inferred"]) || memoryBank["utility"] != nil {
		t.Fatalf("synthetic outcome-less row inferred utility or became promotable: %#v", memoryBank)
	}
	reasons := anyToStringList(memoryBank["promotion_ineligible_reasons"], 8)
	if !retrievalAblationContains(reasons, "synthetic_row") || !retrievalAblationContains(reasons, "outcome_unobserved") {
		t.Fatalf("missing synthetic/outcome ineligibility labels: %#v", memoryBank)
	}
}

func TestRetrievalAblationUsesOnlyExplicitPairedOutcomeLabels(t *testing.T) {
	input := retrievalAblationInput{
		CaseID: "observed-case",
		K:      1,
		Results: []map[string]any{
			{"memory_id": "alpha::notes/a.md", "project": "alpha", "file": "notes/a.md", "source": "qdrant", "score": 0.95},
		},
		ExpectedFiles:                 []string{"notes/a.md"},
		TrafficClass:                  "user",
		SnapshotStable:                true,
		BaselineOutcomeLabel:          "success",
		BaselineOutcomeEvidenceDigest: "sha256:" + sha256Hex("baseline-outcome"),
		CounterfactualOutcomeLabels: map[string]string{
			"leave_one_source:qdrant":            "repair_required",
			"leave_one_memory:alpha::notes/a.md": "success",
		},
		CounterfactualOutcomeEvidenceDigests: map[string]string{
			"leave_one_source:qdrant":            "sha256:" + sha256Hex("source-counterfactual"),
			"leave_one_memory:alpha::notes/a.md": "sha256:" + sha256Hex("memory-counterfactual"),
		},
	}
	report := buildRetrievalAblation(input)
	rows := retrievalAblationTestRows(report["rows"])
	source := retrievalAblationFindRow(t, rows, "leave_one_source", "qdrant")
	if anyToString(source["outcome_change_label"]) != "changed" || !anyToBool(source["outcome_observed"]) || !anyToBool(source["promotion_eligible"]) {
		t.Fatalf("explicit source outcome pair was not labeled exactly: %#v", source)
	}
	memory := retrievalAblationFindRow(t, rows, "leave_one_memory", "alpha::notes/a.md")
	if anyToString(memory["outcome_change_label"]) != "unchanged" || !anyToBool(memory["outcome_observed"]) || !anyToBool(memory["promotion_eligible"]) {
		t.Fatalf("explicit memory outcome pair was not labeled exactly: %#v", memory)
	}
	for _, row := range rows {
		if row["utility"] != nil || anyToBool(row["utility_inferred"]) {
			t.Fatalf("exact outcome labels must not infer utility: %#v", row)
		}
	}

	unverifiedInput := input
	unverifiedInput.BaselineOutcomeEvidenceDigest = ""
	unverifiedInput.CounterfactualOutcomeEvidenceDigests = nil
	unverifiedRows := retrievalAblationTestRows(buildRetrievalAblation(unverifiedInput)["rows"])
	unverified := retrievalAblationFindRow(t, unverifiedRows, "leave_one_source", "qdrant")
	if anyToString(unverified["outcome_change_label"]) != "unverified" || anyToBool(unverified["outcome_observed"]) || anyToBool(unverified["promotion_eligible"]) {
		t.Fatalf("unverified outcome labels became observed or promotable: %#v", unverified)
	}
	if !retrievalAblationContains(anyToStringList(unverified["promotion_ineligible_reasons"], 8), "outcome_pair_unverified") {
		t.Fatalf("missing unverified outcome pair reason: %#v", unverified)
	}
}
