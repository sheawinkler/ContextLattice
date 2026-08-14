package main

import (
	"sort"
	"strings"
	"time"
)

const (
	recallResponseLatestAsOf    = "latest_available"
	recallResponseMaxProofRefs  = 8
	recallResponseProofStrategy = "shared_bounded_v1"
)

var recallResponseNowUTC = func() time.Time { return time.Now().UTC() }

func recallResponseNormalizeAsOf(value string) string {
	normalized, _ := recallResponseNormalizeAsOfWithValidity(value)
	return normalized
}

func recallResponseNormalizeAsOfWithValidity(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, recallResponseLatestAsOf) {
		return recallResponseLatestAsOf, true
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.After(recallResponseNowUTC().Add(time.Minute)) {
		return recallResponseLatestAsOf, false
	}
	return parsed.UTC().Format(time.RFC3339Nano), true
}

func recallResponseEvidenceAtOrBeforeWithGaps(items []any, asOf string) ([]any, int) {
	if asOf == recallResponseLatestAsOf {
		return append([]any(nil), items...), 0
	}
	cutoff, err := time.Parse(time.RFC3339Nano, asOf)
	if err != nil {
		return []any{}, len(items)
	}
	out := make([]any, 0, len(items))
	unverifiable := 0
	for _, raw := range items {
		item := anyMap(raw)
		timestamp := firstNonEmptyStrings(
			anyToString(item["as_of"]), anyToString(item["valid_at"]), anyToString(item["occurred_at"]), anyToString(item["observed_at"]),
			anyToString(item["updated_at"]), anyToString(item["created_at"]), anyToString(item["timestamp"]),
		)
		if timestamp == "" {
			unverifiable++
			continue
		}
		observed, parseErr := time.Parse(time.RFC3339Nano, timestamp)
		if parseErr != nil {
			unverifiable++
			continue
		}
		if !observed.After(cutoff) {
			out = append(out, raw)
		}
	}
	return out, unverifiable
}

func recallResponseTemporalPremise(asOf, temporalState string) map[string]any {
	premise := map[string]any{
		"as_of":          asOf,
		"temporal_state": temporalState,
		"selection_rule": "evidence_observed_at_or_before_as_of",
	}
	premise["digest"] = "sha256:" + sha256Hex(recallResponseCanonicalJSON(premise))
	return premise
}

func recallResponseSnapshotDigest(evidence, conflicts, gaps []any, temporalPremiseDigest string) string {
	material := map[string]any{
		"evidence":                cloneJSONValue(evidence),
		"conflicts":               cloneJSONValue(conflicts),
		"gaps":                    cloneJSONValue(gaps),
		"temporal_premise_digest": temporalPremiseDigest,
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseSnapshotArtifactDigest(
	input, contextPack, sourceCoverage map[string]any,
	rankedEvidence, evidence, conflicts, gaps []any,
	temporalPremiseDigest string,
) string {
	material := map[string]any{
		"ranked_evidence":              cloneJSONValue(rankedEvidence),
		"projected_evidence":           cloneJSONValue(evidence),
		"conflicts":                    cloneJSONValue(conflicts),
		"gaps":                         cloneJSONValue(gaps),
		"source_coverage":              cloneJSONMap(sourceCoverage),
		"source_revision_vector":       cloneJSONValue(contextPack["source_revision_vector"]),
		"snapshot_revision_start":      contextPack["snapshot_revision_start"],
		"snapshot_revision_end":        contextPack["snapshot_revision_end"],
		"snapshot_revision_changed":    anyToBool(input["_snapshot_revision_changed"]),
		"temporal_premise_digest":      temporalPremiseDigest,
		"historical_unverifiable_rows": anyToInt(input["_historical_unverifiable_evidence"], 0),
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseReceiptDigest(receiptRefs []any) string {
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(receiptRefs))
}

func recallResponseReceiptArtifactDigest(input, contextPack map[string]any, receiptRefs []any) string {
	material := map[string]any{
		"receipt_refs":             cloneJSONValue(receiptRefs),
		"memory_trust_assessment":  cloneJSONValue(input["memory_trust_assessment"]),
		"retrieval_decision_trace": cloneJSONValue(input["retrieval_decision_trace"]),
		"context_pack_quality":     cloneJSONValue(input["context_pack_quality"]),
		"durable_quality":          cloneJSONValue(input["_durable_context_pack_quality"]),
		"pack_quality":             cloneJSONValue(contextPack["context_pack_quality"]),
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseProofSpine(
	response map[string]any,
	asOf, temporalPremiseDigest, snapshotDigest, receiptDigest string,
	sources ...map[string]any,
) map[string]any {
	var source map[string]any
	if len(sources) > 0 {
		source = sources[0]
	}
	evidence := contextPackAnyList(response["evidence"])
	conflicts := contextPackAnyList(response["conflicts"])
	gaps := contextPackAnyList(response["gaps"])
	receipts := contextPackAnyList(response["receipt_refs"])

	proofRefs := []string{}
	addProof := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			proofRefs = appendRecallResponseString(proofRefs, value, recallResponseMaxProofRefs)
		}
	}
	evidenceRefs := make([]string, 0, len(evidence))
	for _, raw := range evidence {
		ref := strings.TrimSpace(anyToString(anyMap(raw)["ref_id"]))
		if ref != "" && !containsString(evidenceRefs, ref) {
			evidenceRefs = append(evidenceRefs, ref)
		}
	}
	primaryResult := ""
	components := contextPackAnyList(anyMap(response["answer"])["components"])
	if len(components) > 0 {
		primaryKind := strings.TrimSpace(anyToString(anyMap(components[0])["kind"]))
		preferred := recallResponseModuleRefs(primaryKind, response, evidenceRefs, source)
		if len(preferred) > 0 {
			primaryResult = preferred[0]
		}
	}
	if primaryResult == "" && len(evidenceRefs) > 0 {
		primaryResult = evidenceRefs[0]
	}
	addProof(primaryResult)
	conflictRefs := []string{}
	for _, raw := range conflicts {
		ref := anyToString(anyMap(raw)["conflict_id"])
		conflictRefs = appendRecallResponseString(conflictRefs, ref, recallResponseMaxProofRefs)
		addProof(ref)
	}
	gapRefs := []string{}
	scopeDigest := anyToString(anyMap(response["request_scope"])["scope_digest"])
	for _, raw := range gaps {
		gap := anyMap(raw)
		ref := recallResponseScopedOpaqueRef(scopeDigest, "gap", anyToString(gap["code"]))
		gapRefs = appendRecallResponseString(gapRefs, ref, recallResponseMaxProofRefs)
		addProof(ref)
	}
	for _, raw := range evidence {
		addProof(anyToString(anyMap(raw)["ref_id"]))
	}
	coveredRefs := func(refs []string) []any {
		covered := []string{}
		for _, ref := range refs {
			if containsString(proofRefs, ref) {
				covered = append(covered, ref)
			}
		}
		return recallResponseAnyStrings(covered)
	}

	coverage := []any{
		map[string]any{"obligation": "primary_result", "status": recallResponseCoverageStatus(primaryResult != ""), "proof_refs": recallResponseAnyStrings(proofRefs)},
		map[string]any{"obligation": "temporal_premise", "status": recallResponseCoverageStatus(recallResponseValidDigest(temporalPremiseDigest)), "proof_refs": []any{}},
		map[string]any{"obligation": "bounded_snapshot", "status": recallResponseCoverageStatus(recallResponseValidDigest(snapshotDigest)), "proof_refs": []any{}},
		map[string]any{"obligation": "conflict_free", "status": recallResponseCoverageStatus(len(conflicts) == 0), "proof_refs": coveredRefs(conflictRefs)},
		map[string]any{"obligation": "material_gaps_resolved", "status": recallResponseCoverageStatus(len(gaps) == 0), "proof_refs": coveredRefs(gapRefs)},
	}

	nextMove := anyToString(anyMap(response["next_action"])["kind"])
	confidenceBasis := contextPackAnyList(anyMap(response["confidence"])["basis"])
	return map[string]any{
		"primary_result":          primaryResult,
		"as_of":                   asOf,
		"temporal_premise_digest": temporalPremiseDigest,
		"proof_refs":              recallResponseAnyStrings(proofRefs),
		"confidence_basis":        cloneJSONValue(confidenceBasis),
		"conflict_refs":           recallResponseAnyStrings(conflictRefs),
		"gap_refs":                recallResponseAnyStrings(gapRefs),
		"memory_boundary":         "server_evidence_and_deterministic_inference_only",
		"next_move":               nextMove,
		"receipt_refs":            recallResponseProofReceiptIDs(receipts),
		"disclosure":              "opaque_refs_only",
		"coverage":                coverage,
	}
}

func recallResponseProofReceiptIDs(rows []any) []any {
	out := []string{}
	for _, raw := range rows {
		out = appendRecallResponseString(out, anyToString(anyMap(raw)["ref_id"]), recallResponseMaxProofRefs)
	}
	return recallResponseAnyStrings(out)
}

func recallResponseCoverageStatus(satisfied bool) string {
	if satisfied {
		return "satisfied"
	}
	return "unsatisfied"
}

func validateRecallResponseU2(response map[string]any) bool {
	if !validateRecallResponseNonExclusion(response) {
		return false
	}
	classification := anyMap(response["classification"])
	facets := anyMap(classification["facets"])
	answer := anyMap(response["answer"])
	proof := anyMap(answer["proof_spine"])
	composition := anyMap(answer["composition"])
	scope := anyMap(response["request_scope"])
	if !recallResponseExactFields(facets, []string{"jobs", "memory_objects", "temporal_state", "evidence_state", "consequence"}) ||
		!recallResponseExactFields(proof, []string{"primary_result", "as_of", "temporal_premise_digest", "proof_refs", "confidence_basis", "conflict_refs", "gap_refs", "memory_boundary", "next_move", "receipt_refs", "disclosure", "coverage"}) ||
		!recallResponseExactFields(composition, []string{"condition", "ablation", "primary_module", "ordered_modules", "proof_strategy", "coverage_status", "fallback_reason"}) {
		return false
	}
	if len(contextPackAnyList(facets["jobs"])) > recallResponseMaxFacetLabels || len(contextPackAnyList(facets["memory_objects"])) > recallResponseMaxFacetLabels ||
		len(contextPackAnyList(proof["proof_refs"])) > recallResponseMaxProofRefs ||
		!recallResponseValidDigest(anyToString(proof["temporal_premise_digest"])) ||
		!recallResponseValidDigest(anyToString(anyMap(response["request_scope"])["snapshot_digest"])) ||
		!recallResponseValidDigest(anyToString(anyMap(response["request_scope"])["receipt_digest"])) {
		return false
	}
	if !recallResponseStringList(facets["jobs"]) || !recallResponseStringList(facets["memory_objects"]) ||
		!recallResponseStringList(proof["proof_refs"]) || !recallResponseStringList(proof["confidence_basis"]) ||
		!recallResponseStringList(proof["conflict_refs"]) || !recallResponseStringList(proof["gap_refs"]) ||
		!recallResponseStringList(proof["receipt_refs"]) || !recallResponseStringList(composition["ordered_modules"]) {
		return false
	}
	if !recallResponseOneOf(anyToString(facets["temporal_state"]), "current", "historical", "changed_over_time", "ordered_sequence", "deadline", "recurrence", "mixed", "current_or_unknown") ||
		!recallResponseOneOf(anyToString(facets["evidence_state"]), "absent", "clean", "degraded", "sparse", "conflicting", "quarantined", "superseded") ||
		recallResponseConsequenceRank[anyToString(facets["consequence"])] == 0 ||
		!recallResponseEvalConditionAllowed(anyToString(composition["condition"])) ||
		(anyToString(composition["ablation"]) != "none" && !recallResponseBindingKindsContains(anyToString(composition["ablation"]))) ||
		anyToString(composition["proof_strategy"]) != recallResponseProofStrategy ||
		!recallResponseOneOf(anyToString(composition["coverage_status"]), "satisfied", "unsatisfied") {
		return false
	}
	primaryModule := anyToString(composition["primary_module"])
	orderedModules := contextPackAnyList(composition["ordered_modules"])
	components := contextPackAnyList(answer["components"])
	if primaryModule == "v1_control" {
		if len(orderedModules) != 0 || len(components) != 0 {
			return false
		}
	} else {
		if !recallResponseModuleAllowed(primaryModule) || len(orderedModules) != len(components) || len(components) == 0 || anyToString(orderedModules[0]) != primaryModule || !recallResponseValidateModules(components, proof, scope) {
			return false
		}
		for index, raw := range components {
			if anyToString(anyMap(raw)["kind"]) != anyToString(orderedModules[index]) {
				return false
			}
		}
	}
	if anyToString(proof["as_of"]) != anyToString(scope["as_of"]) ||
		anyToString(proof["temporal_premise_digest"]) != anyToString(scope["temporal_premise_digest"]) ||
		anyToString(composition["condition"]) != anyToString(scope["condition"]) ||
		anyToString(composition["ablation"]) != anyToString(scope["ablation"]) {
		return false
	}
	proofSet := map[string]bool{}
	for _, raw := range contextPackAnyList(proof["proof_refs"]) {
		proofSet[anyToString(raw)] = true
	}
	for _, raw := range contextPackAnyList(proof["coverage"]) {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"obligation", "status", "proof_refs"}) ||
			(anyToString(row["status"]) != "satisfied" && anyToString(row["status"]) != "unsatisfied") ||
			!recallResponseModuleRefsWithin(row["proof_refs"], proofSet) {
			return false
		}
	}
	return true
}

func recallResponseStringList(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		if _, ok := raw.(string); !ok {
			return false
		}
	}
	return true
}

func recallResponseOneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// recallResponseRecomputeClippedIdentity refreshes every U2/U3 proof and
// component identity from the transport-clipped artifact. It reports whether
// it had to replace the candidate with a structurally different control so the
// caller can restamp the shared boundary contract.
func recallResponseRecomputeClippedIdentity(response map[string]any) bool {
	classification := anyMap(response["classification"])
	facets := anyMap(classification["facets"])
	answer := anyMap(response["answer"])
	composition := anyMap(answer["composition"])
	scope := anyMap(response["request_scope"])
	if len(facets) == 0 || len(answer) == 0 || len(composition) == 0 || len(scope) == 0 {
		return false
	}
	asOf := recallResponseNormalizeAsOf(anyToString(scope["as_of"]))
	temporalPremise := recallResponseTemporalPremise(asOf, anyToString(facets["temporal_state"]))
	temporalPremiseDigest := anyToString(temporalPremise["digest"])
	snapshotDigest := anyToString(scope["snapshot_digest"])
	if !recallResponseValidDigest(snapshotDigest) {
		snapshotDigest = recallResponseSnapshotDigest(
			contextPackAnyList(response["evidence"]), contextPackAnyList(response["conflicts"]),
			contextPackAnyList(response["gaps"]), temporalPremiseDigest,
		)
	}
	receiptDigest := anyToString(scope["receipt_digest"])
	if !recallResponseValidDigest(receiptDigest) {
		receiptDigest = recallResponseReceiptDigest(contextPackAnyList(response["receipt_refs"]))
	}
	scope["as_of"] = asOf
	scope["temporal_premise_digest"] = temporalPremiseDigest
	scope["snapshot_digest"] = snapshotDigest
	scope["receipt_digest"] = receiptDigest
	canaryPolicy, canaryPolicyOK := recallResponseCanaryPolicySnapshotFromResponse(response)
	policy := validatedRecallResponsePolicyInput{
		condition: anyToString(scope["condition"]), ablation: anyToString(scope["ablation"]),
		snapshotDigest: snapshotDigest, receiptDigest: receiptDigest,
		canaryPolicy: canaryPolicy,
	}
	proof := recallResponseProofSpine(response, asOf, temporalPremiseDigest, snapshotDigest, receiptDigest)
	if anyToString(composition["primary_module"]) == "v1_control" {
		fallbackReason := anyToString(composition["fallback_reason"])
		answer["proof_spine"] = proof
		answer["components"] = []any{}
		answer["composition"] = recallResponseComposition(policy, proof)
		if fallbackReason != "" {
			anyMap(answer["composition"])["fallback_reason"] = fallbackReason
		}
		return false
	}
	if !canaryPolicyOK {
		fallback := recallResponseFailClosedU2Control(response, policy, asOf)
		recallResponseAttachFallbackStageReceipt(fallback, recallResponseFallbackStageReceipt(recallResponseFallbackStageModuleValidation, recallResponseProofCompression{}, response))
		recallResponseReplaceWithControl(response, fallback)
		return true
	}
	compressed, compression, compressionOK := recallResponseCompressProof(response, proof, policy)
	if !compressionOK || !compression.Sufficient {
		fallback := recallResponseFailClosedU2Control(response, policy, asOf)
		recallResponseAttachFallbackStageReceipt(fallback, recallResponseFallbackStageReceipt(compression.FailureStage, compression, response))
		recallResponseReplaceWithControl(response, fallback)
		return true
	}
	answer["proof_spine"] = compressed
	modules, primary, ordered, modulesOK := recallResponseBuildModules(response, compressed, policy)
	if !modulesOK {
		fallback := recallResponseFailClosedU2Control(response, policy, asOf)
		recallResponseAttachFallbackStageReceipt(fallback, recallResponseFallbackStageReceipt(recallResponseFallbackStageModuleValidation, recallResponseProofCompression{}, response))
		recallResponseReplaceWithControl(response, fallback)
		return true
	}
	answer["components"] = modules
	if !recallResponseMergeComponentUnion(response, modules, policy.ablation) {
		fallback := recallResponseFailClosedU2Control(response, policy, asOf)
		recallResponseAttachFallbackStageReceipt(fallback, recallResponseFallbackStageReceipt(recallResponseFallbackStageModuleValidation, recallResponseProofCompression{}, response))
		recallResponseReplaceWithControl(response, fallback)
		return true
	}
	nextComposition := recallResponseComposition(policy, compressed)
	nextComposition["primary_module"] = primary
	nextComposition["ordered_modules"] = recallResponseAnyStrings(ordered)
	answer["composition"] = nextComposition
	return false
}

func recallResponseReplaceWithControl(target, control map[string]any) {
	for key := range target {
		delete(target, key)
	}
	for key, value := range control {
		target[key] = value
	}
}

func recallResponseProofCoverageStatus(proof map[string]any) string {
	for _, raw := range contextPackAnyList(proof["coverage"]) {
		if anyToString(anyMap(raw)["status"]) != "satisfied" {
			return "unsatisfied"
		}
	}
	return "satisfied"
}

func recallResponseFailClosedU2Control(
	control map[string]any,
	policy validatedRecallResponsePolicyInput,
	asOf string,
) map[string]any {
	response := cloneJSONMap(control)
	serverSilenced := recallResponseServerSilenced(response)
	facets := map[string]any{
		"jobs": []any{"verify"}, "memory_objects": []any{"durable_memory"},
		"temporal_state": "current_or_unknown", "evidence_state": "degraded", "consequence": "high_stakes",
	}
	if asOf != recallResponseLatestAsOf {
		facets["temporal_state"] = "historical"
	}
	response["classification"] = recallResponseLegacyClassification(facets, "abstain")
	// The control fallback withdraws top-level support and components. The
	// authoritative post-eligibility union remains available only in the
	// bounded disclosure surface, with an explicit withdrawal ledger; no
	// candidate claim survives a failed closed-contract validation.
	recallResponseEnsureControlReceipt(response, policy, asOf)
	recallResponseRecordFailClosedWithdrawals(response)
	response["evidence"] = []any{}
	gaps := contextPackAnyList(response["gaps"])
	hasFailureGap := false
	for _, raw := range gaps {
		if anyToString(anyMap(raw)["code"]) == "candidate_projection_invalid" {
			hasFailureGap = true
			break
		}
	}
	if !hasFailureGap {
		gaps = append(gaps, map[string]any{
			"code": "candidate_projection_invalid", "material": true,
			"reason":              "The compositional projection failed its closed contract and was replaced by the v1 control response.",
			"required_for_action": true,
		})
	}
	if len(gaps) > recallResponseMaxGaps {
		gaps = gaps[:recallResponseMaxGaps]
	}
	response["gaps"] = gaps
	response["confidence"] = map[string]any{
		"label": "abstain", "score": 0.0, "basis": []any{"candidate_projection_invalid"}, "calibrated": false,
	}
	response["inferences"] = recallResponseInferences(nil, anyMap(response["confidence"]), "abstain")
	answer := anyMap(response["answer"])
	answer["summary"] = "The compositional projection failed validation; use the bounded v1 control and verify before acting."
	answer["answer_mode"] = "abstention"
	answer["claim_refs"] = []any{}
	answer["components"] = []any{}
	state := anyMap(response["state"])
	state["status"] = "abstain"
	state["evidence_count"] = 0
	state["conflict_count"] = len(contextPackAnyList(response["conflicts"]))
	state["gap_count"] = len(gaps)
	nextAction := anyMap(response["next_action"])
	nextAction["kind"] = "retrieve_or_verify"
	nextAction["label"] = "Verify the response contract and proof snapshot"
	nextAction["reason"] = "Candidate projection validation failed; no candidate identity or action authority is retained."
	if serverSilenced {
		// U6's server-owned hard/ordinary silence is stronger than the generic
		// v1 fallback advice. Preserve the no-dispatch boundary after fallback
		// projection, including its recomputed response identity below.
		nextAction["kind"] = "none"
		nextAction["label"] = "No action"
		nextAction["reason"] = "The server-derived silence decision closed the action boundary."
		nextAction["requires_verification"] = false
		actionBoundary := anyMap(response["action_boundary"])
		actionBoundary["reason"] = "The server-derived silence decision forbids dispatch and external mutation."
		response["writeback_required"] = false
	}
	outcome := anyMap(response["outcome"])
	outcome["status"] = "not_attributable"
	outcome["attributable"] = false
	outcome["receipt_id"] = ""

	temporalPremise := recallResponseTemporalPremise(asOf, anyToString(facets["temporal_state"]))
	temporalPremiseDigest := anyToString(temporalPremise["digest"])
	snapshotDigest := policy.snapshotDigest
	if !recallResponseValidDigest(snapshotDigest) {
		snapshotDigest = recallResponseSnapshotDigest(nil, nil, gaps, temporalPremiseDigest)
	}
	receiptDigest := policy.receiptDigest
	if !recallResponseValidDigest(receiptDigest) {
		receiptDigest = recallResponseReceiptDigest(contextPackAnyList(response["receipt_refs"]))
	}
	scope := anyMap(response["request_scope"])
	scope["as_of"] = asOf
	if taskClass := recallResponseSafeTaskClass(anyToString(scope["task_class"])); taskClass != "" {
		scope["task_class"] = taskClass
	}
	scope["temporal_premise_digest"] = temporalPremiseDigest
	scope["snapshot_digest"] = snapshotDigest
	scope["receipt_digest"] = receiptDigest
	scope["condition"] = policy.condition
	scope["ablation"] = policy.ablation
	// The fail-closed fallback derives compatibility snapshot/receipt digests
	// when no authoritative policy artifact was supplied. Rebind the control
	// receipt after those final scope values are settled so the receipt cannot
	// point at the pre-fallback local hash.
	recallResponseEnsureControlReceipt(response, policy, asOf)
	proof := recallResponseProofSpine(response, asOf, temporalPremiseDigest, snapshotDigest, receiptDigest)
	answer["proof_spine"] = proof
	answer["composition"] = recallResponseComposition(policy, proof)
	anyMap(answer["composition"])["fallback_reason"] = "candidate_projection_invalid"
	response["response_id"] = recallResponseIDForResponse(response)
	response["response_digest"] = recallResponseSemanticDigest(response)
	return response
}

const (
	recallResponseAblationWitnessSchema = "recall_response.synthetic_ablation_witness.v1"
	recallResponseAblationStagePrefix   = "selected_component_ablation:"
)

func recallResponseAblationExpectedStage(productStage string) string {
	if !recallResponseFallbackStages[productStage] {
		return ""
	}
	return recallResponseAblationStagePrefix + productStage
}

func recallResponseAblationExpectedStageValid(value string) bool {
	if !strings.HasPrefix(value, recallResponseAblationStagePrefix) {
		return false
	}
	return recallResponseFallbackStages[strings.TrimPrefix(value, recallResponseAblationStagePrefix)]
}

func recallResponseAblationWitnessDigest(witness map[string]any) string {
	material := cloneJSONMap(witness)
	delete(material, "witness_digest")
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseDefaultAblationWitness() map[string]any {
	witness := map[string]any{
		"schema_id":                  recallResponseAblationWitnessSchema,
		"status":                     "not_applicable",
		"component_ref":              "",
		"baseline_component_ref":     "",
		"component_kind":             "",
		"baseline_union_digest":      "",
		"baseline_response_identity": "",
		"omission_receipt_digest":    "",
		"expected_failure_stage":     "none",
		"observed_failure_stage":     "none",
		"witness_digest":             "",
	}
	witness["witness_digest"] = recallResponseAblationWitnessDigest(witness)
	return witness
}

func recallResponseSyntheticAblationOmissionReceiptDigest(row map[string]any) string {
	material := map[string]any{
		"item_ref": anyToString(row["item_ref"]), "item_type": anyToString(row["item_type"]),
		"reason": anyToString(row["reason"]), "protected": anyToBool(row["protected"]),
		"counterfactual_outcome": anyToString(anyMap(row["same_snapshot_counterfactual"])["outcome"]),
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseEnsureSyntheticAblationOmission(response map[string]any, componentRef string) bool {
	disclosure := recallResponseDisclosure(response)
	protected := false
	foundComponent := false
	for _, raw := range contextPackAnyList(disclosure["component_union"]) {
		component := anyMap(raw)
		if anyToString(component["component_ref"]) == componentRef {
			protected = anyToBool(component["protected"])
			foundComponent = true
			break
		}
	}
	if !foundComponent {
		return false
	}
	ledger := contextPackAnyList(disclosure["omission_ledger"])
	for _, raw := range ledger {
		row := anyMap(raw)
		if anyToString(row["item_type"]) == "component" && anyToString(row["item_ref"]) == componentRef {
			row["reason"] = "synthetic_ablation"
			row["protected"] = protected
			row["evidence_binding"] = recallResponseOmissionBinding(response, componentRef, "component")
			row["same_snapshot_counterfactual"] = recallResponseOmissionCounterfactual(response, "fail_closed_control", protected)
			return true
		}
	}
	row := map[string]any{
		"item_ref": componentRef, "item_type": "component", "reason": "synthetic_ablation", "protected": protected,
		"evidence_binding":             recallResponseOmissionBinding(response, componentRef, "component"),
		"same_snapshot_counterfactual": recallResponseOmissionCounterfactual(response, "fail_closed_control", protected),
	}
	if len(ledger) < recallResponseMaxOmissionLedger {
		disclosure["omission_ledger"] = append(ledger, row)
		return true
	}
	// The evaluator's exact ablated component receipt is mandatory. Replace the
	// deterministically lowest-priority ledger sample; the displaced omission
	// remains represented by authoritative counts, union digest, and cursor.
	replacement := 0
	for index := 1; index < len(ledger); index++ {
		left := anyMap(ledger[index])
		right := anyMap(ledger[replacement])
		leftPriority := recallResponseOmissionPriority(anyToString(left["reason"]), anyToBool(left["protected"]))
		rightPriority := recallResponseOmissionPriority(anyToString(right["reason"]), anyToBool(right["protected"]))
		if leftPriority < rightPriority || leftPriority == rightPriority && recallResponseTypedItemKey(anyToString(left["item_type"]), anyToString(left["item_ref"])) > recallResponseTypedItemKey(anyToString(right["item_type"]), anyToString(right["item_ref"])) {
			replacement = index
		}
	}
	ledger[replacement] = row
	disclosure["omission_ledger"] = ledger
	return true
}

func recallResponseSealAblationWitness(response map[string]any, policy validatedRecallResponsePolicyInput, status, observedStage string) bool {
	base := policy.ablationWitness
	baselineComponentRef := anyToString(base["baseline_component_ref"])
	componentKind := anyToString(base["component_kind"])
	if !policy.synthetic || componentKind != policy.ablation || strings.TrimSpace(baselineComponentRef) == "" ||
		!recallResponseValidDigest(anyToString(base["baseline_union_digest"])) ||
		!recallResponseExactOpaqueID(anyToString(base["baseline_response_identity"]), "rr_") {
		return false
	}
	componentRef := ""
	for _, raw := range contextPackAnyList(recallResponseDisclosure(response)["component_union"]) {
		row := anyMap(raw)
		if anyToString(row["kind"]) == componentKind {
			componentRef = anyToString(row["component_ref"])
			break
		}
	}
	if strings.TrimSpace(componentRef) == "" {
		return false
	}
	if !recallResponseEnsureSyntheticAblationOmission(response, componentRef) {
		return false
	}
	omissionDigest := ""
	for _, raw := range contextPackAnyList(recallResponseDisclosure(response)["omission_ledger"]) {
		row := anyMap(raw)
		if anyToString(row["item_type"]) == "component" && anyToString(row["item_ref"]) == componentRef && anyToString(row["reason"]) == "synthetic_ablation" {
			omissionDigest = recallResponseSyntheticAblationOmissionReceiptDigest(row)
			break
		}
	}
	if !recallResponseValidDigest(omissionDigest) {
		return false
	}
	witness := map[string]any{
		"schema_id":                  recallResponseAblationWitnessSchema,
		"status":                     status,
		"component_ref":              componentRef,
		"baseline_component_ref":     baselineComponentRef,
		"component_kind":             componentKind,
		"baseline_union_digest":      anyToString(base["baseline_union_digest"]),
		"baseline_response_identity": anyToString(base["baseline_response_identity"]),
		"omission_receipt_digest":    omissionDigest,
		"expected_failure_stage":     anyToString(base["expected_failure_stage"]),
		"observed_failure_stage":     observedStage,
		"witness_digest":             "",
	}
	witness["witness_digest"] = recallResponseAblationWitnessDigest(witness)
	disclosure := recallResponseDisclosure(response)
	disclosure["ablation_witness"] = witness
	recallResponseSetContinuationAction(response, anyMap(disclosure["continuation_action"]))
	return true
}

func recallResponseSealAblationFailureWitness(response map[string]any, policy validatedRecallResponsePolicyInput, observedStage string) {
	base := policy.ablationWitness
	if len(base) == 0 {
		return
	}
	componentRef := ""
	for _, raw := range contextPackAnyList(recallResponseDisclosure(response)["component_union"]) {
		row := anyMap(raw)
		if anyToString(row["kind"]) == anyToString(base["component_kind"]) {
			componentRef = anyToString(row["component_ref"])
			break
		}
	}
	witness := map[string]any{
		"schema_id":                  recallResponseAblationWitnessSchema,
		"status":                     "candidate_projection_invalid",
		"component_ref":              componentRef,
		"baseline_component_ref":     anyToString(base["baseline_component_ref"]),
		"component_kind":             anyToString(base["component_kind"]),
		"baseline_union_digest":      anyToString(base["baseline_union_digest"]),
		"baseline_response_identity": anyToString(base["baseline_response_identity"]),
		"omission_receipt_digest":    "",
		"expected_failure_stage":     anyToString(base["expected_failure_stage"]),
		"observed_failure_stage":     firstNonEmptyStrings(observedStage, "unknown"),
		"witness_digest":             "",
	}
	witness["witness_digest"] = recallResponseAblationWitnessDigest(witness)
	disclosure := recallResponseDisclosure(response)
	disclosure["ablation_witness"] = witness
	recallResponseSetContinuationAction(response, anyMap(disclosure["continuation_action"]))
}

func recallResponseAblationWitnessValid(response map[string]any, witness map[string]any) bool {
	if !recallResponseExactFields(witness, []string{
		"schema_id", "status", "component_ref", "baseline_component_ref", "component_kind", "baseline_union_digest",
		"baseline_response_identity", "omission_receipt_digest", "expected_failure_stage",
		"observed_failure_stage", "witness_digest",
	}) || anyToString(witness["schema_id"]) != recallResponseAblationWitnessSchema ||
		anyToString(witness["witness_digest"]) != recallResponseAblationWitnessDigest(witness) {
		return false
	}
	ablation := anyToString(anyMap(response["request_scope"])["ablation"])
	status := anyToString(witness["status"])
	if ablation == "none" {
		return status == "not_applicable" && anyToString(witness["component_ref"]) == "" &&
			anyToString(witness["baseline_component_ref"]) == "" &&
			anyToString(witness["component_kind"]) == "" && anyToString(witness["baseline_union_digest"]) == "" &&
			anyToString(witness["baseline_response_identity"]) == "" && anyToString(witness["omission_receipt_digest"]) == "" &&
			anyToString(witness["expected_failure_stage"]) == "none" && anyToString(witness["observed_failure_stage"]) == "none"
	}
	if anyToString(witness["component_kind"]) != ablation || strings.TrimSpace(anyToString(witness["component_ref"])) == "" ||
		strings.TrimSpace(anyToString(witness["baseline_component_ref"])) == "" ||
		!recallResponseValidDigest(anyToString(witness["baseline_union_digest"])) ||
		!recallResponseExactOpaqueID(anyToString(witness["baseline_response_identity"]), "rr_") {
		return false
	}
	if status == "candidate_projection_invalid" {
		return anyToString(witness["omission_receipt_digest"]) == "" && anyToString(witness["observed_failure_stage"]) != ""
	}
	if status != "accepted_candidate" && status != "accepted_control" || !recallResponseValidDigest(anyToString(witness["omission_receipt_digest"])) {
		return false
	}
	wantExpected, wantObserved := "none", "none"
	if status == "accepted_control" {
		wantExpected = anyToString(witness["expected_failure_stage"])
		wantObserved = wantExpected
		if !recallResponseAblationExpectedStageValid(wantExpected) {
			return false
		}
	}
	if anyToString(witness["expected_failure_stage"]) != wantExpected || anyToString(witness["observed_failure_stage"]) != wantObserved {
		return false
	}
	for _, raw := range contextPackAnyList(recallResponseDisclosure(response)["omission_ledger"]) {
		row := anyMap(raw)
		if anyToString(row["item_type"]) == "component" && anyToString(row["item_ref"]) == anyToString(witness["component_ref"]) &&
			anyToString(row["reason"]) == "synthetic_ablation" {
			return anyToString(witness["omission_receipt_digest"]) == recallResponseSyntheticAblationOmissionReceiptDigest(row)
		}
	}
	return false
}

// recallResponseSyntheticAblationControl is the explicit closed
// counterfactual for a test-only selected-component ablation. It remains an
// abstaining v1 control, but it is not a candidate-validation failure:
// candidate_projection_invalid is reserved for genuine contract defects.
func recallResponseSyntheticAblationControl(
	control map[string]any,
	policy validatedRecallResponsePolicyInput,
	asOf string,
	observedStage string,
) map[string]any {
	response := recallResponseFailClosedU2Control(control, policy, asOf)
	gaps := make([]any, 0, len(contextPackAnyList(response["gaps"])))
	for _, raw := range contextPackAnyList(response["gaps"]) {
		if anyToString(anyMap(raw)["code"]) != "candidate_projection_invalid" {
			gaps = append(gaps, raw)
		}
	}
	response["gaps"] = gaps
	confidence := map[string]any{
		"label": "abstain", "score": 0.0, "basis": []any{"synthetic_ablation"}, "calibrated": false,
	}
	response["confidence"] = confidence
	response["inferences"] = recallResponseInferences(nil, confidence, "abstain")
	state := anyMap(response["state"])
	state["status"] = "abstain"
	state["gap_count"] = len(gaps)
	answer := anyMap(response["answer"])
	answer["summary"] = "The requested synthetic ablation removed a selected component; the evaluator returned an explicit closed counterfactual control."
	nextAction := anyMap(response["next_action"])
	nextAction["kind"] = "retrieve_or_verify"
	nextAction["label"] = "Review the ablated counterfactual"
	nextAction["reason"] = "The synthetic evaluator removed a selected response component; no action authority is retained."

	scope := anyMap(response["request_scope"])
	proof := recallResponseProofSpine(
		response, asOf, anyToString(scope["temporal_premise_digest"]),
		anyToString(scope["snapshot_digest"]), anyToString(scope["receipt_digest"]),
	)
	answer["proof_spine"] = proof
	composition := recallResponseComposition(policy, proof)
	composition["fallback_reason"] = "synthetic_ablation"
	answer["composition"] = composition

	disclosure := recallResponseDisclosure(response)
	for _, raw := range contextPackAnyList(disclosure["component_union"]) {
		component := anyMap(raw)
		if anyToString(component["kind"]) != policy.ablation {
			continue
		}
		itemRef := anyToString(component["component_ref"])
		for _, omissionRaw := range contextPackAnyList(disclosure["omission_ledger"]) {
			omission := anyMap(omissionRaw)
			if anyToString(omission["item_type"]) != "component" || anyToString(omission["item_ref"]) != itemRef {
				continue
			}
			omission["reason"] = "synthetic_ablation"
			omission["evidence_binding"] = recallResponseOmissionBinding(response, itemRef, "component")
			omission["same_snapshot_counterfactual"] = recallResponseOmissionCounterfactual(response, "fail_closed_control", anyToBool(component["protected"]))
			break
		}
		break
	}
	if !recallResponseSealAblationWitness(response, policy, "accepted_control", observedStage) {
		fallback := recallResponseFailClosedU2Control(control, policy, asOf)
		receipt := recallResponseFallbackStageReceipt(recallResponseFallbackStageModuleValidation, recallResponseProofCompression{}, response)
		recallResponseAttachFallbackStageReceipt(fallback, receipt)
		recallResponseSealAblationFailureWitness(fallback, policy, anyToString(receipt["stage"]))
		return fallback
	}
	response["response_id"] = recallResponseIDForResponse(response)
	response["response_digest"] = recallResponseSemanticDigest(response)
	return response
}

func recallResponseExactFields(value map[string]any, expected []string) bool {
	if len(value) != len(expected) {
		return false
	}
	want := append([]string(nil), expected...)
	got := make([]string, 0, len(value))
	for key := range value {
		got = append(got, key)
	}
	sort.Strings(want)
	sort.Strings(got)
	for index := range want {
		if want[index] != got[index] {
			return false
		}
	}
	return true
}
