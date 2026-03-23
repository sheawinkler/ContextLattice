package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
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
							"warnings":              response["warnings"],
							"fanout":                response["fanout"],
							"telemetry_routed":      true,
							"blob_ref":              response["blob_ref"],
							"content_hash":          response["content_hash"],
							"storage_schema":        response["storage_schema"],
							"retention_window_days": response["retention_window_days"],
						}
						ok = true
					}
				} else {
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
		return nil, 0, fmt.Errorf("telemetry sink unavailable")
	}
	timeout := envDurationSeconds("GO_TELEMETRY_INGEST_TIMEOUT_SECS", 8)
	if timeout < time.Second {
		timeout = time.Second
	}
	ingestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	meta := map[string]any{
		"source_path": sourcePath,
		"agent_id":    strings.TrimSpace(anyToString(item.raw["agent_id"])),
		"topic_path":  item.topicPath,
		"ingested_by": "gateway-go",
	}
	result, err := s.telemetrySink.ingestWrite(ingestCtx, item, meta)
	if err != nil {
		return nil, 0, err
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

func (s *server) callBackendJSON(
	ctx context.Context,
	headers http.Header,
	method string,
	path string,
	payload map[string]any,
) (map[string]any, int, error) {
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
