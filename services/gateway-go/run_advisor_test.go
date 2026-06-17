package main

import (
	"strings"
	"testing"
)

func TestRunAdvisorDetectsPartialContextAndObjectiveMismatch(t *testing.T) {
	advisor := buildRunAdvisor(runAdvisorInput{
		Query:         "rust graph edge quality audit",
		Project:       "contextlattice",
		RetrievalMode: "balanced",
		AgentID:       "codex_gpt5_test",
		SourceCoverage: map[string]any{
			"configured":          []any{"qdrant", "letta"},
			"returned":            []any{},
			"pending":             []any{"letta"},
			"warming":             []any{"letta"},
			"failed":              []any{},
			"complete":            false,
			"retrieval_lifecycle": map[string]any{"status": "partial"},
		},
		Retrieval: map[string]any{
			"continuation_async": map[string]any{
				"token":      "cont-test",
				"events_url": "/memory/search/continuations/cont-test/events",
			},
		},
		Objective: objectiveContext{
			Mission:   "Send subscriber billing notices.",
			Objective: "Improve email deliverability.",
			Goal:      "Reduce bounced marketing emails.",
		},
		RankedEvidence:  []any{},
		ReferencePrompt: "",
		Surface:         "/memory/context-pack",
	})
	assertBoundaryContractPassed(t, runAdvisorContractID, advisor)
	if anyToString(advisor["posture"]) != "needs_retrieval" {
		t.Fatalf("expected needs_retrieval posture, got %#v", advisor)
	}
	continuation := anyMap(advisor["continuation"])
	if anyToString(continuation["token"]) != "cont-test" || !anyToBool(continuation["continuation_available"]) {
		t.Fatalf("expected continuation guidance, got %#v", continuation)
	}
	objective := anyMap(advisor["objective_coherence"])
	if anyToString(objective["status"]) != "mismatch" {
		t.Fatalf("expected objective mismatch, got %#v", objective)
	}
}

func TestRunAdvisorObjectiveCoherenceUsesHierarchy(t *testing.T) {
	advisor := buildRunAdvisor(runAdvisorInput{
		Query:         "coordinate agent session objective hierarchy and handoff lineage",
		Project:       "contextlattice",
		RetrievalMode: "balanced",
		AgentID:       "codex_gpt5_test",
		SourceCoverage: map[string]any{
			"returned": []any{"qdrant"},
			"complete": true,
		},
		Objective: objectiveContext{
			Mission:                 "Coordinate agents with durable memory.",
			Objective:               "Ship agent objective hierarchy.",
			Goal:                    "Improve handoff lineage.",
			ProjectPrimaryObjective: "Make ContextLattice the runtime coordination layer for agent work.",
			TopicObjective:          "Represent topic and subtopic objectives for prompt repackaging.",
			SessionObjective:        "Implement session objective hierarchy and handoff lineage.",
			Subobjectives:           []string{"Persist project objective", "Expose session lineage"},
		},
		RankedEvidence:  []any{map[string]any{"text": "objective hierarchy evidence"}},
		ReferencePrompt: strings.Repeat("agent objective hierarchy ", 20),
		Surface:         "/memory/context-pack",
	})
	assertBoundaryContractPassed(t, runAdvisorContractID, advisor)
	objective := anyMap(advisor["objective_coherence"])
	signals := anyMap(objective["signals"])
	if !anyToBool(signals["project_primary_objective_present"]) ||
		!anyToBool(signals["topic_objective_present"]) ||
		!anyToBool(signals["session_objective_present"]) ||
		anyToInt(signals["subobjective_count"], 0) != 2 {
		t.Fatalf("expected hierarchy objective signals, got %#v", signals)
	}
	if anyToString(objective["status"]) == "missing" {
		t.Fatalf("expected hierarchy to contribute objective coherence, got %#v", objective)
	}
}

func TestRunAdvisorGraphQualityActions(t *testing.T) {
	sampled := buildRunAdvisor(runAdvisorInput{
		Query:           "graph neighbor context",
		Project:         "contextlattice",
		RetrievalMode:   "balanced",
		SourceCoverage:  map[string]any{"returned": []any{"qdrant", memoryEdgeSource}, "complete": true},
		RankedEvidence:  []any{map[string]any{"kind": "graph_neighbor", "text": "target graph evidence"}},
		ReferencePrompt: strings.Repeat("graph evidence ", 40),
		GraphQuality: map[string]any{
			"status": "sampled",
			"used":   true,
			"score":  80,
			"signals": map[string]any{
				"added_evidence_count": 1,
			},
		},
		Surface: "/memory/context-pack",
	})
	assertBoundaryContractPassed(t, runAdvisorContractID, sampled)
	graphQuality := anyMap(sampled["graph_quality"])
	if !anyToBool(graphQuality["used"]) || anyToString(graphQuality["status"]) != "sampled" {
		t.Fatalf("expected sampled graph quality to be preserved, got %#v", graphQuality)
	}
	for _, raw := range contextPackAnyList(sampled["next_actions"]) {
		action := anyMap(raw)
		if anyToString(action["label"]) == "repair_graph_edges" {
			t.Fatalf("sampled graph evidence should not request graph repair, got %#v", sampled["next_actions"])
		}
	}

	empty := buildRunAdvisor(runAdvisorInput{
		Query:           "graph neighbor context",
		Project:         "contextlattice",
		RetrievalMode:   "balanced",
		SourceCoverage:  map[string]any{"returned": []any{"qdrant"}, "complete": true},
		RankedEvidence:  []any{map[string]any{"kind": "memory", "text": "seed evidence"}},
		ReferencePrompt: strings.Repeat("seed evidence ", 40),
		GraphQuality: map[string]any{
			"status":         "empty",
			"used":           false,
			"score":          35,
			"recommendation": "Run memory-edge-backfill for older projects.",
		},
		Surface: "/memory/context-pack",
	})
	assertBoundaryContractPassed(t, runAdvisorContractID, empty)
	foundRepair := false
	for _, raw := range contextPackAnyList(empty["next_actions"]) {
		action := anyMap(raw)
		if anyToString(action["label"]) == "repair_graph_edges" {
			foundRepair = strings.Contains(anyToString(action["command"]), "memory-edge-backfill")
			break
		}
	}
	if !foundRepair {
		t.Fatalf("expected empty graph quality to produce graph repair action, got %#v", empty["next_actions"])
	}
}

func TestContextPackContractSynthesizesRunAdvisor(t *testing.T) {
	pack := testContextPackFixture([]any{map[string]any{"text": "graph quality edge audit", "source": "fixture"}})
	payload := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"query":              "graph quality edge audit",
		"context_pack":       pack,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	})
	assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
	advisor := anyMap(payload["run_advisor"])
	if anyToString(advisor["schema_id"]) != runAdvisorContractID {
		t.Fatalf("expected synthesized run advisor, got %#v", advisor)
	}
	assertBoundaryContractPassed(t, runAdvisorContractID, advisor)
}
