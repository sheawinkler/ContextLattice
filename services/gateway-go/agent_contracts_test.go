package main

import (
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
