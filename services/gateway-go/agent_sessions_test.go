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
		"objective":"make ContextLattice coordinate parallel agents",
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

	status, preflightEvent := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/sess-test/events", `{
		"type":"agent.preflight.completed",
		"agent_id":"codex_gpt5_test",
		"project":"contextlattice",
		"summary":"prepare runtime coordination context",
		"metadata":{
			"skills_index_returned":1,
			"skills_index":{
				"returned":1,
				"top":[{"name":"frontend-design","source":"codex-skills","path":"/Users/sheawinkler/.codex/skills/frontend-design/SKILL.md","score":98}]
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
	if !strings.Contains(markdown, "frontend-design") {
		t.Fatalf("expected captured skill in run card, got %q", markdown)
	}
	validation := anyMap(anyMap(traceResponse["format_contract"])["validation"])
	if anyToString(validation["status"]) != "passed" {
		t.Fatalf("expected trace contract validation to pass, got %#v", traceResponse["format_contract"])
	}

	status, runtime := getAgentSessionJSON(t, gateway.URL+"/telemetry/agents/runtime?limit=4")
	if status != http.StatusOK || !anyToBool(runtime["enabled"]) {
		t.Fatalf("expected runtime telemetry ok, status=%d payload=%#v", status, runtime)
	}
	if anyToInt(runtime["active"], 0) != 1 {
		t.Fatalf("expected one active session, got %#v", runtime)
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
	if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("GO_AGENT_SESSIONS_PATH")), "agent_sessions.json")); err != nil {
		t.Fatalf("expected persisted agent session ledger: %v", err)
	}
}
