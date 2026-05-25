package main

import (
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
	policy := buildPolicyContextPackage("codex", "codex_gpt5_test", "contextlattice", "runbooks/codex-integration", "preflight", "fast", pack, pack, nil)
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

func TestAgentContractTelemetryRecordsValidationReasons(t *testing.T) {
	before := anyToInt(agentContractTelemetrySnapshot()["total"], 0)
	recordAgentContractBoundary("codex_gpt5_test", writebackResultContractID, "writeback", "/memory/write", []map[string]any{{"reason": "missing_required_field"}})
	snapshot := agentContractTelemetrySnapshot()
	after := anyToInt(snapshot["total"], 0)
	if after <= before {
		t.Fatalf("expected telemetry total to increase, before=%d after=%d snapshot=%#v", before, after, snapshot)
	}
}
