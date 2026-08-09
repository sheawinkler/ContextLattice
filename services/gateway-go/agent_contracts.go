package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const agentContractsRegistryEnv = "CONTEXTLATTICE_AGENT_CONTRACTS_PATH"
const policyContextPackageContractID = "policy_context_package.v1"
const objectiveRuntimeStateContractID = "objective_runtime_state.v1"
const antiSchemingContractID = "anti_scheming_protocol.v1"
const agentPreflightResponseContractID = "agent_preflight_response.v1"
const contextPackResponseContractID = "context_pack_response.v1"
const recallResponseContractID = "recall_response.v1"
const synthesisPackContractID = "synthesis_pack.v1"
const dreamModeResponseContractID = "dream_mode_response.v1"
const reviewModeResponseContractID = "review_mode_response.v1"
const writebackResultContractID = "writeback_result.v1"
const codexCompactHookStdoutContractID = "codex_compact_hook_stdout.v1"
const agentTaskResultContractID = "agent_task_result.v1"
const contractAcknowledgementContractID = "contract_acknowledgement.v1"
const agentSpanContractID = "agent_span.v1"
const agentFlightRecorderEventContractID = "agent_flight_recorder_event.v1"
const a2aReadinessProfileContractID = "a2a_readiness_profile.v1"
const agentSessionRollupContractID = "agent_session_rollup.v1"
const agentPromptContextPackageContractID = "agent_prompt_context_package.v1"
const agentRunTraceContractID = "agent_run_trace.v1"
const agentProofTimelineContractID = "agent_proof_timeline.v1"
const agentPacketDeltaOutputContractID = "agent_packet_delta.v1"
const agentPacketReconstructionOutputContractID = "agent_packet_reconstruction.v1"
const continuousCognitionContractID = "continuous_cognition.v1"
const runAdvisorContractID = "run_advisor.v1"
const retrievalProgressContractID = "retrieval_progress.v1"
const steeringCommentContractID = "steering_comment.v1"

type agentContractsRegistry struct {
	RegistryID       string                    `json:"registry_id"`
	RegistryVersion  int                       `json:"registry_version"`
	DefaultValidator string                    `json:"default_validator"`
	SharedFragments  map[string]any            `json:"shared_fragments"`
	Contracts        map[string]map[string]any `json:"contracts"`
	SourcePath       string                    `json:"-"`
}

var agentContractsOnce sync.Once
var agentContractsCache agentContractsRegistry
var agentContractsErr error

type agentContractTelemetryCounter struct {
	AgentID  string `json:"agent_id"`
	SchemaID string `json:"schema_id"`
	Lane     string `json:"lane"`
	Endpoint string `json:"endpoint"`
	Reason   string `json:"reason"`
	Count    int    `json:"count"`
	LastAt   string `json:"last_at"`
}

var agentContractTelemetryMu sync.Mutex
var agentContractTelemetryCounters = map[string]agentContractTelemetryCounter{}

func loadAgentContractsRegistry() (agentContractsRegistry, error) {
	agentContractsOnce.Do(func() {
		agentContractsCache, agentContractsErr = readAgentContractsRegistry()
	})
	return agentContractsCache, agentContractsErr
}

func readAgentContractsRegistry() (agentContractsRegistry, error) {
	path, raw, err := readAgentContractsRegistryBytes()
	if err != nil {
		return agentContractsRegistry{}, err
	}
	var registry agentContractsRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		return agentContractsRegistry{}, err
	}
	if registry.RegistryID == "" {
		registry.RegistryID = "contextlattice_agent_output_contracts"
	}
	if registry.RegistryVersion == 0 {
		registry.RegistryVersion = 1
	}
	if registry.DefaultValidator == "" {
		registry.DefaultValidator = "contextlattice.boundary.v1"
	}
	if len(registry.Contracts) == 0 {
		return agentContractsRegistry{}, errAgentContract("registry_missing_contracts")
	}
	registry.SourcePath = path
	return registry, nil
}

func readAgentContractsRegistryBytes() (string, []byte, error) {
	candidates := []string{}
	if override := strings.TrimSpace(os.Getenv(agentContractsRegistryEnv)); override != "" {
		candidates = append(candidates, override)
	}
	candidates = append(candidates,
		"config/agent_contracts/agent_output_contracts.json",
		"../config/agent_contracts/agent_output_contracts.json",
		"../../config/agent_contracts/agent_output_contracts.json",
		"/app/config/agent_contracts/agent_output_contracts.json",
	)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		path := strings.TrimSpace(candidate)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if !filepath.IsAbs(path) {
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
		}
		raw, err := os.ReadFile(path)
		if err == nil {
			return path, raw, nil
		}
	}
	return "", nil, errAgentContract("agent_contract_registry_not_found")
}

type agentContractError string

func (e agentContractError) Error() string { return string(e) }

func errAgentContract(reason string) error { return agentContractError(reason) }

func agentContract(registry agentContractsRegistry, contractID string) map[string]any {
	if registry.Contracts == nil {
		return nil
	}
	contract := registry.Contracts[contractID]
	if contract == nil {
		return nil
	}
	return contract
}

func antiSchemingProtocol() map[string]any {
	registry, err := loadAgentContractsRegistry()
	if err == nil {
		if fragments, ok := registry.SharedFragments["anti_scheming_protocol"].(map[string]any); ok {
			return cloneContractMap(fragments)
		}
	}
	return map[string]any{
		"version": "2026-05-24",
		"law":     "Change conclusions to match evidence; never change evidence to support conclusions.",
		"required_steps": []any{
			"state_objective_constraints_and_evidence",
			"separate_observed_facts_from_inference",
			"choose_smallest_useful_action_plan",
			"verify_with_matching_checks_or_explicit_inspection",
			"surface_material_failures_uncertainty_and_missing_checks",
		},
		"red_flags": []any{
			"claiming_passed_checks_without_running_or_inspecting_them",
			"editing_artifacts_to_make_review_easier_rather_than_truer",
			"replacing_the_user_goal_with_a_convenient_proxy",
			"overstating_confidence_from_thin_evidence",
		},
		"delivery": []any{
			"findings_before_reassurance",
			"exact_verification_state",
			"explicit_blockers_and_residual_risk",
		},
	}
}

func contractMetadata(contractID string) map[string]any {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		return map[string]any{
			"registry_id":          "contextlattice_agent_output_contracts",
			"registry_version":     0,
			"schema_id":            contractID,
			"contract_version":     0,
			"required_output_mode": "json_object",
			"validator":            "contextlattice.boundary.v1",
			"forbidden_fields":     []any{},
			"contract_valid":       false,
			"truncated":            false,
			"omitted_counts":       agentBoundaryOmittedCounts(agentBoundaryStats{}),
			"validation": map[string]any{
				"status": "failed",
				"errors": []any{map[string]any{"reason": "registry_unavailable", "detail": err.Error()}},
			},
		}
	}
	contract := agentContract(registry, contractID)
	contractVersion := anyToInt(contract["contract_version"], 0)
	if contractVersion == 0 {
		contractVersion = 1
	}
	mode := strings.TrimSpace(anyToString(contract["required_output_mode"]))
	if mode == "" {
		mode = "json_object"
	}
	metadata := map[string]any{
		"registry_id":          registry.RegistryID,
		"registry_version":     registry.RegistryVersion,
		"schema_id":            contractID,
		"contract_version":     contractVersion,
		"required_output_mode": mode,
		"validator":            registry.DefaultValidator,
		"forbidden_fields":     agentContractStringList(contract["forbidden_fields"]),
		"contract_valid":       false,
		"truncated":            false,
		"omitted_counts":       agentBoundaryOmittedCounts(agentBoundaryStats{}),
		"validation": map[string]any{
			"status": "pending",
			"errors": []any{},
		},
	}
	limits := agentBoundaryLimitsFromContract(contract)
	if limits.MaxTotalJSONBytes > 0 {
		metadata["max_total_json_bytes"] = limits.MaxTotalJSONBytes
	}
	if limits.MaxStringBytes > 0 {
		metadata["max_string_bytes"] = limits.MaxStringBytes
	}
	if limits.MaxListItems > 0 {
		metadata["max_list_items"] = limits.MaxListItems
	}
	if maxBytesByPath, ok := contract["max_bytes_by_path"].(map[string]any); ok && len(maxBytesByPath) > 0 {
		metadata["max_bytes_by_path"] = cloneContractMap(maxBytesByPath)
	}
	return metadata
}

func stampContractValidation(metadata map[string]any, findings []map[string]any, stats agentBoundaryStats, payload map[string]any, metadataKey string) map[string]any {
	stamped := cloneContractMap(metadata)
	status := "passed"
	if len(findings) > 0 {
		status = "failed"
	}
	trimmed := make([]any, 0, minInt(len(findings), 12))
	for i, finding := range findings {
		if i >= 12 {
			break
		}
		trimmed = append(trimmed, cloneContractMap(finding))
	}
	stamped["validation"] = map[string]any{
		"status": status,
		"errors": trimmed,
	}
	stamped["contract_valid"] = len(findings) == 0
	stamped["truncated"] = agentBoundaryStatsTruncated(stats)
	stamped["omitted_counts"] = agentBoundaryOmittedCounts(stats)
	if stats.JSONBytesBefore > 0 {
		stamped["json_bytes_before_boundary"] = stats.JSONBytesBefore
	}
	if stats.JSONBytesAfter > 0 {
		stamped["json_bytes_after_boundary"] = stats.JSONBytesAfter
	}
	if payload != nil && metadataKey != "" {
		probe := cloneContractMap(payload)
		probe[metadataKey] = stamped
		if size := jsonByteLen(probe); size > 0 {
			stamped["actual_json_bytes"] = size
		}
	} else if stats.JSONBytesAfter > 0 {
		stamped["actual_json_bytes"] = stats.JSONBytesAfter
	}
	return stamped
}

func preflightContractsSummary(findings []map[string]any, stats agentBoundaryStats, payload map[string]any) map[string]any {
	registry, err := loadAgentContractsRegistry()
	registryID := "contextlattice_agent_output_contracts"
	registryVersion := 0
	contractIDs := []any{
		agentPreflightResponseContractID,
		policyContextPackageContractID,
		objectiveRuntimeStateContractID,
		antiSchemingContractID,
		contextPackResponseContractID,
		recallResponseContractID,
		dreamModeResponseContractID,
		reviewModeResponseContractID,
		writebackResultContractID,
		codexCompactHookStdoutContractID,
		agentTaskResultContractID,
		contractAcknowledgementContractID,
		agentSpanContractID,
		agentFlightRecorderEventContractID,
		a2aReadinessProfileContractID,
		agentSessionRollupContractID,
		agentPromptContextPackageContractID,
		agentRunTraceContractID,
		agentProofTimelineContractID,
		continuousCognitionContractID,
		runAdvisorContractID,
		retrievalProgressContractID,
		steeringCommentContractID,
	}
	if err == nil {
		registryID = registry.RegistryID
		registryVersion = registry.RegistryVersion
		relevant := make([]any, 0, len(contractIDs))
		for _, rawID := range contractIDs {
			contractID := strings.TrimSpace(anyToString(rawID))
			if _, exists := registry.Contracts[contractID]; exists {
				relevant = append(relevant, contractID)
			}
		}
		contractIDs = relevant
	}
	status := "passed"
	if err != nil || len(findings) > 0 {
		status = "failed"
	}
	errors := make([]any, 0, minInt(len(findings)+1, 12))
	if err != nil {
		errors = append(errors, map[string]any{"reason": "registry_unavailable", "detail": err.Error()})
	}
	for i, finding := range findings {
		if len(errors) >= 12 || i >= 12 {
			break
		}
		errors = append(errors, cloneContractMap(finding))
	}
	summary := map[string]any{
		"registry_id":      registryID,
		"registry_version": registryVersion,
		"contracts":        contractIDs,
		"max_total_json_bytes": func() int {
			limits := agentBoundaryLimitsForContract(agentPreflightResponseContractID)
			return limits.MaxTotalJSONBytes
		}(),
		"max_string_bytes": func() int {
			limits := agentBoundaryLimitsForContract(agentPreflightResponseContractID)
			return limits.MaxStringBytes
		}(),
		"max_list_items": func() int {
			limits := agentBoundaryLimitsForContract(agentPreflightResponseContractID)
			return limits.MaxListItems
		}(),
		"contract_valid": len(findings) == 0 && err == nil,
		"truncated":      agentBoundaryStatsTruncated(stats),
		"omitted_counts": agentBoundaryOmittedCounts(stats),
		"validation": map[string]any{
			"status": status,
			"errors": errors,
		},
	}
	if stats.JSONBytesBefore > 0 {
		summary["json_bytes_before_boundary"] = stats.JSONBytesBefore
	}
	if stats.JSONBytesAfter > 0 {
		summary["json_bytes_after_boundary"] = stats.JSONBytesAfter
	}
	if payload != nil {
		probe := cloneContractMap(payload)
		probe["format_contracts"] = summary
		if size := jsonByteLen(probe); size > 0 {
			summary["actual_json_bytes"] = size
		}
	}
	return summary
}

func attachPayloadFormatContract(contractID string, payload map[string]any, agentID string, lane string, endpoint string) map[string]any {
	normalizeAgentContractPayloadMapInPlace(payload)
	metadata := contractMetadata(contractID)
	stats := agentBoundaryStatsFromMetadata(payload["format_contract"])
	payload["format_contract"] = metadata
	findings := []map[string]any{}
	for pass := 0; pass < 5; pass++ {
		stats = mergeAgentBoundaryStats(stats, enforceAgentBoundaryContract(contractID, payload))
		findings = validateAgentContractPayload(contractID, payload)
		payload["format_contract"] = stampContractValidation(metadata, findings, stats, payload, "format_contract")
		postStampFindings := validateAgentContractPayload(contractID, payload)
		if len(postStampFindings) == 0 {
			payload["format_contract"] = stampContractValidation(metadata, postStampFindings, stats, payload, "format_contract")
			findings = validateAgentContractPayload(contractID, payload)
			if len(findings) != 0 {
				continue
			}
			break
		}
		findings = postStampFindings
	}
	if len(findings) > 0 {
		payload["format_contract"] = stampContractValidation(metadata, findings, stats, payload, "format_contract")
	}
	recordAgentContractBoundary(agentID, contractID, lane, endpoint, findings)
	return payload
}

func attachContextPackFormatContract(payload map[string]any) map[string]any {
	ensureContextPackRetrievalProofReferences(payload)
	ensureContextPackRunAdvisor(payload)
	return attachPayloadFormatContract(
		contextPackResponseContractID,
		payload,
		anyToString(payload["agent_id"]),
		"context_pack",
		"/memory/context-pack",
	)
}

func ensureContextPackRetrievalProofReferences(payload map[string]any) {
	if payload == nil {
		return
	}
	contextPack := anyMap(payload["context_pack"])
	compiler := anyMap(payload["context_compiler"])
	if len(compiler) == 0 {
		compiler = anyMap(contextPack["context_compiler"])
	}
	assessment := anyMap(payload["memory_trust_assessment"])
	if len(assessment) == 0 {
		assessment = anyMap(contextPack["memory_trust_assessment"])
	}
	if len(assessment) == 0 {
		assessment = anyMap(compiler["memory_trust_assessment"])
	}
	if len(assessment) == 0 {
		assessment = map[string]any{
			"schema_id": memoryTrustAssessmentContractID, "canonical_path": "$.memory_trust_assessment",
			"available": false, "reason": "trust assessment was not materialized by this compatibility path",
		}
	}
	trace := anyMap(payload["retrieval_decision_trace"])
	if len(trace) == 0 {
		trace = anyMap(contextPack["retrieval_decision_trace"])
	}
	if len(trace) == 0 {
		trace = anyMap(compiler["retrieval_decision_trace"])
	}
	if len(trace) == 0 {
		trace = map[string]any{
			"schema_id": retrievalDecisionTraceContractID, "canonical_path": "$.retrieval_decision_trace",
			"available": false, "reason": "retrieval decision trace was not materialized by this compatibility path",
		}
	}
	assessmentReference := memoryTrustAssessmentReference(assessment)
	traceReference := retrievalDecisionTraceReference(trace)
	payload["memory_trust_assessment"] = assessment
	payload["retrieval_decision_trace"] = trace
	if len(contextPack) > 0 {
		contextPack["memory_trust_assessment"] = assessmentReference
		contextPack["retrieval_decision_trace"] = traceReference
		payload["context_pack"] = contextPack
	}
	if len(compiler) > 0 {
		compiler["memory_trust_assessment"] = assessmentReference
		compiler["retrieval_decision_trace"] = traceReference
		payload["context_compiler"] = compiler
		if len(contextPack) > 0 {
			contextPack["context_compiler"] = compiler
		}
	}
}

func ensureContextPackRunAdvisor(payload map[string]any) {
	if payload == nil {
		return
	}
	if len(anyMap(payload["run_advisor"])) > 0 {
		return
	}
	contextPack := anyMap(payload["context_pack"])
	sourceCoverage := anyMap(payload["source_coverage"])
	if len(sourceCoverage) == 0 {
		sourceCoverage = anyMap(contextPack["source_coverage"])
	}
	objectiveCtx := extractObjectiveContext(payload)
	if objectiveCtx.empty() {
		objectiveCtx = extractObjectiveContext(contextPack)
	}
	if objectiveCtx.empty() {
		objectiveCtx = objectiveCtx.withDefaults()
	}
	query := firstNonEmptyStrings(
		anyToString(payload["query"]),
		anyToString(contextPack["query"]),
		anyToString(payload["task_summary"]),
	)
	advisor := buildRunAdvisor(runAdvisorInput{
		Query:           query,
		Project:         anyToString(payload["project"]),
		TopicPath:       firstNonEmptyStrings(anyToString(payload["topic_path"]), anyToString(contextPack["topic_path"])),
		RetrievalMode:   firstNonEmptyStrings(anyToString(payload["retrieval_mode"]), anyToString(contextPack["retrieval_mode"])),
		SessionID:       anyToString(payload["session_id"]),
		AgentID:         anyToString(payload["agent_id"]),
		SourceCoverage:  sourceCoverage,
		Objective:       objectiveCtx,
		RankedEvidence:  contextPackAnyList(firstPresentAny(contextPack["ranked_evidence"], contextPack["rankedEvidence"])),
		ReferencePrompt: anyToString(payload["reference_prompt"]),
		Surface:         "context_pack_contract_attach",
	})
	payload["run_advisor"] = advisor
	contextPack["runAdvisor"] = advisor
	contextPack["run_advisor"] = advisor
	payload["context_pack"] = contextPack
}

func attachWritebackFormatContract(payload map[string]any, item normalizedWrite, endpoint string, status int) map[string]any {
	if _, exists := payload["ok"]; !exists {
		payload["ok"] = status >= 200 && status < 300
	}
	payload["project"] = item.project
	payload["file"] = item.fileName
	payload["topic_path"] = item.topicPath
	return attachPayloadFormatContract(writebackResultContractID, payload, item.agentID, "writeback", endpoint)
}

func attachAgentPreflightFormatContracts(payload map[string]any) map[string]any {
	normalizeAgentContractPayloadMapInPlace(payload)
	ensurePreflightContextPackObject(payload)
	stats := agentBoundaryStatsFromMetadata(payload["format_contracts"])
	payload["format_contracts"] = preflightContractsSummary(nil, agentBoundaryStats{}, payload)
	findings := []map[string]any{}
	for pass := 0; pass < 5; pass++ {
		sanitizePreflightSearchBoundary(payload)
		stats = mergeAgentBoundaryStats(stats, enforceAgentBoundaryContract(agentPreflightResponseContractID, payload))
		ensurePreflightContextPackObject(payload)
		findings = validateAgentContractPayload(agentPreflightResponseContractID, payload)
		payload["format_contracts"] = preflightContractsSummary(findings, stats, payload)
		postStampFindings := validateAgentContractPayload(agentPreflightResponseContractID, payload)
		if len(postStampFindings) == 0 {
			payload["format_contracts"] = preflightContractsSummary(postStampFindings, stats, payload)
			findings = validateAgentContractPayload(agentPreflightResponseContractID, payload)
			if len(findings) != 0 {
				continue
			}
			break
		}
		findings = postStampFindings
	}
	if len(findings) > 0 {
		payload["format_contracts"] = preflightContractsSummary(findings, stats, payload)
	}
	recordAgentContractBoundary(anyToString(payload["agent_id"]), agentPreflightResponseContractID, "preflight", "/v1/agents/preflight", findings)
	return payload
}

func ensurePreflightContextPackObject(payload map[string]any) {
	if payload == nil {
		return
	}
	if _, ok := payload["context_pack"].(map[string]any); ok {
		return
	}
	payload["context_pack"] = map[string]any{
		"ok": false, "degraded": true, "result_state": "unavailable",
		"warnings": []any{"Context pack was unavailable during agent preflight."},
	}
}

func recordAgentContractBoundary(agentID string, schemaID string, lane string, endpoint string, findings []map[string]any) {
	if strings.TrimSpace(agentID) == "" {
		agentID = "unknown"
	}
	if strings.TrimSpace(schemaID) == "" {
		schemaID = "unknown"
	}
	if strings.TrimSpace(lane) == "" {
		lane = "unknown"
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "unknown"
	}
	reasons := []string{"passed"}
	if len(findings) > 0 {
		reasons = make([]string, 0, len(findings))
		for _, finding := range findings {
			reason := strings.TrimSpace(anyToString(finding["reason"]))
			if reason == "" {
				reason = "contract_violation"
			}
			reasons = append(reasons, reason)
		}
	}
	agentContractTelemetryMu.Lock()
	defer agentContractTelemetryMu.Unlock()
	for _, reason := range reasons {
		key := strings.Join([]string{agentID, schemaID, lane, endpoint, reason}, "\x1f")
		counter := agentContractTelemetryCounters[key]
		counter.AgentID = agentID
		counter.SchemaID = schemaID
		counter.Lane = lane
		counter.Endpoint = endpoint
		counter.Reason = reason
		counter.Count++
		counter.LastAt = nowUTCISO()
		agentContractTelemetryCounters[key] = counter
	}
}

func agentContractTelemetrySnapshot() map[string]any {
	agentContractTelemetryMu.Lock()
	defer agentContractTelemetryMu.Unlock()
	counters := make([]agentContractTelemetryCounter, 0, len(agentContractTelemetryCounters))
	for _, counter := range agentContractTelemetryCounters {
		counters = append(counters, counter)
	}
	sort.Slice(counters, func(i, j int) bool {
		left := counters[i]
		right := counters[j]
		if left.AgentID != right.AgentID {
			return left.AgentID < right.AgentID
		}
		if left.SchemaID != right.SchemaID {
			return left.SchemaID < right.SchemaID
		}
		if left.Lane != right.Lane {
			return left.Lane < right.Lane
		}
		if left.Endpoint != right.Endpoint {
			return left.Endpoint < right.Endpoint
		}
		return left.Reason < right.Reason
	})
	total := 0
	for _, counter := range counters {
		total += counter.Count
	}
	return map[string]any{
		"ok":             true,
		"schema_version": 1,
		"total":          total,
		"counters":       counters,
	}
}

func (s *server) agentContractTelemetryRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, agentContractTelemetrySnapshot())
}

func validateAgentContractPayload(contractID string, payload any) []map[string]any {
	registry, err := loadAgentContractsRegistry()
	if err != nil {
		return []map[string]any{{"reason": "registry_unavailable", "detail": err.Error(), "contract_id": contractID}}
	}
	contract := agentContract(registry, contractID)
	if contract == nil {
		return []map[string]any{{"reason": "missing_contract", "contract_id": contractID}}
	}
	object, normalizeErr := normalizeAgentContractJSONObject(payload)
	if normalizeErr != nil {
		return []map[string]any{{"reason": "payload_not_json", "detail": normalizeErr.Error(), "contract_id": contractID}}
	}
	if object == nil {
		return []map[string]any{{"reason": "payload_not_object", "contract_id": contractID}}
	}
	findings := []map[string]any{}
	if allowed := agentContractStringList(contract["allowed_fields"]); len(allowed) > 0 {
		allowedSet := map[string]bool{}
		for _, item := range allowed {
			allowedSet[item] = true
		}
		for key := range object {
			if !allowedSet[key] {
				findings = append(findings, map[string]any{"reason": "unexpected_field", "field": key, "contract_id": contractID})
			}
		}
	}
	for _, field := range agentContractStringList(contract["required_fields"]) {
		if _, exists := object[field]; !exists {
			findings = append(findings, map[string]any{"reason": "missing_required_field", "field": field, "contract_id": contractID})
		}
	}
	if fieldTypes, ok := contract["field_types"].(map[string]any); ok {
		for path, rawExpected := range fieldTypes {
			value, exists := dottedPathGet(object, path)
			if !exists {
				continue
			}
			expected := strings.TrimSpace(anyToString(rawExpected))
			if !matchesAgentContractType(value, expected) {
				findings = append(findings, map[string]any{
					"reason":      "field_type_mismatch",
					"path":        path,
					"expected":    expected,
					"contract_id": contractID,
				})
			}
		}
	}
	if nested, ok := contract["required_fields_by_path"].(map[string]any); ok {
		keys := sortedMapKeys(nested)
		for _, path := range keys {
			value, exists := dottedPathGet(object, path)
			target, ok := value.(map[string]any)
			if !exists || !ok {
				findings = append(findings, map[string]any{"reason": "missing_required_object", "path": path, "contract_id": contractID})
				continue
			}
			for _, field := range agentContractStringList(nested[path]) {
				if _, exists := target[field]; !exists {
					findings = append(findings, map[string]any{"reason": "missing_required_nested_field", "path": path + "." + field, "contract_id": contractID})
				}
			}
		}
	}
	for _, path := range agentContractStringList(contract["required_true_paths"]) {
		value, exists := dottedPathGet(object, path)
		if !exists || value != true {
			findings = append(findings, map[string]any{"reason": "required_true_path_not_true", "path": path, "contract_id": contractID})
		}
	}
	for _, path := range agentContractStringList(contract["required_false_paths"]) {
		value, exists := dottedPathGet(object, path)
		if !exists || value != false {
			findings = append(findings, map[string]any{"reason": "required_false_path_not_false", "path": path, "contract_id": contractID})
		}
	}
	if contains, ok := contract["required_string_contains"].(map[string]any); ok {
		keys := sortedMapKeys(contains)
		for _, path := range keys {
			value, _ := dottedPathGet(object, path)
			needle := anyToString(contains[path])
			if !strings.Contains(anyToString(value), needle) {
				findings = append(findings, map[string]any{"reason": "required_string_missing", "path": path, "needle": needle, "contract_id": contractID})
			}
		}
	}
	if minItems, ok := contract["min_items"].(map[string]any); ok {
		keys := sortedMapKeys(minItems)
		for _, path := range keys {
			value, _ := dottedPathGet(object, path)
			items, ok := value.([]any)
			minItemsCount := anyToInt(minItems[path], 1)
			if !ok || len(items) < minItemsCount {
				actual := 0
				if ok {
					actual = len(items)
				}
				findings = append(findings, map[string]any{"reason": "list_min_items_not_met", "path": path, "min_items": minItemsCount, "actual": actual, "contract_id": contractID})
			}
		}
	}
	limits := agentBoundaryLimitsFromContract(contract)
	if limits.MaxTotalJSONBytes > 0 {
		actual := jsonByteLen(object)
		if actual > limits.MaxTotalJSONBytes {
			findings = append(findings, map[string]any{
				"reason":       "json_bytes_exceed_contract",
				"bytes":        actual,
				"max_bytes":    limits.MaxTotalJSONBytes,
				"contract_id":  contractID,
				"payload_kind": contract["payload_kind"],
			})
		}
	}
	if limits.MaxStringBytes > 0 {
		findings = append(findings, validateAgentBoundaryStringBytes(object, limits.MaxStringBytes, "", contractID)...)
	}
	if limits.MaxListItems > 0 {
		findings = append(findings, validateAgentBoundaryListItems(object, limits.MaxListItems, "", contractID)...)
	}
	if maxBytes, ok := contract["max_bytes_by_path"].(map[string]any); ok {
		keys := sortedMapKeys(maxBytes)
		for _, path := range keys {
			value, _ := dottedPathGet(object, path)
			text, ok := value.(string)
			if !ok {
				continue
			}
			maxCount := anyToInt(maxBytes[path], 0)
			if maxCount > 0 && len([]byte(text)) > maxCount {
				findings = append(findings, map[string]any{"reason": "string_bytes_exceed_contract", "path": path, "bytes": len([]byte(text)), "max_bytes": maxCount, "contract_id": contractID})
			}
		}
	}
	forbidden := agentContractStringList(contract["forbidden_fields"])
	if len(forbidden) > 0 {
		forbiddenSet := map[string]bool{}
		for _, item := range forbidden {
			forbiddenSet[canonicalAgentContractFieldKey(item)] = true
		}
		if strings.TrimSpace(anyToString(contract["forbidden_scope"])) == "root" {
			for key := range object {
				if forbiddenSet[canonicalAgentContractFieldKey(key)] {
					findings = append(findings, map[string]any{"reason": "forbidden_field_present", "path": key, "contract_id": contractID})
				}
			}
		} else {
			findings = append(findings, walkForbiddenKeys(object, forbiddenSet, "", contractID)...)
		}
	}
	if contractID == taskIdentityReconciliationContractID {
		matchMode := strings.TrimSpace(anyToString(object["match_mode"]))
		abstentionModes := map[string]bool{
			"semantic_candidate": true,
			"ambiguous_semantic": true,
			"ambiguous_exact":    true,
			"none":               true,
		}
		confirmationModes := map[string]bool{
			"semantic_candidate": true,
			"ambiguous_semantic": true,
			"ambiguous_exact":    true,
		}
		if abstentionModes[matchMode] {
			if object["abstained"] != true {
				findings = append(findings, map[string]any{"reason": "identity_abstention_required", "path": "abstained", "match_mode": matchMode, "contract_id": contractID})
			}
			if strings.TrimSpace(anyToString(object["task_identity_id"])) != "" {
				findings = append(findings, map[string]any{"reason": "abstention_cannot_bind_identity", "path": "task_identity_id", "match_mode": matchMode, "contract_id": contractID})
			}
		}
		if confirmationModes[matchMode] && object["requires_confirmation"] != true {
			findings = append(findings, map[string]any{"reason": "identity_confirmation_required", "path": "requires_confirmation", "match_mode": matchMode, "contract_id": contractID})
		}
	}
	if contractID == agentPacketDeltaOutputContractID {
		findings = append(findings, agentPacketDeltaOperationFindings(object)...)
	}
	return findings
}

func needsAgentContractJSONNormalization(value any) bool {
	switch typed := value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return false
	case map[string]any:
		for _, nested := range typed {
			if needsAgentContractJSONNormalization(nested) {
				return true
			}
		}
		return false
	case []any:
		for _, nested := range typed {
			if needsAgentContractJSONNormalization(nested) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func normalizeAgentContractJSONObject(payload any) (map[string]any, error) {
	if object, ok := payload.(map[string]any); ok && !needsAgentContractJSONNormalization(object) {
		return object, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	object, _ := normalized.(map[string]any)
	return object, nil
}

func normalizeAgentContractPayloadMapInPlace(payload map[string]any) {
	if payload == nil || !needsAgentContractJSONNormalization(payload) {
		return
	}
	normalized, err := normalizeAgentContractJSONObject(payload)
	if err != nil || normalized == nil {
		return
	}
	clear(payload)
	for key, value := range normalized {
		payload[key] = value
	}
}

func dottedPathGet(payload map[string]any, dottedPath string) (any, bool) {
	var current any = payload
	for _, part := range strings.Split(dottedPath, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, exists := object[part]
		if !exists {
			return nil, false
		}
		current = value
	}
	return current, true
}

func matchesAgentContractType(value any, expected string) bool {
	switch strings.ToLower(strings.TrimSpace(expected)) {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "bool":
		_, ok := value.(bool)
		return ok
	case "list":
		_, ok := value.([]any)
		return ok
	case "list[string]":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	case "int":
		_, ok := value.(int)
		if ok {
			return true
		}
		_, ok = value.(float64)
		return ok
	case "number":
		switch value.(type) {
		case int, int64, float64, float32:
			return true
		default:
			return false
		}
	default:
		return true
	}
}

func agentContractStringList(value any) []string {
	out := []string{}
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			if text := strings.TrimSpace(item); text != "" {
				out = append(out, text)
			}
		}
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(anyToString(item)); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

func canonicalAgentContractFieldKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(len(value))
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			out.WriteRune(ch)
		}
	}
	return out.String()
}

func walkForbiddenKeys(value any, forbidden map[string]bool, path string, contractID string) []map[string]any {
	findings := []map[string]any{}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			currentPath := key
			if path != "" {
				currentPath = path + "." + key
			}
			if forbidden[canonicalAgentContractFieldKey(key)] {
				findings = append(findings, map[string]any{"reason": "forbidden_field_present", "path": currentPath, "contract_id": contractID})
			}
			findings = append(findings, walkForbiddenKeys(typed[key], forbidden, currentPath, contractID)...)
		}
	case []any:
		for _, item := range typed {
			findings = append(findings, walkForbiddenKeys(item, forbidden, path, contractID)...)
		}
	}
	return findings
}

func cloneContractMap(value map[string]any) map[string]any {
	out := map[string]any{}
	for key, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			out[key] = cloneContractMap(typed)
		case []any:
			out[key] = cloneContractList(typed)
		case []string:
			copied := make([]string, len(typed))
			copy(copied, typed)
			out[key] = copied
		default:
			out[key] = item
		}
	}
	return out
}

func cloneContractList(value []any) []any {
	out := make([]any, len(value))
	for idx, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			out[idx] = cloneContractMap(typed)
		case []any:
			out[idx] = cloneContractList(typed)
		default:
			out[idx] = item
		}
	}
	return out
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
