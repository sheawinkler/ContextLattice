package main

import (
	"strings"
	"testing"
)

func TestAgentPacketIsBoundedDeduplicatedAndTransportInclusive(t *testing.T) {
	response := map[string]any{
		"ok":         true,
		"query":      "verify agent packet token truth",
		"project":    "contextlattice",
		"topic_path": "product/agent-packet",
		"session_id": "sess_packet_test",
		"source_coverage": map[string]any{
			"complete": true,
			"returned": []any{"qdrant", "topic_rollups"},
		},
		"context_pack": map[string]any{
			"query": "verify agent packet token truth",
			"ranked_evidence": []any{
				map[string]any{
					"kind":       "decision",
					"text":       "Agent packet transport tokens must include the serialized response.",
					"score":      0.97,
					"project":    "contextlattice",
					"file":       "notes/agent-packet.md",
					"source":     "qdrant",
					"topic_path": "product/agent-packet",
				},
				map[string]any{
					"kind":    "decision",
					"text":    "  agent PACKET transport tokens must include the serialized response. ",
					"score":   0.94,
					"source":  "topic_rollups",
					"project": "contextlattice",
				},
				map[string]any{
					"kind":    "check",
					"text":    strings.Repeat("Verify the hard token limit against the final JSON payload. ", 24),
					"score":   0.91,
					"source":  "topic_rollups",
					"project": "contextlattice",
				},
			},
			"files_to_read": []any{"services/gateway-go/agent_packet.go"},
		},
		"context_pack_quality": map[string]any{"sample_id": "cpq_packet_test"},
		"token_impact": map[string]any{
			"baseline_tokens_estimate":        6000,
			"compiled_prompt_tokens_estimate": 900,
		},
	}
	request := map[string]any{
		"output_mode":                agentPacketContractID,
		"target_context_pack_tokens": defaultAgentPacketTargetTokens,
		"hard_limit_tokens":          defaultAgentPacketHardTokens,
	}

	packet := finalizeAgentPacket(buildAgentPacket(response, request, "context_pack"))
	assertBoundaryContractPassed(t, agentPacketContractID, packet)
	assertBoundaryJSONUnderLimit(t, agentPacketContractID, packet)
	if _, leaked := packet["context_pack"]; leaked {
		t.Fatalf("agent packet leaked full context pack: %#v", packet)
	}
	evidence := contextPackAnyList(packet["evidence"])
	if len(evidence) != 2 {
		t.Fatalf("expected normalized duplicate evidence to collapse, got %d rows: %#v", len(evidence), evidence)
	}
	count := contextPackCountAnyTokens(packet)
	budget := anyMap(packet["token_budget"])
	if actual := anyToInt(budget["actual_tokens"], 0); actual != count.Tokens {
		t.Fatalf("reported packet tokens differ from serialized payload: reported=%d actual=%d budget=%#v", actual, count.Tokens, budget)
	}
	if !anyToBool(budget["within_hard_limit"]) || count.Tokens > defaultAgentPacketHardTokens {
		t.Fatalf("packet exceeded hard limit: count=%d budget=%#v", count.Tokens, budget)
	}
	impact := anyMap(packet["token_impact"])
	if !anyToBool(impact["transport_inclusive"]) || anyToInt(impact["transport_tokens_exact"], 0) != count.Tokens {
		t.Fatalf("expected exact transport token impact, count=%d impact=%#v", count.Tokens, impact)
	}
	if anyToInt(impact["compiled_prompt_tokens_estimate"], 0) != 900 {
		t.Fatalf("expected compiled prompt economics to remain separate, got %#v", impact)
	}
}

func TestAgentPacketTransportMetadataCannotBreakMinimumHardLimit(t *testing.T) {
	evidence := make([]any, 0, 8)
	for i := 0; i < 8; i++ {
		evidence = append(evidence, map[string]any{
			"kind": "finding", "text": strings.Repeat("bounded evidence with provenance ", 40),
			"score": 0.9, "project": "contextlattice", "source": "qdrant",
		})
	}
	packet := finalizeAgentPacket(buildAgentPacket(map[string]any{
		"ok": true, "query": strings.Repeat("hard limit proof ", 80), "project": "contextlattice",
		"source_coverage": map[string]any{"complete": true, "returned": []any{"qdrant"}},
		"context_pack":    map[string]any{"ranked_evidence": evidence},
		"token_impact":    map[string]any{"baseline_tokens_estimate": 12000, "compiled_prompt_tokens_estimate": 2000},
	}, map[string]any{
		"output_mode": agentPacketContractID, "target_context_pack_tokens": 512, "hard_limit_tokens": 512,
	}, "context_pack"))
	count := contextPackCountAnyTokens(packet)
	budget := anyMap(packet["token_budget"])
	hard := anyToInt(budget["hard_limit_tokens"], 0)
	if hard != minimumAgentPacketHardTokens || !anyToBool(budget["hard_limit_adjusted"]) || count.Tokens > hard || anyToInt(budget["actual_tokens"], 0) != count.Tokens || !anyToBool(budget["within_hard_limit"]) {
		t.Fatalf("transport metadata broke hard-limit fixed point: count=%d budget=%#v", count.Tokens, budget)
	}
}
