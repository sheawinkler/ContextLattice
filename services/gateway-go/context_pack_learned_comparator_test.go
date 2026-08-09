package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func contextPackLearnedComparatorCase() (map[string]any, map[string]any) {
	rawCase := map[string]any{
		"query": "alpha", "limit": 6, "max_facts": 6,
		"expected_files":   []any{"expected.md"},
		"forbidden_files":  []any{"forbidden.md"},
		"expected_numeric": []any{"42"},
	}
	results := []any{
		map[string]any{"project": "contextlattice", "file": "one.md", "source": "qdrant", "score": 0.90, "summary": "alpha evidence one"},
		map[string]any{"project": "contextlattice", "file": "two.md", "source": "qdrant", "score": 0.89, "summary": "alpha evidence two"},
		map[string]any{"project": "contextlattice", "file": "three.md", "source": "qdrant", "score": 0.88, "summary": "alpha evidence three"},
		map[string]any{"project": "contextlattice", "file": "four.md", "source": "qdrant", "score": 0.87, "summary": "alpha evidence four"},
		map[string]any{"project": "contextlattice", "file": "forbidden.md", "source": "qdrant", "score": 0.86, "summary": "alpha evidence five"},
		map[string]any{"project": "contextlattice", "file": "expected.md", "source": "qdrant", "score": 0.85, "summary": "alpha evidence contains 42"},
	}
	return rawCase, map[string]any{
		"results": results,
		"grounding": map[string]any{
			"facts": []any{}, "numeric_facts": []any{}, "strict_numeric_copy": true,
		},
	}
}

func TestContextPackLearnedComparatorAuthoritySeparatesIngressAndSigningKeys(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_LEARNED_COMPARATOR_AUTHORITY_KEY", "dedicated-signing-key")
	s := &server{orchestratorAPIKey: "ordinary-ingress-key"}
	payload := map[string]any{contextPackLearnedComparatorAuthorityFlag: true}

	request := httptest.NewRequest("POST", "/memory/recall/evaluate/saved", nil)
	request.Header.Set("X-Api-Key", "dedicated-signing-key")
	if authority := contextPackLearnedComparatorAuthorityForRequest(s, request, payload); authority.Authorized || authority.Reason != "authority_authentication_required" {
		t.Fatalf("signing key authenticated ordinary ingress: %#v", authority)
	}

	request = httptest.NewRequest("POST", "/memory/recall/evaluate/saved", nil)
	request.Header.Set("X-Api-Key", "ordinary-ingress-key")
	if authority := contextPackLearnedComparatorAuthorityForRequest(s, request, payload); authority.Reason == "authority_authentication_required" {
		t.Fatalf("ordinary ingress key was compared with the signing key: %#v", authority)
	}
}

func TestContextPackLearnedComparatorAuthorityRejectsCallerOptions(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_LEARNED_COMPARATOR_AUTHORITY_KEY", "dedicated-signing-key")
	s := &server{orchestratorAPIKey: "ordinary-ingress-key"}
	request := httptest.NewRequest("POST", "/memory/recall/evaluate/saved", nil)
	request.Header.Set("X-Api-Key", "ordinary-ingress-key")

	canonical := map[string]any{contextPackLearnedComparatorAuthorityFlag: true, "mode": "evaluate"}
	if authority := contextPackLearnedComparatorAuthorityForRequest(s, request, canonical); authority.Reason == "caller_personalized_evaluation" {
		t.Fatalf("canonical authority payload was rejected as customized: %#v", authority)
	}
	for _, key := range []string{"include_retrieval_debug", "include_preferences", "user_id", "k", "unknown_option"} {
		payload := cloneJSONMap(canonical)
		payload[key] = true
		if authority := contextPackLearnedComparatorAuthorityForRequest(s, request, payload); authority.Authorized || authority.Reason != "caller_personalized_evaluation" {
			t.Fatalf("caller option %q retained activation authority: %#v", key, authority)
		}
	}
}

func TestContextPackLearnedComparatorActivationProofRequiresExactSignedWorkspace(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_LEARNED_COMPARATOR_AUTHORITY_KEY", "dedicated-signing-key")
	rawCase, searchResponse := contextPackLearnedComparatorCase()
	expectedCandidate := contextPackLearnedComparatorCandidateByFile(t, rawCase, searchResponse, "expected.md")
	forbiddenCandidate := contextPackLearnedComparatorCandidateByFile(t, rawCase, searchResponse, "forbidden.md")
	comparison := newContextPackLearnedActuatorComparison(
		"sha256:"+sha256Hex("authorized-workspace-case-set"), 1,
		map[string]float64{expectedCandidate: 1.15, forbiddenCandidate: 0.85}, "",
	)
	workspaceRef := contextPackLearnedScopeRef("workspace", "workspace-authorized")
	comparison.setAuthorityEnvelope(contextPackLearnedComparatorAuthorityEnvelope("dedicated-signing-key", workspaceRef))
	comparison.addCase(rawCase, searchResponse, 5)
	artifact := comparison.monitorFields()
	shadow := map[string]any{
		"case_count": 1, "case_set_ref": comparison.caseSetRef, "workspace_ref": workspaceRef,
		"learned_actuator_comparator": artifact,
	}
	s := &server{orchestratorAPIKey: "ordinary-ingress-key"}
	if normalized, proofRef, ok := contextPackLearnedActuatorComparatorProofForWorkspace(s, shadow, workspaceRef); !ok || len(normalized) == 0 || !isSearchIntelligenceFullSHA256Ref(proofRef) {
		t.Fatalf("exact signed workspace comparator was rejected: normalized=%#v ref=%q ok=%v", normalized, proofRef, ok)
	}
	if _, _, ok := contextPackLearnedActuatorComparatorProofForWorkspace(s, shadow, contextPackLearnedScopeRef("workspace", "workspace-other")); ok {
		t.Fatal("cross-workspace comparator proof was accepted")
	}
	tampered := cloneJSONMap(shadow)
	tamperedAuthority := anyMap(anyMap(tampered["learned_actuator_comparator"])["authority"])
	tamperedAuthority["authority_signature"] = "sha256:" + strings.Repeat("f", 64)
	if _, _, ok := contextPackLearnedActuatorComparatorProofForWorkspace(s, tampered, workspaceRef); ok {
		t.Fatal("tampered comparator authority was accepted")
	}
}

func contextPackLearnedComparatorOutcomeRows(candidateRef string, asOf time.Time) []map[string]any {
	rows := make([]map[string]any, 0, contextPackLearnedMinimumSamples)
	for index := 0; index < contextPackLearnedMinimumSamples; index++ {
		verifier := []string{"verifier-a", "verifier-b"}[index%2]
		row := evidenceReputationTestOutcome(index+1, true, verifier)
		row["retrieval_intent"] = "decision"
		row["capturedAt"] = asOf.Add(time.Duration(-index-1) * time.Hour).Format(time.RFC3339Nano)
		digest := anyToString(row["verification_evidence_digest"])
		row["evidence_attribution"] = []any{map[string]any{
			"entity_type": "candidate", "entity_id": candidateRef, "candidate_ref": candidateRef,
			"result_level_credit": "selection_receipt_bound", "selection_state": "selected",
			"attribution_method": "counterfactual", "issuer": verifier,
			"producer_agent_id": "retrieval-agent", "verifier_id": verifier,
			"verification_evidence_digest": digest,
		}}
		row["candidate_utility_verification"] = map[string]any{
			"outcome_id": row["outcome_id"], "sample_id": row["sample_id"],
			"independently_verified": true, "verification_status": "verified",
			"evidence_digest": digest, "verifier_id": verifier,
		}
		rows = append(rows, row)
	}
	return rows
}

func contextPackLearnedComparatorCandidateByFile(t *testing.T, rawCase, searchResponse map[string]any, file string) string {
	t.Helper()
	pack := buildContextPackPayload(
		anyToString(rawCase["query"]), searchResponse,
		anyToInt(rawCase["max_facts"], 24), anyToInt(rawCase["limit"], 10),
	)
	allocation := contextPackRankedEvidenceWithLearning(
		anyToString(rawCase["query"]), pack, contextPackTokenBudgetFromRequest(rawCase), contextPackLearnedActivationDecision{},
	)
	for _, item := range allocation.EligibleItems {
		if item.File == file {
			return item.CandidateID
		}
	}
	t.Fatalf("candidate for file %q missing from exact post-trust pool: %#v", file, allocation.EligibleItems)
	return ""
}

func TestContextPackLearnedActuatorComparatorProducesStrictExactProof(t *testing.T) {
	rawCase, searchResponse := contextPackLearnedComparatorCase()
	expectedCandidate := contextPackLearnedComparatorCandidateByFile(t, rawCase, searchResponse, "expected.md")
	forbiddenCandidate := contextPackLearnedComparatorCandidateByFile(t, rawCase, searchResponse, "forbidden.md")
	multipliers := map[string]float64{expectedCandidate: 1.15, forbiddenCandidate: 0.85}
	vectorRef := contextPackLearnedReputationVectorRef(multipliers)
	caseSetRef := "sha256:" + sha256Hex("exact-actuator-case-set")
	comparison := newContextPackLearnedActuatorComparison(caseSetRef, 1, multipliers, "")
	comparison.addCase(rawCase, searchResponse, 5)
	artifact := comparison.monitorFields()
	if !anyToBool(artifact["comparison_valid"]) || anyToString(artifact["comparison_reason"]) != "valid" {
		t.Fatalf("exact comparator did not pass: %#v", artifact)
	}
	if anyToInt(artifact["influenced_case_count"], 0) != 1 || anyToString(artifact["reputation_vector_ref"]) != vectorRef {
		t.Fatalf("exact influenced vector was not bound: %#v", artifact)
	}
	shadow := map[string]any{
		"case_count": 1, "case_set_ref": caseSetRef, "learned_actuator_comparator": artifact,
	}
	normalized, proofRef, ok := contextPackLearnedActuatorComparatorProof(shadow)
	if !ok || !isSearchIntelligenceFullSHA256Ref(proofRef) || anyToString(normalized["reputation_vector_ref"]) != vectorRef {
		t.Fatalf("strict consumer rejected producer artifact: normalized=%#v ref=%q ok=%v", normalized, proofRef, ok)
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"expected.md", "forbidden.md", "alpha evidence", expectedCandidate, forbiddenCandidate} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("comparator artifact leaked raw evidence %q: %s", forbidden, encoded)
		}
	}
}

func TestContextPackLearnedActuatorComparatorAllowsNeutralCasesButRequiresInfluence(t *testing.T) {
	rawCase, searchResponse := contextPackLearnedComparatorCase()
	missingCandidate := "rtc_ffffffffffffffffffffffff"
	neutralMultipliers := map[string]float64{missingCandidate: 1.15}
	neutral := newContextPackLearnedActuatorComparison(
		"sha256:"+sha256Hex("neutral-only"), 1, neutralMultipliers, "",
	)
	neutral.addCase(rawCase, searchResponse, 5)
	neutralArtifact := neutral.monitorFields()
	if anyToBool(neutralArtifact["comparison_valid"]) || anyToString(neutralArtifact["comparison_reason"]) != "reputation_candidate_influence_unavailable" ||
		anyToInt(neutralArtifact["case_count"], 0) != 1 {
		t.Fatalf("all-neutral cohort did not fail closed after exact evaluation: %#v", neutralArtifact)
	}

	expectedCandidate := contextPackLearnedComparatorCandidateByFile(t, rawCase, searchResponse, "expected.md")
	multipliers := map[string]float64{expectedCandidate: 1.15}
	mixed := newContextPackLearnedActuatorComparison(
		"sha256:"+sha256Hex("mixed-neutral"), 2, multipliers, "",
	)
	mixed.addCase(rawCase, searchResponse, 5)
	neutralCase := cloneJSONMap(searchResponse)
	neutralResults := contextPackAnyList(neutralCase["results"])
	for _, row := range neutralResults {
		candidate := anyMap(row)
		candidate["file"] = "neutral-" + anyToString(candidate["file"])
	}
	neutralCase["results"] = neutralResults
	mixed.addCase(rawCase, neutralCase, 5)
	mixedArtifact := mixed.monitorFields()
	if !anyToBool(mixedArtifact["comparison_valid"]) || anyToInt(mixedArtifact["case_count"], 0) != 2 ||
		anyToInt(mixedArtifact["influenced_case_count"], 0) != 1 {
		t.Fatalf("influenced plus neutral exact cohort did not pass: %#v", mixedArtifact)
	}
}

func TestSavedRecallComparisonProducesActuatorComparatorFromVerifiedOutcomes(t *testing.T) {
	rawCase, searchResponse := contextPackLearnedComparatorCase()
	rawCase["project"] = "contextlattice"
	rawCase["task_class"] = "coding"
	rawCase["retrieval_intent"] = "decision"
	rawCase["decision_impact_grade"] = 3
	expectedCandidate := contextPackLearnedComparatorCandidateByFile(t, rawCase, searchResponse, "expected.md")
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	comparison := newSavedRecallImpactComparisonWithOutcomeRows(
		recallEvalSavedConfig{Version: 1, Cases: []map[string]any{rawCase}},
		contextPackLearnedComparatorOutcomeRows(expectedCandidate, asOf), asOf,
	)
	comparison.addActuatorCase(0, rawCase, searchResponse)
	cohort := comparison.cohorts[savedRecallImpactScopeForCase(rawCase)]
	if cohort == nil || cohort.actuator == nil {
		t.Fatal("saved recall evaluator did not construct the exact actuator sidecar")
	}
	artifact := cohort.actuator.monitorFields()
	if !anyToBool(artifact["comparison_valid"]) || anyToString(artifact["comparison_reason"]) != "valid" {
		t.Fatalf("verified outcomes did not produce an actionable exact comparator: %#v", artifact)
	}
	if _, _, ok := contextPackLearnedActuatorComparatorProof(map[string]any{
		"case_count": 1, "case_set_ref": comparison.caseSetRef, "learned_actuator_comparator": artifact,
	}); !ok {
		t.Fatalf("saved evaluator producer did not satisfy the strict activation consumer: %#v", artifact)
	}
}
