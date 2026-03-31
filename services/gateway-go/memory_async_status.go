package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

func (s *server) memorySearchAsyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	token := asyncSearchToken(r.URL.Path)
	if token == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "async search token not found or expired"})
		return
	}
	includeResult, includeProvided := parseBoolLoose(r.URL.Query().Get("include_result"))
	if !includeProvided {
		includeResult = true
	}
	payload, ok := s.continuationStatusPayload(token, includeResult)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "async search token not found or expired"})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) memorySearchJobsRoute(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(strings.TrimSpace(r.URL.Path), "/events") {
		s.memorySearchJobsEvents(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	token := jobsSearchToken(r.URL.Path, false)
	if token == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "async search token not found or expired"})
		return
	}
	includeResult, includeProvided := parseBoolLoose(r.URL.Query().Get("include_result"))
	if !includeProvided {
		includeResult = true
	}
	payload, ok := s.continuationStatusPayload(token, includeResult)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "async search token not found or expired"})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) memorySearchContinuationsRoute(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(strings.TrimSpace(r.URL.Path), "/events") {
		s.continuationEvents(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	token := continuationSearchToken(r.URL.Path, false)
	if token == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "async search token not found or expired"})
		return
	}
	includeResult, includeProvided := parseBoolLoose(r.URL.Query().Get("include_result"))
	if !includeProvided {
		includeResult = true
	}
	payload, ok := s.continuationStatusPayload(token, includeResult)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "async search token not found or expired"})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) memorySearchJobsEvents(w http.ResponseWriter, r *http.Request) {
	token := jobsSearchToken(r.URL.Path, true)
	if token == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "continuation stream not found"})
		return
	}
	cloneReq := r.Clone(r.Context())
	if cloneReq.URL == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	cloneReq.URL.Path = "/memory/search/continuations/" + token + "/events"
	s.continuationEvents(w, cloneReq)
}

func asyncSearchToken(path string) string {
	const prefix = "/memory/search/async/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

func jobsSearchToken(path string, requireEvents bool) string {
	const prefix = "/memory/search/jobs/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	tail := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if tail == "" {
		return ""
	}
	if strings.HasSuffix(tail, "/events") {
		return strings.Trim(strings.TrimSuffix(tail, "/events"), "/")
	}
	if requireEvents {
		return ""
	}
	if strings.Contains(tail, "/") {
		return ""
	}
	return tail
}

func continuationSearchToken(path string, requireEvents bool) string {
	const prefix = "/memory/search/continuations/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	tail := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if tail == "" {
		return ""
	}
	if strings.HasSuffix(tail, "/events") {
		return strings.Trim(strings.TrimSuffix(tail, "/events"), "/")
	}
	if requireEvents {
		return ""
	}
	if strings.Contains(tail, "/") {
		return ""
	}
	return tail
}

func (s *server) continuationStatusPayload(token string, includeResult bool) (map[string]any, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, false
	}
	now := time.Now().UTC()
	s.continuationMu.Lock()
	s.pruneContinuationLocked(now)
	historyRaw, historyOK := s.continuationHistory[token]
	expiresAt, expiresOK := s.continuationExpiry[token]
	history := make([]map[string]any, 0, len(historyRaw))
	for _, row := range historyRaw {
		history = append(history, cloneAnyMap(row))
	}
	s.continuationMu.Unlock()
	if !historyOK && !expiresOK {
		return nil, false
	}

	createdAt := ""
	updatedAt := ""
	lastError := ""
	sourceState := map[string]string{}
	for _, event := range history {
		at := strings.TrimSpace(anyToString(event["at"]))
		if createdAt == "" && at != "" {
			createdAt = at
		}
		if at != "" {
			updatedAt = at
		}
		source := strings.TrimSpace(strings.ToLower(anyToString(event["source"])))
		status := strings.TrimSpace(strings.ToLower(anyToString(event["status"])))
		eventName := strings.TrimSpace(strings.ToLower(anyToString(event["event"])))
		if source != "" {
			switch {
			case eventName == "queued" || status == "queued":
				sourceState[source] = "queued"
			case eventName == "completed":
				if status == "ok" || status == "succeeded" {
					sourceState[source] = "completed"
				} else {
					sourceState[source] = "failed"
				}
			case eventName == "skipped":
				if status == "" {
					status = "skipped"
				}
				sourceState[source] = status
			case status != "":
				sourceState[source] = status
			}
		}
		if status == "error" || status == "failed" {
			lastError = strings.TrimSpace(anyToString(event["error"]))
		}
	}

	pendingSources := []string{}
	failedSources := []string{}
	returnedNow := []string{}
	for source, state := range sourceState {
		switch state {
		case "queued", "running", "pending":
			pendingSources = append(pendingSources, source)
		case "error", "failed", "max_inflight", "cooldown", "inflight_per_source":
			failedSources = append(failedSources, source)
		default:
			returnedNow = append(returnedNow, source)
		}
	}
	sort.Strings(pendingSources)
	sort.Strings(failedSources)
	sort.Strings(returnedNow)

	status := "queued"
	resultState := "pending"
	completedAt := ""
	switch {
	case len(history) == 0:
		status = "queued"
		resultState = "pending"
	case len(pendingSources) > 0:
		status = "running"
		resultState = "pending"
	default:
		status = "completed"
		completedAt = updatedAt
		if len(failedSources) > 0 {
			resultState = "degraded"
		} else if len(returnedNow) > 0 {
			resultState = "ready"
		} else {
			resultState = "empty"
		}
	}

	expiresAtText := ""
	if expiresOK {
		expiresAtText = expiresAt.Format(time.RFC3339Nano)
	}
	pollURL := "/memory/search/async/" + token
	jobPollURL := "/memory/search/jobs/" + token
	eventsURL := "/memory/search/jobs/" + token + "/events"
	continuationPollURL := "/memory/search/continuations/" + token
	continuationEventsURL := "/memory/search/continuations/" + token + "/events"
	sourceSummary := map[string]any{
		"sources":                 []string{},
		"returned_now":            returnedNow,
		"pending_sources":         pendingSources,
		"warming_sources":         pendingSources,
		"timed_out_sources":       []string{},
		"failed_sources":          failedSources,
		"budget_exceeded_sources": []string{},
		"skipped_sources":         []string{},
	}
	lifecycle := buildRetrievalLifecyclePayload(
		resultState,
		returnedNow,
		pendingSources,
		pendingSources,
		failedSources,
		[]string{},
		[]string{},
	)

	payload := map[string]any{
		"ok":                      true,
		"async":                   true,
		"job_id":                  token,
		"token":                   token,
		"status":                  status,
		"async_status":            status,
		"created_at":              createdAt,
		"updated_at":              updatedAt,
		"completed_at":            completedAt,
		"expires_at":              expiresAtText,
		"poll_url":                pollURL,
		"job_poll_url":            jobPollURL,
		"events_url":              eventsURL,
		"continuation_poll_url":   continuationPollURL,
		"continuation_events_url": continuationEventsURL,
		"continuation_async": map[string]any{
			"token":             token,
			"status":            status,
			"poll_url":          continuationPollURL,
			"events_url":        continuationEventsURL,
			"legacy_poll_url":   jobPollURL,
			"legacy_events_url": eventsURL,
		},
		"retrieval_lifecycle": lifecycle,
	}
	if includeResult {
		payload["result"] = map[string]any{
			"result_state":        resultState,
			"source_summary":      sourceSummary,
			"retrieval_lifecycle": lifecycle,
			"warnings":            []string{},
		}
		if lastError != "" {
			payload["error"] = lastError
		}
	}
	return payload, true
}
