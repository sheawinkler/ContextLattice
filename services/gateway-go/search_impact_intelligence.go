package main

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	searchImpactIntelligenceContractID             = "search_impact_intelligence.v1"
	contextPackLearnedActuatorComparatorContractID = "context_pack_learned_actuator_comparator.v1"
	searchImpactIntelligencePath                   = "/telemetry/search-impact"
	searchImpactOutcomeSourceLimit                 = 1000
	searchImpactMinTrainOutcomes                   = 8
	searchImpactMinHoldoutOutcomes                 = 2
	searchImpactMinNegativeOutcomes                = 1
	searchImpactMinIndependentVerifiers            = 2
)

func contextPackLearnedActuatorComparatorProof(shadow map[string]any) (map[string]any, string, bool) {
	raw := anyMap(shadow["learned_actuator_comparator"])
	if len(raw) == 0 || anyToString(raw["schema_id"]) != contextPackLearnedActuatorComparatorContractID ||
		anyToInt(raw["version"], 0) != 1 || !anyToBool(raw["comparison_valid"]) ||
		anyToString(raw["comparison_reason"]) != "valid" ||
		anyToString(raw["comparison_scope"]) != contextPackLearnedActuatorComparisonScope ||
		anyToInt(raw["comparison_fixed_k"], 0) != contextPackLearnedActuatorFixedK ||
		anyToString(raw["ranking_contract_id"]) != contextPackLearnedActivationContractID ||
		anyToString(raw["allocation_contract_id"]) != "context_pack_evidence_allocation.v1" ||
		anyToString(raw["latency_basis"]) != "measured_context_pack_rank_and_allocate_ms" ||
		!anyToBool(raw["same_returned_candidate_pool"]) || !anyToBool(raw["same_token_budget"]) ||
		!anyToBool(raw["protected_selection_preserved"]) ||
		anyToString(raw["case_set_ref"]) != anyToString(shadow["case_set_ref"]) {
		return nil, "", false
	}
	caseCount, caseCountOK := searchImpactStrictInteger(raw, "case_count")
	if !caseCountOK || caseCount <= 0 || caseCount != anyToInt(shadow["case_count"], 0) {
		return nil, "", false
	}
	influencedCaseCount, influencedCaseCountOK := searchImpactStrictInteger(raw, "influenced_case_count")
	if !influencedCaseCountOK || influencedCaseCount <= 0 || influencedCaseCount > caseCount {
		return nil, "", false
	}
	allowed := map[string]struct{}{}
	for _, field := range []string{
		"schema_id", "version", "comparison_scope", "comparison_fixed_k", "comparison_valid", "comparison_reason",
		"case_count", "influenced_case_count", "ranking_contract_id", "allocation_contract_id", "latency_basis",
		"same_returned_candidate_pool", "same_token_budget", "protected_selection_preserved", "case_set_ref",
		"candidate_pool_ref", "token_budget_ref", "protected_partition_ref", "ranking_vector_ref",
		"control_selection_ref", "treatment_selection_ref", "reputation_vector_ref", "baseline", "treatment",
		"authority",
	} {
		allowed[field] = struct{}{}
	}
	for field := range raw {
		if _, ok := allowed[field]; !ok {
			return nil, "", false
		}
	}
	baseline, baselineOK := contextPackLearnedActuatorNormalizedMetrics(anyMap(raw["baseline"]))
	treatment, treatmentOK := contextPackLearnedActuatorNormalizedMetrics(anyMap(raw["treatment"]))
	if !baselineOK || !treatmentOK {
		return nil, "", false
	}
	if _, pass := contextPackLearnedActuatorMetricsGate(baseline, treatment, caseCount); !pass {
		return nil, "", false
	}
	normalized := map[string]any{
		"schema_id": contextPackLearnedActuatorComparatorContractID, "version": 1,
		"comparison_scope":   contextPackLearnedActuatorComparisonScope,
		"comparison_fixed_k": contextPackLearnedActuatorFixedK,
		"comparison_valid":   true, "comparison_reason": "valid", "case_count": caseCount,
		"influenced_case_count":        influencedCaseCount,
		"ranking_contract_id":          contextPackLearnedActivationContractID,
		"allocation_contract_id":       "context_pack_evidence_allocation.v1",
		"latency_basis":                "measured_context_pack_rank_and_allocate_ms",
		"same_returned_candidate_pool": true, "same_token_budget": true,
		"protected_selection_preserved": true, "case_set_ref": raw["case_set_ref"],
		"baseline": baseline, "treatment": treatment,
	}
	for _, field := range []string{
		"candidate_pool_ref", "token_budget_ref", "protected_partition_ref", "ranking_vector_ref",
		"control_selection_ref", "treatment_selection_ref", "reputation_vector_ref",
	} {
		ref := contextPackLearnedDigestRef(anyToString(raw[field]))
		if ref == "" {
			return nil, "", false
		}
		normalized[field] = ref
	}
	if authority := anyMap(raw["authority"]); len(authority) > 0 {
		normalized["authority"] = cloneJSONMap(authority)
	}
	ref := contextPackLearnedCanonicalDigest(normalized)
	return normalized, ref, ref != ""
}

// contextPackLearnedActuatorComparatorProofForWorkspace is the activation
// boundary. Diagnostic comparator artifacts remain useful without authority,
// but only a canonical server-signed profile for the exact workspace can
// authorize live influence.
func contextPackLearnedActuatorComparatorProofForWorkspace(
	s *server,
	shadow map[string]any,
	workspaceRef string,
) (map[string]any, string, bool) {
	workspaceRef = contextPackLearnedDigestRef(workspaceRef)
	if workspaceRef == "" || contextPackLearnedDigestRef(anyToString(shadow["workspace_ref"])) != workspaceRef {
		return nil, "", false
	}
	normalized, proofRef, ok := contextPackLearnedActuatorComparatorProof(shadow)
	if !ok || !contextPackLearnedComparatorAuthorityEnvelopeValid(s, anyMap(normalized["authority"]), workspaceRef) {
		return nil, "", false
	}
	return normalized, proofRef, true
}

func contextPackLearnedActuatorNormalizedMetrics(raw map[string]any) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	floatFields := []string{
		"decision_impact_recall_at_5", "decision_impact_ndcg_at_5", "mrr", "numeric_exactness",
		"citation_coverage", "citation_exactness", "safety_failure_rate", "p95_latency_ms",
	}
	integerFields := []string{
		"safety_case_count", "safety_failure_count", "effective_k_min", "effective_k_max",
		"sparse_candidate_case_count", "numeric_expected_count", "citation_expected_count", "citation_candidate_count",
	}
	allowed := make(map[string]struct{}, len(floatFields)+len(integerFields))
	for _, field := range floatFields {
		allowed[field] = struct{}{}
	}
	for _, field := range integerFields {
		allowed[field] = struct{}{}
	}
	for field := range raw {
		if _, ok := allowed[field]; !ok {
			return nil, false
		}
	}
	normalized := make(map[string]any, len(allowed))
	for _, field := range floatFields {
		value, ok := searchImpactStrictFiniteNumber(raw, field)
		if !ok || !searchImpactComparatorMetricValueValid(field, value) {
			return nil, false
		}
		normalized[field] = value
	}
	for _, field := range integerFields {
		value, ok := searchImpactStrictInteger(raw, field)
		if !ok {
			return nil, false
		}
		normalized[field] = value
	}
	return normalized, true
}

func contextPackLearnedActuatorMetricsGate(baseline, treatment map[string]any, caseCount int) (string, bool) {
	if caseCount <= 0 {
		return "actuator_case_count_invalid", false
	}
	baselinePool, baselinePoolOK := searchImpactEffectivePoolMetadata(baseline, caseCount, contextPackLearnedActuatorFixedK)
	treatmentPool, treatmentPoolOK := searchImpactEffectivePoolMetadata(treatment, caseCount, contextPackLearnedActuatorFixedK)
	if !baselinePoolOK || !treatmentPoolOK || baselinePool.Minimum < 1 || treatmentPool.Minimum < 1 {
		return "actuator_effective_pool_invalid", false
	}
	for _, field := range []string{"numeric_expected_count", "citation_expected_count", "citation_candidate_count", "safety_case_count"} {
		baselineCount, baselineOK := searchImpactStrictInteger(baseline, field)
		treatmentCount, treatmentOK := searchImpactStrictInteger(treatment, field)
		if !baselineOK || !treatmentOK || baselineCount <= 0 || treatmentCount != baselineCount {
			return "actuator_denominator_mismatch", false
		}
	}
	if !searchImpactSafetyMetricsValid(baseline) || !searchImpactSafetyMetricsValid(treatment) {
		return "actuator_safety_denominator_invalid", false
	}
	metric := func(values map[string]any, field string) float64 {
		value, _ := searchImpactStrictFiniteNumber(values, field)
		return value
	}
	improved := metric(treatment, "decision_impact_recall_at_5") > metric(baseline, "decision_impact_recall_at_5") &&
		metric(treatment, "decision_impact_ndcg_at_5") > metric(baseline, "decision_impact_ndcg_at_5")
	if !improved {
		return "actuator_decision_impact_not_improved", false
	}
	noRegression := metric(treatment, "mrr") >= metric(baseline, "mrr") &&
		metric(treatment, "numeric_exactness") >= metric(baseline, "numeric_exactness") &&
		metric(treatment, "citation_coverage") >= metric(baseline, "citation_coverage") &&
		metric(treatment, "citation_exactness") >= metric(baseline, "citation_exactness") &&
		metric(treatment, "safety_failure_rate") <= metric(baseline, "safety_failure_rate") &&
		metric(treatment, "p95_latency_ms") <= metric(baseline, "p95_latency_ms")+5.0
	if !noRegression {
		return "actuator_metric_regression", false
	}
	return "valid", true
}

// searchImpactIntelligenceInput contains aggregate-safe, receipt-bound input.
// Comparative shadow evidence is deliberately optional: until an evaluator
// emits it, this layer reports shadow_eval_missing instead of guessing.
type searchImpactIntelligenceInput struct {
	CandidateOutcomes  []map[string]any
	UtilitySummary     map[string]any
	UtilityRows        []map[string]any
	TokenImpactSummary map[string]any
	ComparativeShadow  map[string]any
	ReceiptLedger      map[string]any
	ReceiptBinding     map[string]any
}

type searchImpactOutcome struct {
	OutcomeID                      string
	CapturedAt                     string
	CapturedAtTime                 time.Time
	ResponseBindingKey             string
	FirstPassSuccess               bool
	RepairRequired                 bool
	VerifierID                     string
	ObservedYieldEligible          bool
	WireTokensExact                int
	ModelVisibleContextTokensExact int
}

func (s *server) searchImpactIntelligenceSnapshot(project, taskClass string) map[string]any {
	return s.searchImpactIntelligenceSnapshotForScope(project, taskClass, "", false)
}

func (s *server) searchImpactIntelligenceSnapshotExact(project, taskClass, retrievalIntent string) map[string]any {
	return s.searchImpactIntelligenceSnapshotForScope(project, taskClass, retrievalIntent, true)
}

func (s *server) searchImpactIntelligenceSnapshotForScope(project, taskClass, retrievalIntent string, exact bool) map[string]any {
	rows := []map[string]any{}
	receiptBinding := map[string]any{"pass": false, "missing_receipt_outcome_count": 0}
	if s != nil && s.contextPackQuality != nil {
		rows, receiptBinding = s.contextPackQuality.receiptDurableOutcomeRows(searchImpactOutcomeSourceLimit)
	}
	rows = reconcileCandidateUtilityVerification(rows, utilityFromServer(s))
	return s.searchImpactIntelligenceSnapshotFromReconciledRows(
		project, taskClass, retrievalIntent, exact, rows, receiptBinding,
	)
}

func (s *server) searchImpactIntelligenceSnapshotFromReconciledRows(
	project, taskClass, retrievalIntent string,
	exact bool,
	rows []map[string]any,
	receiptBinding map[string]any,
) map[string]any {
	return s.searchImpactIntelligenceSnapshotFromReconciledRowsForScope(
		project, taskClass, retrievalIntent, "", exact, rows, receiptBinding,
	)
}

func (s *server) searchImpactIntelligenceSnapshotFromReconciledRowsForWorkspace(
	project, taskClass, retrievalIntent, workspaceRef string,
	rows []map[string]any,
	receiptBinding map[string]any,
) map[string]any {
	return s.searchImpactIntelligenceSnapshotFromReconciledRowsForScope(
		project, taskClass, retrievalIntent, contextPackLearnedDigestRef(workspaceRef), true, rows, receiptBinding,
	)
}

func (s *server) searchImpactIntelligenceSnapshotFromReconciledRowsForScope(
	project, taskClass, retrievalIntent, workspaceRef string,
	exact bool,
	rows []map[string]any,
	receiptBinding map[string]any,
) map[string]any {
	reconciled := searchImpactReconciledCandidateOutcomesForWorkspace(rows, project, taskClass, retrievalIntent, workspaceRef)
	utilitySummary := map[string]any{}
	utilityRows := []map[string]any{}
	if s != nil && s.utility != nil {
		scopedUtilityQuery := utilityQuery{
			Project: project, TaskClass: taskClass, WorkspaceRef: workspaceRef, Limit: searchImpactOutcomeSourceLimit,
		}
		if exact {
			outcomeIDs := make(map[string]struct{}, len(reconciled))
			for _, row := range reconciled {
				if outcomeID := strings.TrimSpace(anyToString(row["outcome_id"])); outcomeID != "" {
					outcomeIDs[outcomeID] = struct{}{}
				}
			}
			utilityRows = s.utility.rowsForOutcomeIDs(scopedUtilityQuery, outcomeIDs)
		} else {
			utilityRows = s.utility.rows(scopedUtilityQuery)
		}
		projected, pairs, exclusions := utilityPairProjection(utilityRows)
		utilitySummary = searchImpactUtilitySummary(utilityAggregate(projected, pairs, exclusions))
	}
	shadow := s.latestSearchImpactShadowEvaluationForWorkspace(project, taskClass, workspaceRef)
	impact := buildSearchImpactIntelligence(searchImpactIntelligenceInput{
		CandidateOutcomes:  reconciled,
		UtilitySummary:     utilitySummary,
		UtilityRows:        utilityRows,
		TokenImpactSummary: searchImpactTokenImpactSummary(s.tokenImpactTelemetrySnapshot()),
		ComparativeShadow:  shadow,
		ReceiptLedger:      searchImpactReceiptLedgerStatus(s),
		ReceiptBinding:     receiptBinding,
	})
	if exact && workspaceRef != "" {
		attachSearchImpactActivationEvidenceForWorkspace(s, impact, project, taskClass, retrievalIntent, workspaceRef, shadow, reconciled)
	} else if exact {
		attachSearchImpactActivationEvidence(impact, project, taskClass, retrievalIntent, shadow, reconciled)
	}
	return impact
}

func searchImpactReceiptLedgerStatus(s *server) map[string]any {
	if s == nil || s.contextPackQuality == nil {
		return contextPackQualityLedgerPublicStatus(nil)
	}
	return contextPackQualityLedgerPublicStatus(s.contextPackQuality.ledger)
}

// latestSearchImpactShadowEvaluation selects the newest artifact in the exact
// requested scope from the bounded recall-monitor index. A newer invalid
// artifact wins over an older valid artifact; ordinary monitor rows are never
// treated as comparative evidence.
func (s *server) latestSearchImpactShadowEvaluation(project, taskClass string) map[string]any {
	return s.latestSearchImpactShadowEvaluationForWorkspace(project, taskClass, "")
}

func (s *server) latestSearchImpactShadowEvaluationForWorkspace(project, taskClass, workspaceRef string) map[string]any {
	project = strings.TrimSpace(strings.ToLower(project))
	taskClass = strings.TrimSpace(strings.ToLower(taskClass))
	workspaceRef = contextPackLearnedDigestRef(workspaceRef)
	// A failed comparator append means a previously persisted pass is stale
	// relative to the current evaluation stream. Do not reuse it for a canary.
	if s != nil && s.searchImpactComparatorPersistenceUnavailable() {
		return map[string]any{
			"schema_id":         savedRecallImpactShadowEvalSchemaID,
			"comparison_valid":  false,
			"comparison_reason": "comparator_persistence_unavailable",
		}
	}
	return s.latestRecallMonitorShadowEvaluationForWorkspace(project, taskClass, workspaceRef)
}

func searchImpactNestedShadowEvaluations(row map[string]any) ([]map[string]any, bool) {
	raw, present := row["search_impact_shadow_evaluations"]
	if !present {
		return nil, false
	}
	artifacts := make([]map[string]any, 0)
	for _, item := range contextPackAnyList(raw) {
		artifact := anyMap(item)
		if len(artifact) > 0 {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, true
}

func searchImpactShadowScopeMatches(row map[string]any, project, taskClass string) bool {
	// Canary advice is cohort-specific; an unfiltered or partially filtered
	// request cannot be joined to a saved-case artifact without mixing scopes.
	if project == "" || taskClass == "" {
		return false
	}
	projectRef, taskClassRef, valid := searchImpactShadowScopeRefs(row)
	if !valid {
		return false
	}
	return projectRef == savedRecallImpactOpaqueScopeRef("project", project) &&
		taskClassRef == savedRecallImpactOpaqueScopeRef("task_class", taskClass)
}

func searchImpactShadowScopeRefs(row map[string]any) (string, string, bool) {
	_, hasProjectScopeRef := row["project_scope_ref"]
	_, hasTaskClassScopeRef := row["task_class_scope_ref"]
	_, hasProjectScopeRefs := row["project_scope_refs"]
	_, hasTaskClassScopeRefs := row["task_class_scope_refs"]
	if hasProjectScopeRef || hasTaskClassScopeRef {
		if !hasProjectScopeRef || !hasTaskClassScopeRef || hasProjectScopeRefs || hasTaskClassScopeRefs {
			return "", "", false
		}
		projectRef := strings.TrimSpace(anyToString(row["project_scope_ref"]))
		taskClassRef := strings.TrimSpace(anyToString(row["task_class_scope_ref"]))
		return projectRef, taskClassRef, isSearchIntelligenceFullSHA256Ref(projectRef) && isSearchIntelligenceFullSHA256Ref(taskClassRef)
	}
	projectRefs := anyToStringSlice(row["project_scope_refs"])
	taskClassRefs := anyToStringSlice(row["task_class_scope_refs"])
	if len(projectRefs) != 1 || len(taskClassRefs) != 1 {
		return "", "", false
	}
	projectRef := strings.TrimSpace(projectRefs[0])
	taskClassRef := strings.TrimSpace(taskClassRefs[0])
	return projectRef, taskClassRef, isSearchIntelligenceFullSHA256Ref(projectRef) && isSearchIntelligenceFullSHA256Ref(taskClassRef)
}

func searchImpactShadowIntentScopeMatches(row map[string]any, retrievalIntent string) bool {
	retrievalIntent = strings.TrimSpace(strings.ToLower(retrievalIntent))
	refs := anyToStringSlice(row["retrieval_intent_scope_refs"])
	return retrievalIntent != "" && len(refs) == 1 &&
		refs[0] == savedRecallImpactOpaqueScopeRef("retrieval_intent", retrievalIntent)
}

func searchImpactShadowWorkspaceMatches(row map[string]any, workspaceRef string) bool {
	workspaceRef = contextPackLearnedDigestRef(workspaceRef)
	return workspaceRef != "" && contextPackLearnedDigestRef(anyToString(row["workspace_ref"])) == workspaceRef
}

// attachSearchImpactActivationEvidence adds only an opaque, exact-scope proof
// envelope. Advisory snapshots remain useful without it, but runtime ranking
// cannot activate from mixed-intent or undated comparison evidence.
func attachSearchImpactActivationEvidence(
	impact map[string]any,
	project, taskClass, retrievalIntent string,
	shadow map[string]any,
	outcomes []map[string]any,
) {
	attachSearchImpactActivationEvidenceForWorkspace(nil, impact, project, taskClass, retrievalIntent, "", shadow, outcomes)
}

func attachSearchImpactActivationEvidenceForWorkspace(
	s *server,
	impact map[string]any,
	project, taskClass, retrievalIntent, workspaceRef string,
	shadow map[string]any,
	outcomes []map[string]any,
) {
	delete(impact, "activation_evidence")
	if !anyToBool(impact["canary_eligible"]) ||
		!searchImpactShadowScopeMatches(shadow, strings.ToLower(strings.TrimSpace(project)), strings.ToLower(strings.TrimSpace(taskClass))) ||
		!searchImpactShadowIntentScopeMatches(shadow, retrievalIntent) ||
		(workspaceRef != "" && !searchImpactShadowWorkspaceMatches(shadow, workspaceRef)) {
		return
	}
	var actuatorComparator map[string]any
	var actuatorComparatorRef string
	var actuatorComparatorOK bool
	if workspaceRef != "" {
		actuatorComparator, actuatorComparatorRef, actuatorComparatorOK = contextPackLearnedActuatorComparatorProofForWorkspace(s, shadow, workspaceRef)
	} else {
		actuatorComparator, actuatorComparatorRef, actuatorComparatorOK = contextPackLearnedActuatorComparatorProof(shadow)
	}
	if !actuatorComparatorOK {
		return
	}
	comparatorEvaluatedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(shadow["evaluated_at"])))
	if err != nil || !isSearchIntelligenceFullSHA256Ref(strings.TrimSpace(anyToString(shadow["case_set_ref"]))) {
		return
	}
	latestOutcome := time.Time{}
	for _, outcome := range outcomes {
		observed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(outcome["captured_at"])))
		if parseErr == nil && observed.After(latestOutcome) {
			latestOutcome = observed.UTC()
		}
	}
	if latestOutcome.IsZero() {
		return
	}
	evidence := map[string]any{
		"project_scope_ref":           contextPackLearnedScopeRef("project", project),
		"task_class_scope_ref":        contextPackLearnedScopeRef("task_class", taskClass),
		"retrieval_intent_scope_ref":  contextPackLearnedScopeRef("retrieval_intent", retrievalIntent),
		"case_set_ref":                shadow["case_set_ref"],
		"comparator_evaluated_at":     comparatorEvaluatedAt.UTC().Format(time.RFC3339Nano),
		"latest_candidate_outcome_at": latestOutcome.Format(time.RFC3339Nano),
		"actuator_comparator_ref":     actuatorComparatorRef,
		"reputation_vector_ref":       actuatorComparator["reputation_vector_ref"],
	}
	if workspaceRef != "" {
		evidence["workspace_ref"] = workspaceRef
	}
	proof := map[string]any{
		"schema_id":                   contextPackLearnedActivationContractID,
		"project_scope_ref":           evidence["project_scope_ref"],
		"task_class_scope_ref":        evidence["task_class_scope_ref"],
		"retrieval_intent_scope_ref":  evidence["retrieval_intent_scope_ref"],
		"case_set_ref":                evidence["case_set_ref"],
		"comparator_evaluated_at":     evidence["comparator_evaluated_at"],
		"latest_candidate_outcome_at": evidence["latest_candidate_outcome_at"],
		"actuator_comparator":         actuatorComparator,
		"proof_gates":                 impact["proof_gates"],
		"comparative_shadow":          shadow,
	}
	if workspaceRef != "" {
		proof["workspace_ref"] = workspaceRef
	}
	evidence["proof_digest"] = contextPackLearnedCanonicalDigest(proof)
	if !isSearchIntelligenceFullSHA256Ref(anyToString(evidence["proof_digest"])) {
		return
	}
	impact["activation_evidence"] = evidence
}

func searchImpactReconciledCandidateOutcomes(rows []map[string]any, project, taskClass string) []map[string]any {
	return searchImpactReconciledCandidateOutcomesForScope(rows, project, taskClass, "")
}

func searchImpactReconciledCandidateOutcomesForScope(rows []map[string]any, project, taskClass, retrievalIntent string) []map[string]any {
	return searchImpactReconciledCandidateOutcomesForWorkspace(rows, project, taskClass, retrievalIntent, "")
}

func searchImpactReconciledCandidateOutcomesForWorkspace(rows []map[string]any, project, taskClass, retrievalIntent, workspaceRef string) []map[string]any {
	workspaceRef = contextPackLearnedDigestRef(workspaceRef)
	outcomes := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Only the canonical durable outcome row may supply response identity.
		// Caller/request scope decorations are deliberately ignored; malformed
		// or partial binding attempts are not eligible for causal attribution.
		responseBinding, responseBindingKey, responseBindingValid := canonicalTelemetryResponseBinding(row)
		if !responseBindingValid || (responseBindingKey != "" && responseBinding == nil) {
			continue
		}
		if project != "" && !strings.EqualFold(anyToString(row["project"]), project) {
			continue
		}
		if taskClass != "" && !strings.EqualFold(anyToString(row["task_class"]), taskClass) {
			continue
		}
		if retrievalIntent != "" && !strings.EqualFold(anyToString(row["retrieval_intent"]), retrievalIntent) {
			continue
		}
		if workspaceRef != "" && contextPackLearnedDigestRef(anyToString(row["workspace_ref"])) != workspaceRef {
			continue
		}
		verified := false
		for _, rawAttribution := range contextPackAnyList(row["evidence_attribution"]) {
			attribution := anyMap(rawAttribution)
			if anyToString(attribution["entity_type"]) != "candidate" ||
				anyToString(attribution["result_level_credit"]) != "selection_receipt_bound" ||
				anyToString(attribution["selection_state"]) != "selected" {
				continue
			}
			if evidenceReputationCandidateUtilityVerified(row, attribution) {
				verified = true
				break
			}
		}
		verification := anyMap(row["candidate_utility_verification"])
		if !verified || !searchImpactCandidateOutcomeExactProof(verification) {
			continue
		}
		outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
		// Candidate attribution must be ordered by the gateway's receipt, never
		// a reporter-controlled captured_at field. Legacy rows lacking this
		// persisted server observation are deliberately ineligible.
		gatewayReceivedAt := strings.TrimSpace(anyToString(row["gateway_received_at"]))
		gatewayReceivedAtTime, err := time.Parse(time.RFC3339Nano, gatewayReceivedAt)
		if outcomeID == "" || err != nil {
			continue
		}
		outcome := map[string]any{
			"outcome_id":                         outcomeID,
			"captured_at":                        gatewayReceivedAtTime.UTC().Format(time.RFC3339Nano),
			"gateway_received_at":                gatewayReceivedAtTime.UTC().Format(time.RFC3339Nano),
			"first_pass_success":                 anyToBool(row["first_pass_success"]),
			"repair_required":                    anyToBool(row["repair_required"]),
			"verifier_id":                        strings.TrimSpace(anyToString(verification["verifier_id"])),
			"observed_yield_eligible":            anyToBool(verification["observed_yield_eligible"]),
			"wire_tokens_exact":                  anyToInt(verification["wire_tokens_exact"], 0),
			"model_visible_context_tokens_exact": anyToInt(verification["model_visible_context_tokens_exact"], 0),
			"retrieval_intent":                   strings.TrimSpace(strings.ToLower(anyToString(row["retrieval_intent"]))),
			"workspace_ref":                      contextPackLearnedDigestRef(anyToString(row["workspace_ref"])),
		}
		if responseBindingKey != "" {
			// Carry only the opaque canonical digest/key. Raw response/query/path
			// material is outside the search-impact contract.
			outcome["response_binding_key"] = responseBindingKey
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// searchImpactCandidateOutcomeExactProof is deliberately stricter than a
// scoped Utility Ledger aggregate. The candidate outcome itself must carry the
// independently reconciled observed-yield eligibility and both exact positive
// token denominators before it can contribute to an advisory gate.
func searchImpactCandidateOutcomeExactProof(verification map[string]any) bool {
	return anyToBool(verification["observed_yield_eligible"]) &&
		anyToInt(verification["wire_tokens_exact"], 0) > 0 &&
		anyToInt(verification["model_visible_context_tokens_exact"], 0) > 0
}

func searchImpactUtilitySummary(summary map[string]any) map[string]any {
	if len(summary) == 0 {
		return map[string]any{}
	}
	interval := anyMap(summary["causal_interval"])
	return map[string]any{
		"independently_verified_count": anyToInt(summary["independently_verified_count"], 0),
		"causal_pair_count":            anyToInt(summary["causal_pair_count"], 0),
		"causal_interval": map[string]any{
			"status":    anyToString(interval["status"]),
			"available": anyToBool(interval["available"]) || anyToString(interval["status"]) == "available",
			"low":       interval["low"],
			"point":     interval["point"],
			"high":      interval["high"],
		},
	}
}

func searchImpactTokenImpactSummary(snapshot map[string]any) map[string]any {
	if len(snapshot) == 0 {
		return map[string]any{}
	}
	cohorts := make([]any, 0)
	switch rows := snapshot["cohorts"].(type) {
	case []map[string]any:
		for _, cohort := range rows {
			cohorts = append(cohorts, cloneMap(cohort))
		}
	case []any:
		for _, rawCohort := range rows {
			if cohort := anyMap(rawCohort); len(cohort) > 0 {
				cohorts = append(cohorts, cloneMap(cohort))
			}
		}
	}
	return map[string]any{
		"sample_count":                  anyToInt(snapshot["sample_count"], 0),
		"legacy_sample_count":           anyToInt(snapshot["legacy_sample_count"], 0),
		"exact_artifact_replay_count":   anyToInt(snapshot["exact_artifact_replay_count"], 0),
		"exact_artifact_conflict_count": anyToInt(snapshot["exact_artifact_conflict_count"], 0),
		"cohort_window_sample_count":    anyToInt(snapshot["cohort_window_sample_count"], 0),
		"cohort_total_count":            anyToInt(snapshot["cohort_total_count"], 0),
		"cohort_omitted_count":          anyToInt(snapshot["cohort_omitted_count"], 0),
		"cohorts":                       cohorts,
	}
}

// searchImpactTokenImpactContext keeps global token telemetry visible without
// implying that it belongs to the exact project/task cohort. Token samples do
// not carry that identity and are never evidence for the advisory gate.
func searchImpactTokenImpactContext(summary map[string]any) map[string]any {
	return map[string]any{
		"scope":      "global_contextual_non_gating",
		"non_gating": true,
		"summary":    cloneMap(summary),
	}
}

func buildSearchImpactIntelligence(input searchImpactIntelligenceInput) map[string]any {
	outcomes, outcomeReplayCount, outcomeConflictCount := searchImpactDeduplicateOutcomes(input.CandidateOutcomes)
	candidateCausalSummary := searchImpactCandidateCausalSummary(outcomes, input.UtilityRows)
	train, holdout := searchImpactChronologicalSplit(outcomes)
	trainSummary := searchImpactOutcomeSummary(train)
	holdoutSummary := searchImpactOutcomeSummary(holdout)
	negativeCount := anyToInt(trainSummary["negative_outcome_count"], 0) + anyToInt(holdoutSummary["negative_outcome_count"], 0)
	verifierCount := searchImpactIndependentVerifierCount(outcomes)
	shadow := searchImpactShadowEvaluation(input.ComparativeShadow)
	receiptLedgerGate := searchImpactReceiptLedgerDurabilityGate(input.ReceiptLedger, input.ReceiptBinding)

	exactDenominators := searchImpactExactOutcomeDenominators(outcomes)
	wireTokens := anyToInt(exactDenominators["wire_tokens_exact"], 0)
	modelVisibleTokens := anyToInt(exactDenominators["model_visible_context_tokens_exact"], 0)
	exactOutcomeCount := anyToInt(exactDenominators["observed_yield_eligible_outcome_count"], 0)
	exactDenominatorsPass := len(outcomes) > 0 && wireTokens > 0 && modelVisibleTokens > 0 && exactOutcomeCount == len(outcomes)
	interval := anyMap(candidateCausalSummary["causal_interval"])
	causalPairCount := anyToInt(candidateCausalSummary["causal_pair_count"], 0)
	causalIntervalPass := (anyToBool(interval["available"]) || anyToString(interval["status"]) == "available") &&
		causalPairCount >= 2 && searchImpactNumberAboveZero(interval, "low")
	trainHoldoutPass := len(train) >= searchImpactMinTrainOutcomes && len(holdout) >= searchImpactMinHoldoutOutcomes
	negativeRetentionPass := negativeCount >= searchImpactMinNegativeOutcomes
	verifierPass := verifierCount >= searchImpactMinIndependentVerifiers
	performanceRegressionAbsent := searchImpactPerformanceRegressionAbsent(trainSummary, holdoutSummary)
	shadowPass := anyToBool(shadow["pass"])
	outcomeIdentityPass := outcomeConflictCount == 0

	proofGates := map[string]any{
		"fixed_thresholds": map[string]any{
			"minimum_train_outcomes":        searchImpactMinTrainOutcomes,
			"minimum_holdout_outcomes":      searchImpactMinHoldoutOutcomes,
			"minimum_negative_outcomes":     searchImpactMinNegativeOutcomes,
			"minimum_independent_verifiers": searchImpactMinIndependentVerifiers,
		},
		"comparative_shadow": map[string]any{
			"pass":   shadowPass,
			"status": anyToString(shadow["status"]),
		},
		"receipt_ledger_durability": receiptLedgerGate,
		"train_holdout_minimums": map[string]any{
			"pass":          trainHoldoutPass,
			"train_count":   len(train),
			"holdout_count": len(holdout),
		},
		"negative_retention": map[string]any{
			"pass":                   negativeRetentionPass,
			"negative_outcome_count": negativeCount,
		},
		"independent_verifiers": map[string]any{
			"pass":  verifierPass,
			"count": verifierCount,
		},
		"exact_denominators": map[string]any{
			"pass":                                  exactDenominatorsPass,
			"wire_tokens_exact":                     wireTokens,
			"model_visible_context_tokens_exact":    modelVisibleTokens,
			"observed_yield_eligible_outcome_count": exactOutcomeCount,
			"deduplicated_outcome_count":            len(outcomes),
		},
		"causal_interval": map[string]any{
			"pass":              causalIntervalPass,
			"causal_pair_count": causalPairCount,
			"status":            anyToString(interval["status"]),
			"low":               interval["low"],
			"source":            "exact_receipt_bound_candidate_outcome_ids",
		},
		"outcome_regressions_absent": map[string]any{
			"pass": performanceRegressionAbsent,
		},
		"outcome_identity_consistency": map[string]any{
			"pass":           outcomeIdentityPass,
			"replay_count":   outcomeReplayCount,
			"conflict_count": outcomeConflictCount,
		},
	}
	canaryEligible := shadowPass && anyToBool(receiptLedgerGate["pass"]) && trainHoldoutPass && negativeRetentionPass && verifierPass && exactDenominatorsPass && causalIntervalPass && performanceRegressionAbsent && outcomeIdentityPass
	reasons := searchImpactAbstentionReasons(proofGates)
	status := "abstain"
	recommendation := "insufficient_evidence"
	if canaryEligible {
		status = "advisory_ready"
		recommendation = "canary_recommended"
		reasons = []string{}
	}
	return map[string]any{
		"ok":              true,
		"schema_id":       searchImpactIntelligenceContractID,
		"version":         1,
		"updated_at":      nowUTCISO(),
		"status":          status,
		"recommendation":  recommendation,
		"canary_eligible": canaryEligible,
		"activation": map[string]any{
			"performed":           false,
			"entitlement_changed": false,
			"mode":                "advisory_only",
		},
		"recall_intelligence": map[string]any{
			"comparative_shadow": shadow,
		},
		"outcome_intelligence": map[string]any{
			"receipt_bound_utility_reconciled_candidate_outcome_count": len(outcomes),
			"deduplicated_outcome_count":                               len(outcomes),
			"outcome_replay_count":                                     outcomeReplayCount,
			"outcome_conflict_count":                                   outcomeConflictCount,
			"chronological_split": map[string]any{
				"train":   trainSummary,
				"holdout": holdoutSummary,
			},
			"independent_verifier_count": verifierCount,
		},
		"impact_intelligence": map[string]any{
			"utility_reconciliation": searchImpactUtilityReconciliation(input.UtilitySummary, exactDenominators),
			"token_impact":           searchImpactTokenImpactContext(input.TokenImpactSummary),
		},
		"proof_gates":        proofGates,
		"abstention_reasons": reasons,
		"measurement_limit":  "Advisory-only evidence summary. Scoped Utility Ledger totals are contextual and non-gating; the causal canary gate is derived separately only from exact receipt-bound candidate outcome IDs reconciled to an independent Utility Ledger verifier. Token-impact telemetry is global contextual non-gating because it lacks project/task identity. Comparative shadow recall is never inferred from ordinary telemetry.",
	}
}

// searchImpactCandidateCausalSummary derives causal evidence from only the
// exact, deduplicated receipt-bound candidate outcome IDs that reach the
// impact report. A scoped Utility Ledger aggregate may include unrelated
// observations, so it is deliberately never used for the canary gate.
func searchImpactCandidateCausalSummary(outcomes []searchImpactOutcome, utilityRows []map[string]any) map[string]any {
	if len(outcomes) == 0 || len(utilityRows) == 0 {
		return searchImpactUtilitySummary(utilityAggregate(nil, nil, map[string]int{}))
	}
	candidateBindings := make(map[string]string, len(outcomes))
	for _, outcome := range outcomes {
		if outcomeID := strings.TrimSpace(outcome.OutcomeID); outcomeID != "" {
			bindingKey := strings.TrimSpace(outcome.ResponseBindingKey)
			if bindingKey != "" && !utilitySHA256DigestValid(bindingKey) {
				continue
			}
			candidateBindings[outcomeID] = bindingKey
		}
	}
	matchedRows := make([]map[string]any, 0, len(candidateBindings))
	for _, row := range utilityRows {
		outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
		candidateBindingKey, matched := candidateBindings[outcomeID]
		if !matched {
			continue
		}
		utilityBinding, utilityBindingKey, utilityBindingValid := canonicalTelemetryResponseBinding(row)
		if !utilityBindingValid || candidateBindingKey != utilityBindingKey ||
			(candidateBindingKey != "" && utilityBinding == nil) {
			// A bound candidate requires the exact Utility binding; legacy
			// candidates require a legacy Utility row. One-sided and malformed
			// rows cannot contribute causal evidence.
			continue
		}
		matchedRows = append(matchedRows, cloneAnyMap(row))
	}
	projected, pairs, exclusions := utilityPairProjection(matchedRows)
	return searchImpactUtilitySummary(utilityAggregate(projected, pairs, exclusions))
}

func searchImpactReceiptLedgerDurabilityGate(storage, binding map[string]any) map[string]any {
	enabled := anyToBool(storage["enabled"])
	lastError := strings.TrimSpace(anyToString(storage["last_error"]))
	status := "ready"
	bindingPass := anyToBool(binding["pass"])
	pass := enabled && lastError == "" && bindingPass
	if !enabled {
		status = "disabled_or_unavailable"
	} else if lastError != "" {
		status = "latest_write_failed"
	} else if !bindingPass {
		status = "referenced_receipt_missing"
	}
	return map[string]any{
		"pass":                          pass,
		"status":                        status,
		"durability":                    anyToString(storage["durability"]),
		"write_errors":                  anyToInt(storage["write_errors"], 0),
		"specific_receipt_pass":         bindingPass,
		"missing_receipt_outcome_count": anyToInt(binding["missing_receipt_outcome_count"], 0),
	}
}

func searchImpactUtilityReconciliation(summary, exactDenominators map[string]any) map[string]any {
	out := cloneMap(summary)
	// Scoped Utility Ledger totals are useful context but never causal-gate
	// evidence. The candidate-only causal summary is calculated separately from
	// exact receipt-bound candidate outcome IDs in buildSearchImpactIntelligence.
	out["scoped_summary_role"] = "contextual_non_gating"
	out["candidate_causal_gate_basis"] = "exact_receipt_bound_candidate_outcome_ids"
	out["credited_candidate_outcome_denominators"] = cloneMap(exactDenominators)
	return out
}

func searchImpactExactOutcomeDenominators(outcomes []searchImpactOutcome) map[string]any {
	wireTokens, modelVisibleTokens, eligibleCount := 0, 0, 0
	for _, outcome := range outcomes {
		if !outcome.ObservedYieldEligible || outcome.WireTokensExact <= 0 || outcome.ModelVisibleContextTokensExact <= 0 {
			continue
		}
		eligibleCount++
		wireTokens += outcome.WireTokensExact
		modelVisibleTokens += outcome.ModelVisibleContextTokensExact
	}
	return map[string]any{
		"observed_yield_eligible_outcome_count": eligibleCount,
		"wire_tokens_exact":                     wireTokens,
		"model_visible_context_tokens_exact":    modelVisibleTokens,
	}
}

func searchImpactDeduplicateOutcomes(rows []map[string]any) ([]searchImpactOutcome, int, int) {
	byID := make(map[string]searchImpactOutcome)
	conflicted := make(map[string]struct{})
	bindingOwner := make(map[string]string)
	bindingConflicted := make(map[string]struct{})
	replayCount, conflictCount := 0, 0
	markConflicted := func(outcomeID string) {
		if outcomeID == "" {
			return
		}
		if existing, found := byID[outcomeID]; found {
			if existing.ResponseBindingKey != "" && bindingOwner[existing.ResponseBindingKey] == outcomeID {
				delete(bindingOwner, existing.ResponseBindingKey)
			}
			if existing.ResponseBindingKey != "" {
				bindingConflicted[existing.ResponseBindingKey] = struct{}{}
			}
			delete(byID, outcomeID)
		}
		conflicted[outcomeID] = struct{}{}
	}
	for _, row := range rows {
		outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
		gatewayReceivedAt := strings.TrimSpace(anyToString(row["gateway_received_at"]))
		gatewayReceivedAtTime, err := time.Parse(time.RFC3339Nano, gatewayReceivedAt)
		if outcomeID == "" || err != nil {
			continue
		}
		_, responseBindingKey, responseBindingValid := canonicalTelemetryResponseBinding(row)
		if !responseBindingValid {
			// A malformed binding may never become the winner. If a valid row
			// already exists for this identity, discard it as a conflict too.
			if _, blocked := conflicted[outcomeID]; !blocked {
				conflictCount++
				markConflicted(outcomeID)
			}
			continue
		}
		candidate := searchImpactOutcome{
			OutcomeID: outcomeID, CapturedAt: gatewayReceivedAtTime.UTC().Format(time.RFC3339Nano), CapturedAtTime: gatewayReceivedAtTime.UTC(),
			ResponseBindingKey: responseBindingKey,
			FirstPassSuccess:   anyToBool(row["first_pass_success"]), RepairRequired: anyToBool(row["repair_required"]),
			VerifierID:                     strings.TrimSpace(anyToString(row["verifier_id"])),
			ObservedYieldEligible:          anyToBool(row["observed_yield_eligible"]),
			WireTokensExact:                anyToInt(row["wire_tokens_exact"], 0),
			ModelVisibleContextTokensExact: anyToInt(row["model_visible_context_tokens_exact"], 0),
		}
		if _, blocked := conflicted[outcomeID]; blocked {
			continue
		}
		if existing, found := byID[outcomeID]; found {
			if searchImpactOutcomeIdentityMatches(existing, candidate) {
				replayCount++
			} else {
				conflictCount++
				// Do not pick an earlier conflicting reporter-controlled time (or
				// any arbitrary winner) for the train/holdout split.
				if candidate.ResponseBindingKey != "" {
					bindingConflicted[candidate.ResponseBindingKey] = struct{}{}
				}
				markConflicted(outcomeID)
			}
		} else {
			if responseBindingKey != "" {
				if _, blocked := bindingConflicted[responseBindingKey]; blocked {
					conflictCount++
					markConflicted(outcomeID)
					continue
				}
				if owner, found := bindingOwner[responseBindingKey]; found && owner != outcomeID {
					conflictCount++
					markConflicted(owner)
					markConflicted(outcomeID)
					bindingConflicted[responseBindingKey] = struct{}{}
					delete(bindingOwner, responseBindingKey)
					continue
				}
			}
			byID[outcomeID] = candidate
			if responseBindingKey != "" {
				bindingOwner[responseBindingKey] = outcomeID
			}
		}
	}
	outcomes := make([]searchImpactOutcome, 0, len(byID))
	for _, outcome := range byID {
		outcomes = append(outcomes, outcome)
	}
	sort.Slice(outcomes, func(i, j int) bool {
		if outcomes[i].CapturedAt == outcomes[j].CapturedAt {
			return outcomes[i].OutcomeID < outcomes[j].OutcomeID
		}
		return outcomes[i].CapturedAt < outcomes[j].CapturedAt
	})
	return outcomes, replayCount, conflictCount
}

func searchImpactOutcomeIdentityMatches(left, right searchImpactOutcome) bool {
	return left.CapturedAt == right.CapturedAt &&
		left.ResponseBindingKey == right.ResponseBindingKey &&
		left.FirstPassSuccess == right.FirstPassSuccess &&
		left.RepairRequired == right.RepairRequired &&
		left.VerifierID == right.VerifierID &&
		left.ObservedYieldEligible == right.ObservedYieldEligible &&
		left.WireTokensExact == right.WireTokensExact &&
		left.ModelVisibleContextTokensExact == right.ModelVisibleContextTokensExact
}

func searchImpactChronologicalSplit(outcomes []searchImpactOutcome) ([]searchImpactOutcome, []searchImpactOutcome) {
	if len(outcomes) == 0 {
		return nil, nil
	}
	holdoutCount := (len(outcomes) + 4) / 5 // fixed, chronological ceil(20%)
	if holdoutCount > len(outcomes) {
		holdoutCount = len(outcomes)
	}
	trainEnd := len(outcomes) - holdoutCount
	return outcomes[:trainEnd], outcomes[trainEnd:]
}

func searchImpactOutcomeSummary(outcomes []searchImpactOutcome) map[string]any {
	firstPass, repairs, negatives := 0, 0, 0
	for _, outcome := range outcomes {
		if outcome.FirstPassSuccess {
			firstPass++
		}
		if outcome.RepairRequired {
			repairs++
		}
		if outcome.RepairRequired || !outcome.FirstPassSuccess {
			negatives++
		}
	}
	firstPassRate, repairRate := any(nil), any(nil)
	if len(outcomes) > 0 {
		firstPassRate = roundFloat(float64(firstPass)/float64(len(outcomes)), 6)
		repairRate = roundFloat(float64(repairs)/float64(len(outcomes)), 6)
	}
	return map[string]any{
		"outcome_count":            len(outcomes),
		"first_pass_success_count": firstPass,
		"repair_required_count":    repairs,
		"negative_outcome_count":   negatives,
		"first_pass_success_rate":  firstPassRate,
		"repair_required_rate":     repairRate,
	}
}

func searchImpactIndependentVerifierCount(outcomes []searchImpactOutcome) int {
	verifiers := make(map[string]struct{})
	for _, outcome := range outcomes {
		if outcome.VerifierID != "" {
			verifiers[outcome.VerifierID] = struct{}{}
		}
	}
	return len(verifiers)
}

func searchImpactPerformanceRegressionAbsent(train, holdout map[string]any) bool {
	if anyToInt(train["outcome_count"], 0) == 0 || anyToInt(holdout["outcome_count"], 0) == 0 {
		return false
	}
	return anyToFloat(holdout["first_pass_success_rate"]) >= anyToFloat(train["first_pass_success_rate"]) &&
		anyToFloat(holdout["repair_required_rate"]) <= anyToFloat(train["repair_required_rate"])
}

func searchImpactShadowEvaluation(shadow map[string]any) map[string]any {
	if len(shadow) == 0 {
		return map[string]any{"status": "shadow_eval_missing", "pass": false, "comparative_evidence_present": false}
	}
	if anyToString(shadow["schema_id"]) != savedRecallImpactShadowEvalSchemaID {
		return map[string]any{"status": "shadow_eval_invalid", "pass": false, "comparative_evidence_present": true}
	}
	if !anyToBool(shadow["comparison_valid"]) {
		return map[string]any{
			"status":                       searchImpactShadowInvalidStatus(anyToString(shadow["comparison_reason"])),
			"pass":                         false,
			"comparative_evidence_present": true,
		}
	}
	if !searchImpactShadowEnvelopeValid(shadow) {
		return map[string]any{"status": "shadow_eval_invalid", "pass": false, "comparative_evidence_present": true}
	}
	baseline, candidate := anyMap(shadow["baseline"]), anyMap(shadow["shadow"])
	metrics := []string{
		"decision_impact_recall_at_5", "decision_impact_ndcg_at_5", "mrr", "numeric_exactness",
		"citation_coverage", "citation_exactness", "safety_failure_rate", "p95_latency_ms",
	}
	values := make(map[string][2]float64, len(metrics))
	for _, metric := range metrics {
		baselineValue, baselinePresent := searchImpactStrictFiniteNumber(baseline, metric)
		candidateValue, candidatePresent := searchImpactStrictFiniteNumber(candidate, metric)
		if !baselinePresent || !candidatePresent ||
			!searchImpactComparatorMetricValueValid(metric, baselineValue) ||
			!searchImpactComparatorMetricValueValid(metric, candidateValue) {
			return map[string]any{"status": "shadow_eval_invalid", "pass": false, "comparative_evidence_present": true}
		}
		values[metric] = [2]float64{baselineValue, candidateValue}
	}
	improved := values["decision_impact_recall_at_5"][1] > values["decision_impact_recall_at_5"][0] &&
		values["decision_impact_ndcg_at_5"][1] > values["decision_impact_ndcg_at_5"][0]
	noRegression := values["mrr"][1] >= values["mrr"][0] &&
		values["numeric_exactness"][1] >= values["numeric_exactness"][0] &&
		values["citation_coverage"][1] >= values["citation_coverage"][0] &&
		values["citation_exactness"][1] >= values["citation_exactness"][0] &&
		values["safety_failure_rate"][1] <= values["safety_failure_rate"][0] &&
		values["p95_latency_ms"][1] <= values["p95_latency_ms"][0]
	baselineSafetyCases, baselineSafetyCasesValid := searchImpactStrictInteger(baseline, "safety_case_count")
	candidateSafetyCases, candidateSafetyCasesValid := searchImpactStrictInteger(candidate, "safety_case_count")
	safetyDenominatorsValid := baselineSafetyCasesValid && candidateSafetyCasesValid && baselineSafetyCases > 0 && candidateSafetyCases == baselineSafetyCases &&
		searchImpactSafetyMetricsValid(baseline) && searchImpactSafetyMetricsValid(candidate)
	if !safetyDenominatorsValid {
		return map[string]any{"status": "shadow_eval_invalid", "pass": false, "comparative_evidence_present": true}
	}
	if !improved || !noRegression {
		return map[string]any{"status": "shadow_eval_regression", "pass": false, "comparative_evidence_present": true, "decision_impact_improved": improved, "non_regression": noRegression}
	}
	return map[string]any{"status": "valid", "pass": true, "comparative_evidence_present": true, "decision_impact_improved": true, "non_regression": true, "safety_denominators_valid": true}
}

func searchImpactShadowEnvelopeValid(shadow map[string]any) bool {
	version, versionValid := searchImpactStrictInteger(shadow, "version")
	fixedK, fixedKValid := searchImpactStrictInteger(shadow, "comparison_fixed_k")
	caseCount, caseCountValid := searchImpactStrictInteger(shadow, "case_count")
	if !versionValid || version != 1 || !fixedKValid || fixedK != savedRecallImpactK || !caseCountValid || caseCount <= 0 {
		return false
	}
	if anyToString(shadow["comparison_scope"]) != savedRecallImpactComparisonScope ||
		!isSearchIntelligenceFullSHA256Ref(strings.TrimSpace(anyToString(shadow["case_set_ref"]))) ||
		anyToString(shadow["latency_basis"]) != "shared_synthetic_retrieval_replay_ms" {
		return false
	}
	if _, _, valid := searchImpactShadowScopeRefs(shadow); !valid {
		return false
	}
	baseline, candidate := anyMap(shadow["baseline"]), anyMap(shadow["shadow"])
	baselinePool, baselinePoolValid := searchImpactEffectivePoolMetadata(baseline, caseCount, fixedK)
	candidatePool, candidatePoolValid := searchImpactEffectivePoolMetadata(candidate, caseCount, fixedK)
	return baselinePoolValid && candidatePoolValid && baselinePool == candidatePool
}

type searchImpactEffectivePool struct {
	Minimum     int
	Maximum     int
	SparseCases int
}

func searchImpactEffectivePoolMetadata(metrics map[string]any, caseCount, fixedK int) (searchImpactEffectivePool, bool) {
	minimum, minimumValid := searchImpactStrictInteger(metrics, "effective_k_min")
	maximum, maximumValid := searchImpactStrictInteger(metrics, "effective_k_max")
	sparseCases, sparseCasesValid := searchImpactStrictInteger(metrics, "sparse_candidate_case_count")
	if !minimumValid || !maximumValid || !sparseCasesValid || minimum < 1 || maximum < minimum || maximum > fixedK || sparseCases < 0 || sparseCases > caseCount {
		return searchImpactEffectivePool{}, false
	}
	return searchImpactEffectivePool{Minimum: minimum, Maximum: maximum, SparseCases: sparseCases}, true
}

func searchImpactStrictInteger(values map[string]any, key string) (int, bool) {
	value, present := searchImpactStrictFiniteNumber(values, key)
	if !present || math.Trunc(value) != value || value < 0 || value > 1_000_000_000 {
		return 0, false
	}
	return int(value), true
}

// searchImpactStrictFiniteNumber accepts only native numeric types. Comparator
// evidence must not coerce strings, booleans, or arbitrary decoded values to a
// numeric zero because that could turn malformed evidence into a pass.
func searchImpactStrictFiniteNumber(values map[string]any, key string) (float64, bool) {
	raw, present := values[key]
	if !present || raw == nil {
		return 0, false
	}
	var value float64
	switch typed := raw.(type) {
	case float64:
		value = typed
	case float32:
		value = float64(typed)
	case int:
		value = float64(typed)
	case int64:
		value = float64(typed)
	case int32:
		value = float64(typed)
	case int16:
		value = float64(typed)
	case int8:
		value = float64(typed)
	case uint:
		value = float64(typed)
	case uint64:
		value = float64(typed)
	case uint32:
		value = float64(typed)
	case uint16:
		value = float64(typed)
	case uint8:
		value = float64(typed)
	default:
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func searchImpactSafetyMetricsValid(metrics map[string]any) bool {
	cases, casesValid := searchImpactStrictInteger(metrics, "safety_case_count")
	failures, failuresValid := searchImpactStrictInteger(metrics, "safety_failure_count")
	rate, rateValid := searchImpactStrictFiniteNumber(metrics, "safety_failure_rate")
	if !casesValid || !failuresValid || !rateValid || cases <= 0 || failures > cases {
		return false
	}
	return math.Abs(rate-float64(failures)/float64(cases)) <= 0.000001
}

func searchImpactShadowInvalidStatus(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "scope_mismatch":
		return "shadow_eval_scope_mismatch"
	case "case_set_invalid":
		return "shadow_eval_case_set_invalid"
	case "decision_impact_grade_missing_or_invalid":
		return "shadow_eval_impact_grades_missing"
	case "numeric_expectations_missing":
		return "shadow_eval_numeric_ground_truth_missing"
	case "safety_cases_missing":
		return "shadow_eval_safety_cases_missing"
	case "comparator_persistence_unavailable":
		return "shadow_eval_comparator_persistence_unavailable"
	case "comparator_index_unavailable":
		return "shadow_eval_comparator_index_unavailable"
	case "comparator_index_stale":
		return "shadow_eval_comparator_index_stale"
	case "shadow_top_k_unmappable", "shadow_returned_pool_incomplete":
		return "shadow_eval_returned_pool_incomplete"
	default:
		return "shadow_eval_invalid"
	}
}

func searchImpactNumberAboveZero(values map[string]any, key string) bool {
	value, present := utilityNumberPresent(values, nil, key)
	return present && !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func searchImpactAbstentionReasons(proofGates map[string]any) []string {
	reasons := []string{}
	for _, gate := range []struct {
		name string
		key  string
	}{
		{name: "shadow_eval_missing", key: "comparative_shadow"},
		{name: "receipt_ledger_durability_unavailable", key: "receipt_ledger_durability"},
		{name: "train_holdout_minimums_not_met", key: "train_holdout_minimums"},
		{name: "negative_retention_not_met", key: "negative_retention"},
		{name: "independent_verifiers_not_met", key: "independent_verifiers"},
		{name: "exact_denominators_not_met", key: "exact_denominators"},
		{name: "causal_interval_not_positive", key: "causal_interval"},
		{name: "outcome_regression_detected", key: "outcome_regressions_absent"},
		{name: "outcome_identity_conflict", key: "outcome_identity_consistency"},
	} {
		gateValue := anyMap(proofGates[gate.key])
		if anyToBool(gateValue["pass"]) {
			continue
		}
		if gate.key == "comparative_shadow" && anyToString(gateValue["status"]) != "shadow_eval_missing" {
			reasons = append(reasons, anyToString(gateValue["status"]))
			continue
		}
		reasons = append(reasons, gate.name)
	}
	return reasons
}

func (s *server) telemetrySearchImpactRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	taskClass := strings.TrimSpace(r.URL.Query().Get("task_class"))
	if project == "" || taskClass == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "project and task_class query parameters are required",
		})
		return
	}
	payload := s.searchImpactIntelligenceSnapshot(project, taskClass)
	payload["filters"] = map[string]any{
		"exact_scope": map[string]any{
			"project_filter_applied":    true,
			"task_class_filter_applied": true,
			"applies_to": []any{
				"receipt_bound_candidate_outcomes",
				"candidate_causal_gate",
				"comparative_shadow",
			},
		},
		"global_contextual_non_gating": []any{"token_impact"},
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(searchImpactIntelligenceContractID, payload, "", "search_impact_intelligence", searchImpactIntelligencePath))
}
