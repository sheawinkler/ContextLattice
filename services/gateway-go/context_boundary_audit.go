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
	surfaces := []contextBoundarySurface{
		{Name: "memory_context_pack", Path: "/memory/context-pack", Surface: "agent_http", ContractID: contextPackResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "prompt-ready context package"},
		{Name: "memory_agent_packet_reconstruct", Path: agentPacketReconstructionRoute, Surface: "agent_http", ContractID: agentPacketReconstructionContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "digest-verified Agent Packet delta reconstruction"},
		{Name: "agent_proof_timeline", Path: "/v1/agents/sessions/{session_id}/proof-timeline", Surface: "agent_http", ContractID: agentProofTimelineContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "deterministic exact-linked agent proof timeline"},
		{Name: "frontier_retrieval_receipt_governance", Path: frontierT4RetrievalReceiptGovernancePath, Surface: "operator_http", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "paid governance discovery; local retrieval receipts remain public"},
		{Name: "frontier_causal_bridge_governance", Path: frontierT4CausalBridgeGovernancePath, Surface: "operator_http", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "paid governance discovery; local causal proof remains public"},
		{Name: "frontier_continuous_counterfactual_eval", Path: frontierT4CounterfactualEvalPath, Surface: "operator_http", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "paid operations discovery; local ablation remains public"},
		{Name: "frontier_evidence_reputation_activation", Path: frontierT4EvidenceReputationPath, Surface: "operator_http", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "paid activation discovery; local reputation remains advisory"},
		{Name: "frontier_continuous_retrieval_regression", Path: frontierT4RetrievalRegressionPath, Surface: "operator_http", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "paid scheduling discovery; local regression derivation remains review-only"},
		{Name: "frontier_adversarial_defense_operations", Path: frontierT4DefenseOperationsPath, Surface: "operator_http", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "paid operations discovery without a public-defense bypass"},
		{Name: "tools_context_pack", Path: "/tools/context_pack", Surface: "agent_tool", ContractID: contextPackResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "MCP/tool context package wrapper"},
		{Name: "task_identity_reconciliation", Path: "/memory/continuity/reconcile", Surface: "agent_http", ContractID: taskIdentityReconciliationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "exact-first task identity reconciliation with semantic abstention"},
		{Name: "task_identity_receipt", Path: "task_identity_receipt", Surface: "contract", ContractID: taskIdentityReceiptContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "immutable manual merge, split, and creation receipt"},
		{Name: "objective_transition", Path: "/memory/objectives/transition", Surface: "agent_http", ContractID: objectiveTransitionContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "append-only typed objective transition"},
		{Name: "objective_graph", Path: "/memory/objectives/graph", Surface: "agent_http", ContractID: objectiveGraphContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "as-of longitudinal objective graph"},
		{Name: "decision_change", Path: "/memory/decision-changes", Surface: "agent_http", ContractID: decisionChangeContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "evidence-linked decision change write"},
		{Name: "decision_change_query", Path: "/memory/decision-changes", Surface: "agent_http", ContractID: decisionChangeQueryContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded decision provenance query"},
		{Name: "memory_synthesis_pack_v2", Path: "/memory/synthesis-pack/v2", Surface: "agent_http", ContractID: synthesisPackV2ContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "proof-carrying deterministic synthesis"},
		{Name: "tools_synthesis_pack_v2", Path: "/tools/synthesis_pack_v2", Surface: "agent_tool", ContractID: synthesisPackV2ContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool proof-carrying synthesis wrapper"},
		{Name: "memory_retrieval_plan", Path: "/memory/retrieval/plan", Surface: "agent_http", ContractID: retrievalPlanContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "advisor-only adaptive retrieval plan"},
		{Name: "tools_retrieval_plan", Path: "/tools/retrieval_plan", Surface: "agent_tool", ContractID: retrievalPlanContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool retrieval-plan wrapper"},
		{Name: "memory_claim_write", Path: "/memory/claims", Surface: "agent_http", ContractID: temporalClaimContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "structured temporal claim write"},
		{Name: "tools_claim_write", Path: "/tools/claim_write", Surface: "agent_tool", ContractID: temporalClaimContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool temporal claim write wrapper"},
		{Name: "memory_claim_query", Path: "/memory/claims/query", Surface: "agent_http", ContractID: temporalClaimQueryContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "time-aware structured claim query"},
		{Name: "tools_claim_query", Path: "/tools/claim_query", Surface: "agent_tool", ContractID: temporalClaimQueryContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool temporal claim query wrapper"},
		{Name: "memory_context_policy_candidate", Path: "/memory/context-policy/candidate", Surface: "agent_http", ContractID: contextPolicyCandidateContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "outcome-trained advisory policy candidate"},
		{Name: "tools_context_policy_candidate", Path: "/tools/context_policy_candidate", Surface: "agent_tool", ContractID: contextPolicyCandidateContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool advisory policy candidate"},
		{Name: "memory_context_policy_evaluate", Path: "/memory/context-policy/evaluate", Surface: "agent_http", ContractID: contextPolicyEvaluationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "one-step shadow and canary evaluation"},
		{Name: "tools_context_policy_evaluate", Path: "/tools/context_policy_evaluate", Surface: "agent_tool", ContractID: contextPolicyEvaluationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool one-step policy evaluation"},
		{Name: "memory_skill_foundry_draft", Path: "/memory/skills/foundry/draft", Surface: "agent_http", ContractID: skillDraftContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "verified workflow skill draft"},
		{Name: "tools_skill_foundry_draft", Path: "/tools/skill_foundry_draft", Surface: "agent_tool", ContractID: skillDraftContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool verified workflow skill draft"},
		{Name: "memory_skill_foundry_evaluate", Path: "/memory/skills/foundry/evaluate", Surface: "agent_http", ContractID: skillEvaluationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "independent skill holdout evaluation"},
		{Name: "tools_skill_foundry_evaluate", Path: "/tools/skill_foundry_evaluate", Surface: "agent_tool", ContractID: skillEvaluationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool independent skill holdout evaluation"},
		{Name: "memory_skill_foundry_export", Path: "/memory/skills/foundry/export", Surface: "agent_http", ContractID: skillExportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "human-approved inactive skill export"},
		{Name: "memory_skill_foundry_retire", Path: "/memory/skills/foundry/retire", Surface: "agent_http", ContractID: skillRetirementContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "immutable inactive-draft retirement"},
		{Name: "tools_skill_foundry_export", Path: "/tools/skill_foundry_export", Surface: "agent_tool", ContractID: skillExportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "tool human-approved inactive skill export"},
		{Name: "memory_context_passport_export", Path: "/memory/context-passport/export", Surface: "agent_http", ContractID: contextPassportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "signed bounded context passport export"},
		{Name: "memory_context_passport_verify", Path: "/memory/context-passport/verify", Surface: "agent_http", ContractID: contextPassportVerifyContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "passport signature, digest, and expiry verification"},
		{Name: "memory_context_passport_diff", Path: "/memory/context-passport/diff", Surface: "agent_http", ContractID: contextPassportDiffContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "deterministic passport revision diff"},
		{Name: "memory_context_passport_replay", Path: "/memory/context-passport/replay", Surface: "agent_http", ContractID: contextPassportReplayContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "validated non-executing passport replay plan"},
		{Name: "memory_context_passport_import", Path: "/memory/context-passport/import", Surface: "agent_http", ContractID: contextPassportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "explicit verified passport import"},
		{Name: "memory_context_mesh_grants", Path: "/memory/context-mesh/grants", Surface: "agent_http", ContractID: contextMeshGrantContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "signed project-scoped recipient grant"},
		{Name: "memory_context_mesh_grant_revoke", Path: "/memory/context-mesh/grants/revoke", Surface: "agent_http", ContractID: contextMeshRevocationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "signed local revocation tombstone"},
		{Name: "memory_context_mesh_export", Path: "/memory/context-mesh/export", Surface: "agent_http", ContractID: contextMeshEnvelopeContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "transport-neutral X25519 encrypted envelope"},
		{Name: "memory_context_mesh_import", Path: "/memory/context-mesh/import", Surface: "agent_http", ContractID: contextMeshImportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "verified dry-run-first mesh reconciliation"},
		{Name: "memory_review", Path: "/memory/review", Surface: "agent_http", ContractID: reviewModeResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "deterministic repeat-pattern review mode"},
		{Name: "tools_review", Path: "/tools/review", Surface: "agent_tool", ContractID: reviewModeResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "MCP/tool review mode wrapper"},
		{Name: "agents_preflight", Path: "/v1/agents/preflight", Surface: "agent_http", ContractID: agentPreflightResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "generic agent preflight"},
		{Name: "codex_preflight", Path: "/v1/codex/preflight", Surface: "agent_http", ContractID: agentPreflightResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "Codex-compatible preflight"},
		{Name: "policy_context_package", Path: "policy_context_package", Surface: "contract", ContractID: policyContextPackageContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "policy/anti-scheming context package"},
		{Name: "contextlattice_pack_cli", Path: "scripts/agent/contextlattice-pack", Surface: "agent_cli", ContractID: contextPackResponseContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "local CLI context package"},
		{Name: "contextlattice_packet_reconstruct_cli", Path: "contextlattice_packet_reconstruct", Surface: "agent_cli", ContractID: agentPacketContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI verified Agent Packet reconstruction"},
		{Name: "contextlattice_agent_trace_proof_cli", Path: "contextlattice_agent_trace --proof", Surface: "agent_cli", ContractID: agentProofTimelineContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI deterministic local agent proof timeline"},
		{Name: "contextlattice_synthesis_pack_v2_cli", Path: "contextlattice_synthesis_pack_v2", Surface: "agent_cli", ContractID: synthesisPackV2ContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI proof-carrying synthesis"},
		{Name: "contextlattice_retrieval_plan_cli", Path: "contextlattice_retrieval_plan", Surface: "agent_cli", ContractID: retrievalPlanContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI advisor-only retrieval plan"},
		{Name: "contextlattice_retrieval_governance_cli", Path: "contextlattice_retrieval_governance", Surface: "operator_cli", ContractID: frontierT4RetrievalGovernanceContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI paid-governance discovery without local defense bypass"},
		{Name: "memory_policy_simulation", Path: policySimulationPath, Surface: "agent_http", ContractID: policySimulationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "same-snapshot no-persist policy replay"},
		{Name: "memory_scoped_policy_card", Path: scopedPolicyCardPath, Surface: "agent_http", ContractID: scopedPolicyCardContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "project and task-scoped sparse-data policy card"},
		{Name: "memory_policy_promotion", Path: policyPromotionRecommendationPath, Surface: "agent_http", ContractID: policyPromotionRecommendationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "assignment and uncertainty-bound promotion recommendation"},
		{Name: "memory_retirement", Path: memoryRetirementPath, Surface: "agent_http", ContractID: memoryRetirementContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "non-destructive reversible memory lifecycle receipt"},
		{Name: "memory_contradiction_resolution", Path: contradictionResolutionPath, Surface: "agent_http", ContractID: contradictionResolutionContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "evidence-weighted contradiction workflow"},
		{Name: "memory_storage_temperature", Path: storageTemperatureDecisionPath, Surface: "agent_http", ContractID: storageTemperatureDecisionContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "reversible retrieval temperature decision"},
		{Name: "contextlattice_policy_lab_cli", Path: "contextlattice_policy_lab", Surface: "agent_cli", ContractID: frontierT5StatusContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI-primary Policy Laboratory surface"},
		{Name: "frontier_t6_steering", Path: frontierT6SteeringPath, Surface: "agent_http", ContractID: frontierT6SteeringDeliverySchemaID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded async steering publish, replay, acknowledgement, and pull fallback"},
		{Name: "frontier_t6_steering_events", Path: frontierT6SteeringEventsPath, Surface: "agent_sse", ContractID: frontierT6SteeringDeliverySchemaID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "resumable event-ID steering stream for capable CLI clients"},
		{Name: "frontier_t6_selection", Path: frontierT6SelectionPath, Surface: "agent_http", ContractID: frontierT6RunnerSelectionSchemaID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "advisor-only runner and model selection receipts"},
		{Name: "frontier_t6_profile", Path: frontierT6ProfilePath, Surface: "agent_http", ContractID: frontierT6ContextProfileSchemaID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "effective agent context profile resolution"},
		{Name: "frontier_t6_context_prep", Path: frontierT6ContextPrepPath, Surface: "agent_http", ContractID: frontierT6ContextPrepSchemaID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "opt-in external-worker context preparation"},
		{Name: "frontier_t6_telemetry", Path: frontierT6TelemetryPath, Surface: "telemetry", ContractID: frontierT6StateSchemaID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded Agent Fit state without local path disclosure"},
		{Name: "contextlattice_claim_write_cli", Path: "contextlattice_claim_write", Surface: "agent_cli", ContractID: temporalClaimContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI temporal claim write"},
		{Name: "contextlattice_claim_query_cli", Path: "contextlattice_claim_query", Surface: "agent_cli", ContractID: temporalClaimQueryContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI temporal claim query"},
		{Name: "contextlattice_continuity_reconcile_cli", Path: "contextlattice_continuity_reconcile", Surface: "agent_cli", ContractID: taskIdentityReconciliationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI exact-first task identity reconciliation"},
		{Name: "contextlattice_objective_transition_cli", Path: "contextlattice_objective_transition", Surface: "agent_cli", ContractID: objectiveTransitionContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI append-only objective transition"},
		{Name: "contextlattice_objective_graph_cli", Path: "contextlattice_objective_graph", Surface: "agent_cli", ContractID: objectiveGraphContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI as-of objective graph"},
		{Name: "contextlattice_decision_change_cli", Path: "contextlattice_decision_change", Surface: "agent_cli", ContractID: decisionChangeContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI evidence-linked decision change write"},
		{Name: "contextlattice_decision_change_query_cli", Path: "contextlattice_decision_change list", Surface: "agent_cli", ContractID: decisionChangeQueryContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI bounded decision provenance query"},
		{Name: "contextlattice_policy_candidate_cli", Path: "contextlattice_policy_candidate", Surface: "agent_cli", ContractID: contextPolicyCandidateContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI advisory policy candidate"},
		{Name: "contextlattice_policy_evaluate_cli", Path: "contextlattice_policy_evaluate", Surface: "agent_cli", ContractID: contextPolicyEvaluationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI one-step policy evaluation"},
		{Name: "contextlattice_skill_draft_cli", Path: "contextlattice_skill_draft", Surface: "agent_cli", ContractID: skillDraftContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI verified workflow skill draft"},
		{Name: "contextlattice_skill_evaluate_cli", Path: "contextlattice_skill_evaluate", Surface: "agent_cli", ContractID: skillEvaluationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI independent skill holdout evaluation"},
		{Name: "contextlattice_skill_export_cli", Path: "contextlattice_skill_export", Surface: "agent_cli", ContractID: skillExportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI human-approved inactive skill export"},
		{Name: "contextlattice_skill_retire_cli", Path: "contextlattice_skill_retire", Surface: "agent_cli", ContractID: skillRetirementContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI immutable inactive-draft retirement"},
		{Name: "contextlattice_passport_export_cli", Path: "contextlattice_passport_export", Surface: "agent_cli", ContractID: contextPassportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI signed passport export"},
		{Name: "contextlattice_passport_verify_cli", Path: "contextlattice_passport_verify", Surface: "agent_cli", ContractID: contextPassportVerifyContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI passport verification"},
		{Name: "contextlattice_passport_diff_cli", Path: "contextlattice_passport_diff", Surface: "agent_cli", ContractID: contextPassportDiffContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI passport diff"},
		{Name: "contextlattice_passport_replay_cli", Path: "contextlattice_passport_replay", Surface: "agent_cli", ContractID: contextPassportReplayContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI validated replay plan"},
		{Name: "contextlattice_passport_import_cli", Path: "contextlattice_passport_import", Surface: "agent_cli", ContractID: contextPassportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI explicit passport import"},
		{Name: "contextlattice_mesh_grant_cli", Path: "contextlattice_mesh_grant", Surface: "agent_cli", ContractID: contextMeshGrantContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI project-scoped mesh grants"},
		{Name: "contextlattice_mesh_grant_revoke_cli", Path: "contextlattice_mesh_grant revoke", Surface: "agent_cli", ContractID: contextMeshRevocationContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI signed local revocation tombstone"},
		{Name: "contextlattice_mesh_export_cli", Path: "contextlattice_mesh_export", Surface: "agent_cli", ContractID: contextMeshEnvelopeContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI encrypted mesh envelope export"},
		{Name: "contextlattice_mesh_import_cli", Path: "contextlattice_mesh_import", Surface: "agent_cli", ContractID: contextMeshImportContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "CLI dry-run or explicit mesh import"},
		{Name: "async_inbox_drain_cli", Path: "contextlattice_async_inbox_drain", Surface: "agent_cli", ContractID: "async_inbox_drain_output.v1", RuntimeOwner: sourceOwnerGoNative, Required: true, LocalMaxJSONBytes: 4000, LocalMaxString: 1200, LocalMaxListItems: 8, Detail: "bounded async continuation inbox drain output"},
		{Name: "precompact_stdout", Path: "scripts/agent_hooks/contextlattice_pre_compaction_write.sh", Surface: "agent_hook", ContractID: codexCompactHookStdoutContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "Codex PreCompact stdout wrapper"},
		{Name: "postcompact_stdout", Path: "scripts/agent_hooks/contextlattice_post_compaction_read.sh", Surface: "agent_hook", ContractID: codexCompactHookStdoutContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "Codex PostCompact stdout wrapper"},
		{Name: "compaction_handoff_payload", Path: "scripts/agent/compaction-handoff-payload", Surface: "agent_hook", ContractID: "local_compaction_handoff_budget.v1", RuntimeOwner: sourceOwnerGoNative, Required: true, LocalMaxJSONBytes: 12000, LocalMaxString: 4000, LocalMaxListItems: 64, Detail: "compact handoff JSON budget before hook writeback"},
		{Name: "agent_session_rollup", Path: "/v1/agents/sessions/{session_id}/rollup", Surface: "agent_http", ContractID: agentSessionRollupContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded session rollup"},
		{Name: "agent_prompt_context_package", Path: "/v1/agents/sessions/{session_id}/context-package", Surface: "agent_http", ContractID: agentPromptContextPackageContractID, RuntimeOwner: sourceOwnerGoNative, Required: true, Detail: "bounded session prompt package"},
	}
	return surfaces
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
		"build":                contextLatticeBuildIdentity(),
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
