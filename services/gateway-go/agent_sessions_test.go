package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func postAgentSessionJSON(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	payload := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s: %v body=%s", url, err, string(raw))
		}
	}
	return resp.StatusCode, payload
}

func getAgentSessionJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	payload := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s: %v body=%s", url, err, string(raw))
		}
	}
	return resp.StatusCode, payload
}

func TestAgentSessionIDsAreLosslessAtCanonicalLimitAndRejectOverflow(t *testing.T) {
	store := &agentSessionStore{
		path: filepath.Join(t.TempDir(), "agent-sessions.json"), maxKeep: 16, maxEvents: 16, idleTTL: time.Hour,
		sessions: map[string]map[string]any{}, order: []string{}, events: map[string][]map[string]any{},
	}
	prefix := strings.Repeat("a", 150)
	firstID, secondID := prefix+"-first", prefix+"-second"
	for _, sessionID := range []string{firstID, secondID} {
		if len(sessionID) > maxAgentSessionIDLength {
			t.Fatalf("test session id exceeds canonical limit: %d", len(sessionID))
		}
		session, err := store.start(map[string]any{"session_id": sessionID, "agent": "codex", "project": "contextlattice"})
		if err != nil || anyToString(session["id"]) != sessionID {
			t.Fatalf("canonical session id was not stored losslessly: id=%q session=%#v err=%v", sessionID, session, err)
		}
		_, event, err := store.appendEvent(sessionID, map[string]any{"type": "checkpoint.written", "project": "contextlattice"})
		if err != nil || anyToString(event["session_id"]) != sessionID {
			t.Fatalf("canonical session event id was not stored losslessly: id=%q event=%#v err=%v", sessionID, event, err)
		}
	}
	if first, _, ok := store.get(firstID); !ok || anyToString(first["id"]) != firstID {
		t.Fatalf("first long session id aliased: %#v", first)
	}
	if second, _, ok := store.get(secondID); !ok || anyToString(second["id"]) != secondID {
		t.Fatalf("second long session id aliased: %#v", second)
	}
	overflow := strings.Repeat("z", maxAgentSessionIDLength+1)
	if _, err := store.start(map[string]any{"session_id": overflow}); !errors.Is(err, errAgentSessionIDInvalid) {
		t.Fatalf("overflow session id was not rejected: %v", err)
	}
}

func TestAgentSessionProjectlessReuseNeverSelectsProjectBackedHistory(t *testing.T) {
	store := &agentSessionStore{
		path: filepath.Join(t.TempDir(), "agent-sessions.json"), maxKeep: 16, maxEvents: 16, idleTTL: time.Hour,
		sessions: map[string]map[string]any{}, order: []string{}, events: map[string][]map[string]any{},
	}
	projectBacked, reused, err := store.startOrReuse(map[string]any{
		"ensure": true, "session_id": "sess-project-backed", "reuse_key": "shared-selector",
		"agent": "codex", "project": "alpha", "objective": "project-backed history",
	})
	if err != nil || reused {
		t.Fatalf("seed project-backed session: session=%#v reused=%t err=%v", projectBacked, reused, err)
	}
	projectless, reused, err := store.startOrReuse(map[string]any{
		"ensure": true, "reuse_key": "shared-selector", "agent": "codex", "objective": "projectless compatibility",
	})
	if err != nil || reused || anyToString(projectless["id"]) == anyToString(projectBacked["id"]) || agentSessionProject(projectless) != "" {
		t.Fatalf("projectless reuse selected project-backed history: project_backed=%#v projectless=%#v reused=%t err=%v", projectBacked, projectless, reused, err)
	}
}

func TestAgentSessionLifecycleAndRuntimeTelemetry(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	t.Setenv(agentProofTimelineFeatureEnv, "true")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, started := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"session_id":"sess-test",
		"agent":"codex",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"repo":"git@example.com:contextlattice/repo.git",
		"branch":"feature/lifecycle-presence",
		"worktree":"/tmp/contextlattice-worktree",
		"cwd":"/tmp/contextlattice-worktree",
		"task_id":"HD-17",
		"native_session_id":"codex-native-123",
		"agent_state":{"state":"working","authority":"hook","source":"codex-session-hook","task_id":"HD-17","repo":"git@example.com:contextlattice/repo.git","branch":"feature/lifecycle-presence","worktree":"/tmp/contextlattice-worktree","cwd":"/tmp/contextlattice-worktree","native_session_id":"codex-native-123"},
		"objective":"make ContextLattice coordinate parallel agents",
		"objective_hierarchy":{
			"schema_id":"contextlattice_objective_hierarchy.v1",
			"project":{"name":"contextlattice","primary_objective":"make ContextLattice the runtime coordination layer for parallel agents"},
			"topic":{"topic_path":"contextlattice/runtime","subtopic":"runtime","path_segments":["contextlattice","runtime"],"objective":"coordinate agent runtime handoffs"},
			"session":{"session_id":"sess-test","objective":"make ContextLattice coordinate parallel agents"},
			"subobjectives":["preserve objective lineage","surface run shaping evidence"],
			"current":{"level":"session","objective":"make ContextLattice coordinate parallel agents"}
		},
		"objective_lineage":{
			"schema_id":"contextlattice_objective_lineage.v1",
			"source":"test",
			"precedence":["session.objective","topic.objective","project.primary_objective"],
			"drift":{"status":"aligned","project_to_topic":"aligned","topic_to_session":"aligned","project_to_session":"aligned"},
			"handoff_rule":"carry objective hierarchy into the next prompt"
		},
		"tags":["runtime","test"]
	}`)
	if status != http.StatusOK || !anyToBool(started["ok"]) {
		t.Fatalf("expected start ok, status=%d payload=%#v", status, started)
	}
	session := anyMap(started["session"])
	if anyToString(session["id"]) != "sess-test" {
		t.Fatalf("unexpected session id: %#v", session)
	}

	status, contextEvent := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-test",
		"type":"context_pack.completed",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"summary":"runtime coordination context",
		"metadata":{"memory_hits":3,"result_count":3}
	}`)
	if status != http.StatusOK || !anyToBool(contextEvent["ok"]) {
		t.Fatalf("expected context event ok, status=%d payload=%#v", status, contextEvent)
	}
	session = anyMap(contextEvent["session"])
	if anyToString(session["status"]) != "active" {
		t.Fatalf("context contribution should not complete session: %#v", session)
	}
	contribution := anyMap(session["memory_contribution"])
	if anyToInt(contribution["context_packs"], 0) != 1 || anyToInt(contribution["score"], 0) <= 0 {
		t.Fatalf("expected context contribution score, got %#v", contribution)
	}

	status, stateEvent := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-test",
		"type":"agent.state.awaiting_user",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"summary":"waiting on approval",
		"status":"paused",
		"metadata":{
			"agent_state":{"state":"awaiting_user","authority":"hook","source":"codex-session-hook","task_id":"HD-17","needs_user":"approve command","repo":"git@example.com:contextlattice/repo.git","branch":"feature/lifecycle-presence","worktree":"/tmp/contextlattice-worktree","cwd":"/tmp/contextlattice-worktree","native_session_id":"codex-native-123"},
			"ownership":{"task_id":"HD-17","repo":"git@example.com:contextlattice/repo.git","branch":"feature/lifecycle-presence","worktree":"/tmp/contextlattice-worktree","cwd":"/tmp/contextlattice-worktree","native_session_id":"codex-native-123"}
		}
	}`)
	if status != http.StatusOK || !anyToBool(stateEvent["ok"]) {
		t.Fatalf("expected state event ok, status=%d payload=%#v", status, stateEvent)
	}
	stateSession := anyMap(stateEvent["session"])
	if anyToString(stateSession["status"]) != "paused" {
		t.Fatalf("expected semantic awaiting_user to map to paused session status, got %#v", stateSession)
	}
	stateRollup := anyMap(stateEvent["rollup"])
	lifecycle := anyMap(stateRollup["agent_lifecycle"])
	if anyToString(lifecycle["state"]) != "awaiting_user" || anyToString(lifecycle["authority"]) != "hook" {
		t.Fatalf("expected lifecycle rollup, got %#v", lifecycle)
	}
	ownership := anyMap(stateRollup["ownership"])
	if anyToString(ownership["task_id"]) != "HD-17" || anyToString(ownership["branch"]) != "feature/lifecycle-presence" || anyToString(ownership["native_session_id"]) != "codex-native-123" {
		t.Fatalf("expected ownership rollup, got %#v", ownership)
	}

	status, preflightEvent := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/events", `{
		"type":"agent.preflight.completed",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"summary":"prepare runtime coordination context",
		"metadata":{
			"skills_index_returned":1,
			"skills_index":{
				"returned":1,
					"top":[{"name":"frontend-design","source":"codex-skills","path":"/home/user/.codex/skills/frontend-design/SKILL.md","score":98}]
			}
		}
	}`)
	if status != http.StatusOK || !anyToBool(preflightEvent["ok"]) {
		t.Fatalf("expected preflight event ok, status=%d payload=%#v", status, preflightEvent)
	}

	status, graphEvent := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/events", `{
		"type":"graph.neighbors_returned",
		"summary":"contextlattice::notes/a.md",
		"metadata":{"edge_count":5,"result_count":7}
	}`)
	if status != http.StatusOK || !anyToBool(graphEvent["ok"]) {
		t.Fatalf("expected graph event ok, status=%d payload=%#v", status, graphEvent)
	}
	contribution = anyMap(anyMap(graphEvent["session"])["memory_contribution"])
	if anyToInt(contribution["graph_touches"], 0) != 1 {
		t.Fatalf("expected graph contribution, got %#v", contribution)
	}
	rollup := anyMap(graphEvent["rollup"])
	if anyToString(rollup["schema_id"]) != agentSessionRollupContractID {
		t.Fatalf("expected session rollup contract, got %#v", rollup)
	}
	if anyToInt(rollup["confidence"], 0) <= 0 {
		t.Fatalf("expected positive rollup confidence, got %#v", rollup)
	}
	if anyToString(anyMap(anyMap(rollup["objective_hierarchy"])["project"])["primary_objective"]) == "" {
		t.Fatalf("expected objective hierarchy on rollup, got %#v", rollup["objective_hierarchy"])
	}
	promptPackage := anyMap(rollup["prompt_package"])
	if !strings.Contains(anyToString(promptPackage["endpoint"]), "/context-package") {
		t.Fatalf("expected context package endpoint in rollup, got %#v", promptPackage)
	}

	status, rollupResponse := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/rollup")
	if status != http.StatusOK || !anyToBool(rollupResponse["ok"]) {
		t.Fatalf("expected rollup route ok, status=%d payload=%#v", status, rollupResponse)
	}
	if anyToString(anyMap(rollupResponse["rollup"])["session_id"]) != "sess-test" {
		t.Fatalf("expected rollup session id, got %#v", rollupResponse)
	}

	status, packageResponse := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/context-package")
	if status != http.StatusOK || !anyToBool(packageResponse["ok"]) {
		t.Fatalf("expected context package route ok, status=%d payload=%#v", status, packageResponse)
	}
	if anyToString(packageResponse["schema_id"]) != agentPromptContextPackageContractID {
		t.Fatalf("expected prompt context package contract, got %#v", packageResponse)
	}
	if !strings.Contains(anyToString(packageResponse["reference_prompt"]), "ContextLattice session package") {
		t.Fatalf("expected bounded reference prompt, got %#v", packageResponse["reference_prompt"])
	}
	if !strings.Contains(anyToString(packageResponse["reference_prompt"]), "Project primary objective") {
		t.Fatalf("expected objective hierarchy in reference prompt, got %#v", packageResponse["reference_prompt"])
	}
	if !strings.Contains(anyToString(packageResponse["reference_prompt"]), "Agent lifecycle: awaiting_user via hook") ||
		!strings.Contains(anyToString(packageResponse["reference_prompt"]), "task=HD-17") {
		t.Fatalf("expected lifecycle and ownership in reference prompt, got %#v", packageResponse["reference_prompt"])
	}

	status, traceResponse := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/trace")
	if status != http.StatusOK || !anyToBool(traceResponse["ok"]) {
		t.Fatalf("expected trace route ok, status=%d payload=%#v", status, traceResponse)
	}
	if anyToString(traceResponse["schema_id"]) != agentRunTraceContractID {
		t.Fatalf("expected run trace contract, got %#v", traceResponse)
	}
	timeline, _ := traceResponse["timeline"].([]any)
	if len(timeline) == 0 {
		t.Fatalf("expected trace timeline, got %#v", traceResponse)
	}
	runCard := anyMap(traceResponse["run_card"])
	markdown := anyToString(runCard["markdown"])
	if !strings.Contains(markdown, "Run-Shaping Evidence") || !strings.Contains(markdown, "Skills That May Be Helpful") {
		t.Fatalf("expected run card sections, got %q", markdown)
	}
	if !strings.Contains(markdown, "Project primary objective") || !strings.Contains(markdown, "Objective lineage") {
		t.Fatalf("expected objective lineage in run card, got %q", markdown)
	}
	if !strings.Contains(markdown, "frontend-design") {
		t.Fatalf("expected captured skill in run card, got %q", markdown)
	}
	if !strings.Contains(markdown, "Agent lifecycle: `awaiting_user` via `hook`") ||
		!strings.Contains(markdown, "task `HD-17`") {
		t.Fatalf("expected lifecycle and ownership in run card, got %q", markdown)
	}
	validation := anyMap(anyMap(traceResponse["format_contract"])["validation"])
	if anyToString(validation["status"]) != "passed" {
		t.Fatalf("expected trace contract validation to pass, got %#v", traceResponse["format_contract"])
	}

	status, proofResponse := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/proof-timeline")
	if status != http.StatusOK || !anyToBool(proofResponse["ok"]) {
		t.Fatalf("expected proof-timeline route ok, status=%d payload=%#v", status, proofResponse)
	}
	if anyToString(proofResponse["schema_id"]) != agentProofTimelineContractID {
		t.Fatalf("expected proof timeline contract, got %#v", proofResponse)
	}
	proofValidation := anyMap(anyMap(proofResponse["format_contract"])["validation"])
	if anyToString(proofValidation["status"]) != "passed" {
		t.Fatalf("expected proof timeline contract validation to pass, got %#v", proofResponse["format_contract"])
	}
	proofIntegrity := anyMap(proofResponse["integrity"])
	if anyToInt(proofIntegrity["provider_calls"], -1) != 0 || anyToInt(proofIntegrity["authoritative_ledger_mutations"], -1) != 0 {
		t.Fatalf("proof timeline must remain a local read-only projection, got %#v", proofIntegrity)
	}

	status, runtime := getAgentSessionJSON(t, gateway.URL+"/telemetry/agents/runtime?limit=4")
	if status != http.StatusOK || !anyToBool(runtime["enabled"]) {
		t.Fatalf("expected runtime telemetry ok, status=%d payload=%#v", status, runtime)
	}
	if anyToInt(runtime["paused"], 0) != 1 {
		t.Fatalf("expected one paused session after awaiting_user lifecycle event, got %#v", runtime)
	}

	status, completed := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-test",
		"type":"session.completed",
		"summary":"done"
	}`)
	if status != http.StatusOK || !anyToBool(completed["ok"]) {
		t.Fatalf("expected complete ok, status=%d payload=%#v", status, completed)
	}
	session = anyMap(completed["session"])
	if anyToString(session["status"]) != "completed" {
		t.Fatalf("expected completed session, got %#v", session)
	}
	completedLifecycle := anyMap(session["agent_state"])
	if anyToString(completedLifecycle["state"]) != "done" {
		t.Fatalf("expected completed session to force agent lifecycle done, got %#v", completedLifecycle)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("GO_AGENT_SESSIONS_PATH")), "agent_sessions.json")); err != nil {
		t.Fatalf("expected persisted agent session ledger: %v", err)
	}
}

func TestBlockedSessionCanRecoverWithoutCompletedAt(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, started := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"session_id":"sess-recover",
		"agent":"codex",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"objective":"prove blocked is recoverable",
		"agent_state":{"state":"working","authority":"hook","source":"test"}
	}`)
	if status != http.StatusOK || !anyToBool(started["ok"]) {
		t.Fatalf("expected start ok, status=%d payload=%#v", status, started)
	}

	status, blocked := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-recover",
		"type":"session.blocked",
		"status":"blocked",
		"summary":"waiting on external fix",
		"metadata":{"agent_state":{"state":"blocked","authority":"hook","source":"test","blocked_by":"external fix"}}
	}`)
	if status != http.StatusOK || !anyToBool(blocked["ok"]) {
		t.Fatalf("expected blocked event ok, status=%d payload=%#v", status, blocked)
	}
	blockedSession := anyMap(blocked["session"])
	if anyToString(blockedSession["status"]) != "blocked" {
		t.Fatalf("expected blocked status, got %#v", blockedSession)
	}
	if anyToString(blockedSession["completed_at"]) != "" {
		t.Fatalf("blocked should be recoverable and must not set completed_at: %#v", blockedSession)
	}

	status, recovered := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-recover",
		"type":"agent.state.working",
		"status":"active",
		"summary":"external fix arrived",
		"metadata":{"agent_state":{"state":"working","authority":"hook","source":"test"}}
	}`)
	if status != http.StatusOK || !anyToBool(recovered["ok"]) {
		t.Fatalf("expected recovery event ok, status=%d payload=%#v", status, recovered)
	}
	recoveredSession := anyMap(recovered["session"])
	if anyToString(recoveredSession["status"]) != "active" {
		t.Fatalf("expected recovered active status, got %#v", recoveredSession)
	}
	if anyToString(recoveredSession["completed_at"]) != "" {
		t.Fatalf("recovered session must keep completed_at empty: %#v", recoveredSession)
	}
	lifecycle := anyMap(anyMap(recovered["rollup"])["agent_lifecycle"])
	if anyToString(lifecycle["state"]) != "working" {
		t.Fatalf("expected lifecycle to recover to working, got %#v", lifecycle)
	}
}

func TestAgentSessionEnsureReusesTaskAndTerminalStateIsAbsorbing(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	request := `{
		"ensure":true,
		"reuse_key":"reuse_task_620",
		"agent":"codex",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"task_id":"issue-620",
		"objective":"implement one task one session"
	}`
	status, created := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", request)
	if status != http.StatusOK || !anyToBool(created["created"]) || anyToBool(created["reused"]) {
		t.Fatalf("expected first ensure to create one session, status=%d payload=%#v", status, created)
	}
	sessionID := anyToString(anyMap(created["session"])["id"])
	status, reused := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", request)
	if status != http.StatusOK || !anyToBool(reused["reused"]) || anyToString(anyMap(reused["session"])["id"]) != sessionID {
		t.Fatalf("expected exact task ensure to reuse %s, status=%d payload=%#v", sessionID, status, reused)
	}
	if anyToInt(anyMap(reused["session"])["event_count"], 0) != 1 {
		t.Fatalf("reused ensure must not append another session.started event: %#v", reused)
	}

	status, listed := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions?project=contextlattice&view=compact")
	rows := contextPackAnyList(listed["sessions"])
	if status != http.StatusOK || len(rows) != 1 || len(anyMap(anyMap(rows[0])["rollup"])) != 0 {
		t.Fatalf("expected one compact session row without heavyweight rollup, status=%d payload=%#v", status, listed)
	}

	status, completed := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{"session_id":"`+sessionID+`","type":"session.completed","status":"completed"}`)
	if status != http.StatusOK || anyToString(anyMap(completed["session"])["status"]) != "completed" {
		t.Fatalf("expected terminal completion, status=%d payload=%#v", status, completed)
	}
	status, correction := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"`+sessionID+`","type":"correction.recorded","status":"active",
		"metadata":{"agent_state":{"state":"working","authority":"self_report","source":"late-correction"}}
	}`)
	correctionSession := anyMap(correction["session"])
	if status != http.StatusOK || anyToString(correctionSession["status"]) != "completed" || anyToString(anyMap(correctionSession["agent_state"])["state"]) != "done" {
		t.Fatalf("post-terminal correction mutated absorbing lifecycle state, status=%d payload=%#v", status, correction)
	}
	status, conflict := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{"ensure":true,"session_id":"`+sessionID+`","project":"contextlattice","agent_id":"codex_gpt5_test"}`)
	if status != http.StatusConflict || anyToString(conflict["error"]) != "agent_session_terminal" {
		t.Fatalf("terminal session id reopened instead of conflicting, status=%d payload=%#v", status, conflict)
	}
	status, replacement := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", request)
	if status != http.StatusOK || !anyToBool(replacement["created"]) || anyToString(anyMap(replacement["session"])["id"]) == sessionID {
		t.Fatalf("expected a fresh task epoch after terminal completion, status=%d payload=%#v", status, replacement)
	}
}

func TestAgentSessionEnsureDoesNotFallbackPastExplicitReuseKey(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	mainRequest := `{"ensure":true,"reuse_key":"reuse_main","agent":"codex","agent_id":"codex_test","project":"contextlattice","task_id":"same-task","branch":"main"}`
	status, mainSession := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", mainRequest)
	if status != http.StatusOK || !anyToBool(mainSession["created"]) {
		t.Fatalf("create main session: status=%d payload=%#v", status, mainSession)
	}
	releaseRequest := `{"ensure":true,"reuse_key":"reuse_release","agent":"codex","agent_id":"codex_test","project":"contextlattice","task_id":"same-task","branch":"release"}`
	status, releaseSession := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", releaseRequest)
	if status != http.StatusOK || !anyToBool(releaseSession["created"]) || anyToBool(releaseSession["reused"]) {
		t.Fatalf("strong reuse key fell back to cross-branch task id: status=%d payload=%#v", status, releaseSession)
	}
	if anyToString(anyMap(mainSession["session"])["id"]) == anyToString(anyMap(releaseSession["session"])["id"]) {
		t.Fatalf("distinct branch reuse keys returned the same session: main=%#v release=%#v", mainSession, releaseSession)
	}
}

func TestExpiredAgentSessionIsNotReportedLiveOrReopened(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	t.Setenv("GO_AGENT_SESSION_IDLE_TTL_SECS", "60")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, started := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"session_id":"sess-expired",
		"agent":"codex",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"objective":"expire stale presence",
		"agent_state":{"state":"working","authority":"hook","source":"test","expires_at":"2020-01-01T00:00:00Z"}
	}`)
	if status != http.StatusOK || !anyToBool(started["ok"]) {
		t.Fatalf("expected session creation before effective expiry projection, status=%d payload=%#v", status, started)
	}
	status, item := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-expired")
	if status != http.StatusOK || anyToString(anyMap(item["session"])["status"]) != "expired" {
		t.Fatalf("expected expired effective status, status=%d payload=%#v", status, item)
	}
	status, conflict := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{"session_id":"sess-expired","type":"agent.state.working","status":"active"}`)
	if status != http.StatusConflict || anyToString(conflict["error"]) != "agent_session_terminal" {
		t.Fatalf("expired session accepted a reopening event, status=%d payload=%#v", status, conflict)
	}
	status, outcome := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-expired","type":"context_pack.outcome_reported","status":"active",
		"metadata":{"agent_state":{"state":"working","authority":"self_report","source":"late-outcome"}}
	}`)
	if status != http.StatusOK || anyToString(anyMap(outcome["session"])["status"]) != "expired" {
		t.Fatalf("allowed post-expiry outcome reopened session, status=%d payload=%#v", status, outcome)
	}
	status, runtime := getAgentSessionJSON(t, gateway.URL+"/telemetry/agents/runtime?limit=8")
	if status != http.StatusOK || anyToInt(runtime["expired"], 0) != 1 || anyToInt(runtime["live"], -1) != 0 || len(contextPackAnyList(runtime["sessions"])) != 0 {
		t.Fatalf("expired session leaked into live runtime, status=%d payload=%#v", status, runtime)
	}
}

func TestAgentSessionIdleTTLUsesLatestEventBeforeStaleAgentStateTimestamp(t *testing.T) {
	now := time.Now().UTC()
	row := map[string]any{
		"status":        "active",
		"started_at":    now.Add(-24 * time.Hour).Format(time.RFC3339),
		"last_event_at": now.Add(-5 * time.Minute).Format(time.RFC3339),
		"agent_state": map[string]any{
			"state": "working", "updated_at": now.Add(-18 * time.Hour).Format(time.RFC3339),
		},
	}
	if got := agentSessionEffectiveStatus(row, now, time.Hour); got != "active" {
		t.Fatalf("recent checkpoint lost to stale agent-state timestamp: got %s", got)
	}
}

func TestAgentSessionCompactResumeUsesBoundedPacket(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()
	status, _ := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"session_id":"sess-resume","agent":"codex","agent_id":"codex_test","project":"contextlattice",
		"objective":"resume bounded task truth","task_id":"issue-620"
	}`)
	if status != http.StatusOK {
		t.Fatalf("start status=%d", status)
	}
	status, _ = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-resume","type":"checkpoint.written","summary":"packet and session tests passed"
	}`)
	if status != http.StatusOK {
		t.Fatalf("checkpoint status=%d", status)
	}
	status, packet := getAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-resume/context-package?view=compact")
	if status != http.StatusOK || anyToString(packet["schema_id"]) != agentPacketContractID || anyToString(packet["surface"]) != "session_resume" {
		t.Fatalf("expected compact resume packet, status=%d payload=%#v", status, packet)
	}
	assertBoundaryContractPassed(t, agentPacketContractID, packet)
	if anyToString(anyMap(packet["decision_gate"])["decision"]) != "verify" {
		t.Fatalf("session memory should require current-local-state verification: %#v", packet["decision_gate"])
	}
	if _, leaked := packet["context_package"]; leaked {
		t.Fatalf("compact resume leaked heavyweight prompt package: %#v", packet)
	}
	count := contextPackCountAnyTokens(packet)
	if count.Tokens > defaultAgentPacketHardTokens || anyToInt(anyMap(packet["token_budget"])["actual_tokens"], 0) != count.Tokens {
		t.Fatalf("resume packet token accounting mismatch count=%d budget=%#v", count.Tokens, packet["token_budget"])
	}
}
