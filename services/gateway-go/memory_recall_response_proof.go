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
	for _, raw := range evidence {
		addProof(anyToString(anyMap(raw)["ref_id"]))
	}
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

	coverage := []any{
		map[string]any{"obligation": "primary_result", "status": recallResponseCoverageStatus(primaryResult != ""), "proof_refs": recallResponseAnyStrings(proofRefs)},
		map[string]any{"obligation": "temporal_premise", "status": recallResponseCoverageStatus(recallResponseValidDigest(temporalPremiseDigest)), "proof_refs": []any{}},
		map[string]any{"obligation": "bounded_snapshot", "status": recallResponseCoverageStatus(recallResponseValidDigest(snapshotDigest)), "proof_refs": []any{}},
		map[string]any{"obligation": "conflict_free", "status": recallResponseCoverageStatus(len(conflicts) == 0), "proof_refs": recallResponseAnyStrings(conflictRefs)},
		map[string]any{"obligation": "material_gaps_resolved", "status": recallResponseCoverageStatus(len(gaps) == 0), "proof_refs": recallResponseAnyStrings(gapRefs)},
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
		recallResponseReplaceWithControl(response, recallResponseFailClosedU2Control(response, policy, asOf))
		return true
	}
	compressed, compression, compressionOK := recallResponseCompressProof(response, proof, policy)
	if !compressionOK || !compression.Sufficient {
		recallResponseReplaceWithControl(response, recallResponseFailClosedU2Control(response, policy, asOf))
		return true
	}
	answer["proof_spine"] = compressed
	modules, primary, ordered, modulesOK := recallResponseBuildModules(response, compressed, policy)
	if !modulesOK {
		recallResponseReplaceWithControl(response, recallResponseFailClosedU2Control(response, policy, asOf))
		return true
	}
	answer["components"] = modules
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
	facets := map[string]any{
		"jobs": []any{"verify"}, "memory_objects": []any{"durable_memory"},
		"temporal_state": "current_or_unknown", "evidence_state": "degraded", "consequence": "high_stakes",
	}
	if asOf != recallResponseLatestAsOf {
		facets["temporal_state"] = "historical"
	}
	response["classification"] = recallResponseLegacyClassification(facets, "abstain")
	response["evidence"] = []any{}
	response["conflicts"] = []any{}
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
	state["conflict_count"] = 0
	state["gap_count"] = len(gaps)
	nextAction := anyMap(response["next_action"])
	nextAction["kind"] = "retrieve_or_verify"
	nextAction["label"] = "Verify the response contract and proof snapshot"
	nextAction["reason"] = "Candidate projection validation failed; no candidate identity or action authority is retained."
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
	proof := recallResponseProofSpine(response, asOf, temporalPremiseDigest, snapshotDigest, receiptDigest)
	answer["proof_spine"] = proof
	answer["composition"] = recallResponseComposition(policy, proof)
	anyMap(answer["composition"])["fallback_reason"] = "candidate_projection_invalid"
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
