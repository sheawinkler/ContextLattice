package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCausalBridgeExplanationSupportsVerifiedCurrentTypedEdge(t *testing.T) {
	asOf := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	graph := []any{causalBridgeTestGraphRow("causes", "edge_alpha_beta", "alpha::notes/cause.md", "beta::notes/effect.md", 0.93, true)}
	claims := []temporalClaim{
		causalBridgeTestClaim("claim_cause", "alpha", "alpha::notes/cause.md", nil, "active", 0.91),
		causalBridgeTestClaim("claim_effect", "beta", "beta::notes/effect.md", []string{"claim_cause"}, "active", 0.89),
	}

	projection := causalBridgeExplanationProjection("alpha", graph, claims, asOf, 8)
	if anyToString(projection["schema_id"]) != causalBridgeExplanationContractID || anyToString(projection["status"]) != "supported" {
		t.Fatalf("unexpected projection identity: %#v", projection)
	}
	explanation := causalBridgeTestFirstExplanation(t, projection)
	if anyToString(explanation["decision"]) != "supported" || !anyToBool(explanation["causality_claimed"]) {
		t.Fatalf("expected supported causal bridge: %#v", explanation)
	}
	typedEdge := anyMap(explanation["typed_edge"])
	if anyToString(typedEdge["edge_type"]) != "causes" || anyToString(typedEdge["semantic_class"]) != "causal" || !anyToBool(typedEdge["causal_capable"]) {
		t.Fatalf("expected typed causal edge: %#v", typedEdge)
	}
	if anyToString(explanation["dangling_edge_status"]) != "resolved" || !anyToBool(explanation["temporally_valid"]) {
		t.Fatalf("expected resolved temporally valid bridge: %#v", explanation)
	}
	if !anyToBool(anyMap(explanation["citation_sufficiency"])["sufficient"]) || len(contextPackAnyList(explanation["missing_proof"])) != 0 {
		t.Fatalf("expected sufficient citations and no missing proof: %#v", explanation)
	}
	if anyToFloat(anyMap(explanation["confidence"])["final"]) <= 0 || len(contextPackAnyList(explanation["support_refs"])) < 5 {
		t.Fatalf("expected decomposed positive confidence and bounded support: %#v", explanation)
	}

	encoded, err := json.Marshal(explanation)
	if err != nil {
		t.Fatalf("marshal explanation: %v", err)
	}
	for _, rawIdentity := range []string{"alpha::notes/cause.md", "beta::notes/effect.md", "claim_cause", "claim_effect", "edge_alpha_beta", "sha256:effect"} {
		if strings.Contains(string(encoded), rawIdentity) {
			t.Fatalf("evidence reference leaked non-opaque identity %q: %s", rawIdentity, string(encoded))
		}
	}
}

func TestCausalBridgeExplanationAbstainsForAssociativeLexicalSimilarity(t *testing.T) {
	asOf := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	graph := []any{causalBridgeTestGraphRow("same_topic", "edge_similar", "alpha::notes/release.md", "beta::notes/release.md", 0.99, true)}
	claims := []temporalClaim{
		causalBridgeTestClaim("claim_alpha_release", "alpha", "alpha::notes/release.md", nil, "active", 0.99),
		causalBridgeTestClaim("claim_beta_release", "beta", "beta::notes/release.md", nil, "active", 0.99),
	}

	projection := causalBridgeExplanationProjection("alpha", graph, claims, asOf, 8)
	explanation := causalBridgeTestFirstExplanation(t, projection)
	if anyToString(explanation["decision"]) != "abstain" || anyToBool(explanation["causality_claimed"]) {
		t.Fatalf("associative similarity must abstain: %#v", explanation)
	}
	if !causalBridgeTestMissingCode(explanation["missing_proof"], "explicit_causal_edge") ||
		!causalBridgeTestMissingCode(explanation["missing_proof"], "structured_causal_claim_link") {
		t.Fatalf("expected explicit causal proof disclosures: %#v", explanation["missing_proof"])
	}
	if anyToFloat(anyMap(explanation["confidence"])["final"]) != 0 {
		t.Fatalf("abstained bridge must expose zero final causal confidence: %#v", explanation["confidence"])
	}
}

func TestCausalBridgeExplanationKeepsHistoricalClaimsAsNonInfluentialOpposition(t *testing.T) {
	asOf := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	graph := []any{causalBridgeTestGraphRow("causes", "edge_history", "alpha::notes/cause.md", "beta::notes/effect.md", 0.9, true)}
	cause := causalBridgeTestClaim("claim_current_cause", "alpha", "alpha::notes/cause.md", nil, "active", 0.9)
	effect := causalBridgeTestClaim("claim_current_effect", "beta", "beta::notes/effect.md", []string{"claim_current_cause", "claim_superseded_cause"}, "active", 0.9)
	expiredEffect := causalBridgeTestClaim("claim_expired_effect", "beta", "beta::notes/effect.md", []string{"claim_current_cause"}, "expired", 0.99)
	supersededCause := causalBridgeTestClaim("claim_superseded_cause", "alpha", "alpha::notes/cause.md", nil, "superseded", 0.99)
	retractedOpposition := causalBridgeTestClaim("claim_retracted_opposition", "beta", "beta::notes/effect.md", nil, "retracted", 0.99)
	retractedOpposition.Contradicts = []string{"claim_current_effect"}

	projection := causalBridgeExplanationProjection("alpha", graph, []temporalClaim{
		cause, effect, expiredEffect, supersededCause, retractedOpposition,
	}, asOf, 8)
	explanation := causalBridgeTestFirstExplanation(t, projection)
	if anyToString(explanation["decision"]) != "supported" {
		t.Fatalf("historical opposition must not veto current verified proof: %#v", explanation)
	}
	statuses := map[string]bool{}
	for _, raw := range contextPackAnyList(explanation["opposition_refs"]) {
		ref := anyMap(raw)
		if !anyToBool(ref["historical"]) || anyToString(ref["influence"]) != "none" {
			t.Fatalf("historical claim gained influence: %#v", ref)
		}
		statuses[anyToString(ref["status"])] = true
	}
	for _, status := range []string{"expired", "superseded", "retracted"} {
		if !statuses[status] {
			t.Fatalf("missing labeled %s opposition: %#v", status, explanation["opposition_refs"])
		}
	}
}

func TestCausalBridgeExplanationAbstainsForCurrentOpposition(t *testing.T) {
	asOf := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	graph := []any{causalBridgeTestGraphRow("causes", "edge_opposed", "alpha::notes/cause.md", "beta::notes/effect.md", 0.9, true)}
	cause := causalBridgeTestClaim("claim_cause", "alpha", "alpha::notes/cause.md", nil, "active", 0.9)
	effect := causalBridgeTestClaim("claim_effect", "beta", "beta::notes/effect.md", []string{"claim_cause"}, "active", 0.9)
	opposition := causalBridgeTestClaim("claim_current_opposition", "beta", "beta::notes/opposition.md", nil, "active", 0.9)
	opposition.Contradicts = []string{"claim_effect"}

	explanation := causalBridgeTestFirstExplanation(t, causalBridgeExplanationProjection("alpha", graph, []temporalClaim{cause, effect, opposition}, asOf, 8))
	if anyToString(explanation["decision"]) != "abstain" || !causalBridgeTestMissingCode(explanation["missing_proof"], "unresolved_current_opposition") {
		t.Fatalf("current opposition must veto the causal conclusion: %#v", explanation)
	}
	refs := contextPackAnyList(explanation["opposition_refs"])
	if len(refs) != 1 || anyToBool(anyMap(refs[0])["historical"]) || anyToString(anyMap(refs[0])["influence"]) != "opposition" {
		t.Fatalf("current opposition was not labeled as influential opposition: %#v", refs)
	}
}

func TestCausalBridgeExplanationIsBoundedDeterministicAndDisclosesDanglingEdges(t *testing.T) {
	asOf := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	graph := make([]any, 0, 12)
	for index := 0; index < 12; index++ {
		graph = append(graph, causalBridgeTestGraphRow(
			"causes",
			fmt.Sprintf("edge_%02d", index),
			"alpha::notes/cause.md",
			fmt.Sprintf("beta%d::notes/effect.md", index),
			1-float64(index)/100,
			index != 0,
		))
	}

	first := causalBridgeExplanationProjection("alpha", graph, nil, asOf, 2)
	second := causalBridgeExplanationProjection("alpha", graph, nil, asOf, 2)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("projection is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if !anyToBool(first["truncated"]) || anyToInt(first["candidate_count"], 0) != 12 || len(contextPackAnyList(first["explanations"])) != 2 {
		t.Fatalf("projection did not enforce requested bound: %#v", first)
	}
	explanation := causalBridgeTestFirstExplanation(t, first)
	if anyToString(explanation["dangling_edge_status"]) != "dangling_target" || !causalBridgeTestMissingCode(explanation["missing_proof"], "resolved_edge_endpoints") {
		t.Fatalf("expected dangling-edge disclosure: %#v", explanation)
	}
	if len(contextPackAnyList(explanation["alternatives"])) != 1 {
		t.Fatalf("expected one bounded alternative among two explanations: %#v", explanation["alternatives"])
	}
}

func TestCausalBridgeExplanationHonorsInverseTypedDirection(t *testing.T) {
	asOf := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	graph := []any{causalBridgeTestGraphRow("depends-on", "edge_dependency", "alpha::notes/effect.md", "beta::notes/cause.md", 0.88, true)}
	claims := []temporalClaim{
		causalBridgeTestClaim("claim_beta_cause", "beta", "beta::notes/cause.md", nil, "active", 0.9),
		causalBridgeTestClaim("claim_alpha_effect", "alpha", "alpha::notes/effect.md", []string{"claim_beta_cause"}, "active", 0.9),
	}

	explanation := causalBridgeTestFirstExplanation(t, causalBridgeExplanationProjection("alpha", graph, claims, asOf, 8))
	typedEdge := anyMap(explanation["typed_edge"])
	if anyToString(typedEdge["edge_type"]) != "depends_on" || anyToString(typedEdge["cause_direction"]) != "target_to_source" || anyToString(explanation["decision"]) != "supported" {
		t.Fatalf("inverse typed relation was not honored: %#v", explanation)
	}
}

func TestCognitionProofSelectorKeepsStaleClaimsOutOfInfluence(t *testing.T) {
	current := temporalClaim{
		ClaimID: "claim_current", Project: "alpha", Subject: "release", Predicate: "state", Object: "ready",
		Statement: "The release is ready.", Status: "active", Confidence: 0.8,
		Contradicts: []string{"claim_retracted"}, CausedBy: []string{"claim_expired"},
	}
	expired := current
	expired.ClaimID, expired.Status, expired.Confidence = "claim_expired", "expired", 1
	superseded := current
	superseded.ClaimID, superseded.Status, superseded.Confidence = "claim_superseded", "superseded", 1
	superseded.Contradicts, superseded.CausedBy = nil, nil
	retracted := current
	retracted.ClaimID, retracted.Status, retracted.Confidence = "claim_retracted", "retracted", 1
	retracted.Contradicts, retracted.CausedBy = nil, nil
	retracted.Subject, retracted.Object, retracted.Statement = "obsolete artifact", "withdrawn", "An obsolete artifact was withdrawn."
	for _, claim := range []*temporalClaim{&current, &expired, &superseded, &retracted} {
		claim.searchText = temporalClaimSearchText(*claim)
	}

	findings := []any{
		map[string]any{"kind": "decision", "text": "The release is ready.", "file": "notes/release.md"},
	}
	selected := relevantTemporalClaims("release ready", findings, []temporalClaim{expired, retracted, current, superseded}, 32)
	if !causalBridgeTestTemporalClaimPresent(selected, "claim_retracted") {
		t.Fatalf("explicitly linked retracted claim was lost during bounded relevance selection: %#v", selected)
	}
	claims, excluded := proofClaimsFromSynthesis("alpha", findings, selected)
	if excluded != 0 || len(claims) != 1 {
		t.Fatalf("unexpected proof projection: excluded=%d claims=%#v", excluded, claims)
	}
	proof := anyMap(claims[0])
	if !causalBridgeTestRefIDPresent(proof["support"], "claim_current") {
		t.Fatalf("current claim missing from support: %#v", proof)
	}
	for _, staleID := range []string{"claim_expired", "claim_superseded", "claim_retracted"} {
		if causalBridgeTestRefIDPresent(proof["support"], staleID) {
			t.Fatalf("stale claim %s influenced support: %#v", staleID, proof["support"])
		}
		ref := causalBridgeTestRefByID(proof["opposition"], staleID)
		if len(ref) == 0 || !anyToBool(ref["historical"]) || anyToString(ref["influence"]) != "none" {
			t.Fatalf("stale claim %s was not labeled historical-only opposition: %#v", staleID, proof["opposition"])
		}
	}
	if len(contextPackAnyList(proof["causal_chain"])) != 0 {
		t.Fatalf("expired causal antecedent leaked into causal chain: %#v", proof["causal_chain"])
	}
	if summaries := proofContradictionSummary([]temporalClaim{current, expired, superseded, retracted}); len(summaries) != 0 {
		t.Fatalf("historical target influenced contradiction gate: %#v", summaries)
	}
	if chains := proofCausalChains([]temporalClaim{current, expired, superseded, retracted}); len(chains) != 0 {
		t.Fatalf("historical antecedent influenced causal chains: %#v", chains)
	}
}

func causalBridgeTestGraphRow(relation string, edgeID string, seedID string, targetID string, score float64, hydrated bool) map[string]any {
	targetProject, targetFile, _, _, _ := canonicalMemoryID(targetID)
	return map[string]any{
		"memory_id": targetID, "project": targetProject, "file": targetFile,
		"seed_memory_id": seedID, "relation": relation, "edge_id": edgeID,
		"edge_direction": "out", "score": score, "hydrated": hydrated,
		"hydration_status": map[bool]string{true: "hydrated", false: "missing"}[hydrated],
		"content_ref":      "sha256:effect",
	}
}

func causalBridgeTestClaim(id string, project string, memoryID string, causedBy []string, status string, confidence float64) temporalClaim {
	claim := temporalClaim{
		ClaimID: id, Project: project, Subject: id, Predicate: "state", Object: "observed",
		Statement: id + " is observed.", Status: status, Confidence: confidence, CausedBy: causedBy,
		Support:      []temporalClaimEvidence{{RefID: memoryID, Kind: "memory", MemoryID: memoryID}},
		Verification: map[string]any{"status": "verified", "method": "test fixture"},
	}
	claim.searchText = temporalClaimSearchText(claim)
	return claim
}

func causalBridgeTestFirstExplanation(t *testing.T, projection map[string]any) map[string]any {
	t.Helper()
	rows := contextPackAnyList(projection["explanations"])
	if len(rows) == 0 {
		t.Fatalf("expected at least one explanation: %#v", projection)
	}
	return anyMap(rows[0])
}

func causalBridgeTestMissingCode(raw any, code string) bool {
	for _, item := range contextPackAnyList(raw) {
		if anyToString(anyMap(item)["code"]) == code {
			return true
		}
	}
	return false
}

func causalBridgeTestRefIDPresent(raw any, refID string) bool {
	return len(causalBridgeTestRefByID(raw, refID)) > 0
}

func causalBridgeTestRefByID(raw any, refID string) map[string]any {
	for _, item := range contextPackAnyList(raw) {
		ref := anyMap(item)
		if anyToString(ref["ref_id"]) == refID {
			return ref
		}
	}
	return nil
}

func causalBridgeTestTemporalClaimPresent(claims []temporalClaim, claimID string) bool {
	for _, claim := range claims {
		if claim.ClaimID == claimID {
			return true
		}
	}
	return false
}
