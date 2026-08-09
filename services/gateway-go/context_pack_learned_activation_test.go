package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func learnedActivationEligibleImpact(now time.Time, project, taskClass, retrievalIntent string) map[string]any {
	pass := func() map[string]any { return map[string]any{"pass": true} }
	workspaceRef := contextPackLearnedScopeRef("workspace", "workspace-test")
	return map[string]any{
		"schema_id":       searchImpactIntelligenceContractID,
		"canary_eligible": true,
		"proof_gates": map[string]any{
			"comparative_shadow":           pass(),
			"receipt_ledger_durability":    pass(),
			"train_holdout_minimums":       pass(),
			"negative_retention":           pass(),
			"independent_verifiers":        pass(),
			"exact_denominators":           pass(),
			"causal_interval":              pass(),
			"outcome_regressions_absent":   pass(),
			"outcome_identity_consistency": pass(),
		},
		"activation_evidence": map[string]any{
			"project_scope_ref":           contextPackLearnedScopeRef("project", project),
			"task_class_scope_ref":        contextPackLearnedScopeRef("task_class", taskClass),
			"retrieval_intent_scope_ref":  contextPackLearnedScopeRef("retrieval_intent", retrievalIntent),
			"workspace_ref":               workspaceRef,
			"proof_digest":                "sha256:" + strings.Repeat("a", 64),
			"actuator_comparator_ref":     "sha256:" + strings.Repeat("d", 64),
			"comparator_evaluated_at":     now.Add(-time.Hour).Format(time.RFC3339Nano),
			"latest_candidate_outcome_at": now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		},
	}
}

func learnedActivationReputation(now time.Time, project, taskClass, retrievalIntent string, entries ...map[string]any) map[string]any {
	rows := make([]any, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, entry)
	}
	return map[string]any{
		"schema_id":    evidenceReputationContractID,
		"generated_at": now.Add(-time.Minute).Format(time.RFC3339Nano),
		"scope": map[string]any{
			"project": project, "task_class": taskClass, "retrieval_intent": retrievalIntent,
			"workspace_ref": contextPackLearnedScopeRef("workspace", "workspace-test"),
		},
		"entries": rows,
	}
}

func learnedActivationCandidate(now time.Time, candidateID string, multiplier float64) map[string]any {
	return map[string]any{
		"entity_type": "candidate", "entity_label": candidateID,
		"calibrated": true, "sample_count": 5, "independent_issuer_count": 2,
		"last_observed_at":    now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		"result_level_credit": "selection_receipt_bound",
		"bounded_influence": map[string]any{
			"proposed_multiplier": multiplier, "minimum": 0.85, "maximum": 1.15,
			"applied": false, "advisory_only": true,
		},
	}
}

func learnedActivationInput(now time.Time, identity string) contextPackLearnedActivationInput {
	project, taskClass, retrievalIntent := "contextlattice", "agent_workflow", "decision"
	input := contextPackLearnedActivationInput{
		Enabled: true, Project: project, TaskClass: taskClass, RetrievalIntent: retrievalIntent,
		TrafficClass: "user", CanaryPercent: 5, Now: now,
		Authority: contextPackLearnedActivationAuthority{
			Authorized: true, WorkspaceID: "workspace-test", PolicyID: "policy-test",
			PolicyDigest: strings.Repeat("b", 64), AssignmentSubject: identity,
		},
		Impact: learnedActivationEligibleImpact(now, project, taskClass, retrievalIntent),
		Reputation: learnedActivationReputation(now, project, taskClass, retrievalIntent,
			learnedActivationCandidate(now, "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", 1.15)),
	}
	learnedActivationBindEvaluatedVector(&input)
	return input
}

func learnedActivationBindEvaluatedVector(input *contextPackLearnedActivationInput) {
	if input == nil {
		return
	}
	multipliers, reason := contextPackLearnedReputationMultipliers(
		input.Reputation, input.Project, input.TaskClass, input.RetrievalIntent, input.Now,
	)
	if reason != "" {
		return
	}
	anyMap(input.Impact["activation_evidence"])["reputation_vector_ref"] = contextPackLearnedReputationVectorRef(multipliers)
}

func treatmentIdentityForTest(t *testing.T, now time.Time) string {
	t.Helper()
	for index := 0; index < 10000; index++ {
		identity := fmt.Sprintf("session-%d", index)
		decision := decideContextPackLearnedActivation(learnedActivationInput(now, identity))
		if decision.AssignedTreatment {
			return identity
		}
	}
	t.Fatal("could not find a deterministic treatment identity")
	return ""
}

func controlIdentityForTest(t *testing.T, now time.Time) string {
	t.Helper()
	for index := 0; index < 10000; index++ {
		identity := fmt.Sprintf("control-session-%d", index)
		decision := decideContextPackLearnedActivation(learnedActivationInput(now, identity))
		if decision.Eligible && !decision.AssignedTreatment && decision.Arm == "control" {
			return identity
		}
	}
	t.Fatal("could not find a deterministic control identity")
	return ""
}

func TestContextPackLearnedActivationIsDeterministicAndExactScope(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	identity := treatmentIdentityForTest(t, now)
	input := learnedActivationInput(now, identity)
	first := decideContextPackLearnedActivation(input)
	second := decideContextPackLearnedActivation(input)
	if !first.Armed || !first.Eligible || !first.AssignedTreatment || first.Arm != "canary" {
		t.Fatalf("eligible exact-scope request was not assigned to treatment: %#v", first)
	}
	if first.ExposureBucket != second.ExposureBucket || first.RequestRef != second.RequestRef || first.ActivationReceiptID != second.ActivationReceiptID {
		t.Fatalf("assignment was not deterministic: first=%#v second=%#v", first, second)
	}
	widerCanary := input
	widerCanary.CanaryPercent = 10
	wider := decideContextPackLearnedActivation(widerCanary)
	if wider.ActivationReceiptID == first.ActivationReceiptID {
		t.Fatalf("activation receipt identity did not bind canary configuration: first=%#v wider=%#v", first, wider)
	}
	policyRevision := input
	policyRevision.Authority.PolicyDigest = strings.Repeat("c", 64)
	revised := decideContextPackLearnedActivation(policyRevision)
	if revised.ExposureBucket != first.ExposureBucket || revised.Arm != first.Arm || revised.ActivationReceiptID == first.ActivationReceiptID {
		t.Fatalf("policy revision did not preserve assignment while changing receipt identity: first=%#v revised=%#v", first, revised)
	}

	wrongProject := input
	wrongProject.Project = "other"
	if got := decideContextPackLearnedActivation(wrongProject); got.Eligible || got.AssignedTreatment || got.Reason != "impact_scope_mismatch" {
		t.Fatalf("project scope mismatch did not fail closed: %#v", got)
	}
	missingTask := input
	missingTask.TaskClass = ""
	if got := decideContextPackLearnedActivation(missingTask); got.Eligible || got.Reason != "exact_scope_required" {
		t.Fatalf("missing task scope did not fail closed: %#v", got)
	}
}

func TestContextPackLearnedActivationGatesAndKillSwitchFailClosed(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	identity := treatmentIdentityForTest(t, now)
	tests := []struct {
		name   string
		mutate func(*contextPackLearnedActivationInput)
		reason string
	}{
		{name: "kill switch", mutate: func(input *contextPackLearnedActivationInput) { input.Enabled = false }, reason: "kill_switch_disabled"},
		{name: "synthetic", mutate: func(input *contextPackLearnedActivationInput) { input.TrafficClass = "synthetic" }, reason: "synthetic_traffic_control"},
		{name: "authority", mutate: func(input *contextPackLearnedActivationInput) { input.Authority.Authorized = false }, reason: "activation_authority_unavailable"},
		{name: "missing identity", mutate: func(input *contextPackLearnedActivationInput) { input.Authority.AssignmentSubject = "" }, reason: "stable_assignment_subject_required"},
		{name: "failed proof gate", mutate: func(input *contextPackLearnedActivationInput) {
			anyMap(input.Impact["proof_gates"])["causal_interval"] = map[string]any{"pass": false}
		}, reason: "impact_proof_gates_failed"},
		{name: "stale comparator", mutate: func(input *contextPackLearnedActivationInput) {
			anyMap(input.Impact["activation_evidence"])["comparator_evaluated_at"] = now.Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
		}, reason: "activation_evidence_stale"},
		{name: "no calibrated influence", mutate: func(input *contextPackLearnedActivationInput) {
			input.Reputation["entries"] = []any{learnedActivationCandidate(now, "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", 1.0)}
		}, reason: "no_calibrated_candidate_influence"},
		{name: "stale candidate influence", mutate: func(input *contextPackLearnedActivationInput) {
			candidate := learnedActivationCandidate(now, "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", 1.15)
			candidate["last_observed_at"] = now.Add(-31 * 24 * time.Hour).Format(time.RFC3339Nano)
			input.Reputation["entries"] = []any{candidate}
		}, reason: "no_calibrated_candidate_influence"},
		{name: "unevaluated reputation vector", mutate: func(input *contextPackLearnedActivationInput) {
			input.Reputation["entries"] = []any{learnedActivationCandidate(now, "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", 0.85)}
		}, reason: "reputation_vector_not_evaluated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := learnedActivationInput(now, identity)
			test.mutate(&input)
			decision := decideContextPackLearnedActivation(input)
			if decision.Eligible || decision.AssignedTreatment || decision.Performed || decision.Reason != test.reason {
				t.Fatalf("gate did not fail closed: %#v", decision)
			}
		})
	}
}

func TestContextPackLearnedActivationUsesExplicitStagedEvaluationPhases(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	input := learnedActivationInput(now, "server-subject")
	input.Impact = nil
	needsImpact := decideContextPackLearnedActivation(input)
	if needsImpact.evaluationPhase != contextPackLearnedActivationNeedsImpact || needsImpact.Reason != "impact_canary_ineligible" {
		t.Fatalf("missing impact did not enter the explicit impact phase: %#v", needsImpact)
	}

	input = learnedActivationInput(now, "server-subject")
	input.Reputation = nil
	needsReputation := decideContextPackLearnedActivation(input)
	if needsReputation.evaluationPhase != contextPackLearnedActivationNeedsReputation || needsReputation.Reason != "reputation_snapshot_invalid" {
		t.Fatalf("missing reputation did not enter the explicit reputation phase: %#v", needsReputation)
	}
}

func TestContextPackLearnedActivationCanaryConfigurationIsBounded(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_LEARNED_ACTIVATION_CANARY_PERCENT", "0")
	if got := contextPackLearnedActivationCanaryPercent(); got != 1 {
		t.Fatalf("zero canary configuration = %d, want 1", got)
	}
	t.Setenv("GO_CONTEXT_PACK_LEARNED_ACTIVATION_CANARY_PERCENT", "99")
	if got := contextPackLearnedActivationCanaryPercent(); got != 10 {
		t.Fatalf("oversized canary configuration = %d, want 10", got)
	}
	t.Setenv("GO_CONTEXT_PACK_LEARNED_ACTIVATION_CANARY_PERCENT", "7")
	if got := contextPackLearnedActivationCanaryPercent(); got != 7 {
		t.Fatalf("valid canary configuration = %d, want 7", got)
	}
}

func TestContextPackLearnedRankingAppliesBoundedCandidateInfluenceWithoutWeakeningProtectedEvidence(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	input := learnedActivationInput(now, treatmentIdentityForTest(t, now))
	input.Reputation = learnedActivationReputation(now, input.Project, input.TaskClass, input.RetrievalIntent,
		learnedActivationCandidate(now, "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", 1.15),
		learnedActivationCandidate(now, "rtc_bbbbbbbbbbbbbbbbbbbbbbbb", 0.85),
		learnedActivationCandidate(now, "rtc_cccccccccccccccccccccccc", 0.85),
	)
	learnedActivationBindEvaluatedVector(&input)
	decision := decideContextPackLearnedActivation(input)
	items := []contextPackEvidenceItem{
		{CandidateID: "rtc_bbbbbbbbbbbbbbbbbbbbbbbb", Kind: "memory", Score: 100, ImpactScore: 100, EstimatedTokens: 10, ValueDensity: 10},
		{CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "memory", Score: 90, ImpactScore: 90, EstimatedTokens: 10, ValueDensity: 9},
		{CandidateID: "rtc_cccccccccccccccccccccccc", Kind: "risk", Score: 80, ImpactScore: 80, EstimatedTokens: 10, ValueDensity: 8, WhySelected: []any{"risk_or_contradiction_signal"}},
	}
	before := append([]contextPackEvidenceItem(nil), items...)
	ranked, applied := applyContextPackLearnedRanking(items, decision)
	if !applied.Performed || applied.AppliedCandidateCount != 2 {
		t.Fatalf("bounded learned influence was not applied: %#v", applied)
	}
	if ranked[0].CandidateID != "rtc_aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("calibrated candidate did not outrank the native leader: %#v", ranked)
	}
	for _, item := range ranked {
		if item.CandidateID == "rtc_cccccccccccccccccccccccc" && (item.Score != 80 || item.LearnedInfluenceApplied) {
			t.Fatalf("protected risk evidence was weakened by learned influence: %#v", item)
		}
	}
	if !reflect.DeepEqual(items, before) {
		t.Fatalf("ranking mutated caller-owned input: before=%#v after=%#v", before, items)
	}
	if applied.RankingVectorDigest == "" || !isSearchIntelligenceFullSHA256Ref(applied.RankingVectorDigest) {
		t.Fatalf("ranking vector digest missing: %#v", applied)
	}
	_, abstained := applyContextPackLearnedRanking([]contextPackEvidenceItem{{
		CandidateID: "rtc_cccccccccccccccccccccccc", Kind: "risk", Score: 80, ImpactScore: 80,
		EstimatedTokens: 10, ValueDensity: 8, WhySelected: []any{"risk_or_contradiction_signal"},
	}}, decision)
	if abstained.Eligible || abstained.AssignedTreatment || abstained.Performed || abstained.Reason != "no_returned_candidate_influence" {
		t.Fatalf("canary without an influenceable returned candidate did not abstain: %#v", abstained)
	}
}

func TestContextPackLearnedRankingPreservesProtectedNoBudgetSelectionBeyondCandidateLimit(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	items := make([]contextPackEvidenceItem, 0, 18)
	for index := 0; index < 18; index++ {
		candidateID := fmt.Sprintf("rtc_%024d", index)
		item := contextPackEvidenceItem{
			CandidateID: candidateID, Kind: "memory", Score: float64(100 - index),
			ImpactScore: float64(100 - index), EstimatedTokens: 10, ValueDensity: float64(100-index) / 10,
		}
		if index == 14 {
			item.Kind = "decision"
		}
		items = append(items, item)
	}
	decision.CandidateMultipliers = map[string]float64{items[17].CandidateID: 1.15}
	ranked, applied := applyContextPackLearnedRanking(items, decision)
	if !applied.Performed || ranked[14].CandidateID != items[14].CandidateID {
		t.Fatalf("learned ranking moved protected evidence out of its native slot: decision=%#v ranked=%#v", applied, ranked)
	}
	nativeSelected, _, _, _ := allocateContextPackEvidence(items, contextPackTokenBudget{})
	treatmentSelected, _, _, _ := allocateContextPackEvidence(ranked, contextPackTokenBudget{})
	if !contextPackLearnedProtectedSelectionPreserved(nativeSelected, treatmentSelected) {
		t.Fatalf("learned no-budget allocation evicted or demoted protected evidence: native=%#v treatment=%#v", nativeSelected, treatmentSelected)
	}
}

func TestContextPackLearnedProtectedSelectionUsesCandidateOccurrenceIdentity(t *testing.T) {
	native := []contextPackEvidenceItem{
		{CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Occurrence: 1, Kind: "decision"},
		{CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Occurrence: 2, Kind: "decision"},
	}
	treatment := []contextPackEvidenceItem{
		native[1],
		{CandidateID: "rtc_bbbbbbbbbbbbbbbbbbbbbbbb", Occurrence: 1, Kind: "memory"},
	}
	if contextPackLearnedProtectedSelectionPreserved(native, treatment) {
		t.Fatal("a duplicate candidate reference masked a missing protected occurrence")
	}
}

func TestContextPackLearnedRankingFailsClosedBeyondReceiptCapacity(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	items := make([]contextPackEvidenceItem, 0, contextPackSelectionReceiptLimit+1)
	decision.CandidateMultipliers = make(map[string]float64, contextPackSelectionReceiptLimit+1)
	for index := 0; index <= contextPackSelectionReceiptLimit; index++ {
		candidateID := fmt.Sprintf("rtc_%024d", index)
		items = append(items, contextPackEvidenceItem{
			CandidateID: candidateID, Kind: "memory", Score: float64(100 - index),
			ImpactScore: float64(100 - index), EstimatedTokens: 10, ValueDensity: float64(100-index) / 10,
		})
		decision.CandidateMultipliers[candidateID] = 1.15
	}
	ranked, result := applyContextPackLearnedRanking(items, decision)
	if result.Eligible || result.AssignedTreatment || result.Performed || result.Reason != "candidate_receipt_capacity_exceeded" {
		t.Fatalf("oversized learned treatment did not fail closed: %#v", result)
	}
	if !reflect.DeepEqual(ranked, items) {
		t.Fatalf("oversized learned treatment returned modified ranking: ranked=%#v native=%#v", ranked, items)
	}
}

func TestContextPackLearnedActivationReceiptIsOpaque(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	receipt := contextPackLearnedActivationReceipt(decision)
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal activation receipt: %v", err)
	}
	encoded := string(raw)
	for _, forbidden := range []string{"contextlattice", "agent_workflow", "workspace-test", "policy-test", "session-"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("activation receipt leaked raw scope or identity %q: %#v", forbidden, receipt)
		}
	}
	if anyToString(receipt["activation_receipt_id"]) == "" || anyToString(receipt["request_ref"]) == "" {
		t.Fatalf("activation receipt omitted durable opaque identity: %#v", receipt)
	}
}

func TestContextPackLearnedActivationReceiptRejectsTamperedIdentity(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	receipt := contextPackLearnedActivationReceipt(decision)
	if normalized := contextPackLearnedActivationReceiptFromSample(receipt); len(normalized) == 0 {
		t.Fatalf("canonical activation receipt was rejected: %#v", receipt)
	}

	tamperedID := cloneJSONMap(receipt)
	tamperedID["activation_receipt_id"] = "cla_" + strings.Repeat("f", 24)
	if normalized := contextPackLearnedActivationReceiptFromSample(tamperedID); len(normalized) != 0 {
		t.Fatalf("tampered activation receipt identity was accepted: %#v", normalized)
	}

	tamperedWorkspace := cloneJSONMap(receipt)
	tamperedWorkspace["workspace_ref"] = contextPackLearnedScopeRef("workspace", "workspace-other")
	if normalized := contextPackLearnedActivationReceiptFromSample(tamperedWorkspace); len(normalized) != 0 {
		t.Fatalf("workspace substitution preserved activation receipt identity: %#v", normalized)
	}
}

func TestContextPackLearnedSelectionReceiptV2CarriesOneOutcomeBindingWithoutRawDuplication(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	_, decision = applyContextPackLearnedRanking([]contextPackEvidenceItem{{
		CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "memory", Score: 90, ImpactScore: 90,
		EstimatedTokens: 10, ValueDensity: 9, Occurrence: 1,
	}}, decision)
	activation := contextPackLearnedActivationReceipt(decision)
	ranked := []any{map[string]any{
		"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1, "occurrence": 1,
		"learned_base_score": 90.0, "learned_multiplier": 1.15, "score": 103.5,
		"learned_influence_applied": true,
		"text":                      "never persist this raw result", "file": "/private/never/persist.md",
	}}
	receipt := contextPackSelectionReceiptWithActivation(ranked, nil, activation)
	if anyToString(receipt["schema_id"]) != contextPackSelectionReceiptV2SchemaID || anyToInt(receipt["version"], 0) != 2 {
		t.Fatalf("learned selection receipt did not use v2: %#v", receipt)
	}
	normalized := contextPackSelectionReceiptFromSample(receipt)
	if !reflect.DeepEqual(receipt, normalized) {
		t.Fatalf("learned selection receipt was not canonically replayable:\nreceipt=%#v\nnormalized=%#v", receipt, normalized)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"never persist this raw result", "/private/never/persist.md", `"project":"contextlattice"`, "workspace-test"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("selection receipt duplicated raw content or scope %q: %s", forbidden, raw)
		}
	}
	candidates := parseRows(receipt["candidates"])
	if len(candidates) != 1 || anyToFloat(candidates[0]["learned_multiplier"]) != 1.15 ||
		anyToString(anyMap(receipt["learned_activation"])["activation_receipt_id"]) == "" {
		t.Fatalf("selection receipt omitted learned actuation facts: %#v", receipt)
	}
}

func TestContextPackLearnedWorkspaceIdentitySurvivesReceiptOutcomeAndUtilityBinding(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	_, decision = applyContextPackLearnedRanking([]contextPackEvidenceItem{{
		CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "memory", Score: 90, ImpactScore: 90,
		EstimatedTokens: 10, ValueDensity: 9, Occurrence: 1,
	}}, decision)
	receipt := contextPackSelectionReceiptWithActivation([]any{map[string]any{
		"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1, "occurrence": 1,
		"learned_base_score": 90.0, "learned_multiplier": 1.15, "score": 103.5,
		"learned_influence_applied": true,
	}}, nil, contextPackLearnedActivationReceipt(decision))
	quality := contextPackQualityEntryFromSample(map[string]any{
		"sample_id": "workspace-bound-sample", "project": "contextlattice",
		"task_class": "agent_workflow", "retrieval_intent": "decision", "selection_receipt": receipt,
	})
	if anyToString(quality["workspace_ref"]) != decision.WorkspaceRef {
		t.Fatalf("quality sample lost canonical workspace identity: quality=%#v decision=%#v", quality, decision)
	}
	outcome, err := contextPackQualityOutcomeFromSampleChecked(map[string]any{
		"sample_id": "workspace-bound-sample", "project": "contextlattice", "retry_count": 1,
	})
	if err != nil || len(outcome) == 0 {
		t.Fatalf("normalize workspace-bound outcome: outcome=%#v err=%v", outcome, err)
	}
	tampered := cloneJSONMap(outcome)
	tampered["workspace_ref"] = contextPackLearnedScopeRef("workspace", "workspace-other")
	if _, err := bindContextPackQualityOutcomeSample(tampered, quality); !errors.Is(err, errContextPackOutcomeSampleConflict) {
		t.Fatalf("cross-workspace outcome rebind was not rejected: %v", err)
	}
	bound, err := bindContextPackQualityOutcomeSample(outcome, quality)
	if err != nil || anyToString(bound["workspace_ref"]) != decision.WorkspaceRef {
		t.Fatalf("bound outcome lost canonical workspace identity: bound=%#v err=%v", bound, err)
	}
	utility := buildUtilityObservation(bound, quality, map[string]any{
		"sample_id": "workspace-bound-sample", "project": "contextlattice",
		"tokenizer_exact": true, "wire_tokens_exact": 10, "model_visible_context_tokens_exact": 8,
	}, nil)
	if anyToString(utility["workspace_ref"]) != decision.WorkspaceRef {
		t.Fatalf("utility observation lost canonical workspace identity: %#v", utility)
	}
}

func TestContextPackLearnedSelectionReceiptRejectsInvalidActivationInsteadOfDowngrading(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	_, decision = applyContextPackLearnedRanking([]contextPackEvidenceItem{{
		CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "memory", Score: 90, ImpactScore: 90,
		EstimatedTokens: 10, ValueDensity: 9,
	}}, decision)
	activation := contextPackLearnedActivationReceipt(decision)
	delete(activation, "actuator_comparator_ref")
	receipt := contextPackSelectionReceiptWithActivation([]any{map[string]any{
		"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1,
		"learned_base_score": 90.0, "learned_multiplier": 1.15, "score": 103.5,
		"learned_influence_applied": true,
	}}, nil, activation)
	if receipt != nil {
		t.Fatalf("invalid learned activation silently downgraded or persisted: %#v", receipt)
	}
}

func TestContextPackLearnedNativeControlRetainsDurableV1BootstrapReceipt(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", t.TempDir()+"/quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	input := learnedActivationInput(now, "bootstrap-control")
	input.Authority.Authorized = false
	decision := decideContextPackLearnedActivation(input)
	if decision.Eligible || decision.Performed || decision.Reason == "" {
		t.Fatalf("fixture did not establish a native control decision: %#v", decision)
	}
	activation := contextPackLearnedActivationReceipt(decision)
	ranked := []any{map[string]any{
		"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1,
	}}
	receipt := contextPackSelectionReceiptWithActivation(ranked, nil, activation)
	if anyToString(receipt["schema_id"]) != contextPackSelectionReceiptSchemaID || anyToInt(receipt["version"], 0) != 1 || len(anyMap(receipt["learned_activation"])) != 0 {
		t.Fatalf("native control did not retain an ordinary V1 bootstrap receipt: %#v", receipt)
	}
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "bootstrap receipt", Project: "contextlattice", TaskClass: "agent_workflow", RetrievalIntent: "decision",
		TokenImpact: map[string]any{}, Compiled: map[string]any{}, SourceCoverage: map[string]any{}, GraphQuality: map[string]any{},
		RankedEvidence: ranked, LearnedActivation: activation,
	})
	telemetry := newContextPackQualityTelemetry(20)
	if err := telemetry.recordQualityDurably(sample); err != nil {
		t.Fatalf("native V1 bootstrap receipt was not durable: %v", err)
	}
	rows, _, err := telemetry.ledger.readRowsUnlocked()
	if err != nil || len(rows) != 1 || anyToString(anyMap(rows[0]["selection_receipt"])["schema_id"]) != contextPackSelectionReceiptSchemaID {
		t.Fatalf("native V1 bootstrap receipt missing from durable ledger: rows=%#v err=%v", rows, err)
	}
	malformed := cloneJSONMap(activation)
	malformed["unexpected"] = true
	if receipt := contextPackSelectionReceiptWithActivation(ranked, nil, malformed); receipt != nil {
		t.Fatalf("malformed native-control activation silently retained a receipt: %#v", receipt)
	}
}

func TestContextPackLearnedV2ReceiptBindsEveryAffectedOccurrence(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	items := []contextPackEvidenceItem{
		{CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Occurrence: 1, Kind: "memory", Score: 90, ImpactScore: 90, EstimatedTokens: 10, ValueDensity: 9},
		{CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Occurrence: 2, Kind: "memory", Score: 80, ImpactScore: 80, EstimatedTokens: 10, ValueDensity: 8},
	}
	_, decision = applyContextPackLearnedRanking(items, decision)
	if !decision.Performed || decision.AppliedCandidateCount != 2 {
		t.Fatalf("fixture did not apply both candidate occurrences: %#v", decision)
	}
	activation := contextPackLearnedActivationReceipt(decision)
	ranked := []any{
		map[string]any{"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1, "occurrence": 1, "learned_influence_applied": true, "learned_base_score": 90.0, "learned_multiplier": 1.15, "score": 103.5},
		map[string]any{"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 2, "occurrence": 2, "learned_influence_applied": true, "learned_base_score": 80.0, "learned_multiplier": 1.15, "score": 92.0},
	}
	receipt := contextPackSelectionReceiptWithActivation(ranked, nil, activation)
	candidates := parseRows(receipt["candidates"])
	if anyToString(receipt["schema_id"]) != contextPackSelectionReceiptV2SchemaID || len(candidates) != 2 ||
		anyToInt(candidates[0]["occurrence"], 0) != 1 || anyToInt(candidates[1]["occurrence"], 0) != 2 ||
		!reflect.DeepEqual(receipt, contextPackSelectionReceiptFromSample(receipt)) {
		t.Fatalf("V2 receipt did not retain an exact occurrence-aware actuation vector: %#v", receipt)
	}
	if incomplete := contextPackSelectionReceiptWithActivation(ranked[:1], nil, activation); incomplete != nil {
		t.Fatalf("V2 receipt accepted an incomplete applied-occurrence capture: %#v", incomplete)
	}
}

func TestContextPackLearnedCompilationBindsInternalOccurrenceAndExactScore(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"project": "contextlattice", "source": "qdrant", "file": "notes/precise-score.md",
		"topic_path": "search", "summary": "precise durable receipt score", "score": 0.987654,
	}
	assessment := memoryTrustAssessmentForCandidate("memory", "precise durable receipt score", row)
	candidateID := anyToString(assessment["candidate_id"])
	input := learnedActivationInput(now, treatmentIdentityForTest(t, now))
	input.Reputation = learnedActivationReputation(now, input.Project, input.TaskClass, input.RetrievalIntent,
		learnedActivationCandidate(now, candidateID, 1.15),
	)
	learnedActivationBindEvaluatedVector(&input)
	decision := decideContextPackLearnedActivation(input)
	artifacts := buildContextPackCompilationArtifacts(contextPackCompilationInput{
		Query: "precise durable receipt score", Project: input.Project, TaskClass: input.TaskClass,
		RetrievalMode: "balanced", RetrievalIntent: input.RetrievalIntent, SessionID: "precision-session", AgentID: "codex-test",
		ContextPack: map[string]any{"results": []any{row}, "facts": []any{}, "relevant_decisions": []any{},
			"known_failure_modes": []any{}, "acceptance_criteria": []any{}, "runbooks": []any{},
			"capabilities_to_use": []any{}, "graph_neighbors": []any{}},
		SearchResponse: map[string]any{}, RequestPayload: map[string]any{"session_id": "precision-session"},
		SourceCoverage: map[string]any{}, GraphQuality: map[string]any{}, Learned: decision,
	})
	if !artifacts.Learned.Performed {
		t.Fatalf("fixture did not establish learned treatment: %#v", artifacts.Learned)
	}
	publicRows := parseRows(artifacts.Compiled["ranked_evidence"])
	internalRows := parseRows(artifacts.Compiled["selection_receipt_ranked_refs"])
	receipt := anyMap(artifacts.Quality["selection_receipt"])
	if len(publicRows) != 1 || len(internalRows) != 1 || anyToInt(internalRows[0]["occurrence"], 0) < 1 ||
		anyToFloat(publicRows[0]["score"]) != roundFloat(anyToFloat(internalRows[0]["score"]), 3) ||
		anyToFloat(publicRows[0]["score"]) == anyToFloat(internalRows[0]["score"]) ||
		anyToString(receipt["schema_id"]) != contextPackSelectionReceiptV2SchemaID ||
		!reflect.DeepEqual(receipt, contextPackSelectionReceiptFromSample(receipt)) {
		t.Fatalf("V2 durable boundary did not bind internal occurrence and six-decimal score: public=%#v internal=%#v receipt=%#v", publicRows, internalRows, receipt)
	}
	candidates := parseRows(receipt["candidates"])
	if len(candidates) != 1 || anyToInt(candidates[0]["occurrence"], 0) != anyToInt(internalRows[0]["occurrence"], 0) ||
		anyToFloat(candidates[0]["final_score"]) != anyToFloat(internalRows[0]["score"]) {
		t.Fatalf("durable V2 row did not retain exact internal actuation facts: internal=%#v receipt=%#v", internalRows, candidates)
	}
}

func TestContextPackLearnedRankingFallsBackWhenReceiptCannotCaptureEveryOccurrence(t *testing.T) {
	contextPack := map[string]any{"results": []any{}}
	for index := 0; index < contextPackSelectionReceiptLimit+1; index++ {
		score := 0.1
		if index == contextPackSelectionReceiptLimit {
			score = 1.0 // The unmodified candidate displaces one downweighted action from the 16+8 capture window.
		}
		contextPack["results"] = append(contextPackAnyList(contextPack["results"]), map[string]any{
			"project": "contextlattice", "file": fmt.Sprintf("result-%02d.md", index), "source": "qdrant",
			"summary": fmt.Sprintf("receipt capture candidate %02d", index), "score": score,
		})
	}
	native := contextPackRankedEvidenceWithLearningAt("receipt capture candidate", contextPack, contextPackTokenBudget{}, contextPackLearnedActivationDecision{}, time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC))
	if len(native.EligibleItems) != contextPackSelectionReceiptLimit+1 {
		t.Fatalf("fixture did not produce the expected eligible pool: %#v", native.EligibleItems)
	}
	decision := contextPackLearnedActivationDecision{
		Armed: true, Eligible: true, AssignedTreatment: true, Arm: "canary", Reason: "test",
		CandidateMultipliers: map[string]float64{},
	}
	for index := 0; index < contextPackSelectionReceiptLimit; index++ {
		decision.CandidateMultipliers[native.EligibleItems[index].CandidateID] = 0.85
	}
	treatment := contextPackRankedEvidenceWithLearningAt("receipt capture candidate", contextPack, contextPackTokenBudget{}, decision, time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC))
	if treatment.LearnedActivation.Performed || treatment.LearnedActivation.Eligible || treatment.LearnedActivation.Reason != "candidate_receipt_capture_incomplete" {
		t.Fatalf("treatment survived without a complete bounded V2 capture: %#v", treatment.LearnedActivation)
	}
	for _, item := range treatment.EligibleItems {
		if item.LearnedInfluenceApplied {
			t.Fatalf("native fallback retained an unreceiptable learned occurrence: %#v", treatment.EligibleItems)
		}
	}
}

func TestContextPackLearnedTreatmentRequiresDurableSelectionReceipt(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", t.TempDir()+"/quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	telemetry := newContextPackQualityTelemetry(20)
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, treatmentIdentityForTest(t, now)))
	_, decision = applyContextPackLearnedRanking([]contextPackEvidenceItem{{
		CandidateID: "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "memory", Score: 90, ImpactScore: 90,
		EstimatedTokens: 10, ValueDensity: 9, Occurrence: 1,
	}}, decision)
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "private query must not persist", Project: "contextlattice", TaskClass: "agent_workflow",
		RetrievalIntent: "decision", TokenImpact: map[string]any{}, Compiled: map[string]any{},
		SourceCoverage: map[string]any{}, GraphQuality: map[string]any{},
		RankedEvidence: []any{map[string]any{
			"candidate_id": "rtc_aaaaaaaaaaaaaaaaaaaaaaaa", "kind": "memory", "rank": 1, "occurrence": 1,
			"learned_base_score": 90.0, "learned_multiplier": 1.15, "score": 103.5,
			"learned_influence_applied": true,
		}},
		LearnedActivation: contextPackLearnedActivationReceipt(decision),
	})
	if err := telemetry.recordQualityDurably(sample); err != nil {
		t.Fatalf("durable learned receipt was rejected: %v", err)
	}
	rows, _, err := telemetry.ledger.readRowsUnlocked()
	if err != nil || len(rows) != 1 || anyToString(anyMap(rows[0]["selection_receipt"])["schema_id"]) != contextPackSelectionReceiptV2SchemaID {
		t.Fatalf("durable learned receipt missing: rows=%#v err=%v", rows, err)
	}

	telemetry.ledger.enabled = false
	retry := cloneJSONMap(sample)
	retry["sample_id"] = "cpq_durable_failure"
	if err := telemetry.recordQualityDurably(retry); err == nil {
		t.Fatal("learned treatment was accepted without a durable receipt ledger")
	}
}

func TestContextPackLearnedPersistenceFailureRebuildsNativeControl(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"project": "contextlattice", "source": "qdrant", "file": "notes/result.md",
		"topic_path": "search", "summary": "bounded result", "score": 0.9,
	}
	assessment := memoryTrustAssessmentForCandidate("memory", "bounded result", row)
	candidateID := anyToString(assessment["candidate_id"])
	input := learnedActivationInput(now, treatmentIdentityForTest(t, now))
	input.Reputation = learnedActivationReputation(now, input.Project, input.TaskClass, input.RetrievalIntent,
		learnedActivationCandidate(now, candidateID, 1.15),
	)
	learnedActivationBindEvaluatedVector(&input)
	decision := decideContextPackLearnedActivation(input)
	compilationInput := contextPackCompilationInput{
		Query: "bounded result", Project: input.Project, TaskClass: input.TaskClass,
		RetrievalMode: "balanced", RetrievalIntent: input.RetrievalIntent, SessionID: "session-test", AgentID: "codex-test",
		ContextPack: map[string]any{"results": []any{row}, "facts": []any{}, "relevant_decisions": []any{},
			"known_failure_modes": []any{}, "acceptance_criteria": []any{}, "runbooks": []any{},
			"capabilities_to_use": []any{}, "graph_neighbors": []any{}},
		SearchResponse: map[string]any{}, RequestPayload: map[string]any{"session_id": "session-test"},
		SourceCoverage: map[string]any{}, GraphQuality: map[string]any{}, Learned: decision,
	}
	artifacts := buildContextPackCompilationArtifacts(compilationInput)
	if !artifacts.Learned.Performed {
		t.Fatalf("fixture did not establish a learned treatment: %#v", artifacts.Learned)
	}
	server := &server{contextPackQuality: newContextPackQualityTelemetryWithLedger(20, &contextPackQualityLedger{enabled: false})}
	fallback := server.persistContextPackCompilationOrFallback(compilationInput, artifacts)
	if fallback.Learned.Performed || fallback.Learned.Eligible || fallback.Learned.Reason != "receipt_persistence_failed" {
		t.Fatalf("persistence failure did not fail closed to native control: %#v", fallback.Learned)
	}
	ranked := parseRows(fallback.Compiled["ranked_evidence"])
	if len(ranked) != 1 || anyToBool(ranked[0]["learned_influence_applied"]) {
		t.Fatalf("fallback retained learned influence: %#v", ranked)
	}
}

func TestContextPackLearnedControlAssignmentAlsoRequiresDurableReceipt(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	decision := decideContextPackLearnedActivation(learnedActivationInput(now, controlIdentityForTest(t, now)))
	if !decision.Eligible || decision.AssignedTreatment || decision.Arm != "control" {
		t.Fatalf("fixture did not establish an eligible control assignment: %#v", decision)
	}
	compilationInput := contextPackCompilationInput{
		Query: "bounded result", Project: "contextlattice", TaskClass: "agent_workflow",
		RetrievalMode: "balanced", RetrievalIntent: "decision", SessionID: "control-session", AgentID: "codex-test",
		ContextPack: map[string]any{"results": []any{}, "facts": []any{}, "relevant_decisions": []any{},
			"known_failure_modes": []any{}, "acceptance_criteria": []any{}, "runbooks": []any{},
			"capabilities_to_use": []any{}, "graph_neighbors": []any{}},
		SearchResponse: map[string]any{}, RequestPayload: map[string]any{"session_id": "control-session"},
		SourceCoverage: map[string]any{}, GraphQuality: map[string]any{}, Learned: decision,
	}
	artifacts := buildContextPackCompilationArtifacts(compilationInput)
	if !artifacts.Learned.Eligible || artifacts.Learned.Arm != "control" {
		t.Fatalf("control assignment was lost before persistence: %#v", artifacts.Learned)
	}
	if anyToString(artifacts.Quality["policy_id"]) != decision.PolicyRef || anyToString(artifacts.Quality["policy_arm"]) != "control" {
		t.Fatalf("control assignment did not retain stable governed policy identity: %#v", artifacts.Quality)
	}
	server := &server{contextPackQuality: newContextPackQualityTelemetryWithLedger(20, &contextPackQualityLedger{enabled: false})}
	fallback := server.persistContextPackCompilationOrFallback(compilationInput, artifacts)
	if fallback.Learned.Eligible || fallback.Learned.Reason != "receipt_persistence_failed" {
		t.Fatalf("undurable control assignment remained cohort eligible: %#v", fallback.Learned)
	}
}
