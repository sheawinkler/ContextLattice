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
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
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
				"context_pack_quality": map[string]any{
					"schema_id":     "contextlattice_context_pack_quality.v1",
					"sample_id":     "cpq_test_pack",
					"query_hash":    "abc123",
					"quality_score": 88,
				},
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
		case "/v1/agents/sessions/sess-test/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
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
	if firstString(asMap(output["outcome_report"])["sample_id"]) != "cpq_test_pack" {
		t.Fatalf("expected context-pack output to include outcome report, got %#v", output["outcome_report"])
	}
}

func TestSynthesisPackCommandUsesNativeEndpoint(t *testing.T) {
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/memory/synthesis-pack":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode synthesis request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":        true,
				"schema_id": "synthesis_pack.v1",
				"synthesis_pack": map[string]any{
					"schema_id":                "synthesis_pack.v1",
					"high_signal_findings":     []any{map[string]any{"kind": "decision", "text": "ship synthesis"}},
					"semantic_tags":            []any{"synthesis_pack_v1"},
					"synthesis_quality":        map[string]any{"status": "strong"},
					"recommended_next_actions": []any{},
				},
				"context_pack": map[string]any{
					"query":           "native synthesis",
					"ranked_evidence": []any{},
					"token_budget":    map[string]any{"active": true},
				},
				"context_pack_quality": map[string]any{
					"schema_id": "contextlattice_context_pack_quality.v1",
					"sample_id": "cpq_synthesis_test",
				},
				"token_impact": map[string]any{"schema_id": "contextlattice_token_impact.v1"},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_synthesis_pack", "native synthesis", "--project", "alpha", "--mode", "fast", "--raw", "--no-auto-session"}); err != nil {
		t.Fatalf("run synthesis pack: %v", err)
	}
	if captured["native_cli_implementation"] != true {
		t.Fatalf("expected native_cli_implementation marker: %#v", captured)
	}
	if captured["retrieval_mode"] != "fast" {
		t.Fatalf("expected fast retrieval mode, got %#v", captured)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["tool"] != "contextlattice_synthesis_pack" || output["pack_surface"] != "synthesis-pack" {
		t.Fatalf("expected synthesis tool markers, got %#v", output)
	}
	if len(asMap(output["synthesis_pack"])) == 0 {
		t.Fatalf("expected synthesis pack in output, got %#v", output)
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

func TestRunnerQualityCommandUsesNativeTelemetryEndpoint(t *testing.T) {
	var requestedPath string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.String()
		if r.URL.Path != "/telemetry/runner-quality" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_id":    "contextlattice_runner_quality_telemetry.v1",
			"sample_count": 2,
			"recommendations": map[string]any{
				"mode":       "advisor_only",
				"task_class": "scout",
				"top_runner": "pi",
			},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_runner_quality", "--task-class", "scout", "--limit", "12", "--pretty"}); err != nil {
		t.Fatalf("run runner quality: %v", err)
	}
	if !strings.Contains(requestedPath, "task_class=scout") || !strings.Contains(requestedPath, "limit=12") {
		t.Fatalf("runner quality query missing expected filters: %s", requestedPath)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if firstString(asMap(output["recommendations"])["mode"]) != "advisor_only" {
		t.Fatalf("expected advisor-only recommendations, got %#v", output)
	}
}

func TestAdapterBootstrapCompactsPreflightResult(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents/sessions/sess-bootstrap/rollup" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
			return
		}
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

func TestAdapterOutcomePostsCompactProviderUsage(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	var outcomePayload map[string]any
	var eventPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/telemetry/context-pack-quality/outcome":
			if err := json.NewDecoder(r.Body).Decode(&outcomePayload); err != nil {
				t.Fatalf("decode outcome request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"outcome": map[string]any{
					"schema_id":                  "contextlattice_context_pack_outcome.v1",
					"sample_id":                  outcomePayload["sample_id"],
					"first_pass_success":         outcomePayload["first_pass_success"],
					"repair_required":            outcomePayload["repair_required"],
					"retry_count":                outcomePayload["retry_count"],
					"provider_prompt_tokens":     outcomePayload["provider_prompt_tokens"],
					"provider_completion_tokens": outcomePayload["provider_completion_tokens"],
					"provider_total_tokens":      outcomePayload["provider_total_tokens"],
					"outcome_source":             "adapter_outcome",
				},
				"telemetry": map[string]any{"outcome_sample_count": 1, "observed_provider_total_tokens": 789},
			})
		case "/v1/agents/sessions/event":
			if err := json.NewDecoder(r.Body).Decode(&eventPayload); err != nil {
				t.Fatalf("decode event request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": map[string]any{"id": "evt-outcome"}})
		case "/v1/agents/sessions/sess-outcome/rollup":
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
		"contextlattice_agent_adapter", "outcome",
		"--agent", "codex",
		"--project", "alpha",
		"--session-id", "sess-outcome",
		"--context-pack-quality-sample-id", "cpq_adapter",
		"--first-pass-success", "true",
		"--repair-required", "false",
		"--retry-count", "0",
		"--followup-tokens", "22",
		"--provider-prompt-tokens", "700",
		"--provider-completion-tokens", "89",
		"--provider-total-tokens", "789",
	}); err != nil {
		t.Fatalf("run adapter outcome: %v", err)
	}
	if outcomePayload["sample_id"] != "cpq_adapter" ||
		outcomePayload["first_pass_success"] != true ||
		outcomePayload["repair_required"] != false ||
		asInt(outcomePayload["provider_total_tokens"]) != 789 {
		t.Fatalf("unexpected outcome payload: %#v", outcomePayload)
	}
	if eventPayload["type"] != "context_pack.outcome_reported" {
		t.Fatalf("expected outcome session event, got %#v", eventPayload)
	}
	metadata := asMap(eventPayload["metadata"])
	if asMap(metadata["outcome"])["provider_total_tokens"] == nil {
		t.Fatalf("expected compact outcome metadata to include provider tokens, got %#v", metadata)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output["command"] != "outcome" || output["ok"] != true {
		t.Fatalf("unexpected outcome output: %#v", output)
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

func TestAgentProcessMatchesPackageManagedHermesExecutables(t *testing.T) {
	patterns := []string{"hermes-agent", "hermes"}
	cases := []struct {
		name    string
		command string
		args    string
		want    bool
	}{
		{
			name:    "homebrew python console script",
			command: "/opt/homebrew/Cellar/python@3.14/3.14.6/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python",
			args:    "/opt/homebrew/Cellar/python@3.14/3.14.6/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python /opt/homebrew/bin/hermes --cli",
			want:    true,
		},
		{
			name:    "nix executable path",
			command: "/nix/store/abc123-hermes-agent/bin/hermes",
			args:    "/nix/store/abc123-hermes-agent/bin/hermes --cli",
			want:    true,
		},
		{
			name:    "macports executable path",
			command: "/opt/local/bin/hermes-agent",
			args:    "/opt/local/bin/hermes-agent --model openai/gpt-5-mini",
			want:    true,
		},
		{
			name:    "python module source launch",
			command: "/Users/example/src/hermes-agent/.venv/bin/python",
			args:    "/Users/example/src/hermes-agent/.venv/bin/python -m hermes_cli.main --cli",
			want:    true,
		},
		{
			name:    "uvx runner launch",
			command: "/opt/homebrew/bin/uvx",
			args:    "uvx --from hermes-agent hermes --cli",
			want:    true,
		},
		{
			name:    "hermes ultra binary is separate",
			command: "/Users/example/.cargo/bin/hermes-agent-ultra",
			args:    "/Users/example/.cargo/bin/hermes-agent-ultra",
			want:    false,
		},
		{
			name:    "hermes ultra python worker path is separate",
			command: "/opt/homebrew/bin/python3.14",
			args:    "/opt/homebrew/bin/python3.14 /Users/example/Projects/hermes-agent-ultra/scripts/upstream_webhook_sync.py worker --repo-root /Users/example/Projects/hermes-agent-ultra",
			want:    false,
		},
		{
			name:    "shell command text is not process identity",
			command: "/bin/zsh",
			args:    "zsh -lc contextlattice_agent_discover --agents hermes-agent",
			want:    false,
		},
		{
			name:    "doctor argument text is not process identity",
			command: "/Users/example/.contextlattice/bin/contextlattice_doctor",
			args:    "contextlattice_doctor --agents hermes-agent --pretty",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentProcessMatches(tc.command, tc.args, patterns); got != tc.want {
				t.Fatalf("agentProcessMatches()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestDiscoverHermesDoesNotCountHermesUltraOrSelfCommands(t *testing.T) {
	fixture := strings.Join([]string{
		"101 1 /opt/homebrew/Cellar/python@3.14/3.14.6/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python /opt/homebrew/Cellar/python@3.14/3.14.6/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python /opt/homebrew/bin/hermes --cli",
		"102 1 /Users/example/.cargo/bin/hermes-agent-ultra /Users/example/.cargo/bin/hermes-agent-ultra",
		"103 1 /opt/homebrew/bin/python3.14 /opt/homebrew/bin/python3.14 /Users/example/Projects/hermes-agent-ultra/scripts/upstream_webhook_sync.py worker --repo-root /Users/example/Projects/hermes-agent-ultra",
		"104 1 /bin/zsh zsh -lc contextlattice_agent_discover --agents hermes-agent",
		"105 1 /Users/example/.contextlattice/bin/contextlattice_doctor contextlattice_doctor --agents hermes-agent --pretty",
	}, "\n")
	processes := discoverAgentProcesses(fixture, []string{"hermes-agent", "hermes"}, 8)
	if len(processes) != 1 {
		t.Fatalf("expected only the real hermes process, got %#v", processes)
	}
	process := processes[0].(map[string]any)
	if firstString(process["pid"]) != "101" {
		t.Fatalf("unexpected process match: %#v", process)
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
	if err := c.run([]string{"contextlattice_adopt", "integrate", "--repo", repo, "--agents", "codex,claude-code,hermes-agent,omp,mercury-agent,pi,droid", "--project", "smoke", "--pretty"}); err != nil {
		t.Fatalf("run integrate: %v", err)
	}
	stdout.Reset()
	if err := c.run([]string{"contextlattice_adopt", "integrate", "--repo", repo, "--agents", "codex,claude-code,hermes-agent,omp,mercury-agent,pi,droid", "--check", "--pretty"}); err != nil {
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
	if len(files) != 6 {
		t.Fatalf("expected six instruction files, got %#v", files)
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
