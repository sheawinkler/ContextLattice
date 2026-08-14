package main

import "strings"

const (
	recallResponseMaxModules          = 6
	recallResponseMaxSecondaryModules = 3
	recallResponseModulePolicyVersion = "recall_response.control.v1"
)

var recallResponseModuleOrder = []string{
	"exact_current_status",
	"project_continuation",
	"decision_rationale",
	"preference_constraint",
	"timeline",
	"procedure",
	"multi_memory_synthesis",
	"conflict_supersession",
	"negative_abstention",
	"memory_to_action",
}

var recallResponseModuleSafety = map[string]bool{
	"conflict_supersession": true,
	"negative_abstention":   true,
}

var recallResponseModulePayloadFields = map[string][]string{
	"exact_current_status":   {"value", "status", "qualifier_refs"},
	"decision_rationale":     {"decision", "rationale_refs", "constraint_refs", "rejected_alternative_refs"},
	"project_continuation":   {"checkpoint_ref", "completed_refs", "open_refs", "blocker_refs", "next_move"},
	"preference_constraint":  {"statement_ref", "scope_ref", "support_count", "contradiction_refs", "sensitivity"},
	"timeline":               {"event_refs", "ordering", "unknown_intervals", "causal_claim_refs"},
	"procedure":              {"tool_ref", "parameter_bindings", "ordered_steps", "refusal_conditions", "recovery_conditions"},
	"multi_memory_synthesis": {"conclusion", "bridge_claims"},
	"conflict_supersession":  {"claim_refs", "winner_ref", "resolution_status", "resolution_reason_ref", "unknown_periods"},
	"negative_abstention":    {"terminal", "coverage_receipt", "negative_claim_ref"},
	"memory_to_action":       {"intended_tool_ref", "parameter_bindings", "ordered_steps", "refusal_conditions", "rollback_conditions"},
}

// recallResponseBuildModules replaces the old identity-only component list
// with bounded, evidence-only payloads. It deliberately consumes the already
// projected response and proof spine; it never reads the raw retrieval pack.
func recallResponseBuildModules(
	response map[string]any,
	proof map[string]any,
	policy validatedRecallResponsePolicyInput,
	sources ...map[string]any,
) ([]any, string, []string, bool) {
	var source map[string]any
	if len(sources) > 0 {
		source = sources[0]
	}
	ordered, ok := recallResponseSelectedModuleKinds(response, proof, policy, source)
	if !ok {
		return nil, "", nil, false
	}
	primary := ordered[0]
	scope := anyMap(response["request_scope"])
	proofRefs := recallResponseModuleProofRefs(proof)
	modules := make([]any, 0, len(ordered))
	for index, kind := range ordered {
		refs := recallResponseModuleRefs(kind, response, proofRefs, source)
		payload := recallResponseModulePayload(kind, response, refs, source)
		binding := recallResponseModuleBinding(kind, response, policy, refs)
		module := map[string]any{
			"component_ref": "rrc_" + sha256Hex(anyToString(scope["scope_digest"]) + "\x00" + kind)[:24],
			"kind":          kind,
			"ordinal":       index + 1,
			"primary":       index == 0,
			"proof_refs":    recallResponseAnyStrings(refs),
			"payload":       payload,
			"binding":       binding,
		}
		if !recallResponseSealComponentIdentity(module) {
			return nil, "", nil, false
		}
		modules = append(modules, module)
	}
	if !recallResponseValidateModules(modules, proof, scope) {
		return nil, "", nil, false
	}
	return modules, primary, ordered, true
}

func recallResponseSelectedModuleKinds(
	response map[string]any,
	proof map[string]any,
	policy validatedRecallResponsePolicyInput,
	sources ...map[string]any,
) ([]string, bool) {
	var source map[string]any
	if len(sources) > 0 {
		source = sources[0]
	}
	answer := anyMap(response["answer"])
	legacy := contextPackAnyList(answer["components"])
	requested := make([]string, 0, len(legacy))
	seen := map[string]bool{}
	ablation := strings.ToLower(strings.TrimSpace(policy.ablation))
	for _, raw := range legacy {
		kind := strings.ToLower(strings.TrimSpace(anyToString(anyMap(raw)["kind"])))
		if !recallResponseModuleAllowed(kind) || seen[kind] || kind == ablation {
			continue
		}
		if kind == "memory_to_action" && recallResponseHasSensitiveUnavailableActionEvidence(recallResponseTemporalRows(source)) {
			if _, ok := recallResponseSensitiveUnavailableActionStatus(recallResponseTemporalRows(source)); !ok {
				// A legacy component hint cannot turn conflicting or excluded
				// sensitive evidence into protected action membership.
				continue
			}
		}
		seen[kind] = true
		requested = append(requested, kind)
	}
	if seen["procedure"] && seen["memory_to_action"] {
		redundant := "procedure"
		if requested[0] == "procedure" {
			redundant = "memory_to_action"
		}
		filtered := requested[:0]
		for _, kind := range requested {
			if kind != redundant {
				filtered = append(filtered, kind)
			}
		}
		requested = filtered
		delete(seen, redundant)
	}
	componentRows := contextPackAnyList(recallResponseDisclosure(response)["component_union"])
	if len(componentRows) > 0 {
		available := map[string]bool{}
		for _, raw := range componentRows {
			available[anyToString(anyMap(raw)["kind"])] = true
		}
		filtered := requested[:0]
		for _, kind := range requested {
			if available[kind] {
				filtered = append(filtered, kind)
				continue
			}
			delete(seen, kind)
		}
		requested = filtered
	}
	if len(requested) > 0 && ablation != "memory_to_action" && !seen["procedure"] && !seen["memory_to_action"] && recallResponseActionProjectionAllowed(source) && recallResponseHasReadyActionEvidence(recallResponseTemporalRows(source)) {
		// A validated structured action witness is relevant regardless of the
		// request's presentation class. Keep the classified primary first, then
		// place the action component ahead of optional secondary layout modules
		// so classification cannot silently remove actionable membership.
		requested = append(requested, "")
		copy(requested[2:], requested[1:])
		requested[1] = "memory_to_action"
		seen["memory_to_action"] = true
	}
	appendProtected := func(kind string, required bool) {
		if required && kind != ablation && !seen[kind] {
			seen[kind] = true
			requested = append(requested, kind)
		}
	}
	appendProtected("conflict_supersession", len(contextPackAnyList(proof["conflict_refs"])) > 0)
	appendProtected("negative_abstention", len(contextPackAnyList(proof["gap_refs"])) > 0 || strings.TrimSpace(anyToString(proof["primary_result"])) == "")
	appendProtected("conflict_supersession", recallResponseTemporalHasRetirement(source))
	appendProtected("negative_abstention", recallResponseExplicitNegativeTerminal(source) != "")
	if len(requested) == 0 {
		if ablation == "negative_abstention" {
			return nil, false
		}
		requested = []string{"negative_abstention"}
	}

	// The first requested module is the primary. Safety modules are retained
	// after the primary/secondary budget and cannot displace counterevidence.
	ordered := []string{requested[0]}
	secondary := 0
	for _, kind := range requested[1:] {
		if recallResponseModuleSafety[kind] {
			ordered = append(ordered, kind)
			continue
		}
		if secondary >= recallResponseMaxSecondaryModules {
			continue
		}
		ordered = append(ordered, kind)
		secondary++
	}
	if len(ordered) > recallResponseMaxModules {
		return nil, false
	}
	return ordered, len(ordered) > 0
}

func recallResponseModuleAllowed(kind string) bool {
	_, ok := recallResponseModulePayloadFields[kind]
	return ok
}

func recallResponseModuleProofRefs(proof map[string]any) []string {
	refs := []string{}
	for _, raw := range contextPackAnyList(proof["proof_refs"]) {
		ref := strings.TrimSpace(anyToString(raw))
		if ref != "" && !containsString(refs, ref) && len(refs) < recallResponseMaxProofRefs {
			refs = append(refs, ref)
		}
	}
	return refs
}

func recallResponseModuleRefs(kind string, response map[string]any, proofRefs []string, sources ...map[string]any) []string {
	var source map[string]any
	if len(sources) > 0 {
		source = sources[0]
	}
	refs := []string{}
	proofSet := map[string]bool{}
	for _, ref := range proofRefs {
		proofSet[ref] = true
	}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && (len(proofRefs) == 0 || proofSet[value]) && !containsString(refs, value) && len(refs) < recallResponseMaxProofRefs {
			refs = append(refs, value)
		}
	}
	if kind == "procedure" || kind == "memory_to_action" {
		// Structured action witnesses outrank generic runbook rows. Without
		// this distinction, the proof minimizer can satisfy an action module
		// with a high-ranked row that carries no tool or parameter evidence.
		for _, raw := range recallResponseTemporalRows(source) {
			row := anyMap(raw)
			_, eligible := recallResponseEvidenceStatus(row)
			_, confidenceValid := recallResponseEvidenceConfidence(row["confidence"])
			if !eligible || !confidenceValid {
				continue
			}
			metadata := recallResponseProjectActionMetadata(row)
			if len(metadata) > 0 {
				add(recallResponseProjectedRowRef(row, response))
			}
		}
		// Post-clipping recomposition no longer has the raw source snapshot.
		// Its already validated module refs are the bounded semantic witness;
		// constrain them to the current candidate set before reuse.
		for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
			module := anyMap(raw)
			if anyToString(module["kind"]) != kind {
				continue
			}
			for _, ref := range contextPackAnyList(module["proof_refs"]) {
				add(anyToString(ref))
			}
		}
		if len(refs) > 0 {
			return refs
		}
		if kind == "memory_to_action" {
			return []string{}
		}
	}
	evidence := contextPackAnyList(response["evidence"])
	for _, raw := range evidence {
		item := anyMap(raw)
		itemKind := strings.ToLower(strings.TrimSpace(anyToString(item["kind"])))
		use := false
		switch kind {
		case "exact_current_status":
			use = true
		case "decision_rationale":
			use = itemKind == "decision" || itemKind == "constraint" || itemKind == "policy"
		case "project_continuation":
			use = itemKind == "project_state" || itemKind == "checkpoint" || itemKind == "continuation"
		case "preference_constraint":
			use = itemKind == "preference" || itemKind == "constraint" || itemKind == "policy"
		case "timeline":
			use = itemKind == "event" || itemKind == "timeline"
		case "procedure", "memory_to_action":
			use = itemKind == "procedure" || itemKind == "runbook" || itemKind == "tool"
		case "multi_memory_synthesis":
			use = true
		case "conflict_supersession":
			use = true
		}
		if use {
			add(anyToString(item["ref_id"]))
		}
	}
	for _, raw := range contextPackAnyList(response["conflicts"]) {
		if kind == "conflict_supersession" || kind == "decision_rationale" || kind == "preference_constraint" {
			add(anyToString(anyMap(raw)["conflict_id"]))
		}
	}
	for _, raw := range contextPackAnyList(response["gaps"]) {
		if recallResponseModuleSafety[kind] || kind == "project_continuation" || kind == "timeline" || kind == "procedure" || kind == "memory_to_action" {
			gap := anyMap(raw)
			add(recallResponseScopedOpaqueRef(anyToString(anyMap(response["request_scope"])["scope_digest"]), "gap", anyToString(gap["code"])))
		}
	}
	if len(refs) == 0 {
		for _, ref := range proofRefs {
			add(ref)
		}
	}
	return refs
}

func recallResponseModulePayload(kind string, response map[string]any, refs []string, sources ...map[string]any) map[string]any {
	var source map[string]any
	if len(sources) > 0 {
		source = sources[0]
	}
	if len(source) == 0 {
		for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
			module := anyMap(raw)
			if anyToString(module["kind"]) == kind {
				// Transport recomposition may have compressed the proof spine
				// since this module was sealed. Reusing a payload whose previous
				// component refs are no longer in the current module witness would
				// create a structurally stale candidate (for example, a conflict
				// payload retaining an optional action ref that was just clipped).
				// Rebuild from the current bounded refs in that case; the source
				// snapshot is intentionally unavailable on this post-clip path.
				previousRefs := anyToStringList(module["proof_refs"], recallResponseMaxProofRefs)
				currentRefs := map[string]bool{}
				for _, ref := range refs {
					currentRefs[ref] = true
				}
				reusable := true
				for _, ref := range previousRefs {
					if !currentRefs[ref] {
						reusable = false
						break
					}
				}
				if reusable {
					return cloneJSONMap(anyMap(module["payload"]))
				}
			}
		}
	}
	conflictRefs := []string{}
	for _, raw := range contextPackAnyList(response["conflicts"]) {
		if ref := strings.TrimSpace(anyToString(anyMap(raw)["conflict_id"])); ref != "" {
			conflictRefs = append(conflictRefs, ref)
		}
	}
	gapRefs := []string{}
	for _, raw := range contextPackAnyList(response["gaps"]) {
		gap := anyMap(raw)
		gapRefs = append(gapRefs, recallResponseScopedOpaqueRef(anyToString(anyMap(response["request_scope"])["scope_digest"]), "gap", anyToString(gap["code"])))
	}
	if len(conflictRefs) > recallResponseMaxProofRefs {
		conflictRefs = conflictRefs[:recallResponseMaxProofRefs]
	}
	if len(gapRefs) > recallResponseMaxProofRefs {
		gapRefs = gapRefs[:recallResponseMaxProofRefs]
	}
	facets := anyMap(anyMap(response["classification"])["facets"])
	posture := anyToString(anyMap(response["classification"])["posture"])
	status := "verify"
	if posture == "answer_with_proof" {
		status = "supported"
	}
	if posture == "abstain" {
		status = "abstain"
	}
	refsAny := recallResponseAnyStrings(refs)
	conflictsAny := recallResponseAnyStrings(conflictRefs)
	gapsAny := recallResponseAnyStrings(gapRefs)
	switch kind {
	case "exact_current_status":
		return map[string]any{"value": "bounded_current_state", "status": status, "qualifier_refs": refsAny}
	case "decision_rationale":
		return map[string]any{"decision": anyToString(facets["consequence"]), "rationale_refs": refsAny, "constraint_refs": refsAny, "rejected_alternative_refs": conflictsAny}
	case "project_continuation":
		return map[string]any{"checkpoint_ref": firstString(refs), "completed_refs": []any{}, "open_refs": gapsAny, "blocker_refs": gapsAny, "next_move": recallResponseSafeNextMove(response)}
	case "preference_constraint":
		return map[string]any{"statement_ref": firstString(refs), "scope_ref": anyToString(anyMap(response["request_scope"])["owner_ref"]), "support_count": len(refs), "contradiction_refs": conflictsAny, "sensitivity": "unspecified"}
	case "timeline":
		return recallResponseTimelinePayload(response, refs, source)
	case "procedure":
		return recallResponseActionPayload("procedure", response, refs, source)
	case "multi_memory_synthesis":
		bridges := []any{}
		if len(refs) > 1 {
			bridges = append(bridges, map[string]any{"proof_refs": refsAny, "basis": "evidence_only"})
		}
		return map[string]any{"conclusion": "bounded_evidence_synthesis", "bridge_claims": bridges}
	case "conflict_supersession":
		return recallResponseConflictPayload(response, refs, source)
	case "negative_abstention":
		return recallResponseNegativePayload(response, refs, source)
	case "memory_to_action":
		return recallResponseActionPayload("memory_to_action", response, refs, source)
	}
	return map[string]any{}
}

func recallResponseModuleBinding(kind string, response map[string]any, policy validatedRecallResponsePolicyInput, refs []string) map[string]any {
	scope := anyMap(response["request_scope"])
	proofDigest := "sha256:" + sha256Hex(recallResponseCanonicalJSON(recallResponseAnyStrings(refs)))
	canaryScope, scopeOK := recallResponseCanaryScopeFromResponse(response)
	resolved := zeroRecallResponseCanaryPolicy{}.ComponentPolicy(recallResponseCanaryScope{}, kind)
	bucket := 0
	if scopeOK {
		resolved = recallResponseResolveComponentPolicy(policy.canaryPolicy, canaryScope, kind)
		bucket = recallResponseComponentBucket(canaryScope, kind, resolved.PolicyVersion)
	}
	return map[string]any{
		"condition":            policy.condition,
		"ablation":             policy.ablation,
		"arm":                  recallResponseComponentArm(bucket, resolved.BasisPoints),
		"exposure_bucket":      bucket,
		"policy_version":       resolved.PolicyVersion,
		"proof_digest":         proofDigest,
		"scope_binding_digest": recallResponseModuleScopeBindingDigest(scope),
		"verifier_digest":      "sha256:" + sha256Hex(kind+"\x00"+proofDigest),
		"component_digest":     "",
	}
}

func recallResponseModuleScopeBindingDigest(scope map[string]any) string {
	material := map[string]any{
		"snapshot_digest":         scope["snapshot_digest"],
		"receipt_digest":          scope["receipt_digest"],
		"owner_ref":               scope["owner_ref"],
		"task_ref":                scope["task_ref"],
		"lane_ref":                scope["execution_lane_ref"],
		"intent":                  scope["retrieval_intent"],
		"temporal_premise_digest": scope["temporal_premise_digest"],
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseSafeNextMove(response map[string]any) string {
	kind := strings.ToLower(strings.TrimSpace(anyToString(anyMap(response["next_action"])["kind"])))
	switch kind {
	case "inspect_proof", "retrieve_or_verify":
		return kind
	default:
		return "retrieve_or_verify"
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// recallResponseValidateModules is the closed U3 seam. Legacy six-field
// components remain valid only for the v1 control fallback; candidate modules
// must satisfy the complete payload/binding shape here.
func recallResponseValidateModules(value []any, proof, scope map[string]any) bool {
	if len(value) == 0 || len(value) > recallResponseMaxModules {
		return false
	}
	secondary := 0
	seenRefs := map[string]bool{}
	seenKinds := map[string]bool{}
	proofSet := map[string]bool{}
	for _, raw := range contextPackAnyList(proof["proof_refs"]) {
		ref, ok := raw.(string)
		if !ok || strings.TrimSpace(ref) == "" || proofSet[ref] {
			return false
		}
		proofSet[ref] = true
	}
	for _, key := range []string{"conflict_refs", "gap_refs"} {
		if !recallResponseModuleRefsWithin(proof[key], proofSet) {
			return false
		}
	}
	for index, raw := range value {
		module, ok := raw.(map[string]any)
		if !ok || !recallResponseModuleShape(module) {
			return false
		}
		kind := anyToString(module["kind"])
		if seenKinds[kind] || seenRefs[anyToString(module["component_ref"])] {
			return false
		}
		seenKinds[kind] = true
		seenRefs[anyToString(module["component_ref"])] = true
		primary, primaryOK := module["primary"].(bool)
		if !recallResponseExactOrdinal(module["ordinal"], index+1) || !primaryOK || primary != (index == 0) {
			return false
		}
		if index > 0 && !recallResponseModuleSafety[kind] {
			secondary++
		}
		if secondary > recallResponseMaxSecondaryModules {
			return false
		}
		if !recallResponseModuleRefsWithin(module["proof_refs"], proofSet) ||
			anyToString(module["component_ref"]) != "rrc_"+sha256Hex(anyToString(scope["scope_digest"]) + "\x00" + kind)[:24] ||
			!recallResponseModuleBindingValid(module, scope) ||
			!recallResponseModulePayloadValid(kind, anyMap(module["payload"]), module["proof_refs"], proof, scope) {
			return false
		}
	}
	if len(contextPackAnyList(proof["conflict_refs"])) > 0 && !seenKinds["conflict_supersession"] {
		return false
	}
	if (len(contextPackAnyList(proof["gap_refs"])) > 0 || strings.TrimSpace(anyToString(proof["primary_result"])) == "") && !seenKinds["negative_abstention"] {
		return false
	}
	return true
}

func recallResponseModuleRefsWithin(value any, allowed map[string]bool) bool {
	items, ok := value.([]any)
	if !ok || len(items) > recallResponseMaxProofRefs {
		return false
	}
	seen := map[string]bool{}
	for _, raw := range items {
		ref, ok := raw.(string)
		if !ok || strings.TrimSpace(ref) == "" || seen[ref] || !allowed[ref] {
			return false
		}
		seen[ref] = true
	}
	return true
}

func recallResponseModuleBindingValid(module, scope map[string]any) bool {
	binding := anyMap(module["binding"])
	proofDigest := "sha256:" + sha256Hex(recallResponseCanonicalJSON(module["proof_refs"]))
	kind := anyToString(module["kind"])
	if !recallResponseEvalConditionAllowed(anyToString(binding["condition"])) ||
		(anyToString(binding["ablation"]) != "none" && !recallResponseModuleAllowed(anyToString(binding["ablation"]))) ||
		anyToString(binding["condition"]) != anyToString(scope["condition"]) ||
		anyToString(binding["ablation"]) != anyToString(scope["ablation"]) ||
		anyToString(binding["proof_digest"]) != proofDigest ||
		anyToString(binding["scope_binding_digest"]) != recallResponseModuleScopeBindingDigest(scope) ||
		anyToString(binding["verifier_digest"]) != "sha256:"+sha256Hex(kind+"\x00"+proofDigest) ||
		anyToString(binding["component_digest"]) != anyToString(module["component_digest"]) {
		return false
	}
	canonical, ok := recallResponseCanonicalComponentBinding(binding, kind)
	if !ok {
		return false
	}
	canaryScope, scopeOK := recallResponseCanaryScopeFromRequestScope(scope)
	if !scopeOK || anyToInt(canonical["exposure_bucket"], -1) != recallResponseComponentBucket(canaryScope, kind, anyToString(canonical["policy_version"])) {
		return false
	}
	return anyToString(canonical["policy_version"]) != recallResponseCanaryZeroPolicyVersion ||
		anyToString(canonical["arm"]) == recallResponseCanaryArmControl
}

func recallResponseModulePayloadValid(kind string, payload map[string]any, moduleRefs any, proof, scope map[string]any) bool {
	proofSet := map[string]bool{}
	for _, raw := range contextPackAnyList(moduleRefs) {
		proofSet[anyToString(raw)] = true
	}
	refs := func(key string) bool { return recallResponseModuleRefsWithin(payload[key], proofSet) }
	optionalRef := func(key string) bool {
		value, ok := payload[key].(string)
		return ok && (value == "" || proofSet[value])
	}
	switch kind {
	case "exact_current_status":
		return recallResponseOneOf(anyToString(payload["status"]), "supported", "verify", "abstain") && anyToString(payload["value"]) == "bounded_current_state" && refs("qualifier_refs")
	case "decision_rationale":
		_, decisionOK := payload["decision"].(string)
		return decisionOK && refs("rationale_refs") && refs("constraint_refs") && refs("rejected_alternative_refs")
	case "project_continuation":
		return optionalRef("checkpoint_ref") && refs("completed_refs") && refs("open_refs") && refs("blocker_refs") && recallResponseOneOf(anyToString(payload["next_move"]), "inspect_proof", "retrieve_or_verify")
	case "preference_constraint":
		return optionalRef("statement_ref") && anyToString(payload["scope_ref"]) == anyToString(scope["owner_ref"]) && recallResponseExactOrdinal(payload["support_count"], len(proofSet)) && refs("contradiction_refs") && anyToString(payload["sensitivity"]) == "unspecified"
	case "timeline":
		return refs("event_refs") && recallResponseOneOf(anyToString(payload["ordering"]), "source_order", "explicit_status_transitions") &&
			recallResponseUnknownPeriodsValid(payload["unknown_intervals"], proofSet, anyToString(scope["as_of"])) && refs("causal_claim_refs")
	case "procedure":
		return recallResponseActionPayloadValid(payload, proofSet, false)
	case "multi_memory_synthesis":
		if anyToString(payload["conclusion"]) != "bounded_evidence_synthesis" {
			return false
		}
		bridges, ok := payload["bridge_claims"].([]any)
		if !ok || len(bridges) > 1 {
			return false
		}
		for _, raw := range bridges {
			bridge := anyMap(raw)
			if !recallResponseExactFields(bridge, []string{"proof_refs", "basis"}) || anyToString(bridge["basis"]) != "evidence_only" || !recallResponseModuleRefsWithin(bridge["proof_refs"], proofSet) {
				return false
			}
		}
		return true
	case "conflict_supersession":
		winner := anyToString(payload["winner_ref"])
		status := anyToString(payload["resolution_status"])
		if !refs("claim_refs") || !optionalRef("winner_ref") || !optionalRef("resolution_reason_ref") ||
			!recallResponseUnknownPeriodsValid(payload["unknown_periods"], proofSet, anyToString(scope["as_of"])) {
			return false
		}
		return (status == "unresolved" && winner == "") || (status == "proven_superseded" && winner != "")
	case "negative_abstention":
		return recallResponseNegativePayloadValid(payload, proofSet, scope)
	case "memory_to_action":
		return recallResponseActionPayloadValid(payload, proofSet, true)
	default:
		return false
	}
}

func recallResponseModuleShape(module map[string]any) bool {
	if module == nil || len(module) != 8 {
		return false
	}
	for _, key := range []string{"component_ref", "kind", "ordinal", "primary", "proof_refs", "payload", "binding", "component_digest"} {
		if _, ok := module[key]; !ok {
			return false
		}
	}
	kind := anyToString(module["kind"])
	if !recallResponseModuleAllowed(kind) || !recallResponseExactOpaqueID(anyToString(module["component_ref"]), "rrc_") || !recallResponseValidDigest(anyToString(module["component_digest"])) || anyToString(module["component_digest"]) != recallResponseComponentDigest(module) {
		return false
	}
	if !recallResponseStringList(module["proof_refs"]) || len(contextPackAnyList(module["proof_refs"])) > recallResponseMaxProofRefs {
		return false
	}
	payload := anyMap(module["payload"])
	if !recallResponseExactFields(payload, recallResponseModulePayloadFields[kind]) {
		return false
	}
	binding := anyMap(module["binding"])
	if len(binding) != len(recallResponseCanonicalBindingFields) {
		return false
	}
	for _, key := range recallResponseCanonicalBindingFields {
		if _, ok := binding[key]; !ok {
			return false
		}
	}
	canonical, ok := recallResponseCanonicalComponentBinding(binding, kind)
	return ok && anyToString(canonical["component_digest"]) == anyToString(module["component_digest"])
}
