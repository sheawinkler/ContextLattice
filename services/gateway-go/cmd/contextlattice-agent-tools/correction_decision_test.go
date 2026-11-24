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

func TestManualDecisionCLIFences(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_NATIVE_SESSION_ID", "")
	var reads, queries atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "chosen", "agent_id": "writer", "project": "example", "status": "active"}})
			return
		}
		queries.Add(1)
		if r.URL.Path != "/memory/claims/query" {
			t.Error("unexpected correction write")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "claims": []string{"unrelated original"}})
	}))
	defer api.Close()
	var output bytes.Buffer
	c := newCLI(&output, io.Discard)
	c.baseURL = api.URL
	common := []string{"contextlattice", "claim-query", "--decision-id", "mode", "--project", "example", "--topic-path", "work"}
	run := func(args ...string) error { return c.run(append(append([]string{}, common...), args...)) }
	if run("--session-id", "chosen") == nil || run("--agent-id", "writer") == nil ||
		run("--agent-id", "writer", "--session-id", "chosen", "--base-url", "https://example.invalid") == nil || reads.Load() != 0 {
		t.Fatal("implicit identity or nonlocal target reached the gateway")
	}
	if run("--agent-id", "other", "--session-id", "chosen") == nil || reads.Load() != 1 || queries.Load() != 0 {
		t.Fatal("foreign actor reached the original")
	}
	if run("--agent-id", "writer", "--session-id", "chosen") == nil || queries.Load() != 1 || output.Len() != 0 {
		t.Fatal("old gateway result was disclosed")
	}
}
