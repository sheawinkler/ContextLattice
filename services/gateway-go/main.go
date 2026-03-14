package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	sourceQdrant      = "qdrant"
	sourceMongoRaw    = "mongo_raw"
	sourceMindsdb     = "mindsdb"
	sourceTopicRollup = "topic_rollups"
	sourceLetta       = "letta"
	sourceMemoryBank  = "memory_bank"
)

var defaultAllSources = []string{
	sourceQdrant,
	sourceMongoRaw,
	sourceMindsdb,
	sourceTopicRollup,
	sourceLetta,
	sourceMemoryBank,
}

type retrievalPolicy struct {
	enabled                     bool
	defaultSources              []string
	fastSources                 []string
	slowSources                 []string
	syncFallbackSources         []string
	minFastResults              int
	lexicalGuardEnabled         bool
	lexicalGuardMinCoverage     float64
	lexicalGuardMinResults      int
	deepBlocking                bool
	qdrantSyncTimeoutCap        time.Duration
	qdrantSyncTimeoutCapByMode  map[string]time.Duration
	failOpenContinuationEnabled bool
	failOpenContinuationSources map[string]struct{}
	timeoutAdaptiveSkipEnabled  bool
	timeoutAdaptiveSkipSources  map[string]struct{}
	sourceTimeouts              map[string]time.Duration
	continuationTimeoutDefault  time.Duration
	continuationTimeoutBySource map[string]time.Duration
	continuationMaxInflight     int
	subcallDisableExpansion     bool
	subcallDisableAutoEscalate  bool
	telemetryBatchEnabled       bool
	telemetryBatchFlushInterval time.Duration
	telemetryBatchSize          int
	telemetryBatchDropLogEvery  uint64
}

type retrievalEvent struct {
	Source    string
	Phase     string
	Status    string
	LatencyMs int64
}

type retrievalTelemetry struct {
	enabled       bool
	flushInterval time.Duration
	batchSize     int
	dropLogEvery  uint64
	ch            chan retrievalEvent
	dropped       atomic.Uint64
	stop          chan struct{}
	stopped       chan struct{}
}

func newRetrievalTelemetry(policy retrievalPolicy) *retrievalTelemetry {
	if !policy.telemetryBatchEnabled {
		return &retrievalTelemetry{enabled: false}
	}
	bufferSize := policy.telemetryBatchSize * 8
	if bufferSize < 64 {
		bufferSize = 64
	}
	return &retrievalTelemetry{
		enabled:       true,
		flushInterval: policy.telemetryBatchFlushInterval,
		batchSize:     policy.telemetryBatchSize,
		dropLogEvery:  policy.telemetryBatchDropLogEvery,
		ch:            make(chan retrievalEvent, bufferSize),
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
}

func (t *retrievalTelemetry) start() {
	if t == nil || !t.enabled {
		return
	}
	go func() {
		defer close(t.stopped)
		ticker := time.NewTicker(t.flushInterval)
		defer ticker.Stop()
		batch := make([]retrievalEvent, 0, t.batchSize)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			type agg struct {
				count   int
				latency int64
			}
			rollup := make(map[string]agg)
			for _, event := range batch {
				key := event.Phase + "|" + event.Source + "|" + event.Status
				row := rollup[key]
				row.count += 1
				row.latency += event.LatencyMs
				rollup[key] = row
			}
			keys := make([]string, 0, len(rollup))
			for key := range rollup {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			rows := make([]string, 0, len(keys))
			for _, key := range keys {
				item := rollup[key]
				avgMs := int64(0)
				if item.count > 0 {
					avgMs = item.latency / int64(item.count)
				}
				rows = append(rows, key+"="+strconv.Itoa(item.count)+"@"+strconv.FormatInt(avgMs, 10)+"ms")
			}
			log.Printf("retrieval telemetry batch: %s", strings.Join(rows, ", "))
			batch = batch[:0]
		}

		for {
			select {
			case <-t.stop:
				flush()
				return
			case <-ticker.C:
				flush()
			case event := <-t.ch:
				batch = append(batch, event)
				if len(batch) >= t.batchSize {
					flush()
				}
			}
		}
	}()
}

func (t *retrievalTelemetry) record(event retrievalEvent) {
	if t == nil || !t.enabled {
		return
	}
	select {
	case t.ch <- event:
	default:
		dropped := t.dropped.Add(1)
		if t.dropLogEvery > 0 && dropped%t.dropLogEvery == 0 {
			log.Printf("retrieval telemetry dropped events=%d", dropped)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type server struct {
	backendURL      string
	client          *http.Client
	retrieval       retrievalPolicy
	telemetry       *retrievalTelemetry
	continuationSem chan struct{}
}

func envDurationSeconds(name string, fallback float64) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return time.Duration(fallback * float64(time.Second))
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || secs <= 0 {
		return time.Duration(fallback * float64(time.Second))
	}
	return time.Duration(secs * float64(time.Second))
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envFloat(name string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

func csvListEnv(name string, fallback string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		raw = fallback
	}
	parts := strings.Split(raw, ",")
	rows := make([]string, 0, len(parts))
	for _, part := range parts {
		candidate := strings.TrimSpace(strings.ToLower(part))
		if candidate == "" {
			continue
		}
		rows = append(rows, candidate)
	}
	return normalizeSourceList(rows)
}

func normalizeSourceList(sources []string) []string {
	out := make([]string, 0, len(sources))
	seen := make(map[string]struct{})
	for _, source := range sources {
		normalized := strings.TrimSpace(strings.ToLower(source))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func toSourceSet(sources []string) map[string]struct{} {
	set := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		set[source] = struct{}{}
	}
	return set
}

func normalizeRustBackendChoice(raw string, allowed map[string]struct{}, fallback string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return fallback
	}
	if _, ok := allowed[value]; ok {
		return value
	}
	return fallback
}

func defaultRustBackendPolicy() map[string]any {
	vectorAllowed := map[string]struct{}{
		"auto":          {},
		"qdrant_remote": {},
		"usearch_ann":   {},
	}
	lexicalAllowed := map[string]struct{}{
		"auto":            {},
		"none":            {},
		"tantivy_lexical": {},
	}
	return map[string]any{
		"vector_backend": normalizeRustBackendChoice(
			os.Getenv("ORCH_RUST_RETRIEVAL_VECTOR_BACKEND"),
			vectorAllowed,
			"auto",
		),
		"lexical_backend": normalizeRustBackendChoice(
			os.Getenv("ORCH_RUST_RETRIEVAL_LEXICAL_BACKEND"),
			lexicalAllowed,
			"auto",
		),
		"strict": envBool("ORCH_RUST_RETRIEVAL_BACKEND_STRICT", false),
	}
}

func resolveRustBackendPolicy(raw any) map[string]any {
	resolved := defaultRustBackendPolicy()
	policy, ok := raw.(map[string]any)
	if !ok {
		return resolved
	}
	if value, ok := policy["vector_backend"]; ok {
		resolved["vector_backend"] = normalizeRustBackendChoice(
			anyToString(value),
			map[string]struct{}{
				"auto":          {},
				"qdrant_remote": {},
				"usearch_ann":   {},
			},
			anyToString(resolved["vector_backend"]),
		)
	}
	if value, ok := policy["lexical_backend"]; ok {
		resolved["lexical_backend"] = normalizeRustBackendChoice(
			anyToString(value),
			map[string]struct{}{
				"auto":            {},
				"none":            {},
				"tantivy_lexical": {},
			},
			anyToString(resolved["lexical_backend"]),
		)
	}
	if value, ok := policy["strict"]; ok {
		resolved["strict"] = anyToBool(value)
	}
	return resolved
}

func loadRetrievalPolicy() retrievalPolicy {
	policy := retrievalPolicy{}
	policy.enabled = envBool("GO_RETRIEVAL_STAGED_ENABLED", true)
	policy.defaultSources = csvListEnv("ORCH_RETRIEVAL_SOURCES", strings.Join(defaultAllSources, ","))
	if len(policy.defaultSources) == 0 {
		policy.defaultSources = append([]string(nil), defaultAllSources...)
	}
	policy.fastSources = csvListEnv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups,qdrant")
	policy.slowSources = csvListEnv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb,mongo_raw,letta,memory_bank")
	policy.syncFallbackSources = csvListEnv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "mindsdb,mongo_raw")
	policy.minFastResults = envInt("ORCH_RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", 2)
	if policy.minFastResults < 1 {
		policy.minFastResults = 1
	}
	policy.lexicalGuardEnabled = envBool("GO_RETRIEVAL_LEXICAL_GUARD_ENABLED", true)
	policy.lexicalGuardMinCoverage = envFloat("GO_RETRIEVAL_LEXICAL_GUARD_MIN_COVERAGE", 0.55)
	if policy.lexicalGuardMinCoverage < 0 {
		policy.lexicalGuardMinCoverage = 0
	}
	if policy.lexicalGuardMinCoverage > 1 {
		policy.lexicalGuardMinCoverage = 1
	}
	policy.lexicalGuardMinResults = envInt("GO_RETRIEVAL_LEXICAL_GUARD_MIN_RESULTS", 1)
	if policy.lexicalGuardMinResults < 1 {
		policy.lexicalGuardMinResults = 1
	}
	policy.deepBlocking = envBool("ORCH_RETRIEVAL_SYNC_ASYNC_DEEP_BLOCKING", false)
	policy.qdrantSyncTimeoutCap = envDurationSeconds("ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_SECS", 4)
	policy.qdrantSyncTimeoutCapByMode = map[string]time.Duration{
		"fast": envDurationSeconds(
			"ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_FAST_SECS",
			policy.qdrantSyncTimeoutCap.Seconds(),
		),
		"balanced": envDurationSeconds(
			"ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_BALANCED_SECS",
			policy.qdrantSyncTimeoutCap.Seconds(),
		),
		"deep": envDurationSeconds(
			"ORCH_RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_DEEP_SECS",
			policy.qdrantSyncTimeoutCap.Seconds(),
		),
	}
	policy.failOpenContinuationEnabled = envBool("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", true)
	policy.failOpenContinuationSources = toSourceSet(csvListEnv(
		"ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES",
		"letta,memory_bank",
	))
	policy.timeoutAdaptiveSkipEnabled = envBool("ORCH_RECALL_TIMEOUT_ADAPTIVE_SOURCE_SKIP_ENABLED", true)
	policy.timeoutAdaptiveSkipSources = toSourceSet(csvListEnv(
		"ORCH_RECALL_TIMEOUT_ADAPTIVE_SKIP_SOURCES",
		"qdrant,mindsdb,mongo_raw",
	))
	policy.sourceTimeouts = map[string]time.Duration{
		sourceQdrant:      envDurationSeconds("ORCH_RETRIEVAL_QDRANT_TIMEOUT_SECS", 8),
		sourceMongoRaw:    envDurationSeconds("ORCH_RETRIEVAL_MONGO_TIMEOUT_SECS", 6),
		sourceMindsdb:     envDurationSeconds("ORCH_RETRIEVAL_MINDSDB_TIMEOUT_SECS", 8),
		sourceTopicRollup: envDurationSeconds("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", 2),
		sourceLetta:       envDurationSeconds("ORCH_RETRIEVAL_LETTA_TIMEOUT_SECS", 45),
		sourceMemoryBank:  envDurationSeconds("ORCH_RETRIEVAL_MEMORY_TIMEOUT_SECS", 3),
	}
	policy.continuationTimeoutDefault = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_TIMEOUT_SECS", 45)
	policy.continuationTimeoutBySource = map[string]time.Duration{
		sourceLetta:      envDurationSeconds("ORCH_RETRIEVAL_LETTA_ASYNC_WARM_TIMEOUT_SECS", 180),
		sourceMemoryBank: envDurationSeconds("ORCH_RETRIEVAL_MEMORY_DEEP_TIMEOUT_CAP_SECS", 18),
	}
	policy.continuationMaxInflight = envInt("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT", 4)
	if policy.continuationMaxInflight < 1 {
		policy.continuationMaxInflight = 1
	}
	policy.subcallDisableExpansion = envBool("GO_RETRIEVAL_SUBCALL_DISABLE_EXPANSION", true)
	policy.subcallDisableAutoEscalate = envBool("GO_RETRIEVAL_SUBCALL_DISABLE_AUTO_ESCALATE", true)
	policy.telemetryBatchEnabled = envBool("GO_RETRIEVAL_EVENT_BATCH_ENABLED", true)
	policy.telemetryBatchFlushInterval = envDurationSeconds("GO_RETRIEVAL_EVENT_BATCH_FLUSH_SECS", 3)
	policy.telemetryBatchSize = envInt("GO_RETRIEVAL_EVENT_BATCH_SIZE", 64)
	if policy.telemetryBatchSize < 8 {
		policy.telemetryBatchSize = 8
	}
	policy.telemetryBatchDropLogEvery = uint64(envInt("GO_RETRIEVAL_EVENT_BATCH_DROP_LOG_EVERY", 100))
	if policy.telemetryBatchDropLogEvery == 0 {
		policy.telemetryBatchDropLogEvery = 100
	}
	return policy
}

func newServer() *server {
	backendURL := strings.TrimRight(strings.TrimSpace(os.Getenv("BACKEND_URL")), "/")
	if backendURL == "" {
		backendURL = "http://contextlattice-orchestrator:8075"
	}
	timeout := envDurationSeconds("GATEWAY_PROXY_TIMEOUT_SECS", 95)
	policy := loadRetrievalPolicy()
	t := newRetrievalTelemetry(policy)
	s := &server{
		backendURL:      backendURL,
		client:          &http.Client{Timeout: timeout},
		retrieval:       policy,
		telemetry:       t,
		continuationSem: make(chan struct{}, policy.continuationMaxInflight),
	}
	t.start()
	return s
}

func isProxyPath(path string) bool {
	if strings.HasPrefix(path, "/memory/search/async/") {
		return true
	}
	if strings.HasPrefix(path, "/memory/search/jobs/") {
		return true
	}
	switch path {
	case "/v1/retrieval/query",
		"/v1/retrieval/query-with-grounding",
		"/v1/retrieval/batch-query",
		"/v1/retrieval/health",
		"/memory/search",
		"/memory/recall/eval-cases",
		"/memory/recall/eval-cases/refresh",
		"/memory/recall/evaluate/saved",
		"/memory/write/batch",
		"/memory/browser-context",
		"/memory/context-pack",
		"/ops/queue/status",
		"/ops/capabilities",
		"/tools/capability_map",
		"/tools/ops_queue_status",
		"/tools/memory_write_batch",
		"/v1/memory/put",
		"/v1/memory/update",
		"/v1/memory/get",
		"/v1/memory/neighbors",
		"/v1/memory/batch-put",
		"/migration/runtime":
		return true
	default:
		return false
	}
}

func isStreamingProxyPath(path string) bool {
	return strings.HasPrefix(path, "/memory/search/jobs/") && strings.HasSuffix(path, "/events")
}

func (s *server) copyHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	if !isProxyPath(r.URL.Path) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	s.proxyWithBody(w, r, bodyBytes)
}

func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func (s *server) proxyWithBody(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	targetURL := s.backendURL + r.URL.Path
	if query := r.URL.RawQuery; query != "" {
		targetURL += "?" + query
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to build proxy request"})
		return
	}
	s.copyHeaders(req.Header, r.Header)
	if req.Header.Get("X-Forwarded-For") == "" {
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")

	if isStreamingProxyPath(r.URL.Path) && strings.EqualFold(r.Method, http.MethodGet) {
		s.proxyStream(w, req)
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     err.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to read backend response"})
		return
	}

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (s *server) proxyStream(w http.ResponseWriter, req *http.Request) {
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     err.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		_, _ = io.Copy(w, resp.Body)
		return
	}
	buffer := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return
		}
		return
	}
}

func (s *server) backendHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.backendURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"service":       "gateway-go",
		"backendUrl":    s.backendURL,
		"backendHealth": s.backendHealthy(ctx),
	})
}

func (s *server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"description": "ContextLattice gateway-go retrieval/memory proxy",
		"backendUrl":  s.backendURL,
		"retrieval": map[string]any{
			"stagedEnabled":            s.retrieval.enabled,
			"fastSources":              s.retrieval.fastSources,
			"slowSources":              s.retrieval.slowSources,
			"qdrantSyncTimeoutCapSecs": s.retrieval.qdrantSyncTimeoutCap.Seconds(),
			"qdrantSyncTimeoutCapByModeSecs": map[string]any{
				"fast":     s.retrieval.qdrantSyncTimeoutCapByMode["fast"].Seconds(),
				"balanced": s.retrieval.qdrantSyncTimeoutCapByMode["balanced"].Seconds(),
				"deep":     s.retrieval.qdrantSyncTimeoutCapByMode["deep"].Seconds(),
			},
			"failOpenContinuationEnabled":     s.retrieval.failOpenContinuationEnabled,
			"timeoutAdaptiveSkipEnabled":      s.retrieval.timeoutAdaptiveSkipEnabled,
			"lexicalGuardEnabled":             s.retrieval.lexicalGuardEnabled,
			"lexicalGuardMinCoverage":         s.retrieval.lexicalGuardMinCoverage,
			"lexicalGuardMinResults":          s.retrieval.lexicalGuardMinResults,
			"continuationMaxInflight":         s.retrieval.continuationMaxInflight,
			"subcallDisableExpansion":         s.retrieval.subcallDisableExpansion,
			"subcallDisableAutoEscalate":      s.retrieval.subcallDisableAutoEscalate,
			"telemetryBatchEnabled":           s.retrieval.telemetryBatchEnabled,
			"telemetryBatchFlushIntervalSecs": s.retrieval.telemetryBatchFlushInterval.Seconds(),
		},
	})
}

func parseJSONMap(body []byte) (map[string]any, error) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, nil
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func anyToStringSlice(value any) []string {
	rows := []string{}
	rawList, ok := value.([]any)
	if ok {
		for _, item := range rawList {
			candidate := strings.TrimSpace(strings.ToLower(anyToString(item)))
			if candidate != "" {
				rows = append(rows, candidate)
			}
		}
		return normalizeSourceList(rows)
	}
	stringList, ok := value.([]string)
	if ok {
		return normalizeSourceList(stringList)
	}
	return rows
}

func anyToString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func anyToInt(value any, fallback int) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err == nil {
			return parsed
		}
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func anyToBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.TrimSpace(strings.ToLower(typed)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return parsed != 0
		}
	}
	return false
}

func clampInt(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func dedupeWarnings(input []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(input))
	for _, warning := range input {
		candidate := strings.TrimSpace(warning)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func parseWarnings(value any) []string {
	switch typed := value.(type) {
	case []any:
		rows := make([]string, 0, len(typed))
		for _, item := range typed {
			candidate := strings.TrimSpace(anyToString(item))
			if candidate != "" {
				rows = append(rows, candidate)
			}
		}
		return rows
	case []string:
		rows := make([]string, 0, len(typed))
		for _, item := range typed {
			candidate := strings.TrimSpace(item)
			if candidate != "" {
				rows = append(rows, candidate)
			}
		}
		return rows
	default:
		return nil
	}
}

func parseRows(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			return typed
		}
		return nil
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if row, ok := item.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

func parseScore(row map[string]any) float64 {
	for _, key := range []string{"score", "hybrid_score", "similarity", "confidence"} {
		value, ok := row[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return typed
		case float32:
			return float64(typed)
		case int:
			return float64(typed)
		case int64:
			return float64(typed)
		case json.Number:
			parsed, err := typed.Float64()
			if err == nil {
				return parsed
			}
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
			if err == nil {
				return parsed
			}
		}
	}
	return 0
}

func rowIdentity(row map[string]any, fallbackSource string) string {
	project := strings.TrimSpace(anyToString(row["project"]))
	file := strings.TrimSpace(anyToString(row["file"]))
	if project != "" && file != "" {
		return project + "::" + file
	}
	memoryID := strings.TrimSpace(anyToString(row["memory_id"]))
	if memoryID != "" {
		return memoryID
	}
	id := strings.TrimSpace(anyToString(row["id"]))
	if id != "" {
		return id
	}
	if file != "" {
		return fallbackSource + "::" + file
	}
	summary := strings.TrimSpace(anyToString(row["summary"]))
	if summary != "" {
		sum := sha1.Sum([]byte(summary))
		return fallbackSource + "::summary::" + strconv.FormatUint(uint64(sum[0])<<8|uint64(sum[1]), 16)
	}
	encoded, _ := json.Marshal(row)
	sum := sha1.Sum(encoded)
	return fallbackSource + "::hash::" + strconv.FormatUint(uint64(sum[0])<<8|uint64(sum[1]), 16)
}

type mergeEntry struct {
	key     string
	score   float64
	row     map[string]any
	sources map[string]struct{}
}

func mergeRows(rowsBySource map[string][]map[string]any, limit int) []map[string]any {
	if limit < 1 {
		limit = 1
	}
	entries := make(map[string]*mergeEntry)
	for source, rows := range rowsBySource {
		for _, row := range rows {
			if row == nil {
				continue
			}
			key := rowIdentity(row, source)
			score := parseScore(row)
			actualSource := strings.TrimSpace(strings.ToLower(anyToString(row["source"])))
			if actualSource == "" {
				actualSource = source
			}
			if existing, ok := entries[key]; ok {
				existing.sources[actualSource] = struct{}{}
				if score > existing.score {
					existing.score = score
					replacement := cloneMap(row)
					replacement["source"] = actualSource
					existing.row = replacement
				} else {
					for field, value := range row {
						if _, present := existing.row[field]; !present || anyToString(existing.row[field]) == "" {
							existing.row[field] = value
						}
					}
				}
				continue
			}
			entry := &mergeEntry{
				key:     key,
				score:   score,
				row:     cloneMap(row),
				sources: map[string]struct{}{actualSource: {}},
			}
			entry.row["source"] = actualSource
			entries[key] = entry
		}
	}
	rows := make([]*mergeEntry, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, entry)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].score == rows[j].score {
			return rows[i].key < rows[j].key
		}
		return rows[i].score > rows[j].score
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]map[string]any, 0, len(rows))
	for _, entry := range rows {
		merged := cloneMap(entry.row)
		if _, hasScore := merged["score"]; !hasScore {
			merged["score"] = entry.score
		}
		sources := make([]string, 0, len(entry.sources))
		for source := range entry.sources {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		merged["sources"] = sources
		out = append(out, merged)
	}
	return out
}

func clipText(value string, maxChars int) string {
	trimmed := strings.TrimSpace(value)
	if maxChars <= 0 {
		return ""
	}
	if len(trimmed) <= maxChars {
		return trimmed
	}
	if maxChars <= 3 {
		return trimmed[:maxChars]
	}
	return trimmed[:maxChars-3] + "..."
}

func buildGrounding(rows []map[string]any) map[string]any {
	facts := make([]map[string]any, 0, 8)
	numericFacts := make([]map[string]any, 0, 16)
	for _, row := range rows {
		if len(facts) < 8 {
			summary := clipText(anyToString(row["summary"]), 240)
			if summary != "" {
				facts = append(facts, map[string]any{
					"text":    summary,
					"project": strings.TrimSpace(anyToString(row["project"])),
					"file":    strings.TrimSpace(anyToString(row["file"])),
					"source":  strings.TrimSpace(anyToString(row["source"])),
				})
			}
		}
		if len(numericFacts) < 16 {
			if scoreRaw, ok := row["score"]; ok {
				numericFacts = append(numericFacts, map[string]any{
					"field":   "score",
					"value":   scoreRaw,
					"project": strings.TrimSpace(anyToString(row["project"])),
					"file":    strings.TrimSpace(anyToString(row["file"])),
					"source":  strings.TrimSpace(anyToString(row["source"])),
				})
			}
		}
	}
	return map[string]any{
		"strict_numeric_copy": true,
		"facts":               facts,
		"numeric_facts":       numericFacts,
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func (s *server) resolveQdrantSyncCap(mode string) time.Duration {
	normalized := strings.TrimSpace(strings.ToLower(mode))
	if timeout, ok := s.retrieval.qdrantSyncTimeoutCapByMode[normalized]; ok && timeout > 0 {
		return timeout
	}
	return s.retrieval.qdrantSyncTimeoutCap
}

func (s *server) resolveSourceTimeout(
	source string,
	retrievalMode string,
	syncPhase bool,
	explicitSourceOverride bool,
) time.Duration {
	timeout, ok := s.retrieval.sourceTimeouts[source]
	if !ok || timeout <= 0 {
		timeout = 8 * time.Second
	}
	if syncPhase && !explicitSourceOverride && source == sourceQdrant {
		capDuration := s.resolveQdrantSyncCap(retrievalMode)
		if capDuration > 0 && timeout > capDuration {
			return capDuration
		}
	}
	return timeout
}

func (s *server) resolveContinuationTimeout(source string) time.Duration {
	if timeout, ok := s.retrieval.continuationTimeoutBySource[source]; ok && timeout > 0 {
		return timeout
	}
	if s.retrieval.continuationTimeoutDefault > 0 {
		return s.retrieval.continuationTimeoutDefault
	}
	return 45 * time.Second
}

func (s *server) shouldScheduleContinuation(source string) bool {
	if !s.retrieval.failOpenContinuationEnabled {
		return false
	}
	_, ok := s.retrieval.failOpenContinuationSources[source]
	return ok
}

func (s *server) shouldAdaptiveSkip(source string) bool {
	if !s.retrieval.timeoutAdaptiveSkipEnabled {
		return false
	}
	_, ok := s.retrieval.timeoutAdaptiveSkipSources[source]
	return ok
}

func (s *server) callBackendSourceQuery(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	explicitSourceOverride bool,
) ([]map[string]any, []string, error) {
	sourceRequest := cloneMap(baseRequest)
	sourceRequest["sources"] = []string{source}
	if s.retrieval.subcallDisableExpansion && !explicitSourceOverride {
		sourceRequest["query_expansion"] = false
	}
	if s.retrieval.subcallDisableAutoEscalate && !explicitSourceOverride {
		sourceRequest["auto_escalate"] = false
	}
	wrapper := map[string]any{"request": sourceRequest}
	payloadBytes, err := json.Marshal(wrapper)
	if err != nil {
		return nil, nil, err
	}
	requestURL := s.backendURL + "/v1/retrieval/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.copyHeaders(req.Header, incomingHeaders)
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, errors.New("backend retrieval status=" + strconv.Itoa(resp.StatusCode))
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		return nil, nil, err
	}
	rows := parseRows(payload["results"])
	for _, row := range rows {
		if strings.TrimSpace(anyToString(row["source"])) == "" {
			row["source"] = source
		}
	}
	warnings := parseWarnings(payload["warnings"])
	return rows, warnings, nil
}

func (s *server) scheduleContinuationWarm(
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	reason string,
) bool {
	select {
	case s.continuationSem <- struct{}{}:
	default:
		log.Printf("continuation warm skipped source=%s reason=%s detail=max_inflight", source, reason)
		return false
	}
	go func() {
		defer func() { <-s.continuationSem }()
		timeout := s.resolveContinuationTimeout(source)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		start := time.Now()
		_, _, err := s.callBackendSourceQuery(ctx, incomingHeaders, baseRequest, source, true)
		status := "ok"
		if err != nil {
			status = "error"
			log.Printf("continuation warm failed source=%s reason=%s error=%s", source, reason, err)
		}
		latency := time.Since(start).Milliseconds()
		s.telemetry.record(retrievalEvent{Source: source, Phase: "continuation", Status: status, LatencyMs: latency})
	}()
	return true
}

type sourceCallResult struct {
	source         string
	phase          string
	rows           []map[string]any
	warnings       []string
	err            error
	timedOut       bool
	budgetExceeded bool
	timeout        time.Duration
	latency        time.Duration
}

type sourceBatchOutput struct {
	rows                  map[string][]map[string]any
	sourceErrors          map[string]map[string]any
	warnings              []string
	timedOutSources       []string
	budgetExceededSources []string
	continuationSources   []string
	skippedSources        []string
}

func (s *server) runSourceBatch(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
	sources []string,
	retrievalMode string,
	phase string,
	explicitSourceOverride bool,
	syncPhase bool,
	suppressSlowTimeoutWarnings bool,
	adaptiveSkipped map[string]struct{},
) sourceBatchOutput {
	output := sourceBatchOutput{
		rows:         make(map[string][]map[string]any),
		sourceErrors: make(map[string]map[string]any),
		warnings:     []string{},
	}
	if len(sources) == 0 {
		return output
	}
	slowSet := toSourceSet(s.retrieval.slowSources)
	resultsCh := make(chan sourceCallResult, len(sources))
	var started int
	for _, source := range sources {
		normalized := strings.TrimSpace(strings.ToLower(source))
		if normalized == "" {
			continue
		}
		if _, skipped := adaptiveSkipped[normalized]; skipped {
			output.skippedSources = append(output.skippedSources, normalized)
			output.warnings = append(
				output.warnings,
				"Adaptive timeout policy skipped timed-out source '"+normalized+"' for phase '"+phase+"'.",
			)
			continue
		}
		started += 1
		sourceTimeout := s.resolveSourceTimeout(
			normalized,
			retrievalMode,
			syncPhase,
			explicitSourceOverride,
		)
		go func(sourceName string, timeout time.Duration) {
			start := time.Now()
			sourceCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			rows, warnings, err := s.callBackendSourceQuery(sourceCtx, incomingHeaders, baseRequest, sourceName, explicitSourceOverride)
			latency := time.Since(start)
			timedOut := false
			budgetExceeded := false
			if err != nil {
				timedOut = isTimeoutError(err) || errors.Is(sourceCtx.Err(), context.DeadlineExceeded)
				if timedOut && syncPhase && !explicitSourceOverride {
					if _, isSlow := slowSet[sourceName]; isSlow {
						budgetExceeded = true
					}
				}
			}
			status := "ok"
			if err != nil {
				if timedOut {
					if budgetExceeded {
						status = "budget_exceeded"
					} else {
						status = "timeout"
					}
				} else {
					status = "error"
				}
			}
			s.telemetry.record(retrievalEvent{
				Source:    sourceName,
				Phase:     phase,
				Status:    status,
				LatencyMs: latency.Milliseconds(),
			})
			resultsCh <- sourceCallResult{
				source:         sourceName,
				phase:          phase,
				rows:           rows,
				warnings:       warnings,
				err:            err,
				timedOut:       timedOut,
				budgetExceeded: budgetExceeded,
				timeout:        timeout,
				latency:        latency,
			}
		}(normalized, sourceTimeout)
	}

	for i := 0; i < started; i++ {
		result := <-resultsCh
		if len(result.rows) > 0 {
			output.rows[result.source] = result.rows
		}
		if len(result.warnings) > 0 {
			for _, warning := range result.warnings {
				output.warnings = append(output.warnings, result.source+": "+warning)
			}
		}
		if result.err != nil {
			errorKind := "error"
			if result.timedOut {
				if result.budgetExceeded {
					errorKind = "budget_exceeded"
				} else {
					errorKind = "timeout"
				}
			}
			errorPayload := map[string]any{
				"error":           result.err.Error(),
				"kind":            errorKind,
				"timeout":         result.timedOut && !result.budgetExceeded,
				"timed_out":       result.timedOut,
				"budget_exceeded": result.budgetExceeded,
				"phase":           result.phase,
				"timeout_secs":    result.timeout.Seconds(),
				"latency_ms":      result.latency.Milliseconds(),
			}
			output.sourceErrors[result.source] = errorPayload
			if result.timedOut {
				if result.budgetExceeded {
					output.budgetExceededSources = append(output.budgetExceededSources, result.source)
					if !suppressSlowTimeoutWarnings {
						output.warnings = append(
							output.warnings,
							result.source+" retrieval sync budget exceeded after "+strconv.FormatFloat(result.timeout.Seconds(), 'f', 1, 64)+"s",
						)
					}
				} else {
					output.timedOutSources = append(output.timedOutSources, result.source)
					output.warnings = append(
						output.warnings,
						result.source+" retrieval timed out after "+strconv.FormatFloat(result.timeout.Seconds(), 'f', 1, 64)+"s",
					)
				}
				if s.shouldScheduleContinuation(result.source) {
					if s.scheduleContinuationWarm(incomingHeaders, baseRequest, result.source, result.phase+"-timeout") {
						output.continuationSources = append(output.continuationSources, result.source)
						if !suppressSlowTimeoutWarnings || !result.budgetExceeded {
							output.warnings = append(
								output.warnings,
								result.source+" timed out; continuing asynchronously for cache warm.",
							)
						}
					}
				}
			} else {
				output.warnings = append(output.warnings, result.source+" retrieval failed: "+result.err.Error())
			}
		}
	}

	output.warnings = dedupeWarnings(output.warnings)
	output.timedOutSources = normalizeSourceList(output.timedOutSources)
	output.budgetExceededSources = normalizeSourceList(output.budgetExceededSources)
	output.continuationSources = normalizeSourceList(output.continuationSources)
	output.skippedSources = normalizeSourceList(output.skippedSources)
	return output
}

func classifySources(allSources []string, fastSet map[string]struct{}, slowSet map[string]struct{}) ([]string, []string) {
	fast := make([]string, 0, len(allSources))
	slow := make([]string, 0, len(allSources))
	for _, source := range allSources {
		if _, ok := fastSet[source]; ok {
			fast = append(fast, source)
			continue
		}
		if _, ok := slowSet[source]; ok {
			slow = append(slow, source)
			continue
		}
		fast = append(fast, source)
	}
	return normalizeSourceList(fast), normalizeSourceList(slow)
}

func lexicalTokenSet(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return tokens
	}
	parts := strings.FieldsFunc(lower, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	})
	for _, part := range parts {
		if len(part) < 3 {
			continue
		}
		tokens[part] = struct{}{}
	}
	return tokens
}

func lexicalCoverageScore(query string, rows []map[string]any) float64 {
	queryTokens := lexicalTokenSet(query)
	if len(queryTokens) == 0 || len(rows) == 0 {
		return 0
	}
	matched := make(map[string]struct{})
	for _, row := range rows {
		corpus := strings.TrimSpace(
			anyToString(row["summary"]) + " " +
				anyToString(row["file"]) + " " +
				anyToString(row["topic_path"]),
		)
		if corpus == "" {
			continue
		}
		rowTokens := lexicalTokenSet(corpus)
		if len(rowTokens) == 0 {
			continue
		}
		for token := range queryTokens {
			if _, ok := rowTokens[token]; ok {
				matched[token] = struct{}{}
			}
		}
	}
	return float64(len(matched)) / float64(len(queryTokens))
}

func (s *server) executeRetrieval(
	ctx context.Context,
	incomingHeaders http.Header,
	payload map[string]any,
	includeGrounding bool,
) (map[string]any, int, error) {
	requestPayload := payload
	if wrapped, ok := payload["request"].(map[string]any); ok {
		requestPayload = wrapped
	}
	requestPayload = cloneMap(requestPayload)
	query := strings.TrimSpace(anyToString(requestPayload["query"]))
	if query == "" {
		return map[string]any{"error": "query is required"}, http.StatusUnprocessableEntity, nil
	}
	limit := clampInt(anyToInt(requestPayload["limit"], 10), 1, 100)
	retrievalMode := strings.TrimSpace(strings.ToLower(anyToString(requestPayload["retrieval_mode"])))
	if retrievalMode == "" {
		retrievalMode = "balanced"
	}
	retrievalIntent := strings.TrimSpace(strings.ToLower(anyToString(requestPayload["retrieval_intent"])))
	if retrievalIntent == "" {
		retrievalIntent = "decision"
	}
	rustBackendPolicy := resolveRustBackendPolicy(requestPayload["backend_policy"])
	requestPayload["backend_policy"] = rustBackendPolicy
	explicitSources := anyToStringSlice(requestPayload["sources"])
	explicitSourceOverride := len(explicitSources) > 0
	resolvedSources := explicitSources
	if len(resolvedSources) == 0 {
		resolvedSources = append([]string(nil), s.retrieval.defaultSources...)
	}
	if len(resolvedSources) == 0 {
		resolvedSources = append([]string{sourceQdrant}, resolvedSources...)
	}

	fastSet := toSourceSet(s.retrieval.fastSources)
	slowSet := toSourceSet(s.retrieval.slowSources)
	fastSources, slowSources := classifySources(resolvedSources, fastSet, slowSet)

	warnings := []string{}
	sourceErrors := map[string]map[string]any{}
	sourceRows := map[string][]map[string]any{}
	timedOutObserved := map[string]struct{}{}
	budgetExceededObserved := map[string]struct{}{}
	adaptiveSkipped := map[string]struct{}{}
	continuationSources := []string{}
	asyncWarmSlowSources := []string{}
	syncFallbackSlowSources := []string{}

	fastBatch := s.runSourceBatch(
		ctx,
		incomingHeaders,
		requestPayload,
		fastSources,
		retrievalMode,
		"fast",
		explicitSourceOverride,
		true,
		false,
		adaptiveSkipped,
	)
	for source, rows := range fastBatch.rows {
		sourceRows[source] = rows
	}
	for source, payload := range fastBatch.sourceErrors {
		sourceErrors[source] = payload
	}
	warnings = append(warnings, fastBatch.warnings...)
	for _, source := range fastBatch.timedOutSources {
		timedOutObserved[source] = struct{}{}
		if s.shouldAdaptiveSkip(source) && !explicitSourceOverride {
			adaptiveSkipped[source] = struct{}{}
		}
	}
	continuationSources = append(continuationSources, fastBatch.continuationSources...)
	for _, source := range fastBatch.budgetExceededSources {
		budgetExceededObserved[source] = struct{}{}
	}

	merged := mergeRows(sourceRows, limit)
	lexicalBackend := strings.TrimSpace(strings.ToLower(anyToString(rustBackendPolicy["lexical_backend"])))
	lexicalGuardEligible := s.retrieval.lexicalGuardEnabled && !explicitSourceOverride && lexicalBackend == "tantivy_lexical"
	lexicalGuardCoverage := 0.0
	lexicalGuardApplied := false
	if lexicalGuardEligible {
		lexicalGuardCoverage = lexicalCoverageScore(query, merged)
	}
	fastPathFailed := len(merged) == 0 || len(fastBatch.sourceErrors) > 0
	skipSlow := !explicitSourceOverride && (retrievalMode != "deep" || !s.retrieval.deepBlocking)
	if len(slowSources) > 0 {
		if skipSlow {
			needsFallback := len(merged) < s.retrieval.minFastResults
			if needsFallback &&
				lexicalGuardEligible &&
				len(merged) >= s.retrieval.lexicalGuardMinResults &&
				lexicalGuardCoverage >= s.retrieval.lexicalGuardMinCoverage {
				needsFallback = false
				lexicalGuardApplied = true
				warnings = append(
					warnings,
					"Lexical backend policy deferred sync slow-source fallback; continuing asynchronously for cache warm.",
				)
			}
			if needsFallback {
				fallback := []string{}
				if len(s.retrieval.syncFallbackSources) > 0 {
					fallbackSet := toSourceSet(s.retrieval.syncFallbackSources)
					for _, source := range slowSources {
						if _, ok := fallbackSet[source]; ok {
							fallback = append(fallback, source)
						}
					}
				} else {
					fallback = append(fallback, slowSources...)
				}
				if len(fallback) == 0 {
					fallback = append(fallback, slowSources...)
				}
				if s.retrieval.timeoutAdaptiveSkipEnabled && !explicitSourceOverride {
					filtered := make([]string, 0, len(fallback))
					for _, source := range fallback {
						if _, skip := adaptiveSkipped[source]; skip {
							continue
						}
						filtered = append(filtered, source)
					}
					fallback = filtered
				}
				syncFallbackSlowSources = append(syncFallbackSlowSources, fallback...)
				slowBatch := s.runSourceBatch(
					ctx,
					incomingHeaders,
					requestPayload,
					fallback,
					retrievalMode,
					"slow-sync-fallback",
					explicitSourceOverride,
					true,
					!fastPathFailed,
					adaptiveSkipped,
				)
				for source, rows := range slowBatch.rows {
					sourceRows[source] = rows
				}
				for source, payload := range slowBatch.sourceErrors {
					sourceErrors[source] = payload
				}
				warnings = append(warnings, slowBatch.warnings...)
				for _, source := range slowBatch.timedOutSources {
					timedOutObserved[source] = struct{}{}
					if s.shouldAdaptiveSkip(source) && !explicitSourceOverride {
						adaptiveSkipped[source] = struct{}{}
					}
				}
				continuationSources = append(continuationSources, slowBatch.continuationSources...)
				for _, source := range slowBatch.budgetExceededSources {
					budgetExceededObserved[source] = struct{}{}
				}
				fallbackSet := toSourceSet(fallback)
				for _, source := range slowSources {
					if _, used := fallbackSet[source]; used {
						continue
					}
					asyncWarmSlowSources = append(asyncWarmSlowSources, source)
				}
			} else {
				asyncWarmSlowSources = append(asyncWarmSlowSources, slowSources...)
			}
		} else {
			slowBatch := s.runSourceBatch(
				ctx,
				incomingHeaders,
				requestPayload,
				slowSources,
				retrievalMode,
				"slow-sync",
				explicitSourceOverride,
				true,
				!fastPathFailed,
				adaptiveSkipped,
			)
			for source, rows := range slowBatch.rows {
				sourceRows[source] = rows
			}
			for source, payload := range slowBatch.sourceErrors {
				sourceErrors[source] = payload
			}
			warnings = append(warnings, slowBatch.warnings...)
			for _, source := range slowBatch.timedOutSources {
				timedOutObserved[source] = struct{}{}
				if s.shouldAdaptiveSkip(source) && !explicitSourceOverride {
					adaptiveSkipped[source] = struct{}{}
				}
			}
			continuationSources = append(continuationSources, slowBatch.continuationSources...)
			for _, source := range slowBatch.budgetExceededSources {
				budgetExceededObserved[source] = struct{}{}
			}
		}
	}

	asyncWarmSlowSources = normalizeSourceList(asyncWarmSlowSources)
	for _, source := range asyncWarmSlowSources {
		if s.scheduleContinuationWarm(incomingHeaders, requestPayload, source, "slow-async-warm") {
			continuationSources = append(continuationSources, source)
		}
	}
	continuationSources = normalizeSourceList(continuationSources)
	warnings = dedupeWarnings(warnings)

	if len(asyncWarmSlowSources) > 0 {
		warnings = append(warnings, "Staged fetch deferred slow sources: "+strings.Join(asyncWarmSlowSources, ", ")+".")
	}
	if len(adaptiveSkipped) > 0 {
		skipped := make([]string, 0, len(adaptiveSkipped))
		for source := range adaptiveSkipped {
			skipped = append(skipped, source)
		}
		sort.Strings(skipped)
		warnings = append(warnings, "Adaptive timeout policy skipped timed-out sources for remaining recall hops: "+strings.Join(skipped, ", ")+".")
	}

	merged = mergeRows(sourceRows, limit)
	returnedSources := make([]string, 0, len(sourceRows))
	for source, rows := range sourceRows {
		if len(rows) == 0 {
			continue
		}
		returnedSources = append(returnedSources, source)
	}
	sort.Strings(returnedSources)
	deferredCandidatesSet := map[string]struct{}{}
	for _, source := range asyncWarmSlowSources {
		deferredCandidatesSet[source] = struct{}{}
	}
	for _, source := range continuationSources {
		deferredCandidatesSet[source] = struct{}{}
	}
	for source := range budgetExceededObserved {
		deferredCandidatesSet[source] = struct{}{}
	}
	deferredCandidates := make([]string, 0, len(deferredCandidatesSet))
	for source := range deferredCandidatesSet {
		deferredCandidates = append(deferredCandidates, source)
	}
	sort.Strings(deferredCandidates)
	hasMaterialSourceErrors := false
	for _, payload := range sourceErrors {
		kind := strings.TrimSpace(strings.ToLower(anyToString(payload["kind"])))
		if kind == "" {
			kind = "error"
		}
		if kind == "timeout" || kind == "error" {
			hasMaterialSourceErrors = true
			break
		}
	}
	if len(returnedSources) > 0 && (len(deferredCandidates) > 0 || hasMaterialSourceErrors) {
		warnings = append(
			warnings,
			"Sources returned now: "+strings.Join(returnedSources, ", ")+".",
		)
	}
	if len(deferredCandidates) > 0 {
		warnings = append(
			warnings,
			"Additional context may be available later from: "+strings.Join(deferredCandidates, ", ")+". Re-run after cache warm or use deep mode / longer timeout budgets for blocking retrieval.",
		)
	}

	sourceCounts := make(map[string]int, len(sourceRows))
	for source, rows := range sourceRows {
		sourceCounts[source] = len(rows)
	}
	timedOutList := make([]string, 0, len(timedOutObserved))
	for source := range timedOutObserved {
		timedOutList = append(timedOutList, source)
	}
	sort.Strings(timedOutList)
	skippedList := make([]string, 0, len(adaptiveSkipped))
	for source := range adaptiveSkipped {
		skippedList = append(skippedList, source)
	}
	sort.Strings(skippedList)
	budgetExceededList := make([]string, 0, len(budgetExceededObserved))
	for source := range budgetExceededObserved {
		budgetExceededList = append(budgetExceededList, source)
	}
	sort.Strings(budgetExceededList)
	debug := map[string]any{
		"retrieval_mode":   retrievalMode,
		"retrieval_intent": retrievalIntent,
		"sources":          resolvedSources,
		"source_counts":    sourceCounts,
		"source_errors":    sourceErrors,
		"source_policy": map[string]any{
			"staged_enabled":               s.retrieval.enabled,
			"fast_sources":                 s.retrieval.fastSources,
			"slow_sources":                 s.retrieval.slowSources,
			"sync_fallback_sources":        s.retrieval.syncFallbackSources,
			"min_fast_results":             s.retrieval.minFastResults,
			"deep_blocking":                s.retrieval.deepBlocking,
			"qdrant_sync_timeout_cap_secs": s.retrieval.qdrantSyncTimeoutCap.Seconds(),
			"qdrant_sync_timeout_cap_by_mode_secs": map[string]any{
				"fast":     s.resolveQdrantSyncCap("fast").Seconds(),
				"balanced": s.resolveQdrantSyncCap("balanced").Seconds(),
				"deep":     s.resolveQdrantSyncCap("deep").Seconds(),
			},
			"fail_open_timeout_continuation_enabled": s.retrieval.failOpenContinuationEnabled,
			"timeout_adaptive_skip_enabled":          s.retrieval.timeoutAdaptiveSkipEnabled,
			"lexical_guard_enabled":                  s.retrieval.lexicalGuardEnabled,
			"lexical_guard_min_coverage":             s.retrieval.lexicalGuardMinCoverage,
			"lexical_guard_min_results":              s.retrieval.lexicalGuardMinResults,
			"runtime_backend_policy":                 rustBackendPolicy,
		},
		"staged_fetch": map[string]any{
			"enabled":                          true,
			"used":                             true,
			"fast_sources":                     fastSources,
			"slow_sources":                     slowSources,
			"sync_fallback_slow_sources":       normalizeSourceList(syncFallbackSlowSources),
			"async_warm_slow_sources":          asyncWarmSlowSources,
			"fail_open_continuation_sources":   continuationSources,
			"timeout_adaptive_skipped_sources": skippedList,
			"timed_out_sources":                timedOutList,
			"budget_exceeded_sources":          budgetExceededList,
			"lexical_backend":                  lexicalBackend,
			"lexical_guard_applied":            lexicalGuardApplied,
			"lexical_guard_coverage":           lexicalGuardCoverage,
		},
	}

	response := map[string]any{
		"results":         merged,
		"retrieval_debug": debug,
		"warnings":        dedupeWarnings(warnings),
	}
	if includeGrounding {
		response["grounding"] = buildGrounding(merged)
	}
	return response, http.StatusOK, nil
}

func (s *server) retrievalQuery(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		s.proxy(w, r)
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	response, status, execErr := s.executeRetrieval(r.Context(), r.Header, payload, false)
	if execErr != nil {
		log.Printf("staged retrieval query failed; falling back to backend proxy: %s", execErr)
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	writeJSON(w, status, response)
}

func (s *server) retrievalQueryWithGrounding(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		s.proxy(w, r)
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	response, status, execErr := s.executeRetrieval(r.Context(), r.Header, payload, true)
	if execErr != nil {
		log.Printf("staged retrieval query-with-grounding failed; falling back to backend proxy: %s", execErr)
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	writeJSON(w, status, response)
}

func (s *server) retrievalBatchQuery(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		s.proxy(w, r)
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	requestsRaw, ok := payload["requests"].([]any)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "requests must be a list"})
		return
	}
	results := make([]map[string]any, 0, len(requestsRaw))
	for _, row := range requestsRaw {
		requestPayload, ok := row.(map[string]any)
		if !ok {
			results = append(results, map[string]any{
				"results":  []any{},
				"warnings": []string{"skipped invalid request payload"},
			})
			continue
		}
		response, _, execErr := s.executeRetrieval(
			r.Context(),
			r.Header,
			map[string]any{"request": requestPayload},
			false,
		)
		if execErr != nil {
			results = append(results, map[string]any{
				"results":  []any{},
				"warnings": []string{"retrieval failed: " + execErr.Error()},
			})
			continue
		}
		results = append(results, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *server) retrievalHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"impl":          "go-staged-retrieval",
		"service":       "gateway-go",
		"backendUrl":    s.backendURL,
		"backendHealth": s.backendHealthy(ctx),
		"stagedEnabled": s.retrieval.enabled,
		"fastSources":   s.retrieval.fastSources,
		"slowSources":   s.retrieval.slowSources,
	})
}

func buildMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/info", s.info)
	// Retrieval + memory engine API (go-first ingress, python fallback backend).
	mux.HandleFunc("/v1/retrieval/query", s.retrievalQuery)
	mux.HandleFunc("/v1/retrieval/query-with-grounding", s.retrievalQueryWithGrounding)
	mux.HandleFunc("/v1/retrieval/batch-query", s.retrievalBatchQuery)
	mux.HandleFunc("/v1/retrieval/health", s.retrievalHealth)
	mux.HandleFunc("/memory/search", s.proxy)
	mux.HandleFunc("/memory/search/async/", s.proxy)
	mux.HandleFunc("/memory/search/jobs/", s.proxy)
	mux.HandleFunc("/memory/recall/eval-cases", s.proxy)
	mux.HandleFunc("/memory/recall/eval-cases/refresh", s.proxy)
	mux.HandleFunc("/memory/recall/evaluate/saved", s.proxy)
	mux.HandleFunc("/memory/write/batch", s.proxy)
	mux.HandleFunc("/memory/browser-context", s.proxy)
	mux.HandleFunc("/memory/context-pack", s.proxy)
	mux.HandleFunc("/ops/queue/status", s.proxy)
	mux.HandleFunc("/ops/capabilities", s.proxy)
	mux.HandleFunc("/tools/capability_map", s.proxy)
	mux.HandleFunc("/tools/ops_queue_status", s.proxy)
	mux.HandleFunc("/tools/memory_write_batch", s.proxy)
	mux.HandleFunc("/v1/memory/put", s.proxy)
	mux.HandleFunc("/v1/memory/update", s.proxy)
	mux.HandleFunc("/v1/memory/get", s.proxy)
	mux.HandleFunc("/v1/memory/neighbors", s.proxy)
	mux.HandleFunc("/v1/memory/batch-put", s.proxy)
	mux.HandleFunc("/migration/runtime", s.proxy)
	return mux
}

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8091"
	}
	srv := newServer()
	mux := buildMux(srv)
	log.Printf("gateway-go listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
