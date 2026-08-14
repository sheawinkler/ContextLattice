package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (roundTripper testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripper(request)
}

func testRegisteredFormatContract(contractID string) map[string]any {
	contract := adapterContractDefinition(contractID)
	return map[string]any{
		"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion,
		"schema_id": contractID, "contract_version": asInt(contract["contract_version"]),
		"required_output_mode": firstString(contract["required_output_mode"]), "validator": "contextlattice.boundary.v1",
		"contract_valid": true, "truncated": false, "omitted_counts": map[string]any{}, "actual_json_bytes": 0,
		"max_total_json_bytes": asInt(contract["max_total_json_bytes"]), "max_string_bytes": asInt(contract["max_string_bytes"]),
		"max_list_items": asInt(contract["max_list_items"]), "validation": map[string]any{"status": "passed", "errors": []any{}},
	}
}

func stabilizeTestRegisteredEnvelope(payload map[string]any) map[string]any {
	for attempts := 0; attempts < 12; attempts++ {
		raw, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		formatContract := asMap(payload["format_contract"])
		if asInt(formatContract["actual_json_bytes"]) == len(raw) {
			return payload
		}
		formatContract["actual_json_bytes"] = len(raw)
	}
	panic("test registered envelope byte accounting did not stabilize")
}

func adapterTestAgentSessionResponse(sessionID string, request map[string]any) map[string]any {
	return map[string]any{
		"ok": true,
		"session": map[string]any{
			"id": sessionID, "status": "active", "project": request["project"],
			"agent": request["agent"], "agent_id": request["agent_id"],
			"task_id": request["task_id"], "reuse_key": request["reuse_key"],
		},
	}
}

func adapterTestObjectiveRuntime(agent, agentID, project, sessionID string) map[string]any {
	payload := map[string]any{
		"version": "1", "agent": agent, "agent_id": agentID, "project": project, "session_id": sessionID,
		"objective_state": "active", "mission": "bounded mission", "objective": "bounded objective", "goal": "bounded goal",
		"objective_hierarchy": map[string]any{
			"schema_id": "contextlattice_objective_hierarchy.v1", "project": map[string]any{"id": project},
			"topic": map[string]any{}, "session": map[string]any{"id": sessionID}, "current": map[string]any{"scope": "session"},
		},
		"objective_lineage": map[string]any{
			"schema_id": "contextlattice_objective_lineage.v1", "source": "test_fixture", "precedence": []any{"session"},
			"drift": map[string]any{"detected": false}, "handoff_rule": "preserve bounded objective authority",
		},
		"scoreboard":      map[string]any{"primary_kpi": "verified task success", "guardrail_kpi": "no boundary violations", "cadence_kpi": "each lifecycle boundary"},
		"action_executed": "preflight validated", "evidence": map[string]any{"required": []any{"request", "contract", "session"}, "current": []any{}},
		"objective_delta": map[string]any{"before": "pending", "after": "active"},
		"risk_or_blocker": map[string]any{"status": "none", "fastest_recovery_path": "repeat validated preflight"},
		"next_action":     "continue with bounded context",
	}
	stamped, ok := adapterStampRegisteredEnvelope("objective_runtime_state.v1", payload)
	if !ok {
		panic("unable to build objective runtime test contract")
	}
	return stamped
}

func adapterTestPolicyContextPackage(agent, agentID, project, sessionID, topicPath, query, retrievalMode string, runtime map[string]any) map[string]any {
	hierarchy := asMap(runtime["objective_hierarchy"])
	lineage := asMap(runtime["objective_lineage"])
	payload := map[string]any{
		"version": "1", "agent": agent, "agent_id": agentID, "project": project, "topic_path": topicPath,
		"query": query, "retrieval_mode": retrievalMode, "mission": "bounded mission", "objective": "bounded objective", "goal": "bounded goal",
		"objective_hierarchy": hierarchy, "objective_lineage": lineage, "skills": map[string]any{"selected": []any{}},
		"policy_contract": map[string]any{
			"retrieve_before_inference": true, "anti_scheming_required": true, "objective_runtime_required": true,
			"checkpoint_during_execution": true, "final_recency_pass_required": true, "include_grounding": true,
			"include_retrieval_debug": true, "broaden_scope_on_zero_or_degraded": true, "format_validation_required": true,
			"contract_boundary_validated": true, "fail_closed_on_contract_violation": true,
		},
		"objective_runtime": runtime,
		"anti_scheming_protocol": map[string]any{
			"version": "1", "law": "Change conclusions to match evidence", "required_steps": []any{"retrieve", "inspect", "verify", "conclude", "report"},
			"red_flags": []any{"unsupported claim", "hidden mutation", "identity drift", "boundary leak"},
			"delivery":  []any{"evidence", "findings", "verification"},
		},
		"handoff":  map[string]any{"disperse_to_agents": true, "handoff_prompt": "change conclusions to match evidence"},
		"evidence": map[string]any{"primary_facts": []any{}, "mission_facts": []any{}, "mission_pack_error": ""},
	}
	stamped, ok := adapterStampRegisteredEnvelope("policy_context_package.v1", payload)
	if !ok {
		panic("unable to build policy context test contract")
	}
	return stamped
}

func adapterTestPreflightResponse(request map[string]any, sessionID string) map[string]any {
	agent := firstString(request["agent"])
	agentID := firstString(request["agent_id"])
	project := firstString(request["project"])
	topicPath := firstString(request["topic_path"])
	query := firstString(request["query"])
	retrievalMode := firstString(request["retrieval_mode"])
	runtime := adapterTestObjectiveRuntime(agent, agentID, project, sessionID)
	policy := adapterTestPolicyContextPackage(agent, agentID, project, sessionID, topicPath, query, retrievalMode, runtime)
	contract := adapterContractDefinition("agent_preflight_response.v1")
	response := map[string]any{
		"ok": true, "service": "gateway-go", "agent": agent, "agent_id": agentID, "project": project,
		"query": query, "topic_path": topicPath, "retrieval_mode": retrievalMode, "session_id": sessionID,
		"context_pack": map[string]any{"ok": true}, "objective_runtime": runtime, "policy_context_package": policy,
		"format_contracts": map[string]any{
			"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion,
			"contracts":      []any{"agent_preflight_response.v1", "objective_runtime_state.v1", "policy_context_package.v1"},
			"contract_valid": true, "truncated": false, "omitted_counts": map[string]any{},
			"actual_json_bytes": 0, "max_total_json_bytes": asInt(contract["max_total_json_bytes"]),
			"max_string_bytes": asInt(contract["max_string_bytes"]), "max_list_items": asInt(contract["max_list_items"]),
			"validation": map[string]any{"status": "passed", "errors": []any{}},
		},
		"agent_runtime": map[string]any{"session": map[string]any{
			"id": sessionID, "status": "active", "project": project, "agent": agent, "agent_id": agentID, "reuse_key": request["reuse_key"],
		}},
		"skills_index": map[string]any{"ok": true, "returned": 0, "results": []any{}},
	}
	return stabilizeTestPreflightResponse(response)
}

func stabilizeTestPreflightResponse(response map[string]any) map[string]any {
	for attempts := 0; attempts < 12; attempts++ {
		raw, err := json.Marshal(response)
		if err != nil {
			panic(err)
		}
		formatContracts := asMap(response["format_contracts"])
		if asInt(formatContracts["actual_json_bytes"]) == len(raw) {
			return response
		}
		formatContracts["actual_json_bytes"] = len(raw)
	}
	panic("test preflight byte accounting did not stabilize")
}

func testAgentPacketResponse(sessionID, sampleID string, extras map[string]any) map[string]any {
	if sampleID == "" {
		sampleID = "cpq_packet_fixture"
	}
	payload := map[string]any{
		"ok": true, "schema_id": agentPacketContractID, "version": 1, "surface": "context_pack",
		"query": "bounded packet fixture", "project": "alpha", "topic_path": "", "session_id": sessionID,
		"prompt": "Use the bounded packet evidence.", "evidence": []any{},
		"provenance":    map[string]any{"source_count": 0, "sources": []any{}, "citation_count": 0},
		"uncertainty":   map[string]any{"status": "bounded", "evidence_alignment": "none", "source_complete": false, "reasons": []any{}},
		"decision_gate": map[string]any{"decision": "continue", "refusal": false, "reasons": []any{}, "policy": "evidence_first"},
		"next_actions":  []any{}, "continuation": map[string]any{"status": "complete", "result_state": "ready", "token": "", "pending_sources": []any{}},
		"packet_identity": map[string]any{
			"transport_digest": "sha256:" + strings.Repeat("a", 64),
			"scope_digest":     "sha256:" + strings.Repeat("b", 64),
		},
		"outcome":            map[string]any{"sample_id": sampleID, "session_id": sessionID, "command": "contextlattice finish"},
		"token_budget":       map[string]any{"target_tokens": 1200, "hard_limit_tokens": 1600, "actual_tokens": 100, "within_hard_limit": true},
		"token_impact":       map[string]any{"baseline_tokens_estimate": 1200, "transport_tokens_exact": 100, "saved_tokens_estimate": 1100, "net_token_delta": -1100, "transport_inclusive": true},
		"writeback_required": true, "format_contract": testRegisteredFormatContract(agentPacketContractID),
	}
	for key, value := range extras {
		payload[key] = value
	}
	return stabilizeTestRegisteredEnvelope(payload)
}

func testAgentPacketDeltaResponse() map[string]any {
	baseDigest := "sha256:" + strings.Repeat("a", 64)
	resultDigest := "sha256:" + strings.Repeat("b", 64)
	modelDigest := "sha256:" + strings.Repeat("c", 64)
	scopeDigest := "sha256:" + strings.Repeat("d", 64)
	accountingDigest := "sha256:" + strings.Repeat("e", 64)
	tokenBudget := map[string]any{
		"full_packet_tokens_exact": 120, "delta_wire_tokens_exact": 12,
		"incremental_model_visible_tokens_exact": 4, "reconstructed_model_visible_tokens_exact": 120,
		"tokens_saved_exact": 108, "delta_smaller_than_full": true, "equal_reconstructed_context": true,
	}
	tokenImpact := map[string]any{
		"baseline_tokens_estimate": 120, "transport_tokens_exact": 12,
		"saved_tokens_estimate": 108, "net_token_delta": -108, "transport_inclusive": true,
	}
	payload := map[string]any{
		"ok": true, "schema_id": agentPacketDeltaContractID, "version": 1, "surface": "synthesis_pack_v2",
		"project": "alpha", "session_id": "session-delta", "agent_id": "agent-safe", "task_id": "task-delta",
		"lineage_id": "lineage-delta", "packet_id": "packet-result", "revision": 8,
		"base_packet_id": "packet_base", "base_revision": 7, "base_digest": baseDigest,
		"result_digest": resultDigest, "model_visible_digest": modelDigest, "scope_digest": scopeDigest,
		"operations": []any{}, "tombstones": []any{}, "ack_cursor": "ack-delta",
		"result_identity": map[string]any{
			"schema_id": "agent_packet_identity.v1", "ack_version": 1, "lineage_id": "lineage-delta",
			"packet_id": "packet-result", "revision": 8, "base_packet_id": "packet_base", "base_digest": baseDigest,
			"model_visible_digest": modelDigest, "transport_digest": resultDigest, "scope_digest": scopeDigest,
			"accounting_digest": accountingDigest, "issued_at": "2026-08-13T00:00:00Z",
			"expires_at": "2026-08-14T00:00:00Z", "ack_cursor": "ack-delta",
		},
		"result_accounting": map[string]any{
			"token_budget": map[string]any{"target_tokens": 120, "hard_limit_tokens": 160, "actual_tokens": 120, "within_hard_limit": true},
			"token_impact": map[string]any{
				"baseline_tokens_estimate": 120, "compiled_prompt_tokens_estimate": 120,
				"transport_tokens_exact": 12, "saved_tokens_estimate": 108, "net_token_delta": -108, "transport_inclusive": true,
			},
			"digest": accountingDigest,
		},
		"reconstruction": map[string]any{"verified": true, "digest_match": true, "operation_count": 0, "contract_id": "agent_packet_reconstruction.v1"},
		"fallback":       map[string]any{"used": false, "reason": ""},
		"token_budget":   tokenBudget,
		"token_impact":   tokenImpact,
	}
	stamped, ok := adapterStampRegisteredEnvelope(agentPacketDeltaContractID, payload)
	if !ok {
		panic("unable to build Agent Packet delta test contract")
	}
	return stamped
}

func graphCorpusTestCustody() map[string]any {
	return map[string]any{
		"schema_id": "saved_recall_graph_custody.v1", "owner": "gateway-go", "mode": "frozen_live_index",
		"synthetic": false, "sealed_holdout": true, "promotional_claims_allowed": false, "oracle_separated": true,
		"case_set_digest": "sha256:graph-case", "manifest_digest": "sha256:graph-manifest",
	}
}

func graphCorpusTestRefreshResponse() map[string]any {
	custody := graphCorpusTestCustody()
	health := map[string]any{
		"valid": true, "benchmark_eligible": true, "status": "healthy", "schema_id": graphEfficacyCorpusSchemaID, "version": 1,
		"case_count": 300, "development_count": 200, "holdout_count": 100,
		"topology_cases":     map[string]any{"references": 90, "same_session": 90, "same_topic": 90, "hard_negative": 30},
		"holdout_topology":   map[string]any{"references": 30, "same_session": 30, "same_topic": 30, "hard_negative": 10},
		"incremental_needed": 90, "holdout_incremental_needed": 30,
		"population":      map[string]any{"projects": 5, "agent_families": 5, "sessions": 20},
		"case_set_digest": "sha256:graph-case", "manifest_digest": "sha256:graph-manifest", "custody": custody, "issues": []any{},
	}
	return map[string]any{
		"ok": true, "schema_id": graphEfficacyCorpusSchemaID, "graph_corpus": true, "case_set_health": health,
		"validation_receipt": map[string]any{
			"schema_id": graphEfficacyValidationSchemaID, "version": 1, "authority": "gateway-go", "server_owned": true,
			"valid": true, "benchmark_eligible": true, "case_count": 300,
			"case_set_digest": "sha256:graph-case", "manifest_digest": "sha256:graph-manifest", "custody_case_set_digest": "sha256:graph-case",
			"captured_at": "2026-08-11T00:00:00Z", "digest": "sha256:graph-validation",
		},
		"savedCaseSet": map[string]any{
			"case_set_id": "opaque:recall_graph_corpus", "schema_id": graphEfficacyCorpusSchemaID, "version": 1,
			"count": 300, "development_count": 200, "holdout_count": 100, "case_set_digest": "sha256:graph-case", "manifest_digest": "sha256:graph-manifest",
			"topology_counts": map[string]any{"references": 90, "same_session": 90, "same_topic": 90, "hard_negative": 30}, "incremental_needed": 90,
			"custody": custody, "cost": map[string]any{"digest": "sha256:graph-cost"},
		},
	}
}

func graphCorpusTestEvaluationResponse() map[string]any {
	custody := graphCorpusTestCustody()
	return map[string]any{
		"ok": true, "passed": true, "mode": "graph", "promotion": map[string]any{"promotion_eligible": true},
		"metrics": map[string]any{"directPassed": true, "graphEfficacyStatus": "passed", "graphContribution": map[string]any{"graph_hits": 90, "status": "passed"}},
		"savedCaseSet": map[string]any{
			"case_set_id": "opaque:recall_graph_corpus", "schema_id": graphEfficacyCorpusSchemaID, "version": 1,
			"count": 300, "evaluation_count": 100, "evaluation_split": "holdout", "case_set_digest": "sha256:graph-case", "custody": custody,
			"manifest": map[string]any{"digest": "sha256:graph-manifest"},
		},
	}
}

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

func TestClientTimeoutForUsesExplicitEnvAndDefault(t *testing.T) {
	tests := []struct {
		name string
		env  string
		args []string
		want float64
	}{
		{name: "repo default", env: "", want: defaultContextLatticeClientTimeoutSecs},
		{name: "finite environment override", env: "49", want: 49},
		{name: "invalid environment falls back", env: "not-a-number", want: defaultContextLatticeClientTimeoutSecs},
		{name: "explicit timeout wins", env: "49", args: []string{"--timeout", "7"}, want: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CONTEXTLATTICE_CLIENT_TIMEOUT_SECS", test.env)
			parsed := parseArgs(test.args, commonStringFlags(), commonBoolFlags())
			if got := clientTimeoutFor(parsed); got != test.want {
				t.Fatalf("clientTimeoutFor()=%v want=%v", got, test.want)
			}
		})
	}
}

func TestCLIOutputDefaultsPreferHumansWithoutBreakingPipes(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		args     []string
		terminal bool
		mode     string
		want     []string
	}{
		{
			name:    "interactive json command defaults pretty",
			command: "search", args: []string{"release readiness"},
			terminal: true, mode: "auto",
			want: []string{"release readiness", "--pretty"},
		},
		{
			name:    "pipe remains compact",
			command: "search", args: []string{"release readiness"},
			terminal: false, mode: "auto",
			want: []string{"release readiness"},
		},
		{
			name:    "raw is authoritative",
			command: "search", args: []string{"release readiness", "--raw"},
			terminal: true, mode: "auto",
			want: []string{"release readiness", "--raw"},
		},
		{
			name:    "trace defaults to its human tree",
			command: "trace", args: []string{"--session-id", "sess"},
			terminal: true, mode: "auto",
			want: []string{"--session-id", "sess"},
		},
		{
			name:    "interactive trace json is readable",
			command: "trace", args: []string{"--session-id", "sess", "--json"},
			terminal: true, mode: "auto",
			want: []string{"--session-id", "sess", "--json", "--pretty"},
		},
		{
			name:    "global compact override",
			command: "search", args: []string{"release readiness"},
			terminal: true, mode: "compact",
			want: []string{"release readiness"},
		},
		{
			name:    "global pretty override works without tty",
			command: "search", args: []string{"release readiness"},
			terminal: false, mode: "pretty",
			want: []string{"release readiness", "--pretty"},
		},
		{
			name:    "interactive default precedes option terminator",
			command: "pack", args: []string{"release readiness", "--", "--budget-chars"},
			terminal: true, mode: "auto",
			want: []string{"release readiness", "--pretty", "--", "--budget-chars"},
		},
		{
			name:    "pipe preserves literal option after terminator",
			command: "pack", args: []string{"release readiness", "--", "--budget-chars"},
			terminal: false, mode: "auto",
			want: []string{"release readiness", "--", "--budget-chars"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := applyCLIOutputDefaults(test.command, test.args, test.terminal, test.mode)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("got=%#v want=%#v", got, test.want)
			}
		})
	}
}

func TestTracePresetsResolveOneDeterministicView(t *testing.T) {
	tests := []struct {
		preset       string
		wantProof    bool
		wantTree     bool
		wantMarkdown bool
		wantJSON     bool
	}{
		{preset: "overview"},
		{preset: "proof", wantProof: true},
		{preset: "export", wantMarkdown: true},
		{preset: "machine", wantJSON: true},
	}
	for _, test := range tests {
		t.Run(test.preset, func(t *testing.T) {
			parsed := parseArgs(
				[]string{"--preset", test.preset},
				map[string]string{"preset": "preset"},
				map[string]string{"proof": "proof", "tree": "tree", "markdown": "markdown", "json": "json"},
			)
			resolved, err := applyTracePreset(parsed)
			if err != nil {
				t.Fatalf("apply preset: %v", err)
			}
			if resolved.bool("proof") != test.wantProof ||
				resolved.bool("tree") != test.wantTree ||
				resolved.bool("markdown") != test.wantMarkdown ||
				resolved.bool("json") != test.wantJSON {
				t.Fatalf("unexpected resolved preset: %#v", resolved)
			}
		})
	}
	parsed := parseArgs(
		[]string{"--tree", "--markdown"},
		map[string]string{},
		map[string]string{"tree": "tree", "markdown": "markdown"},
	)
	if _, err := applyTracePreset(parsed); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected conflicting view error, got %v", err)
	}
}

func TestSessionTerminalCommandsAcceptFlagOnlySummary(t *testing.T) {
	tests := []struct {
		command string
		event   string
		status  string
	}{
		{command: "complete", event: "session.completed", status: "completed"},
		{command: "fail", event: "session.failed", status: "failed"},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
			sessionID := "sess-" + test.command
			summary := "verified " + test.command
			var captured map[string]any
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/agents/sessions/event":
					if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
						t.Fatalf("decode session event: %v", err)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"ok": true, "session": map[string]any{"id": sessionID},
					})
				case "/v1/agents/sessions/" + sessionID + "/rollup":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}},
					})
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run([]string{
				"contextlattice_agent_session", test.command,
				"--session-id", sessionID,
				"--project", "alpha",
				"--summary", summary,
				"--raw",
			}); err != nil {
				t.Fatalf("%s session: %v output=%s", test.command, err, stdout.String())
			}
			if firstString(captured["type"]) != test.event ||
				firstString(captured["status"]) != test.status ||
				firstString(captured["summary"]) != summary {
				t.Fatalf("unexpected session %s payload: %#v", test.command, captured)
			}
		})
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

func TestMemoryGraphCorpusDoesNotEvaluatePreservedCanonicalAfterFailedRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	var evaluationCalls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/memory/recall/eval-cases/refresh":
			refreshCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "canonical_replaced": false, "attempt_receipt_saved": true,
			})
		case "/memory/recall/evaluate/saved":
			evaluationCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "promotion": map[string]any{"promotion_eligible": true},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{"contextlattice_memory_graph_corpus", "--evaluate", "--raw"})
	if err == nil || !strings.Contains(err.Error(), "not promotion eligible") {
		t.Fatalf("failed refresh unexpectedly succeeded: err=%v output=%s", err, stdout.String())
	}
	if refreshCalls.Load() != 1 || evaluationCalls.Load() != 0 {
		t.Fatalf("failed refresh crossed into a preserved canonical evaluation: refresh=%d evaluation=%d", refreshCalls.Load(), evaluationCalls.Load())
	}
	result := map[string]any{}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode command result: %v output=%s", decodeErr, stdout.String())
	}
	if firstString(result["evaluation_skipped"]) == "" || asBool(result["ok"]) {
		t.Fatalf("failed refresh result did not disclose the skipped stale evaluation: %#v", result)
	}
}

func TestMemoryGraphEvaluationDoesNotEvaluatePreservedCanonicalAfterFailedRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	var evaluationCalls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/memory/recall/eval-cases/refresh":
			refreshCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "canonical_replaced": false, "attempt_receipt_saved": true,
			})
		case "/memory/recall/evaluate/saved":
			evaluationCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "promotion": map[string]any{"promotion_eligible": true},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{"contextlattice_memory_graph_evaluation", "--refresh", "--raw"})
	if err == nil || !strings.Contains(err.Error(), "refresh is not promotion eligible") {
		t.Fatalf("failed refresh unexpectedly succeeded: err=%v output=%s", err, stdout.String())
	}
	if refreshCalls.Load() != 1 || evaluationCalls.Load() != 0 {
		t.Fatalf("failed refresh crossed into a preserved canonical evaluation: refresh=%d evaluation=%d", refreshCalls.Load(), evaluationCalls.Load())
	}
	result := map[string]any{}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
		t.Fatalf("decode command result: %v output=%s", decodeErr, stdout.String())
	}
	if firstString(result["evaluation_skipped"]) == "" || asBool(result["ok"]) {
		t.Fatalf("failed refresh result did not disclose the skipped stale evaluation: %#v", result)
	}
}

func TestMemoryGraphCLIUsesExplicitTopicPrefixAndNoImplicitDeadline(t *testing.T) {
	var refreshPayloads []map[string]any
	var evaluationCalls atomic.Int32
	deadlineObserved := atomic.Bool{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			deadlineObserved.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{}
		if r.Method == http.MethodPost && r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode %s: %v", r.URL.Path, err)
			}
		}
		switch r.URL.Path {
		case "/memory/recall/eval-cases/refresh":
			refreshPayloads = append(refreshPayloads, payload)
			_ = json.NewEncoder(w).Encode(graphCorpusTestRefreshResponse())
		case "/memory/recall/evaluate/saved":
			evaluationCalls.Add(1)
			_ = json.NewEncoder(w).Encode(graphCorpusTestEvaluationResponse())
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var corpusOutput bytes.Buffer
	corpusCLI := newCLI(&corpusOutput, ioDiscard{})
	corpusCLI.baseURL = gateway.URL
	if err := corpusCLI.run([]string{"contextlattice_memory_graph_corpus", "--topic-prefix", "runbooks/cache", "--raw"}); err != nil {
		t.Fatalf("graph corpus command failed: %v output=%s", err, corpusOutput.String())
	}

	var evaluationOutput bytes.Buffer
	evaluationCLI := newCLI(&evaluationOutput, ioDiscard{})
	evaluationCLI.baseURL = gateway.URL
	if err := evaluationCLI.run([]string{"contextlattice_memory_graph_evaluation", "--refresh", "--topic-prefix", "runbooks/cache", "--raw"}); err != nil {
		t.Fatalf("graph evaluation command failed: %v output=%s", err, evaluationOutput.String())
	}
	var efficacyOutput bytes.Buffer
	efficacyCLI := newCLI(&efficacyOutput, ioDiscard{})
	efficacyCLI.baseURL = gateway.URL
	if err := efficacyCLI.run([]string{"contextlattice_memory_graph_efficacy", "--refresh-cases", "--project", "alpha", "--topic-prefix", "runbooks/cache", "--raw"}); err != nil {
		t.Fatalf("graph efficacy command failed: %v output=%s", err, efficacyOutput.String())
	}
	efficacyResult := map[string]any{}
	if err := json.Unmarshal(efficacyOutput.Bytes(), &efficacyResult); err != nil {
		t.Fatalf("decode graph efficacy result: %v output=%s", err, efficacyOutput.String())
	}
	refreshedSet := asMap(asMap(efficacyResult["refresh"])["savedCaseSet"])
	evaluatedSet := asMap(asMap(efficacyResult["evaluation"])["savedCaseSet"])
	if firstString(refreshedSet["case_set_id"]) != firstString(evaluatedSet["case_set_id"]) || firstString(refreshedSet["case_set_digest"]) != firstString(evaluatedSet["case_set_digest"]) || firstString(refreshedSet["manifest_digest"]) != firstString(asMap(evaluatedSet["manifest"])["digest"]) {
		t.Fatalf("refreshed and evaluated graph corpus digests diverged: refresh=%#v evaluation=%#v", refreshedSet, evaluatedSet)
	}
	if len(refreshPayloads) != 3 || firstString(refreshPayloads[0]["topic_prefix"]) != "runbooks/cache" || firstString(refreshPayloads[1]["topic_prefix"]) != "runbooks/cache" || firstString(refreshPayloads[2]["topic_prefix"]) != "runbooks/cache" {
		t.Fatalf("topic prefix was not mapped into all graph refresh payloads: %#v", refreshPayloads)
	}
	if evaluationCalls.Load() != 2 || deadlineObserved.Load() {
		t.Fatalf("graph CLI applied an implicit deadline or skipped evaluation: calls=%d deadline=%t", evaluationCalls.Load(), deadlineObserved.Load())
	}
	if timeout, err := graphEvaluationClientTimeout(parseArgs(nil, commonStringFlags(), commonBoolFlags())); err != nil || timeout != 0 {
		t.Fatalf("omitted graph timeout should be unlimited: timeout=%v err=%v", timeout, err)
	}
	if timeout, err := graphEvaluationClientTimeout(parseArgs([]string{"--timeout", "7"}, commonStringFlags(), commonBoolFlags())); err != nil || timeout != 7 {
		t.Fatalf("explicit graph timeout was not preserved: timeout=%v err=%v", timeout, err)
	}
	if timeout, err := graphEvaluationClientTimeout(parseArgs([]string{"--timeout", "0"}, commonStringFlags(), commonBoolFlags())); err != nil || timeout != 0 {
		t.Fatalf("explicit zero graph timeout should mean no client deadline: timeout=%v err=%v", timeout, err)
	}
	unknownCLI := newCLI(ioDiscard{}, ioDiscard{})
	unknownCLI.baseURL = gateway.URL
	if err := unknownCLI.run([]string{"contextlattice_memory_graph_evaluation", "--not-a-flag"}); err == nil {
		t.Fatal("graph evaluation accepted an unknown flag")
	}
}

func TestPackCommandMarksNativeCLIAndSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	var packPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			var startPayload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&startPayload); err != nil {
				t.Fatalf("decode session start: %v", err)
			}
			_ = json.NewEncoder(w).Encode(adapterTestAgentSessionResponse("session-test", startPayload))
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			response := adapterTestContextPackResponse(
				adapterUnavailableRetrievalProof(adapterMemoryTrustAssessmentContractID),
				adapterUnavailableRetrievalProof(adapterRetrievalDecisionTraceContractID),
			)
			response["context_pack_quality"] = map[string]any{
				"schema_id":     "contextlattice_context_pack_quality.v1",
				"sample_id":     "cpq_test_pack",
				"query_hash":    "0123456789abcdef",
				"quality_score": 88,
				"capturedAt":    "2026-08-13T00:00:00Z",
			}
			pack := asMap(response["context_pack"])
			pack["token_budget"] = map[string]any{"active": true}
			pack["omitted_high_value_refs"] = []any{map[string]any{"kind": "decision", "summary": "omitted"}}
			_ = json.NewEncoder(w).Encode(stabilizeTestRegisteredEnvelope(response))
		case "/v1/agents/sessions/session-test/rollup":
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
	if packPayload["session_id"] != "session-test" {
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
	if firstString(asMap(output["outcome_report"])["endpoint"]) != adapterContextPackOutcomeRoute {
		t.Fatalf("expected exact public outcome route, got %#v", output["outcome_report"])
	}
}

func TestPackCommandDoesNotPersistOrAdvertiseInvalidQualityReceipt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			var startPayload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&startPayload); err != nil {
				t.Fatalf("decode session start: %v", err)
			}
			_ = json.NewEncoder(w).Encode(adapterTestAgentSessionResponse("sess-invalid-quality", startPayload))
		case "/memory/context-pack":
			response := adapterTestContextPackResponse(
				adapterUnavailableRetrievalProof(adapterMemoryTrustAssessmentContractID),
				adapterUnavailableRetrievalProof(adapterRetrievalDecisionTraceContractID),
			)
			response["context_pack_quality"] = map[string]any{
				"sample_id": "cpq_invalid", "query_hash": "not-a-digest", "quality_score": 88,
			}
			_ = json.NewEncoder(w).Encode(stabilizeTestRegisteredEnvelope(response))
		case "/v1/agents/sessions/sess-invalid-quality/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", "invalid quality custody", "--project", "alpha", "--raw"}); err != nil {
		t.Fatalf("pack command rejected an otherwise valid context pack: %v output=%s", err, stdout.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode pack output: %v", err)
	}
	if output["outcome_report"] != nil {
		t.Fatalf("invalid quality receipt seeded an outcome report: %#v", output["outcome_report"])
	}
	state := readSessionState("alpha")
	if len(asMap(state["latest_context_pack_quality"])) > 0 || len(asMap(state["pending_context_pack_quality_by_session"])) > 0 {
		t.Fatalf("invalid quality receipt persisted durable state: %#v", state)
	}
}

func TestAgentPacketOutcomeQualityReferenceRequiresCanonicalPublicSampleID(t *testing.T) {
	valid, ok := validatedAgentPacketOutcomeQualityReference(map[string]any{
		"schema_id": agentPacketContractID,
		"outcome":   map[string]any{"sample_id": "cpq_packet_finish"},
	})
	if !ok || firstString(valid["sample_id"]) != "cpq_packet_finish" || len(valid) != 1 {
		t.Fatalf("canonical packet outcome reference was not preserved: ok=%v value=%#v", ok, valid)
	}
	for name, payload := range map[string]map[string]any{
		"wrong schema": {"schema_id": "context_pack_response.v1", "outcome": map[string]any{"sample_id": "cpq_packet_reference"}},
		"missing":      {"schema_id": agentPacketContractID, "outcome": map[string]any{}},
		"private":      {"schema_id": agentPacketContractID, "outcome": map[string]any{"sample_id": "not a public sample"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := validatedAgentPacketOutcomeQualityReference(payload); ok || len(got) != 0 {
				t.Fatalf("invalid packet outcome reference was accepted: ok=%v value=%#v", ok, got)
			}
		})
	}
}

func TestPackCommandDoesNotReplayPostDeliveryTimeout(t *testing.T) {
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_pack", "timeout must not duplicate continuation",
		"--no-auto-session", "--soft", "--timeout", "1", "--retries", "3", "--raw",
	}); err != nil {
		t.Fatalf("run timed-out pack: %v output=%s", err, stdout.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("post-delivery timeout was replayed: requests=%d", got)
	}
	output := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode timeout output: %v output=%s", err, stdout.String())
	}
	evidence := asMap(output["error_evidence"])
	if !asBool(evidence["timed_out"]) || !asBool(evidence["wrote_request"]) || !asBool(evidence["got_connection"]) || asBool(evidence["pre_delivery"]) || asBool(evidence["retryable"]) {
		t.Fatalf("timeout did not preserve non-retryable transport evidence: %#v", evidence)
	}
}

func TestPackCommandDefaultsToOneAttempt(t *testing.T) {
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"gateway unavailable"}`))
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_pack", "default retry budget", "--no-auto-session", "--soft", "--raw",
	}); err != nil {
		t.Fatalf("run default retry budget pack: %v output=%s", err, stdout.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("default retry budget was not zero: requests=%d", got)
	}
	output := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode default retry failure: %v output=%s", err, stdout.String())
	}
	if output["status"] != "failed_without_replay" || output["retry_policy"] != "pre_delivery_connection_failures_only" {
		t.Fatalf("failure envelope advertised replay semantics: %#v", output)
	}
}

func TestPackCommandUsesClientTimeoutResolution(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		extraArgs []string
		expected  float64
	}{
		{name: "repo default", env: "", expected: defaultContextLatticeClientTimeoutSecs},
		{name: "finite environment override", env: "49", expected: 49},
		{name: "explicit timeout wins", env: "49", extraArgs: []string{"--timeout", "7"}, expected: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CONTEXTLATTICE_CLIENT_TIMEOUT_SECS", test.env)
			var deadline time.Time
			c := newCLI(io.Discard, ioDiscard{})
			c.client = &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				var ok bool
				deadline, ok = r.Context().Deadline()
				if !ok {
					return nil, errors.New("retrieval request did not carry a deadline")
				}
				response := testAgentPacketResponse("", "cpq_timeout_fixture", nil)
				encoded, err := json.Marshal(response)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(encoded)),
					Request:    r,
				}, nil
			})}

			var stdout bytes.Buffer
			c.stdout = &stdout
			args := append([]string{"contextlattice_pack", "timeout resolution", "--no-auto-session", "--raw"}, test.extraArgs...)
			if err := c.run(args); err != nil {
				t.Fatalf("run pack: %v output=%s", err, stdout.String())
			}
			remaining := deadline.Sub(time.Now()).Seconds()
			if remaining < test.expected-2 || remaining > test.expected+1 {
				t.Fatalf("retrieval deadline remaining=%v want approximately %v", remaining, test.expected)
			}
		})
	}
}

func TestCLIRequestRetryRequiresPreDeliveryEvidence(t *testing.T) {
	connectionFailure := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	preDelivery := cliRequestFailure(http.MethodPost, "/memory/context-pack", 0, connectionFailure, false, false)
	if !cliRequestRetryable(preDelivery) {
		t.Fatalf("provable pre-delivery connection failure was not retryable: %#v", preDelivery)
	}
	postDelivery := cliRequestFailure(http.MethodPost, "/memory/context-pack", 0, connectionFailure, true, true)
	if cliRequestRetryable(postDelivery) {
		t.Fatalf("post-delivery connection failure was incorrectly retryable: %#v", postDelivery)
	}
	partialWrite := cliRequestFailure(http.MethodPost, "/memory/context-pack", 0, connectionFailure, false, true)
	if cliRequestRetryable(partialWrite) {
		t.Fatalf("connection-acquired write failure was incorrectly retryable: %#v", partialWrite)
	}
	var requestErr *cliRequestError
	if !errors.As(partialWrite, &requestErr) || !requestErr.GotConnection || requestErr.WroteRequest || requestErr.Retryable {
		t.Fatalf("partial-write evidence was incomplete: %#v", partialWrite)
	}
}

func TestRequestRetryRejectsPartialWriteAfterConnection(t *testing.T) {
	var requests atomic.Int32
	c := newCLI(io.Discard, ioDiscard{})
	c.client = &http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		trace := httptrace.ContextClientTrace(request.Context())
		if trace == nil || trace.GotConn == nil {
			return nil, errors.New("test transport did not receive client trace")
		}
		trace.GotConn(httptrace.GotConnInfo{})
		return nil, &net.OpError{Op: "write", Net: "tcp", Err: errors.New("partial write")}
	})}
	_, err := c.requestWithRetries("/memory/context-pack", map[string]any{"query": "partial write"}, 1, 3, 0)
	if requests.Load() != 1 {
		t.Fatalf("partial-write failure was replayed: requests=%d", requests.Load())
	}
	var requestErr *cliRequestError
	if !errors.As(err, &requestErr) || !requestErr.GotConnection || requestErr.WroteRequest || requestErr.Retryable {
		t.Fatalf("partial-write retry evidence was incorrect: %#v", err)
	}
	if !asBool(requestErr.evidence()["got_connection"]) {
		t.Fatalf("typed evidence omitted got_connection: %#v", requestErr.evidence())
	}
}

func TestRequestRetryAllowsPreConnectionDialFailure(t *testing.T) {
	var requests atomic.Int32
	c := newCLI(io.Discard, ioDiscard{})
	c.client = &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	})}
	_, err := c.requestWithRetries("/memory/context-pack", map[string]any{"query": "pre-connect"}, 1, 1, 0)
	if requests.Load() != 2 {
		t.Fatalf("provable pre-connection failure did not use the explicit retry: requests=%d", requests.Load())
	}
	var requestErr *cliRequestError
	if !errors.As(err, &requestErr) || requestErr.GotConnection || !requestErr.PreDelivery || !requestErr.Retryable {
		t.Fatalf("pre-connection retry evidence was incorrect: %#v", err)
	}
}

func TestPackCommandResponseModeUsesBoundedRecallRoute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	var responsePayload, sessionStartPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			if err := json.NewDecoder(r.Body).Decode(&sessionStartPayload); err != nil {
				t.Fatalf("decode session start: %v", err)
			}
			_ = json.NewEncoder(w).Encode(adapterTestAgentSessionResponse("sess-response", sessionStartPayload))
		case "/memory/recall/response":
			if err := json.NewDecoder(r.Body).Decode(&responsePayload); err != nil {
				t.Fatalf("decode recall response request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(failureRecallResponse())
		case "/v1/agents/sessions/sess-response/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", "response task", "--project", "alpha", "--response", "--raw"}); err != nil {
		t.Fatalf("run response mode: %v", err)
	}
	if responsePayload["session_id"] != "sess-response" || responsePayload["include_retrieval_debug"] != false {
		t.Fatalf("response request did not preserve session or suppress debug: %#v", responsePayload)
	}
	if _, ok := responsePayload["output_mode"]; ok {
		t.Fatalf("response request crossed Agent Packet mode boundary: %#v", responsePayload)
	}
	tags := asList(sessionStartPayload["tags"])
	if len(tags) < 3 || firstString(tags[2]) != "recall-response" {
		t.Fatalf("response session was not marked as a recall-response surface: %#v", sessionStartPayload)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode response output: %v", err)
	}
	if output["schema_id"] != "recall_response.v1" {
		t.Fatalf("unexpected response schema: %#v", output)
	}
	if _, ok := output["context_pack"]; ok {
		t.Fatalf("response output leaked context-pack envelope: %#v", output)
	}
	if _, ok := output["agent_packet"]; ok {
		t.Fatalf("response output leaked Agent Packet/raw content: %s", stdout.String())
	}
}

func TestPackCommandResponseModeRejectsLeakingGatewayShape(t *testing.T) {
	serverResponseID := "rr_aaaaaaaaaaaaaaaaaaaaaaaa"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/recall/response" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		response := failureRecallResponse()
		response["response_id"] = serverResponseID
		response["context_pack"] = map[string]any{"raw": "must not cross the response boundary"}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", "malformed response", "--response", "--soft", "--no-auto-session", "--retries", "0", "--raw"}); err != nil {
		t.Fatalf("run malformed response fallback: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode malformed response fallback: %v", err)
	}
	if firstString(output["response_id"]) == serverResponseID || firstString(asMap(output["classification"])["posture"]) != "abstain" {
		t.Fatalf("malformed server response was silently projected: %#v", output)
	}
	if _, exists := output["context_pack"]; exists || strings.Contains(stdout.String(), "must not cross the response boundary") {
		t.Fatalf("malformed server response leaked through fallback: %s", stdout.String())
	}
}

func TestCompactRecallResponseRejectsStaleIdentityAndRegistry(t *testing.T) {
	t.Run("stale_semantic_identity", func(t *testing.T) {
		response := failureRecallResponse()
		asMap(response["answer"])["summary"] = "allowed field changed after identity was stamped"
		if _, err := compactRecallResponse(response); err == nil {
			t.Fatal("stale recall response identity was accepted")
		}
	})
	t.Run("registry_mismatch", func(t *testing.T) {
		response := failureRecallResponse()
		asMap(response["format_contract"])["registry_id"] = "agent_contract_registry:stale"
		if _, err := compactRecallResponse(response); err == nil {
			t.Fatal("stale recall response registry was accepted")
		}
	})
}

func TestPackCommandResponseModeSoftFailureIsBoundedAbstention(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/recall/response" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(failureRecallResponse())
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", "unavailable task", "--response", "--soft", "--no-auto-session", "--retries", "0", "--raw"}); err != nil {
		t.Fatalf("run bounded response failure: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode bounded response failure: %v", err)
	}
	if output["schema_id"] != "recall_response.v1" || firstString(asMap(output["classification"])["posture"]) != "abstain" {
		t.Fatalf("response failure did not remain bounded abstention: %#v", output)
	}
	if _, ok := output["context_pack"]; ok || strings.Contains(stdout.String(), "failed_after_retries") {
		t.Fatalf("response failure leaked context-pack failure envelope: %s", stdout.String())
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
				"id": "sess-cached", "status": "active", "project": "alpha", "agent": "agent-cli", "agent_id": "codex_test", "task_id": taskID, "reuse_key": reuseKey,
			}})
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("sess-cached", "cpq_cached", nil))
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
				"id": "sess-old", "status": "active", "project": "alpha", "agent": "agent-cli", "agent_id": "codex_test", "task_id": oldTaskID, "reuse_key": oldReuseKey,
			}})
		case "/v1/agents/sessions/start":
			startCalls++
			if err := json.NewDecoder(r.Body).Decode(&startPayload); err != nil {
				t.Fatalf("decode session start: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-new", "status": "active", "project": "alpha", "agent": startPayload["agent"], "agent_id": "codex_test", "task_id": startPayload["task_id"], "reuse_key": startPayload["reuse_key"],
			}})
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("sess-new", "cpq_new_task", nil))
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

func TestEnsureSessionForAgentRejectsUnboundResponsesBeforeStateWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{name: "rejected", mutate: func(response, _ map[string]any) { response["ok"] = false }},
		{name: "foreign project", mutate: func(_ map[string]any, session map[string]any) { session["project"] = "other" }},
		{name: "foreign agent", mutate: func(_ map[string]any, session map[string]any) { session["agent"] = "other" }},
		{name: "foreign agent id", mutate: func(_ map[string]any, session map[string]any) { session["agent_id"] = "other" }},
		{name: "foreign reuse key", mutate: func(_ map[string]any, session map[string]any) { session["reuse_key"] = "reuse_other" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/agents/sessions/start" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode session request: %v", err)
				}
				response := adapterTestAgentSessionResponse("session-authority", request)
				tc.mutate(response, asMap(response["session"]))
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer gateway.Close()

			c := newCLI(ioDiscard{}, ioDiscard{})
			c.baseURL = gateway.URL
			id := c.ensureSessionForAgent(
				"alpha", "bind session authority", "codex", "agent-safe",
				map[string]any{"task_id": "task-authority"}, adapterProfile{}, 5,
			)
			if id != "" || firstString(readSessionState("alpha")["session_id"]) != "" {
				t.Fatalf("unbound session became durable: id=%q state=%#v", id, readSessionState("alpha"))
			}
		})
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
				"id": "sess-packet-finish", "status": "active", "project": "alpha", "agent": startPayload["agent"], "agent_id": startPayload["agent_id"],
				"task_id": startPayload["task_id"], "reuse_key": startPayload["reuse_key"],
			}})
		case "/memory/synthesis-pack/v2":
			_ = json.NewDecoder(r.Body).Decode(&contextPayload)
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("sess-packet-finish", "cpq_packet_finish", nil))
		case "/v1/agents/sessions/sess-packet-finish/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		case "/v1/agents/sessions/sess-packet-finish":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{
				"id": "sess-packet-finish", "status": "active", "project": "alpha",
			}})
		case "/telemetry/context-pack-quality/outcome":
			_ = json.NewDecoder(r.Body).Decode(&outcomePayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": map[string]any{
				"schema_id": "contextlattice_context_pack_outcome.v1", "outcome_id": "outcome-packet-finish", "sample_id": "cpq_packet_finish",
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
	if firstString(asMap(packet["outcome"])["sample_id"]) != "cpq_packet_finish" || packet["outcome_report"] != nil {
		t.Fatalf("CLI mutated finalized packet outcome surface: %#v", packet)
	}
	if firstString(contextPayload["task_id"]) == "" {
		t.Fatalf("context request omitted task identity: %#v", contextPayload)
	}
	packetQuality := asMap(readSessionState("alpha")["latest_context_pack_quality"])
	if firstString(packetQuality["sample_id"]) != "cpq_packet_finish" || asBool(packetQuality["reported"]) {
		t.Fatalf("packet outcome did not seed pending quality custody: %#v", packetQuality)
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice", "finish", "packet outcome complete", "--success", "--project", "alpha", "--raw"}); err != nil {
		t.Fatalf("finish command: %v output=%s", err, stdout.String())
	}
	if firstString(outcomePayload["sample_id"]) != "cpq_packet_finish" || !asBool(outcomePayload["first_pass_success"]) || asBool(outcomePayload["repair_required"]) {
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
	backendNoise := strings.Repeat("ordinary-backend-outcome-value-", 3000)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/telemetry/context-pack-quality/outcome":
			if err := json.NewDecoder(r.Body).Decode(&outcomePayload); err != nil {
				t.Fatalf("decode outcome request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"outcome": map[string]any{
					"schema_id": "contextlattice_context_pack_outcome.v1", "outcome_id": "outcome_finish", "sample_id": "cpq_finish",
					"first_pass_success": true, "repair_required": false,
					"utility":             map[string]any{"unit": "acceptance_points", "untrusted_extension": backendNoise},
					"economics":           map[string]any{"latency_ms": 42, "untrusted_extension": backendNoise},
					"pairing":             map[string]any{"pair_id": "pair-finish", "untrusted_extension": backendNoise},
					"untrusted_extension": backendNoise,
				},
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
	if bytes.Contains(stdout.Bytes(), []byte(backendNoise)) || !lifecycleReceiptContractValid(output) {
		t.Fatalf("completion emitted unbounded backend outcome metadata: %s", stdout.String())
	}
	quality := asMap(readSessionState("alpha")["latest_context_pack_quality"])
	if !asBool(quality["reported"]) || firstString(quality["outcome_id"]) != "outcome_finish" {
		t.Fatalf("pending outcome was not retired after durable report: %#v", quality)
	}
}

func TestCompactOutcomeMetadataIsClosedTypedAndBounded(t *testing.T) {
	backendNoise := strings.Repeat("ordinary-backend-outcome-value-", 3000)
	compact := compactOutcomeMetadata(map[string]any{
		"schema_id": "forged-outcome.v9", "outcome_id": strings.Repeat("o", 900), "sample_id": "cpq-safe",
		"first_pass_success": true, "retry_count": 2,
		"utility": map[string]any{
			"value": 7.5, "unit": "acceptance_points", "verification_passed": true,
			"evidence_digest": "sha256:" + strings.Repeat("a", 64), "untrusted_extension": backendNoise,
		},
		"economics": map[string]any{"latency_ms": 42, "untrusted_extension": backendNoise},
		"pairing": map[string]any{
			"pair_id": "pair-safe", "task_match_digest": "sha256:" + strings.Repeat("b", 64),
			"leakage_free": true, "untrusted_extension": backendNoise,
		},
		"untrusted_extension": backendNoise,
	})
	encoded, err := json.Marshal(compact)
	if err != nil {
		t.Fatalf("marshal compact outcome: %v", err)
	}
	if len(encoded) > 4096 || bytes.Contains(encoded, []byte(backendNoise)) || firstString(compact["schema_id"]) != "contextlattice_context_pack_outcome.v1" {
		t.Fatalf("outcome metadata was not closed and bounded: %s", encoded)
	}
	if len([]byte(firstString(compact["outcome_id"]))) > 240 || asMap(compact["utility"])["untrusted_extension"] != nil || asMap(compact["economics"])["untrusted_extension"] != nil || asMap(compact["pairing"])["untrusted_extension"] != nil {
		t.Fatalf("outcome projection retained an unbounded extension: %#v", compact)
	}
}

func TestAdapterCheckpointRejectsWriteWithoutCompletionEvent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	eventCalls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/memory/write":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
		case "/v1/agents/sessions/event":
			eventCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{
		"contextlattice_agent_adapter", "checkpoint",
		"--session-id", "session-checkpoint-rejected",
		"--agent-id", "agent-safe",
		"--project", "alpha",
		"--content", "durable checkpoint content",
		"--raw",
	})
	if err == nil {
		t.Fatalf("expected rejected writeback to fail: %s", stdout.String())
	}
	if eventCalls != 0 {
		t.Fatalf("writeback.completed posted after rejected write: %d", eventCalls)
	}
	var output map[string]any
	if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
		t.Fatalf("decode adapter failure: %v output=%s", decodeErr, stdout.String())
	}
	if output["ok"] != false || !adapterResponseContractValid(output) {
		t.Fatalf("rejected write did not emit a valid bounded failure: %#v", output)
	}
}

func TestUnifiedCheckpointAndFinishPreflightTheirExactBoundedReceipts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	writeCalls := 0
	eventCalls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/memory/write":
			writeCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": strings.Repeat("w", 900)})
		case "/v1/agents/sessions/event":
			eventCalls++
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "event": map[string]any{"id": strings.Repeat("e", 900), "type": payload["type"]},
			})
		case "/v1/agents/sessions/session-lifecycle-preflight/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	longFile := "notes/" + strings.Repeat("f", 700)
	longTopic := strings.Repeat("topic", 140)
	if err := c.run([]string{
		"contextlattice", "remember", "bounded checkpoint", "--session-id", "session-lifecycle-preflight",
		"--project", "alpha", "--file", longFile, "--topic-path", longTopic, "--raw",
	}); err != nil {
		t.Fatalf("bounded checkpoint failed after exact preflight: %v output=%s", err, stdout.String())
	}
	var checkpoint map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &checkpoint); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if !lifecycleReceiptContractValid(checkpoint) || len([]byte(firstString(asMap(checkpoint["checkpoint"])["file"]))) > 500 ||
		len([]byte(firstString(asMap(checkpoint["checkpoint"])["topic_path"]))) > 500 {
		t.Fatalf("checkpoint receipt was not deterministically bounded: %#v", checkpoint)
	}

	stdout.Reset()
	unicodeSummary := strings.Repeat("😀", 600)
	if err := c.run([]string{
		"contextlattice", "finish", unicodeSummary, "--session-id", "session-lifecycle-preflight",
		"--project", "alpha", "--no-outcome", "--raw",
	}); err != nil {
		t.Fatalf("Unicode completion failed after exact preflight: %v output=%s", err, stdout.String())
	}
	var completion map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &completion); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if !lifecycleReceiptContractValid(completion) || len([]byte(firstString(completion["summary"]))) > 360 || !utf8.ValidString(firstString(completion["summary"])) {
		t.Fatalf("completion receipt was not UTF-8 bounded: %#v", completion)
	}
	if writeCalls != 1 || eventCalls != 2 {
		t.Fatalf("unexpected lifecycle mutation counts write=%d events=%d", writeCalls, eventCalls)
	}
}

func TestEmitPreparedAdapterResponseReturnsFailureWhenSuccessIsDowngraded(t *testing.T) {
	response := adapterResponse("status", true, "codex", "agent-safe", "alpha", "session-safe", map[string]any{"status": "active"}, nil)
	response["result"] = map[string]any{"note": strings.Repeat("ordinary", 20000)}
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.emitPreparedAdapterResponse(response, false); err == nil {
		t.Fatal("invalid success was downgraded on stdout but returned shell success")
	}
	var emitted map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &emitted); err != nil {
		t.Fatalf("decode downgraded response: %v output=%s", err, stdout.String())
	}
	if emitted["ok"] != false || !adapterResponseContractValid(emitted) {
		t.Fatalf("downgrade did not emit a valid bounded failure: %#v", emitted)
	}
}

func TestAdapterCompleteRequiresOutcomeEventBeforeTerminalEventAndStateRetirement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	writeSessionStateWithExtras("alpha", "session-complete-ordered", "ordered completion", "agent-safe", map[string]any{
		"latest_context_pack_quality": map[string]any{"sample_id": "cpq_complete_ordered", "reported": false},
	})
	eventTypes := []string{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/telemetry/context-pack-quality/outcome":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"outcome": map[string]any{
					"schema_id":  "contextlattice_context_pack_outcome.v1",
					"outcome_id": "outcome-complete-ordered",
					"sample_id":  "cpq_complete_ordered",
				},
			})
		case "/v1/agents/sessions/event":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			eventTypes = append(eventTypes, firstString(payload["type"]))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{
		"contextlattice_agent_adapter", "complete",
		"--session-id", "session-complete-ordered",
		"--agent-id", "agent-safe",
		"--project", "alpha",
		"--summary", "verified completion",
		"--context-pack-quality-sample-id", "cpq_complete_ordered",
		"--first-pass-success", "true",
		"--raw",
	})
	if err == nil {
		t.Fatalf("expected outcome-event rejection to fail: %s", stdout.String())
	}
	if len(eventTypes) != 1 || eventTypes[0] != "context_pack.outcome_reported" {
		t.Fatalf("terminal event crossed failed outcome event: %#v output=%s", eventTypes, stdout.String())
	}
	quality := asMap(readSessionState("alpha")["latest_context_pack_quality"])
	if asBool(quality["reported"]) {
		t.Fatalf("quality state retired before the outcome event succeeded: %#v", quality)
	}
	var output map[string]any
	if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
		t.Fatalf("decode adapter failure: %v output=%s", decodeErr, stdout.String())
	}
	if output["ok"] != false || !adapterResponseContractValid(output) {
		t.Fatalf("failed outcome ordering did not emit a valid bounded failure: %#v", output)
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
	if !lifecycleReceiptContractValid(remember) || asInt(asMap(remember["format_contract"])["contract_version"]) != generatedLifecycleReceiptContractVersion {
		t.Fatalf("remember receipt did not use the generated lifecycle v2 contract: %#v", remember)
	}
	if _, exists := remember["session_id"]; exists || firstString(asList(remember["identity_omitted"])[0]) != "session_id" {
		t.Fatalf("nonpublic compact session identity was not explicitly omitted: %#v", remember)
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
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("", "cpq_unified_context", map[string]any{"surface": "synthesis_pack_v2"}))
		case "/v1/agents/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sessions": []any{map[string]any{"id": "sess-resume", "status": "active", "project": "alpha"}}})
		case "/v1/agents/sessions/sess-resume/context-package":
			resumeCompact = r.URL.Query().Get("view") == "compact"
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("sess-resume", "cpq_unified_resume", map[string]any{"surface": "session_resume", "project": "alpha"}))
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
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": map[string]any{
				"schema_id": "contextlattice_context_pack_outcome.v1", "outcome_id": "outcome_correct", "sample_id": "cpq_correct",
			}})
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
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("", "cpq_synthesis_test", map[string]any{
				"surface": "synthesis_pack",
				"synthesis_pack": map[string]any{
					"schema_id":                "synthesis_pack.v1",
					"high_signal_findings":     []any{map[string]any{"kind": "decision", "text": "ship synthesis"}},
					"semantic_tags":            []any{"synthesis_pack_v1"},
					"synthesis_quality":        map[string]any{"status": "strong"},
					"recommended_next_actions": []any{},
				},
			}))
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
	if firstString(output["schema_id"]) != agentPacketContractID || output["tool"] != nil || output["pack_surface"] != nil {
		t.Fatalf("synthesis command mutated the registered Agent Packet wire envelope: %#v", output)
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
	delta := testAgentPacketDeltaResponse()
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

func TestContextCommandLegacyBaseNegotiatesSafeFullFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	base := map[string]any{
		"schema_id": agentPacketContractID,
		"prompt":    "legacy packet retained before packet identity was introduced",
	}
	basePath := filepath.Join(t.TempDir(), "legacy-base.json")
	baseRaw, _ := json.Marshal(base)
	if err := os.WriteFile(basePath, baseRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	var captured map[string]any
	full := testAgentPacketResponse("", "cpq_legacy_delta_fallback", map[string]any{
		"delta_fallback": map[string]any{"requested": true, "used": true, "reason": "base_identity_missing"},
	})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/synthesis-pack/v2" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode legacy delta request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(full)
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "context", "continue legacy packet", "--project", "alpha", "--base-packet-file", basePath, "--no-auto-session", "--raw"}); err != nil {
		t.Fatalf("legacy packet should negotiate a gateway full fallback: %v", err)
	}
	if firstString(captured["packet_mode"]) != "delta" || firstString(asMap(captured["base_packet"])["schema_id"]) != agentPacketContractID {
		t.Fatalf("legacy base packet was not sent for safe gateway fallback: %#v", captured)
	}
	for _, field := range []string{"base_packet_id", "base_digest", "base_revision", "base_ack_cursor"} {
		if _, exists := captured[field]; exists {
			t.Fatalf("legacy packet invented trusted identity field %q: %#v", field, captured)
		}
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode legacy fallback output: %v", err)
	}
	if firstString(output["schema_id"]) != agentPacketContractID || firstString(asMap(output["delta_fallback"])["reason"]) != "base_identity_missing" {
		t.Fatalf("legacy packet did not preserve the gateway full fallback: %#v", output)
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
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("", "cpq_cognition_synthesis", map[string]any{"surface": "synthesis_pack_v2"}))
		case "/memory/retrieval/plan":
			_ = json.NewEncoder(w).Encode(testAgentPacketResponse("", "cpq_cognition_plan", map[string]any{"surface": "retrieval_plan"}))
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

func TestUtilityCLIUsesCanonicalLedgerAnalyticsAndGateRoutes(t *testing.T) {
	type requestRecord struct {
		method string
		query  url.Values
		body   map[string]any
	}
	records := map[string]requestRecord{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode %s: %v", r.URL.Path, err)
			}
		}
		records[r.URL.Path] = requestRecord{method: r.Method, query: r.URL.Query(), body: body}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "schema_id": "utility_test.v1"})
	}))
	defer gateway.Close()

	commands := [][]string{
		{"contextlattice", "utility", "status", "--project", "alpha", "--task-class", "coding", "--utility-unit", "acceptance_points", "--limit", "17", "--raw"},
		{"contextlattice", "utility", "analytics", "--project", "alpha", "--from", "2026-07-01T00:00:00Z", "--raw"},
		{"contextlattice", "utility", "gate", "--project", "alpha", "--minimum-pairs", "3", "--minimum-observations", "12", "--minimum-gain-per-1k", "1.5", "--minimum-lower-bound", "0.25", "--maximum-failure-rate", "0.05", "--raw"},
	}
	for _, args := range commands {
		var stdout bytes.Buffer
		c := newCLI(&stdout, ioDiscard{})
		c.baseURL = gateway.URL
		if err := c.run(args); err != nil {
			t.Fatalf("run %v: %v output=%s", args, err, stdout.String())
		}
	}
	if record := records["/telemetry/utility"]; record.method != http.MethodGet || record.query.Get("project") != "alpha" || record.query.Get("task_class") != "coding" || record.query.Get("utility_unit") != "acceptance_points" || record.query.Get("limit") != "17" {
		t.Fatalf("utility status routing mismatch: %#v", record)
	}
	if record := records["/telemetry/utility/analytics"]; record.method != http.MethodGet || record.query.Get("from") != "2026-07-01T00:00:00Z" {
		t.Fatalf("utility analytics routing mismatch: %#v", record)
	}
	gate := records["/telemetry/utility/policy/evaluate"]
	if gate.method != http.MethodPost || asInt(gate.body["minimum_pairs"]) != 3 || asInt(gate.body["minimum_observations"]) != 12 || gate.body["minimum_gain_per_1k"] != 1.5 || gate.body["maximum_failure_rate"] != 0.05 {
		t.Fatalf("utility gate payload mismatch: %#v", gate)
	}
}

func TestUtilityRecordPrimaryCLIAppendsOutcomeReceiptOnly(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	events := []map[string]any{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/telemetry/context-pack-quality/outcome":
			payload := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode utility outcome: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "outcome": map[string]any{
				"schema_id": "contextlattice_context_pack_outcome.v1", "outcome_id": payload["outcome_id"],
				"sample_id": payload["sample_id"], "utility": payload["utility"], "pairing": payload["pairing"],
			}})
		case "/v1/agents/sessions/event":
			payload := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode utility event: %v", err)
			}
			events = append(events, payload)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": map[string]any{"id": payload["id"], "type": payload["type"]}})
		case "/v1/agents/sessions/session_utility_cli/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
		default:
			t.Fatalf("unexpected utility record path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := c.run([]string{
		"contextlattice", "utility", "record", "--agent", "codex", "--agent-id", "codex_test",
		"--project", "alpha", "--session-id", "session_utility_cli", "--context-pack-quality-sample-id", "cpq_utility_cli",
		"--outcome-id", "outcome_utility_cli", "--utility-value", "8", "--utility-unit", "acceptance_points",
		"--verification-event-id", "event_utility_cli", "--verification-evidence-digest", digest,
		"--verification-passed", "true", "--verifier-kind", "deterministic_test", "--verifier-id", "go_holdout",
		"--raw",
	}); err != nil {
		t.Fatalf("primary utility record command failed: %v output=%s", err, stdout.String())
	}
	if len(events) != 1 || firstString(events[0]["type"]) != "context_pack.outcome_reported" {
		t.Fatalf("utility receipt lifecycle is incomplete: %#v", events)
	}
	output := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || firstString(output["command"]) != "utility_record" || !asBool(output["ok"]) {
		t.Fatalf("unexpected primary utility receipt output: err=%v output=%#v", err, output)
	}
}

func TestUtilityVerifyPrimaryCLIAppendsIndependentReceipt(t *testing.T) {
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/sessions/event" || r.Method != http.MethodPost {
			t.Fatalf("unexpected utility verify request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode utility verification receipt: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "event": captured,
			"utility_reconciliation": map[string]any{"ok": true, "status": "reconciled", "revision": 2},
		})
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	digest := "sha256:" + strings.Repeat("d", 64)
	if err := c.run([]string{
		"contextlattice", "utility", "verify", "--agent", "verifier", "--agent-id", "go_holdout",
		"--project", "alpha", "--session-id", "session_utility_cli", "--sample-id", "sample_utility_cli",
		"--outcome-id", "outcome_utility_cli", "--utility-value", "8", "--utility-unit", "acceptance_points",
		"--verification-event-id", "event_utility_cli", "--verification-evidence-digest", digest,
		"--verification-passed", "true", "--verifier-kind", "deterministic_test", "--verifier-id", "go_holdout", "--raw",
	}); err != nil {
		t.Fatalf("primary utility verify command failed: %v output=%s", err, stdout.String())
	}
	proof := asMap(asMap(captured["metadata"])["utility_verification"])
	if firstString(captured["type"]) != "verification.completed" || firstString(captured["agent_id"]) != "go_holdout" ||
		firstString(proof["outcome_id"]) != "outcome_utility_cli" || firstString(proof["evidence_digest"]) != digest || !asBool(proof["verification_passed"]) {
		t.Fatalf("independent verification receipt lost exact identity or evidence: %#v", captured)
	}
	output := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || firstString(output["command"]) != "utility_verify" || !asBool(output["ok"]) {
		t.Fatalf("unexpected primary utility verification output: err=%v output=%#v", err, output)
	}
	if reconciliation := asMap(asMap(output["result"])["utility_reconciliation"]); !asBool(reconciliation["ok"]) || firstString(reconciliation["status"]) != "reconciled" {
		t.Fatalf("utility reconciliation result was hidden from the verifier: %#v", output)
	}
}

func TestUtilityVerifyPrimaryCLIFailsWhenDurableReconciliationFails(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/sessions/event" || r.Method != http.MethodPost {
			t.Fatalf("unexpected utility verify request %s %s", r.Method, r.URL.Path)
		}
		payload := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode utility verification receipt: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "partial": true, "event_recorded": true, "event": payload,
			"utility_reconciliation": map[string]any{"ok": false, "status": "persistence_unavailable", "outcome_id": "outcome_utility_cli"},
		})
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	digest := "sha256:" + strings.Repeat("f", 64)
	err := c.run([]string{
		"contextlattice", "utility", "verify", "--agent", "verifier", "--agent-id", "go_holdout",
		"--project", "alpha", "--session-id", "session_utility_cli", "--sample-id", "sample_utility_cli",
		"--outcome-id", "outcome_utility_cli", "--utility-value", "8", "--utility-unit", "acceptance_points",
		"--verification-event-id", "event_utility_cli_failure", "--verification-evidence-digest", digest,
		"--verification-passed", "true", "--verifier-kind", "deterministic_test", "--verifier-id", "go_holdout", "--raw",
	})
	if err == nil || !strings.Contains(err.Error(), "durable Utility Ledger reconciliation failed") {
		t.Fatalf("CLI did not fail on incomplete durable reconciliation: err=%v output=%s", err, stdout.String())
	}
	output := map[string]any{}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil || asBool(output["ok"]) {
		t.Fatalf("CLI emitted a successful verification envelope: err=%v output=%#v", decodeErr, output)
	}
	result := asMap(output["result"])
	if !asBool(result["event_recorded"]) || firstString(asMap(result["utility_reconciliation"])["status"]) != "persistence_unavailable" || len(asList(output["findings"])) != 1 {
		t.Fatalf("CLI hid authoritative event or reconciliation failure: %#v", output)
	}
	formatContract := asMap(output["format_contract"])
	if firstString(formatContract["registry_id"]) != generatedAgentContractRegistryID ||
		asInt(formatContract["registry_version"]) != generatedAgentContractRegistryVersion ||
		firstString(asMap(formatContract["validation"])["status"]) != "passed" {
		t.Fatalf("operational failure emitted an invalid adapter contract: %#v", output)
	}
}

func TestAdapterResponseContractIsCompleteForSuccessAndOperationalFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ok       bool
		findings []map[string]any
	}{
		{name: "success", ok: true},
		{name: "operational_failure", ok: false, findings: []map[string]any{{"reason": "utility_reconciliation_incomplete"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := adapterResponse("utility_verify", tc.ok, "verifier", "go_holdout", "alpha", "session-safe", map[string]any{}, tc.findings)
			if asBool(response["ok"]) != (tc.ok && len(tc.findings) == 0) {
				t.Fatalf("unexpected operational status: %#v", response)
			}
			contract := asMap(response["adapter_contract"])
			exports := asList(contract["required_exports"])
			if firstString(contract["schema_id"]) != "contextlattice_universal_agent_adapter.v1" || len(exports) != 6 {
				t.Fatalf("adapter contract is incomplete: %#v", contract)
			}
			formatContract := asMap(response["format_contract"])
			validation := asMap(formatContract["validation"])
			if firstString(formatContract["registry_id"]) != generatedAgentContractRegistryID ||
				asInt(formatContract["registry_version"]) != generatedAgentContractRegistryVersion ||
				firstString(formatContract["schema_id"]) != "universal_agent_adapter_response.v1" ||
				asInt(formatContract["contract_version"]) != generatedUniversalAgentAdapterResponseContractVersion ||
				firstString(formatContract["required_output_mode"]) != "json_object" ||
				firstString(formatContract["validator"]) != "contextlattice.boundary.v1" ||
				firstString(validation["status"]) != "passed" || len(asList(validation["errors"])) != 0 {
				t.Fatalf("format contract is incomplete: %#v", formatContract)
			}
		})
	}
}

func TestAdapterResponseV2RequiresPublicIdentityOrExactOmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sessionID string
		agentID   string
		wantIDs   bool
	}{
		{name: "public identities", sessionID: "session-safe", agentID: "agent-safe", wantIDs: true},
		{name: "empty identities", sessionID: "", agentID: "", wantIDs: false},
		{name: "nonpublic identities", sessionID: "sess_0123456789abcdef0123456789abcdef", agentID: "sk-private-marker", wantIDs: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := adapterResponse("context-pack", true, "codex", tc.agentID, "alpha", tc.sessionID, map[string]any{}, nil)
			_, hasSession := response["session_id"]
			_, hasAgent := response["agent_id"]
			if hasSession != tc.wantIDs || hasAgent != tc.wantIDs {
				t.Fatalf("unexpected v2 identity projection: %#v", response)
			}
			omitted := asList(response["identity_omitted"])
			if tc.wantIDs {
				if len(omitted) != 0 {
					t.Fatalf("public identities were also marked omitted: %#v", response)
				}
			} else if len(omitted) != 2 || firstString(omitted[0]) != "session_id" || firstString(omitted[1]) != "agent_id" {
				t.Fatalf("omitted identities lack exact v2 evidence: %#v", response)
			}
			formatContract := asMap(response["format_contract"])
			if firstString(formatContract["schema_id"]) != generatedUniversalAgentAdapterResponseContractID ||
				asInt(formatContract["contract_version"]) != generatedUniversalAgentAdapterResponseContractVersion {
				t.Fatalf("adapter response did not use generated v2 metadata: %#v", formatContract)
			}
		})
	}
}

func TestAdapterResponsePublicProjectionPrecedesReferencePreservationAndFailsClosed(t *testing.T) {
	privateValue := "sk-opaque-marker-value"
	localPath := "/Users/example/private/worktree/file.txt"
	result := map[string]any{
		"api_key":          "artifact-public-looking-reference",
		"key_material":     "ordinary-key-material-marker",
		"auth_header":      "ordinary-auth-header-marker",
		"service_api":      "ordinary-service-api-marker",
		"receipt_id":       privateValue,
		"local_path":       localPath,
		"header":           "Authorization: Bearer ordinary-marker",
		"query":            "https://example.invalid/path?access_token=ordinary-marker",
		"structured_json":  `{"key_material":"nested-key-marker","auth_header":"nested-auth-marker","service_api":"nested-api-marker","path":"/opt/private/context.json"}`,
		"embedded_path":    "failure at /srv/contextlattice/private/report.txt",
		"encoded_query":    "https%3A%2F%2Fexample.invalid%2Fpath%3Faccess%255Ftoken%3Dencoded-marker",
		"encoded_path":     "%252FUsers%252Fexample%252Fprivate%252Fencoded.txt",
		"nested_identity":  map[string]any{"sessionId": "session-response-alias", "agentId": "agent-response-alias"},
		"unsafe/key":       "must not survive as a map key",
		"canonical_digest": "sha256:" + strings.Repeat("a", 64),
		"authorization_id": "authorization-public-receipt",
		"artifact_digests": []any{"sha256:" + strings.Repeat("b", 64), strings.Repeat("c", 64)},
	}
	response := adapterResponse(
		"context-pack", true, "codex", "agent-safe", "alpha", "session-safe", result, nil,
	)
	if !adapterResponseFits(response) {
		t.Fatalf("sanitized response did not retain a valid public envelope: %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal sanitized response: %v", err)
	}
	for _, forbidden := range []string{privateValue, localPath, "ordinary-marker", "ordinary-key-material-marker", "ordinary-auth-header-marker", "ordinary-service-api-marker", "nested-key-marker", "nested-auth-marker", "nested-api-marker", "session-response-alias", "agent-response-alias", "/opt/private/context.json", "/srv/contextlattice/private/report.txt", "unsafe/key", "encoded-marker", "encoded.txt"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public response retained forbidden marker %q: %s", forbidden, encoded)
		}
	}
	projected := asMap(response["result"])
	if firstString(projected["canonical_digest"]) != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("approved digest did not survive exact reference projection: %#v", projected)
	}
	if firstString(projected["authorization_id"]) != "authorization-public-receipt" || len(asList(projected["artifact_digests"])) != 2 {
		t.Fatalf("approved structured references did not survive exact projection: %#v", projected)
	}
	if firstString(projected["encoded_query"]) != "[REDACTED]" || firstString(projected["encoded_path"]) != "[REDACTED_PATH]" {
		t.Fatalf("percent-encoded sensitive text did not fail closed: %#v", projected)
	}
	nestedIdentity := asMap(projected["nested_identity"])
	if nestedIdentity["session_id"] != "session-safe" || nestedIdentity["agent_id"] != "agent-safe" || nestedIdentity["sessionId"] != nil || nestedIdentity["agentId"] != nil {
		t.Fatalf("nested identity aliases were not rebound to request authority: %#v", nestedIdentity)
	}

	tooMany := make([]any, 65)
	for index := range tooMany {
		tooMany[index] = index
	}
	failure := adapterResponse(
		"context-pack", true, "codex", "agent-safe", "alpha", "session-safe",
		map[string]any{"items": tooMany, "provider_path": localPath}, nil,
	)
	if failure["ok"] != false || !adapterResponseContractValid(failure) {
		t.Fatalf("oversized public result did not become a valid constant-data failure: %#v", failure)
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil || strings.Contains(string(failureJSON), localPath) || strings.Contains(string(failureJSON), "items") {
		t.Fatalf("failure envelope retained rejected result evidence: err=%v output=%s", err, failureJSON)
	}
}

func TestAdapterPublicJSONDomainRequiresSignedInt64IntegerLexemes(t *testing.T) {
	for _, value := range []json.Number{
		json.Number("9223372036854775808"),
		json.Number("-9223372036854775809"),
	} {
		if adapterJSONDomainValid(map[string]any{"value": value}, 0) {
			t.Fatalf("out-of-range integer entered the public JSON domain: %s", value)
		}
		if projected, ok := adapterProjectPublicValue(map[string]any{"value": value}, "session-safe", "agent-safe", 0); ok || projected != nil {
			t.Fatalf("out-of-range integer crossed the public projector: %s %#v", value, projected)
		}
	}
	for _, value := range []json.Number{
		json.Number("9223372036854775807"),
		json.Number("-9223372036854775808"),
		json.Number("1.5"),
		json.Number("1e-9999"),
	} {
		if !adapterJSONDomainValid(map[string]any{"value": value}, 0) {
			t.Fatalf("valid signed-int64 or finite number left the public JSON domain: %s", value)
		}
	}

	oversized := json.Number("9223372036854775808")
	response := adapterResponse(
		"context-pack", true, "codex", "agent-safe", "alpha", "session-safe",
		map[string]any{"nested": map[string]any{"value": oversized}}, nil,
	)
	if response["ok"] != false || !adapterResponseContractValid(response) {
		t.Fatalf("out-of-domain integer did not produce a valid public failure: %#v", response)
	}
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.emit(response, false); err != nil {
		t.Fatalf("emit bounded adapter failure: %v", err)
	}
	if strings.Contains(stdout.String(), oversized.String()) {
		t.Fatalf("out-of-domain integer crossed the emitted adapter response: %s", stdout.String())
	}
}

func TestRequestJSONForValidationRejectsUnpairedEscapedSurrogate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"memory_trust_assessment":{"summary":"\ud800"}}`)
	}))
	defer server.Close()

	c := newCLI(ioDiscard{}, ioDiscard{})
	c.baseURL = server.URL
	if _, _, err := c.requestJSONForValidation(context.Background(), http.MethodGet, "/proof", nil, 5); err == nil || err.Error() != "ContextLattice gateway request failed" {
		t.Fatalf("unpaired escaped surrogate was not rejected before proof custody: %v", err)
	}
}

func TestRequestJSONForValidationRejectsTransportOverflowPastValidPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
		_, _ = io.WriteString(w, strings.Repeat(" ", 8<<20))
	}))
	defer server.Close()

	c := newCLI(ioDiscard{}, ioDiscard{})
	c.baseURL = server.URL
	if _, _, err := c.requestJSONForValidation(context.Background(), http.MethodGet, "/proof", nil, 5); err == nil || err.Error() != "ContextLattice gateway request failed" {
		t.Fatalf("oversized response with a valid JSON prefix was accepted: %v", err)
	}
}

func TestRequestJSONFailuresUseConstantPublicErrorData(t *testing.T) {
	privateMarker := "backend-response-marker-must-not-cross"
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "malformed success response", statusCode: http.StatusOK, body: `{"ok":true,"detail":"` + privateMarker},
		{name: "non-success response", statusCode: http.StatusBadGateway, body: `{"ok":false,"detail":"` + privateMarker + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(test.statusCode)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			c := newCLI(ioDiscard{}, ioDiscard{})
			c.baseURL = server.URL
			if _, _, err := c.requestJSONForValidation(context.Background(), http.MethodGet, "/boundary", nil, 5); err == nil || err.Error() != "ContextLattice gateway request failed" || strings.Contains(err.Error(), privateMarker) {
				t.Fatalf("transport error was not constant public data: %v", err)
			}

			var stdout bytes.Buffer
			c.stdout = &stdout
			if err := c.run([]string{"contextlattice_pack", "transport failure", "--soft", "--no-auto-session", "--retries", "0", "--raw"}); err != nil {
				t.Fatalf("soft pack failure was not emitted as structured JSON: %v output=%s", err, stdout.String())
			}
			var output map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
				t.Fatalf("decode soft pack failure: %v output=%s", err, stdout.String())
			}
			if output["ok"] != false || firstString(output["error"]) != "ContextLattice gateway request failed" || strings.Contains(stdout.String(), privateMarker) {
				t.Fatalf("soft pack failure retained raw backend data: %#v output=%s", output, stdout.String())
			}
		})
	}
}

func TestAdapterHandoffRequiresPositiveEventReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = server.URL
	err := c.run([]string{
		"contextlattice_agent_adapter", "handoff", "--session-id", "session-safe",
		"--agent-id", "agent-safe", "--project", "alpha", "--summary", "bounded handoff",
	})
	if err == nil {
		t.Fatalf("empty handoff event receipt was treated as success: %s", stdout.String())
	}
	var output map[string]any
	if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
		t.Fatalf("decode bounded handoff failure: %v output=%s", decodeErr, stdout.String())
	}
	if output["ok"] != false || !adapterResponseContractValid(output) {
		t.Fatalf("empty event did not produce a valid bounded failure: %#v", output)
	}
}

func TestAdapterProfilesUsesUniversalV2Envelope(t *testing.T) {
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	if err := c.run([]string{"contextlattice_agent_adapter", "profiles", "--project", "alpha"}); err != nil {
		t.Fatalf("profiles command failed: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode profiles envelope: %v", err)
	}
	if output["ok"] != true || !adapterResponseContractValid(output) ||
		asInt(asMap(output["format_contract"])["contract_version"]) != generatedUniversalAgentAdapterResponseContractVersion {
		t.Fatalf("profiles output did not use the universal v2 boundary: %#v", output)
	}
}

func TestAdapterStatusUsesBoundedUniversalV2Envelope(t *testing.T) {
	privateMarker := "sk-status-private-marker"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/sessions/session-status-safe" {
			t.Fatalf("unexpected status path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"session": map[string]any{
				"id": "session-status-safe", "status": "active", "project": "alpha", "agent": "codex", "agent_id": "agent-safe",
				"internal": map[string]any{"api_key": privateMarker, "path": "/opt/private/status.json"},
			},
			"rollup": map[string]any{"raw": privateMarker},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_agent_adapter", "status", "--session-id", "session-status-safe",
		"--agent-id", "agent-safe", "--project", "alpha", "--raw",
	}); err != nil {
		t.Fatalf("status command failed: %v output=%s", err, stdout.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode status envelope: %v", err)
	}
	if output["ok"] != true || !adapterResponseContractValid(output) {
		t.Fatalf("status output did not use a valid universal v2 envelope: %#v", output)
	}
	if strings.Contains(stdout.String(), privateMarker) || strings.Contains(stdout.String(), "/opt/private/status.json") {
		t.Fatalf("status output retained raw backend state: %s", stdout.String())
	}
}

func TestAdapterStatusOmitsNonpublicSessionIdentityEverywhere(t *testing.T) {
	privateSessionID := "sess_0123456789abcdef0123456789abcdef"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"session": map[string]any{
				"id": privateSessionID, "status": "active", "project": "alpha", "agent": "codex", "agent_id": "agent-safe",
			},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_agent_adapter", "status", "--session-id", privateSessionID,
		"--agent-id", "agent-safe", "--project", "alpha", "--raw",
	}); err != nil {
		t.Fatalf("status command failed: %v output=%s", err, stdout.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode status envelope: %v", err)
	}
	if strings.Contains(stdout.String(), privateSessionID) || output["session_id"] != nil {
		t.Fatalf("status output retained nonpublic session identity: %#v", output)
	}
	if !adapterResponseContractValid(output) {
		t.Fatalf("status identity omission invalidated the v2 envelope: %#v", output)
	}
}

func TestAdapterStatusRejectsForeignSessionAuthorityWithoutLeakingBackendState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "foreign session", mutate: func(session map[string]any) { session["id"] = "session-foreign" }},
		{name: "foreign project", mutate: func(session map[string]any) { session["project"] = "foreign-project" }},
		{name: "foreign agent", mutate: func(session map[string]any) { session["agent"] = "foreign-agent" }},
		{name: "foreign agent id", mutate: func(session map[string]any) { session["agent_id"] = "foreign-agent-id" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			privateMarker := "backend-status-marker-must-not-cross"
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/agents/sessions/session-status-safe" {
					t.Fatalf("unexpected status path: %s", r.URL.Path)
				}
				session := map[string]any{
					"id": "session-status-safe", "status": "active", "project": "alpha",
					"agent": "codex", "agent_id": "agent-safe", "backend_marker": privateMarker,
				}
				test.mutate(session)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": session})
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			err := c.run([]string{
				"contextlattice_agent_adapter", "status", "--session-id", "session-status-safe",
				"--agent", "codex", "--agent-id", "agent-safe", "--project", "alpha", "--raw",
			})
			if err == nil {
				t.Fatalf("foreign status authority was accepted: %s", stdout.String())
			}
			var output map[string]any
			if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
				t.Fatalf("decode bounded status failure: %v output=%s", decodeErr, stdout.String())
			}
			if output["ok"] != false || !adapterResponseContractValid(output) {
				t.Fatalf("foreign status did not produce a valid bounded failure: %#v", output)
			}
			if strings.Contains(stdout.String(), privateMarker) || strings.Contains(stdout.String(), "session-foreign") || strings.Contains(stdout.String(), "foreign-project") || strings.Contains(stdout.String(), "foreign-agent") {
				t.Fatalf("foreign backend authority leaked through status failure: %s", stdout.String())
			}
		})
	}
}

func adapterTestProofFormatContract(schemaID string) map[string]any {
	return testRegisteredFormatContract(schemaID)
}

func adapterTestFullRetrievalProofPair(count int, tailSummary string) (map[string]any, map[string]any) {
	assessments := make([]any, 0, count)
	decisions := make([]any, 0, count)
	for index := 0; index < count; index++ {
		hexID := fmt.Sprintf("%024x", index+1)
		summary := "bounded retrieval assessment"
		if index == count-1 {
			summary = tailSummary
		}
		assessments = append(assessments, map[string]any{
			"assessment_id": "mta_" + hexID, "candidate_id": "rtc_" + hexID,
			"content_digest": "sha256:" + strings.Repeat("a", 64),
			"quarantine":     map[string]any{"quarantined": false}, "summary": summary,
		})
		decisions = append(decisions, map[string]any{
			"receipt_id": "rdr_" + hexID, "candidate_id": "rtc_" + hexID,
			"candidate_ordinal": index + 1, "decision_order": index + 1, "decision": "selected",
		})
	}
	assessment := map[string]any{
		"ok": true, "schema_id": adapterMemoryTrustAssessmentContractID, "version": 1,
		"input_candidate_count": count, "processed_candidate_count": count, "input_truncated_count": 0,
		"assessed_count": count, "quarantine_count": 0, "deduplicated_count": 0, "policy_omitted_count": 0,
		"assessments":    assessments,
		"input_boundary": map[string]any{"maximum_candidates": count, "truncated": false, "omitted_count": 0, "reason": "bounded test input"},
		"policy": map[string]any{
			"retrieved_memory_is_evidence_not_instruction": true,
			"self_awarded_trust_accepted":                  false,
			"security_defenses_fail_closed":                true,
		},
		"bounded": true, "format_contract": adapterTestProofFormatContract(adapterMemoryTrustAssessmentContractID),
	}
	trace := map[string]any{
		"ok": true, "schema_id": adapterRetrievalDecisionTraceContractID, "version": 1,
		"trace_id": "rdt_0123456789abcdef01234567", "candidate_count": count,
		"processed_candidate_count": count, "input_truncated_count": 0, "decision_count": count,
		"coverage_complete": true, "decisions": decisions, "decision_counts": map[string]any{"selected": count},
		"input_boundary": map[string]any{"maximum_candidates": count, "truncated": false, "omitted_count": 0, "reason": "bounded test input"},
		"marginal_stop":  map[string]any{"stopped": false, "reason": "all candidates processed", "token_budget_active": false},
		"redaction":      map[string]any{"raw_candidate_text_included": false, "secret_values_included": false},
		"bounded":        true, "format_contract": adapterTestProofFormatContract(adapterRetrievalDecisionTraceContractID),
	}
	return stabilizeTestRegisteredEnvelope(assessment), stabilizeTestRegisteredEnvelope(trace)
}

func TestAdapterFullRetrievalProofRequiresExactFormatContractAttestation(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "maximum total bytes", mutate: func(contract map[string]any) { contract["max_total_json_bytes"] = 1 }},
		{name: "maximum string bytes", mutate: func(contract map[string]any) { contract["max_string_bytes"] = 1 }},
		{name: "maximum list items", mutate: func(contract map[string]any) { contract["max_list_items"] = 1 }},
		{name: "actual bytes", mutate: func(contract map[string]any) { contract["actual_json_bytes"] = 1 }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			assessment, trace := adapterTestFullRetrievalProofPair(1, "bounded assessment")
			tc.mutate(asMap(assessment["format_contract"]))
			tc.mutate(asMap(trace["format_contract"]))
			if adapterCanonicalRetrievalProof(assessment, adapterMemoryTrustAssessmentContractID, false) != nil ||
				adapterCanonicalRetrievalProof(trace, adapterRetrievalDecisionTraceContractID, false) != nil {
				t.Fatal("false format-contract attestation claimed canonical proof custody")
			}
		})
	}
}

func adapterTestContextPackResponse(assessment, trace map[string]any) map[string]any {
	unavailableAssessment := adapterUnavailableRetrievalProof(adapterMemoryTrustAssessmentContractID)
	unavailableTrace := adapterUnavailableRetrievalProof(adapterRetrievalDecisionTraceContractID)
	compiler := map[string]any{
		"schema_id": "contextlattice_context_compiler.v1", "version": 1, "strategy": "native_adapter_test",
		"intended_use": "test bounded public adapter output", "recommended_surface": "cli_for_local_agents",
		"ranked_evidence_count": 0, "memory_trust_assessment": unavailableAssessment, "retrieval_decision_trace": unavailableTrace,
	}
	pack := map[string]any{
		"facts": []any{}, "results": []any{}, "citations": []any{}, "ranked_evidence": []any{},
		"prompt_sections": map[string]any{}, "context_compiler": compiler, "relevant_decisions": []any{},
		"files_to_read": []any{}, "files_to_avoid": []any{}, "capabilities_to_use": []any{}, "runbooks": []any{},
		"known_failure_modes": []any{}, "commands": []any{}, "acceptance_criteria": []any{},
		"memory_trust_assessment": unavailableAssessment, "retrieval_decision_trace": unavailableTrace,
	}
	runAdvisor, runAdvisorOK := adapterPublicRunAdvisor(map[string]any{
		"ok": true, "schema_id": "run_advisor.v1", "posture": "minimal_context",
	})
	if !runAdvisorOK {
		panic("test run-advisor fixture did not satisfy its registered contract")
	}
	pack["run_advisor"] = runAdvisor
	response := map[string]any{
		"ok": true, "schema_id": "context_pack_response.v1", "context_pack": pack,
		"source_coverage":    map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
		"writeback_required": true, "context_compiler": compiler, "reference_prompt": "",
		"run_advisor":             runAdvisor,
		"memory_trust_assessment": assessment, "retrieval_decision_trace": trace,
		"format_contract": map[string]any{
			"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion,
			"schema_id": "context_pack_response.v1", "contract_version": asInt(adapterContractDefinition("context_pack_response.v1")["contract_version"]), "required_output_mode": "json_object",
			"validator": "contextlattice.boundary.v1", "contract_valid": true, "truncated": false,
			"omitted_counts": map[string]any{}, "actual_json_bytes": 0,
			"max_total_json_bytes": maxContextPackResponseJSONBytes, "max_string_bytes": 6000, "max_list_items": 64,
			"validation": map[string]any{"status": "passed", "errors": []any{}},
		},
	}
	for attempts := 0; attempts < 8; attempts++ {
		raw, _ := json.Marshal(response)
		format := asMap(response["format_contract"])
		if asInt(format["actual_json_bytes"]) == len(raw) {
			break
		}
		format["actual_json_bytes"] = len(raw)
	}
	return response
}

func TestPackCommandOutputHonorsMinimumEffectiveBudget(t *testing.T) {
	assessment, trace := adapterTestFullRetrievalProofPair(65, "bounded assessment")
	response := adapterTestContextPackResponse(assessment, trace)
	response["reference_prompt"] = strings.Repeat("bounded context ", 300)
	stabilizeTestRegisteredEnvelope(response)
	if !adapterContextPackEnvelopeAttestationValid(response) || !adapterPrepareContextPackRetrievalProofs(response) {
		t.Fatal("full context-pack fixture did not satisfy the pre-public proof boundary")
	}
	public, ok := adapterPublicContextPack(response, "session-public", "agent-public")
	if !ok {
		t.Fatal("full context-pack fixture did not satisfy the public boundary")
	}
	originalTrustDigest := firstString(asMap(public["memory_trust_assessment"])["canonical_digest"])
	originalTraceDigest := firstString(asMap(public["retrieval_decision_trace"])["canonical_digest"])
	bounded, boundedPretty, ok := fitPackCommandOutputBudget(public, 1024, true)
	if !ok {
		minimum, minimumOK := minimumPackCommandOutput(public, 1024, minimumContextPackContractBudgetChars, true)
		minimumBytes, minimumEncoded := packCommandOutputWireBytes(minimum, true)
		t.Fatalf("minimum effective context-pack budget rejected a valid proof-bound response: minimum_ok=%v encoded=%v bytes=%d contract=%v", minimumOK, minimumEncoded, minimumBytes, adapterContextPackContractPassed(minimum))
	}
	wireBytes, encoded := packCommandOutputWireBytes(bounded, boundedPretty)
	if !encoded || wireBytes > minimumContextPackContractBudgetChars {
		t.Fatalf("minimum effective budget exceeded: encoded=%v bytes=%d", encoded, wireBytes)
	}
	if asInt(bounded["requested_context_budget_chars"]) != 1024 ||
		asInt(bounded["context_budget_chars"]) != minimumContextPackContractBudgetChars ||
		bounded["budget_floor_applied"] != true || bounded["clipped"] != true {
		t.Fatalf("minimum budget truth was not preserved: %#v", bounded)
	}
	if !adapterContextPackContractPassed(bounded) {
		t.Fatal("bounded context-pack response did not retain its registered contract")
	}
	boundedTrust := asMap(bounded["memory_trust_assessment"])
	boundedTrace := asMap(bounded["retrieval_decision_trace"])
	if originalTrustDigest == "" || firstString(boundedTrust["canonical_digest"]) != originalTrustDigest ||
		originalTraceDigest == "" || firstString(boundedTrace["canonical_digest"]) != originalTraceDigest {
		t.Fatalf("minimum envelope lost untouched proof custody: trust=%#v trace=%#v", boundedTrust, boundedTrace)
	}
	pack := asMap(bounded["context_pack"])
	tokenBudget := asMap(pack["token_budget"])
	if asInt(tokenBudget["selected_count"]) != len(asList(pack["ranked_evidence"])) {
		t.Fatalf("minimum token-budget selection count drifted: %#v", pack)
	}
}

func TestPackCommandBudgetDowngradesPrettyBeforeRejectingCompactPacket(t *testing.T) {
	items := make([]any, 0, 300)
	for index := 0; index < 300; index++ {
		items = append(items, map[string]any{"n": index})
	}
	payload := map[string]any{"schema_id": agentPacketContractID, "items": items}
	compactBytes, compactOK := packCommandOutputWireBytes(payload, false)
	prettyBytes, prettyOK := packCommandOutputWireBytes(payload, true)
	if !compactOK || !prettyOK || compactBytes > minimumContextPackContractBudgetChars || prettyBytes <= minimumContextPackContractBudgetChars {
		t.Fatalf("packet fixture did not straddle the effective budget: compact=%d pretty=%d", compactBytes, prettyBytes)
	}
	_, outputPretty, ok := fitPackCommandOutputBudget(payload, 1024, true)
	if !ok || outputPretty {
		t.Fatalf("compact packet was rejected instead of downgrading pretty output: ok=%v pretty=%v", ok, outputPretty)
	}
}

func TestPackCommandHonorsMinimumEffectiveBudgetEndToEnd(t *testing.T) {
	assessment, trace := adapterTestFullRetrievalProofPair(65, "bounded assessment")
	response := adapterTestContextPackResponse(assessment, trace)
	response["reference_prompt"] = strings.Repeat("bounded context ", 300)
	stabilizeTestRegisteredEnvelope(response)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memory/context-pack" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		request := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode context-pack request: %v", err)
		}
		if request["include_retrieval_debug"] != true || request["output_mode"] != nil {
			t.Fatalf("explicit output budget did not select the one-request proof-bearing path: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	query := strings.Repeat("bounded ", 188)
	err := c.run([]string{
		"contextlattice_pack", query, "--project", "alpha", "--no-auto-session",
		"--budget-chars", "1024",
	})
	if err != nil {
		t.Fatalf("native context-pack budget path failed: %v output=%s", err, stdout.String())
	}
	if stdout.Len() > minimumContextPackContractBudgetChars {
		t.Fatalf("native context-pack wire output exceeded its effective budget: %d", stdout.Len())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode native context-pack output: %v", err)
	}
	if output["ok"] != true || output["clipped"] != true ||
		asInt(output["requested_context_budget_chars"]) != 1024 ||
		asInt(output["context_budget_chars"]) != minimumContextPackContractBudgetChars ||
		!adapterContextPackContractPassed(output) {
		t.Fatalf("native context-pack output lost budget or contract truth: %#v", output)
	}
	for _, proof := range []string{"memory_trust_assessment", "retrieval_decision_trace"} {
		projected := asMap(output[proof])
		if projected["bounded_projection"] != true || firstString(projected["canonical_digest"]) == "" {
			t.Fatalf("native context-pack output lost %s proof custody: %#v", proof, projected)
		}
	}
}

func TestPackCommandRejectsInvalidExplicitBudgetBeforeSessionOrRequest(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "malformed", args: []string{"--budget-chars", "nope"}},
		{name: "missing", args: []string{"--budget-chars"}},
		{name: "signed integer overflow", args: []string{"--budget-chars", "9223372036854775808"}},
		{name: "separated negative", args: []string{"--budget-chars", "-5"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			globalHome := t.TempDir()
			t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", globalHome)
			t.Setenv("CONTEXTLATTICE_AUTO_SESSION_DISABLED", "0")
			var requests atomic.Int64
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				request := map[string]any{}
				_ = json.NewDecoder(r.Body).Decode(&request)
				_ = json.NewEncoder(w).Encode(adapterTestAgentSessionResponse("session-invalid-budget", request))
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			c := newCLI(&stdout, &stderr)
			c.baseURL = gateway.URL
			args := []string{"contextlattice_pack", "invalid explicit budget must not mutate", "--project", "alpha", "--agent-id", "agent-public", "--raw"}
			args = append(args, testCase.args...)
			err := c.run(args)
			if !errors.Is(err, errCLIReportedFailure) {
				t.Fatalf("invalid explicit budget did not report a failing exit status: %v", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("invalid explicit budget crossed the HTTP boundary: requests=%d", requests.Load())
			}
			entries, readErr := os.ReadDir(globalHome)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("invalid explicit budget mutated local session state: entries=%d err=%v", len(entries), readErr)
			}
			if stderr.Len() != 0 {
				t.Fatalf("invalid explicit budget emitted non-JSON stderr: %q", stderr.String())
			}
			if strings.Contains(stdout.String(), "invalid explicit budget must not mutate") {
				t.Fatal("invalid explicit budget reflected the rejected query")
			}
			output := map[string]any{}
			decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
			decoder.UseNumber()
			if decodeErr := decoder.Decode(&output); decodeErr != nil {
				t.Fatalf("invalid explicit budget did not emit structured JSON: %v output=%s", decodeErr, stdout.String())
			}
			if output["ok"] != false || output["structured_failure"] != true ||
				firstString(output["status"]) != "invalid_context_budget" ||
				asInt(output["requested_context_budget_chars"]) != 1 ||
				asInt(output["context_budget_chars"]) != minimumContextPackContractBudgetChars ||
				!adapterRegisteredEnvelopeAttestationValid("context_pack_response.v1", output) {
				t.Fatalf("invalid explicit budget lost failure, budget, or contract truth: %#v", output)
			}
		})
	}
}

func TestPackCommandInvalidExplicitBudgetSoftFailureIsStructuredAndSideEffectFree(t *testing.T) {
	globalHome := t.TempDir()
	t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", globalHome)
	t.Setenv("CONTEXTLATTICE_AUTO_SESSION_DISABLED", "0")
	var requests atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_pack", "invalid soft budget", "--project", "alpha", "--soft", "--raw", "--budget-chars", "nope",
	}); err != nil {
		t.Fatalf("soft invalid budget returned an operational error: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("soft invalid budget crossed the HTTP boundary: %d", requests.Load())
	}
	output := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || output["ok"] != false || firstString(output["status"]) != "invalid_context_budget" {
		t.Fatalf("soft invalid budget did not emit the typed failure: err=%v output=%#v", err, output)
	}
}

func TestPackCommandBudgetTokenAfterTerminatorRemainsLiteralQuery(t *testing.T) {
	packet := testAgentPacketResponse("", "cpq_packet_literal_budget_token", nil)
	var requests atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/memory/context-pack" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		request := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode literal-budget-token request: %v", err)
		}
		if firstString(request["query"]) != "preserve literal --budget-chars" ||
			request["output_mode"] != agentPacketContractID || request["include_retrieval_debug"] != false {
			t.Fatalf("post-terminator budget token changed query or output mode: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(packet)
	}))
	defer gateway.Close()

	baseArgs := []string{
		"preserve literal", "--project", "alpha", "--no-auto-session", "--", "--budget-chars",
	}
	for _, testCase := range []struct {
		name string
		run  func(*cli) error
	}{
		{
			name: "piped default",
			run: func(c *cli) error {
				return c.run(append([]string{"contextlattice_pack"}, baseArgs...))
			},
		},
		{
			name: "interactive pretty default",
			run: func(c *cli) error {
				args := applyCLIOutputDefaults("pack", baseArgs, true, "auto")
				return c.cmdPackWithRouteMode(args, "contextlattice_pack", "/memory/context-pack", "", false)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := testCase.run(c); err != nil {
				t.Fatalf("literal post-terminator budget token was rejected: %v output=%s", err, stdout.String())
			}
			output := map[string]any{}
			decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
			decoder.UseNumber()
			if err := decoder.Decode(&output); err != nil {
				t.Fatalf("decode literal-budget-token output: %v output=%s", err, stdout.String())
			}
			if output["ok"] != true || firstString(output["schema_id"]) != agentPacketContractID ||
				firstString(output["status"]) == "invalid_context_budget" {
				t.Fatalf("literal post-terminator budget token selected the explicit-budget failure path: %#v", output)
			}
		})
	}
	if requests.Load() != 2 {
		t.Fatalf("literal post-terminator budget token request count drifted: %d", requests.Load())
	}
}

func TestPackCommandFullWithoutExplicitBudgetPreservesFullResponse(t *testing.T) {
	assessment, trace := adapterTestFullRetrievalProofPair(2, "bounded assessment")
	response := adapterTestContextPackResponse(assessment, trace)
	referencePrompt := strings.TrimSpace(strings.Repeat("bounded full context ", 150))
	response["reference_prompt"] = referencePrompt
	stabilizeTestRegisteredEnvelope(response)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode full context-pack request: %v", err)
		}
		if request["include_retrieval_debug"] != true || request["output_mode"] != nil {
			t.Fatalf("full mode did not select the proof-bearing response: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{
		"contextlattice_pack", "preserve the full response", "--project", "alpha", "--no-auto-session", "--full", "--raw",
	})
	if err != nil {
		t.Fatalf("native full context-pack path failed: %v output=%s", err, stdout.String())
	}
	if stdout.Len() > maxContextPackResponseJSONBytes {
		t.Fatalf("native full context-pack wire output exceeded its registered ceiling: %d", stdout.Len())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode native full context-pack output: %v", err)
	}
	if got := firstString(output["reference_prompt"]); got != referencePrompt {
		t.Fatalf("native full context-pack response lost its reference prompt: got=%d want=%d", len(got), len(referencePrompt))
	}
	if output["clipped"] == true ||
		asInt(output["requested_context_budget_chars"]) != maxContextPackResponseJSONBytes ||
		asInt(output["context_budget_chars"]) != maxContextPackResponseJSONBytes {
		t.Fatalf("native full context-pack response was reduced or lost budget truth: %#v", output)
	}
	if !adapterContextPackContractPassed(output) {
		t.Fatalf("native full context-pack response lost contract truth: findings=%v format=%#v", adapterContractFindings("context_pack_response.v1", output), output["format_contract"])
	}
}

func TestPackCommandFullAcceptsExactRegisteredCeilingWithFramingNewline(t *testing.T) {
	query := "preserve the exact full response ceiling"
	assessment, trace := adapterTestFullRetrievalProofPair(2, "bounded assessment")
	response := adapterTestContextPackResponse(assessment, trace)
	stabilizeTestRegisteredEnvelope(response)
	prepared, ok := preparePackCommandOutput(
		response, query, maxContextPackResponseJSONBytes, "", "", "contextlattice_pack", "",
	)
	if !ok {
		t.Fatal("exact-ceiling full response base fixture did not satisfy the public boundary")
	}
	padding := map[string]any{}
	paddingKeys := make([]string, 24)
	for index := range paddingKeys {
		paddingKeys[index] = fmt.Sprintf("padding_%02d", index)
		padding[paddingKeys[index]] = ""
	}
	prepared["compatibility_padding"] = padding
	for attempts := 0; attempts < 16; attempts++ {
		stamped, stampedOK := adapterStampRegisteredEnvelope("context_pack_response.v1", prepared)
		if !stampedOK {
			t.Fatal("exact-ceiling full response could not be restamped within its registered boundary")
		}
		prepared = stamped
		raw, err := json.Marshal(prepared)
		if err != nil {
			t.Fatalf("encode exact-ceiling full response: %v", err)
		}
		if len(raw) == maxContextPackResponseJSONBytes {
			break
		}
		if len(raw) > maxContextPackResponseJSONBytes {
			t.Fatalf("exact-ceiling full response padding overshot: %d", len(raw))
		}
		growth := maxContextPackResponseJSONBytes - len(raw)
		if growth > 32 {
			// Leave room for actual_json_bytes to grow to six digits before
			// applying the final exact adjustment.
			growth -= 16
		}
		remaining := growth
		for _, key := range paddingKeys {
			current := firstString(padding[key])
			capacity := 5900 - len(current)
			if capacity <= 0 {
				continue
			}
			addition := minInt(capacity, remaining)
			boundedText := strings.Repeat("bounded ", (addition+7)/8)
			padding[key] = current + boundedText[:addition]
			remaining -= addition
			if remaining == 0 {
				break
			}
		}
		if remaining != 0 {
			t.Fatalf("exact-ceiling full response exhausted bounded padding capacity: remaining=%d", remaining)
		}
	}
	raw, err := json.Marshal(prepared)
	if err != nil || len(raw) != maxContextPackResponseJSONBytes || !adapterContextPackEnvelopeAttestationValid(prepared) {
		t.Fatalf("exact-ceiling full response fixture is invalid: err=%v bytes=%d", err, len(raw))
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode exact-ceiling full response request: %v", err)
		}
		if request["include_retrieval_debug"] != true || request["output_mode"] != nil {
			t.Fatalf("exact-ceiling full response did not use the implicit proof-bearing surface: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(prepared)
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_pack", query, "--project", "alpha", "--no-auto-session", "--full", "--raw",
	}); err != nil {
		t.Fatalf("exact-ceiling full response was rejected because of framing: %v output=%s", err, stdout.String())
	}
	if stdout.Len() != maxContextPackResponseJSONBytes+1 || stdout.Bytes()[stdout.Len()-1] != '\n' {
		t.Fatalf("exact-ceiling full response wire framing drifted: bytes=%d want=%d", stdout.Len(), maxContextPackResponseJSONBytes+1)
	}
	output := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil || !adapterContextPackContractPassed(output) {
		t.Fatalf("exact-ceiling full response lost its registered envelope: err=%v", err)
	}
	if asInt(asMap(output["format_contract"])["actual_json_bytes"]) != maxContextPackResponseJSONBytes ||
		asInt(output["requested_context_budget_chars"]) != maxContextPackResponseJSONBytes ||
		asInt(output["context_budget_chars"]) != maxContextPackResponseJSONBytes || output["clipped"] == true {
		t.Fatalf("exact-ceiling full response lost canonical budget truth or was reduced: %#v", output)
	}
}

func TestPackCommandImplicitAgentPacketUsesRegisteredCeiling(t *testing.T) {
	packet := testAgentPacketResponse("", "cpq_packet_implicit_ceiling", map[string]any{
		"compatibility_padding_a": strings.Repeat("a", 3000),
		"compatibility_padding_b": strings.Repeat("b", 3000),
		"compatibility_padding_c": strings.Repeat("c", 3000),
	})
	packetWireBytes, encoded := packCommandOutputWireBytes(packet, false)
	registeredMaximum := asInt(adapterContractDefinition(agentPacketContractID)["max_total_json_bytes"])
	if !encoded || packetWireBytes <= 10000 || packetWireBytes > registeredMaximum ||
		!adapterRegisteredEnvelopeAttestationValid(agentPacketContractID, packet) {
		t.Fatalf("implicit Agent Packet fixture did not occupy the registered 10-16 KiB compatibility window: encoded=%v bytes=%d maximum=%d", encoded, packetWireBytes, registeredMaximum)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode implicit Agent Packet request: %v", err)
		}
		if request["output_mode"] != agentPacketContractID || request["include_retrieval_debug"] != false {
			t.Fatalf("implicit Agent Packet mode was not requested: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(packet)
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{
		"contextlattice_pack", "preserve implicit Agent Packet compatibility", "--project", "alpha", "--no-auto-session", "--raw",
	})
	if err != nil {
		t.Fatalf("native implicit Agent Packet path failed: %v output=%s", err, stdout.String())
	}
	if stdout.Len() <= 10000 || stdout.Len() > registeredMaximum+1 {
		t.Fatalf("native implicit Agent Packet left its registered compatibility window: %d", stdout.Len())
	}
	var output map[string]any
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil {
		t.Fatalf("decode native implicit Agent Packet: %v", err)
	}
	if firstString(output["schema_id"]) != agentPacketContractID || !adapterRegisteredEnvelopeAttestationValid(agentPacketContractID, output) {
		t.Fatalf("native implicit Agent Packet lost its registered envelope: %#v", output)
	}
}

func TestPackCommandImplicitAgentPacketAcceptsExactRegisteredCeilingWithFramingNewline(t *testing.T) {
	registeredMaximum := asInt(adapterContractDefinition(agentPacketContractID)["max_total_json_bytes"])
	packet := testAgentPacketResponse("", "cpq_packet_exact_ceiling", map[string]any{
		"compatibility_padding_a": strings.Repeat("a", 3900),
		"compatibility_padding_b": strings.Repeat("b", 3900),
		"compatibility_padding_c": strings.Repeat("c", 3900),
		"compatibility_padding_d": "",
	})
	for attempts := 0; attempts < 12; attempts++ {
		stabilizeTestRegisteredEnvelope(packet)
		raw, err := json.Marshal(packet)
		if err != nil {
			t.Fatalf("encode exact-ceiling Agent Packet: %v", err)
		}
		if len(raw) == registeredMaximum {
			break
		}
		padding := firstString(packet["compatibility_padding_d"])
		nextLength := len(padding) + registeredMaximum - len(raw)
		if nextLength < 0 || nextLength > 3900 {
			t.Fatalf("exact-ceiling Agent Packet padding left its registered string bound: current=%d next=%d raw=%d", len(padding), nextLength, len(raw))
		}
		packet["compatibility_padding_d"] = strings.Repeat("d", nextLength)
	}
	raw, err := json.Marshal(packet)
	if err != nil || len(raw) != registeredMaximum || !adapterRegisteredEnvelopeAttestationValid(agentPacketContractID, packet) {
		t.Fatalf("exact-ceiling Agent Packet fixture is invalid: err=%v bytes=%d maximum=%d", err, len(raw), registeredMaximum)
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode exact-ceiling Agent Packet request: %v", err)
		}
		if request["output_mode"] != agentPacketContractID || request["include_retrieval_debug"] != false {
			t.Fatalf("exact-ceiling Agent Packet did not use the implicit packet surface: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(packet)
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_pack", "accept exact registered Agent Packet ceiling", "--project", "alpha", "--no-auto-session", "--raw",
	}); err != nil {
		t.Fatalf("exact-ceiling Agent Packet was rejected because of framing: %v output=%s", err, stdout.String())
	}
	if stdout.Len() != registeredMaximum+1 || stdout.Bytes()[stdout.Len()-1] != '\n' {
		t.Fatalf("exact-ceiling Agent Packet wire framing drifted: bytes=%d want=%d", stdout.Len(), registeredMaximum+1)
	}
	output := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil || !adapterRegisteredEnvelopeAttestationValid(agentPacketContractID, output) {
		t.Fatalf("exact-ceiling Agent Packet lost its registered envelope: err=%v", err)
	}
}

func TestPackCommandSoftFailureHonorsMinimumEffectiveBudgetEndToEnd(t *testing.T) {
	probe, probeOK := minimumPackCommandOutput(
		failurePack(strings.Repeat("bounded ", 1000), 1024, errors.New("transport failed")),
		1024, minimumContextPackContractBudgetChars, true,
	)
	probeCompactBytes, probeCompactOK := packCommandOutputWireBytes(probe, false)
	probePrettyBytes, probePrettyOK := packCommandOutputWireBytes(probe, true)
	if !probeOK || !probeCompactOK || probeCompactBytes > minimumContextPackContractBudgetChars {
		t.Fatalf(
			"minimum soft failure is not representable: ok=%v compact_ok=%v compact_bytes=%d pretty_ok=%v pretty_bytes=%d findings=%v",
			probeOK, probeCompactOK, probeCompactBytes, probePrettyOK, probePrettyBytes,
			adapterContractFindings("context_pack_response.v1", probe),
		)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"ok":false,"error":"upstream diagnostic must not cross the public boundary"}`))
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	query := strings.Repeat("bounded ", 1000)
	err := c.run([]string{
		"contextlattice_pack", query, "--project", "alpha", "--no-auto-session", "--soft", "--pretty",
		"--budget-chars", "1024",
	})
	if err != nil {
		t.Fatalf("native context-pack soft failure path failed: %v output=%s", err, stdout.String())
	}
	if stdout.Len() > minimumContextPackContractBudgetChars {
		t.Fatalf("native context-pack soft failure exceeded its effective budget: %d", stdout.Len())
	}
	if strings.Contains(stdout.String(), "upstream diagnostic") || strings.Contains(stdout.String(), query) {
		t.Fatal("native context-pack soft failure exposed rejected transport or query payload")
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode native context-pack soft failure: %v", err)
	}
	if output["ok"] != false || output["structured_failure"] != true ||
		asInt(output["requested_context_budget_chars"]) != 1024 ||
		asInt(output["context_budget_chars"]) != minimumContextPackContractBudgetChars ||
		!adapterRegisteredEnvelopeAttestationValid("context_pack_response.v1", output) {
		t.Fatalf("native context-pack soft failure lost budget, failure, or contract truth: %#v", output)
	}
}

func TestAdapterPublicContextPackRestampsClosedRunAdvisorSignals(t *testing.T) {
	response := adapterTestContextPackResponse(
		adapterUnavailableRetrievalProof(adapterMemoryTrustAssessmentContractID),
		adapterUnavailableRetrievalProof(adapterRetrievalDecisionTraceContractID),
	)
	rawAdvisor := map[string]any{
		"ok": true, "schema_id": "run_advisor.v1", "posture": "balanced",
		"prompt_quality":   map[string]any{"score": 88, "state": "ready", "ranked_evidence_count": 2, "reference_prompt_chars": 12, "missing": []any{}},
		"retrieval_advice": map[string]any{"recommended_mode": "balanced", "recommended_surface": "cli_for_local_agents", "rationale": []any{"evidence_ready"}},
		"continuation": map[string]any{
			"status": "partial", "poll_url": "/memory/search/continuations/cont-public", "events_url": "/memory/search/continuations/cont-public/events",
			"pending_sources": []any{"letta"}, "warming_sources": []any{"letta"}, "repair_instruction": "Watch bounded continuation progress.",
			"continuation_available": true,
			"modeled_progress":       map[string]any{"probabilistic": true, "progress_pct": 55.5, "confidence_band": "medium", "pending_sources": []any{"letta"}},
			"retrieval_progress": map[string]any{
				"status": "partial", "result_state": "pending", "poll_url": "/memory/search/continuations/cont-public",
				"source_summary":   map[string]any{"pending_sources": []any{"letta"}},
				"agent_visibility": map[string]any{"best_surface": "session_watch", "session_event_type": "retrieval.continuation.progress"},
			},
			"agent_visibility":        map[string]any{"best_surface": "session_watch", "watch_command": "contextlattice_agent_session watch --continuation-token cont-public --pretty"},
			"agent_followup_command":  "contextlattice_agent_session watch --continuation-token cont-public --pretty",
			"agent_followup_endpoint": "/memory/search/continuations/cont-public", "agent_followup_transport": "http_or_cli_watch",
		},
		"objective_coherence": map[string]any{
			"score": 91, "status": "aligned", "repair_instruction": "Continue.",
			"signals": map[string]any{"project_primary_objective_present": true, "subobjective_count": 2, "credential": "opaque-value"},
		},
		"graph_quality": map[string]any{
			"status": "healthy", "recommendation": "Use graph evidence.",
			"signals": map[string]any{"added_evidence_count": 3, "relations": map[string]any{"references": 2}, "credential": "opaque-value"},
		},
		"next_actions": []any{
			map[string]any{"label": "watch_continuation", "command": "contextlattice_agent_session watch --continuation-token cont-public --pretty", "reason": "Slow-source evidence remains available."},
		},
	}
	rawAdvisor, rawAdvisorOK := adapterStampRegisteredEnvelope("run_advisor.v1", rawAdvisor)
	if !rawAdvisorOK {
		t.Fatal("raw run-advisor fixture did not satisfy its registered contract")
	}
	response["run_advisor"] = rawAdvisor
	asMap(response["context_pack"])["run_advisor"] = rawAdvisor
	stabilizeTestRegisteredEnvelope(response)

	public, ok := adapterPublicContextPack(response, "session-public", "agent-public")
	if !ok {
		t.Fatal("registered context pack failed public projection")
	}
	advisor := asMap(public["run_advisor"])
	if !adapterRegisteredEnvelopeAttestationValid("run_advisor.v1", advisor) {
		t.Fatalf("public run-advisor contract was stale or invalid: %#v", advisor)
	}
	objectiveSignals := asMap(asMap(advisor["objective_coherence"])["signals"])
	graphSignals := asMap(asMap(advisor["graph_quality"])["signals"])
	if objectiveSignals["project_primary_objective_present"] != true || asInt(objectiveSignals["subobjective_count"]) != 2 || objectiveSignals["credential"] != nil {
		t.Fatalf("objective signals were not projected to the closed public shape: %#v", objectiveSignals)
	}
	if asInt(graphSignals["added_evidence_count"]) != 3 || asInt(asMap(graphSignals["relations"])["references"]) != 2 || graphSignals["credential"] != nil {
		t.Fatalf("graph signals were not projected to the closed public shape: %#v", graphSignals)
	}
	continuation := asMap(advisor["continuation"])
	actions := asList(advisor["next_actions"])
	if firstString(continuation["status"]) != "partial" || len(asMap(continuation["modeled_progress"])) == 0 ||
		len(asMap(continuation["retrieval_progress"])) == 0 || len(asMap(continuation["agent_visibility"])) == 0 ||
		firstString(continuation["poll_url"]) != "/memory/search/continuations/cont-public" ||
		firstString(continuation["events_url"]) != "/memory/search/continuations/cont-public/events" ||
		firstString(continuation["agent_followup_endpoint"]) != "/memory/search/continuations/cont-public" ||
		firstString(continuation["agent_followup_transport"]) != "http_or_cli_watch" || len(actions) != 1 ||
		firstString(asMap(actions[0])["label"]) != "watch_continuation" {
		t.Fatalf("production-shaped continuation guidance was dropped: continuation=%#v actions=%#v", continuation, actions)
	}
	if adapterPublicContinuationRoute("/Users/example/private/continuation.json") != "" || adapterPublicContinuationRoute("/memory/search/continuations/../private") != "" {
		t.Fatal("continuation route projector accepted a filesystem or traversal path")
	}
	publicPackAdvisor := asMap(asMap(public["context_pack"])["run_advisor"])
	if !adapterRegisteredEnvelopeAttestationValid("run_advisor.v1", publicPackAdvisor) {
		t.Fatalf("nested context-pack run-advisor contract was stale: %#v", publicPackAdvisor)
	}
}

func TestAdapterPublicContextPackOutcomeReportUsesOnlyTheClosedProductRoute(t *testing.T) {
	valid := contextPackOutcomeReport("session-public", "cpq_public_route")
	projected, ok := adapterProjectPublicValue(valid, "session-public", "agent-public", 0)
	if !ok || firstString(asMap(projected)["endpoint"]) != adapterContextPackOutcomeRoute {
		t.Fatalf("exact outcome route was not preserved: ok=%v projected=%#v", ok, projected)
	}
	for name, endpoint := range map[string]string{
		"filesystem": "/Users/example/private/outcome.json",
		"traversal":  "/telemetry/context-pack-quality/../private",
		"other":      "/telemetry/context-pack-quality/other",
	} {
		t.Run(name, func(t *testing.T) {
			invalid := contextPackOutcomeReport("session-public", "cpq_public_route")
			invalid["endpoint"] = endpoint
			if projected, ok := adapterProjectPublicValue(invalid, "session-public", "agent-public", 0); ok || len(asMap(projected)) != 0 {
				t.Fatalf("noncanonical outcome endpoint crossed the public projector: %#v", projected)
			}
		})
	}
}

func TestAdapterContextPackPreservesExactProofCustodyAcrossHTTPBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	digests := []string{}
	for _, tail := range []string{"tail receipt alpha", "tail receipt beta"} {
		assessment, trace := adapterTestFullRetrievalProofPair(65, tail)
		canonicalAssessment := adapterCanonicalRetrievalProof(assessment, adapterMemoryTrustAssessmentContractID, false)
		canonicalTrace := adapterCanonicalRetrievalProof(trace, adapterRetrievalDecisionTraceContractID, false)
		if len(canonicalAssessment) == 0 || len(canonicalTrace) == 0 || !adapterRetrievalProofPairValid(canonicalAssessment, canonicalTrace) {
			t.Fatalf("test proof pair failed the exact custody gate: assessment_findings=%#v trace_findings=%#v assessment_counts=%v trace_counts=%v",
				adapterContractFindings(adapterMemoryTrustAssessmentContractID, assessment),
				adapterContractFindings(adapterRetrievalDecisionTraceContractID, trace),
				adapterRetrievalProofCountsValid(assessment, adapterMemoryTrustAssessmentContractID, false),
				adapterRetrievalProofCountsValid(trace, adapterRetrievalDecisionTraceContractID, false))
		}
		canonical, err := adapterRetrievalProofCanonicalJSON(assessment)
		if err != nil {
			t.Fatalf("canonicalize untouched proof: %v", err)
		}
		expected := sha256.Sum256([]byte(canonical))
		response := adapterTestContextPackResponse(assessment, trace)
		response["context_pack_quality"] = map[string]any{
			"sample_id": "cpq_proof_route", "query_hash": "0123456789abcdef",
			"quality_score": 90, "capturedAt": "2026-08-13T00:00:00Z",
		}
		stabilizeTestRegisteredEnvelope(response)
		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/memory/context-pack":
				_ = json.NewEncoder(w).Encode(response)
			case r.URL.Path == "/v1/agents/sessions/event":
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": map[string]any{"id": "evt-proof-boundary"}})
			case strings.HasSuffix(r.URL.Path, "/rollup"):
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		}))
		var stdout bytes.Buffer
		c := newCLI(&stdout, ioDiscard{})
		c.baseURL = gateway.URL
		err = c.run([]string{
			"contextlattice_agent_adapter", "context-pack", "--agent", "codex", "--agent-id", "agent-safe",
			"--project", "alpha", "--session-id", "session-proof-boundary", "--query", "proof boundary",
		})
		gateway.Close()
		if err != nil {
			t.Fatalf("context-pack transport: %v output=%s", err, stdout.String())
		}
		var output map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatalf("decode output: %v", err)
		}
		projected := asMap(asMap(asMap(output["result"])["context_pack"])["memory_trust_assessment"])
		digest := firstString(projected["canonical_digest"])
		if digest != "sha256:"+hex.EncodeToString(expected[:]) || !asBool(projected["bounded_projection"]) {
			t.Fatalf("proof digest did not bind untouched HTTP receipt: %#v", projected)
		}
		if strings.Contains(stdout.String(), tail) || projected["assessments"] != nil {
			t.Fatalf("full proof tail crossed the bounded adapter output: %s", stdout.String())
		}
		if firstString(asMap(asMap(output["result"])["outcome_report"])["endpoint"]) != adapterContextPackOutcomeRoute {
			t.Fatalf("adapter context-pack lost the exact public outcome route: %#v", asMap(output["result"])["outcome_report"])
		}
		digests = append(digests, digest)
	}
	if len(digests) != 2 || digests[0] == digests[1] {
		t.Fatalf("distinct post-64 proof tails collapsed to one digest: %#v", digests)
	}
}

func TestAdapterContextPackRejectsBeforeCompletionOrQualityStateAndProjectsPublicSuccess(t *testing.T) {
	unavailableTrust := map[string]any{
		"schema_id": "memory_trust_assessment.v1", "canonical_path": "$.memory_trust_assessment",
		"available": false, "reason": "proof_unavailable",
	}
	unavailableTrace := map[string]any{
		"schema_id": "retrieval_decision_trace.v1", "canonical_path": "$.retrieval_decision_trace",
		"available": false, "reason": "proof_unavailable",
	}
	validResponse := func() map[string]any {
		compiler := map[string]any{
			"schema_id": "contextlattice_context_compiler.v1", "version": 1,
			"strategy": "native_adapter_test", "intended_use": "test bounded public adapter output",
			"recommended_surface": "cli_for_local_agents", "ranked_evidence_count": 0,
			"memory_trust_assessment": unavailableTrust, "retrieval_decision_trace": unavailableTrace,
		}
		pack := map[string]any{
			"facts": []any{}, "results": []any{}, "citations": []any{}, "ranked_evidence": []any{},
			"prompt_sections": map[string]any{}, "context_compiler": compiler,
			"relevant_decisions": []any{}, "files_to_read": []any{}, "files_to_avoid": []any{},
			"capabilities_to_use": []any{}, "runbooks": []any{}, "known_failure_modes": []any{},
			"commands": []any{}, "acceptance_criteria": []any{},
			"memory_trust_assessment": unavailableTrust, "retrieval_decision_trace": unavailableTrace,
			"session_id": "sess_0123456789abcdef0123456789abcdef", "agent_id": "agent-other",
			"agent_runtime": map[string]any{
				"session_id": "sess_0123456789abcdef0123456789abcdef", "agent_id": "agent-other",
			},
		}
		response := map[string]any{
			"ok":                 true,
			"schema_id":          "context_pack_response.v1",
			"session_id":         "sess_0123456789abcdef0123456789abcdef",
			"agent_id":           "agent-other",
			"context_pack":       pack,
			"source_coverage":    map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
			"writeback_required": true, "context_compiler": compiler, "reference_prompt": "",
			"run_advisor": map[string]any{
				"schema_id": "run_advisor.v1", "posture": "minimal_context",
				"prompt_quality": map[string]any{}, "retrieval_advice": map[string]any{},
				"continuation": map[string]any{}, "objective_coherence": map[string]any{},
			},
			"memory_trust_assessment": unavailableTrust, "retrieval_decision_trace": unavailableTrace,
			"format_contract": map[string]any{
				"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion,
				"schema_id": "context_pack_response.v1", "contract_version": asInt(adapterContractDefinition("context_pack_response.v1")["contract_version"]),
				"required_output_mode": "json_object", "validator": "contextlattice.boundary.v1",
				"contract_valid": true, "truncated": false, "omitted_counts": map[string]any{},
				"actual_json_bytes":    0,
				"max_total_json_bytes": maxContextPackResponseJSONBytes,
				"max_string_bytes":     6000, "max_list_items": 64,
				"validation": map[string]any{"status": "passed", "errors": []any{}},
			},
		}
		for attempts := 0; attempts < 8; attempts++ {
			raw, err := json.Marshal(response)
			if err != nil {
				panic(err)
			}
			contract := asMap(response["format_contract"])
			if asInt(contract["actual_json_bytes"]) == len(raw) {
				break
			}
			contract["actual_json_bytes"] = len(raw)
		}
		return response
	}
	stabilizeResponse := func(response map[string]any) {
		for attempts := 0; attempts < 8; attempts++ {
			raw, err := json.Marshal(response)
			if err != nil {
				panic(err)
			}
			contract := asMap(response["format_contract"])
			if asInt(contract["actual_json_bytes"]) == len(raw) {
				return
			}
			contract["actual_json_bytes"] = len(raw)
		}
	}
	validQuality := func() map[string]any {
		return map[string]any{
			"sample_id":     "cpq_adapter_native",
			"query_hash":    "0123456789abcdef",
			"quality_score": 88.5,
			"capturedAt":    "2026-08-13T00:00:00Z",
		}
	}
	for _, tc := range []struct {
		name                  string
		mutate                func(map[string]any)
		preserveBadAccounting bool
		agent                 string
		configure             func(*testing.T, string)
		wantSuccess           bool
		wantEventCount        int32
		wantQuality           bool
	}{
		{
			name: "false self-attested byte accounting",
			mutate: func(response map[string]any) {
				asMap(response["format_contract"])["actual_json_bytes"] = 1
			},
			preserveBadAccounting: true,
		},
		{
			name: "raw rejection",
			mutate: func(response map[string]any) {
				response["ok"] = false
			},
		},
		{
			name: "missing passed contract",
			mutate: func(response map[string]any) {
				delete(asMap(response["format_contract"]), "validation")
			},
		},
		{
			name: "malformed quality receipt",
			mutate: func(response map[string]any) {
				response["context_pack_quality"] = map[string]any{"sample_id": "cpq_unbound"}
			},
			wantSuccess:    true,
			wantEventCount: 1,
		},
		{
			name:  "oversized final adapter envelope",
			agent: "agent-envelope-limit",
			configure: func(t *testing.T, globalHome string) {
				configDir := filepath.Join(globalHome, "config", "agents")
				if err := os.MkdirAll(configDir, 0o700); err != nil {
					t.Fatalf("create profile config directory: %v", err)
				}
				profiles, err := json.Marshal(map[string]any{"profiles": map[string]any{
					"agent-envelope-limit": map[string]any{
						"agent_id": "agent-safe",
						"query":    "bounded adapter envelope",
						"large":    strings.Repeat("ordinary value ", maxAdapterResponseJSONBytes/8),
					},
				}})
				if err != nil {
					t.Fatalf("marshal profile fixture: %v", err)
				}
				if err := os.WriteFile(filepath.Join(configDir, "agent_profiles.json"), profiles, 0o600); err != nil {
					t.Fatalf("write profile fixture: %v", err)
				}
			},
		},
		{
			name: "accepted response uses request identity and closed quality custody",
			mutate: func(response map[string]any) {
				response["context_pack_quality"] = validQuality()
			},
			wantSuccess:    true,
			wantEventCount: 1,
			wantQuality:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalHome := t.TempDir()
			t.Setenv("HOME", globalHome)
			t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", globalHome)
			t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(globalHome, "seen.json"))
			if tc.configure != nil {
				tc.configure(t, globalHome)
			}
			response := validResponse()
			if tc.mutate != nil {
				tc.mutate(response)
			}
			if !tc.preserveBadAccounting {
				stabilizeResponse(response)
			}
			var eventCount atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/memory/context-pack":
					_ = json.NewEncoder(w).Encode(response)
				case r.URL.Path == "/v1/agents/sessions/event":
					eventCount.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": map[string]any{"id": "evt-pack"}})
				case strings.HasSuffix(r.URL.Path, "/rollup"):
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			agent := tc.agent
			if agent == "" {
				agent = "codex"
			}
			err := c.run([]string{
				"contextlattice_agent_adapter", "context-pack",
				"--agent", agent,
				"--agent-id", "agent-safe",
				"--project", "alpha",
				"--session-id", "sess_0123456789abcdef0123456789abcdef",
				"--query", "native adapter boundary",
			})
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("unexpected command result success=%v error=%v output=%s", tc.wantSuccess, err, stdout.String())
			}
			if got := eventCount.Load(); got != tc.wantEventCount {
				t.Fatalf("unexpected completion-event count %d, want %d", got, tc.wantEventCount)
			}
			var output map[string]any
			if unmarshalErr := json.Unmarshal(stdout.Bytes(), &output); unmarshalErr != nil {
				t.Fatalf("decode bounded adapter output: %v; output=%q", unmarshalErr, stdout.String())
			}
			if asBool(output["ok"]) != tc.wantSuccess {
				t.Fatalf("output success truth did not match command result: %#v", output)
			}
			if strings.Contains(stdout.String(), "sess_0123456789abcdef0123456789abcdef") || strings.Contains(stdout.String(), "agent-other") {
				t.Fatalf("adapter output leaked response-owned identities: %s", stdout.String())
			}
			state := readSessionState("alpha")
			quality := asMap(asMap(state["pending_context_pack_quality_by_session"])["sess_0123456789abcdef0123456789abcdef"])
			if (len(quality) > 0) != tc.wantQuality {
				t.Fatalf("unexpected durable quality state: %#v", state)
			}
			if tc.wantSuccess {
				contextPack := asMap(asMap(output["result"])["context_pack"])
				if contextPack["session_id"] != nil || contextPack["agent_id"] != "agent-safe" ||
					firstString(asList(contextPack["identity_omitted"])[0]) != "session_id" {
					t.Fatalf("nested context pack did not bind request-owned identities: %#v", contextPack)
				}
			}
		})
	}
}

func TestUtilityVerifyRejectsVerifierIdentityMismatch(t *testing.T) {
	c := newCLI(ioDiscard{}, ioDiscard{})
	err := c.run([]string{
		"contextlattice", "utility", "verify", "--agent-id", "reporter", "--session-id", "session",
		"--sample-id", "sample", "--outcome-id", "outcome", "--utility-value", "1", "--utility-unit", "point",
		"--verification-event-id", "event", "--verification-evidence-digest", "sha256:" + strings.Repeat("e", 64),
		"--verification-passed", "true", "--verifier-kind", "deterministic_test", "--verifier-id", "independent",
	})
	if err == nil || !strings.Contains(err.Error(), "--agent-id must exactly identify --verifier-id") {
		t.Fatalf("mismatched verifier event identity was not rejected: %v", err)
	}
}

func TestUtilityRecordRejectsReportingAgentAsVerifier(t *testing.T) {
	c := newCLI(ioDiscard{}, ioDiscard{})
	err := c.run([]string{
		"contextlattice", "utility", "record", "--agent-id", "codex_test", "--project", "alpha",
		"--session-id", "session_self", "--context-pack-quality-sample-id", "sample_self",
		"--utility-value", "1", "--utility-unit", "point", "--verifier-id", "codex_test", "--raw",
	})
	if err == nil || !strings.Contains(err.Error(), "independent verifier") {
		t.Fatalf("reporting agent self-verification was not rejected: %v", err)
	}
}

func TestUtilityCLIGateRejectsInvalidNumericThresholds(t *testing.T) {
	for _, args := range [][]string{
		{"contextlattice", "utility", "gate", "--minimum-pairs", "many", "--raw"},
		{"contextlattice", "utility", "gate", "--minimum-gain-per-1k", "NaN", "--raw"},
		{"contextlattice", "utility", "gate", "--maximum-failure-rate", "+Inf", "--raw"},
	} {
		c := newCLI(ioDiscard{}, ioDiscard{})
		if err := c.run(args); err == nil {
			t.Fatalf("invalid utility gate threshold was silently accepted: %v", args)
		}
	}
}

func TestContextPackOutcomeAcceptsUtilityOnlyEvidence(t *testing.T) {
	parsed := parseArgs([]string{
		"--context-pack-quality-sample-id", "sample_utility", "--outcome-id", "outcome_utility",
		"--utility-value", "7.5", "--utility-unit", "acceptance_points",
		"--verification-event-id", "evt_utility", "--verification-evidence-digest", "sha256:" + strings.Repeat("a", 64),
		"--verification-passed", "true", "--verifier-kind", "deterministic_test", "--verifier-id", "go_holdout",
		"--latency-ms", "42", "--cost-microusd", "11", "--tool-calls", "3", "--failures", "0",
		"--pair-id", "pair_utility", "--pair-arm", "treatment", "--matched-control-outcome-id", "control_utility",
		"--task-match-digest", "sha256:" + strings.Repeat("b", 64), "--matching-method", "exact_holdout", "--leakage-free", "true",
		"--experiment-id", "experiment_utility", "--assignment-digest", "sha256:" + strings.Repeat("c", 64),
		"--pair-model", "gpt-test", "--pair-runner", "test-runner", "--pair-harness", "go-test",
		"--context-reconstruction-contract", "agent_packet_reconstruction.v1",
	}, adapterStringFlags(), adapterBoolFlags())
	payload, requested, err := buildContextPackOutcomePayload(parsed, "contextlattice", "session_utility", adapterProfile{agent: "codex", agentID: "codex_test"}, "test")
	if err != nil || !requested {
		t.Fatalf("utility-only outcome was rejected: requested=%v err=%v", requested, err)
	}
	utility := asMap(payload["utility"])
	if utility["value"] != 7.5 || !asBool(utility["verification_passed"]) || firstString(utility["verifier_id"]) != "go_holdout" {
		t.Fatalf("utility proof fields missing: %#v", utility)
	}
	if asInt(asMap(payload["economics"])["latency_ms"]) != 42 || !asBool(asMap(payload["pairing"])["leakage_free"]) {
		t.Fatalf("utility economics or pairing missing: %#v", payload)
	}
	if firstString(asMap(payload["pairing"])["model"]) != "gpt-test" || firstString(asMap(payload["pairing"])["assignment_digest"]) == "" {
		t.Fatalf("matched-control execution context missing: %#v", payload["pairing"])
	}
	if !contextPackOutcomeRequested(parsed) {
		t.Fatal("utility flags did not request outcome reporting")
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
			response := graphCorpusTestRefreshResponse()
			if firstString(payload["project"]) == "empty-graph" {
				response["ok"] = false
				health := asMap(response["case_set_health"])
				health["valid"] = false
				health["benchmark_eligible"] = false
				health["status"] = "invalid"
				health["issues"] = []any{map[string]any{"code": "insufficient_holdout"}}
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/memory/recall/evaluate/saved":
			_ = json.NewEncoder(w).Encode(graphCorpusTestEvaluationResponse())
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer gateway.Close()

	for _, args := range [][]string{
		{"contextlattice_memory_graph_repair", "--project", "alpha", "--max-writes", "25", "--raw"},
		{"contextlattice_memory_graph_efficacy", "--refresh-cases", "--project", "alpha", "--topic-prefix", "runbooks/cache", "--raw"},
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
	if !asBool(refresh["graph_corpus"]) || firstString(refresh["seed"]) != "graph-v1" || asInt(refresh["development_cases"]) != 200 || asInt(refresh["holdout_cases"]) != 100 || firstString(refresh["topic_prefix"]) != "runbooks/cache" {
		t.Fatalf("expected frozen graph-corpus refresh payload: %#v", refresh)
	}
	if _, present := refresh["include_graph_cases"]; present {
		t.Fatalf("graph efficacy must not use the ordinary eval-cases refresh contract: %#v", refresh)
	}
	if len(captured["/memory/recall/evaluate/saved"]) != 1 {
		t.Fatalf("expected one saved evaluation request: %#v", captured)
	}
	evaluation := captured["/memory/recall/evaluate/saved"][0]
	if firstString(evaluation["mode"]) != "graph" || !asBool(evaluation["graph_corpus"]) || firstString(evaluation["split"]) != "holdout" || firstString(evaluation["project"]) != "alpha" || firstString(evaluation["topic_prefix"]) != "runbooks/cache" {
		t.Fatalf("graph efficacy did not use the closed graph evaluator contract: %#v", evaluation)
	}
	binding := asMap(evaluation["graph_corpus_binding"])
	if firstString(binding["case_set_digest"]) != "sha256:graph-case" || firstString(binding["manifest_digest"]) != "sha256:graph-manifest" {
		t.Fatalf("graph efficacy did not carry the refreshed corpus identity into evaluation: %#v", evaluation)
	}

	for _, invalid := range [][]string{
		{"contextlattice_memory_graph_efficacy", "--project", "alpha", "--topic-prefix", "runbooks//cache", "--raw"},
		{"contextlattice_memory_graph_efficacy", "--project", "alpha", "--topic-prefix", "runbooks/cache", "--topic-prefix=runbooks/other", "--raw"},
	} {
		invalidCLI := newCLI(ioDiscard{}, ioDiscard{})
		invalidCLI.baseURL = gateway.URL
		if err := invalidCLI.run(invalid); err == nil {
			t.Fatalf("malformed or duplicate graph topic prefix was accepted: %v", invalid)
		}
	}

	var failedStdout bytes.Buffer
	failedCLI := newCLI(&failedStdout, ioDiscard{})
	failedCLI.baseURL = gateway.URL
	if err := failedCLI.run([]string{"contextlattice_memory_graph_efficacy", "--refresh-cases", "--project", "empty-graph", "--raw"}); err == nil || !strings.Contains(err.Error(), "failed closed") {
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

func TestMemoryGraphEfficacyRejectsInadequateFrozenCorpusBeforeEvaluation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "split quota",
			mutate: func(response map[string]any) {
				asMap(response["case_set_health"])["holdout_count"] = 99
			},
			want: "holdout_count",
		},
		{
			name: "relation quota",
			mutate: func(response map[string]any) {
				topology := asMap(asMap(response["case_set_health"])["topology_cases"])
				topology["references"] = 89
			},
			want: "topology_cases.references",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var evaluationCalls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/memory/recall/eval-cases/refresh":
					response := graphCorpusTestRefreshResponse()
					test.mutate(response)
					_ = json.NewEncoder(w).Encode(response)
				case "/memory/recall/evaluate/saved":
					evaluationCalls.Add(1)
					_ = json.NewEncoder(w).Encode(graphCorpusTestEvaluationResponse())
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			err := c.run([]string{"contextlattice_memory_graph_efficacy", "--refresh-cases", "--project", "alpha", "--raw"})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("inadequate frozen %s was accepted: err=%v output=%s", test.name, err, stdout.String())
			}
			if evaluationCalls.Load() != 0 {
				t.Fatalf("inadequate frozen corpus crossed into evaluation: calls=%d", evaluationCalls.Load())
			}
		})
	}
}

func TestMemoryGraphEfficacyRequiresAuthoritativeTopLevelGates(t *testing.T) {
	var evaluationCalls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/memory/recall/evaluate/saved" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		evaluationCalls.Add(1)
		response := graphCorpusTestEvaluationResponse()
		response["ok"] = false
		response["passed"] = false
		asMap(response["promotion"])["promotion_eligible"] = false
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{"contextlattice_memory_graph_efficacy", "--project", "alpha", "--raw"})
	if err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("authoritative top-level false gates were accepted: err=%v output=%s", err, stdout.String())
	}
	if evaluationCalls.Load() != 1 {
		t.Fatalf("expected one evaluation request, got %d", evaluationCalls.Load())
	}
	result := map[string]any{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode CLI result: %v output=%s", err, stdout.String())
	}
	if asBool(result["ok"]) {
		t.Fatalf("CLI reported success for false authoritative gates: %#v", result)
	}
}

func TestMemoryGraphEfficacyRequiresExplicitGraphContributionStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "missing status",
			mutate: func(response map[string]any) {
				delete(asMap(asMap(response["metrics"])["graphContribution"]), "status")
			},
		},
		{
			name: "wrong status",
			mutate: func(response map[string]any) {
				asMap(asMap(response["metrics"])["graphContribution"])["status"] = "failed"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/memory/recall/evaluate/saved" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				response := graphCorpusTestEvaluationResponse()
				test.mutate(response)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			err := c.run([]string{"contextlattice_memory_graph_efficacy", "--project", "alpha", "--raw"})
			if err == nil || !strings.Contains(err.Error(), "gate") {
				t.Fatalf("non-passed graph contribution status was accepted: err=%v output=%s", err, stdout.String())
			}
		})
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
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	t.Setenv("HOME", "")
	t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", "")
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents/sessions/sess-bootstrap/rollup" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "rollup": map[string]any{"agent_inbox": map[string]any{"items": []any{}}}})
			return
		}
		if r.URL.Path != "/v1/agents/preflight" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode bootstrap request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(adapterTestPreflightResponse(request, "sess-bootstrap"))
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_agent_adapter", "bootstrap", "--agent", "codex", "--agent-id", "codex_gpt5", "--project", "alpha", "--query", "bootstrap smoke", "--mode", "fast"}); err != nil {
		t.Fatalf("run adapter bootstrap: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if strings.Contains(stdout.String(), "sess-bootstrap") {
		t.Fatalf("bootstrap output leaked the nonpublic session identity: %s", stdout.String())
	}
	if output["ok"] != true || output["session_id"] != nil || len(asList(output["identity_omitted"])) != 1 {
		t.Fatalf("unexpected bootstrap output: %#v", output)
	}
	preflight := output["result"].(map[string]any)["preflight"].(map[string]any)
	if preflight["raw_omitted"] != true {
		t.Fatalf("expected compact preflight output: %#v", preflight)
	}
	if _, ok := preflight["agent_profile"]; ok {
		t.Fatalf("compact preflight leaked raw agent profile: %#v", preflight)
	}
	if preflight["session_id"] != nil || asMap(preflight["agent_runtime"])["session_id"] != nil ||
		firstString(asList(preflight["identity_omitted"])[0]) != "session_id" {
		t.Fatalf("compact preflight leaked or lost omission evidence for a nonpublic session identity: %#v", preflight)
	}
	if _, err := os.Stat(filepath.Join(workingDir, ".contextlattice")); !os.IsNotExist(err) {
		t.Fatalf("headless bootstrap wrote optional session state into cwd: %v", err)
	}
}

func TestAdapterBootstrapRejectsStaleOrForeignPreflightBeforeStateWrite(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "foreign project", mutate: func(response map[string]any) { response["project"] = "foreign-project" }},
		{name: "foreign session authority", mutate: func(response map[string]any) {
			asMap(asMap(response["agent_runtime"])["session"])["agent_id"] = "foreign-agent-id"
		}},
		{name: "foreign reuse key", mutate: func(response map[string]any) {
			asMap(asMap(response["agent_runtime"])["session"])["reuse_key"] = "foreign-reuse-key"
		}},
		{name: "terminal session", mutate: func(response map[string]any) {
			asMap(asMap(response["agent_runtime"])["session"])["status"] = "completed"
		}},
		{name: "decimal registry version", mutate: func(response map[string]any) {
			asMap(response["format_contracts"])["registry_version"] = json.Number(fmt.Sprintf("%d.0", generatedAgentContractRegistryVersion))
			stabilizeTestPreflightResponse(response)
		}},
		{name: "stale outer byte accounting", mutate: func(response map[string]any) {
			asMap(response["format_contracts"])["actual_json_bytes"] = 1
		}},
		{name: "missing registered contract", mutate: func(response map[string]any) {
			asMap(response["format_contracts"])["contracts"] = []any{"agent_preflight_response.v1", "objective_runtime_state.v1"}
		}},
		{name: "invalid objective runtime attestation", mutate: func(response map[string]any) {
			asMap(asMap(response["objective_runtime"])["format_contract"])["contract_valid"] = false
		}},
		{name: "invalid policy attestation", mutate: func(response map[string]any) {
			asMap(asMap(response["policy_context_package"])["format_contract"])["registry_version"] = generatedAgentContractRegistryVersion - 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			globalHome := t.TempDir()
			t.Setenv("HOME", globalHome)
			t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", globalHome)
			t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(globalHome, "seen.json"))
			privateMarker := "backend-preflight-marker-must-not-cross"
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/agents/preflight" {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode bootstrap request: %v", err)
				}
				response := adapterTestPreflightResponse(request, "sess-bootstrap-rejected")
				response["backend_marker"] = privateMarker
				test.mutate(response)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			err := c.run([]string{
				"contextlattice_agent_adapter", "bootstrap", "--agent", "codex", "--agent-id", "codex_gpt5",
				"--project", "alpha", "--query", "bootstrap authority rejection", "--mode", "fast", "--raw",
			})
			if err == nil {
				t.Fatalf("stale or foreign preflight was accepted: %s", stdout.String())
			}
			var output map[string]any
			if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
				t.Fatalf("decode bounded bootstrap failure: %v output=%s", decodeErr, stdout.String())
			}
			if output["ok"] != false || !adapterResponseContractValid(output) {
				t.Fatalf("rejected preflight did not produce a valid bounded failure: %#v", output)
			}
			if strings.Contains(stdout.String(), privateMarker) || strings.Contains(stdout.String(), "foreign-project") || strings.Contains(stdout.String(), "foreign-agent-id") || strings.Contains(stdout.String(), "foreign-reuse-key") {
				t.Fatalf("rejected preflight leaked backend authority: %s", stdout.String())
			}
			if _, statErr := os.Stat(sessionStatePath("alpha")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected preflight wrote local session state: %v", statErr)
			}
		})
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
					"outcome_id":                 "outcome_adapter",
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

func TestTraceCommandProofUsesCanonicalProofRouteAndRenderer(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/sessions/sess-proof/proof-timeline" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "schema_id": "agent_proof_timeline.v1",
			"session":     map[string]any{"id": "sess-proof", "agent": "codex", "status": "completed"},
			"integrity":   map[string]any{"status": "verified", "complete": true, "source_anchors_stable": true},
			"metrics":     map[string]any{"joined_row_count": 6, "source_row_count": 6, "eligible_exact_link_coverage": 1, "redaction_count": 0, "display_compacted_count": 2},
			"stage_order": []any{"context", "action", "correction", "verification", "outcome", "learning"},
			"stages": map[string]any{
				"context": map[string]any{"status": "present", "count": 1}, "action": map[string]any{"status": "present", "count": 1},
				"correction": map[string]any{"status": "present", "count": 1}, "verification": map[string]any{"status": "present", "count": 1},
				"outcome": map[string]any{"status": "present", "count": 1}, "learning": map[string]any{"status": "present", "count": 1},
			},
			"gaps": []any{},
			"timeline": []any{map[string]any{
				"ordered_at": "2026-07-16T03:00:00Z", "stage": "verification", "source": "agent_session", "summary": "tests passed",
			}},
			"rollback": map[string]any{"env": "CONTEXTLATTICE_AGENT_PROOF_TIMELINE_ENABLED=false", "fallback_schema": "agent_run_trace.v1"},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_agent_trace", "--session-id", "sess-proof", "--preset", "proof"}); err != nil {
		t.Fatalf("run proof trace: %v", err)
	}
	rendered := stdout.String()
	if !strings.Contains(rendered, "ContextLattice Proof Timeline") || !strings.Contains(rendered, "receipt: verified") || !strings.Contains(rendered, "compacted 2") || !strings.Contains(rendered, "tests passed") {
		t.Fatalf("unexpected proof render:\n%s", rendered)
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

func TestRuntimeAuditsRejectStaleRegistry(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		audit func(map[string]any) map[string]any
		body  map[string]any
	}{
		{
			name: "context boundary", audit: auditContextBoundary,
			body: map[string]any{"schema_id": "contextlattice_context_boundary.v1", "ok": true, "registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion - 1, "routes": []any{}},
		},
		{
			name: "native ownership", audit: auditNativeOwnership,
			body: map[string]any{"schema_id": "strict_runtime_native_ownership.v1", "ok": true, "registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion - 1, "routes": []any{}, "pythonHotPathOwnership": map[string]any{"fallbacks": 0}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := testCase.audit(testCase.body)
			if asBool(result["ok"]) {
				t.Fatalf("stale registry passed audit: %#v", result)
			}
			found := false
			for _, finding := range result["findings"].([]map[string]any) {
				if firstString(finding["reason"]) == "registry_version_mismatch" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("stale registry finding missing: %#v", result)
			}
		})
	}
}

func TestRuntimeAuditsRejectStaleGeneratedAt(t *testing.T) {
	stale := time.Now().Add(-runtimeAuditMaximumAge - time.Minute).UTC().Format(time.RFC3339Nano)
	for _, testCase := range []struct {
		name  string
		audit func(map[string]any) map[string]any
		body  map[string]any
	}{
		{
			name: "context boundary", audit: auditContextBoundary,
			body: map[string]any{"schema_id": "contextlattice_context_boundary.v1", "ok": true, "generatedAt": stale, "registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion, "routes": []any{}},
		},
		{
			name: "native ownership", audit: auditNativeOwnership,
			body: map[string]any{"schema_id": "strict_runtime_native_ownership.v1", "ok": true, "generatedAt": stale, "registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion, "routes": []any{}, "pythonHotPathOwnership": map[string]any{"fallbacks": 0}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := testCase.audit(testCase.body)
			if asBool(result["ok"]) {
				t.Fatalf("stale runtime response passed audit: %#v", result)
			}
			found := false
			for _, finding := range result["findings"].([]map[string]any) {
				if firstString(finding["reason"]) == "generated_at_stale" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("stale generatedAt finding missing: %#v", result)
			}
		})
	}
}

func TestRuntimeAuditCommandsReturnErrorAfterEmittingFailure(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		binaryName string
		path       string
		schemaID   string
	}{
		{name: "context boundary", binaryName: "contextlattice_context_boundary", path: "/ops/context-boundary", schemaID: "contextlattice_context_boundary.v1"},
		{name: "native ownership", binaryName: "contextlattice_strict_runtime_native_ownership", path: "/ops/native-ownership", schemaID: "strict_runtime_native_ownership.v1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != testCase.path {
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"schema_id": testCase.schemaID, "ok": true, "generatedAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
					"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion,
					"routes": []any{}, "pythonHotPathOwnership": map[string]any{"fallbacks": 0},
				})
			}))
			defer gateway.Close()
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run([]string{testCase.binaryName}); err == nil {
				t.Fatal("failed runtime audit returned process success")
			}
			payload := map[string]any{}
			if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
				t.Fatalf("decode emitted audit: %v", err)
			}
			if asBool(payload["ok"]) {
				t.Fatalf("failed runtime audit emitted success: %#v", payload)
			}
		})
	}
}
