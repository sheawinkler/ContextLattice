package main

import (
	"strings"
	"testing"
	"time"
)

func TestContextPackRankingPrefersCurrentIntentAndDeduplicatesAcrossSources(t *testing.T) {
	relevant := "Agent packet session truth reuses one task as one durable session."
	contextPack := map[string]any{
		"relevant_decisions": []any{map[string]any{
			"text": "Release v2 remains the latest public artifact.", "project": "contextlattice",
			"source": "qdrant", "timestamp": "2020-01-01T00:00:00Z", "status": "superseded",
		}},
		"facts": []any{map[string]any{
			"text": relevant, "project": "contextlattice", "file": "notes/session-truth.md",
			"source": "qdrant", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "score": 0.92,
		}},
		"results": []any{
			map[string]any{"summary": "  " + strings.ToUpper(relevant) + " ", "project": "contextlattice", "source": "topic_rollups", "score": 0.88},
			map[string]any{"summary": "Unrelated dashboard color notes.", "project": "contextlattice", "source": "qdrant", "score": 0.99},
		},
	}
	allocation := contextPackRankedEvidence("agent packet session truth", contextPack, contextPackTokenBudget{})
	rows := allocation.RankedEvidence
	if len(rows) < 2 {
		t.Fatalf("expected ranked evidence rows, got %#v", rows)
	}
	first := anyMap(rows[0])
	if !strings.Contains(anyToString(first["text"]), "Agent packet") || anyToFloat(first["query_relevance"]) < 0.5 || anyToString(first["freshness"]) != "current" {
		t.Fatalf("current task-aligned evidence did not win ranking: %#v", first)
	}
	duplicateCount := 0
	for _, raw := range rows {
		row := anyMap(raw)
		if normalizeEvidenceText(anyToString(row["text"])) == normalizeEvidenceText(relevant) {
			duplicateCount++
		}
		if strings.Contains(anyToString(row["text"]), "Release v2") && anyToInt(row["rank"], 0) == 1 {
			t.Fatalf("stale superseded evidence outranked current intent: %#v", row)
		}
	}
	if duplicateCount != 1 {
		t.Fatalf("same evidence survived provenance-level dedupe %d times: %#v", duplicateCount, rows)
	}
}

func TestSynthesisDecisionGateAbstainsBeforeUnsupportedAction(t *testing.T) {
	gate := synthesisDecisionGate(
		[]any{map[string]any{"text": "unrelated memory", "query_relevance": 0.0}},
		map[string]any{"complete": true},
		map[string]any{"status": "strong"},
	)
	if anyToString(gate["decision"]) != "abstain" || !anyToBool(gate["refusal"]) {
		t.Fatalf("weakly aligned evidence did not trigger refusal: %#v", gate)
	}
	actions := synthesisActionsForDecisionGate(gate, []any{map[string]any{"label": "deploy", "command": "git push origin main"}})
	for _, raw := range actions {
		if strings.TrimSpace(anyToString(anyMap(raw)["command"])) != "" {
			t.Fatalf("abstain gate leaked executable action: %#v", actions)
		}
	}
}

func TestProofSynthesisCannotLoosenGateAndContradictionsRequireVerification(t *testing.T) {
	abstain := proofSynthesisDecisionGate(
		map[string]any{"decision": "abstain", "refusal": true, "reasons": []any{"unaligned"}, "policy": "test"},
		[]any{map[string]any{"proof_status": "supported"}}, nil,
		map[string]any{"claim_support_rate": 1.0, "supported_claims": 1},
	)
	if anyToString(abstain["decision"]) != "abstain" {
		t.Fatalf("proof layer loosened upstream refusal: %#v", abstain)
	}
	verify := proofSynthesisDecisionGate(
		map[string]any{"decision": "act", "refusal": false, "reasons": []any{"aligned"}, "policy": "test"},
		[]any{map[string]any{"proof_status": "contested"}}, []any{map[string]any{"claim_id": "claim_a"}},
		map[string]any{"claim_support_rate": 0.5, "supported_claims": 0},
	)
	if anyToString(verify["decision"]) != "verify" || anyToInt(verify["contradiction_count"], 0) != 1 {
		t.Fatalf("contradiction did not tighten act to verify: %#v", verify)
	}
}
