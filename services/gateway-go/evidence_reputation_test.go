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
