package main

import (
	"sort"
	"strings"
	"time"
)

const (
	causalBridgeExplanationContractID = "causal_bridge_explanation.v1"
	causalBridgeExplanationMax        = 8
	causalBridgeCandidateScanMax      = 64
	causalBridgeClaimMax              = 256
	causalBridgeReferenceMax          = 12
	causalBridgeAlternativeMax        = 3
	causalBridgeMissingProofMax       = 10
)

type causalBridgeCandidate struct {
	BridgeID       string
	Project        string
	EdgeID         string
	Relation       string
	Direction      string
	Policy         causalBridgeEdgePolicy
	SourceID       string
	TargetID       string
	SourceProject  string
	TargetProject  string
	OtherProject   string
	Score          float64
	SourceResolved bool
	TargetResolved bool
	EdgeEvidence   []any
}

type causalBridgeClaimPair struct {
	Found  bool
	Cause  temporalClaim
	Effect temporalClaim
}

func causalBridgeProjectionAsOf(payload map[string]any) time.Time {
	if parsed, ok := parseTimeBestEffort(anyToString(payload["as_of"])); ok {
		return parsed
	}
	return time.Now().UTC()
}

func causalBridgeClaimsForProjection(store *temporalClaimStore, project string, graphNeighbors []any, asOf time.Time) []temporalClaim {
	if store == nil {
		return []temporalClaim{}
	}
	candidates, _ := causalBridgeCandidates(project, graphNeighbors)
	if len(candidates) == 0 {
		return []temporalClaim{}
	}
	if len(candidates) > causalBridgeExplanationMax {
		candidates = candidates[:causalBridgeExplanationMax]
	}
	projects := map[string]string{}
	addProject := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			projects[strings.ToLower(value)] = value
		}
	}
	addProject(project)
	for _, candidate := range candidates {
		addProject(candidate.SourceProject)
		addProject(candidate.TargetProject)
	}
	projectKeys := make([]string, 0, len(projects))
	for key := range projects {
		projectKeys = append(projectKeys, key)
	}
	sort.Strings(projectKeys)

	byID := map[string]temporalClaim{}
	for _, key := range projectKeys {
		rows := store.query(temporalClaimQuery{
			Project: projects[key], AsOf: asOf, Limit: 64,
			IncludeExpired: true, IncludeSuperseded: true, IncludeRetracted: true,
		})
		for _, claim := range rows {
			byID[claim.ClaimID] = claim
		}
	}
	claims := make([]temporalClaim, 0, len(byID))
	for _, claim := range byID {
		claims = append(claims, claim)
	}
	sort.SliceStable(claims, func(i, j int) bool {
		leftRank := temporalClaimInfluenceRank(claims[i])
		rightRank := temporalClaimInfluenceRank(claims[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if claims[i].Confidence != claims[j].Confidence {
			return claims[i].Confidence > claims[j].Confidence
		}
		return claims[i].ClaimID < claims[j].ClaimID
	})
	if len(claims) > causalBridgeClaimMax {
		claims = claims[:causalBridgeClaimMax]
	}
	return claims
}

func causalBridgeExplanationProjection(project string, graphNeighbors []any, claims []temporalClaim, asOf time.Time, limit int) map[string]any {
	project = strings.TrimSpace(project)
	if asOf.IsZero() {
		asOf = time.Unix(0, 0).UTC()
	} else {
		asOf = asOf.UTC()
	}
	if limit < 1 {
		limit = causalBridgeExplanationMax
	}
	limit = clampInt(limit, 1, causalBridgeExplanationMax)
	candidates, inputTruncated := causalBridgeCandidates(project, graphNeighbors)
	candidateCount := len(candidates)
	truncated := inputTruncated || candidateCount > limit
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	claims = causalBridgeClaimsAt(claims, asOf)

	explanations := make([]any, 0, len(candidates))
	supported := 0
	for _, candidate := range candidates {
		explanation := causalBridgeBuildExplanation(candidate, claims, asOf)
		if anyToString(explanation["decision"]) == "supported" {
			supported++
		}
		explanations = append(explanations, explanation)
	}
	causalBridgeAttachAlternatives(explanations)

	status := "abstain"
	if len(explanations) == 0 {
		status = "empty"
	} else if supported == len(explanations) {
		status = "supported"
	} else if supported > 0 {
		status = "mixed"
	}
	missing := []any{}
	if len(explanations) == 0 {
		missing = append(missing, causalBridgeMissingProof(
			"explicit_cross_project_graph_edge",
			"No bounded explicit graph edge connected the requested project to cross-project evidence.",
		))
	}
	return map[string]any{
		"schema_id":         causalBridgeExplanationContractID,
		"version":           1,
		"project":           project,
		"as_of":             asOf.Format(time.RFC3339Nano),
		"status":            status,
		"bounded":           true,
		"limit":             limit,
		"candidate_count":   candidateCount,
		"explanation_count": len(explanations),
		"supported_count":   supported,
		"abstained_count":   len(explanations) - supported,
		"truncated":         truncated,
		"explanations":      explanations,
		"missing_proof":     missing,
		"inference_policy":  "Project difference, lexical overlap, and associative graph links never establish causality; unsupported candidates abstain.",
	}
}

func causalBridgeCandidates(project string, graphNeighbors []any) ([]causalBridgeCandidate, bool) {
	scanLimit := minInt(len(graphNeighbors), causalBridgeCandidateScanMax)
	inputTruncated := len(graphNeighbors) > scanLimit
	candidates := []causalBridgeCandidate{}
	seen := map[string]struct{}{}
	for index := 0; index < scanLimit; index++ {
		candidate, ok := causalBridgeCandidateFromRow(project, anyMap(graphNeighbors[index]))
		if !ok {
			continue
		}
		key := strings.ToLower(firstNonEmptyStrings(candidate.EdgeID, candidate.Relation+"\x00"+candidate.SourceID+"\x00"+candidate.TargetID))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Policy.CausalCapable != candidates[j].Policy.CausalCapable {
			return candidates[i].Policy.CausalCapable
		}
		return candidates[i].BridgeID < candidates[j].BridgeID
	})
	return candidates, inputTruncated
}

func causalBridgeCandidateFromRow(project string, row map[string]any) (causalBridgeCandidate, bool) {
	if len(row) == 0 {
		return causalBridgeCandidate{}, false
	}
	baseProject := strings.TrimSpace(project)
	edge := anyMap(row["edge"])
	direction := strings.ToLower(strings.TrimSpace(anyToString(row["edge_direction"])))
	if direction != "in" {
		direction = "out"
	}
	seedID := strings.TrimSpace(anyToString(row["seed_memory_id"]))
	neighborID := strings.TrimSpace(anyToString(row["memory_id"]))
	sourceRaw := strings.TrimSpace(firstNonEmptyStrings(anyToString(edge["source_id"]), anyToString(row["source_id"])))
	targetRaw := strings.TrimSpace(firstNonEmptyStrings(anyToString(edge["target_id"]), anyToString(row["target_id"])))
	if sourceRaw == "" && targetRaw == "" {
		if direction == "in" {
			sourceRaw, targetRaw = neighborID, seedID
		} else {
			sourceRaw, targetRaw = seedID, neighborID
		}
	}
	sourceProject, sourceID, sourceValid := causalBridgeCanonicalEndpoint(sourceRaw)
	targetProject, targetID, targetValid := causalBridgeCanonicalEndpoint(targetRaw)
	rowProject := strings.TrimSpace(anyToString(row["project"]))
	if sourceProject == "" {
		if direction == "in" {
			sourceProject = rowProject
		} else {
			sourceProject = baseProject
		}
	}
	if targetProject == "" {
		if direction == "in" {
			targetProject = baseProject
		} else {
			targetProject = rowProject
		}
	}
	if sourceProject == "" || targetProject == "" || strings.EqualFold(sourceProject, targetProject) {
		return causalBridgeCandidate{}, false
	}
	if !strings.EqualFold(sourceProject, baseProject) && !strings.EqualFold(targetProject, baseProject) {
		return causalBridgeCandidate{}, false
	}
	otherProject := sourceProject
	if strings.EqualFold(sourceProject, baseProject) {
		otherProject = targetProject
	}

	neighborResolved := neighborID != ""
	if _, _, valid := causalBridgeCanonicalEndpoint(neighborID); !valid {
		neighborResolved = false
	}
	if hydrated, exists := row["hydrated"]; exists && !anyToBool(hydrated) {
		neighborResolved = false
	}
	switch strings.ToLower(strings.TrimSpace(anyToString(row["hydration_status"]))) {
	case "missing", "empty", "unavailable":
		neighborResolved = false
	}
	sourceResolved := sourceValid
	targetResolved := targetValid
	if direction == "in" {
		sourceResolved = sourceResolved && neighborResolved
	} else {
		targetResolved = targetResolved && neighborResolved
	}

	relation := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["relation"]), anyToString(edge["relation"])))
	policy := causalBridgePolicyForMemoryRelation(relation)
	edgeID := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["edge_id"]), anyToString(edge["edge_id"])))
	identity := firstNonEmptyStrings(edgeID, policy.EdgeType+"\x00"+sourceRaw+"\x00"+targetRaw)
	return causalBridgeCandidate{
		BridgeID:       "bridge_" + sha256Hex(strings.ToLower(baseProject) + "\x00" + identity)[:24],
		Project:        baseProject,
		EdgeID:         edgeID,
		Relation:       policy.EdgeType,
		Direction:      direction,
		Policy:         policy,
		SourceID:       sourceID,
		TargetID:       targetID,
		SourceProject:  sourceProject,
		TargetProject:  targetProject,
		OtherProject:   otherProject,
		Score:          roundFloat(clampFloat(anyToFloat(row["score"]), 0, 1), 4),
		SourceResolved: sourceResolved,
		TargetResolved: targetResolved,
		EdgeEvidence:   causalBridgeEdgeEvidenceRefs(row, edgeID),
	}, true
}

func causalBridgeCanonicalEndpoint(raw string) (string, string, bool) {
	project, _, canonical, _, err := canonicalMemoryID(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false
	}
	return project, canonical, true
}

func causalBridgeEdgeEvidenceRefs(row map[string]any, edgeID string) []any {
	refs := []any{}
	if edgeID != "" {
		refs = causalBridgeAppendRef(refs, causalBridgeRef("memory_edge", edgeID, "edge_evidence", "current", "support", false), causalBridgeReferenceMax)
	}
	for _, key := range []string{"content_ref", "citation"} {
		if value := strings.TrimSpace(anyToString(row[key])); value != "" {
			refs = causalBridgeAppendRef(refs, causalBridgeRef("edge_evidence", key+"\x00"+value, "edge_evidence", "current", "support", false), causalBridgeReferenceMax)
		}
	}
	provenance := anyMap(anyMap(row["edge"])["provenance"])
	keys := make([]string, 0, len(provenance))
	for key := range provenance {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := strings.TrimSpace(anyToString(provenance[key]))
		if value == "" {
			continue
		}
		refs = causalBridgeAppendRef(refs, causalBridgeRef("edge_provenance", key+"\x00"+value, "edge_evidence", "current", "support", false), causalBridgeReferenceMax)
	}
	causalBridgeSortRefs(refs)
	return refs
}

func causalBridgeBuildExplanation(candidate causalBridgeCandidate, claims []temporalClaim, asOf time.Time) map[string]any {
	pair := causalBridgeBestClaimPair(candidate, claims)
	supportRefs := append([]any{}, candidate.EdgeEvidence...)
	if pair.Found {
		supportRefs = causalBridgeAppendRef(supportRefs, causalBridgeRef("temporal_claim", pair.Cause.ClaimID, "cause_claim", "current", "support", false), causalBridgeReferenceMax)
		supportRefs = causalBridgeAppendRef(supportRefs, causalBridgeRef("temporal_claim", pair.Effect.ClaimID, "effect_claim", "current", "support", false), causalBridgeReferenceMax)
		supportRefs = causalBridgeAppendClaimEvidence(supportRefs, pair.Cause.Support, "cause_evidence", "current", "support", false)
		supportRefs = causalBridgeAppendClaimEvidence(supportRefs, pair.Effect.Support, "effect_evidence", "current", "support", false)
	}
	oppositionRefs, activeOpposition, historicalOpposition := causalBridgeOppositionRefs(candidate, pair, claims)
	causalBridgeSortRefs(supportRefs)
	causalBridgeSortRefs(oppositionRefs)

	edgeCitation := candidate.EdgeID != "" && len(candidate.EdgeEvidence) > 0
	causeCitation := pair.Found && len(pair.Cause.Support) > 0
	effectCitation := pair.Found && len(pair.Effect.Support) > 0
	citationCategories := 0
	for _, present := range []bool{edgeCitation, causeCitation, effectCitation} {
		if present {
			citationCategories++
		}
	}
	citationsSufficient := citationCategories == 3
	verified := pair.Found && causalBridgeClaimVerified(pair.Cause) && causalBridgeClaimVerified(pair.Effect)
	temporallyValid := pair.Found
	danglingStatus := causalBridgeDanglingStatus(candidate)
	resolved := danglingStatus == "resolved"

	missing := []any{}
	if !candidate.Policy.ExplicitlyTyped {
		missing = causalBridgeAppendMissing(missing, "typed_edge", "The graph relation is missing or invalid, so the bridge has no usable edge type.")
	}
	if !candidate.Policy.CausalCapable {
		missing = causalBridgeAppendMissing(missing, "explicit_causal_edge", "The typed graph edge is associative, evidentiary, or otherwise non-causal.")
	}
	if !resolved {
		missing = causalBridgeAppendMissing(missing, "resolved_edge_endpoints", "At least one edge endpoint does not resolve to bounded evidence.")
	}
	if !edgeCitation {
		missing = causalBridgeAppendMissing(missing, "edge_evidence_reference", "The edge lacks a bounded edge-record evidence reference.")
	}
	if !pair.Found {
		missing = causalBridgeAppendMissing(missing, "structured_causal_claim_link", "No current effect claim explicitly names a current cause claim tied to the two edge endpoints.")
	}
	if pair.Found && !verified {
		missing = causalBridgeAppendMissing(missing, "verified_causal_claims", "The current cause and effect claims are not both independently verified.")
	}
	if pair.Found && !causeCitation {
		missing = causalBridgeAppendMissing(missing, "cause_citation", "The current cause claim has no bounded support citation.")
	}
	if pair.Found && !effectCitation {
		missing = causalBridgeAppendMissing(missing, "effect_citation", "The current effect claim has no bounded support citation.")
	}
	if activeOpposition > 0 {
		missing = causalBridgeAppendMissing(missing, "unresolved_current_opposition", "Current opposition remains unresolved and prevents a causal conclusion.")
	}

	decision := "abstain"
	if len(missing) == 0 {
		decision = "supported"
	}
	claimConfidence := 0.0
	if pair.Found {
		claimConfidence = minFloat(clampFloat(pair.Cause.Confidence, 0, 1), clampFloat(pair.Effect.Confidence, 0, 1))
	}
	temporalScore := 0.0
	if temporallyValid {
		temporalScore = 1
	}
	verificationScore := 0.0
	if verified {
		verificationScore = 1
	}
	citationScore := float64(citationCategories) / 3
	oppositionPenalty := minFloat(0.5, float64(activeOpposition)*0.2)
	calculated := candidate.Score*0.2 + claimConfidence*0.35 + temporalScore*0.2 + verificationScore*0.15 + citationScore*0.1 - oppositionPenalty
	finalConfidence := 0.0
	if decision == "supported" {
		finalConfidence = clampFloat(calculated, 0, 1)
	}

	temporalStatus := "unproven"
	if temporallyValid {
		temporalStatus = "valid"
	} else if historicalOpposition > 0 {
		temporalStatus = "historical_only"
	}
	explanation := "Causality is not established; inspect the missing-proof disclosures before using this cross-project candidate."
	if decision == "supported" {
		explanation = "A typed causal edge and verified current claims explicitly connect the cross-project evidence."
	}
	return map[string]any{
		"schema_id":          causalBridgeExplanationContractID,
		"bridge_id":          candidate.BridgeID,
		"project":            candidate.Project,
		"other_project":      candidate.OtherProject,
		"decision":           decision,
		"causality_claimed":  decision == "supported",
		"explanation":        explanation,
		"typed_edge":         causalBridgeTypedEdge(candidate),
		"edge_evidence_refs": candidate.EdgeEvidence,
		"support_refs":       supportRefs,
		"opposition_refs":    oppositionRefs,
		"temporally_valid":   temporallyValid,
		"temporal_validity": map[string]any{
			"status": temporalStatus, "as_of": asOf.Format(time.RFC3339Nano),
			"current_causal_claim_pair": pair.Found, "historical_opposition_count": historicalOpposition,
		},
		"dangling_edge_status": danglingStatus,
		"dangling_edge": map[string]any{
			"status": danglingStatus, "source_resolved": candidate.SourceResolved,
			"target_resolved": candidate.TargetResolved, "resolved": resolved,
		},
		"alternatives": []any{},
		"confidence": map[string]any{
			"edge_evidence": candidate.Score, "structured_claims": roundFloat(claimConfidence, 4),
			"temporal_validity": temporalScore, "verification": verificationScore,
			"citation_sufficiency": roundFloat(citationScore, 4), "opposition_penalty": roundFloat(oppositionPenalty, 4),
			"calculated": roundFloat(clampFloat(calculated, 0, 1), 4), "final": roundFloat(finalConfidence, 4),
		},
		"citation_sufficiency": map[string]any{
			"status": causalBridgeCitationStatus(citationsSufficient), "sufficient": citationsSufficient,
			"required_categories": 3, "available_categories": citationCategories,
			"edge_evidence": edgeCitation, "cause_support": causeCitation, "effect_support": effectCitation,
		},
		"missing_proof": missing,
	}
}

func causalBridgeTypedEdge(candidate causalBridgeCandidate) map[string]any {
	return map[string]any{
		"edge_ref":            causalBridgeOpaqueRef("memory_edge", firstNonEmptyStrings(candidate.EdgeID, candidate.BridgeID)),
		"edge_type":           candidate.Policy.EdgeType,
		"semantic_class":      candidate.Policy.SemanticClass,
		"cause_direction":     candidate.Policy.CauseDirection,
		"causal_capable":      candidate.Policy.CausalCapable,
		"edge_direction":      candidate.Direction,
		"source_project":      candidate.SourceProject,
		"target_project":      candidate.TargetProject,
		"source_evidence_ref": causalBridgeOpaqueRef("memory", firstNonEmptyStrings(candidate.SourceID, "missing_source:"+candidate.SourceProject)),
		"target_evidence_ref": causalBridgeOpaqueRef("memory", firstNonEmptyStrings(candidate.TargetID, "missing_target:"+candidate.TargetProject)),
	}
}

func causalBridgeBestClaimPair(candidate causalBridgeCandidate, claims []temporalClaim) causalBridgeClaimPair {
	if !candidate.Policy.CausalCapable {
		return causalBridgeClaimPair{}
	}
	causeEndpoint, effectEndpoint := candidate.SourceID, candidate.TargetID
	if candidate.Policy.CauseDirection == "target_to_source" {
		causeEndpoint, effectEndpoint = candidate.TargetID, candidate.SourceID
	}
	if causeEndpoint == "" || effectEndpoint == "" {
		return causalBridgeClaimPair{}
	}
	byID := map[string]temporalClaim{}
	for _, claim := range claims {
		byID[claim.ClaimID] = claim
	}
	pairs := []causalBridgeClaimPair{}
	for _, effect := range claims {
		if !temporalClaimCanInfluence(effect) || !causalBridgeClaimReferencesMemory(effect, effectEndpoint) {
			continue
		}
		causeIDs := append([]string(nil), effect.CausedBy...)
		sort.Strings(causeIDs)
		for _, causeID := range causeIDs {
			cause, ok := byID[causeID]
			if !ok || !temporalClaimCanInfluence(cause) || !causalBridgeClaimReferencesMemory(cause, causeEndpoint) {
				continue
			}
			pairs = append(pairs, causalBridgeClaimPair{Found: true, Cause: cause, Effect: effect})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		leftVerified := causalBridgeClaimVerified(pairs[i].Cause) && causalBridgeClaimVerified(pairs[i].Effect)
		rightVerified := causalBridgeClaimVerified(pairs[j].Cause) && causalBridgeClaimVerified(pairs[j].Effect)
		if leftVerified != rightVerified {
			return leftVerified
		}
		leftConfidence := minFloat(pairs[i].Cause.Confidence, pairs[i].Effect.Confidence)
		rightConfidence := minFloat(pairs[j].Cause.Confidence, pairs[j].Effect.Confidence)
		if leftConfidence != rightConfidence {
			return leftConfidence > rightConfidence
		}
		leftID := pairs[i].Cause.ClaimID + "\x00" + pairs[i].Effect.ClaimID
		rightID := pairs[j].Cause.ClaimID + "\x00" + pairs[j].Effect.ClaimID
		return leftID < rightID
	})
	if len(pairs) == 0 {
		return causalBridgeClaimPair{}
	}
	return pairs[0]
}

func causalBridgeOppositionRefs(candidate causalBridgeCandidate, pair causalBridgeClaimPair, claims []temporalClaim) ([]any, int, int) {
	refs := []any{}
	activeCount := 0
	historicalCount := 0
	byID := map[string]temporalClaim{}
	for _, claim := range claims {
		byID[claim.ClaimID] = claim
	}
	pairIDs := map[string]struct{}{}
	if pair.Found {
		pairIDs[pair.Cause.ClaimID] = struct{}{}
		pairIDs[pair.Effect.ClaimID] = struct{}{}
	}
	for _, claim := range claims {
		if _, selected := pairIDs[claim.ClaimID]; selected {
			continue
		}
		if temporalClaimIsHistoricalOpposition(claim) {
			if causalBridgeHistoricalClaimRelevant(candidate, pair, claim, claims) {
				refs = causalBridgeAppendRef(refs, causalBridgeRef("temporal_claim", claim.ClaimID, "historical_opposition", claim.Status, "none", true), causalBridgeReferenceMax)
				historicalCount++
			}
			continue
		}
		if pair.Found && temporalClaimCanInfluence(claim) && causalBridgeClaimsExplicitlyOppose(claim, pair.Cause, pair.Effect) {
			refs = causalBridgeAppendRef(refs, causalBridgeRef("temporal_claim", claim.ClaimID, "opposition", "current", "opposition", false), causalBridgeReferenceMax)
			activeCount++
		}
	}
	if pair.Found {
		for _, selected := range []temporalClaim{pair.Cause, pair.Effect} {
			for _, targetID := range selected.Contradicts {
				target, ok := byID[targetID]
				switch {
				case ok && temporalClaimIsHistoricalOpposition(target):
					refs = causalBridgeAppendRef(refs, causalBridgeRef("temporal_claim", target.ClaimID, "historical_opposition", target.Status, "none", true), causalBridgeReferenceMax)
					historicalCount++
				case ok && !temporalClaimCanInfluence(target):
					refs = causalBridgeAppendRef(refs, causalBridgeRef("temporal_claim", target.ClaimID, "opposition", target.Status, "none", false), causalBridgeReferenceMax)
				case ok:
					refs = causalBridgeAppendRef(refs, causalBridgeRef("temporal_claim", target.ClaimID, "opposition", "current", "opposition", false), causalBridgeReferenceMax)
					activeCount++
				default:
					refs = causalBridgeAppendRef(refs, causalBridgeRef("temporal_claim", targetID, "opposition", "not_in_bounded_result", "opposition", false), causalBridgeReferenceMax)
					activeCount++
				}
			}
			refs = causalBridgeAppendClaimEvidence(refs, selected.Opposition, "opposition", "current", "opposition", false)
			activeCount += len(selected.Opposition)
		}
	}
	refs = causalBridgeUniqueRefs(refs, causalBridgeReferenceMax)
	historicalCount = causalBridgeRefCount(refs, true, "")
	activeCount = causalBridgeRefCount(refs, false, "opposition")
	return refs, activeCount, historicalCount
}

func causalBridgeHistoricalClaimRelevant(candidate causalBridgeCandidate, pair causalBridgeClaimPair, historical temporalClaim, claims []temporalClaim) bool {
	if pair.Found && causalBridgeClaimsExplicitlyOppose(historical, pair.Cause, pair.Effect) {
		return true
	}
	causeEndpoint, effectEndpoint := candidate.SourceID, candidate.TargetID
	if candidate.Policy.CauseDirection == "target_to_source" {
		causeEndpoint, effectEndpoint = candidate.TargetID, candidate.SourceID
	}
	byID := map[string]temporalClaim{}
	for _, claim := range claims {
		byID[claim.ClaimID] = claim
	}
	if causalBridgeClaimReferencesMemory(historical, effectEndpoint) {
		for _, causeID := range historical.CausedBy {
			if cause, ok := byID[causeID]; ok && causalBridgeClaimReferencesMemory(cause, causeEndpoint) {
				return true
			}
		}
	}
	if causalBridgeClaimReferencesMemory(historical, causeEndpoint) {
		for _, effect := range claims {
			if causalBridgeClaimReferencesMemory(effect, effectEndpoint) && stringSliceContainsExact(effect.CausedBy, historical.ClaimID) {
				return true
			}
		}
	}
	return false
}

func causalBridgeClaimsExplicitlyOppose(candidate temporalClaim, left temporalClaim, right temporalClaim) bool {
	for _, targetID := range candidate.Contradicts {
		if targetID == left.ClaimID || targetID == right.ClaimID {
			return true
		}
	}
	for _, selected := range []temporalClaim{left, right} {
		if stringSliceContainsExact(selected.Contradicts, candidate.ClaimID) {
			return true
		}
	}
	return false
}

func causalBridgeClaimReferencesMemory(claim temporalClaim, memoryID string) bool {
	_, _, canonical, _, err := canonicalMemoryID(memoryID)
	if err != nil {
		return false
	}
	for _, evidence := range claim.Support {
		for _, raw := range []string{evidence.MemoryID, evidence.RefID} {
			_, _, candidate, _, candidateErr := canonicalMemoryID(raw)
			if candidateErr == nil && strings.EqualFold(candidate, canonical) {
				return true
			}
		}
	}
	if raw := strings.TrimSpace(anyToString(claim.Provenance["source_id"])); raw != "" {
		_, _, candidate, _, candidateErr := canonicalMemoryID(raw)
		return candidateErr == nil && strings.EqualFold(candidate, canonical)
	}
	return false
}

func causalBridgeClaimsAt(claims []temporalClaim, asOf time.Time) []temporalClaim {
	out := make([]temporalClaim, 0, minInt(len(claims), causalBridgeClaimMax))
	seen := map[string]struct{}{}
	for _, claim := range claims {
		if strings.TrimSpace(claim.ClaimID) == "" {
			continue
		}
		if _, exists := seen[claim.ClaimID]; exists {
			continue
		}
		seen[claim.ClaimID] = struct{}{}
		claim.Status = temporalClaimStatusAt(claim, asOf)
		out = append(out, claim)
		if len(out) >= causalBridgeClaimMax {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		leftRank := temporalClaimInfluenceRank(out[i])
		rightRank := temporalClaimInfluenceRank(out[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return out[i].ClaimID < out[j].ClaimID
	})
	return out
}

func causalBridgeClaimVerified(claim temporalClaim) bool {
	return strings.EqualFold(strings.TrimSpace(anyToString(claim.Verification["status"])), "verified")
}

func causalBridgeAppendClaimEvidence(refs []any, evidence []temporalClaimEvidence, role string, status string, influence string, historical bool) []any {
	identities := make([]string, 0, len(evidence))
	kinds := map[string]string{}
	for _, item := range evidence {
		identity := firstNonEmptyStrings(item.RefID, item.MemoryID, item.URI, item.ContentHash)
		if identity == "" {
			continue
		}
		key := firstNonEmptyStrings(item.Kind, "evidence") + "\x00" + identity
		identities = append(identities, key)
		kinds[key] = firstNonEmptyStrings(item.Kind, "evidence")
	}
	sort.Strings(identities)
	for _, identity := range identities {
		parts := strings.SplitN(identity, "\x00", 2)
		refs = causalBridgeAppendRef(refs, causalBridgeRef(kinds[identity], parts[len(parts)-1], role, status, influence, historical), causalBridgeReferenceMax)
	}
	return refs
}

func causalBridgeRef(kind string, identity string, role string, status string, influence string, historical bool) map[string]any {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil
	}
	kind = causalBridgeRefKind(kind)
	return map[string]any{
		"ref_id": causalBridgeOpaqueRef(kind, identity), "kind": kind,
		"role": role, "status": status, "historical": historical, "influence": influence,
	}
}

func causalBridgeOpaqueRef(kind string, identity string) string {
	kind = causalBridgeRefKind(kind)
	return kind + "_ref_" + sha256Hex(kind + "\x00" + strings.TrimSpace(identity))[:24]
}

func causalBridgeRefKind(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var normalized strings.Builder
	for _, char := range raw {
		switch {
		case (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_':
			normalized.WriteRune(char)
		case char == '-' || char == ' ':
			normalized.WriteByte('_')
		}
		if normalized.Len() >= 32 {
			break
		}
	}
	kind := strings.Trim(normalized.String(), "_")
	if kind == "" {
		return "evidence"
	}
	return kind
}

func causalBridgeAppendRef(refs []any, candidate map[string]any, limit int) []any {
	if len(candidate) == 0 || len(refs) >= limit {
		return refs
	}
	refID := anyToString(candidate["ref_id"])
	for _, raw := range refs {
		if anyToString(anyMap(raw)["ref_id"]) == refID {
			return refs
		}
	}
	return append(refs, candidate)
}

func causalBridgeUniqueRefs(refs []any, limit int) []any {
	out := []any{}
	for _, raw := range refs {
		out = causalBridgeAppendRef(out, anyMap(raw), limit)
	}
	return out
}

func causalBridgeSortRefs(refs []any) {
	sort.SliceStable(refs, func(i, j int) bool {
		return anyToString(anyMap(refs[i])["ref_id"]) < anyToString(anyMap(refs[j])["ref_id"])
	})
}

func causalBridgeRefCount(refs []any, historical bool, influence string) int {
	count := 0
	for _, raw := range refs {
		ref := anyMap(raw)
		if anyToBool(ref["historical"]) != historical {
			continue
		}
		if influence != "" && anyToString(ref["influence"]) != influence {
			continue
		}
		count++
	}
	return count
}

func causalBridgeDanglingStatus(candidate causalBridgeCandidate) string {
	switch {
	case candidate.SourceResolved && candidate.TargetResolved:
		return "resolved"
	case !candidate.SourceResolved && !candidate.TargetResolved:
		return "dangling_both"
	case !candidate.SourceResolved:
		return "dangling_source"
	default:
		return "dangling_target"
	}
}

func causalBridgeCitationStatus(sufficient bool) string {
	if sufficient {
		return "sufficient"
	}
	return "insufficient"
}

func causalBridgeMissingProof(code string, disclosure string) map[string]any {
	return map[string]any{"code": code, "disclosure": disclosure}
}

func causalBridgeAppendMissing(missing []any, code string, disclosure string) []any {
	if len(missing) >= causalBridgeMissingProofMax {
		return missing
	}
	for _, raw := range missing {
		if anyToString(anyMap(raw)["code"]) == code {
			return missing
		}
	}
	return append(missing, causalBridgeMissingProof(code, disclosure))
}

func causalBridgeAttachAlternatives(explanations []any) {
	for index, raw := range explanations {
		explanation := anyMap(raw)
		alternatives := []any{}
		for candidateIndex, candidateRaw := range explanations {
			if index == candidateIndex || len(alternatives) >= causalBridgeAlternativeMax {
				continue
			}
			candidate := anyMap(candidateRaw)
			alternatives = append(alternatives, map[string]any{
				"bridge_ref": causalBridgeOpaqueRef("causal_bridge", anyToString(candidate["bridge_id"])),
				"edge_type":  anyToString(anyMap(candidate["typed_edge"])["edge_type"]),
				"decision":   anyToString(candidate["decision"]),
				"confidence": anyToFloat(anyMap(candidate["confidence"])["final"]),
			})
		}
		explanation["alternatives"] = alternatives
	}
}

func stringSliceContainsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
