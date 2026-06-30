package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgsAllowsFlagsAfterPositionalQuery(t *testing.T) {
	parsed := parseArgs(
		[]string{"release readiness", "--project", "contextlattice", "--mode=fast", "-l", "7", "--pretty"},
		mergeStringFlags(commonStringFlags(), map[string]string{"limit": "limit", "l": "limit"}),
		commonBoolFlags(),
	)
	if got := parsed.string("project", ""); got != "contextlattice" {
		t.Fatalf("project=%q", got)
	}
	if got := parsed.string("mode", ""); got != "fast" {
		t.Fatalf("mode=%q", got)
	}
	if got := parsed.int("limit", 0); got != 7 {
		t.Fatalf("limit=%d", got)
	}
	if !parsed.bool("pretty") {
		t.Fatalf("expected pretty flag")
	}
	if len(parsed.pos) != 1 || parsed.pos[0] != "release readiness" {
		t.Fatalf("unexpected positional args: %#v", parsed.pos)
	}
}

func TestSearchCommandUsesGoNativeHTTPPayload(t *testing.T) {
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/search" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"results": []map[string]any{
				{"text": "result"},
			},
			"retrieval_lifecycle": map[string]any{"status": "succeeded"},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_search", "native cli", "--project", "alpha", "--mode", "fast", "--limit", "3", "--raw"}); err != nil {
		t.Fatalf("run search: %v", err)
	}
	if captured["query"] != "native cli" || captured["project"] != "alpha" || captured["retrieval_mode"] != "fast" {
		t.Fatalf("unexpected search payload: %#v", captured)
	}
	if int(captured["limit"].(float64)) != 3 {
		t.Fatalf("unexpected limit payload: %#v", captured)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["ok"] != true {
		t.Fatalf("expected ok output: %#v", output)
	}
}

func TestPackCommandMarksNativeCLIAndSession(t *testing.T) {
	var packPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "sess-test"}})
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"context_pack": map[string]any{
					"facts": []any{},
					"token_budget": map[string]any{
						"active": true,
					},
					"omitted_high_value_refs": []any{
						map[string]any{"kind": "decision", "summary": "omitted"},
					},
				},
				"format_contract": map[string]any{
					"schema_id":         "context_pack_response.v1",
					"contract_valid":    true,
					"actual_json_bytes": 512,
					"validation":        map[string]any{"status": "passed"},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", "native pack", "--project", "alpha", "--mode", "fast", "--target-context-pack-tokens", "512", "--already-loaded-tokens", "200", "--raw"}); err != nil {
		t.Fatalf("run pack: %v", err)
	}
	if packPayload["native_cli_implementation"] != true {
		t.Fatalf("expected native_cli_implementation marker: %#v", packPayload)
	}
	if packPayload["session_id"] != "sess-test" {
		t.Fatalf("expected session id from auto session: %#v", packPayload)
	}
	if asInt(packPayload["target_context_pack_tokens"]) != 512 || asInt(packPayload["already_loaded_tokens"]) != 200 {
		t.Fatalf("expected token budget fields in pack request: %#v", packPayload)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["ok"] != true {
		t.Fatalf("expected ok output: %#v", output)
	}
	if !asBool(asMap(output["token_budget"])["active"]) {
		t.Fatalf("expected normalized root token_budget from nested pack, got %#v", output)
	}
	if omitted := firstList(output["omitted_high_value_refs"]); len(omitted) == 0 {
		t.Fatalf("expected normalized omitted refs from nested pack, got %#v", output)
	}
}

func TestSkillsIndexCommandUsesNativeEndpoint(t *testing.T) {
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/skills_index_search" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode skills request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"returned": 1,
			"results":  []map[string]any{{"name": "playwright", "source": "active"}},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_skills_index", "search", "browser automation", "--limit", "4"}); err != nil {
		t.Fatalf("run skills index: %v", err)
	}
	if captured["query"] != "browser automation" || int(captured["limit"].(float64)) != 4 || captured["json"] != true {
		t.Fatalf("unexpected skills payload: %#v", captured)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["returned"] != float64(1) {
		t.Fatalf("expected returned count: %#v", output)
	}
}

func TestAdapterBootstrapCompactsPreflightResult(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/preflight" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":             true,
			"agent":          "codex",
			"agent_id":       "codex_gpt5",
			"project":        "alpha",
			"query":          "bootstrap smoke",
			"retrieval_mode": "fast",
			"session_id":     "sess-bootstrap",
			"agent_profile":  map[string]any{"large": strings.Repeat("x", 1000)},
			"objective_runtime": map[string]any{
				"objective_state": "active",
				"next_action":     "continue",
				"format_contract": map[string]any{"validation": map[string]any{"status": "passed"}},
			},
			"policy_context_package": map[string]any{
				"format_contract": map[string]any{"validation": map[string]any{"status": "passed"}},
			},
			"skills_index": map[string]any{
				"ok":       true,
				"returned": 1,
				"results":  []map[string]any{{"name": "research", "source": "active", "path": "/skills/research/SKILL.md", "score": 9}},
			},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_agent_adapter", "bootstrap", "--agent", "codex", "--project", "alpha", "--query", "bootstrap smoke", "--mode", "fast"}); err != nil {
		t.Fatalf("run adapter bootstrap: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["ok"] != true || output["session_id"] != "sess-bootstrap" {
		t.Fatalf("unexpected bootstrap output: %#v", output)
	}
	preflight := output["result"].(map[string]any)["preflight"].(map[string]any)
	if preflight["raw_omitted"] != true {
		t.Fatalf("expected compact preflight output: %#v", preflight)
	}
	if _, ok := preflight["agent_profile"]; ok {
		t.Fatalf("compact preflight leaked raw agent profile: %#v", preflight)
	}
}

func TestAdapterStatePostsLifecycleAndOwnership(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/event":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode event request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": map[string]any{"id": "evt-state"}})
		case "/v1/agents/sessions/sess-state/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_agent_adapter", "state",
		"--agent", "codex",
		"--project", "alpha",
		"--session-id", "sess-state",
		"--state", "awaiting_user",
		"--authority", "hook",
		"--source", "codex-session-hook",
		"--task-id", "HD-17",
		"--repo", "git@example.com:alpha/repo.git",
		"--branch", "feature/lifecycle",
		"--worktree", "/tmp/contextlattice-worktree",
		"--cwd", "/tmp/contextlattice-worktree",
		"--native-session-id", "codex-native-123",
		"--needs-user", "approve shell command",
		"--pretty",
	}); err != nil {
		t.Fatalf("run adapter state: %v", err)
	}
	if captured["type"] != "agent.state.awaiting_user" || captured["status"] != "paused" {
		t.Fatalf("unexpected state event envelope: %#v", captured)
	}
	metadata := asMap(captured["metadata"])
	state := asMap(metadata["agent_state"])
	if state["state"] != "awaiting_user" || state["authority"] != "hook" || state["needs_user"] != "approve shell command" {
		t.Fatalf("unexpected agent_state metadata: %#v", state)
	}
	ownership := asMap(metadata["ownership"])
	if ownership["task_id"] != "HD-17" || ownership["branch"] != "feature/lifecycle" || ownership["native_session_id"] != "codex-native-123" {
		t.Fatalf("unexpected ownership metadata: %#v", ownership)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["command"] != "state" || output["ok"] != true {
		t.Fatalf("unexpected state output: %#v", output)
	}
}

func TestDiscoverUsesProcessFixtureAndProfileAuthority(t *testing.T) {
	globalHome := t.TempDir()
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	t.Setenv("CONTEXTLATTICE_REPO_ROOT", repoRoot)
	t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", globalHome)
	binDir := filepath.Join(globalHome, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	for _, name := range []string{"contextlattice_agent_adapter", "contextlattice_agent_discover"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	fixture := filepath.Join(t.TempDir(), "ps.txt")
	if err := os.WriteFile(fixture, []byte("123 1 codex /opt/homebrew/bin/codex --model gpt-5\n999 1 zsh zsh\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.run([]string{"contextlattice_agent_discover", "--agents", "codex", "--global-home", globalHome, "--ps-fixture", fixture, "--pretty"}); err != nil {
		t.Fatalf("run discover: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	agents := output["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("expected one agent: %#v", output)
	}
	agent := agents[0].(map[string]any)
	if agent["state_authority"] != "hook" || int(agent["process_count"].(float64)) != 1 {
		t.Fatalf("unexpected discover agent: %#v", agent)
	}
	state := asMap(agent["agent_state"])
	if state["state"] != "working" || state["authority"] != "process_probe" {
		t.Fatalf("unexpected discovered state: %#v", state)
	}
}
func TestTraceCommandRendersTree(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/sessions/sess-trace/trace" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"schema_id": "agent_run_trace.v1",
			"session": map[string]any{
				"id":          "sess-trace",
				"agent":       "codex",
				"status":      "active",
				"objective":   "trace smoke",
				"next_action": "continue",
			},
			"format_contract": map[string]any{"validation": map[string]any{"status": "passed"}},
			"run_shaping": map[string]any{
				"context": map[string]any{"validation": "passed", "prompt_ready": true, "reference_prompt_chars": 120},
				"skills":  map[string]any{"items": []any{}},
				"sources": map[string]any{"returned_sources": []any{"qdrant"}},
			},
			"timeline": []map[string]any{{"phase": "context", "type": "context_pack.completed", "status": "completed", "summary": "packed"}},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_agent_trace", "--session-id", "sess-trace", "--tree"}); err != nil {
		t.Fatalf("run trace: %v", err)
	}
	rendered := stdout.String()
	if !strings.Contains(rendered, "ContextLattice Run Trace") || !strings.Contains(rendered, "sess-trace") {
		t.Fatalf("unexpected trace render:\n%s", rendered)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestAdoptIntegrateCheckValidatesManagedBlocks(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.run([]string{"contextlattice_adopt", "integrate", "--repo", repo, "--agents", "codex,claude-code,hermes-agent,pi,droid", "--project", "smoke", "--pretty"}); err != nil {
		t.Fatalf("run integrate: %v", err)
	}
	stdout.Reset()
	if err := c.run([]string{"contextlattice_adopt", "integrate", "--repo", repo, "--agents", "codex,claude-code,hermes-agent,pi,droid", "--check", "--pretty"}); err != nil {
		t.Fatalf("run integrate check: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["ok"] != true {
		t.Fatalf("expected repo integration check to pass: %#v", output)
	}
	files := output["files"].([]any)
	if len(files) != 5 {
		t.Fatalf("expected five instruction files, got %#v", files)
	}
}

func TestAdoptIntegrateCheckFailsMissingBlocks(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.run([]string{"contextlattice_adopt", "integrate", "--repo", repo, "--agents", "codex,droid", "--check", "--pretty"}); err == nil {
		t.Fatalf("expected integrate check to return an error when blocks are missing")
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["ok"] == true {
		t.Fatalf("expected missing repo integration to fail: %#v", output)
	}
	if findings := output["findings"].([]any); len(findings) == 0 {
		t.Fatalf("expected findings: %#v", output)
	}
}
