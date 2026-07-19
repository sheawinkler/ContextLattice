package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
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

	frontierT6MaxPayloadBytes = 256 << 10
	frontierT6MaxOutputBytes  = 1 << 20
	frontierT6MaxSSELineBytes = 64 << 10
	frontierT6MaxWatchEvents  = 128
	frontierT6MaxWatchSeconds = 300
)

type frontierT6CLIOperation struct {
	path      string
	operation string
	kind      string
	scoped    bool
}

var frontierT6CLIOperations = map[string]frontierT6CLIOperation{
	"steering-publish":      {path: frontierT6SteeringPath, operation: "publish", scoped: true},
	"steering-replay":       {path: frontierT6SteeringPath, operation: "replay", scoped: true},
	"steering-ack":          {path: frontierT6SteeringPath, operation: "ack", scoped: true},
	"runner-select":         {path: frontierT6SelectionPath, kind: "runner"},
	"model-select":          {path: frontierT6SelectionPath, kind: "model"},
	"profile-resolve":       {path: frontierT6ProfilePath, operation: "resolve", scoped: true},
	"profile-configure":     {path: frontierT6ProfilePath, operation: "configure", scoped: true},
	"context-prep-schedule": {path: frontierT6ContextPrepPath, operation: "schedule", scoped: true},
	"context-prep-use":      {path: frontierT6ContextPrepPath, operation: "use", scoped: true},
}

func frontierT6AgentFitUsage() string {
	return "contextlattice agent-fit {steering-publish|steering-replay|steering-ack|steering-watch|runner-select|model-select|profile-resolve|profile-configure|context-prep-schedule|context-prep-use} --payload-file request.json [--project p] [--session-id id] [--agent-id id] [--base-url url]"
}

func (c *cli) cmdAgentFit(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"payload-file":  "payload_file",
		"agent-id":      "agent_id",
		"session-id":    "session_id",
		"subscriber-id": "subscriber_id",
		"cursor":        "cursor",
		"event-id":      "event_id",
		"delivery-id":   "delivery_id",
		"limit":         "limit",
		"max-seconds":   "max_seconds",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"once": "once"}))
	if parsed.bool("help") || len(parsed.pos) == 0 {
		return c.emitUsage(frontierT6AgentFitUsage())
	}
	if len(parsed.pos) != 1 {
		return fmt.Errorf("unexpected positional arguments for agent-fit: %s", strings.Join(parsed.pos[1:], " "))
	}

	operation := strings.ToLower(strings.TrimSpace(parsed.pos[0]))
	c.applyBaseURL(parsed)
	payload, err := frontierT6PayloadFromFile(parsed.string("payload_file", ""))
	if err != nil {
		return err
	}
	if frontierT6ContainsCredential(payload) {
		return errors.New("agent-fit payload contains a forbidden credential field; configure credentials through the CLI environment instead")
	}
	if operation == "steering-watch" {
		return c.frontierT6SteeringWatch(parsed, payload)
	}

	spec, ok := frontierT6CLIOperations[operation]
	if !ok {
		return fmt.Errorf("unknown agent-fit operation %q", operation)
	}
	frontierT6PrepareOperationPayload(payload, parsed, spec)
	result, status, err := c.requestJSON(context.Background(), http.MethodPost, spec.path, payload, parsed.float("timeout", 30))
	if err != nil {
		if status > 0 {
			return fmt.Errorf("agent-fit %s request failed with status %d", operation, status)
		}
		return fmt.Errorf("agent-fit %s request failed", operation)
	}
	return c.frontierT6EmitBounded(result, !parsed.bool("raw"))
}

func frontierT6PayloadFromFile(path string) (map[string]any, error) {
	payload := map[string]any{}
	path = strings.TrimSpace(path)
	if path == "" {
		return payload, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open agent-fit payload file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, frontierT6MaxPayloadBytes+1))
	if err != nil {
		return nil, errors.New("read agent-fit payload file failed")
	}
	if len(raw) > frontierT6MaxPayloadBytes {
		return nil, errors.New("agent-fit payload file exceeds the 256 KiB limit")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode agent-fit payload file: %w", err)
	}
	if payload == nil {
		return nil, errors.New("agent-fit payload file must contain one JSON object")
	}
	return payload, nil
}

func frontierT6PrepareOperationPayload(payload map[string]any, parsed parsedArgs, spec frontierT6CLIOperation) {
	if spec.operation != "" {
		payload["operation"] = spec.operation
	}
	if spec.kind != "" {
		payload["kind"] = spec.kind
	}
	if spec.scoped {
		payload["scope"] = frontierT6ScopeFromArgs(payload, parsed)
	}
	if parsed.has("subscriber_id") {
		payload["subscriber_id"] = parsed.string("subscriber_id", "")
	}
	if parsed.has("cursor") {
		payload["cursor"] = parsed.string("cursor", "")
	}
	if parsed.has("event_id") {
		payload["event_id"] = parsed.string("event_id", "")
	}
	if parsed.has("delivery_id") {
		payload["delivery_id"] = parsed.string("delivery_id", "")
	}
	if parsed.has("limit") {
		payload["limit"] = parsed.int("limit", 16)
	}
	if spec.path == frontierT6ProfilePath {
		agentID := parsed.string("agent_id", firstNonEmpty(firstString(payload["agent_id"]), envString("CONTEXTLATTICE_AGENT_ID", "")))
		if agentID != "" {
			payload["agent_id"] = agentID
		}
	}
}

func frontierT6ScopeFromArgs(payload map[string]any, parsed parsedArgs) map[string]any {
	scope := map[string]any{}
	for key, value := range asMap(payload["scope"]) {
		scope[key] = value
	}
	project := parsed.string("project", firstNonEmpty(firstString(scope["project"]), envString("CONTEXTLATTICE_PROJECT", "contextlattice")))
	if project != "" {
		scope["project"] = project
	}
	sessionID := parsed.string("session_id", firstNonEmpty(firstString(scope["session_id"]), envString("CONTEXTLATTICE_SESSION_ID", "")))
	if sessionID != "" {
		scope["session_id"] = sessionID
	}
	agentID := parsed.string("agent_id", firstNonEmpty(firstString(scope["agent_id"]), envString("CONTEXTLATTICE_AGENT_ID", "")))
	if agentID != "" {
		scope["agent_id"] = agentID
	}
	return scope
}

func frontierT6BoundedInt(parsed parsedArgs, name string, fallback, minimum, maximum int) (int, error) {
	raw, ok := parsed.values[name]
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("--%s must be between %d and %d", strings.ReplaceAll(name, "_", "-"), minimum, maximum)
	}
	return value, nil
}

type frontierT6SSEFrame struct {
	id        string
	eventType string
	data      []byte
}

type frontierT6SSEDelivery struct {
	deliveryID string
	eventID    string
	payload    map[string]any
}

func (c *cli) frontierT6SteeringWatch(parsed parsedArgs, payload map[string]any) error {
	limit, err := frontierT6BoundedInt(parsed, "limit", 16, 1, frontierT6MaxWatchEvents)
	if err != nil {
		return err
	}
	if parsed.bool("once") {
		limit = 1
	}
	maxSeconds, err := frontierT6BoundedInt(parsed, "max_seconds", 30, 1, frontierT6MaxWatchSeconds)
	if err != nil {
		return err
	}
	scope := frontierT6ScopeFromArgs(payload, parsed)
	agentID := firstString(scope["agent_id"])
	subscriberID := parsed.string("subscriber_id", firstNonEmpty(firstString(payload["subscriber_id"]), agentID, "contextlattice_agent_fit"))
	cursor := parsed.string("cursor", firstString(payload["cursor"]))
	eventID := parsed.string("event_id", firstString(payload["event_id"]))

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxSeconds)*time.Second)
	defer cancel()
	response, fallbackReason, err := c.frontierT6OpenSteeringSSE(ctx, scope, subscriberID, cursor, eventID, limit)
	if err != nil {
		return err
	}
	if fallbackReason != "" {
		return c.frontierT6SteeringPullFallback(scope, subscriberID, cursor, limit, fallbackReason, parsed)
	}
	defer response.Body.Close()

	emitted, streamErr := c.frontierT6ConsumeSteeringSSE(ctx, response.Body, scope, subscriberID, limit, parsed)
	if streamErr == nil {
		return nil
	}
	if emitted == 0 {
		return c.frontierT6SteeringPullFallback(scope, subscriberID, cursor, limit, "sse_protocol_or_transport_failure", parsed)
	}
	return errors.New("steering SSE stream failed after emitting an event; replay was not attempted to avoid duplicate delivery")
}

func (c *cli) frontierT6OpenSteeringSSE(ctx context.Context, scope map[string]any, subscriberID, cursor, eventID string, limit int) (*http.Response, string, error) {
	query := url.Values{}
	query.Set("project", firstString(scope["project"]))
	if value := firstString(scope["session_id"]); value != "" {
		query.Set("session_id", value)
	}
	if value := firstString(scope["agent_id"]); value != "" {
		query.Set("agent_id", value)
	}
	query.Set("subscriber_id", subscriberID)
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	query.Set("limit", strconv.Itoa(limit))
	target := c.baseURL + frontierT6SteeringEventsPath + "?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", errors.New("build steering SSE request failed")
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	if eventID != "" {
		request.Header.Set("Last-Event-ID", eventID)
	}
	if c.apiKey != "" {
		request.Header.Set("x-api-key", c.apiKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, "sse_transport_unavailable", nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return nil, "sse_negotiation_rejected", nil
	}
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		response.Body.Close()
		return nil, "sse_content_type_unavailable", nil
	}
	return response, "", nil
}

func (c *cli) frontierT6ConsumeSteeringSSE(ctx context.Context, body io.Reader, scope map[string]any, subscriberID string, limit int, parsed parsedArgs) (int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), frontierT6MaxSSELineBytes)
	frame := frontierT6SSEFrame{}
	dataLines := []string{}
	dataBytes := 0
	emitted := 0
	pending := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !pending {
				continue
			}
			if len(dataLines) == 0 {
				frame = frontierT6SSEFrame{}
				pending = false
				continue
			}
			frame.data = []byte(strings.Join(dataLines, "\n"))
			delivery, err := frontierT6DecodeSSEDelivery(frame)
			if err != nil {
				return emitted, err
			}
			output := map[string]any{
				"schema_id":     "contextlattice_agent_fit_steering_delivery.v1",
				"transport":     "sse",
				"event_id":      delivery.eventID,
				"sse_event":     frame.eventType,
				"delivery":      delivery.payload,
				"pull_fallback": false,
			}
			if err := c.frontierT6EmitBounded(output, parsed.bool("pretty") && !parsed.bool("raw")); err != nil {
				return emitted, err
			}
			emitted++
			if err := c.frontierT6AcknowledgeSteering(scope, subscriberID, delivery.deliveryID, delivery.eventID, parsed); err != nil {
				return emitted, err
			}
			if emitted >= limit {
				return emitted, nil
			}
			frame = frontierT6SSEFrame{}
			dataLines = dataLines[:0]
			dataBytes = 0
			pending = false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		pending = true
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "id":
			if strings.ContainsRune(value, '\x00') {
				return emitted, errors.New("invalid SSE event id")
			}
			frame.id = value
		case "event":
			frame.eventType = value
		case "data":
			dataBytes += len(value)
			if len(dataLines) > 0 {
				dataBytes++
			}
			if dataBytes > frontierT6MaxSSELineBytes {
				return emitted, errors.New("steering SSE event exceeds the 64 KiB limit")
			}
			dataLines = append(dataLines, value)
		}
		select {
		case <-ctx.Done():
			return emitted, ctx.Err()
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		return emitted, err
	}
	if pending {
		return emitted, io.ErrUnexpectedEOF
	}
	if emitted == 0 {
		return 0, io.EOF
	}
	return emitted, nil
}

func frontierT6DecodeSSEDelivery(frame frontierT6SSEFrame) (frontierT6SSEDelivery, error) {
	if frame.id == "" || len(frame.data) == 0 || len(frame.data) > frontierT6MaxSSELineBytes {
		return frontierT6SSEDelivery{}, errors.New("incomplete steering SSE event")
	}
	payload := map[string]any{}
	if err := json.Unmarshal(frame.data, &payload); err != nil || payload == nil {
		return frontierT6SSEDelivery{}, errors.New("invalid steering SSE event JSON")
	}
	container := payload
	if nested := asMap(payload["result"]); len(nested) > 0 {
		container = nested
	}
	event := asMap(container["event"])
	deliveryID := firstString(container["delivery_id"])
	eventID := firstNonEmpty(firstString(event["event_id"]), firstString(container["event_id"]), frame.id)
	if deliveryID == "" || len(event) == 0 || eventID == "" || eventID != frame.id {
		return frontierT6SSEDelivery{}, errors.New("steering SSE event lacks matching delivery metadata")
	}
	return frontierT6SSEDelivery{deliveryID: deliveryID, eventID: eventID, payload: payload}, nil
}

func (c *cli) frontierT6AcknowledgeSteering(scope map[string]any, subscriberID, deliveryID, eventID string, parsed parsedArgs) error {
	payload := map[string]any{
		"operation":     "ack",
		"scope":         scope,
		"subscriber_id": subscriberID,
		"delivery_id":   deliveryID,
		"event_id":      eventID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.requestJSON(ctx, http.MethodPost, frontierT6SteeringPath, payload, parsed.float("timeout", 5)); err != nil {
		return errors.New("steering event was emitted but acknowledgement failed")
	}
	return nil
}

func (c *cli) frontierT6SteeringPullFallback(scope map[string]any, subscriberID, cursor string, limit int, reason string, parsed parsedArgs) error {
	payload := map[string]any{
		"operation":     "replay",
		"scope":         scope,
		"subscriber_id": subscriberID,
		"cursor":        cursor,
		"limit":         limit,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, _, err := c.requestJSON(ctx, http.MethodPost, frontierT6SteeringPath, payload, parsed.float("timeout", 5))
	if err != nil {
		return errors.New("steering SSE unavailable and bounded pull fallback failed")
	}
	return c.frontierT6EmitBounded(map[string]any{
		"schema_id":       "contextlattice_agent_fit_steering_fallback.v1",
		"transport":       "bounded_pull_fallback",
		"pull_fallback":   true,
		"fallback_reason": reason,
		"result":          result,
	}, !parsed.bool("raw"))
}

func (c *cli) frontierT6EmitBounded(payload any, pretty bool) error {
	safe := frontierT6SanitizeOutput(payload)
	raw, err := json.Marshal(safe)
	if err != nil {
		return err
	}
	if len(raw) > frontierT6MaxOutputBytes {
		return errors.New("agent-fit response exceeds the 1 MiB output limit")
	}
	return c.emit(safe, pretty)
}

func frontierT6ContainsCredential(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if frontierT6CredentialKey(key) || frontierT6ContainsCredential(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if frontierT6ContainsCredential(nested) {
				return true
			}
		}
	}
	return false
}

func frontierT6SanitizeOutput(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, nested := range typed {
			if frontierT6CredentialKey(key) || strings.EqualFold(strings.TrimSpace(key), "claim_token") {
				clean[key] = "[REDACTED]"
				continue
			}
			clean[key] = frontierT6SanitizeOutput(nested)
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, nested := range typed {
			clean[index] = frontierT6SanitizeOutput(nested)
		}
		return clean
	default:
		return typed
	}
}

func frontierT6CredentialKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(key, "-", "_")))
	switch normalized {
	case "api_key", "apikey", "access_token", "token", "secret", "password", "credential", "private_key", "authorization", "bearer", "runtime_license", "license_key", "entitlement_key", "claim_token":
		return true
	}
	for _, suffix := range []string{"_api_key", "_access_token", "_private_key", "_runtime_license", "_license_key", "_entitlement_key", "_claim_token"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}
