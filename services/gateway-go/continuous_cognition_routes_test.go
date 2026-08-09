package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func continuousCognitionMemoryStore(entries ...memoryStoreEntry) *memoryStore {
	store := &memoryStore{
		policy:       memoryStorePolicy{enabled: true, maxRecent: 6000},
		recent:       make([]memoryStoreEntry, 0, len(entries)),
		currentState: map[string]memoryCurrentState{},
	}
	for _, entry := range entries {
		store.recent = append(store.recent, entry)
		key := memoryStoreKey(entry.Project, entry.FileName)
		candidate := memoryCurrentStateFromEntry(entry)
		if current, exists := store.currentState[key]; !exists || memoryCurrentStateSupersedes(candidate, current) {
			store.currentState[key] = candidate
		}
	}
	store.ready.Store(true)
	return store
}

func continuousCognitionMemoryDigest(store *memoryStore) string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return frontierT6Digest(map[string]any{
		"recent":        append([]memoryStoreEntry(nil), store.recent...),
		"current_state": store.currentState,
	})
}

func continuousCognitionInvestigationRequest(t *testing.T, operation string) continuousCognitionRequest {
	t.Helper()
	request, err := normalizeContinuousCognitionRequest(map[string]any{
		"operation":         operation,
		"query":             "needle-cc-investigation",
		"project":           "cc-route-private",
		"topic_path":        "response/intelligence",
		"retrieval_intent":  "decision",
		"retrieval_mode":    "balanced",
		"agent_id":          "agent-private-cc",
		"session_id":        "session-private-cc",
		"objective_id":      "objective-private-cc",
		"task_id":           "task-private-cc",
		"task_identity_id":  "identity-private-cc",
		"execution_lane_id": "lane-private-cc",
		"workspace_ref":     "workspace-private-cc",
		"limit":             8,
		"token_budget":      4096,
		"as_of":             "2026-08-08T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("normalize investigation request: %v", err)
	}
	return request
}

func continuousCognitionInvestigationFixtureStore() *memoryStore {
	return continuousCognitionMemoryStore(
		memoryStoreEntry{EventID: "evt-old", Project: "cc-route-private", FileName: "notes/superseded.md", TopicPath: "response/intelligence", Summary: "needle-cc-investigation stale", CreatedAt: "2026-08-08T09:00:00Z"},
		memoryStoreEntry{EventID: "evt-current", Project: "cc-route-private", FileName: "notes/superseded.md", TopicPath: "response/intelligence", Summary: "current authority without the matching phrase", CreatedAt: "2026-08-08T10:00:00Z"},
		memoryStoreEntry{EventID: "evt-valid", Project: "cc-route-private", FileName: "notes/valid.md", TopicPath: "response/intelligence", Summary: "needle-cc-investigation verified", ContentHash: strings.Repeat("a", 64), CreatedAt: "2026-08-08T11:00:00Z"},
		memoryStoreEntry{EventID: "evt-deleted", Project: "cc-route-private", FileName: "notes/deleted.md", TopicPath: "response/intelligence", Summary: "needle-cc-investigation deleted", DataClass: "memory_tombstone", CreatedAt: "2026-08-08T11:01:00Z"},
		memoryStoreEntry{EventID: "evt-future", Project: "cc-route-private", FileName: "notes/future.md", TopicPath: "response/intelligence", Summary: "needle-cc-investigation future", CreatedAt: "2026-08-08T13:00:00Z"},
		memoryStoreEntry{EventID: "evt-ephemeral", Project: "cc-route-private", FileName: "notes/ephemeral.md", TopicPath: "test/private", Summary: "needle-cc-investigation ephemeral", Lifecycle: "ephemeral", CreatedAt: "2026-08-08T11:02:00Z"},
		memoryStoreEntry{EventID: "evt-other", Project: "other-private-project", FileName: "notes/other.md", TopicPath: "response/intelligence", Summary: "needle-cc-investigation other", CreatedAt: "2026-08-08T11:03:00Z"},
	)
}

func TestContinuousCognitionInvestigationIsDeterministicAuthoritativeAndReadOnly(t *testing.T) {
	store := continuousCognitionInvestigationFixtureStore()
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate)
	before := continuousCognitionMemoryDigest(store)
	first, firstGaps := investigateContinuousCognitionLocalMemory(context.Background(), store, request)
	second, secondGaps := investigateContinuousCognitionLocalMemory(context.Background(), store, request)
	after := continuousCognitionMemoryDigest(store)

	if before != after {
		t.Fatalf("read-only investigation mutated the memory store: before=%s after=%s", before, after)
	}
	if first != second || frontierT6Digest(firstGaps) != frontierT6Digest(secondGaps) {
		t.Fatalf("investigation is not deterministic: first=%#v/%#v second=%#v/%#v", first, firstGaps, second, secondGaps)
	}
	if first.State != "completed" || !first.ExecutionPerformed || first.RetrievalCount != 1 || first.CompilerCount != 0 || first.NetworkCalls != 0 {
		t.Fatalf("investigation accounting is not exact: %#v", first)
	}
	if !first.MutationsSuppressed || !first.SourceComplete || first.Truncated || first.EvidenceRefCount != 1 || len(firstGaps) != 0 {
		t.Fatalf("authoritative filtering did not retain exactly one current bounded result: result=%#v gaps=%#v", first, firstGaps)
	}
	if !strings.HasPrefix(first.ContextPackRef, "ref_context_pack_") || !strings.HasPrefix(first.RetrievalReceiptRef, "ref_retrieval_receipt_") {
		t.Fatalf("investigation did not return opaque proof refs: %#v", first)
	}
}

func TestContinuousCognitionInvestigationReplaysPreBoundarySupersessionAndTombstone(t *testing.T) {
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate)
	store := continuousCognitionMemoryStore(
		memoryStoreEntry{EventID: "evt-before-update", Project: request.Project, FileName: "notes/pre-boundary-update.md", TopicPath: request.TopicPath, Summary: request.Query, CreatedAt: "2026-08-08T11:00:00Z"},
		memoryStoreEntry{EventID: "evt-future-update", Project: request.Project, FileName: "notes/pre-boundary-update.md", TopicPath: request.TopicPath, Summary: "future replacement", CreatedAt: "2026-08-08T13:00:00Z"},
		memoryStoreEntry{EventID: "evt-before-delete", Project: request.Project, FileName: "notes/pre-boundary-delete.md", TopicPath: request.TopicPath, Summary: request.Query, CreatedAt: "2026-08-08T11:05:00Z"},
		memoryStoreEntry{EventID: "evt-future-delete", Project: request.Project, FileName: "notes/pre-boundary-delete.md", TopicPath: request.TopicPath, DataClass: "memory_tombstone", CreatedAt: "2026-08-08T13:05:00Z"},
	)
	result, gaps := investigateContinuousCognitionLocalMemory(context.Background(), store, request)
	if result.State != "completed" || result.EvidenceRefCount != 2 || !result.SourceComplete || result.Truncated || len(gaps) != 0 {
		t.Fatalf("post-boundary changes hid authoritative pre-boundary evidence: result=%#v gaps=%#v", result, gaps)
	}
}

func TestContinuousCognitionInvestigationFailsClosedWithoutAsOfOrStore(t *testing.T) {
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate)
	request.AsOf = time.Time{}
	result, gaps := investigateContinuousCognitionLocalMemory(context.Background(), continuousCognitionInvestigationFixtureStore(), request)
	if result.ExecutionPerformed || result.RetrievalCount != 0 || len(gaps) != 1 || gaps[0].Code != "investigation_as_of_required" {
		t.Fatalf("missing as_of did not fail closed before retrieval: result=%#v gaps=%#v", result, gaps)
	}

	request = continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate)
	result, gaps = investigateContinuousCognitionLocalMemory(context.Background(), nil, request)
	if result.ExecutionPerformed || result.RetrievalCount != 0 || len(gaps) != 1 || gaps[0].Code != "investigation_memory_unavailable" {
		t.Fatalf("missing store falsely claimed execution: result=%#v gaps=%#v", result, gaps)
	}
}

func TestContinuousCognitionInvestigationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, gaps := investigateContinuousCognitionLocalMemory(ctx, continuousCognitionInvestigationFixtureStore(), continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate))
	if result.State != "degraded" || !result.ExecutionPerformed || result.RetrievalCount != 1 || len(gaps) != 1 || gaps[0].Code != "investigation_cancelled" {
		t.Fatalf("cancelled bounded scan was not surfaced honestly: result=%#v gaps=%#v", result, gaps)
	}
}

func TestContinuousCognitionInvestigationBindsSourceAnchorAndContract(t *testing.T) {
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate)
	observation := continuousCognitionTestObservation()
	before := continuousCognitionCompositeSourceAnchorDigest(observation)
	investigation, gaps := investigateContinuousCognitionLocalMemory(context.Background(), continuousCognitionInvestigationFixtureStore(), request)
	applyContinuousCognitionInvestigation(&observation, investigation, gaps)
	if observation.SourceAnchorDigest == before {
		t.Fatal("investigation refs did not alter the composite source anchor")
	}
	frontier := computeContinuousCognitionFrontier(observation, continuousCognitionFrontierPolicy{MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70})
	payload := buildContinuousCognitionSemanticPayloadWithInvestigation(request, observation, frontier, investigation)
	payload = finalizeContinuousCognitionTransport(payload, request.AgentID, "test", "/test/continuous-cognition")
	if findings := validateAgentContractPayload(continuousCognitionContractID, payload); len(findings) != 0 {
		t.Fatalf("executed investigation failed the continuous cognition contract: %#v", findings)
	}
	projected := anyMap(payload["investigation"])
	coverage := anyMap(projected["source_coverage"])
	if !anyToBool(projected["execution_performed"]) || anyToInt(coverage["retrieval_count"], -1) != 1 || anyToInt(coverage["compiler_count"], -1) != 0 || anyToString(coverage["learned_ranking_state"]) != "control_shadow_only" {
		t.Fatalf("executed investigation projection is not honest: %#v", projected)
	}
}

func continuousCognitionServe(t *testing.T, handler http.Handler, method, path string, body []byte, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	payload := map[string]any{}
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	}
	return recorder, payload
}

func TestContinuousCognitionRoutesAreNativeClosedAndRetrievalBounded(t *testing.T) {
	var backendCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "backend must not be called"})
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	s.memoryStore = continuousCognitionInvestigationFixtureStore()
	handler := buildMux(s)
	backendCalls.Store(0)

	registered := map[string]bool{}
	for _, path := range strictRuntimeRequiredNativeRoutePaths() {
		registered[path] = true
	}
	owned := map[string]nativeOwnedRoute{}
	for _, route := range strictRuntimeOwnedRoutes() {
		owned[route.Path] = route
	}
	boundaries := map[string]contextBoundarySurface{}
	for _, surface := range contextBoundaryRequiredSurfaces() {
		boundaries[surface.Path] = surface
	}
	for _, path := range []string{memoryContinuousCognitionPath, toolsContinuousCognitionPath} {
		if !registered[path] || owned[path].Owner != sourceOwnerGoNative || !owned[path].Required || boundaries[path].ContractID != continuousCognitionContractID {
			t.Fatalf("continuous cognition native inventory is incomplete for %s: owned=%#v boundary=%#v", path, owned[path], boundaries[path])
		}
		_, pattern := buildNativeMux(&server{}).Handler(httptest.NewRequest(http.MethodPost, path, nil))
		if pattern != path {
			t.Fatalf("continuous cognition path is not exact native ownership: path=%s pattern=%s", path, pattern)
		}
	}

	for _, path := range []string{memoryContinuousCognitionPath, toolsContinuousCognitionPath} {
		for _, operation := range []string{
			continuousCognitionOperationObserve, continuousCognitionOperationStatus, continuousCognitionOperationInvestigate,
			continuousCognitionOperationOutcome, continuousCognitionOperationEvaluate,
			continuousCognitionOperationRollback, continuousCognitionOperationRetire,
		} {
			request := continuousCognitionInvestigationRequest(t, operation)
			body, err := json.Marshal(map[string]any{
				"operation": request.Operation, "query": request.Query, "project": request.Project,
				"topic_path": request.TopicPath, "agent_id": request.AgentID, "session_id": request.SessionID,
				"objective_id": request.ObjectiveID, "task_id": request.TaskID, "task_identity_id": request.TaskIdentityID,
				"execution_lane_id": request.ExecutionLaneID, "workspace_ref": request.WorkspaceRef,
				"retrieval_intent": request.RetrievalIntent, "retrieval_mode": request.RetrievalMode,
				"limit": request.Limit, "token_budget": request.TokenBudget, "as_of": request.AsOf.Format(time.RFC3339Nano),
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			recorder, payload := continuousCognitionServe(t, handler, http.MethodPost, path, body, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s %s returned %d: %s", path, operation, recorder.Code, recorder.Body.String())
			}
			if anyToString(payload["operation"]) != operation || anyToString(payload["schema_id"]) != continuousCognitionContractID {
				t.Fatalf("%s %s returned the wrong projection: %#v", path, operation, payload)
			}
			investigation := anyMap(payload["investigation"])
			coverage := anyMap(investigation["source_coverage"])
			wantRetrievals := 0
			wantExecuted := false
			wantRound := 0
			if operation == continuousCognitionOperationInvestigate {
				wantRetrievals = 1
				wantExecuted = true
				wantRound = 1
			}
			if anyToInt(coverage["retrieval_count"], -1) != wantRetrievals || anyToInt(coverage["compiler_count"], -1) != 0 || anyToBool(investigation["execution_performed"]) != wantExecuted {
				t.Fatalf("%s %s violated operation accounting: %#v", path, operation, investigation)
			}
			progress := anyMap(payload["progress"])
			if anyToInt(progress["round"], -1) != wantRound || anyToInt(progress["max_rounds"], -1) != 3 {
				t.Fatalf("%s %s violated bounded round accounting: %#v", path, operation, progress)
			}
			wantStage := map[string]string{
				continuousCognitionOperationObserve: "silence", continuousCognitionOperationStatus: "status",
				continuousCognitionOperationInvestigate: "silence", continuousCognitionOperationOutcome: "outcome",
				continuousCognitionOperationEvaluate: "evaluation", continuousCognitionOperationRollback: "rollback",
				continuousCognitionOperationRetire: "retirement",
			}[operation]
			if anyToString(progress["stage"]) != wantStage {
				t.Fatalf("%s %s returned the wrong lifecycle stage: got=%q want=%q progress=%#v", path, operation, progress["stage"], wantStage, progress)
			}
			if wantStage == "silence" {
				if anyToString(payload["decision"]) != "silence" || anyToString(payload["next_action"]) != "none" || anyToBool(payload["writeback_required"]) {
					t.Fatalf("%s %s did not close the low-utility action boundary: %#v", path, operation, payload)
				}
			} else if anyToString(payload["next_action"]) == "none" || !anyToBool(payload["writeback_required"]) {
				t.Fatalf("%s %s lost its non-silent action/writeback boundary: %#v", path, operation, payload)
			}
			if findings := validateAgentContractPayload(continuousCognitionContractID, payload); len(findings) != 0 {
				t.Fatalf("%s %s failed contract validation: %#v", path, operation, findings)
			}
			encoded := recorder.Body.String()
			for _, raw := range []string{request.Query, request.Project, request.TopicPath, request.AgentID, request.SessionID, request.ObjectiveID, "notes/valid.md", "needle-cc-investigation verified"} {
				if strings.Contains(encoded, raw) {
					t.Fatalf("%s %s leaked raw request or memory material %q: %s", path, operation, raw, encoded)
				}
			}
		}
	}
	if calls := backendCalls.Load(); calls != 0 {
		t.Fatalf("continuous cognition called a backend %d time(s)", calls)
	}
}

func TestContinuousCognitionHardSilenceHasNoDispatchMutationOrWriteback(t *testing.T) {
	var backendCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalls.Add(1)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "hard silence dispatched"})
	}))
	defer backend.Close()
	qualityPath := t.TempDir() + "/quality.ndjson"
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", qualityPath)
	s := newTestServer(t, backend.URL)
	s.memoryStore = continuousCognitionInvestigationFixtureStore()
	handler := buildMux(s)
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationInvestigate)
	body, err := json.Marshal(map[string]any{
		"operation": request.Operation, "query": request.Query, "project": request.Project,
		"topic_path": request.TopicPath, "agent_id": request.AgentID, "session_id": request.SessionID,
		"objective_id": request.ObjectiveID, "task_id": request.TaskID,
		// task_identity_id is deliberately absent: this is a hard-silence predicate.
		"execution_lane_id": request.ExecutionLaneID, "retrieval_intent": request.RetrievalIntent,
		"retrieval_mode": request.RetrievalMode, "limit": request.Limit, "token_budget": request.TokenBudget,
		"as_of": request.AsOf.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.memoryStore.mu.RLock()
	beforeRecent, beforeCurrent, beforeEdges := len(s.memoryStore.recent), len(s.memoryStore.currentState), len(s.memoryStore.edgeOrder)
	s.memoryStore.mu.RUnlock()
	ledgerSize := func() int64 {
		info, statErr := os.Stat(qualityPath)
		if statErr != nil {
			return 0
		}
		return info.Size()
	}
	beforeLedger := ledgerSize()
	recorder, payload := continuousCognitionServe(t, handler, http.MethodPost, memoryContinuousCognitionPath, body, nil)
	if recorder.Code != http.StatusOK || anyToString(payload["decision"]) != "silence" ||
		anyToString(anyMap(payload["silence"])["reason"]) != "missing_identity" {
		t.Fatalf("missing identity did not hard-silence: status=%d payload=%#v", recorder.Code, payload)
	}
	investigation := anyMap(payload["investigation"])
	coverage := anyMap(investigation["source_coverage"])
	if backendCalls.Load() != 0 || anyToBool(investigation["execution_performed"]) ||
		anyToInt(coverage["retrieval_count"], -1) != 0 || anyToInt(coverage["compiler_count"], -1) != 0 ||
		anyToBool(payload["writeback_required"]) || anyToString(payload["next_action"]) != "none" {
		t.Fatalf("hard silence emitted dispatch/writeback evidence: backend=%d payload=%#v", backendCalls.Load(), payload)
	}
	s.memoryStore.mu.RLock()
	afterRecent, afterCurrent, afterEdges := len(s.memoryStore.recent), len(s.memoryStore.currentState), len(s.memoryStore.edgeOrder)
	s.memoryStore.mu.RUnlock()
	if beforeRecent != afterRecent || beforeCurrent != afterCurrent || beforeEdges != afterEdges || beforeLedger != ledgerSize() {
		t.Fatalf("hard silence mutated durable/local state: memory=%d/%d %d/%d %d/%d ledger=%d/%d",
			beforeRecent, afterRecent, beforeCurrent, afterCurrent, beforeEdges, afterEdges, beforeLedger, ledgerSize())
	}
}

func TestContinuousCognitionRouteUsesOwnerWorkspaceInsteadOfCallerHint(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	s.memoryStore = continuousCognitionInvestigationFixtureStore()
	authorization, err := s.frontierT6OwnerAuthorization(nil, frontierT6ProactiveContextPrepFeatureID, "status")
	if err != nil || !authorization.Authorized || strings.TrimSpace(authorization.WorkspaceID) == "" {
		t.Fatalf("derive owner workspace authorization: authorization=%#v err=%v", authorization, err)
	}
	handler := buildMux(s)
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationStatus)
	serve := func(callerWorkspace string) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"operation": request.Operation, "query": request.Query, "project": request.Project,
			"topic_path": request.TopicPath, "agent_id": request.AgentID, "session_id": request.SessionID,
			"objective_id": request.ObjectiveID, "task_id": request.TaskID, "task_identity_id": request.TaskIdentityID,
			"execution_lane_id": request.ExecutionLaneID, "workspace_ref": callerWorkspace,
			"retrieval_intent": request.RetrievalIntent, "retrieval_mode": request.RetrievalMode,
			"limit": request.Limit, "token_budget": request.TokenBudget, "as_of": request.AsOf.Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatalf("marshal caller workspace fixture: %v", err)
		}
		recorder, payload := continuousCognitionServe(t, handler, http.MethodPost, memoryContinuousCognitionPath, body, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("owner-authorized cognition route returned %d: %s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), callerWorkspace) {
			t.Fatalf("caller workspace hint leaked into response: %s", recorder.Body.String())
		}
		return anyMap(payload["request_scope"])
	}
	first := serve("caller-workspace-a")
	second := serve("caller-workspace-b")
	wantWorkspaceRef := continuousCognitionOpaqueRef(
		"workspace", contextPackLearnedScopeRef("workspace", authorization.WorkspaceID),
	)
	if anyToString(first["workspace_ref"]) != wantWorkspaceRef ||
		anyToString(second["workspace_ref"]) != wantWorkspaceRef ||
		anyToString(first["scope_digest"]) != anyToString(second["scope_digest"]) {
		t.Fatalf("caller workspace changed the owner-scoped projection: first=%#v second=%#v want=%q", first, second, wantWorkspaceRef)
	}
}

func TestResponseIntelligenceRoutesRejectOversizedBodiesBeforeParsing(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	handler := buildMux(s)
	body := append([]byte(`{"query":"`), bytes.Repeat([]byte("x"), responseIntelligenceMaxRequestBytes)...)
	body = append(body, []byte(`"}`)...)
	for _, path := range []string{memoryContinuousCognitionPath, toolsContinuousCognitionPath, memoryRecallResponsePath, toolsRecallResponsePath} {
		recorder, payload := continuousCognitionServe(t, handler, http.MethodPost, path, body, nil)
		if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(anyToString(payload["error"]), "bounded response-intelligence") {
			t.Fatalf("%s did not reject its oversized body at the ingress boundary: status=%d payload=%#v", path, recorder.Code, payload)
		}
	}
}

func TestContinuousCognitionRouteHidesCrossProjectSessionIdentity(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationStatus)
	s.agentSessions.mu.Lock()
	s.agentSessions.sessions[request.SessionID] = map[string]any{
		"id": request.SessionID, "project": "another-project", "agent_id": request.AgentID,
		"status": "active", "started_at": "2026-08-08T10:00:00Z", "updated_at": "2026-08-08T11:00:00Z",
		"scope_ownership": newAgentSessionPublicScopeOwnership("another-project"),
	}
	s.agentSessions.events[request.SessionID] = []map[string]any{}
	s.agentSessions.mu.Unlock()
	body, err := json.Marshal(map[string]any{
		"operation": request.Operation, "query": request.Query, "project": request.Project,
		"topic_path": request.TopicPath, "agent_id": request.AgentID, "session_id": request.SessionID,
		"objective_id": request.ObjectiveID, "task_id": request.TaskID, "task_identity_id": request.TaskIdentityID,
		"execution_lane_id": request.ExecutionLaneID, "retrieval_intent": request.RetrievalIntent,
		"retrieval_mode": request.RetrievalMode, "limit": request.Limit, "token_budget": request.TokenBudget,
		"as_of": request.AsOf.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder, payload := continuousCognitionServe(t, buildMux(s), http.MethodPost, memoryContinuousCognitionPath, body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("cross-project session projection returned %d: %s", recorder.Code, recorder.Body.String())
	}
	found := false
	for _, raw := range contextPackAnyList(payload["gaps"]) {
		if anyToString(anyMap(raw)["code"]) == "session_not_found" {
			found = true
		}
	}
	if !found || strings.Contains(recorder.Body.String(), "another-project") {
		t.Fatalf("cross-project session existence was not rendered ambiguous: %s", recorder.Body.String())
	}
}

func TestContinuousCognitionRouteProjectsExactOneShotActivationWithoutMutation(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	if s.frontierT6 != nil {
		s.frontierT6.close()
	}
	s.frontierT6, _ = frontierT6TestStore(t, frontierT6StoreLimits{MaxPreps: 8})
	s.memoryStore = continuousCognitionInvestigationFixtureStore()
	request := continuousCognitionInvestigationRequest(t, continuousCognitionOperationStatus)
	authorization, err := s.frontierT6OwnerAuthorization(nil, frontierT6ProactiveContextPrepFeatureID, "status")
	if err != nil {
		t.Fatalf("derive owner authorization: %v", err)
	}
	scope := frontierT6Scope{
		WorkspaceID: authorization.WorkspaceID,
		Project:     request.Project,
		SessionID:   request.SessionID,
		AgentID:     request.AgentID,
	}
	scheduledAt := request.AsOf.Add(-10 * time.Minute)
	prepRequest := frontierT6TestPrepRequest(scope, scheduledAt, request.TaskID, "bounded_investigation", true)
	prepRequest.Approval.ScopeDigest = frontierT6ScopeDigest(scope)
	prepRequest.Approval.AuthorizationDigest = authorization.AuthorizationDigest
	prepRequest.Provenance.AuthorizationDigest = authorization.AuthorizationDigest
	scheduled, err := s.frontierT6.scheduleContextPrep(prepRequest, scheduledAt)
	if err != nil || scheduled.Prep == nil {
		t.Fatalf("schedule cognition activation: result=%#v err=%v", scheduled, err)
	}
	claim, found, err := s.frontierT6.claimContextPrep(scope, scheduled.Prep.PrepID, "worker_cc", scheduledAt.Add(time.Minute))
	if err != nil || !found {
		t.Fatalf("claim cognition activation: claim=%#v found=%v err=%v", claim, found, err)
	}
	completedAt := scheduledAt.Add(90 * time.Second)
	ready, err := s.frontierT6.completeContextPrep(scope, claim.Prep.PrepID, claim.ClaimToken, frontierT6TestPrepArtifact(claim.Prep, completedAt), completedAt)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("complete cognition activation: prep=%#v err=%v", ready, err)
	}

	body, err := json.Marshal(map[string]any{
		"operation": request.Operation, "query": request.Query, "project": request.Project,
		"topic_path": request.TopicPath, "agent_id": request.AgentID, "session_id": request.SessionID,
		"objective_id": request.ObjectiveID, "task_id": request.TaskID, "task_identity_id": request.TaskIdentityID,
		"execution_lane_id": request.ExecutionLaneID, "workspace_ref": "caller_workspace_is_not_authority",
		"retrieval_intent": request.RetrievalIntent, "retrieval_mode": request.RetrievalMode,
		"limit": request.Limit, "token_budget": request.TokenBudget, "as_of": request.AsOf.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeFile, err := os.ReadFile(s.frontierT6.path)
	if err != nil {
		t.Fatalf("read T6 state before cognition status: %v", err)
	}
	beforeHash := s.frontierT6.state.StateHash
	recorder, payload := continuousCognitionServe(t, buildMux(s), http.MethodPost, memoryContinuousCognitionPath, body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("continuous cognition activation status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	activation := anyMap(payload["activation"])
	progress := anyMap(payload["progress"])
	loopGuard := anyMap(progress["loop_guard"])
	prepRef := anyToString(activation["prep_id"])
	if anyToString(activation["state"]) != "ready" || !strings.HasPrefix(prepRef, "ref_prep_") || prepRef == scheduled.Prep.PrepID || !anyToBool(activation["one_shot"]) || !anyToBool(activation["requires_explicit_cli_use"]) || anyToBool(activation["gateway_execution_performed"]) {
		t.Fatalf("ready activation projection is not exact: %#v", activation)
	}
	if anyToString(progress["status"]) != "activation_ready" || anyToString(progress["stage"]) != "activation" || !anyToBool(loopGuard["persisted"]) || anyToInt(progress["round"], -1) != 0 || anyToInt(progress["max_rounds"], -1) != 3 {
		t.Fatalf("activation progress or loop guard is not bounded: progress=%#v", progress)
	}
	if strings.Contains(recorder.Body.String(), scheduled.Prep.PrepID) || strings.Contains(recorder.Body.String(), request.Query) {
		t.Fatalf("continuous cognition leaked raw activation or query identity: %s", recorder.Body.String())
	}
	afterFile, err := os.ReadFile(s.frontierT6.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeFile, afterFile) || beforeHash != s.frontierT6.state.StateHash {
		t.Fatal("continuous cognition status mutated the T6 preparation store")
	}
	if findings := validateAgentContractPayload(continuousCognitionContractID, payload); len(findings) != 0 {
		t.Fatalf("ready activation projection failed contract: %#v", findings)
	}
	changedAuthorization := projectContinuousCognitionActivation(
		s.frontierT6,
		authorization.WorkspaceID,
		frontierT6Digest(map[string]any{"authorization": "rotated"}),
		request,
		request.AsOf,
	)
	if changedAuthorization.State != "authorization_changed" || !changedAuthorization.Persisted || changedAuthorization.ProjectionRef == "" {
		t.Fatalf("stale authorization was projected as usable: %#v", changedAuthorization)
	}

	consumedAt := scheduledAt.Add(2 * time.Minute)
	use, err := s.frontierT6.useContextPrep(scope, ready.PrepID, ready.TaskID, ready.EffectiveProfileDigest, ready.SourceGeneration, ready.AuthorizationDigest, consumedAt)
	if err != nil || !use.Eligible {
		t.Fatalf("consume cognition activation: result=%#v err=%v", use, err)
	}
	_, consumedPayload := continuousCognitionServe(t, buildMux(s), http.MethodPost, memoryContinuousCognitionPath, body, nil)
	consumedActivation := anyMap(consumedPayload["activation"])
	consumedProgress := anyMap(consumedPayload["progress"])
	if anyToString(consumedActivation["state"]) != "consumed" || !strings.HasPrefix(anyToString(consumedActivation["consumption_ref"]), "ref_consumption_") || anyToString(consumedProgress["status"]) != "activation_consumed" || anyToString(payload["cognition_digest"]) == anyToString(consumedPayload["cognition_digest"]) {
		t.Fatalf("consumed activation was not reflected exactly: activation=%#v progress=%#v", consumedActivation, consumedProgress)
	}

	temporal := projectContinuousCognitionActivation(s.frontierT6, authorization.WorkspaceID, authorization.AuthorizationDigest, request, consumedAt.Add(-time.Second))
	if temporal.State != "temporal_projection_unavailable" || !temporal.Persisted {
		t.Fatalf("historically unreconstructable activation was overstated: %#v", temporal)
	}
	wrongSession := request
	wrongSession.SessionID = "session_other"
	if absent := projectContinuousCognitionActivation(s.frontierT6, authorization.WorkspaceID, authorization.AuthorizationDigest, wrongSession, request.AsOf); absent.State != "absent" || absent.Persisted {
		t.Fatalf("activation leaked across session scope: %#v", absent)
	}
}

func TestContinuousCognitionRoutesRejectInvalidIngressWithoutEcho(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	handler := buildMux(s)

	recorder, _ := continuousCognitionServe(t, handler, http.MethodGet, memoryContinuousCognitionPath, nil, nil)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("wrong method was not rejected exactly: code=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
	recorder, _ = continuousCognitionServe(t, handler, http.MethodPost, memoryContinuousCognitionPath, []byte(`{"operation":`), nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, body := range [][]byte{
		[]byte(`{"operation":"activate","query":"safe","project":"safe"}`),
		[]byte(`{"operation":"observe","query":"safe","project":"safe","caller_secret_field":"must-not-echo"}`),
	} {
		recorder, _ = continuousCognitionServe(t, handler, http.MethodPost, memoryContinuousCognitionPath, body, nil)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid request status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "caller_secret_field") || strings.Contains(recorder.Body.String(), "must-not-echo") {
			t.Fatalf("invalid request echoed caller data: %s", recorder.Body.String())
		}
	}
}

func TestContinuousCognitionRoutesEnforceHTTPAndToolAuthentication(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	s.orchestratorAPIKey = "cc-auth-key"
	s.toolCalls.requireAPIKey = true
	handler := buildMux(s)
	body := []byte(`{"operation":"observe","query":"safe","project":"safe","as_of":"2026-08-08T12:00:00Z"}`)

	recorder, _ := continuousCognitionServe(t, handler, http.MethodPost, memoryContinuousCognitionPath, body, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("trusted HTTP ingress without an explicit key did not preserve existing auth semantics: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, path := range []string{memoryContinuousCognitionPath, toolsContinuousCognitionPath} {
		recorder, _ = continuousCognitionServe(t, handler, http.MethodPost, path, body, map[string]string{"X-Api-Key": "wrong"})
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted a wrong API key: code=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
		recorder, _ = continuousCognitionServe(t, handler, http.MethodPost, path, body, map[string]string{"X-Api-Key": "cc-auth-key"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s rejected the configured API key: code=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder, _ = continuousCognitionServe(t, handler, http.MethodPost, toolsContinuousCognitionPath, body, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tool route accepted a missing required API key: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
