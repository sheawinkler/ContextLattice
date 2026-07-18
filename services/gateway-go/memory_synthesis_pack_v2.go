package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const synthesisPackV2ContractID = "synthesis_pack.v2"

func (s *server) memorySynthesisPackV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareAuthorizedHeaders(w, r)
	if !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseJSONMap(bodyBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	response, status, execErr := s.buildSynthesisPackV2Response(r.Context(), incomingHeaders, payload, "/memory/synthesis-pack/v2")
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "synthesis_pack_v2_unavailable", "detail": sanitizeProviderOverflowText(execErr.Error())})
		return
	}
	writeJSON(w, status, response)
}

func (s *server) toolsSynthesisPackV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	incomingHeaders, ok := s.prepareToolHeaders(w, r, "/tools/synthesis_pack_v2")
	if !ok {
		return
	}
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	payload["_suppress_final_token_impact_recording"] = true
	response, status, execErr := s.buildSynthesisPackV2Response(r.Context(), incomingHeaders, payload, "/tools/synthesis_pack_v2")
	if execErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "synthesis_pack_v2_unavailable", "detail": sanitizeProviderOverflowText(execErr.Error())})
		return
	}
	schemaID := anyToString(response["schema_id"])
	if schemaID != agentPacketContractID && schemaID != agentPacketDeltaContractID {
		response["tool"] = "synthesis_pack_v2"
		attach := func(value map[string]any) map[string]any {
			return attachPayloadFormatContract(synthesisPackV2ContractID, value, anyToString(value["agent_id"]), "synthesis_pack_v2", "/tools/synthesis_pack_v2")
		}
		response = finalizeFullTransport(response, attach, "tools_synthesis_pack_v2_transport", "serialized_tools_synthesis_pack_v2_response_json")
	}
	s.recordTokenImpact(anyMap(response["token_impact"]))
	writeJSON(w, status, response)
}

func (s *server) buildSynthesisPackV2Response(
	ctx context.Context,
	incomingHeaders http.Header,
	payload map[string]any,
	surface string,
) (map[string]any, int, error) {
	requestPayload := cloneMap(payload)
	packetRequested := agentPacketRequested(requestPayload)
	if strings.TrimSpace(anyToString(requestPayload["retrieval_intent"])) == "" {
		requestPayload["retrieval_intent"] = "proof_synthesis"
	}
	legacyRequest := cloneMap(requestPayload)
	delete(legacyRequest, "output_mode")
	delete(legacyRequest, "projection")
	delete(legacyRequest, "response_mode")
	legacyRequest["_suppress_final_token_impact_recording"] = true
	legacy, status, err := s.buildSynthesisPackResponse(ctx, incomingHeaders, legacyRequest, surface)
	if err != nil || status >= http.StatusBadRequest || !anyToBool(legacy["ok"]) {
		return legacy, status, err
	}
	legacyPack := anyMap(legacy["synthesis_pack"])
	project := strings.TrimSpace(anyToString(legacy["project"]))
	query := strings.TrimSpace(anyToString(legacy["query"]))
	topicPath := strings.Trim(strings.TrimSpace(anyToString(legacy["topic_path"])), "/")
	bridgeAsOf := causalBridgeProjectionAsOf(requestPayload)
	claimCandidates := s.temporalClaims.query(temporalClaimQuery{
		Project: project, AsOf: bridgeAsOf, Limit: 200,
		IncludeExpired: true, IncludeSuperseded: true, IncludeRetracted: true,
	})
	claimRows := relevantTemporalClaims(query, contextPackAnyList(legacyPack["high_signal_findings"]), claimCandidates, 32)
	proofClaims, excluded := proofClaimsFromSynthesis(project, contextPackAnyList(legacyPack["high_signal_findings"]), claimRows)
	contradictions := proofContradictionSummary(claimRows)
	causalChains := proofCausalChains(claimRows)
	graphNeighbors := contextPackAnyList(anyMap(legacy["context_pack"])["graph_neighbors"])
	bridgeClaims := causalBridgeClaimsForProjection(s.temporalClaims, project, graphNeighbors, bridgeAsOf)
	causalBridgeExplanation := causalBridgeExplanationProjection(project, graphNeighbors, bridgeClaims, bridgeAsOf, causalBridgeExplanationMax)
	causalBridgeExplanation["ok"] = true
	causalBridgeExplanation = attachPayloadFormatContract(
		causalBridgeExplanationContractID,
		causalBridgeExplanation,
		anyToString(legacy["agent_id"]),
		"causal_bridge_explanation",
		surface,
	)
	evidenceReputation := attachPayloadFormatContract(
		evidenceReputationContractID,
		s.evidenceReputationSnapshot(project, "", evidenceReputationDefaultMinSample, 8),
		anyToString(legacy["agent_id"]),
		"evidence_reputation",
		surface,
	)
	retrievalPlanInput := map[string]any{
		"query": query, "project": project, "topic_path": topicPath,
		"retrieval_mode": legacy["retrieval_mode"], "retrieval_intent": "proof_synthesis",
		"token_budget":         firstNonNil(requestPayload["token_budget"], requestPayload["max_prompt_tokens"]),
		"evidence_obligations": requestPayload["evidence_obligations"],
	}
	for _, key := range []string{
		"agent_context_budget_tokens", "model_context_window_tokens", "reserved_response_tokens",
		"already_loaded_tokens", "target_context_pack_tokens", "hard_limit_tokens",
	} {
		if value, exists := requestPayload[key]; exists {
			retrievalPlanInput[key] = value
		}
	}
	retrievalPlan := s.buildAdaptiveRetrievalPlan(retrievalPlanInput)
	proofCoverage := proofCoverageSummary(proofClaims, contextPackAnyList(retrievalPlan["evidence_obligations"]))
	decisionGate := proofSynthesisDecisionGate(anyMap(legacyPack["decision_gate"]), proofClaims, contradictions, proofCoverage)
	recommendedActions := synthesisActionsForDecisionGate(decisionGate, contextPackAnyList(legacyPack["recommended_next_actions"]))

	v2Pack := map[string]any{
		"schema_id":                   synthesisPackV2ContractID,
		"version":                     2,
		"generated_at":                nowUTCISO(),
		"project":                     project,
		"query":                       query,
		"topic_path":                  topicPath,
		"summary":                     legacyPack["summary"],
		"proof_claims":                proofClaims,
		"claim_count":                 len(proofClaims),
		"temporal_claims":             temporalClaimMaps(claimRows),
		"contradictions":              contradictions,
		"causal_chains":               causalChains,
		"causal_bridge_explanation":   causalBridgeExplanation,
		"evidence_reputation":         evidenceReputation,
		"proof_coverage":              proofCoverage,
		"decision_gate":               decisionGate,
		"topic_gravity":               legacyPack["topic_gravity"],
		"cross_project_bridges":       legacyPack["cross_project_bridges"],
		"must_not_forget":             proofMustNotForget(legacyPack, proofClaims),
		"recommended_next_actions":    recommendedActions,
		"open_questions":              proofOpenQuestions(legacyPack, contradictions, proofClaims),
		"semantic_tags":               appendUniqueAny(contextPackAnyList(legacyPack["semantic_tags"]), []any{"synthesis_pack_v2", "proof_carrying", "temporal_claim_graph", "causal_bridge_explanation", "evidence_reputation"}, 32),
		"synthesis_quality":           proofSynthesisQuality(legacyPack, proofClaims, contradictions),
		"unsupported_claims_excluded": excluded,
		"limits": []any{
			"Proof is bounded to evidence returned by Context Pack v1 and matching structured temporal claims.",
			"Lexical claim matching is deterministic; it does not imply semantic equivalence.",
			"Unverified temporal claims are labeled and cannot silently raise confidence.",
			"Cross-project and lexical similarity never establish causality without an explicit typed edge and current structured claim proof.",
		},
		"synthesis_trace": map[string]any{
			"mode": "deterministic_proof_v2", "llm_used": false, "surface": surface,
			"basis":              []any{"synthesis_pack.v1 high-signal findings", "context_pack ranked evidence", "temporal_claim.v1 ledger", "retrieval_plan.v1 evidence obligations", "causal_bridge_explanation.v1 projection"},
			"inference_boundary": "Every emitted proof claim has at least one bounded evidence reference; unsupported findings are excluded rather than repaired with model inference.",
		},
	}
	response := map[string]any{
		"ok": true, "schema_id": synthesisPackV2ContractID, "version": 2,
		"project": project, "query": query, "topic_path": topicPath,
		"retrieval_mode": legacy["retrieval_mode"], "retrieval_intent": "proof_synthesis",
		"synthesis_pack":       v2Pack,
		"retrieval_plan":       retrievalPlan,
		"context_pack":         legacy["context_pack"],
		"context_compiler":     legacy["context_compiler"],
		"source_coverage":      legacy["source_coverage"],
		"reference_prompt":     proofSynthesisReferencePrompt(v2Pack),
		"token_impact":         legacy["token_impact"],
		"context_pack_quality": legacy["context_pack_quality"],
		"run_advisor":          legacy["run_advisor"],
		"warnings":             legacy["warnings"],
		"writeback_required":   true,
		"agent_id":             legacy["agent_id"], "session_id": legacy["session_id"],
	}
	for _, key := range []string{"objective_runtime", "objective_hierarchy", "objective_lineage", "agent_runtime"} {
		if value, ok := legacy[key]; ok {
			response[key] = value
		}
	}
	if packetRequested {
		packet := finalizeAgentPacketForRequest(buildAgentPacket(response, requestPayload, agentPacketSurfaceForRoute("synthesis_pack_v2", surface)), requestPayload)
		if !anyToBool(requestPayload["_suppress_final_token_impact_recording"]) {
			s.recordTokenImpact(anyMap(packet["token_impact"]))
		}
		return packet, status, nil
	}
	attach := func(value map[string]any) map[string]any {
		return attachPayloadFormatContract(synthesisPackV2ContractID, value, anyToString(value["agent_id"]), "synthesis_pack_v2", surface)
	}
	response = finalizeFullTransport(response, attach, "synthesis_pack_v2_transport", "serialized_synthesis_pack_v2_response_json")
	if !anyToBool(requestPayload["_suppress_final_token_impact_recording"]) {
		s.recordTokenImpact(anyMap(response["token_impact"]))
	}
	return response, status, nil
}

func relevantTemporalClaims(query string, findings []any, candidates []temporalClaim, limit int) []temporalClaim {
	termSets := [][]string{synthesisPackQueryTokens(query)}
	for _, raw := range findings {
		if text := strings.TrimSpace(anyToString(anyMap(raw)["text"])); text != "" {
			termSets = append(termSets, synthesisPackQueryTokens(text))
		}
	}
	type scoredClaim struct {
		claim         temporalClaim
		score         int
		influenceRank int
		explicitLink  bool
	}
	scores := map[string]int{}
	directIDs := map[string]struct{}{}
	for _, claim := range candidates {
		best := 0
		for _, terms := range termSets {
			best = maxInt(best, temporalClaimTermScore(claim, terms))
		}
		if best > 0 {
			directIDs[claim.ClaimID] = struct{}{}
		}
		scores[claim.ClaimID] = best
	}
	linkedIDs := map[string]struct{}{}
	for _, claim := range candidates {
		if _, direct := directIDs[claim.ClaimID]; direct {
			for _, targetID := range proofTemporalClaimLinkIDs(claim) {
				linkedIDs[targetID] = struct{}{}
			}
		}
		for _, targetID := range proofTemporalClaimLinkIDs(claim) {
			if _, targetDirect := directIDs[targetID]; targetDirect {
				linkedIDs[claim.ClaimID] = struct{}{}
			}
		}
	}
	scored := []scoredClaim{}
	for _, claim := range candidates {
		_, linked := linkedIDs[claim.ClaimID]
		if scores[claim.ClaimID] == 0 && !linked {
			continue
		}
		scored = append(scored, scoredClaim{
			claim: claim, score: scores[claim.ClaimID],
			influenceRank: temporalClaimInfluenceRank(claim), explicitLink: linked,
		})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].explicitLink != scored[j].explicitLink {
			return scored[i].explicitLink
		}
		if scored[i].influenceRank != scored[j].influenceRank {
			return scored[i].influenceRank < scored[j].influenceRank
		}
		if scored[i].score == scored[j].score {
			if scored[i].claim.Confidence == scored[j].claim.Confidence {
				return scored[i].claim.UpdatedAt > scored[j].claim.UpdatedAt
			}
			return scored[i].claim.Confidence > scored[j].claim.Confidence
		}
		return scored[i].score > scored[j].score
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]temporalClaim, 0, len(scored))
	for _, item := range scored {
		out = append(out, item.claim)
	}
	return out
}

func proofTemporalClaimLinkIDs(claim temporalClaim) []string {
	out := make([]string, 0, len(claim.Contradicts)+len(claim.CausedBy)+len(claim.Supersedes))
	out = append(out, claim.Contradicts...)
	out = append(out, claim.CausedBy...)
	out = append(out, claim.Supersedes...)
	return out
}

func proofClaimsFromSynthesis(project string, findings []any, temporal []temporalClaim) ([]any, int) {
	out := []any{}
	excluded := 0
	byID := map[string]temporalClaim{}
	reverseOpposition := map[string][]temporalClaim{}
	for _, claim := range temporal {
		byID[claim.ClaimID] = claim
		for _, targetID := range claim.Contradicts {
			reverseOpposition[targetID] = append(reverseOpposition[targetID], claim)
		}
	}
	for _, raw := range findings {
		finding := anyMap(raw)
		statement := clipText(strings.TrimSpace(anyToString(finding["text"])), 1200)
		if statement == "" {
			excluded++
			continue
		}
		ref := proofEvidenceReference(finding)
		if strings.TrimSpace(anyToString(ref["ref_id"])) == "" {
			excluded++
			continue
		}
		terms := synthesisPackQueryTokens(statement)
		currentMatching := []temporalClaim{}
		historicalMatching := []temporalClaim{}
		nonCurrentMatching := 0
		for _, claim := range temporal {
			if !proofClaimLexicallyMatches(claim, terms) {
				continue
			}
			switch {
			case temporalClaimCanInfluence(claim):
				currentMatching = append(currentMatching, claim)
			case temporalClaimIsHistoricalOpposition(claim):
				historicalMatching = append(historicalMatching, claim)
			default:
				nonCurrentMatching++
			}
		}
		proofSortMatchingClaims(currentMatching, terms)
		proofSortMatchingClaims(historicalMatching, terms)
		currentMatching = proofLimitTemporalClaims(currentMatching, 4)
		historicalMatching = proofLimitTemporalClaims(historicalMatching, 4)
		support := []any{ref}
		opposition := []any{}
		temporalState := "evidence_timestamp_only"
		causedBy := []any{}
		verified := false
		contested := false
		missingCurrentCause := false
		for _, claim := range currentMatching {
			claimRef := proofTemporalClaimReference(claim)
			support = appendProofReferenceUnique(support, claimRef)
			for _, targetID := range claim.Contradicts {
				if target, ok := byID[targetID]; ok {
					switch {
					case temporalClaimIsHistoricalOpposition(target):
						opposition = appendProofReferenceUnique(opposition, proofHistoricalTemporalClaimReference(target))
					case temporalClaimCanInfluence(target):
						opposition = appendProofReferenceUnique(opposition, proofTemporalClaimReference(target))
						contested = true
					default:
						opposition = appendProofReferenceUnique(opposition, proofNonInfluentialTemporalClaimReference(target))
					}
				} else {
					opposition = appendProofReferenceUnique(opposition, map[string]any{"ref_id": targetID, "kind": "temporal_claim", "status": "not_in_bounded_result"})
					contested = true
				}
			}
			for _, opposingClaim := range reverseOpposition[claim.ClaimID] {
				switch {
				case temporalClaimIsHistoricalOpposition(opposingClaim):
					opposition = appendProofReferenceUnique(opposition, proofHistoricalTemporalClaimReference(opposingClaim))
				case temporalClaimCanInfluence(opposingClaim):
					opposition = appendProofReferenceUnique(opposition, proofTemporalClaimReference(opposingClaim))
					contested = true
				default:
					opposition = appendProofReferenceUnique(opposition, proofNonInfluentialTemporalClaimReference(opposingClaim))
				}
			}
			for _, evidence := range claim.Opposition {
				opposition = appendProofReferenceUnique(opposition, map[string]any{
					"ref_id": evidence.RefID, "kind": evidence.Kind, "memory_id": evidence.MemoryID,
					"uri": evidence.URI, "content_hash": evidence.ContentHash, "excerpt": evidence.Excerpt,
					"status": "current", "influence": "opposition",
				})
				contested = true
			}
			temporalState = "active"
			for _, cause := range claim.CausedBy {
				causeClaim, ok := byID[cause]
				switch {
				case ok && temporalClaimCanInfluence(causeClaim):
					causedBy = appendUniqueAny(causedBy, []any{cause}, 16)
				case ok && temporalClaimIsHistoricalOpposition(causeClaim):
					opposition = appendProofReferenceUnique(opposition, proofHistoricalTemporalClaimReference(causeClaim))
					missingCurrentCause = true
				default:
					missingCurrentCause = true
				}
			}
			if anyToString(claim.Verification["status"]) == "verified" {
				verified = true
			}
		}
		for _, claim := range historicalMatching {
			opposition = appendProofReferenceUnique(opposition, proofHistoricalTemporalClaimReference(claim))
		}
		if len(opposition) > 12 {
			opposition = opposition[:12]
		}
		baseConfidence := anyToFloat(finding["confidence"])
		if baseConfidence <= 0 {
			baseConfidence = 0.64
		}
		if baseConfidence > 1 {
			baseConfidence = baseConfidence / 100
		}
		sourceDiversity := 0.55
		if len(support) > 1 {
			sourceDiversity = 0.75
		}
		temporalConsistency := 0.7
		if temporalState == "active" {
			temporalConsistency = 0.95
		}
		contradictionPenalty := 0.0
		if contested {
			contradictionPenalty = 0.28
		}
		verificationBonus := 0.0
		if verified {
			verificationBonus = 0.08
		}
		finalConfidence := baseConfidence*0.5 + sourceDiversity*0.2 + temporalConsistency*0.3 + verificationBonus - contradictionPenalty
		if finalConfidence < 0 {
			finalConfidence = 0
		}
		if finalConfidence > 1 {
			finalConfidence = 1
		}
		missing := []any{}
		if len(currentMatching) == 0 {
			missing = append(missing, "structured temporal validity")
		}
		if nonCurrentMatching > 0 {
			missing = append(missing, "currently valid structured temporal claim")
		}
		if missingCurrentCause {
			missing = append(missing, "current causal antecedent")
		}
		if len(support) == 1 {
			missing = append(missing, "independent corroboration")
		}
		status := "supported"
		if contested {
			status = "contested"
		} else if len(missing) > 0 {
			status = "supported_with_limits"
		}
		claimID := "synth_claim_" + sha256Hex(project + "\x00" + statement + "\x00" + anyToString(ref["ref_id"]))[:24]
		out = append(out, map[string]any{
			"claim_id": claimID, "statement": statement, "claim_type": firstNonEmptyStrings(anyToString(finding["kind"]), "finding"),
			"proof_status": status, "support": support, "opposition": opposition,
			"temporal": map[string]any{
				"state": temporalState, "matching_claim_count": len(currentMatching),
				"historical_opposition_count": proofHistoricalOppositionCount(opposition),
			},
			"causal_chain": causedBy, "missing_proof": missing,
			"confidence": map[string]any{
				"base_evidence": roundFloat(baseConfidence, 4), "source_diversity": sourceDiversity,
				"temporal_consistency": temporalConsistency, "verification_bonus": verificationBonus,
				"contradiction_penalty": contradictionPenalty, "final": roundFloat(finalConfidence, 4),
			},
			"why_it_matters": finding["why_it_matters"],
		})
	}
	return out, excluded
}

func proofSortMatchingClaims(claims []temporalClaim, terms []string) {
	sort.SliceStable(claims, func(i, j int) bool {
		left := temporalClaimTermScore(claims[i], terms)
		right := temporalClaimTermScore(claims[j], terms)
		if left != right {
			return left > right
		}
		if claims[i].Confidence != claims[j].Confidence {
			return claims[i].Confidence > claims[j].Confidence
		}
		return claims[i].ClaimID < claims[j].ClaimID
	})
}

func proofLimitTemporalClaims(claims []temporalClaim, limit int) []temporalClaim {
	if len(claims) > limit {
		return claims[:limit]
	}
	return claims
}

func proofClaimLexicallyMatches(claim temporalClaim, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	score := temporalClaimTermScore(claim, terms)
	minimum := 1
	if len(terms) >= 2 {
		minimum = 2
	}
	return score >= minimum
}

func proofTemporalClaimReference(claim temporalClaim) map[string]any {
	return map[string]any{
		"ref_id": claim.ClaimID, "kind": "temporal_claim", "statement": claim.Statement,
		"status": claim.Status, "confidence": claim.Confidence, "observed_at": claim.ObservedAt,
		"valid_from": claim.ValidFrom, "valid_to": claim.ValidTo,
	}
}

func proofHistoricalTemporalClaimReference(claim temporalClaim) map[string]any {
	ref := proofTemporalClaimReference(claim)
	ref["role"] = "historical_opposition"
	ref["historical"] = true
	ref["influence"] = "none"
	return ref
}

func proofNonInfluentialTemporalClaimReference(claim temporalClaim) map[string]any {
	ref := proofTemporalClaimReference(claim)
	ref["role"] = "temporally_invalid_opposition"
	ref["historical"] = false
	ref["influence"] = "none"
	return ref
}

func proofHistoricalOppositionCount(raw []any) int {
	count := 0
	for _, item := range raw {
		if anyToBool(anyMap(item)["historical"]) {
			count++
		}
	}
	return count
}

func appendProofReferenceUnique(rows []any, candidate map[string]any) []any {
	refID := strings.TrimSpace(anyToString(candidate["ref_id"]))
	if refID == "" {
		return rows
	}
	for _, raw := range rows {
		if strings.EqualFold(strings.TrimSpace(anyToString(anyMap(raw)["ref_id"])), refID) {
			return rows
		}
	}
	return append(rows, candidate)
}

func proofEvidenceReference(finding map[string]any) map[string]any {
	citation := strings.TrimSpace(anyToString(finding["citation"]))
	identity := firstNonEmptyStrings(citation, anyToString(finding["file"]))
	if identity == "" {
		return map[string]any{}
	}
	return map[string]any{
		"ref_id": identity, "kind": firstNonEmptyStrings(anyToString(finding["kind"]), "memory"),
		"citation": citation, "project": finding["project"], "file": finding["file"],
		"source": finding["source"], "topic_path": finding["topic_path"], "timestamp": finding["timestamp"],
		"excerpt": clipText(anyToString(finding["text"]), 500),
	}
}

func proofContradictionSummary(claims []temporalClaim) []any {
	byID := map[string]temporalClaim{}
	for _, claim := range claims {
		byID[claim.ClaimID] = claim
	}
	out := []any{}
	for _, claim := range claims {
		if !temporalClaimCanInfluence(claim) {
			continue
		}
		currentTargets := []any{}
		historicalTargets := []any{}
		for _, targetID := range claim.Contradicts {
			target, ok := byID[targetID]
			switch {
			case ok && temporalClaimIsHistoricalOpposition(target):
				historicalTargets = append(historicalTargets, proofHistoricalTemporalClaimReference(target))
			case ok && !temporalClaimCanInfluence(target):
				// Pending or otherwise non-current claims cannot influence the contradiction gate.
			case ok:
				currentTargets = append(currentTargets, targetID)
			default:
				currentTargets = append(currentTargets, targetID)
			}
		}
		if len(currentTargets) == 0 && len(claim.Opposition) == 0 {
			continue
		}
		out = append(out, map[string]any{
			"claim_id": claim.ClaimID, "statement": claim.Statement,
			"contradicts": currentTargets, "opposition": claim.Opposition,
			"historical_opposition": historicalTargets,
			"state":                 "unresolved", "repair_automatic": false,
		})
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func proofCausalChains(claims []temporalClaim) []any {
	byID := map[string]temporalClaim{}
	for _, claim := range claims {
		byID[claim.ClaimID] = claim
	}
	out := []any{}
	for _, claim := range claims {
		if !temporalClaimCanInfluence(claim) {
			continue
		}
		currentCauses := []any{}
		for _, causeID := range claim.CausedBy {
			cause, ok := byID[causeID]
			if ok && temporalClaimCanInfluence(cause) {
				currentCauses = append(currentCauses, causeID)
			}
		}
		if len(currentCauses) == 0 {
			continue
		}
		out = append(out, map[string]any{
			"claim_id": claim.ClaimID, "caused_by": currentCauses,
			"supersedes": claim.Supersedes, "supersession_role": "historical_lineage_only",
			"branch": claim.Branch, "commit": claim.Commit,
		})
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func proofCoverageSummary(claims []any, obligations []any) map[string]any {
	contested := 0
	limited := 0
	for _, raw := range claims {
		switch anyToString(anyMap(raw)["proof_status"]) {
		case "contested":
			contested++
		case "supported_with_limits":
			limited++
		}
	}
	coverage := 0.0
	if len(claims) > 0 {
		coverage = float64(len(claims)-contested) / float64(len(claims))
	}
	return map[string]any{
		"supported_claims": len(claims) - contested, "contested_claims": contested,
		"limited_claims": limited, "evidence_obligation_count": len(obligations),
		"claim_support_rate": roundFloat(coverage, 4),
	}
}

func proofSynthesisDecisionGate(legacy map[string]any, claims []any, contradictions []any, coverage map[string]any) map[string]any {
	gate := cloneAnyMap(legacy)
	decision := strings.ToLower(strings.TrimSpace(anyToString(gate["decision"])))
	if decision == "" {
		decision = "verify"
	}
	reasons := contextPackAnyList(gate["reasons"])
	supportRate := anyToFloat(coverage["claim_support_rate"])
	switch {
	case decision == "abstain":
		// The proof layer may tighten a gate, never loosen an epistemic refusal.
	case len(contradictions) > 0:
		decision = "verify"
		reasons = append(reasons, "Temporal claim graph contains unresolved contradictions.")
	case len(claims) == 0:
		decision = "verify"
		reasons = append(reasons, "No proof-carrying claim survived bounded evidence validation.")
	case supportRate < 0.6:
		decision = "verify"
		reasons = append(reasons, "Proof support rate is below the action threshold.")
	}
	gate["decision"] = decision
	gate["refusal"] = decision == "abstain"
	gate["reasons"] = agentSessionListLimit(reasons, 6)
	gate["claim_support_rate"] = roundFloat(supportRate, 4)
	gate["supported_claim_count"] = anyToInt(coverage["supported_claims"], 0)
	gate["contradiction_count"] = len(contradictions)
	gate["proof_policy"] = "proof may tighten act to verify or abstain; it never loosens an upstream refusal"
	return gate
}

func proofMustNotForget(legacy map[string]any, proofClaims []any) []any {
	byStatement := map[string]map[string]any{}
	for _, raw := range proofClaims {
		claim := anyMap(raw)
		byStatement[strings.TrimSpace(anyToString(claim["statement"]))] = claim
	}
	out := []any{}
	for _, raw := range contextPackAnyList(legacy["must_not_forget"]) {
		item := anyMap(raw)
		if claim, ok := byStatement[strings.TrimSpace(anyToString(item["text"]))]; ok {
			out = append(out, map[string]any{"claim_id": claim["claim_id"], "statement": claim["statement"], "proof_status": claim["proof_status"]})
		}
	}
	return out
}

func proofOpenQuestions(legacy map[string]any, contradictions []any, proofClaims []any) []any {
	out := append([]any{}, contextPackAnyList(legacy["open_questions"])...)
	if len(contradictions) > 0 {
		out = append(out, map[string]any{"question": "Which opposing claim is current, and what evidence would resolve it?", "reason": "temporal claim graph contains unresolved contradictions", "count": len(contradictions)})
	}
	for _, raw := range proofClaims {
		claim := anyMap(raw)
		if len(contextPackAnyList(claim["missing_proof"])) > 0 {
			out = append(out, map[string]any{"question": "Should missing proof be gathered before relying on this claim?", "claim_id": claim["claim_id"], "missing": claim["missing_proof"]})
			break
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func proofSynthesisQuality(legacy map[string]any, claims []any, contradictions []any) map[string]any {
	legacyQuality := anyMap(legacy["synthesis_quality"])
	score := anyToInt(legacyQuality["score"], 0)
	if len(claims) > 0 {
		score += 8
	}
	if len(contradictions) > 0 {
		score -= minInt(20, len(contradictions)*4)
	}
	return map[string]any{
		"status": firstNonEmptyStrings(anyToString(legacyQuality["status"]), "bounded"),
		"score":  clampInt(score, 0, 100),
		"basis":  appendUniqueAny(contextPackAnyList(legacyQuality["basis"]), []any{"claim-level evidence references", "temporal contradiction disclosure"}, 16),
		"limits": appendUniqueAny(contextPackAnyList(legacyQuality["limits"]), []any{"deterministic lexical temporal-claim matching"}, 12),
	}
}

func proofSynthesisReferencePrompt(pack map[string]any) string {
	lines := []string{"# Synthesis Pack v2", "", clipText(anyToString(pack["summary"]), 900), "", "## Proof-Carrying Claims"}
	for _, raw := range contextPackAnyList(pack["proof_claims"]) {
		claim := anyMap(raw)
		lines = append(lines, fmt.Sprintf("- [%s | confidence %.2f] %s", anyToString(claim["proof_status"]), anyToFloat(anyMap(claim["confidence"])["final"]), clipText(anyToString(claim["statement"]), 600)))
		for _, supportRaw := range contextPackAnyList(claim["support"]) {
			support := anyMap(supportRaw)
			lines = append(lines, "  evidence: "+clipText(firstNonEmptyStrings(anyToString(support["citation"]), anyToString(support["ref_id"])), 500))
		}
		if len(lines) >= 48 {
			break
		}
	}
	if len(contextPackAnyList(pack["contradictions"])) > 0 {
		lines = append(lines, "", "## Unresolved Contradictions", "Do not silently resolve contested claims; gather the missing proof or ask the user.")
	}
	lines = append(lines, "", "Use only the evidence above. Preserve uncertainty, temporal state, and contradiction labels.")
	return clipText(strings.Join(lines, "\n"), 8000)
}

func appendUniqueAny(base []any, extra []any, limit int) []any {
	out := []any{}
	seen := map[string]struct{}{}
	for _, value := range append(base, extra...) {
		key := strings.TrimSpace(strings.ToLower(anyToString(value)))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}
