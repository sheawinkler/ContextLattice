package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestAgentContractsRegistryLoadsSharedProtocol(t *testing.T) {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		t.Fatalf("load agent contracts registry: %v", err)
	}
	if strings.TrimSpace(registry.RegistryID) != "contextlattice_agent_output_contracts" {
		t.Fatalf("unexpected registry id: %q", registry.RegistryID)
	}
	if agentContract(registry, policyContextPackageContractID) == nil {
		t.Fatalf("missing %s contract", policyContextPackageContractID)
	}
	protocol := antiSchemingProtocol()
	if !strings.Contains(anyToString(protocol["law"]), "Change conclusions to match evidence") {
		t.Fatalf("unexpected anti-scheming law: %#v", protocol["law"])
	}
	if findings := validateAgentContractPayload(antiSchemingContractID, protocol); len(findings) != 0 {
		t.Fatalf("anti-scheming protocol should validate: %#v", findings)
	}
}

func TestGeneratedAgentContractsMatchRegistry(t *testing.T) {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		t.Fatalf("load agent contracts registry: %v", err)
	}
	if GeneratedAgentContractRegistryID != registry.RegistryID {
		t.Fatalf("generated registry id drift: %q != %q", GeneratedAgentContractRegistryID, registry.RegistryID)
	}
	if GeneratedAgentContractRegistryVersion != registry.RegistryVersion {
		t.Fatalf("generated registry version drift: %d != %d", GeneratedAgentContractRegistryVersion, registry.RegistryVersion)
	}
	ids := make([]string, 0, len(registry.Contracts))
	for id := range registry.Contracts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	generated := append([]string{}, GeneratedAgentContractIDs...)
	sort.Strings(generated)
	if !reflect.DeepEqual(generated, ids) {
		t.Fatalf("generated contract ids drift:\ngenerated=%#v\nregistry=%#v", generated, ids)
	}
}

func testContextCompilerFixture(strategy string, evidenceCount int) map[string]any {
	return map[string]any{
		"schema_id":             "contextlattice_context_compiler.v1",
		"version":               1,
		"strategy":              strategy,
		"intended_use":          "verify bounded prompt-ready context packages",
		"recommended_surface":   "cli_for_local_agents",
		"ranked_evidence_count": evidenceCount,
	}
}

func testPromptSectionsFixture(task string, evidence []any) map[string]any {
	return map[string]any{
		"objective":        task,
		"task":             task,
		"next_action":      "Use ranked evidence, inspect files, and run matching checks.",
		"evidence":         evidence,
		"files_to_inspect": []any{},
		"commands":         []any{},
		"checks":           []any{},
		"risks":            []any{},
		"capabilities":     []any{},
		"constraints":      []any{"Keep output bounded."},
	}
}

func testContextPackFixture(items []any) map[string]any {
	if items == nil {
		items = []any{}
	}
	compiler := testContextCompilerFixture("test_fixture", len(items))
	return map[string]any{
		"facts":               items,
		"numeric_facts":       items,
		"results":             items,
		"citations":           items,
		"context_compiler":    compiler,
		"prompt_sections":     testPromptSectionsFixture("contract test", items),
		"ranked_evidence":     items,
		"relevant_decisions":  items,
		"files_to_read":       []any{},
		"files_to_avoid":      []any{},
		"capabilities_to_use": []any{},
		"runbooks":            []any{},
		"known_failure_modes": []any{},
		"commands":            []any{},
		"acceptance_criteria": []any{},
	}
}

func TestPolicyContextPackageContractValidationPassesAndFails(t *testing.T) {
	pack := map[string]any{
		"context_pack": testContextPackFixture([]any{map[string]any{"text": "f1", "source": "test"}}),
	}
	policy := buildPolicyContextPackage(
		"codex",
		"codex_gpt5_test",
		"contextlattice",
		"runbooks/codex-integration",
		"contract test",
		"fast",
		pack,
		pack,
		nil,
		nil,
		objectiveContext{},
	)
	format, ok := policy["format_contract"].(map[string]any)
	if !ok {
		t.Fatalf("missing format_contract: %#v", policy["format_contract"])
	}
	if strings.TrimSpace(anyToString(format["schema_id"])) != policyContextPackageContractID {
		t.Fatalf("unexpected schema_id: %#v", format["schema_id"])
	}
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected policy validation passed, got %#v", validation)
	}
	assertBoundaryMetadata(t, policy, "format_contract", false)
	if findings := validateAgentContractPayload(policyContextPackageContractID, policy); len(findings) != 0 {
		t.Fatalf("policy package should validate: %#v", findings)
	}
	if findings := validateAgentContractPayload(objectiveRuntimeStateContractID, anyMap(policy["objective_runtime"])); len(findings) != 0 {
		t.Fatalf("objective runtime should validate: %#v", findings)
	}
	if hierarchy := anyMap(policy["objective_hierarchy"]); anyToString(hierarchy["schema_id"]) != "contextlattice_objective_hierarchy.v1" {
		t.Fatalf("expected policy objective hierarchy, got %#v", policy["objective_hierarchy"])
	}
	if lineage := anyMap(policy["objective_lineage"]); anyToString(lineage["schema_id"]) != "contextlattice_objective_lineage.v1" {
		t.Fatalf("expected policy objective lineage, got %#v", policy["objective_lineage"])
	}

	badPolicy := cloneContractMap(policy)
	delete(badPolicy, "format_contract")
	badPolicy["hookSpecificOutput"] = map[string]any{"unsafe": true}
	findings := validateAgentContractPayload(policyContextPackageContractID, badPolicy)
	if len(findings) == 0 {
		t.Fatalf("expected malformed policy package to fail validation")
	}
	joined := ""
	for _, finding := range findings {
		joined += anyToString(finding["reason"]) + " " + anyToString(finding["path"]) + " " + anyToString(finding["field"]) + "\n"
	}
	if !strings.Contains(joined, "missing_required_field") || !strings.Contains(joined, "forbidden_field_present") {
		t.Fatalf("expected missing required and forbidden findings, got %#v", findings)
	}
}

func TestObjectiveRuntimeStateContractValidationPassesAndFails(t *testing.T) {
	runtime := buildObjectiveRuntimeState(
		"generic-agent",
		"generic_agent_test",
		"contextlattice",
		"runbooks/generic-agent",
		"runtime contract test",
		"balanced",
		"sess-runtime-test",
		objectiveContext{Mission: "ship durable coordination", Objective: "prove objective runtime state", Goal: "keep output bounded"},
		"agent.preflight.completed",
	)
	format, ok := runtime["format_contract"].(map[string]any)
	if !ok {
		t.Fatalf("missing objective runtime format_contract: %#v", runtime)
	}
	if strings.TrimSpace(anyToString(format["schema_id"])) != objectiveRuntimeStateContractID {
		t.Fatalf("unexpected objective runtime schema_id: %#v", format["schema_id"])
	}
	validation, _ := format["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected objective runtime validation passed, got %#v", validation)
	}
	assertBoundaryMetadata(t, runtime, "format_contract", false)
	if findings := validateAgentContractPayload(objectiveRuntimeStateContractID, runtime); len(findings) != 0 {
		t.Fatalf("objective runtime should validate: %#v", findings)
	}
	if hierarchy := anyMap(runtime["objective_hierarchy"]); anyToString(anyMap(hierarchy["project"])["primary_objective"]) == "" {
		t.Fatalf("expected runtime project primary objective, got %#v", runtime["objective_hierarchy"])
	}
	if lineage := anyMap(runtime["objective_lineage"]); anyToString(anyMap(lineage["drift"])["status"]) == "" {
		t.Fatalf("expected runtime objective lineage drift status, got %#v", runtime["objective_lineage"])
	}
	bad := cloneContractMap(runtime)
	delete(bad, "next_action")
	bad["raw_prompt"] = "unsafe"
	findings := validateAgentContractPayload(objectiveRuntimeStateContractID, bad)
	if len(findings) == 0 {
		t.Fatalf("expected malformed objective runtime state to fail")
	}
	joined := ""
	for _, finding := range findings {
		joined += anyToString(finding["reason"]) + " " + anyToString(finding["path"]) + " " + anyToString(finding["field"]) + "\n"
	}
	if !strings.Contains(joined, "missing_required_field") || !strings.Contains(joined, "forbidden_field_present") {
		t.Fatalf("expected missing required and forbidden findings, got %#v", findings)
	}
}

func TestAgentPreflightFormatContractValidationPassesAndFails(t *testing.T) {
	contextPack := testContextPackFixture([]any{})
	pack := map[string]any{
		"ok":                 true,
		"context_pack":       contextPack,
		"context_compiler":   contextPack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	}
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "fast", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "fast", pack, pack, nil, objectiveRuntime, objectiveContext{})
	response := attachAgentPreflightFormatContracts(map[string]any{
		"ok":                     true,
		"service":                "gateway-go",
		"agent":                  "codex",
		"agent_id":               "codex_gpt5_test",
		"project":                "contextlattice",
		"query":                  "preflight",
		"topic_path":             "runbooks/codex-integration",
		"retrieval_mode":         "fast",
		"context_pack":           pack,
		"objective_runtime":      objectiveRuntime,
		"policy_context_package": policy,
	})
	contracts, ok := response["format_contracts"].(map[string]any)
	if !ok {
		t.Fatalf("missing format_contracts: %#v", response["format_contracts"])
	}
	validation, _ := contracts["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(validation["status"])) != "passed" {
		t.Fatalf("expected preflight validation passed, got %#v", validation)
	}
	assertBoundaryMetadata(t, response, "format_contracts", false)

	bad := cloneContractMap(response)
	delete(bad, "policy_context_package")
	findings := validateAgentContractPayload(agentPreflightResponseContractID, bad)
	if len(findings) == 0 {
		t.Fatalf("expected malformed preflight response to fail validation")
	}
}

func TestAgentPreflightProfileMatrixCoversLocalAgents(t *testing.T) {
	cases := []struct {
		agent   string
		key     string
		agentID string
		topic   string
	}{
		{"codex", "codex", "codex_gpt5", "runbooks/codex-integration"},
		{"claude-code", "claude-code", "claude_code_agent", "runbooks/claude-code-integration"},
		{"opencode", "opencode", "opencode_agent", "runbooks/opencode-integration"},
		{"hermes", "hermes-agent", "hermes_agent", "runbooks/hermes-agent-integration"},
		{"hermes-ultra", "hermes-ultra", "hermes_ultra_agent", "runbooks/hermes-ultra-integration"},
		{"hermes-agent-ultra", "hermes-ultra", "hermes_ultra_agent", "runbooks/hermes-ultra-integration"},
		{"omp", "omp", "omp_agent", "runbooks/omp-integration"},
		{"oh-my-pi", "omp", "omp_agent", "runbooks/omp-integration"},
		{"mercury", "mercury-agent", "mercury_agent", "runbooks/mercury-agent-integration"},
		{"mercury-agent", "mercury-agent", "mercury_agent", "runbooks/mercury-agent-integration"},
		{"pi", "pi", "pi_agent", "runbooks/pi-integration"},
		{"pi-agent", "pi", "pi_agent", "runbooks/pi-integration"},
		{"droid", "droid", "droid_agent", "runbooks/droid-integration"},
		{"droid-agent", "droid", "droid_agent", "runbooks/droid-integration"},
		{"chatgpt-web", "chatgpt-web", "chatgpt_web_agent", "runbooks/chatgpt-web-integration"},
		{"chatgpt-desktop", "chatgpt-desktop", "chatgpt_desktop_agent", "runbooks/chatgpt-desktop-integration"},
		{"claude-web", "claude-web", "claude_web_agent", "runbooks/claude-web-integration"},
		{"claude-desktop", "claude-desktop", "claude_desktop_agent", "runbooks/claude-desktop-integration"},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			key, profile := resolveAgentPreflightProfile(tc.agent)
			if key != tc.key {
				t.Fatalf("expected key=%q, got %q", tc.key, key)
			}
			if profile.AgentID != tc.agentID {
				t.Fatalf("expected agent_id=%q, got %q", tc.agentID, profile.AgentID)
			}
			if profile.TopicPath != tc.topic {
				t.Fatalf("expected topic=%q, got %q", tc.topic, profile.TopicPath)
			}
			if strings.TrimSpace(profile.Query) == "" {
				t.Fatalf("expected non-empty query")
			}
			if profile.RetrievalMode != "balanced" {
				t.Fatalf("expected balanced retrieval mode, got %q", profile.RetrievalMode)
			}
		})
	}
}

func TestContextPackAndWritebackFormatContractsValidate(t *testing.T) {
	pack := testContextPackFixture([]any{})
	coverage := map[string]any{"configured": []any{"postgres_pgvector"}, "returned": []any{"postgres_pgvector"}, "complete": true}
	contextResponse := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       pack,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    coverage,
		"writeback_required": true,
	})
	contextFormat, _ := contextResponse["format_contract"].(map[string]any)
	contextValidation, _ := contextFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(contextValidation["status"])) != "passed" {
		t.Fatalf("expected context-pack validation passed, got %#v", contextValidation)
	}
	assertBoundaryMetadata(t, contextResponse, "format_contract", false)

	dream := attachPayloadFormatContract(dreamModeResponseContractID, map[string]any{
		"ok":                  true,
		"mode":                "dream",
		"dream_available":     true,
		"intelligence_source": "llm_synthesis",
		"agent_id":            "codex_gpt5_test",
		"project":             "contextlattice",
		"goal":                "invent a better memory primitive",
		"query":               "invent a better memory primitive",
		"topic_path":          "contextlattice/dream-mode",
		"retrieval_mode":      "balanced",
		"novelty_level":       3,
		"risk_tolerance":      "balanced",
		"hypotheses":          []any{map[string]any{"id": "h1", "title": "t", "claim": "c", "supporting_evidence": []any{"e1"}}},
		"experiments":         []any{map[string]any{"id": "x1", "hypothesis_id": "h1", "method": "test"}},
		"evidence": map[string]any{
			"facts":     []any{map[string]any{"id": "e1", "text": "fact"}},
			"results":   []any{},
			"citations": []any{},
			"counts":    map[string]any{"facts": 1},
		},
		"source_coverage":    coverage,
		"llm":                map[string]any{"enabled": true, "used": true, "provider": "ollama", "model": "qwen3.5:9b"},
		"writeback_required": true,
	}, "codex_gpt5_test", "dream", "/memory/dream")
	dreamFormat, _ := dream["format_contract"].(map[string]any)
	dreamValidation, _ := dreamFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(dreamValidation["status"])) != "passed" {
		t.Fatalf("expected dream validation passed, got %#v", dreamValidation)
	}
	assertBoundaryMetadata(t, dream, "format_contract", false)

	writeback := attachWritebackFormatContract(
		map[string]any{"ok": true, "event_id": "evt_test"},
		normalizedWrite{project: "contextlattice", fileName: "notes/test.md", topicPath: "runbooks/codex-integration", agentID: "codex_gpt5_test"},
		"/memory/write",
		200,
	)
	writebackFormat, _ := writeback["format_contract"].(map[string]any)
	writebackValidation, _ := writebackFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(writebackValidation["status"])) != "passed" {
		t.Fatalf("expected writeback validation passed, got %#v", writebackValidation)
	}
}

func TestAgentBoundaryContractClipsOversizedContextPackPayload(t *testing.T) {
	oversized := strings.Repeat("array_above_max_length context length exceeded ", 400)
	items := make([]any, 0, 140)
	for idx := 0; idx < 140; idx++ {
		items = append(items, map[string]any{
			"text":       oversized,
			"summary":    oversized,
			"file":       "notes/oversized.md",
			"source":     "fixture",
			"topic_path": "runbooks/boundary",
		})
	}
	pack := testContextPackFixture(items)
	pack["files_to_read"] = items
	pack["files_to_avoid"] = items
	pack["capabilities_to_use"] = items
	pack["runbooks"] = items
	pack["known_failure_modes"] = items
	pack["commands"] = items
	pack["acceptance_criteria"] = items
	payload := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       pack,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   oversized,
		"source_coverage":    map[string]any{"configured": items, "returned": items, "complete": true},
		"retrieval":          map[string]any{"debug": oversized},
		"writeback_required": true,
	})
	assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
	assertBoundaryMetadata(t, payload, "format_contract", true)
	clippedPack, _ := payload["context_pack"].(map[string]any)
	if results, _ := clippedPack["results"].([]any); len(results) > agentBoundaryLimitsForContract(contextPackResponseContractID).MaxListItems {
		t.Fatalf("expected context_pack.results clipped, got %d", len(results))
	}
}

func TestMinimalContextPackBoundaryPreservesContextAndSynthesisContracts(t *testing.T) {
	pack := minimalContextPackBoundary(map[string]any{"query": "sparse synthesis boundary"})
	coverage := map[string]any{"configured": []any{"qdrant"}, "returned": []any{}, "complete": false}
	runAdvisor := buildRunAdvisor(runAdvisorInput{
		Query:          "sparse synthesis boundary",
		Project:        "contextlattice",
		RetrievalMode:  "fast",
		SourceCoverage: coverage,
		RankedEvidence: []any{},
		Surface:        "/memory/context-pack",
	})
	contextResponse := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"context_pack":       pack,
		"source_coverage":    coverage,
		"context_compiler":   pack["context_compiler"],
		"reference_prompt":   "No ranked evidence was available; broaden retrieval.",
		"run_advisor":        runAdvisor,
		"writeback_required": true,
	})
	assertBoundaryContractPassed(t, contextPackResponseContractID, contextResponse)

	s := newTestServer(t, "http://127.0.0.1:1")
	synthesis := s.buildSynthesisPack(context.Background(), contextResponse, map[string]any{
		"project":        "contextlattice",
		"query":          "sparse synthesis boundary",
		"retrieval_mode": "fast",
	}, "/memory/synthesis-pack")
	synthesisResponse := attachPayloadFormatContract(synthesisPackContractID, map[string]any{
		"ok":                   true,
		"schema_id":            synthesisPackContractID,
		"version":              1,
		"project":              "contextlattice",
		"query":                "sparse synthesis boundary",
		"topic_path":           "",
		"retrieval_mode":       "fast",
		"retrieval_intent":     "synthesis",
		"synthesis_pack":       synthesis,
		"context_pack":         pack,
		"source_coverage":      coverage,
		"reference_prompt":     synthesisPackReferencePrompt(synthesis),
		"token_impact":         map[string]any{},
		"context_pack_quality": map[string]any{},
		"run_advisor":          runAdvisor,
		"writeback_required":   true,
	}, "codex_gpt5_test", "synthesis_pack", "/memory/synthesis-pack")
	assertBoundaryContractPassed(t, synthesisPackContractID, synthesisResponse)
}

func TestForceMinimalContextPackBoundaryPreservesEvidenceKernel(t *testing.T) {
	items := make([]any, 0, 7)
	for idx := 0; idx < 7; idx++ {
		items = append(items, map[string]any{
			"rank":             idx + 1,
			"kind":             "decision",
			"text":             "Preserve evidence before lower-value prompt structure.",
			"project":          "contextlattice",
			"file":             fmt.Sprintf("notes/evidence-%d.md", idx),
			"source":           "qdrant",
			"topic_path":       "architecture/synthesis",
			"score":            0.99 - float64(idx)*0.01,
			"confidence":       0.9,
			"estimated_tokens": 50,
			"value_density":    0.8,
			"why_selected":     []any{"decision", "query_match"},
		})
	}
	pack := testContextPackFixture(items)
	pack["query"] = "preserve sparse evidence"
	pack["omitted_high_value_refs"] = items[4:]
	compiler := anyMap(pack["context_compiler"])
	compiler["token_budget"] = map[string]any{
		"active":                        true,
		"selected_count":                len(items),
		"used_tokens_estimate":          350,
		"ranked_evidence_budget_tokens": 500,
		"compression_level":             "compact",
	}
	payload := map[string]any{
		"ok":                 true,
		"context_pack":       pack,
		"context_compiler":   compiler,
		"reference_prompt":   strings.Repeat("lower-value prompt structure ", 500),
		"source_coverage":    map[string]any{"configured": []any{"qdrant"}, "returned": []any{"qdrant"}, "complete": true},
		"writeback_required": true,
	}
	stats := agentBoundaryStats{}
	forceMinimalContextPackResponseBoundary(payload, &stats)

	minimal := anyMap(payload["context_pack"])
	ranked := contextPackAnyList(minimal["ranked_evidence"])
	if len(ranked) != 4 {
		t.Fatalf("expected four-item evidence kernel, got %#v", ranked)
	}
	if anyToString(anyMap(ranked[0])["file"]) != "notes/evidence-0.md" {
		t.Fatalf("expected highest-ranked evidence first, got %#v", ranked[0])
	}
	minimalCompiler := anyMap(payload["context_compiler"])
	if anyToInt(minimalCompiler["ranked_evidence_count"], 0) != 4 || anyToInt(minimalCompiler["pre_boundary_ranked_evidence_count"], 0) != 7 {
		t.Fatalf("expected reconciled compiler counts, got %#v", minimalCompiler)
	}
	minimalBudget := anyMap(minimalCompiler["token_budget"])
	if anyToInt(minimalBudget["selected_count"], 0) != 4 || anyToInt(minimalBudget["pre_boundary_selected_count"], 0) != 7 {
		t.Fatalf("expected reconciled token-budget counts, got %#v", minimalBudget)
	}
	if promptEvidence := contextPackAnyList(anyMap(minimal["prompt_sections"])["evidence"]); len(promptEvidence) != 4 {
		t.Fatalf("expected prompt sections to preserve evidence kernel, got %#v", promptEvidence)
	}
	if prompt := anyToString(payload["reference_prompt"]); !strings.Contains(prompt, "notes/evidence-0.md") {
		t.Fatalf("expected rebuilt reference prompt to cite preserved evidence, got %q", prompt)
	}

	contextResponse := attachContextPackFormatContract(payload)
	assertBoundaryContractPassed(t, contextPackResponseContractID, contextResponse)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, contextResponse)
	s := newTestServer(t, "http://127.0.0.1:1")
	synthesis := s.buildSynthesisPack(context.Background(), contextResponse, map[string]any{
		"project": "contextlattice",
		"query":   "preserve sparse evidence",
	}, "/memory/synthesis-pack")
	if findings := contextPackAnyList(synthesis["high_signal_findings"]); len(findings) == 0 {
		t.Fatalf("expected grounded synthesis findings from evidence kernel, got %#v", synthesis)
	}
	if evidenceTrail := contextPackAnyList(synthesis["evidence_trail"]); len(evidenceTrail) == 0 {
		t.Fatalf("expected grounded synthesis evidence trail, got %#v", synthesis)
	}
}

func TestContextPackBoundaryReconcilesIntermediateEvidenceTrim(t *testing.T) {
	items := make([]any, 0, 11)
	for idx := 0; idx < 11; idx++ {
		items = append(items, map[string]any{
			"rank":             idx + 1,
			"kind":             "check",
			"text":             fmt.Sprintf("evidence-%d", idx),
			"file":             fmt.Sprintf("notes/check-%d.md", idx),
			"estimated_tokens": 25,
		})
	}
	pack := testContextPackFixture(items)
	compiler := anyMap(pack["context_compiler"])
	compiler["token_budget"] = map[string]any{
		"active":               true,
		"selected_count":       11,
		"used_tokens_estimate": 275,
	}
	pack["ranked_evidence"] = items[:8]
	pack["rankedEvidence"] = items[:8]
	payload := map[string]any{
		"context_pack":     pack,
		"context_compiler": compiler,
		"reference_prompt": "stale pre-boundary prompt",
		"warnings":         []any{},
	}
	stats := agentBoundaryStats{}
	reconcileContextPackBoundaryMetadata(payload, true, &stats)

	reconciledCompiler := anyMap(payload["context_compiler"])
	if anyToInt(reconciledCompiler["ranked_evidence_count"], 0) != 8 || anyToInt(reconciledCompiler["pre_boundary_ranked_evidence_count"], 0) != 11 {
		t.Fatalf("expected truthful ranked evidence counts, got %#v", reconciledCompiler)
	}
	reconciledBudget := anyMap(reconciledCompiler["token_budget"])
	if anyToInt(reconciledBudget["selected_count"], 0) != 8 || anyToInt(reconciledBudget["pre_boundary_selected_count"], 0) != 11 || anyToInt(reconciledBudget["used_tokens_estimate"], 0) != 200 {
		t.Fatalf("expected truthful token budget counts, got %#v", reconciledBudget)
	}
	reconciledPack := anyMap(payload["context_pack"])
	if promptEvidence := contextPackAnyList(anyMap(reconciledPack["prompt_sections"])["evidence"]); len(promptEvidence) != 8 {
		t.Fatalf("expected prompt evidence to match returned evidence, got %d", len(promptEvidence))
	}
	if prompt := anyToString(payload["reference_prompt"]); strings.Contains(prompt, "notes/check-8.md") || !strings.Contains(prompt, "notes/check-0.md") {
		t.Fatalf("expected rebuilt prompt to cite only returned evidence, got %q", prompt)
	}
	if warnings := contextPackAnyList(payload["warnings"]); len(warnings) != 1 {
		t.Fatalf("expected one boundary warning, got %#v", warnings)
	}
}

func TestAgentBoundaryContractClipsOversizedDreamPayload(t *testing.T) {
	oversized := strings.Repeat("array_above_max_length context length exceeded nonlinear hypothesis ", 500)
	items := make([]any, 0, 140)
	for idx := 0; idx < 140; idx++ {
		items = append(items, map[string]any{
			"id":                  "h",
			"title":               oversized,
			"claim":               oversized,
			"why_novel":           oversized,
			"supporting_evidence": []any{"e1", "e2"},
			"experiment":          oversized,
			"expected_signal":     oversized,
		})
	}
	payload := attachPayloadFormatContract(dreamModeResponseContractID, map[string]any{
		"ok":                  true,
		"mode":                "dream",
		"dream_available":     true,
		"intelligence_source": "llm_synthesis",
		"agent_id":            "codex_gpt5_test",
		"project":             "contextlattice",
		"goal":                oversized,
		"query":               oversized,
		"topic_path":          "contextlattice/dream-mode",
		"retrieval_mode":      "balanced",
		"novelty_level":       5,
		"risk_tolerance":      "experimental",
		"hypotheses":          items,
		"experiments":         items,
		"evidence": map[string]any{
			"facts":     items,
			"results":   items,
			"citations": items,
			"combined":  items,
			"counts":    map[string]any{"facts": 140, "results": 140, "citations": 140},
		},
		"source_coverage":    map[string]any{"configured": items, "returned": items, "complete": true},
		"llm":                map[string]any{"enabled": true, "used": true, "provider": "ollama", "model": "qwen3.5:9b", "synthesis_text": oversized, "parsed": map[string]any{"hypotheses": items}},
		"retrieval":          map[string]any{"debug": oversized},
		"writeback":          map[string]any{"debug": oversized},
		"writeback_required": true,
	}, "codex_gpt5_test", "dream", "/memory/dream")
	assertBoundaryContractPassed(t, dreamModeResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, dreamModeResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
	assertBoundaryMetadata(t, payload, "format_contract", true)
	if hypotheses, _ := payload["hypotheses"].([]any); len(hypotheses) > agentBoundaryLimitsForContract(dreamModeResponseContractID).MaxListItems {
		t.Fatalf("expected dream hypotheses clipped, got %d", len(hypotheses))
	}
}

func TestAgentBoundaryContractClipsOversizedPreflightPayload(t *testing.T) {
	oversized := strings.Repeat("context length exceeded array_above_max_length ", 600)
	item := map[string]any{"text": oversized, "summary": oversized, "source": "fixture", "file": "oversized.md"}
	items := []any{}
	for idx := 0; idx < 120; idx++ {
		items = append(items, cloneContractMap(item))
	}
	contextPack := testContextPackFixture(items)
	contextPack["files_to_read"] = items
	contextPack["capabilities_to_use"] = items
	contextPack["runbooks"] = items
	contextPack["known_failure_modes"] = items
	contextPack["commands"] = items
	contextPack["acceptance_criteria"] = items
	pack := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       contextPack,
		"context_compiler":   contextPack["context_compiler"],
		"reference_prompt":   oversized,
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	})
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", oversized, "balanced", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", oversized, "balanced", pack, pack, nil, objectiveRuntime, objectiveContext{})
	response := attachAgentPreflightFormatContracts(map[string]any{
		"ok":                     true,
		"service":                "gateway-go",
		"agent":                  "codex",
		"agent_id":               "codex_gpt5_test",
		"project":                "contextlattice",
		"query":                  oversized,
		"topic_path":             "runbooks/codex-integration",
		"retrieval_mode":         "balanced",
		"status":                 map[string]any{"raw": oversized, "items": items},
		"scoped_search":          map[string]any{"results": items, "degraded": false},
		"broadened_search":       map[string]any{"results": items, "degraded": false},
		"context_pack":           pack,
		"mission_context_pack":   pack,
		"objective_runtime":      objectiveRuntime,
		"policy_context_package": policy,
	})
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, response)
	assertBoundaryJSONUnderLimit(t, agentPreflightResponseContractID, response)
	assertBoundaryJSONHeadroom(t, agentPreflightResponseContractID, response, 1024)
	assertNoRawProviderOverflowShape(t, response)
	assertBoundaryMetadata(t, response, "format_contracts", true)
}

func TestAgentPreflightBoundarySanitizesProviderOverflowSearchTextUnderBudget(t *testing.T) {
	phrase := "documentation mentions array_above_max_length and context length exceeded as historical provider errors"
	contextPack := testContextPackFixture([]any{})
	pack := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       contextPack,
		"context_compiler":   contextPack["context_compiler"],
		"reference_prompt":   "Use this ContextLattice compiled context package as the factual packet for the next reasoning step.",
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	})
	objectiveRuntime := buildObjectiveRuntimeState("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "balanced", "sess-test", objectiveContext{}, "agent.preflight.completed")
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "balanced", pack, pack, nil, objectiveRuntime, objectiveContext{})
	response := attachAgentPreflightFormatContracts(map[string]any{
		"ok":             true,
		"service":        "gateway-go",
		"agent":          "codex",
		"agent_id":       "codex_gpt5_test",
		"project":        "contextlattice",
		"query":          "preflight",
		"topic_path":     "runbooks/codex-integration",
		"retrieval_mode": "balanced",
		"broadened_search": map[string]any{
			"results": map[string]any{"results": []map[string]any{{"summary": phrase}}},
		},
		"context_pack":           pack,
		"objective_runtime":      objectiveRuntime,
		"policy_context_package": policy,
	})
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, response)
	assertNoRawProviderOverflowShape(t, response)
	assertBoundaryMetadata(t, response, "format_contracts", false)
}

func TestContextBoundaryPayloadCoversAgentSurfaces(t *testing.T) {
	payload := contextBoundaryPayload()
	if !anyToBool(payload["ok"]) || anyToInt(payload["violationCount"], -1) != 0 {
		t.Fatalf("expected clean context boundary payload, got %#v", payload)
	}
	routes, _ := payload["routes"].([]map[string]any)
	if len(routes) == 0 {
		rawRoutes, ok := payload["routes"].([]any)
		if !ok || len(rawRoutes) == 0 {
			t.Fatalf("expected context boundary routes, got %#v", payload["routes"])
		}
		for _, raw := range rawRoutes {
			routes = append(routes, anyMap(raw))
		}
	}
	byPath := map[string]map[string]any{}
	for _, route := range routes {
		byPath[anyToString(route["path"])] = route
		if anyToBool(route["required"]) && !anyToBool(route["bounded"]) {
			t.Fatalf("expected required boundary route bounded, got %#v", route)
		}
	}
	for _, path := range []string{
		"/memory/context-pack",
		"/tools/context_pack",
		"/v1/agents/preflight",
		"/v1/codex/preflight",
		"policy_context_package",
		"scripts/agent/contextlattice-pack",
		"scripts/agent/compaction-handoff-payload",
		"scripts/agent_hooks/contextlattice_pre_compaction_write.sh",
		"scripts/agent_hooks/contextlattice_post_compaction_read.sh",
	} {
		route := byPath[path]
		if route == nil {
			t.Fatalf("expected context boundary path %s in payload %#v", path, payload["routes"])
		}
		if anyToInt(route["max_total_json_bytes"], 0) <= 0 || anyToInt(route["max_string_bytes"], 0) <= 0 || anyToInt(route["max_list_items"], 0) <= 0 {
			t.Fatalf("expected boundary limits for %s, got %#v", path, route)
		}
	}
}

func assertBoundaryMetadata(t *testing.T, payload map[string]any, key string, wantTruncated bool) {
	t.Helper()
	metadata, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("expected %s metadata object, got %#v", key, payload[key])
	}
	if anyToBool(metadata["contract_valid"]) != true {
		t.Fatalf("expected %s contract_valid=true, got %#v", key, metadata)
	}
	if anyToBool(metadata["truncated"]) != wantTruncated {
		t.Fatalf("expected %s truncated=%v, got %#v", key, wantTruncated, metadata)
	}
	if anyToInt(metadata["max_total_json_bytes"], 0) <= 0 {
		t.Fatalf("expected %s max_total_json_bytes, got %#v", key, metadata)
	}
	if anyToInt(metadata["max_string_bytes"], 0) <= 0 {
		t.Fatalf("expected %s max_string_bytes, got %#v", key, metadata)
	}
	if anyToInt(metadata["max_list_items"], 0) <= 0 {
		t.Fatalf("expected %s max_list_items, got %#v", key, metadata)
	}
	if anyToInt(metadata["actual_json_bytes"], 0) <= 0 {
		t.Fatalf("expected %s actual_json_bytes, got %#v", key, metadata)
	}
	counts, ok := metadata["omitted_counts"].(map[string]any)
	if !ok {
		t.Fatalf("expected %s omitted_counts object, got %#v", key, metadata["omitted_counts"])
	}
	if wantTruncated {
		total := anyToInt(counts["strings_clipped"], 0) + anyToInt(counts["lists_clipped"], 0) + anyToInt(counts["optional_fields_compacted"], 0) + anyToInt(counts["boundary_passes"], 0) + anyToInt(counts["json_bytes_reduced"], 0)
		if total <= 0 {
			t.Fatalf("expected %s omitted_counts to record clipping, got %#v", key, counts)
		}
	}
}

func assertBoundaryContractPassed(t *testing.T, contractID string, payload map[string]any) {
	t.Helper()
	findings := validateAgentContractPayload(contractID, payload)
	if len(findings) != 0 {
		t.Fatalf("expected %s validation passed, got %#v", contractID, findings)
	}
}

func assertBoundaryJSONUnderLimit(t *testing.T, contractID string, payload map[string]any) {
	t.Helper()
	limits := agentBoundaryLimitsForContract(contractID)
	if limits.MaxTotalJSONBytes <= 0 {
		t.Fatalf("expected %s to define max_total_json_bytes", contractID)
	}
	if size := jsonByteLen(payload); size > limits.MaxTotalJSONBytes {
		t.Fatalf("expected %s JSON bytes <= %d, got %d", contractID, limits.MaxTotalJSONBytes, size)
	}
}

func assertBoundaryJSONHeadroom(t *testing.T, contractID string, payload map[string]any, minHeadroom int) {
	t.Helper()
	limits := agentBoundaryLimitsForContract(contractID)
	if limits.MaxTotalJSONBytes <= 0 {
		t.Fatalf("expected %s to define max_total_json_bytes", contractID)
	}
	headroom := limits.MaxTotalJSONBytes - jsonByteLen(payload)
	if headroom < minHeadroom {
		t.Fatalf("expected %s JSON headroom >= %d bytes, got %d", contractID, minHeadroom, headroom)
	}
}

func assertBoundaryMetadataActualUnderLimit(t *testing.T, contractID string, payload map[string]any, metadataKey string) {
	t.Helper()
	limits := agentBoundaryLimitsForContract(contractID)
	if limits.MaxTotalJSONBytes <= 0 {
		t.Fatalf("expected %s to define max_total_json_bytes", contractID)
	}
	metadata, ok := payload[metadataKey].(map[string]any)
	if !ok {
		t.Fatalf("expected %s metadata object, got %#v", metadataKey, payload[metadataKey])
	}
	actual := anyToInt(metadata["actual_json_bytes"], 0)
	if actual <= 0 {
		t.Fatalf("expected %s actual_json_bytes > 0, got %#v", metadataKey, metadata["actual_json_bytes"])
	}
	if actual > limits.MaxTotalJSONBytes {
		t.Fatalf("expected %s actual_json_bytes <= %d, got %d", contractID, limits.MaxTotalJSONBytes, actual)
	}
}

func assertNoRawProviderOverflowShape(t *testing.T, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"array_above_max_length", "context length exceeded", "maximum context length"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("payload still contains raw provider overflow phrase %q", forbidden)
		}
	}
}

func TestAgentContractTelemetryRecordsValidationReasons(t *testing.T) {
	before := anyToInt(agentContractTelemetrySnapshot()["total"], 0)
	recordAgentContractBoundary("codex_gpt5_test", writebackResultContractID, "writeback", "/memory/write", []map[string]any{{"reason": "missing_required_field"}})
	snapshot := agentContractTelemetrySnapshot()
	after := anyToInt(snapshot["total"], 0)
	if after <= before {
		t.Fatalf("expected telemetry total to increase, before=%d after=%d snapshot=%#v", before, after, snapshot)
	}
}
