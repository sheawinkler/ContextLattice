package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func continuousCognitionCLITestResponse(operation string) map[string]any {
	payload := map[string]any{
		"ok": true, "schema_id": continuousCognitionCLIContractID, "version": 1,
		"cognition_id": "cc_000000000000000000000000", "operation": operation,
		"phase": "frontier", "decision": "stop",
		"request_scope": map[string]any{
			"scope_digest": "sha256:" + strings.Repeat("1", 64), "query_digest": "sha256:" + strings.Repeat("2", 64),
			"workspace_ref": "ref_workspace_opaque", "project_ref": "ref_project_opaque", "topic_ref": "ref_topic_opaque",
			"agent_ref": "ref_agent_opaque", "session_ref": "ref_session_opaque", "task_ref": "ref_task_opaque",
			"task_identity_ref": "ref_task_identity_opaque", "execution_lane_ref": "ref_execution_lane_opaque",
			"retrieval_intent": "decision", "cycle_ref": "cycle_opaque",
		},
		"observation": map[string]any{
			"objective_graph_ref": "ref_objective_opaque", "session_rollup_ref": "ref_session_rollup_opaque",
			"continuity_zero_ref": "ref_continuity_opaque", "proof_timeline_ref": "ref_proof_opaque",
			"retrieval_plan_ref": "ref_retrieval_opaque", "utility_snapshot_ref": "ref_utility_opaque",
			"lifecycle_proof_ref": "ref_lifecycle_opaque", "source_anchor_digest": "sha256:" + strings.Repeat("3", 64),
			"source_complete": false, "gaps": []any{},
		},
		"frontier": map[string]any{
			"frontier_id": "frontier_opaque", "objective_state": "unknown", "uncertainty": 1.0,
			"next_action_class": "stop", "utility_score": 0.0,
			"expected_utility": map[string]any{"action_change_probability": 0.0, "consequence_if_wrong": 0.0, "evidence_reliability": 0.0, "acquisition_cost": 0.0, "score": 0.0},
			"stop_reason":      "bounded_evidence_only",
		},
		"investigation": map[string]any{
			"state": "not_requested", "mode": "read_only", "context_pack_ref": "ref_context_pack_opaque",
			"retrieval_receipt_ref": "ref_receipt_opaque",
			"source_coverage":       map[string]any{"complete": false, "retrieval_count": 0, "compiler_count": 0, "evidence_ref_count": 0, "scanned_count": 0, "truncated": false, "learned_ranking_state": "control_shadow_only", "raw_material_exposed": false},
			"mutations_suppressed":  true, "execution_performed": false, "network_calls": 0,
		},
		"activation": map[string]any{
			"state": "absent", "prep_id": "ref_prep_opaque", "approval_ref": "ref_approval_opaque",
			"authorization_ref": "ref_authorization_opaque", "consumption_ref": "ref_consumption_opaque",
			"execution_owner": "external_cli_worker", "one_shot": true,
			"requires_explicit_cli_use": true, "gateway_execution_performed": false,
		},
		"outcome":    map[string]any{"state": "absent", "outcome_ref": "ref_outcome_opaque", "proof_ref": "ref_outcome_proof_opaque", "utility_observation_ref": "ref_utility_observation_opaque", "independently_verified": false, "causal_eligible": false},
		"evaluation": map[string]any{"state": "unavailable", "utility_status": "unverified", "verified": false, "causal_eligible": false, "reason": "opaque_evidence_only"},
		"rollback":   map[string]any{"state": "not_recommended", "reason_ref": "ref_rollback_opaque", "target_ref": "ref_target_opaque"},
		"retirement": map[string]any{"state": "not_recommended", "reason_ref": "ref_retirement_opaque", "target_ref": "ref_target_opaque"},
		"progress": map[string]any{
			"status": "observed", "stage": "frontier", "round": 0, "max_rounds": 3, "proof_timeline_ref": "ref_proof_opaque",
			"loop_guard": map[string]any{"cycle_ref": "cycle_opaque", "source_anchor_digest": "sha256:" + strings.Repeat("3", 64), "round": 0, "max_rounds": 3, "dedupe_decision": "new", "persisted": false},
		},
		"safety": map[string]any{
			"advisory_only": true, "automatic_model_execution": false, "automatic_external_mutation": false,
			"runner_dispatch": false, "filesystem_mutation": false, "gateway_execution_performed": false,
			"requires_explicit_authorization": true, "requires_external_worker": true, "network_calls": 0,
		},
		"gaps": []any{}, "writeback_required": true,
		"format_contract": map[string]any{
			"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion,
			"schema_id": continuousCognitionCLIContractID, "contract_version": 1,
			"required_output_mode": "json_object", "validator": "contextlattice.boundary.v1",
			"contract_valid": true, "validation": map[string]any{"status": "passed", "errors": []any{}},
		},
	}
	payload["cognition_digest"] = continuousCognitionCLIDigest(payload)
	return payload
}

func TestContinuousCognitionCLIProjectsAllOperationsThroughOneBoundedRequest(t *testing.T) {
	operations := []string{"observe", "investigate", "status", "outcome", "evaluate", "rollback", "retire"}
	requestCount := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodPost || r.URL.Path != continuousCognitionCLIPath {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "cli-test-key" {
			t.Fatalf("shared authenticated request helper did not apply the API key")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if _, exists := request["workspace_ref"]; exists {
			t.Fatalf("CLI sent caller-controlled workspace authority: %#v", request)
		}
		if firstString(request["as_of"]) != "2026-08-08T12:00:00Z" || firstString(request["project"]) != "contextlattice" || firstString(request["task_id"]) != "task-cli" {
			t.Fatalf("CLI request lost its fixed identity boundary: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(continuousCognitionCLITestResponse(firstString(request["operation"])))
	}))
	defer gateway.Close()

	for _, operation := range operations {
		t.Run(operation, func(t *testing.T) {
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			c.apiKey = "cli-test-key"
			if err := c.run([]string{
				"contextlattice_continuous_cognition", operation, "bounded cognition request " + operation,
				"--project", "contextlattice", "--task-id", "task-cli", "--as-of", "2026-08-08T12:00:00Z", "--raw",
			}); err != nil {
				t.Fatalf("run %s: %v output=%s", operation, err, stdout.String())
			}
			var response map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || firstString(response["operation"]) != operation {
				t.Fatalf("invalid %s response: err=%v payload=%s", operation, err, stdout.String())
			}
		})
	}
	if requestCount != len(operations) {
		t.Fatalf("operation matrix made %d requests, want %d", requestCount, len(operations))
	}
	if nativeToolNames["contextlattice_continuous_cognition"] != "continuous-cognition" {
		t.Fatalf("continuous cognition native alias is not registered")
	}
}

func TestContinuousCognitionCLIRejectsRawDisclosureAndDoesNotRetry(t *testing.T) {
	const privateTask = "private task material must stay bounded"
	requests := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		response := continuousCognitionCLITestResponse("observe")
		asMap(response["evaluation"])["reason"] = privateTask
		response["cognition_digest"] = continuousCognitionCLIDigest(response)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{"contextlattice", "continuous-cognition", "observe", privateTask, "--as-of", "2026-08-08T12:00:00Z", "--raw"})
	if err == nil || stdout.Len() != 0 || requests != 1 {
		t.Fatalf("raw disclosure was not rejected exactly once: err=%v requests=%d output=%s", err, requests, stdout.String())
	}
}

func TestContinuousCognitionCLITransportFailureIsOneShot(t *testing.T) {
	requests := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unavailable"})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	err := c.run([]string{"contextlattice_continuous_cognition", "status", "bounded status request", "--as-of", "2026-08-08T12:00:00Z", "--raw"})
	if err == nil || requests != 1 || stdout.Len() != 0 {
		t.Fatalf("transport failure retried or emitted unsafe output: err=%v requests=%d output=%s", err, requests, stdout.String())
	}
}

func TestContinuousCognitionCLIDoesNotFollowRedirects(t *testing.T) {
	var sourceCalls atomic.Int64
	var destinationCalls atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sourceCalls.Add(1)
		w.Header().Set("Location", destination.URL+continuousCognitionCLIPath)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	c := newCLI(&bytes.Buffer{}, ioDiscard{})
	c.baseURL = source.URL
	err := c.run([]string{"contextlattice_continuous_cognition", "observe", "redirect bounded request", "--as-of", "2026-08-08T12:00:00Z", "--raw"})
	if err == nil || sourceCalls.Load() != 1 || destinationCalls.Load() != 0 {
		t.Fatalf("continuous cognition followed or retried a redirect: err=%v source=%d destination=%d", err, sourceCalls.Load(), destinationCalls.Load())
	}
}

func TestContinuousCognitionCLIRejectsInvalidInputBeforeHTTP(t *testing.T) {
	requests := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer gateway.Close()

	for _, args := range [][]string{
		{"contextlattice_continuous_cognition", "activate", "bounded request"},
		{"contextlattice_continuous_cognition", "observe", "bounded request", "--as-of", "not-a-time"},
		{"contextlattice_continuous_cognition", "observe", "bounded request", "--limit", "0"},
	} {
		c := newCLI(&bytes.Buffer{}, ioDiscard{})
		c.baseURL = gateway.URL
		if err := c.run(args); err == nil {
			t.Fatalf("invalid input was accepted: %#v", args)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid input reached HTTP %d times", requests)
	}
}
