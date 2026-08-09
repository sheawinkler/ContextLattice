package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	recallResponseContractID              = "recall_response.v1"
	recallResponseCLIContractMaxJSONBytes = 64000
)

var recallResponseCLIAllowedFields = map[string]bool{
	"ok": true, "schema_id": true, "version": true, "response_id": true, "response_digest": true,
	"request_scope": true, "classification": true, "answer": true, "state": true, "evidence": true,
	"confidence": true, "conflicts": true, "gaps": true, "inferences": true, "next_action": true,
	"action_boundary": true, "disclosure": true, "receipt_refs": true, "outcome": true,
	"writeback_required": true, "format_contract": true,
}

var recallResponseCLIForbiddenFields = map[string]bool{
	"context_pack": true, "raw_contextlattice_json": true, "raw_retrieval_payload": true, "raw_retrieval": true,
	"raw_prompt": true, "prompt": true, "prompts": true, "messages": true, "tool_calls": true,
	"function_call": true, "secret": true, "secrets": true, "api_key": true, "access_token": true, "refresh_token": true, "bearer_token": true, "password": true,
	"credential": true, "authorization": true, "private_key": true, "file_path": true, "local_path": true,
	"paths": true, "query": true, "project": true, "project_name": true, "topic_path": true,
	"timestamp": true, "exact_timestamp": true, "raw_text": true, "text": true, "excerpt": true,
	"content": true, "packet": true, "agent_packet": true,
}

var recallResponseCLIPrivatePathPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])(?:/(?:Users|home|private|tmp|var|etc|opt|Volumes|System|Library|Applications)(?:/|$)|[a-z]:\\Users\\[^\\\s]+)`)

func recallResponseCLICanonicalFieldKey(key string) string {
	var normalized strings.Builder
	var lastWritten rune
	runes := []rune(strings.TrimSpace(key))
	for index, current := range runes {
		if unicode.IsUpper(current) {
			if normalized.Len() > 0 {
				previous := runes[index-1]
				var next rune
				if index+1 < len(runes) {
					next = runes[index+1]
				}
				if previous != '_' && (unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && unicode.IsLower(next))) {
					normalized.WriteByte('_')
					lastWritten = '_'
				}
			}
			current = unicode.ToLower(current)
		}
		if unicode.IsLetter(current) || unicode.IsDigit(current) {
			normalized.WriteRune(current)
			lastWritten = current
			continue
		}
		if normalized.Len() > 0 && lastWritten != '_' {
			normalized.WriteByte('_')
			lastWritten = '_'
		}
	}
	return strings.Trim(normalized.String(), "_")
}

func recallResponseCLIForbiddenKey(key string) bool {
	canonical := recallResponseCLICanonicalFieldKey(key)
	if canonical == "secrets_included" {
		return false
	}
	if recallResponseCLIForbiddenFields[canonical] {
		return true
	}
	for _, fragment := range []string{"secret", "password", "api_key", "access_token", "refresh_token", "bearer_token", "authorization", "private_key", "credential"} {
		if strings.Contains(canonical, fragment) {
			return true
		}
	}
	return false
}

func recallResponseCLIFieldSet(fields ...string) map[string]bool {
	set := make(map[string]bool, len(fields))
	for _, field := range fields {
		set[field] = true
	}
	return set
}

var recallResponseCLIClosedFields = map[string]map[string]bool{
	"":                               recallResponseCLIAllowedFields,
	"request_scope":                  recallResponseCLIFieldSet("scope_digest", "query_digest", "owner_ref", "workspace_ref", "project_ref", "topic_ref", "agent_ref", "session_ref", "task_ref", "task_identity_ref", "execution_lane_ref", "retrieval_intent", "as_of", "task_class", "temporal_premise_digest", "snapshot_digest", "receipt_digest", "condition", "ablation"),
	"classification":                 recallResponseCLIFieldSet("jobs", "objects", "temporal_mode", "evidence_state", "consequence", "posture", "facets"),
	"classification.facets":          recallResponseCLIFieldSet("jobs", "memory_objects", "temporal_state", "evidence_state", "consequence"),
	"answer":                         recallResponseCLIFieldSet("summary", "answer_mode", "basis", "claim_refs", "components", "progressive_disclosure", "proof_spine", "composition"),
	"answer.progressive_disclosure":  recallResponseCLIFieldSet("level", "available_levels", "next_level_requires"),
	"answer.proof_spine":             recallResponseCLIFieldSet("primary_result", "as_of", "temporal_premise_digest", "proof_refs", "confidence_basis", "conflict_refs", "gap_refs", "memory_boundary", "next_move", "receipt_refs", "disclosure", "coverage"),
	"answer.proof_spine.coverage[]":  recallResponseCLIFieldSet("obligation", "status", "proof_refs"),
	"answer.composition":             recallResponseCLIFieldSet("condition", "ablation", "primary_module", "ordered_modules", "proof_strategy", "coverage_status", "fallback_reason"),
	"answer.components[]":            recallResponseCLIFieldSet("component_ref", "kind", "module_type", "status", "basis", "ordinal", "primary", "proof_refs", "temporal_premise_digest", "payload", "binding", "component_digest"),
	"answer.components[].binding":    recallResponseCLIFieldSet("condition", "ablation", "arm", "exposure_bucket", "policy_version", "proof_digest", "snapshot_digest", "receipt_digest", "owner_ref", "task_ref", "lane_ref", "intent", "temporal_premise_digest", "verifier_digest", "component_digest"),
	"state":                          recallResponseCLIFieldSet("status", "source_complete", "evidence_count", "conflict_count", "gap_count", "retrieval_mode"),
	"confidence":                     recallResponseCLIFieldSet("label", "score", "basis", "calibrated"),
	"next_action":                    recallResponseCLIFieldSet("kind", "label", "reason", "requires_verification", "authority", "execution_performed"),
	"action_boundary":                recallResponseCLIFieldSet("can_act", "requires_confirmation", "allowed", "forbidden", "reason", "execution_performed"),
	"disclosure":                     recallResponseCLIFieldSet("bounded", "raw_retrieval_included", "raw_prompt_included", "paths_included", "secrets_included", "inference_boundary", "omission_policy"),
	"outcome":                        recallResponseCLIFieldSet("status", "attributable", "receipt_id", "execution_performed"),
	"evidence[]":                     recallResponseCLIFieldSet("ref_id", "kind", "role", "status", "confidence", "source_ref", "content_digest"),
	"conflicts[]":                    recallResponseCLIFieldSet("conflict_id", "kind", "status", "support_refs", "opposition_refs", "resolution"),
	"gaps[]":                         recallResponseCLIFieldSet("code", "material", "reason", "required_for_action", "refs"),
	"inferences[]":                   recallResponseCLIFieldSet("inference_id", "claim_ref", "basis_refs", "status", "confidence", "disclosure"),
	"receipt_refs[]":                 recallResponseCLIFieldSet("ref_id", "kind", "status"),
	"format_contract":                recallResponseCLIFieldSet("registry_id", "registry_version", "schema_id", "contract_version", "required_output_mode", "validator", "forbidden_fields", "contract_valid", "truncated", "omitted_counts", "actual_json_bytes", "max_total_json_bytes", "max_string_bytes", "max_list_items", "max_bytes_by_path", "validation", "json_bytes_before_boundary", "json_bytes_after_boundary"),
	"format_contract.omitted_counts": recallResponseCLIFieldSet("strings_clipped", "lists_clipped", "optional_fields_compacted", "boundary_passes", "json_bytes_reduced"),
	"format_contract.validation":     recallResponseCLIFieldSet("status", "errors"),
	"answer.components[].payload.unknown_periods[]":     recallResponseCLIFieldSet("start", "end", "basis_ref", "reason"),
	"answer.components[].payload.bridge_claims[]":       recallResponseCLIFieldSet("proof_refs", "basis"),
	"answer.components[].payload.coverage_receipt":      recallResponseCLIFieldSet("basis_digest", "complete", "reason", "receipt_digest"),
	"answer.components[].payload.parameter_bindings[]":  recallResponseCLIFieldSet("parameter_ref", "value_state", "proof_ref", "required", "sensitive"),
	"answer.components[].payload.ordered_steps[]":       recallResponseCLIFieldSet("ordinal", "step_ref", "proof_ref", "requires_confirmation"),
	"answer.components[].payload.refusal_conditions[]":  recallResponseCLIFieldSet("code", "proof_ref"),
	"answer.components[].payload.recovery_conditions[]": recallResponseCLIFieldSet("code", "proof_ref"),
	"answer.components[].payload.rollback_conditions[]": recallResponseCLIFieldSet("code", "proof_ref"),
}

var recallResponseCLIComponentPayloadFields = map[string][]string{
	"exact_current_status":   {"value", "status", "qualifier_refs"},
	"decision_rationale":     {"decision", "rationale_refs", "constraint_refs", "rejected_alternative_refs"},
	"project_continuation":   {"checkpoint_ref", "completed_refs", "open_refs", "blocker_refs", "next_move"},
	"preference_constraint":  {"statement_ref", "scope_ref", "support_count", "contradiction_refs", "sensitivity"},
	"timeline":               {"event_refs", "ordering", "unknown_intervals", "causal_claim_refs"},
	"procedure":              {"tool_ref", "parameter_bindings", "ordered_steps", "refusal_conditions", "recovery_conditions"},
	"multi_memory_synthesis": {"conclusion", "bridge_claims"},
	"conflict_supersession":  {"claim_refs", "winner_ref", "resolution_status", "resolution_reason_ref", "unknown_periods"},
	"negative_abstention":    {"terminal", "coverage_receipt", "negative_claim_ref"},
	"memory_to_action":       {"intended_tool_ref", "parameter_bindings", "ordered_steps", "refusal_conditions", "rollback_conditions"},
}

func recallResponseCLIUsage(command string) string {
	return command + " '<task>' [--project p] [--task-id id] [--topic-path t] [--mode balanced] [--session-id id] [--agent-id id] [--blocking] [--soft] [--retries n] [--pretty|--raw]"
}

func recallResponseCLIStringFlags() map[string]string {
	return mergeStringFlags(commonStringFlags(), contextPackTokenBudgetStringFlags(), map[string]string{
		"budget-chars":         "budget_chars",
		"limit":                "limit",
		"max-facts":            "max_facts",
		"agent-id":             "agent_id",
		"session-id":           "session_id",
		"task-id":              "task_id",
		"retries":              "retries",
		"retry-delay":          "retry_delay",
		"task-phase":           "task_phase",
		"retrieval-intent":     "retrieval_intent",
		"evidence-obligations": "evidence_obligations",
		"base-packet-file":     "base_packet_file",
	})
}

func recallResponseCLIBoolFlags() map[string]string {
	return mergeBoolFlags(commonBoolFlags(), map[string]string{
		"blocking":        "blocking",
		"nonblocking":     "nonblocking",
		"compact":         "compact",
		"full":            "full",
		"debug":           "debug",
		"soft":            "soft",
		"strict":          "strict",
		"auto-session":    "auto_session",
		"no-auto-session": "no_auto_session",
		"response":        "response",
	})
}

func (c *cli) cmdRecallResponse(args []string) error {
	parsed := parseArgs(args, recallResponseCLIStringFlags(), recallResponseCLIBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage(recallResponseCLIUsage("contextlattice recall-response"))
	}
	if parsed.bool("full") || parsed.bool("debug") || parsed.string("base_packet_file", "") != "" {
		return errors.New("recall-response cannot be combined with Agent Packet or debug output options")
	}
	c.applyBaseURL(parsed)
	query := strings.TrimSpace(strings.Join(parsed.pos, " "))
	if query == "" {
		return errors.New("query is required")
	}
	project := parsed.string("project", "contextlattice")
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	agentID := parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", "")))
	taskID := parsed.string("task_id", envString("CONTEXTLATTICE_TASK_ID", derivedAgentTaskID(project, query)))
	if sessionID == "" && !parsed.bool("no_auto_session") && !autoSessionDisabled() {
		sessionID = c.recallResponseTransport().ensureRecallResponseSession(project, query, taskID, agentID, parsed.float("timeout", 30))
	}
	blocking := parsed.bool("blocking") && !parsed.bool("nonblocking")
	payload := map[string]any{
		"query":                     query,
		"project":                   project,
		"projectName":               project,
		"topic_path":                emptyToNil(parsed.string("topic_path", "")),
		"topicPath":                 emptyToNil(parsed.string("topic_path", "")),
		"retrieval_mode":            parsed.string("mode", "balanced"),
		"include_grounding":         true,
		"include_retrieval_debug":   false,
		"combined_sources":          true,
		"wait_for_slow_sources":     blocking,
		"sync_slow_sources":         blocking,
		"limit":                     parsed.int("limit", 12),
		"max_facts":                 parsed.int("max_facts", 24),
		"agent_id":                  emptyToNil(agentID),
		"session_id":                emptyToNil(sessionID),
		"task_id":                   emptyToNil(taskID),
		"native_cli_implementation": true,
	}
	addContextPackTokenBudgetArgs(payload, parsed)
	if value := parsed.string("task_phase", ""); value != "" {
		payload["task_phase"] = value
	}
	if value := parsed.string("retrieval_intent", ""); value != "" {
		payload["retrieval_intent"] = value
	}
	if value := parsed.string("evidence_obligations", ""); value != "" {
		payload["evidence_obligations"] = splitCSV(value)
	}

	transport := c.recallResponseTransport()
	raw, err := transport.requestRecallResponseWithRetries(
		"/memory/recall/response", payload, parsed.float("timeout", 30), parsed.int("retries", 2), parsed.float("retry_delay", 1),
	)
	if err != nil {
		if emitErr := c.emit(failureRecallResponse(), parsed.bool("pretty") || !parsed.bool("raw")); emitErr != nil {
			return emitErr
		}
		if parsed.bool("soft") {
			return nil
		}
		return err
	}
	out, responseErr := compactRecallResponse(raw)
	if responseErr != nil {
		if emitErr := c.emit(failureRecallResponse(), parsed.bool("pretty") || !parsed.bool("raw")); emitErr != nil {
			return emitErr
		}
		if parsed.bool("soft") {
			return nil
		}
		return responseErr
	}
	if err := c.emit(out, parsed.bool("pretty") || !parsed.bool("raw")); err != nil {
		return err
	}
	transport.autoDrainAsyncInbox(sessionID, project, agentID)
	return nil
}

func (c *cli) recallResponseTransport() *cli {
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
	return &scoped
}

func (c *cli) ensureRecallResponseSession(project, objective, taskID, agentID string, timeout float64) string {
	ownership := adapterOwnership(parsedArgs{})
	ownership["task_id"] = taskID
	ownership["session_surface"] = "recall-response"
	return c.ensureSessionForAgent(project, objective, envString("CONTEXTLATTICE_AGENT", "agent-cli"), agentID, ownership, adapterProfile{}, timeout)
}

func (c *cli) requestRecallResponseWithRetries(path string, payload any, timeout float64, retries int, delay float64) (map[string]any, error) {
	var last map[string]any
	var lastErr error
	for attempt := 0; attempt <= maxInt(retries, 0); attempt++ {
		result, _, err := c.requestRecallResponseJSON(context.Background(), http.MethodPost, path, payload, timeout)
		if len(result) > 0 {
			last = result
		}
		if err == nil {
			return result, nil
		}
		lastErr = err
		// A bounded recall response may be returned with a failure HTTP status.
		// Preserve it as the typed abstention/control response without retrying.
		if firstString(result["schema_id"]) == recallResponseContractID {
			return result, nil
		}
		if attempt < retries {
			time.Sleep(time.Duration(delay*float64(attempt+1)) * time.Second)
		}
	}
	return last, lastErr
}

func (c *cli) requestRecallResponseJSON(ctx context.Context, method, path string, payload any, timeout float64) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxFloat(timeout, 1))*time.Second)
	defer cancel()
	var body io.Reader
	var bodyBytes []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		bodyBytes = raw
		body = bytes.NewReader(bodyBytes)
	}
	target := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		target = c.baseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	headers, err := entitlementHeaders()
	if err != nil {
		return nil, 0, err
	}
	for header, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(header, value)
		}
	}
	if err := addRuntimeLicenseRequestProof(req, bodyBytes); err != nil {
		return nil, 0, err
	}
	baseClient := c.client
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := *baseClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < 400 {
		return nil, resp.StatusCode, errors.New("recall-response transport refused redirect")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, recallResponseCLIContractMaxJSONBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) > recallResponseCLIContractMaxJSONBytes {
		return nil, resp.StatusCode, errors.New("gateway recall response exceeded its bounded output contract")
	}
	parsed := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, resp.StatusCode, errors.New("gateway recall response body was empty")
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, resp.StatusCode, errors.New("gateway recall response body was malformed JSON")
	}
	if resp.StatusCode >= 400 {
		return parsed, resp.StatusCode, fmt.Errorf("%s %s returned status=%d", method, path, resp.StatusCode)
	}
	return parsed, resp.StatusCode, nil
}

func recallResponseCLIHasForbiddenField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if recallResponseCLIForbiddenKey(key) || recallResponseCLIHasForbiddenField(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if recallResponseCLIHasForbiddenField(nested) {
				return true
			}
		}
	case string:
		return recallResponseCLIPrivatePathPattern.MatchString(typed)
	}
	return false
}

func recallResponseCLIClosedFieldsForPath(path, componentKind string) (map[string]bool, bool) {
	if path == "answer.components[].payload" {
		fields, ok := recallResponseCLIComponentPayloadFields[componentKind]
		if !ok {
			return map[string]bool{}, true
		}
		return recallResponseCLIFieldSet(fields...), true
	}
	fields, ok := recallResponseCLIClosedFields[path]
	return fields, ok
}

func recallResponseCLIValidateClosedSchema(value any) error {
	return recallResponseCLIValidateClosedValue(value, "", "")
}

func recallResponseCLIValidateClosedValue(value any, path, componentKind string) error {
	switch typed := value.(type) {
	case map[string]any:
		allowed, closed := recallResponseCLIClosedFieldsForPath(path, componentKind)
		for key := range typed {
			if recallResponseCLIForbiddenKey(key) {
				return fmt.Errorf("gateway recall response contained a forbidden field at %s", recallResponseCLIPath(path, key))
			}
			if closed && !allowed[key] {
				return fmt.Errorf("gateway recall response contained an unexpected nested field %s", recallResponseCLIPath(path, key))
			}
		}
		nextComponentKind := componentKind
		if path == "answer.components[]" {
			nextComponentKind = strings.TrimSpace(firstString(typed["kind"]))
		}
		for key, nested := range typed {
			childPath := recallResponseCLIPath(path, key)
			if path == "answer.components[]" && key == "payload" {
				childPath = "answer.components[].payload"
			}
			if err := recallResponseCLIValidateClosedValue(nested, childPath, nextComponentKind); err != nil {
				return err
			}
		}
	case []any:
		itemPath := path + "[]"
		for _, nested := range typed {
			if err := recallResponseCLIValidateClosedValue(nested, itemPath, componentKind); err != nil {
				return err
			}
		}
	case string:
		if recallResponseCLIPrivatePathPattern.MatchString(typed) {
			return fmt.Errorf("gateway recall response contained a private absolute path at %s", path)
		}
	}
	return nil
}

func recallResponseCLIPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func recallResponseCLIRequireObjectFields(payload map[string]any, path string, fields ...string) error {
	value := any(payload)
	for _, part := range strings.Split(path, ".") {
		row, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("gateway recall response field %s was not an object", path)
		}
		value, ok = row[part]
		if !ok {
			return fmt.Errorf("gateway recall response omitted required field %s", path)
		}
	}
	row, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("gateway recall response field %s was not an object", path)
	}
	for _, field := range fields {
		if _, ok := row[field]; !ok {
			return fmt.Errorf("gateway recall response omitted required field %s.%s", path, field)
		}
	}
	return nil
}

func recallResponseCLIFieldTypeValid(value any, kind string) bool {
	switch kind {
	case "bool":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "int":
		number, ok := value.(float64)
		return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "list":
		_, ok := value.([]any)
		return ok
	default:
		return false
	}
}

func compactRecallResponse(raw map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > recallResponseCLIContractMaxJSONBytes {
		return nil, errors.New("gateway recall response exceeded its bounded output contract")
	}
	if firstString(raw["schema_id"]) != recallResponseContractID {
		return nil, fmt.Errorf("gateway did not return %s", recallResponseContractID)
	}
	if recallResponseCLIHasForbiddenField(raw) {
		return nil, errors.New("gateway recall response contained a forbidden raw field")
	}
	if err := recallResponseCLIValidateClosedSchema(raw); err != nil {
		return nil, err
	}
	for key := range raw {
		if !recallResponseCLIAllowedFields[key] {
			return nil, errors.New("gateway recall response contained an unexpected field")
		}
	}
	projected := map[string]any{}
	fieldTypes := map[string]string{
		"ok": "bool", "schema_id": "string", "version": "int", "response_id": "string", "response_digest": "string",
		"request_scope": "object", "classification": "object", "answer": "object", "state": "object", "evidence": "list",
		"confidence": "object", "conflicts": "list", "gaps": "list", "inferences": "list", "next_action": "object",
		"action_boundary": "object", "disclosure": "object", "receipt_refs": "list", "outcome": "object",
		"writeback_required": "bool", "format_contract": "object",
	}
	for key := range recallResponseCLIAllowedFields {
		value, exists := raw[key]
		if !exists {
			return nil, errors.New("gateway recall response omitted a required field")
		}
		if !recallResponseCLIFieldTypeValid(value, fieldTypes[key]) {
			return nil, fmt.Errorf("gateway recall response field %s had an unexpected type", key)
		}
		projected[key] = value
	}
	for path, fields := range map[string][]string{
		"request_scope":                 {"scope_digest", "query_digest", "owner_ref", "workspace_ref", "project_ref", "topic_ref", "agent_ref", "session_ref", "task_ref", "task_identity_ref", "execution_lane_ref", "retrieval_intent", "as_of", "task_class", "temporal_premise_digest", "snapshot_digest", "receipt_digest", "condition", "ablation"},
		"classification":                {"jobs", "objects", "temporal_mode", "evidence_state", "consequence", "posture", "facets"},
		"classification.facets":         {"jobs", "memory_objects", "temporal_state", "evidence_state", "consequence"},
		"answer":                        {"summary", "answer_mode", "basis", "claim_refs", "components", "progressive_disclosure", "proof_spine", "composition"},
		"answer.proof_spine":            {"primary_result", "as_of", "temporal_premise_digest", "proof_refs", "confidence_basis", "conflict_refs", "gap_refs", "memory_boundary", "next_move", "receipt_refs", "disclosure", "coverage"},
		"answer.composition":            {"condition", "ablation", "primary_module", "ordered_modules", "proof_strategy", "coverage_status", "fallback_reason"},
		"answer.progressive_disclosure": {"level", "available_levels", "next_level_requires"},
		"state":                         {"status", "source_complete", "evidence_count", "conflict_count", "gap_count", "retrieval_mode"},
		"confidence":                    {"label", "score", "basis", "calibrated"},
		"next_action":                   {"kind", "label", "reason", "requires_verification", "authority", "execution_performed"},
		"action_boundary":               {"can_act", "requires_confirmation", "allowed", "forbidden", "reason", "execution_performed"},
		"disclosure":                    {"bounded", "raw_retrieval_included", "raw_prompt_included", "paths_included", "secrets_included", "inference_boundary", "omission_policy"},
		"outcome":                       {"status", "attributable", "receipt_id", "execution_performed"},
		"format_contract":               {"registry_id", "registry_version", "schema_id", "contract_version", "required_output_mode", "validator", "contract_valid", "truncated", "omitted_counts", "actual_json_bytes", "max_total_json_bytes", "max_string_bytes", "max_list_items", "validation"},
	} {
		if err := recallResponseCLIRequireObjectFields(projected, path, fields...); err != nil {
			return nil, err
		}
	}
	recomputedID := cliRecallResponseID(projected)
	recomputedDigest := cliRecallResponseDigest(projected)
	if !asBool(projected["ok"]) || asInt(projected["version"]) != 1 ||
		!recallResponseCLIExactID(firstString(projected["response_id"]), "rr_") ||
		!recallResponseCLIValidDigest(firstString(projected["response_digest"])) ||
		firstString(projected["response_id"]) != recomputedID ||
		firstString(projected["response_digest"]) != recomputedDigest {
		return nil, errors.New("gateway recall response identity was malformed")
	}
	format := asMap(projected["format_contract"])
	validation := asMap(format["validation"])
	if firstString(format["registry_id"]) != generatedAgentContractRegistryID ||
		asInt(format["registry_version"]) != generatedAgentContractRegistryVersion ||
		firstString(format["schema_id"]) != recallResponseContractID ||
		!asBool(format["contract_valid"]) || firstString(validation["status"]) != "passed" ||
		asInt(format["max_total_json_bytes"]) != recallResponseCLIContractMaxJSONBytes {
		return nil, errors.New("gateway recall response contract was invalid")
	}
	projectedEncoded, err := json.Marshal(projected)
	if err != nil || len(projectedEncoded) > recallResponseCLIContractMaxJSONBytes {
		return nil, errors.New("projected recall response exceeded its bounded output contract")
	}
	return projected, nil
}

func recallResponseCLIExactID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+24 {
		return false
	}
	for _, ch := range value[len(prefix):] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func recallResponseCLIValidDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, ch := range value[len("sha256:"):] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func cliRecallResponseDigest(payload map[string]any) string {
	semantic := map[string]any{}
	for key, value := range payload {
		if key != "format_contract" && key != "response_digest" {
			semantic[key] = value
		}
	}
	raw, _ := json.Marshal(semantic)
	return "sha256:" + fmt.Sprintf("%x", sha256Sum(raw))
}

func cliRecallResponseID(payload map[string]any) string {
	semantic := map[string]any{}
	for key, value := range payload {
		if key != "response_id" && key != "response_digest" && key != "format_contract" {
			semantic[key] = value
		}
	}
	raw, _ := json.Marshal(semantic)
	return "rr_" + fmt.Sprintf("%x", sha256Sum(raw))[:24]
}

func failureRecallResponse() map[string]any {
	scopeRef := func(kind string) string {
		return "ref_" + fmt.Sprintf("%x", sha256Sum([]byte("recall-response-cli-unavailable\x00"+kind)))[:24]
	}
	asOf := "latest_available"
	temporalPremiseDigest := "sha256:" + fmt.Sprintf("%x", sha256Sum([]byte("recall-response-cli-unavailable\x00temporal-premise")))
	snapshotDigest := "sha256:" + fmt.Sprintf("%x", sha256Sum([]byte("recall-response-cli-unavailable\x00snapshot")))
	receiptDigest := "sha256:" + fmt.Sprintf("%x", sha256Sum([]byte("recall-response-cli-unavailable\x00receipt")))
	gapRef := scopeRef("gap")
	response := map[string]any{
		"ok": true, "schema_id": recallResponseContractID, "version": 1,
		"request_scope": map[string]any{
			"scope_digest": scopeRef("scope"), "query_digest": scopeRef("query"), "workspace_ref": scopeRef("workspace"),
			"owner_ref": scopeRef("owner"), "project_ref": scopeRef("project"), "topic_ref": scopeRef("topic"), "agent_ref": scopeRef("agent"),
			"session_ref": scopeRef("session"), "task_ref": scopeRef("task"), "task_identity_ref": scopeRef("task_identity"),
			"execution_lane_ref": scopeRef("execution_lane"), "retrieval_intent": "decision", "as_of": asOf, "task_class": "general",
			"temporal_premise_digest": temporalPremiseDigest, "snapshot_digest": snapshotDigest, "receipt_digest": receiptDigest,
			"condition": "compositional_router", "ablation": "none",
		},
		"classification": map[string]any{
			"jobs": []any{"verify"}, "objects": []any{"durable_memory"}, "temporal_mode": "current_or_unknown", "evidence_state": "degraded", "consequence": "high_stakes", "posture": "abstain",
			"facets": map[string]any{"jobs": []any{"verify"}, "memory_objects": []any{"durable_memory"}, "temporal_state": "current_or_unknown", "evidence_state": "degraded", "consequence": "high_stakes"},
		},
		"answer": map[string]any{
			"summary": "Recall response unavailable; retrieve or verify before acting.", "answer_mode": "abstention",
			"basis": []any{"bounded_response_projection", "explicit_action_boundary"}, "claim_refs": []any{}, "components": []any{},
			"progressive_disclosure": map[string]any{"level": "summary", "available_levels": []any{"summary", "proof_refs", "next_action"}, "next_level_requires": "recover recall response availability"},
			"proof_spine": map[string]any{
				"primary_result": "", "as_of": asOf, "temporal_premise_digest": temporalPremiseDigest,
				"proof_refs": []any{}, "confidence_basis": []any{"unavailable_surface"}, "conflict_refs": []any{}, "gap_refs": []any{gapRef},
				"memory_boundary": "server_evidence_and_deterministic_inference_only", "next_move": "retrieve_or_verify", "receipt_refs": []any{},
				"disclosure": "bounded_opaque_references_only",
				"coverage":   []any{map[string]any{"obligation": "bounded_recall_availability", "status": "unsatisfied", "proof_refs": []any{}}},
			},
			"composition": map[string]any{
				"condition": "compositional_router", "ablation": "none", "primary_module": "v1_control", "ordered_modules": []any{},
				"proof_strategy": "shared_bounded_v1", "coverage_status": "unsatisfied", "fallback_reason": "unavailable_surface",
			},
		},
		"state":    map[string]any{"status": "abstain", "source_complete": false, "evidence_count": 0, "conflict_count": 0, "gap_count": 1, "retrieval_mode": "balanced"},
		"evidence": []any{}, "confidence": map[string]any{"label": "abstain", "score": 0.0, "basis": []any{"unavailable_surface"}, "calibrated": false},
		"conflicts": []any{}, "gaps": []any{map[string]any{"code": "unavailable_surface", "material": true, "reason": "Recall response retrieval was unavailable.", "required_for_action": true}},
		"inferences":      []any{map[string]any{"inference_id": "inf_" + fmt.Sprintf("%x", sha256Sum([]byte("unavailable")))[:24], "claim_ref": "response_state", "basis_refs": []any{}, "status": "deterministic_metadata_only", "confidence": 0.0, "disclosure": "This is response-state metadata, not a memory fact."}},
		"next_action":     map[string]any{"kind": "retrieve_or_verify", "label": "Recover recall response availability, then retrieve or verify", "reason": "The response is advisory-only and does not authorize external mutation.", "requires_verification": true, "authority": "advisory_only", "execution_performed": false},
		"action_boundary": map[string]any{"can_act": false, "requires_confirmation": true, "allowed": []any{"retrieve_missing_sources"}, "forbidden": []any{"external_mutation", "credential_access", "raw_memory_export"}, "reason": "Recall responses provide evidence and advice only; an agent must independently authorize and execute any mutation.", "execution_performed": false},
		"disclosure":      map[string]any{"bounded": true, "raw_retrieval_included": false, "raw_prompt_included": false, "paths_included": false, "secrets_included": false, "inference_boundary": "Only deterministic response metadata and opaque proof references are returned.", "omission_policy": "Unavailable evidence is disclosed as a gap and never becomes implicit support."},
		"receipt_refs":    []any{}, "outcome": map[string]any{"status": "not_attributable", "attributable": false, "receipt_id": "", "execution_performed": false}, "writeback_required": true,
	}
	response["response_id"] = cliRecallResponseID(response)
	response["response_digest"] = cliRecallResponseDigest(response)
	response["format_contract"] = map[string]any{
		"registry_id": generatedAgentContractRegistryID, "registry_version": generatedAgentContractRegistryVersion, "schema_id": recallResponseContractID, "contract_version": 1,
		"required_output_mode": "json_object", "validator": "contextlattice.boundary.v1", "contract_valid": true, "truncated": false,
		"omitted_counts":    map[string]any{"strings_clipped": 0, "lists_clipped": 0, "optional_fields_compacted": 0, "boundary_passes": 0, "json_bytes_reduced": 0},
		"actual_json_bytes": 0, "max_total_json_bytes": recallResponseCLIContractMaxJSONBytes, "max_string_bytes": 2400, "max_list_items": 32,
		"validation": map[string]any{"status": "passed", "errors": []any{}},
	}
	encoded, _ := json.Marshal(response)
	for previous := -1; previous != len(encoded); {
		previous = len(encoded)
		response["format_contract"].(map[string]any)["actual_json_bytes"] = previous
		encoded, _ = json.Marshal(response)
	}
	return response
}
