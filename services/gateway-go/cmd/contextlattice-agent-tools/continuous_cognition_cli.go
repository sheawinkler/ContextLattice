package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	continuousCognitionCLIPath       = "/memory/continuous-cognition"
	continuousCognitionCLIContractID = "continuous_cognition.v1"
	continuousCognitionCLIMaxQuery   = 2400
)

var continuousCognitionCLIOperations = map[string]bool{
	"observe": true, "investigate": true, "status": true, "outcome": true,
	"evaluate": true, "rollback": true, "retire": true,
}

var continuousCognitionCLIAllowedFields = map[string]bool{
	"ok": true, "schema_id": true, "version": true, "cognition_id": true,
	"cognition_digest": true, "operation": true, "phase": true, "decision": true,
	"request_scope": true, "observation": true, "frontier": true, "investigation": true,
	"activation": true, "outcome": true, "evaluation": true, "rollback": true,
	"retirement": true, "progress": true, "safety": true, "gaps": true,
	"writeback_required": true, "format_contract": true,
}

var continuousCognitionCLIForbiddenFields = map[string]bool{
	"hookSpecificOutput": true, "raw_contextlattice_json": true, "raw_retrieval_payload": true,
	"raw_retrieval": true, "raw_prompt": true, "prompt": true, "prompts": true,
	"messages": true, "tool_calls": true, "function_call": true, "request_body": true,
	"response_body": true, "content": true, "input": true, "output": true, "query": true,
	"project": true, "project_name": true, "topic_path": true, "timestamp": true,
	"exact_timestamp": true, "generated_at": true, "file_path": true, "local_path": true,
	"paths": true, "cwd": true, "worktree": true, "secret": true, "secrets": true,
	"api_key": true, "password": true, "credential": true, "authorization": true,
	"private_key": true, "access_token": true, "refresh_token": true, "bearer": true,
	"token": true,
}

func continuousCognitionCLIUsage() string {
	return "contextlattice continuous-cognition {observe|investigate|status|outcome|evaluate|rollback|retire} '<task>' [--project p] [--session-id id] [--agent-id id] [--task-id id] [--objective-id id] [--as-of RFC3339] [--raw|--pretty]"
}

func (c *cli) cmdContinuousCognition(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"agent-id":          "agent_id",
		"session-id":        "session_id",
		"task-id":           "task_id",
		"task-identity-id":  "task_identity_id",
		"execution-lane-id": "execution_lane_id",
		"objective-id":      "objective_id",
		"cycle-ref":         "cycle_ref",
		"retrieval-intent":  "retrieval_intent",
		"retrieval-mode":    "retrieval_mode",
		"limit":             "limit",
		"token-budget":      "token_budget",
		"as-of":             "as_of",
	}), commonBoolFlags())
	if parsed.bool("help") || len(parsed.pos) == 0 {
		return c.emitUsage(continuousCognitionCLIUsage())
	}

	operation := strings.ToLower(strings.TrimSpace(parsed.pos[0]))
	if !continuousCognitionCLIOperations[operation] {
		return fmt.Errorf("unknown continuous-cognition operation %q", operation)
	}
	query := strings.TrimSpace(strings.Join(parsed.pos[1:], " "))
	if query == "" {
		return errors.New("continuous-cognition task is required")
	}
	if len([]byte(query)) > continuousCognitionCLIMaxQuery || strings.ContainsAny(query, "\x00\r\n") {
		return errors.New("continuous-cognition task exceeds its bounded input contract")
	}

	limit, err := boundedCLIInt(parsed, "limit", 32, 1, 500)
	if err != nil {
		return err
	}
	tokenBudget, err := boundedCLIInt(parsed, "token_budget", 4000, 512, 64000)
	if err != nil {
		return err
	}
	asOf, err := continuousCognitionCLIAsOf(parsed.string("as_of", ""))
	if err != nil {
		return err
	}

	c.applyBaseURL(parsed)
	project := parsed.string("project", envString("CONTEXTLATTICE_PROJECT", "contextlattice"))
	agentID := parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", "")))
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	taskID := parsed.string("task_id", envString("CONTEXTLATTICE_TASK_ID", derivedAgentTaskID(project, query)))
	taskIdentityID := parsed.string("task_identity_id", envString("CONTEXTLATTICE_TASK_IDENTITY_ID", ""))
	executionLaneID := parsed.string("execution_lane_id", envString("CONTEXTLATTICE_EXECUTION_LANE_ID", ""))
	objectiveID := parsed.string("objective_id", envString("CONTEXTLATTICE_OBJECTIVE_ID", ""))
	topicPath := parsed.string("topic_path", "")
	payload := map[string]any{
		"operation": operation, "query": query, "project": project,
		"retrieval_intent": parsed.string("retrieval_intent", "decision"),
		"retrieval_mode":   parsed.string("retrieval_mode", parsed.string("mode", "balanced")),
		"limit":            limit, "token_budget": tokenBudget,
		"as_of": asOf.Format(time.RFC3339Nano),
	}
	for field, value := range map[string]string{
		"topic_path": topicPath, "agent_id": agentID, "session_id": sessionID,
		"task_id": taskID, "task_identity_id": taskIdentityID,
		"execution_lane_id": executionLaneID, "objective_id": objectiveID,
		"cycle_ref": parsed.string("cycle_ref", ""),
	} {
		if strings.TrimSpace(value) != "" {
			payload[field] = value
		}
	}

	result, status, err := c.requestContinuousCognitionJSON(context.Background(), payload, parsed.float("timeout", 30))
	if err != nil {
		if status > 0 {
			return fmt.Errorf("continuous-cognition request failed with status %d", status)
		}
		return errors.New("continuous-cognition request failed")
	}
	result, err = compactContinuousCognitionResponse(result, operation, []string{
		query, project, topicPath, agentID, sessionID, taskID, taskIdentityID, executionLaneID, objectiveID,
	})
	if err != nil {
		return err
	}
	return c.emit(result, parsed.bool("pretty") || !parsed.bool("raw"))
}

func (c *cli) requestContinuousCognitionJSON(ctx context.Context, payload map[string]any, timeout float64) (map[string]any, int, error) {
	baseClient := c.client
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := *baseClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	scoped := *c
	scoped.client = &client
	return scoped.requestJSON(ctx, http.MethodPost, continuousCognitionCLIPath, payload, timeout)
}

func continuousCognitionCLIAsOf(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Now().UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, errors.New("--as-of must be RFC3339")
	}
	return parsed.UTC(), nil
}

func continuousCognitionCLIHasForbiddenField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if continuousCognitionCLIForbiddenFields[key] || continuousCognitionCLIHasForbiddenField(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if continuousCognitionCLIHasForbiddenField(nested) {
				return true
			}
		}
	}
	return false
}

func continuousCognitionCLIContainsRawValue(value any, rawValues []string) bool {
	switch typed := value.(type) {
	case string:
		for index, raw := range rawValues {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if typed == raw || (index == 0 && len([]byte(raw)) >= 12 && strings.Contains(typed, raw)) {
				return true
			}
		}
	case map[string]any:
		for _, nested := range typed {
			if continuousCognitionCLIContainsRawValue(nested, rawValues) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if continuousCognitionCLIContainsRawValue(nested, rawValues) {
				return true
			}
		}
	}
	return false
}

func continuousCognitionCLIDigest(payload map[string]any) string {
	semantic := map[string]any{}
	for key, value := range payload {
		if key != "format_contract" && key != "cognition_digest" {
			semantic[key] = value
		}
	}
	raw, _ := json.Marshal(semantic)
	return "sha256:" + fmt.Sprintf("%x", sha256Sum(raw))
}

func compactContinuousCognitionResponse(raw map[string]any, operation string, rawValues []string) (map[string]any, error) {
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > 96000 {
		return nil, errors.New("gateway continuous cognition response exceeded its bounded output contract")
	}
	if firstString(raw["schema_id"]) != continuousCognitionCLIContractID || firstString(raw["operation"]) != operation {
		return nil, errors.New("gateway did not return the requested continuous_cognition.v1 operation")
	}
	if continuousCognitionCLIHasForbiddenField(raw) || continuousCognitionCLIContainsRawValue(raw, rawValues) {
		return nil, errors.New("gateway continuous cognition response violated the bounded disclosure contract")
	}
	for key := range raw {
		if !continuousCognitionCLIAllowedFields[key] {
			return nil, errors.New("gateway continuous cognition response contained an unexpected field")
		}
	}
	projected := map[string]any{}
	for key := range continuousCognitionCLIAllowedFields {
		value, exists := raw[key]
		if !exists {
			return nil, errors.New("gateway continuous cognition response omitted a required field")
		}
		projected[key] = value
	}
	if !asBool(projected["ok"]) || asInt(projected["version"]) != 1 ||
		!recallResponseCLIExactID(firstString(projected["cognition_id"]), "cc_") ||
		!recallResponseCLIValidDigest(firstString(projected["cognition_digest"])) ||
		firstString(projected["cognition_digest"]) != continuousCognitionCLIDigest(projected) {
		return nil, errors.New("gateway continuous cognition response identity was malformed")
	}
	format := asMap(projected["format_contract"])
	validation := asMap(format["validation"])
	if firstString(format["registry_id"]) != generatedAgentContractRegistryID ||
		asInt(format["registry_version"]) != generatedAgentContractRegistryVersion ||
		firstString(format["schema_id"]) != continuousCognitionCLIContractID ||
		!asBool(format["contract_valid"]) || firstString(validation["status"]) != "passed" {
		return nil, errors.New("gateway continuous cognition response contract was invalid")
	}
	safety := asMap(projected["safety"])
	activation := asMap(projected["activation"])
	investigation := asMap(projected["investigation"])
	if !asBool(safety["advisory_only"]) || !asBool(safety["requires_explicit_authorization"]) ||
		!asBool(safety["requires_external_worker"]) || asBool(safety["automatic_model_execution"]) ||
		asBool(safety["automatic_external_mutation"]) || asBool(safety["runner_dispatch"]) ||
		asBool(safety["filesystem_mutation"]) || asBool(safety["gateway_execution_performed"]) ||
		!asBool(activation["one_shot"]) || asBool(activation["gateway_execution_performed"]) ||
		!asBool(investigation["mutations_suppressed"]) || !asBool(projected["writeback_required"]) {
		return nil, errors.New("gateway continuous cognition safety posture was invalid")
	}
	return projected, nil
}
