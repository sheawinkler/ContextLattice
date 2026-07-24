package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func (s *server) memoryWrite(w http.ResponseWriter, r *http.Request) {
	s.handleWriteIngress(w, r, "/memory/write")
}

func (s *server) memoryPut(w http.ResponseWriter, r *http.Request) {
	s.handleWriteIngress(w, r, "/v1/memory/put")
}

func maybeAttachWritebackContract(path string, payload map[string]any, item normalizedWrite, status int) map[string]any {
	if path != "/memory/write" {
		return payload
	}
	return attachWritebackFormatContract(payload, item, path, status)
}

func (s *server) handleWriteIngress(w http.ResponseWriter, r *http.Request, path string) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	item, err := normalizeWritePayload(path, payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	secretFilter := writeSecretFilterResult{Mode: writeSecretsStorageMode()}
	item, secretFilter, err = secureNormalizedWrite(item)
	if err != nil {
		s.recordWriteSecretFilter(secretFilter, true)
		s.recordMemoryWriteTelemetry(startedAt, 0, 1)
		writeJSON(w, http.StatusUnprocessableEntity, attachWriteSecretFilter(map[string]any{
			"ok":     false,
			"error":  "potential_secret_detected",
			"detail": err.Error(),
		}, secretFilter))
		return
	}
	s.recordWriteSecretFilter(secretFilter, false)
	recordMetadataContractObservation(item)
	if err := s.writePolicy.validateWrite(item); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, maybeAttachWritebackContract(path, attachWriteSecretFilter(map[string]any{"ok": false, "error": err.Error()}, secretFilter), item, http.StatusUnprocessableEntity))
		return
	}
	item = s.classifyWrite(item)

	if s.writePolicy.isTelemetryLike(item) {
		response, status, ingestErr := s.routeTelemetryWrite(r.Context(), item, path)
		if ingestErr != nil {
			writeJSON(w, http.StatusBadGateway, maybeAttachWritebackContract(path, attachWriteSecretFilter(map[string]any{
				"ok":     false,
				"error":  "telemetry ingest failed",
				"detail": ingestErr.Error(),
			}, secretFilter), item, http.StatusBadGateway))
			return
		}
		if status >= 200 && status < 400 {
			s.recordMemoryWriteTelemetry(startedAt, 1, 0)
		}
		writeJSON(w, status, maybeAttachWritebackContract(path, attachWriteSecretFilter(response, secretFilter), item, status))
		return
	}
	if s.memoryStore != nil && s.memoryStore.isEnabled() {
		entry, deduped, storeErr := s.memoryStore.put(item)
		if storeErr != nil {
			writeJSON(w, http.StatusBadGateway, maybeAttachWritebackContract(path, attachWriteSecretFilter(map[string]any{
				"ok":     false,
				"error":  "memory store write failed",
				"detail": storeErr.Error(),
			}, secretFilter), item, http.StatusBadGateway))
			return
		}
		fanout := map[string]any{
			"go_memory_store": "succeeded",
			"python_backend":  "disabled",
		}
		vectorFanout, warnings := s.handleNativeVectorWriteFanout(item, entry.EventID)
		for source, status := range vectorFanout {
			fanout[source] = status
		}
		s.recordMemoryWriteTelemetry(startedAt, 1, 0)
		writeJSON(w, http.StatusOK, maybeAttachWritebackContract(path, attachWriteSecretFilter(map[string]any{
			"ok":                    true,
			"event_id":              entry.EventID,
			"source":                "go_memory_store",
			"data_class":            entry.DataClass,
			"lifecycle":             entry.Lifecycle,
			"content_hash":          entry.ContentHash,
			"content_ref":           entry.ContentRef,
			"warnings":              warnings,
			"rollup_buffered":       entry.DataClass != dataClassRuntimeStateMirror,
			"deduped":               deduped,
			"latest_hash_unchanged": deduped,
			"fanout":                fanout,
		}, secretFilter), item, http.StatusOK))
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, path, "strict_runtime_backend_forward_disabled") {
			return
		}
	}

	forwardPayload := mergeForwardPayload(path, payload, item, s.writePolicy.fanoutExcludeTargetsFor(item))
	response, status, backendErr := s.callBackendJSON(r.Context(), incomingHeaders, http.MethodPost, path, forwardPayload)
	if backendErr != nil {
		writeJSON(w, http.StatusBadGateway, maybeAttachWritebackContract(path, attachWriteSecretFilter(map[string]any{
			"ok":         false,
			"error":      "backend unavailable",
			"detail":     backendErr.Error(),
			"backendUrl": s.backendURL,
		}, secretFilter), item, http.StatusBadGateway))
		return
	}
	if status >= 200 && status < 400 {
		s.recordMemoryWriteTelemetry(startedAt, 1, 0)
	}
	writeJSON(w, status, maybeAttachWritebackContract(path, attachWriteSecretFilter(response, secretFilter), item, status))
}

func (s *server) memoryWriteBatch(w http.ResponseWriter, r *http.Request) {
	s.handleWriteBatchIngress(w, r, "/memory/write/batch", "/memory/write")
}

func (s *server) memoryBatchPut(w http.ResponseWriter, r *http.Request) {
	s.handleWriteBatchIngress(w, r, "/v1/memory/batch-put", "/v1/memory/put")
}

func (s *server) handleWriteBatchIngress(
	w http.ResponseWriter,
	r *http.Request,
	batchPath string,
	singlePath string,
) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	items, err := normalizeWriteBatchPayload(batchPath, payload)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	secretFilter := writeSecretFilterResult{Mode: writeSecretsStorageMode()}
	for idx, item := range items {
		secured, itemSecretFilter, secureErr := secureNormalizedWrite(item)
		secretFilter = mergeWriteSecretFilterResults(secretFilter, itemSecretFilter)
		if secureErr != nil {
			s.recordWriteSecretFilter(secretFilter, true)
			s.recordMemoryWriteTelemetry(startedAt, 0, 1)
			writeJSON(w, http.StatusUnprocessableEntity, attachWriteSecretFilter(map[string]any{
				"ok":     false,
				"error":  "potential_secret_detected",
				"detail": secureErr.Error(),
				"index":  idx,
			}, secretFilter))
			return
		}
		items[idx] = secured
		item = secured
		recordMetadataContractObservation(item)
		if err := s.writePolicy.validateWrite(item); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, attachWriteSecretFilter(map[string]any{
				"error":  "invalid write item",
				"detail": err.Error(),
				"index":  idx,
			}, secretFilter))
			return
		}
	}
	s.recordWriteSecretFilter(secretFilter, false)

	type batchOutcome struct {
		index  int
		result map[string]any
		ok     bool
	}
	results := make([]batchOutcome, 0, len(items))
	resultsMu := sync.Mutex{}
	jobs := make(chan int, len(items))
	workers := s.writePolicy.batchConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for workerIdx := 0; workerIdx < workers; workerIdx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				item := s.classifyWrite(items[idx])
				var row map[string]any
				ok := false
				if s.writePolicy.isTelemetryLike(item) {
					response, _, routeErr := s.routeTelemetryWrite(r.Context(), item, singlePath)
					if routeErr != nil {
						row = map[string]any{
							"index":  idx,
							"itemId": item.itemID,
							"ok":     false,
							"error":  map[string]any{"status": 502, "detail": routeErr.Error()},
						}
					} else {
						row = map[string]any{
							"index":                 idx,
							"itemId":                item.itemID,
							"ok":                    true,
							"event_id":              response["event_id"],
							"lane":                  response["lane"],
							"accepted_degraded":     response["accepted_degraded"],
							"warnings":              response["warnings"],
							"fanout":                response["fanout"],
							"telemetry_routed":      true,
							"telemetry_spooled":     response["telemetry_spooled"],
							"telemetry_buffered":    response["telemetry_buffered"],
							"blob_ref":              response["blob_ref"],
							"ring_ref":              response["ring_ref"],
							"content_hash":          response["content_hash"],
							"storage_schema":        response["storage_schema"],
							"retention_window_days": response["retention_window_days"],
						}
						ok = true
					}
				} else {
					if s.memoryStore != nil && s.memoryStore.isEnabled() {
						entry, deduped, storeErr := s.memoryStore.put(item)
						if storeErr != nil {
							row = map[string]any{
								"index":  idx,
								"itemId": item.itemID,
								"ok":     false,
								"error":  map[string]any{"status": 502, "detail": storeErr.Error()},
							}
						} else {
							row = map[string]any{
								"index":                 idx,
								"itemId":                item.itemID,
								"ok":                    true,
								"event_id":              entry.EventID,
								"content_hash":          entry.ContentHash,
								"content_ref":           entry.ContentRef,
								"data_class":            entry.DataClass,
								"lifecycle":             entry.Lifecycle,
								"warnings":              []string{},
								"fanout":                map[string]any{"go_memory_store": "succeeded", "python_backend": "disabled"},
								"rollup_buffered":       entry.DataClass != dataClassRuntimeStateMirror,
								"deduped":               deduped,
								"latest_hash_unchanged": deduped,
								"source":                "go_memory_store",
							}
							fanout := map[string]any{
								"go_memory_store": "succeeded",
								"python_backend":  "disabled",
							}
							vectorFanout, warnings := s.handleNativeVectorWriteFanout(item, entry.EventID)
							for source, status := range vectorFanout {
								fanout[source] = status
							}
							row["fanout"] = fanout
							row["warnings"] = warnings
							ok = true
						}
					} else {
						if s.strictNoPythonRuntime {
							row = map[string]any{
								"index":  idx,
								"itemId": item.itemID,
								"ok":     false,
								"error":  map[string]any{"status": 503, "detail": "python runtime disabled by strict policy"},
							}
							resultsMu.Lock()
							results = append(results, batchOutcome{index: idx, result: row, ok: false})
							resultsMu.Unlock()
							continue
						}
						forwardPayload := buildForwardPayload(singlePath, item, s.writePolicy.fanoutExcludeTargetsFor(item))
						response, status, backendErr := s.callBackendJSON(
							r.Context(),
							incomingHeaders,
							http.MethodPost,
							singlePath,
							forwardPayload,
						)
						if backendErr != nil {
							row = map[string]any{
								"index":  idx,
								"itemId": item.itemID,
								"ok":     false,
								"error":  map[string]any{"status": 502, "detail": backendErr.Error()},
							}
						} else {
							row = map[string]any{
								"index":                 idx,
								"itemId":                item.itemID,
								"ok":                    anyToBool(response["ok"]) && status >= 200 && status < 300,
								"event_id":              response["event_id"],
								"warnings":              response["warnings"],
								"fanout":                response["fanout"],
								"rollup_buffered":       anyToBool(response["rollup_buffered"]),
								"deduped":               anyToBool(response["deduped"]),
								"latest_hash_unchanged": anyToBool(response["latest_hash_unchanged"]),
							}
							if status < 200 || status >= 300 {
								row["error"] = map[string]any{"status": status, "detail": anyToString(response["detail"])}
							}
							ok = anyToBool(row["ok"])
						}
					}
				}
				resultsMu.Lock()
				results = append(results, batchOutcome{index: idx, result: row, ok: ok})
				resultsMu.Unlock()
			}
		}()
	}
	for idx := range items {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].index < results[j].index
	})
	resultRows := make([]map[string]any, 0, len(results))
	succeeded := 0
	failed := 0
	for _, row := range results {
		resultRows = append(resultRows, row.result)
		if row.ok {
			succeeded += 1
		} else {
			failed += 1
		}
	}
	if succeeded > 0 || failed > 0 {
		s.recordMemoryWriteTelemetry(startedAt, succeeded, failed)
	}
	writeJSON(w, http.StatusOK, attachWriteSecretFilter(map[string]any{
		"ok":        failed == 0,
		"partial":   failed > 0 && succeeded > 0,
		"source":    batchPath,
		"accepted":  len(items),
		"succeeded": succeeded,
		"failed":    failed,
		"results":   resultRows,
	}, secretFilter))
}

func mergeForwardPayload(
	path string,
	original map[string]any,
	item normalizedWrite,
	fanoutExcludeTargets []string,
) map[string]any {
	if path == "/memory/write" {
		forward := cloneMap(item.raw)
		if len(forward) == 0 {
			forward = cloneMap(original)
		}
		forward["projectName"] = item.project
		forward["fileName"] = item.fileName
		forward["content"] = item.content
		if item.topicPath != "" {
			forward["topicPath"] = item.topicPath
		}
		if len(fanoutExcludeTargets) > 0 {
			forward["fanoutExcludeTargets"] = append([]string{}, fanoutExcludeTargets...)
		}
		if item.agentID != "" {
			forward["agent_id"] = item.agentID
		}
		if item.sessionID != "" {
			forward["session_id"] = item.sessionID
		}
		if len(item.tags) > 0 {
			forward["tags"] = append([]string{}, item.tags...)
		}
		if item.createdAt != "" {
			forward["created_at"] = item.createdAt
		}
		return forward
	}
	if path == "/v1/memory/put" {
		forward := map[string]any{}
		rawItem := cloneMap(item.raw)
		rawItem["project"] = item.project
		rawItem["file_name"] = item.fileName
		rawItem["content"] = item.content
		if item.topicPath != "" {
			rawItem["topic_path"] = item.topicPath
		}
		if len(fanoutExcludeTargets) > 0 {
			rawItem["fanout_exclude_targets"] = append([]string{}, fanoutExcludeTargets...)
		}
		if item.agentID != "" {
			rawItem["agent_id"] = item.agentID
		}
		if item.sessionID != "" {
			rawItem["session_id"] = item.sessionID
		}
		if len(item.tags) > 0 {
			rawItem["tags"] = append([]string{}, item.tags...)
		}
		if item.createdAt != "" {
			rawItem["created_at"] = item.createdAt
		}
		forward["item"] = rawItem
		return forward
	}
	return buildForwardPayload(path, item, fanoutExcludeTargets)
}

func writePgvectorFanoutMode() string {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("GO_WRITE_PGVECTOR_FANOUT_MODE")))
	switch mode {
	case "sync", "synchronous":
		return "sync"
	case "disabled", "off", "false":
		return "disabled"
	default:
		return "async"
	}
}

func writePgvectorFanoutTimeout() time.Duration {
	timeout := envDurationSeconds("GO_WRITE_PGVECTOR_FANOUT_TIMEOUT_SECS", 30)
	if timeout < time.Second {
		return time.Second
	}
	return timeout
}

func pgvectorWriteFanoutAsyncMaxInflight() int {
	limit := envInt("GO_WRITE_PGVECTOR_FANOUT_ASYNC_MAX_INFLIGHT", 2)
	if limit < 1 {
		return 1
	}
	if limit > 16 {
		return 16
	}
	return limit
}

func pgvectorWriteFanoutPreflightStatus() (string, bool) {
	if !nativeSourceAdapterEnabled(sourcePgvector, true) {
		return "skipped_adapter_disabled", false
	}
	if !nativePgvectorEnabled() {
		return "skipped_source_disabled", false
	}
	if !nativePgvectorFanoutEnabled() {
		return "skipped_fanout_disabled", false
	}
	if nativePgvectorDSN() == "" {
		return "skipped_unconfigured", false
	}
	return "", true
}

func writeQdrantFanoutMode() string {
	mode := strings.TrimSpace(strings.ToLower(os.Getenv("GO_WRITE_QDRANT_FANOUT_MODE")))
	switch mode {
	case "sync", "synchronous":
		return "sync"
	case "disabled", "off", "false":
		return "disabled"
	default:
		return "async"
	}
}

func writeQdrantFanoutTimeout() time.Duration {
	timeout := envDurationSeconds("GO_WRITE_QDRANT_FANOUT_TIMEOUT_SECS", 30)
	if timeout < time.Second {
		return time.Second
	}
	return timeout
}

func qdrantWriteFanoutAsyncMaxInflight() int {
	limit := envInt("GO_WRITE_QDRANT_FANOUT_ASYNC_MAX_INFLIGHT", 2)
	if limit < 1 {
		return 1
	}
	if limit > 16 {
		return 16
	}
	return limit
}

func qdrantWriteFanoutPreflightStatus() (string, bool) {
	if !nativeSourceAdapterEnabled(sourceQdrant, true) {
		return "skipped_adapter_disabled", false
	}
	if nativeQdrantURL() == "" {
		return "skipped_unconfigured", false
	}
	return "", true
}

func cloneNormalizedWriteForAsync(item normalizedWrite) normalizedWrite {
	copyItem := item
	if item.tags != nil {
		copyItem.tags = append([]string{}, item.tags...)
	}
	copyItem.raw = nil
	return copyItem
}

func (s *server) handleNativeVectorWriteFanout(item normalizedWrite, eventID string) (map[string]any, []string) {
	if item.dataClass == dataClassRuntimeStateMirror || (s.memoryStore != nil && s.memoryStore.isExactStatePath(item.project, item.fileName)) {
		return map[string]any{
			sourceQdrant:   "skipped_exact_state_mirror",
			sourcePgvector: "skipped_exact_state_mirror",
		}, []string{}
	}
	fanout := map[string]any{}
	warnings := []string{}
	if status, sourceWarnings := s.handleQdrantWriteFanout(item, eventID); strings.TrimSpace(status) != "" {
		fanout[sourceQdrant] = status
		warnings = append(warnings, sourceWarnings...)
	}
	if status, sourceWarnings := s.handlePgvectorWriteFanout(item, eventID); strings.TrimSpace(status) != "" {
		fanout[sourcePgvector] = status
		warnings = append(warnings, sourceWarnings...)
	}
	return fanout, warnings
}

func (s *server) handleQdrantWriteFanout(item normalizedWrite, eventID string) (string, []string) {
	mode := writeQdrantFanoutMode()
	if mode == "disabled" {
		return "skipped_write_fanout_mode_disabled", []string{}
	}
	if status, enabled := qdrantWriteFanoutPreflightStatus(); !enabled {
		return status, []string{}
	}
	timeout := writeQdrantFanoutTimeout()
	if mode == "sync" {
		fanoutCtx, cancel := context.WithTimeout(context.Background(), timeout)
		fanoutStatus, fanoutErr := s.upsertQdrantFromWrite(fanoutCtx, item, eventID)
		cancel()
		if fanoutErr != nil {
			log.Printf("qdrant fanout error project=%s file=%s status=%s err=%v", item.project, item.fileName, fanoutStatus, fanoutErr)
			return fanoutStatus, []string{"qdrant fanout " + fanoutStatus + ": " + fanoutErr.Error()}
		}
		return fanoutStatus, []string{}
	}
	if s.qdrantWriteFanoutSem == nil {
		s.qdrantWriteFanoutSem = make(chan struct{}, qdrantWriteFanoutAsyncMaxInflight())
	}
	select {
	case s.qdrantWriteFanoutSem <- struct{}{}:
	default:
		warning := "qdrant fanout skipped_async_backpressure: async fanout workers are saturated"
		log.Printf("qdrant fanout backpressure project=%s file=%s", item.project, item.fileName)
		return "skipped_async_backpressure", []string{warning}
	}
	copyItem := cloneNormalizedWriteForAsync(item)
	go func() {
		defer func() {
			<-s.qdrantWriteFanoutSem
		}()
		fanoutCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		status, err := s.upsertQdrantFromWrite(fanoutCtx, copyItem, eventID)
		if err != nil {
			log.Printf("qdrant async fanout error project=%s file=%s status=%s err=%v", copyItem.project, copyItem.fileName, status, err)
			return
		}
		if envBool("GO_WRITE_QDRANT_FANOUT_LOG_SUCCESS", false) {
			log.Printf("qdrant async fanout complete project=%s file=%s status=%s", copyItem.project, copyItem.fileName, status)
		}
	}()
	return "queued_async", []string{}
}

func (s *server) handlePgvectorWriteFanout(item normalizedWrite, eventID string) (string, []string) {
	if status, enabled := pgvectorWriteFanoutPreflightStatus(); !enabled {
		return status, []string{}
	}
	mode := writePgvectorFanoutMode()
	if mode == "disabled" {
		return "skipped_write_fanout_mode_disabled", []string{}
	}
	timeout := writePgvectorFanoutTimeout()
	if mode == "sync" {
		fanoutCtx, cancel := context.WithTimeout(context.Background(), timeout)
		fanoutStatus, fanoutErr := s.upsertPgvectorFromWrite(fanoutCtx, item, eventID)
		cancel()
		if fanoutErr != nil {
			log.Printf("pgvector fanout error project=%s file=%s status=%s err=%v", item.project, item.fileName, fanoutStatus, fanoutErr)
			return fanoutStatus, []string{"pgvector fanout " + fanoutStatus + ": " + fanoutErr.Error()}
		}
		return fanoutStatus, []string{}
	}
	if s.pgvectorWriteFanoutSem == nil {
		s.pgvectorWriteFanoutSem = make(chan struct{}, pgvectorWriteFanoutAsyncMaxInflight())
	}
	select {
	case s.pgvectorWriteFanoutSem <- struct{}{}:
	default:
		warning := "pgvector fanout skipped_async_backpressure: async fanout workers are saturated"
		log.Printf("pgvector fanout backpressure project=%s file=%s", item.project, item.fileName)
		return "skipped_async_backpressure", []string{warning}
	}
	copyItem := cloneNormalizedWriteForAsync(item)
	go func() {
		defer func() {
			<-s.pgvectorWriteFanoutSem
		}()
		fanoutCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		status, err := s.upsertPgvectorFromWrite(fanoutCtx, copyItem, eventID)
		if err != nil {
			log.Printf("pgvector async fanout error project=%s file=%s status=%s err=%v", copyItem.project, copyItem.fileName, status, err)
			return
		}
		if envBool("GO_WRITE_PGVECTOR_FANOUT_LOG_SUCCESS", false) {
			log.Printf("pgvector async fanout complete project=%s file=%s status=%s", copyItem.project, copyItem.fileName, status)
		}
	}()
	return "queued_async", []string{}
}

func (s *server) routeTelemetryWrite(
	ctx context.Context,
	item normalizedWrite,
	sourcePath string,
) (map[string]any, int, error) {
	if s.telemetrySink == nil || !s.telemetrySink.enabled {
		return s.routeTelemetryToSpool(item, sourcePath, fmt.Errorf("telemetry sink unavailable"))
	}
	timeout := envDurationSeconds("GO_TELEMETRY_INGEST_TIMEOUT_SECS", 8)
	if timeout < time.Second {
		timeout = time.Second
	}
	ingestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	meta := map[string]any{
		"source_path": sourcePath,
		"agent_id":    item.agentID,
		"session_id":  item.sessionID,
		"tags":        append([]string{}, item.tags...),
		"created_at":  item.createdAt,
		"topic_path":  item.topicPath,
		"ingested_by": "gateway-go",
	}
	result, err := s.telemetrySink.ingestWrite(ingestCtx, item, meta)
	if err != nil {
		return s.routeTelemetryToSpool(item, sourcePath, err)
	}
	compressionCodec := strings.TrimSpace(result.Codec)
	if compressionCodec == "" {
		compressionCodec = "inline"
	}
	response := map[string]any{
		"ok":                    true,
		"event_id":              result.EventID,
		"telemetry_routed":      true,
		"lane":                  "telemetry_mongo_only",
		"content_hash":          result.ContentHash,
		"blob_ref":              result.ContentRef,
		"stored_inline":         result.StoredInline,
		"retention_window_days": s.telemetrySink.retentionDays,
		"storage_schema": map[string]any{
			"event_schema_version": telemetryEventSchemaV2,
			"blob_schema_version":  telemetryBlobSchemaVersion,
			"compression":          compressionCodec,
			"reference_mode":       "content_addressed_blob_ref",
		},
		"warnings": []string{
			"Telemetry/state write routed to Mongo telemetry lane only; fanout to qdrant/postgres_pgvector/mindsdb/letta/memory_bank skipped by policy.",
		},
		"fanout": map[string]any{
			"memory_bank":       "skipped_low_value",
			"mongo_raw":         "succeeded",
			"qdrant":            "skipped_low_value",
			"postgres_pgvector": "skipped_low_value",
			"mindsdb":           "skipped_low_value",
			"letta":             "skipped_low_value",
			"langfuse":          "optional",
		},
	}
	return response, http.StatusOK, nil
}

func (s *server) routeTelemetryToSpool(
	item normalizedWrite,
	sourcePath string,
	ingestErr error,
) (map[string]any, int, error) {
	if ingestErr == nil {
		ingestErr = fmt.Errorf("telemetry sink unavailable")
	}

	var spoolErr error
	if s.telemetrySpool == nil || !s.telemetrySpool.enabled {
		spoolErr = fmt.Errorf("telemetry spool disabled")
	} else {
		spooled, err := s.telemetrySpool.spoolWrite(item, sourcePath, ingestErr)
		if err == nil {
			response := map[string]any{
				"ok":                    true,
				"accepted":              true,
				"event_id":              spooled["event_id"],
				"telemetry_routed":      true,
				"telemetry_spooled":     true,
				"lane":                  "telemetry_spool_fallback",
				"spool_ref":             spooled["spool_ref"],
				"retention_window_days": envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
				"storage_schema": map[string]any{
					"event_schema_version": telemetryEventSchemaV2,
					"spool_schema_version": telemetrySpoolSchemaVersion,
					"compression":          "ndjson",
					"reference_mode":       "local_spool_ref",
				},
				"warnings": []string{
					"Telemetry sink unavailable; write accepted into local spool fallback for deferred durability.",
				},
				"fanout": map[string]any{
					"memory_bank":       "skipped_low_value",
					"mongo_raw":         "deferred_spooled",
					"qdrant":            "skipped_low_value",
					"postgres_pgvector": "skipped_low_value",
					"mindsdb":           "skipped_low_value",
					"letta":             "skipped_low_value",
					"langfuse":          "optional",
				},
			}
			if ingestErr != nil {
				response["degraded"] = true
				response["degraded_reason"] = ingestErr.Error()
			}
			return response, http.StatusAccepted, nil
		}
		spoolErr = fmt.Errorf("telemetry spool write failed: %w", err)
	}

	ringSnapshot := map[string]any{}
	if s.telemetryRing != nil {
		ringSnapshot = s.telemetryRing.snapshot()
	}
	if s.telemetryRing != nil && s.telemetryRing.enabled {
		buffered, ringErr := s.telemetryRing.enqueue(item, sourcePath, ingestErr, spoolErr)
		if ringErr == nil {
			log.Printf(
				"telemetry lane degraded accepted: sink_error=%v spool_error=%v lane=telemetry_ring_fallback",
				ingestErr,
				spoolErr,
			)
			response := map[string]any{
				"ok":                    true,
				"accepted":              true,
				"accepted_degraded":     true,
				"event_id":              buffered["event_id"],
				"telemetry_routed":      true,
				"telemetry_buffered":    true,
				"lane":                  "telemetry_ring_fallback",
				"ring_ref":              buffered["ring_ref"],
				"retention_window_days": envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
				"degraded":              true,
				"degraded_reason":       ingestErr.Error(),
				"storage_schema": map[string]any{
					"event_schema_version": telemetryEventSchemaV2,
					"ring_schema_version":  telemetryRingSchemaVersion,
					"compression":          "in_memory",
					"reference_mode":       "inmem_ring_ref",
				},
				"warnings": []string{
					"Telemetry sink and spool unavailable; write accepted in bounded in-memory ring. Persisted durability may lag until downstream recovers.",
				},
				"alerts": []map[string]any{
					{
						"code":     "telemetry_sink_spool_unavailable",
						"severity": "warning",
						"message":  "Telemetry write accepted_degraded via in-memory ring fallback.",
					},
				},
				"metrics": map[string]any{
					"sink_error":       errorString(ingestErr),
					"spool_error":      errorString(spoolErr),
					"ring_depth":       buffered["ring_depth"],
					"ring_capacity":    buffered["ring_capacity"],
					"ring_dropped":     buffered["ring_dropped"],
					"ring_dropped_low": buffered["ring_dropped_low"],
				},
				"fanout": map[string]any{
					"memory_bank":       "skipped_low_value",
					"mongo_raw":         "deferred_ring_buffer",
					"qdrant":            "skipped_low_value",
					"postgres_pgvector": "skipped_low_value",
					"mindsdb":           "skipped_low_value",
					"letta":             "skipped_low_value",
					"langfuse":          "optional",
				},
			}
			if evictedID := strings.TrimSpace(anyToString(buffered["ring_evicted_event_id"])); evictedID != "" {
				response["warnings"] = append(
					response["warnings"].([]string),
					"Ring buffer full: evicted oldest low-value telemetry entry before accepting this write.",
				)
				response["ring_evicted_event_id"] = evictedID
				response["ring_evicted_low_value"] = buffered["ring_evicted_low_value"]
			}
			return response, http.StatusAccepted, nil
		}
		spoolErr = fmt.Errorf("%w; telemetry ring enqueue failed: %v", spoolErr, ringErr)
	}

	log.Printf(
		"telemetry lane accepted_degraded without buffer: sink_error=%v spool_error=%v ring=%v",
		ingestErr,
		spoolErr,
		ringSnapshot,
	)
	response := map[string]any{
		"ok":                 true,
		"accepted":           true,
		"accepted_degraded":  true,
		"telemetry_routed":   true,
		"telemetry_buffered": false,
		"telemetry_dropped":  true,
		"lane":               "telemetry_fail_open_unbuffered",
		"degraded":           true,
		"degraded_reason":    ingestErr.Error(),
		"warnings": []string{
			"Telemetry sink and spool unavailable and ring buffer disabled/unavailable; write accepted_degraded without durable persistence.",
		},
		"alerts": []map[string]any{
			{
				"code":     "telemetry_all_fallbacks_unavailable",
				"severity": "critical",
				"message":  "Telemetry write accepted_degraded but not buffered; verify sink/spool/ring health immediately.",
			},
		},
		"metrics": map[string]any{
			"sink_error":  errorString(ingestErr),
			"spool_error": errorString(spoolErr),
			"ring_state":  ringSnapshot,
		},
		"fanout": map[string]any{
			"memory_bank":       "skipped_low_value",
			"mongo_raw":         "degraded_unbuffered",
			"qdrant":            "skipped_low_value",
			"postgres_pgvector": "skipped_low_value",
			"mindsdb":           "skipped_low_value",
			"letta":             "skipped_low_value",
			"langfuse":          "optional",
		},
	}
	return response, http.StatusAccepted, nil
}

func (s *server) callBackendJSON(
	ctx context.Context,
	headers http.Header,
	method string,
	path string,
	payload map[string]any,
) (map[string]any, int, error) {
	if s.strictNoPythonRuntime {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("python runtime disabled by strict policy")
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	targetURL := s.backendURL + path
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, err
	}
	s.copyHeaders(req.Header, headers)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	response, parseErr := parseJSONMap(responseBody)
	if parseErr != nil {
		response = map[string]any{
			"raw": strings.TrimSpace(string(responseBody)),
		}
	}
	return response, resp.StatusCode, nil
}

func (s *server) telemetryBlobGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.writeAuthorizedRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
		return
	}
	if s.telemetrySink == nil || !s.telemetrySink.enabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "telemetry sink unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.telemetrySink.runBlobGCOnce(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "blob gc failed", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func (s *server) writeAuthorizedRequest(r *http.Request) bool {
	expected := strings.TrimSpace(s.orchestratorAPIKey)
	if expected == "" {
		return true
	}
	provided, explicit := requestAPIKey(r)
	if !explicit {
		return false
	}
	if len(provided) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
