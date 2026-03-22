package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultInferenceOllamaBaseURL    = "http://127.0.0.1:11434"
	defaultInferenceLMStudioBaseURL  = "http://127.0.0.1:1234"
	defaultInferenceOpenAICompatBase = "http://127.0.0.1:8000"
	defaultInferenceLlamaCPPBaseURL  = "http://127.0.0.1:8080"
	defaultInferenceANESidecarURL    = "http://127.0.0.1:9099"
)

type inferenceRoute struct {
	RequestedProvider string `json:"requested_provider"`
	Provider          string `json:"provider"`
	Transport         string `json:"transport"`
	BaseURL           string `json:"base_url"`
	APIKey            string `json:"-"`
	Reason            string `json:"reason"`
	CoreMLEnabled     bool   `json:"coreml_enabled"`
	SidecarEnabled    bool   `json:"sidecar_enabled"`
}

type inferenceRouteRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

type inferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inferenceChatRequest struct {
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	BaseURL  string             `json:"base_url"`
	APIKey   string             `json:"api_key"`
	Messages []inferenceMessage `json:"messages"`
}

type inferenceANEProbeCache struct {
	mu        sync.Mutex
	checkedAt time.Time
	healthy   bool
	detail    string
}

type inferenceFastembedGateCache struct {
	mu                sync.Mutex
	path              string
	mtimeUnix         int64
	loadedAtMonotonic time.Time
	status            map[string]any
}

var aneProbeCache = inferenceANEProbeCache{
	detail: "never checked",
}

var fastembedGateCache = inferenceFastembedGateCache{}

func _inferenceNormalizeOpenAIBase(baseURL string) string {
	cleaned := strings.TrimSpace(strings.TrimRight(baseURL, "/"))
	if cleaned == "" {
		return ""
	}
	if strings.HasSuffix(cleaned, "/v1") {
		return cleaned
	}
	return cleaned + "/v1"
}

func _inferenceBaseURLFromProvider(provider string, override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimRight(strings.TrimSpace(override), "/")
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ollama", "ollama_coreml":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("OLLAMA_BASE_URL"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceOllamaBaseURL
	case "lmstudio":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("LMSTUDIO_BASE_URL"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceLMStudioBaseURL
	case "openai-compatible", "openai_compatible", "vllm":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("OPENAI_API_BASE"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceOpenAICompatBase
	case "llama-cpp", "llama_cpp":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("LLAMA_CPP_BASE_URL"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceLlamaCPPBaseURL
	case "ane", "ane_sidecar":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("ORCH_ANE_SIDECAR_URL"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceANESidecarURL
	default:
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("OPENAI_API_BASE"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceOpenAICompatBase
	}
}

func _inferenceIsMSeriesMac() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

func _inferenceCoreMLDefaultEnabled() bool {
	return envBool("TASK_OLLAMA_COREML_ON_M_SERIES", true)
}

func _inferenceANESidecarEnabled() bool {
	return envBool("ORCH_ANE_SIDECAR_ENABLED", false)
}

func _inferenceANESidecarRequireMSeries() bool {
	return envBool("ORCH_ANE_SIDECAR_REQUIRE_M_SERIES", true)
}

func _inferenceANESidecarFallbackEnabled() bool {
	return envBool("ORCH_ANE_SIDECAR_FALLBACK_ENABLED", true)
}

func _inferenceANESidecarURL() string {
	return _inferenceBaseURLFromProvider("ane_sidecar", "")
}

func _inferenceANESidecarHealthURL(baseURL string) string {
	explicit := strings.TrimSpace(strings.TrimRight(os.Getenv("ORCH_ANE_SIDECAR_HEALTH_URL"), "/"))
	if explicit != "" {
		return explicit
	}
	return strings.TrimRight(baseURL, "/") + "/health"
}

func _inferenceHTTPClient(timeout time.Duration, connectTimeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if connectTimeout <= 0 {
		connectTimeout = 2 * time.Second
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   connectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func _inferenceFastembedAdapterEnabledByFlag() bool {
	return envBool("ORCH_ADAPTER_FASTEMBED_RS_ENABLED", false)
}

func _inferenceFastembedGateRequired() bool {
	return envBool("ORCH_ADAPTER_FASTEMBED_RS_REQUIRE_GATE", false)
}

func _inferenceFastembedPromoteOverrideEnabled() bool {
	return envBool("ORCH_ADAPTER_FASTEMBED_RS_PROMOTE_OVERRIDE", false)
}

func _inferenceFastembedPromoteOverrideReason() string {
	return strings.TrimSpace(os.Getenv("ORCH_ADAPTER_FASTEMBED_RS_PROMOTE_REASON"))
}

func _inferenceFastembedGateMaxAge() time.Duration {
	maxAge := envDurationSeconds("ORCH_ADAPTER_FASTEMBED_RS_GATE_MAX_AGE_SECS", 172800.0)
	if maxAge < time.Minute {
		return time.Minute
	}
	return maxAge
}

func _inferenceFastembedGateFilePath() string {
	token := strings.TrimSpace(os.Getenv("ORCH_ADAPTER_FASTEMBED_RS_GATE_FILE"))
	if token == "" {
		token = "bench/results/fastembed_gate_latest.json"
	}
	return token
}

func _inferenceFastembedApplyPromoteOverride(status map[string]any) map[string]any {
	payload := map[string]any{}
	for key, value := range status {
		payload[key] = value
	}
	required := anyToBool(payload["required"])
	passed := anyToBool(payload["passed"])
	overrideEnabled := _inferenceFastembedPromoteOverrideEnabled()
	overrideActive := overrideEnabled && required && !passed
	overrideReason := _inferenceFastembedPromoteOverrideReason()
	payload["promoteOverrideEnabled"] = overrideEnabled
	if overrideReason == "" {
		payload["promoteOverrideReason"] = nil
	} else {
		payload["promoteOverrideReason"] = overrideReason
	}
	payload["promoteOverrideActive"] = overrideActive
	payload["effectivePassed"] = passed || !required || overrideActive
	if overrideActive {
		baseReason := strings.TrimSpace(anyToString(payload["reason"]))
		if baseReason == "" {
			baseReason = "threshold_not_met"
		}
		note := overrideReason
		if note == "" {
			note = "manual_promotion_override"
		}
		payload["reason"] = baseReason + "; promoted_by_override=" + note
	}
	return payload
}

func _inferenceFastembedGateStatus() map[string]any {
	required := _inferenceFastembedGateRequired()
	path := _inferenceFastembedGateFilePath()
	if strings.TrimSpace(path) == "" {
		return _inferenceFastembedApplyPromoteOverride(map[string]any{
			"required":  required,
			"passed":    !required,
			"available": false,
			"reason":    "gate_file_missing",
		})
	}
	now := time.Now()
	fastembedGateCache.mu.Lock()
	if fastembedGateCache.status != nil &&
		fastembedGateCache.path == path &&
		!fastembedGateCache.loadedAtMonotonic.IsZero() &&
		now.Sub(fastembedGateCache.loadedAtMonotonic) <= 3*time.Second {
		cached := map[string]any{}
		for key, value := range fastembedGateCache.status {
			cached[key] = value
		}
		fastembedGateCache.mu.Unlock()
		return cached
	}
	fastembedGateCache.mu.Unlock()

	stat, err := os.Stat(path)
	if err != nil {
		status := _inferenceFastembedApplyPromoteOverride(map[string]any{
			"required":  required,
			"available": false,
			"passed":    !required,
			"reason":    "gate_file_unreadable",
			"path":      path,
		})
		fastembedGateCache.mu.Lock()
		fastembedGateCache.path = path
		fastembedGateCache.mtimeUnix = 0
		fastembedGateCache.loadedAtMonotonic = now
		fastembedGateCache.status = status
		fastembedGateCache.mu.Unlock()
		return status
	}

	fastembedGateCache.mu.Lock()
	if fastembedGateCache.status != nil &&
		fastembedGateCache.path == path &&
		fastembedGateCache.mtimeUnix == stat.ModTime().Unix() &&
		!fastembedGateCache.loadedAtMonotonic.IsZero() &&
		now.Sub(fastembedGateCache.loadedAtMonotonic) <= 3*time.Second {
		cached := map[string]any{}
		for key, value := range fastembedGateCache.status {
			cached[key] = value
		}
		fastembedGateCache.mu.Unlock()
		return cached
	}
	fastembedGateCache.mu.Unlock()

	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		status := _inferenceFastembedApplyPromoteOverride(map[string]any{
			"required":  required,
			"available": false,
			"passed":    !required,
			"reason":    "gate_file_unreadable",
			"path":      path,
		})
		fastembedGateCache.mu.Lock()
		fastembedGateCache.path = path
		fastembedGateCache.mtimeUnix = stat.ModTime().Unix()
		fastembedGateCache.loadedAtMonotonic = now
		fastembedGateCache.status = status
		fastembedGateCache.mu.Unlock()
		return status
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		status := _inferenceFastembedApplyPromoteOverride(map[string]any{
			"required":  required,
			"available": false,
			"passed":    !required,
			"reason":    "gate_file_unreadable",
			"path":      path,
		})
		fastembedGateCache.mu.Lock()
		fastembedGateCache.path = path
		fastembedGateCache.mtimeUnix = stat.ModTime().Unix()
		fastembedGateCache.loadedAtMonotonic = now
		fastembedGateCache.status = status
		fastembedGateCache.mu.Unlock()
		return status
	}

	age := now.Sub(stat.ModTime())
	maxAge := _inferenceFastembedGateMaxAge()
	stale := age > maxAge
	passedRaw := anyToBool(payload["passed"])
	passed := passedRaw && !stale
	reason := strings.TrimSpace(anyToString(payload["reason"]))
	if reason == "" {
		if stale {
			reason = "stale_gate_result"
		} else {
			reason = "ok"
		}
	}
	status := _inferenceFastembedApplyPromoteOverride(map[string]any{
		"required":    required,
		"available":   true,
		"passed":      passed,
		"reason":      reason,
		"ageSecs":     age.Seconds(),
		"maxAgeSecs":  maxAge.Seconds(),
		"path":        path,
		"generatedAt": payload["generatedAt"],
		"metrics":     payload["metrics"],
		"thresholds":  payload["thresholds"],
	})
	fastembedGateCache.mu.Lock()
	fastembedGateCache.path = path
	fastembedGateCache.mtimeUnix = stat.ModTime().Unix()
	fastembedGateCache.loadedAtMonotonic = now
	fastembedGateCache.status = status
	fastembedGateCache.mu.Unlock()
	return status
}

func _inferenceFastembedAdapterEnabled() bool {
	baseEnabled := _inferenceFastembedAdapterEnabledByFlag()
	if !baseEnabled {
		return false
	}
	gate := _inferenceFastembedGateStatus()
	effectivePassed := anyToBool(gate["effectivePassed"])
	if _inferenceFastembedGateRequired() && !effectivePassed {
		return false
	}
	return true
}

func (s *server) inferenceANESidecarProbe(force bool) (bool, string) {
	ttl := envDurationSeconds("ORCH_ANE_SIDECAR_HEALTH_TTL_SECS", 10.0)
	now := time.Now()
	aneProbeCache.mu.Lock()
	if !force && !aneProbeCache.checkedAt.IsZero() && now.Sub(aneProbeCache.checkedAt) <= ttl {
		healthy := aneProbeCache.healthy
		detail := aneProbeCache.detail
		aneProbeCache.mu.Unlock()
		return healthy, detail
	}
	aneProbeCache.mu.Unlock()

	baseURL := _inferenceANESidecarURL()
	healthURL := _inferenceANESidecarHealthURL(baseURL)
	timeout := envDurationSeconds("ORCH_ANE_SIDECAR_TIMEOUT_SECS", 20.0)
	connectTimeout := envDurationSeconds("ORCH_ANE_SIDECAR_CONNECT_TIMEOUT_SECS", 2.0)
	apiKey := strings.TrimSpace(os.Getenv("ORCH_ANE_SIDECAR_API_KEY"))

	client := _inferenceHTTPClient(timeout, connectTimeout)
	req, err := http.NewRequest(http.MethodGet, healthURL, nil)
	if err != nil {
		aneProbeCache.mu.Lock()
		aneProbeCache.checkedAt = now
		aneProbeCache.healthy = false
		aneProbeCache.detail = err.Error()
		aneProbeCache.mu.Unlock()
		return false, err.Error()
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		aneProbeCache.mu.Lock()
		aneProbeCache.checkedAt = now
		aneProbeCache.healthy = false
		aneProbeCache.detail = err.Error()
		aneProbeCache.mu.Unlock()
		return false, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	healthy := false
	detail := "unreachable"
	if resp.StatusCode < 400 {
		healthy = true
		detail = "ok"
		payload := map[string]any{}
		if json.Unmarshal(raw, &payload) == nil {
			if value, exists := payload["healthy"]; exists {
				healthy = anyToBool(value)
			}
			if value := strings.TrimSpace(anyToString(payload["detail"])); value != "" {
				detail = value
			}
		}
	} else {
		detail = fmt.Sprintf("health %d", resp.StatusCode)
	}
	aneProbeCache.mu.Lock()
	aneProbeCache.checkedAt = now
	aneProbeCache.healthy = healthy
	aneProbeCache.detail = detail
	aneProbeCache.mu.Unlock()
	return healthy, detail
}

func (s *server) resolveInferenceRoute(requestedProvider string, baseURLOverride string, apiKey string) (inferenceRoute, error) {
	requested := strings.ToLower(strings.TrimSpace(requestedProvider))
	providerMode := strings.ToLower(strings.TrimSpace(os.Getenv("ORCH_INFER_PROVIDER")))
	if providerMode == "" {
		providerMode = "auto"
	}
	if requested == "" || requested == "auto" {
		requested = providerMode
	}

	resolveOllama := func(reason string) inferenceRoute {
		coremlEnabled := _inferenceCoreMLDefaultEnabled() && _inferenceIsMSeriesMac()
		provider := "ollama"
		if coremlEnabled {
			provider = "ollama_coreml"
		}
		return inferenceRoute{
			RequestedProvider: requested,
			Provider:          provider,
			Transport:         "ollama",
			BaseURL:           _inferenceBaseURLFromProvider("ollama", baseURLOverride),
			APIKey:            apiKey,
			Reason:            reason,
			CoreMLEnabled:     coremlEnabled,
			SidecarEnabled:    false,
		}
	}

	resolveANERoute := func() (inferenceRoute, bool) {
		if !_inferenceANESidecarEnabled() {
			return inferenceRoute{}, false
		}
		if _inferenceANESidecarRequireMSeries() && !_inferenceIsMSeriesMac() {
			return inferenceRoute{}, false
		}
		healthy, detail := s.inferenceANESidecarProbe(false)
		if !healthy {
			return inferenceRoute{}, false
		}
		sidecarAPIKey := strings.TrimSpace(os.Getenv("ORCH_ANE_SIDECAR_API_KEY"))
		if sidecarAPIKey == "" {
			sidecarAPIKey = strings.TrimSpace(apiKey)
		}
		return inferenceRoute{
			RequestedProvider: requested,
			Provider:          "ane_sidecar",
			Transport:         "openai",
			BaseURL:           _inferenceANESidecarURL(),
			APIKey:            sidecarAPIKey,
			Reason:            fmt.Sprintf("ane sidecar healthy (%s)", detail),
			CoreMLEnabled:     false,
			SidecarEnabled:    true,
		}, true
	}

	switch requested {
	case "ane", "ane_sidecar":
		if route, ok := resolveANERoute(); ok {
			return route, nil
		}
		if !_inferenceANESidecarFallbackEnabled() {
			return inferenceRoute{}, fmt.Errorf("ane sidecar requested but unavailable and fallback disabled")
		}
		return resolveOllama("ane sidecar unavailable; fell back to ollama"), nil
	case "auto":
		if route, ok := resolveANERoute(); ok {
			return route, nil
		}
		return resolveOllama("auto provider selected ollama"), nil
	case "ollama", "ollama_coreml":
		return resolveOllama("explicit ollama provider"), nil
	case "lmstudio", "openai-compatible", "openai_compatible", "vllm", "llama-cpp", "llama_cpp":
		return inferenceRoute{
			RequestedProvider: requested,
			Provider:          requested,
			Transport:         "openai",
			BaseURL:           _inferenceBaseURLFromProvider(requested, baseURLOverride),
			APIKey:            apiKey,
			Reason:            "explicit provider",
			CoreMLEnabled:     false,
			SidecarEnabled:    false,
		}, nil
	default:
		return inferenceRoute{
			RequestedProvider: requested,
			Provider:          "openai-compatible",
			Transport:         "openai",
			BaseURL:           _inferenceBaseURLFromProvider("openai-compatible", baseURLOverride),
			APIKey:            apiKey,
			Reason:            fmt.Sprintf("unknown provider '%s'; defaulted to openai-compatible", requested),
			CoreMLEnabled:     false,
			SidecarEnabled:    false,
		}, nil
	}
}

func _inferenceMessagesToPayload(messages []inferenceMessage) []map[string]string {
	payload := make([]map[string]string, 0, len(messages))
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		payload = append(payload, map[string]string{
			"role":    role,
			"content": message.Content,
		})
	}
	return payload
}

func _inferenceCallOpenAICompatible(
	baseURL string,
	model string,
	messages []inferenceMessage,
	apiKey string,
	timeout time.Duration,
	connectTimeout time.Duration,
) (string, error) {
	endpoint := _inferenceNormalizeOpenAIBase(baseURL) + "/chat/completions"
	payload := map[string]any{
		"model":       model,
		"messages":    _inferenceMessagesToPayload(messages),
		"temperature": 0.2,
		"stream":      false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	client := _inferenceHTTPClient(timeout, connectTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("openai-compatible status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	data := map[string]any{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return "", fmt.Errorf("openai-compatible response missing choices")
	}
	firstChoice, _ := choices[0].(map[string]any)
	message, _ := firstChoice["message"].(map[string]any)
	content := strings.TrimSpace(anyToString(message["content"]))
	if content == "" {
		return "", fmt.Errorf("openai-compatible response missing content")
	}
	return content, nil
}

func _inferenceCallOllama(baseURL string, model string, messages []inferenceMessage) (string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/chat"
	payload := map[string]any{
		"model":    model,
		"messages": _inferenceMessagesToPayload(messages),
		"stream":   false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	client := _inferenceHTTPClient(60*time.Second, 5*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("ollama status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	data := map[string]any{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	message, _ := data["message"].(map[string]any)
	content := strings.TrimSpace(anyToString(message["content"]))
	if content == "" {
		return "", fmt.Errorf("ollama response missing content")
	}
	return content, nil
}

func (s *server) callInferenceChat(route inferenceRoute, model string, messages []inferenceMessage) (string, inferenceRoute, error) {
	if strings.TrimSpace(model) == "" {
		return "", route, fmt.Errorf("model is required")
	}
	if len(messages) == 0 {
		return "", route, fmt.Errorf("messages are required")
	}
	if route.Transport == "ollama" {
		content, err := _inferenceCallOllama(route.BaseURL, model, messages)
		return content, route, err
	}
	timeout := 60 * time.Second
	connectTimeout := 5 * time.Second
	retries := 0
	backoff := 250 * time.Millisecond
	if route.Provider == "ane_sidecar" {
		timeout = envDurationSeconds("ORCH_ANE_SIDECAR_TIMEOUT_SECS", 20.0)
		connectTimeout = envDurationSeconds("ORCH_ANE_SIDECAR_CONNECT_TIMEOUT_SECS", 2.0)
		retries = envInt("ORCH_ANE_SIDECAR_RETRIES", 1)
		if retries < 0 {
			retries = 0
		}
		backoff = envDurationSeconds("ORCH_ANE_SIDECAR_RETRY_BACKOFF_SECS", 0.25)
		if backoff < 0 {
			backoff = 0
		}
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		content, err := _inferenceCallOpenAICompatible(route.BaseURL, model, messages, route.APIKey, timeout, connectTimeout)
		if err == nil {
			return content, route, nil
		}
		lastErr = err
		if attempt < retries {
			sleepFor := backoff * time.Duration(1<<attempt)
			if sleepFor > 0 {
				time.Sleep(sleepFor)
			}
		}
	}
	if route.Provider == "ane_sidecar" && _inferenceANESidecarFallbackEnabled() {
		fallbackRoute := inferenceRoute{
			RequestedProvider: route.RequestedProvider,
			Provider:          "ollama",
			Transport:         "ollama",
			BaseURL:           _inferenceBaseURLFromProvider("ollama", ""),
			Reason:            "ane sidecar request failed; fallback to ollama",
			CoreMLEnabled:     false,
			SidecarEnabled:    false,
		}
		content, err := _inferenceCallOllama(fallbackRoute.BaseURL, model, messages)
		return content, fallbackRoute, err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("inference route failed without an exception")
	}
	return "", route, lastErr
}

func (s *server) inferenceRouteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	rawBody, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload := inferenceRouteRequest{}
	if len(bytes.TrimSpace(rawBody)) > 0 {
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	route, err := s.resolveInferenceRoute(payload.Provider, payload.BaseURL, payload.APIKey)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "route": route})
}

func (s *server) inferenceChatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	rawBody, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload := inferenceChatRequest{}
	if len(bytes.TrimSpace(rawBody)) > 0 {
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	if strings.TrimSpace(payload.Model) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "model is required"})
		return
	}
	if len(payload.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "messages are required"})
		return
	}
	route, err := s.resolveInferenceRoute(payload.Provider, payload.BaseURL, payload.APIKey)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	content, activeRoute, err := s.callInferenceChat(route, payload.Model, payload.Messages)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"error": err.Error(),
			"route": activeRoute,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"model":   payload.Model,
		"content": content,
		"route":   activeRoute,
	})
}

func (s *server) inferenceEmbeddingPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	gate := _inferenceFastembedGateStatus()
	enabledByFlag := _inferenceFastembedAdapterEnabledByFlag()
	enabled := _inferenceFastembedAdapterEnabled()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"fastembedRs": map[string]any{
			"enabled":       enabled,
			"enabledByFlag": enabledByFlag,
			"configured":    strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_BASE_URL")) != "",
			"timeoutSecs":   envDurationSeconds("ORCH_FASTEMBED_RS_TIMEOUT_SECS", 2.5).Seconds(),
			"route":         strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_ROUTE")),
			"gate":          gate,
		},
	})
}
