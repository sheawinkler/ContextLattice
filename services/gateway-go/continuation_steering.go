package main

import "strings"

func (s *server) claimContinuationSteering(token string, status string, resultState string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	state := "progress"
	if strings.EqualFold(strings.TrimSpace(status), "completed") {
		state = "terminal:" + strings.ToLower(strings.TrimSpace(resultState))
	}
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()
	if s.continuationSteeringState == nil {
		s.continuationSteeringState = map[string]string{}
	}
	previous := s.continuationSteeringState[token]
	if strings.HasPrefix(previous, "terminal:") || previous == state {
		return false
	}
	if state == "progress" && previous != "" {
		return false
	}
	s.continuationSteeringState[token] = state
	return true
}

func continuationRequestSessionID(request map[string]any) string {
	return strings.TrimSpace(firstNonEmptyStrings(
		anyToString(request["session_id"]),
		anyToString(request["sessionId"]),
	))
}

func continuationRequestAgentID(request map[string]any) string {
	return strings.TrimSpace(firstNonEmptyStrings(
		anyToString(request["agent_id"]),
		anyToString(request["agentId"]),
	))
}

func continuationEventWithRequest(request map[string]any, payload map[string]any) map[string]any {
	event := cloneAnyMap(payload)
	if sessionID := continuationRequestSessionID(request); sessionID != "" {
		event["session_id"] = sessionID
	}
	if agentID := continuationRequestAgentID(request); agentID != "" {
		event["agent_id"] = agentID
	}
	if project := strings.TrimSpace(anyToString(request["project"])); project != "" {
		event["project"] = project
	}
	if topicPath := strings.TrimSpace(anyToString(request["topic_path"])); topicPath != "" {
		event["topic_path"] = topicPath
	}
	if trafficClass := strings.TrimSpace(strings.ToLower(anyToString(request["traffic_class"]))); trafficClass != "" {
		event["traffic_class"] = trafficClass
	}
	return event
}

func (s *server) emitContinuationSteering(request map[string]any, token string, source string, trigger map[string]any) {
	if retrievalEvaluationSideEffectsSuppressed(nil, request) || strings.EqualFold(strings.TrimSpace(anyToString(trigger["traffic_class"])), "evaluation_holdout") || anyToBool(trigger["side_effects_suppressed"]) {
		return
	}
	sessionID := continuationRequestSessionID(request)
	if sessionID == "" {
		return
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	statusPayload, ok := s.continuationStatusPayload(token, false)
	if !ok {
		return
	}
	agentID := continuationRequestAgentID(request)
	status := anyToString(statusPayload["status"])
	lifecycle := anyMap(statusPayload["retrieval_lifecycle"])
	progressBase := anyMap(statusPayload["retrieval_progress"])
	resultState := firstNonEmptyStrings(anyToString(progressBase["result_state"]), anyToString(lifecycle["result_state"]))
	if !s.claimContinuationSteering(token, status, resultState) {
		return
	}
	sourceSummary := anyMap(progressBase["source_summary"])
	if len(sourceSummary) == 0 {
		sourceSummary = anyMap(anyMap(statusPayload["result"])["source_summary"])
	}
	progressModel := anyMap(progressBase["modeled_progress"])
	if len(progressModel) == 0 {
		progressModel = anyMap(statusPayload["modeled_progress"])
	}
	progress := buildRetrievalProgressPayload(
		token,
		status,
		resultState,
		firstNonEmptyStrings(anyToString(progressBase["created_at"]), anyToString(statusPayload["created_at"])),
		firstNonEmptyStrings(anyToString(progressBase["updated_at"]), anyToString(statusPayload["updated_at"])),
		firstNonEmptyStrings(anyToString(progressBase["completed_at"]), anyToString(statusPayload["completed_at"])),
		firstNonEmptyStrings(anyToString(progressBase["poll_url"]), anyToString(statusPayload["continuation_poll_url"])),
		firstNonEmptyStrings(anyToString(progressBase["events_url"]), anyToString(statusPayload["continuation_events_url"])),
		sourceSummary,
		lifecycle,
		progressModel,
		agentID,
		sessionID,
	)
	visibility := anyMap(progress["agent_visibility"])
	eventType := strings.TrimSpace(anyToString(visibility["session_event_type"]))
	if eventType == "" {
		eventType = continuationAgentEventType(anyToString(progress["status"]), anyToString(progress["result_state"]))
	}
	steering := buildSteeringCommentPayload(request, progress, eventType, source, trigger)
	message := anyToString(steering["message"])
	eventStatus := "active"
	if strings.Contains(eventType, ".ready") || strings.Contains(eventType, ".completed") {
		eventStatus = "completed"
	} else if strings.Contains(eventType, ".degraded") {
		eventStatus = "degraded"
	}
	s.recordAgentSessionEvent(sessionID, eventType, map[string]any{
		"agent_id": agentID,
		"project":  strings.TrimSpace(anyToString(request["project"])),
		"summary":  message,
		"status":   eventStatus,
		"metadata": map[string]any{
			"agent_visible":       true,
			"continuation_token":  token,
			"continuation_source": strings.TrimSpace(strings.ToLower(source)),
			"trigger_event":       strings.TrimSpace(anyToString(trigger["event"])),
			"trigger_status":      strings.TrimSpace(anyToString(trigger["status"])),
			"retrieval_progress":  progress,
			"steering_comment":    steering,
			"modeled_progress":    anyMap(progress["modeled_progress"]),
			"source_summary":      anyMap(progress["source_summary"]),
		},
	})
}

func buildSteeringCommentPayload(
	request map[string]any,
	progress map[string]any,
	eventType string,
	source string,
	trigger map[string]any,
) map[string]any {
	sessionID := continuationRequestSessionID(request)
	agentID := continuationRequestAgentID(request)
	project := strings.TrimSpace(anyToString(request["project"]))
	status := strings.TrimSpace(strings.ToLower(anyToString(progress["status"])))
	resultState := strings.TrimSpace(strings.ToLower(anyToString(progress["result_state"])))
	sourceSummary := anyMap(progress["source_summary"])
	pending := anyToStringList(sourceSummary["pending_sources"], 8)
	failed := anyToStringList(sourceSummary["failed_sources"], 8)
	returned := anyToStringList(sourceSummary["returned_now"], 8)
	message := "Async retrieval is still warming; keep working from fast-now evidence and watch the continuation before making final evidence-backed claims."
	suggestedAction := "Watch the continuation stream or rerun context packaging before the next hard model call if slow-source evidence matters."
	severity := "info"
	reason := "Slow-source retrieval is still in progress."
	switch {
	case status == "completed" && resultState == "ready":
		message = "Async retrieval is ready; slow-source evidence has finished warming for this request."
		suggestedAction = "Rerun context packaging or poll the continuation result before finalizing claims that depend on complete recall."
		reason = "Continuation completed successfully with no failed slow sources."
	case status == "completed" && resultState == "degraded":
		message = "Async retrieval finished with degraded sources; use the returned evidence, but do not treat slow-source recall as complete."
		suggestedAction = "Retry with a narrower query, longer timeout, or smaller source set before making claims that require the failed source."
		severity = "warning"
		reason = "Continuation completed with failed or pressure-shed sources: " + strings.Join(failed, ", ")
	case status == "completed":
		message = "Async retrieval finished, but no additional slow-source evidence was returned."
		suggestedAction = "Continue with fast-now evidence or retry with a narrower query if more recall is required."
		reason = "Continuation reached a terminal state with result_state=" + resultState + "."
	}
	delivery := anyMap(progress["agent_visibility"])
	if len(delivery) == 0 {
		token := anyToString(progress["token"])
		watch := "contextlattice_agent_session watch --continuation-token " + token + " --pretty"
		if sessionID != "" {
			watch = "contextlattice_agent_session watch --session-id " + sessionID + " --continuation-token " + token + " --pretty"
		}
		delivery = map[string]any{
			"session_event_type": eventType,
			"best_surface":       "session_watch_for_local_agents_continuation_sse_for_live_hosts",
			"watch_command":      watch,
			"poll_command":       "curl -fsS http://127.0.0.1:8075" + anyToString(progress["poll_url"]),
		}
	}
	payload := map[string]any{
		"ok":                 true,
		"schema_id":          steeringCommentContractID,
		"audience":           "requesting_agent",
		"severity":           severity,
		"message":            message,
		"suggested_action":   suggestedAction,
		"reason":             reason,
		"project":            project,
		"session_id":         sessionID,
		"agent_id":           agentID,
		"source":             strings.TrimSpace(strings.ToLower(source)),
		"trigger_event":      strings.TrimSpace(anyToString(trigger["event"])),
		"trigger_status":     strings.TrimSpace(anyToString(trigger["status"])),
		"returned_sources":   returned,
		"pending_sources":    pending,
		"failed_sources":     failed,
		"retrieval_progress": progress,
		"delivery":           delivery,
	}
	return attachPayloadFormatContract(steeringCommentContractID, payload, agentID, "agent_steering", "/v1/agents/sessions/{session_id}/events")
}
