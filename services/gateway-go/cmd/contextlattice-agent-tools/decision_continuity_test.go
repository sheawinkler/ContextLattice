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

func cliDecisionTestPayload(kind string) map[string]any {
	selector := map[string]any{"schema_id": "session_decision_binding.v1", "project": "alpha", "objective_id": "obj_test", "task_identity_id": "task_test",
		"repo": "test-repo", "branch": "test-branch", "decision_change_id": "dc_test", "after_decision_id": "decision_test"}
	payload := map[string]any{"project": "alpha", "agent_id": "test-agent", "session_id": "sess_test", "objective": "File-provided intent", "repo": "test-repo", "branch": "test-branch", "task_identity_id": "task_test", "decision_binding": selector}
	if kind == "decision" {
		return map[string]any{"project": "alpha", "idempotency_key": "stable-test", "applicability": map[string]any{"schema_id": "decision_applicability.v1", "inputs": []any{map[string]any{"memory_id": "alpha::b"}, map[string]any{"memory_id": "alpha::a"}}, "assumptions": []any{}}}
	}
	if kind == "ack" {
		payload["type"], payload["event_id"] = "decision.bound", "evt_test"
		payload["decision_ack"] = map[string]any{"schema_id": "session_decision_ack.v1", "sequence": 2, "expected_baseline": map[string]any{"schema_id": "session_decision_baseline.v1", "incarnation": "sdi_original", "revision": 1, "binding_digest": strings.Repeat("a", 64)}}
	}
	return payload
}

func cliDecisionTestReceipt(kind string, payload map[string]any) map[string]any {
	if kind == "decision" {
		app := asMap(payload["applicability"])
		// Server canonicalization may reorder input/assumption arrays.
		rows := asList(app["inputs"])
		app["inputs"] = []any{rows[1], rows[0]}
		return map[string]any{"ok": true, "decision_change": map[string]any{"applicability": app}}
	}
	binding := map[string]any{}
	for key, value := range asMap(payload["decision_binding"]) {
		binding[key] = value
	}
	binding["record_digest"] = strings.Repeat("b", 64)
	baseline := map[string]any{"schema_id": "session_decision_baseline.v1", "revision": 1, "incarnation": "sdi_original", "binding_digest": strings.Repeat("c", 64)}
	session := map[string]any{"id": "sess_test", "decision_binding": binding, "decision_baseline": baseline}
	if kind == "ack" {
		baseline["revision"] = 2
		return map[string]any{"ok": true, "session": session, "event": map[string]any{"id": payload["event_id"], "session_id": payload["session_id"], "type": "decision.bound",
			"metadata": map[string]any{"decision_binding": binding, "decision_baseline": baseline}}}
	}
	return map[string]any{"ok": true, "session": session}
}

func TestDecisionContinuityCLINegotiatesBeforeTypedWriteAndChecksReceipt(t *testing.T) {
	for _, kind := range []string{"decision", "start", "ensure", "ack"} {
		for _, mode := range []string{"old_server", "missing_receipt", "ready_protocol_owner_unavailable"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				payload := cliDecisionTestPayload(kind)
				raw, _ := json.Marshal(payload)
				file := filepath.Join(t.TempDir(), "intent.json")
				if err := os.WriteFile(file, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				calls := []string{}
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls = append(calls, r.Method+" "+r.URL.Path)
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/ops/capabilities" {
						if mode == "old_server" {
							_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true})
						} else {
							_ = json.NewEncoder(w).Encode(map[string]any{"decisionContinuity": map[string]any{"version": 1, "manual_binding": true, "available": false}})
						}
						return
					}
					var received map[string]any
					if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
						t.Error(err)
					}
					if received["project"] != "alpha" {
						t.Error("file scope overwritten by CLI default")
					}
					if kind != "decision" && (received["branch"] != "test-branch" || received["repo"] != "test-repo" || received["task_identity_id"] != "task_test") {
						t.Error("file ownership overwritten by process defaults")
					}
					response := map[string]any{"ok": true}
					if mode != "missing_receipt" {
						response = cliDecisionTestReceipt(kind, received)
					}
					_ = json.NewEncoder(w).Encode(response)
				}))
				defer gateway.Close()
				var output bytes.Buffer
				c := newCLI(&output, ioDiscard{})
				c.baseURL = gateway.URL
				args := []string{"contextlattice_agent_session", kind, "--payload-file", file, "--raw"}
				if kind == "decision" {
					args = []string{"contextlattice_decision_change", "--payload-file", file, "--raw"}
				} else if kind == "ack" {
					args[1] = "event"
				}
				err := c.run(args)
				if mode == "ready_protocol_owner_unavailable" {
					if err != nil || len(calls) != 2 || calls[0] != "GET /ops/capabilities" || output.Len() == 0 {
						t.Fatal("supported typed request/receipt failed or made extra calls", err, calls)
					}
				} else if err == nil || output.Len() != 0 || (mode == "old_server" && len(calls) != 1) || (mode == "missing_receipt" && len(calls) != 2) {
					t.Fatal("old protocol or dropped receipt became success or attempted a forbidden write", err, calls)
				}
			})
		}
	}
}

func TestDecisionContinuityCLINullNeverDowngradesAndFlagsOverrideOnlyExplicitly(t *testing.T) {
	file := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(file, []byte(`{"applicability":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	c := newCLI(&output, ioDiscard{})
	c.baseURL = "http://127.0.0.1:1"
	if err := c.cmdDecisionChange([]string{"--payload-file", file}); err == nil || !strings.Contains(err.Error(), "non-null") {
		t.Fatal("null typed intent was removed or sent as legacy", err)
	}
	payload := map[string]any{"branch": "file-branch", "project": "file-project", "decision_binding": nil, "metadata": map[string]any{"file": true}}
	parsed := parsedArgs{values: map[string]string{"branch": "explicit-branch"}}
	overlaySessionDefaults(payload, map[string]any{"branch": "explicit-branch", "project": "default-project", "metadata": map[string]any{}}, parsed)
	if payload["branch"] != "explicit-branch" || payload["project"] != "file-project" || len(asMap(payload["metadata"])) != 1 || !cliDecisionTyped(payload, "decision_binding") {
		t.Fatal("defaults overwrote file values or removed explicit invalid intent", payload)
	}
}

func TestDecisionContinuityCLIAckReplayChecksOriginalEventNotLatestBinding(t *testing.T) {
	payload := cliDecisionTestPayload("ack")
	response := cliDecisionTestReceipt("ack", payload)
	asMap(response["session"])["decision_binding"] = map[string]any{"newer": true}
	if !cliDecisionAckReceipt(payload, response) {
		t.Fatal("historical receipt was confused with the latest binding")
	}
	asMap(response["event"])["id"] = "wrong_event"
	if cliDecisionAckReceipt(payload, response) {
		t.Fatal("different acknowledgement event was accepted")
	}
}

func TestDecisionContinuityCLIFileOuterAliasesDoNotLoseSelectionOrScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	payload := cliDecisionTestPayload("ensure")
	for canonical, alias := range map[string]string{"project": "project_name", "session_id": "sessionId", "task_identity_id": "taskIdentityId", "agent_id": "agentId"} {
		payload[alias] = payload[canonical]
		delete(payload, canonical)
	}
	file := filepath.Join(t.TempDir(), "aliases.json")
	raw, _ := json.Marshal(payload)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ops/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"decisionContinuity": map[string]any{"version": 1, "manual_binding": true}})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(cliDecisionTestReceipt("ensure", captured))
	}))
	defer gateway.Close()
	var output bytes.Buffer
	c := newCLI(&output, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.cmdSession([]string{"ensure", "--payload-file", file}); err != nil {
		t.Fatal(err)
	}
	if captured["project"] != "alpha" || captured["session_id"] != "sess_test" || captured["task_identity_id"] != "task_test" || captured["agent_id"] != "test-agent" || captured["reuse_key"] != nil {
		t.Fatal("outer aliases lost scope/selection or acquired a synthetic reuse key", captured["project"], captured["session_id"], captured["reuse_key"])
	}
}

func TestDecisionContinuityCLIReceiptRecognizesOnlyOwnerHashCanonicalization(t *testing.T) {
	for _, spelling := range []string{strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 64), " SHA256:" + strings.Repeat("A", 64) + " "} {
		request := map[string]any{"schema_id": "decision_applicability.v1", "inputs": []any{map[string]any{"memory_id": "alpha::input", "target_event_id": "original", "target_content_hash": spelling}}, "assumptions": []any{}}
		canonical := map[string]any{"schema_id": "decision_applicability.v1", "inputs": []any{map[string]any{"memory_id": "alpha::input", "target_event_id": "original", "target_content_hash": strings.Repeat("a", 64)}}, "assumptions": []any{}}
		if !cliDecisionJSONEqual(request, canonical) {
			t.Fatal("valid server-normalized hash was treated as an unconfirmed write", spelling)
		}
		asMap(asList(canonical["inputs"])[0])["target_event_id"] = "replacement"
		if cliDecisionJSONEqual(request, canonical) {
			t.Fatal("hash canonicalization hid an event re-pin")
		}
	}
}

func TestDecisionContinuityCLIOuterAliasConflictsAndEventIDStayExplicit(t *testing.T) {
	for canonical, alias := range map[string]string{"project": "project_name", "session_id": "sessionId", "agent_id": "agentId", "task_id": "taskId", "task_identity_id": "taskIdentityId", "execution_lane_id": "executionLaneId", "native_session_id": "nativeSessionId", "reuse_key": "reuseKey", "agent_kind": "runner"} {
		payload := map[string]any{alias: "from-file"}
		if err := normalizeSessionFileAliases(payload, true); err != nil || payload[canonical] != "from-file" || payload[alias] != nil {
			t.Fatal("supported outer alias was not normalized", canonical, err)
		}
		payload[alias] = "conflict"
		if err := normalizeSessionFileAliases(payload, true); err == nil {
			t.Fatal("conflicting outer aliases were silently resolved", canonical)
		}
	}
	event := map[string]any{"id": "event-id", "sessionId": "session-id", "decision_binding": map[string]any{"TaskIdentityId": "do-not-normalize-nested"}}
	if err := normalizeSessionFileAliases(event, false); err != nil || event["id"] != "event-id" || event["session_id"] != "session-id" || asMap(event["decision_binding"])["TaskIdentityId"] != "do-not-normalize-nested" {
		t.Fatal("outer normalization changed event identity or relaxed a nested contract", err)
	}
}
