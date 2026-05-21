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
	defaultInferenceMLXBaseURL       = "http://127.0.0.1:18087/v1"
	defaultInferenceLMStudioBaseURL  = "http://127.0.0.1:1234"
	defaultInferenceOpenAICompatBase = "http://127.0.0.1:8000"
	defaultInferenceVLLMBaseURL      = "http://127.0.0.1:8000"
	defaultInferenceVLLMMetalBaseURL = "http://127.0.0.1:8000"
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
	HardwareProfile   string `json:"hardware_profile,omitempty"`
	SelectionMode     string `json:"selection_mode,omitempty"`
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

type inferenceProviderProbeCache struct {
	mu      sync.Mutex
	entries map[string]inferenceProviderProbeResult
}

type inferenceProviderProbeResult struct {
	checkedAt time.Time
	healthy   bool
	detail    string
}

var aneProbeCache = inferenceANEProbeCache{
	detail: "never checked",
}

var fastembedGateCache = inferenceFastembedGateCache{}
var providerProbeCache = inferenceProviderProbeCache{entries: map[string]inferenceProviderProbeResult{}}

func _inferenceNormalizeProvider(provider string) string {
	token := strings.ToLower(strings.TrimSpace(provider))
	token = strings.ReplaceAll(token, "_", "-")
	switch token {
	case "":
		return ""
	case "ollama-coreml":
		return "ollama_coreml"
	case "ane", "ane-sidecar":
		return "ane_sidecar"
	case "openai", "openai-compatible", "openai-compat", "openai_compatible":
		return "openai-compatible"
	case "llamacpp", "llama-cpp", "llama_cpp":
		return "llama-cpp"
	case "mlx", "mlx-lm", "mlx_lm", "mtplx":
		return "mlx"
	case "vllm-metal", "vllm-metal-mlx", "vllm-mlx", "vllm_metal", "vllm_mlx":
		return "vllm-metal"
	default:
		return token
	}
}

func _inferenceProviderTransport(provider string) string {
	switch _inferenceNormalizeProvider(provider) {
	case "ollama", "ollama_coreml":
		return "ollama"
	default:
		return "openai"
	}
}

func _inferenceProviderDisplayName(provider string) string {
	switch _inferenceNormalizeProvider(provider) {
	case "vllm-metal":
		return "vLLM Metal"
	case "vllm":
		return "vLLM"
	case "mlx":
		return "MLX"
	case "ollama_coreml":
		return "Ollama CoreML"
	case "ane_sidecar":
		return "ANE sidecar"
	case "llama-cpp":
		return "llama.cpp"
	case "lmstudio":
		return "LM Studio"
	case "openai-compatible":
		return "OpenAI-compatible"
	default:
		return provider
	}
}

func _inferenceHostHardwareProfile() map[string]any {
	override := strings.TrimSpace(firstNonEmptyEnv("ORCH_HOST_HARDWARE_PROFILE", "CONTEXTLATTICE_HOST_HARDWARE_PROFILE"))
	if override != "" {
		override = strings.ToLower(strings.ReplaceAll(override, "-", "_"))
	}
	hostOS := strings.TrimSpace(firstNonEmptyEnv("ORCH_HOST_OS", "CONTEXTLATTICE_HOST_OS"))
	hostArch := strings.TrimSpace(firstNonEmptyEnv("ORCH_HOST_ARCH", "CONTEXTLATTICE_HOST_ARCH"))
	if hostOS == "" {
		hostOS = runtime.GOOS
	}
	if hostArch == "" {
		hostArch = runtime.GOARCH
	}
	kernelVersion := ""
	if raw, err := os.ReadFile("/proc/version"); err == nil {
		kernelVersion = strings.ToLower(string(raw))
	}
	orbStackAppleContainer := runtime.GOOS == "linux" && runtime.GOARCH == "arm64" && strings.Contains(kernelVersion, "orbstack")
	dockerDesktopAppleContainer := runtime.GOOS == "linux" && runtime.GOARCH == "arm64" && strings.Contains(kernelVersion, "linuxkit")
	if (orbStackAppleContainer || dockerDesktopAppleContainer) && firstNonEmptyEnv("ORCH_HOST_OS", "CONTEXTLATTICE_HOST_OS") == "" {
		hostOS = "darwin"
		hostArch = "arm64"
	}
	appleSilicon := (strings.ToLower(hostOS) == "darwin" && (hostArch == "arm64" || hostArch == "aarch64")) ||
		(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") ||
		orbStackAppleContainer ||
		dockerDesktopAppleContainer
	cudaSignals := []string{
		os.Getenv("NVIDIA_VISIBLE_DEVICES"),
		os.Getenv("CUDA_VISIBLE_DEVICES"),
		os.Getenv("NVIDIA_DRIVER_CAPABILITIES"),
	}
	rocmSignals := []string{
		os.Getenv("ROCR_VISIBLE_DEVICES"),
		os.Getenv("HIP_VISIBLE_DEVICES"),
		os.Getenv("HSA_VISIBLE_DEVICES"),
	}
	hasCUDA := false
	for _, value := range cudaSignals {
		token := strings.TrimSpace(strings.ToLower(value))
		if token != "" && token != "none" && token != "void" {
			hasCUDA = true
			break
		}
	}
	if !hasCUDA {
		if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
			hasCUDA = true
		} else if _, err := os.Stat("/dev/nvidia0"); err == nil {
			hasCUDA = true
		}
	}
	hasROCm := false
	for _, value := range rocmSignals {
		token := strings.TrimSpace(strings.ToLower(value))
		if token != "" && token != "none" && token != "void" {
			hasROCm = true
			break
		}
	}
	if !hasROCm {
		if _, err := os.Stat("/dev/kfd"); err == nil {
			hasROCm = true
		}
	}
	profile := "generic_cpu"
	if override != "" {
		profile = override
	} else if appleSilicon {
		profile = "apple_silicon"
	} else if hasCUDA {
		profile = "nvidia_cuda"
	} else if hasROCm {
		profile = "amd_rocm"
	}
	return map[string]any{
		"profile":       profile,
		"goos":          runtime.GOOS,
		"goarch":        runtime.GOARCH,
		"hostOS":        hostOS,
		"hostArch":      hostArch,
		"appleSilicon":  appleSilicon,
		"cudaDetected":  hasCUDA,
		"rocmDetected":  hasROCm,
		"metalDetected": appleSilicon,
		"containerRuntime": map[string]any{
			"orbstackKernel":      orbStackAppleContainer,
			"dockerDesktopKernel": dockerDesktopAppleContainer,
		},
	}
}

func _inferenceDefaultProviderPriorityForHardware(profile string) []string {
	switch profile {
	case "apple_silicon":
		return []string{"vllm-metal", "mlx", "ane_sidecar", "llama-cpp", "ollama"}
	case "nvidia_cuda", "amd_rocm":
		return []string{"vllm", "openai-compatible", "llama-cpp", "lmstudio", "ollama"}
	default:
		return []string{"openai-compatible", "llama-cpp", "lmstudio", "ollama"}
	}
}

func _inferenceProviderPriority() []string {
	raw := strings.TrimSpace(os.Getenv("ORCH_INFER_PROVIDER_PRIORITY"))
	if raw == "" {
		hardware := _inferenceHostHardwareProfile()
		profile := anyToString(hardware["profile"])
		priority := _inferenceDefaultProviderPriorityForHardware(profile)
		if _inferenceAutoPreferMLX() {
			priority = append([]string{"mlx"}, priority...)
		}
		return dedupeStringSlice(priority)
	}
	out := []string{}
	for _, item := range strings.Split(raw, ",") {
		provider := _inferenceNormalizeProvider(item)
		if provider != "" {
			out = append(out, provider)
		}
	}
	if len(out) == 0 {
		hardware := _inferenceHostHardwareProfile()
		return _inferenceDefaultProviderPriorityForHardware(anyToString(hardware["profile"]))
	}
	return dedupeStringSlice(out)
}

func dedupeStringSlice(values []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		token := strings.TrimSpace(value)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

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
	switch _inferenceNormalizeProvider(provider) {
	case "ollama", "ollama_coreml":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("OLLAMA_BASE_URL"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceOllamaBaseURL
	case "mlx":
		value := strings.TrimSpace(strings.TrimRight(os.Getenv("MLX_API_BASE"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceMLXBaseURL
	case "lmstudio":
		value := strings.TrimSpace(strings.TrimRight(firstNonEmptyEnv("LMSTUDIO_BASE_URL", "LM_STUDIO_BASE_URL"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceLMStudioBaseURL
	case "vllm":
		value := strings.TrimSpace(strings.TrimRight(firstNonEmptyEnv("VLLM_BASE_URL", "OPENAI_API_BASE"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceVLLMBaseURL
	case "vllm-metal":
		value := strings.TrimSpace(strings.TrimRight(firstNonEmptyEnv("VLLM_METAL_BASE_URL", "VLLM_BASE_URL", "OPENAI_API_BASE"), "/"))
		if value != "" {
			return value
		}
		return defaultInferenceVLLMMetalBaseURL
	case "openai-compatible":
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

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		value := strings.TrimSpace(os.Getenv(name))
		if value != "" {
			return value
		}
	}
	return ""
}

func _inferenceIsMSeriesMac() bool {
	hardware := _inferenceHostHardwareProfile()
	if appleSilicon, ok := hardware["appleSilicon"].(bool); ok && appleSilicon {
		return true
	}
	return anyToString(hardware["profile"]) == "apple_silicon"
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

func _inferenceAutoPreferMLX() bool {
	return envBool("ORCH_INFER_AUTO_PREFER_MLX", false)
}

func _inferenceAutoProbeEnabled() bool {
	return envBool("ORCH_INFER_AUTO_PROBE_ENABLED", true)
}

func _inferenceAutoProbeTimeout() time.Duration {
	timeout := envDurationSeconds("ORCH_INFER_AUTO_PROBE_TIMEOUT_SECS", 0.45)
	if timeout < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	return timeout
}

func _inferenceAutoProbeTTL() time.Duration {
	ttl := envDurationSeconds("ORCH_INFER_AUTO_PROBE_TTL_SECS", 10.0)
	if ttl < time.Second {
		return time.Second
	}
	return ttl
}

func _inferenceProbeProvider(provider string, baseURL string, transport string) (bool, string) {
	provider = _inferenceNormalizeProvider(provider)
	if !_inferenceAutoProbeEnabled() {
		return true, "probe disabled"
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return false, "base url not configured"
	}
	key := provider + "|" + transport + "|" + baseURL
	now := time.Now()
	ttl := _inferenceAutoProbeTTL()
	providerProbeCache.mu.Lock()
	if cached, ok := providerProbeCache.entries[key]; ok && now.Sub(cached.checkedAt) <= ttl {
		providerProbeCache.mu.Unlock()
		return cached.healthy, cached.detail
	}
	providerProbeCache.mu.Unlock()

	urls := []string{}
	if transport == "ollama" {
		urls = append(urls, baseURL+"/api/tags")
		urls = append(urls, _inferenceNormalizeOpenAIBase(baseURL)+"/models")
	} else {
		urls = append(urls, _inferenceNormalizeOpenAIBase(baseURL)+"/models")
	}
	client := _inferenceHTTPClient(_inferenceAutoProbeTimeout(), _inferenceAutoProbeTimeout())
	healthy := false
	detail := "unreachable"
	for _, probeURL := range urls {
		req, err := http.NewRequest(http.MethodGet, probeURL, nil)
		if err != nil {
			detail = err.Error()
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			detail = err.Error()
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			healthy = true
			detail = "healthy via " + probeURL
			break
		}
		detail = fmt.Sprintf("%s status %d", probeURL, resp.StatusCode)
	}
	providerProbeCache.mu.Lock()
	providerProbeCache.entries[key] = inferenceProviderProbeResult{checkedAt: now, healthy: healthy, detail: detail}
	providerProbeCache.mu.Unlock()
	return healthy, detail
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
	requested := _inferenceNormalizeProvider(requestedProvider)
	providerMode := _inferenceNormalizeProvider(os.Getenv("ORCH_INFER_PROVIDER"))
	if providerMode == "" {
		providerMode = "auto"
	}
	if requested == "" || requested == "auto" || requested == "hardware-auto" {
		requested = providerMode
	}
	if requested == "hardware-auto" {
		requested = "auto"
	}
	hardware := _inferenceHostHardwareProfile()
	hardwareProfile := anyToString(hardware["profile"])

	resolveOllama := func(reason string, selectionMode string) inferenceRoute {
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
			HardwareProfile:   hardwareProfile,
			SelectionMode:     selectionMode,
		}
	}

	resolveOpenAIProvider := func(provider string, reason string, selectionMode string) inferenceRoute {
		provider = _inferenceNormalizeProvider(provider)
		providerAPIKey := strings.TrimSpace(apiKey)
		if provider == "mlx" {
			if mlxAPIKey := strings.TrimSpace(os.Getenv("MLX_API_KEY")); mlxAPIKey != "" {
				providerAPIKey = mlxAPIKey
			}
		}
		return inferenceRoute{
			RequestedProvider: requested,
			Provider:          provider,
			Transport:         "openai",
			BaseURL:           _inferenceBaseURLFromProvider(provider, baseURLOverride),
			APIKey:            providerAPIKey,
			Reason:            reason,
			CoreMLEnabled:     false,
			SidecarEnabled:    false,
			HardwareProfile:   hardwareProfile,
			SelectionMode:     selectionMode,
		}
	}

	resolveANERoute := func(selectionMode string) (inferenceRoute, bool) {
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
			HardwareProfile:   hardwareProfile,
			SelectionMode:     selectionMode,
		}, true
	}

	switch requested {
	case "ane", "ane_sidecar":
		if route, ok := resolveANERoute("explicit"); ok {
			return route, nil
		}
		if !_inferenceANESidecarFallbackEnabled() {
			return inferenceRoute{}, fmt.Errorf("ane sidecar requested but unavailable and fallback disabled")
		}
		return resolveOllama("ane sidecar unavailable; fell back to ollama", "explicit-fallback"), nil
	case "auto":
		probeNotes := []string{}
		priority := _inferenceProviderPriority()
		for _, candidate := range priority {
			provider := _inferenceNormalizeProvider(candidate)
			if provider == "" {
				continue
			}
			if provider == "ane_sidecar" {
				if route, ok := resolveANERoute("auto"); ok {
					return route, nil
				}
				probeNotes = append(probeNotes, "ane_sidecar unavailable")
				continue
			}
			var route inferenceRoute
			if provider == "ollama" || provider == "ollama_coreml" {
				route = resolveOllama("", "auto")
			} else {
				route = resolveOpenAIProvider(provider, "", "auto")
			}
			healthy, detail := _inferenceProbeProvider(route.Provider, route.BaseURL, route.Transport)
			if healthy {
				route.Reason = fmt.Sprintf("auto selected %s for %s (%s)", route.Provider, hardwareProfile, detail)
				return route, nil
			}
			probeNotes = append(probeNotes, route.Provider+" "+detail)
		}
		fallbackProvider := _inferenceNormalizeProvider(os.Getenv("ORCH_INFER_AUTO_FALLBACK_PROVIDER"))
		if fallbackProvider == "" {
			fallbackProvider = "ollama"
		}
		reason := "auto found no healthy preferred provider"
		if len(probeNotes) > 0 {
			reason += ": " + strings.Join(probeNotes, "; ")
		}
		reason += "; fallback to " + fallbackProvider
		if fallbackProvider == "ane_sidecar" {
			if route, ok := resolveANERoute("auto-fallback"); ok {
				route.Reason = reason
				return route, nil
			}
			fallbackProvider = "ollama"
		}
		if fallbackProvider == "ollama" || fallbackProvider == "ollama_coreml" {
			return resolveOllama(reason, "auto-fallback"), nil
		}
		return resolveOpenAIProvider(fallbackProvider, reason, "auto-fallback"), nil
	case "ollama", "ollama_coreml":
		return resolveOllama("explicit ollama provider", "explicit"), nil
	case "mlx":
		return resolveOpenAIProvider("mlx", "explicit mlx provider", "explicit"), nil
	case "lmstudio", "openai-compatible", "vllm", "vllm-metal", "llama-cpp":
		return resolveOpenAIProvider(requested, "explicit provider", "explicit"), nil
	default:
		return resolveOpenAIProvider("openai-compatible", fmt.Sprintf("unknown provider '%s'; defaulted to openai-compatible", requested), "explicit-fallback"), nil
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

func _inferenceProviderUseCase(provider string) string {
	switch _inferenceNormalizeProvider(provider) {
	case "vllm":
		return "automatic on CUDA/ROCm hosts; manual advanced elsewhere"
	case "vllm-metal":
		return "automatic on Apple Silicon when healthy"
	case "mlx":
		return "automatic on Apple Silicon when healthy; mtplx alias supported"
	case "ane_sidecar":
		return "automatic only when explicitly enabled and healthy"
	case "ollama":
		return "fallback and compatibility lane"
	case "llama-cpp", "lmstudio", "openai-compatible":
		return "manual advanced or configured fallback"
	default:
		return "manual advanced"
	}
}

func (s *server) inferenceRuntimePolicyPayload() map[string]any {
	hardware := _inferenceHostHardwareProfile()
	priority := _inferenceProviderPriority()
	selected, err := s.resolveInferenceRoute("auto", "", "")
	candidates := []map[string]any{}
	for _, provider := range priority {
		provider = _inferenceNormalizeProvider(provider)
		if provider == "" {
			continue
		}
		transport := _inferenceProviderTransport(provider)
		baseURL := _inferenceBaseURLFromProvider(provider, "")
		healthy := false
		detail := "not probed"
		if provider == "ane_sidecar" {
			if _inferenceANESidecarEnabled() {
				healthy, detail = s.inferenceANESidecarProbe(false)
			} else {
				detail = "disabled"
			}
		} else {
			healthy, detail = _inferenceProbeProvider(provider, baseURL, transport)
		}
		candidates = append(candidates, map[string]any{
			"provider":       provider,
			"displayName":    _inferenceProviderDisplayName(provider),
			"transport":      transport,
			"baseURL":        baseURL,
			"healthy":        healthy,
			"detail":         detail,
			"useCase":        _inferenceProviderUseCase(provider),
			"manualAdvanced": provider == "openai-compatible" || provider == "lmstudio" || provider == "llama-cpp",
		})
	}
	payload := map[string]any{
		"ok":               err == nil,
		"hardware":         hardware,
		"priority":         priority,
		"autoProbe":        _inferenceAutoProbeEnabled(),
		"autoProbeTTL":     _inferenceAutoProbeTTL().Seconds(),
		"autoProbeTimeout": _inferenceAutoProbeTimeout().Seconds(),
		"manualAdvancedProviders": []string{
			"vllm",
			"vllm-metal",
			"mlx",
			"mtplx",
			"openai-compatible",
			"lmstudio",
			"llama-cpp",
			"ollama",
		},
		"candidates": candidates,
	}
	if err != nil {
		payload["error"] = err.Error()
	} else {
		payload["selected"] = selected
	}
	return payload
}

func (s *server) inferenceRuntimePolicyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.inferenceRuntimePolicyPayload())
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
	provider := nativeEmbeddingProvider()
	selected := "cheap"
	if enabled && nativeEmbeddingProviderUsesFastembed(provider) {
		selected = "fastembed-rs"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"provider": provider,
		"selected": selected,
		"note":     "fastembed-rs is preferred for local embeddings; Ollama is compatibility fallback only.",
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
