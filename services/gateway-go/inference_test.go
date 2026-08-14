package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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

func TestInferenceChatHonorsExplicitRequestTimeout(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "false")

	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"late reply"}}`))
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
		`{"provider":"ollama","model":"qwen3.5:9b","timeout_secs":0.02,"messages":[{"role":"user","content":"hello"}]}`,
	)
	if status != http.StatusBadGateway {
		t.Fatalf("expected provider timeout to return 502, got %d payload=%#v", status, payload)
	}

	status, payload = postJSON(
		t,
		gateway.URL+"/v1/inference/chat",
		`{"provider":"ollama","model":"qwen3.5:9b","timeout_secs":120,"messages":[{"role":"user","content":"hello"}]}`,
	)
	if status != http.StatusOK {
		t.Fatalf("expected policy timeout above 95 seconds to be accepted, got %d payload=%#v", status, payload)
	}

	status, payload = postJSON(
		t,
		gateway.URL+"/v1/inference/chat",
		`{"provider":"ollama","model":"qwen3.5:9b","timeout_secs":-1,"messages":[{"role":"user","content":"hello"}]}`,
	)
	if status != http.StatusBadRequest {
		t.Fatalf("expected invalid timeout to return 400, got %d payload=%#v", status, payload)
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

func TestInferenceChatANEBackoffAndFallbackCannotResetOverallDeadline(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "true")
	t.Setenv("ORCH_ANE_SIDECAR_REQUIRE_M_SERIES", "false")
	t.Setenv("ORCH_ANE_SIDECAR_FALLBACK_ENABLED", "true")
	t.Setenv("ORCH_ANE_SIDECAR_RETRIES", "2")
	t.Setenv("ORCH_ANE_SIDECAR_RETRY_BACKOFF_SECS", "1")

	var sidecarCalls atomic.Int64
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"healthy":true,"detail":"ok"}`))
			return
		}
		sidecarCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"retryable"}`))
	}))
	defer sidecar.Close()

	var fallbackCalls atomic.Int64
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"content":"must not run"}}`))
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

	started := time.Now()
	status, payload := postJSON(
		t,
		gateway.URL+"/v1/inference/chat",
		`{"provider":"ane_sidecar","model":"qwen3.5:9b","timeout_secs":0.15,"messages":[{"role":"user","content":"hello"}]}`,
	)
	if status != http.StatusBadGateway {
		t.Fatalf("expected shared deadline failure, got %d payload=%#v", status, payload)
	}
	if !strings.Contains(anyToString(payload["error"]), context.DeadlineExceeded.Error()) {
		t.Fatalf("expected deadline error, got payload=%#v", payload)
	}
	if calls := sidecarCalls.Load(); calls != 1 {
		t.Fatalf("expected one ANE attempt before bounded backoff, got %d", calls)
	}
	if calls := fallbackCalls.Load(); calls != 0 {
		t.Fatalf("fallback must not receive a reset budget, got %d calls", calls)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("bounded backoff should fail before sleeping past the deadline, elapsed=%s", elapsed)
	}
}

func TestInferenceChatANEFallbackReceivesOnlyRemainingOverallBudget(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_FALLBACK_ENABLED", "true")
	t.Setenv("ORCH_ANE_SIDECAR_RETRIES", "0")

	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"fallback"}`))
	}))
	defer sidecar.Close()

	fallbackStarted := make(chan struct{}, 1)
	fallbackStopped := make(chan struct{}, 1)
	releaseFallback := make(chan struct{})
	releasedFallback := false
	release := func() {
		if !releasedFallback {
			close(releaseFallback)
			releasedFallback = true
		}
	}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackStarted <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-releaseFallback:
		}
		fallbackStopped <- struct{}{}
	}))
	defer ollama.Close()
	defer release()
	t.Setenv("OLLAMA_BASE_URL", ollama.URL)

	route := inferenceRoute{
		RequestedProvider: "ane_sidecar",
		Provider:          "ane_sidecar",
		Transport:         "openai",
		BaseURL:           sidecar.URL,
		SidecarEnabled:    true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, activeRoute, err := (&server{}).callInferenceChatWithContext(
		ctx,
		route,
		"qwen3.5:9b",
		[]inferenceMessage{{Role: "user", Content: "hello"}},
		inferenceChatCallOptions{Timeout: 5 * time.Second, ConnectTimeout: time.Second},
	)
	elapsed := time.Since(started)
	release()
	if err == nil {
		t.Fatal("expected the shared overall deadline to cancel fallback")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("expected deadline cancellation, got %v", err)
	}
	if activeRoute.Provider != "ollama" {
		t.Fatalf("expected attempted fallback route, got %#v", activeRoute)
	}
	select {
	case <-fallbackStarted:
	case <-time.After(time.Second):
		t.Fatal("fallback was not attempted")
	}
	select {
	case <-fallbackStopped:
	case <-time.After(time.Second):
		t.Fatal("fallback provider work did not stop")
	}
	if elapsed < 150*time.Millisecond || elapsed >= 450*time.Millisecond {
		t.Fatalf("fallback exceeded the shared overall budget: %s", elapsed)
	}
}

func TestInferenceChatClientCancellationCancelsOutboundProvider(t *testing.T) {
	providerStarted := make(chan struct{}, 1)
	providerStopped := make(chan struct{}, 1)
	releaseProvider := make(chan struct{})
	releasedProvider := false
	release := func() {
		if !releasedProvider {
			close(releaseProvider)
			releasedProvider = true
		}
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerStarted <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-releaseProvider:
		}
		providerStopped <- struct{}{}
	}))
	defer provider.Close()
	defer release()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/inference/chat",
		bytes.NewBufferString(`{"provider":"openai-compatible","base_url":"`+provider.URL+`","model":"qwen","timeout_secs":30,"messages":[{"role":"user","content":"hello"}]}`),
	).WithContext(ctx)
	recorder := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		s.inferenceChatHandler(recorder, req)
		close(handlerDone)
	}()

	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		t.Fatal("provider request did not start")
	}
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("gateway handler did not stop after client cancellation")
	}
	release()
	select {
	case <-providerStopped:
	case <-time.After(time.Second):
		t.Fatal("outbound provider request did not stop")
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected canceled provider call to fail, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), context.Canceled.Error()) {
		t.Fatalf("expected context cancellation evidence, got %s", recorder.Body.String())
	}
}

func TestInferenceChatWorkerCancellationStopsGatewayAndProviderWork(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		t.Run(fmt.Sprintf("attempt_%d", attempt), func(t *testing.T) {
			providerStarted := make(chan struct{}, 1)
			providerStopped := make(chan struct{}, 1)
			providerListener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen for provider: %v", err)
			}
			defer providerListener.Close()
			go func() {
				connection, acceptErr := providerListener.Accept()
				if acceptErr != nil {
					return
				}
				defer connection.Close()
				request, readErr := http.ReadRequest(bufio.NewReader(connection))
				if readErr != nil {
					return
				}
				_, _ = io.Copy(io.Discard, request.Body)
				_ = request.Body.Close()
				providerStarted <- struct{}{}
				_ = connection.SetReadDeadline(time.Now().Add(time.Second))
				buffer := make([]byte, 1)
				if _, readErr := connection.Read(buffer); readErr != nil {
					providerStopped <- struct{}{}
				}
			}()

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer backend.Close()
			s := newTestServer(t, backend.URL)
			gateway := httptest.NewServer(buildMux(s))
			defer gateway.Close()

			requestID := fmt.Sprintf("%064x", attempt+1)
			providerURL := "http://" + providerListener.Addr().String()
			body := `{"provider":"openai-compatible","base_url":"` + providerURL + `","model":"qwen","request_id":"` + requestID + `","timeout_secs":30,"messages":[{"role":"user","content":"hello"}]}`
			responseDone := make(chan error, 1)
			go func() {
				response, err := http.Post(gateway.URL+"/v1/inference/chat", "application/json", strings.NewReader(body))
				if response != nil {
					_ = response.Body.Close()
				}
				responseDone <- err
			}()
			select {
			case <-providerStarted:
			case <-time.After(time.Second):
				t.Fatal("downstream provider request did not start")
			}
			started := time.Now()
			cancelBody := `{"request_id":"` + requestID + `"}`
			cancelResponse, err := http.Post(gateway.URL+"/v1/inference/cancel", "application/json", strings.NewReader(cancelBody))
			if err != nil {
				t.Fatalf("cancel inference request: %v", err)
			}
			_ = cancelResponse.Body.Close()
			if cancelResponse.StatusCode != http.StatusOK {
				t.Fatalf("cancel inference request status=%d", cancelResponse.StatusCode)
			}
			select {
			case <-responseDone:
			case <-time.After(time.Second):
				t.Fatal("gateway inference response did not stop after cancellation")
			}
			select {
			case <-providerStopped:
			case <-time.After(time.Second):
				t.Fatal("worker cancellation did not cancel downstream provider")
			}
			if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
				t.Fatalf("downstream provider cancellation was not prompt: %s", elapsed)
			}
		})
	}
}

func TestInferenceCancellationTombstoneCapacityIsIndependentFromActiveCapacity(t *testing.T) {
	s := &server{}
	for index := 0; index < inferenceCancellationTombstoneMax; index++ {
		requestID := fmt.Sprintf("%064x", index+1)
		if disposition := s.cancelInferenceRequest(requestID); disposition != inferenceCancellationPending {
			t.Fatalf("tombstone %d disposition=%d", index, disposition)
		}
	}

	firstRequestID := fmt.Sprintf("%064x", 1)
	if disposition := s.cancelInferenceRequest(firstRequestID); disposition != inferenceCancellationPending {
		t.Fatalf("idempotent tombstone disposition=%d", disposition)
	}
	overflowRequestID := fmt.Sprintf("%064x", inferenceCancellationTombstoneMax+1)
	if disposition := s.cancelInferenceRequest(overflowRequestID); disposition != inferenceCancellationPendingCapacity {
		t.Fatalf("overflow tombstone disposition=%d", disposition)
	}

	activeRequestID := fmt.Sprintf("%064x", inferenceCancellationTombstoneMax+2)
	activeCanceled := make(chan struct{}, 1)
	entry, err := s.registerInferenceCancellation(activeRequestID, func() {
		activeCanceled <- struct{}{}
	})
	if err != nil {
		t.Fatalf("active registration after tombstone saturation: %v", err)
	}
	defer s.unregisterInferenceCancellation(activeRequestID, entry)
	if disposition := s.cancelInferenceRequest(activeRequestID); disposition != inferenceCancellationActive {
		t.Fatalf("active cancellation disposition=%d", disposition)
	}
	select {
	case <-activeCanceled:
	case <-time.After(time.Second):
		t.Fatal("active request was not canceled after tombstone saturation")
	}

	s.inferenceCancellationMu.Lock()
	activeCount := len(s.inferenceCancellations)
	tombstoneCount := len(s.inferenceCancellationTombstones)
	s.inferenceCancellationMu.Unlock()
	if activeCount != 1 || tombstoneCount != inferenceCancellationTombstoneMax {
		t.Fatalf("unexpected partition counts active=%d tombstones=%d", activeCount, tombstoneCount)
	}
}

func TestInferenceCancellationExpiredTombstonesReleaseCapacity(t *testing.T) {
	s := &server{
		inferenceCancellationTombstones: map[string]time.Time{},
	}
	expiredAt := time.Now().Add(-inferenceCancellationTombstoneTTL - time.Second)
	for index := 0; index < inferenceCancellationTombstoneMax; index++ {
		requestID := fmt.Sprintf("%064x", index+1)
		s.inferenceCancellationTombstones[requestID] = expiredAt
	}

	freshRequestID := fmt.Sprintf("%064x", inferenceCancellationTombstoneMax+1)
	if disposition := s.cancelInferenceRequest(freshRequestID); disposition != inferenceCancellationPending {
		t.Fatalf("fresh tombstone after expiry disposition=%d", disposition)
	}
	s.inferenceCancellationMu.Lock()
	tombstoneCount := len(s.inferenceCancellationTombstones)
	createdAt := s.inferenceCancellationTombstones[freshRequestID]
	s.inferenceCancellationMu.Unlock()
	if tombstoneCount != 1 {
		t.Fatalf("expired tombstones were not pruned: count=%d", tombstoneCount)
	}
	if createdAt.IsZero() {
		t.Fatal("fresh tombstone was not retained")
	}
}

func TestInferenceCancellationUnknownHTTPStormCannotStarveActiveRequest(t *testing.T) {
	s := &server{}
	for index := 0; index < inferenceCancellationTombstoneMax+32; index++ {
		requestID := fmt.Sprintf("%064x", index+1)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/inference/cancel",
			strings.NewReader(`{"request_id":"`+requestID+`"}`),
		)
		s.inferenceCancelHandler(recorder, request)
		expectedStatus := http.StatusOK
		if index >= inferenceCancellationTombstoneMax {
			expectedStatus = http.StatusTooManyRequests
		}
		if recorder.Code != expectedStatus {
			t.Fatalf("unknown cancellation %d status=%d body=%s", index, recorder.Code, recorder.Body.String())
		}
	}

	activeRequestID := fmt.Sprintf("%064x", inferenceCancellationTombstoneMax+1000)
	activeCanceled := make(chan struct{}, 1)
	entry, err := s.registerInferenceCancellation(activeRequestID, func() {
		activeCanceled <- struct{}{}
	})
	if err != nil {
		t.Fatalf("legitimate registration after unknown storm: %v", err)
	}
	defer s.unregisterInferenceCancellation(activeRequestID, entry)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/inference/cancel",
		strings.NewReader(`{"request_id":"`+activeRequestID+`"}`),
	)
	s.inferenceCancelHandler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("active cancellation after unknown storm status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-activeCanceled:
	case <-time.After(time.Second):
		t.Fatal("active cancellation did not execute after unknown storm")
	}
}

func TestInferenceCancellationBeforeRegistrationStopsProviderWork(t *testing.T) {
	providerCalled := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalled <- struct{}{}
	}))
	defer provider.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	requestID := strings.Repeat("a", 64)
	cancelResponse, err := http.Post(
		gateway.URL+"/v1/inference/cancel",
		"application/json",
		strings.NewReader(`{"request_id":"`+requestID+`"}`),
	)
	if err != nil {
		t.Fatalf("pre-register cancellation: %v", err)
	}
	_ = cancelResponse.Body.Close()
	if cancelResponse.StatusCode != http.StatusOK {
		t.Fatalf("pre-register cancellation status=%d", cancelResponse.StatusCode)
	}

	chatResponse, err := http.Post(
		gateway.URL+"/v1/inference/chat",
		"application/json",
		strings.NewReader(`{"provider":"openai-compatible","base_url":"`+provider.URL+`","model":"qwen","request_id":"`+requestID+`","timeout_secs":30,"messages":[{"role":"user","content":"hello"}]}`),
	)
	if err != nil {
		t.Fatalf("canceled inference request: %v", err)
	}
	_ = chatResponse.Body.Close()
	if chatResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected canceled request to stop during routing, got %d", chatResponse.StatusCode)
	}
	select {
	case <-providerCalled:
		t.Fatal("cancel-before-register race reached provider")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestInferenceChatClientCancellationCancelsAutoRouteProbe(t *testing.T) {
	resetANEProbeCacheForTest()
	t.Setenv("ORCH_ANE_SIDECAR_ENABLED", "false")
	t.Setenv("ORCH_INFER_PROVIDER", "auto")
	t.Setenv("ORCH_INFER_PROVIDER_PRIORITY", "vllm")

	probeStarted := make(chan struct{}, 1)
	probeCanceled := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	releasedProbe := false
	release := func() {
		if !releasedProbe {
			close(releaseProbe)
			releasedProbe = true
		}
	}
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeStarted <- struct{}{}
		select {
		case <-r.Context().Done():
			probeCanceled <- struct{}{}
		case <-releaseProbe:
		}
	}))
	defer probe.Close()
	defer release()
	t.Setenv("VLLM_BASE_URL", probe.URL)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/inference/chat",
		bytes.NewBufferString(`{"provider":"auto","model":"qwen","timeout_secs":30,"messages":[{"role":"user","content":"hello"}]}`),
	).WithContext(ctx)
	recorder := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		s.inferenceChatHandler(recorder, req)
		close(handlerDone)
	}()

	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("auto-route probe did not start")
	}
	cancel()
	select {
	case <-probeCanceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not cancel auto-route probe")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("gateway handler did not stop after route cancellation")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected canceled route resolution to fail, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), context.Canceled.Error()) {
		t.Fatalf("expected route cancellation evidence, got %s", recorder.Body.String())
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
	localOpenSource, _ := modelPolicy["localOpenSource"].(map[string]any)
	if anyToBool(localOpenSource["downloadByDefault"]) {
		t.Fatalf("local open-source shortlist must never download by default, got %#v", localOpenSource)
	}
	selectionRules := anyToStringList(localOpenSource["selectionRules"], 20)
	foundConnectorRule := false
	for _, rule := range selectionRules {
		if strings.Contains(rule, "connector-only") && strings.Contains(rule, "LLAMA_CPP_BASE_URL") {
			foundConnectorRule = true
			break
		}
	}
	if !foundConnectorRule {
		t.Fatalf("expected llama.cpp connector-only rule, got %v", selectionRules)
	}
	foundQwableSmall := false
	for _, raw := range localOpenSource["small"].([]any) {
		model, _ := raw.(map[string]any)
		repo := strings.TrimSpace(anyToString(model["repo"]))
		if repo == "useremma/qwable-9b-claude-fable-mlx-8bit" {
			t.Fatalf("shortlist used unreachable useremma spelling: %#v", model)
		}
		if repo == "usermma/Qwable-9B-Claude-Fable-5-mlx-8Bit" {
			foundQwableSmall = true
			if anyToBool(model["downloadByDefault"]) || !anyToBool(model["optInRequired"]) {
				t.Fatalf("expected Qwable small model to be opt-in/no-download, got %#v", model)
			}
		}
	}
	if !foundQwableSmall {
		t.Fatalf("expected corrected Qwable 9B MLX model in small shortlist, got %#v", localOpenSource["small"])
	}
	foundNexMedium := false
	for _, raw := range localOpenSource["medium"].([]any) {
		model, _ := raw.(map[string]any)
		if strings.TrimSpace(anyToString(model["repo"])) == "nex-agi/Nex-N2-mini" {
			foundNexMedium = true
			if provider := strings.TrimSpace(anyToString(model["primaryProvider"])); provider != "sglang" {
				t.Fatalf("expected Nex-N2-mini to prefer backend-native HF serving, got %#v", model)
			}
		}
	}
	if !foundNexMedium {
		t.Fatalf("expected Nex-N2-mini in medium shortlist, got %#v", localOpenSource["medium"])
	}
	foundHarness := false
	foundPrivacyFilter := false
	for _, raw := range localOpenSource["boundaryModels"].([]any) {
		model, _ := raw.(map[string]any)
		switch strings.TrimSpace(anyToString(model["repo"])) {
		case "pat-jj/harness-1":
			foundHarness = true
		case "openai/privacy-filter":
			foundPrivacyFilter = true
			if strings.TrimSpace(anyToString(model["primaryProvider"])) != "local_classifier" {
				t.Fatalf("expected privacy-filter to be modeled as classifier, got %#v", model)
			}
		}
		if anyToBool(model["downloadByDefault"]) {
			t.Fatalf("boundary model must not download by default: %#v", model)
		}
	}
	if !foundHarness || !foundPrivacyFilter {
		t.Fatalf("expected harness/privacy boundary models, got %#v", localOpenSource["boundaryModels"])
	}
	frontierProviders, _ := modelPolicy["frontierProviders"].(map[string]any)
	if strings.TrimSpace(anyToString(frontierProviders["policy"])) == "" {
		t.Fatalf("expected frontier provider connection guidance, got %#v", frontierProviders)
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
