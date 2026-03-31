package main

import (
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func envBoolAny(fallback bool, names ...string) bool {
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		return envBool(name, fallback)
	}
	return fallback
}

func envStringAny(fallback string, names ...string) string {
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		raw := strings.TrimSpace(os.Getenv(name))
		if raw != "" {
			return raw
		}
	}
	return fallback
}

func retrievalIntentDefault() string {
	intent := strings.TrimSpace(strings.ToLower(envStringAny("decision", "ORCH_RETRIEVAL_INTENT_DEFAULT")))
	switch intent {
	case "decision", "ops", "raw":
		return intent
	default:
		return "decision"
	}
}

func (s *server) currentContinuationBacklog() (int, map[string]int, int) {
	now := time.Now().UTC()
	bySource := map[string]int{}
	cooldownActive := 0
	total := 0
	s.continuationMu.Lock()
	for source, count := range s.continuationInFlight {
		if count <= 0 {
			continue
		}
		bySource[source] = count
		total += count
	}
	for source, until := range s.continuationSourceCooldownUntil {
		if now.Before(until) {
			if _, ok := bySource[source]; !ok {
				bySource[source] = 0
			}
			cooldownActive += 1
		}
	}
	s.continuationMu.Unlock()
	return total, bySource, cooldownActive
}

func (s *server) capabilityMapPayload() map[string]any {
	useRustCodec := envBoolAny(false, "USE_RUST_CODEC", "ORCH_USE_RUST_CODEC")
	useRustMemory := envBoolAny(false, "USE_RUST_MEMORY", "ORCH_USE_RUST_MEMORY")
	useRustRetrieval := envBoolAny(s.retrieval.enabled, "USE_RUST_RETRIEVAL", "ORCH_USE_RUST_RETRIEVAL")
	securityMode := strings.TrimSpace(strings.ToLower(envStringAny("production", "CONTEXTLATTICE_ENV", "MEMMCP_ENV")))
	if securityMode == "" {
		securityMode = "production"
	}
	securityStrict := envBoolAny(false, "ORCH_SECURITY_STRICT", "MESSAGING_OPENCLAW_STRICT_SECURITY")
	productionRequireAPIKey := envBoolAny(true, "ORCH_PRODUCTION_REQUIRE_API_KEY")

	fastSources := append([]string{}, s.retrieval.fastSources...)
	slowSources := append([]string{}, s.retrieval.slowSources...)
	defaultSources := append([]string{}, s.retrieval.defaultSources...)
	if len(defaultSources) == 0 {
		defaultSources = append(defaultSources, fastSources...)
	}
	if len(defaultSources) == 0 {
		defaultSources = append(defaultSources, "topic_rollups")
	}

	return map[string]any{
		"enabled":       true,
		"defaultRunner": "go_scheduler",
		"runtime": map[string]any{
			"useGoOrchestrator": true,
			"useRustCodec":      useRustCodec,
			"useRustMemory":     useRustMemory,
			"useRustRetrieval":  useRustRetrieval,
		},
		"runnerContracts": map[string]any{
			"statusLifecycle":         []string{"queued", "claimed", "partial", "succeeded", "failed"},
			"allowedActions":          []string{"search", "write", "feedback", "status", "task"},
			"securityMode":            securityMode,
			"securityStrict":          securityStrict,
			"productionRequireApiKey": productionRequireAPIKey,
		},
		"retrieval": map[string]any{
			"stagedEnabled":            s.retrieval.enabled,
			"defaultMode":              normalizeRetrievalMode(envStringAny("balanced", "ORCH_RETRIEVAL_MODE_DEFAULT")),
			"defaultIntent":            retrievalIntentDefault(),
			"defaultSources":           defaultSources,
			"fastSources":              fastSources,
			"slowSources":              slowSources,
			"syncFallbackSources":      append([]string{}, s.retrieval.syncFallbackSources...),
			"rustLanePromotionEnabled": s.retrieval.rustLanePromotionEnabled,
			"topicPrefilterEnabled":    s.retrieval.topicPrefilterEnabled,
			"coverageRescueEnabled":    s.retrieval.coverageRescueEnabled,
		},
		"tools": map[string]any{
			"memory_write_batch":     true,
			"ops_queue_status":       true,
			"feedback_submit":        true,
			"browser_context_ingest": envBoolAny(true, "GO_BROWSER_CONTEXT_INGEST_ENABLED", "ORCH_BROWSER_CONTEXT_INGEST_ENABLED"),
		},
		"integrations": map[string]any{
			"openclaw":              envBoolAny(false, "MESSAGING_INTEGRATIONS_ENABLED"),
			"ironclaw":              envBoolAny(false, "IRONCLAW_INTEGRATION_ENABLED"),
			"messagingIntegrations": envBoolAny(false, "MESSAGING_INTEGRATIONS_ENABLED"),
		},
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	queueDepth, _, cooldownActive := s.currentContinuationBacklog()
	queueCap := cap(s.continuationSem)
	if queueCap < 1 {
		queueCap = 1
	}
	batchSize := 0
	if s.retrieval.telemetryBatchEnabled {
		batchSize = s.retrieval.telemetryBatchSize
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"timestamp": nowUTCISO(),
		"telemetry": map[string]any{
			"queueDepth":              queueDepth,
			"batchSize":               batchSize,
			"continuationMaxInflight": queueCap,
			"continuationCooldowns":   cooldownActive,
		},
		"trading": map[string]any{
			"updatedAt":     nil,
			"openPositions": 0,
		},
		"sidecar": map[string]any{
			"healthy": nil,
			"detail":  "gateway-go",
		},
		"service": "gateway-go",
	})
}

func (s *server) opsCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.capabilityMapPayload())
}

type queueDeadletterRow struct {
	at        string
	source    string
	project   string
	fileName  string
	topicPath string
	detail    string
}

func (s *server) collectQueueDeadletters(limit int, target string) ([]map[string]any, map[string]int) {
	items := make([]queueDeadletterRow, 0)
	counts := map[string]int{}
	if s.telemetryRing == nil {
		return []map[string]any{}, counts
	}
	entries := s.telemetryRing.debugEntries()
	for _, entry := range entries {
		detail := strings.TrimSpace(entry.ingestError)
		if detail == "" {
			detail = strings.TrimSpace(entry.spoolError)
		}
		if detail == "" {
			continue
		}
		source := strings.TrimSpace(strings.ToLower(entry.sourcePath))
		if source == "" {
			source = "unknown"
		}
		if target != "" && source != target {
			continue
		}
		counts[source] = counts[source] + 1
		items = append(items, queueDeadletterRow{
			at:        entry.insertedAt.UTC().Format(time.RFC3339Nano),
			source:    source,
			project:   entry.project,
			fileName:  entry.fileName,
			topicPath: entry.topicPath,
			detail:    detail,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].at > items[j].at
	})
	if limit < len(items) {
		items = items[:limit]
	}
	out := make([]map[string]any, 0, len(items))
	for _, row := range items {
		out = append(out, map[string]any{
			"at":         row.at,
			"target":     row.source,
			"project":    row.project,
			"file":       row.fileName,
			"topic_path": row.topicPath,
			"detail":     row.detail,
		})
	}
	return out, counts
}

func parseOptionalBoolQuery(raw string, fallback bool) bool {
	value, ok := parseBoolLoose(raw)
	if !ok {
		return fallback
	}
	return value
}

func parseOptionalIntQuery(raw string, fallback int, min int, max int) int {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return fallback
	}
	value, err := strconv.Atoi(candidate)
	if err != nil {
		return fallback
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

func parseOptionalFloatQuery(raw string, fallback float64, min float64, max float64) float64 {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(candidate, 64)
	if err != nil {
		return fallback
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

func (s *server) buildQueueStatusPayload(
	includeDeadletters bool,
	deadletterLimit int,
	deadletterTarget string,
	queueHighWatermark float64,
	pendingHighThreshold int,
	retryingHighThreshold int,
) map[string]any {
	now := nowUTCISO()
	pending, bySource, cooldownActive := s.currentContinuationBacklog()
	queueMax := cap(s.continuationSem)
	if queueMax < 1 {
		queueMax = 1
	}
	queueRatio := float64(pending) / float64(queueMax)
	ringSnapshot := map[string]any{}
	if s.telemetryRing != nil {
		ringSnapshot = s.telemetryRing.snapshot()
	}
	spoolSnapshot := map[string]any{}
	if s.telemetrySpool != nil {
		spoolSnapshot = s.telemetrySpool.snapshot()
	}
	deadletters := []map[string]any{}
	deadlettersByTarget := map[string]int{}
	if includeDeadletters {
		deadletters, deadlettersByTarget = s.collectQueueDeadletters(deadletterLimit, deadletterTarget)
	}
	nextActions := make([]string, 0, 4)
	if pending >= pendingHighThreshold {
		nextActions = append(nextActions, "Continuation backlog is elevated; keep staged fetch and reduce low-value deep reads until queue normalizes.")
	}
	if queueRatio >= queueHighWatermark {
		nextActions = append(nextActions, "Continuation in-flight ratio crossed high watermark; increase max inflight only if host headroom is available.")
	}
	if cooldownActive > 0 {
		nextActions = append(nextActions, "One or more sources are in cooldown due to prior timeout pressure; rely on continuation events for late arrivals.")
	}
	if len(deadletters) > 0 {
		nextActions = append(nextActions, "Telemetry ingest deadletters detected in ring buffer; inspect sink health and spool fallback durability.")
	}
	if len(nextActions) == 0 {
		nextActions = append(nextActions, "Queue pressure is within configured limits; continue monitoring trend snapshots.")
	}

	return map[string]any{
		"updatedAt": now,
		"queue": map[string]any{
			"pending":               pending,
			"retrying":              0,
			"running":               0,
			"succeeded":             0,
			"failed":                len(deadletters),
			"totalOutstanding":      pending,
			"pendingRaw":            pending,
			"retryingRaw":           0,
			"runningRaw":            0,
			"succeededRaw":          0,
			"failedRaw":             len(deadletters),
			"memoryWriteQueueDepth": pending,
			"memoryWriteQueueMax":   queueMax,
			"memoryWriteQueueRatio": queueRatio,
			"highWatermark":         queueHighWatermark,
			"pendingHighThreshold":  pendingHighThreshold,
			"retryingHighThreshold": retryingHighThreshold,
			"highWatermarkExceeded": queueRatio >= queueHighWatermark,
			"bySource":              bySource,
			"cooldownActive":        cooldownActive,
		},
		"deadletters": map[string]any{
			"included": includeDeadletters,
			"count":    len(deadletters),
			"byTarget": deadlettersByTarget,
			"items":    deadletters,
		},
		"trend": map[string]any{
			"snapshotAt":      now,
			"queueDepth":      pending,
			"outstanding":     pending,
			"processed":       anyToInt(ringSnapshot["accepted"], 0),
			"dropped":         anyToInt(ringSnapshot["dropped"], 0),
			"lastProcessedAt": nil,
			"lastBatchSize":   0,
		},
		"backpressure": map[string]any{
			"enabled":            true,
			"targets":            []string{"continuations"},
			"queueHighWatermark": queueHighWatermark,
			"maxSleepSecs":       0.0,
		},
		"lettaAdmission": map[string]any{
			"enabled":     true,
			"softLimit":   pendingHighThreshold,
			"hardLimit":   queueMax,
			"backlog":     bySource[sourceLetta],
			"lastReason":  nil,
			"lastBacklog": bySource[sourceLetta],
		},
		"health": map[string]any{
			"lastError":       nil,
			"lastProcessedAt": nil,
			"spool":           spoolSnapshot,
			"ring":            ringSnapshot,
		},
		"nextActions": nextActions,
	}
}

func (s *server) opsQueueStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	query := r.URL.Query()
	includeDeadletters := parseOptionalBoolQuery(query.Get("include_deadletters"), true)
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
