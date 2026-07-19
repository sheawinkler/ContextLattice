package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func frontierT6RouteTestServer(t testing.TB) (*server, *frontierT6AgentFitStore) {
	t.Helper()
	root := t.TempDir()
	store, err := newFrontierT6AgentFitStore(root+"/agent-fit.json", frontierT6StoreLimits{})
	if err != nil {
		t.Fatalf("create T6 route store: %v", err)
	}
	t.Cleanup(store.close)
	return &server{
		orchestratorAPIKey: "test-owner-key",
		frontierT6:         store,
		contextPassports:   newTestPassportStore(t, root+"/identity"),
	}, store
}

func frontierT6RouteRequest(t testing.TB, handler http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &body)
	request.Header.Set("X-Api-Key", "test-owner-key")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func frontierT6RoutePublishPayload(now time.Time) map[string]any {
	return map[string]any{
		"operation": "publish",
		"scope":     map[string]any{"project": "contextlattice", "session_id": "session-t6", "agent_id": "agent-t6"},
		"publish": map[string]any{
			"kind": "context_ready", "message": "Reviewed context is ready.",
			"suggested_action":   "Rebuild the bounded pack before the next model call.",
			"injection_boundary": "after_tool", "dedupe_key": "context-ready-one", "ttl_seconds": 300,
			"provenance": frontierT6TestProvenance(now, "source-generation-one", frontierT6TestDigest("route-auth-placeholder"), time.Hour),
		},
	}
}

func TestFrontierT6RoutesDeliverResumeAndAcknowledgeSSE(t *testing.T) {
	now := time.Now().UTC()
	s, _ := frontierT6RouteTestServer(t)
	mux := buildNativeMux(s)
	published := frontierT6RouteRequest(t, mux, http.MethodPost, frontierT6SteeringPath, frontierT6RoutePublishPayload(now))
	if published.Code != http.StatusOK {
		t.Fatalf("publish status=%d body=%s", published.Code, published.Body.String())
	}

	streamPath := frontierT6SteeringEventsPath + "?project=contextlattice&session_id=session-t6&agent_id=agent-t6&subscriber_id=subscriber-t6&once=true&limit=2"
	stream := frontierT6RouteRequest(t, mux, http.MethodGet, streamPath, nil)
	if stream.Code != http.StatusOK || !strings.Contains(strings.ToLower(stream.Header().Get("Content-Type")), "text/event-stream") {
		t.Fatalf("stream status=%d content-type=%q body=%s", stream.Code, stream.Header().Get("Content-Type"), stream.Body.String())
	}
	body := stream.Body.String()
	if !strings.Contains(body, "id: ft6c_1") || !strings.Contains(body, "event: steering") || !strings.Contains(body, "requires_explicit_agent_use") {
		t.Fatalf("SSE response omitted resumable explicit-use contract: %s", body)
	}
	var eventPayload map[string]any
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "delivery_id") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &eventPayload); err != nil {
				t.Fatalf("decode SSE event: %v", err)
			}
			break
		}
	}
	if len(eventPayload) == 0 {
		t.Fatalf("delivery event missing from SSE body: %s", body)
	}
	event := anyMap(eventPayload["event"])
	ack := frontierT6RouteRequest(t, mux, http.MethodPost, frontierT6SteeringPath, map[string]any{
		"operation":     "acknowledge",
		"scope":         map[string]any{"project": "contextlattice", "session_id": "session-t6", "agent_id": "agent-t6"},
		"subscriber_id": "subscriber-t6", "delivery_id": eventPayload["delivery_id"], "event_id": event["event_id"],
	})
	if ack.Code != http.StatusOK || !strings.Contains(ack.Body.String(), "ft6c_1") {
		t.Fatalf("ack status=%d body=%s", ack.Code, ack.Body.String())
	}

	resumeRequest := httptest.NewRequest(http.MethodGet, streamPath, nil)
	resumeRequest.Header.Set("X-Api-Key", "test-owner-key")
	resumeRequest.Header.Set("Last-Event-ID", "ft6c_1")
	resumed := httptest.NewRecorder()
	mux.ServeHTTP(resumed, resumeRequest)
	if resumed.Code != http.StatusOK || strings.Contains(resumed.Body.String(), "event: steering") {
		t.Fatalf("resume replayed acknowledged event: status=%d body=%s", resumed.Code, resumed.Body.String())
	}
}

func TestFrontierT6RoutesBindOwnerWorkspaceAndRejectBadAuth(t *testing.T) {
	s, _ := frontierT6RouteTestServer(t)
	mux := buildNativeMux(s)
	payload := frontierT6RoutePublishPayload(time.Now().UTC())
	anyMap(payload["scope"])["workspace_id"] = "foreign-workspace"
	foreign := frontierT6RouteRequest(t, mux, http.MethodPost, frontierT6SteeringPath, payload)
	if foreign.Code != http.StatusForbidden || !strings.Contains(foreign.Body.String(), "workspace_override_forbidden") {
		t.Fatalf("foreign workspace status=%d body=%s", foreign.Code, foreign.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, frontierT6SteeringPath, strings.NewReader("{}"))
	request.Header.Set("X-Api-Key", "wrong-key")
	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, request)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("bad API key status=%d body=%s", denied.Code, denied.Body.String())
	}
}

func TestFrontierT6RoutesKeepSelectionAdvisoryAndTelemetryPathFree(t *testing.T) {
	now := time.Now().UTC()
	s, _ := frontierT6RouteTestServer(t)
	mux := buildNativeMux(s)
	selection := frontierT6RouteRequest(t, mux, http.MethodPost, frontierT6SelectionPath, frontierT6SelectionHTTPRequest{
		Kind: "runner",
		Request: frontierT6SelectionRequest{
			TaskClass: "go_implementation", Now: now,
			Constraints: frontierT6SelectionConstraints{MinimumSamples: 5, RequiredCapabilities: []string{"go"}},
			Candidates:  []frontierT6SelectionCandidate{frontierT6TestSelectionCandidate(now, "runner-a", 10, 9, 1, 90)},
		},
	})
	if selection.Code != http.StatusOK {
		t.Fatalf("selection status=%d body=%s", selection.Code, selection.Body.String())
	}
	receipt := map[string]any{}
	if err := json.Unmarshal(selection.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if !anyToBool(receipt["advisory_only"]) || anyToBool(receipt["activation_allowed"]) || anyToBool(receipt["execution_performed"]) {
		t.Fatalf("selection crossed advisory boundary: %#v", receipt)
	}

	telemetry := frontierT6RouteRequest(t, mux, http.MethodGet, frontierT6TelemetryPath, nil)
	if telemetry.Code != http.StatusOK || strings.Contains(strings.ToLower(telemetry.Body.String()), "agent-fit.json") || strings.Contains(strings.ToLower(telemetry.Body.String()), "local-root") {
		t.Fatalf("telemetry leaked path or failed: status=%d body=%s", telemetry.Code, telemetry.Body.String())
	}
	if !strings.Contains(telemetry.Body.String(), `"gateway_runner_execution":false`) || !strings.Contains(telemetry.Body.String(), `"network_calls":0`) {
		t.Fatalf("telemetry omitted execution boundary: %s", telemetry.Body.String())
	}
}
