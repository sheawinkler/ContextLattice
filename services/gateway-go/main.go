package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	sourceQdrant      = "qdrant"
	sourceWeaviate    = "weaviate"
	sourcePgvector    = "postgres_pgvector"
	sourceMongoRaw    = "mongo_raw"
	sourceMindsdb     = "mindsdb"
	sourceTopicRollup = "topic_rollups"
	sourceLetta       = "letta"
	sourceMemoryBank  = "memory_bank"

	sourceOwnerGoNative              = "go_native"
	sourceOwnerRustNative            = "rust_native"
	sourceOwnerPythonBackendFallback = "python_backend_fallback"
)

var defaultAllSources = []string{
	sourceQdrant,
	sourceWeaviate,
	sourcePgvector,
	sourceMongoRaw,
	sourceMindsdb,
	sourceTopicRollup,
	sourceLetta,
	sourceMemoryBank,
}

type retrievalPolicy struct {
	enabled                           bool
	defaultSources                    []string
	fastSources                       []string
	slowSources                       []string
	nonDegradableSources              map[string]struct{}
	protectedSources                  map[string]struct{}
	syncFallbackSources               []string
	rustQualityFallbackEnabled        bool
	rustQualityFallbackSources        []string
	rustQualityFallbackMode           string
	minFastResults                    int
	minFastResultsByMode              map[string]int
	disableSyncSlowFallback           bool
	slowSyncTimeoutCap                time.Duration
	rustLanePromotionEnabled          bool
	topicPrefilterEnabled             bool
	coverageRescueEnabled             bool
	coverageRescueMinTokens           int
	lexicalGuardEnabled               bool
	lexicalGuardMinCoverage           float64
	lexicalGuardMinResults            int
	deepBlocking                      bool
	qdrantSyncTimeoutCap              time.Duration
	qdrantSyncTimeoutCapByMode        map[string]time.Duration
	lettaTopKFactor                   float64
	lettaTopKCap                      int
	lettaTopKFactorByMode             map[string]float64
	lettaTopKCapByMode                map[string]int
	failOpenContinuationEnabled       bool
	failOpenContinuationSources       map[string]struct{}
	timeoutAdaptiveSkipEnabled        bool
	timeoutAdaptiveSkipSources        map[string]struct{}
	sourceTimeouts                    map[string]time.Duration
	topicRollupSyncTimeoutFloor       time.Duration
	topicRollupSyncTimeoutFloorByMode map[string]time.Duration
	topicRollupSearchTopN             int
	continuationTimeoutDefault        time.Duration
	continuationTimeoutBySource       map[string]time.Duration
	continuationMaxInflight           int
	continuationMaxInflightPerSource  int
	continuationMaxInflightOverrides  map[string]int
	continuationSourceCooldown        time.Duration
	continuationSourceCooldownBySrc   map[string]time.Duration
	continuationSheddingEnabled       bool
	continuationSheddingQueueRatio    float64
	continuationSheddingPendingHigh   int
	continuationSheddingSources       map[string]struct{}
	syncSourceConcurrencyDefault      int
	syncSourceConcurrencyOverrides    map[string]int
	syncQueueAgeWarnSecs              float64
	syncQueueAgeHighSecs              float64
	timeoutContractGrace              time.Duration
	subcallDisableExpansion           bool
	subcallDisableAutoEscalate        bool
	telemetryBatchEnabled             bool
	telemetryBatchFlushInterval       time.Duration
	telemetryBatchSize                int
	telemetryBatchDropLogEvery        uint64
	adaptiveTimeoutEnabled            bool
	adaptiveTimeoutMinRequests        int
	adaptiveTimeoutWindow             int
	adaptiveTimeoutP95Factor          float64
	adaptiveTimeoutMinScale           float64
	adaptiveTimeoutMaxScale           float64
	adaptiveTimeoutBacklogWeight      float64
	adaptiveTimeoutBacklogCap         int
	continuationEventHistory          int
	continuationEventTTL              time.Duration
	continuationSSEHeartbeat          time.Duration
	continuationDurableEnabled        bool
	continuationDurableDir            string
	continuationDurableMaxPending     int
	continuationDurableDrainBatch     int
	continuationDurablePollInterval   time.Duration
	continuationDurableRetryBase      time.Duration
	continuationDurableRetryMax       time.Duration
	continuationDurableMaxAttempts    int
	sourceOwnershipMode               string
	sourceOwnershipStrictFastAllowPy  map[string]struct{}
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

type toolCallPolicy struct {
	allowAll           bool
	requireAPIKey      bool
	enforceProvidedKey bool
	allowlist          map[string]struct{}
	denylist           map[string]struct{}
	roleSplitEnabled   bool
	roleSplitAuto      bool
	workerKey          string
	workerRole         toolRolePolicy
	orchestratorRole   toolRolePolicy
}

type toolRolePolicy struct {
	allowAll  bool
	allowlist map[string]struct{}
	denylist  map[string]struct{}
}

type adaptiveSourceStats struct {
	latencyMs      []float64
	requests       int
	timeouts       int
	errors         int
	budgetExceeded int
}

type lettaConfig struct {
	url            string
	apiKey         string
	requireAPIKey  bool
	autoSessionID  string
	agentModel     string
	agentEmbedding string
	requestTimeout time.Duration
	verifyInterval time.Duration
}

var queryTermPattern = regexp.MustCompile(`[A-Za-z0-9_:/.-]{3,}`)
var lettaHeaderPattern = regexp.MustCompile(`\b([a-zA-Z_]+)=([^\s]+)`)

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
	backendURL                      string
	orchestratorAPIKey              string
	strictNoPythonRuntime           bool
	client                          *http.Client
	retrieval                       retrievalPolicy
	letta                           lettaConfig
	toolCalls                       toolCallPolicy
	pythonHotPathMode               string
	pythonHotPathFallbacks          atomic.Uint64
	pythonHotPathMu                 sync.Mutex
	pythonHotPathByPath             map[string]uint64
	pythonHotPathByReason           map[string]uint64
	pythonHotPathLastAt             string
	telemetry                       *retrievalTelemetry
	writePolicy                     writeIngressPolicy
	memoryStore                     *memoryStore
	memoryProfilesStore             *memoryProfileStore
	telemetrySink                   *telemetrySink
	telemetrySpool                  *telemetrySpool
	telemetryRing                   *telemetryRing
	telemetryMetricsMu              sync.Mutex
	telemetryMetricsState           map[string]any
	tradingMu                       sync.Mutex
	tradingState                    map[string]any
	tradingHistory                  []map[string]any
	tradingHistoryPath              string
	tradingHistoryLimit             int
	continuationSem                 chan struct{}
	syncSourceSem                   map[string]chan struct{}
	syncQueueMu                     sync.Mutex
	syncSourcePending               map[string][]time.Time
	syncSourceInFlight              map[string]int
	syncSourceRetrying              map[string]int
	adaptiveMu                      sync.Mutex
	adaptiveBySource                map[string]*adaptiveSourceStats
	continuationMu                  sync.Mutex
	continuationInFlight            map[string]int
	continuationInFlightStarted     map[string][]time.Time
	continuationRetrying            map[string]int
	continuationSourceCooldownUntil map[string]time.Time
	continuationSubscribers         map[string][]chan map[string]any
	continuationHistory             map[string][]map[string]any
	continuationExpiry              map[string]time.Time
	continuationDurable             *continuationDurableQueue
	timeoutContractViolations       atomic.Uint64
	timeoutContractMu               sync.Mutex
	timeoutContractBySource         map[string]uint64
	timeoutContractLast             map[string]any
	driftMu                         sync.Mutex
	driftByClass                    map[string]uint64
	driftBySource                   map[string]uint64
	driftLast                       map[string]any
	lettaAgentMu                    sync.Mutex
	lettaAgentBySession             map[string]string
	lettaAgentVerifiedAt            map[string]time.Time
}

func normalizeHotPath(path string) string {
	normalized := "/" + strings.TrimSpace(strings.TrimPrefix(path, "/"))
	if normalized == "/" {
		return "unknown"
	}
	return normalized
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

func normalizeToolPath(raw string) string {
	token := strings.TrimSpace(strings.ToLower(raw))
	if token == "" {
		return ""
	}
	token = strings.TrimPrefix(token, "/")
	if strings.HasPrefix(token, "tools/") {
		return "/" + token
	}
	if strings.Contains(token, "/") {
		return "/" + token
	}
	return "/tools/" + token
}

func parseToolPathSet(raw string) map[string]struct{} {
	res := map[string]struct{}{}
	for _, part := range strings.Split(strings.TrimSpace(raw), ",") {
		normalized := normalizeToolPath(part)
		if normalized == "" {
			continue
		}
		res[normalized] = struct{}{}
	}
	return res
}

func parseToolPathSetWithDefault(raw string, fallback string) map[string]struct{} {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = strings.TrimSpace(fallback)
	}
	return parseToolPathSet(trimmed)
}

func parseNormalizedSet(raw string, fallback string) map[string]struct{} {
	res := map[string]struct{}{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = strings.TrimSpace(fallback)
	}
	for _, part := range strings.Split(trimmed, ",") {
		normalized := strings.TrimSpace(strings.ToLower(part))
		if normalized == "" {
			continue
		}
		res[normalized] = struct{}{}
	}
	return res
}

func loadToolCallPolicy(orchestratorAPIKey string) toolCallPolicy {
	workerKey := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_WORKER_API_KEY"))
	if workerKey == "" {
		workerKey = strings.TrimSpace(os.Getenv("CONTEXTLATTICE_WORKER_API_KEY"))
	}
	orchestratorKey := strings.TrimSpace(orchestratorAPIKey)
	roleSplitAuto := envBool("GO_TOOL_CALLS_ROLE_SPLIT_AUTO", true)
	roleSplitEnabled := envBool("GO_TOOL_CALLS_ROLE_SPLIT_ENABLED", false)
	if roleSplitAuto && orchestratorKey != "" && workerKey != "" && !secureTokenEqual(workerKey, orchestratorKey) {
		roleSplitEnabled = true
	}
	if roleSplitAuto && orchestratorKey != "" && workerKey != "" && secureTokenEqual(workerKey, orchestratorKey) {
		log.Printf("gateway-go: GO tool role split auto skipped (worker key equals orchestrator key)")
	}
	if roleSplitEnabled && workerKey == "" {
		log.Printf("gateway-go: GO tool role split disabled (worker key missing)")
		roleSplitEnabled = false
	}
	if roleSplitEnabled && secureTokenEqual(workerKey, orchestratorKey) {
		log.Printf("gateway-go: GO tool role split disabled (worker key equals orchestrator key)")
		roleSplitEnabled = false
	}
	return toolCallPolicy{
		allowAll:           envBool("GO_TOOL_CALLS_ALLOW_ALL", true),
		requireAPIKey:      envBool("GO_TOOL_CALLS_REQUIRE_API_KEY", false),
		enforceProvidedKey: envBool("GO_TOOL_CALLS_ENFORCE_PROVIDED_KEY", false),
		allowlist:          parseToolPathSet(os.Getenv("GO_TOOL_CALLS_ALLOWLIST")),
		denylist:           parseToolPathSet(os.Getenv("GO_TOOL_CALLS_DENYLIST")),
		roleSplitEnabled:   roleSplitEnabled,
		roleSplitAuto:      roleSplitAuto,
		workerKey:          workerKey,
		orchestratorRole: toolRolePolicy{
			allowAll:  envBool("GO_TOOL_CALLS_ORCHESTRATOR_ALLOW_ALL", true),
			allowlist: parseToolPathSet(os.Getenv("GO_TOOL_CALLS_ORCHESTRATOR_ALLOWLIST")),
			denylist:  parseToolPathSet(os.Getenv("GO_TOOL_CALLS_ORCHESTRATOR_DENYLIST")),
		},
		workerRole: toolRolePolicy{
			allowAll: envBool("GO_TOOL_CALLS_WORKER_ALLOW_ALL", false),
			allowlist: parseToolPathSetWithDefault(
				os.Getenv("GO_TOOL_CALLS_WORKER_ALLOWLIST"),
				"capability_map,ops_queue_status",
			),
			denylist: parseToolPathSetWithDefault(
				os.Getenv("GO_TOOL_CALLS_WORKER_DENYLIST"),
				"memory_write_batch,feedback_submit",
			),
		},
	}
}

func intMapEnv(name string, fallback map[string]int) map[string]int {
	resolved := map[string]int{}
	for key, value := range fallback {
		resolved[strings.TrimSpace(strings.ToLower(key))] = value
	}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return resolved
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return resolved
	}
	for key, value := range parsed {
		normalized := strings.TrimSpace(strings.ToLower(key))
		if normalized == "" {
			continue
		}
		candidate := anyToInt(value, resolved[normalized])
		if candidate < 1 {
			candidate = 1
		}
		resolved[normalized] = candidate
	}
	return resolved
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
		normalized := strings.TrimSpace(strings.ToLower(source))
		if normalized == "" {
			continue
		}
		set[normalized] = struct{}{}
	}
	return set
}

func sourceOwnerForSource(source string) string {
	normalized := strings.TrimSpace(strings.ToLower(source))
	switch normalized {
	case sourceQdrant, sourceWeaviate, sourcePgvector, sourceMongoRaw, sourceMindsdb, sourceTopicRollup, sourceLetta, sourceMemoryBank:
		return sourceOwnerGoNative
	default:
		return sourceOwnerPythonBackendFallback
	}
}

func sourceOwnerCounts(sourceOwners map[string]string) map[string]int {
	counts := map[string]int{}
	for _, owner := range sourceOwners {
		normalized := strings.TrimSpace(strings.ToLower(owner))
		if normalized == "" {
			normalized = "unknown"
		}
		counts[normalized] = counts[normalized] + 1
	}
	return counts
}

func sourceOwnerClass(sourceOwners map[string]string) string {
	if len(sourceOwners) == 0 {
		return "unknown"
	}
	seen := map[string]struct{}{}
	last := ""
	for _, owner := range sourceOwners {
		normalized := strings.TrimSpace(strings.ToLower(owner))
		if normalized == "" {
			normalized = "unknown"
		}
		seen[normalized] = struct{}{}
		last = normalized
	}
	if len(seen) == 1 {
		return last
	}
	return "mixed"
}

func percentileFloat(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	if pct <= 0 {
		return values[0]
	}
	if pct >= 1 {
		return values[len(values)-1]
	}
	index := pct * float64(len(values)-1)
	low := int(index)
	high := low + 1
	if high >= len(values) {
		high = len(values) - 1
	}
	weight := index - float64(low)
	return values[low]*(1.0-weight) + values[high]*weight
}

func roundFloat(value float64, places int) float64 {
	if places < 0 {
		return value
	}
	pow := 1.0
	for i := 0; i < places; i++ {
		pow *= 10.0
	}
	return float64(int64(value*pow+0.5)) / pow
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
	memoryBankAllowed := map[string]struct{}{
		"native":            {},
		"disabled":          {},
		"meilisearch_spike": {},
		"quickwit_spike":    {},
		"tantivy_spike":     {},
		"lancedb_spike":     {},
		"trieve_spike":      {},
		"helixdb_spike":     {},
		"icm_spike":         {},
		"shodh_spike":       {},
		"memvid_spike":      {},
		"surrealdb_spike":   {},
	}
	return map[string]any{
		"vector_backend": normalizeRustBackendChoice(
			os.Getenv("ORCH_RUST_RETRIEVAL_VECTOR_BACKEND"),
			vectorAllowed,
			"qdrant_remote",
		),
		"lexical_backend": normalizeRustBackendChoice(
			os.Getenv("ORCH_RUST_RETRIEVAL_LEXICAL_BACKEND"),
			lexicalAllowed,
			"tantivy_lexical",
		),
		"strict": envBool("ORCH_RUST_RETRIEVAL_BACKEND_STRICT", false),
		"memory_bank_backend": normalizeRustBackendChoice(
			os.Getenv("ORCH_MEMORY_BANK_SEARCH_BACKEND"),
			memoryBankAllowed,
			"shodh_spike",
		),
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
	if value, ok := policy["memory_bank_backend"]; ok {
		resolved["memory_bank_backend"] = normalizeRustBackendChoice(
			anyToString(value),
			map[string]struct{}{
				"native":            {},
				"disabled":          {},
				"meilisearch_spike": {},
				"quickwit_spike":    {},
				"tantivy_spike":     {},
				"lancedb_spike":     {},
				"trieve_spike":      {},
				"helixdb_spike":     {},
				"icm_spike":         {},
				"shodh_spike":       {},
				"memvid_spike":      {},
				"surrealdb_spike":   {},
			},
			anyToString(resolved["memory_bank_backend"]),
		)
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
	policy.fastSources = csvListEnv("ORCH_RETRIEVAL_FAST_SOURCES", "topic_rollups,qdrant,weaviate,postgres_pgvector")
	policy.slowSources = csvListEnv("ORCH_RETRIEVAL_SLOW_SOURCES", "mindsdb,mongo_raw,letta,memory_bank")
	policy.nonDegradableSources = toSourceSet(csvListEnv("GO_RETRIEVAL_NON_DEGRADABLE_SOURCES", "topic_rollups"))
	if len(policy.nonDegradableSources) == 0 {
		policy.nonDegradableSources = map[string]struct{}{sourceTopicRollup: {}}
	}
	policy.protectedSources = toSourceSet(csvListEnv("GO_RETRIEVAL_PROTECTED_SOURCES", "topic_rollups,qdrant,weaviate,postgres_pgvector"))
	if len(policy.protectedSources) == 0 {
		policy.protectedSources = toSourceSet(policy.fastSources)
	}
	policy.syncFallbackSources = csvListEnv("ORCH_RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", "mindsdb,mongo_raw")
	policy.rustQualityFallbackEnabled = envBool("GO_RETRIEVAL_RUST_QUALITY_FALLBACK_ENABLED", true)
	policy.rustQualityFallbackSources = csvListEnv(
		"GO_RETRIEVAL_RUST_QUALITY_FALLBACK_SOURCES",
		"qdrant,topic_rollups",
	)
	policy.rustQualityFallbackMode = strings.TrimSpace(strings.ToLower(os.Getenv("GO_RETRIEVAL_RUST_QUALITY_FALLBACK_MODE")))
	if policy.rustQualityFallbackMode == "" {
		policy.rustQualityFallbackMode = "balanced"
	}
	if policy.rustQualityFallbackMode != "fast" && policy.rustQualityFallbackMode != "balanced" && policy.rustQualityFallbackMode != "deep" {
		policy.rustQualityFallbackMode = "balanced"
	}
	policy.minFastResults = envInt("ORCH_RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", 2)
	if policy.minFastResults < 1 {
		policy.minFastResults = 1
	}
	policy.minFastResultsByMode = intMapEnv(
		"ORCH_RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS_BY_MODE",
		map[string]int{
			"fast":     1,
			"balanced": policy.minFastResults,
			"deep":     policy.minFastResults + 1,
		},
	)
	policy.disableSyncSlowFallback = envBool("GO_RETRIEVAL_DISABLE_SYNC_SLOW_FALLBACK", false)
	policy.slowSyncTimeoutCap = envDurationSeconds("GO_RETRIEVAL_SLOW_SYNC_TIMEOUT_CAP_SECS", 2.5)
	policy.rustLanePromotionEnabled = envBool("GO_RETRIEVAL_RUST_LANE_PROMOTION_ENABLED", false)
	policy.topicPrefilterEnabled = envBool("GO_RETRIEVAL_TOPIC_PREFILTER_ENABLED", true)
	policy.coverageRescueEnabled = envBool("GO_RETRIEVAL_COVERAGE_RESCUE_ENABLED", true)
	policy.coverageRescueMinTokens = envInt("GO_RETRIEVAL_COVERAGE_RESCUE_MIN_TOKENS", 2)
	if policy.coverageRescueMinTokens < 1 {
		policy.coverageRescueMinTokens = 1
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
	policy.lettaTopKFactor = envFloat("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR", 2.0)
	if policy.lettaTopKFactor < 1.0 {
		policy.lettaTopKFactor = 1.0
	}
	policy.lettaTopKCap = envInt("ORCH_RETRIEVAL_LETTA_TOP_K_CAP", 24)
	if policy.lettaTopKCap < 1 {
		policy.lettaTopKCap = 1
	}
	policy.lettaTopKFactorByMode = map[string]float64{
		"fast":     envFloat("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR_FAST", 1.25),
		"balanced": envFloat("ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR_BALANCED", 1.5),
		"deep": envFloat(
			"ORCH_RETRIEVAL_LETTA_TOP_K_FACTOR_DEEP",
			policy.lettaTopKFactor,
		),
	}
	for mode, factor := range policy.lettaTopKFactorByMode {
		if factor < 1.0 {
			factor = 1.0
		}
		if factor > policy.lettaTopKFactor {
			factor = policy.lettaTopKFactor
		}
		policy.lettaTopKFactorByMode[mode] = factor
	}
	policy.lettaTopKCapByMode = map[string]int{
		"fast":     envInt("ORCH_RETRIEVAL_LETTA_TOP_K_CAP_FAST", 12),
		"balanced": envInt("ORCH_RETRIEVAL_LETTA_TOP_K_CAP_BALANCED", 18),
		"deep":     envInt("ORCH_RETRIEVAL_LETTA_TOP_K_CAP_DEEP", policy.lettaTopKCap),
	}
	for mode, capValue := range policy.lettaTopKCapByMode {
		if capValue < 1 {
			capValue = 1
		}
		if capValue > policy.lettaTopKCap {
			capValue = policy.lettaTopKCap
		}
		policy.lettaTopKCapByMode[mode] = capValue
	}
	policy.failOpenContinuationEnabled = envBool("ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", true)
	policy.failOpenContinuationSources = toSourceSet(csvListEnv(
		"ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES",
		"letta,memory_bank",
	))
	policy.timeoutAdaptiveSkipEnabled = envBool("ORCH_RECALL_TIMEOUT_ADAPTIVE_SOURCE_SKIP_ENABLED", true)
	policy.timeoutAdaptiveSkipSources = toSourceSet(csvListEnv(
		"ORCH_RECALL_TIMEOUT_ADAPTIVE_SKIP_SOURCES",
		"qdrant,weaviate,postgres_pgvector,mindsdb,mongo_raw",
	))
	policy.sourceTimeouts = map[string]time.Duration{
		sourceQdrant:      envDurationSeconds("ORCH_RETRIEVAL_QDRANT_TIMEOUT_SECS", 8),
		sourceWeaviate:    envDurationSeconds("ORCH_RETRIEVAL_WEAVIATE_TIMEOUT_SECS", 4),
		sourcePgvector:    envDurationSeconds("ORCH_RETRIEVAL_PGVECTOR_TIMEOUT_SECS", 3),
		sourceMongoRaw:    envDurationSeconds("ORCH_RETRIEVAL_MONGO_TIMEOUT_SECS", 6),
		sourceMindsdb:     envDurationSeconds("ORCH_RETRIEVAL_MINDSDB_TIMEOUT_SECS", 8),
		sourceTopicRollup: envDurationSeconds("ORCH_RETRIEVAL_TOPIC_ROLLUP_TIMEOUT_SECS", 25),
		sourceLetta:       envDurationSeconds("ORCH_RETRIEVAL_LETTA_TIMEOUT_SECS", 45),
		sourceMemoryBank:  envDurationSeconds("ORCH_RETRIEVAL_MEMORY_TIMEOUT_SECS", 3),
	}
	policy.topicRollupSyncTimeoutFloor = envDurationSeconds("GO_RETRIEVAL_TOPIC_ROLLUP_SYNC_TIMEOUT_FLOOR_SECS", 25)
	policy.topicRollupSyncTimeoutFloorByMode = map[string]time.Duration{
		"fast": envDurationSeconds(
			"GO_RETRIEVAL_TOPIC_ROLLUP_SYNC_TIMEOUT_FLOOR_FAST_SECS",
			12,
		),
		"balanced": envDurationSeconds(
			"GO_RETRIEVAL_TOPIC_ROLLUP_SYNC_TIMEOUT_FLOOR_BALANCED_SECS",
			policy.topicRollupSyncTimeoutFloor.Seconds(),
		),
		"deep": envDurationSeconds(
			"GO_RETRIEVAL_TOPIC_ROLLUP_SYNC_TIMEOUT_FLOOR_DEEP_SECS",
			40,
		),
	}
	for mode, floor := range policy.topicRollupSyncTimeoutFloorByMode {
		if floor < 0 {
			floor = 0
		}
		policy.topicRollupSyncTimeoutFloorByMode[mode] = floor
	}
	policy.topicRollupSearchTopN = clampInt(envInt("GO_RETRIEVAL_TOPIC_ROLLUP_SEARCH_TOPN", 2000), 200, 10000)
	policy.continuationTimeoutDefault = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_TIMEOUT_SECS", 45)
	policy.continuationTimeoutBySource = map[string]time.Duration{
		sourceLetta:      envDurationSeconds("ORCH_RETRIEVAL_LETTA_ASYNC_WARM_TIMEOUT_SECS", 180),
		sourceMemoryBank: envDurationSeconds("ORCH_RETRIEVAL_MEMORY_DEEP_TIMEOUT_CAP_SECS", 18),
	}
	policy.continuationMaxInflight = envInt("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT", 4)
	if policy.continuationMaxInflight < 1 {
		policy.continuationMaxInflight = 1
	}
	policy.continuationMaxInflightPerSource = envInt("GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE", 2)
	if policy.continuationMaxInflightPerSource < 1 {
		policy.continuationMaxInflightPerSource = 1
	}
	if policy.continuationMaxInflightPerSource > policy.continuationMaxInflight {
		policy.continuationMaxInflightPerSource = policy.continuationMaxInflight
	}
	policy.continuationMaxInflightOverrides = map[string]int{
		sourceLetta: envInt(
			"GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE_LETTA",
			policy.continuationMaxInflightPerSource,
		),
		sourceMemoryBank: envInt(
			"GO_RETRIEVAL_CONTINUATION_MAX_INFLIGHT_PER_SOURCE_MEMORY_BANK",
			policy.continuationMaxInflightPerSource,
		),
	}
	for source, limit := range policy.continuationMaxInflightOverrides {
		if limit < 1 {
			limit = 1
		}
		if limit > policy.continuationMaxInflight {
			limit = policy.continuationMaxInflight
		}
		policy.continuationMaxInflightOverrides[source] = limit
	}
	continuationCooldownSecs := envFloat("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS", 15)
	if continuationCooldownSecs < 0 {
		continuationCooldownSecs = 0
	}
	policy.continuationSourceCooldown = time.Duration(continuationCooldownSecs * float64(time.Second))
	lettaCooldownSecs := envFloat("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS_LETTA", continuationCooldownSecs)
	if lettaCooldownSecs < 0 {
		lettaCooldownSecs = 0
	}
	memoryBankCooldownSecs := envFloat("GO_RETRIEVAL_CONTINUATION_SOURCE_COOLDOWN_SECS_MEMORY_BANK", continuationCooldownSecs)
	if memoryBankCooldownSecs < 0 {
		memoryBankCooldownSecs = 0
	}
	policy.continuationSourceCooldownBySrc = map[string]time.Duration{
		sourceLetta:      time.Duration(lettaCooldownSecs * float64(time.Second)),
		sourceMemoryBank: time.Duration(memoryBankCooldownSecs * float64(time.Second)),
	}
	for source, cooldown := range policy.continuationSourceCooldownBySrc {
		if cooldown < 0 {
			cooldown = 0
		}
		policy.continuationSourceCooldownBySrc[source] = cooldown
	}
	policy.continuationSheddingEnabled = envBool("GO_RETRIEVAL_CONTINUATION_SHEDDING_ENABLED", true)
	policy.continuationSheddingQueueRatio = envFloat("GO_RETRIEVAL_CONTINUATION_SHEDDING_QUEUE_RATIO", 0.85)
	if policy.continuationSheddingQueueRatio <= 0 {
		policy.continuationSheddingQueueRatio = 0.85
	}
	if policy.continuationSheddingQueueRatio > 1 {
		policy.continuationSheddingQueueRatio = 1
	}
	policy.continuationSheddingPendingHigh = envInt("GO_RETRIEVAL_CONTINUATION_SHEDDING_PENDING_HIGH", maxInt(2, policy.continuationMaxInflight-1))
	if policy.continuationSheddingPendingHigh < 1 {
		policy.continuationSheddingPendingHigh = 1
	}
	policy.continuationSheddingSources = toSourceSet(csvListEnv(
		"GO_RETRIEVAL_CONTINUATION_SHEDDING_SOURCES",
		"letta,memory_bank,mongo_raw,mindsdb",
	))
	policy.syncSourceConcurrencyDefault = envInt("GO_RETRIEVAL_SYNC_SOURCE_CONCURRENCY_DEFAULT", 2)
	if policy.syncSourceConcurrencyDefault < 1 {
		policy.syncSourceConcurrencyDefault = 1
	}
	policy.syncSourceConcurrencyOverrides = intMapEnv(
		"GO_RETRIEVAL_SYNC_SOURCE_CONCURRENCY_OVERRIDES",
		map[string]int{
			sourceTopicRollup: 1,
			sourceQdrant:      policy.syncSourceConcurrencyDefault,
			sourceWeaviate:    policy.syncSourceConcurrencyDefault,
			sourcePgvector:    policy.syncSourceConcurrencyDefault,
			sourceMongoRaw:    1,
			sourceMindsdb:     1,
			sourceLetta:       1,
			sourceMemoryBank:  1,
		},
	)
	policy.syncQueueAgeWarnSecs = envFloat("GO_RETRIEVAL_SYNC_QUEUE_AGE_WARN_SECS", 2.0)
	if policy.syncQueueAgeWarnSecs < 0 {
		policy.syncQueueAgeWarnSecs = 0
	}
	policy.syncQueueAgeHighSecs = envFloat("GO_RETRIEVAL_SYNC_QUEUE_AGE_HIGH_SECS", 5.0)
	if policy.syncQueueAgeHighSecs < policy.syncQueueAgeWarnSecs {
		policy.syncQueueAgeHighSecs = policy.syncQueueAgeWarnSecs
	}
	policy.timeoutContractGrace = envDurationSeconds("GO_RETRIEVAL_TIMEOUT_CONTRACT_GRACE_SECS", 0.075)
	if policy.timeoutContractGrace < 0 {
		policy.timeoutContractGrace = 0
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
	policy.adaptiveTimeoutEnabled = envBool("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_ENABLED", true)
	policy.adaptiveTimeoutMinRequests = envInt("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_MIN_REQUESTS", 8)
	if policy.adaptiveTimeoutMinRequests < 1 {
		policy.adaptiveTimeoutMinRequests = 1
	}
	policy.adaptiveTimeoutWindow = envInt("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_WINDOW", 128)
	if policy.adaptiveTimeoutWindow < 8 {
		policy.adaptiveTimeoutWindow = 8
	}
	policy.adaptiveTimeoutP95Factor = envFloat("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_P95_FACTOR", 1.4)
	if policy.adaptiveTimeoutP95Factor <= 0 {
		policy.adaptiveTimeoutP95Factor = 1.4
	}
	policy.adaptiveTimeoutMinScale = envFloat("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_MIN_SCALE", 0.6)
	if policy.adaptiveTimeoutMinScale <= 0 {
		policy.adaptiveTimeoutMinScale = 0.6
	}
	if policy.adaptiveTimeoutMinScale > 1 {
		policy.adaptiveTimeoutMinScale = 1
	}
	policy.adaptiveTimeoutMaxScale = envFloat("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_MAX_SCALE", 1.8)
	if policy.adaptiveTimeoutMaxScale < 1 {
		policy.adaptiveTimeoutMaxScale = 1
	}
	if policy.adaptiveTimeoutMaxScale < policy.adaptiveTimeoutMinScale {
		policy.adaptiveTimeoutMaxScale = policy.adaptiveTimeoutMinScale
	}
	policy.adaptiveTimeoutBacklogWeight = envFloat("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_BACKLOG_WEIGHT", 0.12)
	if policy.adaptiveTimeoutBacklogWeight < 0 {
		policy.adaptiveTimeoutBacklogWeight = 0
	}
	policy.adaptiveTimeoutBacklogCap = envInt("GO_RETRIEVAL_ADAPTIVE_TIMEOUT_BACKLOG_CAP", 6)
	if policy.adaptiveTimeoutBacklogCap < 1 {
		policy.adaptiveTimeoutBacklogCap = 1
	}
	policy.continuationEventHistory = envInt("GO_RETRIEVAL_CONTINUATION_EVENT_HISTORY", 32)
	if policy.continuationEventHistory < 4 {
		policy.continuationEventHistory = 4
	}
	policy.continuationEventTTL = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_EVENT_TTL_SECS", 900)
	if policy.continuationEventTTL < 30*time.Second {
		policy.continuationEventTTL = 30 * time.Second
	}
	policy.continuationSSEHeartbeat = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_SSE_HEARTBEAT_SECS", 15)
	if policy.continuationSSEHeartbeat < 3*time.Second {
		policy.continuationSSEHeartbeat = 3 * time.Second
	}
	policy.continuationDurableEnabled = envBool("GO_RETRIEVAL_CONTINUATION_DURABLE_ENABLED", true)
	policy.continuationDurableDir = strings.TrimSpace(resolveStoragePath(
		"GO_RETRIEVAL_CONTINUATION_DURABLE_DIR",
		"services/orchestrator/data/continuation_outbox",
	))
	if policy.continuationDurableDir == "" {
		policy.continuationDurableEnabled = false
	}
	policy.continuationDurableMaxPending = envInt("GO_RETRIEVAL_CONTINUATION_DURABLE_MAX_PENDING", 2000)
	if policy.continuationDurableMaxPending < 64 {
		policy.continuationDurableMaxPending = 64
	}
	policy.continuationDurableDrainBatch = envInt("GO_RETRIEVAL_CONTINUATION_DURABLE_DRAIN_BATCH", 32)
	if policy.continuationDurableDrainBatch < 1 {
		policy.continuationDurableDrainBatch = 1
	}
	policy.continuationDurablePollInterval = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_DURABLE_POLL_SECS", 2)
	if policy.continuationDurablePollInterval < 250*time.Millisecond {
		policy.continuationDurablePollInterval = 250 * time.Millisecond
	}
	policy.continuationDurableRetryBase = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_DURABLE_RETRY_BASE_SECS", 2)
	if policy.continuationDurableRetryBase < 500*time.Millisecond {
		policy.continuationDurableRetryBase = 500 * time.Millisecond
	}
	policy.continuationDurableRetryMax = envDurationSeconds("GO_RETRIEVAL_CONTINUATION_DURABLE_RETRY_MAX_SECS", 60)
	if policy.continuationDurableRetryMax < policy.continuationDurableRetryBase {
		policy.continuationDurableRetryMax = policy.continuationDurableRetryBase
	}
	policy.continuationDurableMaxAttempts = envInt("GO_RETRIEVAL_CONTINUATION_DURABLE_MAX_ATTEMPTS", 8)
	if policy.continuationDurableMaxAttempts < 1 {
		policy.continuationDurableMaxAttempts = 1
	}
	policy.sourceOwnershipMode = strings.TrimSpace(strings.ToLower(os.Getenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_MODE")))
	switch policy.sourceOwnershipMode {
	case "", "off":
		policy.sourceOwnershipMode = "off"
	case "warn", "strict":
	default:
		policy.sourceOwnershipMode = "off"
	}
	policy.sourceOwnershipStrictFastAllowPy = parseNormalizedSet(
		os.Getenv("GO_RETRIEVAL_SOURCE_OWNERSHIP_STRICT_FAST_ALLOW_PYTHON"),
		"",
	)
	return policy
}

func loadLettaConfig() lettaConfig {
	lettaURL := strings.TrimSpace(os.Getenv("LETTA_URL"))
	if lettaURL == "" {
		lettaURL = "http://letta:8283"
	}
	lettaURL = strings.TrimRight(lettaURL, "/")
	autoSessionID := strings.TrimSpace(os.Getenv("LETTA_AUTO_SESSION_ID"))
	if autoSessionID == "" {
		autoSessionID = "memmcp-default"
	}
	apiKey := strings.TrimSpace(os.Getenv("LETTA_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("CONTEXTLATTICE_LETTA_API_KEY"))
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("MEMMCP_LETTA_API_KEY"))
	}
	requestTimeout := envDurationSeconds("LETTA_REQUEST_TIMEOUT_SECS", 240)
	if requestTimeout < time.Second {
		requestTimeout = time.Second
	}
	verifyInterval := envDurationSeconds("LETTA_AGENT_VERIFY_INTERVAL_SECS", 300)
	if verifyInterval < time.Second {
		verifyInterval = time.Second
	}
	return lettaConfig{
		url:            lettaURL,
		apiKey:         apiKey,
		requireAPIKey:  envBool("LETTA_REQUIRE_API_KEY", false),
		autoSessionID:  autoSessionID,
		agentModel:     strings.TrimSpace(os.Getenv("LETTA_AGENT_MODEL")),
		agentEmbedding: strings.TrimSpace(os.Getenv("LETTA_AGENT_EMBEDDING")),
		requestTimeout: requestTimeout,
		verifyInterval: verifyInterval,
	}
}

func newServer() *server {
	backendURL := strings.TrimRight(strings.TrimSpace(os.Getenv("BACKEND_URL")), "/")
	if backendURL == "" {
		backendURL = "http://contextlattice-orchestrator:8075"
	}
	strictNoPythonRuntime := envBool("GO_RUNTIME_STRICT_NO_PYTHON", false)
	pythonHotPathMode := strings.TrimSpace(strings.ToLower(os.Getenv("GO_PYTHON_HOT_PATH_OWNERSHIP_MODE")))
	if pythonHotPathMode == "" {
		pythonHotPathMode = "warn"
	}
	if strictNoPythonRuntime {
		pythonHotPathMode = "strict"
	}
	if pythonHotPathMode != "off" && pythonHotPathMode != "warn" && pythonHotPathMode != "strict" {
		pythonHotPathMode = "warn"
	}
	orchestratorAPIKey := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY"))
	if orchestratorAPIKey == "" {
		orchestratorAPIKey = strings.TrimSpace(os.Getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY"))
	}
	timeout := envDurationSeconds("GATEWAY_PROXY_TIMEOUT_SECS", 95)
	policy := loadRetrievalPolicy()
	letta := loadLettaConfig()
	toolPolicy := loadToolCallPolicy(orchestratorAPIKey)
	writePolicy := loadWriteIngressPolicy()
	trackedPaths := defaultTrackedPaths()
	tradingHistoryPath := strings.TrimSpace(trackedPaths["trading_history"])
	if tradingHistoryPath == "" {
		tradingHistoryPath = "services/orchestrator/data/trading_metrics.ndjson"
	}
	tradingHistoryLimit := envInt("TRADING_HISTORY_LIMIT", 256)
	if tradingHistoryLimit < 1 {
		tradingHistoryLimit = 1
	}
	telemetrySinkInstance, sinkErr := newTelemetrySinkFromEnv()
	if sinkErr != nil {
		log.Printf("gateway-go telemetry sink disabled: %v", sinkErr)
		telemetrySinkInstance = &telemetrySink{enabled: false}
	}
	telemetrySpoolInstance := newTelemetrySpoolFromEnv()
	telemetryRingInstance := newTelemetryRingFromEnv()
	memoryStoreInstance, memoryStoreErr := newMemoryStoreFromEnv()
	if memoryStoreErr != nil {
		log.Printf("gateway-go memory store disabled: %v", memoryStoreErr)
		memoryStoreInstance = &memoryStore{policy: memoryStorePolicy{enabled: false}}
	}
	continuationDurable := newContinuationDurableQueue(policy)
	t := newRetrievalTelemetry(policy)
	s := &server{
		backendURL:                      backendURL,
		orchestratorAPIKey:              orchestratorAPIKey,
		strictNoPythonRuntime:           strictNoPythonRuntime,
		pythonHotPathMode:               pythonHotPathMode,
		pythonHotPathByPath:             map[string]uint64{},
		pythonHotPathByReason:           map[string]uint64{},
		client:                          &http.Client{Timeout: timeout},
		retrieval:                       policy,
		letta:                           letta,
		toolCalls:                       toolPolicy,
		telemetry:                       t,
		writePolicy:                     writePolicy,
		memoryStore:                     memoryStoreInstance,
		memoryProfilesStore:             newMemoryProfileStore(policy),
		telemetrySink:                   telemetrySinkInstance,
		telemetrySpool:                  telemetrySpoolInstance,
		telemetryRing:                   telemetryRingInstance,
		telemetryMetricsState:           defaultTelemetryMetricsState(),
		tradingState:                    defaultTradingState(),
		tradingHistory:                  make([]map[string]any, 0, tradingHistoryLimit),
		tradingHistoryPath:              tradingHistoryPath,
		tradingHistoryLimit:             tradingHistoryLimit,
		continuationSem:                 make(chan struct{}, policy.continuationMaxInflight),
		syncSourceSem:                   buildSyncSourceSem(policy),
		syncSourcePending:               make(map[string][]time.Time),
		syncSourceInFlight:              make(map[string]int),
		syncSourceRetrying:              make(map[string]int),
		adaptiveBySource:                make(map[string]*adaptiveSourceStats),
		continuationInFlight:            make(map[string]int),
		continuationInFlightStarted:     make(map[string][]time.Time),
		continuationRetrying:            make(map[string]int),
		continuationSourceCooldownUntil: make(map[string]time.Time),
		continuationSubscribers:         make(map[string][]chan map[string]any),
		continuationHistory:             make(map[string][]map[string]any),
		continuationExpiry:              make(map[string]time.Time),
		continuationDurable:             continuationDurable,
		timeoutContractBySource:         make(map[string]uint64),
		timeoutContractLast:             make(map[string]any),
		driftByClass:                    make(map[string]uint64),
		driftBySource:                   make(map[string]uint64),
		driftLast:                       make(map[string]any),
		lettaAgentBySession:             make(map[string]string),
		lettaAgentVerifiedAt:            make(map[string]time.Time),
	}
	if err := s.loadTradingHistoryFromDisk(); err != nil {
		log.Printf("gateway-go trading history load failed: %v", err)
	}
	t.start()
	s.startContinuationDurableWorker()
	return s
}

func (s *server) recordPythonHotPathFallback(path string, reason string) uint64 {
	normalizedPath := normalizeHotPath(path)
	normalizedReason := strings.TrimSpace(strings.ToLower(reason))
	if normalizedReason == "" {
		normalizedReason = "unspecified"
	}
	total := s.pythonHotPathFallbacks.Add(1)
	s.pythonHotPathMu.Lock()
	s.pythonHotPathByPath[normalizedPath] = s.pythonHotPathByPath[normalizedPath] + 1
	s.pythonHotPathByReason[normalizedReason] = s.pythonHotPathByReason[normalizedReason] + 1
	s.pythonHotPathLastAt = nowUTCISO()
	s.pythonHotPathMu.Unlock()
	return total
}

func (s *server) pythonHotPathOwnershipSnapshot() map[string]any {
	total := s.pythonHotPathFallbacks.Load()
	byPath := map[string]uint64{}
	byReason := map[string]uint64{}
	lastAt := ""
	s.pythonHotPathMu.Lock()
	for key, value := range s.pythonHotPathByPath {
		byPath[key] = value
	}
	for key, value := range s.pythonHotPathByReason {
		byReason[key] = value
	}
	lastAt = s.pythonHotPathLastAt
	s.pythonHotPathMu.Unlock()
	status := "clean"
	if total > 0 {
		status = "python_fallback_detected"
	}
	return map[string]any{
		"mode":           s.pythonHotPathMode,
		"ok":             total == 0,
		"status":         status,
		"fallbacks":      total,
		"byPath":         byPath,
		"byReason":       byReason,
		"lastFallbackAt": lastAt,
	}
}

func (s *server) allowPythonHotPathFallback(w http.ResponseWriter, path string, reason string) bool {
	total := s.recordPythonHotPathFallback(path, reason)
	log.Printf(
		"gateway-go python hot-path fallback mode=%s path=%s reason=%s total=%d",
		s.pythonHotPathMode,
		normalizeHotPath(path),
		strings.TrimSpace(strings.ToLower(reason)),
		total,
	)
	if s.strictNoPythonRuntime || s.pythonHotPathMode == "strict" {
		errorCode := "python_hot_path_fallback_blocked"
		if s.strictNoPythonRuntime {
			errorCode = "python_runtime_disabled"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":        false,
			"error":     errorCode,
			"path":      normalizeHotPath(path),
			"reason":    strings.TrimSpace(strings.ToLower(reason)),
			"fallbacks": total,
			"strict":    true,
		})
		return false
	}
	return true
}

func isProxyPath(path string) bool {
	if strings.HasPrefix(path, "/memory/search/async/") {
		return true
	}
	if strings.HasPrefix(path, "/memory/search/jobs/") {
		return true
	}
	if strings.HasPrefix(path, "/memory/files/") {
		return true
	}
	if strings.HasPrefix(path, "/memory/profiles/") {
		return true
	}
	if strings.HasPrefix(path, "/memory/continuity/snapshots/") {
		return true
	}
	if strings.HasPrefix(path, "/agents/tasks/") {
		return true
	}
	if strings.HasPrefix(path, "/telemetry/") {
		return true
	}
	if strings.HasPrefix(path, "/maintenance/") {
		return true
	}
	switch path {
	case "/v1/retrieval/query",
		"/v1/retrieval/query-with-grounding",
		"/v1/retrieval/batch-query",
		"/v1/retrieval/health",
		"/health",
		"/status",
		"/memory/search",
		"/memory/write",
		"/memory/recall/eval-cases",
		"/memory/recall/eval-cases/refresh",
		"/memory/recall/evaluate/saved",
		"/memory/write/batch",
		"/memory/browser-context",
		"/memory/context-pack",
		"/memory/continuity/snapshot",
		"/memory/continuity/snapshots",
		"/memory/recent",
		"/memory/profiles",
		"/memory/topics",
		"/memory/topics/list",
		"/memory/topic-rollups",
		"/feedback",
		"/agents/tasks",
		"/ops/queue/status",
		"/ops/capabilities",
		"/tools/capability_map",
		"/tools/ops_queue_status",
		"/tools/memory_write_batch",
		"/tools/feedback_submit",
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

func continuationEventsToken(path string) string {
	const prefix = "/memory/search/continuations/"
	const suffix = "/events"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}
	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.TrimSuffix(trimmed, suffix)
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" || strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
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
	if strings.TrimSpace(dst.Get("X-Api-Key")) != "" {
		return
	}
	authHeader := strings.TrimSpace(src.Get("Authorization"))
	if authHeader == "" {
		return
	}
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(strings.ToLower(authHeader), strings.ToLower(bearerPrefix)) {
		return
	}
	token := strings.TrimSpace(authHeader[len(bearerPrefix):])
	if token != "" {
		dst.Set("X-Api-Key", token)
	}
}

func requestAPIKey(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	if token := strings.TrimSpace(r.Header.Get("X-Api-Key")); token != "" {
		return token, true
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "Bearer ") {
		if token := strings.TrimSpace(authHeader[7:]); token != "" {
			return token, true
		}
	}
	query := r.URL.Query()
	for _, key := range []string{"api_key", "x_api_key", "token"} {
		if token := strings.TrimSpace(query.Get(key)); token != "" {
			return token, true
		}
	}
	return "", false
}

func (s *server) prepareAuthorizedHeaders(w http.ResponseWriter, r *http.Request) (http.Header, bool) {
	headers := r.Header.Clone()
	expected := strings.TrimSpace(s.orchestratorAPIKey)
	provided, explicit := requestAPIKey(r)
	if expected != "" {
		if explicit {
			if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
				return nil, false
			}
		}
		// Normalize all backend calls to the configured orchestrator key to prevent subcall auth drift.
		headers.Set("X-Api-Key", expected)
		return headers, true
	}
	if explicit && strings.TrimSpace(headers.Get("X-Api-Key")) == "" {
		headers.Set("X-Api-Key", provided)
	}
	return headers, true
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	if !isProxyPath(r.URL.Path) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "strict_runtime_backend_forward_disabled") {
			return
		}
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
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	s.proxyWithBodyToTarget(w, r, incomingHeaders, r.Method, r.URL.Path, r.URL.RawQuery, bodyBytes)
}

func (s *server) proxyWithBodyToTarget(
	w http.ResponseWriter,
	r *http.Request,
	incomingHeaders http.Header,
	method string,
	targetPath string,
	targetQuery string,
	bodyBytes []byte,
) {
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, targetPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	targetURL := s.backendURL + targetPath
	if query := strings.TrimSpace(targetQuery); query != "" {
		targetURL += "?" + query
	}

	req, err := http.NewRequestWithContext(r.Context(), method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to build proxy request"})
		return
	}
	s.copyHeaders(req.Header, incomingHeaders)
	if req.Header.Get("X-Forwarded-For") == "" {
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")

	if isStreamingProxyPath(targetPath) && strings.EqualFold(method, http.MethodGet) {
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

func parseBoolLoose(raw string) (bool, bool) {
	token := strings.TrimSpace(strings.ToLower(raw))
	if token == "" {
		return false, false
	}
	switch token {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func secureTokenEqual(left string, right string) bool {
	a := strings.TrimSpace(left)
	b := strings.TrimSpace(right)
	if a == "" || b == "" || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func toolPathAllowed(path string, allowAll bool, allowlist map[string]struct{}, denylist map[string]struct{}) bool {
	normalized := normalizeToolPath(path)
	if normalized == "" {
		return false
	}
	if _, blocked := denylist[normalized]; blocked {
		return false
	}
	if allowAll {
		return true
	}
	if len(allowlist) == 0 {
		return false
	}
	_, allowed := allowlist[normalized]
	return allowed
}

func (s *server) toolCallerRole(apiKey string) (string, bool) {
	if secureTokenEqual(apiKey, s.orchestratorAPIKey) {
		return "orchestrator", true
	}
	if secureTokenEqual(apiKey, s.toolCalls.workerKey) {
		return "worker", true
	}
	return "", false
}

func (s *server) toolCallAllowedForRole(path string, role string) bool {
	if role == "worker" {
		return toolPathAllowed(
			path,
			s.toolCalls.workerRole.allowAll,
			s.toolCalls.workerRole.allowlist,
			s.toolCalls.workerRole.denylist,
		)
	}
	return toolPathAllowed(
		path,
		s.toolCalls.orchestratorRole.allowAll,
		s.toolCalls.orchestratorRole.allowlist,
		s.toolCalls.orchestratorRole.denylist,
	)
}

func (s *server) toolCallAllowed(path string) bool {
	return toolPathAllowed(
		path,
		s.toolCalls.allowAll,
		s.toolCalls.allowlist,
		s.toolCalls.denylist,
	)
}

func (s *server) prepareToolHeaders(w http.ResponseWriter, r *http.Request, toolPath string) (http.Header, bool) {
	headers := r.Header.Clone()
	expected := strings.TrimSpace(s.orchestratorAPIKey)
	provided, explicit := requestAPIKey(r)

	if s.toolCalls.roleSplitEnabled {
		if !explicit {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok":    false,
				"error": "API key required for role-scoped tool access",
			})
			return nil, false
		}
		role, recognized := s.toolCallerRole(provided)
		if !recognized {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
			return nil, false
		}
		if !s.toolCallAllowedForRole(toolPath, role) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"ok":    false,
				"error": "Tool call blocked by role policy",
				"role":  role,
				"tool":  normalizeToolPath(toolPath),
			})
			return nil, false
		}
		if expected != "" {
			headers.Set("X-Api-Key", expected)
		} else {
			headers.Set("X-Api-Key", provided)
		}
		headers.Set("X-ContextLattice-Caller-Role", role)
		return headers, true
	}

	if !s.toolCallAllowed(toolPath) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "Tool call blocked by policy", "tool": normalizeToolPath(toolPath)})
		return nil, false
	}

	if s.toolCalls.requireAPIKey {
		if !explicit {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "API key required"})
			return nil, false
		}
		if expected != "" {
			if !secureTokenEqual(provided, expected) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
				return nil, false
			}
			headers.Set("X-Api-Key", expected)
			return headers, true
		}
		headers.Set("X-Api-Key", provided)
		return headers, true
	}

	if expected != "" {
		if explicit && s.toolCalls.enforceProvidedKey {
			if !secureTokenEqual(provided, expected) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
				return nil, false
			}
		}
		headers.Set("X-Api-Key", expected)
		return headers, true
	}

	if explicit && strings.TrimSpace(headers.Get("X-Api-Key")) == "" {
		headers.Set("X-Api-Key", provided)
	}
	return headers, true
}

func (s *server) toolsFeedbackSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, "/tools/feedback_submit", "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	incomingHeaders, ok := s.prepareToolHeaders(w, r, "/tools/feedback_submit")
	if !ok {
		return
	}
	s.proxyWithBodyToTarget(w, r, incomingHeaders, http.MethodPost, "/tools/feedback_submit", r.URL.RawQuery, bodyBytes)
}

func (s *server) toolsMemoryWriteBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/memory_write_batch"); !ok {
		return
	}
	if s.strictNoPythonRuntime || (s.memoryStore != nil && s.memoryStore.policy.enabled) {
		s.handleWriteBatchIngress(w, r, "/tools/memory_write_batch", "/memory/write")
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	s.proxyWithBodyToTarget(w, r, incomingHeaders, http.MethodPost, "/tools/memory_write_batch", r.URL.RawQuery, bodyBytes)
}

func (s *server) toolsCapabilityMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/capability_map"); !ok {
		return
	}
	if r.Method == http.MethodPost {
		if _, err := readRequestBody(r); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
	}
	writeJSON(w, http.StatusOK, s.capabilityMapPayload())
}

func (s *server) toolsOpsQueueStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareToolHeaders(w, r, "/tools/ops_queue_status"); !ok {
		return
	}
	query := r.URL.Query()
	includeDeadletters, hasInclude := parseBoolLoose(query.Get("include_deadletters"))
	if !hasInclude && r.Method == http.MethodPost {
		raw, err := readRequestBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		if strings.TrimSpace(string(raw)) != "" {
			payload := map[string]any{}
			if err := json.Unmarshal(raw, &payload); err == nil {
				if value, present := payload["include_deadletters"]; present {
					includeDeadletters = anyToBool(value)
					hasInclude = true
				}
			}
		}
	}
	if !hasInclude {
		includeDeadletters = false
	}
	deadletterLimit := parseOptionalIntQuery(query.Get("deadletter_limit"), 100, 1, 500)
	deadletterTarget := strings.TrimSpace(strings.ToLower(query.Get("deadletter_target")))
	highWatermark := parseOptionalFloatQuery(query.Get("queue_high_watermark"), 0.85, 0.1, 1.0)
	pendingThreshold := parseOptionalIntQuery(query.Get("pending_high_threshold"), maxInt(3, cap(s.continuationSem)/2), 1, 100000)
	retryingThreshold := parseOptionalIntQuery(query.Get("retrying_high_threshold"), maxInt(2, cap(s.continuationSem)/4), 1, 100000)
	writeJSON(w, http.StatusOK, s.buildQueueStatusPayload(
		includeDeadletters,
		deadletterLimit,
		deadletterTarget,
		highWatermark,
		pendingThreshold,
		retryingThreshold,
	))
}

type agentPreflightRequest struct {
	Project       string `json:"project"`
	TopicPath     string `json:"topic_path"`
	Query         string `json:"query"`
	RetrievalMode string `json:"retrieval_mode"`
	AgentID       string `json:"agent_id"`
	Agent         string `json:"agent"`
}

type agentPreflightProfile struct {
	AgentID       string `json:"agent_id"`
	TopicPath     string `json:"topic_path"`
	Query         string `json:"query"`
	RetrievalMode string `json:"retrieval_mode"`
}

var defaultAgentPreflightProfiles = map[string]agentPreflightProfile{
	"codex": {
		AgentID:       "codex_gpt5",
		TopicPath:     "runbooks/codex-integration",
		Query:         "codex preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"claude-code": {
		AgentID:       "claude_code_agent",
		TopicPath:     "runbooks/claude-code-integration",
		Query:         "claude code preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"opencode": {
		AgentID:       "opencode_agent",
		TopicPath:     "runbooks/opencode-integration",
		Query:         "opencode preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"hermes-agent": {
		AgentID:       "hermes_agent",
		TopicPath:     "runbooks/hermes-agent-integration",
		Query:         "hermes agent preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"chatgpt-web": {
		AgentID:       "chatgpt_web_agent",
		TopicPath:     "runbooks/chatgpt-web-integration",
		Query:         "chatgpt web session preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"chatgpt-desktop": {
		AgentID:       "chatgpt_desktop_agent",
		TopicPath:     "runbooks/chatgpt-desktop-integration",
		Query:         "chatgpt desktop session preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"claude-web": {
		AgentID:       "claude_web_agent",
		TopicPath:     "runbooks/claude-web-integration",
		Query:         "claude web session preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
	"claude-desktop": {
		AgentID:       "claude_desktop_agent",
		TopicPath:     "runbooks/claude-desktop-integration",
		Query:         "claude desktop session preflight connectivity and retrieval",
		RetrievalMode: "balanced",
	},
}

var agentPreflightAliases = map[string]string{
	"codex_gpt5":      "codex",
	"claude-code":     "claude-code",
	"claude_code":     "claude-code",
	"opencode":        "opencode",
	"hermes":          "hermes-agent",
	"hermes-agent":    "hermes-agent",
	"chatgpt":         "chatgpt-web",
	"chatgpt-web":     "chatgpt-web",
	"chatgpt-desktop": "chatgpt-desktop",
	"claude":          "claude-web",
	"claude-web":      "claude-web",
	"claude-desktop":  "claude-desktop",
}

func normalizeAgentPreflightKey(agent string) string {
	candidate := strings.TrimSpace(strings.ToLower(agent))
	if candidate == "" {
		return "codex"
	}
	if mapped, ok := agentPreflightAliases[candidate]; ok {
		return mapped
	}
	return candidate
}

func resolveAgentPreflightProfile(agent string) (string, agentPreflightProfile) {
	key := normalizeAgentPreflightKey(agent)
	profile, ok := defaultAgentPreflightProfiles[key]
	if !ok {
		key = "codex"
		profile = defaultAgentPreflightProfiles[key]
	}
	return key, profile
}

func (s *server) backendJSONRequest(
	ctx context.Context,
	method string,
	path string,
	headers http.Header,
	payload any,
) (map[string]any, int, error) {
	if s.strictNoPythonRuntime {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("python backend forwarding disabled by strict runtime policy")
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.backendURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.copyHeaders(req.Header, headers)
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	out := map[string]any{}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return out, resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		out["raw"] = string(raw)
	}
	return out, resp.StatusCode, nil
}

func resultCount(payload map[string]any) int {
	results, ok := payload["results"].([]any)
	if ok {
		return len(results)
	}
	items, ok := payload["items"].([]any)
	if ok {
		return len(items)
	}
	if total, ok := payload["total"].(float64); ok {
		return int(total)
	}
	return 0
}

func (s *server) codexPreflight(w http.ResponseWriter, r *http.Request) {
	s.agentPreflight(w, r, "codex")
}

func (s *server) agentsPreflight(w http.ResponseWriter, r *http.Request) {
	s.agentPreflight(w, r, "")
}

func (s *server) agentPreflight(w http.ResponseWriter, r *http.Request, forcedAgent string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	reqBody := agentPreflightRequest{}
	rawBody, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	if len(bytes.TrimSpace(rawBody)) > 0 {
		if err := json.Unmarshal(rawBody, &reqBody); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
	}
	if strings.TrimSpace(forcedAgent) != "" {
		reqBody.Agent = strings.TrimSpace(forcedAgent)
	}
	profileKey, profile := resolveAgentPreflightProfile(reqBody.Agent)
	if strings.TrimSpace(reqBody.Project) == "" {
		reqBody.Project = "contextlattice"
	}
	if strings.TrimSpace(reqBody.TopicPath) == "" {
		reqBody.TopicPath = profile.TopicPath
	}
	if strings.TrimSpace(reqBody.Query) == "" {
		reqBody.Query = profile.Query
	}
	if strings.TrimSpace(reqBody.RetrievalMode) == "" {
		reqBody.RetrievalMode = profile.RetrievalMode
	}
	if strings.TrimSpace(reqBody.AgentID) == "" {
		reqBody.AgentID = strings.TrimSpace(profile.AgentID)
	}
	if strings.TrimSpace(reqBody.AgentID) == "" {
		reqBody.AgentID = strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AGENT_ID"))
	}
	if strings.TrimSpace(reqBody.AgentID) == "" {
		reqBody.AgentID = strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AGENT_ID"))
	}
	if strings.TrimSpace(reqBody.AgentID) == "" {
		reqBody.AgentID = "codex_gpt5"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	healthPayload, healthStatus, healthErr := s.backendJSONRequest(ctx, http.MethodGet, "/health", r.Header, nil)
	statusPayload, statusStatus, statusErr := s.backendJSONRequest(ctx, http.MethodGet, "/status", r.Header, nil)

	scopedSearchReq := map[string]any{
		"project":                 reqBody.Project,
		"query":                   reqBody.Query,
		"topic_path":              reqBody.TopicPath,
		"retrieval_mode":          reqBody.RetrievalMode,
		"include_grounding":       true,
		"include_retrieval_debug": true,
		"agent_id":                reqBody.AgentID,
	}
	scopedPayload, scopedStatus, scopedErr := s.backendJSONRequest(
		ctx,
		http.MethodPost,
		"/memory/search",
		r.Header,
		scopedSearchReq,
	)

	var broadenedPayload map[string]any
	var broadenedStatus int
	var broadenedErr error
	needsBroaden := false
	if scopedErr == nil && scopedPayload != nil {
		degraded := anyToBool(scopedPayload["degraded"])
		if degraded || resultCount(scopedPayload) == 0 {
			needsBroaden = true
		}
	}
	if scopedErr != nil {
		needsBroaden = true
	}
	if needsBroaden {
		broadReq := map[string]any{
			"project":                 reqBody.Project,
			"query":                   reqBody.Query,
			"retrieval_mode":          reqBody.RetrievalMode,
			"include_grounding":       true,
			"include_retrieval_debug": true,
			"agent_id":                reqBody.AgentID,
		}
		broadenedPayload, broadenedStatus, broadenedErr = s.backendJSONRequest(
			ctx,
			http.MethodPost,
			"/memory/search",
			r.Header,
			broadReq,
		)
	}

	contextPackReq := map[string]any{
		"project":                 reqBody.Project,
		"query":                   reqBody.Query,
		"topic_path":              reqBody.TopicPath,
		"retrieval_mode":          reqBody.RetrievalMode,
		"include_retrieval_debug": true,
		"agent_id":                reqBody.AgentID,
	}
	contextPackPayload, contextPackStatus, contextPackErr := s.backendJSONRequest(
		ctx,
		http.MethodPost,
		"/memory/context-pack",
		r.Header,
		contextPackReq,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"service":          "gateway-go",
		"agent":            profileKey,
		"agent_profile":    profile,
		"agent_id":         reqBody.AgentID,
		"project":          reqBody.Project,
		"query":            reqBody.Query,
		"topic_path":       reqBody.TopicPath,
		"retrieval_mode":   reqBody.RetrievalMode,
		"backend_url":      s.backendURL,
		"health":           healthPayload,
		"health_status":    healthStatus,
		"health_error":     errString(healthErr),
		"status":           statusPayload,
		"status_status":    statusStatus,
		"status_error":     errString(statusErr),
		"scoped_search":    scopedPayload,
		"scoped_status":    scopedStatus,
		"scoped_error":     errString(scopedErr),
		"broadened_search": broadenedPayload,
		"broadened_status": broadenedStatus,
		"broadened_error":  errString(broadenedErr),
		"context_pack":     contextPackPayload,
		"context_status":   contextPackStatus,
		"context_error":    errString(contextPackErr),
	})
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
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

func writeSSEJSONEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: " + event + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: " + string(body) + "\n\n")); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (s *server) continuationEvents(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Method, http.MethodGet) {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	token := continuationEventsToken(r.URL.Path)
	if token == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "continuation stream not found"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	updates := make(chan map[string]any, s.retrieval.continuationEventHistory)
	history, exists := s.registerContinuationSubscriber(token, updates)
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "continuation stream expired or unknown"})
		return
	}
	defer s.unregisterContinuationSubscriber(token, updates)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for _, item := range history {
		if err := writeSSEJSONEvent(w, flusher, "snapshot", item); err != nil {
			return
		}
	}
	_ = writeSSEJSONEvent(w, flusher, "ready", map[string]any{
		"token": token,
		"at":    nowUTCISO(),
	})

	heartbeat := time.NewTicker(s.retrieval.continuationSSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if err := writeSSEJSONEvent(w, flusher, "heartbeat", map[string]any{
				"token": token,
				"at":    nowUTCISO(),
			}); err != nil {
				return
			}
		case event := <-updates:
			if err := writeSSEJSONEvent(w, flusher, "update", event); err != nil {
				return
			}
		}
	}
}

func (s *server) backendHealthy(ctx context.Context) bool {
	if s.strictNoPythonRuntime {
		return false
	}
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
		"ok":                    true,
		"service":               "gateway-go",
		"backendUrl":            s.backendURL,
		"backendHealth":         s.backendHealthy(ctx),
		"strictNoPythonRuntime": s.strictNoPythonRuntime,
	})
}

func serviceRow(name string, status string, owner string, detail string) map[string]any {
	normalizedStatus := strings.TrimSpace(strings.ToLower(status))
	healthy := normalizedStatus == "healthy"
	row := map[string]any{
		"name":    name,
		"status":  status,
		"healthy": healthy,
		"owner":   owner,
	}
	if token := strings.TrimSpace(detail); token != "" {
		row["detail"] = token
	}
	return row
}

func orderedSourceUnion(primary []string, extras ...[]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(primary))
	push := func(source string) {
		normalized := strings.TrimSpace(strings.ToLower(source))
		if normalized == "" {
			return
		}
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	for _, source := range primary {
		push(source)
	}
	for _, group := range extras {
		for _, source := range group {
			push(source)
		}
	}
	return out
}

func (s *server) strictRuntimeLaneStatus(source string) (string, string, string) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	owner := sourceOwnerForSource(normalized)
	if owner == sourceOwnerPythonBackendFallback && s.strictNoPythonRuntime {
		return "degraded", owner, "lane currently requires python fallback, which is disabled in strict runtime"
	}
	switch normalized {
	case sourceQdrant:
		if !nativeSourceAdapterEnabled(sourceQdrant, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		if strings.TrimSpace(nativeQdrantURL()) == "" {
			return "degraded", sourceOwnerGoNative, "qdrant URL is not configured"
		}
		return "healthy", sourceOwnerGoNative, "native adapter enabled"
	case sourceWeaviate:
		if !envBool("ORCH_WEAVIATE_ENABLED", false) {
			return "disabled", sourceOwnerGoNative, "ORCH_WEAVIATE_ENABLED=false"
		}
		if !nativeSourceAdapterEnabled(sourceWeaviate, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		if strings.TrimSpace(nativeWeaviateURL()) == "" {
			return "degraded", sourceOwnerGoNative, "weaviate URL is not configured"
		}
		return "healthy", sourceOwnerGoNative, "native adapter enabled"
	case sourcePgvector:
		if !nativeSourceAdapterEnabled(sourcePgvector, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		if !nativePgvectorEnabled() {
			return "disabled", sourceOwnerGoNative, "ORCH_PGVECTOR_ENABLED=false"
		}
		if strings.TrimSpace(nativePgvectorDSN()) == "" {
			return "degraded", sourceOwnerGoNative, "pgvector DSN is not configured"
		}
		return "healthy", sourceOwnerGoNative, "native adapter enabled"
	case sourceTopicRollup:
		if s.memoryStore != nil && s.memoryStore.policy.enabled {
			return "healthy", sourceOwnerGoNative, "served from go memory store"
		}
		return "degraded", sourceOwnerGoNative, "memory store policy is disabled"
	case sourceLetta:
		if !nativeSourceAdapterEnabled(sourceLetta, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		if strings.TrimSpace(s.letta.url) == "" {
			return "degraded", sourceOwnerGoNative, "LETTA_URL is not configured"
		}
		return "healthy", sourceOwnerGoNative, "native adapter enabled"
	case sourceMemoryBank:
		if !nativeSourceAdapterEnabled(sourceMemoryBank, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		policy := defaultRustBackendPolicy()
		backend := strings.TrimSpace(strings.ToLower(anyToString(policy["memory_bank_backend"])))
		if backend == "disabled" {
			return "disabled", sourceOwnerGoNative, "memory bank backend is disabled"
		}
		if backend == "" {
			backend = "shodh_spike"
		}
		return "healthy", sourceOwnerGoNative, "backend=" + backend
	case sourceMongoRaw:
		if !nativeSourceAdapterEnabled(sourceMongoRaw, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		if s.telemetrySink != nil && s.telemetrySink.enabled {
			return "healthy", sourceOwnerGoNative, "native mongo telemetry collection adapter enabled"
		}
		if s.telemetrySpool != nil && s.telemetrySpool.enabled {
			return "healthy", sourceOwnerGoNative, "native telemetry spool fallback adapter enabled"
		}
		return "degraded", sourceOwnerGoNative, "telemetry sink and spool adapters are unavailable"
	case sourceMindsdb:
		if !nativeSourceAdapterEnabled(sourceMindsdb, true) {
			return "disabled", sourceOwnerGoNative, "native adapter disabled by config"
		}
		if !nativeMindsdbEnabled() {
			return "disabled", sourceOwnerGoNative, "MINDSDB_ENABLED=false"
		}
		if strings.TrimSpace(nativeMindsdbSQLURL()) == "" {
			return "degraded", sourceOwnerGoNative, "mindsdb SQL endpoint is not configured"
		}
		return "healthy", sourceOwnerGoNative, "native SQL adapter enabled"
	default:
		return "degraded", owner, "lane state unknown"
	}
}

func (s *server) strictRuntimeServices() []map[string]any {
	rows := []map[string]any{
		serviceRow("gateway-go", "healthy", sourceOwnerGoNative, "HTTP orchestrator gateway"),
		serviceRow("memory-store", func() string {
			if s.memoryStore != nil && s.memoryStore.policy.enabled {
				return "healthy"
			}
			return "degraded"
		}(), sourceOwnerGoNative, "topic rollups + local graph store"),
	}

	memoryBankStatus, memoryBankOwner, memoryBankDetail := s.strictRuntimeLaneStatus(sourceMemoryBank)
	rows = append(rows, serviceRow("memory-bank-spike-rs", memoryBankStatus, memoryBankOwner, memoryBankDetail))

	laneSources := orderedSourceUnion(
		s.retrieval.defaultSources,
		s.retrieval.fastSources,
		s.retrieval.slowSources,
		s.retrieval.syncFallbackSources,
	)
	if len(laneSources) == 0 {
		laneSources = append([]string{}, defaultAllSources...)
	}
	for _, source := range laneSources {
		status, owner, detail := s.strictRuntimeLaneStatus(source)
		if strings.TrimSpace(strings.ToLower(status)) != "healthy" {
			continue
		}
		rows = append(rows, serviceRow("retrieval/"+source, status, owner, detail))
	}
	return rows
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	if s.strictNoPythonRuntime {
		continuation := s.continuationQueueSnapshot()
		queueDepth := continuation.Pending
		queueDepthTotal := continuation.PendingTotal
		queueBySource := continuation.BySource
		cooldownActive := continuation.CooldownActive
		syncQueue := s.syncQueueSnapshot()
		queueMax := cap(s.continuationSem)
		if queueMax < 1 {
			queueMax = 1
		}
		queueRatio := float64(queueDepth) / float64(queueMax)
		services := s.strictRuntimeServices()
		healthyServiceCount := 0
		for _, row := range services {
			if anyToBool(row["healthy"]) {
				healthyServiceCount++
			}
		}
		warnings := []string{
			"Python backend forwarding is disabled by strict runtime policy; all active lanes run through Go/Rust services.",
		}
		if continuation.OldestAgeSecs >= s.retrieval.syncQueueAgeWarnSecs && continuation.OldestAgeSecs > 0 {
			warnings = append(
				warnings,
				"Continuation queue age is elevated; staged async warming is under pressure.",
			)
		}
		if anyToFloat64(syncQueue["oldest_age_secs"], 0) >= s.retrieval.syncQueueAgeWarnSecs {
			warnings = append(
				warnings,
				"Sync source queue age crossed warn threshold; consider reducing deep fanout or tuning per-source sync caps.",
			)
		}
		storagePolicy := loadStorageGovernancePolicy()
		storagePressure := "unknown"
		storageDisk := map[string]any{"root": storagePolicy.diskRoot}
		if disk, err := diskUsageSnapshot(storagePolicy.diskRoot); err == nil {
			storageDisk = disk
			storagePressure = pressureBand(
				anyToFloat64(disk["usedRatio"], 0.0),
				uint64(anyToInt64(disk["freeBytes"], 0)),
				storagePolicy,
			)
			if storagePressure == "warn" || storagePressure == "high" {
				warnings = append(
					warnings,
					"Storage pressure is "+storagePressure+" on configured disk root; run maintenance/compaction before risk increases.",
				)
			}
		}
		payload := map[string]any{
			"ok":                            true,
			"statusSource":                  "gateway-go",
			"backendStatusSource":           "disabled_by_strict_runtime",
			"routeOwnerClass":               sourceOwnerGoNative,
			"pythonHotPathOwnership":        s.pythonHotPathOwnershipSnapshot(),
			"gatewayPythonHotPathOwnership": s.pythonHotPathOwnershipSnapshot(),
			"backendPythonHotPathOwnership": map[string]any{"status": "disabled_by_strict_runtime", "fallbacks": 0},
			"strictNoPythonRuntime":         true,
			"sourceOwnershipMode":           s.retrieval.sourceOwnershipMode,
			"services":                      services,
			"serviceHealth": map[string]any{
				"healthy": healthyServiceCount,
				"total":   len(services),
			},
			"runtimeBackendPolicy":    defaultRustBackendPolicy(),
			"retrievalFastSources":    append([]string{}, s.retrieval.fastSources...),
			"retrievalSlowSources":    append([]string{}, s.retrieval.slowSources...),
			"retrievalDefaultSources": append([]string{}, s.retrieval.defaultSources...),
			"queue": map[string]any{
				"pending":               queueDepth,
				"pendingTotal":          queueDepthTotal,
				"memoryWriteQueueDepth": queueDepth,
				"memoryWriteQueueMax":   queueMax,
				"memoryWriteQueueRatio": queueRatio,
				"bySource":              queueBySource,
				"cooldownActive":        cooldownActive,
				"retrying":              continuation.RetryingCount,
				"retryingBySource":      continuation.RetryingBySrc,
				"oldestAgeSecs":         continuation.OldestAgeSecs,
				"durablePending":        continuation.DurablePending,
				"durableBySource":       continuation.DurableBySrc,
				"durableOldestAgeSecs":  continuation.DurableOldest,
				"syncLane":              syncQueue,
				"policy": map[string]any{
					"syncSourceConcurrencyDefault":    s.retrieval.syncSourceConcurrencyDefault,
					"syncSourceConcurrencyOverrides":  cloneIntMap(s.retrieval.syncSourceConcurrencyOverrides),
					"syncQueueAgeWarnSecs":            s.retrieval.syncQueueAgeWarnSecs,
					"syncQueueAgeHighSecs":            s.retrieval.syncQueueAgeHighSecs,
					"continuationSheddingEnabled":     s.retrieval.continuationSheddingEnabled,
					"continuationSheddingQueueRatio":  s.retrieval.continuationSheddingQueueRatio,
					"continuationSheddingPendingHigh": s.retrieval.continuationSheddingPendingHigh,
				},
			},
			"timeoutContract": s.timeoutContractSnapshot(),
			"drift":           s.driftSnapshot(),
			"storageGovernance": map[string]any{
				"diskRoot":      storagePolicy.diskRoot,
				"warnUsedRatio": storagePolicy.warnUsedRatio,
				"highUsedRatio": storagePolicy.highUsedRatio,
				"minFreeBytes":  storagePolicy.minFreeBytes,
				"pressureBand":  storagePressure,
				"disk":          storageDisk,
			},
			"warnings":         warnings,
			"metadataContract": metadataContractSnapshot(),
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	backendPath := "/status"
	if raw := strings.TrimSpace(r.URL.RawQuery); raw != "" {
		backendPath += "?" + raw
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	payload, statusCode, err := s.backendJSONRequest(ctx, http.MethodGet, backendPath, incomingHeaders, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":                     false,
			"error":                  "backend_status_unavailable",
			"detail":                 err.Error(),
			"backendUrl":             s.backendURL,
			"statusSource":           "gateway-go",
			"pythonHotPathOwnership": s.pythonHotPathOwnershipSnapshot(),
		})
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	gatewayOwnership := s.pythonHotPathOwnershipSnapshot()
	backendOwnership, backendOwnershipExists := payload["pythonHotPathOwnership"]
	if backendOwnershipExists {
		payload["backendPythonHotPathOwnership"] = backendOwnership
	}
	payload["pythonHotPathOwnership"] = gatewayOwnership
	payload["gatewayPythonHotPathOwnership"] = gatewayOwnership
	payload["statusSource"] = "gateway-go"
	payload["backendStatusSource"] = "contextlattice-orchestrator"
	payload["routeOwnerClass"] = sourceOwnerGoNative
	payload["fallbackCounts"] = map[string]any{
		"pythonHotPathTotal": anyToInt(gatewayOwnership["fallbacks"], 0),
	}
	payload["sourceOwnershipMode"] = s.retrieval.sourceOwnershipMode
	payload["metadataContract"] = metadataContractSnapshot()

	warnings := parseWarnings(payload["warnings"])
	if backendMap, ok := backendOwnership.(map[string]any); ok {
		backendStatus := strings.TrimSpace(strings.ToLower(anyToString(backendMap["status"])))
		backendNonGateway := anyToInt(backendMap["nonGatewayRequests"], 0)
		gatewayFallbacks := anyToInt(gatewayOwnership["fallbacks"], 0)
		if backendStatus == "non_gateway_hot_path_traffic_detected" && backendNonGateway > 0 && gatewayFallbacks == 0 {
			warnings = append(
				warnings,
				"Backend non-gateway counters indicate direct calls to python orchestrator; gateway fallback counters are authoritative at pythonHotPathOwnership.fallbacks.",
			)
		}
	}
	if len(warnings) > 0 {
		payload["warnings"] = dedupeWarnings(warnings)
	}
	writeJSON(w, statusCode, payload)
}

func (s *server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"description":           "ContextLattice gateway-go retrieval/memory proxy",
		"backendUrl":            s.backendURL,
		"strictNoPythonRuntime": s.strictNoPythonRuntime,
		"memoryStore": map[string]any{
			"enabled": func() bool {
				return s.memoryStore != nil && s.memoryStore.policy.enabled
			}(),
			"rootPath": func() string {
				if s.memoryStore == nil {
					return ""
				}
				return s.memoryStore.policy.rootPath
			}(),
		},
		"retrieval": map[string]any{
			"stagedEnabled":              s.retrieval.enabled,
			"fastSources":                s.retrieval.fastSources,
			"slowSources":                s.retrieval.slowSources,
			"syncFallbackSources":        s.retrieval.syncFallbackSources,
			"minFastResults":             s.retrieval.minFastResults,
			"minFastResultsByMode":       s.retrieval.minFastResultsByMode,
			"disableSyncSlowFallback":    s.retrieval.disableSyncSlowFallback,
			"slowSyncTimeoutCapSecs":     s.retrieval.slowSyncTimeoutCap.Seconds(),
			"rustLanePromotionEnabled":   s.retrieval.rustLanePromotionEnabled,
			"topicPrefilterEnabled":      s.retrieval.topicPrefilterEnabled,
			"coverageRescueEnabled":      s.retrieval.coverageRescueEnabled,
			"coverageRescueMinTokens":    s.retrieval.coverageRescueMinTokens,
			"rustQualityFallbackEnabled": s.retrieval.rustQualityFallbackEnabled,
			"rustQualityFallbackSources": s.retrieval.rustQualityFallbackSources,
			"rustQualityFallbackMode":    s.retrieval.rustQualityFallbackMode,
			"qdrantSyncTimeoutCapSecs":   s.retrieval.qdrantSyncTimeoutCap.Seconds(),
			"qdrantSyncTimeoutCapByModeSecs": map[string]any{
				"fast":     s.retrieval.qdrantSyncTimeoutCapByMode["fast"].Seconds(),
				"balanced": s.retrieval.qdrantSyncTimeoutCapByMode["balanced"].Seconds(),
				"deep":     s.retrieval.qdrantSyncTimeoutCapByMode["deep"].Seconds(),
			},
			"topicRollupSyncTimeoutFloorSecs": s.retrieval.topicRollupSyncTimeoutFloor.Seconds(),
			"topicRollupSyncTimeoutFloorByModeSecs": map[string]any{
				"fast":     s.resolveTopicRollupSyncTimeoutFloor("fast").Seconds(),
				"balanced": s.resolveTopicRollupSyncTimeoutFloor("balanced").Seconds(),
				"deep":     s.resolveTopicRollupSyncTimeoutFloor("deep").Seconds(),
			},
			"failOpenContinuationEnabled":      s.retrieval.failOpenContinuationEnabled,
			"continuationEventHistory":         s.retrieval.continuationEventHistory,
			"continuationEventTTLSecs":         s.retrieval.continuationEventTTL.Seconds(),
			"continuationSSEHeartbeatSecs":     s.retrieval.continuationSSEHeartbeat.Seconds(),
			"timeoutAdaptiveSkipEnabled":       s.retrieval.timeoutAdaptiveSkipEnabled,
			"adaptiveTimeoutEnabled":           s.retrieval.adaptiveTimeoutEnabled,
			"adaptiveTimeoutMinRequests":       s.retrieval.adaptiveTimeoutMinRequests,
			"adaptiveTimeoutWindow":            s.retrieval.adaptiveTimeoutWindow,
			"adaptiveTimeoutP95Factor":         s.retrieval.adaptiveTimeoutP95Factor,
			"adaptiveTimeoutMinScale":          s.retrieval.adaptiveTimeoutMinScale,
			"adaptiveTimeoutMaxScale":          s.retrieval.adaptiveTimeoutMaxScale,
			"adaptiveTimeoutBacklogWeight":     s.retrieval.adaptiveTimeoutBacklogWeight,
			"adaptiveTimeoutBacklogCap":        s.retrieval.adaptiveTimeoutBacklogCap,
			"lexicalGuardEnabled":              s.retrieval.lexicalGuardEnabled,
			"lexicalGuardMinCoverage":          s.retrieval.lexicalGuardMinCoverage,
			"lexicalGuardMinResults":           s.retrieval.lexicalGuardMinResults,
			"continuationMaxInflight":          s.retrieval.continuationMaxInflight,
			"continuationMaxInflightPerSource": s.retrieval.continuationMaxInflightPerSource,
			"continuationMaxInflightOverrides": cloneIntMap(s.retrieval.continuationMaxInflightOverrides),
			"continuationSourceCooldownSecs":   s.retrieval.continuationSourceCooldown.Seconds(),
			"continuationSourceCooldownBySrc":  durationMapToSeconds(s.retrieval.continuationSourceCooldownBySrc),
			"continuationSourceCooldownActive": s.continuationSourceCooldownSnapshot(),
			"continuationSheddingEnabled":      s.retrieval.continuationSheddingEnabled,
			"continuationSheddingQueueRatio":   s.retrieval.continuationSheddingQueueRatio,
			"continuationSheddingPendingHigh":  s.retrieval.continuationSheddingPendingHigh,
			"continuationSheddingSources":      mapKeysSorted(s.retrieval.continuationSheddingSources),
			"syncSourceConcurrencyDefault":     s.retrieval.syncSourceConcurrencyDefault,
			"syncSourceConcurrencyOverrides":   cloneIntMap(s.retrieval.syncSourceConcurrencyOverrides),
			"syncQueueAgeWarnSecs":             s.retrieval.syncQueueAgeWarnSecs,
			"syncQueueAgeHighSecs":             s.retrieval.syncQueueAgeHighSecs,
			"timeoutContractGraceSecs":         s.retrieval.timeoutContractGrace.Seconds(),
			"subcallDisableExpansion":          s.retrieval.subcallDisableExpansion,
			"subcallDisableAutoEscalate":       s.retrieval.subcallDisableAutoEscalate,
			"telemetryBatchEnabled":            s.retrieval.telemetryBatchEnabled,
			"telemetryBatchFlushIntervalSecs":  s.retrieval.telemetryBatchFlushInterval.Seconds(),
			"sourceOwnershipMode":              s.retrieval.sourceOwnershipMode,
			"sourceOwnershipStrictFastAllowPy": mapKeysSorted(s.retrieval.sourceOwnershipStrictFastAllowPy),
			"sourceOwnersKnownNative": map[string]any{
				sourceQdrant:      sourceOwnerGoNative,
				sourceWeaviate:    sourceOwnerGoNative,
				sourcePgvector:    sourceOwnerGoNative,
				sourceTopicRollup: sourceOwnerGoNative,
				sourceLetta:       sourceOwnerGoNative,
			},
			"routeOwnerClass": sourceOwnerGoNative,
		},
		"pythonHotPathOwnership": s.pythonHotPathOwnershipSnapshot(),
		"timeoutContract":        s.timeoutContractSnapshot(),
		"drift":                  s.driftSnapshot(),
		"queueLanes": map[string]any{
			"sync":         s.syncQueueSnapshot(),
			"continuation": s.continuationQueueSnapshot(),
		},
		"writeIngress": map[string]any{
			"enabled":                   s.writePolicy.enabled,
			"strictRequiredFields":      s.writePolicy.strictRequiredFields,
			"telemetryIsolationEnabled": s.writePolicy.telemetryIsolationEnabled,
			"batchConcurrency":          s.writePolicy.batchConcurrency,
			"fanoutExcludeTargets":      s.writePolicy.fanoutExcludeTargets,
			"telemetrySpool":            s.telemetrySpool.snapshot(),
			"telemetryRing":             s.telemetryRing.snapshot(),
		},
		"toolCalls": map[string]any{
			"allowAll":            s.toolCalls.allowAll,
			"requireAPIKey":       s.toolCalls.requireAPIKey,
			"enforceProvidedKey":  s.toolCalls.enforceProvidedKey,
			"allowlist":           mapKeysSorted(s.toolCalls.allowlist),
			"denylist":            mapKeysSorted(s.toolCalls.denylist),
			"roleSplitEnabled":    s.toolCalls.roleSplitEnabled,
			"roleSplitAuto":       s.toolCalls.roleSplitAuto,
			"workerKeyConfigured": strings.TrimSpace(s.toolCalls.workerKey) != "",
			"workerRole": map[string]any{
				"allowAll":  s.toolCalls.workerRole.allowAll,
				"allowlist": mapKeysSorted(s.toolCalls.workerRole.allowlist),
				"denylist":  mapKeysSorted(s.toolCalls.workerRole.denylist),
			},
			"orchestratorRole": map[string]any{
				"allowAll":  s.toolCalls.orchestratorRole.allowAll,
				"allowlist": mapKeysSorted(s.toolCalls.orchestratorRole.allowlist),
				"denylist":  mapKeysSorted(s.toolCalls.orchestratorRole.denylist),
			},
		},
	})
}

func mapKeysSorted(rows map[string]struct{}) []string {
	out := make([]string, 0, len(rows))
	for key := range rows {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
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

func cloneIntMap(input map[string]int) map[string]int {
	out := make(map[string]int, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func durationMapToSeconds(input map[string]time.Duration) map[string]float64 {
	out := make(map[string]float64, len(input))
	for key, value := range input {
		seconds := value.Seconds()
		if seconds < 0 {
			seconds = 0
		}
		out[key] = roundFloat(seconds, 3)
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
	case uint:
		return int(typed)
	case int64:
		return int(typed)
	case uint64:
		return int(typed)
	case int32:
		return int(typed)
	case uint32:
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

func parseJSONArray(value []byte) ([]any, error) {
	var payload []any
	if err := json.Unmarshal(value, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func queryTerms(query string, maxTerms int) []string {
	normalized := strings.ToLower(strings.TrimSpace(query))
	if normalized == "" || maxTerms < 1 {
		return nil
	}
	matches := queryTermPattern.FindAllString(normalized, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	terms := make([]string, 0, maxTerms)
	for _, token := range matches {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
		if len(terms) >= maxTerms {
			break
		}
	}
	return terms
}

func textMatchScore(query string, text string) float64 {
	queryText := strings.ToLower(strings.TrimSpace(query))
	body := strings.ToLower(text)
	if queryText == "" || strings.TrimSpace(body) == "" {
		return 0
	}
	if strings.Contains(body, queryText) {
		return 1.0
	}
	terms := queryTerms(queryText, 10)
	if len(terms) == 0 {
		return 0
	}
	hits := 0
	for _, term := range terms {
		if strings.Contains(body, term) {
			hits += 1
		}
	}
	if hits <= 0 {
		return 0
	}
	density := float64(len(body)) / 4000.0
	if density > 1.0 {
		density = 1.0
	}
	score := (float64(hits) / float64(len(terms))) * (0.55 + 0.45*density)
	if score > 0.95 {
		return 0.95
	}
	return score
}

func parseLettaArchivalContent(text string) map[string]string {
	project := ""
	fileName := ""
	topicPath := ""
	summary := ""
	lines := strings.Split(text, "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if candidate != "" {
			trimmed = append(trimmed, candidate)
		}
	}
	if len(trimmed) > 0 {
		header := trimmed[0]
		for _, match := range lettaHeaderPattern.FindAllStringSubmatch(header, -1) {
			if len(match) < 3 {
				continue
			}
			key := strings.TrimSpace(strings.ToLower(match[1]))
			value := strings.TrimSpace(match[2])
			switch key {
			case "project":
				project = value
			case "file":
				fileName = value
			case "topic":
				topicPath = value
			}
		}
	}
	for _, line := range trimmed[1:] {
		if strings.HasPrefix(strings.ToLower(line), "summary:") {
			summary = strings.TrimSpace(strings.TrimPrefix(line, "summary:"))
			if summary == line {
				summary = strings.TrimSpace(strings.TrimPrefix(line, "Summary:"))
			}
			break
		}
	}
	if summary == "" {
		summary = strings.TrimSpace(text)
		if len(summary) > 500 {
			summary = summary[:500]
		}
	}
	normalizeValue := func(value string) string {
		candidate := strings.TrimSpace(value)
		if candidate == "-" {
			return ""
		}
		return candidate
	}
	return map[string]string{
		"project":    normalizeValue(project),
		"file":       normalizeValue(fileName),
		"topic_path": normalizeValue(topicPath),
		"summary":    summary,
	}
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

func normalizeRetrievalMode(mode string) string {
	normalized := strings.TrimSpace(strings.ToLower(mode))
	switch normalized {
	case "fast", "balanced", "deep":
		return normalized
	default:
		return "balanced"
	}
}

func (s *server) resolveQdrantSyncCap(mode string) time.Duration {
	normalized := normalizeRetrievalMode(mode)
	if timeout, ok := s.retrieval.qdrantSyncTimeoutCapByMode[normalized]; ok && timeout > 0 {
		return timeout
	}
	return s.retrieval.qdrantSyncTimeoutCap
}

func (s *server) resolveTopicRollupSyncTimeoutFloor(mode string) time.Duration {
	normalized := normalizeRetrievalMode(mode)
	if timeout, ok := s.retrieval.topicRollupSyncTimeoutFloorByMode[normalized]; ok && timeout > 0 {
		return timeout
	}
	if s.retrieval.topicRollupSyncTimeoutFloor > 0 {
		return s.retrieval.topicRollupSyncTimeoutFloor
	}
	return 0
}

func (s *server) resolveMinFastResults(mode string) int {
	normalized := normalizeRetrievalMode(mode)
	if value, ok := s.retrieval.minFastResultsByMode[normalized]; ok && value > 0 {
		return value
	}
	if s.retrieval.minFastResults > 0 {
		return s.retrieval.minFastResults
	}
	return 1
}

func (s *server) lettaTopKForMode(mode string, limit int) int {
	requested := limit
	if requested < 1 {
		requested = 1
	}
	normalizedMode := normalizeRetrievalMode(mode)
	factor := s.retrieval.lettaTopKFactor
	if modeFactor, ok := s.retrieval.lettaTopKFactorByMode[normalizedMode]; ok && modeFactor > 0 {
		factor = modeFactor
	}
	if factor < 1.0 {
		factor = 1.0
	}
	capLimit := s.retrieval.lettaTopKCap
	if modeCap, ok := s.retrieval.lettaTopKCapByMode[normalizedMode]; ok && modeCap > 0 {
		capLimit = modeCap
	}
	if capLimit < requested {
		capLimit = requested
	}
	scaledExact := float64(requested) * factor
	scaled := int(scaledExact)
	if float64(scaled) < scaledExact {
		scaled += 1
	}
	if scaled < requested {
		scaled = requested
	}
	if capLimit > 0 && scaled > capLimit {
		scaled = capLimit
	}
	return scaled
}

func normalizeTopicPathCandidate(value string) string {
	segments := strings.Split(value, "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		candidate := strings.TrimSpace(strings.ToLower(segment))
		candidate = strings.Trim(candidate, "[](){}\"'`.,:;")
		if candidate == "" {
			continue
		}
		if strings.Contains(candidate, "http") || strings.Contains(candidate, ".") {
			return ""
		}
		valid := true
		for _, r := range candidate {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
				continue
			}
			valid = false
			break
		}
		if !valid {
			return ""
		}
		cleaned = append(cleaned, candidate)
	}
	if len(cleaned) < 2 {
		return ""
	}
	if len(cleaned) > 6 {
		cleaned = cleaned[:6]
	}
	return strings.Join(cleaned, "/")
}

func inferTopicPathFromQuery(query string) string {
	for _, token := range strings.Fields(query) {
		if strings.Count(token, "/") < 1 {
			continue
		}
		if strings.Contains(token, "://") {
			continue
		}
		if normalized := normalizeTopicPathCandidate(token); normalized != "" {
			return normalized
		}
	}
	return ""
}

func shouldDropCoverageToken(token string) bool {
	normalized := strings.TrimSpace(strings.ToLower(token))
	if normalized == "" {
		return true
	}
	if strings.HasPrefix(normalized, "profile=") ||
		strings.HasPrefix(normalized, "case=") ||
		strings.HasPrefix(normalized, "run=") ||
		strings.HasPrefix(normalized, "nonce=") ||
		strings.HasPrefix(normalized, "ts=") ||
		strings.HasPrefix(normalized, "seed=") ||
		strings.HasPrefix(normalized, "cache=") ||
		strings.HasPrefix(normalized, "id=") {
		return true
	}
	if strings.Contains(normalized, "::") {
		return true
	}
	letters := 0
	digits := 0
	separators := 0
	for _, r := range normalized {
		switch {
		case unicode.IsLetter(r):
			letters += 1
		case unicode.IsDigit(r):
			digits += 1
		case r == '-' || r == '_' || r == '=' || r == ':':
			separators += 1
		}
	}
	if len(normalized) >= 12 && digits >= letters {
		return true
	}
	if separators >= 2 && digits > 0 && len(normalized) >= 10 {
		return true
	}
	return false
}

func deriveCoverageRescueQuery(query string, minTokens int) (string, bool) {
	tokens := strings.Fields(strings.TrimSpace(query))
	if len(tokens) < minTokens {
		return "", false
	}
	kept := make([]string, 0, len(tokens))
	removed := 0
	for _, token := range tokens {
		if shouldDropCoverageToken(token) {
			removed += 1
			continue
		}
		kept = append(kept, token)
	}
	if removed == 0 || len(kept) < minTokens {
		return "", false
	}
	normalized := strings.TrimSpace(strings.Join(kept, " "))
	if normalized == "" || normalized == strings.TrimSpace(query) {
		return "", false
	}
	return normalized, true
}

func (s *server) applyRustLanePromotionGate(policy map[string]any, trafficClass string) (map[string]any, bool) {
	resolved := cloneMap(policy)
	if s.retrieval.rustLanePromotionEnabled {
		return resolved, false
	}
	if strings.TrimSpace(strings.ToLower(trafficClass)) == "benchmark" {
		return resolved, false
	}
	vectorBackend := strings.TrimSpace(strings.ToLower(anyToString(resolved["vector_backend"])))
	strict := anyToBool(resolved["strict"])
	if vectorBackend != "usearch_ann" || !strict {
		return resolved, false
	}
	resolved["vector_backend"] = "qdrant_remote"
	resolved["strict"] = false
	return resolved, true
}

func (s *server) recordAdaptiveObservation(
	source string,
	latency time.Duration,
	timedOut bool,
	errored bool,
	budgetExceeded bool,
) {
	if !s.retrieval.adaptiveTimeoutEnabled {
		return
	}
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return
	}
	ms := float64(latency.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	s.adaptiveMu.Lock()
	defer s.adaptiveMu.Unlock()
	entry := s.adaptiveBySource[normalized]
	if entry == nil {
		entry = &adaptiveSourceStats{}
		s.adaptiveBySource[normalized] = entry
	}
	entry.requests += 1
	if timedOut {
		entry.timeouts += 1
	}
	if errored {
		entry.errors += 1
	}
	if budgetExceeded {
		entry.budgetExceeded += 1
	}
	entry.latencyMs = append(entry.latencyMs, ms)
	window := s.retrieval.adaptiveTimeoutWindow
	if window < 8 {
		window = 8
	}
	if len(entry.latencyMs) > window {
		entry.latencyMs = append([]float64(nil), entry.latencyMs[len(entry.latencyMs)-window:]...)
	}
}

func (s *server) continuationBacklog(source string) int {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return 0
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	return int(s.continuationInFlight[normalized])
}

func (s *server) incrementContinuationBacklog(source string) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	s.continuationInFlight[normalized] = int(s.continuationInFlight[normalized]) + 1
}

func (s *server) decrementContinuationBacklog(source string) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	current := int(s.continuationInFlight[normalized])
	if current <= 1 {
		delete(s.continuationInFlight, normalized)
		return
	}
	s.continuationInFlight[normalized] = current - 1
}

func (s *server) continuationSourceCooldownSnapshot() map[string]float64 {
	now := time.Now().UTC()
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	snapshot := map[string]float64{}
	for source, until := range s.continuationSourceCooldownUntil {
		if !until.After(now) {
			delete(s.continuationSourceCooldownUntil, source)
			continue
		}
		snapshot[source] = roundFloat(until.Sub(now).Seconds(), 3)
	}
	return snapshot
}

func (s *server) continuationMaxInflightForSource(source string) int {
	maxPerSource := s.retrieval.continuationMaxInflightPerSource
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized != "" {
		if override, ok := s.retrieval.continuationMaxInflightOverrides[normalized]; ok && override > 0 {
			maxPerSource = override
		}
	}
	if maxPerSource < 1 {
		maxPerSource = 1
	}
	if s.retrieval.continuationMaxInflight > 0 && maxPerSource > s.retrieval.continuationMaxInflight {
		maxPerSource = s.retrieval.continuationMaxInflight
	}
	return maxPerSource
}

func (s *server) continuationCooldownForSource(source string) time.Duration {
	cooldown := s.retrieval.continuationSourceCooldown
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized != "" {
		if override, ok := s.retrieval.continuationSourceCooldownBySrc[normalized]; ok {
			cooldown = override
		}
	}
	if cooldown < 0 {
		return 0
	}
	return cooldown
}

func (s *server) applyContinuationSourceCooldown(source string) float64 {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return 0
	}
	cooldown := s.continuationCooldownForSource(normalized)
	if cooldown <= 0 {
		return 0
	}
	now := time.Now().UTC()
	until := now.Add(cooldown)
	s.continuationMu.Lock()
	s.continuationSourceCooldownUntil[normalized] = until
	s.continuationMu.Unlock()
	return until.Sub(now).Seconds()
}

func (s *server) tryReserveContinuationSourceSlot(source string) (bool, string, float64) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return false, "invalid_source", 0
	}
	now := time.Now().UTC()
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	if until, ok := s.continuationSourceCooldownUntil[normalized]; ok {
		if until.After(now) {
			return false, "cooldown", until.Sub(now).Seconds()
		}
		delete(s.continuationSourceCooldownUntil, normalized)
	}
	maxPerSource := s.continuationMaxInflightForSource(normalized)
	cooldown := s.continuationCooldownForSource(normalized)
	current := int(s.continuationInFlight[normalized])
	if current >= maxPerSource {
		cooldownRemaining := 0.0
		if cooldown > 0 {
			until := now.Add(cooldown)
			s.continuationSourceCooldownUntil[normalized] = until
			cooldownRemaining = until.Sub(now).Seconds()
		}
		return false, "max_inflight_per_source", cooldownRemaining
	}
	s.continuationInFlight[normalized] = current + 1
	s.continuationInFlightStarted[normalized] = append(s.continuationInFlightStarted[normalized], now)
	return true, "", 0
}

func (s *server) releaseContinuationSourceSlot(source string) {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	current := int(s.continuationInFlight[normalized])
	if current <= 1 {
		delete(s.continuationInFlight, normalized)
		delete(s.continuationInFlightStarted, normalized)
		return
	}
	s.continuationInFlight[normalized] = current - 1
	queue := s.continuationInFlightStarted[normalized]
	if len(queue) <= 1 {
		delete(s.continuationInFlightStarted, normalized)
	} else {
		s.continuationInFlightStarted[normalized] = queue[1:]
	}
}

func (s *server) adaptiveTimeoutForSource(source string, base time.Duration) (time.Duration, map[string]any) {
	detail := map[string]any{
		"enabled":           s.retrieval.adaptiveTimeoutEnabled,
		"base_timeout_secs": roundFloat(base.Seconds(), 3),
		"adjusted":          false,
	}
	if !s.retrieval.adaptiveTimeoutEnabled {
		return base, detail
	}
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return base, detail
	}
	s.adaptiveMu.Lock()
	entry := s.adaptiveBySource[normalized]
	if entry == nil || entry.requests < s.retrieval.adaptiveTimeoutMinRequests || len(entry.latencyMs) == 0 {
		requests := 0
		if entry != nil {
			requests = entry.requests
		}
		s.adaptiveMu.Unlock()
		detail["reason"] = "insufficient_observations"
		detail["requests"] = requests
		detail["min_requests"] = s.retrieval.adaptiveTimeoutMinRequests
		detail["backlog_inflight"] = s.continuationBacklog(normalized)
		return base, detail
	}
	latencyCopy := append([]float64(nil), entry.latencyMs...)
	requests := entry.requests
	timeouts := entry.timeouts
	s.adaptiveMu.Unlock()
	sort.Float64s(latencyCopy)
	p95Ms := percentileFloat(latencyCopy, 0.95)
	timeoutRate := 0.0
	if requests > 0 {
		timeoutRate = float64(timeouts) / float64(requests)
	}

	baseSecs := base.Seconds()
	targetSecs := (p95Ms / 1000.0) * s.retrieval.adaptiveTimeoutP95Factor
	minSecs := baseSecs * s.retrieval.adaptiveTimeoutMinScale
	maxSecs := baseSecs * s.retrieval.adaptiveTimeoutMaxScale
	if targetSecs < minSecs {
		targetSecs = minSecs
	}
	if targetSecs > maxSecs {
		targetSecs = maxSecs
	}
	adjustedSecs := targetSecs

	backlog := s.continuationBacklog(normalized)
	if backlog > 0 {
		capped := backlog
		if capped > s.retrieval.adaptiveTimeoutBacklogCap {
			capped = s.retrieval.adaptiveTimeoutBacklogCap
		}
		penalty := 1.0 - (float64(capped) * s.retrieval.adaptiveTimeoutBacklogWeight)
		minPenalty := s.retrieval.adaptiveTimeoutMinScale
		if penalty < minPenalty {
			penalty = minPenalty
		}
		adjustedSecs = adjustedSecs * penalty
		if adjustedSecs < minSecs {
			adjustedSecs = minSecs
		}
	}

	if adjustedSecs < 0.5 {
		adjustedSecs = 0.5
	}
	adjusted := time.Duration(adjustedSecs * float64(time.Second))
	detail["adjusted"] = adjusted != base
	detail["p95_ms"] = roundFloat(p95Ms, 3)
	detail["requests"] = requests
	detail["timeouts"] = timeouts
	detail["timeout_rate"] = roundFloat(timeoutRate, 6)
	detail["p95_factor"] = roundFloat(s.retrieval.adaptiveTimeoutP95Factor, 3)
	detail["min_scale"] = roundFloat(s.retrieval.adaptiveTimeoutMinScale, 3)
	detail["max_scale"] = roundFloat(s.retrieval.adaptiveTimeoutMaxScale, 3)
	detail["backlog_inflight"] = backlog
	detail["backlog_weight"] = roundFloat(s.retrieval.adaptiveTimeoutBacklogWeight, 3)
	detail["adjusted_timeout_secs"] = roundFloat(adjusted.Seconds(), 3)
	return adjusted, detail
}

func (s *server) resolveSourceTimeout(
	source string,
	retrievalMode string,
	syncPhase bool,
	isSlowSource bool,
	blockingSlowSources bool,
) (time.Duration, map[string]any) {
	normalizedMode := normalizeRetrievalMode(retrievalMode)
	timeout, ok := s.retrieval.sourceTimeouts[source]
	if !ok || timeout <= 0 {
		timeout = 8 * time.Second
	}
	topicRollupFloor := s.resolveTopicRollupSyncTimeoutFloor(normalizedMode)
	if syncPhase && !blockingSlowSources && source == sourceQdrant {
		capDuration := s.resolveQdrantSyncCap(retrievalMode)
		if capDuration > 0 && timeout > capDuration {
			timeout = capDuration
		}
	}
	if syncPhase && !blockingSlowSources && isSlowSource && s.retrieval.slowSyncTimeoutCap > 0 && timeout > s.retrieval.slowSyncTimeoutCap {
		timeout = s.retrieval.slowSyncTimeoutCap
	}
	detail := map[string]any{
		"enabled":               false,
		"base_timeout_secs":     roundFloat(timeout.Seconds(), 3),
		"adjusted":              false,
		"adjusted_timeout_secs": roundFloat(timeout.Seconds(), 3),
	}
	if syncPhase && !blockingSlowSources {
		adjusted, adaptive := s.adaptiveTimeoutForSource(source, timeout)
		if source == sourceTopicRollup && topicRollupFloor > 0 && adjusted < topicRollupFloor {
			adjusted = topicRollupFloor
			adaptive["adjusted"] = true
			adaptive["adjusted_timeout_secs"] = roundFloat(adjusted.Seconds(), 3)
			adaptive["topic_rollup_timeout_floor_secs"] = roundFloat(topicRollupFloor.Seconds(), 3)
			adaptive["topic_rollup_timeout_floor_applied"] = true
			adaptive["topic_rollup_timeout_floor_mode"] = normalizedMode
		}
		return adjusted, adaptive
	}
	if source == sourceTopicRollup && topicRollupFloor > 0 && timeout < topicRollupFloor {
		timeout = topicRollupFloor
		detail["adjusted"] = true
		detail["adjusted_timeout_secs"] = roundFloat(timeout.Seconds(), 3)
		detail["topic_rollup_timeout_floor_secs"] = roundFloat(topicRollupFloor.Seconds(), 3)
		detail["topic_rollup_timeout_floor_applied"] = true
		detail["topic_rollup_timeout_floor_mode"] = normalizedMode
	}
	return timeout, detail
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

func (s *server) isNonDegradableSource(source string) bool {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return false
	}
	_, ok := s.retrieval.nonDegradableSources[normalized]
	return ok
}

func (s *server) isProtectedSource(source string) bool {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return false
	}
	_, ok := s.retrieval.protectedSources[normalized]
	return ok
}

func (s *server) shouldAdaptiveSkip(source string) bool {
	if !s.retrieval.timeoutAdaptiveSkipEnabled {
		return false
	}
	_, ok := s.retrieval.timeoutAdaptiveSkipSources[source]
	return ok
}

func (s *server) lettaConfigEnabled() bool {
	if strings.TrimSpace(s.letta.autoSessionID) == "" {
		return false
	}
	if s.letta.requireAPIKey && strings.TrimSpace(s.letta.apiKey) == "" {
		return false
	}
	return strings.TrimSpace(s.letta.url) != ""
}

func capContextTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= timeout {
			return ctx, func() {}
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *server) doLettaRequest(
	ctx context.Context,
	method string,
	path string,
	queryParams url.Values,
	body any,
) (int, []byte, error) {
	if strings.TrimSpace(s.letta.url) == "" {
		return 0, nil, errors.New("letta url not configured")
	}
	fullURL := strings.TrimRight(s.letta.url, "/") + path
	if len(queryParams) > 0 {
		fullURL = fullURL + "?" + queryParams.Encode()
	}
	var reader io.Reader
	if body != nil {
		payloadBytes, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(payloadBytes)
	}
	requestCtx, cancel := capContextTimeout(ctx, s.letta.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, fullURL, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := strings.TrimSpace(s.letta.apiKey); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}

func parseLettaAgentIDFromList(payload []byte) string {
	rows, err := parseJSONArray(payload)
	if err != nil || len(rows) == 0 {
		return ""
	}
	first, ok := rows[0].(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(anyToString(first["id"]))
}

func parseLettaAgentIDFromObject(payload []byte) string {
	obj, err := parseJSONMap(payload)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(anyToString(obj["id"]))
}

func parseLettaResponseError(statusCode int, payload []byte) error {
	body := strings.TrimSpace(string(payload))
	if len(body) > 300 {
		body = body[:300]
	}
	return fmt.Errorf("Letta request failed: status=%d body=%s", statusCode, body)
}

func (s *server) resolveLettaAgentID(ctx context.Context) (string, error) {
	sessionID := strings.TrimSpace(s.letta.autoSessionID)
	if strings.HasPrefix(sessionID, "agent-") {
		return sessionID, nil
	}
	now := time.Now().UTC()
	s.lettaAgentMu.Lock()
	cached := strings.TrimSpace(s.lettaAgentBySession[sessionID])
	verifiedAt := s.lettaAgentVerifiedAt[sessionID]
	if cached != "" && now.Sub(verifiedAt) < s.letta.verifyInterval {
		s.lettaAgentMu.Unlock()
		return cached, nil
	}
	s.lettaAgentMu.Unlock()

	verifyAgent := func(agentID string) (string, error) {
		if strings.TrimSpace(agentID) == "" {
			return "", errors.New("letta agent id is empty")
		}
		statusCode, payload, err := s.doLettaRequest(ctx, http.MethodGet, "/v1/agents/"+agentID, nil, nil)
		if err != nil {
			return "", err
		}
		if statusCode >= 400 {
			return "", parseLettaResponseError(statusCode, payload)
		}
		s.lettaAgentMu.Lock()
		s.lettaAgentBySession[sessionID] = agentID
		s.lettaAgentVerifiedAt[sessionID] = time.Now().UTC()
		s.lettaAgentMu.Unlock()
		return agentID, nil
	}

	if cached != "" {
		agentID, err := verifyAgent(cached)
		if err == nil {
			return agentID, nil
		}
	}

	lookupParams := url.Values{}
	lookupParams.Set("name", sessionID)
	statusCode, payload, err := s.doLettaRequest(ctx, http.MethodGet, "/v1/agents/", lookupParams, nil)
	if err != nil {
		return "", err
	}
	if statusCode >= 400 {
		return "", parseLettaResponseError(statusCode, payload)
	}
	if agentID := parseLettaAgentIDFromList(payload); agentID != "" {
		return verifyAgent(agentID)
	}

	createPayload := map[string]any{"name": sessionID}
	if model := strings.TrimSpace(s.letta.agentModel); model != "" {
		createPayload["model"] = model
	}
	if embedding := strings.TrimSpace(s.letta.agentEmbedding); embedding != "" {
		createPayload["embedding"] = embedding
	}
	statusCode, payload, err = s.doLettaRequest(ctx, http.MethodPost, "/v1/agents/", nil, createPayload)
	if err != nil {
		return "", err
	}
	if statusCode >= 400 {
		if statusCode == http.StatusConflict || statusCode == http.StatusUnprocessableEntity {
			retryStatus, retryPayload, retryErr := s.doLettaRequest(ctx, http.MethodGet, "/v1/agents/", lookupParams, nil)
			if retryErr != nil {
				return "", retryErr
			}
			if retryStatus >= 400 {
				return "", parseLettaResponseError(retryStatus, retryPayload)
			}
			if agentID := parseLettaAgentIDFromList(retryPayload); agentID != "" {
				return verifyAgent(agentID)
			}
		}
		return "", parseLettaResponseError(statusCode, payload)
	}
	agentID := parseLettaAgentIDFromObject(payload)
	if agentID == "" {
		return "", errors.New("Letta agent create returned no id")
	}
	return verifyAgent(agentID)
}

func parseLettaSearchResults(payload []byte) ([]any, error) {
	response, err := parseJSONMap(payload)
	if err != nil {
		return nil, err
	}
	raw, ok := response["results"].([]any)
	if !ok {
		return nil, nil
	}
	return raw, nil
}

func (s *server) queryLettaSource(
	ctx context.Context,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	if !s.lettaConfigEnabled() {
		return nil, nil, nil
	}
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, nil
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	retrievalMode := normalizeRetrievalMode(anyToString(baseRequest["retrieval_mode"]))
	topK := s.lettaTopKForMode(retrievalMode, limit)
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))

	agentID, err := s.resolveLettaAgentID(ctx)
	if err != nil {
		return nil, nil, err
	}
	params := url.Values{}
	params.Set("query", query)
	params.Set("top_k", strconv.Itoa(topK))
	if projectFilter != "" {
		params.Add("tags", "project:"+projectFilter)
	}
	statusCode, payload, err := s.doLettaRequest(
		ctx,
		http.MethodGet,
		"/v1/agents/"+agentID+"/archival-memory/search",
		params,
		nil,
	)
	if err != nil {
		return nil, nil, err
	}
	if statusCode >= 400 {
		return nil, nil, parseLettaResponseError(statusCode, payload)
	}
	results, err := parseLettaSearchResults(payload)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]map[string]any, 0, len(results))
	for _, item := range results {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content := strings.TrimSpace(anyToString(entry["content"]))
		if content == "" {
			content = strings.TrimSpace(anyToString(entry["text"]))
		}
		parsed := parseLettaArchivalContent(content)
		project := strings.TrimSpace(parsed["project"])
		if project == "" {
			project = projectFilter
		}
		fileName := strings.TrimSpace(parsed["file"])
		topicPath := strings.TrimSpace(parsed["topic_path"])
		summary := strings.TrimSpace(parsed["summary"])
		if summary == "" {
			summary = clipText(content, 500)
		}
		if projectFilter != "" && project != "" && project != projectFilter {
			continue
		}
		if topicFilter != "" && topicPath != "" && !strings.HasPrefix(topicPath, topicFilter) {
			continue
		}
		score := textMatchScore(query, project+"\n"+fileName+"\n"+summary+"\n"+content)
		if score <= 0 {
			continue
		}
		row := map[string]any{
			"project":          nil,
			"file":             nil,
			"summary":          summary,
			"score":            score,
			"source":           sourceLetta,
			"topic_path":       nil,
			"created_at":       entry["timestamp"],
			"letta_passage_id": entry["id"],
		}
		if project != "" {
			row["project"] = project
		}
		if fileName != "" {
			row["file"] = fileName
		}
		if topicPath != "" {
			row["topic_path"] = topicPath
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) == 0 && !s.strictNoPythonRuntime {
		return nil, nil, errors.New("memory store topic rollups empty")
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil, nil
}

func (s *server) queryTopicRollupsSource(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, errors.New("query is required")
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))
	topics := make([]any, 0)
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		topN := s.retrieval.topicRollupSearchTopN
		if topN < limit {
			topN = limit
		}
		if topicFilter != "" {
			// Scoped reads need extra headroom so topic filtering doesn't drop relevant descendants.
			scopedTopN := limit * 120
			if scopedTopN > topN {
				topN = scopedTopN
			}
		}
		topN = clampInt(topN, 200, 5000)
		rollups := s.memoryStore.topicRollupsWithContext(ctx, projectFilter, 1, topN, 0)
		if memoryTopics, ok := rollups["topics"].([]any); ok && len(memoryTopics) > 0 {
			topics = memoryTopics
		}
	}
	if len(topics) == 0 {
		if s.strictNoPythonRuntime {
			return nil, nil, errors.New("memory store topic rollups empty")
		}
		backendPayload := map[string]any{
			"project":  projectFilter,
			"topN":     5000,
			"maxDepth": 1,
		}
		backendRollups, _, err := s.backendJSONRequest(
			ctx,
			http.MethodPost,
			"/memory/topic-rollups",
			incomingHeaders,
			backendPayload,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("backend topic rollups unavailable: %w", err)
		}
		if backendTopics, ok := backendRollups["topics"].([]any); ok && len(backendTopics) > 0 {
			topics = backendTopics
		}
	}
	if len(topics) == 0 {
		return nil, nil, errors.New("topic rollups unavailable")
	}
	rows := make([]map[string]any, 0, len(topics))
	for _, topicRow := range topics {
		topic, ok := topicRow.(map[string]any)
		if !ok {
			continue
		}
		topicPath := strings.TrimSpace(anyToString(topic["path"]))
		if topicPath == "" {
			continue
		}
		if topicFilter != "" {
			if topicPath != topicFilter && !strings.HasPrefix(topicPath, topicFilter+"/") {
				continue
			}
		}
		project := strings.TrimSpace(anyToString(topic["project"]))
		if project == "" {
			project = projectFilter
		}
		snippets := anyToStringSlice(topic["summarySnippets"])
		text := strings.TrimSpace(topicPath + "\n" + strings.Join(snippets, "\n"))
		score := textMatchScore(query, text)
		if score <= 0 {
			continue
		}
		summary := ""
		if len(snippets) > 0 {
			summary = strings.TrimSpace(snippets[0])
		}
		if summary == "" {
			summary = "Topic rollup for " + topicPath
		}
		fileName := "_rollups/topics/" + strings.ReplaceAll(topicPath, "/", "_") + ".json"
		rows = append(rows, map[string]any{
			"project":    project,
			"file":       fileName,
			"summary":    summary,
			"score":      score,
			"source":     sourceTopicRollup,
			"topic_path": topicPath,
			"created_at": topic["latestTimestamp"],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) == 0 {
		return nil, nil, errors.New("topic rollups did not match query")
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil, nil
}

func (s *server) callBackendSourceQuery(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	explicitSourceOverride bool,
) ([]map[string]any, []string, map[string]any, string, error) {
	fallbackWarnings := []string{}
	if source == sourceLetta {
		rows, warnings, err := s.queryLettaSource(ctx, baseRequest)
		return rows, warnings, nil, sourceOwnerGoNative, err
	}
	if source == sourceMemoryBank {
		rows, warnings, sourceTrace, owner, err := s.queryMemoryBankSource(
			ctx,
			incomingHeaders,
			baseRequest,
			explicitSourceOverride,
		)
		return rows, warnings, sourceTrace, owner, err
	}
	if source == sourceMongoRaw {
		rows, warnings, err := s.queryMongoRawSource(ctx, baseRequest)
		if err == nil {
			return rows, warnings, nil, sourceOwnerGoNative, nil
		}
		fallbackWarnings = append(
			fallbackWarnings,
			"mongo_raw go-adapter fallback to backend retrieval lane: "+err.Error(),
		)
	}
	if source == sourceMindsdb {
		rows, warnings, err := s.queryMindsdbSource(ctx, baseRequest)
		if err == nil {
			return rows, warnings, nil, sourceOwnerGoNative, nil
		}
		fallbackWarnings = append(
			fallbackWarnings,
			"mindsdb go-adapter fallback to backend retrieval lane: "+err.Error(),
		)
	}
	if source == sourceQdrant {
		rows, warnings, err := s.queryQdrantSource(ctx, baseRequest)
		if err == nil {
			return rows, warnings, nil, sourceOwnerGoNative, nil
		}
		fallbackWarnings = append(
			fallbackWarnings,
			"qdrant go-adapter fallback to backend retrieval lane: "+err.Error(),
		)
	}
	if source == sourceWeaviate {
		rows, warnings, err := s.queryWeaviateSource(ctx, baseRequest)
		if err == nil {
			return rows, warnings, nil, sourceOwnerGoNative, nil
		}
		fallbackWarnings = append(
			fallbackWarnings,
			"weaviate go-adapter fallback to backend retrieval lane: "+err.Error(),
		)
	}
	if source == sourcePgvector {
		rows, warnings, err := s.queryPostgresPgvectorSource(ctx, baseRequest)
		if err == nil {
			return rows, warnings, nil, sourceOwnerGoNative, nil
		}
		fallbackWarnings = append(
			fallbackWarnings,
			"postgres_pgvector go-adapter fallback to backend retrieval lane: "+err.Error(),
		)
	}
	if source == sourceTopicRollup {
		rows, warnings, err := s.queryTopicRollupsSource(ctx, incomingHeaders, baseRequest)
		if err == nil {
			return rows, warnings, nil, sourceOwnerGoNative, nil
		}
		fallbackWarnings = append(
			fallbackWarnings,
			"topic_rollups go-adapter fallback to backend retrieval lane: "+err.Error(),
		)
	}
	if s.strictNoPythonRuntime {
		if len(fallbackWarnings) > 0 {
			fallbackWarnings = append(fallbackWarnings, "python backend fallback disabled by strict runtime policy")
		}
		return []map[string]any{}, fallbackWarnings, nil, sourceOwnerGoNative, errors.New("python backend fallback disabled for source " + source)
	}
	rows, warnings, _, err := s.queryBackendSourceSingle(
		ctx,
		incomingHeaders,
		baseRequest,
		source,
		explicitSourceOverride,
		"",
	)
	if err != nil {
		return nil, nil, nil, sourceOwnerPythonBackendFallback, err
	}
	for _, row := range rows {
		if strings.TrimSpace(anyToString(row["source"])) == "" {
			row["source"] = source
		}
	}
	warnings = append(warnings, fallbackWarnings...)
	return rows, warnings, nil, sourceOwnerPythonBackendFallback, nil
}

func (s *server) queryBackendSourceSingle(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	explicitSourceOverride bool,
	memoryBankBackendOverride string,
) ([]map[string]any, []string, map[string]any, error) {
	if s.strictNoPythonRuntime {
		return nil, nil, nil, errors.New("python backend retrieval disabled by strict runtime policy")
	}
	sourceRequest := cloneMap(baseRequest)
	sourceRequest["sources"] = []string{source}
	if s.retrieval.subcallDisableExpansion && !explicitSourceOverride {
		sourceRequest["query_expansion"] = false
	}
	if s.retrieval.subcallDisableAutoEscalate && !explicitSourceOverride {
		sourceRequest["auto_escalate"] = false
	}
	if source == sourceMemoryBank && strings.TrimSpace(memoryBankBackendOverride) != "" {
		backendPolicy := map[string]any{}
		if existing, ok := sourceRequest["backend_policy"].(map[string]any); ok {
			backendPolicy = cloneMap(existing)
		}
		backendPolicy["memory_bank_backend"] = strings.TrimSpace(strings.ToLower(memoryBankBackendOverride))
		sourceRequest["backend_policy"] = backendPolicy
	}
	wrapper := map[string]any{"request": sourceRequest}
	payloadBytes, err := json.Marshal(wrapper)
	if err != nil {
		return nil, nil, nil, err
	}
	requestURL := s.backendURL + "/v1/retrieval/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	s.copyHeaders(req.Header, incomingHeaders)
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, nil, errors.New("backend retrieval status=" + strconv.Itoa(resp.StatusCode))
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	rows := parseRows(payload["results"])
	warnings := parseWarnings(payload["warnings"])
	return rows, warnings, payload, nil
}

func cloneAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func mergeSourceChainDebug(
	target map[string][]map[string]any,
	delta map[string][]map[string]any,
) {
	if target == nil || len(delta) == 0 {
		return
	}
	for source, entries := range delta {
		source = strings.TrimSpace(strings.ToLower(source))
		if source == "" || len(entries) == 0 {
			continue
		}
		for _, entry := range entries {
			if len(entry) == 0 {
				continue
			}
			target[source] = append(target[source], cloneAnyMap(entry))
		}
	}
}

func nowUTCISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func continuationTokenForRequest(query string, request map[string]any) string {
	payload := map[string]any{
		"query":   strings.TrimSpace(query),
		"project": strings.TrimSpace(anyToString(request["project"])),
		"topic":   strings.TrimSpace(anyToString(request["topic_path"])),
		"time":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, _ := json.Marshal(payload)
	sum := sha1.Sum(encoded)
	return fmtHex16(sum[:])
}

func fmtHex16(bytes []byte) string {
	if len(bytes) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(bytes); i++ {
		if i >= 8 {
			break
		}
		b.WriteString(strconv.FormatInt(int64(bytes[i]>>4), 16))
		b.WriteString(strconv.FormatInt(int64(bytes[i]&0x0f), 16))
	}
	return b.String()
}

func (s *server) pruneContinuationLocked(now time.Time) {
	if len(s.continuationExpiry) == 0 {
		return
	}
	for token, expiry := range s.continuationExpiry {
		if expiry.After(now) {
			continue
		}
		delete(s.continuationExpiry, token)
		delete(s.continuationHistory, token)
		delete(s.continuationSubscribers, token)
	}
}

func (s *server) publishContinuationEvent(token string, payload map[string]any) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	event := cloneAnyMap(payload)
	if strings.TrimSpace(anyToString(event["at"])) == "" {
		event["at"] = nowUTCISO()
	}
	event["token"] = token
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	s.pruneContinuationLocked(time.Now().UTC())
	if s.retrieval.continuationEventTTL > 0 {
		s.continuationExpiry[token] = time.Now().UTC().Add(s.retrieval.continuationEventTTL)
	}
	history := append(s.continuationHistory[token], event)
	if len(history) > s.retrieval.continuationEventHistory {
		history = append([]map[string]any(nil), history[len(history)-s.retrieval.continuationEventHistory:]...)
	}
	s.continuationHistory[token] = history
	for _, subscriber := range s.continuationSubscribers[token] {
		select {
		case subscriber <- cloneAnyMap(event):
		default:
		}
	}
}

func (s *server) registerContinuationSubscriber(token string, subscriber chan map[string]any) ([]map[string]any, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, false
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	s.pruneContinuationLocked(time.Now().UTC())
	history, historyOk := s.continuationHistory[token]
	if !historyOk {
		if _, exists := s.continuationExpiry[token]; !exists {
			return nil, false
		}
		history = nil
	}
	s.continuationSubscribers[token] = append(s.continuationSubscribers[token], subscriber)
	out := make([]map[string]any, 0, len(history))
	for _, row := range history {
		out = append(out, cloneAnyMap(row))
	}
	return out, true
}

func (s *server) unregisterContinuationSubscriber(token string, subscriber chan map[string]any) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	subscribers := s.continuationSubscribers[token]
	if len(subscribers) == 0 {
		return
	}
	filtered := make([]chan map[string]any, 0, len(subscribers))
	for _, candidate := range subscribers {
		if candidate == subscriber {
			continue
		}
		filtered = append(filtered, candidate)
	}
	if len(filtered) == 0 {
		delete(s.continuationSubscribers, token)
		return
	}
	s.continuationSubscribers[token] = filtered
}

func (s *server) scheduleContinuationWarm(
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	reason string,
	streamToken string,
) bool {
	ok, _, _ := s.scheduleContinuationWarmWithStatus(incomingHeaders, baseRequest, source, reason, streamToken)
	return ok
}

func (s *server) scheduleContinuationWarmWithStatus(
	incomingHeaders http.Header,
	baseRequest map[string]any,
	source string,
	reason string,
	streamToken string,
) (bool, string, map[string]any) {
	if shed, shedReason, shedDetail := s.shouldShedContinuation(source); shed {
		log.Printf("continuation warm skipped source=%s reason=%s detail=%s", source, reason, shedReason)
		payload := map[string]any{
			"event":  "skipped",
			"status": shedReason,
			"source": source,
			"reason": reason,
		}
		if len(shedDetail) > 0 {
			payload["queue"] = shedDetail
		}
		s.publishContinuationEvent(streamToken, payload)
		return false, shedReason, shedDetail
	}
	select {
	case s.continuationSem <- struct{}{}:
	default:
		log.Printf("continuation warm skipped source=%s reason=%s detail=max_inflight", source, reason)
		s.publishContinuationEvent(streamToken, map[string]any{
			"event":  "skipped",
			"status": "max_inflight",
			"source": source,
			"reason": reason,
		})
		return false, "max_inflight", map[string]any{
			"pending_count": s.continuationQueueSnapshot().Pending,
		}
	}
	reserved, reserveStatus, reserveCooldown := s.tryReserveContinuationSourceSlot(source)
	if !reserved {
		<-s.continuationSem
		log.Printf("continuation warm skipped source=%s reason=%s detail=%s", source, reason, reserveStatus)
		skipPayload := map[string]any{
			"event":  "skipped",
			"status": reserveStatus,
			"source": source,
			"reason": reason,
		}
		if reserveCooldown > 0 {
			skipPayload["cooldown_remaining_secs"] = roundFloat(reserveCooldown, 3)
		}
		s.publishContinuationEvent(streamToken, skipPayload)
		statusPayload := map[string]any{}
		if reserveCooldown > 0 {
			statusPayload["cooldown_remaining_secs"] = roundFloat(reserveCooldown, 3)
		}
		return false, reserveStatus, statusPayload
	}
	s.decrementContinuationRetrying(source)
	s.publishContinuationEvent(streamToken, map[string]any{
		"event":  "queued",
		"status": "queued",
		"source": source,
		"reason": reason,
	})
	go func() {
		defer func() { <-s.continuationSem }()
		defer s.releaseContinuationSourceSlot(source)
		timeout := s.resolveContinuationTimeout(source)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		start := time.Now()
		_, _, _, _, err := s.callBackendSourceQuery(ctx, incomingHeaders, baseRequest, source, true)
		status := "ok"
		errorText := ""
		cooldownRemaining := 0.0
		if err != nil {
			s.incrementContinuationRetrying(source)
			status = "error"
			errorText = err.Error()
			cooldownRemaining = s.applyContinuationSourceCooldown(source)
			log.Printf("continuation warm failed source=%s reason=%s error=%s", source, reason, err)
		} else {
			s.decrementContinuationRetrying(source)
		}
		latency := time.Since(start).Milliseconds()
		s.telemetry.record(retrievalEvent{Source: source, Phase: "continuation", Status: status, LatencyMs: latency})
		completePayload := map[string]any{
			"event":      "completed",
			"status":     status,
			"source":     source,
			"reason":     reason,
			"latency_ms": latency,
			"error":      errorText,
		}
		if cooldownRemaining > 0 {
			completePayload["cooldown_remaining_secs"] = roundFloat(cooldownRemaining, 3)
		}
		s.publishContinuationEvent(streamToken, completePayload)
	}()
	return true, "queued", nil
}

type sourceCallResult struct {
	source         string
	sourceOwner    string
	sourceTrace    map[string]any
	phase          string
	rows           []map[string]any
	warnings       []string
	err            error
	timedOut       bool
	budgetExceeded bool
	timeout        time.Duration
	latency        time.Duration
}

type sourceCallPayload struct {
	rows        []map[string]any
	warnings    []string
	sourceTrace map[string]any
	owner       string
	err         error
}

type sourceBatchOutput struct {
	rows                    map[string][]map[string]any
	sourceOwners            map[string]string
	sourceErrors            map[string]map[string]any
	sourceChainDebug        map[string][]map[string]any
	warnings                []string
	timedOutSources         []string
	budgetExceededSources   []string
	continuationSources     []string
	continuationUnavailable []string
	skippedSources          []string
	effectiveTimeoutsSecs   map[string]float64
	adaptiveBudgets         map[string]map[string]any
}

func (s *server) runSourceBatch(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
	sources []string,
	retrievalMode string,
	phase string,
	explicitSourceOverride bool,
	blockingSlowSources bool,
	syncPhase bool,
	suppressSlowTimeoutWarnings bool,
	adaptiveSkipped map[string]struct{},
	continuationToken string,
) sourceBatchOutput {
	output := sourceBatchOutput{
		rows:                  make(map[string][]map[string]any),
		sourceOwners:          make(map[string]string),
		sourceErrors:          make(map[string]map[string]any),
		sourceChainDebug:      make(map[string][]map[string]any),
		warnings:              []string{},
		effectiveTimeoutsSecs: make(map[string]float64),
		adaptiveBudgets:       make(map[string]map[string]any),
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
		_, isSlowSource := slowSet[normalized]
		sourceTimeout, adaptiveBudget := s.resolveSourceTimeout(
			normalized,
			retrievalMode,
			syncPhase,
			isSlowSource,
			blockingSlowSources,
		)
		output.effectiveTimeoutsSecs[normalized] = roundFloat(sourceTimeout.Seconds(), 3)
		output.adaptiveBudgets[normalized] = adaptiveBudget
		go func(sourceName string, timeout time.Duration) {
			start := time.Now()
			sourceCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			queueWait := time.Duration(0)
			if syncPhase {
				waited, acquired := s.acquireSyncSourceSlot(sourceCtx, sourceName)
				queueWait = waited
				if !acquired {
					err := sourceCtx.Err()
					if err == nil {
						err = context.DeadlineExceeded
					}
					latency := time.Since(start)
					s.telemetry.record(retrievalEvent{
						Source:    sourceName,
						Phase:     phase,
						Status:    "queue_timeout",
						LatencyMs: latency.Milliseconds(),
					})
					resultsCh <- sourceCallResult{
						source:         sourceName,
						sourceOwner:    sourceOwnerForSource(sourceName),
						sourceTrace:    map[string]any{"sync_queue_wait_ms": queueWait.Milliseconds(), "queue_acquired": false},
						phase:          phase,
						rows:           nil,
						warnings:       []string{"sync source queue wait exceeded timeout envelope"},
						err:            err,
						timedOut:       true,
						budgetExceeded: false,
						timeout:        timeout,
						latency:        latency,
					}
					return
				}
				defer s.releaseSyncSourceSlot(sourceName)
			}
			callDone := make(chan sourceCallPayload, 1)
			go func() {
				rows, warnings, sourceTrace, owner, err := s.callBackendSourceQuery(
					sourceCtx,
					incomingHeaders,
					baseRequest,
					sourceName,
					explicitSourceOverride,
				)
				callDone <- sourceCallPayload{
					rows:        rows,
					warnings:    warnings,
					sourceTrace: sourceTrace,
					owner:       owner,
					err:         err,
				}
			}()
			rows := []map[string]any{}
			warnings := []string{}
			sourceTrace := map[string]any(nil)
			owner := sourceOwnerForSource(sourceName)
			err := error(nil)
			select {
			case payload := <-callDone:
				rows = payload.rows
				warnings = payload.warnings
				sourceTrace = payload.sourceTrace
				if strings.TrimSpace(payload.owner) != "" {
					owner = payload.owner
				}
				err = payload.err
			case <-sourceCtx.Done():
				err = sourceCtx.Err()
				s.watchTimeoutContract(sourceName, phase, timeout, start, callDone)
			}
			if sourceTrace == nil {
				sourceTrace = map[string]any{}
			}
			if syncPhase {
				sourceTrace["sync_queue_wait_ms"] = queueWait.Milliseconds()
				sourceTrace["queue_acquired"] = true
			}
			latency := time.Since(start)
			timedOut := false
			budgetExceeded := false
			if err != nil {
				timedOut = isTimeoutError(err) || errors.Is(sourceCtx.Err(), context.DeadlineExceeded)
				if timedOut && syncPhase && !blockingSlowSources {
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
				sourceOwner:    owner,
				sourceTrace:    sourceTrace,
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
		if strings.TrimSpace(result.sourceOwner) == "" {
			result.sourceOwner = sourceOwnerForSource(result.source)
		}
		output.sourceOwners[result.source] = result.sourceOwner
		if len(result.sourceTrace) > 0 {
			output.sourceChainDebug[result.source] = append(
				output.sourceChainDebug[result.source],
				cloneAnyMap(result.sourceTrace),
			)
		}
		if len(result.rows) > 0 {
			result.rows = s.normalizeSourceRows(result.source, result.rows)
		}
		for _, row := range result.rows {
			if strings.TrimSpace(anyToString(row["source_owner"])) == "" {
				row["source_owner"] = result.sourceOwner
			}
		}
		if len(result.rows) > 0 {
			output.rows[result.source] = result.rows
		}
		if len(result.warnings) > 0 {
			for _, warning := range result.warnings {
				output.warnings = append(output.warnings, result.source+": "+warning)
			}
		}
		if result.err != nil {
			if looksLikeParseError(result.err) {
				s.recordDrift("parse_error", result.source, result.err.Error())
			}
			nonDegradableLane := s.isNonDegradableSource(result.source)
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
				"non_degradable":  nonDegradableLane,
			}
			output.sourceErrors[result.source] = errorPayload
			if result.timedOut {
				if result.budgetExceeded {
					output.budgetExceededSources = append(output.budgetExceededSources, result.source)
					if !suppressSlowTimeoutWarnings {
						if nonDegradableLane {
							output.warnings = append(
								output.warnings,
								result.source+" retrieval sync budget exceeded after "+strconv.FormatFloat(result.timeout.Seconds(), 'f', 1, 64)+"s (non-degradable lane; continuing asynchronously).",
							)
						} else {
							output.warnings = append(
								output.warnings,
								result.source+" retrieval sync budget exceeded after "+strconv.FormatFloat(result.timeout.Seconds(), 'f', 1, 64)+"s",
							)
						}
					}
				} else {
					output.timedOutSources = append(output.timedOutSources, result.source)
					if nonDegradableLane {
						output.warnings = append(
							output.warnings,
							result.source+" retrieval timed out after "+strconv.FormatFloat(result.timeout.Seconds(), 'f', 1, 64)+"s (non-degradable lane; continuing asynchronously).",
						)
					} else {
						output.warnings = append(
							output.warnings,
							result.source+" retrieval timed out after "+strconv.FormatFloat(result.timeout.Seconds(), 'f', 1, 64)+"s",
						)
					}
				}
				if s.shouldScheduleContinuation(result.source) || nonDegradableLane {
					continuationState, _, _ := s.scheduleOrDeferContinuation(
						incomingHeaders,
						baseRequest,
						result.source,
						result.phase+"-timeout",
						continuationToken,
					)
					if continuationState == "scheduled" {
						output.continuationSources = append(output.continuationSources, result.source)
						if !suppressSlowTimeoutWarnings || !result.budgetExceeded {
							output.warnings = append(
								output.warnings,
								result.source+" timed out; continuing asynchronously for cache warm.",
							)
						}
					} else if continuationState == "deferred" {
						output.continuationSources = append(output.continuationSources, result.source)
						output.warnings = append(
							output.warnings,
							result.source+" timed out; async continuation deferred durably and will retry automatically.",
						)
					} else {
						output.continuationUnavailable = append(output.continuationUnavailable, result.source)
						output.warnings = append(
							output.warnings,
							result.source+" timed out; async continuation unavailable right now due queue/cooldown pressure.",
						)
					}
				}
			} else {
				output.warnings = append(output.warnings, result.source+" retrieval failed: "+result.err.Error())
				if nonDegradableLane {
					continuationState, _, _ := s.scheduleOrDeferContinuation(
						incomingHeaders,
						baseRequest,
						result.source,
						result.phase+"-error",
						continuationToken,
					)
					if continuationState == "scheduled" {
						output.continuationSources = append(output.continuationSources, result.source)
						output.warnings = append(
							output.warnings,
							result.source+" is a non-degradable lane; continuing asynchronously after error.",
						)
					} else if continuationState == "deferred" {
						output.continuationSources = append(output.continuationSources, result.source)
						output.warnings = append(
							output.warnings,
							result.source+" is non-degradable; async continuation deferred durably and will retry automatically.",
						)
					} else {
						output.continuationUnavailable = append(output.continuationUnavailable, result.source)
						output.warnings = append(
							output.warnings,
							result.source+" is non-degradable but async continuation is currently unavailable; retry for warmed context.",
						)
					}
				}
			}
		}
		s.recordAdaptiveObservation(
			result.source,
			result.latency,
			result.timedOut || result.budgetExceeded,
			result.err != nil,
			result.budgetExceeded,
		)
	}

	output.warnings = dedupeWarnings(output.warnings)
	output.timedOutSources = normalizeSourceList(output.timedOutSources)
	output.budgetExceededSources = normalizeSourceList(output.budgetExceededSources)
	output.continuationSources = normalizeSourceList(output.continuationSources)
	output.continuationUnavailable = normalizeSourceList(output.continuationUnavailable)
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

func buildRetrievalLifecyclePayload(
	resultState string,
	returnedNow []string,
	pending []string,
	warming []string,
	failed []string,
	timedOut []string,
	budgetExceeded []string,
) map[string]any {
	normalizedResultState := strings.TrimSpace(strings.ToLower(resultState))
	status := "succeeded"
	if normalizedResultState == "" {
		if len(returnedNow) > 0 {
			normalizedResultState = "ready"
		} else if len(pending) > 0 {
			normalizedResultState = "pending"
		} else {
			normalizedResultState = "empty"
		}
	}
	switch normalizedResultState {
	case "degraded":
		status = "failed"
	case "pending":
		status = "partial"
	default:
		if len(returnedNow) > 0 {
			status = "succeeded"
		} else if len(pending) > 0 {
			status = "partial"
		} else if len(failed) > 0 || len(timedOut) > 0 {
			status = "failed"
		}
	}
	if status != "failed" && (len(pending) > 0 || len(warming) > 0 || len(budgetExceeded) > 0) {
		status = "partial"
	}
	partial := status == "partial" || len(pending) > 0 || len(warming) > 0 || len(budgetExceeded) > 0
	nextActions := []string{}
	if partial {
		nextActions = append(nextActions, "retry_after_cache_warm")
		if len(warming) > 0 {
			nextActions = append(nextActions, "watch_continuation_events")
		}
	}
	if status == "failed" {
		nextActions = append(nextActions, "retry_with_longer_timeout")
	}
	return map[string]any{
		"statusLifecycle": []string{"queued", "running", "partial", "succeeded", "failed"},
		"status":          status,
		"result_state":    normalizedResultState,
		"partial":         partial,
		"sources": map[string]any{
			"returned_now":    returnedNow,
			"pending":         pending,
			"warming":         warming,
			"failed":          failed,
			"timed_out":       timedOut,
			"budget_exceeded": budgetExceeded,
		},
		"next_actions": nextActions,
	}
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
	retrievalMode := normalizeRetrievalMode(anyToString(requestPayload["retrieval_mode"]))
	retrievalIntent := strings.TrimSpace(strings.ToLower(anyToString(requestPayload["retrieval_intent"])))
	if retrievalIntent == "" {
		retrievalIntent = "decision"
	}
	trafficClass := strings.TrimSpace(strings.ToLower(anyToString(requestPayload["traffic_class"])))
	if trafficClass == "" {
		trafficClass = "user"
	}
	rustBackendPolicy := resolveRustBackendPolicy(requestPayload["backend_policy"])
	rustLaneGateApplied := false
	rustBackendPolicy, rustLaneGateApplied = s.applyRustLanePromotionGate(rustBackendPolicy, trafficClass)
	requestPayload["backend_policy"] = rustBackendPolicy
	topicPrefilterHint := ""
	topicPrefilterApplied := false
	if s.retrieval.topicPrefilterEnabled &&
		retrievalMode == "deep" &&
		strings.TrimSpace(anyToString(requestPayload["topic_path"])) == "" {
		topicPrefilterHint = inferTopicPathFromQuery(query)
		if topicPrefilterHint != "" {
			requestPayload["topic_path"] = topicPrefilterHint
			topicPrefilterApplied = true
		}
	}
	explicitSources := anyToStringSlice(requestPayload["sources"])
	explicitSourceOverride := len(explicitSources) > 0
	blockingSlowSources := anyToBool(requestPayload["blocking"]) ||
		anyToBool(requestPayload["sync_slow_sources"]) ||
		anyToBool(requestPayload["wait_for_slow_sources"])
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
	sourceOwners := map[string]string{}
	sourceRows := map[string][]map[string]any{}
	sourceChainDebug := map[string][]map[string]any{}
	effectiveTimeouts := map[string]float64{}
	adaptiveBudgets := map[string]map[string]any{}
	timedOutObserved := map[string]struct{}{}
	budgetExceededObserved := map[string]struct{}{}
	adaptiveSkipped := map[string]struct{}{}
	continuationSources := []string{}
	continuationUnavailable := []string{}
	asyncWarmSlowSources := []string{}
	syncFallbackSlowSources := []string{}
	coverageRescueApplied := false
	coverageRescueQuery := ""
	coverageRescueSources := []string{}
	continuationToken := continuationTokenForRequest(query, requestPayload)
	minFastTarget := s.resolveMinFastResults(retrievalMode)

	fastBatch := s.runSourceBatch(
		ctx,
		incomingHeaders,
		requestPayload,
		fastSources,
		retrievalMode,
		"fast",
		explicitSourceOverride,
		blockingSlowSources,
		true,
		false,
		adaptiveSkipped,
		continuationToken,
	)
	for source, rows := range fastBatch.rows {
		sourceRows[source] = rows
	}
	for source, owner := range fastBatch.sourceOwners {
		sourceOwners[source] = owner
	}
	mergeSourceChainDebug(sourceChainDebug, fastBatch.sourceChainDebug)
	for source, payload := range fastBatch.sourceErrors {
		sourceErrors[source] = payload
	}
	for source, timeoutSecs := range fastBatch.effectiveTimeoutsSecs {
		effectiveTimeouts[source] = timeoutSecs
	}
	for source, budget := range fastBatch.adaptiveBudgets {
		adaptiveBudgets[source] = budget
	}
	warnings = append(warnings, fastBatch.warnings...)
	for _, source := range fastBatch.timedOutSources {
		timedOutObserved[source] = struct{}{}
		if s.shouldAdaptiveSkip(source) && !blockingSlowSources {
			adaptiveSkipped[source] = struct{}{}
		}
	}
	continuationSources = append(continuationSources, fastBatch.continuationSources...)
	continuationUnavailable = append(continuationUnavailable, fastBatch.continuationUnavailable...)
	for _, source := range fastBatch.budgetExceededSources {
		budgetExceededObserved[source] = struct{}{}
	}

	merged := mergeRows(sourceRows, limit)
	lexicalBackend := strings.TrimSpace(strings.ToLower(anyToString(rustBackendPolicy["lexical_backend"])))
	lexicalGuardEligible := s.retrieval.lexicalGuardEnabled && !explicitSourceOverride && lexicalBackend == "tantivy_lexical"
	lexicalGuardCoverage := 0.0
	lexicalGuardApplied := false
	rustQualityFallbackAttempted := false
	rustQualityFallbackApplied := false
	rustQualityFallbackSourcesUsed := []string{}
	rustQualityFallbackModeUsed := ""
	if lexicalGuardEligible {
		lexicalGuardCoverage = lexicalCoverageScore(query, merged)
	}
	fastPathFailed := len(merged) == 0 || len(fastBatch.sourceErrors) > 0
	skipSlow := !blockingSlowSources && (retrievalMode != "deep" || !s.retrieval.deepBlocking || explicitSourceOverride)
	if len(slowSources) > 0 {
		if skipSlow {
			needsFallback := len(merged) < minFastTarget
			if needsFallback && s.retrieval.disableSyncSlowFallback {
				needsFallback = false
			}
			if needsFallback && explicitSourceOverride && !blockingSlowSources {
				needsFallback = false
				warnings = append(
					warnings,
					"Explicit sources requested in staged fail-open mode; slow sources deferred asynchronously. Set blocking=true (or sync_slow_sources=true) to wait for blocking completion.",
				)
			}
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
				if s.retrieval.rustQualityFallbackEnabled && lexicalBackend == "tantivy_lexical" {
					rustFallbackSet := toSourceSet(s.retrieval.rustQualityFallbackSources)
					rustFallback := []string{}
					for _, source := range fastSources {
						if _, ok := rustFallbackSet[source]; ok {
							rustFallback = append(rustFallback, source)
						}
					}
					rustFallback = normalizeSourceList(rustFallback)
					if len(rustFallback) > 0 {
						rustQualityFallbackAttempted = true
						rustQualityFallbackSourcesUsed = append([]string(nil), rustFallback...)
						rustQualityFallbackModeUsed = s.retrieval.rustQualityFallbackMode
						rustRequest := cloneMap(requestPayload)
						rustRequest["retrieval_mode"] = s.retrieval.rustQualityFallbackMode
						rustBatch := s.runSourceBatch(
							ctx,
							incomingHeaders,
							rustRequest,
							rustFallback,
							s.retrieval.rustQualityFallbackMode,
							"rust-quality-sync-fallback",
							explicitSourceOverride,
							blockingSlowSources,
							true,
							!fastPathFailed,
							adaptiveSkipped,
							continuationToken,
						)
						for source, rows := range rustBatch.rows {
							sourceRows[source] = rows
						}
						for source, owner := range rustBatch.sourceOwners {
							sourceOwners[source] = owner
						}
						mergeSourceChainDebug(sourceChainDebug, rustBatch.sourceChainDebug)
						for source, payload := range rustBatch.sourceErrors {
							sourceErrors[source] = payload
						}
						for source, timeoutSecs := range rustBatch.effectiveTimeoutsSecs {
							effectiveTimeouts[source] = timeoutSecs
						}
						for source, budget := range rustBatch.adaptiveBudgets {
							adaptiveBudgets[source] = budget
						}
						warnings = append(warnings, rustBatch.warnings...)
						for _, source := range rustBatch.timedOutSources {
							timedOutObserved[source] = struct{}{}
						}
						continuationSources = append(continuationSources, rustBatch.continuationSources...)
						continuationUnavailable = append(continuationUnavailable, rustBatch.continuationUnavailable...)
						for _, source := range rustBatch.budgetExceededSources {
							budgetExceededObserved[source] = struct{}{}
						}
						merged = mergeRows(sourceRows, limit)
						if len(merged) >= minFastTarget {
							needsFallback = false
							rustQualityFallbackApplied = true
							warnings = append(
								warnings,
								"Rust-first quality fallback satisfied minimum recall coverage; slow-source sync fallback skipped.",
							)
						}
					}
				}
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
				if s.retrieval.timeoutAdaptiveSkipEnabled && !blockingSlowSources {
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
					blockingSlowSources,
					true,
					!fastPathFailed,
					adaptiveSkipped,
					continuationToken,
				)
				for source, rows := range slowBatch.rows {
					sourceRows[source] = rows
				}
				for source, owner := range slowBatch.sourceOwners {
					sourceOwners[source] = owner
				}
				mergeSourceChainDebug(sourceChainDebug, slowBatch.sourceChainDebug)
				for source, payload := range slowBatch.sourceErrors {
					sourceErrors[source] = payload
				}
				for source, timeoutSecs := range slowBatch.effectiveTimeoutsSecs {
					effectiveTimeouts[source] = timeoutSecs
				}
				for source, budget := range slowBatch.adaptiveBudgets {
					adaptiveBudgets[source] = budget
				}
				warnings = append(warnings, slowBatch.warnings...)
				for _, source := range slowBatch.timedOutSources {
					timedOutObserved[source] = struct{}{}
					if s.shouldAdaptiveSkip(source) && !blockingSlowSources {
						adaptiveSkipped[source] = struct{}{}
					}
				}
				continuationSources = append(continuationSources, slowBatch.continuationSources...)
				continuationUnavailable = append(continuationUnavailable, slowBatch.continuationUnavailable...)
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
				blockingSlowSources,
				true,
				!fastPathFailed,
				adaptiveSkipped,
				continuationToken,
			)
			for source, rows := range slowBatch.rows {
				sourceRows[source] = rows
			}
			for source, owner := range slowBatch.sourceOwners {
				sourceOwners[source] = owner
			}
			mergeSourceChainDebug(sourceChainDebug, slowBatch.sourceChainDebug)
			for source, payload := range slowBatch.sourceErrors {
				sourceErrors[source] = payload
			}
			for source, timeoutSecs := range slowBatch.effectiveTimeoutsSecs {
				effectiveTimeouts[source] = timeoutSecs
			}
			for source, budget := range slowBatch.adaptiveBudgets {
				adaptiveBudgets[source] = budget
			}
			warnings = append(warnings, slowBatch.warnings...)
			for _, source := range slowBatch.timedOutSources {
				timedOutObserved[source] = struct{}{}
				if s.shouldAdaptiveSkip(source) && !blockingSlowSources {
					adaptiveSkipped[source] = struct{}{}
				}
			}
			continuationSources = append(continuationSources, slowBatch.continuationSources...)
			continuationUnavailable = append(continuationUnavailable, slowBatch.continuationUnavailable...)
			for _, source := range slowBatch.budgetExceededSources {
				budgetExceededObserved[source] = struct{}{}
			}
		}
	}

	if len(merged) == 0 && s.retrieval.coverageRescueEnabled {
		if rescueQuery, rescueOK := deriveCoverageRescueQuery(query, s.retrieval.coverageRescueMinTokens); rescueOK {
			rescuePayload := cloneMap(requestPayload)
			rescuePayload["query"] = rescueQuery
			rescueSources := append([]string(nil), fastSources...)
			if len(rescueSources) == 0 {
				rescueSources = append([]string(nil), resolvedSources...)
			}
			rescueSources = normalizeSourceList(rescueSources)
			coverageRescueSources = append([]string(nil), rescueSources...)
			rescueBatch := s.runSourceBatch(
				ctx,
				incomingHeaders,
				rescuePayload,
				rescueSources,
				retrievalMode,
				"coverage-rescue-fast",
				explicitSourceOverride,
				blockingSlowSources,
				true,
				false,
				adaptiveSkipped,
				continuationToken,
			)
			for source, rows := range rescueBatch.rows {
				if len(rows) == 0 {
					continue
				}
				sourceRows[source] = rows
			}
			for source, owner := range rescueBatch.sourceOwners {
				sourceOwners[source] = owner
			}
			mergeSourceChainDebug(sourceChainDebug, rescueBatch.sourceChainDebug)
			for source, payload := range rescueBatch.sourceErrors {
				sourceErrors[source] = payload
			}
			for source, timeoutSecs := range rescueBatch.effectiveTimeoutsSecs {
				effectiveTimeouts[source] = timeoutSecs
			}
			for source, budget := range rescueBatch.adaptiveBudgets {
				adaptiveBudgets[source] = budget
			}
			warnings = append(warnings, rescueBatch.warnings...)
			for _, source := range rescueBatch.timedOutSources {
				timedOutObserved[source] = struct{}{}
				if s.shouldAdaptiveSkip(source) && !blockingSlowSources {
					adaptiveSkipped[source] = struct{}{}
				}
			}
			continuationSources = append(continuationSources, rescueBatch.continuationSources...)
			continuationUnavailable = append(continuationUnavailable, rescueBatch.continuationUnavailable...)
			for _, source := range rescueBatch.budgetExceededSources {
				budgetExceededObserved[source] = struct{}{}
			}
			merged = mergeRows(sourceRows, limit)
			coverageRescueQuery = rescueQuery
			if len(merged) > 0 {
				coverageRescueApplied = true
				warnings = append(
					warnings,
					"Coverage rescue query variant returned results from fast sources. variant="+rescueQuery,
				)
			} else {
				warnings = append(
					warnings,
					"Coverage rescue query variant did not return additional results. variant="+rescueQuery,
				)
			}
		}
	}

	asyncWarmSlowSources = normalizeSourceList(asyncWarmSlowSources)
	for _, source := range asyncWarmSlowSources {
		continuationState, _, _ := s.scheduleOrDeferContinuation(
			incomingHeaders,
			requestPayload,
			source,
			"slow-async-warm",
			continuationToken,
		)
		if continuationState == "scheduled" || continuationState == "deferred" {
			continuationSources = append(continuationSources, source)
		} else {
			continuationUnavailable = append(continuationUnavailable, source)
		}
	}
	continuationSources = normalizeSourceList(continuationSources)
	continuationUnavailable = normalizeSourceList(continuationUnavailable)
	continuationDurable := s.continuationDurableSnapshot()
	warmingSources := append([]string(nil), continuationSources...)

	if rustLaneGateApplied {
		warnings = append(
			warnings,
			"Rust strict backend lane was promoted to qdrant_remote/non-strict for non-benchmark traffic to preserve recall quality and reduce empty-result risk.",
		)
	}
	if topicPrefilterApplied && topicPrefilterHint != "" {
		warnings = append(
			warnings,
			"Applied topic prefilter hint from query for deep retrieval: "+topicPrefilterHint+".",
		)
	}

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
	sourceOwnerBySource := map[string]string{}
	for source, owner := range sourceOwners {
		normalizedSource := strings.TrimSpace(strings.ToLower(source))
		if normalizedSource == "" {
			continue
		}
		normalizedOwner := strings.TrimSpace(strings.ToLower(owner))
		if normalizedOwner == "" {
			normalizedOwner = sourceOwnerForSource(normalizedSource)
		}
		sourceOwnerBySource[normalizedSource] = normalizedOwner
	}
	sourceOwnerCountsMap := sourceOwnerCounts(sourceOwnerBySource)
	ownershipViolations := []string{}
	if s.retrieval.sourceOwnershipMode != "off" {
		for _, source := range fastSources {
			owner := strings.TrimSpace(strings.ToLower(sourceOwnerBySource[source]))
			if owner != sourceOwnerPythonBackendFallback {
				continue
			}
			if _, allowed := s.retrieval.sourceOwnershipStrictFastAllowPy[source]; allowed {
				continue
			}
			ownershipViolations = append(ownershipViolations, source)
		}
		ownershipViolations = normalizeSourceList(ownershipViolations)
		if len(ownershipViolations) > 0 {
			message := "Source ownership policy detected python fallback on fast-source lanes: " + strings.Join(ownershipViolations, ", ") + "."
			if s.retrieval.sourceOwnershipMode == "strict" {
				return map[string]any{
					"ok":                    false,
					"error":                 "source_ownership_violation",
					"route_owner_class":     sourceOwnerGoNative,
					"source_owner_class":    sourceOwnerClass(sourceOwnerBySource),
					"source_owners":         sourceOwnerBySource,
					"source_owner_counts":   sourceOwnerCountsMap,
					"ownership_violations":  ownershipViolations,
					"python_hotpath_counts": s.pythonHotPathOwnershipSnapshot(),
					"message":               message,
				}, http.StatusServiceUnavailable, nil
			}
			warnings = append(warnings, message)
		}
	}

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
	materialErrorSources := map[string]struct{}{}
	for source, payload := range sourceErrors {
		kind := strings.TrimSpace(strings.ToLower(anyToString(payload["kind"])))
		if kind == "" {
			kind = "error"
		}
		if s.isNonDegradableSource(source) {
			continue
		}
		if kind == "timeout" || kind == "error" {
			materialErrorSources[source] = struct{}{}
		}
	}
	hasMaterialSourceErrors := false
	if len(materialErrorSources) > 0 {
		protectedCandidates := []string{}
		for _, source := range resolvedSources {
			if s.isProtectedSource(source) {
				protectedCandidates = append(protectedCandidates, source)
			}
		}
		protectedCandidates = normalizeSourceList(protectedCandidates)
		returnedSet := toSourceSet(returnedSources)
		protectedReturned := false
		for _, source := range protectedCandidates {
			if _, ok := returnedSet[source]; ok {
				protectedReturned = true
				break
			}
		}
		protectedFailed := 0
		for _, source := range protectedCandidates {
			if _, ok := materialErrorSources[source]; ok {
				protectedFailed += 1
			}
		}
		if len(protectedCandidates) == 0 {
			hasMaterialSourceErrors = len(returnedSources) == 0
		} else if !protectedReturned && protectedFailed == len(protectedCandidates) && len(returnedSources) == 0 {
			hasMaterialSourceErrors = true
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
	if len(continuationUnavailable) > 0 {
		warnings = append(
			warnings,
			"Async continuation was unavailable for: "+strings.Join(continuationUnavailable, ", ")+". Re-run shortly to pick up warmed sources once queue pressure clears.",
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
	failedSourcesSet := map[string]struct{}{}
	for source, payload := range sourceErrors {
		kind := strings.TrimSpace(strings.ToLower(anyToString(payload["kind"])))
		if kind == "" {
			kind = "error"
		}
		if kind == "error" || kind == "timeout" {
			failedSourcesSet[source] = struct{}{}
		}
	}
	failedSources := make([]string, 0, len(failedSourcesSet))
	for source := range failedSourcesSet {
		failedSources = append(failedSources, source)
	}
	sort.Strings(failedSources)
	timedOutForLifecycle := make([]string, 0, len(timedOutList))
	for _, source := range timedOutList {
		if s.isNonDegradableSource(source) {
			continue
		}
		timedOutForLifecycle = append(timedOutForLifecycle, source)
	}
	failedForLifecycle := make([]string, 0, len(failedSources))
	for _, source := range failedSources {
		if s.isNonDegradableSource(source) {
			continue
		}
		failedForLifecycle = append(failedForLifecycle, source)
	}
	resultState := "empty"
	if len(merged) > 0 {
		resultState = "ready"
	}
	if len(deferredCandidates) > 0 && len(merged) == 0 {
		resultState = "pending"
	}
	if hasMaterialSourceErrors {
		resultState = "degraded"
	}
	sourceChainDebugPayload := map[string]any{}
	for source, entries := range sourceChainDebug {
		if len(entries) == 0 {
			continue
		}
		sourceChainDebugPayload[source] = entries
	}
	memoryBankFallbackChain := []map[string]any{}
	if entries, ok := sourceChainDebug[sourceMemoryBank]; ok && len(entries) > 0 {
		memoryBankFallbackChain = entries
	}
	lifecycle := buildRetrievalLifecyclePayload(
		resultState,
		returnedSources,
		deferredCandidates,
		warmingSources,
		failedForLifecycle,
		timedOutForLifecycle,
		budgetExceededList,
	)
	debug := map[string]any{
		"retrieval_mode":      retrievalMode,
		"retrieval_intent":    retrievalIntent,
		"sources":             resolvedSources,
		"source_counts":       sourceCounts,
		"source_errors":       sourceErrors,
		"source_owners":       sourceOwnerBySource,
		"source_owner_counts": sourceOwnerCountsMap,
		"source_owner_class":  sourceOwnerClass(sourceOwnerBySource),
		"route_owner_class":   sourceOwnerGoNative,
		"fallback_counts": map[string]any{
			"python_hot_path_total": s.pythonHotPathFallbacks.Load(),
		},
		"source_policy": map[string]any{
			"staged_enabled":               s.retrieval.enabled,
			"fast_sources":                 s.retrieval.fastSources,
			"slow_sources":                 s.retrieval.slowSources,
			"protected_sources":            mapKeysSorted(s.retrieval.protectedSources),
			"non_degradable_sources":       mapKeysSorted(s.retrieval.nonDegradableSources),
			"sync_fallback_sources":        s.retrieval.syncFallbackSources,
			"min_fast_results":             s.retrieval.minFastResults,
			"min_fast_results_by_mode":     s.retrieval.minFastResultsByMode,
			"deep_blocking":                s.retrieval.deepBlocking,
			"blocking_slow_sources":        blockingSlowSources,
			"explicit_source_override":     explicitSourceOverride,
			"disable_sync_slow_fallback":   s.retrieval.disableSyncSlowFallback,
			"slow_sync_timeout_cap_secs":   s.retrieval.slowSyncTimeoutCap.Seconds(),
			"qdrant_sync_timeout_cap_secs": s.retrieval.qdrantSyncTimeoutCap.Seconds(),
			"qdrant_sync_timeout_cap_by_mode_secs": map[string]any{
				"fast":     s.resolveQdrantSyncCap("fast").Seconds(),
				"balanced": s.resolveQdrantSyncCap("balanced").Seconds(),
				"deep":     s.resolveQdrantSyncCap("deep").Seconds(),
			},
			"topic_rollup_sync_timeout_floor_secs": s.retrieval.topicRollupSyncTimeoutFloor.Seconds(),
			"topic_rollup_sync_timeout_floor_by_mode_secs": map[string]any{
				"fast":     s.resolveTopicRollupSyncTimeoutFloor("fast").Seconds(),
				"balanced": s.resolveTopicRollupSyncTimeoutFloor("balanced").Seconds(),
				"deep":     s.resolveTopicRollupSyncTimeoutFloor("deep").Seconds(),
			},
			"letta_top_k_by_mode": map[string]any{
				"fast": map[string]any{
					"factor": s.retrieval.lettaTopKFactorByMode["fast"],
					"cap":    s.retrieval.lettaTopKCapByMode["fast"],
				},
				"balanced": map[string]any{
					"factor": s.retrieval.lettaTopKFactorByMode["balanced"],
					"cap":    s.retrieval.lettaTopKCapByMode["balanced"],
				},
				"deep": map[string]any{
					"factor": s.retrieval.lettaTopKFactorByMode["deep"],
					"cap":    s.retrieval.lettaTopKCapByMode["deep"],
				},
			},
			"letta_top_k_global": map[string]any{
				"factor": s.retrieval.lettaTopKFactor,
				"cap":    s.retrieval.lettaTopKCap,
			},
			"letta_native_gateway_lane":              true,
			"letta_native_gateway_config_enabled":    s.lettaConfigEnabled(),
			"fail_open_timeout_continuation_enabled": s.retrieval.failOpenContinuationEnabled,
			"timeout_adaptive_skip_enabled":          s.retrieval.timeoutAdaptiveSkipEnabled,
			"adaptive_timeout_enabled":               s.retrieval.adaptiveTimeoutEnabled,
			"adaptive_timeout_min_requests":          s.retrieval.adaptiveTimeoutMinRequests,
			"adaptive_timeout_window":                s.retrieval.adaptiveTimeoutWindow,
			"adaptive_timeout_p95_factor":            s.retrieval.adaptiveTimeoutP95Factor,
			"adaptive_timeout_min_scale":             s.retrieval.adaptiveTimeoutMinScale,
			"adaptive_timeout_max_scale":             s.retrieval.adaptiveTimeoutMaxScale,
			"adaptive_timeout_backlog_weight":        s.retrieval.adaptiveTimeoutBacklogWeight,
			"adaptive_timeout_backlog_cap":           s.retrieval.adaptiveTimeoutBacklogCap,
			"continuation_max_inflight":              s.retrieval.continuationMaxInflight,
			"continuation_max_inflight_per_source":   s.retrieval.continuationMaxInflightPerSource,
			"continuation_max_inflight_overrides":    cloneIntMap(s.retrieval.continuationMaxInflightOverrides),
			"continuation_source_cooldown_secs":      s.retrieval.continuationSourceCooldown.Seconds(),
			"continuation_source_cooldown_by_source_secs": durationMapToSeconds(
				s.retrieval.continuationSourceCooldownBySrc,
			),
			"continuation_source_cooldown_active": s.continuationSourceCooldownSnapshot(),
			"continuation_shedding_enabled":       s.retrieval.continuationSheddingEnabled,
			"continuation_shedding_queue_ratio":   s.retrieval.continuationSheddingQueueRatio,
			"continuation_shedding_pending_high":  s.retrieval.continuationSheddingPendingHigh,
			"continuation_shedding_sources":       mapKeysSorted(s.retrieval.continuationSheddingSources),
			"continuation_durable_enabled":        s.retrieval.continuationDurableEnabled,
			"continuation_durable_dir":            s.retrieval.continuationDurableDir,
			"continuation_durable_max_pending":    s.retrieval.continuationDurableMaxPending,
			"continuation_durable_drain_batch":    s.retrieval.continuationDurableDrainBatch,
			"continuation_durable_poll_secs":      roundFloat(s.retrieval.continuationDurablePollInterval.Seconds(), 3),
			"continuation_durable_retry_base_secs": roundFloat(
				s.retrieval.continuationDurableRetryBase.Seconds(),
				3,
			),
			"continuation_durable_retry_max_secs": roundFloat(
				s.retrieval.continuationDurableRetryMax.Seconds(),
				3,
			),
			"continuation_durable_max_attempts": s.retrieval.continuationDurableMaxAttempts,
			"sync_source_concurrency_default":   s.retrieval.syncSourceConcurrencyDefault,
			"sync_source_concurrency_overrides": cloneIntMap(s.retrieval.syncSourceConcurrencyOverrides),
			"sync_queue_age_warn_secs":          s.retrieval.syncQueueAgeWarnSecs,
			"sync_queue_age_high_secs":          s.retrieval.syncQueueAgeHighSecs,
			"timeout_contract_grace_secs":       s.retrieval.timeoutContractGrace.Seconds(),
			"lexical_guard_enabled":             s.retrieval.lexicalGuardEnabled,
			"lexical_guard_min_coverage":        s.retrieval.lexicalGuardMinCoverage,
			"lexical_guard_min_results":         s.retrieval.lexicalGuardMinResults,
			"runtime_backend_policy":            rustBackendPolicy,
			"traffic_class":                     trafficClass,
			"rust_lane_gate_applied":            rustLaneGateApplied,
			"topic_prefilter_applied":           topicPrefilterApplied,
			"topic_prefilter_hint":              topicPrefilterHint,
			"coverage_rescue_enabled":           s.retrieval.coverageRescueEnabled,
			"coverage_rescue_min_tokens":        s.retrieval.coverageRescueMinTokens,
			"coverage_rescue_applied":           coverageRescueApplied,
			"coverage_rescue_query":             coverageRescueQuery,
			"coverage_rescue_sources":           coverageRescueSources,
			"memory_bank_backend_effective":     strings.TrimSpace(strings.ToLower(anyToString(rustBackendPolicy["memory_bank_backend"]))),
			"rust_quality_fallback_enabled":     s.retrieval.rustQualityFallbackEnabled,
			"rust_quality_fallback_sources":     s.retrieval.rustQualityFallbackSources,
			"rust_quality_fallback_mode":        s.retrieval.rustQualityFallbackMode,
			"source_ownership_mode":             s.retrieval.sourceOwnershipMode,
			"source_ownership_strict_fast_allow_python": mapKeysSorted(
				s.retrieval.sourceOwnershipStrictFastAllowPy,
			),
			"source_chain_debug":         sourceChainDebugPayload,
			"memory_bank_fallback_chain": memoryBankFallbackChain,
		},
		"staged_fetch": map[string]any{
			"enabled":                          true,
			"used":                             true,
			"fast_sources":                     fastSources,
			"slow_sources":                     slowSources,
			"sync_fallback_slow_sources":       normalizeSourceList(syncFallbackSlowSources),
			"async_warm_slow_sources":          asyncWarmSlowSources,
			"warming_sources":                  warmingSources,
			"fail_open_continuation_sources":   continuationSources,
			"continuation_unavailable_sources": continuationUnavailable,
			"continuation_durable":             continuationDurable,
			"timeout_adaptive_skipped_sources": skippedList,
			"timed_out_sources":                timedOutList,
			"budget_exceeded_sources":          budgetExceededList,
			"lexical_backend":                  lexicalBackend,
			"lexical_guard_applied":            lexicalGuardApplied,
			"lexical_guard_coverage":           lexicalGuardCoverage,
			"rust_quality_fallback_attempted":  rustQualityFallbackAttempted,
			"rust_quality_fallback_applied":    rustQualityFallbackApplied,
			"rust_quality_fallback_sources":    rustQualityFallbackSourcesUsed,
			"rust_quality_fallback_mode":       rustQualityFallbackModeUsed,
			"effective_timeout_secs":           effectiveTimeouts,
			"adaptive_timeout_budget":          adaptiveBudgets,
			"coverage_rescue_applied":          coverageRescueApplied,
			"coverage_rescue_query":            coverageRescueQuery,
			"coverage_rescue_sources":          coverageRescueSources,
		},
	}

	response := map[string]any{
		"results":             merged,
		"retrieval_debug":     debug,
		"warnings":            dedupeWarnings(warnings),
		"result_state":        resultState,
		"retrieval_lifecycle": lifecycle,
		"route_owner_class":   sourceOwnerGoNative,
		"source_owner_class":  sourceOwnerClass(sourceOwnerBySource),
		"fallback_counts": map[string]any{
			"python_hot_path_total": s.pythonHotPathFallbacks.Load(),
		},
		"timeout_contract_violations": s.timeoutContractViolations.Load(),
		"drift":                       s.driftSnapshot(),
		"source_summary": map[string]any{
			"sources":                          resolvedSources,
			"returned_now":                     returnedSources,
			"pending_sources":                  deferredCandidates,
			"warming_sources":                  warmingSources,
			"continuation_unavailable_sources": continuationUnavailable,
			"continuation_durable":             continuationDurable,
			"timed_out_sources":                timedOutList,
			"failed_sources":                   failedSources,
			"budget_exceeded_sources":          budgetExceededList,
			"skipped_sources":                  skippedList,
			"source_owners":                    sourceOwnerBySource,
		},
	}
	if len(continuationSources) > 0 && continuationToken != "" {
		response["continuation_async"] = map[string]any{
			"token":               continuationToken,
			"events_url":          "/memory/search/continuations/" + continuationToken + "/events",
			"pending_sources":     continuationSources,
			"unavailable_sources": continuationUnavailable,
			"heartbeat_secs":      s.retrieval.continuationSSEHeartbeat.Seconds(),
		}
	} else if len(continuationUnavailable) > 0 && continuationToken != "" {
		response["continuation_async"] = map[string]any{
			"token":               continuationToken,
			"events_url":          "/memory/search/continuations/" + continuationToken + "/events",
			"pending_sources":     []string{},
			"unavailable_sources": continuationUnavailable,
			"heartbeat_secs":      s.retrieval.continuationSSEHeartbeat.Seconds(),
		}
	}
	if includeGrounding {
		response["grounding"] = buildGrounding(merged)
	}
	return response, http.StatusOK, nil
}

func (s *server) retrievalQuery(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_disabled") {
			return
		}
		s.proxy(w, r)
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "invalid_json") {
			return
		}
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	response, status, execErr := s.executeRetrieval(r.Context(), incomingHeaders, payload, false)
	if execErr != nil {
		log.Printf("staged retrieval query failed; falling back to backend proxy: %s", execErr)
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_exec_error") {
			return
		}
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	writeJSON(w, status, response)
}

func (s *server) retrievalQueryWithGrounding(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_disabled") {
			return
		}
		s.proxy(w, r)
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "invalid_json") {
			return
		}
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	response, status, execErr := s.executeRetrieval(r.Context(), incomingHeaders, payload, true)
	if execErr != nil {
		log.Printf("staged retrieval query-with-grounding failed; falling back to backend proxy: %s", execErr)
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_exec_error") {
			return
		}
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	writeJSON(w, status, response)
}

func (s *server) memorySearch(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_disabled") {
			return
		}
		s.proxy(w, r)
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "invalid_json") {
			return
		}
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	includeGrounding := anyToBool(payload["include_grounding"])
	response, status, execErr := s.executeRetrieval(r.Context(), incomingHeaders, payload, includeGrounding)
	if execErr != nil {
		log.Printf("staged memory/search failed; falling back to backend proxy: %s", execErr)
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_exec_error") {
			return
		}
		s.proxyWithBody(w, r, bodyBytes)
		return
	}
	retrievalMode := normalizeRetrievalMode(anyToString(payload["retrieval_mode"]))
	retrievalIntent := strings.TrimSpace(strings.ToLower(anyToString(payload["retrieval_intent"])))
	if retrievalIntent == "" {
		retrievalIntent = "decision"
	}
	trafficClass := strings.TrimSpace(strings.ToLower(anyToString(payload["traffic_class"])))
	if trafficClass == "" {
		trafficClass = "user"
	}
	response["learning_enabled"] = true
	response["retrieval_mode"] = retrievalMode
	response["retrieval_intent"] = retrievalIntent
	response["traffic_class"] = trafficClass
	if agentID := strings.TrimSpace(anyToString(payload["agent_id"])); agentID != "" {
		response["agent_id"] = agentID
	}
	resultState := strings.TrimSpace(strings.ToLower(anyToString(response["result_state"])))
	response["degraded"] = resultState == "degraded"
	writeJSON(w, status, response)
}

func (s *server) retrievalBatchQuery(w http.ResponseWriter, r *http.Request) {
	if !s.retrieval.enabled {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "staged_disabled") {
			return
		}
		s.proxy(w, r)
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		if !s.allowPythonHotPathFallback(w, r.URL.Path, "invalid_json") {
			return
		}
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
			incomingHeaders,
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
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/status", s.status)
	mux.HandleFunc("/v1/info", s.info)
	mux.HandleFunc("/v1/codex/preflight", s.codexPreflight)
	mux.HandleFunc("/v1/agents/preflight", s.agentsPreflight)
	mux.HandleFunc("/v1/inference/route", s.inferenceRouteHandler)
	mux.HandleFunc("/v1/inference/chat", s.inferenceChatHandler)
	mux.HandleFunc("/v1/inference/embedding-policy", s.inferenceEmbeddingPolicyHandler)
	// Retrieval + memory engine API (go-first ingress, python fallback backend).
	mux.HandleFunc("/v1/retrieval/query", s.retrievalQuery)
	mux.HandleFunc("/v1/retrieval/query-with-grounding", s.retrievalQueryWithGrounding)
	mux.HandleFunc("/v1/retrieval/batch-query", s.retrievalBatchQuery)
	mux.HandleFunc("/v1/retrieval/health", s.retrievalHealth)
	mux.HandleFunc("/memory/search/continuations/", s.memorySearchContinuationsRoute)
	mux.HandleFunc("/memory/search", s.memorySearch)
	mux.HandleFunc("/memory/search/async/", s.memorySearchAsyncStatus)
	mux.HandleFunc("/memory/search/jobs/", s.memorySearchJobsRoute)
	mux.HandleFunc("/memory/write", s.memoryWrite)
	mux.HandleFunc("/memory/recall/eval-cases", s.memoryRecallEvalCases)
	mux.HandleFunc("/memory/recall/eval-cases/refresh", s.memoryRecallEvalCasesRefresh)
	mux.HandleFunc("/memory/recall/evaluate/saved", s.memoryRecallEvaluateSaved)
	mux.HandleFunc("/memory/write/batch", s.memoryWriteBatch)
	mux.HandleFunc("/memory/recent", s.memoryRecent)
	mux.HandleFunc("/memory/files/", s.memoryFilesByProject)
	mux.HandleFunc("/memory/profiles", s.memoryProfiles)
	mux.HandleFunc("/memory/profiles/", s.memoryProfilesByID)
	mux.HandleFunc("/memory/continuity/snapshot", s.memoryContinuitySnapshot)
	mux.HandleFunc("/memory/continuity/snapshots", s.memoryContinuitySnapshots)
	mux.HandleFunc("/memory/continuity/snapshots/", s.memoryContinuitySnapshotByID)
	mux.HandleFunc("/memory/topics", s.memoryTopicTree)
	mux.HandleFunc("/memory/topics/list", s.memoryTopicList)
	mux.HandleFunc("/memory/topic-rollups", s.memoryTopicRollups)
	mux.HandleFunc("/memory/browser-context", s.memoryBrowserContext)
	mux.HandleFunc("/memory/context-pack", s.memoryContextPack)
	mux.HandleFunc("/feedback", s.feedbackRoute)
	mux.HandleFunc("/agents/tasks", s.agentsTasksRoute)
	mux.HandleFunc("/agents/tasks/", s.agentsTasksRoute)
	mux.HandleFunc("/telemetry/storage", s.storageTelemetry)
	mux.HandleFunc("/telemetry/metrics", s.telemetryMetricsRoute)
	mux.HandleFunc("/telemetry/retrieval", s.telemetryRetrievalRoute)
	mux.HandleFunc("/telemetry/retrieval/source-quality", s.telemetryRetrievalSourceQualityRoute)
	mux.HandleFunc("/telemetry/trading", s.telemetryTradingRoute)
	mux.HandleFunc("/telemetry/trading/history", s.telemetryTradingHistoryRoute)
	mux.HandleFunc("/telemetry/", s.telemetryRoute)
	mux.HandleFunc("/maintenance/storage/run", s.storageMaintenanceRun)
	mux.HandleFunc("/maintenance/telemetry/blob-gc", s.telemetryBlobGC)
	mux.HandleFunc("/maintenance/", s.maintenanceRoute)
	mux.HandleFunc("/ops/queue/status", s.opsQueueStatus)
	mux.HandleFunc("/ops/capabilities", s.opsCapabilities)
	mux.HandleFunc("/tools/capability_map", s.toolsCapabilityMap)
	mux.HandleFunc("/tools/ops_queue_status", s.toolsOpsQueueStatus)
	mux.HandleFunc("/tools/memory_write_batch", s.toolsMemoryWriteBatch)
	mux.HandleFunc("/tools/feedback_submit", s.toolsFeedbackSubmit)
	mux.HandleFunc("/v1/memory/put", s.memoryPut)
	mux.HandleFunc("/v1/memory/update", s.memoryV1Update)
	mux.HandleFunc("/v1/memory/get", s.memoryV1Get)
	mux.HandleFunc("/v1/memory/neighbors", s.memoryV1Neighbors)
	mux.HandleFunc("/v1/memory/batch-put", s.memoryBatchPut)
	mux.HandleFunc("/migration/runtime", s.migrationRuntime)
	return mux
}

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8091"
	}
	bindHost := strings.TrimSpace(os.Getenv("HOST_BIND_ADDRESS"))
	if bindHost == "" {
		bindHost = "0.0.0.0"
	}
	listenAddr := net.JoinHostPort(bindHost, port)
	listenNetwork := strings.TrimSpace(strings.ToLower(os.Getenv("GO_GATEWAY_LISTEN_NETWORK")))
	if listenNetwork == "" {
		listenNetwork = "tcp4"
	}
	srv := newServer()
	mux := buildMux(srv)
	listener, err := net.Listen(listenNetwork, listenAddr)
	if err != nil && listenNetwork != "tcp" {
		log.Printf("gateway-go listen fallback: network=%s addr=%s err=%v", listenNetwork, listenAddr, err)
		listenNetwork = "tcp"
		listener, err = net.Listen(listenNetwork, listenAddr)
	}
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("gateway-go listening on %s (%s)", listenAddr, listenNetwork)
	if err := http.Serve(listener, mux); err != nil {
		log.Fatal(err)
	}
}
