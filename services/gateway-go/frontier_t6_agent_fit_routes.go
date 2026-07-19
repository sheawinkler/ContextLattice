package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	frontierT6SteeringPath       = "/memory/agent-fit/steering"
	frontierT6SteeringEventsPath = "/memory/agent-fit/steering/events"
	frontierT6SelectionPath      = "/memory/agent-fit/selection"
	frontierT6ProfilePath        = "/memory/agent-fit/profile"
	frontierT6ContextPrepPath    = "/memory/agent-fit/context-prep"
	frontierT6TelemetryPath      = "/telemetry/agent-fit"
)

func (s *server) frontierT6OwnerAuthorization(_ *http.Request, _ string, _ string) (frontierT6RequestAuthorization, error) {
	identity := "owner-local"
	if s != nil && s.contextPassports != nil && s.contextPassports.identity != nil {
		identity = firstNonEmptyStrings(s.contextPassports.identity.SigningKeyID, s.contextPassports.identity.InstanceID, identity)
	} else if build := contextLatticeBuildIdentity(); len(build) > 0 {
		identity = firstNonEmptyStrings(anyToString(build["source_commit"]), identity)
	}
	workspaceDigest := frontierT6OpaqueDigest("frontier-t6-owner-workspace", identity)
	workspaceID := "local_" + strings.TrimPrefix(workspaceDigest, "sha256:")[:24]
	return frontierT6RequestAuthorization{
		Authorized: true, WorkspaceID: workspaceID, SubjectID: "owner-local",
		AuthorizationDigest: frontierT6Digest(map[string]any{
			"mode": "owner_local", "workspace_id": workspaceID, "identity": identity, "version": 1,
		}),
		AllowActivation: true, AllowPersistence: true, AllowWorker: true,
	}, nil
}

func (s *server) frontierT6Authorized(w http.ResponseWriter, r *http.Request) bool {
	if s == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_runtime_unavailable"})
		return false
	}
	_, ok := s.prepareAuthorizedHeaders(w, r)
	return ok
}

func (s *server) frontierT6Handlers() frontierT6AgentFitHandlers {
	return frontierT6AgentFitHandlers{Store: s.frontierT6, Authorize: s.frontierT6OwnerAuthorization}
}

func frontierT6AttachFormatContract(schemaID string, payload any, surface string) map[string]any {
	raw, err := json.Marshal(payload)
	if err != nil {
		return attachPayloadFormatContract(schemaID, map[string]any{"schema_id": schemaID}, "", surface, "")
	}
	result := map[string]any{}
	if err := json.Unmarshal(raw, &result); err != nil {
		result = map[string]any{"schema_id": schemaID}
	}
	return attachPayloadFormatContract(schemaID, result, "", surface, "")
}

func (s *server) frontierT6SteeringRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT6Authorized(w, r) {
		return
	}
	s.frontierT6Handlers().Steering(w, r)
}

func (s *server) frontierT6SelectionRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT6Authorized(w, r) {
		return
	}
	s.frontierT6Handlers().Selection(w, r)
}

func (s *server) frontierT6ProfileRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT6Authorized(w, r) {
		return
	}
	s.frontierT6Handlers().Profile(w, r)
}

func (s *server) frontierT6ContextPrepRoute(w http.ResponseWriter, r *http.Request) {
	if !s.frontierT6Authorized(w, r) {
		return
	}
	s.frontierT6Handlers().ContextPrep(w, r)
}

func frontierT6QueryBool(r *http.Request, key string, fallback bool) bool {
	if r == nil {
		return fallback
	}
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func frontierT6WriteSSEEvent(w http.ResponseWriter, flusher http.Flusher, eventID, eventType string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(encoded) > 256*1024 {
		return errors.New("Frontier T6 SSE event exceeds the bounded response limit")
	}
	if eventID != "" {
		if _, err := w.Write([]byte("id: " + strings.TrimSpace(eventID) + "\n")); err != nil {
			return err
		}
	}
	if _, err := w.Write([]byte("event: " + strings.TrimSpace(eventType) + "\n")); err != nil {
		return err
	}
	if _, err := w.Write([]byte("data: " + string(encoded) + "\n\n")); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (s *server) frontierT6SteeringEventsRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	if !s.frontierT6Authorized(w, r) || s.frontierT6 == nil || !s.frontierT6.enabled {
		if s != nil && (s.frontierT6 == nil || !s.frontierT6.enabled) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_store_unavailable"})
		}
		return
	}
	auth, err := s.frontierT6OwnerAuthorization(r, frontierT6AsyncSteeringFeatureID, "claim")
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "frontier_t6_authorization_required"})
		return
	}
	query := r.URL.Query()
	scope := frontierT6Scope{
		WorkspaceID: auth.WorkspaceID,
		Project:     firstNonEmptyStrings(query.Get("project"), "contextlattice"),
		SessionID:   query.Get("session_id"),
		AgentID:     query.Get("agent_id"),
	}
	subscriberID := firstNonEmptyStrings(query.Get("subscriber_id"), scope.AgentID)
	cursor := firstNonEmptyStrings(r.Header.Get("Last-Event-ID"), query.Get("cursor"))
	limit := clampInt(anyToInt(query.Get("limit"), 16), 1, 128)
	maxSeconds := clampInt(anyToInt(query.Get("max_seconds"), 15), 1, 30)
	once := frontierT6QueryBool(r, "once", false)
	capabilities := frontierT6HarnessCapabilities{
		HarnessID: firstNonEmptyStrings(query.Get("harness_id"), "contextlattice-cli"), Transport: "sse",
		SupportsSSE: true, SupportsEventIDs: true, SupportsResume: true, SupportsAck: true,
		InjectionBoundaries: []string{"after_tool", "before_model_call", "idle"}, MaxEventBytes: 256 * 1024,
	}
	batch, err := s.frontierT6.claimSteering(scope, subscriberID, capabilities, cursor, time.Now().UTC(), limit)
	if err != nil {
		frontierT6WriteHandlerError(w, err)
		return
	}
	if !batch.PushNative {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "sse_capability_negotiation_failed", "pull_fallback_available": true,
			"fallback": batch,
		})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "sse_flushing_unavailable", "pull_fallback_available": true})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	deadline := time.NewTimer(time.Duration(maxSeconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	delivered := 0
	for {
		for _, item := range batch.Deliveries {
			payload := frontierT6AttachFormatContract(frontierT6SteeringStreamItemID, map[string]any{
				"ok": true, "schema_id": frontierT6SteeringStreamItemID,
				"delivery_id": item.DeliveryID, "claim_token": item.ClaimToken,
				"cursor": item.Event.Cursor, "event": item.Event,
				"injection_performed": false, "requires_explicit_agent_use": true,
			}, "agent_sse")
			if err := frontierT6WriteSSEEvent(w, flusher, item.Event.Cursor, "steering", payload); err != nil {
				return
			}
			_ = s.frontierT6.recordSteeringDelivered(item.DeliveryID, item.ClaimToken, time.Now().UTC())
			cursor = item.Event.Cursor
			delivered++
		}
		if once || delivered >= limit {
			_ = frontierT6WriteSSEEvent(w, flusher, cursor, "stream-end", map[string]any{
				"ok": true, "schema_id": frontierT6SteeringDeliverySchemaID, "cursor": cursor,
				"delivered": delivered, "reason": "bounded_stream_complete",
			})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			_ = frontierT6WriteSSEEvent(w, flusher, cursor, "stream-end", map[string]any{
				"ok": true, "schema_id": frontierT6SteeringDeliverySchemaID, "cursor": cursor,
				"delivered": delivered, "reason": "bounded_wait_complete",
			})
			return
		case <-ticker.C:
			batch, err = s.frontierT6.claimSteering(scope, subscriberID, capabilities, cursor, time.Now().UTC(), limit-delivered)
			if err != nil {
				_ = frontierT6WriteSSEEvent(w, flusher, cursor, "stream-error", map[string]any{
					"ok": false, "schema_id": frontierT6SteeringDeliverySchemaID, "error": "bounded_replay_failed",
				})
				return
			}
		}
	}
}

func (s *server) frontierT6TelemetryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	if !s.frontierT6Authorized(w, r) {
		return
	}
	if s.frontierT6 == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_store_unavailable"})
		return
	}
	s.frontierT6.mu.RLock()
	steeringEvents := len(s.frontierT6.state.SteeringEvents)
	steeringDeliveries := len(s.frontierT6.state.SteeringDeliveries)
	profiles := len(s.frontierT6.state.Profiles)
	contextPreps := len(s.frontierT6.state.ContextPreps)
	lastSteeringSequence := s.frontierT6.state.LastSteeringSequence
	replayFloor := frontierT6Cursor(s.frontierT6.state.SteeringAnchorSequence)
	updatedAt := s.frontierT6.state.UpdatedAt
	stateHash := s.frontierT6.state.StateHash
	limits := s.frontierT6.limits
	enabled := s.frontierT6.enabled
	s.frontierT6.mu.RUnlock()
	payload := map[string]any{
		"ok": true, "schema_id": frontierT6StatusSchemaID, "enabled": enabled,
		"steering_events": steeringEvents, "steering_deliveries": steeringDeliveries,
		"profiles": profiles, "context_preps": contextPreps,
		"last_steering_sequence": lastSteeringSequence, "replay_floor": replayFloor,
		"updated_at": updatedAt, "state_hash": stateHash,
		"limits": map[string]any{
			"max_bytes": limits.MaxBytes, "max_events": limits.MaxEvents, "max_deliveries": limits.MaxDeliveries,
			"max_profiles": limits.MaxProfiles, "max_context_preps": limits.MaxPreps,
		},
		"execution_owner": "external_cli_worker", "gateway_runner_execution": false,
		"automatic_model_execution": false, "network_calls": 0,
	}
	writeJSON(w, http.StatusOK, frontierT6AttachFormatContract(frontierT6StatusSchemaID, payload, "telemetry"))
}
