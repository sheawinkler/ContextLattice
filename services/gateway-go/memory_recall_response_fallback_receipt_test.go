package main

import (
	"strings"
	"testing"
)

func TestRecallResponseFallbackStageReceiptIsBoundedAndOpaque(t *testing.T) {
	privateRef := "rtc_private_witness_0123456789abcdef"
	candidate := map[string]any{
		"evidence": []any{
			map[string]any{"ref_id": privateRef, "role": "context"},
		},
		"answer": map[string]any{
			"components": []any{map[string]any{"kind": "memory_to_action"}},
		},
	}
	compression := recallResponseProofCompression{
		Candidates:           []string{privateRef},
		Selected:             []string{privateRef},
		ProtectedObligations: 2,
		ProtectedWitnesses:   1,
	}
	receipt := recallResponseFallbackStageReceipt(recallResponseFallbackStageProtectedWitness, compression, candidate)
	if anyToString(receipt["schema_id"]) != recallResponseFallbackStageSchema ||
		anyToString(receipt["stage"]) != recallResponseFallbackStageProtectedWitness ||
		anyToString(receipt["status"]) != "fallback" {
		t.Fatalf("fallback receipt identity is wrong: %#v", receipt)
	}
	if strings.Contains(recallResponseCanonicalJSON(receipt), privateRef) {
		t.Fatalf("fallback receipt exposed a private witness ref: %#v", receipt)
	}
	if got := len([]byte(recallResponseCanonicalJSON(receipt))); got > 2048 {
		t.Fatalf("fallback receipt is not bounded: bytes=%d receipt=%#v", got, receipt)
	}
	digestMaterial := cloneJSONMap(receipt)
	delete(digestMaterial, "receipt_digest")
	if got, want := anyToString(receipt["receipt_digest"]), "sha256:"+sha256Hex(recallResponseCanonicalJSON(digestMaterial)); got != want {
		t.Fatalf("fallback receipt digest is not self-consistent: got=%q want=%q", got, want)
	}
	for _, stage := range []string{
		recallResponseFallbackStageCompression,
		recallResponseFallbackStageProtectedWitness,
		recallResponseFallbackStageModuleValidation,
		recallResponseFallbackStageFit,
	} {
		if got := anyToString(recallResponseFallbackStageReceipt(stage, compression, nil)["stage"]); got != stage {
			t.Fatalf("stage %q was not retained: got=%q", stage, got)
		}
	}
}

func TestRecallResponseFallbackReceiptDoesNotChangeIdentity(t *testing.T) {
	prepared, asOf := recallResponsePrepareTemporalInput(recallResponseTestInput(true))
	policy := recallResponseProductionPolicyInput()
	control := composeRecallResponseV1Control(prepared, policy, asOf)
	policy, _ = recallResponseBindArtifactIdentity(prepared, control, policy, asOf)
	fallback := recallResponseFailClosedU2Control(control, policy, asOf)
	beforeID, beforeDigest := anyToString(fallback["response_id"]), anyToString(fallback["response_digest"])
	recallResponseAttachFallbackStageReceipt(fallback, recallResponseFallbackStageReceipt(recallResponseFallbackStageModuleValidation, recallResponseProofCompression{}, control))
	if got := anyToString(fallback["response_id"]); got != beforeID {
		t.Fatalf("internal receipt changed response id: before=%q after=%q", beforeID, got)
	}
	if got := anyToString(fallback["response_digest"]); got != beforeDigest {
		t.Fatalf("internal receipt changed response digest: before=%q after=%q", beforeDigest, got)
	}
	if got, want := anyToString(fallback["response_digest"]), recallResponseSemanticDigest(fallback); got != want {
		t.Fatalf("fallback identity does not ignore internal receipt: got=%q want=%q", got, want)
	}
	finalized := finalizeRecallResponseTransport(cloneJSONMap(fallback), "agent-alpha", "test", "/test/recall-response")
	if _, ok := finalized[recallResponseFallbackStageReceiptKey]; ok {
		t.Fatalf("internal fallback receipt crossed the public transport boundary: %#v", finalized[recallResponseFallbackStageReceiptKey])
	}
	if findings := validateAgentContractPayload(recallResponseContractID, finalized); len(findings) != 0 {
		t.Fatalf("fallback with internal receipt violated the public contract: %#v", findings)
	}
}

func TestRecallResponseCompressionFallbackCarriesInternalStageReceipt(t *testing.T) {
	input := recallResponseTestInput(true)
	conflicts := make([]any, 0, 8)
	for index := 0; index < 8; index++ {
		conflicts = append(conflicts, map[string]any{
			"conflict_id": "conflict-" + anyToString(index),
			"support":     []any{}, "opposition": []any{},
		})
	}
	input["conflicts"] = conflicts
	input["source_coverage"] = map[string]any{"complete": false, "pending": []any{"archive"}}
	response := composeRecallResponse(input)
	composition := anyMap(anyMap(response["answer"])["composition"])
	if anyToString(composition["primary_module"]) != "v1_control" {
		t.Fatalf("fixture did not produce the expected fail-closed control: %#v", composition)
	}
	receipt := anyMap(response[recallResponseFallbackStageReceiptKey])
	if len(receipt) == 0 || !recallResponseFallbackStages[anyToString(receipt["stage"])] {
		t.Fatalf("compression fallback did not retain an internal stage receipt: %#v", response)
	}
	if strings.Contains(recallResponseCanonicalJSON(receipt), "conflict-") {
		t.Fatalf("compression fallback receipt exposed conflict data: %#v", receipt)
	}
	finalized := finalizeRecallResponseTransport(cloneJSONMap(response), "agent-alpha", "test", "/test/recall-response")
	if _, ok := finalized[recallResponseFallbackStageReceiptKey]; ok {
		t.Fatal("compression fallback receipt crossed the public boundary")
	}
}

func TestRecallResponseFinalizerStripsReceiptAttachedDuringRecomposition(t *testing.T) {
	candidate := composeRecallResponse(recallResponseTestInput(true))
	answer := anyMap(candidate["answer"])
	composition := anyMap(answer["composition"])
	if anyToString(composition["primary_module"]) == "v1_control" {
		t.Fatalf("test input did not produce a candidate response: %#v", composition)
	}
	// Simulate a boundary clip that removes the component list after candidate
	// composition but before identity recomputation. The recomposer must
	// fail closed and attach a diagnostic internally.
	answer["components"] = []any{}
	candidate["response_id"] = recallResponseIDForResponse(candidate)
	candidate["response_digest"] = recallResponseSemanticDigest(candidate)
	staged := attachPayloadFormatContract(recallResponseContractID, cloneJSONMap(candidate), "agent-alpha", "test", "/test/recall-response")
	if !recallResponseRecomputeClippedIdentity(staged) {
		t.Fatal("transport-induced invalid component projection did not trigger recomposition fallback")
	}
	if _, ok := staged[recallResponseFallbackStageReceiptKey]; !ok {
		t.Fatal("recomposition fallback did not attach its internal stage receipt")
	}
	finalized := finalizeRecallResponseTransport(candidate, "agent-alpha", "test", "/test/recall-response")
	if _, ok := finalized[recallResponseFallbackStageReceiptKey]; ok {
		t.Fatalf("receipt attached during recomposition escaped finalizer: %#v", finalized[recallResponseFallbackStageReceiptKey])
	}
	if findings := validateAgentContractPayload(recallResponseContractID, finalized); len(findings) != 0 {
		t.Fatalf("finalized recomposition fallback violated public contract: %#v", findings)
	}
}
