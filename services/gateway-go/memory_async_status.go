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
		if source != "" {
			sourceState[source] = advanceContinuationSourceState(sourceState[source], continuationEventState(event))
		}
		if status == "error" || status == "failed" {
			lastError = strings.TrimSpace(anyToString(event["error"]))
		}
	}

	startedAt := now
	if parsed, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		startedAt = parsed
	}
	if startedAt.After(now) {
		startedAt = now
	}

	pendingSources := []string{}
	failedSources := []string{}
	returnedNow := []string{}
	for source, state := range sourceState {
		switch {
		case state == "pending":
			pendingSources = append(pendingSources, source)
		case continuationStateFailed(state):
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

	expectedBySource := map[string]float64{}
	weightedTotal := 0.0
	weightedProgress := 0.0
	pendingRemainingSecs := 0.0
	elapsedSecs := now.Sub(startedAt).Seconds()
	if elapsedSecs < 0 {
		elapsedSecs = 0
	}
	for source, state := range sourceState {
		timeout := 8 * time.Second
		if configured, ok := s.retrieval.sourceTimeouts[source]; ok && configured > 0 {
			timeout = configured
		}
		if continuationTimeout, ok := s.retrieval.continuationTimeoutBySource[source]; ok && continuationTimeout > timeout {
			timeout = continuationTimeout
		}
		_, adaptive := s.adaptiveTimeoutForSource(source, timeout)
		expected := anyToFloat64(adaptive["adjusted_timeout_secs"], timeout.Seconds())
		if expected <= 0 {
			expected = timeout.Seconds()
		}
		if expected <= 0 {
			expected = 1
		}
		expectedBySource[source] = roundFloat(expected, 3)
		weightedTotal += expected

		switch strings.ToLower(strings.TrimSpace(state)) {
		case "pending":
			ratio := 0.0
			if expected > 0 {
				ratio = elapsedSecs / expected
			}
			if ratio < 0 {
				ratio = 0
			}
			if ratio > 0.95 {
				ratio = 0.95
			}
			weightedProgress += expected * ratio
			remaining := expected - elapsedSecs
			if remaining < 0 {
				remaining = 0
			}
			pendingRemainingSecs += remaining
		default:
			weightedProgress += expected
		}
	}
	modeledProgress := 0.0
	if weightedTotal > 0 {
		modeledProgress = weightedProgress / weightedTotal
	}
	if modeledProgress < 0 {
		modeledProgress = 0
	}
	if status == "completed" {
		modeledProgress = 1.0
		pendingRemainingSecs = 0
	} else if modeledProgress >= 1.0 {
		modeledProgress = 0.999
	}

	confidence := 0.35
	if len(sourceState) >= 2 {
		confidence += 0.1
	}
	if len(sourceState) >= 3 {
		confidence += 0.1
	}
	if len(history) >= 4 {
		confidence += 0.15
	}
	if len(pendingSources) == 0 {
		confidence += 0.2
	} else if len(pendingSources) > 2 {
		confidence -= 0.05
	}
	if confidence < 0.2 {
		confidence = 0.2
	}
	if confidence > 0.95 {
		confidence = 0.95
	}
	progressModel := map[string]any{
		"probabilistic":            true,
		"progress":                 roundFloat(modeledProgress, 4),
		"progress_pct":             roundFloat(modeledProgress*100.0, 2),
		"eta_secs":                 roundFloat(pendingRemainingSecs, 3),
		"confidence":               roundFloat(confidence, 3),
		"confidence_band":          continuationConfidenceBand(confidence),
		"elapsed_secs":             roundFloat(elapsedSecs, 3),
		"pending_sources":          pendingSources,
		"estimated_by_source_secs": expectedBySource,
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
		"sources":                 mapKeysSorted(toSourceStringSet(sourceState)),
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
	lifecycle["modeled_progress"] = progressModel
	retrievalProgress := buildRetrievalProgressPayload(
		token,
		status,
		resultState,
		createdAt,
		updatedAt,
		completedAt,
		continuationPollURL,
		continuationEventsURL,
		sourceSummary,
		lifecycle,
		progressModel,
		"",
		"",
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
			"token":              token,
			"status":             status,
			"poll_url":           continuationPollURL,
			"events_url":         continuationEventsURL,
			"legacy_poll_url":    jobPollURL,
			"legacy_events_url":  eventsURL,
			"modeled_progress":   progressModel,
			"retrieval_progress": retrievalProgress,
		},
		"modeled_progress":    progressModel,
		"retrieval_progress":  retrievalProgress,
		"retrieval_lifecycle": lifecycle,
	}
	if includeResult {
		payload["result"] = map[string]any{
			"result_state":        resultState,
			"source_summary":      sourceSummary,
			"modeled_progress":    progressModel,
			"retrieval_lifecycle": lifecycle,
			"warnings":            []string{},
		}
		if lastError != "" {
			payload["error"] = lastError
		}
	}
	return payload, true
}

func continuationStateFailed(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "failed")
}

func toSourceStringSet(values map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for source := range values {
		out[source] = struct{}{}
	}
	return out
}

func continuationEventState(event map[string]any) string {
	eventName := strings.TrimSpace(strings.ToLower(anyToString(event["event"])))
	status := strings.TrimSpace(strings.ToLower(anyToString(event["status"])))
	switch eventName {
	case "completed":
		if status == "ok" || status == "succeeded" || status == "success" || status == "ready" || status == "empty" {
			return "ready"
		}
		return "failed"
	case "dropped", "failed":
		return "failed"
	case "queued", "running", "deferred", "deferred_retry", "retrying", "heartbeat", "skipped":
		if status == "invalid_source" || status == "durable_max_attempts" || status == "unavailable" {
			return "failed"
		}
		return "pending"
	default:
		switch status {
		case "ok", "succeeded", "success", "ready":
			return "ready"
		case "error", "failed", "invalid_source", "durable_max_attempts", "unavailable":
			return "failed"
		default:
			// Unknown queue states must never be interpreted as returned evidence.
			return "pending"
		}
	}
}

func advanceContinuationSourceState(current string, next string) string {
	current = strings.TrimSpace(strings.ToLower(current))
	next = strings.TrimSpace(strings.ToLower(next))
	if current == "ready" || current == "failed" {
		return current
	}
	if next == "ready" || next == "failed" || next == "pending" {
		return next
	}
	return "pending"
}

func continuationAgentEventType(status string, resultState string) string {
	normalizedStatus := strings.TrimSpace(strings.ToLower(status))
	normalizedState := strings.TrimSpace(strings.ToLower(resultState))
	if normalizedStatus == "completed" {
		if normalizedState == "ready" {
			return "retrieval.continuation.ready"
		}
		if normalizedState == "degraded" {
			return "retrieval.continuation.degraded"
		}
		return "retrieval.continuation.completed"
	}
	return "retrieval.continuation.progress"
}

func buildRetrievalProgressPayload(
	token string,
	status string,
	resultState string,
	createdAt string,
	updatedAt string,
	completedAt string,
	pollURL string,
	eventsURL string,
	sourceSummary map[string]any,
	lifecycle map[string]any,
	progressModel map[string]any,
	agentID string,
	sessionID string,
) map[string]any {
	token = strings.TrimSpace(token)
	sessionID = strings.TrimSpace(sessionID)
	eventType := continuationAgentEventType(status, resultState)
	watchCommand := "contextlattice_agent_session watch --continuation-token " + token + " --pretty"
	if sessionID != "" {
		watchCommand = "contextlattice_agent_session watch --session-id " + sessionID + " --continuation-token " + token + " --pretty"
	}
	pollCommand := "curl -fsS http://127.0.0.1:8075" + pollURL
	payload := map[string]any{
		"ok":                  true,
		"schema_id":           retrievalProgressContractID,
		"token":               token,
		"status":              strings.TrimSpace(strings.ToLower(status)),
		"result_state":        strings.TrimSpace(strings.ToLower(resultState)),
		"created_at":          strings.TrimSpace(createdAt),
		"updated_at":          strings.TrimSpace(updatedAt),
		"completed_at":        strings.TrimSpace(completedAt),
		"modeled_progress":    cloneAnyMap(progressModel),
		"source_summary":      cloneAnyMap(sourceSummary),
		"retrieval_lifecycle": cloneAnyMap(lifecycle),
		"poll_url":            pollURL,
		"events_url":          eventsURL,
		"agent_visibility": map[string]any{
			"best_surface":       "continuation_sse_for_live_agents_session_watch_for_local_agents_context_pack_for_next_call",
			"watch_command":      watchCommand,
			"poll_command":       pollCommand,
			"session_event_type": eventType,
		},
	}
	return attachPayloadFormatContract(retrievalProgressContractID, payload, agentID, "retrieval_progress", "/memory/search/continuations/{token}")
}

func continuationConfidenceBand(confidence float64) string {
	if confidence >= 0.75 {
		return "high"
	}
	if confidence >= 0.5 {
		return "medium"
	}
	return "low"
}
