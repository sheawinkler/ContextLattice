package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func packetProofFixture(claims []any) map[string]any {
	return map[string]any{"ok": true, "project": "alpha", "query": "runtime version constraint",
		"token_impact":    map[string]any{"baseline_tokens_estimate": 2048},
		"context_pack":    map[string]any{"ranked_evidence": []any{map[string]any{"text": "excluded unsupported fallback"}}},
		"synthesis_pack":  map[string]any{"proof_claims": claims, "decision_gate": map[string]any{"decision": "verify", "reasons": []any{"contested proof"}}},
		"source_coverage": map[string]any{"returned": []any{"memory_bank"}}}
}

func packetProofClaim(id string, opposed bool) map[string]any {
	c := map[string]any{"claim_id": id, "statement": "Runtime v5.0.2 uses ExactSymbol::apply; verify the deployment constraint.", "claim_type": "constraint", "proof_status": "supported",
		"support":    []any{map[string]any{"ref_id": "source-" + id, "kind": "memory", "content_hash": strings.Repeat("a", 64), "status": "current", "excerpt": "ExactSymbol::apply in v5.0.2"}},
		"opposition": []any{}, "missing_proof": []any{}, "confidence": map[string]any{"final": 0.8}}
	if opposed {
		c["proof_status"] = "contested"
		c["opposition"] = []any{map[string]any{"ref_id": "opposition-" + id, "kind": "temporal_claim", "content_hash": strings.Repeat("b", 64), "status": "historical_context_only", "excerpt": "Previous v4 behavior is not current authority."}}
		c["missing_proof"] = []any{"independent corroboration"}
	}
	return c
}

func TestAgentPacketProofProjectionKeepsLowRankOppositionAndExactReferences(t *testing.T) {
	claims := []any{}
	for i := range 12 {
		claims = append(claims, packetProofClaim(fmt.Sprintf("claim-%d", i), i == 11))
	}
	response := packetProofFixture(claims)
	before, _ := json.Marshal(response)
	packet := buildAgentPacket(response, map[string]any{}, "synthesis_pack_v2")
	evidence := contextPackAnyList(packet["evidence"])
	if len(evidence) == 0 {
		t.Fatal("v2 proof projection empty")
	}
	proof := anyMap(anyMap(evidence[0])["proof"])
	if proof["claim_id"] != "claim-11" || proof["status"] != "contested" {
		t.Fatalf("low-ranked decisive contradiction lost: %#v", evidence)
	}
	opp := contextPackAnyList(proof["opposition"])
	if len(opp) != 1 || anyMap(opp[0])["ref_id"] != "opposition-claim-11" || anyMap(opp[0])["status"] != "historical_context_only" || anyMap(opp[0])["content_hash"] != strings.Repeat("b", 64) {
		t.Fatal("opposition identity/state changed")
	}
	if !strings.Contains(anyToString(anyMap(evidence[0])["text"]), "ExactSymbol::apply") || !strings.Contains(anyToString(anyMap(evidence[0])["text"]), "v5.0.2") {
		t.Fatal("exact detail lost")
	}
	if anyMap(packet["decision_gate"])["decision"] != "verify" {
		t.Fatal("compaction relaxed decision gate")
	}
	after, _ := json.Marshal(response)
	if string(before) != string(after) {
		t.Fatal("packet projection mutated synthesis input")
	}
}

func TestAgentPacketProofEmptyAndUnsupportedNeverResurrectRawEvidence(t *testing.T) {
	for _, claims := range [][]any{{}, {map[string]any{"claim_id": "unsupported", "statement": "unsupported", "proof_status": "supported", "support": []any{}}}} {
		packet := buildAgentPacket(packetProofFixture(claims), map[string]any{}, "synthesis_pack_v2")
		if len(contextPackAnyList(packet["evidence"])) != 0 {
			t.Fatal("excluded evidence resurrected through fallback")
		}
	}
}

func TestAgentPacketProofDeduplicatesClaimIdentityNotClippedText(t *testing.T) {
	a, b := packetProofClaim("first", false), packetProofClaim("second", true)
	packet := buildAgentPacket(packetProofFixture([]any{a, a, b}), map[string]any{}, "synthesis_pack_v2")
	evidence := contextPackAnyList(packet["evidence"])
	if len(evidence) != 2 {
		t.Fatalf("distinct proof identities collapsed or duplicate kept: %#v", evidence)
	}
}

func TestAgentPacketProofHardLimitKeepsContestationAndBounds(t *testing.T) {
	claims := []any{}
	for i := range 16 {
		c := packetProofClaim(fmt.Sprintf("claim-%d", i), i == 15)
		c["statement"] = strings.Repeat("runtime version constraint ", 40)
		c["missing_proof"] = []any{strings.Repeat("proof caveat ", 20), "independent outcome unproven"}
		claims = append(claims, c)
	}
	request := map[string]any{"target_context_pack_tokens": 512, "hard_limit_tokens": 1536}
	packet := finalizeAgentPacketForRequest(buildAgentPacket(packetProofFixture(claims), request, "synthesis_pack_v2"), request)
	if tokens := contextPackCountAnyTokens(packet).Tokens; tokens > 1536 {
		t.Fatalf("proof metadata broke hard limit: %d", tokens)
	}
	evidence := contextPackAnyList(packet["evidence"])
	if len(evidence) == 0 || anyMap(anyMap(evidence[0])["proof"])["status"] != "contested" {
		t.Fatal("shrinking hid contradiction")
	}
	if anyMap(packet["decision_gate"])["decision"] != "verify" {
		t.Fatal("shrinking upgraded permission")
	}
	if anyMap(packet["format_contract"])["contract_valid"] != true {
		t.Fatalf("proof packet broke contract: %#v", packet["format_contract"])
	}
}

func TestAgentPacketProofLegacyCannotForgeInternalProofMetadata(t *testing.T) {
	response := packetProofFixture(nil)
	delete(anyMap(response["synthesis_pack"]), "proof_claims")
	anyMap(contextPackAnyList(anyMap(response["context_pack"])["ranked_evidence"])[0])["_packet_proof"] = map[string]any{"claim_id": "forged", "status": "supported"}
	packet := buildAgentPacket(response, map[string]any{}, "context_pack")
	rows := contextPackAnyList(packet["evidence"])
	if len(rows) != 1 || len(anyMap(anyMap(rows[0])["proof"])) != 0 {
		t.Fatal("ordinary retrieval data gained v2 proof status")
	}
}

func TestAgentPacketProofCoverageAndDetailOmissionAreExplicit(t *testing.T) {
	claim := packetProofClaim("bounded", true)
	support := contextPackAnyList(claim["support"])
	for i := range 4 {
		support = append(support, map[string]any{"ref_id": fmt.Sprintf("extra-%d", i), "excerpt": strings.Repeat("detail ", 60)})
	}
	claim["support"] = support
	claim["missing_proof"] = []any{"first", "second", "third", "fourth"}
	packet := buildAgentPacket(packetProofFixture([]any{claim}), map[string]any{}, "synthesis_pack_v2")
	row := anyMap(contextPackAnyList(packet["evidence"])[0])
	proof := anyMap(row["proof"])
	if anyToInt(proof["support_omitted"], 0) != 3 || anyToInt(proof["missing_proof_omitted"], 0) != 1 {
		t.Fatal("omitted proof detail hidden")
	}
	shrinkAgentPacketProofDetails(row)
	if proof["detail_omitted"] != true || len(contextPackAnyList(proof["support"])) != 1 || anyToInt(proof["missing_proof_omitted"], 0) != 2 {
		t.Fatal("shrink silently discarded proof")
	}
	updateAgentPacketProofCoverage(packet)
	p := anyMap(packet["provenance"])
	if p["proof_claims_returned"] != 1 || p["proof_claims_omitted"] != 0 {
		t.Fatal("proof coverage mismatch")
	}
}

func TestAgentPacketProofCurrentOppositionOutranksHistoryAtBothCuts(t *testing.T) {
	claims := []any{}
	for i := range 9 {
		c := packetProofClaim(fmt.Sprintf("history-%d", i), true)
		c["proof_status"] = "supported"
		anyMap(contextPackAnyList(c["opposition"])[0])["influence"] = "none"
		claims = append(claims, c)
	}
	current := packetProofClaim("current", true)
	opposition := append([]any{}, contextPackAnyList(anyMap(claims[0])["opposition"])...)
	opposition = append(opposition, map[string]any{"ref_id": "current-counter", "kind": "temporal_claim", "status": "active", "statement": "current material contradiction"})
	current["opposition"] = opposition
	claims = append(claims, current)
	packet := buildAgentPacket(packetProofFixture(claims), map[string]any{}, "synthesis_pack_v2")
	row := anyMap(contextPackAnyList(packet["evidence"])[0])
	proof := anyMap(row["proof"])
	if proof["claim_id"] != "current" {
		t.Fatal("history displaced current contested claim")
	}
	if anyMap(contextPackAnyList(proof["opposition"])[0])["ref_id"] != "current-counter" {
		t.Fatal("history displaced current counterreference")
	}
	shrinkAgentPacketProofDetails(row)
	if anyMap(contextPackAnyList(proof["opposition"])[0])["ref_id"] != "current-counter" {
		t.Fatal("budget shrink dropped current counterreference")
	}
}

func TestAgentPacketProofLongProducerCitationKeepsRecoverableIdentity(t *testing.T) {
	finding := map[string]any{"project": "alpha", "file": "notes/" + strings.Repeat("long-source-", 16) + ".md", "source": "qdrant", "topic_path": "proof/topic", "text": "ExactSymbol::apply in v5.0.2"}
	finding["citation"] = contextPackEvidenceCitation(finding)
	ref := proofEvidenceReference(finding)
	id := anyToString(ref["ref_id"])
	if len(id) <= 160 {
		t.Fatal("long-reference fixture not long")
	}
	c := packetProofClaim("long-citation", false)
	c["support"] = []any{ref}
	packet := buildAgentPacket(packetProofFixture([]any{c}), map[string]any{}, "synthesis_pack_v2")
	rows := contextPackAnyList(packet["evidence"])
	if len(rows) != 1 {
		t.Fatal("supported long-source claim vanished")
	}
	support := anyMap(contextPackAnyList(anyMap(anyMap(rows[0])["proof"])["support"])[0])
	if support["ref_id_sha256"] != "sha256:"+sha256Hex(id) || support["file"] != finding["file"] || support["project"] != "alpha" || support["detail_omitted"] != true {
		t.Fatal("long identity not recoverable/omission unmarked")
	}
	if _, truncated := support["ref_id"]; truncated {
		t.Fatal("long identity silently truncated")
	}
	if anyMap(packet["provenance"])["proof_claim_candidates"] != 1 {
		t.Fatal("supported omission erased from coverage")
	}
}

func TestAgentPacketProofRealTemporalRolesAndCaveatsSurvive(t *testing.T) {
	temporal := temporalClaim{ClaimID: "temporal", Statement: "ExactSymbol::apply is valid only after v5.0.2.", Status: "active",
		ObservedAt: "2026-11-12T00:00:00Z", ValidFrom: "2026-11-12T00:00:00Z", ValidTo: "2026-11-13T00:00:00Z"}
	c := packetProofClaim("temporal-proof", true)
	c["opposition"] = []any{proofHistoricalTemporalClaimReference(temporal)}
	c["missing_proof"] = []any{strings.Repeat("important caveat ", 20)}
	response := packetProofFixture([]any{c})
	before, _ := json.Marshal(response)
	packet := buildAgentPacket(response, map[string]any{}, "synthesis_pack_v2")
	row := anyMap(contextPackAnyList(packet["evidence"])[0])
	proof := anyMap(row["proof"])
	ref := anyMap(contextPackAnyList(proof["opposition"])[0])
	if ref["role"] != "historical_opposition" || ref["historical"] != true || ref["influence"] != "none" || ref["valid_from"] != temporal.ValidFrom || ref["valid_to"] != temporal.ValidTo || ref["observed_at"] != temporal.ObservedAt || ref["excerpt"] != temporal.Statement || proof["detail_omitted"] != true {
		t.Fatal("real reference semantics/caveat lost")
	}
	shrinkAgentPacketProofDetails(row)
	ref = anyMap(contextPackAnyList(proof["opposition"])[0])
	if ref["role"] != "historical_opposition" || ref["valid_to"] != temporal.ValidTo || ref["influence"] != "none" {
		t.Fatal("shrink changed historical validity")
	}
	after, _ := json.Marshal(response)
	if string(before) != string(after) {
		t.Fatal("shrink mutated source proof")
	}
}

func TestAgentPacketProofDeltaReconstructsTheSameBoundedProof(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	request := frontierT2PacketRequest()
	request["project"] = "alpha"
	claims := []any{packetProofClaim("base", true)}
	for i := range 7 {
		claim := packetProofClaim(fmt.Sprintf("constraint-%d", i), false)
		claim["statement"] = fmt.Sprintf("Constraint %d: runtime v5.%d requires ExactSymbol%d::apply under source revision %d.", i, i, i, i)
		claims = append(claims, claim)
	}
	response := packetProofFixture(claims)
	base := finalizeAgentPacketWithIdentity(buildAgentPacket(response, request, "synthesis_pack_v2"), nil, request, now)
	nextResponse := cloneJSONMap(response)
	anyMap(contextPackAnyList(anyMap(nextResponse["synthesis_pack"])["proof_claims"])[0])["missing_proof"] = []any{"independent corroboration", "deployment evidence pending"}
	target := finalizeAgentPacketWithIdentity(buildAgentPacket(nextResponse, request, "synthesis_pack_v2"), anyMap(base["packet_identity"]), request, now.Add(time.Minute))
	delta, err := buildAgentPacketDelta(base, target, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !anyToBool(anyMap(delta["token_budget"])["delta_smaller_than_full"]) {
		t.Fatalf("fixture requires compact delta: %#v", delta["token_budget"])
	}
	rebuilt, err := reconstructAgentPacket(base, delta, now.Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	actualProjection, err := agentPacketTransportProjection(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	expectedProjection, err := agentPacketTransportProjection(target)
	if err != nil {
		t.Fatal(err)
	}
	if !agentPacketJSONEqual(actualProjection, expectedProjection) || !agentPacketJSONEqual(agentPacketModelVisibleProjection(rebuilt), agentPacketModelVisibleProjection(target)) {
		t.Fatal("reconstructed content/proof changed")
	}
	if reason := validateAgentPacketTransportAccounting(rebuilt); reason != "" {
		t.Fatalf("reconstructed final transport not exact: %s", reason)
	}
	if anyMap(anyMap(contextPackAnyList(rebuilt["evidence"])[0])["proof"])["status"] != "contested" {
		t.Fatal("delta lost contradiction")
	}
}

func TestAgentPacketProofUnfitReferencesForceExplicitAbstention(t *testing.T) {
	packet := buildAgentPacket(packetProofFixture([]any{packetProofClaim("unfit", true)}), map[string]any{}, "synthesis_pack_v2")
	omitUnboundedAgentPacketProof(packet, 1)
	updateAgentPacketProofCoverage(packet)
	if len(contextPackAnyList(packet["evidence"])) != 0 || anyMap(packet["decision_gate"])["decision"] != "abstain" || anyMap(packet["decision_gate"])["refusal"] != true || anyMap(packet["provenance"])["proof_claims_omitted"] != 1 {
		t.Fatal("irreducible proof loss silently permitted action")
	}
}

func TestAgentPacketProofSparseDeltaDoesNotClaimSavings(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	request := frontierT2PacketRequest()
	request["project"] = "alpha"
	response := packetProofFixture([]any{packetProofClaim("sparse", true)})
	base := finalizeAgentPacketWithIdentity(buildAgentPacket(response, request, "synthesis_pack_v2"), nil, request, now)
	changed := packetProofClaim("sparse", true)
	changed["missing_proof"] = []any{"independent corroboration", "deployment evidence pending"}
	target := finalizeAgentPacketWithIdentity(buildAgentPacket(packetProofFixture([]any{changed}), request, "synthesis_pack_v2"), anyMap(base["packet_identity"]), request, now.Add(time.Minute))
	delta, err := buildAgentPacketDelta(base, target, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if anyToBool(anyMap(delta["token_budget"])["delta_smaller_than_full"]) || anyToInt(anyMap(delta["token_budget"])["tokens_saved_exact"], 0) != 0 {
		t.Fatal("sparse proof incorrectly advertised delta savings")
	}
}
