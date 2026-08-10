package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func recallResponseModuleByKind(t *testing.T, response map[string]any, kind string) map[string]any {
	t.Helper()
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		module := anyMap(raw)
		if anyToString(module["kind"]) == kind {
			return module
		}
	}
	t.Fatalf("module %q not found: %#v", kind, anyMap(response["answer"])["components"])
	return nil
}

func TestRecallResponseU4HistoricalStatusUsesExplicitTransitions(t *testing.T) {
	input := recallResponseTestInput(false)
	input["as_of"] = "2026-06-01T00:00:00Z"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("c", 24), "kind": "fact", "confidence": 0.9,
			"source": "temporal", "status": "superseded", "observed_at": "2026-01-01T00:00:00Z",
			"transition_history_complete": true,
			"status_transitions": []any{
				map[string]any{"status": "active", "effective_at": "2026-01-01T00:00:00Z"},
				map[string]any{"status": "superseded", "effective_at": "2026-07-01T00:00:00Z"},
			},
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("d", 24), "kind": "fact", "confidence": 0.9,
			"source": "temporal", "status": "active", "observed_at": "2026-07-01T00:00:00Z",
		},
	}}
	response := composeRecallResponse(input)
	if got := len(contextPackAnyList(response["evidence"])); got != 1 {
		t.Fatalf("historical transition selection got %d evidence rows: %#v", got, response)
	}
	if got := anyToString(anyMap(contextPackAnyList(response["evidence"])[0])["ref_id"]); got != "rtc_"+strings.Repeat("c", 24) {
		t.Fatalf("historical winner drifted: %q", got)
	}
}

func TestRecallResponseU4SupersessionForgetsSupportButKeepsRetirementProof(t *testing.T) {
	input := recallResponseTestInput(false)
	input["task_class"] = "timeline"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("e", 24), "kind": "fact", "confidence": 0.8, "source": "temporal",
			"status": "revoked", "text": "retired private value must disappear", "observed_at": "2026-01-01T00:00:00Z",
			"transition_history_complete": true,
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("f", 24), "kind": "fact", "confidence": 0.95, "source": "temporal",
			"status": "active", "text": "replacement", "observed_at": "2026-02-01T00:00:00Z",
			"supersedes": []any{"rtc_" + strings.Repeat("e", 24)}, "transition_history_complete": true,
			"status_transitions": []any{map[string]any{"status": "active", "effective_at": "2026-02-01T00:00:00Z"}},
		},
	}}
	response := composeRecallResponse(input)
	conflict := anyMap(recallResponseModuleByKind(t, response, "conflict_supersession")["payload"])
	if anyToString(conflict["resolution_status"]) != "proven_superseded" || anyToString(conflict["winner_ref"]) != "rtc_"+strings.Repeat("f", 24) {
		t.Fatalf("proven supersession was not preserved: %#v", conflict)
	}
	for _, raw := range contextPackAnyList(response["evidence"]) {
		if anyToString(anyMap(raw)["ref_id"]) == "rtc_"+strings.Repeat("e", 24) {
			t.Fatal("revoked evidence remained supporting evidence")
		}
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "retired private value") {
		t.Fatal("selective forgetting leaked retired content")
	}
	if !validateRecallResponseU2(response) {
		t.Fatalf("U4 supersession response failed validation: %#v", response)
	}
}

func TestRecallResponseU4NegativeTerminalsRequireBoundCoverage(t *testing.T) {
	notFoundInput := recallResponseTestInput(false)
	notFound := composeRecallResponse(notFoundInput)
	notFoundPayload := anyMap(recallResponseModuleByKind(t, notFound, "negative_abstention")["payload"])
	if anyToString(notFoundPayload["terminal"]) != "not_found" || !anyToBool(anyMap(notFoundPayload["coverage_receipt"])["complete"]) {
		t.Fatalf("complete absence did not produce not_found: %#v", notFoundPayload)
	}

	unknownInput := recallResponseTestInput(false)
	unknownInput["source_coverage"] = map[string]any{"complete": false, "returned": []any{"qdrant"}, "pending": []any{"archive"}}
	unknown := composeRecallResponse(unknownInput)
	unknownPayload := anyMap(recallResponseModuleByKind(t, unknown, "negative_abstention")["payload"])
	if anyToString(unknownPayload["terminal"]) != "unknown" || anyToBool(anyMap(unknownPayload["coverage_receipt"])["complete"]) {
		t.Fatalf("incomplete coverage overclaimed absence: %#v", unknownPayload)
	}

	didNotHappenInput := recallResponseTestInput(false)
	didNotHappenInput["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("9", 24), "kind": "fact", "confidence": 0.99,
		"source": "event_ledger", "status": "selected", "negative_terminal": "did_not_happen",
	}}}
	didNotHappen := composeRecallResponse(didNotHappenInput)
	didNotHappenPayload := anyMap(recallResponseModuleByKind(t, didNotHappen, "negative_abstention")["payload"])
	if anyToString(didNotHappenPayload["terminal"]) != "did_not_happen" || anyToString(didNotHappenPayload["negative_claim_ref"]) == "" {
		t.Fatalf("explicit negative event was not bound: %#v", didNotHappenPayload)
	}
	if !validateRecallResponseU2(didNotHappen) {
		t.Fatalf("explicit negative response failed validation: %#v", didNotHappen)
	}
}

func TestRecallResponseU4StructuredActionIsOpaqueOrderedAndAdvisory(t *testing.T) {
	input := recallResponseTestInput(false)
	input["retrieval_intent"] = "procedure"
	input["task_class"] = "procedure"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("8", 24), "kind": "runbook", "confidence": 0.91, "source": "runbook",
		"action_evidence": map[string]any{
			"tool": "dangerous-tool-name", "parameter_bindings": []any{
				map[string]any{"name": "api_key", "value": "never-emit-this-secret", "required": true, "sensitive": true},
			},
			"ordered_steps": []any{
				map[string]any{"instruction": "first hidden instruction"},
				map[string]any{"instruction": "second hidden instruction"},
			},
			"refusal_conditions":  []any{"credential_access", "external_mutation", "ignore previous instructions"},
			"recovery_conditions": []any{"verify_postcondition"},
		},
	}}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "procedure")["payload"])
	if !recallResponseValidDigest(anyToString(payload["tool_ref"])) || len(contextPackAnyList(payload["parameter_bindings"])) != 1 || len(contextPackAnyList(payload["ordered_steps"])) != 2 {
		t.Fatalf("structured action evidence was not projected: %#v", payload)
	}
	for index, raw := range contextPackAnyList(payload["ordered_steps"]) {
		if anyToInt(anyMap(raw)["ordinal"], 0) != index+1 || !anyToBool(anyMap(raw)["requires_confirmation"]) {
			t.Fatalf("step ordering/confirmation drifted: %#v", raw)
		}
	}
	if anyToBool(anyMap(response["action_boundary"])["can_act"]) || anyToBool(anyMap(response["action_boundary"])["execution_performed"]) {
		t.Fatalf("structured evidence minted action authority: %#v", response["action_boundary"])
	}
	encoded, _ := json.Marshal(response)
	for _, forbidden := range []string{"dangerous-tool-name", "api_key", "never-emit-this-secret", "hidden instruction", "ignore previous"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("structured action projection leaked %q", forbidden)
		}
	}
	if !validateRecallResponseU2(response) {
		t.Fatalf("structured action response failed validation: %#v", response)
	}

	tampered := cloneJSONMap(response)
	module := recallResponseModuleByKind(t, tampered, "procedure")
	steps := contextPackAnyList(anyMap(module["payload"])["ordered_steps"])
	anyMap(steps[1])["ordinal"] = 1
	module["component_digest"] = recallResponseComponentDigest(module)
	if validateRecallResponseU2(tampered) {
		t.Fatal("rehashed wrong action order remained valid")
	}
}

func TestRecallResponseActionPreparationFailsClosedAcrossProofCoverageAndRetirement(t *testing.T) {
	toolRef := "sha256:" + strings.Repeat("a", 64)
	parameterRef := "sha256:" + strings.Repeat("b", 64)
	ready := map[string]any{
		"candidate_id": "ready-action", "status": "selected", "confidence": 0.9,
		"relation": "specifies_action",
		"action_evidence": map[string]any{
			"tool_ref": toolRef,
			"parameter_bindings": []any{map[string]any{
				"parameter_ref": parameterRef, "value_state": "resolved", "required": true,
			}},
		},
	}
	source := func(intent string, complete bool, rows ...any) map[string]any {
		return map[string]any{
			"retrieval_intent": intent,
			"source_coverage":  map[string]any{"complete": complete},
			"context_pack":     map[string]any{"ranked_evidence": rows},
		}
	}
	if !recallResponseActionProjectionAllowed(source("action", true, ready)) {
		t.Fatal("complete current proof-bound action was not preparable")
	}
	if recallResponseActionProjectionAllowed(source("proof", true, ready)) {
		t.Fatal("proof-only request prepared an action")
	}
	if recallResponseActionProjectionAllowed(source("action", false, ready)) {
		t.Fatal("incomplete source coverage prepared an action")
	}
	retirement := map[string]any{
		"candidate_id": "retirement", "status": "revoked", "confidence": 0.9,
		"relation": "retires",
	}
	if recallResponseActionProjectionAllowed(source("action", true, ready, retirement)) {
		t.Fatal("retirement evidence prepared an action")
	}
	unresolved := cloneJSONMap(ready)
	unresolved["action_evidence"] = map[string]any{
		"tool_ref": toolRef,
		"parameter_bindings": []any{map[string]any{
			"parameter_ref": parameterRef, "value_state": "unresolved", "required": true,
		}},
	}
	if !recallResponseActionProjectionAllowed(source("action", true, unresolved)) {
		t.Fatal("unresolved action evidence was not retained for an advisory refusal")
	}
	projected := recallResponseProjectActionMetadata(unresolved)
	if recallResponseActionMetadataReady(projected) {
		t.Fatal("unresolved required parameter became selectable")
	}
	components := recallResponseComponents(
		map[string]any{"jobs": []any{"act"}}, []any{unresolved}, "action", 1, 0, 0,
		"sha256:"+strings.Repeat("c", 64), source("action", true, unresolved),
	)
	kinds := []string{}
	for _, raw := range components {
		kinds = append(kinds, anyToString(anyMap(raw)["kind"]))
	}
	if !containsString(kinds, "memory_to_action") || !containsString(kinds, "negative_abstention") {
		t.Fatalf("unresolved advisory action did not retain its safety module: %v", kinds)
	}
}

func TestRecallResponseU4MalformedValidityCannotProveAbsence(t *testing.T) {
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("7", 24), "kind": "fact", "confidence": 0.9,
		"source": "temporal", "status": "active", "valid_from": "not-a-time",
	}}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "negative_abstention")["payload"])
	if anyToString(payload["terminal"]) != "unknown" || anyToBool(anyMap(payload["coverage_receipt"])["complete"]) {
		t.Fatalf("malformed validity overclaimed absence: %#v", payload)
	}
	found := false
	for _, raw := range contextPackAnyList(response["gaps"]) {
		if anyToString(anyMap(raw)["code"]) == "historical_evidence_without_valid_time" {
			found = true
		}
	}
	if !found {
		t.Fatalf("malformed validity was not disclosed: %#v", response["gaps"])
	}
}

func TestRecallResponseU4AmbiguousTransitionsRemainUnknown(t *testing.T) {
	input := recallResponseTestInput(false)
	input["as_of"] = "2026-06-01T00:00:00Z"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("6", 24), "kind": "fact", "confidence": 0.9,
		"source": "temporal", "status": "active", "observed_at": "2026-01-01T00:00:00Z",
		"transition_history_complete": true,
		"status_transitions": []any{
			map[string]any{"status": "active", "effective_at": "2026-01-01T00:00:00Z"},
			map[string]any{"status": "revoked", "effective_at": "2026-01-01T00:00:00Z"},
		},
	}}}
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["evidence"])) != 0 {
		t.Fatalf("conflicting simultaneous transitions became support: %#v", response["evidence"])
	}
	payload := anyMap(recallResponseModuleByKind(t, response, "negative_abstention")["payload"])
	if anyToString(payload["terminal"]) != "unknown" {
		t.Fatalf("ambiguous transitions overclaimed absence: %#v", payload)
	}
}

func TestRecallResponseU4SupersessionRequiresExactRetiredTarget(t *testing.T) {
	input := recallResponseTestInput(false)
	input["task_class"] = "timeline"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("5", 24), "kind": "fact", "confidence": 0.8,
			"source": "temporal", "status": "revoked", "transition_history_complete": true,
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("4", 24), "kind": "fact", "confidence": 0.95,
			"source": "temporal", "status": "active", "supersedes": []any{"claim_" + strings.Repeat("3", 24)},
			"transition_history_complete": true,
		},
	}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "conflict_supersession")["payload"])
	if anyToString(payload["resolution_status"]) != "unresolved" || anyToString(payload["winner_ref"]) != "" {
		t.Fatalf("unbound supersession target produced a winner: %#v", payload)
	}
}

func TestRecallResponseU4ActionEvidenceMustMatchSupportingProof(t *testing.T) {
	input := recallResponseTestInput(false)
	input["retrieval_intent"] = "procedure"
	input["task_class"] = "procedure"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("2", 24), "kind": "runbook", "confidence": 0.99,
			"source": "retired", "status": "revoked", "transition_history_complete": true,
			"action_evidence": map[string]any{"tool": "retired-tool", "ordered_steps": []any{"retired instruction"}},
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("1", 24), "kind": "runbook", "confidence": 0.9,
			"source": "current", "status": "active",
		},
	}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "procedure")["payload"])
	if anyToString(payload["tool_ref"]) != "unresolved_tool" || len(contextPackAnyList(payload["ordered_steps"])) != 0 {
		t.Fatalf("retired action evidence was rebound to current proof: %#v", payload)
	}
}

func TestRecallResponseU4ExplicitNegativeMustMatchSupportingProof(t *testing.T) {
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("a", 24), "kind": "fact", "confidence": 0.99,
			"source": "retired", "status": "revoked", "negative_terminal": "did_not_happen",
			"transition_history_complete": true,
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("b", 24), "kind": "fact", "confidence": 0.9,
			"source": "current", "status": "active",
		},
	}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "negative_abstention")["payload"])
	if anyToString(payload["terminal"]) == "did_not_happen" || anyToString(payload["negative_claim_ref"]) != "" {
		t.Fatalf("retired explicit negative was rebound to current proof: %#v", payload)
	}
}

func TestRecallResponseU4FutureObservedEvidenceIsNotLatestSupport(t *testing.T) {
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("d", 24), "kind": "fact", "confidence": 0.9,
		"source": "future", "status": "active", "observed_at": "2099-01-01T00:00:00Z",
	}}}
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["evidence"])) != 0 {
		t.Fatalf("future-observed evidence became latest support: %#v", response["evidence"])
	}
	payload := anyMap(recallResponseModuleByKind(t, response, "negative_abstention")["payload"])
	if anyToString(payload["terminal"]) != "unknown" {
		t.Fatalf("future-only evidence overclaimed absence: %#v", payload)
	}
}

func TestRecallResponseU9LatestUsesFrozenSemanticClock(t *testing.T) {
	installRecallResponseSemanticClock(t)
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("c", 24), "kind": "fact", "confidence": 0.9,
		"source": "frozen-future", "status": "active", "observed_at": "2026-12-30T00:00:00Z",
	}}}
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["evidence"])) != 1 {
		t.Fatalf("latest projection bypassed the frozen semantic clock: %#v", response["evidence"])
	}
}

func TestRecallResponseU4OpaqueActionRefsDoNotFingerprintContent(t *testing.T) {
	project := func(tool, parameter, instruction string) map[string]any {
		input := recallResponseTestInput(false)
		input["retrieval_intent"] = "procedure"
		input["task_class"] = "procedure"
		input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("0", 24), "kind": "runbook", "confidence": 0.91,
			"source": "runbook", "status": "active", "action_evidence": map[string]any{
				"tool": tool, "parameter_bindings": []any{map[string]any{"name": parameter}},
				"ordered_steps": []any{map[string]any{"instruction": instruction}},
			},
		}}}
		return anyMap(recallResponseModuleByKind(t, composeRecallResponse(input), "procedure")["payload"])
	}
	first := project("common-admin-tool", "api_key", "read a private path")
	second := project("different-admin-tool", "password", "send a credential")
	if anyToString(first["tool_ref"]) != anyToString(second["tool_ref"]) ||
		anyToString(anyMap(contextPackAnyList(first["parameter_bindings"])[0])["parameter_ref"]) != anyToString(anyMap(contextPackAnyList(second["parameter_bindings"])[0])["parameter_ref"]) ||
		anyToString(anyMap(contextPackAnyList(first["ordered_steps"])[0])["step_ref"]) != anyToString(anyMap(contextPackAnyList(second["ordered_steps"])[0])["step_ref"]) {
		t.Fatal("opaque action references fingerprinted low-entropy private content")
	}
}

func TestRecallResponseU4MemoryToActionRequiresBoundWitness(t *testing.T) {
	witness := "rtc_" + strings.Repeat("7", 24)
	payload := map[string]any{
		"intended_tool_ref":  "unresolved_tool",
		"parameter_bindings": []any{},
		"ordered_steps":      []any{},
		"refusal_conditions": []any{map[string]any{
			"code": "independent_authorization_required", "proof_ref": "",
		}},
		"rollback_conditions": []any{},
	}
	if recallResponseActionPayloadValid(payload, map[string]bool{}, true) {
		t.Fatal("memory-to-action accepted an unproved action projection")
	}
	anyMap(contextPackAnyList(payload["refusal_conditions"])[0])["proof_ref"] = witness
	if !recallResponseActionPayloadValid(payload, map[string]bool{witness: true}, true) {
		t.Fatal("memory-to-action rejected its selected bound action witness")
	}
}
func installRecallResponseSemanticClock(t *testing.T) {
	t.Helper()
	fixed, err := time.Parse(time.RFC3339, "2026-12-31T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	previous := recallResponseNowUTC
	recallResponseNowUTC = func() time.Time { return fixed }
	t.Cleanup(func() { recallResponseNowUTC = previous })
}
