package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	t.Setenv("HOME", t.TempDir())
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

func TestPackCommandReusesCachedLiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	objective := "continue existing task"
	taskID := derivedAgentTaskID("alpha", objective)
	ownership := adapterOwnership(parsedArgs{})
	ownership["task_id"] = taskID
	reuseKey := agentSessionReuseKey("alpha", "agent-cli", "codex_test", ownership)
	writeSessionStateWithExtras("alpha", "sess-cached", objective, "codex_test", map[string]any{"reuse_key": reuseKey, "ownership": ownership})
	startCalls := 0
	var packPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			startCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "unexpected"}})
		case "/v1/agents/sessions/sess-cached":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-cached", "status": "active", "project": "alpha", "agent_id": "codex_test", "task_id": taskID, "reuse_key": reuseKey,
			}})
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "schema_id": agentPacketContractID, "session_id": "sess-cached",
				"context_pack_quality": map[string]any{"sample_id": "cpq_cached"},
			})
		case "/v1/agents/sessions/sess-cached/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", objective, "--project", "alpha", "--agent-id", "codex_test", "--raw"}); err != nil {
		t.Fatalf("run pack: %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("cached live session caused %d duplicate start calls", startCalls)
	}
	if firstString(packPayload["session_id"]) != "sess-cached" {
		t.Fatalf("expected cached session id in context request, got %#v", packPayload)
	}
	if firstString(packPayload["task_id"]) != taskID {
		t.Fatalf("expected deterministic task id in context request, got %#v", packPayload)
	}
}

func TestPackCommandSeparatesCachedSessionForDifferentTask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	oldObjective := "old release task"
	oldTaskID := derivedAgentTaskID("alpha", oldObjective)
	oldOwnership := adapterOwnership(parsedArgs{})
	oldOwnership["task_id"] = oldTaskID
	oldReuseKey := agentSessionReuseKey("alpha", "agent-cli", "codex_test", oldOwnership)
	writeSessionStateWithExtras("alpha", "sess-old", oldObjective, "codex_test", map[string]any{"reuse_key": oldReuseKey, "ownership": oldOwnership})

	startCalls := 0
	var startPayload, packPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agents/sessions/sess-old":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-old", "status": "active", "project": "alpha", "agent_id": "codex_test", "task_id": oldTaskID, "reuse_key": oldReuseKey,
			}})
		case "/v1/agents/sessions/start":
			startCalls++
			if err := json.NewDecoder(r.Body).Decode(&startPayload); err != nil {
				t.Fatalf("decode session start: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-new", "status": "active", "project": "alpha", "agent_id": "codex_test", "task_id": startPayload["task_id"], "reuse_key": startPayload["reuse_key"],
			}})
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": agentPacketContractID, "session_id": "sess-new"})
		case "/v1/agents/sessions/sess-new/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	objective := "new token truth task"
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", objective, "--project", "alpha", "--agent-id", "codex_test", "--raw"}); err != nil {
		t.Fatalf("run pack: %v", err)
	}
	newTaskID := derivedAgentTaskID("alpha", objective)
	if startCalls != 1 || firstString(startPayload["task_id"]) != newTaskID || newTaskID == oldTaskID {
		t.Fatalf("different task did not create one distinct session: calls=%d start=%#v", startCalls, startPayload)
	}
	if firstString(packPayload["session_id"]) != "sess-new" || firstString(packPayload["task_id"]) != newTaskID {
		t.Fatalf("context request did not use new task session: %#v", packPayload)
	}
}

func TestDerivedAgentTaskIDIsNormalizedAndTaskSpecific(t *testing.T) {
	first := derivedAgentTaskID("ContextLattice", "  Ship   TOKEN truth ")
	second := derivedAgentTaskID("contextlattice", "ship token TRUTH")
	third := derivedAgentTaskID("contextlattice", "ship session truth")
	if first != second || first == third || !strings.HasPrefix(first, "task_") {
		t.Fatalf("unexpected derived task identities: first=%s second=%s third=%s", first, second, third)
	}
}

func TestAgentSessionReuseKeySeparatesBranches(t *testing.T) {
	base := map[string]any{"repo": "contextlattice", "branch": "main", "worktree": "/repo", "cwd": "/repo"}
	other := map[string]any{"repo": "contextlattice", "branch": "release", "worktree": "/repo", "cwd": "/repo"}
	mainKey := agentSessionReuseKey("contextlattice", "codex", "codex_test", base)
	releaseKey := agentSessionReuseKey("contextlattice", "codex", "codex_test", other)
	if mainKey == releaseKey {
		t.Fatalf("branch change did not create a distinct session identity: %s", mainKey)
	}
}

func TestContextPackQualitySampleReadsAgentPacketOutcome(t *testing.T) {
	quality := contextPackQualitySample(map[string]any{
		"schema_id": agentPacketContractID,
		"outcome":   map[string]any{"sample_id": "cpq_packet"},
	})
	if firstString(quality["sample_id"]) != "cpq_packet" {
		t.Fatalf("agent packet outcome did not expose pending quality sample: %#v", quality)
	}
}

func TestPendingContextOutcomeIsSessionScopedAndBounded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	recordContextPackQualityPending("alpha", "sess-a", "task a", "codex_test", map[string]any{"sample_id": "cpq-a"})
	recordContextPackQualityPending("alpha", "sess-b", "task b", "codex_test", map[string]any{"sample_id": "cpq-b"})
	parsedA := parsedArgs{values: map[string]string{"session_id": "sess-a"}}
	parsedB := parsedArgs{values: map[string]string{"session_id": "sess-b"}}
	if got := resolvePendingContextPackQualitySampleID(parsedA, "alpha"); got != "cpq-a" {
		t.Fatalf("session A resolved wrong pending sample: %q", got)
	}
	if got := resolvePendingContextPackQualitySampleID(parsedB, "alpha"); got != "cpq-b" {
		t.Fatalf("session B resolved wrong pending sample: %q", got)
	}
	markContextPackQualityReported("alpha", "sess-a", map[string]any{"outcome_id": "outcome-a"})
	if got := resolvePendingContextPackQualitySampleID(parsedA, "alpha"); got != "" {
		t.Fatalf("reported session A sample remained pending: %q", got)
	}
	if got := resolvePendingContextPackQualitySampleID(parsedB, "alpha"); got != "cpq-b" {
		t.Fatalf("reporting session A retired session B sample: %q", got)
	}
	for index := 0; index < 40; index++ {
		recordContextPackQualityPending("alpha", fmt.Sprintf("sess-%02d", index), "bounded task", "codex_test", map[string]any{"sample_id": fmt.Sprintf("cpq-%02d", index)})
	}
	bySession := asMap(readSessionState("alpha")["pending_context_pack_quality_by_session"])
	if len(bySession) != 32 {
		t.Fatalf("pending session sample map is not bounded: %d", len(bySession))
	}
}

func TestUnifiedContextSeedsAutomaticFinishOutcomeFromAgentPacket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	var contextPayload, outcomePayload map[string]any
	eventTypes := []string{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			var startPayload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&startPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-packet-finish", "status": "active", "project": "alpha", "agent_id": startPayload["agent_id"],
				"task_id": startPayload["task_id"], "reuse_key": startPayload["reuse_key"],
			}})
		case "/memory/synthesis-pack/v2":
			_ = json.NewDecoder(r.Body).Decode(&contextPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "schema_id": agentPacketContractID, "session_id": "sess-packet-finish",
				"outcome":      map[string]any{"sample_id": "cpq-packet-finish", "command": "contextlattice finish"},
				"token_impact": map[string]any{"transport_tokens_exact": 100, "tokenizer_exact": true},
			})
		case "/v1/agents/sessions/sess-packet-finish/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		case "/v1/agents/sessions/sess-packet-finish":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-packet-finish", "status": "active", "project": "alpha",
			}})
		case "/telemetry/context-pack-quality/outcome":
			_ = json.NewDecoder(r.Body).Decode(&outcomePayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": map[string]any{
				"schema_id": "contextlattice_context_pack_outcome.v1", "outcome_id": "outcome-packet-finish", "sample_id": "cpq-packet-finish",
			}})
		case "/v1/agents/sessions/event":
			var event map[string]any
			_ = json.NewDecoder(r.Body).Decode(&event)
			eventTypes = append(eventTypes, firstString(event["type"]))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "sess-packet-finish"}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "context", "packet outcome bridge", "--project", "alpha", "--raw"}); err != nil {
		t.Fatalf("context command: %v", err)
	}
	var packet map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &packet); err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if firstString(asMap(packet["outcome"])["sample_id"]) != "cpq-packet-finish" || packet["outcome_report"] != nil {
		t.Fatalf("CLI mutated finalized packet outcome surface: %#v", packet)
	}
	if firstString(contextPayload["task_id"]) == "" {
		t.Fatalf("context request omitted task identity: %#v", contextPayload)
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice", "finish", "packet outcome complete", "--success", "--project", "alpha", "--raw"}); err != nil {
		t.Fatalf("finish command: %v output=%s", err, stdout.String())
	}
	if firstString(outcomePayload["sample_id"]) != "cpq-packet-finish" || !asBool(outcomePayload["first_pass_success"]) || asBool(outcomePayload["repair_required"]) {
		t.Fatalf("packet outcome did not seed automatic finish: %#v", outcomePayload)
	}
	if len(eventTypes) != 2 || eventTypes[0] != "context_pack.outcome_reported" || eventTypes[1] != "session.completed" {
		t.Fatalf("packet outcome lifecycle order is wrong: %#v", eventTypes)
	}
	var finished map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &finished); err != nil {
		t.Fatalf("decode finish output: %v", err)
	}
	if firstString(finished["outcome_mode"]) != "automatic_success" {
		t.Fatalf("expected automatic packet outcome, got %#v", finished)
	}
}

func TestUnifiedFinishAutomaticallyReportsPendingContextOutcome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	writeSessionStateWithExtras("alpha", "sess-finish", "finish task", "codex_test", map[string]any{
		"latest_context_pack_quality": map[string]any{"sample_id": "cpq_finish", "reported": false},
	})
	var outcomePayload map[string]any
	eventTypes := []string{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/telemetry/context-pack-quality/outcome":
			if err := json.NewDecoder(r.Body).Decode(&outcomePayload); err != nil {
				t.Fatalf("decode outcome request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"outcome": map[string]any{"schema_id": "contextlattice_context_pack_outcome.v1", "outcome_id": "outcome_finish", "sample_id": "cpq_finish", "first_pass_success": true, "repair_required": false},
			})
		case "/v1/agents/sessions/event":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode session event: %v", err)
			}
			eventTypes = append(eventTypes, firstString(payload["type"]))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "sess-finish"}})
		case "/v1/agents/sessions/sess-finish/rollup":
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
		"contextlattice", "finish", "--session-id", "sess-finish", "--project", "alpha",
		"--agent", "codex", "--agent-id", "codex_test", "--summary", "verified complete", "--raw",
	}); err != nil {
		t.Fatalf("adapter complete: %v output=%s", err, stdout.String())
	}
	if !asBool(outcomePayload["first_pass_success"]) || asBool(outcomePayload["repair_required"]) || firstString(outcomePayload["sample_id"]) != "cpq_finish" {
		t.Fatalf("automatic finish outcome is not a first-pass success: %#v", outcomePayload)
	}
	if len(eventTypes) != 2 || eventTypes[0] != "context_pack.outcome_reported" || eventTypes[1] != "session.completed" {
		t.Fatalf("outcome was not bound before terminal completion: %#v", eventTypes)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if firstString(output["outcome_mode"]) != "automatic_success" {
		t.Fatalf("expected automatic outcome mode, got %#v", output)
	}
	quality := asMap(readSessionState("alpha")["latest_context_pack_quality"])
	if !asBool(quality["reported"]) || firstString(quality["outcome_id"]) != "outcome_finish" {
		t.Fatalf("pending outcome was not retired after durable report: %#v", quality)
	}
}

func TestUnifiedLifecycleReceiptsStayCompactAndFullAdapterOutputRemainsAvailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	huge := strings.Repeat("backend-internal-payload-", 3000)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/memory/write":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": "write-compact", "internal": huge})
		case "/v1/agents/sessions/event":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			status := firstString(payload["status"], "active")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"event": map[string]any{
					"id": "evt-compact", "type": payload["type"], "status": status, "created_at": "2026-07-12T00:00:00Z",
				},
				"rollup": map[string]any{"internal": huge},
			})
		case "/v1/agents/sessions/sess-compact/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "remember", "bounded checkpoint", "--session-id", "sess-compact", "--project", "alpha", "--pretty"}); err != nil {
		t.Fatalf("compact remember: %v output=%s", err, stdout.String())
	}
	if stdout.Len() > 2000 || strings.Contains(stdout.String(), huge[:100]) {
		t.Fatalf("remember receipt leaked oversized backend data: bytes=%d", stdout.Len())
	}
	rememberBytes := stdout.Len()
	var remember map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &remember); err != nil {
		t.Fatalf("decode remember receipt: %v", err)
	}
	if firstString(remember["schema_id"]) != "contextlattice_lifecycle_receipt.v1" || firstString(remember["command"]) != "remember" || firstString(asMap(remember["event"])["id"]) != "evt-compact" {
		t.Fatalf("unexpected remember receipt: %#v", remember)
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice", "finish", "verified compact completion", "--session-id", "sess-compact", "--project", "alpha", "--success", "--pretty"}); err != nil {
		t.Fatalf("compact finish: %v output=%s", err, stdout.String())
	}
	if stdout.Len() > 2000 || strings.Contains(stdout.String(), huge[:100]) {
		t.Fatalf("finish receipt leaked oversized backend data: bytes=%d", stdout.Len())
	}
	finishBytes := stdout.Len()
	var finish map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &finish); err != nil {
		t.Fatalf("decode finish receipt: %v", err)
	}
	if firstString(finish["schema_id"]) != "contextlattice_lifecycle_receipt.v1" || firstString(finish["status"]) != "completed" || firstString(finish["outcome_mode"]) != "skipped_no_pending_sample" {
		t.Fatalf("unexpected finish receipt: %#v", finish)
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice", "finish", "full completion", "--session-id", "sess-compact", "--project", "alpha", "--full", "--raw"}); err != nil {
		t.Fatalf("full unified finish: %v output=%s", err, stdout.String())
	}
	var full map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &full); err != nil {
		t.Fatalf("decode full unified finish: %v", err)
	}
	if firstString(full["schema_id"]) != "universal_agent_adapter_response.v1" || len(asMap(full["adapter_contract"])) == 0 || len(asMap(full["result"])) == 0 {
		t.Fatalf("--full did not preserve adapter response: %#v", full)
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice_agent_adapter", "complete", "--summary", "adapter completion", "--session-id", "sess-compact", "--project", "alpha", "--raw"}); err != nil {
		t.Fatalf("advanced adapter complete: %v output=%s", err, stdout.String())
	}
	var adapter map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &adapter); err != nil {
		t.Fatalf("decode advanced adapter output: %v", err)
	}
	if firstString(adapter["schema_id"]) != "universal_agent_adapter_response.v1" || len(asMap(adapter["adapter_contract"])) == 0 {
		t.Fatalf("advanced adapter output was compacted unexpectedly: %#v", adapter)
	}
	t.Logf("remember_receipt_bytes=%d finish_receipt_bytes=%d injected_backend_bytes=%d", rememberBytes, finishBytes, len(huge))
}

func TestUnifiedContextAndResumeCommandsUseCompactContracts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var contextPayload map[string]any
	resumeCompact := false
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/memory/synthesis-pack/v2":
			if err := json.NewDecoder(r.Body).Decode(&contextPayload); err != nil {
				t.Fatalf("decode context request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": agentPacketContractID, "surface": "synthesis_pack_v2"})
		case "/v1/agents/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessions": []any{map[string]any{"id": "sess-resume", "status": "active", "project": "alpha"}}})
		case "/v1/agents/sessions/sess-resume/context-package":
			resumeCompact = r.URL.Query().Get("view") == "compact"
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": agentPacketContractID, "surface": "session_resume", "session_id": "sess-resume", "project": "alpha"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var contextOut bytes.Buffer
	c := newCLI(&contextOut, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "context", "prove current task", "--project", "alpha", "--no-auto-session", "--raw"}); err != nil {
		t.Fatalf("context command: %v", err)
	}
	if firstString(contextPayload["output_mode"]) != agentPacketContractID || asInt(contextPayload["hard_limit_tokens"]) != defaultAgentPacketHardTokens {
		t.Fatalf("unified context did not request compact proof synthesis: %#v", contextPayload)
	}

	var resumeOut bytes.Buffer
	c = newCLI(&resumeOut, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "resume", "--project", "alpha", "--raw"}); err != nil {
		t.Fatalf("resume command: %v", err)
	}
	if !resumeCompact {
		t.Fatalf("resume did not request compact session packet")
	}
}

func TestUnifiedCorrectSeparatesFeedbackFromFactualClaimMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	writeSessionStateWithExtras("alpha", "sess-correct", "correct task", "codex_test", map[string]any{
		"latest_context_pack_quality": map[string]any{"sample_id": "cpq_correct", "reported": false},
	})
	var feedbackPayload map[string]any
	var claimPayload map[string]any
	var outcomePayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tools/feedback_submit":
			_ = json.NewDecoder(r.Body).Decode(&feedbackPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "feedback": map[string]any{"id": "feedback-correct"}})
		case "/memory/claims":
			_ = json.NewDecoder(r.Body).Decode(&claimPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "claim": map[string]any{"claim_id": "claim_new"}})
		case "/telemetry/context-pack-quality/outcome":
			_ = json.NewDecoder(r.Body).Decode(&outcomePayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": map[string]any{"outcome_id": "outcome_correct", "sample_id": "cpq_correct"}})
		case "/v1/agents/sessions/event":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "sess-correct"}})
		case "/v1/agents/sessions/sess-correct/rollup":
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
		"contextlattice", "correct", "The current release is v4", "--category", "wrong", "--factual",
		"--subject", "public release", "--predicate", "version", "--object", "v4", "--target-claim-id", "claim_old",
		"--session-id", "sess-correct", "--project", "alpha", "--agent-id", "codex_test", "--raw",
	}); err != nil {
		t.Fatalf("correct command: %v output=%s", err, stdout.String())
	}
	metadata := asMap(feedbackPayload["metadata"])
	if !asBool(metadata["factual"]) || firstString(metadata["category"]) != "wrong" {
		t.Fatalf("correction feedback lost category boundary: %#v", feedbackPayload)
	}
	if values := firstList(claimPayload["contradicts"]); len(values) != 1 || firstString(values[0]) != "claim_old" || len(firstList(claimPayload["supersedes"])) != 0 {
		t.Fatalf("wrong factual correction did not create explicit contradiction: %#v", claimPayload)
	}
	if asBool(outcomePayload["first_pass_success"]) || !asBool(outcomePayload["repair_required"]) {
		t.Fatalf("negative correction did not train retrieval outcome: %#v", outcomePayload)
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
	if captured["output_mode"] != agentPacketContractID || asInt(captured["hard_limit_tokens"]) != defaultAgentPacketHardTokens {
		t.Fatalf("expected compact agent packet request by default, got %#v", captured)
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

func TestContextCommandNegotiatesDeltaFromTrustedBaseFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	base := map[string]any{
		"schema_id": agentPacketContractID,
		"packet_identity": map[string]any{
			"packet_id":        "packet_base",
			"transport_digest": "sha256:base",
			"revision":         7,
			"ack_cursor":       "ack_base",
		},
	}
	basePath := filepath.Join(t.TempDir(), "base.json")
	baseRaw, _ := json.Marshal(base)
	if err := os.WriteFile(basePath, baseRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	var captured map[string]any
	delta := map[string]any{
		"ok": true, "schema_id": agentPacketDeltaContractID, "version": 1,
		"base_packet_id": "packet_base", "operations": []any{},
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/synthesis-pack/v2" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode delta request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(delta)
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "context", "continue packet task", "--project", "alpha", "--base-packet-file", basePath, "--no-auto-session", "--raw"}); err != nil {
		t.Fatalf("run context delta: %v", err)
	}
	if firstString(captured["packet_mode"]) != "delta" || firstString(captured["base_packet_id"]) != "packet_base" || firstString(captured["base_digest"]) != "sha256:base" || asInt(captured["base_revision"]) != 7 || firstString(captured["base_ack_cursor"]) != "ack_base" {
		t.Fatalf("delta negotiation fields missing: %#v", captured)
	}
	if firstString(asMap(captured["base_packet"])["schema_id"]) != agentPacketContractID {
		t.Fatalf("trusted base packet missing from request: %#v", captured)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode delta output: %v", err)
	}
	if firstString(output["schema_id"]) != agentPacketDeltaContractID || output["tool"] != nil || output["task_summary"] != nil {
		t.Fatalf("CLI mutated delta wire envelope: %#v", output)
	}
}

func TestPacketReconstructCommandEmitsVerifiedPacket(t *testing.T) {
	tempDir := t.TempDir()
	base := map[string]any{"schema_id": agentPacketContractID, "packet_identity": map[string]any{"packet_id": "packet_base"}}
	delta := map[string]any{"schema_id": agentPacketDeltaContractID, "packet_id": "packet_result"}
	basePath := filepath.Join(tempDir, "base.json")
	deltaPath := filepath.Join(tempDir, "delta.json")
	for path, payload := range map[string]map[string]any{basePath: base, deltaPath: delta} {
		raw, _ := json.Marshal(payload)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var captured map[string]any
	packet := map[string]any{"ok": true, "schema_id": agentPacketContractID, "packet_identity": map[string]any{"packet_id": "packet_result"}}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/agent-packet/reconstruct" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode reconstruction request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "schema_id": agentPacketReconstructionID, "verified": true, "packet": packet,
		})
	}))
	defer gateway.Close()

	commands := map[string][]string{
		"primary": {"contextlattice", "packet-reconstruct", "--base-packet-file", basePath, "--delta-file", deltaPath, "--raw"},
		"alias":   {"contextlattice_packet_reconstruct", "--base-packet-file", basePath, "--delta-file", deltaPath, "--raw"},
	}
	for name, argv := range commands {
		t.Run(name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run(argv); err != nil {
				t.Fatalf("run packet reconstruction: %v", err)
			}
			if firstString(asMap(captured["base_packet"])["schema_id"]) != agentPacketContractID || firstString(asMap(captured["delta"])["schema_id"]) != agentPacketDeltaContractID {
				t.Fatalf("reconstruction payload lost packet inputs: %#v", captured)
			}
			var output map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("decode reconstructed packet: %v", err)
			}
			if firstString(output["schema_id"]) != agentPacketContractID || firstString(asMap(output["packet_identity"])["packet_id"]) != "packet_result" {
				t.Fatalf("default reconstruction output is not the verified packet: %#v", output)
			}
		})
	}
}

func TestAgentPacketCLIFileBoundaryRejectsOversizeAndWrongSchema(t *testing.T) {
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), maxAgentPacketCLIFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedJSONObject(oversized, agentPacketContractID); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized packet was not rejected: %v", err)
	}
	wrong := filepath.Join(t.TempDir(), "wrong.json")
	if err := os.WriteFile(wrong, []byte(`{"schema_id":"other.v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedJSONObject(wrong, agentPacketContractID); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("wrong-schema packet was not rejected: %v", err)
	}
}

func TestCognitionProofCommandsUseNativeEndpoints(t *testing.T) {
	captured := map[string]map[string]any{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode %s request: %v", r.URL.Path, err)
		}
		captured[r.URL.Path] = payload
		switch r.URL.Path {
		case "/memory/synthesis-pack/v2":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "synthesis_pack.v2", "synthesis_pack": map[string]any{"schema_id": "synthesis_pack.v2", "proof_claims": []any{}}, "context_pack": map[string]any{"query": "proof", "ranked_evidence": []any{}}})
		case "/memory/retrieval/plan":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "retrieval_plan.v1", "mode": "advisor", "activation_state": "shadow_only"})
		case "/memory/claims":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "temporal_claim.v1", "recorded": true, "claim": map[string]any{"claim_id": "claim_test"}})
		case "/memory/claims/query":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "temporal_claim_query.v1", "claim_count": 1, "claims": []any{map[string]any{"claim_id": "claim_test"}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"synthesis-v2", []string{"contextlattice_synthesis_pack_v2", "proof", "--project", "alpha", "--raw", "--no-auto-session"}, "/memory/synthesis-pack/v2"},
		{"retrieval-plan", []string{"contextlattice_retrieval_plan", "debug retrieval", "--project", "alpha", "--raw", "--no-auto-session"}, "/memory/retrieval/plan"},
		{"claim-write", []string{"contextlattice_claim_write", "--project", "alpha", "--subject", "release", "--predicate", "current_version", "--object", "3.12.0", "--raw"}, "/memory/claims"},
		{"claim-query", []string{"contextlattice_claim_query", "current release", "--project", "alpha", "--raw"}, "/memory/claims/query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run(tc.args); err != nil {
				t.Fatalf("run %s: %v", tc.name, err)
			}
			if _, ok := captured[tc.path]; !ok {
				t.Fatalf("expected request to %s, captured=%#v", tc.path, captured)
			}
		})
	}
	if captured["/memory/synthesis-pack/v2"]["native_cli_implementation"] != true {
		t.Fatalf("expected native CLI marker on v2 synthesis: %#v", captured["/memory/synthesis-pack/v2"])
	}
	if captured["/memory/claims"]["subject"] != "release" || captured["/memory/claims"]["object"] != "3.12.0" {
		t.Fatalf("expected structured claim payload: %#v", captured["/memory/claims"])
	}
}

func TestContinuityCommandsUseNativeEndpoints(t *testing.T) {
	captured := map[string]map[string]any{}
	queries := map[string]url.Values{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.Method + " " + r.URL.Path
		payload := map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode %s: %v", key, err)
			}
		}
		captured[key] = payload
		queries[key] = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "test.v1"})
	}))
	defer gateway.Close()

	cases := []struct {
		name string
		args []string
		key  string
	}{
		{
			name: "continuity-reconcile",
			args: []string{"contextlattice_continuity_reconcile", "Ship continuity identity", "--project", "alpha", "--repo", "repo", "--task-id", "T1", "--branch", "main", "--idempotency-key", "identity-t1", "--raw"},
			key:  "POST /memory/continuity/reconcile",
		},
		{
			name: "objective-transition",
			args: []string{"contextlattice_objective_transition", "Ship T1", "--project", "alpha", "--objective-id", "obj_t1", "--transition-id", "ot_t1", "--idempotency-key", "objective-t1", "--type", "started", "--actor", "codex", "--outcome-id", "out_t1", "--checkpoint-id", "checkpoint_t1", "--raw"},
			key:  "POST /memory/objectives/transition",
		},
		{
			name: "objective-graph",
			args: []string{"contextlattice_objective_graph", "--project", "alpha", "--objective-id", "obj_t1", "--as-of", "2026-07-13T12:00:00Z", "--no-transitions", "--raw"},
			key:  "GET /memory/objectives/graph",
		},
		{
			name: "decision-change",
			args: []string{"contextlattice_decision_change", "--project", "alpha", "--objective-id", "obj_t1", "--decision-change-id", "dc_t1", "--idempotency-key", "decision-t1", "--before", "old", "--after", "new", "--confidence-before", "0.4", "--confidence-after", "0.8", "--evidence", "eval:case", "--actor", "codex", "--rationale", "holdout changed", "--reason-code", "new_evidence", "--raw"},
			key:  "POST /memory/decision-changes",
		},
		{
			name: "decision-change-list",
			args: []string{"contextlattice_decision_change", "list", "--project", "alpha", "--objective-id", "obj_t1", "--cursor", "cursor-test", "--raw"},
			key:  "GET /memory/decision-changes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run(tc.args); err != nil {
				t.Fatalf("run %s: %v", tc.name, err)
			}
			if _, ok := captured[tc.key]; !ok {
				t.Fatalf("missing %s call: captured=%#v", tc.key, captured)
			}
		})
	}
	if payload := captured["POST /memory/continuity/reconcile"]; payload["objective"] != "Ship continuity identity" || payload["task_id"] != "T1" || payload["idempotency_key"] != "identity-t1" {
		t.Fatalf("continuity payload mismatch: %#v", payload)
	}
	if payload := captured["POST /memory/objectives/transition"]; payload["transition_type"] != "started" || payload["actor"] != "codex" ||
		payload["transition_id"] != "ot_t1" || payload["idempotency_key"] != "objective-t1" ||
		payload["outcome_id"] != "out_t1" || payload["checkpoint_id"] != "checkpoint_t1" {
		t.Fatalf("objective transition payload mismatch: %#v", payload)
	}
	if query := queries["GET /memory/objectives/graph"]; query.Get("objective_id") != "obj_t1" || query.Get("include_transitions") != "false" {
		t.Fatalf("objective graph query mismatch: %#v", query)
	}
	if payload := captured["POST /memory/decision-changes"]; payload["reason_code"] != "new_evidence" || len(asList(payload["trigger_evidence"])) != 1 ||
		payload["decision_change_id"] != "dc_t1" || payload["idempotency_key"] != "decision-t1" {
		t.Fatalf("decision change payload mismatch: %#v", payload)
	}
	if query := queries["GET /memory/decision-changes"]; query.Get("cursor") != "cursor-test" {
		t.Fatalf("decision change cursor missing: %#v", query)
	}
	var generatedStdout bytes.Buffer
	generatedCLI := newCLI(&generatedStdout, ioDiscard{})
	generatedCLI.baseURL = gateway.URL
	if err := generatedCLI.run([]string{
		"contextlattice_objective_transition", "Auto-keyed transition", "--project", "alpha",
		"--objective-id", "obj_auto_key", "--type", "started", "--actor", "codex", "--raw",
	}); err != nil {
		t.Fatalf("run auto-keyed objective transition: %v", err)
	}
	if key := strings.TrimSpace(fmt.Sprint(captured["POST /memory/objectives/transition"]["idempotency_key"])); key == "" {
		t.Fatalf("CLI did not create an idempotency key: %#v", captured["POST /memory/objectives/transition"])
	}
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_continuity_reconcile", "--operation", "compact", "--actor", "codex",
		"--reason", "canonical ledger rewrite", "--project", "alpha", "--raw",
	}); err != nil {
		t.Fatalf("run continuity compaction: %v", err)
	}
	if payload := captured["POST /memory/continuity/reconcile"]; payload["operation"] != "compact" ||
		payload["actor"] != "codex" || payload["reason"] != "canonical ledger rewrite" {
		t.Fatalf("continuity compaction payload mismatch: %#v", payload)
	}
}

func TestOutcomePolicyAndSkillFoundryCommandsUseNativeEndpoints(t *testing.T) {
	captured := map[string]map[string]any{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode %s: %v", r.URL.Path, err)
			}
		}
		captured[r.URL.Path] = payload
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "test.v1"})
	}))
	defer gateway.Close()

	payloadPath := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(payloadPath, []byte(`{"workflow_runs":[],"holdouts":[],"control":{},"canary":{}}`), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	cases := []struct {
		name string
		args []string
		path string
	}{
		{"policy-candidate", []string{"contextlattice_policy_candidate", "--project", "alpha", "--minimum-outcomes", "20", "--raw"}, "/memory/context-policy/candidate"},
		{"policy-evaluate", []string{"contextlattice_policy_evaluate", "--candidate-id", "ctxpol_test", "--payload-file", payloadPath, "--apply-transition", "--raw"}, "/memory/context-policy/evaluate"},
		{"skill-draft", []string{"contextlattice_skill_draft", "--payload-file", payloadPath, "--name", "bounded-proof", "--description", "Bounded proof", "--raw"}, "/memory/skills/foundry/draft"},
		{"skill-evaluate", []string{"contextlattice_skill_evaluate", "--draft-id", "skilldraft_test", "--payload-file", payloadPath, "--raw"}, "/memory/skills/foundry/evaluate"},
		{"skill-export", []string{"contextlattice_skill_export", "--draft-id", "skilldraft_test", "--human-approved", "--approver", "owner", "--raw"}, "/memory/skills/foundry/export"},
		{"skill-retire", []string{"contextlattice_skill_retire", "--draft-id", "skilldraft_test", "--operator", "owner", "--reason", "smoke complete", "--raw"}, "/memory/skills/foundry/retire"},
		{"policy-status", []string{"contextlattice_policy_status", "--raw"}, "/telemetry/context-policy"},
		{"foundry-status", []string{"contextlattice_skill_foundry_status", "--raw"}, "/telemetry/skills/foundry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run(tc.args); err != nil {
				t.Fatalf("run %s: %v", tc.name, err)
			}
			if _, ok := captured[tc.path]; !ok {
				t.Fatalf("expected %s, captured=%#v", tc.path, captured)
			}
		})
	}
	if !asBool(captured["/memory/context-policy/evaluate"]["apply_transition"]) {
		t.Fatalf("expected explicit transition flag: %#v", captured["/memory/context-policy/evaluate"])
	}
	if !asBool(captured["/memory/skills/foundry/export"]["human_approved"]) {
		t.Fatalf("expected explicit human approval: %#v", captured["/memory/skills/foundry/export"])
	}
}

func TestMemoryGraphRepairAndEfficacyCommandsUseBoundedNativeEndpoints(t *testing.T) {
	captured := map[string][]map[string]any{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode %s: %v", r.URL.Path, err)
			}
		}
		captured[r.URL.Path] = append(captured[r.URL.Path], payload)
		switch r.URL.Path {
		case "/memory/recall/eval-cases/refresh":
			graphCaseCount := 2
			if firstString(payload["project"]) == "empty-graph" {
				graphCaseCount = 0
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "savedCaseSet": map[string]any{"graphCaseCount": graphCaseCount},
			})
		case "/memory/recall/evaluate/saved":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "passed": true,
				"metrics": map[string]any{"directPassed": true, "graphEfficacyStatus": "passed", "graphContribution": map[string]any{"status": "passed"}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer gateway.Close()

	for _, args := range [][]string{
		{"contextlattice_memory_graph_repair", "--project", "alpha", "--max-writes", "25", "--raw"},
		{"contextlattice_memory_graph_efficacy", "--refresh-cases", "--project", "alpha", "--graph-max-cases", "2", "--raw"},
	} {
		var stdout bytes.Buffer
		c := newCLI(&stdout, ioDiscard{})
		c.baseURL = gateway.URL
		if err := c.run(args); err != nil {
			t.Fatalf("run %s: %v output=%s", args[0], err, stdout.String())
		}
	}
	repair := captured["/v1/memory/edges/backfill"][0]
	if !asBool(repair["dry_run"]) || asInt(repair["max_writes"]) != 25 || asBool(repair["include_inferred"]) {
		t.Fatalf("expected dry-run identity-first repair payload: %#v", repair)
	}
	refresh := captured["/memory/recall/eval-cases/refresh"][0]
	if !asBool(refresh["include_graph_cases"]) || asInt(refresh["graph_max_cases"]) != 2 {
		t.Fatalf("expected graph-aware refresh payload: %#v", refresh)
	}
	if len(captured["/memory/recall/evaluate/saved"]) != 1 {
		t.Fatalf("expected one saved evaluation request: %#v", captured)
	}

	var failedStdout bytes.Buffer
	failedCLI := newCLI(&failedStdout, ioDiscard{})
	failedCLI.baseURL = gateway.URL
	if err := failedCLI.run([]string{"contextlattice_memory_graph_efficacy", "--refresh-cases", "--project", "empty-graph", "--raw"}); err == nil || !strings.Contains(err.Error(), "no graph holdouts") {
		t.Fatalf("graph-free refresh must fail before evaluation, got err=%v output=%s", err, failedStdout.String())
	}
	if len(captured["/memory/recall/evaluate/saved"]) != 1 {
		t.Fatalf("failed refresh must not evaluate stale cases: %#v", captured)
	}

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_memory_graph_repair", "--project", "alpha", "--write", "--raw"}); err == nil || !strings.Contains(err.Error(), "confirm-project") {
		t.Fatalf("write mode must require exact project confirmation, got %v", err)
	}
}

func TestPassportAndMeshCommandsUseNativeEndpoints(t *testing.T) {
	captured := map[string][]map[string]any{}
	passport := map[string]any{"schema_id": "context_passport.v1", "passport_id": "passport_test", "project": "alpha"}
	envelope := map[string]any{"schema_id": "context_mesh_envelope.v1", "envelope_id": "mesh_test", "project": "alpha"}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode %s: %v", r.URL.Path, err)
			}
		}
		captured[r.URL.Path] = append(captured[r.URL.Path], payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "schema_id": "test.v1", "passport": passport, "envelope": envelope,
		})
	}))
	defer gateway.Close()

	temp := t.TempDir()
	passportFile := filepath.Join(temp, "passport.json")
	envelopeFile := filepath.Join(temp, "envelope.json")
	if raw, err := json.Marshal(passport); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(passportFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := json.Marshal(envelope); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(envelopeFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	passportOutput := filepath.Join(temp, "passport-output.json")
	meshOutput := filepath.Join(temp, "mesh-output.json")
	if err := os.WriteFile(passportOutput, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		path string
	}{
		{"passport-export", []string{"contextlattice_passport_export", "portable task", "--project", "alpha", "--output", passportOutput, "--raw"}, "/memory/context-passport/export"},
		{"passport-verify", []string{"contextlattice_passport_verify", "--file", passportFile, "--raw"}, "/memory/context-passport/verify"},
		{"passport-import", []string{"contextlattice_passport_import", "--file", passportFile, "--project", "alpha", "--raw"}, "/memory/context-passport/import"},
		{"passport-diff", []string{"contextlattice_passport_diff", "--base-file", passportFile, "--target-file", passportFile, "--raw"}, "/memory/context-passport/diff"},
		{"passport-replay", []string{"contextlattice_passport_replay", "--file", passportFile, "--agent-id", "codex", "--raw"}, "/memory/context-passport/replay"},
		{"passport-status", []string{"contextlattice_passport_status", "--raw"}, "/telemetry/context-passport"},
		{"mesh-identity", []string{"contextlattice_mesh_identity", "--raw"}, "/memory/context-mesh/identity"},
		{"mesh-grant-list", []string{"contextlattice_mesh_grant", "list", "--raw"}, "/memory/context-mesh/grants"},
		{"mesh-grant-create", []string{"contextlattice_mesh_grant", "create", "--recipient-id", "peer", "--recipient", "age1test", "--projects", "alpha", "--raw"}, "/memory/context-mesh/grants"},
		{"mesh-grant-revoke", []string{"contextlattice_mesh_grant", "revoke", "--grant-id", "grant_test", "--raw"}, "/memory/context-mesh/grants/revoke"},
		{"mesh-export", []string{"contextlattice_mesh_export", "--passport-id", "passport_test", "--grant-ids", "grant_test", "--output", meshOutput, "--raw"}, "/memory/context-mesh/export"},
		{"mesh-import", []string{"contextlattice_mesh_import", "--file", envelopeFile, "--apply", "--raw"}, "/memory/context-mesh/import"},
		{"mesh-status", []string{"contextlattice_mesh_status", "--raw"}, "/telemetry/context-mesh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run(tc.args); err != nil {
				t.Fatalf("run %s: %v", tc.name, err)
			}
			if len(captured[tc.path]) == 0 {
				t.Fatalf("expected %s, captured=%#v", tc.path, captured)
			}
		})
	}
	if !asBool(captured["/memory/context-mesh/import"][0]["apply"]) {
		t.Fatalf("mesh import did not preserve explicit apply: %#v", captured["/memory/context-mesh/import"])
	}
	for _, path := range []string{passportOutput, meshOutput} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %s mode/error: %v %v", path, info, err)
		}
	}
}

func TestPortableArtifactReadRejectsOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((16 << 20) + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPortableArtifact(path, "passport"); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized artifact error = %v", err)
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

func TestRunnerDiscoveryUsesInstalledRootWhenInheritedRootIsStale(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	isolatedDir := t.TempDir()
	if err := os.Chdir(isolatedDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	globalHome := t.TempDir()
	installedRoot := t.TempDir()
	staleRoot := t.TempDir()
	adapterPath := filepath.Join(installedRoot, "scripts", "agent_runners", "pi_runner.py")
	if err := os.MkdirAll(filepath.Dir(adapterPath), 0755); err != nil {
		t.Fatalf("create adapter directory: %v", err)
	}
	if err := os.WriteFile(adapterPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write adapter: %v", err)
	}
	contractPath := filepath.Join(globalHome, "config", "agent_contracts", "agent_output_contracts.json")
	if err := os.MkdirAll(filepath.Dir(contractPath), 0755); err != nil {
		t.Fatalf("create contract directory: %v", err)
	}
	if err := os.WriteFile(contractPath, []byte(`{"contracts":{"runner_capability.v1":{}}}`), 0644); err != nil {
		t.Fatalf("write contract registry: %v", err)
	}
	hookEnv := filepath.Join(globalHome, "agent_hooks.env")
	if err := os.WriteFile(hookEnv, []byte("export CONTEXTLATTICE_REPO_ROOT='"+installedRoot+"'\n"), 0600); err != nil {
		t.Fatalf("write hook environment: %v", err)
	}
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("write fake pi binary: %v", err)
	}
	t.Setenv("PATH", fakeBin)
	t.Setenv("CONTEXTLATTICE_REPO_ROOT", staleRoot)

	runner := runnerDiscoveryMetadata("pi", "", globalHome)
	if !asBool(runner["runner_ready"]) {
		t.Fatalf("expected installed-root fallback to make runner ready: %#v", runner)
	}
	if got := firstString(runner["adapter"]); got != adapterPath {
		t.Fatalf("adapter=%q want=%q", got, adapterPath)
	}
}

func TestRunnerDiscoveryPrefersExplicitRepo(t *testing.T) {
	explicitRepo := t.TempDir()
	roots := runnerContextLatticeRoots(explicitRepo, "")
	if len(roots) == 0 || roots[0] != explicitRepo {
		t.Fatalf("roots=%#v want explicit repo first", roots)
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

func TestContextBoundaryAuditCannotHideUnboundedDuplicatePathContract(t *testing.T) {
	required := []string{
		"/memory/context-pack", "/tools/context_pack", "/v1/agents/preflight", "/v1/codex/preflight",
		"/memory/synthesis-pack/v2", "/tools/synthesis_pack_v2", "/memory/retrieval/plan", "/tools/retrieval_plan",
		"/memory/claims", "/memory/claims/query", "/tools/claim_write", "/tools/claim_query",
		"/memory/continuity/reconcile", "/memory/objectives/transition", "/memory/objectives/graph",
		"policy_context_package", "scripts/agent/contextlattice-pack", "scripts/agent/compaction-handoff-payload",
		"contextlattice_synthesis_pack_v2", "contextlattice_retrieval_plan", "contextlattice_claim_write", "contextlattice_claim_query",
		"contextlattice_continuity_reconcile", "contextlattice_objective_transition", "contextlattice_objective_graph",
		"contextlattice_decision_change", "contextlattice_decision_change list", "contextlattice_async_inbox_drain",
		"scripts/agent_hooks/contextlattice_pre_compaction_write.sh", "scripts/agent_hooks/contextlattice_post_compaction_read.sh",
	}
	contracts := map[string]string{
		"/memory/continuity/reconcile":        "task_identity_reconciliation.v1",
		"/memory/objectives/transition":       "objective_transition.v1",
		"/memory/objectives/graph":            "objective_graph.v1",
		"contextlattice_continuity_reconcile": "task_identity_reconciliation.v1",
		"contextlattice_objective_transition": "objective_transition.v1",
		"contextlattice_objective_graph":      "objective_graph.v1",
		"contextlattice_decision_change":      "decision_change.v1",
		"contextlattice_decision_change list": "decision_change_query.v1",
	}
	metadataFields := []any{"contract_valid", "truncated", "omitted_counts", "actual_json_bytes", "max_total_json_bytes", "max_string_bytes", "max_list_items"}
	row := func(path string, contractID string, bounded bool) map[string]any {
		return map[string]any{
			"path": path, "name": path, "contract_id": contractID, "bounded": bounded,
			"max_total_json_bytes": 1000, "max_string_bytes": 100, "max_list_items": 10,
			"metadata_fields": metadataFields,
		}
	}
	routes := []any{}
	for _, path := range required {
		routes = append(routes, row(path, firstNonEmpty(contracts[path], "test.v1"), true))
	}
	routes = append(routes,
		row("/memory/decision-changes", "decision_change.v1", true),
		row("/memory/decision-changes", "decision_change_query.v1", false),
	)
	audit := auditContextBoundary(map[string]any{
		"schema_id": "contextlattice_context_boundary.v1", "ok": true, "status": "healthy",
		"violationCount": 0, "routes": routes,
	})
	if asBool(audit["ok"]) {
		t.Fatalf("duplicate-path query contract hid an unbounded surface: %#v", audit)
	}
	found := false
	findings, ok := audit["findings"].([]map[string]any)
	if !ok {
		t.Fatalf("unexpected findings type: %#v", audit["findings"])
	}
	for _, finding := range findings {
		if firstString(finding["reason"]) == "required_boundary_not_bounded" &&
			firstString(finding["path"]) == "/memory/decision-changes" &&
			firstString(finding["contract_id"]) == "decision_change_query.v1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit did not identify the unbounded query contract: %#v", audit)
	}
}
