package main

import (
	"strings"
)

const (
	recallResponseInitialContractID = "recall_response.initial.v1"
	recallResponseInitialShape      = "initial_compact"
	// The strongest qualified U9 control is 2,173 bytes / 544 chars-div-four
	// tokens. Integer limits deliberately use floor(baseline*110/100): a
	// fractional byte or token cannot expand the production transport budget.
	recallResponseInitialMaxBytes  = 2390
	recallResponseInitialMaxTokens = 598
)

type recallResponseInitialContinuationPlan struct {
	action       map[string]any
	record       recallResponseContinuationRecord
	token        string
	replaceToken string
	discardToken string
	terminal     bool
}

func recallResponseInitialSourceCandidateValid(response map[string]any) bool {
	if response == nil || !validateRecallResponseU2(response) ||
		anyToString(response["response_id"]) != recallResponseIDForResponse(response) ||
		anyToString(response["response_digest"]) != recallResponseSemanticDigest(response) {
		return false
	}
	if recallResponseIsV1Control(response) {
		return true
	}
	_, ok := recallResponseBindingFromResponse(response)
	return ok
}

func recallResponseInitialSourceIdentity(response map[string]any) (map[string]any, bool) {
	if response == nil {
		return nil, false
	}
	stable := cloneJSONMap(response)
	scopeDigest := anyToString(anyMap(stable["request_scope"])["scope_digest"])
	if !recallResponseValidDigest(scopeDigest) {
		return nil, false
	}
	recallResponseSetContinuationAction(stable, recallResponseUnavailableContinuationAction(scopeDigest))
	disclosure := recallResponseDisclosure(stable)
	identity := map[string]any{
		"response_id":     stable["response_id"],
		"response_digest": stable["response_digest"],
		"union_digest":    disclosure["union_digest"],
	}
	if !recallResponseExactOpaqueID(anyToString(identity["response_id"]), "rr_") ||
		!recallResponseValidDigest(anyToString(identity["response_digest"])) ||
		!recallResponseValidDigest(anyToString(identity["union_digest"])) ||
		anyToString(identity["response_id"]) != recallResponseIDForResponse(stable) ||
		anyToString(identity["response_digest"]) != recallResponseSemanticDigest(stable) {
		return nil, false
	}
	return identity, true
}

func recallResponseInitialRefs(value any, limit int) ([]any, bool) {
	rows, ok := value.([]any)
	if !ok || len(rows) > limit {
		return nil, false
	}
	out := make([]any, 0, len(rows))
	seen := map[string]bool{}
	for _, raw := range rows {
		ref := strings.TrimSpace(anyToString(raw))
		if ref == "" || seen[ref] {
			return nil, false
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out, true
}

func recallResponseInitialPrimary(response map[string]any) (map[string]any, string, bool) {
	answer := anyMap(response["answer"])
	composition := anyMap(answer["composition"])
	proof := anyMap(answer["proof_spine"])
	module := anyToString(composition["primary_module"])
	if module == "" {
		return nil, "", false
	}
	proofRefs, ok := recallResponseInitialRefs(proof["proof_refs"], recallResponseMaxProofRefs)
	if !ok {
		return nil, "", false
	}
	componentRef, componentDigest := "", ""
	for _, raw := range contextPackAnyList(answer["components"]) {
		component := anyMap(raw)
		if anyToString(component["kind"]) != module {
			continue
		}
		componentRef = anyToString(component["component_ref"])
		componentDigest = anyToString(component["component_digest"])
		break
	}
	if module != "v1_control" && (!recallResponseExactOpaqueID(componentRef, "rrc_") || !recallResponseValidDigest(componentDigest)) {
		return nil, "", false
	}
	if module == "v1_control" && (componentRef != "" || componentDigest != "") {
		return nil, "", false
	}
	return map[string]any{
		"summary":          answer["summary"],
		"answer_mode":      answer["answer_mode"],
		"module":           module,
		"component_ref":    componentRef,
		"component_digest": componentDigest,
		"proof_count":      len(proofRefs),
		"proof_digest":     "sha256:" + sha256Hex(recallResponseCanonicalJSON(proofRefs)),
	}, componentRef, true
}

func recallResponseInitialSafety(response, composition map[string]any, membership recallResponseContinuationMembershipSet) (map[string]any, []any, []any, bool) {
	proof := anyMap(anyMap(response["answer"])["proof_spine"])
	conflictRefs, ok := recallResponseInitialRefs(proof["conflict_refs"], recallResponseMaxProofRefs)
	if !ok {
		return nil, nil, nil, false
	}
	gapRefs, ok := recallResponseInitialRefs(proof["gap_refs"], recallResponseMaxProofRefs)
	if !ok {
		return nil, nil, nil, false
	}
	hardExclusions, protected := 0, 0
	for _, raw := range membership.All {
		row := anyMap(raw)
		if anyToString(row["disposition"]) == "hard_excluded" {
			hardExclusions++
		}
		if anyToBool(row["protected"]) {
			protected++
		}
	}
	state := anyMap(response["state"])
	silence := anyMap(state["silence"])
	silenceObserved := len(silence) > 0
	silenced := false
	silenceReason := "not_observed"
	silenceObservationDigest := ""
	if silenceObserved {
		observation, observationPresent := recallResponseServerObservation(composition)
		expectedSilence, expectedSilenceObserved := recallResponseServerSilence(composition)
		expectedDecision := continuousCognitionSilenceMap(expectedSilence)
		if !observationPresent || !expectedSilenceObserved ||
			recallResponseCanonicalJSON(silence) != recallResponseCanonicalJSON(expectedDecision) {
			return nil, nil, nil, false
		}
		silenced = anyToBool(state["silenced"])
		silenceReason = anyToString(silence["reason"])
		silenceObservationDigest = "sha256:" + sha256Hex(recallResponseCanonicalJSON(map[string]any{
			"observation": observation,
			"decision":    expectedDecision,
		}))
	} else if _, observationPresent := recallResponseServerObservation(composition); observationPresent {
		return nil, nil, nil, false
	}
	material := map[string]any{
		"silence_observed":           silenceObserved,
		"silenced":                   silenced,
		"silence_reason":             silenceReason,
		"silence_observation_digest": silenceObservationDigest,
		"conflict_refs":              conflictRefs,
		"gap_refs":                   gapRefs,
		"hard_exclusion_count":       hardExclusions,
		"protected_count":            protected,
	}
	return material, conflictRefs, gapRefs, true
}

func recallResponseInitialVisibleKeys(componentRef string, conflictRefs, gapRefs []any) map[string]bool {
	visible := map[string]bool{}
	if componentRef != "" {
		visible[recallResponseTypedItemKey("component", componentRef)] = true
	}
	for _, raw := range conflictRefs {
		visible[recallResponseTypedItemKey("conflict", anyToString(raw))] = true
	}
	for _, raw := range gapRefs {
		visible[recallResponseTypedItemKey("proof", anyToString(raw))] = true
	}
	return visible
}

func recallResponseInitialPartition(membership recallResponseContinuationMembershipSet, visible map[string]bool) recallResponseContinuationMembershipSet {
	omitted := make([]any, 0, len(membership.All))
	for _, raw := range membership.All {
		row := anyMap(raw)
		key := recallResponseTypedItemKey(anyToString(row["item_type"]), anyToString(row["item_ref"]))
		if !visible[key] {
			omitted = append(omitted, cloneJSONValue(raw))
		}
	}
	return recallResponseContinuationMembershipSet{
		All: cloneJSONValue(membership.All).([]any), Omitted: omitted, Digest: membership.Digest,
	}
}

func (s *server) planRecallResponseInitialContinuation(
	response, request map[string]any,
	membership recallResponseContinuationMembershipSet,
	agentID, endpoint string,
) (recallResponseInitialContinuationPlan, bool) {
	if s == nil || !recallResponseOneOf(endpoint, memoryRecallResponsePath, toolsRecallResponsePath) {
		return recallResponseInitialContinuationPlan{}, false
	}
	scope := anyMap(response["request_scope"])
	scopeDigest := anyToString(scope["scope_digest"])
	snapshotDigest := anyToString(scope["snapshot_digest"])
	requestDigest := recallResponseContinuationRequestDigest(request)
	continuationRef := anyToString(recallResponseDisclosure(response)["continuation_ref"])
	if !recallResponseValidDigest(scopeDigest) || !recallResponseValidDigest(snapshotDigest) ||
		!recallResponseValidDigest(requestDigest) || !recallResponseValidDigest(membership.Digest) ||
		!recallResponseExactOpaqueID(continuationRef, "ref_continuation_") {
		return recallResponseInitialContinuationPlan{}, false
	}
	existingToken := anyToString(anyMap(recallResponseDisclosure(response)["continuation_action"])["token"])
	if len(membership.Omitted) == 0 {
		return recallResponseInitialContinuationPlan{
			action: recallResponseTerminalContinuationAction(scopeDigest), discardToken: existingToken, terminal: true,
		}, true
	}
	now := s.recallResponseNow()
	if recallResponseValidContinuationToken(existingToken) {
		s.recallResponseContinuationMu.Lock()
		s.pruneExpiredRecallResponseContinuationsLocked(now)
		record, found := s.recallResponseContinuations[existingToken]
		s.recallResponseContinuationMu.Unlock()
		if !found || record.AgentID != agentID || record.Endpoint != endpoint || record.ScopeDigest != scopeDigest ||
			record.SnapshotDigest != snapshotDigest || record.RequestDigest != requestDigest ||
			record.SourceMembershipDigest != membership.Digest {
			return recallResponseInitialContinuationPlan{}, false
		}
		record.Items = cloneJSONValue(membership.Omitted).([]any)
		record.Offset = 0
		record.Page = 1
		record.ContinuationRef = continuationRef
		record.StoredBytes = recallResponseContinuationRecordBytes(record)
		return recallResponseInitialContinuationPlan{
			action: recallResponseContinuationAction(scopeDigest, requestDigest, endpoint, existingToken, 1),
			record: record, token: existingToken, replaceToken: existingToken,
		}, true
	}
	token, ok := recallResponseNewContinuationToken()
	if !ok {
		return recallResponseInitialContinuationPlan{}, false
	}
	record := recallResponseContinuationRecord{
		AgentID: agentID, Endpoint: endpoint, ScopeDigest: scopeDigest, SnapshotDigest: snapshotDigest,
		RequestDigest: requestDigest, SourceMembershipDigest: membership.Digest, ContinuationRef: continuationRef,
		Items: cloneJSONValue(membership.Omitted).([]any), Offset: 0, Page: 1,
		CreatedAt: now, ExpiresAt: now.Add(recallResponseContinuationTTL),
	}
	record.StoredBytes = recallResponseContinuationRecordBytes(record)
	return recallResponseInitialContinuationPlan{
		action: recallResponseContinuationAction(scopeDigest, requestDigest, endpoint, token, 1),
		record: record, token: token,
	}, true
}

func (s *server) applyRecallResponseInitialContinuationPlan(plan recallResponseInitialContinuationPlan) bool {
	if s == nil {
		return false
	}
	now := s.recallResponseNow()
	s.recallResponseContinuationMu.Lock()
	defer s.recallResponseContinuationMu.Unlock()
	s.pruneExpiredRecallResponseContinuationsLocked(now)
	if plan.terminal {
		if recallResponseValidContinuationToken(plan.discardToken) {
			delete(s.recallResponseContinuations, plan.discardToken)
			s.recountRecallResponseContinuationsLocked()
		}
		return true
	}
	if plan.replaceToken == "" {
		return s.admitRecallResponseContinuationLocked(plan.token, plan.record)
	}
	existing, found := s.recallResponseContinuations[plan.replaceToken]
	if !found || existing.AgentID != plan.record.AgentID || existing.Endpoint != plan.record.Endpoint ||
		existing.ScopeDigest != plan.record.ScopeDigest || existing.SnapshotDigest != plan.record.SnapshotDigest ||
		existing.RequestDigest != plan.record.RequestDigest || existing.SourceMembershipDigest != plan.record.SourceMembershipDigest {
		s.recallResponseContinuationStats.InvalidRequests++
		return false
	}
	agentRecords, agentItems, agentBytes := 0, 0, 0
	globalRecords, globalItems, globalBytes := 0, 0, 0
	for token, record := range s.recallResponseContinuations {
		if token == plan.replaceToken {
			continue
		}
		storedBytes := record.StoredBytes
		if storedBytes <= 0 {
			storedBytes = recallResponseContinuationRecordBytes(record)
		}
		globalRecords++
		globalItems += len(record.Items)
		globalBytes += storedBytes
		if record.AgentID == plan.record.AgentID {
			agentRecords++
			agentItems += len(record.Items)
			agentBytes += storedBytes
		}
	}
	if agentRecords+1 > recallResponseContinuationMaximumRecordsPerAgent ||
		agentItems+len(plan.record.Items) > recallResponseContinuationMaximumItemsPerAgent ||
		agentBytes+plan.record.StoredBytes > recallResponseContinuationMaximumBytesPerAgent {
		s.recallResponseContinuationStats.FairnessRejected++
		return false
	}
	if globalRecords+1 > recallResponseContinuationMaximumRecords ||
		globalItems+len(plan.record.Items) > recallResponseContinuationMaximumStoredItems ||
		globalBytes+plan.record.StoredBytes > recallResponseContinuationMaximumStoredBytes {
		s.recallResponseContinuationStats.CapacityRejected++
		return false
	}
	s.recallResponseContinuations[plan.replaceToken] = plan.record
	s.recountRecallResponseContinuationsLocked()
	return true
}

func (s *server) projectRecallResponseInitial(
	response, composition, request map[string]any,
	policy validatedRecallResponsePolicyInput,
	agentID, endpoint string,
) (map[string]any, bool) {
	initial, reason := s.projectRecallResponseInitialWithReason(response, composition, request, policy, agentID, endpoint)
	return initial, reason == ""
}

func (s *server) projectRecallResponseInitialWithReason(
	response, composition, request map[string]any,
	policy validatedRecallResponsePolicyInput,
	agentID, endpoint string,
) (map[string]any, string) {
	if !recallResponseInitialSourceCandidateValid(response) {
		return nil, "source_response_invalid"
	}
	if !anyToBool(anyMap(response["request_scope"])["source_bound"]) {
		return nil, "source_response_unbound"
	}
	identity, ok := recallResponseInitialSourceIdentity(response)
	if !ok {
		return nil, "source_identity_invalid"
	}
	membership, ok := recallResponseContinuationMembership(composition, response, policy)
	if !ok {
		return nil, "source_membership_invalid"
	}
	primary, componentRef, ok := recallResponseInitialPrimary(response)
	if !ok {
		return nil, "primary_invalid"
	}
	safety, conflictRefs, gapRefs, ok := recallResponseInitialSafety(response, composition, membership)
	if !ok {
		return nil, "safety_invalid"
	}
	membership = recallResponseInitialPartition(membership, recallResponseInitialVisibleKeys(componentRef, conflictRefs, gapRefs))
	plan, ok := s.planRecallResponseInitialContinuation(response, request, membership, agentID, endpoint)
	if !ok {
		return nil, "continuation_plan_unavailable"
	}
	scope := anyMap(response["request_scope"])
	initial := map[string]any{
		"ok":                     true,
		"schema_id":              recallResponseInitialContractID,
		"version":                1,
		"source_response_id":     identity["response_id"],
		"source_response_digest": identity["response_digest"],
		"scope": map[string]any{
			"scope_digest": scope["scope_digest"], "snapshot_digest": scope["snapshot_digest"],
			"receipt_digest": scope["receipt_digest"], "source_bound": scope["source_bound"],
		},
		"primary": primary,
		"safety":  safety,
		"union": map[string]any{
			"item_count": len(membership.All), "initial_item_count": len(membership.All) - len(membership.Omitted),
			"source_membership_digest": membership.Digest,
			"continuation_ref":         recallResponseDisclosure(response)["continuation_ref"],
			"union_digest":             identity["union_digest"],
		},
		"continuation_action": plan.action,
	}
	if !validateRecallResponseInitial(initial) {
		return nil, "initial_contract_invalid"
	}
	compactBytes, compactTokens := recallResponseCompactBudget(initial)
	if compactBytes > recallResponseInitialMaxBytes || compactTokens > recallResponseInitialMaxTokens {
		return nil, "initial_budget_exceeded:" + anyToString(compactBytes) + "/" + anyToString(compactTokens)
	}
	if !s.applyRecallResponseInitialContinuationPlan(plan) {
		return nil, "continuation_admission_rejected"
	}
	return initial, ""
}

func validateRecallResponseInitial(response map[string]any) bool {
	if !recallResponseExactFields(response, []string{
		"ok", "schema_id", "version", "source_response_id", "source_response_digest", "scope",
		"primary", "safety", "union", "continuation_action",
	}) || !anyToBool(response["ok"]) || anyToString(response["schema_id"]) != recallResponseInitialContractID ||
		anyToInt(response["version"], 0) != 1 || !recallResponseExactOpaqueID(anyToString(response["source_response_id"]), "rr_") ||
		!recallResponseValidDigest(anyToString(response["source_response_digest"])) {
		return false
	}
	scope := anyMap(response["scope"])
	if !recallResponseExactFields(scope, []string{"scope_digest", "snapshot_digest", "receipt_digest", "source_bound"}) ||
		!recallResponseValidDigest(anyToString(scope["scope_digest"])) || !recallResponseValidDigest(anyToString(scope["snapshot_digest"])) ||
		!recallResponseValidDigest(anyToString(scope["receipt_digest"])) || !anyToBool(scope["source_bound"]) {
		return false
	}
	primary := anyMap(response["primary"])
	if !recallResponseExactFields(primary, []string{
		"summary", "answer_mode", "module", "component_ref", "component_digest", "proof_count", "proof_digest",
	}) || strings.TrimSpace(anyToString(primary["summary"])) == "" || len([]byte(anyToString(primary["summary"]))) > 512 ||
		!recallResponseOneOf(anyToString(primary["answer_mode"]), "answer", "qualified_answer", "abstention") ||
		(!recallResponseModuleAllowed(anyToString(primary["module"])) && anyToString(primary["module"]) != "v1_control") ||
		anyToInt(primary["proof_count"], -1) < 0 || anyToInt(primary["proof_count"], -1) > recallResponseMaxProofRefs ||
		!recallResponseValidDigest(anyToString(primary["proof_digest"])) {
		return false
	}
	if anyToString(primary["module"]) == "v1_control" {
		if anyToString(primary["component_ref"]) != "" || anyToString(primary["component_digest"]) != "" {
			return false
		}
	} else if !recallResponseExactOpaqueID(anyToString(primary["component_ref"]), "rrc_") ||
		!recallResponseValidDigest(anyToString(primary["component_digest"])) {
		return false
	}
	safety := anyMap(response["safety"])
	if !recallResponseExactFields(safety, []string{
		"silence_observed", "silenced", "silence_reason", "silence_observation_digest",
		"conflict_refs", "gap_refs", "hard_exclusion_count", "protected_count",
	}) || !recallResponseOneOf(anyToString(safety["silence_reason"]), "not_observed", "not_silenced", "terminal", "duplicate", "low_utility", "missing_identity", "policy_suppressed") ||
		anyToInt(safety["hard_exclusion_count"], -1) < 0 || anyToInt(safety["protected_count"], -1) < 0 {
		return false
	}
	silenceObserved := anyToBool(safety["silence_observed"])
	if silenceObserved {
		if anyToString(safety["silence_reason"]) == "not_observed" ||
			anyToBool(safety["silenced"]) != (anyToString(safety["silence_reason"]) != "not_silenced") ||
			!recallResponseValidDigest(anyToString(safety["silence_observation_digest"])) {
			return false
		}
	} else if anyToBool(safety["silenced"]) || anyToString(safety["silence_reason"]) != "not_observed" ||
		anyToString(safety["silence_observation_digest"]) != "" {
		return false
	}
	if _, ok := recallResponseInitialRefs(safety["conflict_refs"], recallResponseMaxProofRefs); !ok {
		return false
	}
	if _, ok := recallResponseInitialRefs(safety["gap_refs"], recallResponseMaxProofRefs); !ok {
		return false
	}
	union := anyMap(response["union"])
	itemCount := anyToInt(union["item_count"], -1)
	initialItemCount := anyToInt(union["initial_item_count"], -1)
	if !recallResponseExactFields(union, []string{
		"item_count", "initial_item_count", "source_membership_digest", "continuation_ref", "union_digest",
	}) || itemCount < 0 || initialItemCount < 0 || initialItemCount > itemCount ||
		!recallResponseValidDigest(anyToString(union["source_membership_digest"])) ||
		!recallResponseExactOpaqueID(anyToString(union["continuation_ref"]), "ref_continuation_") ||
		!recallResponseValidDigest(anyToString(union["union_digest"])) {
		return false
	}
	action := anyMap(response["continuation_action"])
	fakeResponse := map[string]any{"request_scope": map[string]any{"scope_digest": scope["scope_digest"]}}
	if !recallResponseContinuationActionValid(fakeResponse, action) ||
		(anyToString(action["kind"]) == "terminal") != (initialItemCount == itemCount) ||
		!recallResponseOneOf(anyToString(action["kind"]), "continue_snapshot", "terminal") {
		return false
	}
	return true
}
