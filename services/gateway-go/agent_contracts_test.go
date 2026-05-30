package main

import (
	"encoding/json"
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

func TestPolicyContextPackageContractValidationPassesAndFails(t *testing.T) {
	pack := map[string]any{
		"context_pack": map[string]any{
			"facts":   []any{map[string]any{"text": "f1", "source": "test"}},
			"results": []any{},
		},
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
	if findings := validateAgentContractPayload(policyContextPackageContractID, policy); len(findings) != 0 {
		t.Fatalf("policy package should validate: %#v", findings)
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

func TestAgentPreflightFormatContractValidationPassesAndFails(t *testing.T) {
	pack := map[string]any{"context_pack": map[string]any{"facts": []any{}, "results": []any{}}}
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "fast", pack, pack, nil, objectiveContext{})
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

	bad := cloneContractMap(response)
	delete(bad, "policy_context_package")
	findings := validateAgentContractPayload(agentPreflightResponseContractID, bad)
	if len(findings) == 0 {
		t.Fatalf("expected malformed preflight response to fail validation")
	}
}

func TestContextPackAndWritebackFormatContractsValidate(t *testing.T) {
	pack := map[string]any{
		"facts":               []any{},
		"results":             []any{},
		"citations":           []any{},
		"relevant_decisions":  []any{},
		"files_to_read":       []any{},
		"files_to_avoid":      []any{},
		"capabilities_to_use": []any{},
		"runbooks":            []any{},
		"known_failure_modes": []any{},
		"commands":            []any{},
		"acceptance_criteria": []any{},
	}
	coverage := map[string]any{"configured": []any{"postgres_pgvector"}, "returned": []any{"postgres_pgvector"}, "complete": true}
	contextResponse := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       pack,
		"source_coverage":    coverage,
		"writeback_required": true,
	})
	contextFormat, _ := contextResponse["format_contract"].(map[string]any)
	contextValidation, _ := contextFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(contextValidation["status"])) != "passed" {
		t.Fatalf("expected context-pack validation passed, got %#v", contextValidation)
	}

	dream := attachPayloadFormatContract(dreamModeResponseContractID, map[string]any{
		"ok":             true,
		"mode":           "dream",
		"agent_id":       "codex_gpt5_test",
		"project":        "contextlattice",
		"goal":           "invent a better memory primitive",
		"query":          "invent a better memory primitive",
		"topic_path":     "contextlattice/dream-mode",
		"retrieval_mode": "balanced",
		"novelty_level":  3,
		"risk_tolerance": "balanced",
		"hypotheses":     []any{map[string]any{"id": "h1", "title": "t", "claim": "c", "supporting_evidence": []any{"e1"}}},
		"experiments":    []any{map[string]any{"id": "x1", "hypothesis_id": "h1", "method": "test"}},
		"evidence": map[string]any{
			"facts":     []any{map[string]any{"id": "e1", "text": "fact"}},
			"results":   []any{},
			"citations": []any{},
			"counts":    map[string]any{"facts": 1},
		},
		"source_coverage":    coverage,
		"llm":                map[string]any{"enabled": false, "used": false, "provider": "auto", "model": "qwen3.5:9b"},
		"writeback_required": true,
	}, "codex_gpt5_test", "dream", "/memory/dream")
	dreamFormat, _ := dream["format_contract"].(map[string]any)
	dreamValidation, _ := dreamFormat["validation"].(map[string]any)
	if strings.TrimSpace(anyToString(dreamValidation["status"])) != "passed" {
		t.Fatalf("expected dream validation passed, got %#v", dreamValidation)
	}

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
	pack := map[string]any{
		"facts":               items,
		"numeric_facts":       items,
		"citations":           items,
		"results":             items,
		"relevant_decisions":  items,
		"files_to_read":       items,
		"files_to_avoid":      items,
		"capabilities_to_use": items,
		"runbooks":            items,
		"known_failure_modes": items,
		"commands":            items,
		"acceptance_criteria": items,
	}
	payload := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       pack,
		"source_coverage":    map[string]any{"configured": items, "returned": items, "complete": true},
		"retrieval":          map[string]any{"debug": oversized},
		"writeback_required": true,
	})
	assertBoundaryContractPassed(t, contextPackResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, contextPackResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
	clippedPack, _ := payload["context_pack"].(map[string]any)
	if results, _ := clippedPack["results"].([]any); len(results) > agentBoundaryLimitsForContract(contextPackResponseContractID).MaxListItems {
		t.Fatalf("expected context_pack.results clipped, got %d", len(results))
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
		"ok":             true,
		"mode":           "dream",
		"agent_id":       "codex_gpt5_test",
		"project":        "contextlattice",
		"goal":           oversized,
		"query":          oversized,
		"topic_path":     "contextlattice/dream-mode",
		"retrieval_mode": "balanced",
		"novelty_level":  5,
		"risk_tolerance": "experimental",
		"hypotheses":     items,
		"experiments":    items,
		"evidence": map[string]any{
			"facts":     items,
			"results":   items,
			"citations": items,
			"combined":  items,
			"counts":    map[string]any{"facts": 140, "results": 140, "citations": 140},
		},
		"source_coverage":    map[string]any{"configured": items, "returned": items, "complete": true},
		"llm":                map[string]any{"enabled": true, "used": false, "provider": "ollama", "model": "qwen3.5:9b", "synthesis_text": oversized, "parsed": map[string]any{"hypotheses": items}},
		"retrieval":          map[string]any{"debug": oversized},
		"writeback":          map[string]any{"debug": oversized},
		"writeback_required": true,
	}, "codex_gpt5_test", "dream", "/memory/dream")
	assertBoundaryContractPassed(t, dreamModeResponseContractID, payload)
	assertBoundaryJSONUnderLimit(t, dreamModeResponseContractID, payload)
	assertNoRawProviderOverflowShape(t, payload)
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
	pack := attachContextPackFormatContract(map[string]any{
		"ok":                 true,
		"agent_id":           "codex_gpt5_test",
		"context_pack":       map[string]any{"facts": items, "results": items, "citations": items, "relevant_decisions": items, "files_to_read": items, "files_to_avoid": []any{}, "capabilities_to_use": []any{}, "runbooks": []any{}, "known_failure_modes": []any{}, "commands": []any{}, "acceptance_criteria": []any{}},
		"source_coverage":    map[string]any{"configured": []any{"fixture"}, "returned": []any{"fixture"}, "complete": true},
		"writeback_required": true,
	})
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", oversized, "balanced", pack, pack, nil, objectiveContext{})
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
		"policy_context_package": policy,
	})
	assertBoundaryContractPassed(t, agentPreflightResponseContractID, response)
	assertBoundaryJSONUnderLimit(t, agentPreflightResponseContractID, response)
	assertNoRawProviderOverflowShape(t, response)
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
