package main

import (
	"strings"
	"testing"
)

func TestRecallResponseU3CompressionExhaustiveMinimality(t *testing.T) {
	candidates := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64), "sha256:" + strings.Repeat("c", 64)}
	obligations := []string{"primary:" + candidates[0], "gap:" + candidates[2]}
	minimum, ok := recallResponseExhaustiveMinimum(candidates, obligations)
	if !ok || len(minimum) != 2 || minimum[0] != candidates[0] || minimum[1] != candidates[2] {
		t.Fatalf("bounded exhaustive oracle returned the wrong minimum: ok=%v minimum=%v", ok, minimum)
	}
	if _, ok := recallResponseExhaustiveMinimum(candidates, []string{"primary:missing"}); ok {
		t.Fatal("oracle accepted an obligation with no candidate")
	}
}

func TestRecallResponseU3CompressionCoversDistinctModuleEvidence(t *testing.T) {
	decisionRef := "sha256:" + strings.Repeat("d", 64)
	timelineRef := "sha256:" + strings.Repeat("e", 64)
	response := map[string]any{
		"answer": map[string]any{"components": []any{
			map[string]any{"kind": "decision_rationale"},
			map[string]any{"kind": "timeline"},
		}},
		"evidence": []any{
			map[string]any{"kind": "decision", "ref_id": decisionRef},
			map[string]any{"kind": "event", "ref_id": timelineRef},
		},
	}
	proof := map[string]any{
		"primary_result": decisionRef, "proof_refs": []any{decisionRef, timelineRef},
		"conflict_refs": []any{}, "gap_refs": []any{}, "coverage": []any{},
	}
	compressed, result, ok := recallResponseCompressProof(response, proof, recallResponseProductionPolicyInput())
	if !ok || !result.Sufficient || !result.OraclePassed {
		t.Fatalf("module-aware compression failed: result=%#v", result)
	}
	selected := anyToStringList(compressed["proof_refs"], recallResponseMaxProofRefs)
	if len(selected) != 2 || !containsString(selected, decisionRef) || !containsString(selected, timelineRef) {
		t.Fatalf("distinct module evidence was discarded: %#v", compressed)
	}
	for _, name := range []string{"module:decision_rationale", "module:timeline"} {
		found := false
		for _, raw := range contextPackAnyList(compressed["coverage"]) {
			if anyToString(anyMap(raw)["obligation"]) == name && anyToString(anyMap(raw)["status"]) == "satisfied" {
				found = true
			}
		}
		if !found {
			t.Fatalf("module obligation %q was not proven: %#v", name, compressed["coverage"])
		}
	}
}

func TestRecallResponseU3CompressionPreservesProtectedRefsAndBudget(t *testing.T) {
	response := recallResponseTestInput(true)
	response["source_coverage"] = map[string]any{"complete": false, "pending": []any{"archive"}}
	response["context_pack"].(map[string]any)["proof_claims"] = []any{map[string]any{
		"claim_id": "claim-current", "proof_status": "contested",
		"support":    []any{map[string]any{"ref_id": "rtc_" + strings.Repeat("a", 24)}},
		"opposition": []any{map[string]any{"ref_id": "claim-opposing"}},
	}}
	composed := composeRecallResponse(response)
	proof := anyMap(anyMap(composed["answer"])["proof_spine"])
	if len(contextPackAnyList(proof["proof_refs"])) > recallResponseMaxProofRefs || len(contextPackAnyList(proof["coverage"])) < 1 {
		t.Fatalf("compressed proof exceeded bound or lost coverage: %#v", proof)
	}
	for _, ref := range append(contextPackAnyList(proof["conflict_refs"]), contextPackAnyList(proof["gap_refs"])...) {
		if !containsString(anyToStringList(proof["proof_refs"], recallResponseMaxProofRefs), anyToString(ref)) {
			t.Fatalf("protected ref was omitted from proof spine: %q %#v", ref, proof)
		}
	}
	bytes, tokens := recallResponseCompactBudget(composed)
	if bytes > recallResponseMaxCompactBytes || tokens > recallResponseMaxCompactTokens {
		t.Fatalf("compact response budget exceeded: bytes=%d tokens=%d", bytes, tokens)
	}
}

func TestRecallResponseU3CompressionRetainsLateStructuredActionWitness(t *testing.T) {
	input := recallResponseTestInput(false)
	input["query"] = "Prepare the next advisory action with verified parameters and rollback conditions"
	input["retrieval_intent"] = "procedure"
	input["task_class"] = "action"
	rows := make([]any, 0, 9)
	for index := 0; index < 9; index++ {
		row := map[string]any{
			"candidate_id": "rtc_" + strings.Repeat(anyToString(index), 24),
			"kind":         "runbook",
			"status":       "active",
			"confidence":   0.9,
			"source":       "bounded-runbook",
		}
		if index == 8 {
			row["action_evidence"] = map[string]any{
				"tool_ref": "sha256:" + strings.Repeat("d", 64),
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": "sha256:" + strings.Repeat("e", 64),
					"value_state":   "resolved",
					"required":      true,
				}},
				"rollback_conditions": []any{"restore_previous_state"},
			}
		}
		rows = append(rows, row)
	}
	input["context_pack"] = map[string]any{"ranked_evidence": rows}
	response := composeRecallResponse(input)
	modules := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(modules) == 0 || anyToString(anyMap(modules[0])["kind"]) != "memory_to_action" {
		t.Fatalf("action task lost its primary module: %#v", modules)
	}
	payload := anyMap(anyMap(modules[0])["payload"])
	proofRefs := anyToStringList(anyMap(anyMap(response["answer"])["proof_spine"])["proof_refs"], recallResponseMaxProofRefs)
	witness := "rtc_" + strings.Repeat("8", 24)
	if !containsString(proofRefs, witness) || anyToString(payload["intended_tool_ref"]) == "unresolved_tool" || len(contextPackAnyList(payload["parameter_bindings"])) != 1 {
		t.Fatalf("late structured action witness was not retained: proof=%v payload=%#v", proofRefs, payload)
	}
}

func TestRecallResponseU3StructuredActionIsBoundedSecondaryForStatus(t *testing.T) {
	input := recallResponseTestInput(false)
	input["query"] = "What is the current status?"
	input["retrieval_intent"] = "status"
	input["task_class"] = "status"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("1", 24), "kind": "fact", "status": "active", "confidence": 0.95,
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("2", 24), "kind": "runbook", "status": "active", "confidence": 0.9,
			"action_evidence": map[string]any{"tool_ref": "sha256:" + strings.Repeat("f", 64)},
		},
	}}
	response := composeRecallResponse(input)
	modules := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(modules) < 2 || anyToString(anyMap(modules[0])["kind"]) != "exact_current_status" || anyToString(anyMap(modules[1])["kind"]) != "memory_to_action" {
		t.Fatalf("structured action did not remain a bounded secondary: %#v", modules)
	}
	if anyToBool(anyMap(response["action_boundary"])["can_act"]) || anyToBool(anyMap(response["action_boundary"])["execution_performed"]) {
		t.Fatalf("advisory action secondary minted authority: %#v", response["action_boundary"])
	}
}

func TestRecallResponseU3BudgetPreservesProofBoundActionBeforeOptionalEvidence(t *testing.T) {
	input := recallResponseTestInput(false)
	input["query"] = "What is the current status and the next evidence-bound advisory action?"
	input["retrieval_intent"] = "status"
	input["task_class"] = "status"
	rows := make([]any, 0, recallResponseMaxEvidence)
	for index := 0; index < recallResponseMaxEvidence; index++ {
		row := map[string]any{
			"candidate_id": "bounded-evidence-" + anyToString(index),
			"kind":         "fact",
			"status":       "active",
			"confidence":   0.9,
			"source":       "bounded-source-" + anyToString(index),
			"support":      "context",
		}
		if index <= 1 {
			row["support"] = "direct"
		}
		if index == recallResponseMaxEvidence-1 {
			row["kind"] = "runbook"
			row["support"] = "direct"
			row["action_evidence"] = map[string]any{
				"tool_ref": "sha256:" + strings.Repeat("a", 64),
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": "sha256:" + strings.Repeat("b", 64),
					"value_state":   "resolved",
					"required":      true,
				}},
			}
		}
		rows = append(rows, row)
	}
	input["context_pack"] = map[string]any{"ranked_evidence": rows}
	response := composeRecallResponse(input)
	modules := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(modules) < 2 || anyToString(anyMap(modules[0])["kind"]) != "exact_current_status" || anyToString(anyMap(modules[1])["kind"]) != "memory_to_action" {
		t.Fatalf("budget pressure displaced the proof-bound action module: %#v", modules)
	}
	proofRefs := anyToStringList(anyMap(anyMap(response["answer"])["proof_spine"])["proof_refs"], recallResponseMaxProofRefs)
	actionRef := recallResponseProjectedRowRef(anyMap(rows[len(rows)-1]), response)
	if actionRef == "" || !containsString(proofRefs, actionRef) {
		t.Fatalf("budget pressure removed the action witness: ref=%q proof=%v", actionRef, proofRefs)
	}
	directRef := recallResponseProjectedRowRef(anyMap(rows[1]), response)
	evidenceRefs := make([]string, 0, len(contextPackAnyList(response["evidence"])))
	for _, raw := range contextPackAnyList(response["evidence"]) {
		evidenceRefs = append(evidenceRefs, anyToString(anyMap(raw)["ref_id"]))
	}
	if directRef == "" || !containsString(evidenceRefs, directRef) {
		t.Fatalf("budget pressure pruned direct support before optional context: ref=%q evidence=%v", directRef, evidenceRefs)
	}
	bytes, tokens := recallResponseCompactBudget(response)
	if bytes > recallResponseMaxCompactBytes || tokens > recallResponseMaxCompactTokens {
		t.Fatalf("proof-bound action response exceeded compact budget: bytes=%d tokens=%d", bytes, tokens)
	}
}

func TestRecallResponseU3BudgetP95(t *testing.T) {
	sizes := []int{}
	for index := 0; index < 5; index++ {
		input := recallResponseTestInput(true)
		input["query"] = "status verification variant " + anyToString(index)
		response := composeRecallResponse(input)
		bytes, _ := recallResponseCompactBudget(response)
		sizes = append(sizes, bytes)
	}
	if got := recallResponseNearestRank(sizes, 95); got > recallResponseMaxCompactBytes {
		t.Fatalf("p95 compact bytes exceeded budget: sizes=%v p95=%d", sizes, got)
	}
}

func TestRecallResponseU3CompressionFailureFallsBackToV1(t *testing.T) {
	input := recallResponseTestInput(true)
	conflicts := []any{}
	for index := 0; index < 8; index++ {
		conflicts = append(conflicts, map[string]any{"conflict_id": "conflict-" + anyToString(index), "support": []any{}, "opposition": []any{}})
	}
	input["conflicts"] = conflicts
	input["source_coverage"] = map[string]any{"complete": false, "pending": []any{"archive"}}
	response := composeRecallResponse(input)
	composition := anyMap(anyMap(response["answer"])["composition"])
	if anyToString(composition["primary_module"]) != "v1_control" || anyToString(composition["fallback_reason"]) != "candidate_projection_invalid" {
		t.Fatalf("compression failure did not use v1 control fallback: %#v", composition)
	}
}
