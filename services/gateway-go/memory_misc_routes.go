package main

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func methodAllowed(method string, allowed ...string) bool {
	normalized := strings.TrimSpace(strings.ToUpper(method))
	if normalized == "" {
		return false
	}
	for _, candidate := range allowed {
		if normalized == strings.TrimSpace(strings.ToUpper(candidate)) {
			return true
		}
	}
	return false
}

func backendPathWithQuery(path string, rawQuery string) string {
	trimmed := strings.TrimSpace(rawQuery)
	if trimmed == "" {
		return path
	}
	return path + "?" + trimmed
}

func (s *server) forwardJSONGET(w http.ResponseWriter, r *http.Request, backendPath string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	response, status, err := s.backendJSONRequest(
		r.Context(),
		http.MethodGet,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
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

func (s *server) forwardJSONPOST(w http.ResponseWriter, r *http.Request, backendPath string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
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
	response, status, backendErr := s.backendJSONRequest(
		r.Context(),
		http.MethodPost,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
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
	writeJSON(w, status, response)
}

func (s *server) forwardJSONAny(w http.ResponseWriter, r *http.Request, backendPath string) {
	method := strings.TrimSpace(strings.ToUpper(r.Method))
	if method == "" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if s.strictNoPythonRuntime {
		if !s.allowPythonHotPathFallback(w, backendPath, "strict_runtime_backend_forward_disabled") {
			return
		}
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	var payload map[string]any
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		trimmed := strings.TrimSpace(string(rawBody))
		if trimmed != "" {
			parsed, err := parseJSONMap(rawBody)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
				return
			}
			payload = parsed
		}
	}
	response, status, backendErr := s.backendJSONRequest(
		r.Context(),
		method,
		backendPathWithQuery(backendPath, r.URL.RawQuery),
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
	writeJSON(w, status, response)
}

func (s *server) memoryRecent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/recent")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset")))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	topicPath := strings.TrimSpace(r.URL.Query().Get("topic_path"))
	items := s.memoryStore.recentItems(project, topicPath, limit, offset)
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *server) memoryTopicTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/topics")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	writeJSON(w, http.StatusOK, s.memoryStore.topicTree(project))
}

func (s *server) memoryTopicList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/topics/list")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	minCount, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_count")))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	depth, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("depth")))
	writeJSON(w, http.StatusOK, s.memoryStore.topicList(project, minCount, limit, depth))
}

func (s *server) memoryTopicRollups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, "/memory/topic-rollups")
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	minCount, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("min_count")))
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset")))
	writeJSON(w, http.StatusOK, s.memoryStore.topicRollups(project, minCount, limit, offset))
}

func (s *server) memoryContinuitySnapshot(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONPOST(w, r, "/memory/continuity/snapshot")
}

func (s *server) memoryContinuitySnapshots(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, "/memory/continuity/snapshots")
}

func (s *server) memoryContinuitySnapshotByID(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, r.URL.Path)
}

func (s *server) memoryRecallEvalCases(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONGET(w, r, "/memory/recall/eval-cases")
}

func (s *server) memoryRecallEvalCasesRefresh(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONPOST(w, r, "/memory/recall/eval-cases/refresh")
}

func (s *server) memoryRecallEvaluateSaved(w http.ResponseWriter, r *http.Request) {
	s.forwardJSONPOST(w, r, "/memory/recall/evaluate/saved")
}

func (s *server) feedbackRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.forwardJSONGET(w, r, "/feedback")
	case http.MethodPost:
		s.forwardJSONPOST(w, r, "/feedback")
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) migrationRuntime(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.forwardJSONGET(w, r, "/migration/runtime")
	case http.MethodPost:
		s.forwardJSONPOST(w, r, "/migration/runtime")
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *server) memoryFilesByProject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s.memoryStore == nil || !s.memoryStore.policy.enabled {
		s.forwardJSONGET(w, r, r.URL.Path)
		return
	}
	project, fileName, err := s.memoryStore.parseProjectFileFromPath(r.URL.Path)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	content, info, readErr := s.memoryStore.readFile(project, fileName)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "memory file not found"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "memory file read failed", "detail": readErr.Error()})
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Header().Set("x-contextlattice-source", "go_memory_store")
	if info != nil {
		w.Header().Set("x-contextlattice-last-modified", info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func (s *server) agentsTasksRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONAny(w, r, r.URL.Path)
}

func (s *server) telemetryRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONGET(w, r, r.URL.Path)
}

func (s *server) maintenanceRoute(w http.ResponseWriter, r *http.Request) {
	if s.entitlementPathProtected(r.URL.Path) {
		if !s.enforceV4Entitlement(w, r, r.URL.Path) {
			return
		}
	}
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.forwardJSONAny(w, r, r.URL.Path)
}
