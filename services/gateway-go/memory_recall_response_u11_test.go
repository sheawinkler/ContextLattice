package main

import (
	"strings"
	"testing"
)

func TestRecallResponseU11FallbackRetainsArtifactAndTaskIdentity(t *testing.T) {
	project := func(text string) map[string]any {
		input := recallResponseTestInput(true)
		input["task_class"] = "timeline"
		anyMap(contextPackAnyList(anyMap(input["context_pack"])["ranked_evidence"])[0])["text"] = text
		return projectRecallResponseV1ControlFromArtifacts(input, recallResponseProductionPolicyInput())
	}
	first := project("first retained artifact")
	second := project("second retained artifact")
	firstScope := anyMap(first["request_scope"])
	secondScope := anyMap(second["request_scope"])
	if !recallResponseIsV1Control(first) || anyToString(firstScope["task_class"]) != "timeline" {
		t.Fatalf("fallback lost its control/task identity: %#v", first)
	}
	if anyToString(firstScope["snapshot_digest"]) == anyToString(secondScope["snapshot_digest"]) ||
		!recallResponseValidDigest(anyToString(firstScope["snapshot_digest"])) ||
		!recallResponseValidDigest(anyToString(firstScope["receipt_digest"])) {
		t.Fatalf("different retained artifacts shared fallback identity: first=%#v second=%#v", firstScope, secondScope)
	}
	if anyToString(anyMap(anyMap(first["answer"])["proof_spine"])["temporal_premise_digest"]) != anyToString(firstScope["temporal_premise_digest"]) {
		t.Fatalf("fallback proof premise drifted from request scope: %#v", first)
	}
}

func TestRecallResponseU11CoverageRefsMustBelongToProofSpine(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	if recallResponseIsV1Control(response) {
		t.Fatalf("fixture unexpectedly returned control: %#v", response)
	}
	coverage := contextPackAnyList(anyMap(anyMap(response["answer"])["proof_spine"])["coverage"])
	if len(coverage) == 0 {
		t.Fatal("fixture omitted proof coverage")
	}
	anyMap(coverage[0])["proof_refs"] = []any{"ref_fabricated_outside_spine"}
	response["response_id"] = recallResponseIDForResponse(response)
	response["response_digest"] = recallResponseSemanticDigest(response)
	if validateRecallResponseU2(response) {
		t.Fatal("rehashed coverage accepted a proof ref outside the bounded spine")
	}
}

func TestRecallResponseU11HistoricalFutureClaimsCannotResolveSupersession(t *testing.T) {
	targetID := "rtc_" + strings.Repeat("3", 24)
	winnerID := "rtc_" + strings.Repeat("4", 24)
	input := recallResponseTestInput(false)
	input["task_class"] = "timeline"
	input["as_of"] = "2026-06-01T00:00:00Z"
	input["context_pack"] = map[string]any{
		"ranked_evidence": []any{
			map[string]any{
				"candidate_id": targetID, "kind": "fact", "confidence": 0.8,
				"status": "revoked", "observed_at": "2026-01-01T00:00:00Z", "transition_history_complete": true,
				"status_transitions": []any{
					map[string]any{"status": "active", "effective_at": "2026-01-01T00:00:00Z"},
					map[string]any{"status": "revoked", "effective_at": "2026-02-01T00:00:00Z"},
				},
			},
			map[string]any{
				"candidate_id": winnerID, "kind": "fact", "confidence": 0.95,
				"status": "active", "observed_at": "2026-01-01T00:00:00Z", "transition_history_complete": true,
				"status_transitions": []any{map[string]any{"status": "active", "effective_at": "2026-01-01T00:00:00Z"}},
			},
		},
		"temporal_claims": []any{map[string]any{
			"candidate_id": winnerID, "kind": "fact", "confidence": 0.95,
			"status": "active", "observed_at": "2026-07-01T00:00:00Z", "supersedes": []any{targetID},
			"transition_history_complete": true,
		}},
	}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "conflict_supersession")["payload"])
	if anyToString(payload["resolution_status"]) != "unresolved" || anyToString(payload["winner_ref"]) != "" {
		t.Fatalf("future temporal claim resolved a historical winner: %#v", payload)
	}
}

func TestRecallResponseU11SupersessionProjectsOrdinaryCandidateIDs(t *testing.T) {
	targetID := "memory-retired"
	winnerID := "memory-current"
	input := recallResponseTestInput(false)
	input["task_class"] = "timeline"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": targetID, "kind": "fact", "confidence": 0.8,
			"status": "revoked", "transition_history_complete": true,
		},
		map[string]any{
			"candidate_id": winnerID, "kind": "fact", "confidence": 0.95,
			"status": "active", "supersedes": []any{targetID}, "transition_history_complete": true,
		},
	}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "conflict_supersession")["payload"])
	evidence := contextPackAnyList(response["evidence"])
	if len(evidence) != 1 || anyToString(payload["resolution_status"]) != "proven_superseded" ||
		anyToString(payload["winner_ref"]) != anyToString(anyMap(evidence[0])["ref_id"]) {
		t.Fatalf("ordinary candidate identity did not bind the proven winner: evidence=%#v payload=%#v", evidence, payload)
	}
}

func TestRecallResponseU11SyntheticSafetyAblationDoesNotReinsertModule(t *testing.T) {
	input := recallResponseTestInput(true)
	input["task_class"] = "timeline"
	input["query"] = "show the timeline"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("5", 24), "kind": "fact", "confidence": 0.8,
			"status": "revoked", "transition_history_complete": true,
		},
		map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("6", 24), "kind": "fact", "confidence": 0.95,
			"status": "active", "transition_history_complete": true,
		},
	}}
	policy := validatedRecallResponsePolicyInput{
		condition: string(recallResponseConditionCompositional), ablation: "conflict_supersession", synthetic: true,
		snapshotDigest: "sha256:" + strings.Repeat("a", 64), receiptDigest: "sha256:" + strings.Repeat("b", 64),
		canaryPolicy: zeroRecallResponseCanaryPolicy{},
	}
	response := composeRecallResponseWithPolicy(input, policy)
	if recallResponseIsV1Control(response) {
		t.Fatalf("safety ablation invalidated unrelated selected modules: %#v", response)
	}
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		if anyToString(anyMap(raw)["kind"]) == policy.ablation {
			t.Fatalf("ablated safety module was reinserted: %#v", response)
		}
	}
}

func TestRecallResponseU11ExplicitEmptyComponentBindingIsInvalid(t *testing.T) {
	sample := map[string]any{
		"recall_response_id":      "rr_" + strings.Repeat("a", 24),
		"recall_response_digest":  "sha256:" + strings.Repeat("b", 64),
		"response_component_refs": []any{},
	}
	if binding, ok := recallResponseBindingFromSample(sample); ok || binding != nil {
		t.Fatalf("explicit empty component binding was accepted: %#v", binding)
	}
}

func TestRecallResponseU11CanaryBindingRejectsRehashedBucketAndArm(t *testing.T) {
	base := composeRecallResponse(recallResponseTestInput(true))
	if recallResponseIsV1Control(base) {
		t.Fatalf("fixture unexpectedly returned control: %#v", base)
	}
	for _, tc := range []struct {
		name   string
		reseal bool
		mutate func(map[string]any, map[string]any)
	}{
		{name: "bucket", reseal: true, mutate: func(_ map[string]any, binding map[string]any) {
			binding["exposure_bucket"] = (anyToInt(binding["exposure_bucket"], 0) + 1) % 10000
		}},
		{name: "zero-policy arm", reseal: true, mutate: func(_ map[string]any, binding map[string]any) {
			binding["arm"] = recallResponseCanaryArmCandidate
		}},
		{name: "ordinal", reseal: true, mutate: func(module, _ map[string]any) {
			module["ordinal"] = 2
		}},
		{name: "component digest", mutate: func(module, binding map[string]any) {
			forged := "sha256:" + strings.Repeat("f", 64)
			module["component_digest"] = forged
			binding["component_digest"] = forged
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := cloneJSONMap(base)
			module := anyMap(contextPackAnyList(anyMap(response["answer"])["components"])[0])
			tc.mutate(module, anyMap(module["binding"]))
			if tc.reseal && !recallResponseSealComponentIdentity(module) {
				t.Fatal("failed to reseal tampered component")
			}
			response["response_id"] = recallResponseIDForResponse(response)
			response["response_digest"] = recallResponseSemanticDigest(response)
			if validateRecallResponseU2(response) {
				t.Fatalf("rehashed %s tamper passed validation: %#v", tc.name, module)
			}
		})
	}
}

func TestRecallResponseU11FailureProjectionOmitsCallerWorkspaceAuthority(t *testing.T) {
	request := map[string]any{
		"query": "bounded failure", "project": "contextlattice", "workspace_ref": "forged-workspace",
		"workspace_id": "forged-workspace-id", "workspaceId": "forged-workspace-alias",
	}
	input := recallResponseCompositionInput(request, map[string]any{
		"source_coverage": map[string]any{"complete": false, "failed": []any{"context_pack"}},
	})
	response := projectRecallResponseV1ControlFromArtifacts(input, recallResponseProductionPolicyInput())
	if got := anyToString(anyMap(response["request_scope"])["workspace_ref"]); got != recallResponseScopeRef("workspace", "") {
		t.Fatalf("failure response stamped caller workspace authority: %q", got)
	}
	encoded := recallResponseCanonicalJSON(response)
	for _, forbidden := range []string{"forged-workspace", "forged-workspace-id", "forged-workspace-alias"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("failure response retained caller workspace %q: %s", forbidden, encoded)
		}
	}
}

func TestRecallResponseU11ActionFailureStatesRemainOpaqueAndNonAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		state       string
		sensitive   bool
		wantState   string
		wantRefusal string
	}{
		{state: "resolved", wantState: "bound_redacted"},
		{state: "unresolved", wantState: "unresolved", wantRefusal: "missing_required_parameter"},
		{state: "sensitive_unresolved", sensitive: true, wantState: "sensitive_unresolved", wantRefusal: "sensitive_value_unavailable"},
		{state: "unsafe", sensitive: true, wantState: "unsafe", wantRefusal: "credential_access"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			input := recallResponseTestInput(false)
			input["retrieval_intent"] = "procedure"
			input["task_class"] = "procedure"
			input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
				"candidate_id": "rtc_" + strings.Repeat("8", 24), "kind": "runbook", "confidence": 0.91,
				"action_evidence": map[string]any{
					"tool": "wrong-private-tool-" + tc.state,
					"parameter_bindings": []any{map[string]any{
						"parameter_ref": "parameter-" + tc.state, "value_state": tc.state,
						"required": true, "sensitive": tc.sensitive,
					}},
					"ordered_steps":       []any{map[string]any{"step_ref": "step-" + tc.state}},
					"recovery_conditions": []any{"verify_postcondition"},
				},
			}}}
			response := composeRecallResponse(input)
			payload := anyMap(recallResponseModuleByKind(t, response, "procedure")["payload"])
			parameters := contextPackAnyList(payload["parameter_bindings"])
			if len(parameters) != 1 || anyToString(anyMap(parameters[0])["value_state"]) != tc.wantState ||
				!recallResponseValidDigest(anyToString(payload["tool_ref"])) {
				t.Fatalf("action state was overclaimed or unbound: %#v", payload)
			}
			codes := map[string]bool{}
			for _, raw := range contextPackAnyList(payload["refusal_conditions"]) {
				codes[anyToString(anyMap(raw)["code"])] = true
			}
			if !codes["independent_authorization_required"] || (tc.wantRefusal != "" && !codes[tc.wantRefusal]) {
				t.Fatalf("action refusal matrix was incomplete: %#v", payload)
			}
			if len(contextPackAnyList(payload["recovery_conditions"])) != 1 ||
				anyToBool(anyMap(response["action_boundary"])["can_act"]) ||
				anyToBool(anyMap(response["action_boundary"])["execution_performed"]) ||
				strings.Contains(recallResponseCanonicalJSON(response), "wrong-private-tool") {
				t.Fatalf("action evidence leaked or minted authority: %#v", response)
			}
		})
	}
}

func TestRecallResponseU11ActionPreparationSelectsRollbackModule(t *testing.T) {
	input := recallResponseTestInput(false)
	input["query"] = "Prepare the next advisory action with rollback conditions"
	input["retrieval_intent"] = "procedure"
	input["task_class"] = "action"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("9", 24), "kind": "runbook", "confidence": 0.95,
		"action_evidence": map[string]any{
			"tool_ref":            "sha256:" + strings.Repeat("c", 64),
			"rollback_conditions": []any{"restore_previous_state"},
		},
	}}}
	response := composeRecallResponse(input)
	payload := anyMap(recallResponseModuleByKind(t, response, "memory_to_action")["payload"])
	if len(contextPackAnyList(payload["rollback_conditions"])) != 1 ||
		anyToString(anyMap(contextPackAnyList(payload["rollback_conditions"])[0])["code"]) != "restore_previous_state" {
		t.Fatalf("action preparation lost rollback evidence: %#v", payload)
	}
}

func TestRecallResponseU11AmbiguousMixedIntentDoesNotGuessExecution(t *testing.T) {
	input := recallResponseTestInput(true)
	input["query"] = "maybe continue the checkpoint or execute the runbook steps and explain why"
	input["task_class"] = "general"
	input["retrieval_intent"] = "decision"
	response := composeRecallResponse(input)
	classification := anyMap(response["classification"])
	jobs := anyToStringList(anyMap(classification["facets"])["jobs"], recallResponseMaxFacetLabels)
	if containsString(jobs, "apply") || containsString(jobs, "act") || !containsString(jobs, "verify") ||
		anyToString(classification["posture"]) == "answer_with_proof" {
		t.Fatalf("ambiguous mixed intent guessed a stronger action state: %#v", classification)
	}
}
