package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T, backendURL string) *server {
	t.Helper()
	t.Setenv("BACKEND_URL", backendURL)
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
		t.Setenv("MEMMCP_ORCHESTRATOR_API_KEY", "")
	}
	return newServer()
}

func TestProxyForwardsRetrievalRequest(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	var capturedBody string
	var capturedPath string
	var capturedHeader string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capturedBody = string(raw)
		capturedPath = r.URL.Path
		capturedHeader = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"source":"qdrant"}]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/retrieval/query-with-grounding", strings.NewReader(`{"request":{"query":"alpha","limit":5}}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"source":"qdrant"`) {
		t.Fatalf("unexpected response body: %s", string(body))
	}
	if capturedPath != "/v1/retrieval/query-with-grounding" {
		t.Fatalf("expected proxied path, got %s", capturedPath)
	}
	if !strings.Contains(capturedBody, `"query":"alpha"`) {
		t.Fatalf("expected body to be proxied, got %s", capturedBody)
	}
	if capturedHeader != "secret" {
		t.Fatalf("expected x-api-key header forwarded")
	}
}

func TestProxyMapsBearerAuthorizationToAPIKey(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	var capturedHeader string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodGet, gateway.URL+"/status", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer abc123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if capturedHeader != "abc123" {
		t.Fatalf("expected bearer token mirrored to x-api-key, got %q", capturedHeader)
	}
}

func TestProxyForwardsQueryParams(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	var capturedRawQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"memory":{"id":"alpha::notes/a.md"}}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/v1/memory/get?memory_id=alpha%3A%3Anotes%2Fa.md")
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(capturedRawQuery, "memory_id=") {
		t.Fatalf("expected memory_id query to be forwarded, got %q", capturedRawQuery)
	}
}

func TestProxyForwardsMemorySearchRequest(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	var capturedBody string
	var capturedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capturedBody = string(raw)
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"source":"topic_rollups"}],"retrieval_intent":"decision"}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/memory/search",
		strings.NewReader(`{"query":"profitability tuning","project":"algotraderv2_rust","retrieval_intent":"decision"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if capturedPath != "/memory/search" {
		t.Fatalf("expected /memory/search to be proxied, got %s", capturedPath)
	}
	if !strings.Contains(capturedBody, `"retrieval_intent":"decision"`) {
		t.Fatalf("expected retrieval_intent payload to be proxied, got %s", capturedBody)
	}
}

func TestMemorySearchUsesGoStagedRetrieval(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")

	var calledPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPath = r.URL.Path
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"alpha summary","score":0.93}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"query":"alpha","limit":5,"include_grounding":true,"agent_id":"codex_gpt5"}`
	resp, err := http.Post(gateway.URL+"/memory/search", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	rows, ok := payload["results"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("expected staged retrieval rows, got %#v", payload["results"])
	}
	if strings.TrimSpace(anyToString(payload["retrieval_mode"])) != "balanced" {
		t.Fatalf("expected retrieval_mode=balanced default, got %#v", payload["retrieval_mode"])
	}
	if !anyToBool(payload["learning_enabled"]) {
		t.Fatalf("expected learning_enabled=true")
	}
	grounding, ok := payload["grounding"].(map[string]any)
	if !ok || !anyToBool(grounding["strict_numeric_copy"]) {
		t.Fatalf("expected grounding.strict_numeric_copy=true, got %#v", payload["grounding"])
	}
	if calledPath != "/v1/retrieval/query" {
		t.Fatalf("expected go staged path via /v1/retrieval/query, got %s", calledPath)
	}
}

func TestMemorySearchRejectsExplicitInvalidAPIKey(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"ok","score":0.9}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/memory/search", strings.NewReader(`{"query":"alpha","project":"alpha"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "bad-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 401, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 0 {
		t.Fatalf("expected no backend calls on invalid key, got %d", backendCalls)
	}
}

func TestMemorySearchInjectsConfiguredAPIKeyWhenMissing(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	var capturedAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/retrieval/query" {
			capturedAPIKey = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"ok","score":0.9}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/memory/search", "application/json", strings.NewReader(`{"query":"alpha","project":"alpha"}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if capturedAPIKey != "good-key" {
		t.Fatalf("expected gateway to inject configured key, got %q", capturedAPIKey)
	}
}

func TestMemorySearchAcceptsQueryParamAPIKey(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	var capturedAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/retrieval/query" {
			capturedAPIKey = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"ok","score":0.9}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/memory/search?api_key=good-key",
		strings.NewReader(`{"query":"alpha","project":"alpha"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if capturedAPIKey != "good-key" {
		t.Fatalf("expected query param key to authorize staged retrieval, got %q", capturedAPIKey)
	}
}

func TestProxyForwardsAsyncMemorySearchPath(t *testing.T) {
	var capturedPath string
	var capturedQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"status":"running"}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/memory/search/async/token-123?include_result=false")
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if capturedPath != "/memory/search/async/token-123" {
		t.Fatalf("expected async path proxied, got %s", capturedPath)
	}
	if capturedQuery != "include_result=false" {
		t.Fatalf("expected query params forwarded, got %s", capturedQuery)
	}
}

func TestProxyForwardsAsyncMemorySearchEventsPath(t *testing.T) {
	var capturedPath string
	var capturedQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: snapshot\ndata: {\"status\":\"running\"}\n\n"))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/memory/search/jobs/token-123/events?include_result=false")
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: snapshot") {
		t.Fatalf("expected stream payload, got %s", string(body))
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		t.Fatalf("expected text/event-stream content type, got %s", resp.Header.Get("Content-Type"))
	}
	if capturedPath != "/memory/search/jobs/token-123/events" {
		t.Fatalf("expected events path proxied, got %s", capturedPath)
	}
	if capturedQuery != "include_result=false" {
		t.Fatalf("expected query params forwarded, got %s", capturedQuery)
	}
}

func TestProxyForwardsBatchAndOpsQueuePaths(t *testing.T) {
	var capturedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/tools/memory_write_batch",
		strings.NewReader(`{"items":[{"projectName":"alpha","fileName":"notes/a.md","content":"x"}]}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for tools/memory_write_batch, got %d", resp.StatusCode)
	}
	if capturedPath != "/tools/memory_write_batch" {
		t.Fatalf("expected /tools/memory_write_batch to be proxied, got %s", capturedPath)
	}

	reqFeedback, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/tools/feedback_submit",
		strings.NewReader(`{"project":"alpha","content":"good result","tags":["quality"]}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	reqFeedback.Header.Set("Content-Type", "application/json")
	respFeedback, err := http.DefaultClient.Do(reqFeedback)
	if err != nil {
		t.Fatalf("feedback submit request failed: %v", err)
	}
	defer respFeedback.Body.Close()
	if respFeedback.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /tools/feedback_submit, got %d", respFeedback.StatusCode)
	}
	if capturedPath != "/tools/feedback_submit" {
		t.Fatalf("expected /tools/feedback_submit to be proxied, got %s", capturedPath)
	}

	resp2, err := http.Get(gateway.URL + "/ops/queue/status?include_deadletters=false")
	if err != nil {
		t.Fatalf("ops queue request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /ops/queue/status, got %d", resp2.StatusCode)
	}
	if capturedPath != "/ops/queue/status" {
		t.Fatalf("expected /ops/queue/status to be proxied, got %s", capturedPath)
	}

	req3, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/memory/browser-context",
		strings.NewReader(`{"projectName":"alpha","pageUrl":"https://example.com","textSnapshot":"hello world"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("browser context request failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /memory/browser-context, got %d", resp3.StatusCode)
	}
	if capturedPath != "/memory/browser-context" {
		t.Fatalf("expected /memory/browser-context to be proxied, got %s", capturedPath)
	}

	resp4, err := http.Get(gateway.URL + "/ops/capabilities")
	if err != nil {
		t.Fatalf("capabilities request failed: %v", err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /ops/capabilities, got %d", resp4.StatusCode)
	}
	if capturedPath != "/ops/capabilities" {
		t.Fatalf("expected /ops/capabilities to be proxied, got %s", capturedPath)
	}

	req5, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/memory/recall/eval-cases/refresh",
		strings.NewReader(`{"max_cases":3,"min_hits":1}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req5.Header.Set("Content-Type", "application/json")
	resp5, err := http.DefaultClient.Do(req5)
	if err != nil {
		t.Fatalf("recall refresh request failed: %v", err)
	}
	defer resp5.Body.Close()
	if resp5.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /memory/recall/eval-cases/refresh, got %d", resp5.StatusCode)
	}
	if capturedPath != "/memory/recall/eval-cases/refresh" {
		t.Fatalf("expected /memory/recall/eval-cases/refresh to be proxied, got %s", capturedPath)
	}
}

func TestToolsCapabilityMapGETIsServedViaPOSTBackend(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	var capturedPath string
	var capturedMethod string
	var capturedAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		capturedAPIKey = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/tools/capability_map")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if capturedPath != "/tools/capability_map" {
		t.Fatalf("expected /tools/capability_map backend path, got %s", capturedPath)
	}
	if capturedMethod != http.MethodPost {
		t.Fatalf("expected backend POST, got %s", capturedMethod)
	}
	if capturedAPIKey != "good-key" {
		t.Fatalf("expected configured key injected, got %q", capturedAPIKey)
	}
}

func TestToolsOpsQueueStatusDefaultsToExcludeDeadletters(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	var capturedPath string
	var capturedMethod string
	var capturedQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/tools/ops_queue_status", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if capturedPath != "/ops/queue/status" {
		t.Fatalf("expected /ops/queue/status backend path, got %s", capturedPath)
	}
	if capturedMethod != http.MethodGet {
		t.Fatalf("expected backend GET, got %s", capturedMethod)
	}
	if !strings.Contains(capturedQuery, "include_deadletters=false") {
		t.Fatalf("expected include_deadletters=false query, got %q", capturedQuery)
	}
}

func TestToolsDefaultOpenIgnoresExplicitInvalidKeyUnlessEnforced(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	backendCalls := 0
	var capturedAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		capturedAPIKey = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/tools/capability_map", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "stale-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 1 {
		t.Fatalf("expected backend call, got %d", backendCalls)
	}
	if capturedAPIKey != "good-key" {
		t.Fatalf("expected configured key to be used, got %q", capturedAPIKey)
	}

	t.Setenv("GO_TOOL_CALLS_ENFORCE_PROVIDED_KEY", "true")
	sStrict := newTestServer(t, backend.URL)
	gatewayStrict := httptest.NewServer(buildMux(sStrict))
	defer gatewayStrict.Close()

	reqStrict, err := http.NewRequest(http.MethodPost, gatewayStrict.URL+"/tools/capability_map", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request build failed: %v", err)
	}
	reqStrict.Header.Set("Content-Type", "application/json")
	reqStrict.Header.Set("X-Api-Key", "stale-key")
	respStrict, err := http.DefaultClient.Do(reqStrict)
	if err != nil {
		t.Fatalf("strict request failed: %v", err)
	}
	defer respStrict.Body.Close()
	if respStrict.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(respStrict.Body)
		t.Fatalf("expected 401 in strict mode, got %d body=%s", respStrict.StatusCode, string(body))
	}
}

func TestToolAllowlistCanLimitCalls(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TOOL_CALLS_ALLOW_ALL", "false")
	t.Setenv("GO_TOOL_CALLS_ALLOWLIST", "capability_map")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	respAllowed, err := http.Get(gateway.URL + "/tools/capability_map")
	if err != nil {
		t.Fatalf("allowed request failed: %v", err)
	}
	defer respAllowed.Body.Close()
	if respAllowed.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respAllowed.Body)
		t.Fatalf("expected 200 for allowlisted tool, got %d body=%s", respAllowed.StatusCode, string(body))
	}

	respBlocked, err := http.Post(gateway.URL+"/tools/feedback_submit", "application/json", strings.NewReader(`{"project":"x","content":"y"}`))
	if err != nil {
		t.Fatalf("blocked request failed: %v", err)
	}
	defer respBlocked.Body.Close()
	if respBlocked.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(respBlocked.Body)
		t.Fatalf("expected 403 for blocked tool, got %d body=%s", respBlocked.StatusCode, string(body))
	}
}

func TestToolRoleSplitWorkerLaneAllowsReadOnlyTools(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "orch-key")
	t.Setenv("CONTEXTLATTICE_WORKER_API_KEY", "worker-key")
	t.Setenv("GO_TOOL_CALLS_ROLE_SPLIT_ENABLED", "true")
	t.Setenv("GO_TOOL_CALLS_WORKER_ALLOW_ALL", "false")
	t.Setenv("GO_TOOL_CALLS_WORKER_ALLOWLIST", "capability_map,ops_queue_status")
	t.Setenv("GO_TOOL_CALLS_WORKER_DENYLIST", "feedback_submit,memory_write_batch")

	backendCalls := 0
	var capturedPath string
	var capturedRole string
	var capturedAPIKey string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		capturedPath = r.URL.Path
		capturedRole = strings.TrimSpace(r.Header.Get("X-ContextLattice-Caller-Role"))
		capturedAPIKey = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqAllowed, err := http.NewRequest(http.MethodPost, gateway.URL+"/tools/capability_map", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("allowed request build failed: %v", err)
	}
	reqAllowed.Header.Set("Content-Type", "application/json")
	reqAllowed.Header.Set("X-Api-Key", "worker-key")
	respAllowed, err := http.DefaultClient.Do(reqAllowed)
	if err != nil {
		t.Fatalf("allowed request failed: %v", err)
	}
	defer respAllowed.Body.Close()
	if respAllowed.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respAllowed.Body)
		t.Fatalf("expected 200 for worker capability_map, got %d body=%s", respAllowed.StatusCode, string(body))
	}
	if capturedPath != "/tools/capability_map" {
		t.Fatalf("expected /tools/capability_map backend path, got %s", capturedPath)
	}
	if capturedRole != "worker" {
		t.Fatalf("expected worker role header, got %q", capturedRole)
	}
	if capturedAPIKey != "orch-key" {
		t.Fatalf("expected orchestrator key injected upstream, got %q", capturedAPIKey)
	}

	reqBlocked, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/tools/feedback_submit",
		strings.NewReader(`{"project":"x","content":"y"}`),
	)
	if err != nil {
		t.Fatalf("blocked request build failed: %v", err)
	}
	reqBlocked.Header.Set("Content-Type", "application/json")
	reqBlocked.Header.Set("X-Api-Key", "worker-key")
	respBlocked, err := http.DefaultClient.Do(reqBlocked)
	if err != nil {
		t.Fatalf("blocked request failed: %v", err)
	}
	defer respBlocked.Body.Close()
	if respBlocked.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(respBlocked.Body)
		t.Fatalf("expected 403 for worker feedback_submit, got %d body=%s", respBlocked.StatusCode, string(body))
	}
	if backendCalls != 1 {
		t.Fatalf("expected only the allowlisted worker call to reach backend, got %d backend calls", backendCalls)
	}
}

func TestToolRoleSplitRejectsMissingAPIKey(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "orch-key")
	t.Setenv("CONTEXTLATTICE_WORKER_API_KEY", "worker-key")
	t.Setenv("GO_TOOL_CALLS_ROLE_SPLIT_ENABLED", "true")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/tools/capability_map")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 401 without key in role-split mode, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 0 {
		t.Fatalf("expected no backend calls on missing key, got %d", backendCalls)
	}
}

func TestToolRoleSplitAutoSkipsWhenWorkerKeyMatchesOrchestrator(t *testing.T) {
	t.Setenv("GO_TOOL_CALLS_ROLE_SPLIT_AUTO", "true")
	t.Setenv("GO_TOOL_CALLS_ROLE_SPLIT_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_WORKER_API_KEY", "same-key")
	policy := loadToolCallPolicy("same-key")
	if policy.roleSplitEnabled {
		t.Fatalf("expected role split disabled when worker key equals orchestrator key")
	}
}

func TestProxyForwardsMemoryWriteAndStatusPaths(t *testing.T) {
	var capturedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqWrite, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/memory/write",
		strings.NewReader(`{"projectName":"alpha","fileName":"notes/a.md","content":"hello"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	reqWrite.Header.Set("Content-Type", "application/json")
	respWrite, err := http.DefaultClient.Do(reqWrite)
	if err != nil {
		t.Fatalf("memory/write request failed: %v", err)
	}
	defer respWrite.Body.Close()
	if respWrite.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /memory/write, got %d", respWrite.StatusCode)
	}
	if capturedPath != "/memory/write" {
		t.Fatalf("expected /memory/write proxied, got %s", capturedPath)
	}

	respStatus, err := http.Get(gateway.URL + "/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer respStatus.Body.Close()
	if respStatus.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /status, got %d", respStatus.StatusCode)
	}
	if capturedPath != "/status" {
		t.Fatalf("expected /status proxied, got %s", capturedPath)
	}
}

func TestCodexPreflightBroadensScopeAndRequestsContextPack(t *testing.T) {
	searchCalls := 0
	contextPackCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		case "/status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"service":{"ok":true}}`))
			return
		case "/memory/search":
			searchCalls += 1
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(anyToString(payload["agent_id"])) != "codex_gpt5_test" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing agent_id"}`))
				return
			}
			topic := strings.TrimSpace(anyToString(payload["topic_path"]))
			if topic != "" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"degraded":true,"results":[]}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"degraded":false,"results":[{"project":"contextlattice","file":"notes/a.md"}]}`))
			return
		case "/memory/context-pack":
			contextPackCalls += 1
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(anyToString(payload["agent_id"])) != "codex_gpt5_test" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing agent_id"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"context_pack":{"facts":[{"text":"f1"}],"results":[{"file":"_rollups/topics/a.json"}]}}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","topic_path":"runbooks/codex-integration","query":"codex preflight","agent_id":"codex_gpt5_test"}`
	resp, err := http.Post(gateway.URL+"/v1/codex/preflight", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("preflight request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode preflight response: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true, got %#v", payload["ok"])
	}
	if strings.TrimSpace(anyToString(payload["agent_id"])) != "codex_gpt5_test" {
		t.Fatalf("unexpected agent_id in response: %#v", payload["agent_id"])
	}
	if searchCalls != 2 {
		t.Fatalf("expected two search calls (scoped+broad), got %d", searchCalls)
	}
	if contextPackCalls != 1 {
		t.Fatalf("expected one context-pack call, got %d", contextPackCalls)
	}
	if payload["broadened_search"] == nil {
		t.Fatalf("expected broadened_search payload, got nil")
	}
	if payload["context_pack"] == nil {
		t.Fatalf("expected context_pack payload, got nil")
	}
}

func TestAgentsPreflightUsesNamedProfileDefaults(t *testing.T) {
	searchCalls := 0
	contextPackCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		case "/status":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"service":{"ok":true}}`))
			return
		case "/memory/search":
			searchCalls += 1
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(anyToString(payload["agent_id"])) != "claude_code_agent" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing claude agent_id"}`))
				return
			}
			topic := strings.TrimSpace(anyToString(payload["topic_path"]))
			if topic != "" {
				if topic != "runbooks/claude-code-integration" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"unexpected topic path"}`))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"degraded":true,"results":[]}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"degraded":false,"results":[{"project":"contextlattice","file":"notes/a.md"}]}`))
			return
		case "/memory/context-pack":
			contextPackCalls += 1
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(anyToString(payload["agent_id"])) != "claude_code_agent" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing claude agent_id"}`))
				return
			}
			if strings.TrimSpace(anyToString(payload["topic_path"])) != "runbooks/claude-code-integration" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unexpected context-pack topic path"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"context_pack":{"facts":[{"text":"f1"}],"results":[{"file":"_rollups/topics/a.json"}]}}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"agent":"claude-code","project":"contextlattice"}`
	resp, err := http.Post(gateway.URL+"/v1/agents/preflight", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("preflight request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode preflight response: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true, got %#v", payload["ok"])
	}
	if strings.TrimSpace(anyToString(payload["agent"])) != "claude-code" {
		t.Fatalf("unexpected agent in response: %#v", payload["agent"])
	}
	if strings.TrimSpace(anyToString(payload["agent_id"])) != "claude_code_agent" {
		t.Fatalf("unexpected agent_id in response: %#v", payload["agent_id"])
	}
	if searchCalls != 2 {
		t.Fatalf("expected two search calls (scoped+broad), got %d", searchCalls)
	}
	if contextPackCalls != 1 {
		t.Fatalf("expected one context-pack call, got %d", contextPackCalls)
	}
	if payload["broadened_search"] == nil {
		t.Fatalf("expected broadened_search payload, got nil")
	}
	if payload["context_pack"] == nil {
		t.Fatalf("expected context_pack payload, got nil")
	}
}

func TestStagedRetrievalMergesSourcesAndGrounding(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups,qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups,qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "letta")

	var mu sync.Mutex
	calledSources := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, _ := payload["request"].(map[string]any)
		sources, _ := request["sources"].([]any)
		source := ""
		if len(sources) > 0 {
			source = strings.TrimSpace(strings.ToLower(anyToString(sources[0])))
		}
		mu.Lock()
		calledSources = append(calledSources, source)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch source {
		case "topic_rollups":
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","summary":"rollup a","score":0.91}],"warnings":[]}`))
		case "qdrant":
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","summary":"vector a","score":0.70},{"project":"alpha","file":"b.md","summary":"vector b","score":0.84}],"warnings":[]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
		}
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"alpha","limit":5,"retrieval_mode":"balanced"}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query-with-grounding", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resultsRaw, ok := payload["results"].([]any)
	if !ok || len(resultsRaw) < 2 {
		t.Fatalf("expected at least two merged results, got %#v", payload["results"])
	}

	var mergedA map[string]any
	for _, row := range resultsRaw {
		typed, _ := row.(map[string]any)
		if strings.TrimSpace(anyToString(typed["file"])) == "a.md" {
			mergedA = typed
			break
		}
	}
	if mergedA == nil {
		t.Fatalf("expected merged row for a.md, got %#v", resultsRaw)
	}
	sourcesList := anyToStringSlice(mergedA["sources"])
	sort.Strings(sourcesList)
	if strings.Join(sourcesList, ",") != "qdrant,topic_rollups" {
		t.Fatalf("expected merged sources qdrant,topic_rollups got %v", sourcesList)
	}

	grounding, ok := payload["grounding"].(map[string]any)
	if !ok {
		t.Fatalf("expected grounding payload")
	}
	strict, ok := grounding["strict_numeric_copy"].(bool)
	if !ok || !strict {
		t.Fatalf("expected strict_numeric_copy=true, got %#v", grounding["strict_numeric_copy"])
	}

	mu.Lock()
	sort.Strings(calledSources)
	observed := strings.Join(calledSources, ",")
	mu.Unlock()
	if observed != "qdrant,topic_rollups" {
		t.Fatalf("expected staged source fanout to qdrant/topic_rollups, got %s", observed)
	}
}

func TestStagedRetrievalAppliesQdrantSyncCap(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_TIMEOUT_SECS", "3")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_SECS", "0.2")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(350 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","score":0.9}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"alpha","limit":5,"retrieval_mode":"balanced"}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	warnings := parseWarnings(payload["warnings"])
	joinedWarnings := strings.ToLower(strings.Join(warnings, " | "))
	if !strings.Contains(joinedWarnings, "qdrant retrieval timed out") {
		t.Fatalf("expected qdrant timeout warning, got %v", warnings)
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	errorsMap, _ := debug["source_errors"].(map[string]any)
	qdrantErr, _ := errorsMap["qdrant"].(map[string]any)
	if timeoutFlag, ok := qdrantErr["timeout"].(bool); !ok || !timeoutFlag {
		t.Fatalf("expected source_errors.qdrant.timeout=true, got %#v", qdrantErr)
	}
}

func TestStagedRetrievalAppliesQdrantSyncCapByMode(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_TIMEOUT_SECS", "3")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_SECS", "0.6")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_FAST_SECS", "0.2")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_BALANCED_SECS", "0.5")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_DEEP_SECS", "1.0")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(350 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","score":0.9}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	fastReq := `{"request":{"query":"alpha","limit":5,"retrieval_mode":"fast"}}`
	fastResp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(fastReq))
	if err != nil {
		t.Fatalf("fast request failed: %v", err)
	}
	defer fastResp.Body.Close()
	if fastResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(fastResp.Body)
		t.Fatalf("expected 200 for fast mode, got %d body=%s", fastResp.StatusCode, string(body))
	}
	var fastPayload map[string]any
	if err := json.NewDecoder(fastResp.Body).Decode(&fastPayload); err != nil {
		t.Fatalf("decode fast response: %v", err)
	}
	fastWarnings := strings.ToLower(strings.Join(parseWarnings(fastPayload["warnings"]), " | "))
	if !strings.Contains(fastWarnings, "qdrant retrieval timed out") {
		t.Fatalf("expected fast mode timeout warning, got %v", fastPayload["warnings"])
	}

	deepReq := `{"request":{"query":"alpha","limit":5,"retrieval_mode":"deep"}}`
	deepResp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(deepReq))
	if err != nil {
		t.Fatalf("deep request failed: %v", err)
	}
	defer deepResp.Body.Close()
	if deepResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(deepResp.Body)
		t.Fatalf("expected 200 for deep mode, got %d body=%s", deepResp.StatusCode, string(body))
	}
	var deepPayload map[string]any
	if err := json.NewDecoder(deepResp.Body).Decode(&deepPayload); err != nil {
		t.Fatalf("decode deep response: %v", err)
	}
	deepWarnings := strings.ToLower(strings.Join(parseWarnings(deepPayload["warnings"]), " | "))
	if strings.Contains(deepWarnings, "qdrant retrieval timed out") {
		t.Fatalf("expected deep mode to avoid qdrant timeout, warnings=%v", deepPayload["warnings"])
	}
	deepRows, _ := deepPayload["results"].([]any)
	if len(deepRows) == 0 {
		t.Fatalf("expected deep mode qdrant rows, got %#v", deepPayload["results"])
	}
}

func TestStagedRetrievalSuppressesSlowTimeoutWarningsWhenFastSourcesSucceed(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups,mindsdb")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", "2")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "mindsdb")
	t.Setenv("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", "2")
	t.Setenv("ORCH_RETRIEVAL_MINDSDB_TIMEOUT_SECS", "0.2")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES", "mindsdb")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, _ := payload["request"].(map[string]any)
		sources, _ := request["sources"].([]any)
		source := ""
		if len(sources) > 0 {
			source = strings.TrimSpace(strings.ToLower(anyToString(sources[0])))
		}
		if source == "mindsdb" {
			time.Sleep(450 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if source == "topic_rollups" {
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"rollup.md","summary":"fast row","score":0.9}],"warnings":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"alpha","limit":5,"retrieval_mode":"balanced"}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if strings.Contains(warnings, "mindsdb retrieval timed out") {
		t.Fatalf("expected slow timeout warning suppression, got warnings=%v", payload["warnings"])
	}
	if !strings.Contains(warnings, "sources returned now: topic_rollups") {
		t.Fatalf("expected returned source summary warning, got %v", payload["warnings"])
	}
	if !strings.Contains(warnings, "additional context may be available later from: mindsdb") {
		t.Fatalf("expected deferred source summary warning, got %v", payload["warnings"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	errorsMap, _ := debug["source_errors"].(map[string]any)
	mindsdbErr, _ := errorsMap["mindsdb"].(map[string]any)
	if kind := strings.TrimSpace(anyToString(mindsdbErr["kind"])); kind != "budget_exceeded" {
		t.Fatalf("expected mindsdb error kind budget_exceeded, got %#v", mindsdbErr)
	}
	if timeoutFlag, ok := mindsdbErr["timeout"].(bool); !ok || timeoutFlag {
		t.Fatalf("expected source_errors.mindsdb.timeout=false, got %#v", mindsdbErr)
	}
}

func TestStagedRetrievalLexicalGuardDefersSyncSlowFallback(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_LEXICAL_GUARD_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_LEXICAL_GUARD_MIN_COVERAGE", "0.4")
	t.Setenv("GO_RETRIEVAL_LEXICAL_GUARD_MIN_RESULTS", "1")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups,mindsdb")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", "2")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "mindsdb")
	t.Setenv("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", "2")
	t.Setenv("ORCH_RETRIEVAL_MINDSDB_TIMEOUT_SECS", "0.2")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, _ := payload["request"].(map[string]any)
		sources, _ := request["sources"].([]any)
		source := ""
		if len(sources) > 0 {
			source = strings.TrimSpace(strings.ToLower(anyToString(sources[0])))
		}
		if source == "mindsdb" {
			time.Sleep(450 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if source == "topic_rollups" {
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"runbook.md","summary":"profitability baseline ladder tuning","score":0.95,"source":"topic_rollups"}],"warnings":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"profitability baseline ladder","limit":5,"retrieval_mode":"balanced","backend_policy":{"lexical_backend":"tantivy_lexical"}}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if !strings.Contains(warnings, "lexical backend policy deferred sync slow-source fallback") {
		t.Fatalf("expected lexical guard warning, got %v", payload["warnings"])
	}
	if strings.Contains(warnings, "mindsdb retrieval sync budget exceeded") {
		t.Fatalf("did not expect sync slow fallback timeout warning, got %v", payload["warnings"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	staged, _ := debug["staged_fetch"].(map[string]any)
	if applied, ok := staged["lexical_guard_applied"].(bool); !ok || !applied {
		t.Fatalf("expected lexical_guard_applied=true, got %#v", staged["lexical_guard_applied"])
	}
	if fallbackSources := anyToStringSlice(staged["sync_fallback_slow_sources"]); len(fallbackSources) != 0 {
		t.Fatalf("expected no sync fallback sources, got %v", fallbackSources)
	}
	errorsMap, _ := debug["source_errors"].(map[string]any)
	if _, exists := errorsMap["mindsdb"]; exists {
		t.Fatalf("expected mindsdb to be deferred from sync source errors, got %#v", errorsMap["mindsdb"])
	}
	lifecycle, _ := payload["retrieval_lifecycle"].(map[string]any)
	if strings.TrimSpace(strings.ToLower(anyToString(lifecycle["status"]))) != "partial" {
		t.Fatalf("expected retrieval_lifecycle.status=partial, got %#v", lifecycle["status"])
	}
	sourcesBlock, _ := lifecycle["sources"].(map[string]any)
	pending := anyToStringSlice(sourcesBlock["pending"])
	if len(pending) != 1 || pending[0] != "mindsdb" {
		t.Fatalf("expected pending sources [mindsdb], got %v", pending)
	}
}

func TestStagedRetrievalWithoutLexicalGuardRunsSyncSlowFallback(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_LEXICAL_GUARD_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_LEXICAL_GUARD_MIN_COVERAGE", "0.4")
	t.Setenv("GO_RETRIEVAL_LEXICAL_GUARD_MIN_RESULTS", "1")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups,mindsdb")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", "2")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "mindsdb")
	t.Setenv("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", "2")
	t.Setenv("ORCH_RETRIEVAL_MINDSDB_TIMEOUT_SECS", "0.2")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, _ := payload["request"].(map[string]any)
		sources, _ := request["sources"].([]any)
		source := ""
		if len(sources) > 0 {
			source = strings.TrimSpace(strings.ToLower(anyToString(sources[0])))
		}
		if source == "mindsdb" {
			time.Sleep(450 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if source == "topic_rollups" {
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"runbook.md","summary":"profitability baseline ladder tuning","score":0.95,"source":"topic_rollups"}],"warnings":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"profitability baseline ladder","limit":5,"retrieval_mode":"balanced","backend_policy":{"lexical_backend":"auto"}}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	staged, _ := debug["staged_fetch"].(map[string]any)
	if applied, _ := staged["lexical_guard_applied"].(bool); applied {
		t.Fatalf("expected lexical_guard_applied=false, got true")
	}
	fallbackSources := anyToStringSlice(staged["sync_fallback_slow_sources"])
	if len(fallbackSources) != 1 || fallbackSources[0] != "mindsdb" {
		t.Fatalf("expected sync fallback mindsdb, got %v", fallbackSources)
	}
	errorsMap, _ := debug["source_errors"].(map[string]any)
	mindsdbErr, _ := errorsMap["mindsdb"].(map[string]any)
	if kind := strings.TrimSpace(anyToString(mindsdbErr["kind"])); kind != "budget_exceeded" {
		t.Fatalf("expected mindsdb source error kind budget_exceeded, got %#v", mindsdbErr)
	}
	lifecycle, _ := payload["retrieval_lifecycle"].(map[string]any)
	if lifecycle == nil {
		t.Fatalf("expected retrieval_lifecycle payload")
	}
}

func TestStagedRetrievalCoverageRescueQueryVariant(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_COVERAGE_RESCUE_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_COVERAGE_RESCUE_MIN_TOKENS", "2")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", "2")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, _ := payload["request"].(map[string]any)
		query := strings.TrimSpace(strings.ToLower(anyToString(request["query"])))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(query, "run=1") {
			_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
			return
		}
		if strings.Contains(query, "profitability baseline ladder") {
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"runbook.md","summary":"profitability baseline ladder","score":0.91,"source":"topic_rollups"}],"warnings":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"profitability baseline ladder run=1","limit":5,"retrieval_mode":"balanced"}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	rows, _ := payload["results"].([]any)
	if len(rows) == 0 {
		t.Fatalf("expected coverage rescue results, got %#v", payload["results"])
	}
	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if !strings.Contains(warnings, "coverage rescue query variant returned results") {
		t.Fatalf("expected coverage rescue warning, got %v", payload["warnings"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	policy, _ := debug["source_policy"].(map[string]any)
	if applied, ok := policy["coverage_rescue_applied"].(bool); !ok || !applied {
		t.Fatalf("expected coverage_rescue_applied=true, got %#v", policy["coverage_rescue_applied"])
	}
	if strings.TrimSpace(anyToString(policy["coverage_rescue_query"])) != "profitability baseline ladder" {
		t.Fatalf("expected coverage_rescue_query to be sanitized, got %#v", policy["coverage_rescue_query"])
	}
}

func TestStagedRetrievalCarriesRuntimeBackendPolicy(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_RUST_LANE_PROMOTION_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_RUST_RETRIEVAL_VECTOR_BACKEND", "qdrant_remote")
	t.Setenv("ORCH_RUST_RETRIEVAL_LEXICAL_BACKEND", "auto")
	t.Setenv("ORCH_RUST_RETRIEVAL_BACKEND_STRICT", "false")

	capturedPolicy := map[string]any{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request, _ := payload["request"].(map[string]any)
		if backendPolicy, ok := request["backend_policy"].(map[string]any); ok {
			for key, value := range backendPolicy {
				capturedPolicy[key] = value
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"rollup.md","summary":"row","score":0.9,"source":"topic_rollups"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"alpha","limit":5,"retrieval_mode":"balanced","backend_policy":{"vector_backend":"usearch_ann","lexical_backend":"tantivy_lexical","memory_bank_backend":"quickwit_spike","strict":true}}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	policyBlock, _ := debug["source_policy"].(map[string]any)
	runtimePolicy, _ := policyBlock["runtime_backend_policy"].(map[string]any)
	if strings.TrimSpace(anyToString(runtimePolicy["vector_backend"])) != "usearch_ann" {
		t.Fatalf("expected vector backend override propagated, got %#v", runtimePolicy)
	}
	if strings.TrimSpace(anyToString(runtimePolicy["lexical_backend"])) != "tantivy_lexical" {
		t.Fatalf("expected lexical backend override propagated, got %#v", runtimePolicy)
	}
	if strings.TrimSpace(anyToString(runtimePolicy["memory_bank_backend"])) != "quickwit_spike" {
		t.Fatalf("expected memory_bank_backend override propagated, got %#v", runtimePolicy)
	}
	if strict, ok := runtimePolicy["strict"].(bool); !ok || !strict {
		t.Fatalf("expected strict=true propagated, got %#v", runtimePolicy)
	}
	if strings.TrimSpace(anyToString(capturedPolicy["vector_backend"])) != "usearch_ann" {
		t.Fatalf("expected backend subcall payload to include policy, got %#v", capturedPolicy)
	}
	if strings.TrimSpace(anyToString(capturedPolicy["memory_bank_backend"])) != "quickwit_spike" {
		t.Fatalf("expected backend subcall payload to include memory_bank_backend, got %#v", capturedPolicy)
	}
}

func TestResolveRustBackendPolicyAcceptsExtendedMemoryBackends(t *testing.T) {
	cases := []string{
		"lancedb_spike",
		"trieve_spike",
		"helixdb_spike",
		"icm_spike",
		"shodh_spike",
		"memvid_spike",
		"surrealdb_spike",
	}
	for _, backend := range cases {
		resolved := resolveRustBackendPolicy(
			map[string]any{
				"memory_bank_backend": backend,
			},
		)
		if got := strings.TrimSpace(anyToString(resolved["memory_bank_backend"])); got != backend {
			t.Fatalf("expected backend %q to be preserved, got %q", backend, got)
		}
	}
}

func TestHealthzIncludesBackendStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if payload["service"] != "gateway-go" {
		t.Fatalf("unexpected service payload: %v", payload)
	}
	if healthy, ok := payload["backendHealth"].(bool); !ok || !healthy {
		t.Fatalf("expected backendHealth=true, got %v", payload["backendHealth"])
	}
}

func TestContinuationEventsEndpointStreamsHistoryAndUpdates(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	token := "cont-token-test"
	s.publishContinuationEvent(token, map[string]any{
		"event":  "queued",
		"status": "queued",
		"source": "letta",
	})

	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/memory/search/continuations/" + token + "/events")
	if err != nil {
		t.Fatalf("events request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		t.Fatalf("expected text/event-stream content type, got %s", resp.Header.Get("Content-Type"))
	}

	firstChunkCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		firstChunkCh <- string(buf[:n])
	}()
	var firstChunk string
	select {
	case firstChunk = <-firstChunkCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial continuation stream chunk")
	}
	if !strings.Contains(firstChunk, "event: snapshot") {
		t.Fatalf("expected snapshot event in first chunk, got: %s", firstChunk)
	}

	s.publishContinuationEvent(token, map[string]any{
		"event":  "completed",
		"status": "ok",
		"source": "letta",
	})

	updateChunkCh := make(chan string, 1)
	go func() {
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		updateChunkCh <- string(buf[:n])
	}()
	var updateChunk string
	select {
	case updateChunk = <-updateChunkCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for continuation update chunk")
	}
	if !strings.Contains(updateChunk, "event: update") &&
		!strings.Contains(updateChunk, "event: heartbeat") &&
		!strings.Contains(updateChunk, "event: ready") {
		t.Fatalf("expected update, heartbeat, or ready event, got: %s", updateChunk)
	}
}

func TestAdaptiveTimeoutUsesP95AndBacklogPressure(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_MIN_REQUESTS", "3")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_WINDOW", "32")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_P95_FACTOR", "1.4")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_MIN_SCALE", "0.5")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_MAX_SCALE", "2.0")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_BACKLOG_WEIGHT", "0.2")
	t.Setenv("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_BACKLOG_CAP", "4")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TIMEOUT_SECS", "20")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	s.recordAdaptiveObservation(sourceLetta, 25*time.Second, false)
	s.recordAdaptiveObservation(sourceLetta, 30*time.Second, false)
	s.recordAdaptiveObservation(sourceLetta, 28*time.Second, false)

	adjustedNoBacklog, detailNoBacklog := s.resolveSourceTimeout(sourceLetta, "balanced", true, false, false)
	if adjustedNoBacklog <= 20*time.Second {
		t.Fatalf("expected adaptive timeout to exceed base timeout, got %s", adjustedNoBacklog)
	}
	if adjusted, _ := detailNoBacklog["adjusted"].(bool); !adjusted {
		t.Fatalf("expected adaptive detail adjusted=true, got %#v", detailNoBacklog)
	}

	s.incrementContinuationBacklog(sourceLetta)
	s.incrementContinuationBacklog(sourceLetta)
	s.incrementContinuationBacklog(sourceLetta)
	defer s.decrementContinuationBacklog(sourceLetta)
	defer s.decrementContinuationBacklog(sourceLetta)
	defer s.decrementContinuationBacklog(sourceLetta)

	adjustedWithBacklog, detailWithBacklog := s.resolveSourceTimeout(sourceLetta, "balanced", true, false, false)
	if adjustedWithBacklog >= adjustedNoBacklog {
		t.Fatalf("expected backlog pressure to reduce adaptive timeout: no_backlog=%s with_backlog=%s", adjustedNoBacklog, adjustedWithBacklog)
	}
	backlogInFlight := anyToInt(detailWithBacklog["backlog_inflight"], 0)
	if backlogInFlight < 1 {
		t.Fatalf("expected backlog_inflight in adaptive detail, got %#v", detailWithBacklog)
	}
}
