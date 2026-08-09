package main

import (
	"reflect"
	"testing"
)

func TestRecallResponseRouterDerivesFiveFacetsAndHybridOrder(t *testing.T) {
	input := recallResponseTestInput(true)
	input["query"] = "continue the project and explain why the decision was made"
	input["task_class"] = "continuation"
	input["classification"] = map[string]any{
		"jobs": []any{"act"}, "objects": []any{"identity"},
		"evidence_state": "clean", "posture": "answer_with_proof",
	}
	response := composeRecallResponse(input)
	classification := anyMap(response["classification"])
	facets := anyMap(classification["facets"])
	if !recallResponseExactFields(facets, []string{"jobs", "memory_objects", "temporal_state", "evidence_state", "consequence"}) {
		t.Fatalf("five-facet router was not closed: %#v", facets)
	}
	if containsString(anyToStringList(facets["jobs"], 8), "act") || containsString(anyToStringList(facets["memory_objects"], 8), "identity") {
		t.Fatalf("caller-forged facets entered the server projection: %#v", facets)
	}
	wantKinds := []string{"project_continuation", "decision_rationale", "multi_memory_synthesis"}
	gotKinds := []string{}
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		gotKinds = append(gotKinds, anyToString(anyMap(raw)["kind"]))
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("hybrid component order drifted: got=%v want=%v", gotKinds, wantKinds)
	}
	if anyToString(classification["posture"]) != recallResponseDerivedPosture(facets, 2, 0, 0) {
		t.Fatalf("posture was not derived from the five facets: %#v", classification)
	}
}

func TestRecallResponseRouterCallerSignalsOnlyIncreaseCaution(t *testing.T) {
	base := recallResponseTestInput(true)
	baseResponse := composeRecallResponse(base)
	baseConsequence := anyToString(anyMap(anyMap(baseResponse["classification"])["facets"])["consequence"])

	cautious := recallResponseTestInput(true)
	cautious["consequence_hint"] = "high_stakes"
	cautious["classification"] = map[string]any{
		"consequence": "informational", "evidence_state": "clean", "posture": "answer_with_proof",
	}
	response := composeRecallResponse(cautious)
	classification := anyMap(response["classification"])
	if got := anyToString(anyMap(classification["facets"])["consequence"]); got != "high_stakes" || recallResponseConsequenceRank[got] < recallResponseConsequenceRank[baseConsequence] {
		t.Fatalf("caller caution was lowered or ignored: base=%q got=%q", baseConsequence, got)
	}
	if got := anyToString(classification["posture"]); got != "verify_before_action" {
		t.Fatalf("high-consequence signal strengthened posture: %q", got)
	}
}

func TestRecallResponseRouterNormalizedTaskClassOwnsPrimaryModule(t *testing.T) {
	for _, tc := range []struct {
		taskClass string
		query     string
		want      string
	}{
		{taskClass: "status", query: "show the current state and explain changes", want: "exact_current_status"},
		{taskClass: "decision", query: "verify and explain the decision", want: "decision_rationale"},
		{taskClass: "continuation", query: "verify and continue the project", want: "project_continuation"},
		{taskClass: "preference", query: "show the durable preference constraint", want: "preference_constraint"},
		{taskClass: "timeline", query: "reconstruct the timeline and current state", want: "timeline"},
		{taskClass: "procedure", query: "show the verified procedure", want: "procedure"},
		{taskClass: "action", query: "prepare the next advisory action", want: "memory_to_action"},
	} {
		t.Run(tc.taskClass, func(t *testing.T) {
			input := recallResponseTestInput(true)
			input["task_class"] = tc.taskClass
			input["query"] = tc.query
			if tc.taskClass == "action" {
				rows := contextPackAnyList(anyMap(input["context_pack"])["ranked_evidence"])
				anyMap(rows[0])["action_evidence"] = map[string]any{"tool_ref": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			}
			response := composeRecallResponse(input)
			modules := contextPackAnyList(anyMap(response["answer"])["components"])
			if len(modules) == 0 || anyToString(anyMap(modules[0])["kind"]) != tc.want {
				t.Fatalf("task class %q selected the wrong primary: %#v", tc.taskClass, modules)
			}
		})
	}
}

func TestRecallResponseRouterRecognizesConflictAndNegativeRequests(t *testing.T) {
	for _, tc := range []struct {
		name     string
		query    string
		decorate func(map[string]any)
		want     string
	}{
		{
			name: "conflict", query: "Resolve the competing bounded claims only if supersession is proven.", want: "conflict_supersession",
		},
		{
			name: "negative", query: "Determine whether the bounded event did not happen.", want: "negative_abstention",
			decorate: func(input map[string]any) {
				row := anyMap(contextPackAnyList(anyMap(input["context_pack"])["ranked_evidence"])[0])
				row["negative_terminal"] = "did_not_happen"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := recallResponseTestInput(true)
			input["task_class"] = "general"
			input["retrieval_intent"] = "proof"
			input["query"] = tc.query
			if tc.decorate != nil {
				tc.decorate(input)
			}
			response := composeRecallResponse(input)
			modules := contextPackAnyList(anyMap(response["answer"])["components"])
			if len(modules) == 0 || anyToString(anyMap(modules[0])["kind"]) != tc.want {
				t.Fatalf("%s request selected the wrong primary: %#v", tc.name, modules)
			}
		})
	}
}

func TestRecallResponseRouterDoesNotPromoteExplicitDistractors(t *testing.T) {
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{"candidate_id": "rtc_111111111111111111111111", "kind": "fact", "status": "active", "confidence": 0.9, "support": "direct"},
		map[string]any{"candidate_id": "rtc_222222222222222222222222", "kind": "fact", "status": "active", "confidence": 0.9, "support": "distractor"},
	}}
	response := composeRecallResponse(input)
	evidence := contextPackAnyList(response["evidence"])
	if len(evidence) != 1 || anyToString(anyMap(evidence[0])["ref_id"]) != "rtc_111111111111111111111111" {
		t.Fatalf("explicit distractor became response support: %#v", evidence)
	}
}
