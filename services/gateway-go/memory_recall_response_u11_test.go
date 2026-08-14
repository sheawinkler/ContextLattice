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
	var witnessOK bool
	policy, witnessOK = recallResponseBindSyntheticAblationWitness(input, policy)
	if !witnessOK {
		t.Fatal("safety ablation did not bind its exact finalized baseline witness")
	}
	response := composeRecallResponseWithPolicy(input, policy)
	if recallResponseIsV1Control(response) {
		composition := anyMap(anyMap(response["answer"])["composition"])
		witness := anyMap(recallResponseDisclosure(response)["ablation_witness"])
		if anyToString(composition["fallback_reason"]) != "synthetic_ablation" ||
			!recallResponseAblationWitnessValid(response, witness) || anyToString(witness["status"]) != "accepted_control" {
			t.Fatalf("safety ablation returned an untyped product failure: %#v", response)
		}
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

func TestRecallResponseU11SensitiveActionReadinessMatrixFailsClosed(t *testing.T) {
	toolRef := "sha256:" + strings.Repeat("a", 64)
	parameterRef := "sha256:" + strings.Repeat("b", 64)
	for _, required := range []bool{true, false} {
		t.Run(map[bool]string{true: "required", false: "optional"}[required], func(t *testing.T) {
			metadata := map[string]any{
				"tool_ref": toolRef,
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": parameterRef, "value_state": "sensitive_unresolved",
					"required": required, "sensitive": true,
				}},
				"refusal_conditions":  []any{"sensitive_value_unavailable"},
				"recovery_conditions": []any{"verify_postcondition"},
			}
			if recallResponseActionMetadataReady(metadata) {
				t.Fatal("sensitive unresolved parameter became metadata-ready")
			}
			payload := map[string]any{
				"intended_tool_ref": toolRef,
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": parameterRef, "value_state": "sensitive_unresolved",
					"required": required, "sensitive": true,
				}},
				"refusal_conditions":  []any{map[string]any{"code": "sensitive_value_unavailable"}},
				"rollback_conditions": []any{map[string]any{"code": "restore_previous_state"}},
			}
			if recallResponseActionPayloadReady(payload) {
				t.Fatal("sensitive unresolved parameter became payload-ready")
			}
			projected := recallResponseProjectActionMetadata(map[string]any{
				"candidate_id": "sensitive-readiness",
				"action_evidence": map[string]any{
					"tool_ref": toolRef,
					"parameter_bindings": []any{map[string]any{
						"parameter_ref": parameterRef, "value_state": "resolved",
						"required": required, "sensitive": true,
					}},
					"recovery_conditions": []any{"verify_postcondition"},
				},
			})
			parameters := contextPackAnyList(projected["parameter_bindings"])
			if len(parameters) != 1 || anyToString(anyMap(parameters[0])["value_state"]) != "sensitive_unresolved" ||
				anyToString(anyMap(parameters[0])["value_state"]) == "bound_redacted" {
				t.Fatalf("sensitive parameter was represented as bound: %#v", projected)
			}
			refusals := contextPackAnyList(projected["refusal_conditions"])
			foundRefusal := false
			for _, raw := range refusals {
				if anyToString(raw) == "sensitive_value_unavailable" {
					foundRefusal = true
				}
			}
			if !foundRefusal {
				t.Fatalf("typed sensitive refusal was not preserved: %#v", projected)
			}
		})
	}

	input := recallResponseTestInput(false)
	input["retrieval_intent"] = "action"
	input["task_class"] = "action"
	input["source_coverage"] = map[string]any{"complete": true}
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("a", 24), "kind": "runbook", "confidence": 0.91,
		"status": "current", "action_evidence": map[string]any{
			"tool_ref": toolRef,
			"parameter_bindings": []any{map[string]any{
				"parameter_ref": parameterRef, "value_state": "sensitive_unresolved",
				"required": true, "sensitive": true,
			}},
			"recovery_conditions": []any{"verify_postcondition"},
		},
	}}}
	response := composeRecallResponse(input)
	if recallResponseIsV1Control(response) || !anyToBool(response["ok"]) {
		t.Fatalf("sensitive status did not remain a valid product response: %#v", response)
	}
	module := recallResponseModuleByKind(t, response, "memory_to_action")
	payload := anyMap(module["payload"])
	if !anyToBool(module["primary"]) || anyToString(payload["intended_tool_ref"]) != "unresolved_tool" ||
		len(contextPackAnyList(payload["parameter_bindings"])) != 0 || len(contextPackAnyList(payload["ordered_steps"])) != 0 ||
		len(contextPackAnyList(payload["rollback_conditions"])) != 0 || recallResponseActionPayloadReady(payload) {
		t.Fatalf("sensitive capability status carried executable action material: %#v", module)
	}
	refusalCodes := map[string]bool{}
	refusalProof := ""
	for _, raw := range contextPackAnyList(payload["refusal_conditions"]) {
		row := anyMap(raw)
		refusalCodes[anyToString(row["code"])] = true
		if refusalProof == "" {
			refusalProof = anyToString(row["proof_ref"])
		} else if anyToString(row["proof_ref"]) != refusalProof {
			t.Fatalf("sensitive status refusals did not bind one witness: %#v", payload)
		}
	}
	if len(refusalCodes) != 2 || !refusalCodes["independent_authorization_required"] ||
		!refusalCodes["sensitive_value_unavailable"] || refusalProof == "" {
		t.Fatalf("sensitive status lost its closed refusal semantics: %#v", payload)
	}
	recallResponseModuleByKind(t, response, "negative_abstention")
	proofSet := map[string]bool{}
	for _, raw := range contextPackAnyList(module["proof_refs"]) {
		proofSet[anyToString(raw)] = true
	}
	if !recallResponseActionPayloadValid(payload, proofSet, true) {
		t.Fatalf("sensitive status payload did not satisfy the closed validator: %#v", payload)
	}
	if anyToBool(anyMap(response["action_boundary"])["can_act"]) ||
		anyToBool(anyMap(response["action_boundary"])["execution_performed"]) {
		t.Fatalf("sensitive unresolved response acquired authority: %#v", response["action_boundary"])
	}
}

func TestRecallResponseU11SensitiveUnavailableStatusFailsClosedOnConflictOrHardExclusion(t *testing.T) {
	toolRef := func(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }
	parameterRef := "sha256:" + strings.Repeat("f", 64)
	actionRow := func(candidateID, tool string) map[string]any {
		return map[string]any{
			"candidate_id": candidateID, "kind": "runbook", "confidence": 0.91,
			"status": "current", "state": "current", "support": "context", "content": "same advisory action",
			"action_evidence": map[string]any{
				"tool_ref": tool,
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": parameterRef, "value_state": "sensitive_unresolved",
					"required": true, "sensitive": true,
				}},
				"refusal_conditions": []any{"sensitive_value_unavailable"},
			},
		}
	}
	compose := func(rows []any) map[string]any {
		input := recallResponseTestInput(false)
		input["retrieval_intent"] = "action"
		input["task_class"] = "action"
		input["source_coverage"] = map[string]any{"complete": true}
		input["context_pack"] = map[string]any{"ranked_evidence": rows}
		return composeRecallResponse(input)
	}
	assertClosed := func(t *testing.T, response map[string]any) {
		t.Helper()
		if recallResponseIsV1Control(response) || !anyToBool(response["ok"]) {
			t.Fatalf("conflict/exclusion did not retain a typed product response: %#v", response)
		}
		for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
			if anyToString(anyMap(raw)["kind"]) == "memory_to_action" {
				t.Fatalf("conflicting or excluded sensitive action gained module membership: %#v", raw)
			}
		}
		recallResponseModuleByKind(t, response, "negative_abstention")
		if anyToBool(anyMap(response["action_boundary"])["can_act"]) ||
			anyToBool(anyMap(response["action_boundary"])["execution_performed"]) {
			t.Fatalf("conflicting or excluded action acquired authority: %#v", response["action_boundary"])
		}
	}

	t.Run("conflicting eligible unavailable payloads", func(t *testing.T) {
		response := compose([]any{
			actionRow("rtc_"+strings.Repeat("b", 24), toolRef("1")),
			actionRow("rtc_"+strings.Repeat("c", 24), toolRef("2")),
		})
		assertClosed(t, response)
	})

	t.Run("same identity hard exclusion", func(t *testing.T) {
		eligible := actionRow("rtc_"+strings.Repeat("d", 24), toolRef("3"))
		excluded := cloneJSONMap(eligible)
		excluded["source"] = "excluded-copy"
		excluded["state"] = "unknown"
		excluded["status"] = "unknown"
		excluded["support"] = "distractor"
		if _, ok := recallResponseSensitiveUnavailableActionStatus([]any{eligible, excluded}); ok {
			t.Fatal("same-identity hard exclusion did not veto sensitive status")
		}
		merged := mergeRowsAll(map[string][]map[string]any{
			"eligible": {eligible},
			"excluded": {excluded},
		})
		if len(merged) != 1 || merged[0]["action_evidence"] != nil || anyToString(merged[0]["status"]) != "unknown" {
			t.Fatalf("production duplicate merge did not preserve hard exclusion: %#v", merged)
		}
		assertClosed(t, compose([]any{merged[0]}))
	})
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
