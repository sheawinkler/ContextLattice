package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const recallResponseFuzzSensitiveValue = "fuzz-private-secret-value"

func boundedRecallResponseFuzzString(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func recallResponseFuzzInput(query, evidenceText, asOf string, rowCount uint8) map[string]any {
	input := recallResponseTestInput(false)
	input["query"] = boundedRecallResponseFuzzString(query, 4096)
	input["as_of"] = boundedRecallResponseFuzzString(asOf, 128)
	rows := make([]any, 0, int(rowCount%32))
	kinds := []string{"fact", "decision", "check", "runbook", "contradiction", "temporal_claim", "graph_neighbor"}
	statuses := []string{"selected", "superseded", "quarantined", "unknown"}
	for index := 0; index < int(rowCount%32); index++ {
		row := map[string]any{
			"candidate_id":   "rtc_" + sha256Hex(fmt.Sprintf("%d\x00%s", index, evidenceText))[:24],
			"kind":           kinds[index%len(kinds)],
			"status":         statuses[index%len(statuses)],
			"confidence":     float64(index%11) / 10,
			"source":         "fuzz-source",
			"content_digest": "sha256:" + sha256Hex(fmt.Sprintf("content-%d", index)),
			"observed_at":    "2026-01-01T00:00:00Z",
			"text":           recallResponseFuzzSensitiveValue + boundedRecallResponseFuzzString(evidenceText, 4096),
			"raw_prompt":     recallResponseFuzzSensitiveValue,
			"nested": map[string]any{
				"credential": recallResponseFuzzSensitiveValue,
				"oracle":     boundedRecallResponseFuzzString(evidenceText, 256),
			},
		}
		if index%7 == 0 {
			row["recall_metadata"] = map[string]any{
				"action_evidence": map[string]any{
					"tool_ref": boundedRecallResponseFuzzString(evidenceText, 256),
					"parameter_bindings": []any{map[string]any{
						"parameter_ref": boundedRecallResponseFuzzString(evidenceText, 256),
						"required":      true,
						"value":         recallResponseFuzzSensitiveValue,
					}},
					"ordered_steps":      []any{map[string]any{"step_ref": boundedRecallResponseFuzzString(evidenceText, 256)}},
					"refusal_conditions": []any{"credential_access", boundedRecallResponseFuzzString(evidenceText, 128)},
				},
			}
		}
		rows = append(rows, row)
	}
	input["context_pack"] = map[string]any{"ranked_evidence": rows}
	return input
}

func recallResponseFuzzHasForbiddenKey(value any) bool {
	forbidden := map[string]bool{
		"credential": true, "expected": true, "oracle": true,
		"password": true, "raw_prompt": true, "secret": true,
	}
	var walk func(any) bool
	walk = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[strings.ToLower(strings.TrimSpace(key))] || walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

func assertRecallResponseFuzzInvariants(t *testing.T, response map[string]any) {
	t.Helper()
	if !validateRecallResponseU2(response) {
		t.Fatalf("response failed nested validation: %#v", response)
	}
	if findings := validateAgentContractPayload(recallResponseContractID, response); len(findings) != 0 {
		t.Fatalf("response failed generated contract validation: %#v", findings)
	}
	assertBoundaryContractPassed(t, recallResponseContractID, response)
	assertBoundaryJSONUnderLimit(t, recallResponseContractID, response)
	if anyToString(response["response_id"]) != recallResponseIDForResponse(response) ||
		anyToString(response["response_digest"]) != recallResponseSemanticDigest(response) {
		t.Fatal("response identity was not recomputed from the transported artifact")
	}
	answer := anyMap(response["answer"])
	proof := anyMap(answer["proof_spine"])
	if len(contextPackAnyList(response["evidence"])) > recallResponseMaxEvidence ||
		len(contextPackAnyList(response["conflicts"])) > recallResponseMaxConflicts ||
		len(contextPackAnyList(response["gaps"])) > recallResponseMaxGaps ||
		len(contextPackAnyList(response["receipt_refs"])) > recallResponseMaxReceipts ||
		len(contextPackAnyList(proof["proof_refs"])) > recallResponseMaxProofRefs ||
		len(contextPackAnyList(answer["components"])) > recallResponseMaxModules {
		t.Fatal("response exceeded a closed list bound")
	}
	if recallResponseFuzzHasForbiddenKey(response) || strings.Contains(recallResponseCanonicalJSON(response), recallResponseFuzzSensitiveValue) {
		t.Fatal("response retained a forbidden recursive field or sensitive value")
	}
	if anyToBool(anyMap(response["action_boundary"])["can_act"]) || anyToBool(anyMap(response["action_boundary"])["execution_performed"]) {
		t.Fatal("recall response crossed the non-authority boundary")
	}
}

func finalizeRecallResponseFuzzCandidate(input, candidate map[string]any) map[string]any {
	response := finalizeRecallResponseTransport(candidate, "fuzz-agent", "fuzz", "/fuzz/recall-response")
	if recallResponseTransportCandidateValid(response) {
		return response
	}
	fallback := projectRecallResponseV1ControlFromArtifacts(input, recallResponseProductionPolicyInput())
	return finalizeRecallResponseTransport(fallback, "fuzz-agent", "fuzz", "/fuzz/recall-response")
}

func FuzzRecallResponseCompositionBoundsAndIdentity(f *testing.F) {
	f.Add("verified current status", "ordinary evidence", "2026-01-01T00:00:00Z", uint8(4))
	f.Add("", recallResponseFuzzSensitiveValue, "latest_available", uint8(0))
	f.Add(strings.Repeat("q", 4096), strings.Repeat("x", 4096), "2099-01-01T00:00:00Z", uint8(31))
	f.Fuzz(func(t *testing.T, query, evidenceText, asOf string, rowCount uint8) {
		input := recallResponseFuzzInput(query, evidenceText, asOf, rowCount)
		first := finalizeRecallResponseFuzzCandidate(input, composeRecallResponse(input))
		second := finalizeRecallResponseFuzzCandidate(input, composeRecallResponse(input))
		if !reflect.DeepEqual(first, second) {
			t.Fatal("composition or clipping was not deterministic")
		}
		assertRecallResponseFuzzInvariants(t, first)
	})
}

func FuzzRecallResponseClippingFailsClosed(f *testing.F) {
	f.Add("bounded", uint16(0), false)
	f.Add("oversized", uint16(32000), false)
	f.Add("recursive-forbidden", uint16(1024), true)
	f.Fuzz(func(t *testing.T, query string, inflation uint16, injectForbidden bool) {
		input := recallResponseFuzzInput(query, query, "2026-01-01T00:00:00Z", 8)
		candidate := composeRecallResponse(input)
		answer := anyMap(candidate["answer"])
		answer["summary"] = strings.Repeat("x", int(inflation))
		proof := anyMap(answer["proof_spine"])
		proof["proof_refs"] = append(contextPackAnyList(proof["proof_refs"]), strings.Repeat("r", int(inflation)))
		if injectForbidden {
			components := contextPackAnyList(answer["components"])
			if len(components) > 0 {
				anyMap(anyMap(components[0])["payload"])["raw_prompt"] = recallResponseFuzzSensitiveValue
			}
		}
		response := finalizeRecallResponseFuzzCandidate(input, candidate)
		assertRecallResponseFuzzInvariants(t, response)
	})
}

func FuzzContinuousCognitionRequestBounds(f *testing.F) {
	f.Add("observe", "bounded query", "contextlattice", "workspace", "2026-01-01T00:00:00Z", uint16(32), false)
	f.Add("activate", strings.Repeat("q", continuousCognitionMaxQueryBytes+1), "../invalid", strings.Repeat("r", continuousCognitionMaxReferenceBytes+1), "bad-time", uint16(0), true)
	f.Fuzz(func(t *testing.T, operation, query, project, reference, asOf string, limit uint16, injectUnknown bool) {
		payload := map[string]any{
			"operation":     boundedRecallResponseFuzzString(operation, 64),
			"query":         boundedRecallResponseFuzzString(query, continuousCognitionMaxQueryBytes+512),
			"project":       boundedRecallResponseFuzzString(project, continuousCognitionMaxProjectBytes+128),
			"workspace_ref": boundedRecallResponseFuzzString(reference, continuousCognitionMaxReferenceBytes+128),
			"topic_path":    boundedRecallResponseFuzzString(reference, continuousCognitionMaxTopicBytes+128),
			"as_of":         boundedRecallResponseFuzzString(asOf, 128),
			"limit":         int(limit),
		}
		if injectUnknown {
			payload["raw_prompt"] = recallResponseFuzzSensitiveValue
		}
		first, firstErr := normalizeContinuousCognitionRequest(payload)
		second, secondErr := normalizeContinuousCognitionRequest(payload)
		if (firstErr == nil) != (secondErr == nil) || (firstErr != nil && firstErr.Error() != secondErr.Error()) {
			t.Fatal("request rejection was not deterministic")
		}
		if firstErr != nil {
			return
		}
		if injectUnknown {
			t.Fatal("cognition request admitted an unsupported recursive field")
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatal("request normalization was not deterministic")
		}
		if len(first.Query) > continuousCognitionMaxQueryBytes || len(first.Project) > continuousCognitionMaxProjectBytes ||
			len(first.WorkspaceRef) > continuousCognitionMaxReferenceBytes || len(first.TopicPath) > continuousCognitionMaxTopicBytes ||
			first.Limit < 1 || first.Limit > continuousCognitionMaxLimit {
			t.Fatal("normalized cognition request exceeded a string bound")
		}
		if _, allowed := continuousCognitionOperations[first.Operation]; !allowed {
			t.Fatal("normalized cognition request admitted an unsupported operation")
		}
	})
}
