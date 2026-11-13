package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentDecisionNativeCLIEndToEnd(t *testing.T) {
	binary := os.Getenv("CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	if binary == "" {
		t.Skip("explicit source-bound native CLI binary required; build once and set CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("native test binary must have an explicit absolute path")
	}
	continuity, owner, decision := decisionApplicabilityFixture(t)
	input := anyMap(contextPackAnyList(anyMap(decision["applicability"])["inputs"])[0])
	input["target_content_hash"] = " SHA256:" + strings.ToUpper(anyToString(input["target_content_hash"])) + " "
	store := &agentSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), maxKeep: 16, maxEvents: 8, idleTTL: time.Hour,
		sessions: map[string]map[string]any{}, events: map[string][]map[string]any{}, order: []string{}}
	s := &server{agentSessions: store, continuity: continuity, memoryStore: owner}
	s.retrieval.enabled = true
	var requests atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/ops/capabilities":
			writeJSON(w, http.StatusOK, map[string]any{"decisionContinuity": agentDecisionContinuityCapability()})
		case "/memory/decision-changes":
			s.memoryDecisionChanges(w, r)
		case "/v1/agents/sessions/start":
			s.agentsSessionsStart(w, r)
		case "/v1/agents/sessions/event":
			s.agentsSessionsEvent(w, r, "")
		case "/v1/agents/sessions/sess_native/context-package":
			s.agentsSessionContextPackage(w, r, "sess_native")
		default:
			t.Errorf("unexpected native CLI request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()
	home := t.TempDir()
	commands := 0
	run := func(payload map[string]any, arguments ...string) map[string]any {
		t.Helper()
		if payload != nil {
			file := filepath.Join(home, "intent.json")
			raw, _ := json.Marshal(payload)
			if err := os.WriteFile(file, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			arguments = append(arguments, "--payload-file", file)
		}
		arguments = append(arguments, "--raw")
		command := exec.Command(binary, arguments...)
		command.Dir = home
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TMPDIR=" + home,
			"CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL, "MEMMCP_ORCHESTRATOR_URL=" + gateway.URL}
		commands++
		raw, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("native command %s failed: %v output=%s", arguments[0], err, raw)
		}
		var response map[string]any
		if json.Unmarshal(raw, &response) != nil || response["ok"] != true {
			t.Fatalf("native command did not return a confirmed envelope: %s", raw)
		}
		return response
	}
	changed := anyMap(run(decision, "contextlattice_decision_change")["decision_change"])
	selector := map[string]any{"schema_id": agentDecisionBindingSchemaID, "project": "alpha", "objective_id": changed["objective_id"], "task_identity_id": changed["task_identity_id"],
		"decision_change_id": changed["decision_change_id"], "after_decision_id": anyMap(changed["after"])["decision_id"], "repo": "alpha-repo", "branch": "main"}
	start := map[string]any{"session_id": "sess_native", "agent_id": "native-test", "project": "alpha", "repo": "alpha-repo", "branch": "main",
		"task_identity_id": changed["task_identity_id"], "objective": "Verify native decision continuity"}
	row := anyMap(run(start, "contextlattice_agent_session", "start")["session"])
	binding, err := normalizeAgentDecisionBinding(selector, false)
	if err != nil {
		t.Fatal(err)
	}
	ack := agentDecisionAckPayload(t, row, binding, "evt_native_ack")
	ack["session_id"] = "sess_native"
	bound := run(ack, "contextlattice_agent_session", "event")
	original := anyMap(bound["session"])
	for _, full := range []bool{false, true} {
		args := []string{"contextlattice", "resume", "--session-id", "sess_native", "--project", "alpha"}
		if full {
			args = append(args, "--full")
		}
		if agentDecisionResumeProjection(run(nil, args...), !full)["status"] != "verified_match" {
			t.Fatal("native resume lost original owner check")
		}
	}
	seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Corrected acceptance after native checkpoint", "test", "session")
	s.continuity.close()
	s.continuity, err = newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer s.continuity.close()
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	// Registration replay and acknowledgement replay must not need fresh owner
	// availability or re-pin to corrected content, including after restart.
	s.retrieval.enabled = false
	replay := run(decision, "contextlattice_decision_change")
	if replay["idempotent_replay"] != true || !strings.EqualFold(agentDecisionDigest(anyMap(replay["decision_change"])["applicability"]), agentDecisionDigest(changed["applicability"])) {
		t.Fatal("native historical registration rebound or lost its receipt")
	}
	if anyMap(run(ack, "contextlattice_agent_session", "event")["event"])["idempotent_replay"] != true {
		t.Fatal("native acknowledgement retry was not historical")
	}
	s.retrieval.enabled = true
	for _, compact := range []bool{false, true} {
		view := "full"
		if compact {
			view = "compact"
		}
		response := run(nil, "contextlattice_agent_session", "context-package", "--session-id", "sess_native", "--view", view)
		projection := agentDecisionResumeProjection(response, compact)
		if projection["status"] != "changed" || projection["record_digest"] != anyMap(original["decision_binding"])["record_digest"] {
			t.Fatal("native resume refreshed pins after correction/restart", projection)
		}
		assertAgentDecisionResumeNoAction(t, response, compact)
	}
	typed := cloneJSONMap(start)
	typed["session_id"], typed["decision_binding"] = "sess_native_typed", selector
	first := anyMap(run(typed, "contextlattice_agent_session", "start")["session"])
	ensured := run(typed, "contextlattice_agent_session", "ensure")
	if ensured["reused"] != true || agentDecisionDigest(anyMap(ensured["session"])["decision_baseline"]) != agentDecisionDigest(first["decision_baseline"]) {
		t.Fatal("native typed ensure replaced or rebound the session")
	}
	t.Logf("native_cli_commands=%d http_requests=%d provider_calls=0 fixture_scope=isolated_same_project", commands, requests.Load())
	alias := cloneJSONMap(typed)
	alias["session_id"] = "sess_native_alias"
	for canonical, name := range map[string]string{"project": "project_name", "session_id": "sessionId", "task_identity_id": "taskIdentityId", "agent_id": "agentId"} {
		alias[name] = alias[canonical]
		delete(alias, canonical)
	}
	aliasRow := anyMap(run(alias, "contextlattice_agent_session", "start")["session"])
	before, _ := os.ReadFile(store.path)
	aliasEnsure := run(alias, "contextlattice_agent_session", "ensure")
	after, _ := os.ReadFile(store.path)
	if aliasEnsure["reused"] != true || anyMap(aliasEnsure["session"])["id"] != aliasRow["id"] ||
		agentDecisionDigest(anyMap(aliasEnsure["session"])["decision_baseline"]) != agentDecisionDigest(aliasRow["decision_baseline"]) || !bytes.Equal(before, after) {
		t.Fatal("native outer aliases changed selection, binding, or durable session bytes")
	}
	t.Logf("expanded_native_cli_commands=%d http_requests=%d provider_calls=0", commands, requests.Load())
}
