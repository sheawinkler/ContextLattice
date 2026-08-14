package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const agentContractsRegistryEnv = "CONTEXTLATTICE_AGENT_CONTRACTS_PATH"
const policyContextPackageContractID = "policy_context_package.v1"
const objectiveRuntimeStateContractID = "objective_runtime_state.v1"
const antiSchemingContractID = "anti_scheming_protocol.v1"
const agentPreflightResponseContractID = "agent_preflight_response.v1"
const contextPackResponseContractID = "context_pack_response.v1"
const contextPackPublicTraceIDMaxBytes = 47
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
		stabilizeAgentContractActualJSONBytes(payload, metadataKey, stamped)
	} else if stats.JSONBytesAfter > 0 {
		stamped["actual_json_bytes"] = stats.JSONBytesAfter
	}
	return stamped
}

func stabilizeAgentContractActualJSONBytes(payload map[string]any, metadataKey string, metadata map[string]any) {
	if payload == nil || metadataKey == "" || metadata == nil {
		return
	}
	previous, existed := payload[metadataKey]
	payload[metadataKey] = metadata
	defer func() {
		if existed {
			payload[metadataKey] = previous
		} else {
			delete(payload, metadataKey)
		}
	}()
	previousActual, actualExisted := metadata["actual_json_bytes"]
	metadata["actual_json_bytes"] = 0
	zeroSize := jsonByteLen(payload)
	if zeroSize <= 0 {
		if actualExisted {
			metadata["actual_json_bytes"] = previousActual
		} else {
			delete(metadata, "actual_json_bytes")
		}
		return
	}
	// Only the decimal width of actual_json_bytes changes after this marshal.
	// Solve that integer fixed point without cloning and re-encoding the payload.
	baseSize := zeroSize - 1
	size := zeroSize
	for pass := 0; pass < 8; pass++ {
		next := baseSize + len(strconv.Itoa(size))
		if next == size {
			metadata["actual_json_bytes"] = size
			return
		}
		size = next
	}
	metadata["actual_json_bytes"] = size
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
		stabilizeAgentContractActualJSONBytes(payload, "format_contracts", summary)
	}
	return summary
}

func attachPayloadFormatContract(contractID string, payload map[string]any, agentID string, lane string, endpoint string) map[string]any {
	domainFindings := []map[string]any{}
	if err := validateAgentContractJSONDomain(payload, 0); err != nil {
		clear(payload)
		payload["ok"] = false
		domainFindings = append(domainFindings, map[string]any{
			"reason": "payload_json_domain_invalid", "detail": err.Error(), "contract_id": contractID,
		})
	}
	normalizeAgentContractPayloadMapInPlace(payload)
	metadata := contractMetadata(contractID)
	stats := agentBoundaryStatsFromMetadata(payload["format_contract"])
	payload["format_contract"] = metadata
	findings := append([]map[string]any{}, domainFindings...)
	for pass := 0; pass < 5; pass++ {
		stats = mergeAgentBoundaryStats(stats, enforceAgentBoundaryContract(contractID, payload))
		findings = append(append([]map[string]any{}, domainFindings...), validateAgentContractPayload(contractID, payload)...)
		payload["format_contract"] = stampContractValidation(metadata, findings, stats, payload, "format_contract")
		postStampFindings := append(append([]map[string]any{}, domainFindings...), validateAgentContractPayload(contractID, payload)...)
		if len(postStampFindings) == 0 {
			payload["format_contract"] = stampContractValidation(metadata, postStampFindings, stats, payload, "format_contract")
			findings = append(append([]map[string]any{}, domainFindings...), validateAgentContractPayload(contractID, payload)...)
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
	assessment := map[string]any{}
	trace := map[string]any{}
	selectedOrigin := false
	for _, origin := range []struct {
		owner          map[string]any
		allowReference bool
	}{{payload, true}, {contextPack, false}, {compiler, false}} {
		_, assessmentPresent := origin.owner["memory_trust_assessment"]
		_, tracePresent := origin.owner["retrieval_decision_trace"]
		if !assessmentPresent && !tracePresent {
			continue
		}
		selectedOrigin = true
		candidateAssessment := canonicalContextPackRetrievalProof(anyMap(origin.owner["memory_trust_assessment"]), memoryTrustAssessmentContractID, "assessments", "$.memory_trust_assessment", origin.allowReference)
		candidateTrace := canonicalContextPackRetrievalProof(anyMap(origin.owner["retrieval_decision_trace"]), retrievalDecisionTraceContractID, "decisions", "$.retrieval_decision_trace", origin.allowReference)
		if len(candidateAssessment) > 0 && len(candidateTrace) > 0 && contextPackRetrievalProofPairValid(candidateAssessment, candidateTrace) {
			assessment, trace = candidateAssessment, candidateTrace
		}
		break
	}
	if !selectedOrigin || len(assessment) == 0 || len(trace) == 0 {
		assessment = contextPackUnavailableRetrievalProof(
			memoryTrustAssessmentContractID,
			"a same-origin retrieval proof pair was not available before the outer boundary",
		)
		trace = contextPackUnavailableRetrievalProof(
			retrievalDecisionTraceContractID,
			"a same-origin retrieval proof pair was not available before the outer boundary",
		)
	}
	assessmentReference := memoryTrustAssessmentReference(assessment)
	if available, ok := assessment["available"].(bool); (ok && !available) || anyToBool(assessment["bounded_projection"]) {
		assessmentReference = cloneAnyMap(assessment)
	}
	traceReference := retrievalDecisionTraceReference(trace)
	if available, ok := trace["available"].(bool); (ok && !available) || anyToBool(trace["bounded_projection"]) {
		traceReference = cloneAnyMap(trace)
	}
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

func canonicalContextPackRetrievalProof(proof map[string]any, schemaID, receiptList, canonicalPath string, allowReference bool) map[string]any {
	if anyToString(proof["schema_id"]) != schemaID {
		return nil
	}
	if value, exists := proof[receiptList]; exists {
		formatContract := anyMap(proof["format_contract"])
		validation := anyMap(formatContract["validation"])
		receipts, receiptsOK := asAnySlice(value)
		validationErrors, errorsOK := asAnySlice(validation["errors"])
		proofOK, proofOKTyped := proof["ok"].(bool)
		bounded, boundedTyped := proof["bounded"].(bool)
		contractValid, contractValidTyped := formatContract["contract_valid"].(bool)
		countField := "decision_count"
		if schemaID == memoryTrustAssessmentContractID {
			countField = "assessed_count"
		}
		count, countOK := strictAgentContractInteger(proof[countField])
		registry, registryErr := loadAgentContractsRegistry()
		contract := registry.Contracts[schemaID]
		registryVersion, registryVersionOK := strictAgentContractInteger(formatContract["registry_version"])
		contractVersion, contractVersionOK := strictAgentContractInteger(formatContract["contract_version"])
		maximumTotal, maximumTotalOK := strictAgentContractInteger(formatContract["max_total_json_bytes"])
		maximumString, maximumStringOK := strictAgentContractInteger(formatContract["max_string_bytes"])
		maximumList, maximumListOK := strictAgentContractInteger(formatContract["max_list_items"])
		actual, actualOK := strictAgentContractInteger(formatContract["actual_json_bytes"])
		encoded, encodedErr := json.Marshal(proof)
		if registryErr == nil && receiptsOK && errorsOK && len(validationErrors) == 0 &&
			proofOKTyped && proofOK && boundedTyped && bounded &&
			contractValidTyped && contractValid && anyToString(formatContract["registry_id"]) == GeneratedAgentContractRegistryID &&
			registryVersionOK && registryVersion == GeneratedAgentContractRegistryVersion && anyToString(formatContract["schema_id"]) == schemaID &&
			contractVersionOK && contractVersion == int64(anyToInt(contract["contract_version"], 0)) &&
			maximumTotalOK && maximumTotal == int64(anyToInt(contract["max_total_json_bytes"], 0)) &&
			maximumStringOK && maximumString == int64(anyToInt(contract["max_string_bytes"], 0)) &&
			maximumListOK && maximumList == int64(anyToInt(contract["max_list_items"], 0)) &&
			actualOK && actual > 0 && actual <= maximumTotal && encodedErr == nil && actual == int64(len(encoded)) &&
			anyToString(formatContract["required_output_mode"]) == anyToString(contract["required_output_mode"]) &&
			anyToString(formatContract["validator"]) == "contextlattice.boundary.v1" &&
			anyToString(validation["status"]) == "passed" && countOK && count == int64(len(receipts)) &&
			len(validateAgentContractPayload(schemaID, proof)) == 0 && contextPackRetrievalProofCountsValid(proof, schemaID, false) {
			return proof
		}
		return nil
	}
	if available, exists := proof["available"].(bool); exists && !available && anyToString(proof["canonical_path"]) == canonicalPath {
		return contextPackUnavailableRetrievalProof(schemaID, "retrieval proof was unavailable at this boundary")
	}
	digest := anyToString(proof["canonical_digest"])
	if contextPackCanonicalProjectedRetrievalProof(proof, schemaID, canonicalPath, digest) {
		return proof
	}
	if allowReference && anyToString(proof["canonical_path"]) == canonicalPath {
		var expected map[string]any
		switch schemaID {
		case memoryTrustAssessmentContractID:
			for _, field := range []string{"assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count"} {
				count, ok := strictAgentContractInteger(proof[field])
				if !ok || count < 0 {
					return nil
				}
			}
			expected = memoryTrustAssessmentReference(proof)
		case retrievalDecisionTraceContractID:
			_, traceIDOK := contextPackPublicTraceID(proof["trace_id"])
			if !traceIDOK {
				return nil
			}
			for _, field := range []string{"candidate_count", "decision_count", "input_truncated_count"} {
				count, ok := strictAgentContractInteger(proof[field])
				if !ok || count < 0 {
					return nil
				}
			}
			expected = retrievalDecisionTraceReference(proof)
		}
		proofJSON, proofErr := json.Marshal(proof)
		expectedJSON, expectedErr := json.Marshal(expected)
		if proofErr == nil && expectedErr == nil && string(proofJSON) == string(expectedJSON) {
			return proof
		}
	}
	return nil
}

func contextPackCanonicalProjectedRetrievalProof(proof map[string]any, schemaID, canonicalPath, digest string) bool {
	bounded, boundedOK := proof["bounded_projection"].(bool)
	available, availableOK := proof["available"].(bool)
	if !boundedOK || !bounded || !availableOK || !available ||
		anyToString(proof["canonical_path"]) != canonicalPath ||
		!strings.HasPrefix(digest, "sha256:") || digest != strings.ToLower(digest) ||
		len(digest) != len("sha256:")+64 || !isHexDigest(strings.TrimPrefix(digest, "sha256:")) {
		return false
	}
	countFields := []string{"candidate_count", "decision_count", "input_truncated_count"}
	expectedKeys := map[string]struct{}{
		"schema_id": {}, "canonical_path": {}, "available": {}, "bounded_projection": {}, "canonical_digest": {},
		"candidate_count": {}, "decision_count": {}, "input_truncated_count": {},
		"trace_id": {}, "coverage_complete": {},
	}
	if schemaID == memoryTrustAssessmentContractID {
		countFields = []string{"assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count"}
		expectedKeys = map[string]struct{}{
			"schema_id": {}, "canonical_path": {}, "available": {}, "bounded_projection": {}, "canonical_digest": {},
			"assessed_count": {}, "quarantine_count": {}, "deduplicated_count": {}, "policy_omitted_count": {}, "input_truncated_count": {},
		}
	}
	for _, field := range countFields {
		count, ok := strictAgentContractInteger(proof[field])
		if !ok || count < 0 {
			return false
		}
	}
	if schemaID == retrievalDecisionTraceContractID {
		coverage, coverageOK := proof["coverage_complete"].(bool)
		_ = coverage
		if !coverageOK {
			return false
		}
		traceID, traceIDOK := proof["trace_id"].(string)
		omitted, hasOmitted := proof["trace_id_omitted"].(bool)
		if hasOmitted {
			if !omitted || !traceIDOK || traceID != "" {
				return false
			}
			expectedKeys["trace_id_omitted"] = struct{}{}
		} else if !traceIDOK {
			return false
		} else if _, ok := contextPackPublicTraceID(traceID); !ok {
			return false
		}
	}
	if len(proof) != len(expectedKeys) {
		return false
	}
	for key := range proof {
		if _, ok := expectedKeys[key]; !ok {
			return false
		}
	}
	return true
}

// contextPackRetrievalProofCanonicalJSON matches Python's compact
// json.dumps(..., ensure_ascii=False, sort_keys=True) representation. The
// receipt digest is computed before the enclosing context-pack boundary can
// clip a nested list, sanitize a provider-overflow phrase, or truncate a
// string. Keep this encoder deliberately closed over JSON-domain values.
func contextPackRetrievalProofCanonicalJSON(value any) (string, error) {
	var out strings.Builder
	if err := writeContextPackRetrievalProofCanonicalJSON(&out, value, 0); err != nil {
		return "", err
	}
	return out.String(), nil
}

func writeContextPackRetrievalProofCanonicalJSON(out *strings.Builder, value any, depth int) error {
	if depth > 64 {
		return fmt.Errorf("retrieval proof JSON exceeds maximum depth")
	}
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		if !utf8.ValidString(typed) {
			return fmt.Errorf("retrieval proof contains invalid UTF-8")
		}
		writeContextPackRetrievalProofJSONString(out, typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return fmt.Errorf("retrieval proof key contains invalid UTF-8")
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeContextPackRetrievalProofJSONString(out, key)
			out.WriteByte(':')
			if err := writeContextPackRetrievalProofCanonicalJSON(out, typed[key], depth+1); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeContextPackRetrievalProofCanonicalJSON(out, item, depth+1); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case []string:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if !utf8.ValidString(item) {
				return fmt.Errorf("retrieval proof contains invalid UTF-8")
			}
			writeContextPackRetrievalProofJSONString(out, item)
		}
		out.WriteByte(']')
	case int:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int8:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int16:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int32:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case uint:
		if uint64(typed) >= uint64(1)<<63 {
			return fmt.Errorf("retrieval proof integer exceeds signed int64")
		}
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint8:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint16:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint32:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		if typed >= uint64(1)<<63 {
			return fmt.Errorf("retrieval proof integer exceeds signed int64")
		}
		out.WriteString(strconv.FormatUint(typed, 10))
	case float32:
		wire := strconv.FormatFloat(float64(typed), 'g', -1, 32)
		number, err := strconv.ParseFloat(wire, 64)
		if err != nil {
			return err
		}
		formatted, err := contextPackPythonFloatJSON(number)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	case float64:
		formatted, err := contextPackPythonFloatJSON(typed)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	case json.Number:
		raw := typed.String()
		if raw == "" || strings.TrimSpace(raw) != raw || !json.Valid([]byte(raw)) {
			return fmt.Errorf("invalid JSON number")
		}
		if !strings.ContainsAny(raw, ".eE") {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return fmt.Errorf("noncanonical JSON integer: %w", err)
			}
			out.WriteString(strconv.FormatInt(parsed, 10))
			break
		}
		number, err := strconv.ParseFloat(raw, 64)
		if err != nil && !(errors.Is(err, strconv.ErrRange) && number == 0) {
			return err
		}
		formatted, err := contextPackPythonFloatJSON(number)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	default:
		return fmt.Errorf("unsupported retrieval proof JSON type %T", value)
	}
	return nil
}

func writeContextPackRetrievalProofJSONString(out *strings.Builder, value string) {
	out.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if char < 0x20 {
				_, _ = fmt.Fprintf(out, `\u%04x`, char)
			} else {
				out.WriteRune(char)
			}
		}
	}
	out.WriteByte('"')
}

func contextPackPythonFloatJSON(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", fmt.Errorf("nonfinite retrieval proof number")
	}
	abs := math.Abs(value)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		return strconv.FormatFloat(value, 'e', -1, 64), nil
	}
	formatted := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(formatted, ".") {
		formatted += ".0"
	}
	return formatted, nil
}

func contextPackRetrievalProofForOuterBoundary(proof map[string]any, schemaID string) map[string]any {
	if len(proof) == 0 {
		return proof
	}
	if bounded, ok := proof["bounded_projection"].(bool); ok && bounded {
		return proof
	}
	receiptList := "decisions"
	canonicalPath := "$.retrieval_decision_trace"
	countFields := []string{"candidate_count", "decision_count", "input_truncated_count"}
	if schemaID == memoryTrustAssessmentContractID {
		receiptList = "assessments"
		canonicalPath = "$.memory_trust_assessment"
		countFields = []string{"assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count"}
	}
	if _, fullReceipt := proof[receiptList]; !fullReceipt {
		return proof
	}
	canonical, err := contextPackRetrievalProofCanonicalJSON(proof)
	if err != nil {
		return contextPackUnavailableRetrievalProof(schemaID, "retrieval proof could not be encoded before the outer boundary")
	}
	if len(canonicalContextPackRetrievalProof(proof, schemaID, receiptList, canonicalPath, false)) == 0 {
		return contextPackUnavailableRetrievalProof(schemaID, "retrieval proof failed validation before the outer boundary")
	}
	projection := map[string]any{
		"schema_id":          schemaID,
		"canonical_path":     canonicalPath,
		"available":          true,
		"bounded_projection": true,
		"canonical_digest":   "sha256:" + sha256Hex(canonical),
	}
	for _, field := range countFields {
		count, ok := strictAgentContractInteger(proof[field])
		if !ok || count < 0 {
			return contextPackUnavailableRetrievalProof(schemaID, "retrieval proof count failed validation before the outer boundary")
		}
		projection[field] = count
	}
	if schemaID == retrievalDecisionTraceContractID {
		traceID, traceIDOK := contextPackPublicTraceID(proof["trace_id"])
		if traceIDOK {
			projection["trace_id"] = traceID
		} else {
			projection["trace_id"] = ""
			projection["trace_id_omitted"] = true
		}
		coverage, ok := proof["coverage_complete"].(bool)
		if !ok {
			return contextPackUnavailableRetrievalProof(schemaID, "retrieval proof coverage failed validation before the outer boundary")
		}
		projection["coverage_complete"] = coverage
	}
	return projection
}

func contextPackPublicTraceID(value any) (string, bool) {
	traceID, ok := value.(string)
	if !ok || len(traceID) != len("rdt_")+24 || len([]byte(traceID)) > contextPackPublicTraceIDMaxBytes || !strings.HasPrefix(traceID, "rdt_") {
		return "", false
	}
	for _, char := range strings.TrimPrefix(traceID, "rdt_") {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return "", false
		}
	}
	return traceID, true
}

func contextPackUnavailableRetrievalProof(schemaID, reason string) map[string]any {
	canonicalPath := "$.retrieval_decision_trace"
	if schemaID == memoryTrustAssessmentContractID {
		canonicalPath = "$.memory_trust_assessment"
	}
	return map[string]any{
		"schema_id": schemaID, "canonical_path": canonicalPath,
		"available": false, "reason": reason,
	}
}

func contextPackExactRetrievalReceiptID(value any, prefix string) bool {
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, prefix) || len(text) != len(prefix)+24 {
		return false
	}
	for _, char := range strings.TrimPrefix(text, prefix) {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func contextPackRetrievalProofPairValid(assessment, trace map[string]any) bool {
	assessmentAvailable, assessmentHasAvailability := assessment["available"].(bool)
	traceAvailable, traceHasAvailability := trace["available"].(bool)
	assessmentUnavailable := assessmentHasAvailability && !assessmentAvailable
	traceUnavailable := traceHasAvailability && !traceAvailable
	if assessmentUnavailable || traceUnavailable {
		return assessmentUnavailable && traceUnavailable
	}
	assessedCount, assessedOK := strictAgentContractInteger(assessment["assessed_count"])
	quarantineCount, quarantineOK := strictAgentContractInteger(assessment["quarantine_count"])
	deduplicatedCount, deduplicatedOK := strictAgentContractInteger(assessment["deduplicated_count"])
	policyOmittedCount, policyOmittedOK := strictAgentContractInteger(assessment["policy_omitted_count"])
	assessmentTruncated, assessmentTruncatedOK := strictAgentContractInteger(assessment["input_truncated_count"])
	candidateCount, candidateOK := strictAgentContractInteger(trace["candidate_count"])
	decisionCount, decisionOK := strictAgentContractInteger(trace["decision_count"])
	traceTruncated, traceTruncatedOK := strictAgentContractInteger(trace["input_truncated_count"])
	coverageComplete, coverageOK := trace["coverage_complete"].(bool)
	if !assessedOK || !quarantineOK || !deduplicatedOK || !policyOmittedOK || !assessmentTruncatedOK ||
		!candidateOK || !decisionOK || !traceTruncatedOK || !coverageOK ||
		assessedCount < 0 || quarantineCount < 0 || deduplicatedCount < 0 || policyOmittedCount < 0 ||
		assessmentTruncated < 0 || candidateCount < 0 || decisionCount < 0 || traceTruncated < 0 ||
		assessedCount != decisionCount || assessmentTruncated != traceTruncated ||
		candidateCount != decisionCount+traceTruncated || coverageComplete != (traceTruncated == 0) ||
		quarantineCount > assessedCount || deduplicatedCount > assessedCount-quarantineCount ||
		policyOmittedCount > assessedCount-quarantineCount-deduplicatedCount {
		return false
	}
	assessments, assessmentsOK := asAnySlice(assessment["assessments"])
	decisions, decisionsOK := asAnySlice(trace["decisions"])
	if !assessmentsOK || !decisionsOK {
		return true
	}
	inputCount, inputOK := strictAgentContractInteger(assessment["input_candidate_count"])
	assessmentProcessed, assessmentProcessedOK := strictAgentContractInteger(assessment["processed_candidate_count"])
	traceProcessed, traceProcessedOK := strictAgentContractInteger(trace["processed_candidate_count"])
	fullAssessmentTruncated, fullAssessmentTruncatedOK := strictAgentContractInteger(assessment["input_truncated_count"])
	fullTraceTruncated, fullTraceTruncatedOK := strictAgentContractInteger(trace["input_truncated_count"])
	fullDecisionCount, fullDecisionCountOK := strictAgentContractInteger(trace["decision_count"])
	if !inputOK || !candidateOK || !assessmentProcessedOK || !traceProcessedOK ||
		!fullAssessmentTruncatedOK || !fullTraceTruncatedOK || !fullDecisionCountOK ||
		inputCount != candidateCount || assessmentProcessed != traceProcessed ||
		fullAssessmentTruncated != fullTraceTruncated || fullDecisionCount != assessmentProcessed {
		return false
	}
	assessmentCandidates := map[string]int{}
	quarantinedCandidates := map[string]int{}
	for _, raw := range assessments {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		candidateID, ok := row["candidate_id"].(string)
		quarantine := anyMap(row["quarantine"])
		quarantined, quarantinedOK := quarantine["quarantined"].(bool)
		if !ok || !quarantinedOK {
			return false
		}
		assessmentCandidates[candidateID]++
		if quarantined {
			quarantinedCandidates[candidateID]++
		}
	}
	decisionCandidates := map[string]int{}
	traceQuarantinedCandidates := map[string]int{}
	for _, raw := range decisions {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		candidateID, candidateOK := row["candidate_id"].(string)
		decision, decisionOK := row["decision"].(string)
		if !candidateOK || !decisionOK {
			return false
		}
		decisionCandidates[candidateID]++
		if decision == "quarantined" {
			traceQuarantinedCandidates[candidateID]++
		}
	}
	if !reflect.DeepEqual(assessmentCandidates, decisionCandidates) || !reflect.DeepEqual(quarantinedCandidates, traceQuarantinedCandidates) {
		return false
	}
	decisionCounts := anyMap(trace["decision_counts"])
	fullQuarantineCount, fullQuarantineOK := strictAgentContractInteger(assessment["quarantine_count"])
	fullDeduplicatedCount, fullDeduplicatedOK := strictAgentContractInteger(assessment["deduplicated_count"])
	fullPolicyOmittedCount, policyOK := strictAgentContractInteger(assessment["policy_omitted_count"])
	traceQuarantineCount, traceQuarantineOK := strictAgentContractInteger(decisionCounts["quarantined"])
	traceDeduplicatedCount, traceDeduplicatedOK := strictAgentContractInteger(decisionCounts["deduplicated"])
	omittedCount, omittedOK := strictAgentContractInteger(decisionCounts["omitted"])
	omittedTruncatedCount, omittedTruncatedOK := strictAgentContractInteger(decisionCounts["omitted_truncated"])
	if !traceQuarantineOK {
		traceQuarantineCount = 0
	}
	if !traceDeduplicatedOK {
		traceDeduplicatedCount = 0
	}
	if !omittedOK {
		omittedCount = 0
	}
	if !omittedTruncatedOK {
		omittedTruncatedCount = 0
	}
	omittedCategoriesCoverPolicy := omittedCount >= fullPolicyOmittedCount
	if !omittedCategoriesCoverPolicy {
		omittedCategoriesCoverPolicy = omittedTruncatedCount >= fullPolicyOmittedCount-omittedCount
	}
	return fullQuarantineOK && fullDeduplicatedOK && policyOK &&
		traceQuarantineCount == fullQuarantineCount && traceDeduplicatedCount == fullDeduplicatedCount &&
		omittedCategoriesCoverPolicy
}

func contextPackRetrievalProofCountsValid(proof map[string]any, schemaID string, reference bool) bool {
	fields := []string{}
	if !reference {
		fields = append(fields, "version")
	}
	switch schemaID {
	case memoryTrustAssessmentContractID:
		fields = append(fields, "assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count")
		if !reference {
			fields = append(fields, "input_candidate_count", "processed_candidate_count")
		}
	case retrievalDecisionTraceContractID:
		fields = append(fields, "candidate_count", "decision_count", "input_truncated_count")
		if !reference {
			fields = append(fields, "processed_candidate_count")
		}
	default:
		return false
	}
	for _, field := range fields {
		count, ok := strictAgentContractInteger(proof[field])
		if !ok || count < 0 {
			return false
		}
	}
	if !reference {
		version, ok := strictAgentContractInteger(proof["version"])
		if !ok || version != 1 {
			return false
		}
		boundary := anyMap(proof["input_boundary"])
		maximumCandidates, maximumOK := strictAgentContractInteger(boundary["maximum_candidates"])
		omittedCount, omittedOK := strictAgentContractInteger(boundary["omitted_count"])
		truncatedBoundary, truncatedOK := boundary["truncated"].(bool)
		if len(boundary) == 0 || !maximumOK || maximumCandidates < 0 || !omittedOK || omittedCount < 0 || !truncatedOK {
			return false
		}
		switch schemaID {
		case memoryTrustAssessmentContractID:
			inputCount, _ := strictAgentContractInteger(proof["input_candidate_count"])
			processed, _ := strictAgentContractInteger(proof["processed_candidate_count"])
			truncated, _ := strictAgentContractInteger(proof["input_truncated_count"])
			assessed, _ := strictAgentContractInteger(proof["assessed_count"])
			quarantined, _ := strictAgentContractInteger(proof["quarantine_count"])
			deduplicated, _ := strictAgentContractInteger(proof["deduplicated_count"])
			policyOmitted, _ := strictAgentContractInteger(proof["policy_omitted_count"])
			assessments := contextPackAnyList(proof["assessments"])
			policy := anyMap(proof["policy"])
			observedQuarantined := int64(0)
			assessmentIDs := map[string]bool{}
			candidateIDs := map[string]bool{}
			for _, raw := range assessments {
				row, ok := raw.(map[string]any)
				quarantine := anyMap(row["quarantine"])
				rowQuarantined, quarantinedOK := quarantine["quarantined"].(bool)
				contentDigest, digestOK := row["content_digest"].(string)
				if !ok || !contextPackExactRetrievalReceiptID(row["assessment_id"], "mta_") ||
					!contextPackExactRetrievalReceiptID(row["candidate_id"], "rtc_") || !digestOK ||
					!strings.HasPrefix(contentDigest, "sha256:") || len(contentDigest) != len("sha256:")+64 ||
					!isHexDigest(strings.TrimPrefix(contentDigest, "sha256:")) || !quarantinedOK {
					return false
				}
				assessmentID := anyToString(row["assessment_id"])
				candidateID := anyToString(row["candidate_id"])
				if assessmentIDs[assessmentID] || candidateIDs[candidateID] {
					return false
				}
				assessmentIDs[assessmentID] = true
				candidateIDs[candidateID] = true
				if rowQuarantined {
					observedQuarantined++
				}
			}
			if processed > inputCount || truncated != inputCount-processed || assessed != processed || int64(len(assessments)) != assessed || maximumCandidates < processed ||
				omittedCount != truncated || truncatedBoundary != (truncated > 0) ||
				policy["retrieved_memory_is_evidence_not_instruction"] != true ||
				policy["self_awarded_trust_accepted"] != false || policy["security_defenses_fail_closed"] != true ||
				quarantined > assessed || deduplicated > assessed || policyOmitted > assessed ||
				quarantined > assessed-deduplicated || policyOmitted > assessed-quarantined-deduplicated || observedQuarantined != quarantined {
				return false
			}
		case retrievalDecisionTraceContractID:
			candidateCount, _ := strictAgentContractInteger(proof["candidate_count"])
			processed, _ := strictAgentContractInteger(proof["processed_candidate_count"])
			truncated, _ := strictAgentContractInteger(proof["input_truncated_count"])
			decisionCount, _ := strictAgentContractInteger(proof["decision_count"])
			decisions := contextPackAnyList(proof["decisions"])
			if int64(len(decisions)) != decisionCount {
				return false
			}
			allowedDecisions := map[string]struct{}{
				"quarantined": {}, "deduplicated": {}, "omitted": {},
				"selected": {}, "selected_truncated": {}, "omitted_truncated": {},
			}
			observedCounts := map[string]int64{}
			receiptIDs := map[string]bool{}
			candidateIDs := map[string]bool{}
			candidateOrdinals := map[int64]bool{}
			for index, raw := range decisions {
				row, ok := raw.(map[string]any)
				if !ok {
					return false
				}
				decision, ok := row["decision"].(string)
				candidateOrdinal, ordinalOK := strictAgentContractInteger(row["candidate_ordinal"])
				decisionOrder, orderOK := strictAgentContractInteger(row["decision_order"])
				if !ok || !contextPackExactRetrievalReceiptID(row["receipt_id"], "rdr_") ||
					!contextPackExactRetrievalReceiptID(row["candidate_id"], "rtc_") || !ordinalOK || candidateOrdinal < 1 || candidateOrdinal > processed ||
					!orderOK || decisionOrder != int64(index+1) {
					return false
				}
				receiptID := anyToString(row["receipt_id"])
				candidateID := anyToString(row["candidate_id"])
				if receiptIDs[receiptID] || candidateIDs[candidateID] || candidateOrdinals[candidateOrdinal] {
					return false
				}
				receiptIDs[receiptID] = true
				candidateIDs[candidateID] = true
				candidateOrdinals[candidateOrdinal] = true
				if _, allowed := allowedDecisions[decision]; !allowed {
					return false
				}
				observedCounts[decision]++
			}
			decisionCounts, ok := proof["decision_counts"].(map[string]any)
			if !ok {
				return false
			}
			if len(decisionCounts) != len(observedCounts) {
				return false
			}
			for category, value := range decisionCounts {
				if _, allowed := allowedDecisions[category]; !allowed {
					return false
				}
				count, ok := strictAgentContractInteger(value)
				if !ok || count < 0 || observedCounts[category] != count {
					return false
				}
			}
			coverageComplete, ok := proof["coverage_complete"].(bool)
			expectedComplete := truncated == 0
			if !ok || processed > candidateCount || truncated != candidateCount-processed || decisionCount != processed ||
				maximumCandidates < processed || omittedCount != truncated || truncatedBoundary != (truncated > 0) ||
				coverageComplete != expectedComplete {
				return false
			}
		}
	}
	return true
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
	if err := validateAgentContractJSONDomain(payload, 0); err != nil {
		return []map[string]any{{"reason": "payload_json_domain_invalid", "detail": err.Error(), "contract_id": contractID}}
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
	if closed, ok := contract["closed_fields_by_path"].(map[string]any); ok {
		keys := sortedMapKeys(closed)
		for _, path := range keys {
			value, exists := dottedPathGet(object, path)
			if !exists {
				continue
			}
			target, ok := value.(map[string]any)
			if !ok {
				findings = append(findings, map[string]any{"reason": "closed_path_not_object", "path": path, "contract_id": contractID})
				continue
			}
			allowed := map[string]bool{}
			for _, field := range agentContractStringList(closed[path]) {
				allowed[field] = true
			}
			for field := range target {
				if !allowed[field] {
					findings = append(findings, map[string]any{
						"reason": "unexpected_nested_field", "path": path + "." + field, "contract_id": contractID,
					})
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
	if stateMatrix, ok := contract["state_matrix"].([]any); ok && len(stateMatrix) > 0 {
		matched := false
		for _, rawRow := range stateMatrix {
			row, ok := rawRow.(map[string]any)
			if !ok || len(row) == 0 {
				continue
			}
			rowMatched := true
			for path, expected := range row {
				actual, exists := dottedPathGet(object, path)
				if !exists || !reflect.DeepEqual(actual, expected) {
					rowMatched = false
					break
				}
			}
			if rowMatched {
				matched = true
				break
			}
		}
		if !matched {
			findings = append(findings, map[string]any{"reason": "state_matrix_mismatch", "path": "state_matrix", "contract_id": contractID})
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
	if maxItems, ok := contract["max_items_by_path"].(map[string]any); ok {
		keys := sortedMapKeys(maxItems)
		for _, path := range keys {
			value, exists := dottedPathGet(object, path)
			if !exists {
				continue
			}
			items, ok := value.([]any)
			maxItemsCount := anyToInt(maxItems[path], 0)
			if !ok || maxItemsCount <= 0 || len(items) > maxItemsCount {
				actual := 0
				if ok {
					actual = len(items)
				}
				findings = append(findings, map[string]any{
					"reason": "list_max_items_exceeded", "path": path, "max_items": maxItemsCount,
					"actual": actual, "contract_id": contractID,
				})
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
	if contractID == recallResponseContractID && !validateRecallResponseU2(object) {
		findings = append(findings, map[string]any{
			"reason": "recall_response_nested_contract_invalid", "contract_id": contractID,
		})
	}
	if contractID == continuousCognitionContractID {
		findings = append(findings, continuousCognitionContractFindings(object)...)
	}
	if contractID == portableEvidenceIdentitySchemaID {
		findings = append(findings, portableEvidenceIdentityContractFindings(object)...)
	}
	if contractID == contextPassportContractID {
		passport := anyMap(object["passport"])
		if identity, present := passport["portable_evidence_identity"]; present {
			identityObject := anyMap(identity)
			if identityObject == nil {
				findings = append(findings, map[string]any{"reason": "portable_evidence_identity_not_object", "path": "passport.portable_evidence_identity", "contract_id": contractID})
			} else {
				findings = append(findings, portableEvidenceIdentityContractFindings(identityObject)...)
			}
		}
	}
	if contractID == GeneratedAgentContractUniversalAgentAdapterResponseV1 || contractID == GeneratedAgentContractContextlatticeLifecycleReceiptV1 {
		identityFields := []string{"session_id", "agent_id"}
		if contractID == GeneratedAgentContractContextlatticeLifecycleReceiptV1 {
			identityFields = []string{"session_id"}
		}
		allowedIdentityField := map[string]bool{}
		for _, field := range identityFields {
			allowedIdentityField[field] = true
		}
		omitted := map[string]bool{}
		if raw, exists := object["identity_omitted"]; exists {
			items, ok := raw.([]any)
			if !ok {
				findings = append(findings, map[string]any{"reason": "identity_omission_marker_invalid", "path": "identity_omitted", "contract_id": contractID})
			} else {
				valid := true
				for _, item := range items {
					field, ok := item.(string)
					if !ok || !allowedIdentityField[field] {
						valid = false
						continue
					}
					omitted[field] = true
				}
				if !valid {
					findings = append(findings, map[string]any{"reason": "identity_omission_marker_invalid", "path": "identity_omitted", "contract_id": contractID})
				}
			}
		}
		for _, field := range identityFields {
			value, typed := object[field].(string)
			present := typed && strings.TrimSpace(value) != ""
			marked := omitted[field]
			if present == marked {
				reason := "identity_or_omission_required"
				if present {
					reason = "identity_omission_conflict"
				}
				findings = append(findings, map[string]any{"reason": reason, "path": field, "contract_id": contractID})
			}
		}
	}
	if contractID == memoryTrustAssessmentContractID || contractID == retrievalDecisionTraceContractID {
		if !contextPackRetrievalProofCountsValid(object, contractID, false) {
			findings = append(findings, map[string]any{
				"reason": "retrieval_proof_count_invariant_mismatch", "path": "retrieval_counts", "contract_id": contractID,
			})
		}
	}
	return findings
}

func validateAgentContractJSONDomain(value any, depth int) error {
	if depth > 64 {
		return fmt.Errorf("agent contract JSON exceeds maximum depth")
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if !utf8.ValidString(key) {
				return fmt.Errorf("agent contract JSON contains invalid UTF-8")
			}
			if err := validateAgentContractJSONDomain(nested, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, nested := range typed {
			if err := validateAgentContractJSONDomain(nested, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return validateAgentContractJSONValue(reflect.ValueOf(value), depth)
}

func validateAgentContractJSONNumber(raw string) error {
	if raw == "" || strings.TrimSpace(raw) != raw || !json.Valid([]byte(raw)) {
		return fmt.Errorf("agent contract JSON contains invalid number")
	}
	if !strings.ContainsAny(raw, ".eE") {
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return fmt.Errorf("agent contract JSON integer is outside signed int64: %w", err)
		}
		return nil
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if (err != nil && !(errors.Is(err, strconv.ErrRange) && parsed == 0)) || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fmt.Errorf("agent contract JSON contains invalid finite number")
	}
	return nil
}

func validateAgentContractJSONValue(rv reflect.Value, depth int) error {
	if depth > 64 {
		return fmt.Errorf("agent contract JSON exceeds maximum depth")
	}
	if !rv.IsValid() {
		return nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Type() == reflect.TypeOf(json.Number("")) {
		return validateAgentContractJSONNumber(rv.String())
	}
	switch rv.Kind() {
	case reflect.Bool:
		return nil
	case reflect.String:
		if !utf8.ValidString(rv.String()) {
			return fmt.Errorf("agent contract JSON contains invalid UTF-8")
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if rv.Uint() >= uint64(1)<<63 {
			return fmt.Errorf("agent contract JSON integer exceeds signed int64")
		}
		return nil
	case reflect.Float32, reflect.Float64:
		value := rv.Float()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("agent contract JSON contains nonfinite number")
		}
		return nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("agent contract JSON map key is not a string")
		}
		iter := rv.MapRange()
		for iter.Next() {
			if err := validateAgentContractJSONValue(iter.Key(), depth+1); err != nil {
				return err
			}
			if err := validateAgentContractJSONValue(iter.Value(), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice, reflect.Array:
		for index := 0; index < rv.Len(); index++ {
			if err := validateAgentContractJSONValue(rv.Index(index), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		// Inspect exported JSON fields before normalization so typed producer
		// receipts retain their exact values while malformed UTF-8 and numeric
		// values still fail closed. json.Marshal handles field naming and custom
		// encoders during the subsequent normalization pass.
		typeOfValue := rv.Type()
		for index := 0; index < rv.NumField(); index++ {
			field := typeOfValue.Field(index)
			if (field.PkgPath != "" && !field.Anonymous) || strings.Split(field.Tag.Get("json"), ",")[0] == "-" {
				continue
			}
			if err := validateAgentContractJSONValue(rv.Field(index), depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("agent contract JSON contains unsupported type %s", rv.Type())
	}
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
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
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
		_, ok := agentContractInteger(value)
		return ok
	case "number":
		switch typed := value.(type) {
		case int, int8, int16, int32, int64, uint8, uint16, uint32:
			return true
		case uint:
			return uint64(typed) < uint64(1)<<63
		case uint64:
			return typed < uint64(1)<<63
		case float32:
			number := float64(typed)
			return !math.IsNaN(number) && !math.IsInf(number, 0)
		case float64:
			return !math.IsNaN(typed) && !math.IsInf(typed, 0)
		case json.Number:
			raw := strings.TrimSpace(typed.String())
			if !strings.ContainsAny(raw, ".eE") {
				_, err := strconv.ParseInt(raw, 10, 64)
				return err == nil
			}
			number, err := strconv.ParseFloat(raw, 64)
			return (err == nil || (errors.Is(err, strconv.ErrRange) && number == 0)) && !math.IsNaN(number) && !math.IsInf(number, 0)
		default:
			return false
		}
	default:
		return true
	}
}

func agentContractInteger(value any) (int64, bool) {
	if parsed, ok := strictAgentContractInteger(value); ok {
		return parsed, true
	}
	var number float64
	switch typed := value.(type) {
	case json.Number:
		raw := strings.TrimSpace(typed.String())
		if !strings.ContainsAny(raw, ".eE") {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil && !(errors.Is(err, strconv.ErrRange) && parsed == 0) {
			return 0, false
		}
		number = parsed
	case float32:
		number = float64(typed)
	case float64:
		number = typed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < -float64(uint64(1)<<63) || number >= float64(uint64(1)<<63) {
		return 0, false
	}
	return int64(number), true
}

func strictAgentContractInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		raw := strings.TrimSpace(typed.String())
		if raw == "" || strings.ContainsAny(raw, ".eE") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		return parsed, err == nil
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) >= uint64(1)<<63 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed >= uint64(1)<<63 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		return 0, false
	case float64:
		return 0, false
	default:
		return 0, false
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
