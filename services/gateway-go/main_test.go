package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
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

func TestStatusOverlaysGatewayHotPathOwnership(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"pythonHotPathOwnership": {
				"mode": "warn",
				"ok": false,
				"status": "non_gateway_hot_path_traffic_detected",
				"nonGatewayRequests": 5,
				"byPath": {"/memory/search": 5}
			}
		}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/status")
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	if strings.TrimSpace(anyToString(payload["statusSource"])) != "gateway-go" {
		t.Fatalf("expected statusSource=gateway-go, got %v", payload["statusSource"])
	}
	if strings.TrimSpace(anyToString(payload["routeOwnerClass"])) != sourceOwnerGoNative {
		t.Fatalf("expected routeOwnerClass=%s got %v", sourceOwnerGoNative, payload["routeOwnerClass"])
	}
	fallbackCounts, ok := payload["fallbackCounts"].(map[string]any)
	if !ok {
		t.Fatalf("expected fallbackCounts payload, got %#v", payload["fallbackCounts"])
	}
	if anyToInt(fallbackCounts["pythonHotPathTotal"], -1) != 0 {
		t.Fatalf("expected pythonHotPathTotal=0, got %#v", fallbackCounts["pythonHotPathTotal"])
	}
	ownership, ok := payload["pythonHotPathOwnership"].(map[string]any)
	if !ok {
		t.Fatalf("missing gateway pythonHotPathOwnership: %#v", payload["pythonHotPathOwnership"])
	}
	if strings.TrimSpace(anyToString(ownership["status"])) != "clean" {
		t.Fatalf("expected gateway ownership status clean, got %v", ownership["status"])
	}
	backendOwnership, ok := payload["backendPythonHotPathOwnership"].(map[string]any)
	if !ok {
		t.Fatalf("missing backendPythonHotPathOwnership: %#v", payload["backendPythonHotPathOwnership"])
	}
	if strings.TrimSpace(anyToString(backendOwnership["status"])) != "non_gateway_hot_path_traffic_detected" {
		t.Fatalf("expected backend status preserved, got %v", backendOwnership["status"])
	}
	warnings := parseWarnings(payload["warnings"])
	found := false
	for _, warning := range warnings {
		if strings.Contains(warning, "direct calls to python orchestrator") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning about backend direct python calls, got %v", warnings)
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

func TestMemoryV1UpdateMergesPatchAndWritesViaV1Put(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	callOrder := []string{}
	var putPayload map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callOrder = append(callOrder, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/memory/get":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"memory":{"id":"alpha::notes/a.md","project":"alpha","file_name":"notes/a.md","content":"{\"alpha\":1}"}}`))
		case "/v1/memory/put":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &putPayload)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"memory_id":"alpha::notes/a.md","result":{"event_id":"evt_alpha"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unexpected path"}`))
		}
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/v1/memory/update",
		strings.NewReader(`{"memory_id":"alpha::notes/a.md","patch":{"beta":2,"topic_path":"runbooks/testing"}}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("memory update request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for /v1/memory/update, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode update payload: %v", err)
	}
	if strings.TrimSpace(anyToString(payload["memory_id"])) != "alpha::notes/a.md" {
		t.Fatalf("unexpected memory_id response: %#v", payload)
	}
	if len(callOrder) != 2 || callOrder[0] != "/v1/memory/get" || callOrder[1] != "/v1/memory/put" {
		t.Fatalf("expected get->put call order, got %v", callOrder)
	}
	item, ok := putPayload["item"].(map[string]any)
	if !ok {
		t.Fatalf("expected item payload, got %#v", putPayload)
	}
	if strings.TrimSpace(anyToString(item["project"])) != "alpha" {
		t.Fatalf("unexpected project in put payload: %#v", item)
	}
	if strings.TrimSpace(anyToString(item["file_name"])) != "notes/a.md" {
		t.Fatalf("unexpected file_name in put payload: %#v", item)
	}
	if strings.TrimSpace(anyToString(item["topic_path"])) != "runbooks/testing" {
		t.Fatalf("unexpected topic_path in put payload: %#v", item)
	}
	content := strings.TrimSpace(anyToString(item["content"]))
	if !strings.Contains(content, `"alpha":1`) || !strings.Contains(content, `"beta":2`) {
		t.Fatalf("expected merged json content in put payload, got %s", content)
	}
}

func TestMemoryV1NeighborsRequiresMemoryID(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/memory/neighbors", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("neighbors request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 for missing memory_id, got %d body=%s", resp.StatusCode, string(body))
	}
}

func TestMemoryV1NeighborsFallsBackToBackendWhenStagedRetrievalDisabled(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	capturedPath := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"source":"topic_rollups","project":"alpha","file":"notes/a.md","summary":"ok","score":0.8}]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(
		gateway.URL+"/v1/memory/neighbors",
		"application/json",
		strings.NewReader(`{"memory_id":"alpha::notes/a.md","limit":5}`),
	)
	if err != nil {
		t.Fatalf("neighbors request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for neighbors fallback, got %d body=%s", resp.StatusCode, string(body))
	}
	if capturedPath != "/v1/memory/neighbors" {
		t.Fatalf("expected neighbors fallback path, got %s", capturedPath)
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

func TestGoFirstRetrievalContractDefaults(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_PYTHON_HOT_PATH_OWNERSHIP_MODE", "")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	if !s.retrieval.enabled {
		t.Fatalf("expected GO_RETRIEVAL_STAGED_ENABLED default to true")
	}
	fast := toSourceSet(s.retrieval.fastSources)
	for _, source := range []string{"topic_rollups", "qdrant", "weaviate", "postgres_pgvector"} {
		if _, ok := fast[source]; !ok {
			t.Fatalf("expected %s in default fast sources, got %v", source, s.retrieval.fastSources)
		}
	}
	if strings.TrimSpace(s.pythonHotPathMode) != "warn" {
		t.Fatalf("expected default GO_PYTHON_HOT_PATH_OWNERSHIP_MODE=warn, got %q", s.pythonHotPathMode)
	}
}

func TestMemorySearchFallbackRecordsPythonHotPathOwnership(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_PYTHON_HOT_PATH_OWNERSHIP_MODE", "warn")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")

	backendCalls := 0
	lastBackendPath := ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		lastBackendPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"source":"python-fallback"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/memory/search", "application/json", strings.NewReader(`{"query"`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 1 || lastBackendPath != "/memory/search" {
		t.Fatalf("expected one fallback proxy call to /memory/search, calls=%d path=%s", backendCalls, lastBackendPath)
	}

	infoResp, err := http.Get(gateway.URL + "/v1/info")
	if err != nil {
		t.Fatalf("info request failed: %v", err)
	}
	defer infoResp.Body.Close()
	if infoResp.StatusCode != http.StatusOK {
		t.Fatalf("expected info 200, got %d", infoResp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(infoResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode info response: %v", err)
	}
	ownership, ok := payload["pythonHotPathOwnership"].(map[string]any)
	if !ok {
		t.Fatalf("missing pythonHotPathOwnership payload: %#v", payload["pythonHotPathOwnership"])
	}
	if anyToBool(ownership["ok"]) {
		t.Fatalf("expected ownership assertion to flag fallback")
	}
	if anyToInt(ownership["fallbacks"], 0) != 1 {
		t.Fatalf("expected fallback count=1, got %#v", ownership["fallbacks"])
	}
	if strings.TrimSpace(anyToString(ownership["status"])) != "python_fallback_detected" {
		t.Fatalf("expected status python_fallback_detected, got %#v", ownership["status"])
	}
}

func TestMemorySearchFallbackBlockedWhenPythonHotPathModeStrict(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_PYTHON_HOT_PATH_OWNERSHIP_MODE", "strict")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")

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

	resp, err := http.Post(gateway.URL+"/memory/search", "application/json", strings.NewReader(`{"query"`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode strict fallback payload: %v", err)
	}
	if strings.TrimSpace(anyToString(payload["error"])) != "python_hot_path_fallback_blocked" {
		t.Fatalf("unexpected strict fallback error payload: %#v", payload)
	}
	if backendCalls != 0 {
		t.Fatalf("expected strict mode to block backend proxy fallback, calls=%d", backendCalls)
	}
}

func TestHotPathRoutesRemainGoOwned(t *testing.T) {
	sourceBytes, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	source := string(sourceBytes)
	required := []string{
		`mux.HandleFunc("/memory/search", s.memorySearch)`,
		`mux.HandleFunc("/memory/browser-context", s.memoryBrowserContext)`,
		`mux.HandleFunc("/memory/recall/eval-cases", s.memoryRecallEvalCases)`,
		`mux.HandleFunc("/memory/recall/eval-cases/refresh", s.memoryRecallEvalCasesRefresh)`,
		`mux.HandleFunc("/memory/recall/evaluate/saved", s.memoryRecallEvaluateSaved)`,
		`mux.HandleFunc("/memory/recent", s.memoryRecent)`,
		`mux.HandleFunc("/memory/files/", s.memoryFilesByProject)`,
		`mux.HandleFunc("/memory/continuity/snapshot", s.memoryContinuitySnapshot)`,
		`mux.HandleFunc("/memory/continuity/snapshots", s.memoryContinuitySnapshots)`,
		`mux.HandleFunc("/memory/continuity/snapshots/", s.memoryContinuitySnapshotByID)`,
		`mux.HandleFunc("/memory/topics", s.memoryTopicTree)`,
		`mux.HandleFunc("/memory/topics/list", s.memoryTopicList)`,
		`mux.HandleFunc("/memory/topic-rollups", s.memoryTopicRollups)`,
		`mux.HandleFunc("/feedback", s.feedbackRoute)`,
		`mux.HandleFunc("/agents/tasks", s.agentsTasksRoute)`,
		`mux.HandleFunc("/agents/tasks/", s.agentsTasksRoute)`,
		`mux.HandleFunc("/telemetry/metrics", s.telemetryMetricsRoute)`,
		`mux.HandleFunc("/telemetry/retrieval", s.telemetryRetrievalRoute)`,
		`mux.HandleFunc("/telemetry/retrieval/source-quality", s.telemetryRetrievalSourceQualityRoute)`,
		`mux.HandleFunc("/telemetry/trading", s.telemetryTradingRoute)`,
		`mux.HandleFunc("/telemetry/trading/history", s.telemetryTradingHistoryRoute)`,
		`mux.HandleFunc("/telemetry/", s.telemetryRoute)`,
		`mux.HandleFunc("/maintenance/", s.maintenanceRoute)`,
		`mux.HandleFunc("/v1/retrieval/query", s.retrievalQuery)`,
		`mux.HandleFunc("/v1/retrieval/query-with-grounding", s.retrievalQueryWithGrounding)`,
		`mux.HandleFunc("/v1/retrieval/batch-query", s.retrievalBatchQuery)`,
		`mux.HandleFunc("/v1/skills/quarantine/search", s.skillsQuarantineSearchRoute)`,
		`mux.HandleFunc("/v1/skills/quarantine/reindex", s.skillsQuarantineReindexRoute)`,
		`mux.HandleFunc("/v1/skills/index/search", s.skillsQuarantineSearchRoute)`,
		`mux.HandleFunc("/v1/skills/index/reindex", s.skillsQuarantineReindexRoute)`,
		`mux.HandleFunc("/v1/memory/get", s.memoryV1Get)`,
		`mux.HandleFunc("/v1/memory/update", s.memoryV1Update)`,
		`mux.HandleFunc("/v1/memory/neighbors", s.memoryV1Neighbors)`,
	}
	for _, needle := range required {
		if !strings.Contains(source, needle) {
			t.Fatalf("hot-path route ownership missing required handler mapping: %s", needle)
		}
	}
	if !strings.Contains(source, `"/migration/runtime", s.migrationRuntime`) {
		t.Fatalf("hot-path route ownership missing required migration runtime mapping to native handler")
	}
	blocked := []string{
		`mux.HandleFunc("/memory/search", s.proxy)`,
		`mux.HandleFunc("/memory/browser-context", s.proxy)`,
		`mux.HandleFunc("/memory/recall/eval-cases", s.proxy)`,
		`mux.HandleFunc("/memory/recall/eval-cases/refresh", s.proxy)`,
		`mux.HandleFunc("/memory/recall/evaluate/saved", s.proxy)`,
		`mux.HandleFunc("/memory/recent", s.proxy)`,
		`mux.HandleFunc("/memory/files/", s.proxy)`,
		`mux.HandleFunc("/memory/continuity/snapshot", s.proxy)`,
		`mux.HandleFunc("/memory/continuity/snapshots", s.proxy)`,
		`mux.HandleFunc("/memory/continuity/snapshots/", s.proxy)`,
		`mux.HandleFunc("/memory/topics", s.proxy)`,
		`mux.HandleFunc("/memory/topics/list", s.proxy)`,
		`mux.HandleFunc("/memory/topic-rollups", s.proxy)`,
		`mux.HandleFunc("/feedback", s.proxy)`,
		`mux.HandleFunc("/agents/tasks", s.proxy)`,
		`mux.HandleFunc("/agents/tasks/", s.proxy)`,
		`mux.HandleFunc("/telemetry/metrics", s.proxy)`,
		`mux.HandleFunc("/telemetry/retrieval", s.proxy)`,
		`mux.HandleFunc("/telemetry/retrieval/source-quality", s.proxy)`,
		`mux.HandleFunc("/telemetry/trading", s.proxy)`,
		`mux.HandleFunc("/telemetry/trading/history", s.proxy)`,
		`mux.HandleFunc("/telemetry/", s.proxy)`,
		`mux.HandleFunc("/maintenance/", s.proxy)`,
		`mux.HandleFunc("/migration/runtime", s.proxy)`,
		`registerEntitled("/migration/runtime", s.proxy)`,
		`mux.HandleFunc("/v1/retrieval/query", s.proxy)`,
		`mux.HandleFunc("/v1/retrieval/query-with-grounding", s.proxy)`,
		`mux.HandleFunc("/v1/retrieval/batch-query", s.proxy)`,
		`mux.HandleFunc("/v1/skills/quarantine/search", s.proxy)`,
		`mux.HandleFunc("/v1/skills/quarantine/reindex", s.proxy)`,
		`mux.HandleFunc("/v1/skills/index/search", s.proxy)`,
		`mux.HandleFunc("/v1/skills/index/reindex", s.proxy)`,
		`mux.HandleFunc("/v1/memory/get", s.proxy)`,
		`mux.HandleFunc("/v1/memory/update", s.proxy)`,
		`mux.HandleFunc("/v1/memory/neighbors", s.proxy)`,
	}
	for _, needle := range blocked {
		if strings.Contains(source, needle) {
			t.Fatalf("hot-path route drifted back to direct python proxy mapping: %s", needle)
		}
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

func TestMemorySearchAsyncStatusServedFromGatewayState(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	token := "token-123"
	s.publishContinuationEvent(token, map[string]any{
		"event":  "queued",
		"status": "queued",
		"source": "memory_bank",
	})
	s.publishContinuationEvent(token, map[string]any{
		"event":  "completed",
		"status": "ok",
		"source": "memory_bank",
	})
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/memory/search/async/" + token + "?include_result=false")
	if err != nil {
		t.Fatalf("async status request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode async status payload: %v", err)
	}
	if strings.TrimSpace(anyToString(payload["status"])) != "completed" {
		t.Fatalf("expected completed status, got %#v", payload["status"])
	}
	if _, present := payload["result"]; present {
		t.Fatalf("expected include_result=false to omit result payload")
	}
	if backendCalls != 0 {
		t.Fatalf("expected zero backend proxy calls, got %d", backendCalls)
	}
}

func TestMemorySearchJobsEventsServedFromGatewayContinuationStream(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	token := "token-123"
	s.publishContinuationEvent(token, map[string]any{
		"event":  "queued",
		"status": "queued",
		"source": "memory_bank",
	})

	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Get(gateway.URL + "/memory/search/jobs/" + token + "/events?include_result=false")
	if err != nil {
		t.Fatalf("events request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	reader := bufio.NewReader(resp.Body)
	lines := []string{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		lines = append(lines, line)
		if strings.Contains(line, "event: ready") {
			break
		}
	}
	rendered := strings.Join(lines, "")
	if !strings.Contains(rendered, "event: snapshot") {
		t.Fatalf("expected snapshot SSE event, got %s", rendered)
	}
	if !strings.Contains(rendered, token) {
		t.Fatalf("expected token in SSE payload, got %s", rendered)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		t.Fatalf("expected text/event-stream content type, got %s", resp.Header.Get("Content-Type"))
	}
	if backendCalls != 0 {
		t.Fatalf("expected zero backend proxy calls, got %d", backendCalls)
	}
}

func TestMemoryContextPackServedFromGatewayHandler(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	proxyPathCalls := 0
	retrievalCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/retrieval/query":
			retrievalCalls += 1
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/a.md","source":"qdrant","score":0.88,"summary":"context pack fact","topic_path":"runbooks/codex-integration","timestamp":"2026-03-30T00:00:00Z"}],"warnings":[]}`))
			return
		case "/memory/context-pack":
			proxyPathCalls += 1
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"proxy path should not be called"}`))
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

	reqBody := `{"project":"contextlattice","query":"gateway native context pack","topic_path":"runbooks/codex-integration","limit":5,"max_facts":10,"include_retrieval_debug":true}`
	resp, err := http.Post(gateway.URL+"/memory/context-pack", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("context-pack request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode context-pack payload: %v", err)
	}
	if payload["context_pack"] == nil {
		t.Fatalf("expected context_pack payload, got %#v", payload)
	}
	contextPack, _ := payload["context_pack"].(map[string]any)
	if strings.TrimSpace(anyToString(contextPack["query"])) != "gateway native context pack" {
		t.Fatalf("unexpected context pack query: %#v", contextPack["query"])
	}
	if retrievalCalls < 1 {
		t.Fatalf("expected at least one retrieval backend call, got %d", retrievalCalls)
	}
	if proxyPathCalls != 0 {
		t.Fatalf("expected zero backend /memory/context-pack proxy calls, got %d", proxyPathCalls)
	}
}

func TestProxyForwardsBatchAndOpsQueuePaths(t *testing.T) {
	var capturedPath string
	var capturedBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		capturedBody = string(raw)
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
	if capturedPath == "/ops/queue/status" {
		t.Fatalf("expected /ops/queue/status to be handled natively, got proxied path %s", capturedPath)
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
	if capturedPath != "/memory/write" {
		t.Fatalf("expected /memory/browser-context to route through native writer, got backend path %s", capturedPath)
	}
	if !strings.Contains(capturedBody, `"topicPath":"browser/context"`) {
		t.Fatalf("expected browser context topic path in forwarded write body, got %s", capturedBody)
	}
	if !strings.Contains(capturedBody, "Browser Context Snapshot") {
		t.Fatalf("expected browser snapshot header in forwarded write body, got %s", capturedBody)
	}

	resp4, err := http.Get(gateway.URL + "/ops/capabilities")
	if err != nil {
		t.Fatalf("capabilities request failed: %v", err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /ops/capabilities, got %d", resp4.StatusCode)
	}
	if capturedPath == "/ops/capabilities" {
		t.Fatalf("expected /ops/capabilities to be handled natively, got proxied path %s", capturedPath)
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

	resp6, err := http.Get(gateway.URL + "/memory/recent?project=contextlattice")
	if err != nil {
		t.Fatalf("memory recent request failed: %v", err)
	}
	defer resp6.Body.Close()
	if resp6.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /memory/recent, got %d", resp6.StatusCode)
	}
	if capturedPath != "/memory/recent" {
		t.Fatalf("expected /memory/recent to be forwarded by native route, got %s", capturedPath)
	}

	resp7, err := http.Get(gateway.URL + "/memory/topics?project=contextlattice")
	if err != nil {
		t.Fatalf("memory topics request failed: %v", err)
	}
	defer resp7.Body.Close()
	if resp7.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /memory/topics, got %d", resp7.StatusCode)
	}
	if capturedPath != "/memory/topics" {
		t.Fatalf("expected /memory/topics to be forwarded by native route, got %s", capturedPath)
	}

	req8, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/feedback",
		strings.NewReader(`{"project":"alpha","content":"native feedback route"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req8.Header.Set("Content-Type", "application/json")
	resp8, err := http.DefaultClient.Do(req8)
	if err != nil {
		t.Fatalf("feedback route request failed: %v", err)
	}
	defer resp8.Body.Close()
	if resp8.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /feedback, got %d", resp8.StatusCode)
	}
	if capturedPath != "/feedback" {
		t.Fatalf("expected /feedback to be forwarded by native route, got %s", capturedPath)
	}

	resp9, err := http.Get(gateway.URL + "/agents/tasks?project=contextlattice")
	if err != nil {
		t.Fatalf("agents tasks request failed: %v", err)
	}
	defer resp9.Body.Close()
	if resp9.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /agents/tasks, got %d", resp9.StatusCode)
	}
	if capturedPath != "/agents/tasks" {
		t.Fatalf("expected /agents/tasks to be forwarded by native route, got %s", capturedPath)
	}

	resp10, err := http.Get(gateway.URL + "/telemetry/recall")
	if err != nil {
		t.Fatalf("telemetry request failed: %v", err)
	}
	defer resp10.Body.Close()
	if resp10.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /telemetry/recall, got %d", resp10.StatusCode)
	}
	if capturedPath != "/telemetry/recall" {
		t.Fatalf("expected /telemetry/recall to be forwarded by native route, got %s", capturedPath)
	}

	resp11, err := http.Get(gateway.URL + "/maintenance/diagnostics")
	if err != nil {
		t.Fatalf("maintenance request failed: %v", err)
	}
	defer resp11.Body.Close()
	if resp11.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /maintenance/diagnostics, got %d", resp11.StatusCode)
	}
	if capturedPath != "/maintenance/diagnostics" {
		t.Fatalf("expected /maintenance/diagnostics to be forwarded by native route, got %s", capturedPath)
	}
}

func TestTelemetryRouteRejectsNonGET(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/telemetry/recall", strings.NewReader(`{"noop":true}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("telemetry method gate request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 405, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 0 {
		t.Fatalf("expected zero backend calls for disallowed telemetry method, got %d", backendCalls)
	}
}

func TestTelemetryNativeRoutesStayGoOwnedInStrictRuntime(t *testing.T) {
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	t.Setenv("TRADING_HISTORY_LIMIT", "16")
	t.Setenv("TRADING_HISTORY_PATH", filepath.Join(t.TempDir(), "trading_metrics.ndjson"))
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant,weaviate,postgres_pgvector,mongo_raw,mindsdb,topic_rollups,letta,memory_bank")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups,qdrant,weaviate,postgres_pgvector")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb,mongo_raw,letta,memory_bank")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	metricsPost, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/telemetry/metrics",
		strings.NewReader(`{"timestamp":"2026-04-01T00:00:00Z","queueDepth":3,"batchSize":64,"totals":{"enqueued":10,"dropped":1,"batches":2,"flushedEvents":9}}`),
	)
	if err != nil {
		t.Fatalf("metrics post request: %v", err)
	}
	metricsPost.Header.Set("Content-Type", "application/json")
	metricsPostResp, err := http.DefaultClient.Do(metricsPost)
	if err != nil {
		t.Fatalf("metrics post failed: %v", err)
	}
	defer metricsPostResp.Body.Close()
	if metricsPostResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(metricsPostResp.Body)
		t.Fatalf("expected 200 for /telemetry/metrics POST, got %d body=%s", metricsPostResp.StatusCode, string(body))
	}

	metricsResp, err := http.Get(gateway.URL + "/telemetry/metrics")
	if err != nil {
		t.Fatalf("metrics get failed: %v", err)
	}
	defer metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(metricsResp.Body)
		t.Fatalf("expected 200 for /telemetry/metrics GET, got %d body=%s", metricsResp.StatusCode, string(body))
	}
	var metricsPayload map[string]any
	if err := json.NewDecoder(metricsResp.Body).Decode(&metricsPayload); err != nil {
		t.Fatalf("decode metrics payload: %v", err)
	}
	if anyToInt(metricsPayload["queueDepth"], 0) != 3 {
		t.Fatalf("expected queueDepth=3, got %#v", metricsPayload["queueDepth"])
	}

	retrievalResp, err := http.Get(gateway.URL + "/telemetry/retrieval?traffic_class=user")
	if err != nil {
		t.Fatalf("retrieval telemetry failed: %v", err)
	}
	defer retrievalResp.Body.Close()
	if retrievalResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(retrievalResp.Body)
		t.Fatalf("expected 200 for /telemetry/retrieval, got %d body=%s", retrievalResp.StatusCode, string(body))
	}
	var retrievalPayload map[string]any
	if err := json.NewDecoder(retrievalResp.Body).Decode(&retrievalPayload); err != nil {
		t.Fatalf("decode retrieval payload: %v", err)
	}
	latency, _ := retrievalPayload["latency"].(map[string]any)
	sources, _ := latency["sources"].(map[string]any)
	if len(sources) == 0 {
		t.Fatalf("expected retrieval latency sources in payload, got %#v", retrievalPayload)
	}

	qualityResp, err := http.Get(gateway.URL + "/telemetry/retrieval/source-quality?traffic_class=user&window_mins=30")
	if err != nil {
		t.Fatalf("source-quality request failed: %v", err)
	}
	defer qualityResp.Body.Close()
	if qualityResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(qualityResp.Body)
		t.Fatalf("expected 200 for /telemetry/retrieval/source-quality, got %d body=%s", qualityResp.StatusCode, string(body))
	}
	var qualityPayload map[string]any
	if err := json.NewDecoder(qualityResp.Body).Decode(&qualityPayload); err != nil {
		t.Fatalf("decode source-quality payload: %v", err)
	}
	rows, _ := qualityPayload["sources"].([]any)
	if len(rows) == 0 {
		t.Fatalf("expected source-quality rows, got %#v", qualityPayload)
	}

	tradingPost, err := http.NewRequest(
		http.MethodPost,
		gateway.URL+"/telemetry/trading",
		strings.NewReader(`{"timestamp":"2026-04-01T00:01:00Z","open_positions":2,"total_value_usd":1200.5,"unrealized_pnl":12.3,"realized_pnl":5.1,"daily_pnl":17.4,"positions":[{"symbol":"SOL","size":1.2}]}`),
	)
	if err != nil {
		t.Fatalf("trading post request: %v", err)
	}
	tradingPost.Header.Set("Content-Type", "application/json")
	tradingPostResp, err := http.DefaultClient.Do(tradingPost)
	if err != nil {
		t.Fatalf("trading post failed: %v", err)
	}
	defer tradingPostResp.Body.Close()
	if tradingPostResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tradingPostResp.Body)
		t.Fatalf("expected 200 for /telemetry/trading POST, got %d body=%s", tradingPostResp.StatusCode, string(body))
	}

	tradingResp, err := http.Get(gateway.URL + "/telemetry/trading")
	if err != nil {
		t.Fatalf("trading get failed: %v", err)
	}
	defer tradingResp.Body.Close()
	if tradingResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tradingResp.Body)
		t.Fatalf("expected 200 for /telemetry/trading GET, got %d body=%s", tradingResp.StatusCode, string(body))
	}
	var tradingPayload map[string]any
	if err := json.NewDecoder(tradingResp.Body).Decode(&tradingPayload); err != nil {
		t.Fatalf("decode trading payload: %v", err)
	}
	if anyToInt(tradingPayload["openPositions"], 0) != 2 {
		t.Fatalf("expected openPositions=2, got %#v", tradingPayload["openPositions"])
	}

	historyResp, err := http.Get(gateway.URL + "/telemetry/trading/history?limit=5")
	if err != nil {
		t.Fatalf("trading history get failed: %v", err)
	}
	defer historyResp.Body.Close()
	if historyResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(historyResp.Body)
		t.Fatalf("expected 200 for /telemetry/trading/history, got %d body=%s", historyResp.StatusCode, string(body))
	}
	var historyPayload map[string]any
	if err := json.NewDecoder(historyResp.Body).Decode(&historyPayload); err != nil {
		t.Fatalf("decode history payload: %v", err)
	}
	historyRows, _ := historyPayload["history"].([]any)
	if len(historyRows) != 1 {
		t.Fatalf("expected trading history size=1, got %#v", historyPayload["history"])
	}

	if backendCalls != 0 {
		t.Fatalf("expected zero backend calls for go-native telemetry routes, got %d", backendCalls)
	}
	if s.pythonHotPathFallbacks.Load() != 0 {
		t.Fatalf("expected zero python fallback count, got %d", s.pythonHotPathFallbacks.Load())
	}
}

func TestMaintenanceRouteMethodGateAndProtectedEntitlement(t *testing.T) {
	t.Setenv("GO_V4_ENTITLEMENT_MODE", "enforce")
	t.Setenv("GO_V4_ENTITLEMENT_DEV_ALLOW", "false")
	t.Setenv("GO_V4_ENTITLEMENT_PROTECTED_PATHS", "/maintenance/diagnostics")
	t.Setenv("GO_V4_ENTITLEMENT_ALLOWED_PLANS", "team,enterprise")
	t.Setenv("GO_V4_ENTITLEMENT_ALLOWED_ROLES", "owner,admin")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqMethod, err := http.NewRequest(http.MethodPut, gateway.URL+"/maintenance/diagnostics", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	respMethod, err := http.DefaultClient.Do(reqMethod)
	if err != nil {
		t.Fatalf("maintenance method gate request failed: %v", err)
	}
	defer respMethod.Body.Close()
	if respMethod.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(respMethod.Body)
		t.Fatalf("expected 402 for protected maintenance path without entitlement, got %d body=%s", respMethod.StatusCode, string(body))
	}

	reqEntitled, err := http.NewRequest(http.MethodGet, gateway.URL+"/maintenance/diagnostics", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	reqEntitled.Header.Set("X-ContextLattice-Plan", "team")
	reqEntitled.Header.Set("X-ContextLattice-Workspace-Role", "owner")
	respEntitled, err := http.DefaultClient.Do(reqEntitled)
	if err != nil {
		t.Fatalf("maintenance entitled request failed: %v", err)
	}
	defer respEntitled.Body.Close()
	if respEntitled.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respEntitled.Body)
		t.Fatalf("expected 200 for entitled maintenance request, got %d body=%s", respEntitled.StatusCode, string(body))
	}
	if backendCalls != 1 {
		t.Fatalf("expected one backend call for entitled maintenance request, got %d", backendCalls)
	}
}

func TestMemoryBrowserContextValidationBlocksMissingProject(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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
		gateway.URL+"/memory/browser-context",
		strings.NewReader(`{"pageUrl":"https://example.com","textSnapshot":"hello world"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("browser context request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422 for missing projectName, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 0 {
		t.Fatalf("expected zero backend calls on validation failure, got %d", backendCalls)
	}
}

func TestMemoryBrowserContextDisabledByEnv(t *testing.T) {
	t.Setenv("ORCH_BROWSER_CONTEXT_INGEST_ENABLED", "false")
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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
		gateway.URL+"/memory/browser-context",
		strings.NewReader(`{"projectName":"alpha","pageUrl":"https://example.com","textSnapshot":"hello world"}`),
	)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("browser context request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503 when browser context ingest is disabled, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 0 {
		t.Fatalf("expected zero backend calls when ingest is disabled, got %d", backendCalls)
	}
}

func TestToolsCapabilityMapGETIsServedNatively(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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
	if backendCalls != 0 {
		t.Fatalf("expected no backend calls for native capability map, got %d", backendCalls)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !anyToBool(payload["enabled"]) {
		t.Fatalf("expected enabled=true, got %#v", payload["enabled"])
	}
	tools, _ := payload["tools"].(map[string]any)
	if !anyToBool(tools["browser_context_ingest"]) {
		t.Fatalf("expected browser_context_ingest=true by default, got %#v", tools["browser_context_ingest"])
	}
}

func TestToolsCapabilityMapReflectsBrowserContextToggle(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")
	t.Setenv("ORCH_BROWSER_CONTEXT_INGEST_ENABLED", "false")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	tools, _ := payload["tools"].(map[string]any)
	if anyToBool(tools["browser_context_ingest"]) {
		t.Fatalf("expected browser_context_ingest=false when disabled by env, got %#v", tools["browser_context_ingest"])
	}
}

func TestToolsOpsQueueStatusDefaultsToExcludeDeadletters(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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
	if backendCalls != 0 {
		t.Fatalf("expected no backend calls for native ops queue status, got %d", backendCalls)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	deadletters, _ := payload["deadletters"].(map[string]any)
	if anyToBool(deadletters["included"]) {
		t.Fatalf("expected include_deadletters default false for tools route, got %#v", deadletters["included"])
	}
}

func TestToolsDefaultOpenIgnoresExplicitInvalidKeyUnlessEnforced(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_GATEWAY_TEST_KEEP_ORCH_KEY", "true")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "good-key")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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
	if backendCalls != 0 {
		t.Fatalf("expected native capability map with zero backend calls, got %d", backendCalls)
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
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
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
	if backendCalls != 0 {
		t.Fatalf("expected capability_map to be served natively, got backendCalls=%d", backendCalls)
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
	if backendCalls != 0 {
		t.Fatalf("expected zero backend calls for native allowlisted/blocked worker tool routes, got %d backend calls", backendCalls)
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

func TestMemoryProfilesCRUDServedNatively(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	profilePath := filepath.Join(t.TempDir(), "agent_memory_profiles.json")
	t.Setenv("AGENT_MEMORY_PROFILE_PATH", profilePath)

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

	respList, err := http.Get(gateway.URL + "/memory/profiles")
	if err != nil {
		t.Fatalf("list profiles request failed: %v", err)
	}
	defer respList.Body.Close()
	if respList.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respList.Body)
		t.Fatalf("expected 200 listing profiles, got %d body=%s", respList.StatusCode, string(body))
	}
	var listPayload map[string]any
	if err := json.NewDecoder(respList.Body).Decode(&listPayload); err != nil {
		t.Fatalf("decode list payload: %v", err)
	}
	if anyToInt(listPayload["count"], 0) < 1 {
		t.Fatalf("expected at least default profile, got %#v", listPayload["count"])
	}

	reqUpsert, err := http.NewRequest(
		http.MethodPut,
		gateway.URL+"/memory/profiles/codex_gpt5",
		strings.NewReader(`{"retrieval_mode":"fast","sources":["topic_rollups","qdrant"],"default_project":"algotraderv2_rust"}`),
	)
	if err != nil {
		t.Fatalf("build upsert request: %v", err)
	}
	reqUpsert.Header.Set("Content-Type", "application/json")
	respUpsert, err := http.DefaultClient.Do(reqUpsert)
	if err != nil {
		t.Fatalf("upsert profile request failed: %v", err)
	}
	defer respUpsert.Body.Close()
	if respUpsert.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respUpsert.Body)
		t.Fatalf("expected 200 upserting profile, got %d body=%s", respUpsert.StatusCode, string(body))
	}

	respGet, err := http.Get(gateway.URL + "/memory/profiles/codex_gpt5")
	if err != nil {
		t.Fatalf("get profile request failed: %v", err)
	}
	defer respGet.Body.Close()
	if respGet.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respGet.Body)
		t.Fatalf("expected 200 getting profile, got %d body=%s", respGet.StatusCode, string(body))
	}
	var getPayload map[string]any
	if err := json.NewDecoder(respGet.Body).Decode(&getPayload); err != nil {
		t.Fatalf("decode get payload: %v", err)
	}
	if !anyToBool(getPayload["exists"]) {
		t.Fatalf("expected exists=true for codex profile")
	}
	profile, _ := getPayload["profile"].(map[string]any)
	if strings.TrimSpace(anyToString(profile["retrieval_mode"])) != "fast" {
		t.Fatalf("expected retrieval_mode=fast, got %#v", profile["retrieval_mode"])
	}

	reqDeleteDefault, err := http.NewRequest(http.MethodDelete, gateway.URL+"/memory/profiles/default", nil)
	if err != nil {
		t.Fatalf("build default delete request: %v", err)
	}
	respDeleteDefault, err := http.DefaultClient.Do(reqDeleteDefault)
	if err != nil {
		t.Fatalf("delete default request failed: %v", err)
	}
	defer respDeleteDefault.Body.Close()
	if respDeleteDefault.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(respDeleteDefault.Body)
		t.Fatalf("expected 400 deleting default profile, got %d body=%s", respDeleteDefault.StatusCode, string(body))
	}

	reqDelete, err := http.NewRequest(http.MethodDelete, gateway.URL+"/memory/profiles/codex_gpt5", nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	respDelete, err := http.DefaultClient.Do(reqDelete)
	if err != nil {
		t.Fatalf("delete profile request failed: %v", err)
	}
	defer respDelete.Body.Close()
	if respDelete.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respDelete.Body)
		t.Fatalf("expected 200 deleting profile, got %d body=%s", respDelete.StatusCode, string(body))
	}

	if backendCalls != 0 {
		t.Fatalf("expected native profile handlers without backend calls, got %d", backendCalls)
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

func TestTelemetryWriteFallsBackToSpoolWhenSinkUnavailable(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "true")
	spoolPath := filepath.Join(t.TempDir(), "telemetry_spool.ndjson")
	t.Setenv("GO_TELEMETRY_SPOOL_PATH", spoolPath)

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

	reqBody := `{"projectName":"alpha","fileName":"telemetry__agg-latest.json","content":"{\"cpu\":0.8}","topicPath":"telemetry/runtime"}`
	resp, err := http.Post(gateway.URL+"/memory/write", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202 from spool fallback, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !anyToBool(payload["telemetry_spooled"]) {
		t.Fatalf("expected telemetry_spooled=true, got %#v", payload["telemetry_spooled"])
	}
	if strings.TrimSpace(anyToString(payload["lane"])) != "telemetry_spool_fallback" {
		t.Fatalf("unexpected lane=%#v", payload["lane"])
	}
	if backendCalls != 0 {
		t.Fatalf("expected no backend fanout on telemetry write, got %d", backendCalls)
	}
	raw, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatalf("expected spool file to exist: %v", err)
	}
	if !strings.Contains(string(raw), `"project":"alpha"`) {
		t.Fatalf("spool file missing project payload: %s", string(raw))
	}
}

func TestTelemetryWriteFailsWhenSinkAndSpoolUnavailable(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_RING_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_RING_CAPACITY", "32")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"projectName":"alpha","fileName":"telemetry__agg-latest.json","content":"{\"cpu\":0.8}","topicPath":"telemetry/runtime"}`
	resp, err := http.Post(gateway.URL+"/memory/write", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202 accepted_degraded when sink+spool unavailable, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !anyToBool(payload["accepted_degraded"]) {
		t.Fatalf("expected accepted_degraded=true, got %#v", payload["accepted_degraded"])
	}
	if strings.TrimSpace(anyToString(payload["lane"])) != "telemetry_ring_fallback" {
		t.Fatalf("expected telemetry_ring_fallback lane, got %#v", payload["lane"])
	}
	if !anyToBool(payload["telemetry_buffered"]) {
		t.Fatalf("expected telemetry_buffered=true, got %#v", payload["telemetry_buffered"])
	}
}

func TestTelemetryRingEvictsOldestLowValueFirst(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_RING_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_RING_CAPACITY", "2")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	requests := []string{
		`{"projectName":"alpha","fileName":"telemetry__heartbeat.json","content":"{\"cpu\":0.8}","topicPath":"telemetry/runtime/heartbeat"}`,
		`{"projectName":"alpha","fileName":"telemetry__incident.json","content":"{\"error\":\"timeout\"}","topicPath":"telemetry/runtime/alerts"}`,
		`{"projectName":"alpha","fileName":"telemetry__incident2.json","content":"{\"error\":\"critical\"}","topicPath":"telemetry/runtime/alerts"}`,
	}
	for idx, reqBody := range requests {
		resp, err := http.Post(gateway.URL+"/memory/write", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("request %d failed: %v", idx, err)
		}
		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("request %d expected 202, got %d body=%s", idx, resp.StatusCode, string(body))
		}
		resp.Body.Close()
	}

	entries := s.telemetryRing.debugEntries()
	if len(entries) != 2 {
		t.Fatalf("expected ring depth=2, got %d", len(entries))
	}
	for _, row := range entries {
		if row.fileName == "telemetry__heartbeat.json" {
			t.Fatalf("expected low-value heartbeat entry to be evicted first")
		}
	}
	stats := s.telemetryRing.snapshot()
	if anyToInt(stats["dropped"], 0) != 1 {
		t.Fatalf("expected dropped=1, got %#v", stats["dropped"])
	}
	if anyToInt(stats["droppedLowValue"], 0) != 1 {
		t.Fatalf("expected droppedLowValue=1, got %#v", stats["droppedLowValue"])
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

func TestStagedRetrievalReportsSourceOwnershipDebug(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_MODE", "off")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")

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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","summary":"vector row","score":0.88,"source":"qdrant"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(`{"request":{"query":"alpha","limit":5}}`))
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
	if strings.TrimSpace(anyToString(payload["route_owner_class"])) != sourceOwnerGoNative {
		t.Fatalf("expected route_owner_class=%s got %#v", sourceOwnerGoNative, payload["route_owner_class"])
	}
	if strings.TrimSpace(anyToString(payload["source_owner_class"])) != sourceOwnerPythonBackendFallback {
		t.Fatalf("expected source_owner_class=%s got %#v", sourceOwnerPythonBackendFallback, payload["source_owner_class"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	sourceOwners, _ := debug["source_owners"].(map[string]any)
	if strings.TrimSpace(anyToString(sourceOwners["qdrant"])) != sourceOwnerPythonBackendFallback {
		t.Fatalf("expected qdrant source owner=%s got %#v", sourceOwnerPythonBackendFallback, sourceOwners["qdrant"])
	}
	sourcePolicy, _ := debug["source_policy"].(map[string]any)
	if strings.TrimSpace(anyToString(sourcePolicy["source_ownership_mode"])) != "off" {
		t.Fatalf("expected source_ownership_mode=off got %#v", sourcePolicy["source_ownership_mode"])
	}
}

func TestStagedRetrievalQdrantGoAdapterOwnership(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_MODE", "off")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("QDRANT_LOCAL_URL", "")

	backendRetrievalCalls := 0
	qdrantCollectionCalls := 0
	qdrantSearchCalls := 0
	var qdrantCapturedBody string
	var qdrantCapturedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		switch r.URL.Path {
		case "/collections/contextlattice_notes":
			qdrantCollectionCalls += 1
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
			return
		case "/collections/contextlattice_notes/points/search":
			qdrantSearchCalls += 1
			raw, _ := io.ReadAll(r.Body)
			qdrantCapturedBody = string(raw)
			qdrantCapturedPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":[{"score":0.91,"payload":{"project":"alpha","file":"notes/a.md","summary":"profitability baseline ladder","topic_path":"runbooks/testing","created_at":"2026-03-30T00:00:00Z"}}]}`))
			return
		case "/v1/retrieval/query":
			backendRetrievalCalls += 1
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer backend.Close()
	t.Setenv("QDRANT_URL", backend.URL)

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(`{"request":{"query":"profitability baseline ladder","project":"alpha","topic_path":"runbooks/testing","limit":5}}`))
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
	results, _ := payload["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected qdrant go adapter result, got %#v", payload["results"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	sourceOwners, _ := debug["source_owners"].(map[string]any)
	if strings.TrimSpace(anyToString(sourceOwners["qdrant"])) != sourceOwnerGoNative {
		t.Fatalf("expected qdrant source owner=%s got %#v", sourceOwnerGoNative, sourceOwners["qdrant"])
	}
	if strings.TrimSpace(anyToString(debug["source_owner_class"])) != sourceOwnerGoNative {
		t.Fatalf("expected source_owner_class=%s got %#v", sourceOwnerGoNative, debug["source_owner_class"])
	}
	if qdrantCollectionCalls < 1 {
		t.Fatalf("expected qdrant collection probe call, got %d", qdrantCollectionCalls)
	}
	if qdrantSearchCalls != 1 {
		t.Fatalf("expected one qdrant search call, got %d", qdrantSearchCalls)
	}
	if backendRetrievalCalls != 0 {
		t.Fatalf("expected zero backend /v1/retrieval/query calls for qdrant go adapter success, got %d", backendRetrievalCalls)
	}
	if qdrantCapturedPath != "/collections/contextlattice_notes/points/search" {
		t.Fatalf("unexpected qdrant path: %s", qdrantCapturedPath)
	}
	if !strings.Contains(qdrantCapturedBody, `"vector":[`) {
		t.Fatalf("expected qdrant payload to include query vector, got %s", qdrantCapturedBody)
	}
}

func TestStagedRetrievalTopicRollupsGoAdapterOwnership(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_MODE", "off")

	backendRetrievalCalls := 0
	backendRollupCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		switch r.URL.Path {
		case "/memory/topic-rollups":
			backendRollupCalls += 1
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"topics":[{"project":"alpha","path":"runbooks/testing","summarySnippets":["profitability baseline ladder"],"latestTimestamp":"2026-03-30T00:00:00Z"}]}`))
			return
		case "/v1/retrieval/query":
			backendRetrievalCalls += 1
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
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

	resp, err := http.Post(
		gateway.URL+"/v1/retrieval/query",
		"application/json",
		strings.NewReader(`{"request":{"query":"profitability baseline ladder","project":"alpha","limit":5}}`),
	)
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
	results, _ := payload["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected go adapter to produce topic rollup results, got %#v", payload["results"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	sourceOwners, _ := debug["source_owners"].(map[string]any)
	if strings.TrimSpace(anyToString(sourceOwners["topic_rollups"])) != sourceOwnerGoNative {
		t.Fatalf("expected topic_rollups source owner=%s got %#v", sourceOwnerGoNative, sourceOwners["topic_rollups"])
	}
	if strings.TrimSpace(anyToString(debug["source_owner_class"])) != sourceOwnerGoNative {
		t.Fatalf("expected source_owner_class=%s got %#v", sourceOwnerGoNative, debug["source_owner_class"])
	}
	if backendRollupCalls != 1 {
		t.Fatalf("expected one /memory/topic-rollups adapter call, got %d", backendRollupCalls)
	}
	if backendRetrievalCalls != 0 {
		t.Fatalf("expected zero backend /v1/retrieval/query calls for go adapter success, got %d", backendRetrievalCalls)
	}
}

func TestStagedRetrievalSourceOwnershipStrictGate(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_MODE", "strict")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_STRICT_FAST_ALLOW_PYTHON", "")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","summary":"vector row","score":0.88,"source":"qdrant"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(`{"request":{"query":"alpha","limit":5}}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 503 strict ownership violation, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.TrimSpace(anyToString(payload["error"])) != "source_ownership_violation" {
		t.Fatalf("expected source_ownership_violation error, got %#v", payload["error"])
	}
	violations := anyToStringSlice(payload["ownership_violations"])
	if len(violations) != 1 || violations[0] != "qdrant" {
		t.Fatalf("expected ownership violation [qdrant], got %v", violations)
	}
	if backendCalls < 1 {
		t.Fatalf("expected at least one backend call for source ownership accounting, got %d", backendCalls)
	}
}

func TestStagedRetrievalSourceOwnershipStrictAllowlist(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_MODE", "strict")
	t.Setenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_STRICT_FAST_ALLOW_PYTHON", "qdrant")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")

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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"a.md","summary":"vector row","score":0.88,"source":"qdrant"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(`{"request":{"query":"alpha","limit":5}}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 with strict allowlist, got %d body=%s", resp.StatusCode, string(body))
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

func TestStagedRetrievalAppliesLettaTopKByMode(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR", "2.0")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_CAP", "24")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR_FAST", "1.2")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR_BALANCED", "1.6")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR_DEEP", "2.0")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_CAP_FAST", "6")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_CAP_BALANCED", "9")
	t.Setenv("ORCH_RETRIEVAL_LETTA_TOP_K_CAP_DEEP", "15")
	t.Setenv("LETTA_AUTO_SESSION_ID", "gateway-go-test")
	t.Setenv("LETTA_REQUIRE_API_KEY", "false")

	var mu sync.Mutex
	capturedLimitByMode := map[string]int{}
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	letta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents/") && strings.HasSuffix(r.URL.Path, "/archival-memory/search"):
			topK := anyToInt(r.URL.Query().Get("top_k"), 0)
			query := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("query")))
			mode := "balanced"
			if strings.Contains(query, "mode:fast") {
				mode = "fast"
			} else if strings.Contains(query, "mode:deep") {
				mode = "deep"
			}
			mu.Lock()
			capturedLimitByMode[mode] = topK
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":[{"id":"passage-1","timestamp":"2026-03-29T00:00:00Z","content":"project=alpha file=archive.md topic=runbooks/testing\nsummary: letta row"}]}`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents/":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"agent-test"}]`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents/agent-test":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"agent-test","name":"gateway-go-test"}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer letta.Close()
	t.Setenv("LETTA_URL", letta.URL)

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	cases := []struct {
		mode          string
		expectedLimit int
	}{
		{mode: "fast", expectedLimit: 6},
		{mode: "balanced", expectedLimit: 8},
		{mode: "deep", expectedLimit: 10},
	}
	for _, tc := range cases {
		reqBody := `{"request":{"query":"alpha mode:` + tc.mode + `","limit":5,"retrieval_mode":"` + tc.mode + `"}}`
		resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("%s request failed: %v", tc.mode, err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("expected 200 for %s mode, got %d body=%s", tc.mode, resp.StatusCode, string(body))
		}
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode response for %s mode: %v", tc.mode, err)
		}
		resp.Body.Close()
		debug, _ := payload["retrieval_debug"].(map[string]any)
		policy, _ := debug["source_policy"].(map[string]any)
		topKByMode, _ := policy["letta_top_k_by_mode"].(map[string]any)
		if len(topKByMode) == 0 {
			t.Fatalf("expected letta_top_k_by_mode policy block for %s mode", tc.mode)
		}
		if !anyToBool(policy["letta_native_gateway_lane"]) {
			t.Fatalf("expected letta native gateway lane enabled for %s mode", tc.mode)
		}
	}
	if backendCalls != 0 {
		t.Fatalf("expected no python backend subcalls for letta source, got %d", backendCalls)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, tc := range cases {
		if got := capturedLimitByMode[tc.mode]; got != tc.expectedLimit {
			t.Fatalf("expected letta top_k %d for mode %s, got %d", tc.expectedLimit, tc.mode, got)
		}
	}
}

func TestStagedRetrievalLettaConfigDisabledSkipsWithoutPythonFallback(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "letta")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", "false")
	t.Setenv("LETTA_AUTO_SESSION_ID", "gateway-go-test")
	t.Setenv("LETTA_REQUIRE_API_KEY", "true")
	t.Setenv("LETTA_API_KEY", "")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusNotFound)
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
	rows, _ := payload["results"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected empty results when letta config disabled, got %#v", payload["results"])
	}
	if backendCalls != 0 {
		t.Fatalf("expected no python backend calls when letta config disabled, got %d", backendCalls)
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

func TestStagedRetrievalExplicitSourcesRemainFailOpenByDefault(t *testing.T) {
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
			time.Sleep(600 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if source == "topic_rollups" {
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"runbook.md","summary":"explicit source fast row","score":0.92,"source":"topic_rollups"}],"warnings":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"explicit source fallback behavior","limit":5,"retrieval_mode":"balanced","sources":["topic_rollups","mindsdb"]}}`
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
	if !strings.Contains(warnings, "explicit sources requested in staged fail-open mode") {
		t.Fatalf("expected explicit-source staged warning, got %v", payload["warnings"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	staged, _ := debug["staged_fetch"].(map[string]any)
	if fallbackSources := anyToStringSlice(staged["sync_fallback_slow_sources"]); len(fallbackSources) != 0 {
		t.Fatalf("expected no sync fallback sources in explicit fail-open mode, got %v", fallbackSources)
	}
	errorsMap, _ := debug["source_errors"].(map[string]any)
	if _, exists := errorsMap["mindsdb"]; exists {
		t.Fatalf("expected mindsdb to remain async/deferred, got source_errors=%#v", errorsMap["mindsdb"])
	}
	lifecycle, _ := payload["retrieval_lifecycle"].(map[string]any)
	sourcesBlock, _ := lifecycle["sources"].(map[string]any)
	pending := anyToStringSlice(sourcesBlock["pending"])
	if len(pending) != 1 || pending[0] != "mindsdb" {
		t.Fatalf("expected pending sources [mindsdb], got %v", pending)
	}
}

func TestStagedRetrievalTopicRollupsTimeoutIsNonDegradable(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", "0.25")
	t.Setenv("GO_RETRIEVAL_TOPIC_ROLLUP_SYNC_TIMEOUT_FLOOR_SECS", "0.25")
	t.Setenv("GO_RETRIEVAL_NON_DEGRADABLE_SOURCES", "topic_rollups")
	t.Setenv("GO_RETRIEVAL_PROTECTED_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", "true")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path == "/memory/topic-rollups" {
			time.Sleep(450 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"topics":[{"path":"runbooks/profitability","summarySnippets":["selector tuning"],"latestTimestamp":"2026-04-01T00:00:00Z"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"selector tuning","limit":5,"retrieval_mode":"balanced","sources":["topic_rollups"]}}`
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
	if strings.TrimSpace(strings.ToLower(anyToString(payload["result_state"]))) == "degraded" {
		t.Fatalf("topic_rollups timeout must not mark response degraded: %#v", payload)
	}
	if degraded, _ := payload["degraded"].(bool); degraded {
		t.Fatalf("expected degraded=false, got true")
	}

	lifecycle, _ := payload["retrieval_lifecycle"].(map[string]any)
	if strings.TrimSpace(strings.ToLower(anyToString(lifecycle["status"]))) == "failed" {
		t.Fatalf("expected lifecycle status to avoid failed on non-degradable lane timeout, got %#v", lifecycle["status"])
	}

	summary, _ := payload["source_summary"].(map[string]any)
	timedOut := anyToStringSlice(summary["timed_out_sources"])
	if len(timedOut) != 1 || timedOut[0] != "topic_rollups" {
		t.Fatalf("expected timed_out_sources=[topic_rollups], got %v", timedOut)
	}

	continuation, _ := payload["continuation_async"].(map[string]any)
	if continuation == nil {
		t.Fatalf("expected continuation_async for non-degradable timeout lane")
	}
	pending := anyToStringSlice(continuation["pending_sources"])
	if len(pending) == 0 || pending[0] != "topic_rollups" {
		t.Fatalf("expected continuation pending source topic_rollups, got %v", pending)
	}

	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if !strings.Contains(warnings, "non-degradable lane") {
		t.Fatalf("expected non-degradable warning context, got %v", payload["warnings"])
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

func TestStagedRetrievalMemoryBankFallbackChainDebug(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "memory_bank")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "memory_bank")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_MEMORY_BANK_SEARCH_BACKEND", "icm_spike")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_HTTP_URL", "")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_FALLBACK_BACKENDS", "")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_FALLBACK_TO_NATIVE", "true")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_EMPTY_RESULT_FALLBACK", "true")

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
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"native fallback row","score":0.82,"source":"memory_bank","topic_path":"runbooks/testing"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"alpha","project":"alpha","topic_path":"runbooks/testing","limit":5,"retrieval_mode":"balanced"}}`
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
		t.Fatalf("expected fallback native rows, got %#v", payload["results"])
	}
	if got := strings.TrimSpace(anyToString(capturedPolicy["memory_bank_backend"])); got != "native" {
		t.Fatalf("expected fallback native backend policy in backend subcall, got %#v", capturedPolicy)
	}

	debug, _ := payload["retrieval_debug"].(map[string]any)
	policyBlock, _ := debug["source_policy"].(map[string]any)
	fallbackChainAny, ok := policyBlock["memory_bank_fallback_chain"].([]any)
	if !ok || len(fallbackChainAny) == 0 {
		t.Fatalf("expected memory_bank_fallback_chain entries, got %#v", policyBlock["memory_bank_fallback_chain"])
	}
	chainEntry, _ := fallbackChainAny[0].(map[string]any)
	if strings.TrimSpace(anyToString(chainEntry["backend_requested"])) != "icm_spike" {
		t.Fatalf("expected backend_requested=icm_spike, got %#v", chainEntry["backend_requested"])
	}
	steps, _ := chainEntry["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("expected fallback chain steps, got %#v", chainEntry)
	}
	foundFallbackNative := false
	foundNativeSuccess := false
	for _, rawStep := range steps {
		step, _ := rawStep.(map[string]any)
		if strings.TrimSpace(anyToString(step["policy_action"])) == "fallback_native" {
			foundFallbackNative = true
		}
		if strings.TrimSpace(anyToString(step["backend"])) == "native" &&
			strings.TrimSpace(anyToString(step["status"])) == "success" {
			foundNativeSuccess = true
		}
	}
	if !foundFallbackNative || !foundNativeSuccess {
		t.Fatalf("expected fallback_native + native success steps, got %#v", steps)
	}

	sourceChainDebug, _ := policyBlock["source_chain_debug"].(map[string]any)
	if _, ok := sourceChainDebug["memory_bank"]; !ok {
		t.Fatalf("expected source_chain_debug entry for memory_bank, got %#v", sourceChainDebug)
	}
}

func TestStagedRetrievalMemoryBankHedgePolicyDebug(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "memory_bank")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "memory_bank")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_MEMORY_BANK_SEARCH_BACKEND", "shodh_spike")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_FALLBACK_BACKENDS", "surrealdb_spike,memvid_spike")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_HTTP_URL", "")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_FALLBACK_TO_NATIVE", "true")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_EMPTY_RESULT_FALLBACK", "true")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_MAX_CHAIN_BACKENDS", "3")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_HEDGE_ENABLED", "true")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_HEDGE_MAX_PARALLEL", "2")
	t.Setenv("ORCH_MEMORY_BANK_SPIKE_HEDGE_BACKENDS", "shodh_spike,surrealdb_spike")

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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/hedge.md","summary":"native fallback row","score":0.91,"source":"memory_bank","topic_path":"runbooks/testing"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"alpha","project":"alpha","topic_path":"runbooks/testing","limit":5,"retrieval_mode":"balanced"}}`
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
		t.Fatalf("expected fallback native rows, got %#v", payload["results"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	policyBlock, _ := debug["source_policy"].(map[string]any)
	fallbackChainAny, _ := policyBlock["memory_bank_fallback_chain"].([]any)
	if len(fallbackChainAny) == 0 {
		t.Fatalf("expected memory_bank_fallback_chain entries, got %#v", policyBlock["memory_bank_fallback_chain"])
	}
	chainEntry, _ := fallbackChainAny[0].(map[string]any)
	policy, _ := chainEntry["policy"].(map[string]any)
	if enabled, _ := policy["hedge_enabled"].(bool); !enabled {
		t.Fatalf("expected hedge_enabled=true, got %#v", policy["hedge_enabled"])
	}
	hedgeBackends, _ := policy["hedge_backends"].([]any)
	if len(hedgeBackends) < 2 {
		t.Fatalf("expected hedge_backends in policy, got %#v", policy["hedge_backends"])
	}
	if strings.TrimSpace(anyToString(hedgeBackends[0])) != "shodh_spike" ||
		strings.TrimSpace(anyToString(hedgeBackends[1])) != "surrealdb_spike" {
		t.Fatalf("unexpected hedge backend ordering: %#v", hedgeBackends)
	}
	steps, _ := chainEntry["steps"].([]any)
	foundHedgeProbe := false
	foundNativeSuccess := false
	for _, rawStep := range steps {
		step, _ := rawStep.(map[string]any)
		if strings.TrimSpace(anyToString(step["trigger"])) == "hedge_probe" {
			foundHedgeProbe = true
		}
		if strings.TrimSpace(anyToString(step["backend"])) == "native" &&
			strings.TrimSpace(anyToString(step["status"])) == "success" {
			foundNativeSuccess = true
		}
	}
	if !foundHedgeProbe {
		t.Fatalf("expected hedge_probe steps in chain trace, got %#v", steps)
	}
	if !foundNativeSuccess {
		t.Fatalf("expected native success step after hedge fallback, got %#v", steps)
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
	s.recordAdaptiveObservation(sourceLetta, 25*time.Second, false, false, false)
	s.recordAdaptiveObservation(sourceLetta, 30*time.Second, false, false, false)
	s.recordAdaptiveObservation(sourceLetta, 28*time.Second, false, false, false)

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

func TestContinuationPerSourceInflightAndCooldown(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT", "8")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE", "1")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS", "10")

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
	ok, status, _ := s.tryReserveContinuationSourceSlot(sourceLetta)
	if !ok || status != "" {
		t.Fatalf("expected first reservation to pass, got ok=%v status=%q", ok, status)
	}
	ok, status, remaining := s.tryReserveContinuationSourceSlot(sourceLetta)
	if ok || status != "max_inflight_per_source" {
		t.Fatalf("expected per-source cap rejection, got ok=%v status=%q", ok, status)
	}
	if remaining <= 0 {
		t.Fatalf("expected cooldown to be applied after per-source cap hit, got %f", remaining)
	}
	s.releaseContinuationSourceSlot(sourceLetta)
	ok, status, remaining = s.tryReserveContinuationSourceSlot(sourceLetta)
	if ok || status != "cooldown" {
		t.Fatalf("expected cooldown rejection after cap hit, got ok=%v status=%q", ok, status)
	}
	if remaining <= 0 {
		t.Fatalf("expected positive cooldown remaining, got %f", remaining)
	}
}

func TestContinuationPerSourceInflightOverrideByLane(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT", "8")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE", "1")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE_LETTA", "2")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS", "10")

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

	ok, status, _ := s.tryReserveContinuationSourceSlot(sourceLetta)
	if !ok || status != "" {
		t.Fatalf("expected first Letta reservation to pass, got ok=%v status=%q", ok, status)
	}
	ok, status, _ = s.tryReserveContinuationSourceSlot(sourceLetta)
	if !ok || status != "" {
		t.Fatalf("expected second Letta reservation to pass with override, got ok=%v status=%q", ok, status)
	}
	ok, status, _ = s.tryReserveContinuationSourceSlot(sourceLetta)
	if ok || status != "max_inflight_per_source" {
		t.Fatalf("expected third Letta reservation to fail, got ok=%v status=%q", ok, status)
	}
	s.releaseContinuationSourceSlot(sourceLetta)
	s.releaseContinuationSourceSlot(sourceLetta)

	ok, status, _ = s.tryReserveContinuationSourceSlot(sourceMemoryBank)
	if !ok || status != "" {
		t.Fatalf("expected first memory-bank reservation to pass, got ok=%v status=%q", ok, status)
	}
	ok, status, _ = s.tryReserveContinuationSourceSlot(sourceMemoryBank)
	if ok || status != "max_inflight_per_source" {
		t.Fatalf("expected second memory-bank reservation to fail with global cap, got ok=%v status=%q", ok, status)
	}
	s.releaseContinuationSourceSlot(sourceMemoryBank)
}

func TestContinuationPerSourceCooldownOverrideByLane(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT", "8")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE", "1")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS", "10")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS_LETTA", "0")

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
	ok, status, _ := s.tryReserveContinuationSourceSlot(sourceLetta)
	if !ok || status != "" {
		t.Fatalf("expected first Letta reservation to pass, got ok=%v status=%q", ok, status)
	}
	ok, status, remaining := s.tryReserveContinuationSourceSlot(sourceLetta)
	if ok || status != "max_inflight_per_source" {
		t.Fatalf("expected Letta max inflight rejection, got ok=%v status=%q", ok, status)
	}
	if remaining != 0 {
		t.Fatalf("expected zero cooldown remaining with Letta override cooldown=0, got %f", remaining)
	}
	s.releaseContinuationSourceSlot(sourceLetta)
	ok, status, _ = s.tryReserveContinuationSourceSlot(sourceLetta)
	if !ok || status != "" {
		t.Fatalf("expected Letta reservation to recover immediately with cooldown override, got ok=%v status=%q", ok, status)
	}
	s.releaseContinuationSourceSlot(sourceLetta)
}

func TestContinuationSourceFailureAppliesCooldown(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT", "8")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE", "2")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS", "5")

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
	remaining := s.applyContinuationSourceCooldown(sourceMemoryBank)
	if remaining <= 0 {
		t.Fatalf("expected positive cooldown after failure, got %f", remaining)
	}
	ok, status, _ := s.tryReserveContinuationSourceSlot(sourceMemoryBank)
	if ok || status != "cooldown" {
		t.Fatalf("expected cooldown gate, got ok=%v status=%q", ok, status)
	}
	s.continuationMu.Lock()
	s.continuationSourceCooldownUntil[sourceMemoryBank] = time.Now().UTC().Add(-1 * time.Second)
	s.continuationMu.Unlock()
	ok, status, _ = s.tryReserveContinuationSourceSlot(sourceMemoryBank)
	if !ok || status != "" {
		t.Fatalf("expected reservation to recover after cooldown expiry, got ok=%v status=%q", ok, status)
	}
	s.releaseContinuationSourceSlot(sourceMemoryBank)
}
