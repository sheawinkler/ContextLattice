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

func TestInferenceRouteSupportsSGLangProvider(t *testing.T) {
	resetANEProbeCacheForTest()
	sglang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3.6"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer sglang.Close()
	t.Setenv("SGLANG_BASE_URL", sglang.URL)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	status, payload := postJSON(t, gateway.URL+"/v1/inference/route", `{"provider":"sgl"}`)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d payload=%#v", status, payload)
	}
	route, _ := payload["route"].(map[string]any)
	if provider := strings.TrimSpace(anyToString(route["provider"])); provider != "sglang" {
		t.Fatalf("expected sgl alias to resolve to sglang, got %q", provider)
	}
	if baseURL := strings.TrimSpace(anyToString(route["base_url"])); baseURL != sglang.URL {
		t.Fatalf("expected sglang base URL, got %q", baseURL)
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

func TestInferenceOpenAIMessageContentSupportsArrayParts(t *testing.T) {
	content, err := _inferenceOpenAIMessageContent(map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "first"},
			map[string]any{"type": "output_text", "content": "second"},
		},
	})
	if err != nil {
		t.Fatalf("expected content array to parse: %v", err)
	}
	if content != "first\nsecond" {
		t.Fatalf("unexpected content %q", content)
	}
}

func TestInferenceOpenAIMessageContentRejectsReasoningOnlyOutput(t *testing.T) {
	content, err := _inferenceOpenAIMessageContent(map[string]any{
		"reasoning": "hidden scratchpad only",
	})
	if err == nil {
		t.Fatalf("expected reasoning-only error, got content %q", content)
	}
	if !strings.Contains(err.Error(), "final-content chat template") {
		t.Fatalf("expected template repair instruction, got %v", err)
	}
}

func TestInferenceOllamaRejectsReasoningOnlyOutput(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"thinking":"hidden scratchpad only","content":""}}`))
	}))
	defer ollama.Close()

	content, err := _inferenceCallOllamaWithOptions(ollama.URL, "qwen-test", []inferenceMessage{{Role: "user", Content: "hello"}}, inferenceChatCallOptions{
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
	})
	if err == nil {
		t.Fatalf("expected reasoning-only error, got content %q", content)
	}
	if !strings.Contains(err.Error(), "final-content chat template") {
		t.Fatalf("expected template repair instruction, got %v", err)
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
	if !anyToBool(payload["singleActiveBackend"]) {
		t.Fatalf("expected single-active backend policy to default true, payload=%#v", payload)
	}
	recommendation, _ := payload["recommendation"].(map[string]any)
	if strings.TrimSpace(anyToString(recommendation["modelStrategy"])) == "" {
		t.Fatalf("expected runtime recommendation with model strategy, payload=%#v", payload)
	}
}

func TestInferenceRuntimePolicyRecommendsSGLangOnAccelerator(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_HOST_HARDWARE_PROFILE", "nvidia_cuda")
	t.Setenv("ORCH_HOST_VRAM_GB", "48")
	t.Setenv("ORCH_INFER_PROVIDER_PRIORITY", "sglang,vllm,ollama")
	t.Setenv("ORCH_INFER_AUTO_PROBE_TIMEOUT_SECS", "0.05")
	t.Setenv("VLLM_BASE_URL", "http://127.0.0.1:1")

	sglang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3.6"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer sglang.Close()
	t.Setenv("SGLANG_BASE_URL", sglang.URL)

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
	selected, _ := payload["selected"].(map[string]any)
	if provider := strings.TrimSpace(anyToString(selected["provider"])); provider != "sglang" {
		t.Fatalf("expected sglang selected, got %q payload=%#v", provider, payload)
	}
	recommendation, _ := payload["recommendation"].(map[string]any)
	if provider := strings.TrimSpace(anyToString(recommendation["provider"])); provider != "sglang" {
		t.Fatalf("expected sglang recommendation, got %q recommendation=%#v", provider, recommendation)
	}
	modelSizing, _ := recommendation["modelSizing"].(map[string]any)
	if !anyToBool(modelSizing["resourceKnown"]) {
		t.Fatalf("expected known resource sizing, got %#v", modelSizing)
	}
	foundSGLangCandidate := false
	for _, raw := range payload["candidates"].([]any) {
		candidate, _ := raw.(map[string]any)
		if strings.TrimSpace(anyToString(candidate["provider"])) == "sglang" {
			foundSGLangCandidate = true
			if strings.TrimSpace(anyToString(candidate["setupHint"])) == "" {
				t.Fatalf("expected sglang setup hint, got %#v", candidate)
			}
			formats, _ := candidate["modelFormats"].([]any)
			if len(formats) == 0 {
				t.Fatalf("expected sglang model formats, got %#v", candidate)
			}
		}
	}
	if !foundSGLangCandidate {
		t.Fatalf("expected sglang candidate, payload=%#v", payload)
	}
}

func TestInferenceRuntimePolicyIncludesOptInQwen36ModelPolicy(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_HOST_HARDWARE_PROFILE", "apple_silicon")
	t.Setenv("ORCH_HOST_MEMORY_GB", "64")
	t.Setenv("ORCH_INFER_PROVIDER_PRIORITY", "llama-cpp")
	t.Setenv("ORCH_INFER_AUTO_PROBE_TIMEOUT_SECS", "0.05")

	llamaCPP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3.6-gguf"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer llamaCPP.Close()
	t.Setenv("LLAMA_CPP_BASE_URL", llamaCPP.URL)

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
	modelPolicy, _ := payload["modelPolicy"].(map[string]any)
	if anyToBool(modelPolicy["downloadByDefault"]) {
		t.Fatalf("expected model policy to avoid default downloads, got %#v", modelPolicy)
	}
	if repo := strings.TrimSpace(anyToString(modelPolicy["qwen36GGUFDefault"])); !strings.Contains(repo, "mudler/Qwen3.6-35B-A3B") {
		t.Fatalf("expected mudler GGUF default, got %q policy=%#v", repo, modelPolicy)
	}
	privateEval, _ := modelPolicy["privateEval"].(map[string]any)
	if anyToBool(privateEval["enabled"]) {
		t.Fatalf("expected private eval disabled by default, got %#v", privateEval)
	}
	if repo := strings.TrimSpace(anyToString(privateEval["repoIfEnabled"])); !strings.Contains(repo, "Huihui-Qwen3.6") {
		t.Fatalf("expected Huihui private-eval repo metadata, got %q", repo)
	}
	templateConformance, _ := modelPolicy["templateConformance"].(map[string]any)
	if !anyToBool(templateConformance["finalContentRequired"]) || !anyToBool(templateConformance["reasoningOnlyRejected"]) {
		t.Fatalf("expected final-content template conformance policy, got %#v", templateConformance)
	}

	foundGGUFPolicy := false
	for _, raw := range payload["candidates"].([]any) {
		candidate, _ := raw.(map[string]any)
		if strings.TrimSpace(anyToString(candidate["provider"])) != "llama-cpp" {
			continue
		}
		models, _ := candidate["modelPolicy"].([]any)
		for _, rawModel := range models {
			model, _ := rawModel.(map[string]any)
			if strings.TrimSpace(anyToString(model["id"])) == "qwen36_gguf_opt_in" {
				foundGGUFPolicy = true
				if !anyToBool(model["optInRequired"]) || anyToBool(model["downloadByDefault"]) {
					t.Fatalf("expected opt-in/no-download qwen36 policy, got %#v", model)
				}
			}
		}
	}
	if !foundGGUFPolicy {
		t.Fatalf("expected llama.cpp candidate to include qwen36 GGUF policy, payload=%#v", payload)
	}
}

func TestInferenceMSeriesDetectionHonorsHostProfileOverride(t *testing.T) {
	t.Setenv("ORCH_HOST_HARDWARE_PROFILE", "apple_silicon")
	if !_inferenceIsMSeriesMac() {
		t.Fatal("expected Apple Silicon host override to satisfy M-series checks")
	}
}
