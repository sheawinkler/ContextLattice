package main

import (
	"net/http"
	"sort"
)

type contextBoundarySurface struct {
	Name              string
	Path              string
	Surface           string
	ContractID        string
	RuntimeOwner      string
	Required          bool
	LocalMaxJSONBytes int
	LocalMaxString    int
	LocalMaxListItems int
	Detail            string
}

func contextBoundaryRequiredSurfaces() []contextBoundarySurface {
	return []contextBoundarySurface{
		{Name: "memory_context_pack", Path: "/memory/context-pack", Surface: "agent_http", ContractID: contextPackResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "prompt-ready context package"},
		{Name: "tools_context_pack", Path: "/tools/context_pack", Surface: "agent_tool", ContractID: contextPackResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "MCP/tool context package wrapper"},
		{Name: "agents_preflight", Path: "/v1/agents/preflight", Surface: "agent_http", ContractID: agentPreflightResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "generic agent preflight"},
		{Name: "codex_preflight", Path: "/v1/codex/preflight", Surface: "agent_http", ContractID: agentPreflightResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "Codex-compatible preflight"},
		{Name: "policy_context_package", Path: "policy_context_package", Surface: "contract", ContractID: policyContextPackageContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "policy/anti-scheming context package"},
		{Name: "contextlattice_pack_cli", Path: "scripts/agent/contextlattice-pack", Surface: "agent_cli", ContractID: contextPackResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "local CLI context package"},
		{Name: "precompact_stdout", Path: "scripts/agent_hooks/contextlattice_pre_compaction_write.sh", Surface: "agent_hook", ContractID: codexCompactHookStdoutContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "Codex PreCompact stdout wrapper"},
		{Name: "postcompact_stdout", Path: "scripts/agent_hooks/contextlattice_post_compaction_read.sh", Surface: "agent_hook", ContractID: codexCompactHookStdoutContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "Codex PostCompact stdout wrapper"},
		{Name: "compaction_handoff_payload", Path: "scripts/agent/compaction-handoff-payload", Surface: "agent_hook", ContractID: "local_compaction_handoff_budget.v1", RuntimeOwner: sourceOwnerGoNative, Required: true, LocalMaxJSONBytes: 12000, LocalMaxString: 4000, LocalMaxListItems: 64, Detail: "compact handoff JSON budget before hook writeback"},
		{Name: "agent_session_rollup", Path: "/v1/agents/sessions/{session_id}/rollup", Surface: "agent_http", ContractID: agentSessionRollupContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded session rollup"},
		{Name: "agent_prompt_context_package", Path: "/v1/agents/sessions/{session_id}/context-package", Surface: "agent_http", ContractID: agentPromptContextPackageContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded session prompt package"},
	}
}

func contextBoundaryPayload() map[string]any {
	registry, err := loadAgentContractsRegistry()
	rows := make([]map[string]any, 0, len(contextBoundaryRequiredSurfaces()))
	violations := []map[string]any{}
	bySurface := map[string]int{}
	for _, surface := range contextBoundaryRequiredSurfaces() {
		limits := agentBoundaryLimits{}
		contractPresent := false
		contractVersion := 1
		payloadKind := "local_budget"
		if surface.LocalMaxJSONBytes > 0 || surface.LocalMaxString > 0 || surface.LocalMaxListItems > 0 {
			contractPresent = true
			limits = agentBoundaryLimits{
				MaxTotalJSONBytes: surface.LocalMaxJSONBytes,
				MaxStringBytes:    surface.LocalMaxString,
				MaxListItems:      surface.LocalMaxListItems,
			}
		} else if err == nil {
			contract := agentContract(registry, surface.ContractID)
			if contract != nil {
				contractPresent = true
				contractVersion = anyToInt(contract["contract_version"], 1)
				payloadKind = anyToString(contract["payload_kind"])
				limits = agentBoundaryLimitsFromContract(contract)
			}
		}
		bounded := contractPresent && limits.MaxTotalJSONBytes > 0 && limits.MaxStringBytes > 0 && limits.MaxListItems > 0
		row := map[string]any{
			"name":                 surface.Name,
			"path":                 surface.Path,
			"surface":              surface.Surface,
			"runtimeOwner":         surface.RuntimeOwner,
			"required":             surface.Required,
			"detail":               surface.Detail,
			"contract_id":          surface.ContractID,
			"contract_present":     contractPresent,
			"contract_version":     contractVersion,
			"payload_kind":         payloadKind,
			"bounded":              bounded,
			"max_total_json_bytes": limits.MaxTotalJSONBytes,
			"max_string_bytes":     limits.MaxStringBytes,
			"max_list_items":       limits.MaxListItems,
			"metadata_fields": []any{
				"contract_valid",
				"truncated",
				"omitted_counts",
				"actual_json_bytes",
				"max_total_json_bytes",
				"max_string_bytes",
				"max_list_items",
			},
		}
		rows = append(rows, row)
		bySurface[surface.Surface]++
		if surface.Required && !bounded {
			violation := cloneContractMap(row)
			if err != nil && surface.LocalMaxJSONBytes == 0 {
				violation["reason"] = "registry_unavailable"
				violation["detail"] = err.Error()
			} else if !contractPresent {
				violation["reason"] = "contract_missing"
			} else {
				violation["reason"] = "boundary_limits_missing"
			}
			violations = append(violations, violation)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return anyToString(rows[i]["surface"])+":"+anyToString(rows[i]["path"]) < anyToString(rows[j]["surface"])+":"+anyToString(rows[j]["path"])
	})
	ok := len(violations) == 0
	registryID := "contextlattice_agent_output_contracts"
	registryVersion := 0
	if err == nil {
		registryID = registry.RegistryID
		registryVersion = registry.RegistryVersion
	}
	status := "bounded"
	if !ok {
		status = "boundary_violation"
	}
	return map[string]any{
		"ok":                   ok,
		"schema_id":            "contextlattice_context_boundary.v1",
		"generatedAt":          nowUTCISO(),
		"status":               status,
		"registry_id":          registryID,
		"registry_version":     registryVersion,
		"validator":            "contextlattice.boundary.v1",
		"requiredSurfaceCount": len(rows),
		"boundedSurfaceCount":  len(rows) - len(violations),
		"violationCount":       len(violations),
		"surfaces":             bySurface,
		"routes":               rows,
		"violations":           violations,
		"forbidden_error_markers": []string{
			"array_above_max_length",
			"context length exceeded",
			"maximum context length",
			"max context length",
			"input array is too long",
			"oversized input",
		},
		"agent_ready": map[string]any{
			"ok":             ok,
			"recommended":    "Agents can rely on ContextLattice only when ok=true and violationCount=0.",
			"doctor_command": "contextlattice_context_boundary --pretty",
		},
	}
}

func (s *server) opsContextBoundary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, contextBoundaryPayload())
}
