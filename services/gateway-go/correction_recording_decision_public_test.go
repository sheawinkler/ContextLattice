package main

import "testing"

func TestManualDecisionPublicOwnerBoundary(t *testing.T) {
	s := correctionTestServer(t)
	s.orchestratorAPIKey = "manual-fixture-key"
	h := buildMux(s)
	write := manualDecisionBody("decision_change", map[string]any{"id": "review", "effect": "set", "expected_version": 0, "value": "selected only"})
	read := manualDecisionBody("decision_context", map[string]any{"id": "review"})
	manualDecisionPost(t, h, "/memory/claims", "", write, 404)
	manualDecisionPost(t, h, "/tools/claim_write", s.orchestratorAPIKey, write, 404)
	manualDecisionPost(t, h, "/memory/claims", s.orchestratorAPIKey, write, 200)
	manualDecisionPost(t, h, "/memory/claims/query", s.orchestratorAPIKey, read, 200)
	for _, change := range []struct{ key, value string }{{"session_id", "missing"}, {"session_id", ""}, {"agent_id", "another"}, {"project", "ALPHA"}, {"topic_path", "release/other"}} {
		for _, c := range []struct {
			path string
			body map[string]any
		}{{"/memory/claims/query", read}, {"/memory/claims", write}} {
			b := cloneJSONMap(c.body)
			b[change.key] = change.value
			// A different topic is a distinct local address. It cannot edit or read the original.
			if change.key == "topic_path" && c.path == "/memory/claims" {
				op := anyMap(b["decision_change"])
				op["expected_version"], op["target_version"], op["effect"] = 1, 1, "replace"
				manualDecisionPost(t, h, c.path, s.orchestratorAPIKey, b, 409)
				continue
			}
			manualDecisionPost(t, h, c.path, s.orchestratorAPIKey, b, 404)
		}
	}
	manualDecisionPost(t, h, "/v1/agents/sessions/start", s.orchestratorAPIKey, map[string]any{"project": correctionTestProject, "agent": "codex", "agent_id": "second", "session_id": "second-session"}, 200)
	other := cloneJSONMap(read)
	other["agent_id"], other["session_id"] = "second", "second-session"
	manualDecisionPost(t, h, "/memory/claims/query", s.orchestratorAPIKey, other, 404)
	for _, bad := range []map[string]any{{"status": "completed"}, {"scope_ownership": map[string]any{"kind": "foreign"}}, {"scopeOwnership": map[string]any{}}} {
		s.agentSessions.mu.Lock()
		original := s.agentSessions.sessions[correctionTestSession]
		row := cloneJSONMap(original)
		for k, v := range bad {
			row[k] = v
		}
		s.agentSessions.sessions[correctionTestSession] = row
		s.agentSessions.mu.Unlock()
		manualDecisionPost(t, h, "/memory/claims/query", s.orchestratorAPIKey, read, 404)
		manualDecisionPost(t, h, "/memory/claims", s.orchestratorAPIKey, write, 404)
		s.agentSessions.mu.Lock()
		s.agentSessions.sessions[correctionTestSession] = original
		s.agentSessions.mu.Unlock()
	}
	bad := cloneJSONMap(write)
	anyMap(bad["decision_change"])["effect"] = "adopt"
	manualDecisionPost(t, h, "/memory/claims", s.orchestratorAPIKey, bad, 422)
	if s.temporalClaims.correctionStamp.Seq != 1 {
		t.Fatal("denied operation created a receipt")
	}
}
