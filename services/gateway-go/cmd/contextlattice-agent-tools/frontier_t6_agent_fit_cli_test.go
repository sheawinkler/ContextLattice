package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type frontierT6EmissionWriter struct {
	bytes.Buffer
	emitted atomic.Bool
}

func (w *frontierT6EmissionWriter) Write(raw []byte) (int, error) {
	w.emitted.Store(true)
	return w.Buffer.Write(raw)
}

func frontierT6CLIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONTEXTLATTICE_CONFIG_HOME", t.TempDir())
	t.Setenv(frontierT6ContextPrepClaimTokenEnv, "")
}

func TestFrontierT6AgentFitSteeringWatchResumesEmitsThenAcknowledges(t *testing.T) {
	frontierT6CLIEnv(t)
	var acknowledgements atomic.Int32
	writer := &frontierT6EmissionWriter{}
	var acknowledgement map[string]any

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == frontierT6SteeringEventsPath:
			if got := r.Header.Get("Last-Event-ID"); got != "event_previous" {
				t.Errorf("Last-Event-ID=%q", got)
			}
			if r.URL.Query().Get("cursor") != "cursor_previous" || r.URL.Query().Get("project") != "project_t6" || r.URL.Query().Get("session_id") != "session_t6" || r.URL.Query().Get("agent_id") != "agent_t6" || r.URL.Query().Get("subscriber_id") != "subscriber_t6" {
				t.Errorf("resume query=%s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = w.Write([]byte("id: event_current\nevent: steering\ndata: {\"delivery_id\":\"delivery_current\",\"claim_token\":\"never-print-this\",\"event\":{\"event_id\":\"event_current\",\"cursor\":\"cursor_current\",\"message\":\"review the new evidence\"}}\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case r.Method == http.MethodPost && r.URL.Path == frontierT6SteeringPath:
			if !writer.emitted.Load() {
				t.Error("acknowledgement arrived before successful event emission")
			}
			if err := json.NewDecoder(r.Body).Decode(&acknowledgement); err != nil {
				t.Errorf("decode acknowledgement: %v", err)
			}
			acknowledgements.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"acknowledged": true}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()

	c := newCLI(writer, ioDiscard{})
	c.baseURL = gateway.URL
	c.apiKey = ""
	err := c.run([]string{
		"contextlattice_agent_fit", "steering-watch",
		"--project", "project_t6", "--session-id", "session_t6", "--agent-id", "agent_t6",
		"--subscriber-id", "subscriber_t6", "--cursor", "cursor_previous", "--event-id", "event_previous",
		"--limit", "1", "--once", "--max-seconds", "2", "--raw",
	})
	if err != nil {
		t.Fatalf("watch steering: %v", err)
	}
	if acknowledgements.Load() != 1 {
		t.Fatalf("acknowledgements=%d", acknowledgements.Load())
	}
	if acknowledgement["operation"] != "ack" || acknowledgement["delivery_id"] != "delivery_current" || acknowledgement["event_id"] != "event_current" || acknowledgement["subscriber_id"] != "subscriber_t6" {
		t.Fatalf("acknowledgement=%#v", acknowledgement)
	}
	output := writer.String()
	if !strings.Contains(output, `"transport":"sse"`) || !strings.Contains(output, `"event_id":"event_current"`) {
		t.Fatalf("watch output=%s", output)
	}
	if strings.Contains(output, "never-print-this") {
		t.Fatalf("claim token leaked into output: %s", output)
	}
}

func TestFrontierT6AgentFitSteeringWatchNeverAcknowledgesMalformedOrTruncatedEvents(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: "id: event_bad\ndata: {\n\n"},
		{name: "truncated frame", body: "id: event_truncated\ndata: {\"delivery_id\":\"delivery_truncated\",\"event\":{\"event_id\":\"event_truncated\"}}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frontierT6CLIEnv(t)
			var acknowledgements atomic.Int32
			var replays atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == frontierT6SteeringEventsPath {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(test.body))
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == frontierT6SteeringPath {
					payload := map[string]any{}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					if payload["operation"] == "ack" {
						acknowledgements.Add(1)
					} else if payload["operation"] == "replay" {
						replays.Add(1)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"events": []any{}}})
					return
				}
				http.NotFound(w, r)
			}))
			defer gateway.Close()

			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			c.apiKey = ""
			if err := c.run([]string{"contextlattice", "agent-fit", "steering-watch", "--project", "project_t6", "--once", "--max-seconds", "2", "--raw"}); err != nil {
				t.Fatalf("watch malformed stream: %v", err)
			}
			if acknowledgements.Load() != 0 {
				t.Fatalf("malformed stream acknowledgements=%d", acknowledgements.Load())
			}
			if replays.Load() != 1 {
				t.Fatalf("bounded replay count=%d", replays.Load())
			}
			if output := stdout.String(); !strings.Contains(output, "bounded_pull_fallback") || strings.Contains(strings.ToLower(output), "degraded") {
				t.Fatalf("fallback output=%s", output)
			}
		})
	}
}

func TestFrontierT6AgentFitSteeringWatchUsesClearlyLabeledPullFallback(t *testing.T) {
	frontierT6CLIEnv(t)
	var replay map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == frontierT6SteeringEventsPath {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == frontierT6SteeringPath {
			if err := json.NewDecoder(r.Body).Decode(&replay); err != nil {
				t.Errorf("decode replay: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"delivery_mode": "bounded_pull_replay", "events": []any{}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	c.apiKey = ""
	if err := c.run([]string{"contextlattice_agent_fit", "steering-watch", "--project", "project_t6", "--cursor", "cursor_9", "--limit", "4", "--max-seconds", "2", "--raw"}); err != nil {
		t.Fatalf("watch fallback: %v", err)
	}
	if replay["operation"] != "replay" || replay["cursor"] != "cursor_9" || int(replay["limit"].(float64)) != 4 {
		t.Fatalf("replay payload=%#v", replay)
	}
	output := stdout.String()
	if !strings.Contains(output, `"transport":"bounded_pull_fallback"`) || !strings.Contains(output, `"fallback_reason":"sse_negotiation_rejected"`) || strings.Contains(strings.ToLower(output), "degraded") {
		t.Fatalf("fallback output=%s", output)
	}
}

func TestFrontierT6AgentFitGenericOperationRouting(t *testing.T) {
	frontierT6CLIEnv(t)
	payloadPath := filepath.Join(t.TempDir(), "agent-fit.json")
	if err := os.WriteFile(payloadPath, []byte(`{"request":{"task_class":"go_implementation"},"publish":{"message":"bounded"},"schedule":{"task_id":"task_1"},"fields":{"target_tokens":1200}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	type capturedRequest struct {
		path    string
		payload map[string]any
	}
	captured := []capturedRequest{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode %s: %v", r.URL.Path, err)
		}
		captured = append(captured, capturedRequest{path: r.URL.Path, payload: payload})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": r.URL.Path})
	}))
	defer gateway.Close()

	cases := []struct {
		operation string
		path      string
		field     string
		value     string
		extra     []string
	}{
		{operation: "steering-publish", path: frontierT6SteeringPath, field: "operation", value: "publish"},
		{operation: "steering-replay", path: frontierT6SteeringPath, field: "operation", value: "replay", extra: []string{"--cursor", "cursor_1", "--limit", "3"}},
		{operation: "steering-ack", path: frontierT6SteeringPath, field: "operation", value: "ack", extra: []string{"--delivery-id", "delivery_1", "--event-id", "event_1"}},
		{operation: "runner-select", path: frontierT6SelectionPath, field: "kind", value: "runner"},
		{operation: "model-select", path: frontierT6SelectionPath, field: "kind", value: "model"},
		{operation: "profile-resolve", path: frontierT6ProfilePath, field: "operation", value: "resolve"},
		{operation: "profile-configure", path: frontierT6ProfilePath, field: "operation", value: "configure"},
		{operation: "context-prep-schedule", path: frontierT6ContextPrepPath, field: "operation", value: "schedule"},
		{operation: "context-prep-use", path: frontierT6ContextPrepPath, field: "operation", value: "use"},
	}
	for index, test := range cases {
		var stdout bytes.Buffer
		c := newCLI(&stdout, ioDiscard{})
		c.baseURL = gateway.URL
		c.apiKey = ""
		argv := []string{"contextlattice", "agent-fit", test.operation, "--payload-file", payloadPath, "--project", "project_cli", "--session-id", "session_cli", "--agent-id", "agent_cli", "--raw"}
		argv = append(argv, test.extra...)
		if err := c.run(argv); err != nil {
			t.Fatalf("%s: %v", test.operation, err)
		}
		request := captured[index]
		if request.path != test.path || request.payload[test.field] != test.value {
			t.Fatalf("%s request=%#v", test.operation, request)
		}
		if test.path != frontierT6SelectionPath {
			scope := asMap(request.payload["scope"])
			if scope["project"] != "project_cli" || scope["session_id"] != "session_cli" || scope["agent_id"] != "agent_cli" {
				t.Fatalf("%s scope=%#v", test.operation, scope)
			}
		}
	}
}

func TestFrontierT6AgentFitContextPrepClaimLifecycleUsesProtectedHeaderAndFile(t *testing.T) {
	frontierT6CLIEnv(t)
	claimToken := "ft6prepclaim_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tokenPath := filepath.Join(t.TempDir(), "claims", "prep.claim")
	type capturedRequest struct {
		operation string
		header    string
		payload   map[string]any
	}
	captured := []capturedRequest{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode context prep lifecycle request: %v", err)
			return
		}
		operation := firstString(payload["operation"])
		captured = append(captured, capturedRequest{operation: operation, header: r.Header.Get(frontierT6ContextPrepClaimHeader), payload: payload})
		if operation == "claim" {
			w.Header().Set(frontierT6ContextPrepClaimHeader, claimToken)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"claimed": operation == "claim"}})
	}))
	defer gateway.Close()

	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	c.apiKey = ""
	common := []string{"--project", "project_cli", "--session-id", "session_cli", "--agent-id", "agent_cli", "--prep-id", "ft6prep_example", "--claim-token-file", tokenPath, "--raw"}
	claimArgs := append([]string{"contextlattice", "agent-fit", "context-prep-claim", "--worker-id", "worker_cli"}, common...)
	if err := c.run(claimArgs); err != nil {
		t.Fatalf("claim context preparation: %v", err)
	}
	if strings.Contains(stdout.String(), claimToken) || strings.Contains(stdout.String(), "claim_token") {
		t.Fatalf("protected claim leaked to stdout: %s", stdout.String())
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("inspect protected claim file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("claim file was not owner-only: mode=%v", info.Mode().Perm())
	}
	rawToken, err := os.ReadFile(tokenPath)
	if err != nil || strings.TrimSpace(string(rawToken)) != claimToken {
		t.Fatalf("claim file did not contain the protected claim: err=%v", err)
	}

	stdout.Reset()
	completeArgs := append([]string{"contextlattice", "agent-fit", "context-prep-complete"}, common...)
	if err := c.run(completeArgs); err != nil {
		t.Fatalf("complete context preparation: %v", err)
	}
	stdout.Reset()
	failArgs := append([]string{"contextlattice", "agent-fit", "context-prep-fail", "--reason-code", "worker_failed"}, common...)
	if err := c.run(failArgs); err != nil {
		t.Fatalf("fail context preparation: %v", err)
	}
	if len(captured) != 3 {
		t.Fatalf("captured lifecycle requests=%d", len(captured))
	}
	if captured[0].operation != "claim" || captured[0].header != "" {
		t.Fatalf("claim request crossed protected header boundary: %#v", captured[0])
	}
	for _, request := range captured[1:] {
		if request.header != claimToken {
			t.Fatalf("%s omitted protected claim header", request.operation)
		}
		if _, leaked := request.payload["claim_token"]; leaked || strings.Contains(compactJSON(request.payload), claimToken) {
			t.Fatalf("%s leaked claim into JSON: %#v", request.operation, request.payload)
		}
	}

	credentialPayload := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(credentialPayload, []byte(`{"claim_token":"must_not_leave_process"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.run([]string{"contextlattice", "agent-fit", "context-prep-use", "--payload-file", credentialPayload, "--project", "project_cli", "--session-id", "session_cli", "--agent-id", "agent_cli", "--raw"}); err == nil || !strings.Contains(err.Error(), "forbidden credential") {
		t.Fatalf("JSON claim credential was not rejected: %v", err)
	}
	if len(captured) != 3 {
		t.Fatal("forbidden credential payload reached the gateway")
	}

	if runtime.GOOS != "windows" {
		unsafeTokenPath := filepath.Join(t.TempDir(), "unsafe.claim")
		if err := os.WriteFile(unsafeTokenPath, []byte(claimToken+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafeTokenPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.run([]string{"contextlattice", "agent-fit", "context-prep-complete", "--project", "project_cli", "--session-id", "session_cli", "--agent-id", "agent_cli", "--prep-id", "ft6prep_example", "--claim-token-file", unsafeTokenPath, "--raw"}); err == nil || !strings.Contains(err.Error(), "owner-only") {
			t.Fatalf("unsafe claim file was not rejected: %v", err)
		}
		if len(captured) != 3 {
			t.Fatal("unsafe claim file reached the gateway")
		}
	}
}
