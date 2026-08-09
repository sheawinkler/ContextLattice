package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	contextPackLearnedActivationContractID = "context_pack_learned_activation.v1"
	contextPackLearnedComparatorMaxAge     = 7 * 24 * time.Hour
	contextPackLearnedOutcomeMaxAge        = 30 * 24 * time.Hour
	contextPackLearnedReputationMaxAge     = 15 * time.Minute
	contextPackLearnedMinimumSamples       = 5
	contextPackLearnedMinimumIssuers       = 2
)

type contextPackLearnedActivationPhase string

const (
	contextPackLearnedActivationFinal           contextPackLearnedActivationPhase = "final"
	contextPackLearnedActivationNeedsImpact     contextPackLearnedActivationPhase = "needs_impact"
	contextPackLearnedActivationNeedsReputation contextPackLearnedActivationPhase = "needs_reputation"
)

type contextPackLearnedAuthorityContextKey struct{}

func contextWithContextPackLearnedAuthority(ctx context.Context, authority contextPackLearnedActivationAuthority) context.Context {
	return context.WithValue(ctx, contextPackLearnedAuthorityContextKey{}, authority)
}

func contextPackLearnedAuthorityFromContext(ctx context.Context) contextPackLearnedActivationAuthority {
	if ctx == nil {
		return contextPackLearnedActivationAuthority{Reason: "activation_authority_unavailable"}
	}
	authority, ok := ctx.Value(contextPackLearnedAuthorityContextKey{}).(contextPackLearnedActivationAuthority)
	if !ok {
		return contextPackLearnedActivationAuthority{Reason: "activation_authority_unavailable"}
	}
	return authority
}

func (s *server) contextWithContextPackLearnedRequestAuthority(
	ctx context.Context,
	r *http.Request,
	payload map[string]any,
	bodyBytes []byte,
) context.Context {
	if !contextPackLearnedActivationEnabled() {
		return ctx
	}
	if r != nil {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	return contextWithContextPackLearnedAuthority(ctx, optionalContextPackLearnedRequestAuthority(s, r))
}

func contextPackLearnedActivationEnabled() bool {
	return envBool("GO_CONTEXT_PACK_LEARNED_ACTIVATION_ENABLED", true)
}

func contextPackLearnedActivationCanaryPercent() int {
	return clampInt(envInt("GO_CONTEXT_PACK_LEARNED_ACTIVATION_CANARY_PERCENT", 5), 1, 10)
}

type contextPackLearnedActivationAuthority struct {
	Authorized        bool
	WorkspaceID       string
	AssignmentSubject string
	PolicyID          string
	PolicyDigest      string
	Reason            string
}

type contextPackLearnedActivationInput struct {
	Enabled         bool
	Project         string
	TaskClass       string
	RetrievalIntent string
	TrafficClass    string
	CanaryPercent   int
	Now             time.Time
	Authority       contextPackLearnedActivationAuthority
	Impact          map[string]any
	Reputation      map[string]any
}

type contextPackLearnedActivationDecision struct {
	Armed                   bool
	Eligible                bool
	AssignedTreatment       bool
	Performed               bool
	Arm                     string
	Reason                  string
	CanaryPercent           int
	ExposureBucket          int
	RequestRef              string
	ProjectScopeRef         string
	TaskClassScopeRef       string
	RetrievalIntentScopeRef string
	WorkspaceRef            string
	PolicyRef               string
	ImpactProofRef          string
	ActuatorComparatorRef   string
	ReputationSnapshotRef   string
	ActivationReceiptID     string
	RankingVectorDigest     string
	AppliedCandidateCount   int
	CandidateMultipliers    map[string]float64
	evaluationPhase         contextPackLearnedActivationPhase
}

func contextPackLearnedScopeRef(kind, value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	return "sha256:" + sha256Hex("context-pack-learned-scope\x00"+strings.ToLower(strings.TrimSpace(kind))+"\x00"+value)
}

func contextPackLearnedCanonicalDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func contextPackLearnedActivationReceiptID(decision contextPackLearnedActivationDecision) string {
	receiptSeed := strings.Join([]string{
		contextPackLearnedActivationContractID, decision.RequestRef, decision.ProjectScopeRef,
		decision.TaskClassScopeRef, decision.RetrievalIntentScopeRef, decision.WorkspaceRef, decision.PolicyRef,
		decision.ImpactProofRef, decision.ActuatorComparatorRef, decision.ReputationSnapshotRef, strconv.Itoa(decision.CanaryPercent),
		strconv.Itoa(decision.ExposureBucket), decision.Arm,
	}, "\x00")
	return "cla_" + sha256Hex(receiptSeed)[:24]
}

func contextPackLearnedDigestRef(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 64 {
		value = "sha256:" + value
	}
	if !isSearchIntelligenceFullSHA256Ref(value) {
		return ""
	}
	return value
}

func contextPackLearnedProofGatesPass(impact map[string]any) bool {
	gates := anyMap(impact["proof_gates"])
	for _, name := range []string{
		"comparative_shadow", "receipt_ledger_durability", "train_holdout_minimums",
		"negative_retention", "independent_verifiers", "exact_denominators",
		"causal_interval", "outcome_regressions_absent", "outcome_identity_consistency",
	} {
		if !anyToBool(anyMap(gates[name])["pass"]) {
			return false
		}
	}
	return true
}

func contextPackLearnedEvidenceFresh(evidence map[string]any, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	check := func(field string, maxAge time.Duration) bool {
		observed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(evidence[field])))
		if err != nil || observed.After(now.Add(time.Minute)) {
			return false
		}
		return now.Sub(observed) <= maxAge
	}
	return check("comparator_evaluated_at", contextPackLearnedComparatorMaxAge) &&
		check("latest_candidate_outcome_at", contextPackLearnedOutcomeMaxAge)
}

func contextPackLearnedReputationMultipliers(snapshot map[string]any, project, taskClass, retrievalIntent string, now time.Time) (map[string]float64, string) {
	if anyToString(snapshot["schema_id"]) != evidenceReputationContractID {
		return nil, "reputation_snapshot_invalid"
	}
	scope := anyMap(snapshot["scope"])
	if !strings.EqualFold(strings.TrimSpace(anyToString(scope["project"])), project) ||
		!strings.EqualFold(strings.TrimSpace(anyToString(scope["task_class"])), taskClass) ||
		!strings.EqualFold(strings.TrimSpace(anyToString(scope["retrieval_intent"])), retrievalIntent) {
		return nil, "reputation_scope_mismatch"
	}
	generatedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(snapshot["generated_at"])))
	if err != nil || generatedAt.After(now.Add(time.Minute)) || now.Sub(generatedAt) > contextPackLearnedReputationMaxAge {
		return nil, "reputation_snapshot_stale"
	}
	multipliers := map[string]float64{}
	seen := map[string]struct{}{}
	for _, raw := range contextPackAnyList(snapshot["entries"]) {
		entry := anyMap(raw)
		if anyToString(entry["entity_type"]) != "candidate" || !anyToBool(entry["calibrated"]) ||
			anyToInt(entry["sample_count"], 0) < contextPackLearnedMinimumSamples ||
			anyToInt(entry["independent_issuer_count"], 0) < contextPackLearnedMinimumIssuers ||
			anyToString(entry["result_level_credit"]) != "selection_receipt_bound" {
			continue
		}
		lastObservedAt, observedErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(entry["last_observed_at"])))
		if observedErr != nil || lastObservedAt.After(now.Add(time.Minute)) || now.Sub(lastObservedAt) > contextPackLearnedOutcomeMaxAge {
			continue
		}
		candidateID := contextPackOpaqueCandidateRef(entry["entity_label"])
		if candidateID == "" {
			continue
		}
		if _, duplicate := seen[candidateID]; duplicate {
			return nil, "reputation_snapshot_conflict"
		}
		seen[candidateID] = struct{}{}
		influence := anyMap(entry["bounded_influence"])
		multiplier, multiplierOK := contextPolicyNumber(influence["proposed_multiplier"])
		minimum, minimumOK := contextPolicyNumber(influence["minimum"])
		maximum, maximumOK := contextPolicyNumber(influence["maximum"])
		if !multiplierOK || !minimumOK || !maximumOK || minimum != 0.85 || maximum != 1.15 ||
			multiplier < minimum || multiplier > maximum || anyToBool(influence["applied"]) || !anyToBool(influence["advisory_only"]) {
			continue
		}
		if math.Abs(multiplier-1) > 1e-9 {
			multipliers[candidateID] = multiplier
		}
	}
	if len(multipliers) == 0 {
		return nil, "no_calibrated_candidate_influence"
	}
	return multipliers, ""
}

// contextPackLearnedReputationVectorRef binds the exact opaque candidate
// multiplier vector validated by the offline actuator comparator. Generated-at
// metadata is intentionally excluded so an unchanged vector remains stable.
func contextPackLearnedReputationVectorRef(multipliers map[string]float64) string {
	rows := make([]map[string]any, 0, len(multipliers))
	for candidateRef, multiplier := range multipliers {
		candidateRef = contextPackOpaqueCandidateRef(candidateRef)
		if candidateRef == "" || multiplier < 0.85 || multiplier > 1.15 || math.Abs(multiplier-1) <= 1e-9 {
			return ""
		}
		rows = append(rows, map[string]any{
			"candidate_ref": candidateRef,
			"multiplier":    roundFloat(multiplier, 6),
		})
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool {
		return anyToString(rows[i]["candidate_ref"]) < anyToString(rows[j]["candidate_ref"])
	})
	return contextPackLearnedCanonicalDigest(map[string]any{
		"schema_id": "context_pack_learned_reputation_vector.v1",
		"entries":   rows,
	})
}

func decideContextPackLearnedActivation(input contextPackLearnedActivationInput) contextPackLearnedActivationDecision {
	decision := contextPackLearnedActivationDecision{
		Arm: "shadow", Reason: "kill_switch_disabled", ExposureBucket: -1,
		CandidateMultipliers: map[string]float64{}, evaluationPhase: contextPackLearnedActivationFinal,
	}
	if !input.Enabled {
		return decision
	}
	decision.Armed = input.Authority.Authorized
	if !input.Authority.Authorized {
		decision.Reason = strings.TrimSpace(input.Authority.Reason)
		if decision.Reason == "" {
			decision.Reason = "activation_authority_unavailable"
		}
		return decision
	}
	now := input.Now.UTC()
	if input.Now.IsZero() {
		now = time.Now().UTC()
	}
	project := strings.ToLower(strings.TrimSpace(input.Project))
	taskClass := strings.ToLower(strings.TrimSpace(input.TaskClass))
	retrievalIntent := strings.ToLower(strings.TrimSpace(input.RetrievalIntent))
	if project == "" || taskClass == "" || retrievalIntent == "" {
		decision.Reason = "exact_scope_required"
		return decision
	}
	decision.ProjectScopeRef = contextPackLearnedScopeRef("project", project)
	decision.TaskClassScopeRef = contextPackLearnedScopeRef("task_class", taskClass)
	decision.RetrievalIntentScopeRef = contextPackLearnedScopeRef("retrieval_intent", retrievalIntent)
	decision.WorkspaceRef = contextPackLearnedScopeRef("workspace", input.Authority.WorkspaceID)
	decision.PolicyRef = contextPackLearnedDigestRef(input.Authority.PolicyDigest)
	policyAssignmentRef := contextPackLearnedScopeRef("policy", input.Authority.PolicyID)
	if decision.WorkspaceRef == "" || policyAssignmentRef == "" || decision.PolicyRef == "" {
		decision.Reason = "activation_authority_invalid"
		return decision
	}
	if strings.EqualFold(strings.TrimSpace(input.TrafficClass), "synthetic") {
		decision.Reason = "synthetic_traffic_control"
		return decision
	}
	assignmentSubject := strings.TrimSpace(input.Authority.AssignmentSubject)
	if assignmentSubject == "" {
		decision.Reason = "stable_assignment_subject_required"
		return decision
	}
	decision.RequestRef = contextPackLearnedScopeRef("assignment_subject", assignmentSubject)
	if len(input.Impact) == 0 {
		decision.Reason = "impact_canary_ineligible"
		decision.evaluationPhase = contextPackLearnedActivationNeedsImpact
		return decision
	}
	if anyToString(input.Impact["schema_id"]) != searchImpactIntelligenceContractID || !anyToBool(input.Impact["canary_eligible"]) {
		decision.Reason = "impact_canary_ineligible"
		return decision
	}
	evidence := anyMap(input.Impact["activation_evidence"])
	if len(evidence) == 0 {
		decision.Reason = "actuator_comparator_unavailable"
		return decision
	}
	if anyToString(evidence["project_scope_ref"]) != decision.ProjectScopeRef ||
		anyToString(evidence["task_class_scope_ref"]) != decision.TaskClassScopeRef ||
		anyToString(evidence["retrieval_intent_scope_ref"]) != decision.RetrievalIntentScopeRef {
		decision.Reason = "impact_scope_mismatch"
		return decision
	}
	if anyToString(evidence["workspace_ref"]) != decision.WorkspaceRef {
		decision.Reason = "impact_workspace_mismatch"
		return decision
	}
	decision.ImpactProofRef = contextPackLearnedDigestRef(anyToString(evidence["proof_digest"]))
	decision.ActuatorComparatorRef = contextPackLearnedDigestRef(anyToString(evidence["actuator_comparator_ref"]))
	evaluatedReputationVectorRef := contextPackLearnedDigestRef(anyToString(evidence["reputation_vector_ref"]))
	if decision.ActuatorComparatorRef == "" || evaluatedReputationVectorRef == "" {
		decision.Reason = "actuator_comparator_unavailable"
		return decision
	}
	if decision.ImpactProofRef == "" || !contextPackLearnedEvidenceFresh(evidence, now) {
		decision.Reason = "activation_evidence_stale"
		return decision
	}
	if !contextPackLearnedProofGatesPass(input.Impact) {
		decision.Reason = "impact_proof_gates_failed"
		return decision
	}
	if len(input.Reputation) == 0 {
		decision.Reason = "reputation_snapshot_invalid"
		decision.evaluationPhase = contextPackLearnedActivationNeedsReputation
		return decision
	}
	if anyToString(anyMap(input.Reputation["scope"])["workspace_ref"]) != decision.WorkspaceRef {
		decision.Reason = "reputation_workspace_mismatch"
		return decision
	}
	multipliers, reason := contextPackLearnedReputationMultipliers(input.Reputation, project, taskClass, retrievalIntent, now)
	if reason != "" {
		decision.Reason = reason
		return decision
	}
	if currentVectorRef := contextPackLearnedReputationVectorRef(multipliers); currentVectorRef == "" || currentVectorRef != evaluatedReputationVectorRef {
		decision.Reason = "reputation_vector_not_evaluated"
		return decision
	}
	decision.ReputationSnapshotRef = contextPackLearnedCanonicalDigest(input.Reputation)
	if decision.ReputationSnapshotRef == "" {
		decision.Reason = "reputation_snapshot_invalid"
		return decision
	}
	decision.CandidateMultipliers = multipliers
	decision.CanaryPercent = clampInt(input.CanaryPercent, 1, 10)
	bucketSeed := strings.Join([]string{
		decision.RequestRef, decision.ProjectScopeRef, decision.TaskClassScopeRef,
		decision.RetrievalIntentScopeRef, policyAssignmentRef,
	}, "\x00")
	bucketRaw, err := strconv.ParseUint(sha256Hex(bucketSeed)[:8], 16, 32)
	if err != nil {
		decision.Reason = "assignment_unavailable"
		return decision
	}
	decision.ExposureBucket = int(bucketRaw % 10000)
	decision.Eligible = true
	decision.Arm = "control"
	decision.Reason = "deterministic_control"
	if decision.ExposureBucket < decision.CanaryPercent*100 {
		decision.AssignedTreatment = true
		decision.Arm = "canary"
		decision.Reason = "canary_assigned"
	}
	decision.ActivationReceiptID = contextPackLearnedActivationReceiptID(decision)
	return decision
}

func (s *server) contextPackLearnedActivationDecision(
	ctx context.Context,
	payload map[string]any,
	project, taskClass, retrievalIntent, trafficClass string,
	concurrentPolicyIntervention bool,
) contextPackLearnedActivationDecision {
	enabled := contextPackLearnedActivationEnabled()
	callerDisabled := false
	if value, present := payload["rerank_with_learning"]; present && !anyToBool(value) {
		enabled = false
		callerDisabled = true
	}
	authority := contextPackLearnedAuthorityFromContext(ctx)
	if enabled && authority.Authorized {
		authority = optionalContextPackLearnedPolicyAuthority(s, authority, project, taskClass, retrievalIntent)
	}
	now := time.Now().UTC()
	input := contextPackLearnedActivationInput{
		Enabled: enabled, Project: project, TaskClass: taskClass, RetrievalIntent: retrievalIntent,
		TrafficClass: trafficClass, CanaryPercent: contextPackLearnedActivationCanaryPercent(), Now: now, Authority: authority,
	}
	decision := decideContextPackLearnedActivation(input)
	if callerDisabled {
		decision.Reason = "caller_disabled"
		return decision
	}
	// A nil impact snapshot deliberately marks the boundary after every cheap
	// exact-scope, traffic, identity, and authority gate. Only requests that
	// reach that boundary may scan durable outcome evidence.
	if decision.evaluationPhase != contextPackLearnedActivationNeedsImpact || s == nil {
		return decision
	}
	if concurrentPolicyIntervention {
		return contextPackLearnedForceControl(decision, "concurrent_policy_intervention")
	}
	rows := []map[string]any{}
	receiptBinding := map[string]any{"pass": false, "missing_receipt_outcome_count": 0}
	if s.contextPackQuality != nil {
		rows, receiptBinding = s.contextPackQuality.receiptDurableOutcomeRows(searchImpactOutcomeSourceLimit)
	}
	rows = reconcileCandidateUtilityVerification(rows, utilityFromServer(s))
	input.Impact = s.searchImpactIntelligenceSnapshotFromReconciledRowsForWorkspace(
		project, taskClass, retrievalIntent, decision.WorkspaceRef, rows, receiptBinding,
	)
	decision = decideContextPackLearnedActivation(input)
	if decision.evaluationPhase != contextPackLearnedActivationNeedsReputation {
		return decision
	}
	input.Reputation = evidenceReputationSnapshotFromReconciledRowsForWorkspace(
		rows, project, taskClass, retrievalIntent, decision.WorkspaceRef,
		contextPackLearnedMinimumSamples, evidenceReputationMaxEntries, now,
	)
	return decideContextPackLearnedActivation(input)
}

func contextPackLearnedForceControl(decision contextPackLearnedActivationDecision, reason string) contextPackLearnedActivationDecision {
	decision.Eligible = false
	decision.AssignedTreatment = false
	decision.Performed = false
	decision.Arm = "shadow"
	decision.Reason = reason
	decision.ActivationReceiptID = ""
	decision.RankingVectorDigest = ""
	decision.AppliedCandidateCount = 0
	decision.CandidateMultipliers = map[string]float64{}
	decision.evaluationPhase = contextPackLearnedActivationFinal
	return decision
}

func contextPackLearnedProtectedEvidence(item contextPackEvidenceItem) bool {
	if item.Kind == "decision" || item.Kind == "risk" || item.Kind == "check" {
		return true
	}
	return containsAnyInList(item.WhySelected, "risk_or_contradiction_signal")
}

func applyContextPackLearnedRanking(items []contextPackEvidenceItem, decision contextPackLearnedActivationDecision) ([]contextPackEvidenceItem, contextPackLearnedActivationDecision) {
	if !decision.Eligible || !decision.AssignedTreatment {
		return items, decision
	}
	ranked := append([]contextPackEvidenceItem(nil), items...)
	vector := make([]map[string]any, 0, len(ranked))
	reorderableIndexes := make([]int, 0, len(ranked))
	for index := range ranked {
		item := &ranked[index]
		if !contextPackLearnedProtectedEvidence(*item) {
			reorderableIndexes = append(reorderableIndexes, index)
		}
		multiplier, exists := decision.CandidateMultipliers[item.CandidateID]
		if !exists || contextPackLearnedProtectedEvidence(*item) {
			continue
		}
		if decision.AppliedCandidateCount >= contextPackSelectionReceiptLimit {
			return items, contextPackLearnedForceControl(decision, "candidate_receipt_capacity_exceeded")
		}
		baseScore := item.Score
		baseImpact := item.ImpactScore
		item.LearnedBaseScore = roundFloat(baseScore, 6)
		item.LearnedMultiplier = roundFloat(multiplier, 6)
		item.Score = roundFloat(baseScore*multiplier, 6)
		item.ImpactScore = roundFloat(baseImpact*multiplier, 6)
		item.ValueDensity = roundFloat(item.Score/float64(maxInt(item.EstimatedTokens, 1)), 6)
		item.LearnedInfluenceApplied = true
		decision.AppliedCandidateCount++
		vector = append(vector, map[string]any{
			"candidate_ref": item.CandidateID, "occurrence": item.Occurrence,
			"base_score": item.LearnedBaseScore,
			"multiplier": item.LearnedMultiplier, "final_score": item.Score,
		})
	}
	if decision.AppliedCandidateCount == 0 {
		return ranked, contextPackLearnedForceControl(decision, "no_returned_candidate_influence")
	}
	sort.Slice(vector, func(i, j int) bool {
		leftRef := anyToString(vector[i]["candidate_ref"])
		rightRef := anyToString(vector[j]["candidate_ref"])
		if leftRef == rightRef {
			return anyToInt(vector[i]["occurrence"], 0) < anyToInt(vector[j]["occurrence"], 0)
		}
		return leftRef < rightRef
	})
	decision.RankingVectorDigest = contextPackLearnedCanonicalDigest(vector)
	if decision.RankingVectorDigest == "" {
		return append([]contextPackEvidenceItem(nil), items...), contextPackLearnedForceControl(decision, "ranking_vector_unavailable")
	}
	reorderable := make([]contextPackEvidenceItem, 0, len(reorderableIndexes))
	for _, index := range reorderableIndexes {
		reorderable = append(reorderable, ranked[index])
	}
	sort.SliceStable(reorderable, func(i, j int) bool {
		if reorderable[i].Score != reorderable[j].Score {
			return reorderable[i].Score > reorderable[j].Score
		}
		if reorderable[i].Kind != reorderable[j].Kind {
			return reorderable[i].Kind < reorderable[j].Kind
		}
		if reorderable[i].CandidateID != reorderable[j].CandidateID {
			return reorderable[i].CandidateID < reorderable[j].CandidateID
		}
		return reorderable[i].Occurrence < reorderable[j].Occurrence
	})
	for position, index := range reorderableIndexes {
		ranked[index] = reorderable[position]
	}
	decision.Performed = true
	decision.Reason = "bounded_candidate_reputation_applied"
	return ranked, decision
}

func contextPackLearnedActivationReceipt(decision contextPackLearnedActivationDecision) map[string]any {
	return map[string]any{
		"schema_id":                    contextPackLearnedActivationContractID,
		"version":                      1,
		"activation_receipt_id":        decision.ActivationReceiptID,
		"armed":                        decision.Armed,
		"eligible":                     decision.Eligible,
		"arm":                          decision.Arm,
		"performed":                    decision.Performed,
		"reason":                       decision.Reason,
		"canary_percent":               decision.CanaryPercent,
		"exposure_bucket_basis_points": decision.ExposureBucket,
		"request_ref":                  decision.RequestRef,
		"project_scope_ref":            decision.ProjectScopeRef,
		"task_class_scope_ref":         decision.TaskClassScopeRef,
		"retrieval_intent_scope_ref":   decision.RetrievalIntentScopeRef,
		"workspace_ref":                decision.WorkspaceRef,
		"policy_ref":                   decision.PolicyRef,
		"impact_proof_ref":             decision.ImpactProofRef,
		"actuator_comparator_ref":      decision.ActuatorComparatorRef,
		"reputation_snapshot_ref":      decision.ReputationSnapshotRef,
		"ranking_vector_digest":        decision.RankingVectorDigest,
		"applied_candidate_count":      decision.AppliedCandidateCount,
		"raw_query_or_content_stored":  false,
	}
}
