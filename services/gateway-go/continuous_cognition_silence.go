package main

import (
	"strings"
)

const (
	continuousCognitionSilencePolicyVersion = "continuous_cognition_silence.v1"
	continuousCognitionSilenceThreshold     = 2
)

type continuousCognitionSilence struct {
	Reason          string
	ValueInputs     map[string]any
	ValueScore      int
	Threshold       int
	PolicyVersion   string
	LoopGuardDigest string
}

func continuousCognitionSilenceDefaults() continuousCognitionSilence {
	return continuousCognitionSilence{
		Reason:        "not_silenced",
		ValueInputs:   map[string]any{},
		Threshold:     continuousCognitionSilenceThreshold,
		PolicyVersion: continuousCognitionSilencePolicyVersion,
	}
}

func continuousCognitionIdentityComplete(request continuousCognitionRequest, observation continuousCognitionObservation) bool {
	if strings.TrimSpace(request.Project) == "" || strings.TrimSpace(request.AgentID) == "" ||
		strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TaskID) == "" ||
		strings.TrimSpace(request.TaskIdentityID) == "" || strings.TrimSpace(request.ExecutionLaneID) == "" ||
		strings.TrimSpace(request.RetrievalIntent) == "" {
		return false
	}
	return strings.TrimSpace(observation.Scope.ScopeDigest) != "" &&
		strings.TrimSpace(observation.Scope.SessionRef) != continuousCognitionUnavailableRef("session") &&
		strings.TrimSpace(observation.Scope.TaskRef) != continuousCognitionUnavailableRef("task") &&
		strings.TrimSpace(observation.Scope.TaskIdentityRef) != continuousCognitionUnavailableRef("task_identity") &&
		strings.TrimSpace(observation.Scope.ExecutionLaneRef) != continuousCognitionUnavailableRef("execution_lane")
}

func continuousCognitionDuplicateCycle(observation continuousCognitionObservation) bool {
	for _, gap := range observation.Gaps {
		switch strings.ToLower(strings.TrimSpace(gap.Code)) {
		case "duplicate_cycle", "duplicate_cognition_cycle", "cycle_duplicate":
			return true
		}
	}
	return observation.ActivationState == "consumed" || observation.ActivationState == "terminal"
}

func continuousCognitionHardSilence(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "terminal", "duplicate", "missing_identity", "insufficient_identity", "policy_suppressed":
		return true
	default:
		return false
	}
}

func continuousCognitionSilenceSignals(
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
) (map[string]any, int, bool) {
	materialNewProof := observation.SourceComplete && observation.ProofComplete &&
		strings.TrimSpace(observation.SourceAnchorDigest) != "" &&
		!strings.Contains(observation.SourceAnchorDigest, "unavailable")
	blockedNextAction := observation.ObjectiveState == "blocked"
	switch frontier.NextActionClass {
	case "request_explicit_identity", "repair_source_identity", "request_more_evidence", "bounded_read_only_investigation":
		blockedNextAction = true
	}
	highImpactConflict := false
	if observation.ExpectedUtility.ConsequenceIfWrong >= 0.70 {
		for _, gap := range observation.Gaps {
			switch strings.ToLower(strings.TrimSpace(gap.Code)) {
			case "source_conflict", "identity_conflict", "conflict":
				if gap.Material {
					highImpactConflict = true
				}
			}
		}
	}
	actionableGap := false
	for _, gap := range observation.Gaps {
		if gap.Material && strings.TrimSpace(gap.DetailRef) != "" && !strings.Contains(gap.DetailRef, "unavailable") {
			actionableGap = true
			break
		}
	}
	inputs := map[string]any{
		"material_new_proof":             materialNewProof,
		"blocked_next_action":            blockedNextAction,
		"supported_high_impact_conflict": highImpactConflict,
		"actionable_gap":                 actionableGap,
		"utility_verified":               observation.UtilityVerified,
		"utility_status":                 firstNonEmptyStrings(observation.UtilityStatus, "unavailable"),
		"source_complete":                observation.SourceComplete,
		"proof_complete":                 observation.ProofComplete,
	}
	score := 0
	for _, key := range []string{"material_new_proof", "blocked_next_action", "supported_high_impact_conflict", "actionable_gap"} {
		if anyToBool(inputs[key]) {
			score++
		}
	}
	valueProven := observation.UtilityVerified && strings.TrimSpace(observation.UtilitySnapshotRef) != "" &&
		!strings.Contains(observation.UtilitySnapshotRef, "unavailable") &&
		observation.UtilityStatus != "" && observation.UtilityStatus != "contextual_unverified" && observation.UtilityStatus != "not_observed"
	return inputs, score, valueProven
}

// decideContinuousCognitionSilence is pure and deterministic. It is called
// after the bounded proof snapshot/frontier and before activation advice. It
// never dispatches, mutates, writes back, or records a cycle.
func decideContinuousCognitionSilence(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	policySuppressed bool,
) continuousCognitionSilence {
	result := continuousCognitionSilenceDefaults()
	inputs, score, valueProven := continuousCognitionSilenceSignals(observation, frontier)
	result.ValueInputs, result.ValueScore = inputs, score
	reason := "not_silenced"
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	switch {
	case policySuppressed:
		reason = "policy_suppressed"
	case observation.ObjectiveTerminal || continuousCognitionTerminalState(observation.ObjectiveState) || frontier.Decision == "retire":
		reason = "terminal"
	case continuousCognitionDuplicateCycle(observation):
		reason = "duplicate"
	case !continuousCognitionIdentityComplete(request, observation):
		reason = "missing_identity"
	case (operation == continuousCognitionOperationObserve || operation == continuousCognitionOperationInvestigate) && (!valueProven || score < result.Threshold):
		reason = "low_utility"
	}
	result.Reason = reason
	result.LoopGuardDigest = continuousCognitionDigestPrefix("silence_", map[string]any{
		"scope_digest": observation.Scope.ScopeDigest, "cycle_ref": observation.Scope.CycleRef,
		"source_anchor_digest": observation.SourceAnchorDigest, "reason": reason,
		"value_inputs": inputs, "value_score": score, "threshold": result.Threshold,
		"policy_version": result.PolicyVersion,
	})
	return result
}

func continuousCognitionSilenceMap(silence continuousCognitionSilence) map[string]any {
	if strings.TrimSpace(silence.Reason) == "" {
		silence = continuousCognitionSilenceDefaults()
	}
	if silence.ValueInputs == nil {
		silence.ValueInputs = map[string]any{}
	}
	if silence.Threshold <= 0 {
		silence.Threshold = continuousCognitionSilenceThreshold
	}
	if strings.TrimSpace(silence.PolicyVersion) == "" {
		silence.PolicyVersion = continuousCognitionSilencePolicyVersion
	}
	if strings.TrimSpace(silence.LoopGuardDigest) == "" {
		silence.LoopGuardDigest = continuousCognitionUnavailableRef("silence")
	}
	return map[string]any{
		"reason": silence.Reason, "value_inputs": continuousCognitionStableValue(silence.ValueInputs, 0),
		"value_score": silence.ValueScore, "threshold": silence.Threshold,
		"policy_version": silence.PolicyVersion, "loop_guard_digest": silence.LoopGuardDigest,
	}
}

func continuousCognitionContractFindings(object map[string]any) []map[string]any {
	findings := []map[string]any{}
	decision := strings.TrimSpace(anyToString(object["decision"]))
	allowedDecisions := map[string]struct{}{
		"continue": {}, "investigate": {}, "abstain": {}, "retire": {}, "silence": {},
	}
	if _, ok := allowedDecisions[decision]; !ok {
		findings = append(findings, map[string]any{"reason": "decision_invalid", "path": "decision", "contract_id": continuousCognitionContractID})
	}
	nextAction := strings.TrimSpace(anyToString(object["next_action"]))
	if nextAction == "" {
		findings = append(findings, map[string]any{"reason": "next_action_missing", "path": "next_action", "contract_id": continuousCognitionContractID})
	}
	silence := anyMap(object["silence"])
	knownSilenceFields := map[string]struct{}{"reason": {}, "value_inputs": {}, "value_score": {}, "threshold": {}, "policy_version": {}, "loop_guard_digest": {}}
	for key := range silence {
		if _, ok := knownSilenceFields[key]; !ok {
			findings = append(findings, map[string]any{"reason": "silence_field_not_closed", "path": "silence." + key, "contract_id": continuousCognitionContractID})
		}
	}
	valueInputs := anyMap(silence["value_inputs"])
	knownValueInputs := map[string]struct{}{
		"material_new_proof": {}, "blocked_next_action": {}, "supported_high_impact_conflict": {},
		"actionable_gap": {}, "utility_verified": {}, "utility_status": {}, "source_complete": {}, "proof_complete": {},
	}
	for key := range valueInputs {
		if _, ok := knownValueInputs[key]; !ok {
			findings = append(findings, map[string]any{"reason": "value_input_not_closed", "path": "silence.value_inputs." + key, "contract_id": continuousCognitionContractID})
		}
	}
	reason := strings.TrimSpace(anyToString(silence["reason"]))
	allowedReasons := map[string]struct{}{"not_silenced": {}, "terminal": {}, "duplicate": {}, "low_utility": {}, "missing_identity": {}, "policy_suppressed": {}}
	if _, ok := allowedReasons[reason]; !ok {
		findings = append(findings, map[string]any{"reason": "silence_reason_invalid", "path": "silence.reason", "contract_id": continuousCognitionContractID})
	}
	if decision == "silence" {
		if reason == "not_silenced" {
			findings = append(findings, map[string]any{"reason": "silence_reason_missing", "path": "silence.reason", "contract_id": continuousCognitionContractID})
		}
		if nextAction != "none" {
			findings = append(findings, map[string]any{"reason": "silence_next_action_must_be_none", "path": "next_action", "contract_id": continuousCognitionContractID})
		}
		if anyToBool(object["writeback_required"]) {
			findings = append(findings, map[string]any{"reason": "silence_writeback_forbidden", "path": "writeback_required", "contract_id": continuousCognitionContractID})
		}
		if anyToString(anyMap(object["activation"])["state"]) != "not_requested" {
			findings = append(findings, map[string]any{"reason": "silence_activation_forbidden", "path": "activation.state", "contract_id": continuousCognitionContractID})
		}
		for _, path := range []string{"safety.automatic_model_execution", "safety.automatic_external_mutation", "safety.runner_dispatch", "safety.filesystem_mutation", "safety.gateway_execution_performed", "activation.gateway_execution_performed"} {
			if anyToBool(dottedPathValue(object, path)) {
				findings = append(findings, map[string]any{"reason": "silence_side_effect_forbidden", "path": path, "contract_id": continuousCognitionContractID})
			}
		}
	} else if !anyToBool(object["writeback_required"]) {
		findings = append(findings, map[string]any{"reason": "non_silence_writeback_required", "path": "writeback_required", "contract_id": continuousCognitionContractID})
	} else if reason != "not_silenced" {
		findings = append(findings, map[string]any{"reason": "non_silence_reason_mismatch", "path": "silence.reason", "contract_id": continuousCognitionContractID})
	}
	return findings
}

func dottedPathValue(object map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var current any = object
	for _, part := range parts {
		mapValue, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = mapValue[part]
		if !ok {
			return nil
		}
	}
	return current
}
