package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func resetANEProbeCacheForTest() {
	aneProbeCache.mu.Lock()
	aneProbeCache.checkedAt = time.Time{}
	aneProbeCache.healthy = false
	aneProbeCache.detail = "never checked"
	aneProbeCache.mu.Unlock()
}

func postJSON(t *testing.T, url string, payload string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http call failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	data := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("decode response: %v body=%s", err, string(raw))
		}
	}
	return resp.StatusCode, data
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http call failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	data := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("decode response: %v body=%s", err, string(raw))
		}
	}
	return resp.StatusCode, data
}

func TestInferenceRouteAutoUsesOllamaByDefault(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "false")
	t.Setenv("ORCH_INFER_PROVIDER", "auto")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := postJSON(t, gateway.URL+"/v1/inference/route", `{"provider":"auto"}`)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	route, _ := payload["route"].(map[string]any)
	provider := strings.TrimSpace(anyToString(route["provider"]))
	if provider != "ollama" && provider != "ollama_coreml" {
		t.Fatalf("expected ollama route, got %q", provider)
	}
}

func TestInferenceChatUsesOllamaEndpoint(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "false")

	ollamaPath := ""
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"gateway ollama reply"}}`))
	}))
	defer ollama.Close()

	t.Setenv("OLLAMA_BASE_URL", ollama.URL)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := postJSON(
		t,
		gateway.URL+"/v1/inference/chat",
		`{"provider":"ollama","model":"qwen3.5:9b","messages":[{"role":"user","content":"hello"}]}`,
	)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	if strings.TrimSpace(anyToString(payload["content"])) != "gateway ollama reply" {
		t.Fatalf("unexpected content: %#v", payload["content"])
	}
	if ollamaPath != "/api/chat" {
		t.Fatalf("expected /api/chat call, got %s", ollamaPath)
	}
}

func TestInferenceChatANEFallbackToOllama(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "true")
	t.Setenv("ORCH_ANE_SIDECAR_REQUIRE_M_SERIES", "false")
	t.Setenv("ORCH_ANE_SIDECAR_FALLBACK_ENABLED", "true")
	t.Setenv("ORCH_ANE_SIDECAR_RETRIES", "0")

	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"healthy":true,"detail":"ok"}`))
			return
		}
		// Force fallback after route resolution picks ANE.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer sidecar.Close()

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"fallback ollama reply"}}`))
	}))
	defer ollama.Close()

	t.Setenv("ORCH_ANE_SIDECAR_URL", sidecar.URL)
	t.Setenv("OLLAMA_BASE_URL", ollama.URL)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := postJSON(
		t,
		gateway.URL+"/v1/inference/chat",
		`{"provider":"auto","model":"qwen3.5:9b","messages":[{"role":"user","content":"hello"}]}`,
	)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	if strings.TrimSpace(anyToString(payload["content"])) != "fallback ollama reply" {
		t.Fatalf("unexpected content: %#v", payload["content"])
	}
	route, _ := payload["route"].(map[string]any)
	provider := strings.TrimSpace(anyToString(route["provider"]))
	if provider != "ollama" && provider != "ollama_coreml" {
		t.Fatalf("expected ollama fallback provider, got %q", provider)
	}
}

func TestInferenceEmbeddingPolicyEndpoint(t *testing.T) {
	gatePath := t.TempDir() + "/fastembed_gate_latest.json"
	rawGate := []byte(`{"passed":true,"reason":"ok","generatedAt":"2026-03-22T21:00:00Z","metrics":{"improvementPct":16.0},"thresholds":{"minImprovementPct":5.0}}`)
	if err := os.WriteFile(gatePath, rawGate, 0o644); err != nil {
		t.Fatalf("write gate file: %v", err)
	}
	t.Setenv("ORCH_ADAPTER_FASTEMBED_RS_ENABLED", "true")
	t.Setenv("ORCH_ADAPTER_FASTEMBED_RS_REQUIRE_GATE", "true")
	t.Setenv("ORCH_ADAPTER_FASTEMBED_RS_PROMOTE_OVERRIDE", "false")
	t.Setenv("ORCH_ADAPTER_FASTEMBED_RS_GATE_FILE", gatePath)
	t.Setenv("ORCH_FASTEMBED_RS_BASE_URL", "http://127.0.0.1:8123")
	t.Setenv("ORCH_FASTEMBED_RS_ROUTE", "/embed")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := getJSON(t, gateway.URL+"/v1/inference/embedding-policy")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	fastembed, _ := payload["fastembedRs"].(map[string]any)
	if !anyToBool(fastembed["enabled"]) {
		t.Fatalf("expected fastembed policy enabled=true, payload=%#v", payload)
	}
	gate, _ := fastembed["gate"].(map[string]any)
	if !anyToBool(gate["available"]) {
		t.Fatalf("expected gate available=true, gate=%#v", gate)
	}
}
