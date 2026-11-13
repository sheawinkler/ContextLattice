package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func agentDecisionResumeFixture(t *testing.T, mutations ...func(map[string]any)) (*server, *memoryStore, string) {
	t.Helper()
	continuity, owner, payload := decisionApplicabilityFixture(t)
	for _, mutate := range mutations {
		mutate(payload)
	}
	change, _, err := continuity.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner))
	if err != nil {
		t.Fatal(err)
	}
	selected := &agentDecisionBinding{SchemaID: agentDecisionBindingSchemaID, Project: "alpha", ObjectiveID: change.ObjectiveID,
		TaskIdentityID: change.TaskIdentityID, DecisionChangeID: change.DecisionChangeID, AfterDecisionID: change.After.DecisionID,
		Repo: "alpha-repo", Branch: "main"}
	start := map[string]any{"session_id": "sess_resume", "project": "alpha", "agent_id": "test", "repo": "alpha-repo", "branch": "main",
		"task_identity_id": change.TaskIdentityID, "objective": "Verify the deployment", "decision_binding": agentDecisionJSONMap(selected)}
	binding, err := continuity.resolveAgentDecisionBinding(selected, start)
	if err != nil {
		t.Fatal(err)
	}
	store := &agentSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), maxKeep: 16, maxEvents: 16, idleTTL: time.Hour,
		sessions: map[string]map[string]any{}, events: map[string][]map[string]any{}, order: []string{}}
	if _, _, err := store.startOrReuseDecisionScoped(start, nil, binding); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.appendEvent("sess_resume", map[string]any{"type": "checkpoint.written", "summary": "Historical deployment intent.",
		"metadata": map[string]any{"next_action": "Deploy the pinned build immediately"}}); err != nil {
		t.Fatal(err)
	}
	s := &server{agentSessions: store, continuity: continuity, memoryStore: owner}
	s.retrieval.enabled = true
	return s, owner, "sess_resume"
}

func decisionResumeResponse(t *testing.T, s *server, sessionID string, compact bool) map[string]any {
	t.Helper()
	url := "/v1/agents/sessions/" + sessionID + "/context-package"
	if compact {
		url += "?view=compact"
	}
	recorder := httptest.NewRecorder()
	s.agentsSessionContextPackage(recorder, httptest.NewRequest(http.MethodGet, url, nil), sessionID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("context package status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestAgentDecisionResumeUsesOriginalOwnerPins(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "compact"}[compact], func(t *testing.T) {
			s, owner, sessionID := agentDecisionResumeFixture(t)
			projection := func(response map[string]any) map[string]any {
				if compact {
					return anyMap(anyMap(response["provenance"])["decision_continuity"])
				}
				return anyMap(response["decision_continuity"])
			}
			positive := projection(decisionResumeResponse(t, s, sessionID, compact))
			if positive["status"] != "verified_match" {
				t.Fatalf("owner-pinned applicability absent or not checked: %#v", positive)
			}
			seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Corrected deployment acceptance.", "test", "session")
			response := decisionResumeResponse(t, s, sessionID, compact)
			negative := projection(response)
			if negative["status"] != "changed" || negative["record_digest"] != positive["record_digest"] {
				t.Fatal("source correction refreshed pins or erased selected decision identity")
			}
			var action string
			if compact {
				action = anyToString(anyMap(response["session_rollup"])["next_action"])
			} else {
				action = anyToString(anyMap(response["context_package"])["next_action"])
			}
			if action == "" || action == "Deploy the pinned build immediately" {
				t.Fatal("changed original acceptance owner left stale action available")
			}
		})
	}
}
