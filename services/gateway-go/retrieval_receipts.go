package main

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	memoryTrustAssessmentContractID  = "memory_trust_assessment.v1"
	retrievalDecisionTraceContractID = "retrieval_decision_trace.v1"
	retrievalReceiptMaxCandidates    = 128
	retrievalReceiptMaxDecisions     = 160
	retrievalReceiptMaxReasons       = 12
)

var retrievalReceiptBase64Pattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9+/])([A-Za-z0-9+/]{40,}={0,2})(?:$|[^A-Za-z0-9+/])`)

type retrievalDecisionRecord struct {
	CandidateID string
	Occurrence  int
	Decision    string
	Reasons     []string
	WhyNow      []string
	Metadata    map[string]any
}

type retrievalTrustResult struct {
	Eligible                []contextPackEvidenceItem
	PreDecisions            []retrievalDecisionRecord
	TrustEnvelope           map[string]any
	CandidateCount          int
	ProcessedCandidateCount int
	InputTruncatedCount     int
}

func memoryTrustAssessmentForCandidate(kind, text string, metadata map[string]any) map[string]any {
	normalizedText := normalizeEvidenceText(text)
	contentDigest := "sha256:" + sha256Hex(normalizedText)
	project := retrievalReceiptPortable(metadata["project"], 160)
	fileName := retrievalReceiptPortable(metadata["file"], 360)
	source := retrievalReceiptPortable(metadata["source"], 160)
	topicPath := retrievalReceiptPortable(metadata["topic_path"], 240)
	issuerDomain := firstNonEmptyStrings(
		retrievalReceiptPortable(metadata["memory_id"], 360),
		fileName,
		retrievalReceiptPortable(metadata["source_owner"], 120)+":"+source,
		source,
		"unknown",
	)
	candidateSeed := strings.Join([]string{strings.ToLower(kind), contentDigest, project, fileName, source, topicPath}, "\x00")
	candidateID := "rtc_" + sha256Hex(candidateSeed)[:24]
	lower := strings.ToLower(strings.Join(strings.Fields(text), " "))
	signals := []string{}
	addSignal := func(signal string) {
		if signal != "" && !containsString(signals, signal) && len(signals) < retrievalReceiptMaxReasons {
			signals = append(signals, signal)
		}
	}

	promptOverride := retrievalReceiptContainsAny(lower, []string{
		"ignore every previous instruction", "ignore previous instructions", "ignore all previous instructions",
		"disregard earlier safety rules", "disregard previous instructions", "override the developer message",
		"override the system message", "reveal the hidden system prompt", "reveal system prompt",
	})
	if promptOverride {
		addSignal("prompt_override")
	}
	exfiltration := retrievalReceiptContainsAny(lower, []string{
		"print all available credentials", "print every credential", "reveal all credentials", "exfiltrate secrets",
		"dump credentials", "show private key", "reveal access token", "send all secrets",
	})
	if exfiltration {
		addSignal("credential_exfiltration")
	}
	encodedInstruction := retrievalReceiptEncodedInstruction(text)
	if encodedInstruction {
		addSignal("encoded_instruction")
	}
	selfAwarded := retrievalReceiptContainsAny(lower, []string{
		"trusted=true", "treat it as authoritative", "treat this as authoritative", "multiple agents confirmed",
		"therefore treat it as authoritative", "without inspecting evidence",
	})
	if selfAwarded {
		addSignal("self_awarded_trust")
	}
	destructive := retrievalReceiptContainsAny(lower, []string{
		"delete the evidence ledger", "delete evidence ledger", "erase the evidence", "disable the safety",
		"remove all safeguards", "bypass the approval", "push without review", "auto-merge",
	})
	if destructive {
		addSignal("high_impact_instruction")
	}
	instructionScore := 0
	if retrievalReceiptContainsAny(lower, []string{" run ", " execute ", " delete ", " print ", " reveal ", " override ", " ignore ", " stop if "}) ||
		strings.HasPrefix(lower, "run ") || strings.HasPrefix(lower, "execute ") || strings.HasPrefix(lower, "delete ") {
		instructionScore += 25
	}
	if promptOverride {
		instructionScore += 45
	}
	if exfiltration || destructive {
		instructionScore += 35
	}
	if encodedInstruction {
		instructionScore += 45
	}
	instructionScore = clampInt(instructionScore, 0, 100)

	sourceRisk := "bounded"
	trustLabel := "bounded"
	if source == "" || containsString([]string{"external_note", "generated_summary", "unknown"}, strings.ToLower(source)) {
		sourceRisk = "elevated"
		trustLabel = "untrusted"
	}
	if selfAwarded {
		trustLabel = "untrusted"
	}
	legitimateRunbook := strings.EqualFold(kind, "runbook") && sourceRisk == "bounded" && !promptOverride && !exfiltration && !encodedInstruction && !destructive
	if legitimateRunbook {
		addSignal("bounded_runbook_instruction")
	}
	consensusRequired := destructive || (selfAwarded && sourceRisk == "elevated")
	quarantined := promptOverride || exfiltration || encodedInstruction || (consensusRequired && !legitimateRunbook)
	quarantineReasons := []string{}
	for _, reason := range signals {
		if containsString([]string{"prompt_override", "credential_exfiltration", "encoded_instruction", "high_impact_instruction", "self_awarded_trust"}, reason) {
			quarantineReasons = append(quarantineReasons, reason)
		}
	}
	if !quarantined {
		quarantineReasons = []string{}
	}
	multiplier := 1.0
	if trustLabel == "untrusted" {
		multiplier = 0.75
	}
	if quarantined {
		multiplier = 0
		trustLabel = "quarantined"
	}
	return map[string]any{
		"schema_id":      memoryTrustAssessmentContractID,
		"version":        1,
		"assessment_id":  "mta_" + sha256Hex(candidateID + "\x00" + contentDigest)[:24],
		"candidate_id":   candidateID,
		"content_digest": contentDigest,
		"issuer": map[string]any{
			"label": source, "domain_ref": evidenceReputationOpaqueRef("issuer", issuerDomain),
			"server_observed": true, "self_declared_authority_accepted": false,
		},
		"provenance": map[string]any{
			"project": project, "file": fileName, "source": source, "topic_path": topicPath,
			"risk": sourceRisk, "server_observed": true,
		},
		"trust_label": trustLabel,
		"instruction_shape": map[string]any{
			"score": instructionScore, "signals": retrievalReceiptStrings(signals, retrievalReceiptMaxReasons),
			"legitimate_bounded_runbook": legitimateRunbook,
		},
		"duplicate_campaign": map[string]any{
			"detected": false, "cluster_size": 1, "similarity_floor": 0.0,
		},
		"consensus": map[string]any{
			"required": consensusRequired, "independent_issuer_count": 1,
			"minimum_independent_issuers": 2, "met": !consensusRequired,
		},
		"trust_isolation": map[string]any{
			"evidence_only": true, "instruction_authority": false,
			"policy_authority": false, "behavior_authority": false,
		},
		"quarantine": map[string]any{
			"quarantined": quarantined, "reasons": retrievalReceiptStrings(quarantineReasons, retrievalReceiptMaxReasons),
			"fail_closed": true,
		},
		"influence": map[string]any{
			"ranking_allowed": !quarantined, "multiplier": multiplier,
		},
		"self_awarded_trust_rejected": selfAwarded,
		"bounded":                     true,
	}
}

// memoryTrustAssessmentForServerCandidate preserves a canonical identity that
// was attached by the gateway after source-row normalization. Backend JSON
// cannot satisfy the typed provenance marker, so reporter-controlled rows still
// use the derived assessment above. A source content digest is an identity
// fact, not a trust assertion; the trust policy remains computed from the
// normalized summary and metadata.
func memoryTrustAssessmentForServerCandidate(kind, text string, metadata map[string]any, candidateID, contentDigest string) map[string]any {
	assessment := memoryTrustAssessmentForCandidate(kind, text, metadata)
	if !recallResponseExactOpaqueID(candidateID, "rtc_") || !recallResponseValidDigest(contentDigest) {
		return assessment
	}
	assessment["candidate_id"] = candidateID
	assessment["content_digest"] = contentDigest
	assessment["assessment_id"] = "mta_" + sha256Hex(candidateID + "\x00" + contentDigest)[:24]
	return assessment
}

func applyMemoryTrustPolicy(items []contextPackEvidenceItem) retrievalTrustResult {
	inputCandidateCount := len(items)
	if len(items) > retrievalReceiptMaxCandidates {
		items = items[:retrievalReceiptMaxCandidates]
	}
	processedCandidateCount := len(items)
	inputTruncatedCount := maxInt(0, inputCandidateCount-processedCandidateCount)
	for index := range items {
		assessment := retrievalReceiptPrecomputedAssessment(items[index].TrustAssessment)
		if len(assessment) == 0 {
			metadata := map[string]any{
				"project": items[index].Project, "file": items[index].File, "source": items[index].Source,
				"topic_path": items[index].TopicPath, "source_owner": items[index].SourceOwner,
				"memory_id": items[index].MemoryID,
			}
			if items[index].GatewaySourceObserved {
				assessment = memoryTrustAssessmentForServerCandidate(
					items[index].Kind, items[index].Text, metadata,
					items[index].CandidateID, items[index].ContentDigest,
				)
			} else {
				assessment = memoryTrustAssessmentForCandidate(items[index].Kind, items[index].Text, metadata)
			}
		}
		items[index].CandidateID = anyToString(assessment["candidate_id"])
		items[index].ContentDigest = anyToString(assessment["content_digest"])
		items[index].TrustAssessment = assessment
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		if items[i].CandidateID != items[j].CandidateID {
			return items[i].CandidateID < items[j].CandidateID
		}
		return items[i].Occurrence < items[j].Occurrence
	})

	clusterSizes := make([]int, len(items))
	clusterMinSimilarity := make([]float64, len(items))
	for index := range clusterSizes {
		clusterSizes[index] = 1
		clusterMinSimilarity[index] = 1
	}
	for left := 0; left < len(items); left++ {
		for right := left + 1; right < len(items); right++ {
			if items[left].ContentDigest == items[right].ContentDigest {
				continue
			}
			similarity := retrievalReceiptJaccard(items[left].Text, items[right].Text)
			leftRisk := anyToInt(anyMap(items[left].TrustAssessment["instruction_shape"])["score"], 0) >= 45
			rightRisk := anyToInt(anyMap(items[right].TrustAssessment["instruction_shape"])["score"], 0) >= 45
			if !leftRisk && !rightRisk {
				continue
			}
			if similarity < 0.62 && (similarity < 0.18 || !retrievalReceiptSharedRiskSignal(items[left].TrustAssessment, items[right].TrustAssessment)) {
				continue
			}
			clusterSizes[left]++
			clusterSizes[right]++
			clusterMinSimilarity[left] = minFloat(clusterMinSimilarity[left], similarity)
			clusterMinSimilarity[right] = minFloat(clusterMinSimilarity[right], similarity)
		}
	}
	for index := range items {
		if clusterSizes[index] < 2 {
			continue
		}
		campaign := anyMap(items[index].TrustAssessment["duplicate_campaign"])
		campaign["detected"] = true
		campaign["cluster_size"] = clusterSizes[index]
		campaign["similarity_floor"] = roundFloat(clusterMinSimilarity[index], 4)
		campaign["coordinated_trust_credit_allowed"] = false
		items[index].TrustAssessment["duplicate_campaign"] = campaign
		quarantine := anyMap(items[index].TrustAssessment["quarantine"])
		quarantine["quarantined"] = true
		quarantine["reasons"] = appendUniqueAny(contextPackAnyList(quarantine["reasons"]), []any{"duplicate_campaign"}, retrievalReceiptMaxReasons)
		items[index].TrustAssessment["quarantine"] = quarantine
		items[index].TrustAssessment["trust_label"] = "quarantined"
		items[index].TrustAssessment["influence"] = map[string]any{"ranking_allowed": false, "multiplier": 0.0}
	}

	seenContent := map[string]struct{}{}
	eligible := make([]contextPackEvidenceItem, 0, len(items))
	decisions := make([]retrievalDecisionRecord, 0)
	assessments := make([]any, 0, len(items))
	quarantineCount := 0
	deduplicatedCount := 0
	policyOmittedCount := 0
	for _, item := range items {
		assessments = append(assessments, retrievalReceiptAssessmentProjection(item.TrustAssessment))
		metadata := retrievalReceiptItemMetadata(item)
		quarantine := anyMap(item.TrustAssessment["quarantine"])
		if anyToBool(quarantine["quarantined"]) {
			quarantineCount++
			decisions = append(decisions, retrievalDecisionRecord{
				CandidateID: item.CandidateID, Occurrence: item.Occurrence, Decision: "quarantined",
				Reasons: anyToStringList(quarantine["reasons"], retrievalReceiptMaxReasons), Metadata: metadata,
			})
			continue
		}
		if _, duplicate := seenContent[item.ContentDigest]; duplicate {
			deduplicatedCount++
			decisions = append(decisions, retrievalDecisionRecord{
				CandidateID: item.CandidateID, Occurrence: item.Occurrence, Decision: "deduplicated",
				Reasons: []string{"exact_duplicate_content"}, Metadata: metadata,
			})
			continue
		}
		seenContent[item.ContentDigest] = struct{}{}
		if item.Freshness == "superseded" || retrievalReceiptContainsAny(strings.ToLower(item.Status), []string{"superseded", "retracted", "obsolete", "deprecated"}) {
			policyOmittedCount++
			decisions = append(decisions, retrievalDecisionRecord{
				CandidateID: item.CandidateID, Occurrence: item.Occurrence, Decision: "omitted",
				Reasons: []string{"superseded_or_retracted_policy"}, Metadata: metadata,
			})
			continue
		}
		multiplier := clampFloat(anyToFloat(anyMap(item.TrustAssessment["influence"])["multiplier"]), 0, 1)
		item.Score *= multiplier
		item.ImpactScore *= multiplier
		item.ValueDensity = roundFloat(item.Score/float64(maxInt(item.EstimatedTokens, 1)), 4)
		item.Confidence = roundFloat(clampFloat(item.Confidence*multiplier, 0.05, 0.99), 3)
		item.WhyNow = retrievalReceiptWhyNow(item)
		eligible = append(eligible, item)
	}
	return retrievalTrustResult{
		Eligible: eligible, PreDecisions: decisions, CandidateCount: inputCandidateCount,
		ProcessedCandidateCount: processedCandidateCount, InputTruncatedCount: inputTruncatedCount,
		TrustEnvelope: map[string]any{
			"ok": true, "schema_id": memoryTrustAssessmentContractID, "version": 1,
			"input_candidate_count": inputCandidateCount, "processed_candidate_count": processedCandidateCount,
			"input_truncated_count": inputTruncatedCount,
			"assessed_count":        len(assessments), "quarantine_count": quarantineCount,
			"deduplicated_count": deduplicatedCount, "policy_omitted_count": policyOmittedCount,
			"assessments": assessments, "bounded": true,
			"input_boundary": map[string]any{
				"maximum_candidates": retrievalReceiptMaxCandidates,
				"truncated":          inputTruncatedCount > 0, "omitted_count": inputTruncatedCount,
				"reason": "bounded_candidate_scan_limit",
			},
			"policy": map[string]any{
				"retrieved_memory_is_evidence_not_instruction": true,
				"self_awarded_trust_accepted":                  false, "security_defenses_fail_closed": true,
			},
		},
	}
}

func retrievalReceiptPrecomputedAssessment(raw map[string]any) map[string]any {
	if len(raw) == 0 ||
		!strings.HasPrefix(anyToString(raw["assessment_id"]), "mta_") ||
		!strings.HasPrefix(anyToString(raw["candidate_id"]), "rtc_") ||
		!strings.HasPrefix(anyToString(raw["content_digest"]), "sha256:") ||
		!anyToBool(anyMap(raw["issuer"])["server_observed"]) ||
		!anyToBool(anyMap(raw["provenance"])["server_observed"]) ||
		len(anyMap(raw["quarantine"])) == 0 ||
		len(anyMap(raw["influence"])) == 0 {
		return nil
	}
	return cloneJSONMap(raw)
}

func retrievalReceiptMergeInputBoundary(trust retrievalTrustResult, raw any) retrievalTrustResult {
	boundary := anyMap(raw)
	upstreamOmitted := maxInt(0, anyToInt(boundary["source_omitted_count"], 0))
	if upstreamOmitted == 0 {
		return trust
	}
	trust.CandidateCount += upstreamOmitted
	trust.InputTruncatedCount += upstreamOmitted
	trust.TrustEnvelope["input_candidate_count"] = trust.CandidateCount
	trust.TrustEnvelope["input_truncated_count"] = trust.InputTruncatedCount
	inputBoundary := anyMap(trust.TrustEnvelope["input_boundary"])
	inputBoundary["truncated"] = true
	inputBoundary["omitted_count"] = trust.InputTruncatedCount
	inputBoundary["upstream_source_candidate_count"] = maxInt(0, anyToInt(boundary["source_candidate_count"], 0))
	inputBoundary["upstream_source_retained_count"] = maxInt(0, anyToInt(boundary["source_retained_count"], 0))
	inputBoundary["upstream_source_omitted_count"] = upstreamOmitted
	inputBoundary["reason"] = "context_pack_source_limits_or_bounded_candidate_scan_limit"
	trust.TrustEnvelope["input_boundary"] = inputBoundary
	return trust
}

func buildRetrievalDecisionTrace(
	trust retrievalTrustResult,
	selected []contextPackEvidenceItem,
	omitted []contextPackEvidenceItem,
	tokenBudget contextPackTokenBudget,
	promotion ...map[string]any,
) map[string]any {
	decisions := append([]retrievalDecisionRecord{}, trust.PreDecisions...)
	for _, item := range selected {
		decision := "selected"
		reasons := []string{"highest_bounded_impact_per_token"}
		if item.DisplayTruncated || containsAnyInList(item.WhySelected, "compressed_to_fit_budget") {
			decision = "selected_truncated"
			reasons = append(reasons, "display_or_budget_truncated")
		}
		decisions = append(decisions, retrievalDecisionRecord{
			CandidateID: item.CandidateID, Occurrence: item.Occurrence, Decision: decision,
			Reasons: reasons, WhyNow: item.WhyNow, Metadata: retrievalReceiptItemMetadata(item),
		})
	}
	for _, item := range omitted {
		reason := "candidate_limit_or_lower_marginal_value"
		if tokenBudget.Active {
			reason = "token_budget_or_lower_marginal_value"
		}
		decision := "omitted"
		if item.DisplayTruncated {
			decision = "omitted_truncated"
		}
		decisions = append(decisions, retrievalDecisionRecord{
			CandidateID: item.CandidateID, Occurrence: item.Occurrence, Decision: decision,
			Reasons: []string{reason}, WhyNow: item.WhyNow, Metadata: retrievalReceiptItemMetadata(item),
		})
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		if decisions[i].CandidateID != decisions[j].CandidateID {
			return decisions[i].CandidateID < decisions[j].CandidateID
		}
		if decisions[i].Occurrence != decisions[j].Occurrence {
			return decisions[i].Occurrence < decisions[j].Occurrence
		}
		return decisions[i].Decision < decisions[j].Decision
	})
	if len(decisions) > retrievalReceiptMaxDecisions {
		decisions = decisions[:retrievalReceiptMaxDecisions]
	}
	rendered := make([]any, 0, len(decisions))
	counts := map[string]int{}
	for index, decision := range decisions {
		counts[decision.Decision]++
		rendered = append(rendered, map[string]any{
			"receipt_id":   "rdr_" + sha256Hex(decision.CandidateID + "\x00" + anyToString(decision.Occurrence) + "\x00" + decision.Decision)[:24],
			"candidate_id": decision.CandidateID, "candidate_ordinal": decision.Occurrence,
			"decision": decision.Decision, "reasons": retrievalReceiptStrings(decision.Reasons, retrievalReceiptMaxReasons),
			"why_now": retrievalReceiptStrings(decision.WhyNow, 6), "metadata": decision.Metadata,
			"decision_order": index + 1,
		})
	}
	marginalReason := "all_eligible_candidates_selected"
	if len(omitted) > 0 && tokenBudget.Active {
		marginalReason = "token_budget_exhausted_or_lower_value_density"
	} else if len(omitted) > 0 {
		marginalReason = "ranked_candidate_limit_reached"
	}
	trace := map[string]any{
		"ok": true, "schema_id": retrievalDecisionTraceContractID, "version": 1,
		"trace_id":        "rdt_" + sha256Hex(retrievalDecisionTraceDigestBasis(rendered, marginalReason))[:24],
		"candidate_count": trust.CandidateCount, "processed_candidate_count": trust.ProcessedCandidateCount,
		"input_truncated_count": trust.InputTruncatedCount, "decision_count": len(rendered),
		"coverage_complete": len(rendered) == trust.ProcessedCandidateCount && trust.InputTruncatedCount == 0,
		"decisions":         rendered, "decision_counts": counts,
		"input_boundary": map[string]any{
			"maximum_candidates": retrievalReceiptMaxCandidates,
			"truncated":          trust.InputTruncatedCount > 0, "omitted_count": trust.InputTruncatedCount,
			"reason": "bounded_candidate_scan_limit",
		},
		"marginal_stop": map[string]any{
			"stopped": true, "reason": marginalReason,
			"token_budget_active": tokenBudget.Active,
		},
		"redaction": map[string]any{"raw_candidate_text_included": false, "secret_values_included": false},
		"bounded":   true,
	}
	if len(promotion) > 0 && len(promotion[0]) > 0 {
		trace["promotion"] = cloneJSONMap(promotion[0])
	}
	return trace
}

func retrievalDecisionTraceDigestBasis(decisions []any, marginalReason string) string {
	parts := make([]string, 0, len(decisions)+1)
	for _, raw := range decisions {
		row := anyMap(raw)
		parts = append(parts, strings.Join([]string{
			anyToString(row["candidate_id"]), anyToString(row["candidate_ordinal"]), anyToString(row["decision"]),
		}, "\x00"))
	}
	parts = append(parts, marginalReason)
	return strings.Join(parts, "\x01")
}

func retrievalReceiptAssessmentProjection(assessment map[string]any) map[string]any {
	return map[string]any{
		"assessment_id": assessment["assessment_id"], "candidate_id": assessment["candidate_id"],
		"content_digest": assessment["content_digest"], "issuer": assessment["issuer"],
		"provenance": assessment["provenance"], "trust_label": assessment["trust_label"],
		"instruction_shape": assessment["instruction_shape"], "duplicate_campaign": assessment["duplicate_campaign"],
		"consensus": assessment["consensus"], "trust_isolation": assessment["trust_isolation"],
		"quarantine": assessment["quarantine"], "influence": assessment["influence"],
		"self_awarded_trust_rejected": assessment["self_awarded_trust_rejected"],
	}
}

func memoryTrustAssessmentReference(assessment map[string]any) map[string]any {
	return map[string]any{
		"schema_id":             memoryTrustAssessmentContractID,
		"canonical_path":        "$.memory_trust_assessment",
		"assessed_count":        anyToInt(assessment["assessed_count"], 0),
		"quarantine_count":      anyToInt(assessment["quarantine_count"], 0),
		"deduplicated_count":    anyToInt(assessment["deduplicated_count"], 0),
		"policy_omitted_count":  anyToInt(assessment["policy_omitted_count"], 0),
		"input_truncated_count": anyToInt(assessment["input_truncated_count"], 0),
	}
}

func retrievalDecisionTraceReference(trace map[string]any) map[string]any {
	return map[string]any{
		"schema_id":             retrievalDecisionTraceContractID,
		"canonical_path":        "$.retrieval_decision_trace",
		"trace_id":              anyToString(trace["trace_id"]),
		"candidate_count":       anyToInt(trace["candidate_count"], 0),
		"decision_count":        anyToInt(trace["decision_count"], 0),
		"input_truncated_count": anyToInt(trace["input_truncated_count"], 0),
		"coverage_complete":     anyToBool(trace["coverage_complete"]),
	}
}

func retrievalReceiptItemMetadata(item contextPackEvidenceItem) map[string]any {
	metadata := map[string]any{
		"kind": item.Kind, "project": retrievalReceiptPortable(item.Project, 160),
		"file": retrievalReceiptPortable(item.File, 360), "source": retrievalReceiptPortable(item.Source, 160),
		"topic_path": retrievalReceiptPortable(item.TopicPath, 240), "freshness": item.Freshness,
		"content_digest": item.ContentDigest, "estimated_tokens": item.EstimatedTokens,
		"trust_label": anyToString(item.TrustAssessment["trust_label"]),
	}
	if item.EvidenceBasis != "" {
		metadata["evidence_basis"] = item.EvidenceBasis
	}
	if strings.HasPrefix(item.SourceContentHash, "sha256:") {
		metadata["source_content_hash"] = item.SourceContentHash
	}
	return metadata
}

func retrievalReceiptWhyNow(item contextPackEvidenceItem) []string {
	lower := strings.ToLower(item.Text)
	out := []string{}
	if item.Kind == "risk" && retrievalReceiptContainsAny(lower, []string{"current blocker", "failed", "must be repaired", "before release"}) {
		out = append(out, "current_blocker")
	}
	if item.Freshness == "current" || item.Freshness == "recent" {
		out = append(out, "fresh_evidence")
	}
	if item.Kind == "check" || item.Kind == "decision" {
		out = append(out, "task_gate")
	}
	if item.QueryRelevance > 0 {
		out = append(out, "query_alignment")
	}
	if len(out) == 0 {
		out = append(out, "bounded_ranked_impact")
	}
	return retrievalReceiptStrings(out, 6)
}

func retrievalReceiptSafePromptLists(rankedEvidence []any) (files, commands, checks, risks, capabilities []string) {
	seenFiles := map[string]struct{}{}
	for _, raw := range rankedEvidence {
		item := anyMap(raw)
		fileName := retrievalReceiptPortable(item["file"], 360)
		if fileName != "" {
			if _, exists := seenFiles[fileName]; !exists && len(files) < 12 {
				seenFiles[fileName] = struct{}{}
				files = append(files, fileName)
			}
		}
		text := clipText(strings.TrimSpace(anyToString(item["text"])), 360)
		if text == "" {
			continue
		}
		switch anyToString(item["kind"]) {
		case "runbook":
			if len(commands) < 8 {
				commands = append(commands, text)
			}
		case "check":
			if len(checks) < 8 {
				checks = append(checks, text)
			}
		case "risk":
			if len(risks) < 8 {
				risks = append(risks, text)
			}
		case "capability":
			if len(capabilities) < 8 {
				capabilities = append(capabilities, text)
			}
		}
	}
	return files, commands, checks, risks, capabilities
}

func retrievalReceiptSanitizeFacts(facts []any) []any {
	out := make([]any, 0, len(facts))
	for _, raw := range facts {
		fact := anyMap(raw)
		if len(fact) == 0 {
			out = append(out, raw)
			continue
		}
		fact = cloneJSONMap(fact)
		for _, key := range []string{"project", "file", "source", "topic_path", "source_owner", "memory_id"} {
			if value := retrievalReceiptPortable(fact[key], 360); value != "" {
				fact[key] = value
			}
		}
		text := firstNonEmptyStrings(anyToString(fact["text"]), anyToString(fact["summary"]), anyToString(fact["claim"]))
		assessment := memoryTrustAssessmentForCandidate("fact", text, fact)
		fact["memory_trust_assessment"] = retrievalReceiptAssessmentProjection(assessment)
		if anyToBool(anyMap(assessment["quarantine"])["quarantined"]) {
			for _, key := range []string{"text", "summary", "claim"} {
				if _, exists := fact[key]; exists {
					fact[key] = "[quarantined retrieved content]"
				}
			}
			fact["quarantined"] = true
		}
		out = append(out, fact)
	}
	return out
}

func retrievalReceiptPortable(value any, limit int) string {
	stats := &portableRedactionStats{}
	return clipText(strings.TrimSpace(portableString(anyToString(value), stats)), limit)
}

func retrievalReceiptStrings(values []string, limit int) []string {
	out := make([]string, 0, minInt(len(values), limit))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || containsString(out, value) {
			continue
		}
		out = append(out, clipText(value, 120))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func retrievalReceiptContainsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func retrievalReceiptEncodedInstruction(value string) bool {
	lower := strings.ToLower(value)
	if !retrievalReceiptContainsAny(lower, []string{"decode", "encoded", "base64", "payload", "obey"}) {
		return false
	}
	match := retrievalReceiptBase64Pattern.FindStringSubmatch(value)
	if len(match) < 2 {
		return false
	}
	raw := match[1]
	if len(raw) > 4096 {
		return true
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(raw, "="))
	}
	if err != nil {
		return true
	}
	decodedLower := strings.ToLower(string(decoded))
	return retrievalReceiptContainsAny(decodedLower, []string{"ignore", "instruction", "obey", "secret", "credential", "exfiltrate", "system prompt"})
}

func retrievalReceiptJaccard(left, right string) float64 {
	leftSet := retrievalReceiptTokenSet(left)
	rightSet := retrievalReceiptTokenSet(right)
	if len(leftSet) == 0 || len(rightSet) == 0 {
		return 0
	}
	intersection := 0
	union := len(leftSet)
	for token := range rightSet {
		if _, exists := leftSet[token]; exists {
			intersection++
		} else {
			union++
		}
	}
	return float64(intersection) / float64(maxInt(union, 1))
}

func retrievalReceiptSharedRiskSignal(left, right map[string]any) bool {
	highRisk := map[string]struct{}{
		"prompt_override": {}, "credential_exfiltration": {}, "encoded_instruction": {},
		"high_impact_instruction": {}, "self_awarded_trust": {},
	}
	leftSignals := anyToStringList(anyMap(left["instruction_shape"])["signals"], retrievalReceiptMaxReasons)
	rightSignals := anyToStringList(anyMap(right["instruction_shape"])["signals"], retrievalReceiptMaxReasons)
	for _, leftSignal := range leftSignals {
		if _, eligible := highRisk[leftSignal]; !eligible {
			continue
		}
		if containsString(rightSignals, leftSignal) {
			return true
		}
	}
	return false
}

func retrievalReceiptTokenSet(value string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := map[string]struct{}{}
	for _, field := range fields {
		if len(field) >= 3 {
			out[field] = struct{}{}
		}
		if len(out) >= 128 {
			break
		}
	}
	return out
}

func containsAnyInList(values []any, target string) bool {
	for _, value := range values {
		if anyToString(value) == target {
			return true
		}
	}
	return false
}
