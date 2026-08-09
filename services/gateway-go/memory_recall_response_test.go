package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func recallResponseTestInput(withEvidence bool) map[string]any {
	input := map[string]any{
		"query":            "what is the verified next action",
		"project":          "private-project",
		"topic_path":       "decision/retrieval",
		"agent_id":         "agent-alpha",
		"retrieval_intent": "decision",
		"retrieval_mode":   "impact_per_token",
		"source_coverage":  map[string]any{"complete": true, "returned": []any{"qdrant"}},
		"memory_trust_assessment": map[string]any{
			"schema_id": "memory_trust_assessment.v1", "assessment_id": "mta_" + strings.Repeat("1", 24),
		},
		"retrieval_decision_trace": map[string]any{
			"schema_id": "retrieval_decision_trace.v1", "trace_id": "rdt_" + strings.Repeat("2", 24),
		},
	}
	if withEvidence {
		input["context_pack"] = map[string]any{
			"ranked_evidence": []any{
				map[string]any{
					"candidate_id": "rtc_" + strings.Repeat("a", 24), "kind": "decision", "confidence": 0.92,
					"source": "qdrant", "content_digest": "sha256:evidence-a",
					"text": "Do not expose /private/path or super-secret material.",
				},
				map[string]any{
					"candidate_id": "rtc_" + strings.Repeat("b", 24), "kind": "check", "confidence": 0.81,
					"source": "topic_rollups", "content_digest": "sha256:evidence-b",
					"text": "Run the deterministic verification gate.",
				},
			},
		}
	}
	return input
}

func TestComposeRecallResponseIsDeterministicAndContractBounded(t *testing.T) {
	input := recallResponseTestInput(true)
	first := composeRecallResponse(input)
	second := composeRecallResponse(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("pure composer was not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}

	if got := anyToString(first["response_id"]); !strings.HasPrefix(got, "rr_") {
		t.Fatalf("response id is not opaque and stable: %q", got)
	}
	if got := anyToString(first["response_digest"]); !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("response digest is not content addressed: %q", got)
	}
	if got := anyToString(anyMap(first["classification"])["evidence_state"]); got != "clean" {
		t.Fatalf("classification did not reflect supported evidence: %#v", first["classification"])
	}
	if anyToBool(anyMap(first["action_boundary"])["can_act"]) || anyToBool(anyMap(first["action_boundary"])["execution_performed"]) {
		t.Fatalf("recall response granted or performed action: %#v", first["action_boundary"])
	}

	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	serialized := string(encoded)
	for _, leaked := range []string{"super-secret", "/private/path", "Do not expose"} {
		if strings.Contains(serialized, leaked) {
			t.Fatalf("raw retrieval content leaked into response: %q", leaked)
		}
	}
	valid := attachPayloadFormatContract(recallResponseContractID, first, "agent-alpha", "test", "/test/recall-response")
	assertBoundaryContractPassed(t, recallResponseContractID, valid)
	assertBoundaryJSONUnderLimit(t, recallResponseContractID, valid)
	if !anyToBool(anyMap(valid["format_contract"])["contract_valid"]) {
		t.Fatalf("format contract did not record a valid response: %#v", valid["format_contract"])
	}
	for _, raw := range contextPackAnyList(first["evidence"]) {
		digest := anyToString(anyMap(raw)["content_digest"])
		if !recallResponseValidDigest(digest) || digest == "sha256:evidence-a" {
			t.Fatalf("malformed caller digest was retained: %q", digest)
		}
	}
	if anyToBool(anyMap(first["outcome"])["attributable"]) {
		t.Fatal("unbound response became attributable without durable quality and selection receipts")
	}
}

func TestComposeRecallResponseAbstainsWithoutEvidenceAndAttribution(t *testing.T) {
	input := recallResponseTestInput(false)
	input["source_coverage"] = map[string]any{
		"complete": false, "pending": []any{"slow_source"}, "timed_out": []any{"archive"},
	}
	input["context_pack"] = map[string]any{"omitted_high_value_refs": []any{"opaque-ref"}}
	response := composeRecallResponse(input)
	if got := anyToString(anyMap(response["state"])["status"]); got != "abstain" {
		t.Fatalf("missing evidence did not abstain: %#v", response["state"])
	}
	classification := anyMap(response["classification"])
	if got := anyToString(classification["evidence_state"]); got != "degraded" || anyToString(classification["posture"]) != "abstain" {
		t.Fatalf("abstention classification incomplete: %#v", classification)
	}
	outcome := anyMap(response["outcome"])
	if anyToBool(outcome["attributable"]) || anyToBool(anyMap(response["action_boundary"])["can_act"]) {
		t.Fatalf("abstention became attributable or actionable: %#v", response)
	}
	valid := attachPayloadFormatContract(recallResponseContractID, response, "agent-alpha", "test", "/test/recall-response")
	assertBoundaryContractPassed(t, recallResponseContractID, valid)
}

func TestComposeRecallResponseDisclosesConflictsWithoutRawClaims(t *testing.T) {
	input := recallResponseTestInput(true)
	input["source_coverage"] = map[string]any{"complete": true}
	input["context_pack"].(map[string]any)["proof_claims"] = []any{
		map[string]any{
			"claim_id": "claim_current", "proof_status": "contested",
			"support":    []any{map[string]any{"ref_id": "rtc_" + strings.Repeat("a", 24)}},
			"opposition": []any{map[string]any{"ref_id": "claim_opposing"}},
			"statement":  "raw claim text must not be copied",
		},
	}
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["conflicts"])) != 1 {
		t.Fatalf("conflict was not surfaced: %#v", response["conflicts"])
	}
	if got := anyToString(anyMap(response["state"])["status"]); got != "verify" {
		t.Fatalf("conflict response did not require verification: %#v", response["state"])
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(encoded), "raw claim text must not be copied") {
		t.Fatal("raw proof claim text leaked into recall response")
	}
	valid := attachPayloadFormatContract(recallResponseContractID, response, "agent-alpha", "test", "/test/recall-response")
	assertBoundaryContractPassed(t, recallResponseContractID, valid)
}

func TestComposeRecallResponseQuarantinedOnlyRowsBecomeExcludedGaps(t *testing.T) {
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{
		"ranked_evidence": []any{map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("c", 24), "kind": "runbook", "status": "quarantined",
			"source": "external_note", "content_digest": "sha256:bad",
		}},
	}
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["evidence"])) != 0 {
		t.Fatalf("quarantined evidence became support: %#v", response["evidence"])
	}
	gaps := contextPackAnyList(response["gaps"])
	seenExcluded := false
	for _, raw := range gaps {
		if anyToString(anyMap(raw)["code"]) == "excluded_evidence" {
			seenExcluded = true
		}
	}
	if !seenExcluded || anyToBool(anyMap(response["outcome"])["attributable"]) {
		t.Fatalf("quarantined-only response did not fail closed: gaps=%#v outcome=%#v", gaps, response["outcome"])
	}
	valid := attachPayloadFormatContract(recallResponseContractID, response, "agent-alpha", "test", "/test/recall-response")
	assertBoundaryContractPassed(t, recallResponseContractID, valid)
}

func TestComposeRecallResponseTreatsFreshnessSeparatelyFromProofStatus(t *testing.T) {
	input := recallResponseTestInput(false)
	input["context_pack"] = map[string]any{
		"ranked_evidence": []any{
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("1", 24), "freshness": "undated", "confidence": 0.8},
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("2", 24), "freshness": "current", "confidence": 0.9},
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("3", 24), "freshness": "stale", "confidence": 0.9},
			map[string]any{"candidate_id": "rtc_" + strings.Repeat("4", 24), "freshness": "superseded", "confidence": 0.9},
		},
	}
	response := composeRecallResponse(input)
	if got := len(contextPackAnyList(response["evidence"])); got != 2 {
		t.Fatalf("ordinary context-pack freshness was confused with proof status: got=%d evidence=%#v", got, response["evidence"])
	}
	if got := anyToString(anyMap(response["classification"])["posture"]); got == "abstain" {
		t.Fatalf("eligible current/undated evidence incorrectly forced abstention: %#v", response["classification"])
	}
	gaps := contextPackAnyList(response["gaps"])
	seenExcluded := false
	for _, raw := range gaps {
		if anyToString(anyMap(raw)["code"]) == "excluded_evidence" {
			seenExcluded = true
			break
		}
	}
	if !seenExcluded {
		t.Fatalf("stale or superseded evidence was not disclosed as excluded: %#v", gaps)
	}
}

func TestComposeRecallResponseCannotBeStrengthenedByCallerClassification(t *testing.T) {
	input := recallResponseTestInput(false)
	input["classification"] = map[string]any{
		"jobs": []any{"look_up"}, "objects": []any{"durable_memory"},
		"temporal_mode": "current", "evidence_state": "clean",
		"consequence": "informational", "posture": "answer_with_proof",
	}
	response := composeRecallResponse(input)
	classification := anyMap(response["classification"])
	if anyToString(classification["evidence_state"]) != "absent" || anyToString(classification["posture"]) != "abstain" {
		t.Fatalf("caller classification overrode bounded evidence state: %#v", classification)
	}
	valid := attachPayloadFormatContract(recallResponseContractID, response, "agent-alpha", "test", "/test/recall-response")
	assertBoundaryContractPassed(t, recallResponseContractID, valid)
}

func TestComposeRecallResponseAttributionRequiresBoundDurableReceipts(t *testing.T) {
	input := recallResponseTestInput(true)
	qualityID := "cpq_" + strings.Repeat("d", 24)
	receipt := contextPackSelectionReceipt([]any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("a", 24), "kind": "decision", "rank": 1,
	}}, []any{})
	receiptID := anyToString(receipt["receipt_id"])
	input["_durable_context_pack_quality"] = map[string]any{
		"sample_id":         qualityID,
		"selection_receipt": receipt,
	}
	response := composeRecallResponse(input)
	outcome := anyMap(response["outcome"])
	if !anyToBool(outcome["attributable"]) || anyToString(outcome["receipt_id"]) != receiptID {
		t.Fatalf("bound durable receipts did not make response attributable: %#v", outcome)
	}
	refs := contextPackAnyList(response["receipt_refs"])
	if len(refs) < 2 {
		t.Fatalf("durable trust/quality refs were not emitted: %#v", refs)
	}
	valid := attachPayloadFormatContract(recallResponseContractID, response, "agent-alpha", "test", "/test/recall-response")
	assertBoundaryContractPassed(t, recallResponseContractID, valid)
}

func TestComposeRecallResponseRejectsForgedPublicReceiptAndInvalidConfidence(t *testing.T) {
	input := recallResponseTestInput(true)
	input["context_pack_quality"] = map[string]any{
		"sample_id": "cpq_" + strings.Repeat("d", 24),
		"selection_receipt": map[string]any{
			"receipt_id": "cpr_" + strings.Repeat("e", 24),
		},
	}
	rows := input["context_pack"].(map[string]any)["ranked_evidence"].([]any)
	rows[0].(map[string]any)["confidence"] = "0.99"
	rows[1].(map[string]any)["confidence"] = 2.0
	response := composeRecallResponse(input)
	if len(contextPackAnyList(response["evidence"])) != 0 {
		t.Fatalf("invalid numeric confidence entered support: %#v", response["evidence"])
	}
	if anyToBool(anyMap(response["outcome"])["attributable"]) {
		t.Fatalf("caller-visible receipt fields forged attribution: %#v", response["outcome"])
	}
	if got := anyToString(anyMap(response["classification"])["posture"]); got != "abstain" {
		t.Fatalf("invalid evidence did not force abstention: %#v", response["classification"])
	}
}

func TestComposeRecallResponseBindsScopeAndSemanticDigestBeforeTransport(t *testing.T) {
	firstInput := recallResponseTestInput(true)
	firstInput["workspace_ref"] = "workspace-alpha"
	firstInput["session_id"] = "session-alpha"
	firstInput["task_id"] = "task-alpha"
	first := composeRecallResponse(firstInput)
	semanticDigest := anyToString(first["response_digest"])
	if !recallResponseValidDigest(semanticDigest) {
		t.Fatalf("response digest is malformed: %q", semanticDigest)
	}
	attached := attachPayloadFormatContract(recallResponseContractID, first, "agent-alpha", "test", "/test/recall-response")
	if anyToString(attached["response_digest"]) != semanticDigest {
		t.Fatalf("transport attachment changed semantic digest: before=%q after=%q", semanticDigest, attached["response_digest"])
	}

	secondInput := recallResponseTestInput(true)
	secondInput["workspace_ref"] = "workspace-beta"
	secondInput["session_id"] = "session-alpha"
	secondInput["task_id"] = "task-alpha"
	second := composeRecallResponse(secondInput)
	if anyToString(first["response_id"]) == anyToString(second["response_id"]) ||
		anyToString(first["response_digest"]) == anyToString(second["response_digest"]) {
		t.Fatalf("cross-workspace responses shared identity: first=%#v second=%#v", first["request_scope"], second["request_scope"])
	}
	firstScope := anyMap(first["request_scope"])
	for _, field := range []string{"workspace_ref", "project_ref", "topic_ref", "agent_ref", "session_ref", "task_ref", "task_identity_ref", "execution_lane_ref"} {
		value := anyToString(firstScope[field])
		if !strings.HasPrefix(value, "ref_") && !recallResponseValidDigest(value) {
			t.Fatalf("scope field %s is not opaque: %q", field, value)
		}
	}
}

func TestRecallResponseContractRejectsUnexpectedTopLevelFields(t *testing.T) {
	response := composeRecallResponse(recallResponseTestInput(false))
	response["context_pack"] = map[string]any{"raw": "must not cross the response boundary"}
	attached := attachPayloadFormatContract(recallResponseContractID, response, "agent-alpha", "test", "/test/recall-response")
	if anyToBool(anyMap(attached["format_contract"])["contract_valid"]) {
		t.Fatalf("closed response schema admitted an unexpected top-level field: %#v", attached["format_contract"])
	}
}
