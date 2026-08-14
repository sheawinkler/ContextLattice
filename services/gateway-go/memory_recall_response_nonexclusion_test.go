package main

import (
	"fmt"
	"strings"
	"testing"
)

func recallResponseDisclosureRefs(response map[string]any, field string) map[string]bool {
	refs := map[string]bool{}
	for _, raw := range contextPackAnyList(anyMap(response["disclosure"])[field]) {
		row := anyMap(raw)
		ref := strings.TrimSpace(anyToString(row["ref_id"]))
		if ref == "" {
			ref = strings.TrimSpace(anyToString(raw))
		}
		if ref != "" {
			refs[ref] = true
		}
	}
	return refs
}

func TestRecallResponseNonExclusionForcedMisclassificationKeepsEvidenceAndProofUnion(t *testing.T) {
	base := recallResponseTestInput(true)
	neutral := composeRecallResponse(base)
	forcedInput := cloneJSONMap(base)
	forcedInput["classification"] = map[string]any{
		"jobs": []any{"act"}, "objects": []any{"negative"}, "temporal_mode": "deadline",
		"evidence_state": "clean", "consequence": "informational", "posture": "answer_with_proof",
		"facets": map[string]any{"jobs": []any{"act"}, "memory_objects": []any{"negative"}, "temporal_state": "deadline", "evidence_state": "clean", "consequence": "informational"},
	}
	forced := composeRecallResponse(forcedInput)
	neutralEvidence := recallResponseDisclosureRefs(neutral, "evidence_union")
	forcedEvidence := recallResponseDisclosureRefs(forced, "evidence_union")
	if len(neutralEvidence) == 0 || len(neutralEvidence) != len(forcedEvidence) {
		t.Fatalf("forced taxonomy changed authoritative evidence union size: neutral=%#v forced=%#v", neutralEvidence, forcedEvidence)
	}
	for ref := range neutralEvidence {
		if !forcedEvidence[ref] {
			t.Fatalf("forced taxonomy silently removed evidence identity %q: neutral=%#v forced=%#v", ref, neutralEvidence, forcedEvidence)
		}
	}
	neutralProof := recallResponseDisclosureRefs(neutral, "proof_union")
	forcedProof := recallResponseDisclosureRefs(forced, "proof_union")
	for _, response := range []map[string]any{neutral, forced} {
		for _, raw := range contextPackAnyList(anyMap(anyMap(response["answer"])["proof_spine"])["proof_refs"]) {
			if !neutralProof[anyToString(raw)] || !forcedProof[anyToString(raw)] {
				t.Fatalf("proof identity was not retained in the same-snapshot proof union: response=%#v", response)
			}
		}
	}
	if !validateRecallResponseU2(neutral) || !validateRecallResponseU2(forced) {
		t.Fatalf("forced-misclassification response violated the non-exclusion contract: neutral=%#v forced=%#v", neutral["disclosure"], forced["disclosure"])
	}
}

func TestRecallResponseNonExclusionCanonicalAliasesDoNotDuplicateOrReplaceRows(t *testing.T) {
	rowA := map[string]any{"candidate_id": "rtc_" + strings.Repeat("a", 24), "confidence": 0.9, "status": "current"}
	rowB := map[string]any{"candidate_id": "rtc_" + strings.Repeat("b", 24), "confidence": 0.8, "status": "current"}
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{
		"ranked_evidence": []any{rowA, cloneJSONMap(rowA), rowB},
		"rankedEvidence":  []any{map[string]any{"candidate_id": "rtc_" + strings.Repeat("c", 24), "confidence": 0.7}},
	}
	input["contextPack"] = map[string]any{
		"ranked_evidence": []any{map[string]any{"candidate_id": "rtc_" + strings.Repeat("d", 24), "confidence": 0.7}},
	}
	response := composeRecallResponse(input)
	disclosure := anyMap(response["disclosure"])
	if got := anyToInt(anyMap(disclosure["source_counts"])["evidence"], -1); got != 2 {
		t.Fatalf("canonical aliases were merged or duplicate rows were counted: got=%d disclosure=%#v", got, disclosure)
	}
	refs := recallResponseDisclosureRefs(response, "evidence_union")
	if !refs[anyToString(rowA["candidate_id"])] || !refs[anyToString(rowB["candidate_id"])] || len(refs) != 2 {
		t.Fatalf("canonical snake-case source rows were not retained exactly once: %#v", refs)
	}
}

func TestRecallResponseNonExclusionHybridRetainsTemporalAndExclusionReceipts(t *testing.T) {
	input := recallResponseTestInput(false)
	input["query"] = "what happened over time, what was decided, and what is the next action"
	input["as_of"] = "2026-01-01T00:00:00Z"
	input["context_pack"] = map[string]any{
		"ranked_evidence": []any{
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("1", 24), "confidence": 0.91, "status": "current", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("a", 64), "occurred_at": "2025-12-01T00:00:00Z"},
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("2", 24), "confidence": 0.92, "freshness": "superseded", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("b", 64), "occurred_at": "2025-12-01T00:00:00Z"},
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("3", 24), "confidence": 0.95, "status": "current", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("c", 64), "occurred_at": "2027-01-01T00:00:00Z"},
		},
		"temporal_claims": []any{map[string]any{"candidate_id": "rtc_" + strings.Repeat("4", 24), "occurred_at": "2027-02-01T00:00:00Z"}},
	}
	response := composeRecallResponse(input)
	evidence := recallResponseDisclosureRefs(response, "evidence_union")
	if !evidence["rtc_"+strings.Repeat("1", 24)] || evidence["rtc_"+strings.Repeat("2", 24)] || evidence["rtc_"+strings.Repeat("3", 24)] {
		t.Fatalf("temporal/retirement enforcement was bypassed by union projection: %#v", response["disclosure"])
	}
	exclusions := recallResponseDisclosureRefs(response, "exclusion_refs")
	for _, ref := range []string{"rtc_" + strings.Repeat("2", 24), "rtc_" + strings.Repeat("3", 24), "rtc_" + strings.Repeat("4", 24)} {
		if !exclusions[ref] {
			t.Fatalf("excluded temporal/history identity was not retained as an opaque receipt: want=%s disclosure=%#v", ref, response["disclosure"])
		}
	}
	if !recallResponseContinuationActionValid(response, anyMap(anyMap(response["disclosure"])["continuation_action"])) || !validateRecallResponseU2(response) {
		t.Fatalf("hybrid temporal response did not preserve bounded non-exclusion metadata: %#v", response["disclosure"])
	}
}

func TestRecallResponseNonExclusionLedgerRequiresRealCounterfactual(t *testing.T) {
	input := recallResponseTestInput(true)
	pack := anyMap(input["context_pack"])
	ranked := contextPackAnyList(pack["ranked_evidence"])
	for index, raw := range ranked {
		anyMap(raw)["content_digest"] = "sha256:" + strings.Repeat(string(rune('a'+index)), 64)
	}
	input["context_pack"] = recallResponseServerOwnedSourcePack(pack, ranked)
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("a", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("b", 64)
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(input, "validated_policy", nil)
	response := composeRecallResponseWithPolicy(input, policy)
	disclosure := anyMap(response["disclosure"])
	if !anyToBool(anyMap(disclosure["control_receipt"])["source_bound"]) {
		t.Fatal("explicit source-bound policy did not produce an authoritative control receipt")
	}
	row := contextPackAnyList(response["evidence"])[len(contextPackAnyList(response["evidence"]))-1]
	refID := anyToString(anyMap(row)["ref_id"])
	recallResponseRecordOmission(response, refID, "evidence", "test_context_budget", false, "no_loss_verified")
	if !validateRecallResponseU2(response) {
		t.Fatalf("real same-snapshot omission receipt was rejected: %#v", disclosure["omission_ledger"])
	}
	ledger := contextPackAnyList(disclosure["omission_ledger"])
	anyMap(anyMap(ledger[0])["same_snapshot_counterfactual"])["verified"] = false
	if validateRecallResponseU2(response) {
		t.Fatal("unverified omission was accepted by the non-exclusion contract")
	}
}

func TestRecallResponseNonExclusionMissingDigestStaysUnbound(t *testing.T) {
	input := recallResponseTestInput(true)
	ranked := contextPackAnyList(anyMap(input["context_pack"])["ranked_evidence"])
	delete(anyMap(ranked[0]), "content_digest")
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("c", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("d", 64)
	response := composeRecallResponseWithPolicy(input, policy)
	rows := contextPackAnyList(anyMap(response["disclosure"])["evidence_union"])
	if len(rows) == 0 {
		t.Fatal("missing-digest input lost the eligible evidence union")
	}
	if anyToBool(anyMap(anyMap(rows[0])["evidence_binding"])["source_bound"]) {
		t.Fatal("missing original content digest became source-bound")
	}
	recallResponseRecordOmission(response, anyToString(anyMap(rows[0])["ref_id"]), "evidence", "test_context_budget", false, "no_loss_verified")
	ledger := contextPackAnyList(anyMap(response["disclosure"])["omission_ledger"])
	if anyToBool(anyMap(anyMap(ledger[0])["same_snapshot_counterfactual"])["verified"]) {
		t.Fatal("missing-digest evidence received a verified no-loss counterfactual")
	}
}

func TestRecallResponseNonExclusionUnboundCounterfactualCannotVerify(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	row := contextPackAnyList(response["evidence"])[len(contextPackAnyList(response["evidence"]))-1]
	recallResponseRecordOmission(response, anyToString(anyMap(row)["ref_id"]), "evidence", "test_context_budget", false, "no_loss_verified")
	ledger := contextPackAnyList(anyMap(response["disclosure"])["omission_ledger"])
	if len(ledger) == 0 || anyToBool(anyMap(anyMap(ledger[0])["same_snapshot_counterfactual"])["verified"]) {
		t.Fatal("self-derived snapshot/receipt was treated as an authoritative counterfactual")
	}
	if validateRecallResponseU2(response) {
		t.Fatal("unbound no-loss omission was accepted by the non-exclusion contract")
	}
}

func TestRecallResponseNonExclusionValidDigestsDoNotInferSourceAuthority(t *testing.T) {
	policy := recallResponseProductionPolicyInput()
	policy.snapshotDigest = "sha256:" + strings.Repeat("a", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("b", 64)
	policy.sourceBound = false
	response := composeRecallResponseWithPolicy(recallResponseTestInput(true), policy)
	receipt := anyMap(anyMap(response["disclosure"])["control_receipt"])
	if anyToBool(receipt["source_bound"]) || anyToBool(anyMap(response["request_scope"])["source_bound"]) {
		t.Fatalf("valid-looking local digests inferred source authority: scope=%#v receipt=%#v", response["request_scope"], receipt)
	}
}

func TestRecallResponseNonExclusionForgedFormattedEvidenceNeedsValidatedBinding(t *testing.T) {
	input := recallResponseTestInput(false)
	row := map[string]any{
		"candidate_id":        "rtc_" + strings.Repeat("a", 24),
		"source":              "qdrant",
		"content_digest":      "sha256:" + strings.Repeat("b", 64),
		"confidence":          0.99,
		"status":              "current",
		"required_for_action": true,
	}
	input["context_pack"] = map[string]any{"ranked_evidence": []any{row}}
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("c", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("d", 64)

	unbound := composeRecallResponseWithPolicy(input, policy)
	unboundRows := contextPackAnyList(anyMap(unbound["disclosure"])["evidence_union"])
	if len(unboundRows) != 1 {
		t.Fatalf("forged formatted row disappeared instead of remaining derived: %#v", unbound["disclosure"])
	}
	unboundBinding := anyMap(anyMap(unboundRows[0])["evidence_binding"])
	if anyToBool(unboundBinding["source_bound"]) || anyToString(unboundBinding["binding_authority"]) != "derived" || anyToBool(anyMap(unboundRows[0])["protected"]) {
		t.Fatalf("formatted caller fields asserted evidence authority: row=%#v", unboundRows[0])
	}

	boundInput := cloneJSONMap(input)
	boundPack := anyMap(boundInput["context_pack"])
	boundSourceRows := contextPackAnyList(boundPack["ranked_evidence"])
	boundInput["context_pack"] = recallResponseServerOwnedSourcePack(boundPack, boundSourceRows)
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(boundInput, "validated_policy", nil)
	bound := composeRecallResponseWithPolicy(boundInput, policy)
	boundRows := contextPackAnyList(anyMap(bound["disclosure"])["evidence_union"])
	boundBinding := anyMap(anyMap(boundRows[0])["evidence_binding"])
	if !anyToBool(boundBinding["source_bound"]) || anyToString(boundBinding["binding_authority"]) != "validated_policy" || !anyToBool(anyMap(boundRows[0])["protected"]) {
		t.Fatalf("typed validated binding was not threaded into projection: row=%#v", boundRows[0])
	}

	forged := cloneJSONMap(boundInput)
	forgedPack := anyMap(forged["context_pack"])
	anyMap(contextPackAnyList(forgedPack["ranked_evidence"])[0])["content_digest"] = "sha256:" + strings.Repeat("e", 64)
	anyMap(contextPackAnyList(forgedPack["_recall_response_source_rows"])[0])["content_digest"] = "sha256:" + strings.Repeat("e", 64)
	mismatched := composeRecallResponseWithPolicy(forged, policy)
	mismatchedBinding := anyMap(anyMap(contextPackAnyList(anyMap(mismatched["disclosure"])["evidence_union"])[0])["evidence_binding"])
	if anyToBool(mismatchedBinding["source_bound"]) || anyToString(mismatchedBinding["binding_authority"]) != "derived" {
		t.Fatalf("valid-looking digest outside the typed binding was upgraded: %#v", mismatchedBinding)
	}
}

func TestRecallResponseNonExclusionHighConfidenceAloneIsSheddable(t *testing.T) {
	item := map[string]any{
		"candidate_id":   "rtc_" + strings.Repeat("a", 24),
		"source":         "qdrant",
		"content_digest": "sha256:" + strings.Repeat("a", 64),
		"confidence":     0.99,
		"status":         "current",
	}
	status, eligible := recallResponseEvidenceStatus(item)
	confidence, confidenceValid := recallResponseEvidenceConfidence(item["confidence"])
	if recallResponseEvidenceProtected(item, status, eligible, confidence, confidenceValid, false) {
		t.Fatal("confidence and ordinary bindings alone made evidence unsheddable")
	}
}

func TestRecallResponseNonExclusionValidatedProtectedRowSurvivesPresentationPruning(t *testing.T) {
	input := recallResponseTestInput(false)
	rows := make([]any, 0, 5)
	protectedID := "rtc_" + strings.Repeat("f", 24)
	for index := 0; index < 5; index++ {
		candidateID := fmt.Sprintf("rtc_%024x", index+1)
		required := false
		if index == 4 {
			candidateID = protectedID
			required = true
		}
		rows = append(rows, map[string]any{
			"candidate_id":        candidateID,
			"confidence":          0.8,
			"status":              "current",
			"support":             "context",
			"source":              "qdrant",
			"content_digest":      fmt.Sprintf("sha256:%064x", index+1),
			"required_for_action": required,
		})
	}
	input["context_pack"] = recallResponseServerOwnedSourcePack(
		map[string]any{"ranked_evidence": rows}, rows,
	)
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("a", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("b", 64)
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(input, "validated_policy", nil)
	response := composeRecallResponseWithPolicy(input, policy)
	union := recallResponseDisclosureRefs(response, "evidence_union")
	if !union[protectedID] || !recallResponseNonExclusionProtected(response, protectedID) {
		t.Fatalf("validated protected row was clipped from the bounded union: %#v", response["disclosure"])
	}
	if !recallResponsePruneLowestUnprovedEvidence(response, map[string]any{"proof_refs": []any{protectedID}}) {
		t.Fatal("presentation pruning did not find an ordinary context row")
	}
	for _, raw := range contextPackAnyList(response["evidence"]) {
		if anyToString(anyMap(raw)["ref_id"]) == protectedID {
			return
		}
	}
	t.Fatalf("presentation pruning removed the validated protected row: %#v", response["evidence"])
}

func TestRecallResponseNonExclusionUnsafeRowsOnlyHaveOpaqueExclusions(t *testing.T) {
	input := recallResponseTestInput(false)
	input["as_of"] = "2026-01-01T00:00:00Z"
	badRows := []any{
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("1", 24), "confidence": 0.95, "status": "revoked", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("1", 64)},
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("2", 24), "confidence": 0.95, "status": "retracted", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("2", 64)},
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("3", 24), "confidence": 0.95, "status": "expired", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("3", 64)},
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("4", 24), "confidence": 0.95, "status": "current", "source": "qdrant", "content_digest": "sha256:" + strings.Repeat("4", 64), "occurred_at": "2027-01-01T00:00:00Z"},
	}
	input["context_pack"] = map[string]any{"ranked_evidence": badRows}
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["evidence"])) != 0 || len(contextPackAnyList(anyMap(response["disclosure"])["evidence_union"])) != 0 {
		t.Fatalf("unsafe rows became top-level or union support: response=%#v", response)
	}
	exclusions := recallResponseDisclosureRefs(response, "exclusion_refs")
	for _, row := range badRows {
		ref := anyToString(anyMap(row)["candidate_id"])
		if !exclusions[ref] {
			t.Fatalf("unsafe row was not retained as an opaque exclusion: %s disclosure=%#v", ref, response["disclosure"])
		}
	}
}

func TestRecallResponseNonExclusionSourceCaptureAndContinuationAreBounded(t *testing.T) {
	input := recallResponseTestInput(false)
	rows := make([]any, 0, recallResponseMaxSourceCapture+1)
	for index := 0; index < recallResponseMaxSourceCapture+1; index++ {
		rows = append(rows, map[string]any{
			"candidate_id":   fmt.Sprintf("rtc_%024x", index+1),
			"confidence":     0.8,
			"status":         "current",
			"source":         "qdrant",
			"content_digest": fmt.Sprintf("sha256:%064x", index+1),
		})
	}
	input["context_pack"] = map[string]any{"ranked_evidence": rows}
	response := composeRecallResponse(input)
	disclosure := anyMap(response["disclosure"])
	if anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0) != recallResponseMaxSourceCapture+1 || !anyToBool(disclosure["source_truncated"]) || !anyToBool(disclosure["union_truncated"]) {
		t.Fatalf("source capture did not expose bounded truncation: %#v", disclosure)
	}
	if anyToInt(anyMap(disclosure["omitted_counts"])["source"], 0) == 0 || !recallResponseExactOpaqueID(anyToString(disclosure["continuation_ref"]), "ref_continuation_") {
		t.Fatalf("source continuation accounting is missing: %#v", disclosure)
	}
	if !recallResponseValidDigest(anyToString(disclosure["continuation_digest"])) || !recallResponseValidDigest(anyToString(disclosure["union_digest"])) {
		t.Fatalf("source truncation was not closed by bounded membership digests: %#v", disclosure)
	}
}

func TestRecallResponseNonExclusionMixedLargeUnionCountsAndContinuationAction(t *testing.T) {
	input := recallResponseTestInput(false)
	input["as_of"] = "2026-01-01T00:00:00Z"
	makeEvidence := func(offset, count int, kind string) []any {
		rows := make([]any, 0, count)
		for index := 0; index < count; index++ {
			rows = append(rows, map[string]any{
				"candidate_id":   fmt.Sprintf("rtc_%024x", offset+index+1),
				"kind":           kind,
				"confidence":     0.8,
				"status":         "current",
				"source":         "qdrant",
				"content_digest": fmt.Sprintf("sha256:%064x", offset+index+1),
				"occurred_at":    "2025-12-01T00:00:00Z",
			})
		}
		return rows
	}
	conflicts := make([]any, 0, 24)
	for index := 0; index < 24; index++ {
		conflicts = append(conflicts, map[string]any{
			"candidate_id": fmt.Sprintf("rtc_%024x", 109+index),
			"kind":         "contradiction",
			"statement":    fmt.Sprintf("conflict-%d", index+1),
		})
	}
	input["context_pack"] = map[string]any{
		"ranked_evidence": makeEvidence(0, 36, "fact"),
		"temporal_claims": makeEvidence(36, 36, "temporal_claim"),
		"proof_claims":    makeEvidence(72, 36, "proof_claim"),
		"conflicts":       conflicts,
	}
	response := composeRecallResponse(input)
	disclosure := anyMap(response["disclosure"])
	sourceCounts := anyMap(disclosure["source_counts"])
	for class, want := range map[string]int{"evidence": 36, "temporal": 36, "proof": 36, "conflicts": 24} {
		if got := anyToInt(sourceCounts[class], -1); got != want {
			t.Fatalf("mixed source class %s disappeared: got=%d want=%d disclosure=%#v", class, got, want, disclosure)
		}
	}
	unionCounts := anyMap(disclosure["union_counts"])
	omittedCounts := anyMap(disclosure["omitted_counts"])
	visibleByClass := map[string]int{
		"evidence":   len(contextPackAnyList(disclosure["evidence_union"])),
		"exclusions": len(contextPackAnyList(disclosure["exclusion_refs"])),
		"proof":      len(contextPackAnyList(disclosure["proof_union"])),
		"components": len(contextPackAnyList(disclosure["component_union"])),
	}
	for class, visible := range visibleByClass {
		if got, want := anyToInt(unionCounts[class], -1), visible+anyToInt(omittedCounts[class], -1); got != want {
			t.Fatalf("union count did not reconcile for %s: got=%d want=%d disclosure=%#v", class, got, want, disclosure)
		}
	}
	if anyToInt(unionCounts["evidence"], 0) != 108 || anyToInt(unionCounts["proof"], 0) <= 128 || !anyToBool(disclosure["union_truncated"]) {
		t.Fatalf("mixed >128 union was not completely accounted: %#v", disclosure)
	}
	if !recallResponseContinuationActionValid(response, anyMap(disclosure["continuation_action"])) || anyToString(anyMap(disclosure["continuation_action"])["snapshot_semantics"]) != "not_served" {
		t.Fatalf("pure projection claimed a continuation its owner had not served: %#v", disclosure["continuation_action"])
	}
	if !validateRecallResponseNonExclusion(response) || !validateRecallResponseU2(response) {
		t.Fatalf("mixed >128 pre-transport response was invalid: non_exclusion=%t u2=%t control=%t facets=%#v proof=%#v composition=%#v scope=%#v components=%#v", validateRecallResponseNonExclusion(response), validateRecallResponseU2(response), recallResponseIsV1Control(response), anyMap(response["classification"])["facets"], anyMap(anyMap(response["answer"])["proof_spine"]), anyMap(anyMap(response["answer"])["composition"]), response["request_scope"], anyMap(response["answer"])["components"])
	}
	finalized := finalizeRecallResponseTransport(response, "large-union-test", "test", memoryRecallResponsePath)
	assertBoundaryContractPassed(t, recallResponseContractID, finalized)
	assertBoundaryJSONUnderLimit(t, recallResponseContractID, finalized)
	compactBytes, compactTokens := recallResponseCompactBudget(finalized)
	if compactBytes > recallResponseMaxCompactBytes || compactTokens > recallResponseMaxCompactTokens {
		t.Fatalf("mixed >128 response exceeded the bounded fallback: bytes=%d tokens=%d", compactBytes, compactTokens)
	}
}

func TestRecallResponseNonExclusionCombinedUnionClipIsLedgered(t *testing.T) {
	input := recallResponseTestInput(false)
	rows := make([]any, 0, recallResponseMaxUnionRefs+1)
	for index := 0; index < 30; index++ {
		rows = append(rows, map[string]any{
			"candidate_id":   fmt.Sprintf("rtc_%024x", index+1),
			"confidence":     0.8,
			"status":         "current",
			"source":         "qdrant",
			"content_digest": fmt.Sprintf("sha256:%064x", index+1),
		})
	}
	for index := 30; index < 33; index++ {
		rows = append(rows, map[string]any{
			"candidate_id":   fmt.Sprintf("rtc_%024x", index+1),
			"confidence":     0.8,
			"status":         "revoked",
			"source":         "qdrant",
			"content_digest": fmt.Sprintf("sha256:%064x", index+1),
		})
	}
	input["context_pack"] = map[string]any{"ranked_evidence": rows}
	response := composeRecallResponse(input)
	disclosure := anyMap(response["disclosure"])
	if !anyToBool(disclosure["union_truncated"]) || anyToInt(anyMap(disclosure["omitted_counts"])["proof"], 0) == 0 {
		t.Fatalf("combined eligible+excluded cap was not accounted: %#v", disclosure)
	}
	if !recallResponseValidDigest(anyToString(disclosure["continuation_digest"])) ||
		anyToInt(anyMap(disclosure["union_counts"])["exclusions"], 0) < len(contextPackAnyList(disclosure["exclusion_refs"])) {
		t.Fatalf("combined exclusion clip was not closed by counts and continuation: %#v", disclosure)
	}
}

func TestRecallResponseNonExclusionModuleCapHasContinuation(t *testing.T) {
	components := make([]any, 0, recallResponseMaxModules+1)
	for index := 0; index < recallResponseMaxModules+1; index++ {
		components = append(components, map[string]any{
			"component_ref": fmt.Sprintf("ref_component_%024x", index+1),
			"kind":          "fact",
			"proof_refs":    []any{},
		})
	}
	accounting := recallResponseComponentUnionRowsWithAccounting(components)
	if !accounting.truncated || len(accounting.rows) != recallResponseMaxModules || len(accounting.clippedRefs) == 0 || !recallResponseValidDigest(accounting.continuation) {
		t.Fatalf("module cap was not bounded/accounted: %#v", accounting)
	}
}

func TestRecallResponseNonExclusionSpoofedControlReceiptFailsClosed(t *testing.T) {
	input := recallResponseTestInput(true)
	rows := contextPackAnyList(anyMap(input["context_pack"])["ranked_evidence"])
	for index, raw := range rows {
		anyMap(raw)["content_digest"] = fmt.Sprintf("sha256:%064x", index+1)
	}
	input["context_pack"] = recallResponseServerOwnedSourcePack(anyMap(input["context_pack"]), rows)
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("a", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("b", 64)
	response := composeRecallResponseWithPolicy(input, policy)
	receipt := anyMap(anyMap(response["disclosure"])["control_receipt"])
	if !anyToBool(receipt["source_bound"]) || !validateRecallResponseU2(response) {
		t.Fatalf("spoof fixture did not start from an authoritative valid receipt: %#v", receipt)
	}
	delete(receipt, "source_bound")
	if validateRecallResponseU2(response) {
		t.Fatal("control receipt missing authority field was accepted")
	}
	response = composeRecallResponseWithPolicy(input, policy)
	receipt = anyMap(anyMap(response["disclosure"])["control_receipt"])
	receipt["snapshot_digest"] = "sha256:" + strings.Repeat("c", 64)
	if validateRecallResponseU2(response) {
		t.Fatal("wrong-snapshot control receipt was accepted")
	}
}

func TestRecallResponseNonExclusionBudgetClipKeepsUnionContinuationBinding(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	disclosure := anyMap(response["disclosure"])
	unionDigest := anyToString(disclosure["union_digest"])
	continuation := anyToString(disclosure["continuation_digest"])
	proof := anyMap(anyMap(response["answer"])["proof_spine"])
	evidence := contextPackAnyList(response["evidence"])
	if len(evidence) == 0 {
		t.Fatal("fixture did not produce evidence for budget clip")
	}
	evidence = append(evidence, map[string]any{
		"ref_id": "ref_context_budget_000000000000000000000001", "kind": "fact", "role": "context", "status": "selected", "confidence": 0.5,
		"source_ref": "ref_source_budget_000000000000000000000001", "content_digest": "sha256:" + strings.Repeat("f", 64),
	})
	response["evidence"] = evidence
	if !recallResponsePruneLowestUnprovedEvidence(response, proof) {
		t.Fatal("budget fixture did not prune optional context")
	}
	if anyToString(disclosure["union_digest"]) != unionDigest || anyToString(disclosure["continuation_digest"]) != continuation {
		t.Fatal("budget pruning changed authoritative union or continuation identity")
	}
	ledger := contextPackAnyList(disclosure["omission_ledger"])
	if len(ledger) == 0 || !recallResponseOmissionBindingValid(disclosure, anyMap(ledger[len(ledger)-1])) {
		t.Fatalf("budget omission was not continuation-bound: %#v", ledger)
	}
}

func TestRecallResponsePresentationClassClosureRequiresOwnerCursor(t *testing.T) {
	disclosure := map[string]any{
		"union_truncated":     true,
		"continuation_digest": "sha256:" + strings.Repeat("a", 64),
		"omitted_counts":      map[string]any{"evidence": 1, "components": 1},
		"continuation_action": map[string]any{"kind": "owner_cursor_unavailable"},
	}
	if recallResponsePresentationClassClosedByContinuation(disclosure, "evidence") ||
		recallResponsePresentationClassClosedByContinuation(disclosure, "components") {
		t.Fatal("an unavailable owner cursor closed presentation classes")
	}
	disclosure["continuation_action"] = map[string]any{"kind": "continue_snapshot"}
	if !recallResponsePresentationClassClosedByContinuation(disclosure, "evidence") ||
		!recallResponsePresentationClassClosedByContinuation(disclosure, "components") {
		t.Fatal("a valid same-snapshot owner cursor did not close omitted presentation classes")
	}
	if recallResponsePresentationClassClosedByContinuation(disclosure, "proof") {
		t.Fatal("a class with no omitted rows was closed")
	}
}
