package main

import (
	"strings"
	"time"
)

func continuousCognitionRetrievalProjection(s *server, request continuousCognitionRequest) (string, bool) {
	if s == nil {
		return continuousCognitionUnavailableRef("retrieval_plan"), false
	}
	plan := s.buildAdaptiveRetrievalPlanAt(map[string]any{
		"query": request.Query, "project": request.Project,
		"topic_path": request.TopicPath, "retrieval_intent": request.RetrievalIntent,
		"retrieval_mode": request.RetrievalMode, "token_budget": request.TokenBudget,
	}, request.AsOf.UTC().Format(time.RFC3339Nano))
	if len(plan) == 0 {
		return continuousCognitionUnavailableRef("retrieval_plan"), false
	}
	stable := map[string]any{}
	for _, key := range []string{"mode", "activation_state", "task_phase", "retrieval_intent", "retrieval_mode", "token_budget", "evidence_obligations", "source_plan", "expansion", "budget_allocation", "stop_conditions", "calibration", "proof"} {
		if value, exists := plan[key]; exists {
			stable[key] = continuousCognitionStableValue(value, 0)
		}
	}
	return continuousCognitionDigestPrefix("ref_retrieval_plan_", stable), true
}

func continuousCognitionUtilityProjection(s *server, request continuousCognitionRequest) (string, string, bool, float64, continuousCognitionExpectedUtility, bool) {
	if s == nil || s.utility == nil {
		return continuousCognitionUnavailableRef("utility_snapshot"), "unavailable", false, 0, continuousCognitionExpectedUtility{}, false
	}
	rows := s.utility.rows(utilityQuery{
		Project: request.Project, RetrievalIntent: request.RetrievalIntent,
		WorkspaceRef: contextPackLearnedDigestRef(request.WorkspaceRef), To: request.AsOf.UTC(), Limit: request.Limit,
	})
	if len(rows) == 0 {
		return continuousCognitionUnavailableRef("utility_snapshot"), "unavailable", false, 0, continuousCognitionExpectedUtility{}, false
	}
	projected, pairs, pairExclusions := utilityPairProjection(rows)
	summary := utilityAggregate(projected, pairs, pairExclusions)
	stable := map[string]any{
		"observation_count":             summary["observation_count"],
		"independently_verified_count":  summary["independently_verified_count"],
		"observed_yield_eligible_count": summary["observed_yield_eligible_count"],
		"causal_pair_count":             summary["causal_pair_count"],
		"utility_unit_count":            summary["utility_unit_count"],
		"claim_status":                  summary["claim_status"],
		"denominators":                  continuousCognitionStableValue(summary["denominators"], 0),
		"observation_exclusions":        continuousCognitionStableValue(summary["observation_exclusions"], 0),
		"causal_exclusions":             continuousCognitionStableValue(summary["causal_exclusions"], 0),
	}
	// Commit 1 may observe cohort Utility rows for context, but they cannot
	// authorize the current cognition cycle or become a verified expectation.
	verified := false
	status := "contextual_unverified"
	expected := continuousCognitionExpectedUtility{}
	ref := continuousCognitionDigestPrefix("ref_utility_snapshot_", stable)
	return ref, status, verified, 0, expected, true
}

func continuousCognitionDefaultGovernance() continuousCognitionGovernance {
	return continuousCognitionGovernance{
		Outcome: continuousCognitionOutcome{
			State: "not_requested", OutcomeRef: continuousCognitionUnavailableRef("outcome"),
			ProofRef:              continuousCognitionUnavailableRef("proof"),
			UtilityObservationRef: continuousCognitionUnavailableRef("utility_observation"),
		},
		Evaluation: continuousCognitionEvaluation{
			State: "not_requested", UtilityStatus: "not_evaluated", Reason: "outcome_evaluation_not_requested",
		},
		Rollback: continuousCognitionLifecycleAdvice{
			State: "not_requested", ReasonRef: continuousCognitionUnavailableRef("rollback_reason"),
			TargetRef: continuousCognitionUnavailableRef("rollback_target"),
		},
		Retirement: continuousCognitionLifecycleAdvice{
			State: "not_requested", ReasonRef: continuousCognitionUnavailableRef("retirement_reason"),
			TargetRef: continuousCognitionUnavailableRef("retirement_target"),
		},
		ProjectionRef: continuousCognitionUnavailableRef("lifecycle_proof"),
	}
}

func continuousCognitionFinalizeGovernance(governance continuousCognitionGovernance) continuousCognitionGovernance {
	governance.ProjectionRef = continuousCognitionDigestPrefix("ref_lifecycle_proof_", map[string]any{
		"outcome":    continuousCognitionOutcomeMap(governance.Outcome),
		"evaluation": continuousCognitionEvaluationMap(governance.Evaluation),
		"rollback":   continuousCognitionLifecycleAdviceMap(governance.Rollback),
		"retirement": continuousCognitionLifecycleAdviceMap(governance.Retirement),
	})
	return governance
}

func continuousCognitionCanonicalIdentityMatches(row map[string]any, request continuousCognitionRequest, authorizedWorkspaceRef string) bool {
	if len(row) == 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TaskID) == "" ||
		strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.TaskIdentityID) == "" ||
		strings.TrimSpace(request.ExecutionLaneID) == "" || strings.TrimSpace(request.RetrievalIntent) == "" ||
		contextPackLearnedDigestRef(authorizedWorkspaceRef) == "" {
		return false
	}
	if !strings.EqualFold(anyToString(row["project"]), request.Project) ||
		anyToString(row["session_id"]) != request.SessionID || anyToString(row["task_id"]) != request.TaskID ||
		anyToString(row["agent_id"]) != request.AgentID ||
		!strings.EqualFold(anyToString(row["retrieval_intent"]), request.RetrievalIntent) ||
		anyToString(row["task_identity_id"]) != request.TaskIdentityID ||
		anyToString(row["execution_lane_id"]) != request.ExecutionLaneID ||
		contextPackLearnedDigestRef(anyToString(row["workspace_ref"])) != contextPackLearnedDigestRef(authorizedWorkspaceRef) {
		return false
	}
	binding, valid := recallResponseBindingFromSample(row)
	return valid && binding != nil
}

func continuousCognitionOutcomeTimestamp(row map[string]any) (time.Time, bool) {
	return continuousCognitionMapTimeAt(row, "gateway_received_at", "capturedAt", "captured_at")
}

func continuousCognitionSelectBoundOutcome(
	snapshot agentProofTimelineSnapshot,
	request continuousCognitionRequest,
	authorizedWorkspaceRef string,
) (map[string]any, map[string]any, bool, bool) {
	samples := make(map[string]map[string]any, len(snapshot.QualitySamples))
	for _, sample := range snapshot.QualitySamples {
		sampleID := strings.TrimSpace(anyToString(sample["sample_id"]))
		if sampleID == "" || !continuousCognitionCanonicalIdentityMatches(sample, request, authorizedWorkspaceRef) {
			continue
		}
		if existing, duplicate := samples[sampleID]; duplicate && contextPackQualitySampleAdmissionRef(existing) != contextPackQualitySampleAdmissionRef(sample) {
			return nil, nil, false, true
		}
		samples[sampleID] = cloneAnyMap(sample)
	}
	var selectedOutcome, selectedSample map[string]any
	var selectedAt time.Time
	for _, outcome := range snapshot.QualityOutcomes {
		sample := samples[anyToString(outcome["sample_id"])]
		if len(sample) == 0 || !continuousCognitionCanonicalIdentityMatches(outcome, request, authorizedWorkspaceRef) ||
			!contextPackOutcomeHasAuthoritativeSampleAdmission(outcome) ||
			contextPackQualitySampleAdmissionRef(sample) != anyToString(outcome["quality_sample_admission_ref"]) ||
			!contextPackQualityResponseBindingsEqual(sample, outcome) {
			continue
		}
		capturedAt, ok := continuousCognitionOutcomeTimestamp(outcome)
		if !ok || capturedAt.After(request.AsOf.UTC()) {
			continue
		}
		outcomeID := anyToString(outcome["outcome_id"])
		selectedID := anyToString(selectedOutcome["outcome_id"])
		if selectedOutcome == nil || capturedAt.After(selectedAt) || (capturedAt.Equal(selectedAt) && outcomeID > selectedID) {
			selectedOutcome, selectedSample, selectedAt = cloneAnyMap(outcome), cloneAnyMap(sample), capturedAt
		}
	}
	return selectedSample, selectedOutcome, selectedOutcome != nil, false
}

func continuousCognitionProjectOutcomeEvaluation(
	s *server,
	request continuousCognitionRequest,
	snapshot agentProofTimelineSnapshot,
	authorizedWorkspaceRef string,
) (continuousCognitionOutcome, continuousCognitionEvaluation, []continuousCognitionGap) {
	defaults := continuousCognitionDefaultGovernance()
	outcomeProjection, evaluation := defaults.Outcome, defaults.Evaluation
	outcomeProjection.State = "absent"
	evaluation.State = "unavailable"
	evaluation.UtilityStatus = "not_observed"
	evaluation.Reason = "exact_response_bound_outcome_unavailable"
	materialGap := request.Operation == continuousCognitionOperationOutcome || request.Operation == continuousCognitionOperationEvaluate
	gap := func(code, source string) []continuousCognitionGap {
		return []continuousCognitionGap{{Code: code, Source: source, Material: materialGap, DetailRef: continuousCognitionUnavailableRef(source)}}
	}
	if s == nil || s.contextPackQuality == nil || s.utility == nil {
		outcomeProjection.State = "source_unavailable"
		return outcomeProjection, evaluation, gap("outcome_authority_unavailable", "outcome")
	}
	if strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.TaskID) == "" || strings.TrimSpace(request.AgentID) == "" ||
		contextPackLearnedDigestRef(authorizedWorkspaceRef) == "" {
		outcomeProjection.State = "identity_incomplete"
		evaluation.Reason = "exact_identity_incomplete"
		return outcomeProjection, evaluation, gap("outcome_identity_incomplete", "outcome")
	}
	proofSample, proofOutcome, selected, sourceConflict := continuousCognitionSelectBoundOutcome(snapshot, request, authorizedWorkspaceRef)
	if sourceConflict {
		outcomeProjection.State = "source_conflict"
		evaluation.Reason = "conflicting_quality_receipts"
		return outcomeProjection, evaluation, gap("source_conflict", "outcome")
	}
	if !selected {
		return outcomeProjection, evaluation, gap("response_bound_outcome_not_found", "outcome")
	}
	sampleID := anyToString(proofSample["sample_id"])
	durableSample, durableOutcome, sampleFound, outcomeFound, durableErr := s.contextPackQuality.durableQualitySampleAndOutcomeForSample(sampleID)
	if durableErr != nil || !sampleFound || !outcomeFound ||
		contextPackQualitySampleAdmissionRef(durableSample) != anyToString(durableOutcome["quality_sample_admission_ref"]) ||
		contextPackQualitySampleAdmissionRef(durableSample) != contextPackQualitySampleAdmissionRef(proofSample) ||
		contextPackOutcomeLogicalClaimDigest(durableOutcome) != contextPackOutcomeLogicalClaimDigest(proofOutcome) ||
		!contextPackQualityResponseBindingsEqual(durableSample, durableOutcome) ||
		!continuousCognitionCanonicalIdentityMatches(durableSample, request, authorizedWorkspaceRef) ||
		!continuousCognitionCanonicalIdentityMatches(durableOutcome, request, authorizedWorkspaceRef) {
		outcomeProjection.State = "source_conflict"
		evaluation.Reason = "canonical_outcome_join_failed"
		return outcomeProjection, evaluation, gap("source_conflict", "outcome")
	}
	if capturedAt, ok := continuousCognitionOutcomeTimestamp(durableOutcome); !ok || capturedAt.After(request.AsOf.UTC()) {
		outcomeProjection.State = "temporal_projection_unavailable"
		evaluation.Reason = "outcome_time_unverifiable"
		return outcomeProjection, evaluation, gap("outcome_time_unverifiable", "outcome")
	}

	binding, _ := recallResponseBindingFromSample(durableOutcome)
	outcomeID := anyToString(durableOutcome["outcome_id"])
	outcomeProjection.State = "recorded"
	outcomeProjection.OutcomeRef = continuousCognitionOpaqueRef("outcome", outcomeID)
	outcomeProjection.ProofRef = continuousCognitionDigestPrefix("ref_outcome_proof_", map[string]any{
		"sample_admission_ref": anyToString(durableOutcome["quality_sample_admission_ref"]),
		"source_claim_digest":  contextPackOutcomeLogicalClaimDigest(durableOutcome),
		"response_binding_key": recallResponseBindingKey(binding),
	})

	rows := s.utility.rowsForOutcomeIDs(utilityQuery{
		Project: request.Project, TaskClass: anyToString(durableOutcome["task_class"]),
		WorkspaceRef: authorizedWorkspaceRef, To: request.AsOf.UTC(), Limit: request.Limit,
	}, map[string]struct{}{outcomeID: {}})
	if len(rows) == 0 {
		evaluation.State = "evidence_incomplete"
		evaluation.Reason = "utility_observation_unavailable"
		return outcomeProjection, evaluation, gap("utility_observation_unavailable", "utility")
	}
	projected, pairs, _ := utilityPairProjection(rows)
	var utilityRow map[string]any
	for _, row := range projected {
		if anyToString(row["outcome_id"]) == outcomeID {
			utilityRow = row
			break
		}
	}
	if len(utilityRow) == 0 || utilitySourceClaimDigest(durableOutcome) != anyToString(utilityRow["source_claim_digest"]) ||
		!recallResponseBindingsEqual(durableOutcome, utilityRow) ||
		!continuousCognitionCanonicalIdentityMatches(utilityRow, request, authorizedWorkspaceRef) {
		outcomeProjection.State = "source_conflict"
		evaluation.State = "unavailable"
		evaluation.Reason = "canonical_utility_join_failed"
		return outcomeProjection, evaluation, gap("source_conflict", "utility")
	}
	utilityClaim := anyMap(utilityRow["utility"])
	eligibility := anyMap(utilityRow["eligibility"])
	verified := anyToBool(utilityClaim["independently_verified"]) && anyToString(utilityClaim["verification_status"]) == "verified"
	causalEligible := anyToBool(eligibility["causal_gain_eligible"])
	if causalEligible {
		causalEligible = false
		for _, pair := range pairs {
			if pair.TreatmentOutcomeID == outcomeID {
				causalEligible = true
				break
			}
		}
	}
	outcomeProjection.UtilityObservationRef = continuousCognitionDigestPrefix("ref_utility_observation_", map[string]any{
		"observation_id": utilityRow["observation_id"], "observation_digest": utilityRow["observation_digest"],
		"revision": utilityRow["revision"], "source_claim_digest": utilityRow["source_claim_digest"],
	})
	outcomeProjection.IndependentlyVerified = verified
	outcomeProjection.CausalEligible = causalEligible
	evaluation.State = "evaluated"
	evaluation.UtilityStatus = firstNonEmptyStrings(anyToString(utilityRow["status"]), "excluded")
	evaluation.Verified = verified
	evaluation.CausalEligible = causalEligible
	evaluation.Reason = "exact_canonical_utility_observation"
	return outcomeProjection, evaluation, nil
}

func projectContinuousCognitionGovernance(
	s *server,
	request continuousCognitionRequest,
	snapshot agentProofTimelineSnapshot,
	observation continuousCognitionObservation,
	activation continuousCognitionActivation,
	authorizedWorkspaceRef string,
) (continuousCognitionGovernance, []continuousCognitionGap) {
	governance := continuousCognitionDefaultGovernance()
	if !continuousCognitionLifecycleOperation(request.Operation) {
		return governance, nil
	}
	var gaps []continuousCognitionGap
	governance.Outcome, governance.Evaluation, gaps = continuousCognitionProjectOutcomeEvaluation(s, request, snapshot, authorizedWorkspaceRef)
	if request.Operation == continuousCognitionOperationRollback {
		governance.Rollback.State = "not_applicable"
		governance.Rollback.ReasonRef = continuousCognitionDigestPrefix("ref_rollback_reason_", map[string]any{"activation_state": activation.State})
		if activation.Persisted && !continuousCognitionActivationTerminalState(activation.State) {
			governance.Rollback.State = "recommended"
			governance.Rollback.TargetRef = activation.PrepID
		}
	}
	if request.Operation == continuousCognitionOperationRetire {
		governance.Retirement.State = "not_ready"
		governance.Retirement.ReasonRef = continuousCognitionDigestPrefix("ref_retirement_reason_", map[string]any{
			"objective_state": observation.ObjectiveState, "activation_state": activation.State,
		})
		if observation.ObjectiveTerminal || continuousCognitionActivationTerminalState(activation.State) {
			governance.Retirement.State = "recommended"
			governance.Retirement.TargetRef = observation.ObjectiveGraphRef
			if activation.Persisted {
				governance.Retirement.TargetRef = activation.PrepID
			}
		}
	}
	return continuousCognitionFinalizeGovernance(governance), gaps
}

func applyContinuousCognitionGovernance(observation *continuousCognitionObservation, governance continuousCognitionGovernance, gaps []continuousCognitionGap) {
	if observation == nil {
		return
	}
	observation.LifecycleProofRef = governance.ProjectionRef
	observation.Gaps = continuousCognitionNormalizeGaps(append(observation.Gaps, gaps...))
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(*observation)
	observation.SourceComplete = continuousCognitionSourceIsComplete(*observation)
}

func continuousCognitionAddGap(observation *continuousCognitionObservation, code, source string, material bool) {
	if observation == nil {
		return
	}
	observation.Gaps = append(observation.Gaps, continuousCognitionGap{
		Code: code, Source: source, Material: material, DetailRef: continuousCognitionUnavailableRef(source),
	})
}

func continuousCognitionCompositeSourceAnchorDigest(observation continuousCognitionObservation) string {
	return frontierT6Digest(map[string]any{
		"proof_anchor_digest":  observation.ProofAnchorDigest,
		"objective_graph_ref":  observation.ObjectiveGraphRef,
		"session_rollup_ref":   observation.SessionRollupRef,
		"retrieval_plan_ref":   observation.RetrievalPlanRef,
		"investigation_ref":    observation.InvestigationRef,
		"investigation_proof":  observation.InvestigationProof,
		"utility_snapshot_ref": observation.UtilitySnapshotRef,
		"activation_ref":       observation.ActivationRef,
		"activation_state":     observation.ActivationState,
		"lifecycle_proof_ref":  observation.LifecycleProofRef,
		"normalized_gaps":      continuousCognitionGapMaps(continuousCognitionNormalizeGaps(observation.Gaps)),
		"continuity_zero_ref":  observation.ContinuityZeroRef,
		"proof_timeline_ref":   observation.ProofTimelineRef,
	})
}

func continuousCognitionSourceIsComplete(observation continuousCognitionObservation) bool {
	for _, gap := range observation.Gaps {
		if gap.Material {
			return false
		}
	}
	return observation.ObjectiveAvailable && observation.SessionPresent && observation.ProofComplete
}
