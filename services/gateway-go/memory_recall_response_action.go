package main

import (
	"strings"
	"time"
)

const (
	recallResponseMaxActionParameters = 12
	recallResponseMaxActionSteps      = 12
	recallResponseMaxActionConditions = 8
)

var recallResponseActionConditionCodes = map[string]bool{
	"independent_authorization_required": true,
	"credential_access":                  true,
	"external_mutation":                  true,
	"missing_required_parameter":         true,
	"sensitive_value_unavailable":        true,
	"proof_mismatch":                     true,
	"rollback_required":                  true,
	"verify_postcondition":               true,
	"restore_previous_state":             true,
	"request_new_authorization":          true,
}

var recallResponseActionValueStates = map[string]bool{
	"bound_redacted": true, "unresolved": true, "sensitive_unresolved": true, "unsafe": true,
}

// recallResponseProjectEvidenceMetadata is the only context-pack bridge added
// by U4. It projects status/action structure into codes and opaque references;
// raw instructions, parameter values, paths, and credentials are never copied.
func recallResponseProjectEvidenceMetadata(source map[string]any) map[string]any {
	out := map[string]any{}
	if temporal := recallResponseProjectTemporalMetadata(source); len(temporal) > 0 {
		out["temporal"] = temporal
	}
	if action := recallResponseProjectActionMetadata(source); len(action) > 0 {
		out["action"] = action
	}
	if terminal := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(source["negative_terminal"]), anyToString(source["event_status"])))); terminal == "did_not_happen" {
		out["negative_terminal"] = terminal
	}
	return out
}

func recallResponseProjectTemporalMetadata(source map[string]any) map[string]any {
	raw := anyMap(source["temporal_evidence"])
	status := recallResponseSafeStatus(firstNonEmptyStrings(anyToString(raw["status"]), anyToString(source["status"]), anyToString(source["proof_status"])))
	supersedes := contextPackAnyList(raw["supersedes"])
	if len(supersedes) == 0 {
		supersedes = contextPackAnyList(source["supersedes"])
	}
	transitions := recallResponseProjectedTransitions(firstPresentAny(raw["transitions"], source["status_transitions"]))
	validFrom := recallResponseProjectedTime(firstNonEmptyStrings(anyToString(raw["valid_from"]), anyToString(source["valid_from"])))
	validTo := recallResponseProjectedTime(firstNonEmptyStrings(anyToString(raw["valid_to"]), anyToString(source["valid_to"])))
	if status == "unknown" && len(supersedes) == 0 && len(transitions) == 0 && validFrom == "" && validTo == "" {
		return nil
	}
	out := map[string]any{
		"valid_from": validFrom, "valid_to": validTo,
		"revision":                    clampInt(anyToInt(firstPresentAny(raw["revision"], source["revision"]), 0), 0, 1_000_000_000),
		"supersedes_count":            minInt(len(supersedes), recallResponseMaxProofRefs),
		"transition_history_complete": anyToBool(firstPresentAny(raw["transition_history_complete"], source["transition_history_complete"])),
		"transitions":                 transitions,
	}
	if status != "unknown" {
		out["status"] = status
	}
	return out
}

func recallResponseProjectedTransitions(value any) []any {
	out := []any{}
	for _, raw := range contextPackAnyList(value) {
		if len(out) >= recallResponseMaxProofRefs {
			break
		}
		row := anyMap(raw)
		status := recallResponseSafeStatus(anyToString(row["status"]))
		at := recallResponseProjectedTime(firstNonEmptyStrings(anyToString(row["effective_at"]), anyToString(row["observed_at"]), anyToString(row["at"])))
		if status == "unknown" || at == "" {
			continue
		}
		out = append(out, map[string]any{"status": status, "effective_at": at})
	}
	return out
}

func recallResponseProjectedTime(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}

func recallResponseProjectActionMetadata(source map[string]any) map[string]any {
	raw := anyMap(source["action_evidence"])
	if len(raw) == 0 {
		raw = anyMap(source["structured_action"])
	}
	if len(raw) == 0 {
		return nil
	}
	namespace := firstNonEmptyStrings(anyToString(source["candidate_id"]), anyToString(source["memory_id"]), anyToString(source["id"]), "bounded_action")
	toolIdentity := strings.TrimSpace(firstNonEmptyStrings(anyToString(raw["tool_ref"]), anyToString(raw["tool"])))
	toolRef := "unresolved_tool"
	if toolIdentity != "" {
		if recallResponseValidDigest(toolIdentity) {
			toolRef = toolIdentity
		} else {
			// Do not turn low-entropy tool names into a dictionary oracle.
			toolRef = "sha256:" + sha256Hex(namespace+"\x00tool")
		}
	}
	parameters := []any{}
	derivedRefusals := append([]any(nil), contextPackAnyList(raw["refusal_conditions"])...)
	for index, item := range contextPackAnyList(raw["parameter_bindings"]) {
		if len(parameters) >= recallResponseMaxActionParameters {
			break
		}
		row := anyMap(item)
		parameterRef := anyToString(row["parameter_ref"])
		if !recallResponseValidDigest(parameterRef) {
			parameterRef = "sha256:" + sha256Hex(namespace+"\x00parameter\x00"+anyToString(index))
		}
		valueState := strings.ToLower(strings.TrimSpace(anyToString(row["value_state"])))
		switch {
		case anyToBool(row["sensitive"]) && valueState != "unsafe":
			valueState = "sensitive_unresolved"
		case valueState == "resolved" || valueState == "bound":
			valueState = "bound_redacted"
		case !recallResponseActionValueStates[valueState]:
			valueState = "unresolved"
		}
		parameters = append(parameters, map[string]any{
			"parameter_ref": parameterRef,
			"value_state":   valueState, "required": anyToBool(row["required"]), "sensitive": anyToBool(row["sensitive"]),
		})
		switch valueState {
		case "unresolved":
			derivedRefusals = append(derivedRefusals, "missing_required_parameter")
		case "sensitive_unresolved":
			derivedRefusals = append(derivedRefusals, "sensitive_value_unavailable")
		case "unsafe":
			derivedRefusals = append(derivedRefusals, "credential_access")
		}
	}
	steps := []any{}
	for index, item := range contextPackAnyList(raw["ordered_steps"]) {
		if len(steps) >= recallResponseMaxActionSteps {
			break
		}
		row := anyMap(item)
		stepRef := anyToString(row["step_ref"])
		if !recallResponseValidDigest(stepRef) {
			stepRef = "sha256:" + sha256Hex(namespace+"\x00step\x00"+anyToString(index))
		}
		steps = append(steps, map[string]any{
			"ordinal": len(steps) + 1, "step_ref": stepRef,
			"requires_confirmation": true,
		})
	}
	return map[string]any{
		"tool_ref": toolRef, "parameter_bindings": parameters, "ordered_steps": steps,
		"refusal_conditions":  recallResponseProjectedConditionCodes(derivedRefusals, true),
		"recovery_conditions": recallResponseProjectedConditionCodes(raw["recovery_conditions"], false),
		"rollback_conditions": recallResponseProjectedConditionCodes(raw["rollback_conditions"], false),
	}
}

func recallResponseHasStructuredActionEvidence(items []any) bool {
	for _, raw := range items {
		row := anyMap(raw)
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if !eligible || !confidenceValid {
			continue
		}
		action := anyMap(anyMap(row["recall_metadata"])["action"])
		if len(action) == 0 {
			action = recallResponseProjectActionMetadata(row)
		}
		if len(action) > 0 {
			return true
		}
	}
	return false
}

func recallResponseProjectedConditionCodes(value any, requireAuthorization bool) []any {
	values := []string{}
	if requireAuthorization {
		values = append(values, "independent_authorization_required")
	}
	for _, raw := range contextPackAnyList(value) {
		code := strings.ToLower(strings.TrimSpace(anyToString(raw)))
		code = strings.ReplaceAll(code, "-", "_")
		code = strings.ReplaceAll(code, " ", "_")
		if recallResponseActionConditionCodes[code] && !containsString(values, code) && len(values) < recallResponseMaxActionConditions {
			values = append(values, code)
		}
	}
	return recallResponseAnyStrings(values)
}

func recallResponseActionPayload(kind string, response map[string]any, refs []string, source map[string]any) map[string]any {
	action := map[string]any{}
	proofRef := ""
	for _, raw := range recallResponseTemporalRows(source) {
		row := anyMap(raw)
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if !eligible || !confidenceValid {
			continue
		}
		rowRef := recallResponseProjectedRowRef(row, response)
		if rowRef == "" || !containsString(refs, rowRef) {
			continue
		}
		metadata := anyMap(anyMap(row["recall_metadata"])["action"])
		if len(metadata) == 0 {
			metadata = recallResponseProjectActionMetadata(row)
		}
		if len(metadata) > 0 {
			action = metadata
			proofRef = rowRef
			break
		}
	}
	parameters := recallResponseBoundActionRows(action["parameter_bindings"], proofRef, "parameter")
	steps := recallResponseBoundActionRows(action["ordered_steps"], proofRef, "step")
	refusal := recallResponseActionConditions(action["refusal_conditions"], proofRef, true)
	recovery := recallResponseActionConditions(action["recovery_conditions"], proofRef, false)
	rollback := recallResponseActionConditions(action["rollback_conditions"], proofRef, false)
	toolRef := firstNonEmptyStrings(anyToString(action["tool_ref"]), "unresolved_tool")
	if kind == "procedure" {
		return map[string]any{
			"tool_ref": toolRef, "parameter_bindings": parameters, "ordered_steps": steps,
			"refusal_conditions": refusal, "recovery_conditions": recovery,
		}
	}
	return map[string]any{
		"intended_tool_ref": toolRef, "parameter_bindings": parameters, "ordered_steps": steps,
		"refusal_conditions": refusal, "rollback_conditions": rollback,
	}
}

func recallResponseBoundActionRows(value any, proofRef, kind string) []any {
	out := []any{}
	limit := recallResponseMaxActionParameters
	if kind == "step" {
		limit = recallResponseMaxActionSteps
	}
	for _, raw := range contextPackAnyList(value) {
		if len(out) >= limit || proofRef == "" {
			break
		}
		row := anyMap(raw)
		if kind == "parameter" {
			ref := anyToString(row["parameter_ref"])
			valueState := anyToString(row["value_state"])
			if !recallResponseValidDigest(ref) || !recallResponseActionValueStates[valueState] {
				continue
			}
			out = append(out, map[string]any{
				"parameter_ref": ref, "value_state": valueState, "proof_ref": proofRef,
				"required": anyToBool(row["required"]), "sensitive": anyToBool(row["sensitive"]),
			})
			continue
		}
		ref := anyToString(row["step_ref"])
		if !recallResponseValidDigest(ref) {
			continue
		}
		out = append(out, map[string]any{
			"ordinal": len(out) + 1, "step_ref": ref, "proof_ref": proofRef, "requires_confirmation": true,
		})
	}
	return out
}

func recallResponseActionConditions(value any, proofRef string, requireAuthorization bool) []any {
	codes := recallResponseProjectedConditionCodes(value, requireAuthorization)
	out := []any{}
	for _, raw := range codes {
		if len(out) >= recallResponseMaxActionConditions {
			break
		}
		out = append(out, map[string]any{"code": anyToString(raw), "proof_ref": proofRef})
	}
	return out
}

func recallResponseActionPayloadValid(payload map[string]any, proofSet map[string]bool, memoryToAction bool) bool {
	toolKey := "tool_ref"
	conditionKey := "recovery_conditions"
	if memoryToAction {
		toolKey = "intended_tool_ref"
		conditionKey = "rollback_conditions"
	}
	toolRef := anyToString(payload[toolKey])
	if toolRef != "unresolved_tool" && !recallResponseValidDigest(toolRef) {
		return false
	}
	parameters, ok := payload["parameter_bindings"].([]any)
	if !ok || len(parameters) > recallResponseMaxActionParameters {
		return false
	}
	for _, raw := range parameters {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"parameter_ref", "value_state", "proof_ref", "required", "sensitive"}) ||
			!recallResponseValidDigest(anyToString(row["parameter_ref"])) || !recallResponseActionValueStates[anyToString(row["value_state"])] ||
			!proofSet[anyToString(row["proof_ref"])] {
			return false
		}
		if _, ok := row["required"].(bool); !ok {
			return false
		}
		if _, ok := row["sensitive"].(bool); !ok {
			return false
		}
	}
	steps, ok := payload["ordered_steps"].([]any)
	if !ok || len(steps) > recallResponseMaxActionSteps {
		return false
	}
	for index, raw := range steps {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"ordinal", "step_ref", "proof_ref", "requires_confirmation"}) ||
			!recallResponseExactOrdinal(row["ordinal"], index+1) || !recallResponseValidDigest(anyToString(row["step_ref"])) ||
			!proofSet[anyToString(row["proof_ref"])] || !anyToBool(row["requires_confirmation"]) {
			return false
		}
	}
	if !recallResponseActionConditionsValid(payload["refusal_conditions"], proofSet, true) ||
		!recallResponseActionConditionsValid(payload[conditionKey], proofSet, false) {
		return false
	}
	if memoryToAction {
		// A memory-to-action component is admissible only when its mandatory
		// independent-authorization refusal is bound to the selected action
		// witness. An unresolved tool is still safe, but an unproved action
		// projection is not a valid component.
		for _, raw := range contextPackAnyList(payload["refusal_conditions"]) {
			row := anyMap(raw)
			if anyToString(row["code"]) == "independent_authorization_required" && proofSet[anyToString(row["proof_ref"])] {
				return true
			}
		}
		return false
	}
	return true
}

func recallResponseActionConditionsValid(value any, proofSet map[string]bool, requireAuthorization bool) bool {
	rows, ok := value.([]any)
	if !ok || len(rows) > recallResponseMaxActionConditions {
		return false
	}
	foundAuthorization := false
	seen := map[string]bool{}
	for _, raw := range rows {
		row := anyMap(raw)
		code := anyToString(row["code"])
		if !recallResponseExactFields(row, []string{"code", "proof_ref"}) || !recallResponseActionConditionCodes[code] ||
			seen[code] || (anyToString(row["proof_ref"]) != "" && !proofSet[anyToString(row["proof_ref"])]) {
			return false
		}
		seen[code] = true
		if code == "independent_authorization_required" {
			foundAuthorization = true
		}
	}
	return !requireAuthorization || foundAuthorization
}
