package main

import (
	"context"
	"sort"
	"strings"
	"time"
)

// recallResponseServerObservationCaptureKey is an unexported context key used
// only by the context-pack compiler. Raw retrieval routes never install it, so
// a caller cannot make executeRetrieval attach the private observation carrier
// by adding a request field with a matching name.
type recallResponseServerObservationCaptureKey struct{}

func withRecallResponseServerObservationCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, recallResponseServerObservationCaptureKey{}, true)
}

func recallResponseServerObservationCaptureEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, _ := ctx.Value(recallResponseServerObservationCaptureKey{}).(bool)
	return enabled
}

// recallResponseServerSilence is the narrow bridge between the server-owned
// proactive observation and the recall-response projection. The response
// route never accepts this observation as a request field; it can only arrive
// from an already materialized server context-pack/protocol envelope. This
// keeps U6's decision authority on the server side while preserving the v1
// response for callers that do not have a proactive observation.
func recallResponseServerSilence(input map[string]any) (continuousCognitionSilence, bool) {
	observation, present := recallResponseServerObservation(input)
	if !present {
		return continuousCognitionSilenceDefaults(), false
	}
	request := recallResponseSilenceRequest(input, observation)
	projected := recallResponseSilenceObservation(request, observation)
	frontier := computeContinuousCognitionFrontier(projected, continuousCognitionFrontierPolicy{
		MaxRounds: 3, InvestigateThreshold: 0.55, ContinueThreshold: 0.70, ConsequenceHighThreshold: 0.70,
	})
	policySuppressed := !anyToBoolOrDefault(observation["policy_allowed"], true) || anyToBool(observation["policy_suppressed"])
	return decideContinuousCognitionSilence(request, projected, frontier, policySuppressed), true
}

func recallResponseServerObservation(input map[string]any) (map[string]any, bool) {
	// `_server_proactive_observation` is installed by the context-pack
	// compilation seam from the already materialized retrieval response. The
	// `_u9_protocol` envelope remains test/evaluation-only. The context-pack
	// envelopes are accepted only after server compilation; the production
	// recall request allowlist excludes all of these keys.
	candidates := []map[string]any{
		anyMap(input["_server_proactive_observation"]),
		anyMap(anyMap(input["_u9_protocol"])["proactive_observation"]),
		anyMap(anyMap(input["context_pack"])["proactive_observation"]),
		anyMap(anyMap(input["contextPack"])["proactive_observation"]),
		anyMap(anyMap(input["context_pack"])["continuous_cognition"]),
	}
	for _, candidate := range candidates {
		projected := recallResponseProjectServerObservation(candidate)
		if recallResponseServerObservationPresent(projected) {
			return projected, true
		}
	}
	return nil, false
}

// recallResponseServerObservationFromCompilation is deliberately fed only
// server-owned compilation inputs. It gives the production recall hook a
// stable, internal carrier for U6's observation without widening the caller
// request allowlist or publishing the observation in the response.
func recallResponseServerObservationFromCompilation(
	input contextPackCompilationInput,
	contextPack map[string]any,
	compiled map[string]any,
) map[string]any {
	for _, owner := range []map[string]any{input.SearchResponse, contextPack, compiled} {
		for _, key := range []string{"_server_proactive_observation", "proactive_observation", "continuous_cognition", "server_proactive_observation"} {
			candidate := recallResponseProjectServerObservation(anyMap(owner[key]))
			if recallResponseServerObservationPresent(candidate) {
				return candidate
			}
		}
	}
	return nil
}

// recallResponseServerObservationFromBackendPayload is the retrieval-boundary
// adapter. The backend may carry this server-owned envelope at the top level;
// only the recognized observation object is forwarded, never the raw payload.
func recallResponseServerObservationFromBackendPayload(payload map[string]any) map[string]any {
	for _, key := range []string{"proactive_observation", "continuous_cognition", "server_proactive_observation"} {
		candidate := recallResponseProjectServerObservation(anyMap(payload[key]))
		if recallResponseServerObservationPresent(candidate) {
			return candidate
		}
	}
	return nil
}

// recallResponseProjectServerObservation is a closed, value-free projection
// of the server envelope. Unknown fields, raw values, instructions, and
// authority/execution claims are discarded before the envelope crosses any
// retrieval or composition boundary.
func recallResponseProjectServerObservation(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	projected := map[string]any{}
	for _, key := range []string{
		"terminal", "objective_terminal", "duplicate", "duplicate_cycle",
		"identity_present", "identity_complete", "identity_incomplete", "incomplete",
		"policy_allowed", "policy_suppressed", "material_new_proof", "blocked_next_action",
		"supported_high_impact_conflict", "actionable_gap", "utility_verified",
	} {
		if value, present := source[key]; present {
			projected[key] = anyToBool(value)
		}
	}
	if valueInputs := anyMap(source["value_inputs"]); len(valueInputs) > 0 {
		inputs := map[string]any{}
		for _, key := range []string{
			"material_new_proof", "blocked_next_action", "supported_high_impact_conflict",
			"actionable_gap", "utility_verified", "source_complete", "proof_complete",
		} {
			if value, present := valueInputs[key]; present {
				inputs[key] = anyToBool(value)
			}
		}
		if status := strings.TrimSpace(anyToString(valueInputs["utility_status"])); status != "" {
			inputs["utility_status"] = clipText(status, 64)
		}
		if len(inputs) > 0 {
			projected["value_inputs"] = inputs
		}
	}
	if status := strings.TrimSpace(anyToString(source["utility_status"])); status != "" {
		projected["utility_status"] = clipText(status, 64)
	}
	if value, present := source["utility_snapshot_ref"]; present {
		ref := strings.TrimSpace(anyToString(value))
		// A utility snapshot is an authority-bearing proof reference. Preserve
		// it only when the server supplied the canonical digest form; hashing an
		// arbitrary value here would turn unverified input into apparent proof.
		if recallResponseValidDigest(ref) {
			projected["utility_snapshot_ref"] = ref
		}
	}
	return projected
}

func recallResponseAllowedUtilityStatus(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "verified", "verified_exact":
		return true
	default:
		return false
	}
}

func mergeRecallResponseServerObservations(
	target map[string]map[string]any,
	delta map[string]map[string]any,
) {
	if target == nil {
		return
	}
	for source, observation := range delta {
		projected := recallResponseProjectServerObservation(observation)
		if recallResponseServerObservationPresent(projected) {
			target[source] = projected
		}
	}
}

func recallResponseFirstServerObservation(observations map[string]map[string]any) map[string]any {
	if len(observations) == 0 {
		return nil
	}
	sources := make([]string, 0, len(observations))
	for source := range observations {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		if observation := recallResponseProjectServerObservation(observations[source]); recallResponseServerObservationPresent(observation) {
			return observation
		}
	}
	return nil
}

func recallResponseServerObservationPresent(value map[string]any) bool {
	if len(value) == 0 {
		return false
	}
	for _, key := range []string{
		"terminal", "objective_terminal", "duplicate", "duplicate_cycle",
		"identity_present", "identity_complete", "identity_incomplete", "incomplete",
		"policy_allowed", "policy_suppressed", "value_inputs", "material_new_proof",
		"blocked_next_action", "supported_high_impact_conflict", "actionable_gap",
		"utility_verified", "utility_snapshot_ref",
	} {
		if _, present := value[key]; present {
			return true
		}
	}
	return false
}

func recallResponseSilenceRequest(input, observation map[string]any) continuousCognitionRequest {
	request := continuousCognitionRequest{
		Operation:       continuousCognitionOperationObserve,
		Query:           strings.TrimSpace(anyToString(input["query"])),
		Project:         strings.TrimSpace(anyToString(input["project"])),
		WorkspaceRef:    strings.TrimSpace(firstNonEmptyStrings(anyToString(input["workspace_ref"]), anyToString(input["workspaceRef"]))),
		TopicPath:       strings.Trim(strings.TrimSpace(anyToString(input["topic_path"])), "/"),
		RetrievalIntent: recallResponseSafeMode(firstNonEmptyStrings(anyToString(input["retrieval_intent"]), "decision"), "decision"),
		RetrievalMode:   recallResponseSafeRetrievalMode(anyToString(input["retrieval_mode"])),
		AgentID:         strings.TrimSpace(firstNonEmptyStrings(anyToString(input["agent_id"]), anyToString(input["agentId"]))),
		SessionID:       strings.TrimSpace(firstNonEmptyStrings(anyToString(input["session_id"]), anyToString(input["sessionId"]))),
		TaskID:          strings.TrimSpace(firstNonEmptyStrings(anyToString(input["task_id"]), anyToString(input["taskId"]))),
		TaskIdentityID:  strings.TrimSpace(firstNonEmptyStrings(anyToString(input["task_identity_id"]), anyToString(input["taskIdentityId"]))),
		ExecutionLaneID: strings.TrimSpace(firstNonEmptyStrings(anyToString(input["execution_lane_id"]), anyToString(input["executionLaneId"]))),
	}
	if raw := firstNonEmptyStrings(anyToString(input["as_of"]), anyToString(input["asOf"])); raw != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			request.AsOf = parsed.UTC()
		}
	}
	// `identity_present` is only an observation about the server's state. It
	// cannot manufacture the request's concrete, server-bound scope IDs. U6
	// must see every required ID in the actual composition scope or return
	// missing_identity, even when the observation claims completeness.
	return request
}

func recallResponseSilenceObservation(request continuousCognitionRequest, source map[string]any) continuousCognitionObservation {
	observation := continuousCognitionObservation{
		Scope:              continuousCognitionScopeFromRequest(request),
		ObjectiveState:     "active",
		ProofStatus:        "unavailable",
		UtilityStatus:      "not_observed",
		UtilityVerified:    false,
		UtilitySnapshotRef: continuousCognitionUnavailableRef("utility_snapshot"),
		ProofAnchorDigest:  continuousCognitionDigestPrefix("ref_proof_anchor_", source),
		SourceAnchorDigest: continuousCognitionDigestPrefix("ref_source_anchor_", source),
	}
	valueInputs := anyMap(source["value_inputs"])
	if len(valueInputs) == 0 {
		valueInputs = source
	}
	boolInput := func(key string) bool {
		return anyToBool(valueInputs[key])
	}
	materialNewProof := boolInput("material_new_proof")
	blockedNextAction := boolInput("blocked_next_action")
	supportedConflict := boolInput("supported_high_impact_conflict")
	actionableGap := boolInput("actionable_gap")
	if materialNewProof {
		observation.SourceComplete = true
		observation.ProofComplete = true
	}
	if blockedNextAction {
		observation.ObjectiveState = "blocked"
	}
	if supportedConflict {
		observation.ExpectedUtility.ConsequenceIfWrong = 0.8
		observation.Gaps = append(observation.Gaps, continuousCognitionGap{
			Code: "source_conflict", Source: "server_observation", Material: true,
			DetailRef: continuousCognitionDigestPrefix("ref_conflict_", source),
		})
	}
	if actionableGap {
		observation.Gaps = append(observation.Gaps, continuousCognitionGap{
			Code: "actionable_gap", Source: "server_observation", Material: true,
			DetailRef: continuousCognitionDigestPrefix("ref_gap_", source),
		})
	}
	if raw, present := source["utility_verified"]; present {
		observation.UtilityVerified = anyToBool(raw)
	}
	if raw, present := valueInputs["utility_verified"]; present {
		observation.UtilityVerified = anyToBool(raw)
	}
	for _, utilityStatus := range []string{
		strings.TrimSpace(anyToString(valueInputs["utility_status"])),
		strings.TrimSpace(anyToString(source["utility_status"])),
	} {
		if recallResponseAllowedUtilityStatus(utilityStatus) {
			observation.UtilityStatus = strings.ToLower(utilityStatus)
			break
		}
	}
	if raw, present := source["utility_snapshot_ref"]; present {
		ref := strings.TrimSpace(anyToString(raw))
		if recallResponseValidDigest(ref) {
			observation.UtilitySnapshotRef = ref
		}
	}
	if anyToBool(source["terminal"]) || anyToBool(source["objective_terminal"]) {
		observation.ObjectiveTerminal = true
		observation.ObjectiveState = "completed"
	}
	if anyToBool(source["duplicate"]) || anyToBool(source["duplicate_cycle"]) {
		observation.Gaps = append(observation.Gaps, continuousCognitionGap{
			Code: "duplicate_cycle", Source: "server_observation", Material: true,
			DetailRef: continuousCognitionDigestPrefix("ref_duplicate_", source),
		})
	}
	if !anyToBool(source["identity_present"]) && (source["identity_present"] != nil || anyToBool(source["identity_incomplete"]) || anyToBool(source["incomplete"])) {
		request.TaskIdentityID = ""
		observation.Scope = continuousCognitionScopeFromRequest(request)
	}
	if anyToBool(source["identity_present"]) || anyToBool(source["identity_complete"]) {
		observation.Scope = continuousCognitionScopeFromRequest(request)
	}
	observation.Gaps = continuousCognitionNormalizeGaps(observation.Gaps)
	observation.SourceComplete = observation.SourceComplete || materialNewProof
	observation.SourceAnchorDigest = continuousCognitionCompositeSourceAnchorDigest(observation)
	return observation
}

// recallResponseApplyServerSilence changes only the existing v1 projection
// fields plus a closed state.silence object. A missing observation is a
// deliberate legacy path: old callers retain byte-compatible v1 behavior.
func recallResponseApplyServerSilence(response map[string]any, decision continuousCognitionSilence, observed bool) bool {
	if !observed {
		return false
	}
	state := anyMap(response["state"])
	state["silence"] = continuousCognitionSilenceMap(decision)
	state["silenced"] = decision.Reason != "not_silenced"
	if decision.Reason == "not_silenced" {
		return false
	}
	state["status"] = "abstain"
	classification := anyMap(response["classification"])
	classification["posture"] = "abstain"
	answer := anyMap(response["answer"])
	answer["answer_mode"] = "abstention"
	answer["summary"] = "The server-derived cognition policy found no safe high-value next action; no dispatch, mutation, or writeback is required."
	nextAction := anyMap(response["next_action"])
	nextAction["kind"] = "none"
	nextAction["label"] = "No action"
	nextAction["reason"] = "The server-derived silence decision closed the action boundary."
	nextAction["requires_verification"] = false
	actionBoundary := anyMap(response["action_boundary"])
	actionBoundary["reason"] = "The server-derived silence decision forbids dispatch and external mutation."
	outcome := anyMap(response["outcome"])
	outcome["status"] = "not_attributable"
	outcome["attributable"] = false
	outcome["receipt_id"] = ""
	response["writeback_required"] = false
	return true
}

func recallResponseServerSilenced(response map[string]any) bool {
	state := anyMap(response["state"])
	return anyToBool(state["silenced"]) && len(anyMap(state["silence"])) > 0
}

// recallResponseProjectFallbackWithServerSilence is used by route-level
// fallback paths that project directly from compiled artifacts. Those paths
// do not pass through composeRecallResponseWithPolicy, so they must explicitly
// carry the same server observation into the closed v1 control response.
func recallResponseProjectFallbackWithServerSilence(input map[string]any, policy validatedRecallResponsePolicyInput) map[string]any {
	response := projectRecallResponseV1ControlFromArtifacts(input, policy)
	silence, observed := recallResponseServerSilence(input)
	recallResponseApplyServerSilence(response, silence, observed)
	return response
}
