package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecallResponseHistoricalProjectionIgnoresFutureOnlyEvidence(t *testing.T) {
	input := recallResponseTestInput(true)
	input["as_of"] = "2026-01-01T09:30:00-07:00"
	rows := input["context_pack"].(map[string]any)["ranked_evidence"].([]any)
	rows[0].(map[string]any)["created_at"] = "2026-01-01T15:00:00Z"
	rows[1].(map[string]any)["created_at"] = "2026-01-01T16:00:00Z"
	first := composeRecallResponse(input)
	if got := anyToString(anyMap(first["request_scope"])["as_of"]); got != "2026-01-01T16:30:00Z" {
		t.Fatalf("as_of was not normalized to UTC: %q", got)
	}

	mutated := cloneJSONMap(input)
	pack := anyMap(mutated["context_pack"])
	pack["ranked_evidence"] = append(contextPackAnyList(pack["ranked_evidence"]), map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("f", 24), "kind": "fact", "confidence": 1.0,
		"created_at": "2026-01-02T00:00:00Z", "text": "future-only mutation",
	})
	second := composeRecallResponse(mutated)
	for _, key := range []string{"answer", "classification", "confidence", "conflicts", "evidence", "gaps", "state"} {
		if !reflect.DeepEqual(first[key], second[key]) {
			t.Fatalf("future-only evidence changed historical support field %s:\nfirst=%#v\nsecond=%#v", key, first[key], second[key])
		}
	}
	futureRef := "rtc_" + strings.Repeat("f", 24)
	if !recallResponseDisclosureRefs(second, "exclusion_refs")[futureRef] {
		t.Fatalf("future-only evidence was not retained as a hard temporal exclusion: %#v", second["disclosure"])
	}
	for _, raw := range contextPackAnyList(second["evidence"]) {
		if anyToString(anyMap(raw)["ref_id"]) == futureRef {
			t.Fatalf("future-only evidence became support: %#v", second["evidence"])
		}
	}
}

func TestRecallResponseProofSpineIsBoundedClosedAndIdentityBound(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	if !validateRecallResponseU2(response) {
		t.Fatalf("valid U2 projection failed explicit nested validation: %#v", response)
	}
	proof := anyMap(anyMap(response["answer"])["proof_spine"])
	if got := len(contextPackAnyList(proof["proof_refs"])); got == 0 || got > recallResponseMaxProofRefs {
		t.Fatalf("proof spine ref bound failed: %d %#v", got, proof)
	}
	scope := anyMap(response["request_scope"])
	for _, key := range []string{"temporal_premise_digest", "snapshot_digest", "receipt_digest"} {
		if !recallResponseValidDigest(anyToString(scope[key])) {
			t.Fatalf("scope identity %s is not bound: %#v", key, scope)
		}
	}
	tampered := cloneJSONMap(response)
	anyMap(anyMap(tampered["answer"])["proof_spine"])["unexpected"] = true
	if validateRecallResponseU2(tampered) {
		t.Fatal("closed nested validation admitted an unexpected proof field")
	}
	tampered = finalizeRecallResponseTransport(response, "agent-alpha", "test", "/test/recall-response")
	anyMap(anyMap(tampered["answer"])["proof_spine"])["unexpected"] = true
	found := false
	for _, finding := range validateAgentContractPayload(recallResponseContractID, tampered) {
		if anyToString(finding["reason"]) == "unexpected_nested_field" && anyToString(finding["path"]) == "answer.proof_spine.unexpected" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("shared contract validator did not enforce closed nested proof fields")
	}
}

func TestRecallResponseSnapshotRevisionChangeDegradesProjection(t *testing.T) {
	input := recallResponseTestInput(true)
	pack := input["context_pack"].(map[string]any)
	pack["snapshot_revision_start"] = "rev-a"
	pack["snapshot_revision_end"] = "rev-b"
	response := composeRecallResponse(input)
	if got := anyToString(anyMap(anyMap(response["classification"])["facets"])["evidence_state"]); got != "degraded" {
		t.Fatalf("revision change was presented as stable: %q", got)
	}
	if got := anyToString(anyMap(anyMap(response["answer"])["composition"])["coverage_status"]); got != "unsatisfied" {
		t.Fatalf("revision change kept satisfied coverage: %q", got)
	}
}

func TestRecallResponseHistoricalPremiseFailsClosedOnInvalidOrUntimedEvidence(t *testing.T) {
	t.Run("invalid and future as_of", func(t *testing.T) {
		for _, asOf := range []string{"not-a-time", time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)} {
			input := recallResponseTestInput(true)
			input["as_of"] = asOf
			response := composeRecallResponse(input)
			if len(contextPackAnyList(response["evidence"])) != 0 || anyToString(anyMap(response["classification"])["posture"]) != "abstain" {
				t.Fatalf("invalid temporal premise retained support: as_of=%q response=%#v", asOf, response)
			}
			if !recallResponseHasGap(response, "invalid_temporal_premise") {
				t.Fatalf("invalid temporal premise was not disclosed: as_of=%q gaps=%#v", asOf, response["gaps"])
			}
		}
	})

	t.Run("untimed historical evidence", func(t *testing.T) {
		input := recallResponseTestInput(true)
		input["as_of"] = "2026-01-01T16:30:00Z"
		rows := anyMap(input["context_pack"])["ranked_evidence"].([]any)
		rows[0].(map[string]any)["created_at"] = "2026-01-01T15:00:00Z"
		response := composeRecallResponse(input)
		if got := len(contextPackAnyList(response["evidence"])); got != 1 {
			t.Fatalf("historical projection did not exclude the untimed row: got=%d", got)
		}
		if !recallResponseHasGap(response, "historical_evidence_without_valid_time") {
			t.Fatalf("untimed historical evidence was not disclosed: %#v", response["gaps"])
		}
	})
}

func TestRecallResponseSnapshotAndReceiptDigestsBindFullArtifacts(t *testing.T) {
	input := recallResponseTestInput(true)
	pack := anyMap(input["context_pack"])
	pack["source_revision_vector"] = map[string]any{"source-a": "revision-1"}
	first := composeRecallResponse(input)

	changedRevision := cloneJSONMap(input)
	anyMap(changedRevision["context_pack"])["source_revision_vector"] = map[string]any{"source-a": "revision-2"}
	second := composeRecallResponse(changedRevision)
	if anyToString(anyMap(first["request_scope"])["snapshot_digest"]) == anyToString(anyMap(second["request_scope"])["snapshot_digest"]) {
		t.Fatal("different source revision vectors shared a snapshot digest")
	}

	changedReceipt := cloneJSONMap(input)
	anyMap(changedReceipt["memory_trust_assessment"])["artifact_revision"] = "revision-2"
	third := composeRecallResponse(changedReceipt)
	if anyToString(anyMap(first["request_scope"])["receipt_digest"]) == anyToString(anyMap(third["request_scope"])["receipt_digest"]) {
		t.Fatal("different receipt artifacts shared a receipt digest")
	}
}

func TestRecallResponseFinalTransportRecomputesClippedIdentity(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(true))
	response = finalizeRecallResponseTransport(response, "agent-alpha", "test", "/test/recall-response")
	if got, want := anyToString(response["response_digest"]), recallResponseSemanticDigest(response); got != want {
		t.Fatalf("post-clipping semantic digest is stale: got=%q want=%q", got, want)
	}
	if got, want := anyToString(response["response_id"]), recallResponseIDForResponse(response); got != want {
		t.Fatalf("post-clipping response id is stale: got=%q want=%q", got, want)
	}
	if !validateRecallResponseU2(response) {
		t.Fatalf("transport projection broke U2 identity: %#v", response)
	}
}

func TestRecallResponseCandidateFailureProducesValidFailClosedControl(t *testing.T) {
	prepared, asOf := recallResponsePrepareTemporalInput(recallResponseTestInput(true))
	policy := recallResponseProductionPolicyInput()
	legacy := composeRecallResponseV1Control(prepared, policy, asOf)
	fallback := recallResponseFailClosedU2Control(legacy, policy, asOf)
	if !validateRecallResponseU2(fallback) {
		t.Fatalf("fail-closed control is not a valid recall_response.v1 projection: %#v", fallback)
	}
	if anyToString(anyMap(fallback["classification"])["posture"]) != "abstain" ||
		anyToBool(anyMap(fallback["outcome"])["attributable"]) || len(contextPackAnyList(fallback["evidence"])) != 0 {
		t.Fatalf("candidate failure retained support or attribution: %#v", fallback)
	}
	if got := anyToString(anyMap(anyMap(fallback["answer"])["composition"])["fallback_reason"]); got != "candidate_projection_invalid" {
		t.Fatalf("fallback reason is not explicit: %q", got)
	}
	finalized := finalizeRecallResponseTransport(fallback, "agent-alpha", "test", "/test/recall-response")
	if findings := validateAgentContractPayload(recallResponseContractID, finalized); len(findings) != 0 {
		t.Fatalf("fail-closed control violates the public contract: %#v", findings)
	}
}

func recallResponseHasGap(response map[string]any, code string) bool {
	for _, raw := range contextPackAnyList(response["gaps"]) {
		if anyToString(anyMap(raw)["code"]) == code {
			return true
		}
	}
	return false
}
