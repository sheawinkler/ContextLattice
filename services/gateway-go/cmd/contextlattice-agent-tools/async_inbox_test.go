package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestAsyncInboxDrainDeliversTerminalItemsAndAcks(t *testing.T) {
	ackPath := filepath.Join(t.TempDir(), "async-seen.json")
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/sessions/sess-async/rollup" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"rollup": map[string]any{
				"agent_inbox": map[string]any{
					"items": []map[string]any{
						{
							"event_id":         "evt-ready",
							"type":             "retrieval.continuation.ready",
							"status":           "completed",
							"result_state":     "ready",
							"message":          "Async retrieval is ready; slow-source evidence has finished warming for this request.",
							"suggested_action": "Rerun context packaging before finalizing.",
							"token":            "cont-ready",
							"progress_pct":     100,
						},
						{
							"event_id":         "evt-progress",
							"type":             "retrieval.continuation.progress",
							"status":           "running",
							"result_state":     "pending",
							"message":          "Async retrieval is still warming.",
							"suggested_action": "Keep working.",
							"token":            "cont-progress",
							"progress_pct":     25,
						},
					},
				},
			},
		})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_async_inbox_drain", "--session-id", "sess-async", "--ack-path", ackPath}); err != nil {
		t.Fatalf("run async inbox drain: %v", err)
	}
	rendered := stdout.String()
	if !strings.Contains(rendered, "ContextLattice async retrieval ready") || !strings.Contains(rendered, "cont-ready") {
		t.Fatalf("expected ready async notice, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "still warming") || strings.Contains(rendered, "cont-progress") {
		t.Fatalf("progress item should not be delivered by default:\n%s", rendered)
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice_async_inbox_drain", "--session-id", "sess-async", "--ack-path", ackPath}); err != nil {
		t.Fatalf("run second async inbox drain: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != "" {
		t.Fatalf("expected acked item to stay quiet, got:\n%s", stdout.String())
	}

	stdout.Reset()
	if err := c.run([]string{"contextlattice_async_inbox_drain", "--session-id", "sess-async", "--ack-path", ackPath, "--include-progress", "--peek"}); err != nil {
		t.Fatalf("run async inbox drain with progress: %v", err)
	}
	rendered = stdout.String()
	if !strings.Contains(rendered, "ContextLattice async retrieval warming") || !strings.Contains(rendered, "cont-progress") {
		t.Fatalf("expected progress item to render as warming, got:\n%s", rendered)
	}
	if strings.Contains(rendered, "ContextLattice async retrieval ready") {
		t.Fatalf("progress item must not render as ready, got:\n%s", rendered)
	}
}

func TestPackAutoDrainWritesNoticeToStderrWithoutCorruptingStdout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_ASYNC_INBOX_ACK_PATH", filepath.Join(t.TempDir(), "seen.json"))
	var packPayload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/sessions/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "session": map[string]any{"id": "sess-pack-drain"}})
		case "/memory/context-pack":
			if err := json.NewDecoder(r.Body).Decode(&packPayload); err != nil {
				t.Fatalf("decode pack request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "context_pack": map[string]any{"facts": []any{}}})
		case "/v1/agents/sessions/sess-pack-drain/rollup":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"rollup": map[string]any{
					"agent_inbox": map[string]any{
						"items": []map[string]any{{
							"event_id":         "evt-pack-ready",
							"type":             "retrieval.continuation.ready",
							"status":           "completed",
							"result_state":     "ready",
							"message":          "Async retrieval is ready.",
							"suggested_action": "Rerun context packaging.",
							"token":            "cont-pack",
							"progress_pct":     100,
						}},
					},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer gateway.Close()

	var stdout, stderr bytes.Buffer
	c := newCLI(&stdout, &stderr)
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_pack", "native pack", "--project", "alpha", "--mode", "fast", "--raw"}); err != nil {
		t.Fatalf("run pack: %v", err)
	}
	if packPayload["session_id"] != "sess-pack-drain" {
		t.Fatalf("expected session id from auto session: %#v", packPayload)
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("stdout should remain valid JSON, got %q: %v", stdout.String(), err)
	}
	if !strings.Contains(stderr.String(), "ContextLattice async retrieval ready") || !strings.Contains(stderr.String(), "cont-pack") {
		t.Fatalf("expected async notice on stderr, got:\n%s", stderr.String())
	}
}
