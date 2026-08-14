package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacyTaskRoutesAreFailClosedAndStateless(t *testing.T) {
	handler := newServer().handler()
	for _, path := range []string{
		"/v1/tasks/submit",
		"/v1/tasks/claim",
		"/v1/tasks/status",
		"/v1/tasks/retry",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"task_id":"foreign","title":"must-not-persist"}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusGone {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["error"] != "legacy_orchestrator_task_authority_disabled" || payload["authoritative_backend"] != "gateway-go-sqlite-wal" || payload["authoritative_route"] != "/agents/tasks" {
				t.Fatalf("unexpected migration response: %#v", payload)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/tasks/metrics", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d body=%s", response.Code, response.Body.String())
	}
	var metrics map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics["authoritative"] != false || metrics["mutations_enabled"] != false || metrics["authoritative_backend"] != "gateway-go-sqlite-wal" {
		t.Fatalf("unexpected authority metrics: %#v", metrics)
	}
	totals, ok := metrics["totals"].(map[string]any)
	if !ok || totals["tasks"] != float64(0) {
		t.Fatalf("legacy process retained task state: %#v", metrics)
	}
}
