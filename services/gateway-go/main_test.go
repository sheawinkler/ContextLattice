package main

import (
	"bufio"
	"context"
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
	if os.Getenv("GO_TOKEN_IMPACT_LEDGER_ENABLED") == "" {
		t.Setenv("GO_TOKEN_IMPACT_LEDGER_ENABLED", "false")
	}
	if os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED") == "" {
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "false")
	}
	if os.Getenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED") == "" {
		t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", "false")
	}
	if os.Getenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED") == "" {
		t.Setenv("CONTEXTLATTICE_CONTEXT_POLICY_ENABLED", "false")
	}
	if os.Getenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED") == "" {
		t.Setenv("CONTEXTLATTICE_SKILL_FOUNDRY_ENABLED", "false")
	}
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

func TestStrictRuntimeStatusDoesNotWarnOnHealthyAsyncContinuationAge(t *testing.T) {
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_TIMEOUT_SECS", "45")
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	}
	s := newServer()
	s.continuationMu.Lock()
	s.continuationInFlight[sourceLetta] = 1
	s.continuationInFlightStarted[sourceLetta] = []time.Time{time.Now().UTC().Add(-10 * time.Second)}
	s.continuationMu.Unlock()
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
	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if strings.Contains(warnings, "continuation queue age is elevated") {
		t.Fatalf("did not expect async continuation age warning below backlog/stale thresholds, got %v", payload["warnings"])
	}
}

func TestMemoryWriteQueuesPgvectorFanoutAsync(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_WRITE_PGVECTOR_FANOUT_MODE", "async")
	t.Setenv("GO_WRITE_PGVECTOR_FANOUT_TIMEOUT_SECS", "1")
	t.Setenv("ORCH_PGVECTOR_ENABLED", "true")
	t.Setenv("ORCH_PGVECTOR_FANOUT_ENABLED", "true")
	t.Setenv("ORCH_PGVECTOR_DSN", "postgresql://postgres:postgres@127.0.0.1:1/contextlattice?sslmode=disable")
	t.Setenv("ORCH_PGVECTOR_CONNECT_TIMEOUT_SECS", "1")
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	}
	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(
		gateway.URL+"/memory/write",
		"application/json",
		strings.NewReader(`{"projectName":"alpha","fileName":"notes/async-pgvector.md","content":"hello async pgvector","topicPath":"runbooks/testing"}`),
	)
	if err != nil {
		t.Fatalf("memory/write request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode write payload: %v", err)
	}
	fanout, ok := payload["fanout"].(map[string]any)
	if !ok {
		t.Fatalf("expected fanout payload, got %#v", payload["fanout"])
	}
	if got := anyToString(fanout["postgres_pgvector"]); got != "queued_async" {
		t.Fatalf("expected postgres_pgvector queued_async, got %q payload=%#v", got, payload)
	}
}

func TestMemoryWriteSyncsQdrantFanout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_WRITE_QDRANT_FANOUT_MODE", "sync")
	t.Setenv("GO_WRITE_QDRANT_FANOUT_TIMEOUT_SECS", "2")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("QDRANT_API_KEY", "test-qdrant-key")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("ORCH_PGVECTOR_ENABLED", "false")
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	}
	createCalls := 0
	upsertCalls := 0
	var upsertPayload map[string]any
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "test-qdrant-key" {
			t.Fatalf("expected qdrant api-key header, got %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection contextlattice_notes"}}`))
			return
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes":
			createCalls += 1
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode qdrant collection create payload: %v", err)
			}
			vectors, _ := payload["vectors"].(map[string]any)
			if int(anyToFloat(vectors["size"])) != 768 {
				t.Fatalf("expected qdrant collection size 768, got %#v", vectors["size"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":true}`))
			return
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes/points":
			upsertCalls += 1
			if r.URL.Query().Get("wait") != "true" {
				t.Fatalf("expected wait=true on qdrant upsert, got raw query %q", r.URL.RawQuery)
			}
			if err := json.NewDecoder(r.Body).Decode(&upsertPayload); err != nil {
				t.Fatalf("decode qdrant upsert payload: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":{"operation_id":1,"status":"completed"}}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer qdrant.Close()
	t.Setenv("QDRANT_URL", qdrant.URL)

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(
		gateway.URL+"/memory/write",
		"application/json",
		strings.NewReader(`{"projectName":"alpha","fileName":"notes/qdrant.md","content":"hello qdrant native fanout","topicPath":"runbooks/testing"}`),
	)
	if err != nil {
		t.Fatalf("memory/write request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode write payload: %v", err)
	}
	fanout, ok := payload["fanout"].(map[string]any)
	if !ok {
		t.Fatalf("expected fanout payload, got %#v", payload["fanout"])
	}
	if got := anyToString(fanout["qdrant"]); got != "succeeded" {
		t.Fatalf("expected qdrant fanout succeeded, got %q payload=%#v", got, payload)
	}
	if createCalls != 1 {
		t.Fatalf("expected one qdrant collection create call, got %d", createCalls)
	}
	if upsertCalls != 1 {
		t.Fatalf("expected one qdrant upsert call, got %d", upsertCalls)
	}
	points, _ := upsertPayload["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("expected one qdrant point, got %#v", upsertPayload["points"])
	}
	point, _ := points[0].(map[string]any)
	if id := anyToString(point["id"]); len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("expected deterministic UUID-like qdrant id, got %#v", point["id"])
	}
	vector, _ := point["vector"].([]any)
	if len(vector) != 768 {
		t.Fatalf("expected qdrant vector length 768, got %d", len(vector))
	}
	pointPayload, _ := point["payload"].(map[string]any)
	if got := anyToString(pointPayload["project"]); got != "alpha" {
		t.Fatalf("expected qdrant payload project alpha, got %#v", pointPayload["project"])
	}
	tags, _ := pointPayload["topic_tags"].([]any)
	tagText := make([]string, 0, len(tags))
	for _, tag := range tags {
		tagText = append(tagText, anyToString(tag))
	}
	if strings.Join(tagText, ",") != "runbooks,runbooks/testing" {
		t.Fatalf("expected hierarchical qdrant topic tags, got %#v", pointPayload["topic_tags"])
	}
}

func TestDreamModePersistSyncsQdrantFanout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_WRITE_QDRANT_FANOUT_MODE", "sync")
	t.Setenv("GO_WRITE_QDRANT_FANOUT_TIMEOUT_SECS", "2")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("ORCH_PGVECTOR_ENABLED", "false")
	t.Setenv("GO_DREAM_LLM_ENABLED", "true")
	if !envBool("GO_GATEWAY_TEST_KEEP_ORCH_KEY", false) {
		t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	}
	searchCalls := 0
	upsertCalls := 0
	var upsertPayload map[string]any
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/collections/contextlattice_notes":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/collections/contextlattice_notes/points/search":
			searchCalls += 1
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":[{"score":0.91,"payload":{"project":"alpha","file":"notes/evidence.md","summary":"qdrant evidence for dream persistence","topic_path":"runbooks/testing","created_at":"2026-06-18T00:00:00Z"}}]}`))
			return
		case r.Method == http.MethodPut && r.URL.Path == "/collections/contextlattice_notes/points":
			upsertCalls += 1
			if r.URL.Query().Get("wait") != "true" {
				t.Fatalf("expected wait=true on qdrant upsert, got raw query %q", r.URL.RawQuery)
			}
			if err := json.NewDecoder(r.Body).Decode(&upsertPayload); err != nil {
				t.Fatalf("decode qdrant upsert payload: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result":{"operation_id":1,"status":"completed"}}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))
	defer qdrant.Close()
	t.Setenv("BACKEND_URL", qdrant.URL)
	t.Setenv("QDRANT_URL", qdrant.URL)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"content":"{\"hypotheses\":[{\"title\":\"Use qdrant writeback as Dream Mode's durable proof\",\"claim\":\"A successful qdrant writeback proves Dream Mode can persist LLM synthesis without pgvector.\",\"supporting_evidence\":[\"e1\"],\"experiment\":\"Run Dream Mode with sync qdrant fanout and assert the durable point lands in qdrant.\",\"expected_signal\":\"The Dream response is persisted and qdrant fanout reports succeeded.\"}],\"experiments\":[],\"next_best_action\":\"keep qdrant as the Lite durable vector path\"}"}}`))
	}))
	defer llmServer.Close()

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(
		gateway.URL+"/memory/dream",
		"application/json",
		strings.NewReader(`{"project":"alpha","goal":"prove qdrant dream writeback","topic_path":"runbooks/testing","retrieval_mode":"fast","use_llm":true,"provider":"ollama","base_url":"`+llmServer.URL+`","model":"dream-test","persist":true}`),
	)
	if err != nil {
		t.Fatalf("memory/dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream payload: %v", err)
	}
	if source := strings.TrimSpace(anyToString(payload["intelligence_source"])); source != "llm_synthesis" {
		t.Fatalf("expected llm_synthesis intelligence source, got %q payload=%#v", source, payload)
	}
	if !anyToBool(payload["persisted"]) {
		t.Fatalf("expected persisted=true, got %#v", payload)
	}
	writeback, _ := payload["writeback"].(map[string]any)
	if !anyToBool(writeback["ok"]) {
		t.Fatalf("expected writeback ok=true, got %#v", writeback)
	}
	fanout, ok := writeback["fanout"].(map[string]any)
	if !ok {
		t.Fatalf("expected writeback fanout payload, got %#v", writeback["fanout"])
	}
	if got := anyToString(fanout["qdrant"]); got != "succeeded" {
		t.Fatalf("expected qdrant fanout succeeded, got %q writeback=%#v", got, writeback)
	}
	if got := anyToString(fanout["postgres_pgvector"]); got != "skipped_source_disabled" {
		t.Fatalf("expected pgvector skipped_source_disabled for Lite-style tool write, got %q fanout=%#v", got, fanout)
	}
	if searchCalls != 1 {
		t.Fatalf("expected one qdrant search call before writeback, got %d", searchCalls)
	}
	if upsertCalls != 1 {
		t.Fatalf("expected one qdrant upsert call, got %d", upsertCalls)
	}
	points, _ := upsertPayload["points"].([]any)
	if len(points) != 1 {
		t.Fatalf("expected one qdrant point, got %#v", upsertPayload["points"])
	}
	point, _ := points[0].(map[string]any)
	pointPayload, _ := point["payload"].(map[string]any)
	if got := anyToString(pointPayload["lifecycle"]); got != "durable" {
		t.Fatalf("expected qdrant payload lifecycle=durable, got %#v", pointPayload["lifecycle"])
	}
	if got := anyToString(writeback["writeback_source"]); got != "dream_mode" {
		t.Fatalf("expected dream_mode writeback source, got %#v", writeback["writeback_source"])
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
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")

	var calledPath string
	var retrievalRequestBody string
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
		raw, _ := io.ReadAll(r.Body)
		retrievalRequestBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"alpha summary","score":0.93}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"query":"alpha","limit":5,"include_grounding":true,"agent_id":"codex_gpt5","objective":"ship premium launch","goal":"increase subscriber conversion","mission":"compound knowledge over time"}`
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
	objectiveContext, ok := payload["objective_context"].(map[string]any)
	if !ok {
		t.Fatalf("expected objective_context payload, got %#v", payload["objective_context"])
	}
	if strings.TrimSpace(anyToString(objectiveContext["objective"])) != "ship premium launch" {
		t.Fatalf("expected objective_context.objective to propagate request objective, got %#v", objectiveContext["objective"])
	}
	objectiveCapture, ok := payload["objective_context_capture"].(map[string]any)
	if !ok {
		t.Fatalf("expected objective_context_capture payload, got %#v", payload["objective_context_capture"])
	}
	if strings.TrimSpace(anyToString(objectiveCapture["reason"])) != "memory_store_unavailable" {
		t.Fatalf("expected objective context capture to explain skipped write in tests, got %#v", objectiveCapture)
	}
	grounding, ok := payload["grounding"].(map[string]any)
	if !ok || !anyToBool(grounding["strict_numeric_copy"]) {
		t.Fatalf("expected grounding.strict_numeric_copy=true, got %#v", payload["grounding"])
	}
	if !strings.Contains(retrievalRequestBody, `"objective_context"`) {
		t.Fatalf("expected retrieval request to include objective_context, got %s", retrievalRequestBody)
	}
	if !strings.Contains(retrievalRequestBody, `"query_expansion"`) {
		t.Fatalf("expected retrieval request to include query_expansion setting, got %s", retrievalRequestBody)
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
		`mux.HandleFunc("/memory/synthesis-pack", s.memorySynthesisPack)`,
		`mux.HandleFunc("/memory/synthesis-pack/v2", s.memorySynthesisPackV2)`,
		`mux.HandleFunc("/memory/retrieval/plan", s.memoryRetrievalPlan)`,
		`mux.HandleFunc("/memory/claims", s.memoryClaimsWrite)`,
		`mux.HandleFunc("/memory/claims/query", s.memoryClaimsQuery)`,
		`mux.HandleFunc("/memory/review", s.memoryReview)`,
		`mux.HandleFunc("/preferences", s.preferencesRoute)`,
		`mux.HandleFunc("/feedback", s.feedbackRoute)`,
		`mux.HandleFunc("/tools/feedback_submit", s.toolsFeedbackSubmit)`,
		`mux.HandleFunc("/tools/synthesis_pack", s.toolsSynthesisPack)`,
		`mux.HandleFunc("/tools/synthesis_pack_v2", s.toolsSynthesisPackV2)`,
		`mux.HandleFunc("/tools/retrieval_plan", s.toolsRetrievalPlan)`,
		`mux.HandleFunc("/tools/claim_write", s.toolsClaimWrite)`,
		`mux.HandleFunc("/tools/claim_query", s.toolsClaimQuery)`,
		`mux.HandleFunc("/memory/context-policy/candidate", s.memoryContextPolicyCandidate)`,
		`mux.HandleFunc("/memory/context-policy/evaluate", s.memoryContextPolicyEvaluate)`,
		`mux.HandleFunc("/tools/context_policy_candidate", s.toolsContextPolicyCandidate)`,
		`mux.HandleFunc("/tools/context_policy_evaluate", s.toolsContextPolicyEvaluate)`,
		`mux.HandleFunc("/telemetry/context-policy", s.telemetryContextPolicy)`,
		`mux.HandleFunc("/memory/skills/foundry/draft", s.memorySkillFoundryDraft)`,
		`mux.HandleFunc("/memory/skills/foundry/evaluate", s.memorySkillFoundryEvaluate)`,
		`mux.HandleFunc("/memory/skills/foundry/export", s.memorySkillFoundryExport)`,
		`mux.HandleFunc("/tools/skill_foundry_draft", s.toolsSkillFoundryDraft)`,
		`mux.HandleFunc("/tools/skill_foundry_evaluate", s.toolsSkillFoundryEvaluate)`,
		`mux.HandleFunc("/tools/skill_foundry_export", s.toolsSkillFoundryExport)`,
		`mux.HandleFunc("/telemetry/skills/foundry", s.telemetrySkillFoundry)`,
		`mux.HandleFunc("/agents/tasks", s.agentsTasksRoute)`,
		`mux.HandleFunc("/agents/tasks/", s.agentsTasksRoute)`,
		`mux.HandleFunc("/telemetry/metrics", s.telemetryMetricsRoute)`,
		`mux.HandleFunc("/telemetry/token-impact", s.telemetryTokenImpactRoute)`,
		`mux.HandleFunc("/telemetry/context-pack-quality", s.telemetryContextPackQualityRoute)`,
		`mux.HandleFunc("/telemetry/context-pack-quality/outcome", s.telemetryContextPackQualityOutcomeRoute)`,
		`mux.HandleFunc("/telemetry/claim-graph", s.telemetryClaimGraph)`,
		`mux.HandleFunc("/telemetry/runner-quality", s.telemetryRunnerQualityRoute)`,
		`mux.HandleFunc("/telemetry/retrieval", s.telemetryRetrievalRoute)`,
		`mux.HandleFunc("/telemetry/retrieval/source-quality", s.telemetryRetrievalSourceQualityRoute)`,
		`mux.HandleFunc("/telemetry/fanout", s.telemetryFanoutRoute)`,
		`mux.HandleFunc("/telemetry/memory", s.telemetryMemoryRoute)`,
		`mux.HandleFunc("/telemetry/memory/graph", s.telemetryMemoryGraphRoute)`,
		`mux.HandleFunc("/telemetry/sidecar-health", s.telemetrySidecarHealthRoute)`,
		`mux.HandleFunc("/telemetry/strategies", s.telemetryStrategiesRoute)`,
		`mux.HandleFunc("/telemetry/strategies/history", s.telemetryStrategiesHistoryRoute)`,
		`mux.HandleFunc("/telemetry/agent-contracts", s.agentContractTelemetryRoute)`,
		`mux.HandleFunc("/telemetry/recall", s.telemetryRecallRoute)`,
		`mux.HandleFunc("/telemetry/recall/monitor", s.telemetryRecallMonitorRoute)`,
		`mux.HandleFunc("/telemetry/tools/invocations", s.telemetryToolsInvocationsRoute)`,
		`mux.HandleFunc("/telemetry/trading", s.telemetryTradingRoute)`,
		`mux.HandleFunc("/telemetry/trading/history", s.telemetryTradingHistoryRoute)`,
		`mux.HandleFunc("/telemetry/", s.telemetryRoute)`,
		`mux.HandleFunc("/maintenance/memory/graph/prune-volatile", s.maintenanceMemoryGraphPruneVolatile)`,
		`mux.HandleFunc("/maintenance/", s.maintenanceRoute)`,
		`mux.HandleFunc("/ops/context-boundary", s.opsContextBoundary)`,
		`mux.HandleFunc("/ops/native-ownership", s.opsNativeOwnership)`,
		`mux.HandleFunc("/v1/retrieval/query", s.retrievalQuery)`,
		`mux.HandleFunc("/v1/retrieval/query-with-grounding", s.retrievalQueryWithGrounding)`,
		`mux.HandleFunc("/v1/retrieval/batch-query", s.retrievalBatchQuery)`,
		`mux.HandleFunc("/v1/skills/quarantine/search", s.skillsQuarantineSearchRoute)`,
		`mux.HandleFunc("/v1/skills/quarantine/reindex", s.skillsQuarantineReindexRoute)`,
		`mux.HandleFunc("/v1/skills/index/search", s.skillsIndexSearchRoute)`,
		`mux.HandleFunc("/v1/skills/index/reindex", s.skillsIndexReindexRoute)`,
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
		`mux.HandleFunc("/memory/synthesis-pack", s.proxy)`,
		`mux.HandleFunc("/memory/synthesis-pack/v2", s.proxy)`,
		`mux.HandleFunc("/memory/retrieval/plan", s.proxy)`,
		`mux.HandleFunc("/memory/claims", s.proxy)`,
		`mux.HandleFunc("/memory/claims/query", s.proxy)`,
		`mux.HandleFunc("/memory/review", s.proxy)`,
		`mux.HandleFunc("/preferences", s.proxy)`,
		`mux.HandleFunc("/feedback", s.proxy)`,
		`mux.HandleFunc("/tools/feedback_submit", s.proxy)`,
		`mux.HandleFunc("/tools/synthesis_pack", s.proxy)`,
		`mux.HandleFunc("/tools/synthesis_pack_v2", s.proxy)`,
		`mux.HandleFunc("/tools/retrieval_plan", s.proxy)`,
		`mux.HandleFunc("/tools/claim_write", s.proxy)`,
		`mux.HandleFunc("/tools/claim_query", s.proxy)`,
		`mux.HandleFunc("/memory/context-policy/candidate", s.proxy)`,
		`mux.HandleFunc("/memory/context-policy/evaluate", s.proxy)`,
		`mux.HandleFunc("/tools/context_policy_candidate", s.proxy)`,
		`mux.HandleFunc("/tools/context_policy_evaluate", s.proxy)`,
		`mux.HandleFunc("/telemetry/context-policy", s.proxy)`,
		`mux.HandleFunc("/memory/skills/foundry/draft", s.proxy)`,
		`mux.HandleFunc("/memory/skills/foundry/evaluate", s.proxy)`,
		`mux.HandleFunc("/memory/skills/foundry/export", s.proxy)`,
		`mux.HandleFunc("/tools/skill_foundry_draft", s.proxy)`,
		`mux.HandleFunc("/tools/skill_foundry_evaluate", s.proxy)`,
		`mux.HandleFunc("/tools/skill_foundry_export", s.proxy)`,
		`mux.HandleFunc("/telemetry/skills/foundry", s.proxy)`,
		`mux.HandleFunc("/agents/tasks", s.proxy)`,
		`mux.HandleFunc("/agents/tasks/", s.proxy)`,
		`mux.HandleFunc("/telemetry/metrics", s.proxy)`,
		`mux.HandleFunc("/telemetry/token-impact", s.proxy)`,
		`mux.HandleFunc("/telemetry/context-pack-quality", s.proxy)`,
		`mux.HandleFunc("/telemetry/context-pack-quality/outcome", s.proxy)`,
		`mux.HandleFunc("/telemetry/claim-graph", s.proxy)`,
		`mux.HandleFunc("/telemetry/runner-quality", s.proxy)`,
		`mux.HandleFunc("/telemetry/retrieval", s.proxy)`,
		`mux.HandleFunc("/telemetry/retrieval/source-quality", s.proxy)`,
		`mux.HandleFunc("/telemetry/fanout", s.proxy)`,
		`mux.HandleFunc("/telemetry/memory", s.proxy)`,
		`mux.HandleFunc("/telemetry/memory/graph", s.proxy)`,
		`mux.HandleFunc("/telemetry/sidecar-health", s.proxy)`,
		`mux.HandleFunc("/telemetry/strategies", s.proxy)`,
		`mux.HandleFunc("/telemetry/strategies/history", s.proxy)`,
		`mux.HandleFunc("/telemetry/recall", s.proxy)`,
		`mux.HandleFunc("/telemetry/recall/monitor", s.proxy)`,
		`mux.HandleFunc("/telemetry/tools/invocations", s.proxy)`,
		`mux.HandleFunc("/telemetry/trading", s.proxy)`,
		`mux.HandleFunc("/telemetry/trading/history", s.proxy)`,
		`mux.HandleFunc("/telemetry/", s.proxy)`,
		`mux.HandleFunc("/maintenance/", s.proxy)`,
		`mux.HandleFunc("/ops/context-boundary", s.proxy)`,
		`mux.HandleFunc("/ops/native-ownership", s.proxy)`,
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

func TestToolsFeedbackSubmitNativeSemanticParity(t *testing.T) {
	t.Setenv("FEEDBACK_HISTORY_PATH", filepath.Join(t.TempDir(), "feedback.ndjson"))
	t.Setenv("LEARNING_LOOP_ENABLED", "true")
	t.Setenv("PREFERENCE_MAX_ENTRIES", "10")
	t.Setenv("FEEDBACK_SUBMIT_IDEMPOTENCY_TTL_SECS", "900")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"proxied":true}`))
	}))
	defer backend.Close()
	gateway := httptest.NewServer(buildMux(newTestServer(t, backend.URL)))
	defer gateway.Close()

	firstResp, err := http.Post(
		gateway.URL+"/tools/feedback_submit",
		"application/json",
		strings.NewReader(`{"project":"alpha","user_id":"u1","content":"Excellent scoped recall","rating":5,"sentiment":"good","tags":["Quality","quality","retrieval"],"topic_path":"runbooks/feedback","idempotencyKey":"idem-1"}`),
	)
	if err != nil {
		t.Fatalf("submit feedback: %v", err)
	}
	defer firstResp.Body.Close()
	if firstResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(firstResp.Body)
		t.Fatalf("expected first submit 200 got %d body=%s", firstResp.StatusCode, string(body))
	}
	var firstPayload map[string]any
	if err := json.NewDecoder(firstResp.Body).Decode(&firstPayload); err != nil {
		t.Fatalf("decode first feedback response: %v", err)
	}
	if backendCalls != 0 {
		t.Fatalf("expected /tools/feedback_submit to stay Go-native, backend calls=%d", backendCalls)
	}
	feedback := anyMap(firstPayload["feedback"])
	if anyToString(feedback["source"]) != "agent" {
		t.Fatalf("expected default source agent, got %#v", feedback)
	}
	if anyToString(feedback["sentiment"]) != "positive" {
		t.Fatalf("expected sentiment alias to normalize to positive, got %#v", feedback)
	}
	tags := anyToStringSlice(feedback["tags"])
	if len(tags) != 2 || tags[0] != "quality" || tags[1] != "retrieval" {
		t.Fatalf("expected deduped normalized tags, got %#v", feedback["tags"])
	}
	learning := anyMap(firstPayload["learning"])
	if !anyToBool(learning["enabled"]) || !anyToBool(learning["preferenceUpdated"]) || anyToBool(learning["memoryIndexed"]) {
		t.Fatalf("unexpected learning payload: %#v", learning)
	}
	preferences := anyMap(firstPayload["preferences"])
	if anyToInt(preferences["total"], 0) != 1 || len(anyToStringSlice(preferences["positive"])) != 1 {
		t.Fatalf("expected one positive preference, got %#v", preferences)
	}

	replayResp, err := http.Post(
		gateway.URL+"/tools/feedback_submit",
		"application/json",
		strings.NewReader(`{"project":"alpha","user_id":"u1","content":"Excellent scoped recall","rating":5,"sentiment":"good","tags":["Quality","quality","retrieval"],"topic_path":"runbooks/feedback","idempotencyKey":"idem-1"}`),
	)
	if err != nil {
		t.Fatalf("replay feedback: %v", err)
	}
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(replayResp.Body)
		t.Fatalf("expected replay 200 got %d body=%s", replayResp.StatusCode, string(body))
	}
	var replayPayload map[string]any
	if err := json.NewDecoder(replayResp.Body).Decode(&replayPayload); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if !anyToBool(replayPayload["idempotentReplay"]) || anyToString(replayPayload["idempotencyScope"]) != "request" {
		t.Fatalf("expected idempotent replay marker, got %#v", replayPayload)
	}
	if anyToString(anyMap(replayPayload["feedback"])["id"]) != anyToString(feedback["id"]) {
		t.Fatalf("expected replay to return original feedback id")
	}

	conflictResp, err := http.Post(
		gateway.URL+"/tools/feedback_submit",
		"application/json",
		strings.NewReader(`{"project":"alpha","content":"Different payload","rating":4,"idempotencyKey":"idem-1"}`),
	)
	if err != nil {
		t.Fatalf("conflict feedback: %v", err)
	}
	defer conflictResp.Body.Close()
	if conflictResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(conflictResp.Body)
		t.Fatalf("expected idempotency conflict 409 got %d body=%s", conflictResp.StatusCode, string(body))
	}

	malformedResp, err := http.Post(
		gateway.URL+"/tools/feedback_submit",
		"application/json",
		strings.NewReader(`{"project":"alpha","tags":["bad tag"],"content":"invalid tag shape"}`),
	)
	if err != nil {
		t.Fatalf("malformed feedback: %v", err)
	}
	defer malformedResp.Body.Close()
	if malformedResp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(malformedResp.Body)
		t.Fatalf("expected malformed tag 422 got %d body=%s", malformedResp.StatusCode, string(body))
	}

	preferencesResp, err := http.Get(gateway.URL + "/preferences?project=alpha&user_id=u1")
	if err != nil {
		t.Fatalf("preferences request: %v", err)
	}
	defer preferencesResp.Body.Close()
	if preferencesResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(preferencesResp.Body)
		t.Fatalf("expected preferences 200 got %d body=%s", preferencesResp.StatusCode, string(body))
	}
	var preferencesPayload map[string]any
	if err := json.NewDecoder(preferencesResp.Body).Decode(&preferencesPayload); err != nil {
		t.Fatalf("decode preferences: %v", err)
	}
	if !anyToBool(preferencesPayload["enabled"]) || anyToInt(anyMap(preferencesPayload["preferences"])["total"], 0) != 1 {
		t.Fatalf("expected enabled preference context, got %#v", preferencesPayload)
	}

	memoryResp, err := http.Get(gateway.URL + "/telemetry/memory")
	if err != nil {
		t.Fatalf("memory telemetry: %v", err)
	}
	defer memoryResp.Body.Close()
	if memoryResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(memoryResp.Body)
		t.Fatalf("expected memory telemetry 200 got %d body=%s", memoryResp.StatusCode, string(body))
	}
	var memoryPayload map[string]any
	if err := json.NewDecoder(memoryResp.Body).Decode(&memoryPayload); err != nil {
		t.Fatalf("decode memory telemetry: %v", err)
	}
	feedbackSubmit := anyMap(memoryPayload["feedbackSubmit"])
	metrics := anyMap(feedbackSubmit["metrics"])
	if anyToInt(metrics["accepted"], 0) != 1 || anyToInt(metrics["idempotentHits"], 0) != 1 || anyToInt(metrics["rejected"], 0) != 2 {
		t.Fatalf("unexpected feedback metrics: %#v", metrics)
	}
	if backendCalls != 0 {
		t.Fatalf("expected no backend proxy calls after all feedback checks, got %d", backendCalls)
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

func TestMemoryRecallEvaluateSavedIsGoNative(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")

	recallCasesPath := filepath.Join(t.TempDir(), "recall_eval_cases.json")
	recallMonitorPath := filepath.Join(t.TempDir(), "recall_monitor.ndjson")
	if err := os.WriteFile(
		recallCasesPath,
		[]byte(`{
  "version": 1,
  "updatedAt": "2026-04-28T00:00:00Z",
  "k": 5,
  "gate": {"minRecallAtK": 0.5, "minMrr": 0.5, "minNumericExactness": 0.0},
  "cases": [
    {
      "id": "native-go-route",
      "query": "alpha",
      "limit": 5,
      "project": "alpha",
      "topic_path": "runbooks/testing",
      "sources": ["qdrant"],
      "expected_files": ["notes/alpha.md"],
      "expected_substrings": ["alpha"]
    }
  ]
}`),
		0o644,
	); err != nil {
		t.Fatalf("write saved recall eval config: %v", err)
	}
	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", recallCasesPath)
	t.Setenv("RECALL_MONITOR_PATH", recallMonitorPath)

	var capturedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/retrieval/query":
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/alpha.md","summary":"alpha topic","source":"qdrant","score":0.92}],"warnings":[]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	req, err := http.NewRequest(http.MethodPost, gateway.URL+"/memory/recall/evaluate/saved", strings.NewReader(`{"include_retrieval_debug":true}`))
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
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response payload: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=true, got %#v", payload)
	}
	if !anyToBool(payload["passed"]) {
		t.Fatalf("expected passed=true, got %#v", payload)
	}
	metrics, _ := payload["metrics"].(map[string]any)
	if anyToInt(metrics["casesEvaluated"], 0) != 1 {
		t.Fatalf("expected one evaluated case, got %#v", metrics)
	}
	if anyToFloat64(metrics["citationCoverage"], 0) != 1 {
		t.Fatalf("expected citation coverage default 1.0, got %#v", metrics)
	}
	if anyToFloat64(metrics["sourceDiversity"], 0) != 1 {
		t.Fatalf("expected one source in diversity metric, got %#v", metrics)
	}
	if anyToFloat64(metrics["p95LatencyMs"], -1) < 0 {
		t.Fatalf("expected p95 latency metric, got %#v", metrics)
	}
	graphContribution, _ := metrics["graphContribution"].(map[string]any)
	if anyToBool(graphContribution["memoryGraphStoreActive"]) {
		t.Fatalf("expected disabled graph store in native route smoke, got %#v", graphContribution)
	}
	cases, _ := payload["cases"].([]any)
	if len(cases) != 1 {
		t.Fatalf("expected one case report, got %#v", payload["cases"])
	}
	caseReport, _ := cases[0].(map[string]any)
	if _, ok := caseReport["graph_contribution"].(map[string]any); !ok {
		t.Fatalf("expected case graph contribution, got %#v", caseReport)
	}
	monitorRaw, err := os.ReadFile(recallMonitorPath)
	if err != nil {
		t.Fatalf("expected recall monitor sample: %v", err)
	}
	if !strings.Contains(string(monitorRaw), `"recallAtK"`) {
		t.Fatalf("expected recall monitor sample to include eval metrics, got %s", string(monitorRaw))
	}
	if capturedPath != "/v1/retrieval/query" {
		t.Fatalf("expected go-native route to call retrieval query path, got %s", capturedPath)
	}
}

func TestMemoryRecallEvaluateSavedFailsFastForUnhealthyCaseSet(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")

	recallCasesPath := filepath.Join(t.TempDir(), "recall_eval_cases.json")
	recallMonitorPath := filepath.Join(t.TempDir(), "recall_monitor.ndjson")
	if err := os.WriteFile(
		recallCasesPath,
		[]byte(`{
  "version": 1,
  "updatedAt": "2026-04-28T00:00:00Z",
  "k": 5,
  "gate": {"minRecallAtK": 0.75, "minMrr": 0.55, "minNumericExactness": 0.9},
  "cases": [
    {
      "id": "health-surface",
      "query": "root",
      "topic_path": "root",
      "limit": 10,
      "expected_files": ["notes/a.md", "notes/b.md"]
    }
  ]
}`),
		0o644,
	); err != nil {
		t.Fatalf("write unhealthy saved recall eval config: %v", err)
	}
	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", recallCasesPath)
	t.Setenv("RECALL_MONITOR_PATH", recallMonitorPath)

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/a.md","summary":"alpha","source":"qdrant","score":0.92}]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/memory/recall/evaluate/saved", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 fail-fast payload, got %d body=%s", resp.StatusCode, string(body))
	}
	if backendCalls != 0 {
		t.Fatalf("expected fail-fast validation to avoid retrieval, got backend calls=%d", backendCalls)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response payload: %v", err)
	}
	if anyToBool(payload["ok"]) || anyToBool(payload["passed"]) || !anyToBool(payload["failed_fast"]) {
		t.Fatalf("expected failed_fast invalid payload, got %#v", payload)
	}
	if anyToString(payload["quality_status"]) != "case_set_invalid" {
		t.Fatalf("expected case_set_invalid status, got %#v", payload)
	}
	health, _ := payload["case_set_health"].(map[string]any)
	if anyToBool(health["valid"]) || anyToInt(health["issue_count"], 0) < 4 {
		t.Fatalf("expected multiple case-set health issues, got %#v", health)
	}
	instructions := anyToStringSlice(payload["agent_instructions"])
	if len(instructions) == 0 || !strings.Contains(strings.Join(instructions, " "), "/memory/write") {
		t.Fatalf("expected agent remediation instructions, got %#v", payload["agent_instructions"])
	}
	if !strings.Contains(strings.Join(instructions, " "), "/memory/recall/eval-cases/refresh") {
		t.Fatalf("expected refresh remediation instruction, got %#v", payload["agent_instructions"])
	}
	monitorRaw, err := os.ReadFile(recallMonitorPath)
	if err != nil {
		t.Fatalf("expected recall monitor fail-fast sample: %v", err)
	}
	if !strings.Contains(string(monitorRaw), `"failedFast":true`) || !strings.Contains(string(monitorRaw), `"case_set_invalid"`) {
		t.Fatalf("expected fail-fast monitor sample, got %s", string(monitorRaw))
	}
}

func TestMemoryRecallEvaluateSavedScoresGraphContribution(t *testing.T) {
	t.Setenv("BACKEND_URL", "http://127.0.0.1:1")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "false")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))
	t.Setenv("RECALL_MONITOR_PATH", filepath.Join(root, "_contextlattice", "recall_monitor.ndjson"))
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")

	recallCasesPath := filepath.Join(t.TempDir(), "recall_eval_cases.json")
	if err := os.WriteFile(
		recallCasesPath,
		[]byte(`{
  "version": 1,
  "updatedAt": "2026-04-28T00:00:00Z",
  "k": 3,
  "gate": {"minRecallAtK": 0.0, "minMrr": 0.0, "minNumericExactness": 0.0},
  "cases": [
    {
      "id": "graph-lift",
      "query": "target by neighbor",
      "limit": 3,
      "project": "alpha",
      "topic_path": "recall/graph",
      "sources": ["qdrant"],
      "expected_files": ["notes/target.md"]
    }
  ]
}`),
		0o644,
	); err != nil {
		t.Fatalf("write saved recall eval config: %v", err)
	}
	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", recallCasesPath)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/seed.md","memory_id":"alpha::notes/seed.md","summary":"seed only","source":"qdrant","score":0.91}],"warnings":[]}`))
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)

	s := newServer()
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		t.Fatalf("expected enabled memory store")
	}
	for _, item := range []normalizedWrite{
		{project: "alpha", fileName: "notes/seed.md", content: "seed memory", topicPath: "recall/graph"},
		{project: "alpha", fileName: "notes/target.md", content: "target memory", topicPath: "recall/graph"},
	} {
		if _, _, err := s.memoryStore.put(item); err != nil {
			t.Fatalf("seed memory store: %v", err)
		}
	}
	if _, err := s.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID:   "alpha::notes/seed.md",
		TargetID:   "alpha::notes/target.md",
		Relation:   "inferred_related",
		Project:    "alpha",
		TopicPath:  "recall/graph",
		Confidence: 0.92,
		CreatedAt:  nowUTCISO(),
		Source:     memoryEdgeSource,
	}); err != nil {
		t.Fatalf("seed memory edge: %v", err)
	}

	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/memory/recall/evaluate/saved", "application/json", strings.NewReader(`{"include_retrieval_debug":true}`))
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
		t.Fatalf("decode response payload: %v", err)
	}
	metrics, _ := payload["metrics"].(map[string]any)
	if anyToFloat64(metrics["recallAtK"], -1) != 0 {
		t.Fatalf("expected top-k miss before graph expansion, got %#v", metrics)
	}
	if anyToFloat64(metrics["graphLift"], 0) != 1 {
		t.Fatalf("expected graph lift to recover the case, got metrics=%#v cases=%#v", metrics, payload["cases"])
	}
	graphContribution, _ := metrics["graphContribution"].(map[string]any)
	if anyToInt(graphContribution["helpedCases"], 0) != 1 {
		t.Fatalf("expected one helped graph case, got %#v", graphContribution)
	}
	cases, _ := payload["cases"].([]any)
	caseReport, _ := cases[0].(map[string]any)
	caseGraph, _ := caseReport["graph_contribution"].(map[string]any)
	if !anyToBool(caseGraph["helped"]) || anyToInt(caseGraph["added_expected_hit_count"], 0) != 1 {
		t.Fatalf("expected per-case graph contribution, got %#v", caseGraph)
	}
}

func TestMemoryContextPackIncludesBoundedGraphNeighbors(t *testing.T) {
	t.Setenv("BACKEND_URL", "http://127.0.0.1:1")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_SEED_MAX", "2")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_MAX", "2")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_PER_SEED", "2")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/seed.md","summary":"seed memory qdrant result","source":"qdrant","score":0.91,"topic_path":"graph/test"}],"warnings":[]}`))
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)

	s := newServer()
	seedGraphContextMemory(t, s)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/memory/context-pack", "application/json", strings.NewReader(`{"project":"alpha","topic_path":"graph/test","query":"target by neighbor","limit":5,"include_retrieval_debug":true,"retrieval_mode":"balanced"}`))
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
		t.Fatalf("decode context-pack response: %v", err)
	}
	assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, payload)
	pack := anyMap(payload["context_pack"])
	graphNeighbors := contextPackAnyList(pack["graph_neighbors"])
	if len(graphNeighbors) != 1 {
		t.Fatalf("expected one graph neighbor, got %#v", graphNeighbors)
	}
	neighbor := anyMap(graphNeighbors[0])
	if anyToString(neighbor["file"]) != "notes/target.md" || anyToString(neighbor["relation"]) != "supports" {
		t.Fatalf("expected target graph neighbor, got %#v", neighbor)
	}
	rankedEvidence := contextPackAnyList(pack["ranked_evidence"])
	foundGraphEvidence := false
	for _, raw := range rankedEvidence {
		item := anyMap(raw)
		if anyToString(item["kind"]) == "graph_neighbor" && anyToString(item["file"]) == "notes/target.md" {
			foundGraphEvidence = true
			break
		}
	}
	if !foundGraphEvidence {
		t.Fatalf("expected graph neighbor ranked evidence, got %#v", rankedEvidence)
	}
	sourceCoverage := anyMap(payload["source_coverage"])
	if !testStringSliceContains(anyToStringList(sourceCoverage["returned"], 16), memoryEdgeSource) {
		t.Fatalf("expected source coverage to include memory_edges, got %#v", sourceCoverage)
	}
	runAdvisor := anyMap(payload["run_advisor"])
	graphQuality := anyMap(runAdvisor["graph_quality"])
	if anyToString(graphQuality["status"]) != "sampled" || !anyToBool(graphQuality["used"]) {
		t.Fatalf("expected sampled graph quality, got %#v", graphQuality)
	}
	signals := anyMap(graphQuality["signals"])
	if anyToInt(signals["added_evidence_count"], 0) != 1 {
		t.Fatalf("expected one added graph evidence signal, got %#v", signals)
	}
}

func TestSynthesisPackRoutesProduceContractValidSynthesis(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/retrieval/query":
			_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/synthesis.md","source":"qdrant","score":0.91,"summary":"decision: Synthesis Pack should verify ` + "`go test ./...`" + ` and avoid known failure regression loops","topic_path":"runbooks/synthesis-pack","timestamp":"2026-07-08T00:00:00Z"}],"warnings":[]}`))
			return
		case "/memory/synthesis-pack":
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

	for _, path := range []string{"/memory/synthesis-pack", "/tools/synthesis_pack"} {
		t.Run(path, func(t *testing.T) {
			reqBody := `{"project":"contextlattice","query":"synthesize context intelligence","topic_path":"runbooks/synthesis-pack","limit":8,"retrieval_mode":"balanced","agent_id":"codex_gpt5_synthesis_test"}`
			resp, err := http.Post(gateway.URL+path, "application/json", strings.NewReader(reqBody))
			if err != nil {
				t.Fatalf("synthesis-pack request failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
			}
			var payload map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatalf("decode synthesis-pack payload: %v", err)
			}
			assertBoundaryContractPassed(t, synthesisPackContractID, payload)
			assertBoundaryJSONUnderLimit(t, synthesisPackContractID, payload)
			if anyToString(payload["schema_id"]) != synthesisPackContractID {
				t.Fatalf("expected synthesis schema id, got %#v", payload["schema_id"])
			}
			pack := anyMap(payload["synthesis_pack"])
			if len(contextPackAnyList(pack["high_signal_findings"])) == 0 {
				t.Fatalf("expected high signal findings, got %#v", pack)
			}
			if !testStringSliceContains(anyToStringList(pack["semantic_tags"], 20), "decision_memory") {
				t.Fatalf("expected decision_memory semantic tag, got %#v", pack["semantic_tags"])
			}
			if !strings.Contains(anyToString(payload["reference_prompt"]), "Synthesis Pack v1") {
				t.Fatalf("expected synthesis reference prompt, got %q", anyToString(payload["reference_prompt"]))
			}
			if path == "/tools/synthesis_pack" && strings.TrimSpace(anyToString(payload["tool"])) != "synthesis_pack" {
				t.Fatalf("expected tool marker on /tools/synthesis_pack, got %#v", payload["tool"])
			}
		})
	}
}

func TestSynthesisPackUsesTopicGravityAndCrossProjectGraphBridge(t *testing.T) {
	t.Setenv("BACKEND_URL", "http://127.0.0.1:1")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_SEED_MAX", "2")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_MAX", "4")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_PER_SEED", "2")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"notes/seed.md","source":"qdrant","score":0.92,"summary":"decision: alpha seed links to beta architecture verification","topic_path":"graph/test","timestamp":"2026-07-08T00:00:00Z"}],"warnings":[]}`))
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)

	s := newServer()
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		t.Fatalf("expected enabled memory store")
	}
	for _, item := range []normalizedWrite{
		{project: "alpha", fileName: "notes/seed.md", content: "seed memory", topicPath: "graph/test"},
		{project: "beta", fileName: "notes/target.md", content: "target memory", topicPath: "graph/test/link"},
	} {
		if _, _, err := s.memoryStore.put(item); err != nil {
			t.Fatalf("seed memory store: %v", err)
		}
	}
	if _, err := s.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID:   "alpha::notes/seed.md",
		TargetID:   "beta::notes/target.md",
		Relation:   "supports",
		Project:    "alpha",
		TopicPath:  "graph/test",
		Confidence: 0.94,
		CreatedAt:  nowUTCISO(),
		Source:     memoryEdgeSource,
	}); err != nil {
		t.Fatalf("seed memory edge: %v", err)
	}

	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"alpha","topic_path":"graph/test","query":"synthesize alpha beta graph link","limit":5,"include_retrieval_debug":true,"retrieval_mode":"balanced"}`
	resp, err := http.Post(gateway.URL+"/memory/synthesis-pack", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("synthesis-pack request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode synthesis-pack response: %v", err)
	}
	assertBoundaryContractPassed(t, synthesisPackContractID, payload)
	pack := anyMap(payload["synthesis_pack"])
	if len(contextPackAnyList(pack["topic_gravity"])) == 0 {
		t.Fatalf("expected topic gravity, got %#v", pack)
	}
	bridges := contextPackAnyList(pack["cross_project_bridges"])
	foundBeta := false
	for _, raw := range bridges {
		if anyToString(anyMap(raw)["project"]) == "beta" {
			foundBeta = true
			break
		}
	}
	if !foundBeta {
		t.Fatalf("expected beta cross-project bridge, got %#v", bridges)
	}
	if !testStringSliceContains(anyToStringList(pack["semantic_tags"], 24), "cross_project_bridge") {
		t.Fatalf("expected cross_project_bridge tag, got %#v", pack["semantic_tags"])
	}
}

func TestCodexPreflightCarriesGraphContextPackEvidence(t *testing.T) {
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "true")
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_SEED_MAX", "2")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_MAX", "2")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBOR_PER_SEED", "2")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("GO_MEMORY_GRAPH_EDGE_PATH", filepath.Join(root, "_contextlattice", "memory_edges.ndjson"))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health", "/status":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/collections/contextlattice_notes":
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
		case "/collections/contextlattice_notes/points/search":
			_, _ = w.Write([]byte(`{"result":[{"score":0.91,"payload":{"project":"alpha","file":"notes/seed.md","summary":"seed memory qdrant result","topic_path":"graph/test","created_at":"2026-06-17T00:00:00Z"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)
	t.Setenv("QDRANT_URL", backend.URL)

	s := newServer()
	seedGraphContextMemory(t, s)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1/codex/preflight", "application/json", strings.NewReader(`{"project":"alpha","topic_path":"graph/test","query":"target by neighbor","agent_id":"codex_gpt5_test","retrieval_mode":"balanced","objective":"prove graph context handoff","blocking":false,"wait_for_slow_sources":false,"sync_slow_sources":false,"combined_sources":false}`))
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
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, payload)
	contextPackEnvelope := anyMap(payload["context_pack"])
	contextPackPayload := anyMap(contextPackEnvelope["payload"])
	contextPack := anyMap(contextPackPayload["context_pack"])
	graphNeighbors := contextPackAnyList(contextPack["graph_neighbors"])
	if len(graphNeighbors) != 1 {
		t.Fatalf("expected preflight context pack graph neighbor, got %#v", graphNeighbors)
	}
	runAdvisor := anyMap(contextPackPayload["run_advisor"])
	graphQuality := anyMap(runAdvisor["graph_quality"])
	if anyToString(graphQuality["status"]) != "sampled" || !anyToBool(graphQuality["used"]) {
		t.Fatalf("expected preflight graph quality used, got %#v", graphQuality)
	}
}

func seedGraphContextMemory(t *testing.T, s *server) {
	t.Helper()
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		t.Fatalf("expected enabled memory store")
	}
	for _, item := range []normalizedWrite{
		{project: "alpha", fileName: "notes/seed.md", content: "seed memory", topicPath: "graph/test"},
		{project: "alpha", fileName: "notes/target.md", content: "target memory", topicPath: "graph/test"},
	} {
		if _, _, err := s.memoryStore.put(item); err != nil {
			t.Fatalf("seed memory store: %v", err)
		}
	}
	if _, err := s.memoryStore.upsertMemoryEdge(context.Background(), memoryEdgeEntry{
		SourceID:   "alpha::notes/seed.md",
		TargetID:   "alpha::notes/target.md",
		Relation:   "supports",
		Project:    "alpha",
		TopicPath:  "graph/test",
		Confidence: 0.94,
		CreatedAt:  nowUTCISO(),
		Source:     memoryEdgeSource,
	}); err != nil {
		t.Fatalf("seed memory edge: %v", err)
	}
}

func testStringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

func TestRecallEvalCasesRefreshUsesLiveFileBackedMemory(t *testing.T) {
	t.Setenv("BACKEND_URL", "http://127.0.0.1:1")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")

	s := newServer()
	for _, item := range []normalizedWrite{
		{
			project:   "contextlattice",
			fileName:  "notes/releases/v3.3.37-recall-quality-loop.md",
			content:   "recall quality loop graph contribution dashboard tuning",
			topicPath: "contextlattice/recall-quality-loop",
		},
		{
			project:   "contextlattice",
			fileName:  "notes/ops/live-recall-gate.md",
			content:   "live recall gate saved eval file backed memory",
			topicPath: "contextlattice/recall-quality-loop",
		},
	} {
		if _, _, err := s.memoryStore.put(item); err != nil {
			t.Fatalf("seed memory store: %v", err)
		}
	}

	refreshed := s.buildRefreshedRecallEvalCaseSet(5, 1, "contextlattice", "contextlattice/recall-quality-loop")
	cases, _ := refreshed["cases"].([]map[string]any)
	if len(cases) == 0 {
		t.Fatalf("expected refreshed recall cases, got %#v", refreshed)
	}
	for _, item := range cases {
		if strings.HasPrefix(anyToString(item["id"]), "health-") {
			t.Fatalf("refresh should not fall back to default cases, got %#v", cases)
		}
		if len(anyToStringSlice(item["expected_files"])) != 1 {
			t.Fatalf("expected file-backed recall case, got %#v", item)
		}
		if !strings.Contains(anyToString(item["query"]), "recall") {
			t.Fatalf("expected topic-derived recall query, got %#v", item)
		}
		if anyToString(item["project"]) != "contextlattice" {
			t.Fatalf("expected case to stay project-scoped, got %#v", item)
		}
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
			_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/a.md","source":"qdrant","score":0.88,"summary":"decision: run ` + "`scripts/agent/audit-agent-context`" + ` and verify acceptance criteria","topic_path":"runbooks/codex-integration","timestamp":"2026-03-30T00:00:00Z"}],"warnings":[]}`))
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
	if coverage, ok := payload["source_coverage"].(map[string]any); !ok || len(anyToStringList(coverage["returned"], 20)) == 0 {
		t.Fatalf("expected source_coverage returned sources, got %#v", payload["source_coverage"])
	}
	if files := anyToStringList(contextPack["filesToRead"], 20); len(files) == 0 || files[0] != "notes/a.md" {
		t.Fatalf("expected filesToRead to include notes/a.md, got %#v", contextPack["filesToRead"])
	}
	if commands, ok := contextPack["commands"].([]any); !ok || len(commands) == 0 {
		t.Fatalf("expected extracted commands in context pack, got %#v", contextPack["commands"])
	}
	compiler := anyMap(payload["context_compiler"])
	if anyToString(compiler["schema_id"]) != "contextlattice_context_compiler.v1" {
		t.Fatalf("expected compiler metadata, got %#v", compiler)
	}
	if prompt := anyToString(payload["reference_prompt"]); !strings.Contains(prompt, "Ranked evidence") || !strings.Contains(prompt, "notes/a.md") {
		t.Fatalf("expected prompt-ready reference prompt with ranked evidence, got %q", prompt)
	}
	if prompt := anyToString(payload["reference_prompt"]); !strings.Contains(prompt, "Agent guidance hints") {
		t.Fatalf("expected reference prompt to include agent guidance hints, got %q", prompt)
	}
	ranked, _ := asAnySlice(contextPack["ranked_evidence"])
	if len(ranked) == 0 {
		t.Fatalf("expected ranked evidence in context pack, got %#v", contextPack["ranked_evidence"])
	}
	firstEvidence := anyMap(ranked[0])
	if anyToInt(firstEvidence["rank"], 0) != 1 || strings.TrimSpace(anyToString(firstEvidence["reason"])) == "" {
		t.Fatalf("expected ranked evidence reason and rank, got %#v", firstEvidence)
	}
	promptSections := anyMap(contextPack["prompt_sections"])
	if !strings.Contains(anyToString(promptSections["next_action"]), "ranked evidence") {
		t.Fatalf("expected prompt sections next action, got %#v", promptSections)
	}
	agentGuidance := anyMap(payload["agent_guidance"])
	if strings.TrimSpace(anyToString(agentGuidance["schema_id"])) != "contextlattice_agent_guidance.v1" {
		t.Fatalf("expected root agent guidance schema, got %#v", agentGuidance)
	}
	if anyToBool(agentGuidance["authoritative"]) || !anyToBool(agentGuidance["not_dream_mode"]) {
		t.Fatalf("expected deterministic non-authoritative non-Dream guidance, got %#v", agentGuidance)
	}
	if themes := contextPackAnyList(agentGuidance["themes"]); len(themes) == 0 {
		t.Fatalf("expected guidance themes, got %#v", agentGuidance)
	}
	if hints := anyToStringList(agentGuidance["prompt_hints"], 10); len(hints) == 0 || strings.Contains(strings.ToLower(strings.Join(hints, " ")), "hypothesis") {
		t.Fatalf("expected bounded non-hypothesis prompt hints, got %#v", agentGuidance["prompt_hints"])
	}
	packGuidance := anyMap(contextPack["agent_guidance"])
	if strings.TrimSpace(anyToString(packGuidance["schema_id"])) != "contextlattice_agent_guidance.v1" {
		t.Fatalf("expected nested agent guidance schema, got %#v", packGuidance)
	}
	sectionGuidance := anyMap(promptSections["agent_guidance"])
	if strings.TrimSpace(anyToString(sectionGuidance["source"])) != "deterministic_evidence_analysis" {
		t.Fatalf("expected prompt section guidance, got %#v", sectionGuidance)
	}
	contextCompiler := anyMap(contextPack["context_compiler"])
	if anyToString(contextCompiler["recommended_surface"]) != "cli_for_local_agents" {
		t.Fatalf("expected CLI-first compiler surface, got %#v", contextCompiler)
	}
	if retrievalCalls < 1 {
		t.Fatalf("expected at least one retrieval backend call, got %d", retrievalCalls)
	}
	if proxyPathCalls != 0 {
		t.Fatalf("expected zero backend /memory/context-pack proxy calls, got %d", proxyPathCalls)
	}
}

func TestContextPackTokenizerExactAccounting(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_TOKENIZER_ENCODING", "cl100k_base")
	result := contextPackCountTokens("hello world")
	if !result.TokenizerExact || result.Method != "tiktoken" || result.CalibrationGrade != "tokenizer_exact" || result.Encoding != "cl100k_base" {
		t.Fatalf("expected tokenizer-exact cl100k accounting, got %#v", result)
	}
	if result.Tokens != 2 {
		t.Fatalf("expected cl100k token count for hello world to be 2, got %d", result.Tokens)
	}
}

func TestGatewayContextPackUsesImpactTokenBudgetAllocator(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_ENABLED", "true")
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_MAX_BYTES", "65536")
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_MAX_SAMPLES", "20")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_BYTES", "65536")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_SAMPLES", "20")
	t.Setenv("CONTEXTLATTICE_TOKENIZER_ENCODING", "cl100k_base")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		results := []map[string]any{
			{
				"project":    "contextlattice",
				"file":       "notes/high-impact/decision.md",
				"source":     "qdrant",
				"score":      0.98,
				"summary":    "decision: preserve user work; do not revert unrelated files; verify with `go test ./...` before claiming completion; risk regression if omitted.",
				"topic_path": "runbooks/context-pack",
				"timestamp":  "2026-06-28T00:00:00Z",
			},
			{
				"project":    "contextlattice",
				"file":       "notes/high-impact/checks.md",
				"source":     "qdrant",
				"score":      0.94,
				"summary":    "acceptance criteria: context pack must expose token_budget, omitted_high_value_refs, and selected evidence with estimated_tokens.",
				"topic_path": "runbooks/context-pack",
			},
			{
				"project":    "contextlattice",
				"file":       "notes/high-impact/risk.md",
				"source":     "qdrant",
				"score":      0.92,
				"summary":    "known failure mode: returning the biggest memories first can waste the model budget and block actionable verification.",
				"topic_path": "runbooks/context-pack",
			},
			{
				"project":    "contextlattice",
				"file":       "notes/background/architecture.md",
				"source":     "qdrant",
				"score":      0.71,
				"summary":    strings.Repeat("background architecture detail with lower immediate action value. ", 30),
				"topic_path": "background/context-pack",
			},
			{
				"project":    "contextlattice",
				"file":       "notes/background/history.md",
				"source":     "qdrant",
				"score":      0.69,
				"summary":    strings.Repeat("historical context that may be useful later but is not the next check. ", 30),
				"topic_path": "background/context-pack",
			},
			{
				"project":    "contextlattice",
				"file":       "notes/background/ideas.md",
				"source":     "qdrant",
				"score":      0.67,
				"summary":    strings.Repeat("idea backlog context without direct acceptance criteria. ", 30),
				"topic_path": "background/context-pack",
			},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results, "warnings": []any{}})
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","query":"budgeted context pack","topic_path":"runbooks/context-pack","limit":12,"max_facts":24,"target_context_pack_tokens":260,"include_retrieval_debug":true}`
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
	assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, payload)
	contextPack := anyMap(payload["context_pack"])
	tokenBudget := anyMap(payload["token_budget"])
	if !anyToBool(tokenBudget["active"]) || anyToInt(tokenBudget["target_context_pack_tokens"], 0) != 260 {
		t.Fatalf("expected active target token budget, got %#v", tokenBudget)
	}
	if anyToString(tokenBudget["selection_strategy"]) != "impact_per_estimated_token_with_provenance_diversity" {
		t.Fatalf("expected impact token selection strategy, got %#v", tokenBudget)
	}
	if nested := anyMap(contextPack["token_budget"]); !anyToBool(nested["active"]) {
		t.Fatalf("expected nested token budget, got %#v", contextPack["token_budget"])
	}
	compiler := anyMap(payload["context_compiler"])
	if anyToString(compiler["strategy"]) != "impact_per_token_prompt_packet" {
		t.Fatalf("expected impact compiler strategy, got %#v", compiler)
	}
	ranked := contextPackAnyList(contextPack["ranked_evidence"])
	if len(ranked) == 0 {
		t.Fatalf("expected ranked evidence, got %#v", contextPack["ranked_evidence"])
	}
	highImpactSelected := false
	for _, raw := range ranked {
		item := anyMap(raw)
		if anyToInt(item["estimated_tokens"], 0) <= 0 || anyToFloat(item["value_density"]) <= 0 || len(contextPackAnyList(item["why_selected"])) == 0 {
			t.Fatalf("expected token-aware evidence metadata, got %#v", item)
		}
		switch anyToString(item["kind"]) {
		case "decision", "risk", "check":
			highImpactSelected = true
		}
	}
	if !highImpactSelected {
		t.Fatalf("expected protected decision/risk/check evidence under constrained budget, got %#v", ranked)
	}
	omitted := contextPackAnyList(payload["omitted_high_value_refs"])
	if len(omitted) == 0 || len(contextPackAnyList(contextPack["omitted_high_value_refs"])) == 0 {
		t.Fatalf("expected omitted high-value refs at root and nested pack, got root=%#v nested=%#v", payload["omitted_high_value_refs"], contextPack["omitted_high_value_refs"])
	}
	promptSections := anyMap(contextPack["prompt_sections"])
	if !anyToBool(anyMap(promptSections["token_budget"])["active"]) || len(contextPackAnyList(promptSections["omitted_high_value_refs"])) == 0 {
		t.Fatalf("expected prompt sections to expose budget and omitted refs, got %#v", promptSections)
	}
	referencePrompt := anyToString(payload["reference_prompt"])
	if !strings.Contains(referencePrompt, "Context budget:") || !strings.Contains(referencePrompt, "Omitted high-value refs") {
		t.Fatalf("expected reference prompt to describe token budget and omitted refs, got %q", referencePrompt)
	}
	tokenImpact := anyMap(payload["token_impact"])
	if anyToString(tokenImpact["schema_id"]) != "contextlattice_token_impact.v1" {
		t.Fatalf("expected token impact sample, got %#v", tokenImpact)
	}
	baselineTokens := anyToInt(tokenImpact["baseline_tokens_estimate"], 0)
	packedTokens := anyToInt(tokenImpact["packed_tokens_estimate"], 0)
	savedTokens := anyToInt(tokenImpact["saved_tokens_estimate"], 0)
	if baselineTokens <= packedTokens || savedTokens != baselineTokens-packedTokens {
		t.Fatalf("expected positive token savings, got baseline=%d packed=%d saved=%d payload=%#v", baselineTokens, packedTokens, savedTokens, tokenImpact)
	}
	if anyToString(tokenImpact["calibration_grade"]) != "tokenizer_exact" ||
		anyToString(tokenImpact["estimate_method"]) != "tiktoken" ||
		anyToString(tokenImpact["tokenizer_encoding"]) != "cl100k_base" ||
		!anyToBool(tokenImpact["tokenizer_exact"]) {
		t.Fatalf("expected tokenizer-exact impact metadata, got %#v", tokenImpact)
	}
	nestedTokenImpact := anyMap(contextPack["token_impact"])
	if anyToInt(nestedTokenImpact["saved_tokens_estimate"], 0) != savedTokens {
		t.Fatalf("expected nested token impact to match root sample, root=%#v nested=%#v", tokenImpact, nestedTokenImpact)
	}
	contextPackQuality := anyMap(payload["context_pack_quality"])
	if anyToString(contextPackQuality["schema_id"]) != contextPackQualitySchemaID {
		t.Fatalf("expected context pack quality sample, got %#v", contextPackQuality)
	}
	qualitySampleID := anyToString(contextPackQuality["sample_id"])
	if qualitySampleID == "" || !strings.HasPrefix(qualitySampleID, "cpq_") {
		t.Fatalf("expected stable quality sample id, got %#v", contextPackQuality)
	}
	if anyToInt(contextPackQuality["exact_prompt_tokens_saved"], 0) != savedTokens ||
		anyToInt(contextPackQuality["modeled_inference_tokens_avoided"], 0) <= 0 ||
		anyToInt(contextPackQuality["quality_score"], 0) <= 0 {
		t.Fatalf("expected quality sample to include exact savings and modeled avoidance, got %#v", contextPackQuality)
	}
	if anyToString(contextPackQuality["calibration_grade"]) != "modeled_counterfactual" ||
		anyToString(contextPackQuality["counterfactual_baseline"]) != "raw_candidate_replay" {
		t.Fatalf("expected confidence-banded counterfactual quality metadata, got %#v", contextPackQuality)
	}
	if anyToString(contextPackQuality["query_hash"]) == "" || strings.Contains(anyToString(contextPackQuality["query_hash"]), "budgeted") {
		t.Fatalf("expected hashed query provenance only, got %#v", contextPackQuality)
	}
	telemetryResp, err := http.Get(gateway.URL + "/telemetry/token-impact")
	if err != nil {
		t.Fatalf("token impact telemetry request failed: %v", err)
	}
	defer telemetryResp.Body.Close()
	if telemetryResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(telemetryResp.Body)
		t.Fatalf("expected token impact telemetry 200, got %d body=%s", telemetryResp.StatusCode, string(body))
	}
	var telemetryPayload map[string]any
	if err := json.NewDecoder(telemetryResp.Body).Decode(&telemetryPayload); err != nil {
		t.Fatalf("decode token impact telemetry payload: %v", err)
	}
	if anyToString(telemetryPayload["schema_id"]) != "contextlattice_token_impact_telemetry.v1" ||
		anyToInt(telemetryPayload["sample_count"], 0) < 1 ||
		anyToInt(telemetryPayload["saved_tokens_estimate"], 0) < savedTokens ||
		!anyToBool(telemetryPayload["tokenizer_exact"]) {
		t.Fatalf("expected token impact telemetry aggregate to include sample, got %#v", telemetryPayload)
	}
	storage := anyMap(telemetryPayload["storage"])
	if !anyToBool(storage["enabled"]) || anyToString(storage["durability"]) != "bounded_ndjson" {
		t.Fatalf("expected bounded persisted token-impact storage, got %#v", storage)
	}
	ledgerPath := filepath.Join(root, "_contextlattice", "token_impact_ledger.ndjson")
	ledgerRaw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("expected persisted token impact ledger: %v", err)
	}
	if !strings.Contains(string(ledgerRaw), `"tokenizer_exact":true`) || strings.Contains(string(ledgerRaw), "preserve user work") {
		t.Fatalf("expected compact exact ledger row without raw prompt text, got %s", string(ledgerRaw))
	}
	qualityResp, err := http.Get(gateway.URL + "/telemetry/context-pack-quality")
	if err != nil {
		t.Fatalf("context pack quality telemetry request failed: %v", err)
	}
	defer qualityResp.Body.Close()
	if qualityResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(qualityResp.Body)
		t.Fatalf("expected context pack quality telemetry 200, got %d body=%s", qualityResp.StatusCode, string(body))
	}
	var qualityPayload map[string]any
	if err := json.NewDecoder(qualityResp.Body).Decode(&qualityPayload); err != nil {
		t.Fatalf("decode context pack quality telemetry payload: %v", err)
	}
	if anyToString(qualityPayload["schema_id"]) != contextPackQualityTelemetrySchemaID ||
		anyToInt(qualityPayload["sample_count"], 0) < 1 ||
		anyToInt(qualityPayload["modeled_inference_tokens_avoided"], 0) <= 0 ||
		anyToInt(qualityPayload["exact_prompt_tokens_saved"], 0) < savedTokens {
		t.Fatalf("expected quality aggregate to include sample, got %#v", qualityPayload)
	}
	qualityStorage := anyMap(qualityPayload["storage"])
	if !anyToBool(qualityStorage["enabled"]) || anyToString(qualityStorage["durability"]) != "bounded_ndjson" {
		t.Fatalf("expected bounded persisted context-pack quality storage, got %#v", qualityStorage)
	}
	qualityLedgerPath := filepath.Join(root, "_contextlattice", "context_pack_quality_ledger.ndjson")
	qualityLedgerRaw, err := os.ReadFile(qualityLedgerPath)
	if err != nil {
		t.Fatalf("expected persisted context pack quality ledger: %v", err)
	}
	if !strings.Contains(string(qualityLedgerRaw), contextPackQualitySchemaID) || strings.Contains(string(qualityLedgerRaw), "preserve user work") || strings.Contains(string(qualityLedgerRaw), "budgeted context pack") {
		t.Fatalf("expected compact quality ledger row without raw query/source text, got %s", string(qualityLedgerRaw))
	}
	outcomeBody, err := json.Marshal(map[string]any{
		"sample_id":          qualitySampleID,
		"first_pass_success": true,
		"repair_required":    false,
		"retry_count":        0,
		"followup_tokens":    123,
		"usage": map[string]any{
			"prompt_tokens":     456,
			"completion_tokens": 78,
			"total_tokens":      534,
		},
		"outcome_source": "test_agent",
	})
	if err != nil {
		t.Fatalf("encode quality outcome: %v", err)
	}
	outcomeResp, err := http.Post(gateway.URL+"/telemetry/context-pack-quality/outcome", "application/json", strings.NewReader(string(outcomeBody)))
	if err != nil {
		t.Fatalf("context pack quality outcome request failed: %v", err)
	}
	defer outcomeResp.Body.Close()
	if outcomeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(outcomeResp.Body)
		t.Fatalf("expected quality outcome 200, got %d body=%s", outcomeResp.StatusCode, string(body))
	}
	var outcomePayload map[string]any
	if err := json.NewDecoder(outcomeResp.Body).Decode(&outcomePayload); err != nil {
		t.Fatalf("decode context pack quality outcome payload: %v", err)
	}
	outcomeTelemetry := anyMap(outcomePayload["telemetry"])
	if anyToInt(outcomeTelemetry["outcome_sample_count"], 0) < 1 ||
		anyToString(outcomeTelemetry["calibration_grade"]) != "outcome_seeded" ||
		anyToFloat(outcomeTelemetry["observed_first_pass_success_rate"]) <= 0 ||
		anyToInt(outcomeTelemetry["observed_followup_tokens"], 0) != 123 ||
		anyToInt(outcomeTelemetry["observed_provider_total_tokens"], 0) != 534 ||
		anyToInt(outcomeTelemetry["observed_provider_usage_count"], 0) != 1 {
		t.Fatalf("expected outcome-seeded quality telemetry, got %#v", outcomeTelemetry)
	}

	reloaded := newTestServer(t, backend.URL)
	reloadedGateway := httptest.NewServer(buildMux(reloaded))
	defer reloadedGateway.Close()
	reloadedResp, err := http.Get(reloadedGateway.URL + "/telemetry/token-impact")
	if err != nil {
		t.Fatalf("reloaded token impact telemetry request failed: %v", err)
	}
	defer reloadedResp.Body.Close()
	var reloadedPayload map[string]any
	if err := json.NewDecoder(reloadedResp.Body).Decode(&reloadedPayload); err != nil {
		t.Fatalf("decode reloaded token impact telemetry payload: %v", err)
	}
	if anyToInt(reloadedPayload["sample_count"], 0) < 1 || anyToInt(reloadedPayload["saved_tokens_estimate"], 0) < savedTokens {
		t.Fatalf("expected reloaded telemetry to include persisted token impact sample, got %#v", reloadedPayload)
	}
	reloadedQualityResp, err := http.Get(reloadedGateway.URL + "/telemetry/context-pack-quality")
	if err != nil {
		t.Fatalf("reloaded context pack quality telemetry request failed: %v", err)
	}
	defer reloadedQualityResp.Body.Close()
	var reloadedQuality map[string]any
	if err := json.NewDecoder(reloadedQualityResp.Body).Decode(&reloadedQuality); err != nil {
		t.Fatalf("decode reloaded context pack quality telemetry payload: %v", err)
	}
	if anyToInt(reloadedQuality["sample_count"], 0) < 1 ||
		anyToInt(reloadedQuality["outcome_sample_count"], 0) < 1 ||
		anyToInt(reloadedQuality["modeled_inference_tokens_avoided"], 0) <= 0 ||
		anyToInt(reloadedQuality["observed_followup_tokens"], 0) != 123 ||
		anyToInt(reloadedQuality["observed_provider_total_tokens"], 0) != 534 {
		t.Fatalf("expected reloaded quality telemetry to include persisted sample and outcome, got %#v", reloadedQuality)
	}
}

func TestContextPackAgentRoutesClipOversizedBackendPayloads(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/retrieval/query":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oversizedBoundarySearchResponse())
			return
		case "/memory/context-pack":
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

	for _, path := range []string{"/memory/context-pack", "/tools/context_pack"} {
		t.Run(path, func(t *testing.T) {
			reqBody := `{"project":"contextlattice","query":"route boundary canary","topic_path":"runbooks/boundary","limit":100,"max_facts":100,"include_retrieval_debug":true,"retrieval_mode":"balanced","agent_id":"codex_gpt5_boundary_test"}`
			resp, err := http.Post(gateway.URL+path, "application/json", strings.NewReader(reqBody))
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
			assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
			assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, payload)
			assertNoRawProviderOverflowShape(t, payload)
			assertBoundaryMetadata(t, payload, "format_contract", true)
			assertBoundaryMetadataActualUnderLimit(t, contextPackResponseContractID, payload, "format_contract")
			if path == "/tools/context_pack" && strings.TrimSpace(anyToString(payload["tool"])) != "context_pack" {
				t.Fatalf("expected tool marker on /tools/context_pack, got %#v", payload["tool"])
			}
		})
	}
}

func TestPreflightRoutesClipOversizedBackendPayloads(t *testing.T) {
	const missionQuery = "mission objective goal cross-project synthesis longitudinal learning policy context package retrieval discipline"
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
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(oversizedBoundarySearchResponse())
			return
		case "/memory/context-pack":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			envelope := oversizedBoundaryContextPackEnvelope()
			if strings.TrimSpace(anyToString(payload["query"])) == missionQuery {
				envelope["query"] = "mission policy package boundary canary"
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(envelope)
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

	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "codex",
			path: "/v1/codex/preflight",
			body: `{"project":"contextlattice","topic_path":"runbooks/boundary","query":"preflight boundary canary","agent_id":"codex_gpt5_boundary_test","mission":"boundary mission","objective":"clip oversized backend payloads","goal":"no raw provider overflow shape leaves preflight","blocking":false,"wait_for_slow_sources":false,"sync_slow_sources":false,"combined_sources":false}`,
		},
		{
			name: "agents",
			path: "/v1/agents/preflight",
			body: `{"agent":"codex","project":"contextlattice","topic_path":"runbooks/boundary","query":"agents preflight boundary canary","agent_id":"codex_gpt5_boundary_test","mission":"boundary mission","objective":"clip oversized backend payloads","goal":"no raw provider overflow shape leaves preflight","blocking":false,"wait_for_slow_sources":false,"sync_slow_sources":false,"combined_sources":false}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(gateway.URL+tc.path, "application/json", strings.NewReader(tc.body))
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
				t.Fatalf("decode preflight payload: %v", err)
			}
			assertBoundaryContractPassed(t, agentPreflightResponseContractID, payload)
			assertBoundaryJSONUnderLimit(t, agentPreflightResponseContractID, payload)
			assertNoRawProviderOverflowShape(t, payload)
			assertBoundaryMetadata(t, payload, "format_contracts", true)
			assertBoundaryMetadataActualUnderLimit(t, agentPreflightResponseContractID, payload, "format_contracts")
			policy := anyMap(payload["policy_context_package"])
			assertBoundaryContractPassed(t, policyContextPackageContractID, policy)
			assertBoundaryJSONUnderLimit(t, policyContextPackageContractID, policy)
			assertNoRawProviderOverflowShape(t, policy)
		})
	}
}

func oversizedBoundaryText() string {
	return strings.Repeat("array_above_max_length context length exceeded maximum context length oversized input ", 240)
}

func oversizedBoundaryItems(count int) []any {
	text := oversizedBoundaryText()
	items := make([]any, 0, count)
	for idx := 0; idx < count; idx++ {
		items = append(items, map[string]any{
			"text":       text,
			"summary":    text,
			"file":       "notes/oversized-boundary.md",
			"source":     "fixture",
			"topic_path": "runbooks/boundary",
			"score":      0.9,
		})
	}
	return items
}

func oversizedBoundarySearchResponse() map[string]any {
	text := oversizedBoundaryText()
	items := oversizedBoundaryItems(140)
	return map[string]any{
		"degraded": false,
		"results":  items,
		"warnings": []any{text, text},
		"retrieval_debug": map[string]any{
			"raw":     text,
			"results": items,
		},
	}
}

func oversizedBoundaryContextPackEnvelope() map[string]any {
	text := oversizedBoundaryText()
	items := oversizedBoundaryItems(140)
	compiler := map[string]any{
		"schema_id":              "contextlattice_context_compiler.v1",
		"version":                1,
		"strategy":               "oversized_route_fixture",
		"intended_use":           "test boundary clipping",
		"recommended_surface":    "cli_for_local_agents",
		"ranked_evidence_count":  len(items),
		"retrieval_debug_detail": text,
	}
	promptSections := map[string]any{
		"objective":        text,
		"task":             text,
		"next_action":      text,
		"evidence":         items,
		"files_to_inspect": items,
		"commands":         items,
		"checks":           items,
		"risks":            items,
		"capabilities":     items,
		"constraints":      items,
	}
	contextPack := map[string]any{
		"query":               "oversized backend context pack",
		"retrieval_mode":      "balanced",
		"facts":               items,
		"numericFacts":        items,
		"numeric_facts":       items,
		"citations":           items,
		"results":             items,
		"rankedEvidence":      items,
		"ranked_evidence":     items,
		"relevantDecisions":   items,
		"relevant_decisions":  items,
		"filesToRead":         items,
		"files_to_read":       items,
		"filesToAvoid":        items,
		"files_to_avoid":      items,
		"capabilitiesToUse":   items,
		"capabilities_to_use": items,
		"runbooks":            items,
		"knownFailureModes":   items,
		"known_failure_modes": items,
		"commands":            items,
		"acceptanceCriteria":  items,
		"acceptance_criteria": items,
		"promptSections":      promptSections,
		"prompt_sections":     promptSections,
		"contextCompiler":     compiler,
		"context_compiler":    compiler,
	}
	return map[string]any{
		"ok":                 true,
		"query":              text,
		"context_pack":       contextPack,
		"context_compiler":   compiler,
		"reference_prompt":   text,
		"source_coverage":    map[string]any{"configured": items, "returned": items, "pending": items, "complete": false},
		"warnings":           items,
		"retrieval":          map[string]any{"debug": text, "results": items},
		"writeback_required": true,
	}
}

func TestMemoryDreamRejectsWithoutLLM(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_DREAM_LLM_ENABLED", "false")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("Dream Mode without LLM must not call backend path %s", r.URL.Path)
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","goal":"invent the next ContextLattice memory primitive","topic_path":"contextlattice/dream-mode","novelty_level":4,"risk_tolerance":"relaxed","use_llm":false}`
	resp, err := http.Post(gateway.URL+"/memory/dream", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFailedDependency {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 424, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream payload: %v", err)
	}
	if anyToBool(payload["ok"]) {
		t.Fatalf("expected ok=false, got %#v", payload)
	}
	if mode := strings.TrimSpace(anyToString(payload["mode"])); mode != "dream_unavailable" {
		t.Fatalf("expected dream_unavailable mode, got %q payload=%#v", mode, payload)
	}
	if anyToBool(payload["dream_available"]) {
		t.Fatalf("expected dream_available=false, got %#v", payload)
	}
	if source := strings.TrimSpace(anyToString(payload["intelligence_source"])); source != "none" {
		t.Fatalf("expected no intelligence source, got %q payload=%#v", source, payload)
	}
	if errCode := strings.TrimSpace(anyToString(payload["error"])); errCode != "llm_synthesis_required" {
		t.Fatalf("expected llm_synthesis_required, got %q payload=%#v", errCode, payload)
	}
	hypotheses, _ := payload["hypotheses"].([]any)
	if len(hypotheses) != 0 {
		t.Fatalf("expected no hypotheses without LLM synthesis, got %#v", hypotheses)
	}
	experiments, _ := payload["experiments"].([]any)
	if len(experiments) != 0 {
		t.Fatalf("expected no experiments without LLM synthesis, got %#v", experiments)
	}
	evidence, _ := payload["evidence"].(map[string]any)
	if results, _ := evidence["results"].([]any); len(results) != 0 {
		t.Fatalf("expected no Dream evidence payload without LLM synthesis, got %#v", evidence)
	}
	llm, _ := payload["llm"].(map[string]any)
	if anyToBool(llm["enabled"]) || anyToBool(llm["used"]) {
		t.Fatalf("expected llm disabled, got %#v", llm)
	}
	format, _ := payload["format_contract"].(map[string]any)
	if strings.TrimSpace(anyToString(format["schema_id"])) != dreamModeResponseContractID {
		t.Fatalf("expected dream format contract, got %#v", format)
	}
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected dream validation passed, got %#v", validation)
	}
}

func TestMemoryReviewReturnsBoundedMitigationPatterns(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	t.Setenv("GO_MEMORY_STORE_HISTORY_PATH", filepath.Join(root, "_contextlattice", "memory_write_history.ndjson"))
	t.Setenv("GO_MEMORY_STORE_ACCESS_LOG_PATH", filepath.Join(root, "_contextlattice", "memory_access_log.ndjson"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", filepath.Join(root, "_contextlattice", "objects"))

	store, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatalf("newMemoryStoreFromEnv failed: %v", err)
	}
	writes := []normalizedWrite{
		{project: "contextlattice", fileName: "notes/review-1.md", content: "overflow retry blocked mitigation one", topicPath: "runbooks/review", agentID: "agent-a", sessionID: "session-a"},
		{project: "contextlattice", fileName: "notes/review-2.md", content: "overflow retry blocked mitigation two", topicPath: "runbooks/review", agentID: "agent-b", sessionID: "session-b"},
		{project: "contextlattice", fileName: "notes/review-3.md", content: "overflow retry blocked mitigation three", topicPath: "runbooks/review", agentID: "agent-a", sessionID: "session-a"},
		{project: "contextlattice", fileName: "notes/review-4.md", content: "checkpoint handoff preflight", topicPath: "runbooks/review", agentID: "agent-c", sessionID: "session-c"},
	}
	for _, item := range writes {
		if _, _, err := store.put(item); err != nil {
			t.Fatalf("put failed: %v", err)
		}
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"review should be native"}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	s.memoryStore = store
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","topic_path":"runbooks/review","query":"review repeat patterns","max_patterns":4,"window_hours":168,"agent_id":"agent-a","session_id":"session-a"}`
	resp, err := http.Post(gateway.URL+"/memory/review", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("review request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode review payload: %v", err)
	}
	if !anyToBool(payload["ok"]) {
		t.Fatalf("expected ok review payload, got %#v", payload)
	}
	patterns, _ := payload["patterns"].([]any)
	if len(patterns) == 0 {
		t.Fatalf("expected review patterns, got %#v", payload)
	}
	format, _ := payload["format_contract"].(map[string]any)
	if strings.TrimSpace(anyToString(format["schema_id"])) != reviewModeResponseContractID {
		t.Fatalf("expected review format contract, got %#v", format)
	}
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected review validation passed, got %#v", validation)
	}
	analysis := anyMap(payload["evidence_analysis"])
	if strings.TrimSpace(anyToString(analysis["schema_id"])) != "contextlattice_agent_guidance.v1" {
		t.Fatalf("expected review evidence analysis guidance schema, got %#v", analysis)
	}
	if anyToBool(analysis["authoritative"]) || !anyToBool(analysis["not_dream_mode"]) {
		t.Fatalf("expected review evidence analysis to be non-authoritative and non-Dream, got %#v", analysis)
	}
	if risks := contextPackAnyList(analysis["risk_markers"]); len(risks) == 0 {
		t.Fatalf("expected review evidence analysis risk markers, got %#v", analysis)
	}
	assertBoundaryJSONUnderLimit(t, reviewModeResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
}

func TestMemoryDreamUsesBackendLLMWhenRequested(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_DREAM_LLM_ENABLED", "true")

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"content":"{\"hypotheses\":[{\"title\":\"Let graph edges steer Dream Mode\",\"claim\":\"Use memory edges as a hypothesis prior before retrieval ranking.\",\"supporting_evidence\":[\"e1\"],\"experiment\":\"Compare dream output with and without graph-edge priors.\",\"expected_signal\":\"Higher useful hypothesis rate.\"}],\"experiments\":[],\"next_best_action\":\"ship bounded route\"}"}}`))
	}))
	defer llmServer.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/edges.md","source":"qdrant","score":0.91,"summary":"memory edges make related agent decisions visible to synthesis","topic_path":"contextlattice/graph"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","goal":"use the backend llm for nonlinear memory synthesis","topic_path":"contextlattice/dream-mode","use_llm":true,"provider":"ollama","base_url":"` + llmServer.URL + `","model":"dream-test","max_hypotheses":5}`
	resp, err := http.Post(gateway.URL+"/memory/dream", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream payload: %v", err)
	}
	if mode := strings.TrimSpace(anyToString(payload["mode"])); mode != "dream" {
		t.Fatalf("expected dream mode, got %q payload=%#v", mode, payload)
	}
	if !anyToBool(payload["dream_available"]) {
		t.Fatalf("expected dream_available=true, got %#v", payload)
	}
	if source := strings.TrimSpace(anyToString(payload["intelligence_source"])); source != "llm_synthesis" {
		t.Fatalf("expected llm_synthesis intelligence source, got %q payload=%#v", source, payload)
	}
	llm, _ := payload["llm"].(map[string]any)
	if !anyToBool(llm["enabled"]) || !anyToBool(llm["used"]) {
		t.Fatalf("expected llm used, got %#v", llm)
	}
	hypotheses, _ := payload["hypotheses"].([]any)
	foundLLM := false
	for _, raw := range hypotheses {
		item, _ := raw.(map[string]any)
		if strings.TrimSpace(anyToString(item["type"])) == "llm_synthesis" {
			foundLLM = true
			break
		}
	}
	if !foundLLM {
		t.Fatalf("expected llm_synthesis hypothesis, got %#v", hypotheses)
	}
	format, _ := payload["format_contract"].(map[string]any)
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected dream validation passed, got %#v", validation)
	}
}

func TestMemoryDreamRejectsUnstructuredLLMOutput(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_DREAM_LLM_ENABLED", "true")

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"content":"I cannot produce structured hypotheses."}}`))
	}))
	defer llmServer.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/edges.md","source":"qdrant","score":0.91,"summary":"memory edges make related agent decisions visible to synthesis","topic_path":"contextlattice/graph"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","goal":"use the backend llm for nonlinear memory synthesis","topic_path":"contextlattice/dream-mode","use_llm":true,"provider":"ollama","base_url":"` + llmServer.URL + `","model":"dream-test"}`
	resp, err := http.Post(gateway.URL+"/memory/dream", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFailedDependency {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 424, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream payload: %v", err)
	}
	if anyToBool(payload["ok"]) || anyToBool(payload["dream_available"]) {
		t.Fatalf("expected unavailable dream response, got %#v", payload)
	}
	if mode := strings.TrimSpace(anyToString(payload["mode"])); mode != "dream_unavailable" {
		t.Fatalf("expected dream_unavailable mode, got %q payload=%#v", mode, payload)
	}
	if source := strings.TrimSpace(anyToString(payload["intelligence_source"])); source != "none" {
		t.Fatalf("expected no intelligence source, got %q payload=%#v", source, payload)
	}
	if errCode := strings.TrimSpace(anyToString(payload["error"])); errCode != "llm_synthesis_unstructured" {
		t.Fatalf("expected llm_synthesis_unstructured, got %q payload=%#v", errCode, payload)
	}
	if hypotheses, _ := payload["hypotheses"].([]any); len(hypotheses) != 0 {
		t.Fatalf("expected no hypotheses for unstructured LLM output, got %#v", hypotheses)
	}
	if experiments, _ := payload["experiments"].([]any); len(experiments) != 0 {
		t.Fatalf("expected no experiments for unstructured LLM output, got %#v", experiments)
	}
	evidence, _ := payload["evidence"].(map[string]any)
	if results, _ := evidence["results"].([]any); len(results) != 0 {
		t.Fatalf("expected no Dream evidence payload on unavailable response, got %#v", evidence)
	}
	assertBoundaryContractPassed(t, dreamModeResponseContractID, payload)
}

func TestMemoryDreamDeepensWhenReflectionFindsWeakOutput(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_DREAM_LLM_ENABLED", "true")

	var mu sync.Mutex
	chatCalls := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/chat" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		chatCalls++
		call := chatCalls
		mu.Unlock()
		if call == 1 {
			_, _ = w.Write([]byte(`{"message":{"content":"{\"hypotheses\":[{\"title\":\"Combine v3.3.41 dream mode with v3.3.41 dream mode\",\"claim\":\"Combine Dream Mode with itself.\",\"supporting_evidence\":[\"e1\"],\"experiment\":\"Repeat the same query.\",\"expected_signal\":\"Same output repeats.\"}]}"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":{"content":"{\"hypotheses\":[{\"title\":\"Use template conformance as Dream Mode's runtime gate\",\"claim\":\"A template conformance pass can detect reasoning-only local runtime outputs before Dream Mode accepts synthesis, then reroute through a final-content profile to improve usable novelty.\",\"supporting_evidence\":[\"e1\"],\"experiment\":\"Run Dream Mode against default and final-content MLX templates and compare reflection score, llm usage, and generic hypothesis rate.\",\"expected_signal\":\"Reflection score crosses the sigma target while reasoning-only outputs fail with repair instructions.\"}],\"experiments\":[],\"next_best_action\":\"ship the conformance gate\"}"}}`))
	}))
	defer llmServer.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/templates.md","source":"qdrant","score":0.94,"summary":"Dream Mode must reject reasoning-only template outputs and prefer final content conformance before accepting synthesis","topic_path":"contextlattice/dream-mode"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","goal":"make Dream Mode template conformance novel and useful","topic_path":"contextlattice/dream-mode","use_llm":true,"provider":"ollama","base_url":"` + llmServer.URL + `","model":"dream-test","max_hypotheses":1,"novelty_level":5,"reflection_min_score":0.8,"reflection_max_passes":1}`
	resp, err := http.Post(gateway.URL+"/memory/dream", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream payload: %v", err)
	}
	mu.Lock()
	gotCalls := chatCalls
	mu.Unlock()
	if gotCalls < 2 {
		t.Fatalf("expected primary plus deepening LLM calls, got %d payload=%#v", gotCalls, payload)
	}
	reflection, _ := payload["reflection"].(map[string]any)
	if !anyToBool(reflection["deepening_attempted"]) || !anyToBool(reflection["deepening_used"]) {
		t.Fatalf("expected reflection to use deepening, got %#v", reflection)
	}
	if !anyToBool(reflection["sigma_level"]) {
		t.Fatalf("expected deepened output to reach sigma target, got %#v", reflection)
	}
	llm, _ := payload["llm"].(map[string]any)
	deepening, _ := llm["deepening"].(map[string]any)
	if !anyToBool(deepening["used"]) {
		t.Fatalf("expected deepening llm used, got %#v", llm)
	}
	hypotheses, _ := payload["hypotheses"].([]any)
	if len(hypotheses) == 0 {
		t.Fatalf("expected hypotheses, got %#v", payload["hypotheses"])
	}
	first, _ := hypotheses[0].(map[string]any)
	if !strings.Contains(anyToString(first["title"]), "template conformance") {
		t.Fatalf("expected deepened hypothesis to replace weak output, got %#v", first)
	}
}

func TestMemoryDreamReplacesDeprecatedModelAndPassesPatientLLMOptions(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_DREAM_LLM_ENABLED", "true")

	var chatPayload map[string]any
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5-coder:7b"},{"name":"qwen3.5:9b"},{"name":"qwen3.6:35b-a3b"}]}`))
		case "/api/chat":
			if err := json.NewDecoder(r.Body).Decode(&chatPayload); err != nil {
				t.Fatalf("decode ollama chat payload: %v", err)
			}
			_, _ = w.Write([]byte(`{"message":{"content":"{\"hypotheses\":[{\"title\":\"Patient nonlinear synthesis\",\"claim\":\"A longer bounded inference budget lets Dream Mode connect distant evidence without exposing raw thinking.\",\"supporting_evidence\":[\"e1\"],\"experiment\":\"Run the same dream query with 60s and 600s budgets.\",\"expected_signal\":\"More useful cross-source hypotheses.\"}]}"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer llmServer.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/dream.md","source":"qdrant","score":0.91,"summary":"Dream Mode should synthesize nonlinear relationships across shared memory","topic_path":"contextlattice/dream-mode"}],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","goal":"patient dream synthesis","topic_path":"contextlattice/dream-mode","use_llm":true,"provider":"ollama","base_url":"` + llmServer.URL + `","model":"qwen2.5-coder:7b","llm_timeout_secs":7,"llm_max_tokens":123,"llm_temperature":0.7,"reflection_max_passes":0}`
	resp, err := http.Post(gateway.URL+"/memory/dream", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream payload: %v", err)
	}
	llm, _ := payload["llm"].(map[string]any)
	if got := strings.TrimSpace(anyToString(llm["model"])); got != "qwen3.6:35b-a3b" {
		t.Fatalf("expected qwen3.6 runtime model, got %q llm=%#v", got, llm)
	}
	if got := strings.TrimSpace(anyToString(llm["deprecated_model_replaced"])); got != "qwen2.5-coder:7b" {
		t.Fatalf("expected deprecated model replacement, got %#v", llm)
	}
	if int(anyToFloat(llm["timeout_secs"])) != 7 || int(anyToFloat(llm["max_tokens"])) != 123 {
		t.Fatalf("expected patient llm controls in response, got %#v", llm)
	}
	if got := strings.TrimSpace(anyToString(chatPayload["model"])); got != "qwen3.6:35b-a3b" {
		t.Fatalf("expected ollama request to use selected model, got %q payload=%#v", got, chatPayload)
	}
	options, _ := chatPayload["options"].(map[string]any)
	if int(anyToFloat(options["num_predict"])) != 123 {
		t.Fatalf("expected ollama num_predict=123, got %#v", options)
	}
	if temp := anyToFloat(options["temperature"]); temp < 0.69 || temp > 0.71 {
		t.Fatalf("expected ollama temperature=0.7, got %#v", options)
	}
}

func TestMemoryDreamRejectsMissingGoalWithInstructions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("missing dream goal should not call backend path %s", r.URL.Path)
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/memory/dream", "application/json", strings.NewReader(`{"project":"contextlattice"}`))
	if err != nil {
		t.Fatalf("dream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 422, got %d body=%s", resp.StatusCode, string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode dream error payload: %v", err)
	}
	if strings.TrimSpace(anyToString(payload["error"])) != "goal_or_query_required" {
		t.Fatalf("expected goal_or_query_required, got %#v", payload)
	}
	if mode := strings.TrimSpace(anyToString(payload["mode"])); mode != "dream_unavailable" {
		t.Fatalf("expected dream_unavailable mode, got %q payload=%#v", mode, payload)
	}
	if anyToBool(payload["dream_available"]) {
		t.Fatalf("expected dream_available=false, got %#v", payload)
	}
	if source := strings.TrimSpace(anyToString(payload["intelligence_source"])); source != "none" {
		t.Fatalf("expected no intelligence source, got %q payload=%#v", source, payload)
	}
	if !strings.Contains(anyToString(payload["instructions"]), "goal or query") {
		t.Fatalf("expected repair instructions, got %#v", payload["instructions"])
	}
	assertBoundaryContractPassed(t, dreamModeResponseContractID, payload)
}

func TestMemoryContextPackBlocksForConfiguredSlowSourcesByDefault(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant,mindsdb")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb")
	t.Setenv("GO_RETRIEVAL_DISABLE_SYNC_SLOW_FALLBACK", "false")
	t.Setenv("MINDSDB_ENABLED", "false")

	var mu sync.Mutex
	calledSources := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
		w.WriteHeader(http.StatusOK)
		switch source {
		case "qdrant":
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"fast.md","summary":"fast source fact","score":0.9,"source":"qdrant"}],"warnings":[]}`))
		case "mindsdb":
			_, _ = w.Write([]byte(`{"results":[{"project":"alpha","file":"slow.md","summary":"slow source runbook fact","score":0.8,"source":"mindsdb","topic_path":"runbooks/slow"}],"warnings":[]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
		}
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"alpha","query":"combined context pack","retrieval_mode":"balanced","limit":5,"include_retrieval_debug":true}`
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
	mu.Lock()
	sort.Strings(calledSources)
	observed := strings.Join(calledSources, ",")
	mu.Unlock()
	if observed != "mindsdb,qdrant" {
		t.Fatalf("expected context-pack to synchronously query fast and slow sources, got %s", observed)
	}
	coverage, _ := payload["source_coverage"].(map[string]any)
	returned := anyToStringList(coverage["returned"], 20)
	sort.Strings(returned)
	if strings.Join(returned, ",") != "mindsdb,qdrant" {
		t.Fatalf("expected coverage returned mindsdb,qdrant, got %#v", coverage)
	}
	if complete, _ := coverage["complete"].(bool); !complete {
		t.Fatalf("expected complete source coverage, got %#v", coverage)
	}
}

func TestProxyForwardsBatchAndOpsQueuePaths(t *testing.T) {
	var capturedPath string
	var capturedBody string
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls += 1
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

	t.Setenv("ORCH_RECALL_EVAL_CASES_PATH", filepath.Join(t.TempDir(), "recall_eval_cases.json"))
	backendCallsBeforeRecallRefresh := backendCalls
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
	if backendCalls != backendCallsBeforeRecallRefresh {
		t.Fatalf("expected /memory/recall/eval-cases/refresh to stay go-native, backend calls before=%d after=%d", backendCallsBeforeRecallRefresh, backendCalls)
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

	backendCallsBeforeFeedback := backendCalls
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
	if backendCalls != backendCallsBeforeFeedback {
		t.Fatalf("expected /feedback to stay go-native, backend calls before=%d after=%d", backendCallsBeforeFeedback, backendCalls)
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

	backendCallsBeforeTelemetry := backendCalls
	resp10, err := http.Get(gateway.URL + "/telemetry/recall")
	if err != nil {
		t.Fatalf("telemetry request failed: %v", err)
	}
	defer resp10.Body.Close()
	if resp10.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /telemetry/recall, got %d", resp10.StatusCode)
	}
	backendCallsAfterRecall := backendCalls
	if backendCallsAfterRecall != backendCallsBeforeTelemetry {
		t.Fatalf("expected /telemetry/recall to stay go-native, backend calls before=%d after=%d", backendCallsBeforeTelemetry, backendCallsAfterRecall)
	}

	resp10b, err := http.Get(gateway.URL + "/telemetry/fanout")
	if err != nil {
		t.Fatalf("fanout telemetry request failed: %v", err)
	}
	defer resp10b.Body.Close()
	if resp10b.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /telemetry/fanout, got %d", resp10b.StatusCode)
	}
	if backendCalls != backendCallsAfterRecall {
		t.Fatalf("expected /telemetry/fanout to stay go-native, backend calls before=%d after=%d", backendCallsAfterRecall, backendCalls)
	}

	resp10c, err := http.Get(gateway.URL + "/telemetry/tools/invocations")
	if err != nil {
		t.Fatalf("tools telemetry request failed: %v", err)
	}
	defer resp10c.Body.Close()
	if resp10c.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /telemetry/tools/invocations, got %d", resp10c.StatusCode)
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

	backendCallsBeforeMemoryTelemetry := backendCalls
	memoryResp, err := http.Get(gateway.URL + "/telemetry/memory")
	if err != nil {
		t.Fatalf("memory telemetry failed: %v", err)
	}
	defer memoryResp.Body.Close()
	if memoryResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(memoryResp.Body)
		t.Fatalf("expected 200 for /telemetry/memory, got %d body=%s", memoryResp.StatusCode, string(body))
	}
	var memoryPayload map[string]any
	if err := json.NewDecoder(memoryResp.Body).Decode(&memoryPayload); err != nil {
		t.Fatalf("decode memory telemetry payload: %v", err)
	}
	if !anyToBool(memoryPayload["ok"]) {
		t.Fatalf("expected ok memory telemetry payload, got %#v", memoryPayload)
	}
	if anyToString(memoryPayload["runtimeOwner"]) != sourceOwnerGoNative {
		t.Fatalf("expected go-native memory telemetry owner, got %#v", memoryPayload["runtimeOwner"])
	}
	if !anyToBool(memoryPayload["strictRuntimeCompatible"]) {
		t.Fatalf("expected strict-runtime-compatible memory telemetry payload, got %#v", memoryPayload)
	}
	memoryBank, _ := memoryPayload["memoryBank"].(map[string]any)
	if anyToInt(memoryBank["queueDepth"], -1) != 3 {
		t.Fatalf("expected memory telemetry queue depth to reflect native metrics snapshot, got %#v", memoryBank)
	}
	fanoutMemory, _ := memoryPayload["fanout"].(map[string]any)
	targets, _ := fanoutMemory["targets"].(map[string]any)
	if targets[sourceQdrant] == nil || targets[sourcePgvector] == nil {
		t.Fatalf("expected native vector fanout targets in memory telemetry, got %#v", fanoutMemory)
	}
	if backendCalls != backendCallsBeforeMemoryTelemetry {
		t.Fatalf("expected /telemetry/memory to stay native in strict runtime, backend calls before=%d after=%d", backendCallsBeforeMemoryTelemetry, backendCalls)
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

	recallResp, err := http.Get(gateway.URL + "/telemetry/recall?traffic_class=user")
	if err != nil {
		t.Fatalf("recall telemetry failed: %v", err)
	}
	defer recallResp.Body.Close()
	if recallResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(recallResp.Body)
		t.Fatalf("expected 200 for /telemetry/recall, got %d body=%s", recallResp.StatusCode, string(body))
	}
	var recallPayload map[string]any
	if err := json.NewDecoder(recallResp.Body).Decode(&recallPayload); err != nil {
		t.Fatalf("decode recall payload: %v", err)
	}
	if recallPayload["quality"] == nil {
		t.Fatalf("expected recall quality payload, got %#v", recallPayload)
	}

	monitorResp, err := http.Get(gateway.URL + "/telemetry/recall/monitor?limit=8")
	if err != nil {
		t.Fatalf("recall monitor telemetry failed: %v", err)
	}
	defer monitorResp.Body.Close()
	if monitorResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(monitorResp.Body)
		t.Fatalf("expected 200 for /telemetry/recall/monitor, got %d body=%s", monitorResp.StatusCode, string(body))
	}
	var monitorPayload map[string]any
	if err := json.NewDecoder(monitorResp.Body).Decode(&monitorPayload); err != nil {
		t.Fatalf("decode recall monitor payload: %v", err)
	}
	monitorRows, _ := monitorPayload["history"].([]any)
	if len(monitorRows) == 0 {
		t.Fatalf("expected recall monitor history entries, got %#v", monitorPayload)
	}

	fanoutResp, err := http.Get(gateway.URL + "/telemetry/fanout")
	if err != nil {
		t.Fatalf("fanout telemetry failed: %v", err)
	}
	defer fanoutResp.Body.Close()
	if fanoutResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(fanoutResp.Body)
		t.Fatalf("expected 200 for /telemetry/fanout, got %d body=%s", fanoutResp.StatusCode, string(body))
	}
	var fanoutPayload map[string]any
	if err := json.NewDecoder(fanoutResp.Body).Decode(&fanoutPayload); err != nil {
		t.Fatalf("decode fanout payload: %v", err)
	}
	fanoutSummary, _ := fanoutPayload["summary"].(map[string]any)
	fanoutByStatus, _ := fanoutSummary["by_status"].(map[string]any)
	if fanoutByStatus == nil {
		t.Fatalf("expected fanout by_status payload, got %#v", fanoutPayload)
	}

	sidecarResp, err := http.Get(gateway.URL + "/telemetry/sidecar-health")
	if err != nil {
		t.Fatalf("sidecar health telemetry failed: %v", err)
	}
	defer sidecarResp.Body.Close()
	if sidecarResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(sidecarResp.Body)
		t.Fatalf("expected 200 for /telemetry/sidecar-health, got %d body=%s", sidecarResp.StatusCode, string(body))
	}
	var sidecarPayload map[string]any
	if err := json.NewDecoder(sidecarResp.Body).Decode(&sidecarPayload); err != nil {
		t.Fatalf("decode sidecar health payload: %v", err)
	}
	if anyToString(sidecarPayload["runtimeOwner"]) != sourceOwnerGoNative {
		t.Fatalf("expected go-native sidecar health telemetry owner, got %#v", sidecarPayload)
	}
	if sidecarPayload["gateway-go"] == nil {
		t.Fatalf("expected gateway-go sidecar health row, got %#v", sidecarPayload)
	}

	strategyResp, err := http.Get(gateway.URL + "/telemetry/strategies")
	if err != nil {
		t.Fatalf("strategy telemetry failed: %v", err)
	}
	defer strategyResp.Body.Close()
	if strategyResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(strategyResp.Body)
		t.Fatalf("expected 200 for /telemetry/strategies, got %d body=%s", strategyResp.StatusCode, string(body))
	}
	var strategyPayload map[string]any
	if err := json.NewDecoder(strategyResp.Body).Decode(&strategyPayload); err != nil {
		t.Fatalf("decode strategy telemetry payload: %v", err)
	}
	if anyToString(strategyPayload["runtimeOwner"]) != sourceOwnerGoNative {
		t.Fatalf("expected go-native strategy telemetry owner, got %#v", strategyPayload)
	}
	if _, ok := strategyPayload["strategies"].([]any); !ok {
		t.Fatalf("expected strategy telemetry strategies array, got %#v", strategyPayload)
	}

	strategyHistoryResp, err := http.Get(gateway.URL + "/telemetry/strategies/history?limit=5")
	if err != nil {
		t.Fatalf("strategy history telemetry failed: %v", err)
	}
	defer strategyHistoryResp.Body.Close()
	if strategyHistoryResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(strategyHistoryResp.Body)
		t.Fatalf("expected 200 for /telemetry/strategies/history, got %d body=%s", strategyHistoryResp.StatusCode, string(body))
	}
	var strategyHistoryPayload map[string]any
	if err := json.NewDecoder(strategyHistoryResp.Body).Decode(&strategyHistoryPayload); err != nil {
		t.Fatalf("decode strategy history payload: %v", err)
	}
	if anyToString(strategyHistoryPayload["runtimeOwner"]) != sourceOwnerGoNative {
		t.Fatalf("expected go-native strategy history owner, got %#v", strategyHistoryPayload)
	}
	if _, ok := strategyHistoryPayload["history"].([]any); !ok {
		t.Fatalf("expected strategy history array, got %#v", strategyHistoryPayload)
	}

	ownershipResp, err := http.Get(gateway.URL + "/ops/native-ownership")
	if err != nil {
		t.Fatalf("native ownership audit failed: %v", err)
	}
	defer ownershipResp.Body.Close()
	if ownershipResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(ownershipResp.Body)
		t.Fatalf("expected 200 for /ops/native-ownership, got %d body=%s", ownershipResp.StatusCode, string(body))
	}
	var ownershipPayload map[string]any
	if err := json.NewDecoder(ownershipResp.Body).Decode(&ownershipPayload); err != nil {
		t.Fatalf("decode native ownership payload: %v", err)
	}
	if !anyToBool(ownershipPayload["ok"]) || anyToInt(ownershipPayload["violationCount"], -1) != 0 {
		t.Fatalf("expected clean native ownership payload, got %#v", ownershipPayload)
	}

	boundaryResp, err := http.Get(gateway.URL + "/ops/context-boundary")
	if err != nil {
		t.Fatalf("context boundary audit failed: %v", err)
	}
	defer boundaryResp.Body.Close()
	if boundaryResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(boundaryResp.Body)
		t.Fatalf("expected 200 for /ops/context-boundary, got %d body=%s", boundaryResp.StatusCode, string(body))
	}
	var boundaryPayload map[string]any
	if err := json.NewDecoder(boundaryResp.Body).Decode(&boundaryPayload); err != nil {
		t.Fatalf("decode context boundary payload: %v", err)
	}
	if !anyToBool(boundaryPayload["ok"]) || anyToInt(boundaryPayload["violationCount"], -1) != 0 {
		t.Fatalf("expected clean context boundary payload, got %#v", boundaryPayload)
	}

	toolsResp, err := http.Get(gateway.URL + "/telemetry/tools/invocations?limit=10&status_min=400")
	if err != nil {
		t.Fatalf("tools telemetry failed: %v", err)
	}
	defer toolsResp.Body.Close()
	if toolsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(toolsResp.Body)
		t.Fatalf("expected 200 for /telemetry/tools/invocations, got %d body=%s", toolsResp.StatusCode, string(body))
	}
	var toolsPayload map[string]any
	if err := json.NewDecoder(toolsResp.Body).Decode(&toolsPayload); err != nil {
		t.Fatalf("decode tools telemetry payload: %v", err)
	}
	if toolsPayload["items"] == nil {
		t.Fatalf("expected tools telemetry items payload, got %#v", toolsPayload)
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
	t.Setenv("GO_PAID_ENTITLEMENT_MODE", "enforce")
	t.Setenv("GO_PAID_ENTITLEMENT_DEV_ALLOW", "false")
	t.Setenv("GO_PAID_ENTITLEMENT_PROTECTED_PATHS", "/maintenance/diagnostics")
	t.Setenv("GO_PAID_ENTITLEMENT_ALLOWED_PLANS", "team,enterprise")
	t.Setenv("GO_PAID_ENTITLEMENT_ALLOWED_ROLES", "owner,admin")

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

func assertPolicyContextIncludesAntiScheming(t *testing.T, raw any) {
	t.Helper()
	policy, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expected policy_context_package object, got %#v", raw)
	}
	contract, ok := policy["policy_contract"].(map[string]any)
	if !ok {
		t.Fatalf("expected policy_contract object, got %#v", policy["policy_contract"])
	}
	if !anyToBool(contract["anti_scheming_required"]) {
		t.Fatalf("expected anti_scheming_required=true, got %#v", contract["anti_scheming_required"])
	}
	if !anyToBool(contract["objective_runtime_required"]) {
		t.Fatalf("expected objective_runtime_required=true, got %#v", contract["objective_runtime_required"])
	}
	assertObjectiveRuntimeContractPassed(t, policy["objective_runtime"])
	protocol, ok := policy["anti_scheming_protocol"].(map[string]any)
	if !ok {
		t.Fatalf("expected anti_scheming_protocol object, got %#v", policy["anti_scheming_protocol"])
	}
	law := strings.TrimSpace(anyToString(protocol["law"]))
	if !strings.Contains(law, "Change conclusions to match evidence") {
		t.Fatalf("unexpected anti-scheming law: %q", law)
	}
	steps, ok := protocol["required_steps"].([]any)
	if !ok || len(steps) == 0 {
		t.Fatalf("expected anti-scheming required_steps, got %#v", protocol["required_steps"])
	}
	handoff, ok := policy["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("expected handoff object, got %#v", policy["handoff"])
	}
	if !strings.Contains(anyToString(handoff["handoff_prompt"]), "change conclusions to match evidence") {
		t.Fatalf("handoff prompt missing anti-scheming instruction: %#v", handoff["handoff_prompt"])
	}
	format, ok := policy["format_contract"].(map[string]any)
	if !ok {
		t.Fatalf("expected format_contract object, got %#v", policy["format_contract"])
	}
	if strings.TrimSpace(anyToString(format["schema_id"])) != policyContextPackageContractID {
		t.Fatalf("unexpected policy format contract schema_id: %#v", format["schema_id"])
	}
	validation, ok := format["validation"].(map[string]any)
	if !ok || strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected policy format validation passed, got %#v", format["validation"])
	}
}

func assertObjectiveRuntimeContractPassed(t *testing.T, raw any) {
	t.Helper()
	runtime, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("expected objective_runtime object, got %#v", raw)
	}
	format, ok := runtime["format_contract"].(map[string]any)
	if !ok {
		t.Fatalf("expected objective_runtime format_contract object, got %#v", runtime["format_contract"])
	}
	if strings.TrimSpace(anyToString(format["schema_id"])) != objectiveRuntimeStateContractID {
		t.Fatalf("unexpected objective_runtime schema_id: %#v", format["schema_id"])
	}
	validation, ok := format["validation"].(map[string]any)
	if !ok || strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected objective_runtime validation passed, got %#v", format["validation"])
	}
	for _, key := range []string{"objective_state", "action_executed", "evidence", "objective_delta", "risk_or_blocker", "next_action"} {
		if _, ok := runtime[key]; !ok {
			t.Fatalf("objective_runtime missing %s: %#v", key, runtime)
		}
	}
}

func TestCodexPreflightBroadensScopeAndRequestsContextPack(t *testing.T) {
	searchCalls := 0
	contextPackCalls := 0
	missionContextCalls := 0
	contextPackControlChecks := 0
	const missionQuery = "mission objective goal cross-project synthesis longitudinal learning policy context package retrieval discipline"
	activeSkillRoot := filepath.Join(t.TempDir(), "skills_active")
	writeSkillIndexFixture(t, activeSkillRoot, "boundary-graph-gate", "boundary-graph-gate", "Use when shipping a boundary graph gate for agent handoff protection.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", activeSkillRoot)
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
			for _, key := range []string{"blocking", "wait_for_slow_sources", "sync_slow_sources", "combined_sources"} {
				value, present := payload[key]
				if !present || anyToBool(value) {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"missing nonblocking control"}`))
					return
				}
			}
			contextPackControlChecks += 1
			if strings.TrimSpace(anyToString(payload["query"])) == missionQuery {
				missionContextCalls += 1
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"context_pack":{"facts":[{"text":"mission_f1"}],"results":[{"file":"_rollups/topics/mission.json"}]}}`))
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

	reqBody := `{"project":"contextlattice","topic_path":"runbooks/codex-integration","query":"codex preflight","agent_id":"codex_gpt5_test","mission":"compound release evidence","objective":"ship boundary graph gate","goal":"protect every agent handoff","blocking":false,"wait_for_slow_sources":false,"sync_slow_sources":false,"combined_sources":false}`
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
	if contextPackCalls != 2 {
		t.Fatalf("expected two context-pack calls (primary+mission), got %d", contextPackCalls)
	}
	if missionContextCalls != 1 {
		t.Fatalf("expected one mission context-pack call, got %d", missionContextCalls)
	}
	if contextPackControlChecks != 2 {
		t.Fatalf("expected nonblocking controls on primary and mission context-pack calls, got %d", contextPackControlChecks)
	}
	if payload["broadened_search"] == nil {
		t.Fatalf("expected broadened_search payload, got nil")
	}
	if payload["context_pack"] == nil {
		t.Fatalf("expected context_pack payload, got nil")
	}
	if payload["mission_context_pack"] == nil {
		t.Fatalf("expected mission_context_pack payload, got nil")
	}
	if payload["policy_context_package"] == nil {
		t.Fatalf("expected policy_context_package payload, got nil")
	}
	skillsIndex, _ := payload["skills_index"].(map[string]any)
	skillResults, _ := skillsIndex["results"].([]any)
	if len(skillResults) == 0 {
		t.Fatalf("expected preflight skills_index recommendations, got %#v", skillsIndex)
	}
	firstSkill, _ := skillResults[0].(map[string]any)
	if strings.TrimSpace(anyToString(firstSkill["name"])) != "boundary-graph-gate" {
		t.Fatalf("expected boundary graph skill recommendation, got %#v", firstSkill)
	}
	assertObjectiveRuntimeContractPassed(t, payload["objective_runtime"])
	assertPolicyContextIncludesAntiScheming(t, payload["policy_context_package"])
	objectiveContext, ok := payload["objective_context"].(map[string]any)
	if !ok || strings.TrimSpace(anyToString(objectiveContext["objective"])) != "ship boundary graph gate" {
		t.Fatalf("expected preflight objective_context override, got %#v", payload["objective_context"])
	}
	policyContext, _ := payload["policy_context_package"].(map[string]any)
	if strings.TrimSpace(anyToString(policyContext["mission"])) != "compound release evidence" ||
		strings.TrimSpace(anyToString(policyContext["objective"])) != "ship boundary graph gate" ||
		strings.TrimSpace(anyToString(policyContext["goal"])) != "protect every agent handoff" {
		t.Fatalf("expected policy package to mirror requested mission/objective/goal, got %#v", policyContext)
	}
	contracts, ok := payload["format_contracts"].(map[string]any)
	if !ok {
		t.Fatalf("expected format_contracts object, got %#v", payload["format_contracts"])
	}
	validation, _ := contracts["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected preflight format validation passed, got %#v", validation)
	}
}

func TestCodexPreflightStrictRuntimeIncludesSkillsIndex(t *testing.T) {
	activeSkillRoot := filepath.Join(t.TempDir(), "skills_active")
	writeSkillIndexFixture(t, activeSkillRoot, "objective-loop", "objective-loop", "Use when an agent needs to stay focused through an objective loop.")
	t.Setenv("ORCH_SKILLS_INDEX_ROOTS", activeSkillRoot)
	t.Setenv("BACKEND_URL", "http://127.0.0.1:1")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "false")
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "memory_bank")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "memory_bank")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_MEMORY_BANK_SEARCH_BACKEND", "disabled")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"project":"contextlattice","topic_path":"runbooks/codex-integration","query":"objective loop adoption","agent_id":"codex_gpt5_test","objective":"use objective loop skills","blocking":false,"wait_for_slow_sources":false,"sync_slow_sources":false,"combined_sources":false}`
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
	skillsIndex, _ := payload["skills_index"].(map[string]any)
	skillResults, _ := skillsIndex["results"].([]any)
	if len(skillResults) == 0 {
		t.Fatalf("expected strict preflight skills_index recommendations, got %#v", skillsIndex)
	}
	firstSkill, _ := skillResults[0].(map[string]any)
	if strings.TrimSpace(anyToString(firstSkill["name"])) != "objective-loop" {
		t.Fatalf("expected objective-loop skill recommendation, got %#v", firstSkill)
	}
	if strings.TrimSpace(anyToString(firstSkill["source"])) != "active" {
		t.Fatalf("expected active skill source, got %#v", firstSkill)
	}
	contracts, ok := payload["format_contracts"].(map[string]any)
	if !ok {
		t.Fatalf("expected format_contracts object, got %#v", payload["format_contracts"])
	}
	validation, _ := contracts["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected preflight format validation passed, got %#v", validation)
	}
}

func TestAgentsPreflightUsesNamedProfileDefaults(t *testing.T) {
	searchCalls := 0
	contextPackCalls := 0
	missionContextCalls := 0
	const missionQuery = "mission objective goal cross-project synthesis longitudinal learning policy context package retrieval discipline"
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
			if strings.TrimSpace(anyToString(payload["query"])) == missionQuery {
				missionContextCalls += 1
				if strings.TrimSpace(anyToString(payload["topic_path"])) != "runbooks/context-policy" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"unexpected mission topic path"}`))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"context_pack":{"facts":[{"text":"mission_f1"}],"results":[{"file":"_rollups/topics/mission.json"}]}}`))
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
	if contextPackCalls != 2 {
		t.Fatalf("expected two context-pack calls (primary+mission), got %d", contextPackCalls)
	}
	if missionContextCalls != 1 {
		t.Fatalf("expected one mission context-pack call, got %d", missionContextCalls)
	}
	if payload["broadened_search"] == nil {
		t.Fatalf("expected broadened_search payload, got nil")
	}
	if payload["context_pack"] == nil {
		t.Fatalf("expected context_pack payload, got nil")
	}
	if payload["mission_context_pack"] == nil {
		t.Fatalf("expected mission_context_pack payload, got nil")
	}
	if payload["policy_context_package"] == nil {
		t.Fatalf("expected policy_context_package payload, got nil")
	}
	assertPolicyContextIncludesAntiScheming(t, payload["policy_context_package"])
	contracts, ok := payload["format_contracts"].(map[string]any)
	if !ok {
		t.Fatalf("expected format_contracts object, got %#v", payload["format_contracts"])
	}
	validation, _ := contracts["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected preflight format validation passed, got %#v", validation)
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
	if len(timedOut) != 0 {
		t.Fatalf("expected agent-facing timed_out_sources to exclude warming continuation sources, got %v", timedOut)
	}
	deferredTimeouts := anyToStringSlice(summary["deferred_timeout_sources"])
	if len(deferredTimeouts) != 1 || deferredTimeouts[0] != "topic_rollups" {
		t.Fatalf("expected deferred_timeout_sources=[topic_rollups], got %v", deferredTimeouts)
	}
	syncTimeouts := anyToStringSlice(summary["sync_timed_out_sources"])
	if len(syncTimeouts) != 1 || syncTimeouts[0] != "topic_rollups" {
		t.Fatalf("expected sync_timed_out_sources=[topic_rollups], got %v", syncTimeouts)
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

func TestStagedRetrievalFastTimeoutQueuedForContinuationIsPending(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_TIMEOUT_SECS", "0.2")
	t.Setenv("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_SECS", "0.2")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES", "qdrant")
	t.Setenv("GO_RETRIEVAL_PROTECTED_SOURCES", "qdrant")

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
		if len(sources) > 0 && strings.TrimSpace(strings.ToLower(anyToString(sources[0]))) == "qdrant" {
			time.Sleep(450 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	reqBody := `{"request":{"query":"slow protected vector source","limit":5,"retrieval_mode":"balanced","sources":["qdrant"]}}`
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
	if got := strings.TrimSpace(strings.ToLower(anyToString(payload["result_state"]))); got != "pending" {
		t.Fatalf("expected result_state=pending for warming continuation source, got %q payload=%#v", got, payload)
	}
	if degraded, _ := payload["degraded"].(bool); degraded {
		t.Fatalf("expected degraded=false while qdrant continuation is warming")
	}

	lifecycle, _ := payload["retrieval_lifecycle"].(map[string]any)
	if got := strings.TrimSpace(strings.ToLower(anyToString(lifecycle["status"]))); got != "partial" {
		t.Fatalf("expected lifecycle status partial, got %q lifecycle=%#v", got, lifecycle)
	}
	if got := strings.TrimSpace(strings.ToLower(anyToString(lifecycle["result_state"]))); got != "pending" {
		t.Fatalf("expected lifecycle result_state pending, got %q lifecycle=%#v", got, lifecycle)
	}

	summary, _ := payload["source_summary"].(map[string]any)
	if timedOut := anyToStringSlice(summary["timed_out_sources"]); len(timedOut) != 0 {
		t.Fatalf("expected no terminal timed_out_sources while qdrant is warming, got %v", timedOut)
	}
	deferredTimeouts := anyToStringSlice(summary["deferred_timeout_sources"])
	if len(deferredTimeouts) != 1 || deferredTimeouts[0] != "qdrant" {
		t.Fatalf("expected deferred_timeout_sources=[qdrant], got %v", deferredTimeouts)
	}
	if failed := anyToStringSlice(summary["failed_sources"]); len(failed) != 0 {
		t.Fatalf("expected no terminal failed_sources while qdrant is warming, got %v", failed)
	}
	deferredFailed := anyToStringSlice(summary["deferred_failed_sources"])
	if len(deferredFailed) != 1 || deferredFailed[0] != "qdrant" {
		t.Fatalf("expected deferred_failed_sources=[qdrant], got %v", deferredFailed)
	}

	continuation, _ := payload["continuation_async"].(map[string]any)
	if continuation == nil {
		t.Fatalf("expected continuation_async for qdrant timeout")
	}
	pending := anyToStringSlice(continuation["pending_sources"])
	if len(pending) != 1 || pending[0] != "qdrant" {
		t.Fatalf("expected continuation pending source qdrant, got %v", pending)
	}
	progress := anyMap(continuation["retrieval_progress"])
	visibility := anyMap(progress["agent_visibility"])
	if got := anyToString(visibility["session_event_type"]); got != "retrieval.continuation.progress" {
		t.Fatalf("expected progress event type while qdrant is warming, got %#v", visibility)
	}
}

func TestStagedRetrievalTopicRollupsNoLexicalMatchDoesNotFallback(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", "2")
	t.Setenv("GO_RETRIEVAL_NON_DEGRADABLE_SOURCES", "topic_rollups")
	t.Setenv("GO_RETRIEVAL_PROTECTED_SOURCES", "topic_rollups")
	t.Setenv("BACKEND_URL", "http://127.0.0.1:9")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.URL.Path == "/v1/retrieval/query" {
			backendCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":[],"warnings":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	writeBody := `{"projectName":"alpha","fileName":"notes/rollup-smoke.md","content":"profitability baseline ladder tuning","topicPath":"runbooks/profitability"}`
	writeResp, err := http.Post(gateway.URL+"/memory/write", "application/json", strings.NewReader(writeBody))
	if err != nil {
		t.Fatalf("write request failed: %v", err)
	}
	defer writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(writeResp.Body)
		t.Fatalf("expected write status 200, got %d body=%s", writeResp.StatusCode, string(body))
	}

	queryBody := `{"request":{"query":"unrelated lexical needle","limit":5,"retrieval_mode":"fast","sources":["topic_rollups"]}}`
	resp, err := http.Post(gateway.URL+"/v1/retrieval/query", "application/json", strings.NewReader(queryBody))
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

	if backendCalls != 0 {
		t.Fatalf("expected zero backend fallback calls on topic_rollups lexical miss, got %d", backendCalls)
	}
	rows, _ := payload["results"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected empty results on lexical miss, got %#v", rows)
	}
	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if strings.Contains(warnings, "topic_rollups go-adapter fallback to backend retrieval lane") {
		t.Fatalf("unexpected topic_rollups fallback warning: %v", payload["warnings"])
	}
	if strings.Contains(warnings, "python backend fallback disabled for source topic_rollups") {
		t.Fatalf("unexpected python fallback warning on lexical miss: %v", payload["warnings"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	errorsMap, _ := debug["source_errors"].(map[string]any)
	if _, exists := errorsMap["topic_rollups"]; exists {
		t.Fatalf("expected no source_errors.topic_rollups on lexical miss, got %#v", errorsMap["topic_rollups"])
	}
}

func TestStagedRetrievalTopicRollupsEmptyStoreIsNoData(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_RETRIEVAL_NON_DEGRADABLE_SOURCES", "topic_rollups")
	t.Setenv("GO_RETRIEVAL_PROTECTED_SOURCES", "topic_rollups")
	t.Setenv("GATEWAY_PROXY_TIMEOUT_SECS", "2")
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	t.Setenv("GO_MEMORY_STORE_ENABLED", "true")
	t.Setenv("GO_MEMORY_STORE_ROOT", t.TempDir())
	t.Setenv("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", "false")
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")

	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/retrieval/query" {
			backendCalls++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	t.Setenv("BACKEND_URL", backend.URL)

	s := newServer()
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	resp, err := http.Post(
		gateway.URL+"/v1/retrieval/query",
		"application/json",
		strings.NewReader(`{"request":{"query":"empty topic rollup smoke","limit":5,"retrieval_mode":"fast","sources":["topic_rollups"]}}`),
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
	if backendCalls != 0 {
		t.Fatalf("expected zero backend fallback calls on empty topic rollup store, got %d", backendCalls)
	}
	rows, _ := payload["results"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected empty results on empty topic rollup store, got %#v", rows)
	}
	warnings := strings.ToLower(strings.Join(parseWarnings(payload["warnings"]), " | "))
	if strings.Contains(warnings, "topic_rollups go-adapter fallback to backend retrieval lane") {
		t.Fatalf("unexpected topic_rollups fallback warning: %v", payload["warnings"])
	}
	debug, _ := payload["retrieval_debug"].(map[string]any)
	errorsMap, _ := debug["source_errors"].(map[string]any)
	if _, exists := errorsMap["topic_rollups"]; exists {
		t.Fatalf("expected no source_errors.topic_rollups on empty topic rollup store, got %#v", errorsMap["topic_rollups"])
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

func TestContinuationStatusPayloadIncludesModeledProgress(t *testing.T) {
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
	token := "status-progress-token"
	s.publishContinuationEvent(token, map[string]any{
		"event":  "queued",
		"status": "queued",
		"source": "letta",
	})
	s.publishContinuationEvent(token, map[string]any{
		"event":  "completed",
		"status": "ok",
		"source": "topic_rollups",
	})

	payload, ok := s.continuationStatusPayload(token, true)
	if !ok {
		t.Fatalf("expected continuation status payload for token %s", token)
	}
	progress, ok := payload["modeled_progress"].(map[string]any)
	if !ok {
		t.Fatalf("expected modeled_progress map on payload, got %T", payload["modeled_progress"])
	}
	if !anyToBool(progress["probabilistic"]) {
		t.Fatalf("expected probabilistic modeled progress, got %#v", progress)
	}
	if anyToFloat64(progress["progress_pct"], 0) <= 0 {
		t.Fatalf("expected positive progress_pct, got %#v", progress)
	}
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result payload, got %T", payload["result"])
	}
	resultProgress, ok := result["modeled_progress"].(map[string]any)
	if !ok {
		t.Fatalf("expected modeled_progress on result payload, got %T", result["modeled_progress"])
	}
	if anyToString(resultProgress["confidence_band"]) == "" {
		t.Fatalf("expected confidence_band in modeled progress, got %#v", resultProgress)
	}
	retrievalProgress := anyMap(payload["retrieval_progress"])
	if anyToString(retrievalProgress["schema_id"]) != retrievalProgressContractID {
		t.Fatalf("expected retrieval progress contract payload, got %#v", retrievalProgress)
	}
	progressValidation := anyMap(anyMap(retrievalProgress["format_contract"])["validation"])
	if anyToString(progressValidation["status"]) != "passed" {
		t.Fatalf("expected retrieval progress validation passed, got %#v", retrievalProgress["format_contract"])
	}
	visibility := anyMap(retrievalProgress["agent_visibility"])
	if !strings.Contains(anyToString(visibility["watch_command"]), "contextlattice_agent_session watch") {
		t.Fatalf("expected agent watch command, got %#v", visibility)
	}
	if anyToString(visibility["session_event_type"]) != "retrieval.continuation.progress" {
		t.Fatalf("expected continuation progress event type, got %#v", visibility)
	}
}

func TestContinuationSteeringWritesAgentSessionInbox(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "agent_sessions.json"))
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
	sessionID := "sess-continuation-steering"
	token := "cont-steering-token"
	if _, err := s.agentSessions.start(map[string]any{
		"session_id": sessionID,
		"agent":      "codex",
		"agent_id":   "codex_gpt5_test",
		"project":    "contextlattice",
		"objective":  "prove async continuation steering reaches the requesting agent",
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	request := map[string]any{
		"session_id": sessionID,
		"agent_id":   "codex_gpt5_test",
		"project":    "contextlattice",
		"topic_path": "contextlattice/async-continuation-agent-steering",
		"query":      "async continuation steering proof",
	}
	s.publishContinuationEvent(token, continuationEventWithRequest(request, map[string]any{
		"event":  "queued",
		"status": "queued",
		"source": sourceLetta,
	}))
	completed := continuationEventWithRequest(request, map[string]any{
		"event":      "completed",
		"status":     "ok",
		"source":     sourceLetta,
		"reason":     "test",
		"latency_ms": 4,
	})
	s.publishContinuationEvent(token, completed)
	s.emitContinuationSteering(request, token, sourceLetta, completed)

	session, events, ok := s.agentSessions.get(sessionID)
	if !ok {
		t.Fatalf("expected session %s", sessionID)
	}
	rollup := buildAgentSessionRollup(session, events, time.Now().UTC())
	inbox := anyMap(rollup["agent_inbox"])
	latest := anyMap(inbox["latest"])
	if anyToString(latest["type"]) != "retrieval.continuation.ready" {
		t.Fatalf("expected ready steering event in inbox, got %#v", latest)
	}
	if !strings.Contains(anyToString(latest["message"]), "Async retrieval is ready") {
		t.Fatalf("expected ready steering message, got %#v", latest)
	}
	delivery := anyMap(latest["delivery"])
	if !strings.Contains(anyToString(delivery["watch_command"]), "--session-id "+sessionID) {
		t.Fatalf("expected session-specific watch command, got %#v", delivery)
	}
	rollupValidation := anyMap(anyMap(rollup["format_contract"])["validation"])
	if anyToString(rollupValidation["status"]) != "passed" {
		t.Fatalf("expected rollup validation passed, got %#v", rollup["format_contract"])
	}

	promptPackage := buildAgentPromptContextPackage(session, events, time.Now().UTC())
	if !strings.Contains(anyToString(promptPackage["reference_prompt"]), "Latest agent steering: Async retrieval is ready") {
		t.Fatalf("expected latest steering in reference prompt, got %q", anyToString(promptPackage["reference_prompt"]))
	}
	promptValidation := anyMap(anyMap(promptPackage["format_contract"])["validation"])
	if anyToString(promptValidation["status"]) != "passed" {
		t.Fatalf("expected prompt context package validation passed, got %#v", promptPackage["format_contract"])
	}

	trace := buildAgentRunTrace(session, events, time.Now().UTC())
	traceInbox := anyMap(anyMap(trace["run_shaping"])["agent_inbox"])
	if anyToString(anyMap(traceInbox["latest"])["token"]) != token {
		t.Fatalf("expected continuation token in trace inbox, got %#v", traceInbox)
	}
	markdown := anyToString(anyMap(trace["run_card"])["markdown"])
	if !strings.Contains(markdown, "## Agent Steering") {
		t.Fatalf("expected agent steering run-card section, got %q", markdown)
	}
	traceValidation := anyMap(anyMap(trace["format_contract"])["validation"])
	if anyToString(traceValidation["status"]) != "passed" {
		t.Fatalf("expected trace validation passed, got %#v", trace["format_contract"])
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
