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

func (s *server) handleWriteIngress(w http.ResponseWriter, r *http.Request, path string) {
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
	recordMetadataContractObservation(item)
	if err := s.writePolicy.validateWrite(item); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	if s.writePolicy.isTelemetryLike(item) {
		response, status, ingestErr := s.routeTelemetryWrite(r.Context(), item, path)
		if ingestErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  "telemetry ingest failed",
				"detail": ingestErr.Error(),
			})
			return
		}
		writeJSON(w, status, response)
		return
	}
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		entry, deduped, storeErr := s.memoryStore.put(item)
		if storeErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  "memory store write failed",
				"detail": storeErr.Error(),
			})
			return
		}
		fanout := map[string]any{
			"go_memory_store": "succeeded",
			"python_backend":  "disabled",
		}
		warnings := []string{}
		fanoutCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 4*time.Second)
		fanoutStatus, fanoutErr := s.upsertPgvectorFromWrite(fanoutCtx, item)
		cancel()
		if strings.TrimSpace(fanoutStatus) != "" {
			fanout["postgres_pgvector"] = fanoutStatus
		}
		if fanoutErr != nil {
			warnings = append(warnings, "pgvector fanout "+fanoutStatus+": "+fanoutErr.Error())
			log.Printf("pgvector fanout error project=%s file=%s status=%s err=%v", item.project, item.fileName, fanoutStatus, fanoutErr)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                    true,
			"event_id":              entry.EventID,
			"source":                "go_memory_store",
			"content_hash":          entry.ContentHash,
			"content_ref":           entry.ContentRef,
			"warnings":              warnings,
			"rollup_buffered":       true,
			"deduped":               deduped,
			"latest_hash_unchanged": deduped,
			"fanout":                fanout,
		})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, path, "strict_runtime_backend_forward_disabled") {
			return
		}
	}

	forwardPayload := mergeForwardPayload(path, payload, item, s.writePolicy.fanoutExcludeTargets)
	response, status, backendErr := s.callBackendJSON(r.Context(), incomingHeaders, http.MethodPost, path, forwardPayload)
	if backendErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     backendErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
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
	for idx, item := range items {
		recordMetadataContractObservation(item)
		if err := s.writePolicy.validateWrite(item); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "invalid write item",
				"detail": err.Error(),
				"index":  idx,
			})
			return
		}
	}

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
				item := items[idx]
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
					if s.memoryStore != nil && s.memoryStore.policy.enabled {
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
								"warnings":              []string{},
								"fanout":                map[string]any{"go_memory_store": "succeeded", "python_backend": "disabled"},
								"rollup_buffered":       true,
								"deduped":               deduped,
								"latest_hash_unchanged": deduped,
								"source":                "go_memory_store",
							}
							fanout := map[string]any{
								"go_memory_store": "succeeded",
								"python_backend":  "disabled",
							}
							warnings := []string{}
							fanoutCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 4*time.Second)
							fanoutStatus, fanoutErr := s.upsertPgvectorFromWrite(fanoutCtx, item)
							cancel()
							if strings.TrimSpace(fanoutStatus) != "" {
								fanout["postgres_pgvector"] = fanoutStatus
							}
							if fanoutErr != nil {
								warnings = append(warnings, "pgvector fanout "+fanoutStatus+": "+fanoutErr.Error())
								log.Printf("pgvector fanout error project=%s file=%s status=%s err=%v", item.project, item.fileName, fanoutStatus, fanoutErr)
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
						forwardPayload := buildForwardPayload(singlePath, item, s.writePolicy.fanoutExcludeTargets)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        failed == 0,
		"partial":   failed > 0 && succeeded > 0,
		"source":    batchPath,
		"accepted":  len(items),
		"succeeded": succeeded,
		"failed":    failed,
		"results":   resultRows,
	})
}

func mergeForwardPayload(
	path string,
	original map[string]any,
	item normalizedWrite,
	fanoutExcludeTargets []string,
) map[string]any {
	if path == "/memory/write" {
		forward := cloneMap(original)
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
		forward := cloneMap(original)
		rawItem, _ := forward["item"].(map[string]any)
		if rawItem == nil {
			rawItem = map[string]any{}
		}
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
