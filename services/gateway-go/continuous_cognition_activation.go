package main

import (
	"strings"
	"time"
)

func continuousCognitionFinalizeActivation(activation continuousCognitionActivation) continuousCognitionActivation {
	activation.ProjectionRef = continuousCognitionDigestPrefix("ref_activation_", map[string]any{
		"state": activation.State, "prep_id": activation.PrepID,
		"approval_ref": activation.ApprovalRef, "authorization_ref": activation.AuthorizationRef,
		"consumption_ref": activation.ConsumptionRef, "persisted": activation.Persisted,
	})
	return activation
}

// projectContinuousCognitionActivation takes one bounded read-only snapshot of
// the existing T6 preparation store. The authorized workspace is supplied by
// the server rather than accepted from the cognition request.
func projectContinuousCognitionActivation(
	store *frontierT6AgentFitStore,
	authorizedWorkspaceID string,
	currentAuthorizationDigest string,
	request continuousCognitionRequest,
	asOf time.Time,
) continuousCognitionActivation {
	activation := continuousCognitionDefaultActivation()
	if asOf.IsZero() {
		activation.State = "as_of_required"
		return continuousCognitionFinalizeActivation(activation)
	}
	if store == nil {
		activation.State = "unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	currentAuthorizationDigest = strings.ToLower(strings.TrimSpace(currentAuthorizationDigest))
	if !frontierT6ValidDigest(currentAuthorizationDigest) {
		activation.State = "authorization_unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	scope, err := frontierT6NormalizeContextPrepScope(frontierT6Scope{
		WorkspaceID: authorizedWorkspaceID,
		Project:     request.Project,
		SessionID:   request.SessionID,
		AgentID:     request.AgentID,
	})
	if err != nil || strings.TrimSpace(request.TaskID) == "" {
		activation.State = "identity_incomplete"
		return continuousCognitionFinalizeActivation(activation)
	}
	taskID, err := frontierT6NormalizeID(request.TaskID, "task_id", 160)
	if err != nil {
		activation.State = "identity_invalid"
		return continuousCognitionFinalizeActivation(activation)
	}

	store.mu.RLock()
	enabled := store.enabled
	var newestCandidate *frontierT6ContextPrepRecord
	var newestAuthorizedCandidate *frontierT6ContextPrepRecord
	newer := func(candidate frontierT6ContextPrepRecord, current *frontierT6ContextPrepRecord) bool {
		return current == nil || candidate.CreatedAt > current.CreatedAt ||
			(candidate.CreatedAt == current.CreatedAt && candidate.PrepID > current.PrepID)
	}
	for _, candidate := range store.state.ContextPreps {
		if candidate.Scope != scope || candidate.TaskID != taskID {
			continue
		}
		createdAt, ok := frontierT6ParseTime(candidate.CreatedAt)
		if !ok || createdAt.After(asOf.UTC()) {
			continue
		}
		candidate = frontierT6CopyContextPrepRecord(candidate)
		if newer(candidate, newestCandidate) {
			copyCandidate := candidate
			newestCandidate = &copyCandidate
		}
		if strings.EqualFold(strings.TrimSpace(candidate.AuthorizationDigest), currentAuthorizationDigest) && newer(candidate, newestAuthorizedCandidate) {
			copyCandidate := candidate
			newestAuthorizedCandidate = &copyCandidate
		}
	}
	store.mu.RUnlock()
	if !enabled {
		activation.State = "unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	if newestCandidate == nil {
		activation.State = "absent"
		return continuousCognitionFinalizeActivation(activation)
	}
	selected := *newestCandidate
	authorizationCurrent := newestAuthorizedCandidate != nil
	if authorizationCurrent {
		selected = *newestAuthorizedCandidate
	}
	activation.Persisted = true
	activation.PrepID = continuousCognitionOpaqueRef("prep", selected.PrepID)
	activation.ApprovalRef = continuousCognitionOpaqueRef("approval", selected.ApprovalDigest)
	activation.AuthorizationRef = continuousCognitionOpaqueRef("authorization", selected.AuthorizationDigest)
	if frontierT6ValidDigest(selected.ConsumptionDigest) {
		activation.ConsumptionRef = continuousCognitionOpaqueRef("consumption", selected.ConsumptionDigest)
	}
	updatedAt, updatedOK := frontierT6ParseTime(selected.UpdatedAt)
	if !updatedOK {
		activation.State = "state_invalid"
		return continuousCognitionFinalizeActivation(activation)
	}
	if updatedAt.After(asOf.UTC()) {
		activation.State = "temporal_projection_unavailable"
		return continuousCognitionFinalizeActivation(activation)
	}
	if !authorizationCurrent {
		activation.State = "authorization_changed"
		return continuousCognitionFinalizeActivation(activation)
	}
	activation.State = selected.Status
	if selected.Status != "consumed" && selected.Status != "failed" && selected.Status != "canceled" {
		if expiresAt, ok := frontierT6ParseTime(selected.ExpiresAt); !ok || !asOf.UTC().Before(expiresAt) {
			activation.State = "expired"
		} else if selected.Status == "ready" && selected.Artifact != nil {
			if artifactExpires, ok := frontierT6ParseTime(selected.Artifact.ExpiresAt); !ok || !asOf.UTC().Before(artifactExpires) {
				activation.State = "expired"
			}
		}
	}
	return continuousCognitionFinalizeActivation(activation)
}

func applyContinuousCognitionActivation(observation *continuousCognitionObservation, activation continuousCognitionActivation) {
	if observation == nil {
		return
	}
	observation.ActivationRef = activation.ProjectionRef
	observation.ActivationState = activation.State
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(*observation)
}

func snapshotContinuousCognitionWithProof(s *server, request continuousCognitionRequest, asOf time.Time) (continuousCognitionObservation, agentProofTimelineSnapshot) {
	return snapshotContinuousCognitionWithProofForVisibility(s, request, asOf, nil)
}

func snapshotContinuousCognitionWithProofForVisibility(
	s *server,
	request continuousCognitionRequest,
	asOf time.Time,
	sessionVisible func(map[string]any) bool,
) (continuousCognitionObservation, agentProofTimelineSnapshot) {
	var proofSnapshot agentProofTimelineSnapshot
	if asOf.IsZero() {
		asOf = request.AsOf
	}
	if !asOf.IsZero() {
		request.AsOf = asOf.UTC()
		asOf = request.AsOf
	}
	observation := continuousCognitionObservation{
		Scope:              continuousCognitionScopeFromRequest(request),
		ObjectiveGraphRef:  continuousCognitionUnavailableRef("objective_graph"),
		ObjectiveState:     "unknown",
		SessionRollupRef:   continuousCognitionUnavailableRef("session_rollup"),
		ContinuityZeroRef:  continuousCognitionUnavailableRef("continuity_zero_not_requested"),
		ProofTimelineRef:   continuousCognitionUnavailableRef("proof_timeline"),
		ProofStatus:        "unavailable",
		RetrievalPlanRef:   continuousCognitionUnavailableRef("retrieval_plan"),
		InvestigationRef:   continuousCognitionUnavailableRef("investigation"),
		InvestigationProof: continuousCognitionUnavailableRef("investigation_receipt"),
		UtilitySnapshotRef: continuousCognitionUnavailableRef("utility_snapshot"),
		UtilityStatus:      "unavailable",
		ActivationRef:      continuousCognitionUnavailableRef("activation"),
		ActivationState:    "not_requested",
		LifecycleProofRef:  continuousCognitionUnavailableRef("lifecycle_proof"),
		ExpectedUtility:    continuousCognitionExpectedUtility{},
		ProofAnchorDigest:  continuousCognitionUnavailableRef("proof_anchor"),
	}
	if asOf.IsZero() {
		continuousCognitionAddGap(&observation, "as_of_required", "snapshot", true)
		observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
		observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
		return observation, proofSnapshot
	}
	if s == nil {
		continuousCognitionAddGap(&observation, "server_unavailable", "snapshot", true)
		observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
		observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
		return observation, proofSnapshot
	}
	if strings.TrimSpace(request.ObjectiveID) == "" {
		continuousCognitionAddGap(&observation, "objective_id_required", "objective_graph", true)
	} else if s.continuity == nil {
		continuousCognitionAddGap(&observation, "objective_graph_unavailable", "objective_graph", true)
	} else {
		graph := s.continuity.objectiveGraph(request.Project, request.ObjectiveID, asOf.UTC(), false, request.Limit)
		state, graphRef, available, complete := continuousCognitionObjectiveProjection(graph, request.ObjectiveID)
		observation.ObjectiveState = state
		observation.ObjectiveGraphRef = graphRef
		observation.ObjectiveAvailable = available
		observation.ObjectiveTerminal = continuousCognitionTerminalState(state)
		if !available {
			continuousCognitionAddGap(&observation, "objective_not_found", "objective_graph", true)
		}
		if !complete {
			continuousCognitionAddGap(&observation, "objective_graph_incomplete", "objective_graph", true)
		}
	}
	if strings.TrimSpace(request.SessionID) == "" {
		continuousCognitionAddGap(&observation, "session_id_required", "agent_session", true)
	} else {
		session, events, found, temporalComplete := continuousCognitionSessionAtVisible(s.agentSessions, request.SessionID, asOf, sessionVisible)
		if !found {
			continuousCognitionAddGap(&observation, "session_not_found", "agent_session", true)
		} else {
			observation.SessionPresent = true
			observation.SessionAmbiguous = !temporalComplete
			observation.SessionRollupRef = continuousCognitionSessionProjection(session, events, asOf)
			proofSnapshot = continuousCognitionCaptureProofSnapshot(s, session, events)
			var temporalOmitted int
			proofSnapshot, temporalOmitted = continuousCognitionProofSnapshotAt(proofSnapshot, asOf)
			proofRef, proofStatus, proofComplete, anchorDigest := continuousCognitionProofProjectionFromSnapshot(proofSnapshot)
			observation.ProofTimelineRef = proofRef
			observation.ProofStatus = proofStatus
			observation.ProofComplete = proofComplete
			observation.ProofAnchorDigest = anchorDigest
			if proofStatus == "unavailable" {
				continuousCognitionAddGap(&observation, "proof_timeline_unavailable", "proof_timeline", true)
			}
			if !proofComplete {
				continuousCognitionAddGap(&observation, "proof_timeline_incomplete", "proof_timeline", true)
			}
			if !temporalComplete || temporalOmitted > 0 {
				continuousCognitionAddGap(&observation, "proof_temporal_projection_incomplete", "proof_timeline", true)
			}
		}
	}
	if retrievalRef, available := continuousCognitionRetrievalProjection(s, request); available {
		observation.RetrievalPlanRef = retrievalRef
	} else {
		continuousCognitionAddGap(&observation, "retrieval_plan_unavailable", "retrieval_plan", false)
	}
	utilityRef, utilityStatus, utilityVerified, utilityScore, expected, utilityAvailable := continuousCognitionUtilityProjection(s, request)
	observation.UtilitySnapshotRef = utilityRef
	observation.UtilityStatus = utilityStatus
	observation.UtilityVerified = utilityVerified
	observation.UtilityScore = utilityScore
	observation.ExpectedUtility = expected
	if !utilityAvailable {
		continuousCognitionAddGap(&observation, "utility_observation_unavailable", "utility", false)
	}
	observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
	observation.SourceComplete = continuousCognitionSourceIsComplete(observation)
	return observation, proofSnapshot
}

func snapshotContinuousCognition(s *server, request continuousCognitionRequest, asOf time.Time) continuousCognitionObservation {
	observation, _ := snapshotContinuousCognitionWithProof(s, request, asOf)
	return observation
}
