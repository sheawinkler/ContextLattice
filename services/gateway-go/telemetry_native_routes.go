package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func defaultTelemetryMetricsState() map[string]any {
	return map[string]any{
		"updatedAt":  nil,
		"queueDepth": 0,
		"batchSize":  0,
		"totals": map[string]any{
			"enqueued":      0,
			"dropped":       0,
			"batches":       0,
			"flushedEvents": 0,
		},
	}
}

func defaultTradingState() map[string]any {
	return map[string]any{
		"updatedAt":           nil,
		"openPositions":       0,
		"totalValueUsd":       0.0,
		"unrealizedPnl":       0.0,
		"realizedPnl":         0.0,
		"dailyPnl":            0.0,
		"positions":           []any{},
		"priceCacheEntries":   0,
		"priceCacheMaxAge":    0.0,
		"priceCacheTtl":       0.0,
		"priceCacheFreshness": 0.0,
		"priceCachePenalty":   1.0,
	}
}

func cloneStringAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func normalizeTrafficClass(raw string) string {
	token := strings.TrimSpace(strings.ToLower(raw))
	if token == "" {
		return "user"
	}
	if token == "all" {
		return "all"
	}
	return token
}

func (s *server) telemetryMetricsSnapshot() map[string]any {
	s.telemetryMetricsMu.Lock()
	defer s.telemetryMetricsMu.Unlock()
	payload := cloneStringAnyMap(s.telemetryMetricsState)
	if _, ok := payload["updatedAt"]; !ok {
		payload["updatedAt"] = nil
	}
	if _, ok := payload["queueDepth"]; !ok {
		payload["queueDepth"] = 0
	}
	if _, ok := payload["batchSize"]; !ok {
		payload["batchSize"] = 0
	}
	totals, ok := payload["totals"].(map[string]any)
	if !ok {
		totals = map[string]any{}
	}
	payload["totals"] = map[string]any{
		"enqueued":      anyToInt(totals["enqueued"], 0),
		"dropped":       anyToInt(totals["dropped"], 0),
		"batches":       anyToInt(totals["batches"], 0),
		"flushedEvents": anyToInt(totals["flushedEvents"], 0),
	}
	return payload
}

func (s *server) telemetryMetricsRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}

	if r.Method == http.MethodPost {
		rawBody, err := readRequestBody(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		payload, err := parseJSONMap(rawBody)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
			return
		}
		timestamp := strings.TrimSpace(anyToString(payload["timestamp"]))
		if timestamp == "" {
			timestamp = nowUTCISO()
		}
		queueDepth := anyToInt(payload["queueDepth"], 0)
		if queueDepth < 0 {
			queueDepth = 0
		}
		batchSize := anyToInt(payload["batchSize"], 0)
		if batchSize < 0 {
			batchSize = 0
		}
		totalsPayload, _ := payload["totals"].(map[string]any)
		totals := map[string]any{
			"enqueued":      maxInt(0, anyToInt(totalsPayload["enqueued"], 0)),
			"dropped":       maxInt(0, anyToInt(totalsPayload["dropped"], 0)),
			"batches":       maxInt(0, anyToInt(totalsPayload["batches"], 0)),
			"flushedEvents": maxInt(0, anyToInt(totalsPayload["flushedEvents"], 0)),
		}
		s.telemetryMetricsMu.Lock()
		s.telemetryMetricsState = map[string]any{
			"updatedAt":  timestamp,
			"queueDepth": queueDepth,
			"batchSize":  batchSize,
			"totals":     totals,
		}
		s.telemetryMetricsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	payload := s.telemetryMetricsSnapshot()
	gate := _inferenceFastembedGateStatus()
	enabledByFlag := _inferenceFastembedAdapterEnabledByFlag()
	enabled := _inferenceFastembedAdapterEnabled()
	payload["embeddingCache"] = map[string]any{
		"fastembedRs": map[string]any{
			"enabled":       enabled,
			"enabledByFlag": enabledByFlag,
			"configured":    strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_BASE_URL")) != "",
			"timeoutSecs":   envDurationSeconds("ORCH_FASTEMBED_RS_TIMEOUT_SECS", 2.5).Seconds(),
			"route":         strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_ROUTE")),
			"gate":          gate,
			"attempts":      0,
			"successes":     0,
			"failures":      0,
			"fallbacks":     0,
			"batchCalls":    0,
			"batchItems":    0,
			"batchFailures": 0,
			"lastError":     nil,
			"lastLatencyMs": nil,
		},
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) recordMemoryWriteTelemetry(startedAt time.Time, succeeded int, dropped int) {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	if succeeded < 0 {
		succeeded = 0
	}
	if dropped < 0 {
		dropped = 0
	}
	latencyMs := float64(time.Since(startedAt).Microseconds()) / 1000.0
	if latencyMs < 0 {
		latencyMs = 0
	}

	s.memoryTelemetryMu.Lock()
	defer s.memoryTelemetryMu.Unlock()
	s.memoryTelemetryLastWriteAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.memoryTelemetryLastWriteLatency = roundFloat(latencyMs, 3)
	s.memoryTelemetryProcessed += int64(succeeded)
	s.memoryTelemetryDropped += int64(dropped)
}

func (s *server) memoryWriteTelemetrySnapshot() (map[string]any, int, int) {
	s.memoryTelemetryMu.Lock()
	defer s.memoryTelemetryMu.Unlock()
	lastWriteAt := any(nil)
	lastWriteLatency := any(nil)
	if strings.TrimSpace(s.memoryTelemetryLastWriteAt) != "" {
		lastWriteAt = s.memoryTelemetryLastWriteAt
		lastWriteLatency = s.memoryTelemetryLastWriteLatency
	}
	return map[string]any{
		"lastWriteAt":        lastWriteAt,
		"lastWriteLatencyMs": lastWriteLatency,
		"processed":          s.memoryTelemetryProcessed,
		"dropped":            s.memoryTelemetryDropped,
		"secretFilter": map[string]any{
			"mode":       writeSecretsStorageMode(),
			"findings":   s.writeSecretFindings.Load(),
			"redactions": s.writeSecretRedactions.Load(),
			"blocked":    s.writeSecretBlocked.Load(),
		},
	}, int(s.memoryTelemetryProcessed), int(s.memoryTelemetryDropped)
}

func fanoutSemaphoreDepth(sem chan struct{}) (int, int) {
	if sem == nil {
		return 0, 0
	}
	return len(sem), cap(sem)
}

func (s *server) telemetryMemoryPayload() map[string]any {
	metricsSnapshot := s.telemetryMetricsSnapshot()
	totals, _ := metricsSnapshot["totals"].(map[string]any)
	writeSnapshot, writeProcessed, writeDropped := s.memoryWriteTelemetrySnapshot()

	qdrantDepth, qdrantMax := fanoutSemaphoreDepth(s.qdrantWriteFanoutSem)
	pgvectorDepth, pgvectorMax := fanoutSemaphoreDepth(s.pgvectorWriteFanoutSem)
	qdrantPreflightStatus, qdrantPreflightEnabled := qdrantWriteFanoutPreflightStatus()
	pgvectorPreflightStatus, pgvectorPreflightEnabled := pgvectorWriteFanoutPreflightStatus()
	if qdrantPreflightStatus == "" && qdrantPreflightEnabled {
		qdrantPreflightStatus = "ready"
	}
	if pgvectorPreflightStatus == "" && pgvectorPreflightEnabled {
		pgvectorPreflightStatus = "ready"
	}

	queueDepth := maxInt(0, anyToInt(metricsSnapshot["queueDepth"], 0))
	queueMax := maxInt(0, anyToInt(metricsSnapshot["batchSize"], 0))
	if queueMax == 0 && s.retrieval.telemetryBatchSize > 0 {
		queueMax = s.retrieval.telemetryBatchSize
	}
	processed := writeProcessed
	if processed == 0 {
		processed = maxInt(0, anyToInt(totals["flushedEvents"], 0))
	}
	dropped := writeDropped
	if dropped == 0 {
		dropped = maxInt(0, anyToInt(totals["dropped"], 0))
	}

	fanoutTargets := map[string]any{
		sourceQdrant: map[string]any{
			"mode":          writeQdrantFanoutMode(),
			"enabled":       qdrantPreflightEnabled && writeQdrantFanoutMode() != "disabled",
			"status":        qdrantPreflightStatus,
			"queueDepth":    qdrantDepth,
			"queueMax":      qdrantMax,
			"timeoutSecs":   writeQdrantFanoutTimeout().Seconds(),
			"runtimeOwner":  sourceOwnerGoNative,
			"adapterSource": sourceQdrant,
		},
		sourcePgvector: map[string]any{
			"mode":          writePgvectorFanoutMode(),
			"enabled":       pgvectorPreflightEnabled && writePgvectorFanoutMode() != "disabled",
			"status":        pgvectorPreflightStatus,
			"queueDepth":    pgvectorDepth,
			"queueMax":      pgvectorMax,
			"timeoutSecs":   writePgvectorFanoutTimeout().Seconds(),
			"runtimeOwner":  sourceOwnerGoNative,
			"adapterSource": sourcePgvector,
		},
	}

	lastWriteAt := writeSnapshot["lastWriteAt"]
	lastWriteLatency := writeSnapshot["lastWriteLatencyMs"]
	updatedAt := nowUTCISO()
	if lastWriteAt != nil {
		updatedAt = anyToString(lastWriteAt)
	} else if timestamp := strings.TrimSpace(anyToString(metricsSnapshot["updatedAt"])); timestamp != "" {
		updatedAt = timestamp
	}

	return map[string]any{
		"ok":                      true,
		"source":                  "gateway-go",
		"runtimeOwner":            sourceOwnerGoNative,
		"runtime":                 sourceOwnerGoNative,
		"strictRuntimeCompatible": true,
		"updatedAt":               updatedAt,
		"lastWriteAt":             lastWriteAt,
		"lastWriteLatencyMs":      lastWriteLatency,
		"secretFilter":            writeSnapshot["secretFilter"],
		"memoryBank": map[string]any{
			"queueDepth": queueDepth,
			"queueMax":   queueMax,
			"workers":    maxInt(1, s.writePolicy.batchConcurrency),
			"processed":  processed,
			"dropped":    dropped,
		},
		"fanout": map[string]any{
			"queueDepth": qdrantDepth + pgvectorDepth,
			"queueMax":   qdrantMax + pgvectorMax,
			"workers":    qdrantMax + pgvectorMax,
			"processed":  0,
			"dropped":    0,
			"targets":    fanoutTargets,
		},
		"queues": map[string]any{
			"memory": map[string]any{
				"depth":    queueDepth,
				"capacity": queueMax,
				"owner":    sourceOwnerGoNative,
			},
			"fanout": map[string]any{
				"depth":    qdrantDepth + pgvectorDepth,
				"capacity": qdrantMax + pgvectorMax,
				"owner":    sourceOwnerGoNative,
				"targets":  fanoutTargets,
			},
		},
		"telemetryMetrics": metricsSnapshot,
		"feedbackSubmit":   s.feedbackStore.feedbackSubmitStatus(),
	}
}

func (s *server) telemetryMemoryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.telemetryMemoryPayload())
}

type sourceTelemetryStats struct {
	Requests       int
	Timeouts       int
	Errors         int
	BudgetExceeded int
	AvgMs          float64
	P50Ms          float64
	P95Ms          float64
	P99Ms          float64
}

func (s *server) retrievalSourceStatsSnapshot() ([]string, map[string]sourceTelemetryStats) {
	snapshot := map[string]adaptiveSourceStats{}
	s.adaptiveMu.Lock()
	for key, value := range s.adaptiveBySource {
		if value == nil {
			continue
		}
		snapshot[key] = adaptiveSourceStats{
			latencyMs:      append([]float64(nil), value.latencyMs...),
			requests:       value.requests,
			timeouts:       value.timeouts,
			errors:         value.errors,
			budgetExceeded: value.budgetExceeded,
		}
	}
	s.adaptiveMu.Unlock()

	observedSources := make([]string, 0, len(snapshot))
	for source := range snapshot {
		observedSources = append(observedSources, source)
	}
	sort.Strings(observedSources)
	orderedSources := orderedSourceUnion(
		s.retrieval.defaultSources,
		s.retrieval.fastSources,
		s.retrieval.slowSources,
		s.retrieval.syncFallbackSources,
		observedSources,
	)
	if len(orderedSources) == 0 {
		orderedSources = append([]string{}, defaultAllSources...)
	}

	out := make(map[string]sourceTelemetryStats, len(orderedSources))
	for _, source := range orderedSources {
		entry, ok := snapshot[source]
		if !ok {
			out[source] = sourceTelemetryStats{}
			continue
		}
		latencyValues := append([]float64(nil), entry.latencyMs...)
		sort.Float64s(latencyValues)
		avgMs := 0.0
		for _, value := range latencyValues {
			avgMs += value
		}
		if len(latencyValues) > 0 {
			avgMs = avgMs / float64(len(latencyValues))
		}
		errorsCount := entry.errors
		if entry.timeouts > errorsCount {
			errorsCount = entry.timeouts
		}
		out[source] = sourceTelemetryStats{
			Requests:       maxInt(0, entry.requests),
			Timeouts:       maxInt(0, entry.timeouts),
			Errors:         maxInt(0, errorsCount),
			BudgetExceeded: maxInt(0, entry.budgetExceeded),
			AvgMs:          roundFloat(avgMs, 3),
			P50Ms:          roundFloat(percentileFloat(latencyValues, 0.50), 3),
			P95Ms:          roundFloat(percentileFloat(latencyValues, 0.95), 3),
			P99Ms:          roundFloat(percentileFloat(latencyValues, 0.99), 3),
		}
	}
	return orderedSources, out
}

func (s *server) retrievalDegradedSources(order []string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, source := range order {
		status, owner, detail := s.strictRuntimeLaneStatus(source)
		if strings.EqualFold(strings.TrimSpace(status), "healthy") {
			continue
		}
		out[source] = map[string]any{
			"status": status,
			"owner":  owner,
			"detail": detail,
		}
	}
	return out
}

func buildRetrievalAlerts(
	order []string,
	statsBySource map[string]sourceTelemetryStats,
) []map[string]any {
	alerts := make([]map[string]any, 0)
	for _, source := range order {
		stats := statsBySource[source]
		if stats.Requests <= 0 {
			continue
		}
		timeoutRate := float64(stats.Timeouts) / float64(stats.Requests)
		errorRate := float64(stats.Errors) / float64(stats.Requests)
		if timeoutRate >= 0.25 {
			alerts = append(alerts, map[string]any{
				"severity": "warn",
				"source":   source,
				"message":  "Source timeout rate is above 25%",
				"kind":     "timeout_rate",
			})
		}
		if errorRate >= 0.25 {
			alerts = append(alerts, map[string]any{
				"severity": "error",
				"source":   source,
				"message":  "Source error rate is above 25%",
				"kind":     "error_rate",
			})
		}
		if stats.P95Ms >= 25000 {
			alerts = append(alerts, map[string]any{
				"severity": "warn",
				"source":   source,
				"message":  "Source p95 latency is above 25s",
				"kind":     "latency_p95",
			})
		}
	}
	return alerts
}

func (s *server) telemetryRetrievalRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	query := r.URL.Query()
	limit := parseOptionalIntQuery(query.Get("limit"), 20, 1, 500)
	trafficClass := normalizeTrafficClass(query.Get("traffic_class"))
	capturedAt := nowUTCISO()

	order, statsBySource := s.retrievalSourceStatsSnapshot()
	latencySources := map[string]any{}
	qualityBySource := map[string]any{}
	for _, source := range order {
		stats := statsBySource[source]
		timeoutRate := 0.0
		errorRate := 0.0
		if stats.Requests > 0 {
			timeoutRate = float64(stats.Timeouts) / float64(stats.Requests)
			errorRate = float64(stats.Errors) / float64(stats.Requests)
		}
		latencySources[source] = map[string]any{
			"requests":       stats.Requests,
			"timeouts":       stats.Timeouts,
			"errors":         stats.Errors,
			"budgetExceeded": stats.BudgetExceeded,
			"avgMs":          stats.AvgMs,
			"p50Ms":          stats.P50Ms,
			"p95Ms":          stats.P95Ms,
			"p99Ms":          stats.P99Ms,
		}
		qualityBySource[source] = map[string]any{
			"requests":       stats.Requests,
			"timeouts":       stats.Timeouts,
			"errors":         stats.Errors,
			"budgetExceeded": stats.BudgetExceeded,
			"timeoutRate":    roundFloat(timeoutRate, 6),
			"errorRate":      roundFloat(errorRate, 6),
		}
	}

	queueDepth, queueBySource, _ := s.currentContinuationBacklog()
	degradedSources := s.retrievalDegradedSources(order)
	alerts := buildRetrievalAlerts(order, statsBySource)
	latencyClassPayload := map[string]any{
		"updatedAt":    capturedAt,
		"historyLimit": limit,
		"sources":      latencySources,
		"modes":        map[string]any{},
	}
	qualityClassPayload := map[string]any{
		"updatedAt": capturedAt,
		"bySource":  qualityBySource,
		"recent":    []any{},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updatedAt":    capturedAt,
		"trafficClass": trafficClass,
		"latency": map[string]any{
			"updatedAt":      capturedAt,
			"historyLimit":   limit,
			"sources":        latencySources,
			"modes":          map[string]any{},
			"byTrafficClass": map[string]any{"user": latencyClassPayload, "all": latencyClassPayload},
		},
		"recallQuality": map[string]any{
			"updatedAt":      capturedAt,
			"bySource":       qualityBySource,
			"recent":         []any{},
			"byTrafficClass": map[string]any{"user": qualityClassPayload, "all": qualityClassPayload},
		},
		"sourceCircuit": map[string]any{
			"degradedSources": degradedSources,
		},
		"backlogGating": map[string]any{
			"enabled":                  true,
			"queueOutstanding":         queueDepth,
			"lettaOutstandingMax":      queueBySource[sourceLetta],
			"memoryBankOutstandingMax": queueBySource[sourceMemoryBank],
		},
		"alerts": map[string]any{
			"active": alerts,
			"count":  len(alerts),
		},
	})
}

func (s *server) telemetryRetrievalSourceQualityRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	query := r.URL.Query()
	limit := parseOptionalIntQuery(query.Get("limit"), 20, 1, 500)
	windowMins := parseOptionalIntQuery(query.Get("window_mins"), 0, 0, 1440)
	trafficClass := normalizeTrafficClass(query.Get("traffic_class"))
	capturedAt := nowUTCISO()

	order, statsBySource := s.retrievalSourceStatsSnapshot()
	rows := make([]map[string]any, 0, len(order))
	lifetimeRows := make([]map[string]any, 0, len(order))
	baselineErrorRate := 0.0
	if baseline, ok := statsBySource[sourceQdrant]; ok && baseline.Requests > 0 {
		baselineErrorRate = float64(baseline.Errors) / float64(baseline.Requests)
	}
	totalSamples := 0
	for _, source := range order {
		stats := statsBySource[source]
		totalSamples += stats.Requests
		timeoutRate := 0.0
		errorRate := 0.0
		budgetRate := 0.0
		if stats.Requests > 0 {
			timeoutRate = float64(stats.Timeouts) / float64(stats.Requests)
			errorRate = float64(stats.Errors) / float64(stats.Requests)
			budgetRate = float64(stats.BudgetExceeded) / float64(stats.Requests)
		}
		row := map[string]any{
			"source":                 source,
			"requests":               stats.Requests,
			"timeouts":               stats.Timeouts,
			"budgetExceeded":         stats.BudgetExceeded,
			"errors":                 stats.Errors,
			"timeoutRate":            roundFloat(timeoutRate, 6),
			"budgetExceededRate":     roundFloat(budgetRate, 6),
			"errorRate":              roundFloat(errorRate, 6),
			"errorRateDeltaVsQdrant": roundFloat(errorRate-baselineErrorRate, 6),
			"p50Ms":                  stats.P50Ms,
			"p95Ms":                  stats.P95Ms,
			"p99Ms":                  stats.P99Ms,
			"lifetimeRequests":       stats.Requests,
			"lifetimeTimeouts":       stats.Timeouts,
		}
		rows = append(rows, row)
		lifetimeRows = append(lifetimeRows, cloneAnyMap(row))
	}
	sort.Slice(rows, func(i, j int) bool {
		leftTimeout := anyToFloat64(rows[i]["timeoutRate"], 0)
		rightTimeout := anyToFloat64(rows[j]["timeoutRate"], 0)
		if leftTimeout == rightTimeout {
			leftError := anyToFloat64(rows[i]["errorRate"], 0)
			rightError := anyToFloat64(rows[j]["errorRate"], 0)
			if leftError == rightError {
				return anyToFloat64(rows[i]["p95Ms"], 0) > anyToFloat64(rows[j]["p95Ms"], 0)
			}
			return leftError > rightError
		}
		return leftTimeout > rightTimeout
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(lifetimeRows) > limit {
		lifetimeRows = lifetimeRows[:limit]
	}

	recommendations := make([]string, 0, 4)
	if len(rows) > 0 && anyToFloat64(rows[0]["timeoutRate"], 0) >= 0.25 {
		recommendations = append(recommendations, "At least one source has timeout rate >= 25%; keep it out of default blocking path.")
	}
	if hasHighBudgetExceeded(rows, 0.25) {
		recommendations = append(recommendations, "Budget-exceeded rates are elevated for slow sources; use deep mode for completeness-critical reads.")
	}
	if letaRow, ok := findSourceRow(rows, sourceLetta); ok && anyToFloat64(letaRow["timeoutRate"], 0) >= 0.5 {
		recommendations = append(recommendations, "Letta timeout rate remains high; keep staged fetch enabled and avoid blocking reads on Letta.")
	}
	degradedSources := s.retrievalDegradedSources(order)
	if len(degradedSources) > 0 {
		names := make([]string, 0, len(degradedSources))
		for source := range degradedSources {
			names = append(names, source)
		}
		sort.Strings(names)
		recommendations = append(recommendations, "Adaptive source circuit is currently open for: "+strings.Join(names, ", ")+".")
	}
	if len(recommendations) == 0 {
		recommendations = append(recommendations, "Source quality is within expected bounds for current thresholds.")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"updatedAt":      capturedAt,
		"trafficClass":   trafficClass,
		"baselineSource": sourceQdrant,
		"window": map[string]any{
			"minutes":     windowMins,
			"windowSecs":  float64(windowMins) * 60.0,
			"startAt":     nil,
			"endAt":       capturedAt,
			"sampleCount": totalSamples,
			"active":      windowMins > 0 && totalSamples > 0,
		},
		"sources":         rows,
		"lifetimeSources": lifetimeRows,
		"recommendations": recommendations,
	})
}

func hasHighBudgetExceeded(rows []map[string]any, threshold float64) bool {
	for _, row := range rows {
		if anyToFloat64(row["budgetExceededRate"], 0) >= threshold {
			return true
		}
	}
	return false
}

func findSourceRow(rows []map[string]any, source string) (map[string]any, bool) {
	target := strings.TrimSpace(strings.ToLower(source))
	for _, row := range rows {
		if strings.TrimSpace(strings.ToLower(anyToString(row["source"]))) == target {
			return row, true
		}
	}
	return nil, false
}

func normalizeTradingSnapshot(payload map[string]any, now time.Time) map[string]any {
	snapshot := map[string]any{}
	timestamp := strings.TrimSpace(anyToString(payload["timestamp"]))
	if timestamp == "" {
		timestamp = strings.TrimSpace(anyToString(payload["updatedAt"]))
	}
	if timestamp == "" {
		timestamp = now.UTC().Format(time.RFC3339Nano)
	}
	snapshot["timestamp"] = timestamp
	snapshot["open_positions"] = anyToInt(coalesceAny(payload["open_positions"], payload["openPositions"]), 0)
	snapshot["total_value_usd"] = anyToFloat64(coalesceAny(payload["total_value_usd"], payload["totalValueUsd"]), 0)
	snapshot["unrealized_pnl"] = anyToFloat64(coalesceAny(payload["unrealized_pnl"], payload["unrealizedPnl"]), 0)
	snapshot["realized_pnl"] = anyToFloat64(coalesceAny(payload["realized_pnl"], payload["realizedPnl"]), 0)
	snapshot["daily_pnl"] = anyToFloat64(coalesceAny(payload["daily_pnl"], payload["dailyPnl"]), 0)
	positions, ok := coalesceAny(payload["positions"]).([]any)
	if !ok {
		positions = []any{}
	}
	snapshot["positions"] = positions
	snapshot["price_cache_entries"] = anyToInt(coalesceAny(payload["price_cache_entries"], payload["priceCacheEntries"]), 0)
	snapshot["price_cache_max_age"] = anyToFloat64(coalesceAny(payload["price_cache_max_age"], payload["priceCacheMaxAge"]), 0)
	snapshot["price_cache_ttl"] = anyToFloat64(coalesceAny(payload["price_cache_ttl"], payload["priceCacheTtl"]), 0)
	snapshot["price_cache_freshness"] = anyToFloat64(coalesceAny(payload["price_cache_freshness"], payload["priceCacheFreshness"]), 0)
	penalty := anyToFloat64(coalesceAny(payload["price_cache_penalty"], payload["priceCachePenalty"]), 1.0)
	if penalty == 0 {
		penalty = 1.0
	}
	snapshot["price_cache_penalty"] = penalty
	return snapshot
}

func coalesceAny(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if token, ok := value.(string); ok {
			if strings.TrimSpace(token) == "" {
				continue
			}
		}
		return value
	}
	return nil
}

func tradingStateFromSnapshot(snapshot map[string]any) map[string]any {
	state := defaultTradingState()
	state["updatedAt"] = anyToString(snapshot["timestamp"])
	state["openPositions"] = anyToInt(snapshot["open_positions"], 0)
	state["totalValueUsd"] = anyToFloat64(snapshot["total_value_usd"], 0)
	state["unrealizedPnl"] = anyToFloat64(snapshot["unrealized_pnl"], 0)
	state["realizedPnl"] = anyToFloat64(snapshot["realized_pnl"], 0)
	state["dailyPnl"] = anyToFloat64(snapshot["daily_pnl"], 0)
	positions, ok := snapshot["positions"].([]any)
	if !ok {
		positions = []any{}
	}
	state["positions"] = positions
	state["priceCacheEntries"] = anyToInt(snapshot["price_cache_entries"], 0)
	state["priceCacheMaxAge"] = anyToFloat64(snapshot["price_cache_max_age"], 0)
	state["priceCacheTtl"] = anyToFloat64(snapshot["price_cache_ttl"], 0)
	state["priceCacheFreshness"] = anyToFloat64(snapshot["price_cache_freshness"], 0)
	state["priceCachePenalty"] = anyToFloat64(snapshot["price_cache_penalty"], 1.0)
	return state
}

func (s *server) loadTradingHistoryFromDisk() error {
	path := strings.TrimSpace(s.tradingHistoryPath)
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	loaded := make([]map[string]any, 0, s.tradingHistoryLimit)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		snapshot := normalizeTradingSnapshot(row, time.Now().UTC())
		loaded = append(loaded, snapshot)
		if len(loaded) > s.tradingHistoryLimit {
			loaded = append([]map[string]any(nil), loaded[len(loaded)-s.tradingHistoryLimit:]...)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	s.tradingMu.Lock()
	s.tradingHistory = loaded
	if len(loaded) > 0 {
		s.tradingState = tradingStateFromSnapshot(loaded[len(loaded)-1])
	} else {
		s.tradingState = defaultTradingState()
	}
	s.tradingMu.Unlock()
	return nil
}

func (s *server) appendTradingSnapshot(snapshot map[string]any) (int, error) {
	s.tradingMu.Lock()
	s.tradingHistory = append(s.tradingHistory, cloneAnyMap(snapshot))
	if len(s.tradingHistory) > s.tradingHistoryLimit {
		s.tradingHistory = append([]map[string]any(nil), s.tradingHistory[len(s.tradingHistory)-s.tradingHistoryLimit:]...)
	}
	s.tradingState = tradingStateFromSnapshot(snapshot)
	historySize := len(s.tradingHistory)
	s.tradingMu.Unlock()
	return historySize, s.persistTradingSnapshot(snapshot)
}

func (s *server) persistTradingSnapshot(snapshot map[string]any) error {
	path := strings.TrimSpace(s.tradingHistoryPath)
	if path == "" {
		return nil
	}
	lineBytes, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	file, err := openOwnerOnlyAppend(path, false)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(lineBytes, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *server) telemetryTradingRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if r.Method == http.MethodGet {
		s.tradingMu.Lock()
		payload := cloneStringAnyMap(s.tradingState)
		s.tradingMu.Unlock()
		writeJSON(w, http.StatusOK, payload)
		return
	}
	rawBody, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(rawBody)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	snapshot := normalizeTradingSnapshot(payload, time.Now().UTC())
	historySize, persistErr := s.appendTradingSnapshot(snapshot)
	warning := any(nil)
	if persistErr != nil {
		warning = persistErr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"historySize":    historySize,
		"mindsdb_synced": false,
		"warning":        warning,
		"external_sync": map[string]any{
			"enabled": false,
			"targets": []string{},
			"mindsdb": "disabled",
		},
	})
}

func (s *server) telemetryTradingHistoryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > s.tradingHistoryLimit {
		limit = s.tradingHistoryLimit
	}

	s.tradingMu.Lock()
	start := len(s.tradingHistory) - limit
	if start < 0 {
		start = 0
	}
	rows := make([]map[string]any, 0, len(s.tradingHistory)-start)
	for _, row := range s.tradingHistory[start:] {
		rows = append(rows, cloneAnyMap(row))
	}
	s.tradingMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"history": rows})
}

func (s *server) telemetryFanoutRoute(w http.ResponseWriter, r *http.Request) {
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
	queuePayload := s.buildQueueStatusPayload(
		includeDeadletters,
		deadletterLimit,
		deadletterTarget,
		highWatermark,
		pendingThreshold,
		retryingThreshold,
	)
	queue, _ := queuePayload["queue"].(map[string]any)
	deadletters, _ := queuePayload["deadletters"].(map[string]any)

	metricsSnapshot := s.telemetryMetricsSnapshot()
	totals, _ := metricsSnapshot["totals"].(map[string]any)
	enqueued := anyToInt(totals["enqueued"], 0)
	pending := anyToInt(queue["pending"], 0)
	retrying := anyToInt(queue["retrying"], 0)
	deadletterCount := anyToInt(deadletters["count"], 0)
	succeeded := enqueued - pending - retrying - deadletterCount
	if succeeded < 0 {
		succeeded = 0
	}

	lastError := any(nil)
	if items, ok := deadletters["items"].([]map[string]any); ok && len(items) > 0 {
		lastError = anyToString(items[0]["detail"])
	} else if genericItems, ok := deadletters["items"].([]any); ok && len(genericItems) > 0 {
		if first, ok := genericItems[0].(map[string]any); ok {
			lastError = anyToString(first["detail"])
		}
	}

	outboxBackend := strings.TrimSpace(os.Getenv("FANOUT_OUTBOX_BACKEND"))
	if outboxBackend == "" {
		outboxBackend = "sqlite"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"updatedAt":     nowUTCISO(),
		"outboxBackend": outboxBackend,
		"summary": map[string]any{
			"by_status": map[string]any{
				"succeeded":  succeeded,
				"failed":     deadletterCount,
				"deadletter": deadletterCount,
				"retrying":   retrying,
				"pending":    pending,
			},
			"queueDepth": pending,
			"queueMax":   maxInt(1, cap(s.continuationSem)),
			"total":      maxInt(0, succeeded+deadletterCount+retrying+pending),
		},
		"health": map[string]any{
			"lastError": lastError,
			"spool":     s.telemetrySpool.snapshot(),
			"ring":      s.telemetryRing.snapshot(),
		},
		"letta": map[string]any{
			"enabled":                 s.lettaConfigEnabled(),
			"runtimeEnabled":          s.lettaConfigEnabled(),
			"disabledReason":          nil,
			"transientErrorStreak":    0,
			"transientErrorThreshold": 0,
		},
		"lettaAutoPrune": map[string]any{
			"enabled":      envBool("LETTA_AUTO_PRUNE_ENABLED", false),
			"intervalSecs": envDurationSeconds("LETTA_AUTO_PRUNE_INTERVAL_SECS", 60.0).Seconds(),
			"state": map[string]any{
				"lastRunAt":         nil,
				"lastDeleted":       0,
				"lastSkippedReason": nil,
				"lastError":         nil,
			},
		},
	})
}

func (s *server) telemetryRecallRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	trafficClass := normalizeTrafficClass(r.URL.Query().Get("traffic_class"))
	updatedAt := nowUTCISO()
	order, statsBySource := s.retrievalSourceStatsSnapshot()
	bySource := make(map[string]any, len(order))
	totalRequests := 0
	totalTimeouts := 0
	totalErrors := 0
	for _, source := range order {
		stats := statsBySource[source]
		totalRequests += stats.Requests
		totalTimeouts += stats.Timeouts
		totalErrors += stats.Errors
		bySource[source] = map[string]any{
			"requests":       stats.Requests,
			"timeouts":       stats.Timeouts,
			"errors":         stats.Errors,
			"budgetExceeded": stats.BudgetExceeded,
			"p95Ms":          stats.P95Ms,
		}
	}

	sourceErrorRate := 0.0
	if totalRequests > 0 {
		sourceErrorRate = float64(totalErrors) / float64(totalRequests)
	}
	alerts := buildRetrievalAlerts(order, statsBySource)
	monitorRows := s.readRecallMonitorHistory(envInt("ORCH_RECALL_MONITOR_HISTORY_LIMIT", 96))
	latestQuality := latestRecallEvalMonitorSample(monitorRows)
	qualityTotals := map[string]any{
		"requests":          totalRequests,
		"timeouts":          totalTimeouts,
		"errors":            totalErrors,
		"sourceErrorRate":   roundFloat(sourceErrorRate, 6),
		"noHitRate":         0.0,
		"lowConfidenceRate": 0.0,
		"staleHitRate":      0.0,
		"recallAtK":         nil,
		"mrr":               nil,
		"numericExactness":  nil,
		"citationCoverage":  nil,
		"sourceDiversity":   nil,
		"graphLift":         nil,
		"evalP95Ms":         nil,
		"lastEvalAt":        nil,
	}
	qualityStatus := "unknown"
	if latestQuality != nil {
		qualityStatus = strings.TrimSpace(anyToString(latestQuality["qualityStatus"]))
		if qualityStatus == "" {
			qualityStatus = recallQualityStatusFromSample(latestQuality)
		}
		qualityTotals["noHitRate"] = anyToFloat64(latestQuality["noHitRate"], 0.0)
		qualityTotals["lowConfidenceRate"] = anyToFloat64(latestQuality["lowConfidenceRate"], 0.0)
		qualityTotals["staleHitRate"] = anyToFloat64(latestQuality["staleHitRate"], 0.0)
		qualityTotals["recallAtK"] = anyToFloat64(latestQuality["recallAtK"], 0.0)
		qualityTotals["mrr"] = anyToFloat64(latestQuality["mrr"], 0.0)
		qualityTotals["numericExactness"] = anyToFloat64(latestQuality["numericExactness"], 0.0)
		qualityTotals["citationCoverage"] = anyToFloat64(latestQuality["citationCoverage"], 0.0)
		qualityTotals["sourceDiversity"] = anyToFloat64(latestQuality["sourceDiversity"], 0.0)
		qualityTotals["graphLift"] = anyToFloat64(latestQuality["graphLift"], 0.0)
		qualityTotals["evalP95Ms"] = anyToFloat64(latestQuality["evalP95Ms"], 0.0)
		qualityTotals["lastEvalAt"] = latestQuality["timestamp"]
	}
	recentQuality := recentRecallEvalMonitorSamples(monitorRows, 10)
	writeJSON(w, http.StatusOK, map[string]any{
		"updatedAt":    updatedAt,
		"trafficClass": trafficClass,
		"quality": map[string]any{
			"updatedAt":   updatedAt,
			"status":      qualityStatus,
			"totals":      qualityTotals,
			"bySource":    bySource,
			"recent":      recentQuality,
			"sampleCount": len(recentQuality),
			"recommendations": recallTelemetryQualityRecommendations(
				latestQuality,
				totalRequests,
				sourceErrorRate,
			),
		},
		"alerts": map[string]any{
			"thresholds": map[string]any{
				"noHitRate":         envFloat("RECALL_ALERT_NO_HIT_RATE", 0.25),
				"lowConfidenceRate": envFloat("RECALL_ALERT_LOW_CONFIDENCE_RATE", 0.25),
				"staleHitRate":      envFloat("RECALL_ALERT_STALE_HIT_RATE", 0.25),
				"sourceErrorRate":   envFloat("RECALL_ALERT_SOURCE_ERROR_RATE", 0.25),
				"minRequests":       envInt("RECALL_ALERT_MIN_REQUESTS", 10),
			},
			"active": alerts,
			"count":  len(alerts),
		},
	})
}

func (s *server) readRecallMonitorHistory(limit int) []map[string]any {
	if limit < 1 {
		return []map[string]any{}
	}
	path := resolveStoragePath(
		"RECALL_MONITOR_PATH",
		filepath.Join("services", "orchestrator", "data", "recall_monitor.ndjson"),
	)
	if path == "" {
		return []map[string]any{}
	}
	file, err := os.Open(path)
	if err != nil {
		return []map[string]any{}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 1 {
		return []map[string]any{}
	}

	end, complete := recallMonitorLastCompleteLineEnd(file, info.Size())
	if !complete {
		return []map[string]any{}
	}
	// Lines are collected from EOF backwards, then restored to the original
	// oldest-to-newest history order expected by existing telemetry consumers.
	rows := make([]map[string]any, 0, limit)
	scanned := 0
	for end >= 0 && scanned < limit {
		line, start, oversized, ok := recallMonitorTailLine(file, end)
		if !ok || oversized {
			break
		}
		scanned++
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			row := map[string]any{}
			if err := json.Unmarshal(line, &row); err == nil {
				rows = append(rows, row)
			}
		}
		if start == 0 {
			break
		}
		end = start - 1 // the preceding newline terminates the next older line
	}
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	return rows
}

const recallMonitorHistoryMaxLineBytes = 1024 * 1024

// recallMonitorLastCompleteLineEnd returns the byte offset of the newline
// terminating the newest complete row. An unterminated final fragment is never
// parsed as a monitor record.
func recallMonitorLastCompleteLineEnd(file *os.File, size int64) (int64, bool) {
	if size < 1 {
		return 0, false
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, size-1); err != nil {
		return 0, false
	}
	if last[0] == '\n' {
		return size - 1, true
	}
	start := maxInt64(0, size-int64(recallMonitorHistoryMaxLineBytes+1))
	buffer := make([]byte, size-start)
	if _, err := file.ReadAt(buffer, start); err != nil {
		return 0, false
	}
	index := bytes.LastIndexByte(buffer, '\n')
	if index < 0 {
		return 0, false
	}
	return start + int64(index), true
}

// recallMonitorTailLine reads at most one bounded complete line ending at end.
// If the preceding newline is farther than the 1 MiB line bound, callers stop
// rather than scanning or decoding arbitrary historical data.
func recallMonitorTailLine(file *os.File, end int64) ([]byte, int64, bool, bool) {
	if end < 0 {
		return nil, 0, false, false
	}
	start := maxInt64(0, end-int64(recallMonitorHistoryMaxLineBytes+1))
	buffer := make([]byte, end-start)
	if len(buffer) > 0 {
		if _, err := file.ReadAt(buffer, start); err != nil {
			return nil, 0, false, false
		}
	}
	separator := bytes.LastIndexByte(buffer, '\n')
	if separator < 0 {
		if start != 0 {
			return nil, 0, true, true
		}
		return buffer, 0, false, true
	}
	lineStart := start + int64(separator) + 1
	return buffer[separator+1:], lineStart, false, true
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (s *server) syntheticRecallMonitorSample() map[string]any {
	updatedAt := nowUTCISO()
	order, statsBySource := s.retrievalSourceStatsSnapshot()
	alertCount := len(buildRetrievalAlerts(order, statsBySource))
	lettaP95 := 0.0
	if stats, ok := statsBySource[sourceLetta]; ok {
		lettaP95 = stats.P95Ms
	}
	return map[string]any{
		"timestamp":           updatedAt,
		"lettaP95Ms":          roundFloat(lettaP95, 3),
		"retrievalAlertCount": alertCount,
	}
}

func latestRecallEvalMonitorSample(rows []map[string]any) map[string]any {
	for idx := len(rows) - 1; idx >= 0; idx-- {
		row := rows[idx]
		if row == nil {
			continue
		}
		if _, exists := row["recallAtK"]; exists {
			return row
		}
		if _, exists := row["mrr"]; exists {
			return row
		}
	}
	return nil
}

func recentRecallEvalMonitorSamples(rows []map[string]any, limit int) []map[string]any {
	if limit < 1 {
		limit = 1
	}
	out := make([]map[string]any, 0, limit)
	for idx := len(rows) - 1; idx >= 0 && len(out) < limit; idx-- {
		row := rows[idx]
		if row == nil {
			continue
		}
		if _, exists := row["recallAtK"]; !exists {
			if _, exists := row["mrr"]; !exists {
				continue
			}
		}
		out = append(out, row)
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out
}

func recallQualityStatusFromSample(sample map[string]any) string {
	if sample == nil {
		return "unknown"
	}
	if anyToBool(sample["passed"]) {
		return "healthy"
	}
	recallAtK := anyToFloat64(sample["recallAtK"], 0)
	mrr := anyToFloat64(sample["mrr"], 0)
	if recallAtK < 0.5 || mrr < 0.35 {
		return "repair_recommended"
	}
	return "watch"
}

func recallTelemetryQualityRecommendations(sample map[string]any, totalRequests int, sourceErrorRate float64) []string {
	recommendations := make([]string, 0, 5)
	if sample == nil {
		recommendations = append(recommendations, "Run scripts/agent/recall-quality-eval to seed recall quality telemetry.")
	} else {
		recallAtK := anyToFloat64(sample["recallAtK"], 0)
		mrr := anyToFloat64(sample["mrr"], 0)
		citationCoverage := anyToFloat64(sample["citationCoverage"], 1)
		graphLift := anyToFloat64(sample["graphLift"], 0)
		sourceDiversity := anyToFloat64(sample["sourceDiversity"], 0)
		if recallAtK < 0.75 {
			recommendations = append(recommendations, "Recall@K is below the production floor; refresh saved cases and inspect failing queries.")
		}
		if mrr < 0.55 {
			recommendations = append(recommendations, "MRR is below target; tune ranking and staged source ordering before increasing context size.")
		}
		if citationCoverage < 0.9 {
			recommendations = append(recommendations, "Citation coverage is weak; prioritize file-backed hits in context packs.")
		}
		if graphLift > 0 {
			recommendations = append(recommendations, "Graph neighbors improve recall; keep first-hop edge expansion enabled in agent-boundary context packaging.")
		}
		if sourceDiversity < 1.5 {
			recommendations = append(recommendations, "Recall is leaning on too few sources; verify qdrant, pgvector, and topic rollups are healthy.")
		}
	}
	if totalRequests > 0 && sourceErrorRate >= 0.25 {
		recommendations = append(recommendations, "Retrieval source error rate is elevated; inspect /telemetry/retrieval/source-quality.")
	}
	if len(recommendations) == 0 {
		recommendations = append(recommendations, "Recall quality telemetry is inside current production thresholds.")
	}
	return recommendations
}

func (s *server) telemetryRecallMonitorRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	limit := parseOptionalIntQuery(r.URL.Query().Get("limit"), 96, 1, 512)
	rows := s.readRecallMonitorHistory(limit)
	rows = append(rows, s.syntheticRecallMonitorSample())
	if len(rows) > limit {
		rows = append([]map[string]any(nil), rows[len(rows)-limit:]...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updatedAt": nowUTCISO(),
		"history":   rows,
		"count":     len(rows),
		"config": map[string]any{
			"historyLimit":  limit,
			"lookbackHours": envFloat("RECALL_MONITOR_LOOKBACK_HOURS", 24.0),
		},
	})
}

func recallMonitorSamplesForWindow(rows []map[string]any, lookbackHours float64, maxSamples int) []map[string]any {
	if maxSamples < 1 {
		maxSamples = 1
	}
	if len(rows) == 0 {
		return []map[string]any{}
	}
	cutoff := time.Now().UTC().Add(-time.Duration(lookbackHours * float64(time.Hour)))
	selected := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		parsed, ok := parseRecallMonitorSampleTimestamp(row["timestamp"])
		if !ok {
			continue
		}
		if parsed.After(cutoff) || parsed.Equal(cutoff) {
			selected = append(selected, row)
		}
	}
	if len(selected) == 0 {
		if len(rows) <= maxSamples {
			return append([]map[string]any(nil), rows...)
		}
		return append([]map[string]any(nil), rows[len(rows)-maxSamples:]...)
	}
	if len(selected) > maxSamples {
		selected = append([]map[string]any(nil), selected[len(selected)-maxSamples:]...)
	} else {
		selected = append([]map[string]any(nil), selected...)
	}
	return selected
}

func parseRecallMonitorSampleTimestamp(value any) (time.Time, bool) {
	token := strings.TrimSpace(anyToString(value))
	if token == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, token); err == nil {
		return parsed.UTC(), true
	}
	if parsed, err := time.Parse(time.RFC3339, token); err == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}

func recommendRecallRateThreshold(values []float64, current float64, floor float64, ceiling float64) float64 {
	clean := make([]float64, 0, len(values))
	for _, value := range values {
		if value < 0 {
			value = 0
		}
		clean = append(clean, value)
	}
	if len(clean) == 0 {
		return roundFloat(clampFloat(current, floor, ceiling), 6)
	}
	sort.Float64s(clean)
	p95 := percentileFloat(clean, 0.95)
	p99 := percentileFloat(clean, 0.99)
	suggested := maxFloat(current*0.8, maxFloat(p95*1.2, p99*1.05))
	return roundFloat(clampFloat(suggested, floor, ceiling), 6)
}

func recommendRecallLatencyThreshold(values []float64, current float64, floor float64) float64 {
	clean := make([]float64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		clean = append(clean, value)
	}
	if len(clean) == 0 {
		return roundFloat(maxFloat(current, floor), 3)
	}
	sort.Float64s(clean)
	p95 := percentileFloat(clean, 0.95)
	p99 := percentileFloat(clean, 0.99)
	suggested := maxFloat(current*0.85, maxFloat(p95*1.15, maxFloat(p99*1.05, floor)))
	return roundFloat(suggested, 3)
}

func maxFloat(left float64, right float64) float64 {
	if left >= right {
		return left
	}
	return right
}

func recallPercentile(values []float64, pct float64) float64 {
	clean := make([]float64, 0, len(values))
	for _, value := range values {
		if value < 0 {
			continue
		}
		clean = append(clean, value)
	}
	if len(clean) == 0 {
		return 0
	}
	sort.Float64s(clean)
	return roundFloat(percentileFloat(clean, pct), 6)
}

func buildRecallQualityTuningRecommendation(
	latest map[string]any,
	recallAtKValues []float64,
	mrrValues []float64,
	citationCoverageValues []float64,
	sourceDiversityValues []float64,
	graphLiftValues []float64,
	evalP95Values []float64,
	defaultSources []string,
) map[string]any {
	recallLatest := anyToFloat64(latest["recallAtK"], 0)
	mrrLatest := anyToFloat64(latest["mrr"], 0)
	graphLiftLatest := anyToFloat64(latest["graphLift"], 0)
	sourceDiversityLatest := anyToFloat64(latest["sourceDiversity"], 0)
	citationLatest := anyToFloat64(latest["citationCoverage"], 0)
	depth := 0
	neighborLimit := 0
	if latest != nil && (graphLiftLatest > 0 || recallLatest < 0.75 || mrrLatest < 0.55) {
		depth = 1
		neighborLimit = 12
		if graphLiftLatest >= 0.15 {
			neighborLimit = 20
		}
	}
	sourceOrder := orderedSourceUnion(
		defaultSources,
		[]string{sourceTopicRollup, sourceQdrant, sourcePgvector, sourceMemoryBank},
	)
	recommendations := make([]string, 0, 5)
	if latest == nil {
		recommendations = append(recommendations, "Run saved recall evaluation before applying quality tuning.")
	} else {
		if recallLatest < 0.75 || mrrLatest < 0.55 {
			recommendations = append(recommendations, "Keep source fanout broad for boundary context packs until recall and MRR are back above floor.")
		}
		if graphLiftLatest > 0 {
			recommendations = append(recommendations, "Use first-hop graph expansion for agent context packages; graph neighbors are contributing measurable recall.")
		}
		if citationLatest > 0 && citationLatest < 0.9 {
			recommendations = append(recommendations, "Prefer file-backed citations in ranking when context packs need auditable memory evidence.")
		}
		if sourceDiversityLatest > 0 && sourceDiversityLatest < 1.5 {
			recommendations = append(recommendations, "Do not narrow retrieval source order yet; current quality samples show low source diversity.")
		}
	}
	if len(recommendations) == 0 {
		recommendations = append(recommendations, "Quality samples support current source order and graph expansion defaults.")
	}
	return map[string]any{
		"latest": map[string]any{
			"recallAtK":        roundFloat(recallLatest, 6),
			"mrr":              roundFloat(mrrLatest, 6),
			"citationCoverage": roundFloat(citationLatest, 6),
			"sourceDiversity":  roundFloat(sourceDiversityLatest, 3),
			"graphLift":        roundFloat(graphLiftLatest, 6),
		},
		"baselines": map[string]any{
			"recallAtKP50":        recallPercentile(recallAtKValues, 0.50),
			"recallAtKP95":        recallPercentile(recallAtKValues, 0.95),
			"mrrP50":              recallPercentile(mrrValues, 0.50),
			"citationCoverageP50": recallPercentile(citationCoverageValues, 0.50),
			"sourceDiversityP50":  recallPercentile(sourceDiversityValues, 0.50),
			"graphLiftP95":        recallPercentile(graphLiftValues, 0.95),
			"evalP95MsP95":        recallPercentile(evalP95Values, 0.95),
		},
		"graphExpansion": map[string]any{
			"enabled":       depth > 0,
			"depth":         depth,
			"neighborLimit": neighborLimit,
			"policy":        "first_hop_only",
		},
		"sourceOrder": sourceOrder,
		"cadence": map[string]any{
			"savedEval":     "hourly_or_before_release",
			"caseRefresh":   "daily_or_after_memory_schema_change",
			"openCoreAudit": "before_public_or_paid_sync",
		},
		"recommendations": recommendations,
	}
}

func (s *server) telemetryRecallTuningRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}

	query := r.URL.Query()
	lookbackHours := parseOptionalFloatQuery(
		query.Get("lookback_hours"),
		maxFloat(1.0, envFloat("ORCH_RECALL_MONITOR_LOOKBACK_HOURS", 24.0)),
		1.0,
		24.0*365.0,
	)
	minSamples := parseOptionalIntQuery(
		query.Get("min_samples"),
		maxInt(4, envInt("ORCH_RECALL_TUNING_MIN_SAMPLES", 16)),
		1,
		100000,
	)
	maxSamples := parseOptionalIntQuery(
		query.Get("max_samples"),
		maxInt(24, envInt("ORCH_RECALL_MONITOR_HISTORY_LIMIT", 288)),
		1,
		100000,
	)

	rows := s.readRecallMonitorHistory(maxSamples)
	windowSamples := recallMonitorSamplesForWindow(rows, lookbackHours, maxSamples)

	noHitValues := make([]float64, 0, len(windowSamples))
	lowConfidenceValues := make([]float64, 0, len(windowSamples))
	staleValues := make([]float64, 0, len(windowSamples))
	sourceErrorValues := make([]float64, 0, len(windowSamples))
	lettaP95Values := make([]float64, 0, len(windowSamples))
	lettaP99Values := make([]float64, 0, len(windowSamples))
	lettaTimeoutValues := make([]float64, 0, len(windowSamples))
	recallAtKValues := make([]float64, 0, len(windowSamples))
	mrrValues := make([]float64, 0, len(windowSamples))
	citationCoverageValues := make([]float64, 0, len(windowSamples))
	sourceDiversityValues := make([]float64, 0, len(windowSamples))
	graphLiftValues := make([]float64, 0, len(windowSamples))
	evalP95Values := make([]float64, 0, len(windowSamples))
	for _, sample := range windowSamples {
		noHitValues = append(noHitValues, anyToFloat64(sample["noHitRate"], 0.0))
		lowConfidenceValues = append(lowConfidenceValues, anyToFloat64(sample["lowConfidenceRate"], 0.0))
		staleValues = append(staleValues, anyToFloat64(sample["staleHitRate"], 0.0))
		sourceErrorValues = append(sourceErrorValues, anyToFloat64(sample["maxSourceErrorRate"], 0.0))

		lettaP95 := anyToFloat64(sample["lettaP95Ms"], 0.0)
		if lettaP95 > 0 {
			lettaP95Values = append(lettaP95Values, lettaP95)
		}
		lettaP99 := anyToFloat64(sample["lettaP99Ms"], 0.0)
		if lettaP99 > 0 {
			lettaP99Values = append(lettaP99Values, lettaP99)
		}
		lettaTimeoutValues = append(lettaTimeoutValues, anyToFloat64(sample["lettaTimeoutRate"], 0.0))
		if _, exists := sample["recallAtK"]; exists {
			recallAtKValues = append(recallAtKValues, anyToFloat64(sample["recallAtK"], 0.0))
			mrrValues = append(mrrValues, anyToFloat64(sample["mrr"], 0.0))
			citationCoverageValues = append(citationCoverageValues, anyToFloat64(sample["citationCoverage"], 0.0))
			sourceDiversityValues = append(sourceDiversityValues, anyToFloat64(sample["sourceDiversity"], 0.0))
			graphLiftValues = append(graphLiftValues, anyToFloat64(sample["graphLift"], 0.0))
			evalP95Values = append(evalP95Values, anyToFloat64(sample["evalP95Ms"], 0.0))
		}
	}

	currentRecallNoHit := clampFloat(envFloat("ORCH_RECALL_ALERT_NO_HIT_RATE", 0.35), 0.0, 1.0)
	currentRecallLowConfidence := clampFloat(envFloat("ORCH_RECALL_ALERT_LOW_CONFIDENCE_RATE", 0.4), 0.0, 1.0)
	currentRecallStale := clampFloat(envFloat("ORCH_RECALL_ALERT_STALE_HIT_RATE", 0.45), 0.0, 1.0)
	currentRecallSourceError := clampFloat(envFloat("ORCH_RECALL_ALERT_SOURCE_ERROR_RATE", 0.25), 0.0, 1.0)
	currentRecallMinRequests := maxInt(5, envInt("ORCH_RECALL_ALERT_MIN_REQUESTS", 50))

	currentRetrievalLettaP95 := maxFloat(1000.0, envFloat("ORCH_RETRIEVAL_ALERT_LETTA_P95_MS", 30000.0))
	currentRetrievalLettaP99 := maxFloat(currentRetrievalLettaP95, envFloat("ORCH_RETRIEVAL_ALERT_LETTA_P99_MS", 45000.0))
	currentRetrievalLettaTimeout := clampFloat(envFloat("ORCH_RETRIEVAL_ALERT_LETTA_TIMEOUT_RATE", 0.05), 0.0, 1.0)
	currentRetrievalMinRequests := maxInt(1, envInt("ORCH_RETRIEVAL_ALERT_MIN_REQUESTS", 20))

	recommended := map[string]any{
		"recall": map[string]any{
			"noHitRate": recommendRecallRateThreshold(noHitValues, currentRecallNoHit, 0.001, 1.0),
			"lowConfidenceRate": recommendRecallRateThreshold(
				lowConfidenceValues,
				currentRecallLowConfidence,
				0.001,
				1.0,
			),
			"staleHitRate": recommendRecallRateThreshold(staleValues, currentRecallStale, 0.001, 1.0),
			"sourceErrorRate": recommendRecallRateThreshold(
				sourceErrorValues,
				currentRecallSourceError,
				0.001,
				1.0,
			),
			"minRequests": currentRecallMinRequests,
		},
		"retrieval": map[string]any{
			"lettaP95Ms": recommendRecallLatencyThreshold(
				lettaP95Values,
				currentRetrievalLettaP95,
				1000.0,
			),
			"lettaP99Ms": recommendRecallLatencyThreshold(
				lettaP99Values,
				currentRetrievalLettaP99,
				currentRetrievalLettaP95,
			),
			"lettaTimeoutRate": recommendRecallRateThreshold(
				lettaTimeoutValues,
				currentRetrievalLettaTimeout,
				0.001,
				1.0,
			),
			"minRequests": currentRetrievalMinRequests,
		},
	}
	latestQualitySample := latestRecallEvalMonitorSample(windowSamples)
	qualityRecommendation := buildRecallQualityTuningRecommendation(
		latestQualitySample,
		recallAtKValues,
		mrrValues,
		citationCoverageValues,
		sourceDiversityValues,
		graphLiftValues,
		evalP95Values,
		s.retrieval.defaultSources,
	)
	recommended["quality"] = qualityRecommendation

	warnings := make([]string, 0, 1)
	if len(windowSamples) < minSamples {
		warnings = append(
			warnings,
			"Only "+strconv.Itoa(len(windowSamples))+" recall monitor samples available; collect at least "+
				strconv.Itoa(minSamples)+" for stable tuning.",
		)
	}

	monitorLimit := maxSamples
	if monitorLimit > 20 {
		monitorLimit = 20
	}
	monitorRows := s.readRecallMonitorHistory(monitorLimit)
	latestSample := any(nil)
	if len(windowSamples) > 0 {
		latestSample = windowSamples[len(windowSamples)-1]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window": map[string]any{
			"lookbackHours": lookbackHours,
			"samples":       len(windowSamples),
			"minSamples":    minSamples,
			"sufficient":    len(windowSamples) >= minSamples,
		},
		"current": map[string]any{
			"recall": map[string]any{
				"noHitRate":         currentRecallNoHit,
				"lowConfidenceRate": currentRecallLowConfidence,
				"staleHitRate":      currentRecallStale,
				"sourceErrorRate":   currentRecallSourceError,
				"minRequests":       currentRecallMinRequests,
			},
			"retrieval": map[string]any{
				"lettaP95Ms":       currentRetrievalLettaP95,
				"lettaP99Ms":       currentRetrievalLettaP99,
				"lettaTimeoutRate": currentRetrievalLettaTimeout,
				"minRequests":      currentRetrievalMinRequests,
			},
		},
		"recommended": recommended,
		"env": map[string]any{
			"ORCH_RECALL_ALERT_NO_HIT_RATE":               recommended["recall"].(map[string]any)["noHitRate"],
			"ORCH_RECALL_ALERT_LOW_CONFIDENCE_RATE":       recommended["recall"].(map[string]any)["lowConfidenceRate"],
			"ORCH_RECALL_ALERT_STALE_HIT_RATE":            recommended["recall"].(map[string]any)["staleHitRate"],
			"ORCH_RECALL_ALERT_SOURCE_ERROR_RATE":         recommended["recall"].(map[string]any)["sourceErrorRate"],
			"ORCH_RETRIEVAL_ALERT_LETTA_P95_MS":           recommended["retrieval"].(map[string]any)["lettaP95Ms"],
			"ORCH_RETRIEVAL_ALERT_LETTA_P99_MS":           recommended["retrieval"].(map[string]any)["lettaP99Ms"],
			"ORCH_RETRIEVAL_ALERT_LETTA_TIMEOUT_RATE":     recommended["retrieval"].(map[string]any)["lettaTimeoutRate"],
			"CONTEXTLATTICE_RECALL_GRAPH_EXPANSION_DEPTH": qualityRecommendation["graphExpansion"].(map[string]any)["depth"],
			"CONTEXTLATTICE_RECALL_GRAPH_EXPANSION_LIMIT": qualityRecommendation["graphExpansion"].(map[string]any)["neighborLimit"],
		},
		"warnings":     warnings,
		"latestSample": latestSample,
		"monitor": map[string]any{
			"updatedAt": nowUTCISO(),
			"history":   monitorRows,
			"count":     len(monitorRows),
			"config": map[string]any{
				"historyLimit":  monitorLimit,
				"lookbackHours": maxFloat(1.0, envFloat("ORCH_RECALL_MONITOR_LOOKBACK_HOURS", 24.0)),
			},
		},
	})
}

func (s *server) telemetryToolsInvocationsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	query := r.URL.Query()
	limit := parseOptionalIntQuery(query.Get("limit"), 100, 1, 500)
	toolFilter := strings.TrimSpace(strings.ToLower(query.Get("tool")))
	statusMin := parseOptionalIntQuery(query.Get("status_min"), 0, 0, 599)

	entries := s.telemetryRing.debugEntries()
	items := make([]map[string]any, 0, minInt(limit, len(entries)))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		path := strings.TrimSpace(entry.sourcePath)
		if path == "" {
			path = "/memory/write"
		}
		statusCode := 503
		if statusCode < statusMin {
			continue
		}
		if toolFilter != "" && !strings.Contains(strings.ToLower(path), toolFilter) {
			continue
		}
		errorText := strings.TrimSpace(entry.ingestError)
		if errorText == "" {
			errorText = strings.TrimSpace(entry.spoolError)
		}
		if errorText == "" {
			errorText = "telemetry sink unavailable"
		}
		items = append(items, map[string]any{
			"id":          entry.eventID,
			"timestamp":   entry.insertedAt.UTC().Format(time.RFC3339Nano),
			"path":        path,
			"tool":        strings.TrimPrefix(path, "/tools/"),
			"status_code": statusCode,
			"duration_ms": nil,
			"project":     entry.project,
			"agent_id":    nil,
			"error":       errorText,
		})
		if len(items) >= limit {
			break
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
		"limit": limit,
		"filters": map[string]any{
			"tool":      nullableString(toolFilter),
			"statusMin": nullableInt(statusMin),
		},
		"audit": map[string]any{
			"enabled":           false,
			"retentionDays":     0,
			"pruneIntervalSecs": 0,
			"state":             map[string]any{},
		},
	})
}

func nullableString(value string) any {
	token := strings.TrimSpace(value)
	if token == "" {
		return nil
	}
	return token
}

func nullableInt(value int) any {
	if value <= 0 {
		return nil
	}
	return value
}
