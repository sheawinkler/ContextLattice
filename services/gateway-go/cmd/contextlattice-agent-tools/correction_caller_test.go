package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCorrectRejectsForeignSessionBeforeFeedback(t *testing.T) {
	for _, fault := range []string{"actor", "project", "native_flag", "native_env", "unavailable", "cached", "foreign_scope"} {
		t.Run(fault, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CONTEXTLATTICE_NATIVE_SESSION_ID", "")
			var gets, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					w.WriteHeader(http.StatusConflict)
					return
				}
				gets.Add(1)
				if r.URL.Path != "/v1/agents/sessions/selected" || fault == "unavailable" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				row := map[string]any{"id": "selected", "project": "alpha", "agent_id": "pi_agent", "status": "active", "native_session_id": "other-run"}
				if fault == "actor" || fault == "cached" {
					row["agent_id"] = "another-actor"
				}
				if fault == "project" {
					row["project"] = "another-project"
				}
				if fault == "foreign_scope" {
					row["scope_ownership"] = map[string]any{"schema_id": "agent_session_scope_ownership.v1", "kind": "unknown", "project": "alpha"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": row})
			}))
			defer server.Close()
			c := newCLI(io.Discard, io.Discard)
			c.baseURL = server.URL
			args := []string{"contextlattice", "correct", "review note", "--category", "stale", "--project", "alpha", "--agent-id", "pi_agent", "--raw"}
			if fault == "cached" {
				nativeCaller{agentID: "pi_agent"}.writeSessionState("alpha", "selected", "test", nil)
			} else {
				args = append(args, "--session-id", "selected")
			}
			if fault == "native_flag" {
				args = append(args, "--native-session-id", "intended-run")
			} else if fault == "native_env" {
				t.Setenv("CONTEXTLATTICE_NATIVE_SESSION_ID", "intended-run")
			}
			if err := c.run(args); err == nil || gets.Load() != 1 || writes.Load() != 0 {
				t.Fatalf("expected one authority lookup and no writes: err=%v gets=%d writes=%d", err, gets.Load(), writes.Load())
			}
		})
	}
}

func TestCorrectDoesNotTrustRequestCacheReceipt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_SESSION_ID", "")
	t.Setenv("CONTEXTLATTICE_CONTEXT_PACK_QUALITY_SAMPLE_ID", "")
	var writes atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
		}
		if r.URL.Path != "/tools/feedback_submit" {
			http.NotFound(w, r)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		feedback := map[string]any{"id": "cache-only", "source": "agent", "project": payload["project"],
			"metadata": payload["metadata"], "content": payload["content"]}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "recorded": true, "feedback": feedback,
			"correction_operation_id": payload["idempotencyKey"], "idempotencyScope": "request"})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, io.Discard)
	c.baseURL = gateway.URL
	err := c.run([]string{"contextlattice", "correct", "review note", "--category", "useful",
		"--project", "alpha", "--factual", "--subject", "release", "--predicate", "version", "--object", "new", "--raw"})
	var out map[string]any
	if json.Unmarshal(stdout.Bytes(), &out) != nil || err == nil || writes.Load() != 1 ||
		out["status"] != "failed" || out["feedback_saved"] != false || out["claim_persisted"] != false ||
		out["event_recorded"] != false || out["feedback"] != nil {
		t.Fatalf("a request-cache reply advanced the correction: err=%v writes=%d out=%v", err, writes.Load(), out)
	}
}
