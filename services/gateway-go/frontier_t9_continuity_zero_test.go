package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const frontierT9TestCommit = "0123456789abcdef0123456789abcdef01234567"

func frontierT9TestServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(root, "sessions.json"))
	t.Setenv(frontierT6StatePathEnv, filepath.Join(root, "agent-fit.json"))
	t.Setenv("CONTEXTLATTICE_CONTEXT_PASSPORT_PATH", filepath.Join(root, "passports.ndjson"))
	t.Setenv("CONTEXTLATTICE_CONTEXT_IDENTITY_PATH", filepath.Join(root, "identity.json"))
	t.Setenv("CONTEXTLATTICE_CONTEXT_MESH_STATE_PATH", filepath.Join(root, "mesh.json"))
	s := newTestServer(t, "")
	return s, buildNativeMux(s)
}

func frontierT9SeedSession(t *testing.T, s *server, id, status, commit string) {
	t.Helper()
	_, _, err := s.agentSessions.startOrReuse(map[string]any{
		"session_id": id, "agent": "codex", "agent_id": "codex_gpt5",
		"project": "contextlattice", "status": status,
		"objective":       "Finish the release using /Volumes/private-work/evidence without leaking paths.",
		"objective_state": "implementation", "next_action": "Inspect /Users/example/private and run the focused gate.",
		"repo": "/workspace/ContextLattice", "branch": "main",
		"task_id": "task-frontier-t9",
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	_, _, err = s.agentSessions.appendEvent(id, map[string]any{
		"type": "memory.checkpoint", "summary": "Checkpointed /Volumes/private-work/frontier T9 state.",
		"metadata": map[string]any{
			"commit": commit, "objective_state": "implementation",
			"next_action": "Run the targeted route proof.",
			"agent_state": map[string]any{"state": "working", "authority": "hook", "source": "test", "cwd": "/Users/example/private"},
		},
	})
	if err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
}

func frontierT9RequestPayload(sessionID string) map[string]any {
	return map[string]any{
		"project": "contextlattice", "agent": "codex", "agent_id": "codex_gpt5",
		"harness": "codex", "session_id": sessionID,
		"repository_id":      "sheawinkler/ContextLattice",
		"repository_aliases": []any{"ContextLattice"},
		"branch":             "main", "commit": frontierT9TestCommit,
	}
}

func frontierT9RouteCall(t *testing.T, handler http.Handler, payload map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, frontierT9ContinuityZeroPath, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	result := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response status=%d body=%s: %v", recorder.Code, recorder.Body.String(), err)
	}
	return recorder, result
}

func frontierT9AssertContract(t *testing.T, result map[string]any) {
	t.Helper()
	if anyToString(result["schema_id"]) != frontierT9ContinuityZeroSchemaID {
		t.Fatalf("unexpected schema: %#v", result["schema_id"])
	}
	validation := anyMap(anyMap(result["format_contract"])["validation"])
	if anyToString(validation["status"]) != "passed" {
		t.Fatalf("continuity-zero contract failed: %#v", result["format_contract"])
	}
}

func TestFrontierT9ContinuityZeroSelectsOneSessionAndReturnsPathFreeBoundProof(t *testing.T) {
	s, handler := frontierT9TestServer(t)
	frontierT9SeedSession(t, s, "sess_frontier_t9_ready", "active", frontierT9TestCommit)
	_, beforeEvents, _ := s.agentSessions.get("sess_frontier_t9_ready")
	recorder, result := frontierT9RouteCall(t, handler, frontierT9RequestPayload(""))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT9AssertContract(t, result)
	if !anyToBool(result["ok"]) || anyToString(result["decision"]) != "ready" {
		t.Fatalf("unique active session was not selected: %#v", result)
	}
	session := anyMap(result["session"])
	if anyToString(session["session_id"]) != "sess_frontier_t9_ready" || !strings.HasPrefix(anyToString(session["packet_digest"]), "sha256:") {
		t.Fatalf("session packet binding missing: %#v", session)
	}
	if anyToString(anyMap(result["checkpoint"])["state"]) != "present" {
		t.Fatalf("checkpoint binding missing: %#v", result["checkpoint"])
	}
	if anyToString(anyMap(result["effective_profile"])["profile_digest"]) == "" {
		t.Fatalf("effective profile binding missing: %#v", result["effective_profile"])
	}
	if anyToBool(anyMap(result["safety"])["automatic_model_execution"]) || anyToBool(anyMap(result["safety"])["filesystem_mutation"]) {
		t.Fatalf("continuity-zero crossed its advisory boundary: %#v", result["safety"])
	}
	encoded := recorder.Body.String()
	for _, forbidden := range []string{"/Users/", "/Volumes/", "private-work", `"cwd":`, `"worktree":`} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("HTTP manifest leaked %q: %s", forbidden, encoded)
		}
	}
	_, afterEvents, _ := s.agentSessions.get("sess_frontier_t9_ready")
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("advisory route mutated session events: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
}

func TestFrontierT9ContinuityZeroRecursivelyRedactsPathBearingState(t *testing.T) {
	s, handler := frontierT9TestServer(t)
	frontierT9SeedSession(t, s, "sess_frontier_t9_recursive_redaction", "active", frontierT9TestCommit)
	s.agentSessions.mu.Lock()
	session := s.agentSessions.sessions["sess_frontier_t9_recursive_redaction"]
	session["objective_state"] = "review /Users/example/secret-state"
	session["agent_state"] = map[string]any{
		"state": "working", "authority": "hook", "source": "/Volumes/private/agent-hook",
	}
	events := s.agentSessions.events["sess_frontier_t9_recursive_redaction"]
	metadata := anyMap(events[len(events)-1]["metadata"])
	metadata["objective_state"] = "review /Users/example/secret-state"
	metadata["agent_state"] = map[string]any{
		"state": "working", "authority": "hook", "source": "/Volumes/private/agent-hook",
	}
	s.agentSessions.mu.Unlock()

	recorder, result := frontierT9RouteCall(t, handler, frontierT9RequestPayload("sess_frontier_t9_recursive_redaction"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	frontierT9AssertContract(t, result)
	encoded := recorder.Body.String()
	for _, forbidden := range []string{"/Users/", "/Volumes/", "secret-state", "private/agent-hook"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("recursive manifest redaction leaked %q: %s", forbidden, encoded)
		}
	}
	redaction := anyMap(anyMap(result["measurement"])["redaction"])
	if !anyToBool(redaction["applied"]) || anyToInt(redaction["paths"], 0) < 1 {
		t.Fatalf("recursive redaction was not measured: %#v", redaction)
	}
}

func TestFrontierT9ContinuityZeroAbstainsOnMultipleActiveObjectives(t *testing.T) {
	s, handler := frontierT9TestServer(t)
	frontierT9SeedSession(t, s, "sess_frontier_t9_a", "active", frontierT9TestCommit)
	frontierT9SeedSession(t, s, "sess_frontier_t9_b", "active", frontierT9TestCommit)
	payload := frontierT9RequestPayload("")
	_, result := frontierT9RouteCall(t, handler, payload)
	frontierT9AssertContract(t, result)
	if anyToString(result["decision"]) != "abstain" || !frontierT8TestContains(result["reasons"], "multiple_active_objectives") || len(contextPackAnyList(result["candidate_sessions"])) != 2 {
		t.Fatalf("ambiguous objectives did not abstain: %#v", result)
	}
}

func TestFrontierT9ContinuityZeroRejectsStaleRepositoryCommitHarnessAndRevokedContext(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*testing.T, *server, map[string]any)
	}{
		{name: "stale_session", want: "stale_session", mutate: func(_ *testing.T, s *server, _ map[string]any) {
			s.agentSessions.mu.Lock()
			s.agentSessions.sessions["sess_frontier_t9_guard"]["status"] = "expired"
			s.agentSessions.mu.Unlock()
		}},
		{name: "wrong_repository", want: "repository_mismatch", mutate: func(_ *testing.T, _ *server, payload map[string]any) {
			payload["repository_id"], payload["repository_aliases"] = "example/wrong-repository", []any{"wrong-repository"}
		}},
		{name: "wrong_commit", want: "commit_mismatch", mutate: func(_ *testing.T, _ *server, payload map[string]any) {
			payload["commit"] = "ffffffffffffffffffffffffffffffffffffffff"
		}},
		{name: "unsupported_harness", want: "unsupported_harness", mutate: func(_ *testing.T, _ *server, payload map[string]any) {
			payload["harness"] = "unknown-headless-agent"
		}},
		{name: "revoked_context", want: "context_grant_revoked_or_invalid", mutate: func(_ *testing.T, s *server, payload map[string]any) {
			s.contextMesh = &contextMeshStore{
				grants:      map[string]contextMeshGrant{"grant-revoked": {GrantID: "grant-revoked"}},
				revocations: map[string]contextMeshRevocation{"grant-revoked": {GrantID: "grant-revoked"}},
			}
			payload["mesh_grant_id"] = "grant-revoked"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, handler := frontierT9TestServer(t)
			frontierT9SeedSession(t, s, "sess_frontier_t9_guard", "active", frontierT9TestCommit)
			payload := frontierT9RequestPayload("sess_frontier_t9_guard")
			test.mutate(t, s, payload)
			_, result := frontierT9RouteCall(t, handler, payload)
			frontierT9AssertContract(t, result)
			if anyToString(result["decision"]) != "rejected" || !frontierT8TestContains(result["reasons"], test.want) {
				t.Fatalf("decision=%q reasons=%#v want rejected/%s", anyToString(result["decision"]), result["reasons"], test.want)
			}
		})
	}
}

func TestFrontierT9ContinuityZeroRejectsMissingRepositoryBranchAndCommitEvidence(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(map[string]any, []map[string]any)
	}{
		{name: "repository", want: "repository_evidence_absent", mutate: func(session map[string]any, _ []map[string]any) {
			session["repo"] = ""
		}},
		{name: "branch", want: "branch_evidence_absent", mutate: func(session map[string]any, _ []map[string]any) {
			session["branch"] = ""
		}},
		{name: "commit", want: "commit_evidence_absent", mutate: func(_ map[string]any, events []map[string]any) {
			for _, event := range events {
				delete(anyMap(event["metadata"]), "commit")
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, handler := frontierT9TestServer(t)
			frontierT9SeedSession(t, s, "sess_frontier_t9_missing_evidence", "active", frontierT9TestCommit)
			s.agentSessions.mu.Lock()
			test.mutate(s.agentSessions.sessions["sess_frontier_t9_missing_evidence"], s.agentSessions.events["sess_frontier_t9_missing_evidence"])
			s.agentSessions.mu.Unlock()
			_, result := frontierT9RouteCall(t, handler, frontierT9RequestPayload("sess_frontier_t9_missing_evidence"))
			frontierT9AssertContract(t, result)
			if anyToString(result["decision"]) != "rejected" || !frontierT8TestContains(result["reasons"], test.want) {
				t.Fatalf("missing evidence decision=%q reasons=%#v want rejected/%s", anyToString(result["decision"]), result["reasons"], test.want)
			}
		})
	}
}

func TestFrontierT9ContinuityZeroMissingOptionalEvidenceDegradesWithoutBlocking(t *testing.T) {
	s, handler := frontierT9TestServer(t)
	frontierT9SeedSession(t, s, "sess_frontier_t9_optional", "active", frontierT9TestCommit)
	s.frontierT6 = nil
	s.contextPassports = nil
	_, result := frontierT9RouteCall(t, handler, frontierT9RequestPayload("sess_frontier_t9_optional"))
	frontierT9AssertContract(t, result)
	if anyToString(result["decision"]) != "ready" {
		t.Fatalf("optional evidence blocked base continuity: %#v", result)
	}
	if anyToString(anyMap(result["effective_profile"])["state"]) != "generic_default" {
		t.Fatalf("generic profile fallback was not preserved: %#v", result["effective_profile"])
	}
	for _, reason := range []string{"context_preparation_unavailable", "context_passport_unavailable"} {
		if !frontierT8TestContains(result["reasons"], reason) {
			t.Fatalf("missing degradation reason %q: %#v", reason, result["reasons"])
		}
	}
}

func TestFrontierT9ContinuityZeroRejectsExpiredExplicitPassport(t *testing.T) {
	s, handler := frontierT9TestServer(t)
	frontierT9SeedSession(t, s, "sess_frontier_t9_passport", "active", frontierT9TestCommit)
	passport := contextPassport{
		SchemaID: contextPassportContractID, Version: 1, PassportID: "passport-expired", LineageID: "lineage-expired",
		Project: "contextlattice", Revision: 1, CreatedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano),
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), Lineage: map[string]any{"session_id": "sess_frontier_t9_passport"},
	}
	s.contextPassports.mu.Lock()
	s.contextPassports.enabled = true
	s.contextPassports.passports[passport.PassportID] = passport
	s.contextPassports.order = append(s.contextPassports.order, passport.PassportID)
	s.contextPassports.mu.Unlock()
	payload := frontierT9RequestPayload("sess_frontier_t9_passport")
	payload["passport_id"] = passport.PassportID
	_, result := frontierT9RouteCall(t, handler, payload)
	frontierT9AssertContract(t, result)
	if anyToString(result["decision"]) != "rejected" || !frontierT8TestContains(result["reasons"], "context_passport_invalid_or_expired") {
		t.Fatalf("expired passport did not fail closed: %#v", result)
	}
}

func TestFrontierT9ContinuityZeroRejectsUnknownFieldsAndNeverCreatesSession(t *testing.T) {
	s, handler := frontierT9TestServer(t)
	payload := frontierT9RequestPayload("")
	payload["cwd"] = "/private/path"
	before := len(s.agentSessions.list("all", "", "", 500, true, true))
	recorder, result := frontierT9RouteCall(t, handler, payload)
	if recorder.Code != http.StatusBadRequest || anyToString(result["error"]) != "invalid_continuity_zero_request" {
		t.Fatalf("unknown local path field was accepted status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	after := len(s.agentSessions.list("all", "", "", 500, true, true))
	if before != after {
		t.Fatalf("failed request created a hidden session: before=%d after=%d", before, after)
	}
}
