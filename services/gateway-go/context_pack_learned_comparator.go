package main

import (
	"sort"
	"strings"
	"time"
)

const (
	contextPackLearnedActuatorComparisonScope = "context_pack_rank_and_allocate_same_returned_candidate_pool"
	contextPackLearnedActuatorFixedK          = 5
)

// contextPackLearnedActuatorComparison evaluates the exact bounded actuator,
// not the raw-search shadow reranker. It retains raw candidates only for the
// duration of the saved-case run and emits aggregate metrics plus opaque refs.
type contextPackLearnedActuatorComparison struct {
	caseSetRef          string
	expectedCases       int
	evaluatedCases      int
	influencedCases     int
	valid               bool
	reason              string
	reputationVectorRef string
	multipliers         map[string]float64
	baseline            savedRecallImpactMetrics
	treatment           savedRecallImpactMetrics
	candidatePools      []string
	tokenBudgets        []string
	protectedSets       []string
	rankingVectors      []string
	controlSelections   []string
	treatmentSelections []string
	authorityEnvelope   map[string]any
}

func (c *contextPackLearnedActuatorComparison) setAuthorityEnvelope(envelope map[string]any) {
	if c == nil || len(envelope) == 0 {
		return
	}
	c.authorityEnvelope = cloneJSONMap(envelope)
}

func newContextPackLearnedActuatorComparison(caseSetRef string, expectedCases int, multipliers map[string]float64, reason string) *contextPackLearnedActuatorComparison {
	reputationVectorRef := contextPackLearnedReputationVectorRef(multipliers)
	comparison := &contextPackLearnedActuatorComparison{
		caseSetRef:          caseSetRef,
		expectedCases:       expectedCases,
		valid:               reason == "" && len(multipliers) > 0,
		reason:              reason,
		reputationVectorRef: reputationVectorRef,
		multipliers:         make(map[string]float64, len(multipliers)),
		baseline:            savedRecallImpactMetrics{latencyValues: make([]float64, 0, expectedCases)},
		treatment:           savedRecallImpactMetrics{latencyValues: make([]float64, 0, expectedCases)},
		candidatePools:      make([]string, 0, expectedCases),
		tokenBudgets:        make([]string, 0, expectedCases),
		protectedSets:       make([]string, 0, expectedCases),
		rankingVectors:      make([]string, 0, expectedCases),
		controlSelections:   make([]string, 0, expectedCases),
		treatmentSelections: make([]string, 0, expectedCases),
	}
	for candidateID, multiplier := range multipliers {
		comparison.multipliers[candidateID] = multiplier
	}
	if comparison.reason == "" && len(comparison.multipliers) == 0 {
		comparison.reason = "reputation_candidate_influence_unavailable"
	} else if comparison.reason == "" && comparison.reputationVectorRef == "" {
		comparison.reason = "reputation_vector_unavailable"
		comparison.valid = false
	}
	return comparison
}

func (c *contextPackLearnedActuatorComparison) invalidate(reason string) {
	if c == nil {
		return
	}
	c.valid = false
	if c.reason == "" {
		c.reason = reason
	}
}

func (c *contextPackLearnedActuatorComparison) addCase(rawCase, searchResponse map[string]any, grade int) {
	if c == nil || !c.valid {
		return
	}
	query := strings.TrimSpace(anyToString(rawCase["query"]))
	if query == "" {
		c.invalidate("actuator_case_query_missing")
		return
	}
	maxFacts := clampInt(anyToInt(rawCase["max_facts"], 24), 1, 100)
	maxResults := clampInt(anyToInt(rawCase["limit"], 10), 1, 100)
	contextPack := buildContextPackPayload(query, searchResponse, maxFacts, maxResults)
	tokenBudget := contextPackTokenBudgetFromRequest(rawCase)
	evaluatedAt := time.Now().UTC()

	controlStarted := time.Now()
	control := contextPackRankedEvidenceWithLearningAt(query, contextPack, tokenBudget, contextPackLearnedActivationDecision{}, evaluatedAt)
	controlLatency := float64(time.Since(controlStarted).Microseconds()) / 1000.0
	treatmentDecision := contextPackLearnedActivationDecision{
		Armed: true, Eligible: true, AssignedTreatment: true, Arm: "canary",
		Reason: "offline_actuator_comparator", CandidateMultipliers: c.multipliers,
	}
	treatmentStarted := time.Now()
	treatment := contextPackRankedEvidenceWithLearningAt(query, contextPack, tokenBudget, treatmentDecision, evaluatedAt)
	treatmentLatency := float64(time.Since(treatmentStarted).Microseconds()) / 1000.0
	influenced := treatment.LearnedActivation.Performed
	if !influenced && treatment.LearnedActivation.Reason != "no_returned_candidate_influence" {
		c.invalidate(firstNonEmptyStrings(treatment.LearnedActivation.Reason, "actuator_treatment_unavailable"))
		return
	}

	controlPool := contextPackLearnedActuatorPoolProjection(control.EligibleItems)
	treatmentPool := contextPackLearnedActuatorPoolProjection(treatment.EligibleItems)
	controlPoolRef := contextPackLearnedCanonicalDigest(controlPool)
	if controlPoolRef == "" || controlPoolRef != contextPackLearnedCanonicalDigest(treatmentPool) {
		c.invalidate("returned_candidate_pool_mismatch")
		return
	}
	if !contextPackLearnedProtectedSelectionPreserved(control.SelectedItems, treatment.SelectedItems) {
		c.invalidate("protected_selection_not_preserved")
		return
	}
	tokenBudgetRef := contextPackLearnedCanonicalDigest(contextPackLearnedActuatorTokenBudgetProjection(tokenBudget))
	protectedRef := contextPackLearnedCanonicalDigest(contextPackLearnedActuatorProtectedProjection(control.EligibleItems))
	controlSelectionRef := contextPackLearnedCanonicalDigest(contextPackLearnedActuatorSelectionProjection(control.SelectedItems))
	treatmentSelectionRef := contextPackLearnedCanonicalDigest(contextPackLearnedActuatorSelectionProjection(treatment.SelectedItems))
	rankingVectorRef := contextPackLearnedDigestRef(treatment.LearnedActivation.RankingVectorDigest)
	if !influenced {
		rankingVectorRef = contextPackLearnedCanonicalDigest(map[string]any{
			"schema_id":          "context_pack_learned_noop_vector.v1",
			"candidate_pool_ref": controlPoolRef,
		})
	}
	if tokenBudgetRef == "" || protectedRef == "" || controlSelectionRef == "" || treatmentSelectionRef == "" || rankingVectorRef == "" {
		c.invalidate("actuator_comparator_reference_unavailable")
		return
	}

	baselineCandidates := contextPackLearnedActuatorMetricCandidates(control.SelectedItems)
	treatmentCandidates := contextPackLearnedActuatorMetricCandidates(treatment.SelectedItems)
	if len(baselineCandidates) == 0 || len(treatmentCandidates) == 0 {
		c.invalidate("actuator_selection_empty")
		return
	}
	expectedFiles := normalizeExpectedFileTokens(rawCase["expected_files"])
	forbiddenFiles := normalizeExpectedFileTokens(rawCase["forbidden_files"])
	expectedNumeric := normalizeExpectedNumeric(rawCase["expected_numeric"])
	baselineNumeric, baselineMapped := savedRecallImpactMatchedNumericFacts(baselineCandidates, expectedNumeric)
	treatmentNumeric, treatmentMapped := savedRecallImpactMatchedNumericFacts(treatmentCandidates, expectedNumeric)
	if !baselineMapped || !treatmentMapped {
		c.invalidate("candidate_bound_numeric_evidence_missing")
		return
	}
	c.baseline.addCase(baselineCandidates, expectedFiles, forbiddenFiles, expectedNumeric, baselineNumeric, grade, controlLatency)
	c.treatment.addCase(treatmentCandidates, expectedFiles, forbiddenFiles, expectedNumeric, treatmentNumeric, grade, treatmentLatency)
	c.candidatePools = append(c.candidatePools, controlPoolRef)
	c.tokenBudgets = append(c.tokenBudgets, tokenBudgetRef)
	c.protectedSets = append(c.protectedSets, protectedRef)
	c.rankingVectors = append(c.rankingVectors, rankingVectorRef)
	c.controlSelections = append(c.controlSelections, controlSelectionRef)
	c.treatmentSelections = append(c.treatmentSelections, treatmentSelectionRef)
	c.evaluatedCases++
	if influenced {
		c.influencedCases++
	}
}

func contextPackLearnedActuatorPoolProjection(items []contextPackEvidenceItem) []map[string]any {
	projection := make([]map[string]any, 0, len(items))
	for _, item := range items {
		projection = append(projection, map[string]any{
			"candidate_ref": item.CandidateID, "occurrence": item.Occurrence,
			"content_ref": item.ContentDigest, "kind": item.Kind,
			"estimated_tokens": item.EstimatedTokens,
			"protected":        contextPackLearnedProtectedEvidence(item),
		})
	}
	sort.Slice(projection, func(i, j int) bool {
		if anyToString(projection[i]["candidate_ref"]) == anyToString(projection[j]["candidate_ref"]) {
			return anyToInt(projection[i]["occurrence"], 0) < anyToInt(projection[j]["occurrence"], 0)
		}
		return anyToString(projection[i]["candidate_ref"]) < anyToString(projection[j]["candidate_ref"])
	})
	return projection
}

func contextPackLearnedActuatorProtectedProjection(items []contextPackEvidenceItem) []map[string]any {
	projection := make([]map[string]any, 0)
	for _, item := range items {
		if !contextPackLearnedProtectedEvidence(item) {
			continue
		}
		projection = append(projection, map[string]any{
			"candidate_ref": item.CandidateID, "occurrence": item.Occurrence, "kind": item.Kind,
		})
	}
	sort.Slice(projection, func(i, j int) bool {
		if anyToString(projection[i]["candidate_ref"]) == anyToString(projection[j]["candidate_ref"]) {
			return anyToInt(projection[i]["occurrence"], 0) < anyToInt(projection[j]["occurrence"], 0)
		}
		return anyToString(projection[i]["candidate_ref"]) < anyToString(projection[j]["candidate_ref"])
	})
	return projection
}

func contextPackLearnedActuatorSelectionProjection(items []contextPackEvidenceItem) []map[string]any {
	projection := make([]map[string]any, 0, len(items))
	for index, item := range items {
		row := map[string]any{
			"candidate_ref": item.CandidateID, "rank": index + 1, "kind": item.Kind,
			"score": roundFloat(item.Score, 6), "estimated_tokens": item.EstimatedTokens,
			"learned_influence_applied": item.LearnedInfluenceApplied,
		}
		if item.LearnedInfluenceApplied {
			row["learned_multiplier"] = roundFloat(item.LearnedMultiplier, 6)
		}
		projection = append(projection, row)
	}
	return projection
}

func contextPackLearnedActuatorTokenBudgetProjection(tokenBudget contextPackTokenBudget) map[string]any {
	result := map[string]any{
		"active":                      tokenBudget.Active,
		"agent_context_budget_tokens": tokenBudget.AgentContextBudgetTokens,
		"model_context_window_tokens": tokenBudget.ModelContextWindowTokens,
		"reserved_response_tokens":    tokenBudget.ReservedResponseTokens,
		"already_loaded_tokens":       tokenBudget.AlreadyLoadedTokens,
		"target_context_pack_tokens":  tokenBudget.TargetContextPackTokens,
		"ranked_evidence_tokens":      tokenBudget.RankedEvidenceTokens,
		"estimate_method":             tokenBudget.EstimateMethod,
		"calibration_grade":           tokenBudget.CalibrationGrade,
		"tokenizer_encoding":          tokenBudget.TokenizerEncoding,
		"tokenizer_exact":             tokenBudget.TokenizerExact,
	}
	return result
}

func contextPackLearnedActuatorMetricCandidates(items []contextPackEvidenceItem) []savedRecallImpactCandidate {
	limit := minInt(len(items), contextPackLearnedActuatorFixedK)
	candidates := make([]savedRecallImpactCandidate, 0, limit)
	for index := 0; index < limit; index++ {
		item := items[index]
		candidates = append(candidates, savedRecallImpactCandidate{
			ref: item.CandidateID,
			rows: []map[string]any{{
				"project": item.Project, "file": item.File, "source": item.Source,
				"topic_path": item.TopicPath, "text": item.Text,
			}},
		})
	}
	return candidates
}

func contextPackLearnedActuatorMetrics(metrics savedRecallImpactMetrics) map[string]any {
	result := metrics.monitorMetrics()
	result["numeric_expected_count"] = metrics.numericExpected
	result["citation_expected_count"] = metrics.citationExpected
	result["citation_candidate_count"] = metrics.citationCandidates
	return result
}

func contextPackLearnedActuatorAggregateRef(kind, caseSetRef string, refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return contextPackLearnedCanonicalDigest(map[string]any{
		"schema_id": "context_pack_learned_actuator_ref.v1", "kind": kind,
		"case_set_ref": caseSetRef, "case_refs": append([]string(nil), refs...),
	})
}

func (c *contextPackLearnedActuatorComparison) monitorFields() map[string]any {
	if c == nil {
		return map[string]any{}
	}
	baseline := contextPackLearnedActuatorMetrics(c.baseline)
	treatment := contextPackLearnedActuatorMetrics(c.treatment)
	structuralComplete := c.evaluatedCases == c.expectedCases && c.expectedCases > 0 &&
		len(c.candidatePools) == c.expectedCases && len(c.tokenBudgets) == c.expectedCases &&
		len(c.protectedSets) == c.expectedCases && len(c.rankingVectors) == c.expectedCases &&
		len(c.controlSelections) == c.expectedCases && len(c.treatmentSelections) == c.expectedCases
	valid := c.valid && structuralComplete && c.influencedCases > 0
	reason := c.reason
	if valid {
		if status, pass := contextPackLearnedActuatorMetricsGate(baseline, treatment, c.evaluatedCases); !pass {
			valid = false
			reason = status
		}
	}
	if reason == "" {
		if valid {
			reason = "valid"
		} else if c.influencedCases == 0 && structuralComplete {
			reason = "reputation_candidate_influence_unavailable"
		} else if c.evaluatedCases != c.expectedCases {
			reason = "actuator_case_count_mismatch"
		} else {
			reason = "actuator_comparison_invalid"
		}
	}
	result := map[string]any{
		"schema_id": contextPackLearnedActuatorComparatorContractID, "version": 1,
		"comparison_scope":   contextPackLearnedActuatorComparisonScope,
		"comparison_fixed_k": contextPackLearnedActuatorFixedK,
		"comparison_valid":   valid, "comparison_reason": reason,
		"case_count": c.evaluatedCases, "influenced_case_count": c.influencedCases, "case_set_ref": c.caseSetRef,
		"reputation_vector_ref":         c.reputationVectorRef,
		"ranking_contract_id":           contextPackLearnedActivationContractID,
		"allocation_contract_id":        "context_pack_evidence_allocation.v1",
		"latency_basis":                 "measured_context_pack_rank_and_allocate_ms",
		"same_returned_candidate_pool":  structuralComplete,
		"same_token_budget":             structuralComplete,
		"protected_selection_preserved": structuralComplete,
		"candidate_pool_ref":            contextPackLearnedActuatorAggregateRef("candidate_pool", c.caseSetRef, c.candidatePools),
		"token_budget_ref":              contextPackLearnedActuatorAggregateRef("token_budget", c.caseSetRef, c.tokenBudgets),
		"protected_partition_ref":       contextPackLearnedActuatorAggregateRef("protected_partition", c.caseSetRef, c.protectedSets),
		"ranking_vector_ref":            contextPackLearnedActuatorAggregateRef("ranking_vector", c.caseSetRef, c.rankingVectors),
		"control_selection_ref":         contextPackLearnedActuatorAggregateRef("control_selection", c.caseSetRef, c.controlSelections),
		"treatment_selection_ref":       contextPackLearnedActuatorAggregateRef("treatment_selection", c.caseSetRef, c.treatmentSelections),
		"baseline":                      baseline, "treatment": treatment,
	}
	if len(c.authorityEnvelope) > 0 {
		result["authority"] = cloneJSONMap(c.authorityEnvelope)
	}
	return result
}
