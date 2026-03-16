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

func TestStagedRetrievalCarriesRuntimeBackendPolicy(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
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
