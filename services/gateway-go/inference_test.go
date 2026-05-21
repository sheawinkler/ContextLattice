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
	providerProbeCache.mu.Lock()
	providerProbeCache.entries = map[string]inferenceProviderProbeResult{}
	providerProbeCache.mu.Unlock()
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
	t.Setenv("ORCH_INFER_PROVIDER_PRIORITY", "vllm,ollama")
	t.Setenv("ORCH_INFER_AUTO_PROBE_TIMEOUT_SECS", "0.05")
	t.Setenv("VLLM_BASE_URL", "http://127.0.0.1:1")

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

func TestInferenceRouteAutoSelectsHealthyVLLM(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "false")
	t.Setenv("ORCH_INFER_PROVIDER", "auto")
	t.Setenv("ORCH_INFER_PROVIDER_PRIORITY", "vllm,ollama")

	vllm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer vllm.Close()
	t.Setenv("VLLM_BASE_URL", vllm.URL)

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
	if provider := strings.TrimSpace(anyToString(route["provider"])); provider != "vllm" {
		t.Fatalf("expected vllm route, got %q payload=%#v", provider, payload)
	}
}

func TestInferenceRouteAliasesMLXAndVLLMMetal(t *testing.T) {
	resetANEProbeCacheForTest()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := postJSON(t, gateway.URL+"/v1/inference/route", `{"provider":"mtplx","base_url":"http://127.0.0.1:18087/v1"}`)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	route, _ := payload["route"].(map[string]any)
	if provider := strings.TrimSpace(anyToString(route["provider"])); provider != "mlx" {
		t.Fatalf("expected mtplx alias to resolve to mlx, got %q", provider)
	}

	t.Setenv("VLLM_METAL_BASE_URL", "http://127.0.0.1:28000")
	status, payload = postJSON(t, gateway.URL+"/v1/inference/route", `{"provider":"vllm_metal"}`)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	route, _ = payload["route"].(map[string]any)
	if provider := strings.TrimSpace(anyToString(route["provider"])); provider != "vllm-metal" {
		t.Fatalf("expected vllm_metal alias to resolve to vllm-metal, got %q", provider)
	}
	if baseURL := strings.TrimSpace(anyToString(route["base_url"])); baseURL != "http://127.0.0.1:28000" {
		t.Fatalf("expected vllm-metal base url, got %q", baseURL)
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
	if strings.TrimSpace(anyToString(payload["selected"])) != "fastembed-rs" {
		t.Fatalf("expected fastembed-rs selected, payload=%#v", payload)
	}
}

func TestInferenceRuntimePolicyEndpoint(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_INFER_PROVIDER_PRIORITY", "vllm,ollama")
	t.Setenv("ORCH_INFER_AUTO_PROBE_TIMEOUT_SECS", "0.05")
	t.Setenv("VLLM_BASE_URL", "http://127.0.0.1:1")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := getJSON(t, gateway.URL+"/v1/inference/runtime-policy")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	if _, ok := payload["hardware"].(map[string]any); !ok {
		t.Fatalf("expected hardware policy payload, got %#v", payload)
	}
	if candidates, ok := payload["candidates"].([]any); !ok || len(candidates) == 0 {
		t.Fatalf("expected runtime candidates, got %#v", payload["candidates"])
	}
}

func TestInferenceMSeriesDetectionHonorsHostProfileOverride(t *testing.T) {
	t.Setenv("ORCH_HOST_HARDWARE_PROFILE", "apple_silicon")
	if !_inferenceIsMSeriesMac() {
		t.Fatal("expected Apple Silicon host override to satisfy M-series checks")
	}
}
