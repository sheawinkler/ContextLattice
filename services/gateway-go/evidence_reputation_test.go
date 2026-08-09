package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func evidenceReputationTestOutcome(index int, success bool, verifier string) map[string]any {
	class := "success"
	if !success {
		class = "repair_required"
	}
	digest := "sha256:" + sha256Hex("reputation-verification-"+anyToString(index))
	return map[string]any{
		"outcome_id":                   "outcome-" + anyToString(index),
		"project":                      "contextlattice",
		"task_class":                   "coding",
		"capturedAt":                   time.Date(2026, 7, 17, index, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		"calibration_eligible":         true,
		"verification_passed":          true,
		"verification_evidence_digest": digest,
		"verifier_id":                  verifier,
		"first_pass_success":           success,
		"repair_required":              !success,
		"outcome_class":                class,
		"evidence_attribution": []any{map[string]any{
			"entity_type": "source", "entity_id": "qdrant", "issuer": verifier,
			"producer_agent_id": "retrieval_agent", "verifier_id": verifier,
			"attribution_method": "counterfactual", "role": "support",
			"verification_evidence_digest": digest,
		}},
	}
}

func evidenceReputationBoundCandidateFixture(t *testing.T, outcomeID string) (map[string]any, map[string]any, map[string]any) {
	t.Helper()
	_, binding := contextPackOutcomeResponseBindingFixture(t, "reputation-bound-"+outcomeID)
	digest := "sha256:" + sha256Hex("reputation-bound-"+outcomeID)
	row := map[string]any{
		"outcome_id": outcomeID, "sample_id": "sample-" + outcomeID, "workspace_ref": "sha256:" + strings.Repeat("a", 64),
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "entity_id": "candidate-bound", "candidate_ref": "candidate-bound",
			"result_level_credit": "selection_receipt_bound", "selection_state": "selected",
			"attribution_method": "counterfactual", "verifier_id": "verifier-bound",
			"verification_evidence_digest": digest,
		}},
	}
	if !recallResponseCopyBinding(row, binding) {
		t.Fatal("failed to attach canonical response binding to candidate row")
	}
	observation := map[string]any{
		"outcome_id": outcomeID, "sample_id": row["sample_id"], "workspace_ref": row["workspace_ref"],
		"utility": map[string]any{
			"independently_verified": true, "verification_status": "verified", "evidence_digest": digest, "verifier_id": "verifier-bound",
		},
		"eligibility": map[string]any{"observed_yield_eligible": true},
		"denominator": map[string]any{
			"wire_tokens_exact": true, "wire_tokens": 91,
			"model_visible_context_tokens_exact": true, "model_visible_context_tokens": 73,
		},
	}
	if !recallResponseCopyBinding(observation, binding) {
		t.Fatal("failed to attach canonical response binding to Utility observation")
	}
	return row, binding, observation
}

func TestEvidenceReputationRequiresVerifiedIndependentSamplesAndIsDeterministic(t *testing.T) {
	rows := []map[string]any{
		evidenceReputationTestOutcome(1, true, "verifier-a"),
		evidenceReputationTestOutcome(2, true, "verifier-b"),
		evidenceReputationTestOutcome(3, true, "verifier-a"),
		evidenceReputationTestOutcome(4, true, "verifier-b"),
		evidenceReputationTestOutcome(5, false, "verifier-a"),
	}
	duplicate := cloneJSONMap(rows[0])
	duplicate["outcome_id"] = "duplicate-verification"
	rows = append(rows, duplicate)
	unverified := evidenceReputationTestOutcome(6, true, "verifier-b")
	unverified["verification_passed"] = false
	rows = append(rows, unverified)

	options := evidenceReputationOptions{
		Project: "contextlattice", TaskClass: "coding",
		AsOf: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MinimumSamples: 5, MaxEntries: 10,
	}
	first := buildEvidenceReputation(rows, options)
	second := buildEvidenceReputation(append([]map[string]any{}, rows...), options)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reputation projection is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	entries := parseRows(first["entries"])
	if len(entries) != 1 {
		t.Fatalf("expected one bounded entity, got %#v", entries)
	}
	entry := entries[0]
	if !anyToBool(entry["calibrated"]) || anyToInt(entry["sample_count"], 0) != 5 || anyToInt(entry["independent_issuer_count"], 0) != 2 {
		t.Fatalf("verified independent sample floor was not enforced: %#v", entry)
	}
	if anyToString(entry["trust_label"]) != "reliable" || anyToFloat(anyMap(entry["bounded_influence"])["proposed_multiplier"]) <= 1 {
		t.Fatalf("positive verified evidence did not earn bounded advisory reputation: %#v", entry)
	}
	if anyToBool(anyMap(entry["bounded_influence"])["applied"]) || !anyToBool(anyMap(entry["bounded_influence"])["advisory_only"]) {
		t.Fatalf("public reputation silently affected ranking: %#v", entry)
	}
	exclusions := anyMap(anyMap(first["summary"])["exclusions"])
	if anyToInt(exclusions["duplicate_verification"], 0) != 1 || anyToInt(exclusions["verification_missing"], 0) != 1 {
		t.Fatalf("expected duplicate and unverified evidence exclusions: %#v", exclusions)
	}
}

func TestEvidenceReputationRejectsSelfAwardedTrustAndRedactsAttribution(t *testing.T) {
	sample := map[string]any{
		"agent_id": "agent-a", "outcome_source": "agent-a", "verifier_id": "agent-a",
		"verification_evidence_digest": "sha256:" + sha256Hex("self"),
		"evidence_attribution": []any{map[string]any{
			"entity_type": "agent", "agent_id": "agent-a", "attribution_method": "explicit_verified",
			"trusted": true, "trust_label": "authoritative", "authorization": "Bearer never-emit-this-secret",
		}},
	}
	attribution := normalizeEvidenceAttributions(sample)
	if len(attribution) != 1 {
		t.Fatalf("expected one normalized attribution, got %#v", attribution)
	}
	raw, err := json.Marshal(attribution)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"authoritative", "never-emit-this-secret", "\"trusted\"", "authorization"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("self-awarded or secret field crossed attribution boundary: %s", raw)
		}
	}

	row := evidenceReputationTestOutcome(9, true, "agent-a")
	row["verification_evidence_digest"] = anyToString(anyMap(attribution[0])["verification_evidence_digest"])
	row["evidence_attribution"] = attribution
	projection := buildEvidenceReputation([]map[string]any{row}, evidenceReputationOptions{
		AsOf: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MinimumSamples: 2, MaxEntries: 10,
	})
	if got := len(parseRows(projection["entries"])); got != 0 {
		t.Fatalf("agent self-attribution created reputation: %#v", projection)
	}
	if anyToInt(anyMap(anyMap(projection["summary"])["exclusions"])["self_attribution"], 0) != 1 {
		t.Fatalf("self-attribution exclusion was not explicit: %#v", projection)
	}
}

func TestEvidenceReputationRemainsUncalibratedAcrossOneIssuerAndDomainScope(t *testing.T) {
	rows := []map[string]any{}
	for index := 1; index <= 6; index++ {
		row := evidenceReputationTestOutcome(index, true, "single-verifier")
		if index == 6 {
			row["project"] = "other-project"
		}
		rows = append(rows, row)
	}
	projection := buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: "contextlattice", AsOf: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MinimumSamples: 5, MaxEntries: 10,
	})
	entries := parseRows(projection["entries"])
	if len(entries) != 1 || anyToInt(entries[0]["sample_count"], 0) != 5 || anyToBool(entries[0]["calibrated"]) {
		t.Fatalf("one issuer or cross-project evidence escaped calibration boundary: %#v", projection)
	}
	if anyToFloat(anyMap(entries[0]["bounded_influence"])["proposed_multiplier"]) != 1 {
		t.Fatalf("uncalibrated reputation proposed ranking influence: %#v", entries[0])
	}
}

func TestEvidenceReputationDerivesIssuerIndependenceFromVerifiedIdentity(t *testing.T) {
	rows := make([]map[string]any, 0, 5)
	for index := 1; index <= 5; index++ {
		row := evidenceReputationTestOutcome(index, true, "verifier-a")
		attribution := anyMap(contextPackAnyList(row["evidence_attribution"])[0])
		attribution["issuer"] = "reporter-claimed-issuer-" + anyToString(index)
		rows = append(rows, row)
	}
	projection := buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: "contextlattice", TaskClass: "coding",
		AsOf: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MinimumSamples: 5, MaxEntries: 10,
	})
	entries := parseRows(projection["entries"])
	if len(entries) != 1 || anyToInt(entries[0]["independent_issuer_count"], 0) != 1 || anyToBool(entries[0]["calibrated"]) {
		t.Fatalf("reporter issuer labels forged verifier independence: %#v", projection)
	}
}

func TestEvidenceReputationCandidateBindingIsReconciledExactly(t *testing.T) {
	row, binding, observation := evidenceReputationBoundCandidateFixture(t, "outcome-bound-reconcile")
	utility := &utilityTelemetry{byOutcome: map[string]int{"outcome-bound-reconcile": 0}, observations: []map[string]any{observation}}
	reconciled := reconcileCandidateUtilityVerification([]map[string]any{row}, utility)
	verification := anyMap(reconciled[0]["candidate_utility_verification"])
	if !recallResponseBindingsEqual(verification, binding) {
		t.Fatalf("reconciled verification lost the complete canonical binding: verification=%#v binding=%#v", verification, binding)
	}
	if !evidenceReputationCandidateUtilityVerified(reconciled[0], anyMap(contextPackAnyList(row["evidence_attribution"])[0])) {
		t.Fatal("exactly reconciled candidate binding did not earn verification")
	}
}

func TestEvidenceReputationCandidateBindingMalformedMissingOrMismatchedCannotEarnCredit(t *testing.T) {
	row, binding, _ := evidenceReputationBoundCandidateFixture(t, "outcome-bound-gate")
	digest := anyToString(anyMap(contextPackAnyList(row["evidence_attribution"])[0])["verification_evidence_digest"])
	verification := map[string]any{
		"outcome_id": row["outcome_id"], "sample_id": row["sample_id"],
		"independently_verified": true, "verification_status": "verified", "evidence_digest": digest,
		"verifier_id": "verifier-bound", "workspace_ref": row["workspace_ref"],
	}
	if !recallResponseCopyBinding(verification, binding) {
		t.Fatal("failed to attach exact verification binding")
	}
	attribution := anyMap(contextPackAnyList(row["evidence_attribution"])[0])
	exact := cloneJSONMap(row)
	exact["candidate_utility_verification"] = verification
	if !evidenceReputationCandidateUtilityVerified(exact, attribution) {
		t.Fatal("exact row/verification binding was rejected")
	}

	cases := map[string]func(map[string]any, map[string]any){
		"row missing": func(candidate, checked map[string]any) {
			delete(candidate, "recall_response_id")
			delete(candidate, "recall_response_digest")
			delete(candidate, "response_component_refs")
		},
		"verification missing": func(candidate, checked map[string]any) {
			delete(checked, "recall_response_id")
			delete(checked, "recall_response_digest")
			delete(checked, "response_component_refs")
		},
		"row malformed": func(candidate, checked map[string]any) {
			candidate["recall_response_id"] = binding["recall_response_id"]
			delete(candidate, "recall_response_digest")
		},
		"verification malformed": func(candidate, checked map[string]any) {
			checked["recall_response_id"] = binding["recall_response_id"]
			delete(checked, "recall_response_digest")
		},
		"mismatched": func(candidate, checked map[string]any) {
			otherBinding := cloneJSONMap(binding)
			otherBinding["recall_response_id"] = "rr_" + strings.Repeat("b", 24)
			otherBinding["recall_response_digest"] = "sha256:" + strings.Repeat("b", 64)
			if !recallResponseCopyBinding(checked, otherBinding) {
				t.Fatal("failed to attach mismatched binding")
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := cloneJSONMap(exact)
			checked := cloneJSONMap(anyMap(exact["candidate_utility_verification"]))
			candidate["candidate_utility_verification"] = checked
			mutate(candidate, checked)
			if evidenceReputationCandidateUtilityVerified(candidate, attribution) {
				t.Fatalf("%s binding unexpectedly earned candidate credit", name)
			}
		})
	}

	legacy := cloneJSONMap(exact)
	delete(legacy, "recall_response_id")
	delete(legacy, "recall_response_digest")
	delete(legacy, "response_component_refs")
	legacyVerification := cloneJSONMap(anyMap(exact["candidate_utility_verification"]))
	delete(legacyVerification, "recall_response_id")
	delete(legacyVerification, "recall_response_digest")
	delete(legacyVerification, "response_component_refs")
	legacy["candidate_utility_verification"] = legacyVerification
	if !evidenceReputationCandidateUtilityVerified(legacy, attribution) {
		t.Fatal("legacy both-unbound candidate behavior changed")
	}
}

func TestEvidenceReputationActivationScopeDoesNotMixRetrievalIntents(t *testing.T) {
	rows := []map[string]any{}
	for index := 1; index <= 10; index++ {
		row := evidenceReputationTestOutcome(index, true, "verifier-a")
		if index%2 == 0 {
			row["verifier_id"] = "verifier-b"
			attribution := anyMap(contextPackAnyList(row["evidence_attribution"])[0])
			attribution["verifier_id"] = "verifier-b"
			attribution["issuer"] = "verifier-b"
		}
		row["retrieval_intent"] = "decision"
		if index > 5 {
			row["retrieval_intent"] = "exploration"
		}
		rows = append(rows, row)
	}
	projection := buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: "contextlattice", TaskClass: "coding", RetrievalIntent: "decision",
		AsOf: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MinimumSamples: 5, MaxEntries: 10,
	})
	entries := parseRows(projection["entries"])
	if len(entries) != 1 || anyToInt(entries[0]["sample_count"], 0) != 5 {
		t.Fatalf("retrieval intents were mixed in activation reputation: %#v", projection)
	}
	if got := anyToString(anyMap(projection["scope"])["retrieval_intent"]); got != "decision" {
		t.Fatalf("retrieval intent scope missing from reputation snapshot: %#v", projection["scope"])
	}
}
