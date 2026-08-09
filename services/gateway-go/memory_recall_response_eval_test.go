package main

import (
	"strings"
	"testing"
)

func TestProjectRecallResponseConditionAcceptsOnlyValidatedSyntheticPolicy(t *testing.T) {
	seenIDs := map[string]bool{}
	for _, condition := range []recallResponseEvalCondition{
		recallResponseConditionRawRetrieval,
		recallResponseConditionUniversalTemplate,
		recallResponseConditionFlatRouter,
		recallResponseConditionCompositional,
	} {
		snapshot := recallResponseTestFrozenSnapshot(condition, "")
		projected := projectRecallResponseCondition(snapshot, condition, "")
		if projected == nil || anyToString(anyMap(anyMap(projected["answer"])["composition"])["condition"]) != string(condition) {
			t.Fatalf("synthetic condition %q was not projected: %#v", condition, projected)
		}
		scope := anyMap(projected["request_scope"])
		if got := anyToString(scope["snapshot_digest"]); got != snapshot.SnapshotDigest {
			t.Fatalf("condition %q changed the fixed snapshot identity: %q", condition, got)
		}
		if got := anyToString(scope["receipt_digest"]); got != snapshot.ReceiptDigest {
			t.Fatalf("condition %q changed the fixed receipt identity: %q", condition, got)
		}
		responseID := anyToString(projected["response_id"])
		if seenIDs[responseID] {
			t.Fatalf("condition %q collided with another response identity: %q", condition, responseID)
		}
		seenIDs[responseID] = true
	}

	invalid := recallResponseTestFrozenSnapshot(recallResponseEvalCondition("forged"), "")
	if projected := projectRecallResponseCondition(invalid, "forged", ""); projected != nil {
		t.Fatal("unknown condition was accepted")
	}
	tampered := recallResponseTestFrozenSnapshot(recallResponseConditionCompositional, "")
	tampered.InputDigest = "sha256:" + strings.Repeat("f", 64)
	if projected := projectRecallResponseCondition(tampered, recallResponseConditionCompositional, ""); projected != nil {
		t.Fatal("mismatched condition input digest was accepted")
	}
}

func TestRecallResponseProductionRequestExcludesEvaluationKeys(t *testing.T) {
	payload := map[string]any{
		"query": "bounded", "condition": "candidate", "ablation": "timeline",
		"fixture": "fixture-a", "snapshot": "forged", "policy_input": map[string]any{"synthetic": true},
	}
	request := recallResponseRequestPayload(payload)
	for _, key := range []string{"condition", "ablation", "fixture", "snapshot", "policy_input"} {
		if _, present := request[key]; present {
			t.Fatalf("synthetic evaluation key crossed the production allowlist: %s", key)
		}
	}
	response := composeRecallResponse(recallResponseTestInput(true))
	if got := anyToString(anyMap(anyMap(response["answer"])["composition"])["condition"]); got != string(recallResponseConditionCompositional) {
		t.Fatalf("production projection used a non-compositional condition: %q", got)
	}
}

func recallResponseTestFrozenSnapshot(condition recallResponseEvalCondition, omitted recallResponseModuleType) recallResponseFrozenSnapshot {
	ablation := strings.TrimSpace(string(omitted))
	if ablation == "" {
		ablation = "none"
	}
	snapshot := recallResponseFrozenSnapshot{
		SchemaID:          recallResponseSyntheticSnapshotSchema,
		Input:             recallResponseTestInput(true),
		PolicyInputDigest: "sha256:" + strings.Repeat("1", 64),
		RequestDigest:     "sha256:" + strings.Repeat("2", 64),
		SnapshotDigest:    "sha256:" + strings.Repeat("3", 64),
		ReceiptDigest:     "sha256:" + strings.Repeat("4", 64),
	}
	identity := map[string]any{
		"condition": string(condition), "ablation": ablation,
		"policy_input_digest": snapshot.PolicyInputDigest, "request_digest": snapshot.RequestDigest,
		"snapshot_digest": snapshot.SnapshotDigest, "receipt_digest": snapshot.ReceiptDigest,
	}
	snapshot.InputDigest = "sha256:" + sha256Hex(recallResponseCanonicalJSON(identity))
	return snapshot
}
