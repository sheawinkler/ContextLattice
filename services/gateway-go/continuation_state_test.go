package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestContinuationStateIsMonotonicAcrossDurableFallback(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	token := "continuation-monotonic"

	for _, event := range []map[string]any{
		{"event": "skipped", "status": "max_inflight", "source": sourceLetta},
		{"event": "deferred", "status": "durable_queued", "source": sourceLetta},
		{"event": "deferred_retry", "status": "cooldown", "source": sourceLetta},
	} {
		s.publishContinuationEvent(token, event)
	}
	pending, ok := s.continuationStatusPayload(token, true)
	if !ok || anyToString(pending["status"]) != "running" || anyToString(anyMap(pending["retrieval_lifecycle"])["result_state"]) != "pending" {
		t.Fatalf("transient durable fallback became terminal: %#v", pending)
	}
	summary := anyMap(anyMap(pending["result"])["source_summary"])
	if len(anyToStringList(summary["returned_now"], 8)) != 0 || len(anyToStringList(summary["pending_sources"], 8)) != 1 {
		t.Fatalf("queued source was misreported as returned evidence: %#v", summary)
	}

	s.publishContinuationEvent(token, map[string]any{"event": "completed", "status": "ok", "source": sourceLetta})
	// A late retry event cannot reopen a terminal source.
	s.publishContinuationEvent(token, map[string]any{"event": "deferred_retry", "status": "max_inflight", "source": sourceLetta})
	ready, ok := s.continuationStatusPayload(token, true)
	if !ok || anyToString(ready["status"]) != "completed" || anyToString(anyMap(ready["retrieval_lifecycle"])["result_state"]) != "ready" {
		t.Fatalf("terminal success did not remain absorbing: %#v", ready)
	}
}

func TestContinuationSteeringEmitsProgressThenOneTerminalNotification(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	sessionID := "sess-monotonic-steering"
	if _, err := s.agentSessions.start(map[string]any{
		"session_id": sessionID, "agent": "codex", "agent_id": "codex_test",
		"project": "contextlattice", "objective": "prove monotonic continuation notifications",
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	request := map[string]any{"session_id": sessionID, "agent_id": "codex_test", "project": "contextlattice"}
	token := "steering-monotonic"

	skipped := continuationEventWithRequest(request, map[string]any{"event": "skipped", "status": "max_inflight", "source": sourceLetta})
	s.publishContinuationEvent(token, skipped)
	s.emitContinuationSteering(request, token, sourceLetta, skipped)
	deferred := continuationEventWithRequest(request, map[string]any{"event": "deferred", "status": "durable_queued", "source": sourceLetta})
	s.publishContinuationEvent(token, deferred)
	s.emitContinuationSteering(request, token, sourceLetta, deferred)
	completed := continuationEventWithRequest(request, map[string]any{"event": "completed", "status": "ok", "source": sourceLetta})
	s.publishContinuationEvent(token, completed)
	s.emitContinuationSteering(request, token, sourceLetta, completed)
	s.emitContinuationSteering(request, token, sourceLetta, completed)

	_, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		t.Fatalf("session %s missing", sessionID)
	}
	progressCount := 0
	readyCount := 0
	degradedCount := 0
	for _, event := range events {
		switch anyToString(event["type"]) {
		case "retrieval.continuation.progress":
			progressCount++
		case "retrieval.continuation.ready":
			readyCount++
		case "retrieval.continuation.degraded":
			degradedCount++
		}
	}
	if progressCount != 1 || readyCount != 1 || degradedCount != 0 {
		t.Fatalf("unexpected steering sequence progress=%d ready=%d degraded=%d events=%#v", progressCount, readyCount, degradedCount, events)
	}
}

func TestDisabledRetrievalSourcesAreConfiguredButNotEffective(t *testing.T) {
	t.Setenv("ORCH_WEAVIATE_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("disabled source unexpectedly called backend path %s", r.URL.Path)
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	s.strictNoPythonRuntime = true
	response, status, err := s.executeRetrieval(context.Background(), http.Header{}, map[string]any{
		"query": "disabled source coverage", "project": "contextlattice", "sources": []any{sourceWeaviate},
	}, false)
	if err != nil || status != http.StatusOK {
		t.Fatalf("execute retrieval status=%d err=%v payload=%#v", status, err, response)
	}
	summary := anyMap(response["source_summary"])
	if len(anyToStringList(summary["configured_sources"], 8)) != 1 || len(anyToStringList(summary["effective_sources"], 8)) != 0 || len(anyToStringList(summary["disabled_sources"], 8)) != 1 {
		t.Fatalf("configured/effective/disabled source truth is incoherent: %#v", summary)
	}
	coverage := contextPackSourceCoverage(response)
	if !anyToBool(coverage["complete"]) || len(anyToStringList(coverage["disabled"], 8)) != 1 || len(anyToStringList(coverage["queried"], 8)) != 0 {
		t.Fatalf("disabled source poisoned effective coverage: %#v", coverage)
	}
}
