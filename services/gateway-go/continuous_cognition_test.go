package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func continuousCognitionTestRequest(t *testing.T) continuousCognitionRequest {
	t.Helper()
	request, err := normalizeContinuousCognitionRequest(map[string]any{
		"operation":         "observe",
		"query":             "verify the current bounded frontier",
		"project":           "contextlattice",
		"workspace_ref":     "workspace-test",
		"topic_path":        "response-intelligence",
		"retrieval_intent":  "decision",
		"retrieval_mode":    "balanced",
		"agent_id":          "codex_gpt5_test",
		"session_id":        "session-test",
		"task_id":           "task-test",
		"task_identity_id":  "identity-test",
		"execution_lane_id": "lane-test",
		"cycle_ref":         "cycle-test",
		"objective_id":      "objective-test",
		"limit":             32,
		"token_budget":      4096,
		"as_of":             "2026-08-08T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("normalize request: %v", err)
	}
	return request
}

func continuousCognitionTestObservation() continuousCognitionObservation {
	scope := continuousCognitionScope{
		ScopeDigest:      "sha256:" + strings.Repeat("1", 64),
		QueryDigest:      "sha256:" + strings.Repeat("2", 64),
		WorkspaceRef:     "ref_workspace_111111111111111111111111",
		ProjectRef:       "ref_project_222222222222222222222222",
		TopicRef:         "ref_topic_333333333333333333333333",
		AgentRef:         "ref_agent_444444444444444444444444",
		SessionRef:       "ref_session_555555555555555555555555",
		TaskRef:          "ref_task_666666666666666666666666",
		TaskIdentityRef:  "ref_task_identity_777777777777777777",
		ExecutionLaneRef: "ref_execution_lane_888888888888888888",
		RetrievalIntent:  "verification",
		CycleRef:         "cycle_999999999999999999999999",
	}
	return continuousCognitionObservation{
		Scope:              scope,
		ObjectiveGraphRef:  "ref_objective_graph_aaaaaaaaaaaaaaaaaaaaaaaa",
		ObjectiveState:     "active",
		ObjectiveAvailable: true,
		SessionRollupRef:   "ref_session_rollup_bbbbbbbbbbbbbbbbbbbbbbbb",
		SessionPresent:     true,
		ContinuityZeroRef:  "ref_continuity_zero_unavailable",
		ProofTimelineRef:   "ref_proof_timeline_cccccccccccccccccccccccc",
		ProofStatus:        "verified",
		ProofComplete:      true,
		RetrievalPlanRef:   "ref_retrieval_plan_dddddddddddddddddddddddd",
		UtilitySnapshotRef: "ref_utility_snapshot_eeeeeeeeeeeeeeeeeeeeeeee",
		UtilityStatus:      "contextual_unverified",
		UtilityVerified:    false,
		UtilityScore:       0.75,
		ExpectedUtility:    continuousCognitionExpectedUtility{ActionChangeProbability: 1, ConsequenceIfWrong: 0.9, EvidenceReliability: 0.9, Score: 0.99},
		SourceAnchorDigest: "sha256:" + strings.Repeat("f", 64),
		SourceComplete:     true,
		Gaps:               []continuousCognitionGap{},
	}
}

func TestContinuousCognitionRequestRejectsUnboundedOrUnknownFields(t *testing.T) {
	base := map[string]any{
		"operation": "observe",
		"query":     "bounded query",
		"project":   "contextlattice",
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown field": func(payload map[string]any) {
			payload["raw_prompt"] = "must not enter the kernel"
		},
		"unbounded query": func(payload map[string]any) {
			payload["query"] = strings.Repeat("q", continuousCognitionMaxQueryBytes+1)
		},
		"invalid operation": func(payload map[string]any) {
			payload["operation"] = "activate"
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{}
			for key, value := range base {
				payload[key] = value
			}
			mutate(payload)
			if _, err := normalizeContinuousCognitionRequest(payload); err == nil {
				t.Fatalf("unsafe request unexpectedly normalized: %#v", payload)
			}
		})
	}
}

func TestContinuousCognitionRequestNormalizationIsBoundedAndCanonical(t *testing.T) {
	request, err := normalizeContinuousCognitionRequest(map[string]any{
		"operation":        " OBSERVE ",
		"query":            "bounded query",
		"project":          " contextlattice ",
		"retrieval_intent": "caller-invented-intent",
		"retrieval_mode":   "caller-invented-mode",
	})
	if err != nil {
		t.Fatalf("normalize canonical request: %v", err)
	}
	if request.Operation != continuousCognitionOperation {
		t.Fatalf("operation was not lower-cased: %q", request.Operation)
	}
	if request.Project != "contextlattice" {
		t.Fatalf("project was not sanitized: %q", request.Project)
	}
	if request.RetrievalIntent != "decision" {
		t.Fatalf("arbitrary retrieval intent escaped normalization: %q", request.RetrievalIntent)
	}
	if request.RetrievalMode != "balanced" {
		t.Fatalf("arbitrary retrieval mode escaped normalization: %q", request.RetrievalMode)
	}
}

func TestContinuousCognitionScopeAndPayloadRenormalizeInternalIntent(t *testing.T) {
	request := continuousCognitionTestRequest(t)
	request.RetrievalIntent = "caller-invented-intent"
	request.RetrievalMode = "caller-invented-mode"
	scope := continuousCognitionScopeFromRequest(request)
	if scope.RetrievalIntent != "decision" {
		t.Fatalf("scope emitted an unbounded retrieval intent: %#v", scope)
	}
	observation := continuousCognitionTestObservation()
	observation.Scope.RetrievalIntent = "caller-invented-intent"
	payload := buildContinuousCognitionSemanticPayload(request, observation, computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{}))
	if got := anyToString(anyMap(payload["request_scope"])["retrieval_intent"]); got != "decision" {
		t.Fatalf("payload emitted an unbounded retrieval intent: %q", got)
	}
}

func TestContinuousCognitionUnknownFieldErrorIsDeterministicAndOpaque(t *testing.T) {
	base := map[string]any{"operation": "observe", "query": "bounded query", "project": "contextlattice"}
	first := cloneAnyMap(base)
	first["caller_secret_field_z"] = "do not echo"
	second := cloneAnyMap(base)
	second["caller_secret_field_a"] = "do not echo either"
	_, firstErr := normalizeContinuousCognitionRequest(first)
	_, secondErr := normalizeContinuousCognitionRequest(second)
	if firstErr == nil || secondErr == nil {
		t.Fatalf("unknown fields unexpectedly normalized: first=%v second=%v", firstErr, secondErr)
	}
	if firstErr.Error() != secondErr.Error() {
		t.Fatalf("unknown-field errors are not deterministic: %q != %q", firstErr, secondErr)
	}
	for _, raw := range []string{"caller_secret_field_z", "caller_secret_field_a", "do not echo"} {
		if strings.Contains(firstErr.Error(), raw) || strings.Contains(secondErr.Error(), raw) {
			t.Fatalf("unknown-field error echoed caller data %q: first=%q second=%q", raw, firstErr, secondErr)
		}
	}
}

func TestContinuousCognitionExpectedUtilityScoreIgnoresSuppliedScore(t *testing.T) {
	value := continuousCognitionExpectedUtility{
		ActionChangeProbability: 1,
		ConsequenceIfWrong:      0.8,
		EvidenceReliability:     0.5,
		AcquisitionCost:         0.1,
		Score:                   0.99,
	}
	normalized := continuousCognitionExpectedUtilityValue(value)
	if normalized.Score != 0.3 {
		t.Fatalf("score used caller-supplied value instead of computed utility: %#v", normalized)
	}
	value.Score = -100
	if second := continuousCognitionExpectedUtilityValue(value); second.Score != normalized.Score {
		t.Fatalf("score changed when only supplied score changed: first=%#v second=%#v", normalized, second)
	}
}

func TestContinuousCognitionFrontierDeterministicGates(t *testing.T) {
	policy := continuousCognitionFrontierPolicy{
		MaxRounds:                3,
		InvestigateThreshold:     0.55,
		ContinueThreshold:        0.70,
		ConsequenceHighThreshold: 0.70,
	}
	cases := []struct {
		name     string
		mutate   func(*continuousCognitionObservation)
		decision string
	}{
		{name: "terminal retires", mutate: func(observation *continuousCognitionObservation) {
			observation.ObjectiveState = "completed"
		}, decision: "retire"},
		{name: "missing session abstains", mutate: func(observation *continuousCognitionObservation) {
			observation.SessionPresent = false
		}, decision: "abstain"},
		{name: "incomplete proof investigates", mutate: func(observation *continuousCognitionObservation) {
			observation.ProofComplete = false
			observation.SourceComplete = false
		}, decision: "investigate"},
		{name: "complete verified evidence continues", mutate: func(observation *continuousCognitionObservation) {
			observation.UtilityVerified = true
		}, decision: "continue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observation := continuousCognitionTestObservation()
			tc.mutate(&observation)
			first := computeContinuousCognitionFrontier(observation, policy)
			second := computeContinuousCognitionFrontier(observation, policy)
			if first.Decision != tc.decision {
				t.Fatalf("decision=%q, want %q: %#v", first.Decision, tc.decision, first)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("frontier is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
			}
		})
	}
}

func TestContinuousCognitionSemanticPayloadIsDeterministicAndClosed(t *testing.T) {
	request := continuousCognitionTestRequest(t)
	observation := continuousCognitionTestObservation()
	policy := continuousCognitionFrontierPolicy{MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70}
	frontier := computeContinuousCognitionFrontier(observation, policy)
	first := buildContinuousCognitionSemanticPayload(request, observation, frontier)
	second := buildContinuousCognitionSemanticPayload(request, observation, frontier)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("semantic payload is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if _, ok := first["format_contract"]; ok {
		t.Fatal("pure semantic builder must not attach a contract or mutate contract telemetry")
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal semantic payload: %v", err)
	}
	if len(encoded) == 0 || len(encoded) > 96*1024 {
		t.Fatalf("semantic payload is unexpectedly bounded: %d bytes", len(encoded))
	}
	for _, forbidden := range []string{"verify the current bounded frontier", "contextlattice", "response-intelligence"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("raw request value leaked into semantic payload: %q", forbidden)
		}
	}
	if anyToString(first["cognition_digest"]) == "" || !strings.HasPrefix(anyToString(first["cognition_digest"]), "sha256:") {
		t.Fatalf("missing deterministic cognition digest: %#v", first["cognition_digest"])
	}
}

func TestContinuousCognitionContractValidationPassesAndRejectsRawFields(t *testing.T) {
	request := continuousCognitionTestRequest(t)
	observation := continuousCognitionTestObservation()
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70})
	valid := attachPayloadFormatContract(
		continuousCognitionContractID,
		buildContinuousCognitionSemanticPayload(request, observation, frontier),
		"codex_gpt5_test",
		"test",
		"/test/continuous-cognition",
	)
	if findings := validateAgentContractPayload(continuousCognitionContractID, valid); len(findings) != 0 {
		t.Fatalf("valid continuous cognition payload failed: %#v", findings)
	}
	format := anyMap(valid["format_contract"])
	if status := anyToString(anyMap(format["validation"])["status"]); status != "passed" {
		t.Fatalf("contract attachment did not pass validation: %#v", format)
	}

	bad := cloneContractMap(valid)
	anyMap(bad["observation"])["query"] = "raw query must be rejected"
	findings := validateAgentContractPayload(continuousCognitionContractID, bad)
	if len(findings) == 0 {
		t.Fatal("raw query field unexpectedly passed continuous cognition validation")
	}
	foundForbidden := false
	for _, finding := range findings {
		if anyToString(finding["reason"]) == "forbidden_field_present" {
			foundForbidden = true
			break
		}
	}
	if !foundForbidden {
		t.Fatalf("expected forbidden-field finding, got %#v", findings)
	}
}

func TestContinuousCognitionSnapshotRequiresExplicitAsOf(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	request := continuousCognitionTestRequest(t)
	request.AsOf = time.Time{}
	observation := snapshotContinuousCognition(s, request, time.Time{})
	if observation.SourceComplete {
		t.Fatalf("zero as-of unexpectedly produced a complete snapshot: %#v", observation)
	}
	found := false
	for _, gap := range observation.Gaps {
		if gap.Code == "as_of_required" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing explicit as_of gap: %#v", observation.Gaps)
	}
}

func TestContinuousCognitionSnapshotBindsEffectiveAsOfIntoScope(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	request := continuousCognitionTestRequest(t)
	request.AsOf = time.Time{}
	effectiveAsOf := time.Date(2026, time.August, 8, 12, 0, 0, 123000000, time.UTC)
	observation := snapshotContinuousCognition(s, request, effectiveAsOf)
	boundRequest := request
	boundRequest.AsOf = effectiveAsOf
	wantScope := continuousCognitionScopeFromRequest(boundRequest)
	if observation.Scope.QueryDigest != wantScope.QueryDigest || observation.Scope.ScopeDigest != wantScope.ScopeDigest {
		t.Fatalf("effective as_of was not bound into scope: got=%#v want=%#v", observation.Scope, wantScope)
	}
	zeroScope := continuousCognitionScopeFromRequest(request)
	if observation.Scope.ScopeDigest == zeroScope.ScopeDigest || observation.Scope.QueryDigest == zeroScope.QueryDigest {
		t.Fatalf("effective as_of did not change scope digests: effective=%#v zero=%#v", observation.Scope, zeroScope)
	}
}

func TestContinuousCognitionSourceCompletenessIgnoresNonmaterialGaps(t *testing.T) {
	observation := continuousCognitionTestObservation()
	observation.Gaps = []continuousCognitionGap{{Code: "optional_retrieval_missing", Source: "retrieval_plan", Material: false}}
	if !continuousCognitionSourceIsComplete(observation) {
		t.Fatalf("nonmaterial gap incorrectly made source incomplete: %#v", observation)
	}
	observation.Gaps = append(observation.Gaps, continuousCognitionGap{Code: "proof_missing", Source: "proof_timeline", Material: true})
	if continuousCognitionSourceIsComplete(observation) {
		t.Fatal("material gap incorrectly ignored by source completeness")
	}
}

func TestContinuousCognitionObjectiveProjectionSortsNodesAndEdgesByStableIDs(t *testing.T) {
	graph := map[string]any{
		"ok": true, "complete": true, "graph_truncated": false,
		"node_count": 2, "edge_count": 2, "transition_count": 0,
		"nodes": []objectiveGraphNode{
			{ObjectiveID: "objective-z", Status: "active"},
			{ObjectiveID: "objective-a", Status: "active"},
		},
		"edges": []objectiveGraphEdge{
			{EdgeID: "edge-z", FromID: "objective-z", ToID: "objective-a", Type: "parent_of"},
			{EdgeID: "edge-a", FromID: "objective-a", ToID: "objective-z", Type: "depends_on"},
		},
	}
	_, firstRef, firstAvailable, firstComplete := continuousCognitionObjectiveProjection(graph, "objective-a")
	graph["nodes"] = []objectiveGraphNode{graph["nodes"].([]objectiveGraphNode)[1], graph["nodes"].([]objectiveGraphNode)[0]}
	graph["edges"] = []objectiveGraphEdge{graph["edges"].([]objectiveGraphEdge)[1], graph["edges"].([]objectiveGraphEdge)[0]}
	_, secondRef, secondAvailable, secondComplete := continuousCognitionObjectiveProjection(graph, "objective-a")
	if !firstAvailable || !secondAvailable || !firstComplete || !secondComplete || firstRef != secondRef {
		t.Fatalf("objective projection was not stable under source ordering: first=%q second=%q available=%t/%t complete=%t/%t", firstRef, secondRef, firstAvailable, secondAvailable, firstComplete, secondComplete)
	}
}

func TestContinuousCognitionProofRequiresRetainedMaterial(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	session := map[string]any{"id": "empty-proof-session", "event_count": 0}
	_, status, complete, _ := continuousCognitionProofProjection(s, session, nil)
	if complete || status != "degraded" {
		t.Fatalf("empty proof stores were treated as verified material: status=%q complete=%t", status, complete)
	}
}

func TestContinuousCognitionCompositeSourceAnchorBindsAllProjectionRefsAndGaps(t *testing.T) {
	observation := continuousCognitionTestObservation()
	observation.ProofAnchorDigest = "sha256:" + strings.Repeat("a", 64)
	observation.Gaps = []continuousCognitionGap{{Code: "optional_gap", Source: "retrieval_plan", Material: false}}
	first := continuousCognitionCompositeSourceAnchorDigest(observation)
	observation.RetrievalPlanRef = "ref_retrieval_plan_changed"
	second := continuousCognitionCompositeSourceAnchorDigest(observation)
	if first == second {
		t.Fatal("composite source anchor digest ignored a projection reference")
	}
	observation.RetrievalPlanRef = continuousCognitionTestObservation().RetrievalPlanRef
	observation.Gaps = append(observation.Gaps, continuousCognitionGap{Code: "material_gap", Source: "proof_timeline", Material: true})
	third := continuousCognitionCompositeSourceAnchorDigest(observation)
	if first == third {
		t.Fatal("composite source anchor digest ignored normalized gaps")
	}
}

func TestContinuousCognitionUtilityCohortContextCannotAuthorizeCurrentCycle(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	s.utility.mu.Lock()
	s.utility.applyLocked(map[string]any{
		"schema_id": utilityObservationContractID, "outcome_id": "cohort-context-row",
		"project": "contextlattice", "task_class": "agent_workflow", "retrieval_intent": "decision",
		"captured_at": "2026-08-08T11:00:00Z", "revision": 1,
	})
	s.utility.mu.Unlock()
	request := continuousCognitionTestRequest(t)
	ref, status, verified, score, expected, available := continuousCognitionUtilityProjection(s, request)
	if ref == "" || !available || status != "contextual_unverified" || verified || score != 0 || expected.EvidenceReliability != 0 {
		t.Fatalf("cohort context authorized current utility unexpectedly: ref=%q status=%q verified=%t score=%v expected=%#v available=%t", ref, status, verified, score, expected, available)
	}
	observation := continuousCognitionTestObservation()
	observation.UtilityVerified = verified
	observation.ExpectedUtility = expected
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70})
	if frontier.Decision == "continue" {
		t.Fatalf("cohort context enabled continue: %#v", frontier)
	}
}

func TestContinuousCognitionSemanticPayloadKeepsLifecycleInertAndGoverned(t *testing.T) {
	request := continuousCognitionTestRequest(t)
	observation := continuousCognitionTestObservation()
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70})
	payload := buildContinuousCognitionSemanticPayload(request, observation, frontier)
	activation := anyMap(payload["activation"])
	outcome := anyMap(payload["outcome"])
	evaluation := anyMap(payload["evaluation"])
	if anyToString(activation["execution_owner"]) != "external_cli_worker" || !anyToBool(activation["one_shot"]) {
		t.Fatalf("activation authority/ownership is not exact: %#v", activation)
	}
	if anyToString(outcome["proof_ref"]) != continuousCognitionUnavailableRef("proof") || anyToString(outcome["utility_observation_ref"]) != continuousCognitionUnavailableRef("utility_observation") {
		t.Fatalf("inert lifecycle leaked current-cycle refs: %#v", outcome)
	}
	if anyToString(evaluation["utility_status"]) != "not_evaluated" || anyToBool(evaluation["verified"]) {
		t.Fatalf("inert lifecycle was evaluated: %#v", evaluation)
	}
	if findings := validateAgentContractPayload(continuousCognitionContractID, attachPayloadFormatContract(continuousCognitionContractID, payload, "codex_gpt5_test", "test", "/test/continuous-cognition")); len(findings) != 0 {
		t.Fatalf("governed inert payload failed contract: %#v", findings)
	}
	bad := cloneContractMap(attachPayloadFormatContract(continuousCognitionContractID, payload, "codex_gpt5_test", "test", "/test/continuous-cognition"))
	anyMap(bad["activation"])["one_shot"] = false
	found := false
	for _, finding := range validateAgentContractPayload(continuousCognitionContractID, bad) {
		if anyToString(finding["reason"]) == "required_true_path_not_true" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("contract did not enforce activation.one_shot: %#v", bad["activation"])
	}
}

func TestContinuousCognitionSnapshotIsReadOnlyAtFixedAsOf(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	request := continuousCognitionTestRequest(t)
	request.SessionID = "missing-session-for-read-only-test"
	request.AsOf = time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	stateDigest := func() string {
		s.agentSessions.mu.Lock()
		sessions := map[string]any{}
		for id, session := range s.agentSessions.sessions {
			sessions[id] = cloneAnyMap(session)
		}
		events := map[string]any{}
		for id, rows := range s.agentSessions.events {
			events[id] = cloneMapSlice(rows)
		}
		s.agentSessions.mu.Unlock()
		s.utility.mu.Lock()
		utilityRows := cloneMapSlice(s.utility.observations)
		s.utility.mu.Unlock()
		return frontierT6Digest(map[string]any{
			"sessions": sessions, "events": events, "utility_rows": utilityRows,
			"contract_telemetry": agentContractTelemetrySnapshot(),
		})
	}
	before := stateDigest()
	first := snapshotContinuousCognition(s, request, request.AsOf)
	second := snapshotContinuousCognition(s, request, request.AsOf)
	after := stateDigest()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("fixed-as-of snapshot is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if before != after {
		t.Fatalf("snapshot mutated authoritative or contract-telemetry state: before=%s after=%s", before, after)
	}
}

func TestAdaptiveRetrievalPlanAtPreservesDefaultBehaviorAndState(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	payload := map[string]any{"project": "contextlattice", "query": "debug a bounded retrieval regression", "token_budget": 5000}
	stateDigest := func() string {
		s.agentSessions.mu.Lock()
		sessions := map[string]any{}
		for id, session := range s.agentSessions.sessions {
			sessions[id] = cloneAnyMap(session)
		}
		events := map[string]any{}
		for id, rows := range s.agentSessions.events {
			events[id] = cloneMapSlice(rows)
		}
		s.agentSessions.mu.Unlock()
		s.utility.mu.Lock()
		utilityRows := cloneMapSlice(s.utility.observations)
		s.utility.mu.Unlock()
		return frontierT6Digest(map[string]any{
			"sessions": sessions, "events": events, "utility_rows": utilityRows,
			"contract_telemetry": agentContractTelemetrySnapshot(),
		})
	}
	before := stateDigest()
	defaultPlan := s.buildAdaptiveRetrievalPlan(payload)
	fixedPlan := s.buildAdaptiveRetrievalPlanAt(payload, anyToString(defaultPlan["generated_at"]))
	delete(defaultPlan, "generated_at")
	delete(fixedPlan, "generated_at")
	if !reflect.DeepEqual(defaultPlan, fixedPlan) {
		t.Fatalf("default planner behavior changed beyond generated_at: default=%#v fixed=%#v", defaultPlan, fixedPlan)
	}
	if after := stateDigest(); before != after {
		t.Fatalf("adaptive planner mutated state or contract telemetry: before=%s after=%s", before, after)
	}
}
