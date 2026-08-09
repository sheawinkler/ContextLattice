package main

import (
	"sort"
	"strings"
)

func continuousCognitionNormalizeGaps(gaps []continuousCognitionGap) []continuousCognitionGap {
	copyGaps := append([]continuousCognitionGap{}, gaps...)
	sort.SliceStable(copyGaps, func(i, j int) bool {
		left := strings.Join([]string{copyGaps[i].Code, copyGaps[i].Source, copyGaps[i].DetailRef}, "\x00")
		right := strings.Join([]string{copyGaps[j].Code, copyGaps[j].Source, copyGaps[j].DetailRef}, "\x00")
		return left < right
	})
	result := make([]continuousCognitionGap, 0, len(copyGaps))
	seen := map[string]struct{}{}
	for _, gap := range copyGaps {
		key := strings.Join([]string{gap.Code, gap.Source, gap.DetailRef}, "\x00")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, gap)
	}
	return result
}

func continuousCognitionExpectedUtilityMap(value continuousCognitionExpectedUtility) map[string]any {
	value = continuousCognitionExpectedUtilityValue(value)
	return map[string]any{
		"action_change_probability": value.ActionChangeProbability,
		"consequence_if_wrong":      value.ConsequenceIfWrong,
		"evidence_reliability":      value.EvidenceReliability,
		"acquisition_cost":          value.AcquisitionCost,
		"score":                     value.Score,
	}
}

func continuousCognitionOutcomeMap(outcome continuousCognitionOutcome) map[string]any {
	defaults := continuousCognitionDefaultGovernance().Outcome
	if strings.TrimSpace(outcome.State) == "" {
		outcome.State = defaults.State
	}
	if strings.TrimSpace(outcome.OutcomeRef) == "" {
		outcome.OutcomeRef = defaults.OutcomeRef
	}
	if strings.TrimSpace(outcome.ProofRef) == "" {
		outcome.ProofRef = defaults.ProofRef
	}
	if strings.TrimSpace(outcome.UtilityObservationRef) == "" {
		outcome.UtilityObservationRef = defaults.UtilityObservationRef
	}
	return map[string]any{
		"state": outcome.State, "outcome_ref": outcome.OutcomeRef, "proof_ref": outcome.ProofRef,
		"utility_observation_ref": outcome.UtilityObservationRef,
		"independently_verified":  outcome.IndependentlyVerified, "causal_eligible": outcome.CausalEligible,
	}
}

func continuousCognitionEvaluationMap(evaluation continuousCognitionEvaluation) map[string]any {
	defaults := continuousCognitionDefaultGovernance().Evaluation
	if strings.TrimSpace(evaluation.State) == "" {
		evaluation.State = defaults.State
	}
	if strings.TrimSpace(evaluation.UtilityStatus) == "" {
		evaluation.UtilityStatus = defaults.UtilityStatus
	}
	if strings.TrimSpace(evaluation.Reason) == "" {
		evaluation.Reason = defaults.Reason
	}
	return map[string]any{
		"state": evaluation.State, "utility_status": evaluation.UtilityStatus,
		"verified": evaluation.Verified, "causal_eligible": evaluation.CausalEligible, "reason": evaluation.Reason,
	}
}

func continuousCognitionLifecycleAdviceMap(advice continuousCognitionLifecycleAdvice) map[string]any {
	if strings.TrimSpace(advice.State) == "" {
		advice.State = "not_requested"
	}
	if strings.TrimSpace(advice.ReasonRef) == "" {
		advice.ReasonRef = continuousCognitionUnavailableRef("lifecycle_reason")
	}
	if strings.TrimSpace(advice.TargetRef) == "" {
		advice.TargetRef = continuousCognitionUnavailableRef("lifecycle_target")
	}
	return map[string]any{"state": advice.State, "reason_ref": advice.ReasonRef, "target_ref": advice.TargetRef}
}

func buildContinuousCognitionSemanticPayload(request continuousCognitionRequest, observation continuousCognitionObservation, frontier continuousCognitionFrontier) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithInvestigation(
		request,
		observation,
		frontier,
		continuousCognitionInvestigation{},
	)
}

func buildContinuousCognitionSemanticPayloadWithInvestigation(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithLifecycle(
		request,
		observation,
		frontier,
		investigation,
		continuousCognitionDefaultActivation(),
	)
}

func buildContinuousCognitionSemanticPayloadWithLifecycle(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithGovernance(
		request, observation, frontier, investigation, activation, continuousCognitionDefaultGovernance(),
	)
}

func buildContinuousCognitionSemanticPayloadWithGovernance(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
	governance continuousCognitionGovernance,
) map[string]any {
	return buildContinuousCognitionSemanticPayloadWithGovernanceAndSilence(
		request, observation, frontier, investigation, activation, governance,
		decideContinuousCognitionSilence(request, observation, frontier, false),
	)
}

func buildContinuousCognitionSemanticPayloadWithGovernanceAndSilence(
	request continuousCognitionRequest,
	observation continuousCognitionObservation,
	frontier continuousCognitionFrontier,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
	governance continuousCognitionGovernance,
	silence continuousCognitionSilence,
) map[string]any {
	if strings.TrimSpace(silence.Reason) == "" {
		silence = continuousCognitionSilenceDefaults()
	}
	request.Operation = strings.ToLower(strings.TrimSpace(request.Operation))
	if _, allowed := continuousCognitionOperations[request.Operation]; !allowed {
		request.Operation = continuousCognitionOperationObserve
	}
	if observation.Scope.ScopeDigest == "" {
		observation.Scope = continuousCognitionScopeFromRequest(request)
	}
	observation.Scope.RetrievalIntent = normalizeRetrievalIntent(observation.Scope.RetrievalIntent, "decision")
	observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
	frontier.ExpectedUtility = continuousCognitionExpectedUtilityValue(frontier.ExpectedUtility)
	frontier.UtilityScore = continuousCognitionFinite01(frontier.UtilityScore)
	cognitionID := continuousCognitionDigestPrefix("cc_", map[string]any{
		"scope_digest":  observation.Scope.ScopeDigest,
		"cycle_ref":     observation.Scope.CycleRef,
		"objective_ref": continuousCognitionOpaqueRef("objective", request.ObjectiveID),
		"operation":     request.Operation,
	})
	frontier.FrontierID = continuousCognitionDigestPrefix("frontier_", map[string]any{
		"cognition_id":         cognitionID,
		"decision":             frontier.Decision,
		"source_anchor_digest": observation.SourceAnchorDigest,
	})
	observationMap := map[string]any{
		"objective_graph_ref":  observation.ObjectiveGraphRef,
		"session_rollup_ref":   observation.SessionRollupRef,
		"continuity_zero_ref":  observation.ContinuityZeroRef,
		"proof_timeline_ref":   observation.ProofTimelineRef,
		"retrieval_plan_ref":   observation.RetrievalPlanRef,
		"utility_snapshot_ref": observation.UtilitySnapshotRef,
		"lifecycle_proof_ref":  observation.LifecycleProofRef,
		"source_anchor_digest": observation.SourceAnchorDigest,
		"source_complete":      observation.SourceComplete,
		"gaps":                 continuousCognitionGapMaps(observation.Gaps),
	}
	phase := "frontier"
	progressStatus := "observed"
	if silence.Reason != "not_silenced" {
		frontier.Decision = "silence"
		frontier.NextActionClass = "none"
		frontier.StopReason = silence.Reason
		phase = "silence"
		progressStatus = "silenced"
	}
	if silence.Reason == "not_silenced" && request.Operation == continuousCognitionOperationInvestigate {
		phase = "investigation"
		progressStatus = "investigated"
	} else if silence.Reason == "not_silenced" && request.Operation == continuousCognitionOperationStatus {
		phase = "status"
		progressStatus = "status"
	} else if silence.Reason == "not_silenced" && request.Operation == continuousCognitionOperationOutcome {
		phase = "outcome"
		progressStatus = "outcome_projected"
	} else if silence.Reason == "not_silenced" && request.Operation == continuousCognitionOperationEvaluate {
		phase = "evaluation"
		progressStatus = "evaluation_projected"
	} else if silence.Reason == "not_silenced" && request.Operation == continuousCognitionOperationRollback {
		phase = "rollback"
		progressStatus = "rollback_advisory"
	} else if silence.Reason == "not_silenced" && request.Operation == continuousCognitionOperationRetire {
		phase = "retirement"
		progressStatus = "retirement_advisory"
	}
	if strings.TrimSpace(investigation.State) == "" {
		investigation = continuousCognitionDefaultInvestigation(request.Operation, observation.SourceComplete)
	}
	if strings.TrimSpace(activation.State) == "" {
		activation = continuousCognitionDefaultActivation()
	}
	if silence.Reason != "not_silenced" {
		activation = continuousCognitionDefaultActivation()
	}
	progress := continuousCognitionProgressMap(request.Operation, phase, progressStatus, observation, investigation, activation)
	nextAction := frontier.NextActionClass
	writebackRequired := true
	if silence.Reason != "not_silenced" {
		nextAction = "none"
		writebackRequired = false
	}
	payload := map[string]any{
		"ok": true, "schema_id": continuousCognitionContractID, "version": 1,
		"cognition_id": cognitionID, "operation": request.Operation,
		"phase": phase, "decision": frontier.Decision, "next_action": nextAction,
		"request_scope": map[string]any{
			"scope_digest": observation.Scope.ScopeDigest, "query_digest": observation.Scope.QueryDigest,
			"workspace_ref": observation.Scope.WorkspaceRef, "project_ref": observation.Scope.ProjectRef,
			"topic_ref": observation.Scope.TopicRef, "agent_ref": observation.Scope.AgentRef,
			"session_ref": observation.Scope.SessionRef, "task_ref": observation.Scope.TaskRef,
			"task_identity_ref": observation.Scope.TaskIdentityRef, "execution_lane_ref": observation.Scope.ExecutionLaneRef,
			"retrieval_intent": observation.Scope.RetrievalIntent, "cycle_ref": observation.Scope.CycleRef,
		},
		"observation": observationMap,
		"frontier": map[string]any{
			"frontier_id": frontier.FrontierID, "objective_state": frontier.ObjectiveState,
			"uncertainty": frontier.Uncertainty, "next_action_class": frontier.NextActionClass,
			"utility_score": frontier.UtilityScore, "expected_utility": continuousCognitionExpectedUtilityMap(frontier.ExpectedUtility),
			"stop_reason": frontier.StopReason,
		},
		"investigation": continuousCognitionInvestigationMap(investigation),
		"activation":    continuousCognitionActivationMap(activation),
		"silence":       continuousCognitionSilenceMap(silence),
		"outcome":       continuousCognitionOutcomeMap(governance.Outcome),
		"evaluation":    continuousCognitionEvaluationMap(governance.Evaluation),
		"rollback":      continuousCognitionLifecycleAdviceMap(governance.Rollback),
		"retirement":    continuousCognitionLifecycleAdviceMap(governance.Retirement),
		"progress":      progress,
		"safety": map[string]any{
			"advisory_only": true, "automatic_model_execution": false, "automatic_external_mutation": false,
			"runner_dispatch": false, "filesystem_mutation": false, "gateway_execution_performed": false,
			"requires_explicit_authorization": true, "requires_external_worker": true, "network_calls": 0,
		},
		"gaps": continuousCognitionGapMaps(observation.Gaps), "writeback_required": writebackRequired,
	}
	digestMaterial := cloneAnyMap(payload)
	delete(digestMaterial, "cognition_digest")
	payload["cognition_digest"] = frontierT6Digest(digestMaterial)
	return payload
}

func continuousCognitionDefaultActivation() continuousCognitionActivation {
	return continuousCognitionActivation{
		State:            "not_requested",
		PrepID:           continuousCognitionUnavailableRef("prep"),
		ApprovalRef:      continuousCognitionUnavailableRef("approval"),
		AuthorizationRef: continuousCognitionUnavailableRef("authorization"),
		ConsumptionRef:   continuousCognitionUnavailableRef("consumption"),
		ProjectionRef:    continuousCognitionUnavailableRef("activation"),
		ExecutionOwner:   "external_cli_worker",
	}
}

func continuousCognitionActivationMap(activation continuousCognitionActivation) map[string]any {
	defaults := continuousCognitionDefaultActivation()
	if strings.TrimSpace(activation.State) == "" {
		activation.State = defaults.State
	}
	if strings.TrimSpace(activation.PrepID) == "" {
		activation.PrepID = defaults.PrepID
	}
	if strings.TrimSpace(activation.ApprovalRef) == "" {
		activation.ApprovalRef = defaults.ApprovalRef
	}
	if strings.TrimSpace(activation.AuthorizationRef) == "" {
		activation.AuthorizationRef = defaults.AuthorizationRef
	}
	if strings.TrimSpace(activation.ConsumptionRef) == "" {
		activation.ConsumptionRef = defaults.ConsumptionRef
	}
	if strings.TrimSpace(activation.ExecutionOwner) == "" {
		activation.ExecutionOwner = defaults.ExecutionOwner
	}
	return map[string]any{
		"state": activation.State, "prep_id": activation.PrepID,
		"approval_ref": activation.ApprovalRef, "authorization_ref": activation.AuthorizationRef,
		"consumption_ref": activation.ConsumptionRef,
		"execution_owner": activation.ExecutionOwner, "one_shot": true,
		"requires_explicit_cli_use": true, "gateway_execution_performed": false,
	}
}

func continuousCognitionProgressMap(
	operation string,
	phase string,
	status string,
	observation continuousCognitionObservation,
	investigation continuousCognitionInvestigation,
	activation continuousCognitionActivation,
) map[string]any {
	round := 0
	if operation == continuousCognitionOperationInvestigate && investigation.ExecutionPerformed {
		round = 1
	}
	dedupeDecision := "not_persisted"
	persisted := false
	if activation.Persisted {
		persisted = true
		dedupeDecision = "existing_one_shot_preparation"
		if operation == continuousCognitionOperationObserve || operation == continuousCognitionOperationInvestigate || operation == continuousCognitionOperationStatus {
			phase = "activation"
			switch activation.State {
			case "queued", "retry_pending":
				status = "activation_pending"
			case "preparing":
				status = "activation_preparing"
			case "ready":
				status = "activation_ready"
			case "consumed":
				status = "activation_consumed"
			case "failed", "expired", "canceled":
				status = "activation_terminal"
			default:
				status = "activation_state_unavailable"
			}
		}
	}
	return map[string]any{
		"status": status, "stage": phase, "round": round, "max_rounds": 3,
		"proof_timeline_ref": observation.ProofTimelineRef,
		"loop_guard": map[string]any{
			"cycle_ref": observation.Scope.CycleRef, "source_anchor_digest": observation.SourceAnchorDigest,
			"round": round, "max_rounds": 3, "dedupe_decision": dedupeDecision, "persisted": persisted,
		},
	}
}

func continuousCognitionDefaultInvestigation(operation string, sourceComplete bool) continuousCognitionInvestigation {
	state := "not_requested"
	mode := "read_only"
	if operation == continuousCognitionOperationInvestigate {
		state = "not_executed"
		mode = "read_only_investigation"
	}
	return continuousCognitionInvestigation{
		State:               state,
		Mode:                mode,
		ContextPackRef:      continuousCognitionUnavailableRef("context_pack"),
		RetrievalReceiptRef: continuousCognitionUnavailableRef("retrieval_receipt"),
		SourceComplete:      sourceComplete,
		MutationsSuppressed: true,
	}
}

func continuousCognitionInvestigationMap(investigation continuousCognitionInvestigation) map[string]any {
	if strings.TrimSpace(investigation.State) == "" {
		investigation = continuousCognitionDefaultInvestigation(continuousCognitionOperationObserve, false)
	}
	if strings.TrimSpace(investigation.Mode) == "" {
		investigation.Mode = "read_only"
	}
	if strings.TrimSpace(investigation.ContextPackRef) == "" {
		investigation.ContextPackRef = continuousCognitionUnavailableRef("context_pack")
	}
	if strings.TrimSpace(investigation.RetrievalReceiptRef) == "" {
		investigation.RetrievalReceiptRef = continuousCognitionUnavailableRef("retrieval_receipt")
	}
	return map[string]any{
		"state":                 investigation.State,
		"mode":                  investigation.Mode,
		"context_pack_ref":      investigation.ContextPackRef,
		"retrieval_receipt_ref": investigation.RetrievalReceiptRef,
		"source_coverage": map[string]any{
			"complete":              investigation.SourceComplete,
			"retrieval_count":       investigation.RetrievalCount,
			"compiler_count":        investigation.CompilerCount,
			"evidence_ref_count":    investigation.EvidenceRefCount,
			"scanned_count":         investigation.ScannedCount,
			"truncated":             investigation.Truncated,
			"learned_ranking_state": "control_shadow_only",
			"raw_material_exposed":  false,
		},
		"mutations_suppressed": investigation.MutationsSuppressed,
		"execution_performed":  investigation.ExecutionPerformed,
		"network_calls":        investigation.NetworkCalls,
	}
}
