package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func recallResponseServerSilenceFixture(t *testing.T, valueInputs map[string]any) map[string]any {
	t.Helper()
	input := recallResponseTestInput(true)
	input["task_id"] = "task-silence"
	input["task_identity_id"] = "task-identity-silence"
	input["session_id"] = "session-silence"
	input["execution_lane_id"] = "lane-silence"
	input["_u9_protocol"] = map[string]any{
		"proactive_observation": map[string]any{
			"identity_present": true, "policy_allowed": true,
			"value_inputs": valueInputs,
		},
	}
	return input
}

func TestRecallResponseProductionCompositionUsesCompiledServerObservation(t *testing.T) {
	serverObservation := map[string]any{
		"identity_present": true,
		"policy_allowed":   true,
		"value_inputs":     map[string]any{},
	}
	input := contextPackCompilationInput{
		Query:   "compiled server observation",
		Project: "contextlattice", WorkspaceRef: "workspace-ref",
		TopicPath: "responses", TaskClass: "decision",
		RetrievalMode: "balanced", RetrievalIntent: "decision",
		SessionID: "session-compiled", AgentID: "agent-compiled",
		ContextPack: map[string]any{
			"results": []any{}, "facts": []any{},
		},
		// This represents the server retrieval response. It is intentionally
		// absent from ContextPack: the compiler must carry it through the
		// internal artifact field rather than relying on a public pack field.
		SearchResponse: map[string]any{
			"proactive_observation": serverObservation,
		},
		RequestPayload: map[string]any{
			"session_id": "session-compiled", "task_id": "task-compiled",
			"task_identity_id": "identity-compiled", "execution_lane_id": "lane-compiled",
		},
		SourceCoverage: map[string]any{"complete": true},
		GraphQuality:   map[string]any{},
	}
	artifacts := buildContextPackCompilationArtifacts(input)
	if len(artifacts.ServerProactiveObservation) == 0 {
		t.Fatalf("compiled artifacts dropped the server observation: %#v", artifacts)
	}
	request := map[string]any{
		"query": input.Query, "project": input.Project, "topic_path": input.TopicPath,
		"agent_id": input.AgentID, "session_id": input.SessionID,
		"task_id": "task-compiled", "task_identity_id": "identity-compiled",
		"execution_lane_id": "lane-compiled", "retrieval_mode": input.RetrievalMode,
		"retrieval_intent": input.RetrievalIntent, "task_class": input.TaskClass,
	}
	composition := recallResponseCompositionInputFromCompilation(request, input, artifacts, false)
	if _, ok := composition["_u9_protocol"]; ok {
		t.Fatal("production compilation composition unexpectedly used the evaluation protocol")
	}
	if len(anyMap(composition["_server_proactive_observation"])) == 0 {
		t.Fatalf("production composition lost its compiled server observation: %#v", composition)
	}
	response := composeRecallResponse(composition)
	state := anyMap(response["state"])
	if anyToString(anyMap(state["silence"])["reason"]) != "low_utility" || !anyToBool(state["silenced"]) {
		t.Fatalf("compiled server observation did not reach production composition: %#v", state)
	}
	if _, accepted := recallResponseRequestPayload(map[string]any{
		"query": "compiled server observation", "proactive_observation": serverObservation,
		"_server_proactive_observation": serverObservation,
	})["proactive_observation"]; accepted {
		t.Fatal("caller allowlist accepted a server-owned proactive observation")
	}
	if _, accepted := recallResponseRequestPayload(map[string]any{
		"query": "compiled server observation", "_server_proactive_observation": serverObservation,
	})["_server_proactive_observation"]; accepted {
		t.Fatal("caller allowlist accepted the private server observation carrier")
	}
	forgedRequest := recallResponseRequestPayload(map[string]any{
		"query": "caller-forged silence", "_u9_protocol": map[string]any{
			"proactive_observation": map[string]any{"terminal": true, "policy_allowed": false},
		},
	})
	if _, observed := recallResponseServerObservation(forgedRequest); observed {
		t.Fatalf("caller-controlled protocol envelope synthesized a server silence decision: %#v", forgedRequest)
	}
}

func TestRecallResponseHardSilenceSuppressesContextPackPersistence(t *testing.T) {
	s := contextPackPersistenceTestServer(t, true)
	input := contextPackPersistenceTestInput(contextPackLearnedActivationDecision{})
	input.SearchResponse["proactive_observation"] = map[string]any{
		"identity_present": true, "policy_allowed": true,
		"value_inputs": map[string]any{},
	}
	artifacts := buildContextPackCompilationArtifacts(input)
	request := map[string]any{
		"query": input.Query, "project": input.Project, "topic_path": input.TopicPath,
		"agent_id": input.AgentID, "session_id": input.SessionID,
		"task_id": "task-persistence", "task_identity_id": "identity-persistence",
		"execution_lane_id": "lane-persistence", "retrieval_mode": input.RetrievalMode,
		"retrieval_intent": input.RetrievalIntent, "task_class": input.TaskClass,
	}
	calls := []bool{}
	got := s.persistContextPackCompilationOrFallbackWithHook(input, artifacts, func(
		gotInput contextPackCompilationInput,
		gotArtifacts contextPackCompilationArtifacts,
		durable bool,
	) contextPackCompilationArtifacts {
		calls = append(calls, durable)
		response := composeRecallResponse(recallResponseCompositionInputFromCompilation(request, gotInput, gotArtifacts, durable))
		if !recallResponseServerSilenced(response) {
			t.Fatalf("persistence hook did not receive the server silence decision: %#v", response["state"])
		}
		gotArtifacts.SideEffectsSuppressed = true
		return gotArtifacts
	})
	if len(calls) != 1 {
		t.Fatalf("unexpected persistence hook sequence for no-value silence: %v", calls)
	}
	if s.contextPackQuality.sampleCount != 0 || len(s.contextPackQuality.samples) != 0 || len(s.contextPackQuality.durableReceiptSamples) != 0 {
		t.Fatalf("silenced compilation wrote quality state: count=%d samples=%d receipts=%d", s.contextPackQuality.sampleCount, len(s.contextPackQuality.samples), len(s.contextPackQuality.durableReceiptSamples))
	}
	if got.SideEffectsSuppressed != true {
		t.Fatalf("silenced artifacts lost suppression state: %#v", got)
	}
}

func TestRecallResponseRouteUsesServerObservationWithoutRecallSideEffects(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", t.TempDir()+"/quality.ndjson")
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_ENABLED", "false")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			_, _ = w.Write([]byte(`{"results":[],"warnings":[],"proactive_observation":{"identity_present":true,"policy_allowed":true,"value_inputs":{}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	t.Cleanup(gateway.Close)
	rawResp, _, rawSearch := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+"/v1/retrieval/query",
		`{"request":{"query":"raw search must not expose server observation","project":"contextlattice","proactive_observation":{"terminal":true},"_server_proactive_observation":{"terminal":true}}}`, nil)
	if rawResp.StatusCode != http.StatusOK {
		t.Fatalf("raw retrieval failed: status=%d body=%s", rawResp.StatusCode, rawSearch)
	}
	for _, leaked := range []string{"proactive_observation", "_server_proactive_observation", "policy_allowed", "identity_present"} {
		if strings.Contains(rawSearch, leaked) {
			t.Fatalf("raw retrieval exposed or accepted private server observation %q: %s", leaked, rawSearch)
		}
	}
	resp, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath,
		`{"query":"server-derived silence","project":"contextlattice","agent_id":"compiled-route-test","session_id":"compiled-route-session","task_id":"compiled-route-task","task_identity_id":"compiled-route-identity","execution_lane_id":"compiled-route-lane"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server-derived silence route failed: status=%d body=%s", resp.StatusCode, raw)
	}
	state := anyMap(payload["state"])
	if anyToString(anyMap(state["silence"])["reason"]) != "low_utility" || !anyToBool(state["silenced"]) || anyToBool(payload["writeback_required"]) {
		t.Fatalf("server observation did not control the production route: %#v", payload)
	}
	if s.contextPackQuality == nil || s.contextPackQuality.sampleCount != 0 || len(s.contextPackQuality.samples) != 0 {
		t.Fatalf("hard silence wrote context-pack quality state: %#v", s.contextPackQuality)
	}
	if s.tokenImpact == nil || s.tokenImpact.sampleCount != 0 {
		t.Fatalf("hard silence wrote token-impact state: %#v", s.tokenImpact)
	}
	for _, leaked := range []string{"proactive_observation", "policy_allowed", "identity_present"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("server observation leaked through recall response %q: %s", leaked, raw)
		}
	}
}

func TestRecallResponseCompilationPreservesOpaqueServerActionMetadata(t *testing.T) {
	toolDigest := "sha256:" + strings.Repeat("a", 64)
	parameterDigest := "sha256:" + strings.Repeat("b", 64)
	stepDigest := "sha256:" + strings.Repeat("c", 64)
	serverRow := map[string]any{
		"summary": "server-derived action evidence",
		"project": "contextlattice", "source": "server", "topic_path": "runbooks/actions",
		"score": 0.99, "recall_metadata": map[string]any{"action": map[string]any{
			"tool_ref": toolDigest,
			"parameter_bindings": []any{map[string]any{
				"parameter_ref": parameterDigest, "value_state": "bound_redacted", "required": true, "sensitive": true,
				"raw_value": "super-secret-parameter",
			}},
			"instruction":         "execute raw instruction",
			"ordered_steps":       []any{map[string]any{"step_ref": stepDigest}},
			"refusal_conditions":  []any{"external_mutation"},
			"recovery_conditions": []any{"verify_postcondition"},
			"rollback_conditions": []any{"restore_previous_state"},
		}},
	}
	pack := buildContextPackPayload("prepare server action", map[string]any{
		"results": []any{serverRow},
	}, 20, 10)
	packBytes, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret-parameter", "execute raw instruction"} {
		if strings.Contains(string(packBytes), forbidden) {
			t.Fatalf("context-pack compilation leaked raw action metadata %q: %s", forbidden, packBytes)
		}
	}
	allocation := contextPackRankedEvidence("prepare server action", pack, contextPackTokenBudget{})
	if len(allocation.RankedEvidence) != 1 {
		t.Fatalf("server action metadata was not retained by compilation: %#v", allocation.RankedEvidence)
	}
	row := anyMap(allocation.RankedEvidence[0])
	metadata := anyMap(row["recall_metadata"])
	if len(anyMap(metadata["action"])) == 0 {
		t.Fatalf("compiled evidence lost server action metadata: %#v", row)
	}
	input := recallResponseTestInput(false)
	input["retrieval_intent"] = "procedure"
	input["task_class"] = "procedure"
	input["context_pack"] = map[string]any{"ranked_evidence": []any{row}}
	response := composeRecallResponse(input)
	module := recallResponseModuleByKind(t, response, "procedure")
	payload := anyMap(module["payload"])
	proofSet := map[string]bool{}
	for _, raw := range contextPackAnyList(anyMap(anyMap(response["answer"])["proof_spine"])["proof_refs"]) {
		proofSet[anyToString(raw)] = true
	}
	if anyToString(payload["tool_ref"]) != toolDigest {
		t.Fatalf("server tool identity changed during composition: %#v", payload)
	}
	for _, raw := range contextPackAnyList(payload["parameter_bindings"]) {
		if anyToString(anyMap(raw)["parameter_ref"]) != parameterDigest || !proofSet[anyToString(anyMap(raw)["proof_ref"])] {
			t.Fatalf("parameter evidence was not opaque/proof-bound: %#v proof=%#v", raw, proofSet)
		}
	}
	for _, raw := range contextPackAnyList(payload["ordered_steps"]) {
		if anyToString(anyMap(raw)["step_ref"]) != stepDigest || !proofSet[anyToString(anyMap(raw)["proof_ref"])] {
			t.Fatalf("ordered step evidence was not opaque/proof-bound: %#v proof=%#v", raw, proofSet)
		}
	}
	for _, key := range []string{"refusal_conditions", "recovery_conditions", "rollback_conditions"} {
		for _, raw := range contextPackAnyList(payload[key]) {
			if !proofSet[anyToString(anyMap(raw)["proof_ref"])] {
				t.Fatalf("%s was not bound to a selected proof reference: %#v proof=%#v", key, raw, proofSet)
			}
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"server-derived action evidence", "parameter value", "raw_parameter", "raw_value", "identity_present", "policy_allowed"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("compiled action projection leaked raw metadata %q: %s", forbidden, encoded)
		}
	}
	if !validateRecallResponseU2(response) {
		t.Fatalf("compiled action response failed closed validation: %#v", response)
	}
}

func TestRecallResponseU6ServerSilenceDispositionAndLegacyFallback(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(map[string]any)
		wantReason    string
		wantSilenced  bool
		wantWriteback bool
	}{
		{name: "ordinary low utility", mutate: func(_ map[string]any) {}, wantReason: "low_utility", wantSilenced: true, wantWriteback: false},
		{name: "duplicate hard silence", mutate: func(input map[string]any) {
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["duplicate"] = true
		}, wantReason: "duplicate", wantSilenced: true, wantWriteback: false},
		{name: "terminal hard silence", mutate: func(input map[string]any) {
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["terminal"] = true
		}, wantReason: "terminal", wantSilenced: true, wantWriteback: false},
		{name: "incomplete identity hard silence", mutate: func(input map[string]any) {
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["identity_present"] = false
		}, wantReason: "missing_identity", wantSilenced: true, wantWriteback: false},
		{name: "policy suppressed hard silence", mutate: func(input map[string]any) {
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["policy_allowed"] = false
		}, wantReason: "policy_suppressed", wantSilenced: true, wantWriteback: false},
		{name: "high value missing utility proof", mutate: func(input map[string]any) {
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["value_inputs"] = map[string]any{
				"material_new_proof": true, "blocked_next_action": true,
			}
		}, wantReason: "low_utility", wantSilenced: true, wantWriteback: false},
		{name: "high value invalid utility proof", mutate: func(input map[string]any) {
			observation := anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])
			observation["value_inputs"] = map[string]any{
				"material_new_proof": true, "blocked_next_action": true,
				"utility_verified": true, "utility_status": "verified",
			}
			observation["utility_snapshot_ref"] = "not-a-server-digest"
		}, wantReason: "low_utility", wantSilenced: true, wantWriteback: false},
		{name: "high value cannot forge concrete identity", mutate: func(input map[string]any) {
			observation := anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])
			observation["value_inputs"] = map[string]any{
				"material_new_proof": true, "blocked_next_action": true,
				"utility_verified": true, "utility_status": "verified",
			}
			observation["utility_snapshot_ref"] = "sha256:" + strings.Repeat("e", 64)
			delete(input, "task_id")
			delete(input, "task_identity_id")
			delete(input, "execution_lane_id")
		}, wantReason: "missing_identity", wantSilenced: true, wantWriteback: false},
		{name: "high value non-silence", mutate: func(input map[string]any) {
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["value_inputs"] = map[string]any{
				"material_new_proof": true, "blocked_next_action": true,
				"utility_verified": true, "utility_status": "verified",
			}
			anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["utility_snapshot_ref"] = "sha256:" + strings.Repeat("d", 64)
		}, wantReason: "not_silenced", wantSilenced: false, wantWriteback: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := recallResponseServerSilenceFixture(t, map[string]any{})
			test.mutate(input)
			response := composeRecallResponse(input)
			state := anyMap(response["state"])
			silence := anyMap(state["silence"])
			if anyToString(silence["reason"]) != test.wantReason || anyToBool(state["silenced"]) != test.wantSilenced || anyToBool(response["writeback_required"]) != test.wantWriteback {
				t.Fatalf("silence disposition drifted: reason=%#v state=%#v writeback=%v", silence, state, response["writeback_required"])
			}
			if test.wantSilenced {
				if anyToString(anyMap(response["next_action"])["kind"]) != "none" || anyToBool(anyMap(response["action_boundary"])["can_act"]) || anyToBool(anyMap(response["action_boundary"])["execution_performed"]) || anyToBool(anyMap(response["outcome"])["attributable"]) {
					t.Fatalf("silence crossed action/writeback boundary: %#v", response)
				}
			}
			if findings := validateAgentContractPayload(recallResponseContractID, attachPayloadFormatContract(recallResponseContractID, response, "silence-test", "test", "/test/recall-response")); len(findings) != 0 {
				t.Fatalf("silence response failed contract: %#v", findings)
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "policy_allowed") || strings.Contains(string(encoded), "identity_present") {
				t.Fatalf("server observation leaked through recall response: %s", encoded)
			}
		})
	}

	legacy := composeRecallResponse(recallResponseTestInput(true))
	legacyState := anyMap(legacy["state"])
	if _, present := legacyState["silence"]; present || anyToBool(legacyState["silenced"]) || !anyToBool(legacy["writeback_required"]) {
		t.Fatalf("caller without a server observation lost v1 fallback behavior: %#v", legacy)
	}
	if !validateRecallResponseU2(legacy) {
		t.Fatalf("legacy v1 fallback failed nested validation: %#v", legacy)
	}
	malformed := composeRecallResponse(recallResponseServerSilenceFixture(t, map[string]any{}))
	anyMap(anyMap(malformed["state"])["silence"])["unexpected"] = "must-be-rejected"
	if findings := validateAgentContractPayload(recallResponseContractID, malformed); len(findings) == 0 {
		t.Fatal("recall_response.v1 accepted an unknown state.silence field")
	}
}

func TestRecallResponseFallbackRetainsServerHardSilence(t *testing.T) {
	input := recallResponseServerSilenceFixture(t, map[string]any{})
	observation := anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])
	observation["duplicate"] = true
	conflicts := make([]any, 0, 8)
	for index := 0; index < 8; index++ {
		conflicts = append(conflicts, map[string]any{
			"conflict_id": "fallback-silence-conflict-" + anyToString(index),
			"support":     []any{}, "opposition": []any{},
		})
	}
	input["conflicts"] = conflicts
	response := composeRecallResponse(input)
	if !recallResponseIsV1Control(response) {
		t.Fatalf("fixture did not force the v1 fallback: %#v", anyMap(anyMap(response["answer"])["composition"]))
	}
	state := anyMap(response["state"])
	if anyToString(anyMap(state["silence"])["reason"]) != "duplicate" || !anyToBool(state["silenced"]) || anyToBool(response["writeback_required"]) {
		t.Fatalf("v1 fallback discarded the hard silence decision: %#v", response)
	}
	if anyToString(anyMap(response["next_action"])["kind"]) != "none" || anyToBool(anyMap(response["action_boundary"])["can_act"]) || anyToBool(anyMap(response["action_boundary"])["execution_performed"]) {
		t.Fatalf("v1 fallback crossed the hard-silence action boundary: %#v", response)
	}
	finalized := finalizeRecallResponseTransport(cloneJSONMap(response), "silence-fallback", "recall_response", "/test/recall-response")
	if findings := validateAgentContractPayload(recallResponseContractID, finalized); len(findings) != 0 {
		t.Fatalf("silence-preserving fallback failed the closed response contract: %#v", findings)
	}
}

func TestRecallResponseRouteFallbackRetainsServerHardSilence(t *testing.T) {
	input := recallResponseServerSilenceFixture(t, map[string]any{})
	anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])["terminal"] = true
	response := recallResponseProjectFallbackWithServerSilence(input, recallResponseProductionPolicyInput())
	state := anyMap(response["state"])
	if anyToString(anyMap(state["silence"])["reason"]) != "terminal" || !anyToBool(state["silenced"]) || anyToBool(response["writeback_required"]) {
		t.Fatalf("route fallback discarded terminal silence: %#v", response)
	}
	if anyToString(anyMap(response["next_action"])["kind"]) != "none" || anyToBool(anyMap(response["action_boundary"])["can_act"]) {
		t.Fatalf("route fallback crossed terminal action boundary: %#v", response)
	}
	finalized := finalizeRecallResponseTransport(cloneJSONMap(response), "silence-fallback", "recall_response", "/test/recall-response")
	if findings := validateAgentContractPayload(recallResponseContractID, finalized); len(findings) != 0 {
		t.Fatalf("route silence fallback failed the closed response contract: %#v", findings)
	}
}

func TestRecallResponseUtilityProofIsExplicitAndClosed(t *testing.T) {
	input := recallResponseServerSilenceFixture(t, map[string]any{
		"material_new_proof": true, "blocked_next_action": true,
		"utility_verified": true, "utility_status": "verified",
	})
	source := anyMap(anyMap(input["_u9_protocol"])["proactive_observation"])
	source["utility_snapshot_ref"] = "not-a-digest"
	projected := recallResponseProjectServerObservation(source)
	if _, present := projected["utility_snapshot_ref"]; present {
		t.Fatalf("invalid utility reference was upgraded or retained: %#v", projected)
	}
	request := recallResponseSilenceRequest(input, projected)
	observation := recallResponseSilenceObservation(request, projected)
	if observation.UtilityVerified != true || observation.UtilityStatus != "verified" || observation.UtilitySnapshotRef != continuousCognitionUnavailableRef("utility_snapshot") {
		t.Fatalf("utility proof projection did not fail closed: %#v", observation)
	}
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{
		MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70,
	})
	decision := decideContinuousCognitionSilence(request, observation, frontier, false)
	if decision.Reason != "low_utility" || decision.ValueInputs["utility_verified"] != true {
		t.Fatalf("invalid utility proof enabled a non-silence decision: %#v", decision)
	}
}
