package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestAgentSessionLifecycleAndRuntimeTelemetry(t *testing.T) {
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
