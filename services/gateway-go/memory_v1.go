package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func splitEngineMemoryID(memoryID string) (string, string, error) {
	token := strings.TrimSpace(memoryID)
	if token == "" {
		return "", "", fmt.Errorf("memory_id is required")
	}
	var project string
	var fileName string
	if strings.Contains(token, "::") {
		parts := strings.SplitN(token, "::", 2)
		project = strings.TrimSpace(parts[0])
		fileName = strings.TrimSpace(parts[1])
	} else if strings.Contains(token, "/") {
		parts := strings.SplitN(token, "/", 2)
		project = strings.TrimSpace(parts[0])
		fileName = strings.TrimSpace(parts[1])
	} else {
		return "", "", fmt.Errorf("memory_id must be in '<project>::<file>' or '<project>/<file>' form")
	}
	if project == "" || fileName == "" {
		return "", "", fmt.Errorf("memory_id must include both project and file")
	}
	return project, fileName, nil
}

func (s *server) memoryV1Get(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	memoryID := strings.TrimSpace(r.URL.Query().Get("memory_id"))
	if memoryID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "memory_id is required"})
		return
	}
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		payload, err := s.memoryStore.fetchByMemoryID(memoryID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "memory_id not found"})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "memory store unavailable", "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	response, status, err := s.backendJSONRequest(
		r.Context(),
		http.MethodGet,
		"/v1/memory/get?memory_id="+url.QueryEscape(memoryID),
		incomingHeaders,
		nil,
	)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     err.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) memoryV1Update(w http.ResponseWriter, r *http.Request) {
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
	memoryID := strings.TrimSpace(anyToString(payload["memory_id"]))
	if memoryID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "memory_id is required"})
		return
	}
	patch, _ := payload["patch"].(map[string]any)
	project, fileName, splitErr := splitEngineMemoryID(memoryID)
	if splitErr != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": splitErr.Error()})
		return
	}

	previousPayload, fetchErr := s.fetchV1MemoryPayload(r.Context(), incomingHeaders, memoryID)
	if fetchErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     fetchErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	for key, value := range patch {
		previousPayload[key] = value
	}
	encoded, marshalErr := json.Marshal(previousPayload)
	if marshalErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to encode patch", "detail": marshalErr.Error()})
		return
	}

	item := map[string]any{
		"project":   project,
		"file_name": fileName,
		"content":   string(encoded),
	}
	if topicPath := strings.TrimSpace(anyToString(patch["topic_path"])); topicPath != "" {
		item["topic_path"] = topicPath
	}
	var (
		putResponse map[string]any
		putStatus   int
		putErr      error
	)
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		entry, _, storeErr := s.memoryStore.put(normalizedWrite{
			project:   project,
			fileName:  fileName,
			content:   anyToString(item["content"]),
			topicPath: anyToString(item["topic_path"]),
		})
		if storeErr != nil {
			putErr = storeErr
		} else {
			putStatus = http.StatusOK
			putResponse = map[string]any{
				"ok":        true,
				"memory_id": project + "::" + fileName,
				"result": map[string]any{
					"event_id": entry.EventID,
				},
			}
		}
	} else {
		putResponse, putStatus, putErr = s.backendJSONRequest(
			r.Context(),
			http.MethodPost,
			"/v1/memory/put",
			incomingHeaders,
			map[string]any{"item": item},
		)
	}
	if putErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     putErr.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	if putStatus < 200 || putStatus >= 300 {
		writeJSON(w, putStatus, putResponse)
		return
	}
	updatedID := strings.TrimSpace(anyToString(putResponse["memory_id"]))
	if updatedID == "" {
		if rawResult, ok := putResponse["result"].(map[string]any); ok {
			updatedID = strings.TrimSpace(anyToString(rawResult["event_id"]))
		}
	}
	if updatedID == "" {
		updatedID = memoryID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"memory_id": updatedID,
	})
}

func (s *server) memoryV1Neighbors(w http.ResponseWriter, r *http.Request) {
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
	memoryID := strings.TrimSpace(anyToString(payload["memory_id"]))
	if memoryID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "memory_id is required"})
		return
	}
	limit := anyToInt(payload["limit"], 10)
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}

	project, fileName, splitErr := splitEngineMemoryID(memoryID)
	if splitErr != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": splitErr.Error()})
		return
	}
	if !s.retrieval.enabled {
		if s.strictNoPythonRuntime {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":     false,
				"error":  "python_runtime_disabled",
				"reason": "neighbors_requires_retrieval",
			})
			return
		}
		backendResp, backendStatus, backendErr := s.backendJSONRequest(
			r.Context(),
			http.MethodPost,
			"/v1/memory/neighbors",
			incomingHeaders,
			payload,
		)
		if backendErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":      "backend unavailable",
				"detail":     backendErr.Error(),
				"backendUrl": s.backendURL,
			})
			return
		}
		writeJSON(w, backendStatus, backendResp)
		return
	}
	topicFilter := ""
	if filters, ok := payload["filters"].(map[string]any); ok {
		topicFilter = strings.TrimSpace(anyToString(filters["topic_path"]))
	}
	retrievalPayload := map[string]any{
		"query":   strings.TrimSpace(project + " " + fileName),
		"limit":   limit,
		"project": project,
	}
	if topicFilter != "" {
		retrievalPayload["topic_path"] = topicFilter
	}
	response, _, execErr := s.executeRetrieval(r.Context(), incomingHeaders, retrievalPayload, false)
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  "memory neighbors retrieval failed",
			"detail": execErr.Error(),
		})
		return
	}
	results, _ := response["results"].([]any)
	if results == nil {
		results = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *server) fetchV1MemoryPayload(
	ctx context.Context,
	incomingHeaders http.Header,
	memoryID string,
) (map[string]any, error) {
	if s.memoryStore != nil && s.memoryStore.policy.enabled {
		payload, err := s.memoryStore.fetchByMemoryID(memoryID)
		if err != nil {
			return nil, err
		}
		memoryRow, _ := payload["memory"].(map[string]any)
		content := strings.TrimSpace(anyToString(memoryRow["content"]))
		if content == "" {
			return map[string]any{}, nil
		}
		previousPayload, parseErr := parseJSONMap([]byte(content))
		if parseErr != nil {
			return map[string]any{"content": content}, nil
		}
		return previousPayload, nil
	}
	getResponse, getStatus, getErr := s.backendJSONRequest(
		ctx,
		http.MethodGet,
		"/v1/memory/get?memory_id="+url.QueryEscape(memoryID),
		incomingHeaders,
		nil,
	)
	if getErr != nil {
		return nil, getErr
	}
	if getStatus >= 400 {
		return nil, fmt.Errorf("memory fetch failed with status %d", getStatus)
	}
	memoryRow, _ := getResponse["memory"].(map[string]any)
	content := strings.TrimSpace(anyToString(memoryRow["content"]))
	if content == "" {
		return map[string]any{}, nil
	}
	previousPayload, parseErr := parseJSONMap([]byte(content))
	if parseErr != nil {
		return map[string]any{"content": content}, nil
	}
	return previousPayload, nil
}
