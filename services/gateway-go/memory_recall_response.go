package main

import (
	"encoding/json"
	"math"
	"strings"
)

const (
	recallResponseMaxEvidence  = 16
	recallResponseMaxConflicts = 8
	recallResponseMaxGaps      = 12
	recallResponseMaxReceipts  = 8
)

// composeRecallResponse is the side-effect-free response projection used by
// recall surfaces. It consumes already-materialized retrieval/receipt maps and
// emits only bounded claims and opaque references. It deliberately does not
// read memory, write telemetry, execute actions, or attach a transport
// contract; callers attach the shared validator contract at their boundary.
func composeRecallResponse(input map[string]any) map[string]any {
	contextPack := anyMap(input["context_pack"])
	if len(contextPack) == 0 {
		contextPack = anyMap(input["contextPack"])
	}
	sourceCoverage := anyMap(input["source_coverage"])
	if len(sourceCoverage) == 0 {
		sourceCoverage = anyMap(input["sourceCoverage"])
	}

	query := strings.Join(strings.Fields(anyToString(input["query"])), " ")
	project := strings.TrimSpace(anyToString(input["project"]))
	topicPath := strings.Trim(strings.TrimSpace(anyToString(input["topic_path"])), "/")
	agentID := strings.TrimSpace(firstNonEmptyStrings(anyToString(input["agent_id"]), anyToString(input["agentId"])))
	workspace := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(input["workspace_ref"]), anyToString(input["workspace_id"]), anyToString(input["workspaceId"]),
		anyToString(anyMap(input["learned_activation"])["workspace_ref"]),
	))
	sessionID := strings.TrimSpace(firstNonEmptyStrings(anyToString(input["session_id"]), anyToString(input["sessionId"])))
	taskID := strings.TrimSpace(firstNonEmptyStrings(anyToString(input["task_id"]), anyToString(input["taskId"])))
	taskIdentityID := strings.TrimSpace(firstNonEmptyStrings(anyToString(input["task_identity_id"]), anyToString(input["taskIdentityId"])))
	executionLaneID := strings.TrimSpace(firstNonEmptyStrings(anyToString(input["execution_lane_id"]), anyToString(input["executionLaneId"])))
	retrievalIntent := recallResponseSafeMode(firstNonEmptyStrings(anyToString(input["retrieval_intent"]), "decision"), "decision")
	retrievalMode := recallResponseSafeRetrievalMode(anyToString(input["retrieval_mode"]))

	queryDigest := "sha256:" + sha256Hex(firstNonEmptyStrings(query, "<empty-query>"))
	workspaceRef := recallResponseScopeRef("workspace", workspace)
	projectRef := recallResponseScopeRef("project", project)
	scopeNamespace := "sha256:" + sha256Hex(workspaceRef+"\x00"+projectRef)
	scopeDigest := "sha256:" + sha256Hex(strings.Join([]string{
		queryDigest, workspaceRef, projectRef, topicPath, agentID, sessionID, taskID, taskIdentityID, executionLaneID, retrievalIntent, retrievalMode,
	}, "\x00"))
	scope := map[string]any{
		"scope_digest":       scopeDigest,
		"query_digest":       queryDigest,
		"workspace_ref":      workspaceRef,
		"project_ref":        projectRef,
		"topic_ref":          recallResponseScopedOpaqueRef(scopeNamespace, "topic", topicPath),
		"agent_ref":          recallResponseScopedOpaqueRef(scopeNamespace, "agent", agentID),
		"session_ref":        recallResponseScopedOpaqueRef(scopeNamespace, "session", sessionID),
		"task_ref":           recallResponseScopedOpaqueRef(scopeNamespace, "task", taskID),
		"task_identity_ref":  recallResponseScopedOpaqueRef(scopeNamespace, "task_identity", taskIdentityID),
		"execution_lane_ref": recallResponseScopedOpaqueRef(scopeNamespace, "execution_lane", executionLaneID),
		"retrieval_intent":   retrievalIntent,
	}

	rankedEvidence := contextPackAnyList(contextPack["ranked_evidence"])
	if len(rankedEvidence) == 0 {
		rankedEvidence = contextPackAnyList(contextPack["rankedEvidence"])
	}
	if len(rankedEvidence) == 0 {
		rankedEvidence = contextPackAnyList(input["evidence"])
	}
	evidence := recallResponseEvidenceRefs(rankedEvidence, scopeDigest, recallResponseMaxEvidence)

	conflicts := recallResponseConflicts(input, contextPack, scopeDigest, recallResponseMaxConflicts)
	gaps := recallResponseGaps(input, contextPack, sourceCoverage, rankedEvidence, scopeDigest, len(evidence), len(conflicts), recallResponseMaxGaps)
	confidence := recallResponseConfidence(evidence, sourceCoverage, conflicts, gaps)
	stateStatus := recallResponseStateStatus(len(evidence), len(conflicts), len(gaps))
	classification := recallResponseClassification(input, rankedEvidence, sourceCoverage, retrievalIntent, len(evidence), len(conflicts), len(gaps))

	claimRefs := make([]any, 0, len(evidence))
	for _, raw := range evidence {
		if refID := strings.TrimSpace(anyToString(anyMap(raw)["ref_id"])); refID != "" {
			claimRefs = append(claimRefs, refID)
		}
	}
	basis := []any{"bounded_response_projection", "opaque_proof_references", "explicit_action_boundary"}
	if len(evidence) > 0 {
		basis = append(basis, "ranked_evidence")
	}
	if len(conflicts) > 0 {
		basis = append(basis, "conflict_disclosure")
	}
	if len(gaps) > 0 {
		basis = append(basis, "gap_disclosure")
	}
	answerSummary := "No bounded evidence supports a reliable answer; retrieve or verify before acting."
	answerMode := "abstention"
	if len(evidence) > 0 && len(conflicts) == 0 && len(gaps) == 0 {
		answerSummary = "Bounded evidence supports a response; inspect the proof references before relying on it."
		answerMode = "answer"
	} else if len(evidence) > 0 {
		answerSummary = "Bounded evidence is available, but unresolved limits remain; verify before acting."
		answerMode = "qualified_answer"
	}
	components := recallResponseComponents(classification, len(evidence), len(conflicts), len(gaps), scopeDigest)

	nextActionKind := "retrieve_or_verify"
	nextActionLabel := "Retrieve or verify the remaining proof"
	nextActionReason := "The response is advisory-only and does not authorize external mutation."
	if len(evidence) > 0 && len(conflicts) == 0 && len(gaps) == 0 {
		nextActionKind = "inspect_proof"
		nextActionLabel = "Inspect proof references before relying on the response"
		nextActionReason = "The bounded response has no disclosed conflicts or source gaps, but proof inspection remains required."
	}

	receiptRefs := recallResponseReceiptRefs(input, contextPack, scopeDigest, recallResponseMaxReceipts)
	qualitySampleID, selectionReceiptID := recallResponseDurableReceiptIDs(input)
	attributable := len(evidence) > 0 && len(conflicts) == 0 && len(gaps) == 0 && anyToBool(sourceCoverage["complete"]) && qualitySampleID != "" && selectionReceiptID != ""
	outcomeStatus := "not_attributable"
	if attributable {
		outcomeStatus = "pending_writeback"
	}
	outcomeReceiptID := ""
	if attributable {
		outcomeReceiptID = selectionReceiptID
	}
	response := map[string]any{
		"ok":             true,
		"schema_id":      recallResponseContractID,
		"version":        1,
		"request_scope":  scope,
		"classification": classification,
		"answer": map[string]any{
			"summary":     answerSummary,
			"answer_mode": answerMode,
			"basis":       basis,
			"claim_refs":  claimRefs,
			"components":  components,
			"progressive_disclosure": map[string]any{
				"level":               "summary",
				"available_levels":    []any{"summary", "proof_refs", "next_action"},
				"next_level_requires": "explicit_request_for_bounded_proof_references",
			},
		},
		"state": map[string]any{
			"status":          stateStatus,
			"source_complete": anyToBool(sourceCoverage["complete"]),
			"evidence_count":  len(evidence),
			"conflict_count":  len(conflicts),
			"gap_count":       len(gaps),
			"retrieval_mode":  retrievalMode,
		},
		"evidence":   evidence,
		"confidence": confidence,
		"conflicts":  conflicts,
		"gaps":       gaps,
		"inferences": recallResponseInferences(evidence, confidence, stateStatus),
		"next_action": map[string]any{
			"kind":                  nextActionKind,
			"label":                 nextActionLabel,
			"reason":                nextActionReason,
			"requires_verification": true,
			"authority":             "advisory_only",
			"execution_performed":   false,
		},
		"action_boundary": map[string]any{
			"can_act":               false,
			"requires_confirmation": true,
			"allowed":               []any{"inspect_proof_refs", "retrieve_missing_sources"},
			"forbidden":             []any{"external_mutation", "credential_access", "raw_memory_export"},
			"reason":                "Recall responses provide evidence and advice only; an agent must independently authorize and execute any mutation.",
			"execution_performed":   false,
		},
		"disclosure": map[string]any{
			"bounded":                true,
			"raw_retrieval_included": false,
			"raw_prompt_included":    false,
			"paths_included":         false,
			"secrets_included":       false,
			"inference_boundary":     "Only deterministic response metadata and opaque proof references are returned; no new fact is inferred from omitted or unverified memory.",
			"omission_policy":        "Omitted, pending, quarantined, or conflicting evidence remains disclosed as a gap or conflict and never becomes implicit support.",
		},
		"receipt_refs": receiptRefs,
		"outcome": map[string]any{
			"status":              outcomeStatus,
			"attributable":        attributable,
			"receipt_id":          outcomeReceiptID,
			"execution_performed": false,
		},
		"writeback_required": true,
	}

	response["response_id"] = recallResponseIDForResponse(response)
	response["response_digest"] = recallResponseSemanticDigest(response)
	if binding := anyMap(input["_recall_response_binding"]); len(binding) > 0 {
		// The binding was created from this exact projection before quality
		// persistence. A mismatch is fail-closed: never copy caller/ledger
		// identity fields into a response whose evidence projection differs.
		_ = recallResponseApplyBinding(response, binding)
	}
	return response
}

func recallResponseEvidenceRefs(items []any, scopeDigest string, limit int) []any {
	if limit <= 0 {
		return []any{}
	}
	out := make([]any, 0, minInt(len(items), limit))
	for index, raw := range items {
		if len(out) >= limit {
			break
		}
		item := anyMap(raw)
		kind := recallResponseSafeKind(anyToString(item["kind"]))
		identity := firstNonEmptyStrings(
			anyToString(item["candidate_id"]), anyToString(item["ref_id"]),
			anyToString(item["memory_id"]), anyToString(item["content_digest"]),
			anyToString(item["citation"]), anyToString(item["source"]),
			anyToString(item["file"]), anyToString(item["text"]),
			"evidence-"+anyToString(index),
		)
		refID := recallResponseScopedOpaqueRef(scopeDigest, "evidence", identity+"\x00"+anyToString(index))
		if candidateID := strings.TrimSpace(anyToString(item["candidate_id"])); recallResponseSafeOpaqueID(candidateID) != "" {
			refID = recallResponseSafeOpaqueID(candidateID)
		}
		status, eligible := recallResponseEvidenceStatus(item)
		confidence, confidenceValid := recallResponseEvidenceConfidence(item["confidence"])
		if !eligible || !confidenceValid {
			// Unsafe or policy-omitted rows are represented only through gap
			// disclosures; they can never become supporting evidence.
			continue
		}
		digest := strings.TrimSpace(anyToString(item["content_digest"]))
		if !recallResponseValidDigest(digest) {
			digest = "sha256:" + sha256Hex(firstNonEmptyStrings(anyToString(item["text"]), identity))
		}
		out = append(out, map[string]any{
			"ref_id":         refID,
			"kind":           kind,
			"role":           "support",
			"status":         status,
			"confidence":     roundFloat(confidence, 4),
			"source_ref":     recallResponseScopedOpaqueRef(scopeDigest, "source", anyToString(item["source"])),
			"content_digest": digest,
		})
	}
	return out
}

func recallResponseConflicts(input, contextPack map[string]any, scopeDigest string, limit int) []any {
	rows := []any{}
	rows = append(rows, contextPackAnyList(input["conflicts"])...)
	rows = append(rows, contextPackAnyList(contextPack["conflicts"])...)
	rows = append(rows, contextPackAnyList(contextPack["contradictions"])...)
	for _, raw := range contextPackAnyList(contextPack["proof_claims"]) {
		claim := anyMap(raw)
		if strings.EqualFold(anyToString(claim["proof_status"]), "contested") || len(contextPackAnyList(claim["opposition"])) > 0 {
			rows = append(rows, claim)
		}
	}
	out := []any{}
	seen := map[string]struct{}{}
	for index, raw := range rows {
		if len(out) >= limit {
			break
		}
		item := anyMap(raw)
		identity := firstNonEmptyStrings(anyToString(item["conflict_id"]), anyToString(item["claim_id"]), anyToString(item["ref_id"]), anyToString(item["statement"]), "conflict-"+anyToString(index))
		conflictID := recallResponseScopedOpaqueRef(scopeDigest, "conflict", identity)
		if _, ok := seen[conflictID]; ok {
			continue
		}
		seen[conflictID] = struct{}{}
		support := recallResponseReferenceIDs(item["support"], scopeDigest, 8)
		opposition := recallResponseReferenceIDs(item["opposition"], scopeDigest, 8)
		if len(opposition) == 0 {
			opposition = recallResponseReferenceIDs(item["conflicts"], scopeDigest, 8)
		}
		out = append(out, map[string]any{
			"conflict_id":     conflictID,
			"kind":            recallResponseSafeKind(firstNonEmptyStrings(anyToString(item["kind"]), "contradiction")),
			"status":          "unresolved",
			"support_refs":    support,
			"opposition_refs": opposition,
			"resolution":      "abstain_until_reconciled",
		})
	}
	return out
}

func recallResponseGaps(input, contextPack, sourceCoverage map[string]any, rankedEvidence []any, scopeDigest string, evidenceCount, conflictCount, limit int) []any {
	out := []any{}
	add := func(code, reason string, material bool) {
		if len(out) >= limit {
			return
		}
		for _, raw := range out {
			if anyToString(anyMap(raw)["code"]) == code {
				return
			}
		}
		out = append(out, map[string]any{"code": code, "material": material, "reason": reason, "required_for_action": true})
	}
	if !anyToBool(sourceCoverage["complete"]) {
		if len(anyToStringList(sourceCoverage["pending"], 8)) > 0 {
			add("pending_sources", "Configured sources have not all returned.", true)
		}
		if len(anyToStringList(sourceCoverage["timed_out"], 8)) > 0 {
			add("timed_out_sources", "One or more configured sources timed out.", true)
		}
		if len(anyToStringList(sourceCoverage["failed"], 8)) > 0 {
			add("failed_sources", "One or more configured sources failed.", true)
		}
		add("incomplete_source_coverage", "Source coverage is not complete.", true)
	}
	omitted := len(contextPackAnyList(contextPack["omitted_high_value_refs"]))
	if omitted == 0 {
		omitted = len(contextPackAnyList(contextPack["omittedHighValueRefs"]))
	}
	if omitted > 0 || anyToInt(input["omitted_count"], 0) > 0 {
		add("omitted_high_value_evidence", "Bounded retrieval omitted potentially useful evidence.", true)
	}
	excludedRefs := recallResponseExcludedEvidenceRefs(rankedEvidence, scopeDigest)
	if len(excludedRefs) > 0 {
		before := len(out)
		add("excluded_evidence", "Invalid, quarantined, superseded, or policy-omitted evidence was excluded from support.", true)
		if len(out) > before {
			anyMap(out[len(out)-1])["refs"] = excludedRefs
		}
	}
	if evidenceCount == 0 {
		add("no_bounded_evidence", "No bounded evidence reference survived the response projection.", true)
	}
	if conflictCount > 0 {
		add("unresolved_conflicts", "Conflicting evidence remains unresolved.", true)
	}
	for _, raw := range contextPackAnyList(contextPack["proof_claims"]) {
		claim := anyMap(raw)
		if len(contextPackAnyList(claim["missing_proof"])) > 0 {
			add("missing_claim_proof", "A proof claim has explicit missing proof obligations.", true)
			break
		}
	}
	return out
}

func recallResponseConfidence(evidence []any, sourceCoverage map[string]any, conflicts, gaps []any) map[string]any {
	score := 0.0
	if len(evidence) > 0 {
		for _, raw := range evidence {
			score += clampFloat(anyToFloat(anyMap(raw)["confidence"]), 0, 1)
		}
		score /= float64(len(evidence))
	}
	if !anyToBool(sourceCoverage["complete"]) {
		score -= 0.15
	}
	score -= float64(len(conflicts)) * 0.2
	score -= float64(len(gaps)) * 0.08
	score = clampFloat(score, 0, 1)
	label := "abstain"
	if score >= 0.75 {
		label = "high"
	} else if score >= 0.5 {
		label = "medium"
	} else if score > 0 {
		label = "low"
	}
	basis := []any{"bounded_evidence_confidence"}
	if !anyToBool(sourceCoverage["complete"]) {
		basis = append(basis, "incomplete_source_coverage")
	}
	if len(conflicts) > 0 {
		basis = append(basis, "conflict_penalty")
	}
	if len(gaps) > 0 {
		basis = append(basis, "gap_penalty")
	}
	return map[string]any{"label": label, "score": roundFloat(score, 4), "basis": basis, "calibrated": false}
}

func recallResponseInferences(evidence []any, confidence map[string]any, state string) []any {
	basisRefs := []any{}
	for _, raw := range evidence {
		if refID := anyToString(anyMap(raw)["ref_id"]); refID != "" {
			basisRefs = append(basisRefs, refID)
		}
		if len(basisRefs) >= 8 {
			break
		}
	}
	return []any{map[string]any{
		"inference_id": "inf_" + sha256Hex(strings.Join([]string{state, anyToString(confidence["label"]), anyToString(len(basisRefs))}, "\x00"))[:24],
		"claim_ref":    "response_state",
		"basis_refs":   basisRefs,
		"status":       "deterministic_metadata_only",
		"confidence":   confidence["score"],
		"disclosure":   "This is a response-state inference, not a new memory fact.",
	}}
}

func recallResponseReceiptRefs(input, contextPack map[string]any, scopeDigest string, limit int) []any {
	rows := []struct {
		kind string
		row  map[string]any
	}{
		{"memory_trust", anyMap(input["memory_trust_assessment"])},
		{"retrieval_decision", anyMap(input["retrieval_decision_trace"])},
		{"quality", anyMap(input["context_pack_quality"])},
		{"selection", anyMap(anyMap(input["context_pack_quality"])["selection_receipt"])},
		{"memory_trust", anyMap(contextPack["memory_trust_assessment"])},
		{"retrieval_decision", anyMap(contextPack["retrieval_decision_trace"])},
		{"quality", anyMap(contextPack["context_pack_quality"])},
		{"selection", anyMap(anyMap(contextPack["context_pack_quality"])["selection_receipt"])},
		{"quality", anyMap(input["_durable_context_pack_quality"])},
		{"selection", anyMap(anyMap(input["_durable_context_pack_quality"])["selection_receipt"])},
	}
	for _, raw := range contextPackAnyList(input["decision_receipts"]) {
		rows = append(rows, struct {
			kind string
			row  map[string]any
		}{"decision", anyMap(raw)})
	}
	out := []any{}
	seen := map[string]struct{}{}
	for _, item := range rows {
		if len(out) >= limit || len(item.row) == 0 {
			continue
		}
		selectionReceipt := anyMap(item.row["selection_receipt"])
		identity := firstNonEmptyStrings(
			anyToString(item.row["assessment_id"]), anyToString(item.row["trace_id"]),
			anyToString(item.row["receipt_id"]), anyToString(item.row["decision_id"]),
			anyToString(item.row["sample_id"]), anyToString(item.row["selection_receipt_id"]),
			anyToString(selectionReceipt["receipt_id"]),
		)
		if identity == "" {
			continue
		}
		refID := recallResponseSafeOpaqueID(identity)
		if refID == "" {
			refID = recallResponseScopedOpaqueRef(scopeDigest, "receipt", identity)
		}
		if _, ok := seen[refID]; ok {
			continue
		}
		seen[refID] = struct{}{}
		out = append(out, map[string]any{"ref_id": refID, "kind": item.kind, "status": "observed"})
	}
	return out
}

func recallResponseDurableReceiptIDs(input map[string]any) (string, string) {
	quality := anyMap(input["_durable_context_pack_quality"])
	if len(quality) == 0 {
		return "", ""
	}
	sampleID := recallResponseDurableID(firstNonEmptyStrings(anyToString(quality["sample_id"]), anyToString(quality["context_pack_quality_sample_id"])), "cpq_")
	selection := contextPackSelectionReceiptFromSample(quality["selection_receipt"])
	selectionID := recallResponseDurableID(anyToString(selection["receipt_id"]), "cpr_")
	if sampleID == "" || selectionID == "" || !recallResponseValidDigest(anyToString(selection["receipt_digest"])) {
		return "", ""
	}
	return sampleID, selectionID
}

func recallResponseDurableID(value, prefix string) string {
	value = strings.TrimSpace(value)
	if recallResponseExactOpaqueID(value, prefix) {
		return value
	}
	return ""
}

func recallResponseReferenceIDs(value any, scopeDigest string, limit int) []any {
	out := []any{}
	for index, raw := range contextPackAnyList(value) {
		if len(out) >= limit {
			break
		}
		item := anyMap(raw)
		identity := firstNonEmptyStrings(anyToString(item["ref_id"]), anyToString(item["claim_id"]), anyToString(item["candidate_id"]), anyToString(raw), "ref-"+anyToString(index))
		refID := recallResponseSafeOpaqueID(identity)
		if refID == "" {
			refID = recallResponseScopedOpaqueRef(scopeDigest, "proof", identity)
		}
		out = append(out, refID)
	}
	return out
}

func recallResponseClassification(input map[string]any, rankedEvidence []any, sourceCoverage map[string]any, retrievalIntent string, evidenceCount, conflictCount, gapCount int) map[string]any {
	classification := anyMap(input["classification"])
	defaultJobs := map[string][]string{
		"decision": {"verify", "explain"}, "procedure": {"apply"}, "status": {"look_up", "verify"},
		"proof": {"verify"}, "synthesis": {"explain", "compare"}, "exact": {"look_up"},
		"advisory": {"explain"}, "bounded": {"look_up"},
	}[retrievalIntent]
	if len(defaultJobs) == 0 {
		defaultJobs = []string{"look_up"}
	}
	jobs := recallResponseSafeCodeList(classification["jobs"], defaultJobs, 4)
	objects := recallResponseSafeCodeList(classification["objects"], []string{"durable_memory"}, 4)
	if evidenceCount > 0 {
		objects = recallResponseSafeCodeList(classification["objects"], []string{"durable_memory", "proof_receipt"}, 4)
	}
	temporalMode := recallResponseSafeCode(firstNonEmptyStrings(anyToString(classification["temporal_mode"]), anyToString(input["temporal_mode"])), "current_or_unknown", map[string]bool{
		"current": true, "historical": true, "changed_over_time": true, "ordered_sequence": true,
		"deadline": true, "recurrence": true, "mixed": true, "current_or_unknown": true,
	})
	evidenceState := "absent"
	if evidenceCount > 0 {
		evidenceState = "clean"
	}
	if !anyToBool(sourceCoverage["complete"]) {
		evidenceState = "degraded"
	} else if gapCount > 0 && evidenceCount > 0 {
		evidenceState = "sparse"
	}
	if conflictCount > 0 {
		evidenceState = "conflicting"
	}
	if evidenceCount == 0 {
		for _, raw := range rankedEvidence {
			status, _ := recallResponseEvidenceStatus(anyMap(raw))
			if status == "quarantined" {
				evidenceState = "quarantined"
				break
			}
			if status == "superseded" || status == "retracted" {
				evidenceState = "superseded"
			}
		}
	}
	computedConsequence := "informational"
	if retrievalIntent == "decision" {
		computedConsequence = "decision_supporting"
	} else if retrievalIntent == "procedure" {
		computedConsequence = "executable"
	}
	consequence := recallResponseStricterConsequence(computedConsequence, anyToString(classification["consequence"]))
	posture := "abstain"
	if evidenceCount > 0 && conflictCount == 0 && gapCount == 0 {
		posture = "answer_with_proof"
	} else if evidenceCount > 0 {
		posture = "verify_before_action"
	}
	if evidenceCount == 0 {
		jobs = appendRecallResponseCode(jobs, "verify", 4)
	}
	return map[string]any{
		"jobs":           jobs,
		"objects":        objects,
		"temporal_mode":  temporalMode,
		"evidence_state": evidenceState,
		"consequence":    consequence,
		"posture":        posture,
	}
}

func recallResponseSafeCode(value, fallback string, allowed map[string]bool) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if allowed[value] {
		return value
	}
	return fallback
}

func recallResponseSafeCodeList(value any, fallback []string, limit int) []any {
	allowed := map[string]bool{
		"look_up": true, "verify": true, "reconstruct": true, "explain": true,
		"compare": true, "continue": true, "apply": true, "act": true,
		"fact": true, "event": true, "decision": true, "preference": true,
		"constraint": true, "procedure": true, "project_state": true, "policy": true,
		"relationship": true, "identity": true, "durable_memory": true,
		"proof_receipt": true, "decision_receipt": true,
	}
	values := []string{}
	for _, raw := range contextPackAnyList(value) {
		if code := recallResponseSafeCode(anyToString(raw), "", allowed); code != "" && !containsString(values, code) {
			values = append(values, code)
		}
	}
	if len(values) == 0 {
		values = append(values, fallback...)
	}
	if limit > 0 && len(values) > limit {
		values = values[:limit]
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func appendRecallResponseCode(values []any, code string, limit int) []any {
	for _, raw := range values {
		if anyToString(raw) == code {
			return values
		}
	}
	if limit <= 0 || len(values) < limit {
		values = append(values, code)
	}
	return values
}

func recallResponseStricterConsequence(computed, supplied string) string {
	order := map[string]int{
		"informational": 1, "decision_supporting": 2, "executable": 3, "sensitive": 4, "high_stakes": 5,
	}
	supplied = strings.ToLower(strings.TrimSpace(supplied))
	if order[supplied] > order[computed] {
		return supplied
	}
	return computed
}

func recallResponseComponents(classification map[string]any, evidenceCount, conflictCount, gapCount int, scopeDigest string) []any {
	selected := map[string]bool{}
	add := func(kind string) {
		if kind != "" {
			selected[kind] = true
		}
	}
	for _, raw := range contextPackAnyList(classification["jobs"]) {
		switch anyToString(raw) {
		case "look_up":
			add("exact_current_status")
		case "verify", "explain", "compare":
			add("multi_memory_synthesis")
		case "reconstruct":
			add("timeline")
		case "continue":
			add("project_continuation")
		case "apply":
			add("procedure")
		case "act":
			add("memory_to_action")
		}
	}
	for _, raw := range contextPackAnyList(classification["objects"]) {
		switch anyToString(raw) {
		case "decision":
			add("decision_rationale")
		case "preference", "constraint":
			add("preference_constraint")
		case "event":
			add("timeline")
		case "procedure":
			add("procedure")
		case "project_state":
			add("project_continuation")
		}
	}
	if conflictCount > 0 {
		add("conflict_supersession")
	}
	if evidenceCount == 0 || gapCount > 0 {
		add("negative_abstention")
	}
	if evidenceCount > 0 && len(selected) == 0 {
		add("multi_memory_synthesis")
	}
	order := []string{
		"exact_current_status", "decision_rationale", "project_continuation", "preference_constraint",
		"timeline", "procedure", "multi_memory_synthesis", "conflict_supersession",
		"negative_abstention", "memory_to_action",
	}
	out := []any{}
	for _, kind := range order {
		if !selected[kind] {
			continue
		}
		component := map[string]any{
			"component_ref": "rrc_" + sha256Hex(scopeDigest + "\x00" + kind)[:24],
			"kind":          kind,
			"status":        "included",
			"basis":         "deterministic_faceted_taxonomy",
			"ordinal":       len(out) + 1,
		}
		component["component_digest"] = recallResponseComponentDigest(component)
		out = append(out, component)
	}
	return out
}

func recallResponseStateStatus(evidenceCount, conflictCount, gapCount int) string {
	if evidenceCount == 0 {
		return "abstain"
	}
	if conflictCount > 0 || gapCount > 0 {
		return "verify"
	}
	return "supported"
}

func recallResponseSafeMode(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	allowed := map[string]bool{"bounded": true, "decision": true, "procedure": true, "status": true, "proof": true, "synthesis": true, "exact": true, "advisory": true}
	if allowed[value] {
		return value
	}
	return fallback
}

func recallResponseSafeRetrievalMode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "fast", "balanced", "deep", "bounded", "impact_per_token":
		return value
	default:
		return "balanced"
	}
}

func recallResponseSafeKind(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	allowed := map[string]bool{"fact": true, "decision": true, "risk": true, "check": true, "runbook": true, "capability": true, "graph_neighbor": true, "temporal_claim": true, "contradiction": true, "evidence": true}
	if allowed[value] {
		return value
	}
	return "evidence"
}

func recallResponseSafeStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	allowed := map[string]bool{"selected": true, "selected_truncated": true, "supported": true, "supported_with_limits": true, "contested": true, "quarantined": true, "omitted": true, "stale": true, "superseded": true, "retracted": true, "unknown": true}
	if allowed[value] {
		return value
	}
	return "unknown"
}

func recallResponseSafeOpaqueID(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"rtc_", "mta_", "rdr_", "rdt_", "synth_claim_", "claim_", "cpq_", "cpr_", "cpo_", "rr_", "rrc_", "inf_"} {
		if recallResponseExactOpaqueID(value, prefix) {
			return value
		}
	}
	return ""
}

func recallResponseExactOpaqueID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+24 {
		return false
	}
	for _, ch := range value[len(prefix):] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func recallResponseEvidenceStatus(item map[string]any) (string, bool) {
	if rawStatus := strings.TrimSpace(firstNonEmptyStrings(anyToString(item["status"]), anyToString(item["proof_status"]))); rawStatus != "" {
		status := recallResponseSafeStatus(rawStatus)
		switch status {
		case "selected", "selected_truncated", "supported", "supported_with_limits":
			return status, true
		default:
			return status, false
		}
	}

	// Context-pack evidence carries recency in freshness, not proof status.
	// Ordinary current, dated, and undated rows remain eligible; stale and
	// superseded rows are disclosed as excluded gaps and never become support.
	switch freshness := strings.ToLower(strings.TrimSpace(anyToString(item["freshness"]))); freshness {
	case "", "current", "recent", "aging", "dated", "undated":
		return "selected", true
	case "stale", "superseded":
		return freshness, false
	default:
		return "unknown", false
	}
}

func recallResponseEvidenceConfidence(value any) (float64, bool) {
	if value == nil {
		return 0.5, true
	}
	var score float64
	switch typed := value.(type) {
	case float64:
		score = typed
	case float32:
		score = float64(typed)
	case int:
		score = float64(typed)
	case int8:
		score = float64(typed)
	case int16:
		score = float64(typed)
	case int32:
		score = float64(typed)
	case int64:
		score = float64(typed)
	case uint:
		score = float64(typed)
	case uint8:
		score = float64(typed)
	case uint16:
		score = float64(typed)
	case uint32:
		score = float64(typed)
	case uint64:
		score = float64(typed)
	default:
		return 0, false
	}
	if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
		return 0, false
	}
	return score, true
}

func recallResponseValidDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, ch := range value[len("sha256:"):] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func recallResponseExcludedEvidenceRefs(items []any, scopeDigest string) []any {
	out := []any{}
	seen := map[string]struct{}{}
	for index, raw := range items {
		item := anyMap(raw)
		status, eligible := recallResponseEvidenceStatus(item)
		_, confidenceValid := recallResponseEvidenceConfidence(item["confidence"])
		if eligible && confidenceValid {
			continue
		}
		if !confidenceValid {
			status = "invalid_confidence"
		}
		identity := firstNonEmptyStrings(anyToString(item["candidate_id"]), anyToString(item["ref_id"]), anyToString(item["memory_id"]), anyToString(item["content_digest"]), "excluded-"+anyToString(index))
		refID := recallResponseSafeOpaqueID(identity)
		if refID == "" {
			refID = recallResponseScopedOpaqueRef(scopeDigest, "excluded", identity)
		}
		if _, ok := seen[refID]; ok {
			continue
		}
		seen[refID] = struct{}{}
		out = append(out, map[string]any{"ref_id": refID, "status": status, "role": "excluded"})
	}
	return out
}

func recallResponseScopedOpaqueRef(scopeDigest, kind, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "<empty>"
	}
	return "ref_" + strings.ToLower(strings.TrimSpace(kind)) + "_" + sha256Hex(scopeDigest + "\x00" + value)[:24]
}

func recallResponseScopeRef(kind, value string) string {
	value = strings.TrimSpace(value)
	if recallResponseValidDigest(value) {
		return value
	}
	if ref := contextPackLearnedScopeRef(kind, value); ref != "" {
		return ref
	}
	return "sha256:" + sha256Hex("recall-response-scope\x00"+kind+"\x00<absent>")
}

func recallResponseCanonicalJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
