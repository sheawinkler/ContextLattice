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
	lifecycle := recallResponseProjectLifecycleMetadata(source)
	status := recallResponseSafeStatus(firstNonEmptyStrings(anyToString(raw["status"]), anyToString(source["status"]), anyToString(source["proof_status"])))
	supersedes := contextPackAnyList(raw["supersedes"])
	if len(supersedes) == 0 {
		supersedes = contextPackAnyList(source["supersedes"])
	}
	transitions := recallResponseProjectedTransitions(firstPresentAny(raw["transitions"], source["status_transitions"]))
	validFrom := recallResponseProjectedTime(firstNonEmptyStrings(anyToString(raw["valid_from"]), anyToString(source["valid_from"])))
	validTo := recallResponseProjectedTime(firstNonEmptyStrings(anyToString(raw["valid_to"]), anyToString(source["valid_to"])))
	if status == "unknown" && len(supersedes) == 0 && len(transitions) == 0 && validFrom == "" && validTo == "" && len(lifecycle) == 0 {
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
	for key, value := range lifecycle {
		out[key] = value
	}
	return out
}

// recallResponseProjectLifecycleMetadata carries only closed lifecycle and
// freshness fields across the retrieval-to-context-pack boundary. Raw
// recall_metadata is not trusted or copied wholesale, but omitting these
// server-owned markers would let a row that looks current at the top level
// lose its retired, forgotten, or test classification before response policy
// sees it.
func recallResponseProjectLifecycleMetadata(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	fields := []string{
		"lifecycle", "lifecycle_status", "revocation_status", "forgetting_status", "quarantine_status",
		"state", "trust_class", "safety_class", "sensitivity", "freshness", "temporal_state",
		"memory_type", "record_type", "memory_class", "classification", "data_class",
		"is_test", "test", "test_memory", "synthetic", "fixture", "forgotten", "retired",
	}
	out := map[string]any{}
	for _, values := range recallResponseLifecycleCarrierMaps(source) {
		for _, key := range fields {
			value, present := values[key]
			if !present {
				continue
			}
			switch typed := value.(type) {
			case bool:
				// Exclusion flags are monotonic across carriers. A copied false
				// value cannot erase an authoritative true value found elsewhere.
				out[key] = anyToBool(out[key]) || typed
			case string:
				if strings.TrimSpace(typed) != "" {
					out[key] = strings.TrimSpace(typed)
				}
			}
		}
	}
	lifecycle := recallResponseCanonicalLifecycle(source)
	if lifecycle.hard {
		out["lifecycle"] = lifecycle.canonical
		switch lifecycle.canonical {
		case "test":
			out["test"] = true
		case "forgotten":
			out["forgotten"] = true
		case "retired":
			if lifecycle.retirement {
				out["retired"] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
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

// recallResponseAgreedRawActionMetadata resolves every action alias consumed by
// the production compiler, including the already-projected nested carrier. A
// row is an action witness only when all present non-empty aliases are exact
// canonical matches. Malformed, partial-vs-complete, or conflicting aliases
// fail closed instead of relying on alias precedence.
func recallResponseAgreedRawActionMetadata(source map[string]any) (map[string]any, bool, bool) {
	type carrier struct {
		value   any
		present bool
	}
	carriers := make([]carrier, 0, 4)
	for _, key := range []string{"action_evidence", "structured_action", "action"} {
		value, present := source[key]
		carriers = append(carriers, carrier{value: value, present: present})
	}
	metadata := anyMap(source["recall_metadata"])
	nested, nestedPresent := metadata["action"]
	carriers = append(carriers, carrier{value: nested, present: nestedPresent})

	var selected map[string]any
	canonical := ""
	present := false
	for _, candidate := range carriers {
		if !candidate.present || candidate.value == nil {
			continue
		}
		payload := anyMap(candidate.value)
		if len(payload) == 0 {
			// Empty maps and blank strings are absence, matching the historical
			// optional aliases. Any other non-map value is an invalid carrier.
			switch typed := candidate.value.(type) {
			case map[string]any:
				continue
			case string:
				if strings.TrimSpace(typed) == "" {
					continue
				}
			}
			return nil, true, false
		}
		encoded := recallResponseCanonicalJSON(payload)
		if encoded == "{}" {
			return nil, true, false
		}
		if !present {
			selected = cloneJSONMap(payload)
			canonical = encoded
			present = true
			continue
		}
		if encoded != canonical {
			return nil, true, false
		}
	}
	return selected, present, true
}

func recallResponseProjectActionMetadata(source map[string]any) map[string]any {
	raw, present, valid := recallResponseAgreedRawActionMetadata(source)
	if !present || !valid || len(raw) == 0 {
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
		action := recallResponseProjectActionMetadata(row)
		if len(action) > 0 {
			return true
		}
	}
	return false
}

func recallResponseHasReadyActionEvidence(items []any) bool {
	for _, raw := range items {
		row := anyMap(raw)
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if !eligible || !confidenceValid {
			continue
		}
		action := recallResponseProjectActionMetadata(row)
		if recallResponseActionMetadataReady(action) {
			return true
		}
	}
	return false
}

func recallResponseHasSensitiveUnavailableActionEvidence(items []any) bool {
	for _, raw := range items {
		row := anyMap(raw)
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if !eligible || !confidenceValid {
			continue
		}
		action := recallResponseProjectActionMetadata(row)
		if len(action) == 0 {
			continue
		}
		if recallResponseActionHasSensitiveUnavailableRefusal(action["refusal_conditions"]) {
			return true
		}
		for _, parameter := range contextPackAnyList(action["parameter_bindings"]) {
			if strings.EqualFold(strings.TrimSpace(anyToString(anyMap(parameter)["value_state"])), "sensitive_unresolved") {
				return true
			}
		}
	}
	return false
}

// recallResponseSensitiveUnavailableActionStatus returns one deterministic
// eligible witness only when every eligible action carrier agrees on the same
// complete projected action payload. The witness remains memory-as-data: this
// helper establishes a non-executable capability/status disclosure, never an
// executable action. An excluded copy of the same source identity vetoes the
// status, while an unrelated quarantined row remains typed exclusion evidence
// and cannot erase an independent eligible witness.
func recallResponseSensitiveUnavailableActionStatus(items []any) (map[string]any, bool) {
	excludedIdentities := map[string]bool{}
	eligibleIdentities := map[string]bool{}
	rows := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		row := anyMap(raw)
		if len(row) == 0 {
			continue
		}
		rows = append(rows, row)
		identity := recallResponseCanonicalSourceRef(row, "evidence")
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if identity != "" && (!eligible || !confidenceValid) {
			excludedIdentities[identity] = true
		}
	}

	canonical := ""
	var selected map[string]any
	selectedIdentity := ""
	var selectedAction map[string]any
	for _, row := range rows {
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if !eligible || !confidenceValid {
			continue
		}
		action := recallResponseProjectActionMetadata(row)
		if len(action) == 0 {
			continue
		}
		encoded := recallResponseCanonicalJSON(action)
		if encoded == "{}" || (canonical != "" && encoded != canonical) {
			return nil, false
		}
		if canonical == "" {
			canonical = encoded
			selectedAction = action
		}
		identity := recallResponseCanonicalSourceRef(row, "evidence")
		if identity != "" {
			eligibleIdentities[identity] = true
		}
		if selected == nil || identity < selectedIdentity {
			selected = row
			selectedIdentity = identity
		}
	}
	if selected == nil || len(selectedAction) == 0 ||
		(!recallResponseActionHasSensitiveUnavailableBinding(selectedAction["parameter_bindings"]) &&
			!recallResponseActionHasSensitiveUnavailableRefusal(selectedAction["refusal_conditions"])) {
		return nil, false
	}
	for identity := range eligibleIdentities {
		if excludedIdentities[identity] {
			return nil, false
		}
	}
	return selected, true
}

// recallResponseActionProjectionAllowed is the production action-preparation
// gate. Action metadata remains memory-as-data: proof/verification requests,
// incomplete source coverage, and retirement/supersession evidence may still
// be explained, but they cannot become a prepared tool or parameter binding.
// A structured action is projected only when at least one eligible witness is
// present. Parameter readiness is tracked separately: unresolved/sensitive
// bindings may be explained with refusal conditions but are not selectable.
func recallResponseActionProjectionAllowed(source map[string]any) bool {
	if len(source) == 0 {
		// Post-boundary recomposition has no raw source snapshot. It may reuse
		// only the already validated component payload below.
		return true
	}
	retrievalIntent := strings.ToLower(strings.TrimSpace(anyToString(source["retrieval_intent"])))
	if retrievalIntent == "proof" || retrievalIntent == "verification" {
		return false
	}
	coverage := anyMap(source["source_coverage"])
	if len(coverage) == 0 {
		coverage = anyMap(source["sourceCoverage"])
	}
	if len(coverage) > 0 && !anyToBool(coverage["complete"]) {
		return false
	}
	if recallResponseTemporalHasRetirement(source) {
		return false
	}
	for _, raw := range recallResponseTemporalRows(source) {
		row := anyMap(raw)
		relation := strings.ToLower(strings.TrimSpace(anyToString(row["relation"])))
		if relation == "retires" || relation == "retired" {
			return false
		}
		_, eligible := recallResponseEvidenceStatus(row)
		_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
		if !eligible || !confidenceValid {
			continue
		}
		action := recallResponseProjectActionMetadata(row)
		if len(action) > 0 {
			return true
		}
	}
	return false
}

func recallResponseActionMetadataReady(action map[string]any) bool {
	toolRef := anyToString(action["tool_ref"])
	if toolRef == "unresolved_tool" || !recallResponseValidDigest(toolRef) {
		return false
	}
	return recallResponseActionParameterBindingsReady(action["parameter_bindings"]) &&
		!recallResponseActionHasSensitiveUnavailableRefusal(action["refusal_conditions"])
}

func recallResponseActionPayloadReady(payload map[string]any) bool {
	toolRef := firstNonEmptyStrings(anyToString(payload["tool_ref"]), anyToString(payload["intended_tool_ref"]))
	if toolRef == "unresolved_tool" || !recallResponseValidDigest(toolRef) {
		return false
	}
	if !recallResponseActionParameterBindingsReady(payload["parameter_bindings"]) {
		return false
	}
	for _, raw := range contextPackAnyList(payload["refusal_conditions"]) {
		switch anyToString(anyMap(raw)["code"]) {
		case "credential_access", "missing_required_parameter", "sensitive_value_unavailable", "proof_mismatch":
			return false
		}
	}
	return true
}

// recallResponseActionParameterBindingsReady is the execution/selectability
// gate for an already projected action. Sensitive values are never available
// at this boundary: a sensitive unresolved binding is therefore not ready
// even when optional, and an unsafe binding can never be treated as bound.
// Ordinary optional unresolved values remain explainable without authorizing
// execution; required ordinary unresolved values remain not ready.
func recallResponseActionParameterBindingsReady(value any) bool {
	for _, raw := range contextPackAnyList(value) {
		row := anyMap(raw)
		state := strings.ToLower(strings.TrimSpace(anyToString(row["value_state"])))
		if state == "sensitive_unresolved" || state == "unsafe" {
			return false
		}
		if anyToBool(row["required"]) && state != "bound_redacted" {
			return false
		}
	}
	return true
}

func recallResponseActionHasSensitiveUnavailableBinding(value any) bool {
	for _, raw := range contextPackAnyList(value) {
		row := anyMap(raw)
		state := strings.ToLower(strings.TrimSpace(anyToString(row["value_state"])))
		if state == "sensitive_unresolved" || (anyToBool(row["sensitive"]) && state != "bound_redacted") {
			return true
		}
	}
	return false
}

func recallResponseActionHasSensitiveUnavailableRefusal(value any) bool {
	for _, raw := range contextPackAnyList(value) {
		if anyToString(anyMap(raw)["code"]) == "sensitive_value_unavailable" ||
			strings.ToLower(strings.TrimSpace(anyToString(raw))) == "sensitive_value_unavailable" {
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
	if len(source) == 0 {
		// Boundary recomposition operates only on the bounded response. Reuse
		// the previously validated payload instead of attempting to recreate an
		// action from source material that is intentionally unavailable here.
		for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
			module := anyMap(raw)
			if anyToString(module["kind"]) == kind {
				if payload := anyMap(module["payload"]); len(payload) > 0 {
					return cloneJSONMap(payload)
				}
			}
		}
	}
	if kind == "memory_to_action" {
		if row, ok := recallResponseSensitiveUnavailableActionStatus(recallResponseTemporalRows(source)); ok {
			proofRef := recallResponseProjectedRowRef(row, response)
			if !containsString(refs, proofRef) {
				proofRef = ""
			}
			return map[string]any{
				"intended_tool_ref":  "unresolved_tool",
				"parameter_bindings": []any{},
				"ordered_steps":      []any{},
				"refusal_conditions": recallResponseActionConditions(
					[]any{"independent_authorization_required", "sensitive_value_unavailable"}, proofRef, false,
				),
				"rollback_conditions": []any{},
			}
		}
	}
	action := map[string]any{}
	proofRef := ""
	if !recallResponseActionProjectionAllowed(source) {
		proofRef = firstString(refs)
		action = map[string]any{
			"tool_ref":            "unresolved_tool",
			"parameter_bindings":  []any{},
			"ordered_steps":       []any{},
			"refusal_conditions":  []any{"independent_authorization_required", "proof_mismatch"},
			"recovery_conditions": []any{},
			"rollback_conditions": []any{},
		}
	}
	for _, raw := range recallResponseTemporalRows(source) {
		if len(action) > 0 {
			break
		}
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
		metadata := recallResponseProjectActionMetadata(row)
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
		if recallResponseActionHasSensitiveUnavailableBinding(payload["parameter_bindings"]) {
			return false
		}
		if recallResponseActionHasSensitiveUnavailableRefusal(payload["refusal_conditions"]) {
			return recallResponseSensitiveUnavailableActionPayloadValid(payload, proofSet)
		}
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

func recallResponseSensitiveUnavailableActionPayloadValid(payload map[string]any, proofSet map[string]bool) bool {
	if anyToString(payload["intended_tool_ref"]) != "unresolved_tool" ||
		len(contextPackAnyList(payload["parameter_bindings"])) != 0 ||
		len(contextPackAnyList(payload["ordered_steps"])) != 0 ||
		len(contextPackAnyList(payload["rollback_conditions"])) != 0 {
		return false
	}
	refusals := contextPackAnyList(payload["refusal_conditions"])
	if len(refusals) != 2 {
		return false
	}
	seen := map[string]bool{}
	proofRef := ""
	for _, raw := range refusals {
		row := anyMap(raw)
		code := anyToString(row["code"])
		ref := anyToString(row["proof_ref"])
		if !recallResponseOneOf(code, "independent_authorization_required", "sensitive_value_unavailable") ||
			seen[code] || ref == "" || !proofSet[ref] || (proofRef != "" && ref != proofRef) {
			return false
		}
		seen[code] = true
		proofRef = ref
	}
	return seen["independent_authorization_required"] && seen["sensitive_value_unavailable"]
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
