package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(backendURL string) *server {
	return &server{
		backendURL: strings.TrimRight(backendURL, "/"),
		client:     &http.Client{Timeout: 2 * time.Second},
	}
}

func TestProxyForwardsRetrievalRequest(t *testing.T) {
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

	s := newTestServer(backend.URL)
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
	var capturedRawQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"memory":{"id":"alpha::notes/a.md"}}`))
	}))
	defer backend.Close()

	s := newTestServer(backend.URL)
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

	s := newTestServer(backend.URL)
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

	s := newTestServer(backend.URL)
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
