package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	frontierT8ReusableSkillCandidateSchemaID   = "reusable_skill_candidate.v1"
	frontierT8SkillRetirementCandidateSchemaID = "skill_retirement_candidate.v1"
	frontierT8MaxInputBytes                    = 512 * 1024
	frontierT8MaxOutputBytes                   = 128 * 1024
	frontierT8MaxReceipts                      = 48
	frontierT8MaxEvidenceRefs                  = 12
)

var (
	frontierT8IdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$`)
	frontierT8SHA256Pattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	frontierT8SensitiveKey      = regexp.MustCompile(`(?i)(api[_-]?key|(^|[_-])token($|[_-])|secret|password|credential|private[_-]?key|authorization|bearer)`)
	frontierT8SecretValue       = regexp.MustCompile(`(?i)(bearer\s+[A-Za-z0-9._~+/=-]{12,}|sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{12,}|AKIA[A-Z0-9]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
	frontierT8PersonalPath      = regexp.MustCompile(`(?i)(/Users/[^/\s]+|/home/[^/\s]+|[A-Z]:\\Users\\[^\\\s]+)`)
	frontierT8UnsafeMaterial    = regexp.MustCompile(`(?i)(\brm\s+-rf\b|\bgit\s+reset\s+--hard\b|\bgit\s+push\b[^\n]*--force|\bchmod\s+-R\b|\bchown\s+-R\b|\bsudo\b|\bcurl\b[^\n|]*\|\s*(sh|bash)\b|\bwget\b[^\n|]*\|\s*(sh|bash)\b)`)
)

type frontierT8EvidenceRef struct {
	RefID          string
	Kind           string
	Digest         string
	ResolvedDigest string
	ProducerID     string
	VerifierID     string
	VerificationID string
}

type frontierT8ReceiptEconomics struct {
	InputTokens        int64
	OutputTokens       int64
	ToolCalls          int64
	ProviderCostMicros int64
	LatencyMS          int64
	NetworkCalls       int64
	ModelCalls         int64
	ExecutionCount     int64
}

type frontierT8WorkflowReceipt struct {
	ReceiptID                 string
	WorkflowID                string
	Partition                 string
	FixtureID                 string
	EnvironmentID             string
	ProducerID                string
	VerifierID                string
	VerifiedAt                time.Time
	VerifiedAtText            string
	VerificationCommand       string
	VerificationCommandDigest string
	ReceiptDigest             string
	PreviousReceiptDigest     string
	WorkflowSignature         string
	Steps                     []string
	Checks                    []string
	Prerequisites             []string
	Rollback                  []string
	SideEffects               []string
	PlatformConstraints       []string
	Evidence                  []frontierT8EvidenceRef
	Economics                 frontierT8ReceiptEconomics
}

func frontierT8ReusableSkillCandidate(payload map[string]any) (map[string]any, error) {
	if err := frontierT8ValidateAdvisoryInput(payload); err != nil {
		return nil, err
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(anyToString(payload["name"])), "_", "-"))
	if !skillFoundryNamePattern.MatchString(name) {
		return nil, errors.New("name must be 2-64 lowercase letters, digits, or hyphens")
	}
	description, err := frontierT8BoundedText(payload["description"], "description", 500, true)
	if err != nil {
		return nil, err
	}
	asOf, asOfText, err := frontierT8Timestamp(payload["as_of"], "as_of")
	if err != nil {
		return nil, err
	}
	minimumTraining := frontierT8ClampedInt(payload["minimum_training_receipts"], 3, 3, 20)
	minimumHoldouts := frontierT8ClampedInt(payload["minimum_holdout_receipts"], 3, 3, 20)
	maxAgeDays := frontierT8ClampedInt(payload["max_verification_age_days"], 30, 1, 365)

	training, err := frontierT8NormalizeReceiptChain(payload["training_receipts"], "training", minimumTraining, asOf, maxAgeDays)
	if err != nil {
		return nil, fmt.Errorf("training receipts: %w", err)
	}
	holdouts, err := frontierT8NormalizeReceiptChain(payload["holdout_receipts"], "holdout", minimumHoldouts, asOf, maxAgeDays)
	if err != nil {
		return nil, fmt.Errorf("holdout receipts: %w", err)
	}
	workflowID := training[0].WorkflowID
	workflowSignature := training[0].WorkflowSignature
	for _, receipt := range append(append([]frontierT8WorkflowReceipt{}, training...), holdouts...) {
		if receipt.WorkflowID != workflowID || receipt.WorkflowSignature != workflowSignature {
			return nil, errors.New("superficially similar workflows are not reusable: exact workflow identity and bounded material must match")
		}
	}
	if err := frontierT8VerifyPartitionSeparation(training, holdouts); err != nil {
		return nil, err
	}

	allReceipts := append(append([]frontierT8WorkflowReceipt{}, training...), holdouts...)
	economics := frontierT8AggregateEconomics(allReceipts)
	provenance := map[string]any{
		"hash_chains_verified":          true,
		"evidence_resolved":             true,
		"independent_verification":      true,
		"training_holdout_separated":    true,
		"fixture_environment_separated": true,
		"training":                      frontierT8ReceiptProvenance(training),
		"holdouts":                      frontierT8ReceiptProvenance(holdouts),
	}
	foundryRuns := make([]any, 0, len(training))
	foundryHoldouts := make([]any, 0, len(holdouts))
	for _, receipt := range training {
		foundryRuns = append(foundryRuns, frontierT8FoundryRun(receipt, false))
	}
	for _, receipt := range holdouts {
		foundryHoldouts = append(foundryHoldouts, frontierT8FoundryRun(receipt, true))
	}
	verificationCommands := frontierT8UniqueVerificationCommands(allReceipts)
	seed := map[string]any{
		"project": project, "name": name, "description": description, "as_of": asOfText,
		"workflow_id": workflowID, "workflow_signature": workflowSignature,
		"training_receipts": frontierT8ReceiptDigestList(training), "holdout_receipts": frontierT8ReceiptDigestList(holdouts),
	}
	seedRaw, _ := json.Marshal(seed)
	candidateID := "skillcand_" + sha256Hex(string(seedRaw))[:24]
	candidate := map[string]any{
		"schema_id": frontierT8ReusableSkillCandidateSchemaID, "version": 1,
		"candidate_id": candidateID, "project": project, "name": name, "description": description,
		"candidate_kind": "runbook_and_skill", "status": "review_required", "as_of": asOfText,
		"workflow_id": workflowID, "workflow_signature": workflowSignature,
		"proposal": map[string]any{
			"steps": stringSliceAny(training[0].Steps), "checks": stringSliceAny(training[0].Checks),
			"prerequisites": stringSliceAny(training[0].Prerequisites), "rollback": stringSliceAny(training[0].Rollback),
			"side_effects": stringSliceAny(training[0].SideEffects), "platform_constraints": stringSliceAny(training[0].PlatformConstraints),
			"verification_commands": verificationCommands,
		},
		"provenance": provenance,
		"economics":  economics,
		"measurement": map[string]any{
			"training_receipt_count": len(training), "holdout_receipt_count": len(holdouts),
			"max_verification_age_days": maxAgeDays,
			"network_calls":             economics["network_calls"], "model_calls": economics["model_calls"], "execution_count": economics["execution_count"],
			"kernel_network_calls": 0, "kernel_model_calls": 0, "kernel_execution_count": 0,
			"limitations": []any{
				"Receipt evidence is deterministically validated from the supplied evidence bundle; the coordinator must re-resolve it before Foundry submission.",
				"Repeated verified outcomes establish reuse fitness for the represented fixtures and environments, not universal causal efficacy.",
				"This advisory kernel does not inspect active Skills Index collisions or mutate ordinary memory.",
			},
		},
		"review": map[string]any{
			"required": true, "state": "pending", "automatic_approval": false,
			"requirements": []any{
				"Re-resolve every evidence digest.",
				"Re-run the fixture- and environment-separated holdouts.",
				"Inspect prerequisites, side effects, rollback, and platform constraints.",
				"Review the inactive export before any installation or activation.",
			},
		},
		"export": map[string]any{
			"mode": "inactive_only", "state": "inactive", "automatic": false,
			"activation_allowed": false, "installation_performed": false, "filesystem_mutation": false,
		},
		"skill_foundry_handoff": map[string]any{
			"target_contract": skillDraftContractID, "target_surface": "skill_foundry_draft",
			"ready": true, "automatic_submit": false, "automatic_export": false,
			"draft_payload": map[string]any{
				"project": project, "name": name, "description": description,
				"minimum_verified_runs": minimumTraining, "workflow_runs": foundryRuns,
			},
			"evaluation_template": map[string]any{
				"minimum_holdouts": minimumHoldouts, "holdouts": foundryHoldouts,
			},
		},
		"safety": map[string]any{
			"advisory_only": true, "activation_performed": false, "deactivation_performed": false,
			"ordinary_memory_writes": 0, "filesystem_mutations": 0, "provider_calls": 0,
			"network_calls": 0, "subprocess_calls": 0, "paid_automation": false,
		},
	}
	if err := frontierT8EnsureBounded(candidate); err != nil {
		return nil, err
	}
	return candidate, nil
}

func frontierT8SkillRetirementCandidate(payload map[string]any) (map[string]any, error) {
	if err := frontierT8ValidateAdvisoryInput(payload); err != nil {
		return nil, err
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(anyToString(payload["project"]), "contextlattice"))
	if err != nil {
		return nil, err
	}
	skillID, err := frontierT8Identifier(payload["skill_id"], "skill_id")
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(anyToString(payload["name"])), "_", "-"))
	if !skillFoundryNamePattern.MatchString(name) {
		return nil, errors.New("name must be 2-64 lowercase letters, digits, or hyphens")
	}
	skillVersion := frontierT8ClampedInt(payload["skill_version"], 1, 1, 1000000)
	asOf, asOfText, err := frontierT8Timestamp(payload["as_of"], "as_of")
	if err != nil {
		return nil, err
	}
	lastVerifiedAt, lastVerifiedText, err := frontierT8Timestamp(payload["last_verified_at"], "last_verified_at")
	if err != nil {
		return nil, err
	}
	if lastVerifiedAt.After(asOf) {
		return nil, errors.New("last_verified_at cannot be after as_of")
	}
	window := anyMap(payload["review_window"])
	windowStart, windowStartText, err := frontierT8Timestamp(window["start_at"], "review_window.start_at")
	if err != nil {
		return nil, err
	}
	windowEnd, windowEndText, err := frontierT8Timestamp(window["end_at"], "review_window.end_at")
	if err != nil {
		return nil, err
	}
	if !windowEnd.After(windowStart) || asOf.Before(windowStart) || !asOf.Before(windowEnd) {
		return nil, errors.New("review_window must contain as_of and end after start")
	}

	metrics := anyMap(payload["metrics"])
	baselineRate, err := frontierT8Rate(metrics["baseline_verified_success_rate"], "metrics.baseline_verified_success_rate")
	if err != nil {
		return nil, err
	}
	currentRate, err := frontierT8Rate(metrics["current_verified_success_rate"], "metrics.current_verified_success_rate")
	if err != nil {
		return nil, err
	}
	baselineSamples, err := frontierT8RequiredNonNegativeInt(metrics, "baseline_sample_count")
	if err != nil {
		return nil, err
	}
	currentSamples, err := frontierT8RequiredNonNegativeInt(metrics, "current_sample_count")
	if err != nil {
		return nil, err
	}
	useCount, err := frontierT8RequiredNonNegativeInt(metrics, "use_count")
	if err != nil {
		return nil, err
	}
	regressions, err := frontierT8RequiredNonNegativeInt(metrics, "verified_regression_count")
	if err != nil {
		return nil, err
	}
	temporaryProviderFailures, err := frontierT8RequiredNonNegativeInt(metrics, "temporary_provider_failure_count")
	if err != nil {
		return nil, err
	}
	networkCalls, err := frontierT8RequiredNonNegativeInt(metrics, "network_calls")
	if err != nil {
		return nil, err
	}
	modelCalls, err := frontierT8RequiredNonNegativeInt(metrics, "model_calls")
	if err != nil {
		return nil, err
	}
	executionCount, err := frontierT8RequiredNonNegativeInt(metrics, "execution_count")
	if err != nil {
		return nil, err
	}
	totalCostMicros, err := frontierT8RequiredNonNegativeInt(metrics, "total_cost_micros")
	if err != nil {
		return nil, err
	}
	totalLatencyMS, err := frontierT8RequiredNonNegativeInt(metrics, "total_latency_ms")
	if err != nil {
		return nil, err
	}

	thresholds := anyMap(payload["thresholds"])
	minimumSamples := int64(frontierT8ClampedInt(thresholds["minimum_samples"], 20, 10, 1000))
	decayThreshold := frontierT8ClampedFloat(thresholds["efficacy_decay"], 0.15, 0.05, 0.50)
	staleDaysThreshold := int64(frontierT8ClampedInt(thresholds["stale_days"], 90, 30, 730))
	lowUseThreshold := int64(frontierT8ClampedInt(thresholds["low_use_count"], 2, 0, 100))
	regressionThreshold := int64(frontierT8ClampedInt(thresholds["verified_regressions"], 3, 1, 100))
	replacementCoverageThreshold := frontierT8ClampedFloat(thresholds["replacement_coverage"], 0.95, 0.80, 1.0)
	rareValueThreshold := int64(frontierT8ClampedInt(thresholds["rare_value_per_use_micros"], 1000000, 1000, 1000000000))

	evidence, err := frontierT8NormalizeEvidence(payload["evidence_refs"], "skill_telemetry", "independent_reviewer")
	if err != nil {
		return nil, fmt.Errorf("retirement evidence: %w", err)
	}
	securityChange, err := frontierT8ChangeSignal(payload["security_change"], "security_change")
	if err != nil {
		return nil, err
	}
	dependencyChange, err := frontierT8ChangeSignal(payload["dependency_change"], "dependency_change")
	if err != nil {
		return nil, err
	}
	replacement, err := frontierT8Replacement(payload["replacement"])
	if err != nil {
		return nil, err
	}
	impact, err := frontierT8Impact(payload["impact"])
	if err != nil {
		return nil, err
	}
	seasonality := anyMap(payload["seasonality"])
	seasonal := anyToBool(seasonality["seasonal"])
	fullSeasonObserved := anyToBool(seasonality["full_observation_cycle"])
	seasonID, err := frontierT8BoundedText(seasonality["season_id"], "seasonality.season_id", 120, false)
	if err != nil {
		return nil, err
	}

	staleDays := int64(asOf.Sub(lastVerifiedAt).Hours() / 24)
	efficacyDecay := roundFloat(baselineRate-currentRate, 6)
	sampleSufficient := baselineSamples >= minimumSamples && currentSamples >= minimumSamples
	efficacySignal := sampleSufficient && efficacyDecay >= decayThreshold
	stalenessSignal := staleDays >= staleDaysThreshold
	lowUseSignal := useCount <= lowUseThreshold
	regressionSignal := sampleSufficient && regressions >= regressionThreshold
	securitySignal := anyToBool(securityChange["detected"])
	dependencySignal := anyToBool(dependencyChange["detected"])
	valuePerUseMicros := anyToInt(impact["value_per_use_micros"], 0)
	rareHighValue := anyToBool(payload["rare_high_value"]) || (lowUseSignal && int64(valuePerUseMicros) >= rareValueThreshold)
	temporaryProviderNoise := regressions > 0 && temporaryProviderFailures*2 >= regressions
	replacementPresent := anyToBool(replacement["present"])
	replacementVerified := anyToBool(replacement["verified"])
	replacementCoverage := anyToFloat(replacement["coverage_ratio"])
	narrowerReplacement := replacementPresent && (!replacementVerified || replacementCoverage < replacementCoverageThreshold)

	protections := make([]any, 0, 4)
	if seasonal && !fullSeasonObserved {
		protections = append(protections, "seasonal_evidence_window_incomplete")
	}
	if rareHighValue {
		protections = append(protections, "rare_high_value_skill")
	}
	if temporaryProviderNoise {
		protections = append(protections, "temporary_provider_failure_dominates_regressions")
	}
	if narrowerReplacement {
		protections = append(protections, "replacement_coverage_is_narrower_or_unverified")
	}
	materialSignals := 0
	for _, active := range []bool{efficacySignal, stalenessSignal, regressionSignal, securitySignal, dependencySignal} {
		if active {
			materialSignals++
		}
	}
	eligible := len(protections) == 0 && (materialSignals >= 2 || (securitySignal && strings.EqualFold(anyToString(securityChange["severity"]), "critical")))
	status := "insufficient_evidence"
	recommendation := "retain_and_remeasure"
	if len(protections) > 0 {
		status = "protected"
		recommendation = "retain_pending_protection_review"
	} else if eligible {
		status = "candidate"
		recommendation = "retirement_review"
	}

	signals := map[string]any{
		"efficacy_decay":  map[string]any{"detected": efficacySignal, "delta": efficacyDecay, "threshold": decayThreshold, "sample_sufficient": sampleSufficient},
		"staleness":       map[string]any{"detected": stalenessSignal, "days": staleDays, "threshold_days": staleDaysThreshold},
		"low_use":         map[string]any{"detected": lowUseSignal, "use_count": useCount, "threshold": lowUseThreshold, "qualifies_alone": false},
		"regressions":     map[string]any{"detected": regressionSignal, "verified_count": regressions, "threshold": regressionThreshold},
		"security_change": securityChange, "dependency_change": dependencyChange,
	}
	seed := map[string]any{
		"project": project, "skill_id": skillID, "skill_version": skillVersion, "as_of": asOfText,
		"signals": signals, "replacement": replacement, "protections": protections,
		"evidence": frontierT8EvidenceMaps(evidence),
	}
	seedRaw, _ := json.Marshal(seed)
	candidateID := "skillretcand_" + sha256Hex(string(seedRaw))[:24]
	candidate := map[string]any{
		"schema_id": frontierT8SkillRetirementCandidateSchemaID, "version": 1,
		"candidate_id": candidateID, "project": project, "skill_id": skillID, "name": name, "skill_version": skillVersion,
		"status": status, "recommendation": recommendation, "as_of": asOfText,
		"terminal_retirement": false, "automatic": false, "mutation_performed": false,
		"signals": signals,
		"metrics": map[string]any{
			"baseline_verified_success_rate": baselineRate, "current_verified_success_rate": currentRate,
			"baseline_sample_count": baselineSamples, "current_sample_count": currentSamples,
			"use_count": useCount, "verified_regression_count": regressions,
			"temporary_provider_failure_count": temporaryProviderFailures,
			"network_calls":                    networkCalls, "model_calls": modelCalls, "execution_count": executionCount,
			"total_cost_micros": totalCostMicros, "total_latency_ms": totalLatencyMS, "exact": true,
		},
		"last_verified_at": lastVerifiedText,
		"seasonality":      map[string]any{"seasonal": seasonal, "full_observation_cycle": fullSeasonObserved, "season_id": seasonID},
		"replacement":      replacement, "protections": protections,
		"review_window": map[string]any{"start_at": windowStartText, "end_at": windowEndText},
		"impact":        impact,
		"approval": map[string]any{
			"required": true, "explicit": true, "state": "pending", "approved": false,
			"terminal_action_surface": skillRetirementContractID,
		},
		"provenance": map[string]any{
			"evidence_resolved": true, "independently_verified": true, "evidence_refs": frontierT8EvidenceMaps(evidence),
		},
		"measurement": map[string]any{
			"network_calls": networkCalls, "model_calls": modelCalls, "execution_count": executionCount,
			"kernel_network_calls": 0, "kernel_model_calls": 0, "kernel_execution_count": 0,
			"limitations": []any{
				"Retirement signals are observational and require explicit operator review.",
				"Seasonality, provider attribution, and replacement coverage are limited to the supplied verified window.",
				"This candidate neither deactivates a skill nor invokes terminal Foundry retirement.",
			},
		},
		"safety": map[string]any{
			"advisory_only": true, "activation_performed": false, "deactivation_performed": false,
			"ordinary_memory_writes": 0, "filesystem_mutations": 0, "provider_calls": 0,
			"network_calls": 0, "subprocess_calls": 0, "paid_automation": false,
		},
	}
	if err := frontierT8EnsureBounded(candidate); err != nil {
		return nil, err
	}
	return candidate, nil
}

func frontierT8ValidateAdvisoryInput(payload map[string]any) error {
	if payload == nil {
		return errors.New("payload is required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("payload must be JSON-compatible: %w", err)
	}
	if len(raw) > frontierT8MaxInputBytes {
		return fmt.Errorf("payload exceeds %d bytes", frontierT8MaxInputBytes)
	}
	for _, key := range []string{"apply", "activate", "deactivate", "retire", "automatic_activation", "write_memory", "persist", "execute"} {
		if anyToBool(payload[key]) {
			return fmt.Errorf("advisory kernel rejects mutation intent %q", key)
		}
	}
	return frontierT8RejectUnsafeValue(payload, "payload", 0)
}

func frontierT8RejectUnsafeValue(value any, path string, depth int) error {
	if depth > 12 {
		return fmt.Errorf("%s exceeds maximum nesting", path)
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if frontierT8SensitiveKey.MatchString(key) && frontierT8Meaningful(typed[key]) {
				return fmt.Errorf("%s.%s contains secret-bearing material", path, key)
			}
			if err := frontierT8RejectUnsafeValue(typed[key], path+"."+key, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for index, item := range typed {
			if err := frontierT8RejectUnsafeValue(item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
	case []map[string]any:
		for index, item := range typed {
			if err := frontierT8RejectUnsafeValue(item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
	case []string:
		for index, item := range typed {
			if err := frontierT8RejectUnsafeValue(item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
	case string:
		if len(typed) > 8192 {
			return fmt.Errorf("%s exceeds bounded text size", path)
		}
		if frontierT8SecretValue.MatchString(typed) {
			return fmt.Errorf("%s contains secret-bearing material", path)
		}
		if frontierT8PersonalPath.MatchString(typed) {
			return fmt.Errorf("%s contains a personal absolute path", path)
		}
		if frontierT8UnsafeMaterial.MatchString(typed) {
			return fmt.Errorf("%s contains unsafe workflow material", path)
		}
	}
	return nil
}

func frontierT8Meaningful(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case []string:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func frontierT8NormalizeReceiptChain(raw any, partition string, minimum int, asOf time.Time, maxAgeDays int) ([]frontierT8WorkflowReceipt, error) {
	items := contextPackAnyList(raw)
	if len(items) < minimum {
		return nil, fmt.Errorf("at least %d independently verified repeated receipts are required", minimum)
	}
	if len(items) > frontierT8MaxReceipts {
		return nil, fmt.Errorf("receipt count exceeds %d", frontierT8MaxReceipts)
	}
	receipts := make([]frontierT8WorkflowReceipt, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		receipt, err := frontierT8NormalizeReceipt(anyMap(item), partition, asOf, maxAgeDays)
		if err != nil {
			return nil, fmt.Errorf("receipt %d: %w", index+1, err)
		}
		if _, duplicate := seen[receipt.ReceiptID]; duplicate {
			return nil, fmt.Errorf("receipt_id %q is duplicated", receipt.ReceiptID)
		}
		seen[receipt.ReceiptID] = struct{}{}
		if index == 0 {
			if receipt.PreviousReceiptDigest != "genesis" {
				return nil, errors.New("first receipt must declare previous_receipt_digest=genesis")
			}
		} else if receipt.PreviousReceiptDigest != receipts[index-1].ReceiptDigest {
			return nil, fmt.Errorf("receipt %q breaks the hash chain", receipt.ReceiptID)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func frontierT8NormalizeReceipt(row map[string]any, partition string, asOf time.Time, maxAgeDays int) (frontierT8WorkflowReceipt, error) {
	var receipt frontierT8WorkflowReceipt
	if len(row) == 0 {
		return receipt, errors.New("receipt payload is required")
	}
	if err := frontierT8RejectUnsafeValue(row, "receipt", 0); err != nil {
		return receipt, err
	}
	for _, key := range []string{"hidden_manual_steps", "manual_steps", "operator_intervention", "unrecorded_steps"} {
		if frontierT8Meaningful(row[key]) {
			return receipt, fmt.Errorf("hidden or manual workflow step %q is not reusable", key)
		}
	}
	var err error
	receipt.ReceiptID, err = frontierT8Identifier(row["receipt_id"], "receipt_id")
	if err != nil {
		return receipt, err
	}
	receipt.WorkflowID, err = frontierT8Identifier(row["workflow_id"], "workflow_id")
	if err != nil {
		return receipt, err
	}
	receipt.Partition = strings.ToLower(strings.TrimSpace(anyToString(row["partition"])))
	if receipt.Partition != partition {
		return receipt, fmt.Errorf("partition=%q want %q", receipt.Partition, partition)
	}
	receipt.FixtureID, err = frontierT8Identifier(row["fixture_id"], "fixture_id")
	if err != nil {
		return receipt, err
	}
	receipt.EnvironmentID, err = frontierT8Identifier(row["environment_id"], "environment_id")
	if err != nil {
		return receipt, err
	}
	receipt.ProducerID, err = frontierT8Identifier(row["producer_id"], "producer_id")
	if err != nil {
		return receipt, err
	}
	receipt.VerifierID, err = frontierT8Identifier(row["verifier_id"], "verifier_id")
	if err != nil {
		return receipt, err
	}
	if strings.EqualFold(receipt.ProducerID, receipt.VerifierID) {
		return receipt, errors.New("receipt verifier must be independent from its producer")
	}
	if !anyToBool(row["success"]) || !anyToBool(row["verification_passed"]) || !anyToBool(row["checks_passed"]) {
		return receipt, errors.New("receipt must be successful with independent verification and checks passed")
	}
	receipt.VerifiedAt, receipt.VerifiedAtText, err = frontierT8Timestamp(row["verified_at"], "verified_at")
	if err != nil {
		return receipt, err
	}
	if receipt.VerifiedAt.After(asOf.Add(5 * time.Minute)) {
		return receipt, errors.New("verified_at cannot be in the future")
	}
	if asOf.Sub(receipt.VerifiedAt) > time.Duration(maxAgeDays)*24*time.Hour {
		return receipt, fmt.Errorf("verification command is stale: verified_at exceeds %d days", maxAgeDays)
	}
	receipt.VerificationCommand, err = frontierT8BoundedText(row["verification_command"], "verification_command", 1000, true)
	if err != nil {
		return receipt, err
	}
	receipt.VerificationCommandDigest = strings.ToLower(strings.TrimSpace(anyToString(row["verification_command_digest"])))
	if !frontierT8SHA256Pattern.MatchString(receipt.VerificationCommandDigest) || receipt.VerificationCommandDigest != frontierT8CommandDigest(receipt.VerificationCommand) {
		return receipt, errors.New("verification_command_digest does not match the bounded verification command")
	}
	receipt.Steps, err = frontierT8StringList(row["steps"], "steps", 24, 500, true)
	if err != nil {
		return receipt, err
	}
	receipt.Checks, err = frontierT8StringList(row["checks"], "checks", 16, 400, true)
	if err != nil {
		return receipt, err
	}
	receipt.Prerequisites, err = frontierT8StringList(row["prerequisites"], "prerequisites", 16, 400, true)
	if err != nil {
		return receipt, err
	}
	receipt.Rollback, err = frontierT8StringList(row["rollback"], "rollback", 16, 400, true)
	if err != nil {
		return receipt, err
	}
	receipt.SideEffects, err = frontierT8StringList(row["side_effects"], "side_effects", 16, 400, true)
	if err != nil {
		return receipt, err
	}
	receipt.PlatformConstraints, err = frontierT8StringList(row["platform_constraints"], "platform_constraints", 16, 400, true)
	if err != nil {
		return receipt, err
	}
	receipt.WorkflowSignature = frontierT8WorkflowSignature(receipt)
	receipt.Evidence, err = frontierT8NormalizeEvidence(row["evidence_refs"], receipt.ProducerID, receipt.VerifierID)
	if err != nil {
		return receipt, err
	}
	cost := anyMap(row["cost"])
	if receipt.Economics.InputTokens, err = frontierT8RequiredNonNegativeInt(cost, "input_tokens"); err != nil {
		return receipt, err
	}
	if receipt.Economics.OutputTokens, err = frontierT8RequiredNonNegativeInt(cost, "output_tokens"); err != nil {
		return receipt, err
	}
	if receipt.Economics.ToolCalls, err = frontierT8RequiredNonNegativeInt(cost, "tool_calls"); err != nil {
		return receipt, err
	}
	if receipt.Economics.ProviderCostMicros, err = frontierT8RequiredNonNegativeInt(cost, "provider_cost_micros"); err != nil {
		return receipt, err
	}
	if receipt.Economics.LatencyMS, err = frontierT8RequiredNonNegativeInt(row, "latency_ms"); err != nil {
		return receipt, err
	}
	if receipt.Economics.NetworkCalls, err = frontierT8RequiredNonNegativeInt(row, "network_calls"); err != nil {
		return receipt, err
	}
	if receipt.Economics.ModelCalls, err = frontierT8RequiredNonNegativeInt(row, "model_calls"); err != nil {
		return receipt, err
	}
	if receipt.Economics.ExecutionCount, err = frontierT8RequiredNonNegativeInt(row, "execution_count"); err != nil {
		return receipt, err
	}
	if receipt.Economics.ExecutionCount == 0 {
		return receipt, errors.New("execution_count must be at least 1 for a workflow receipt")
	}
	receipt.PreviousReceiptDigest = strings.ToLower(strings.TrimSpace(anyToString(row["previous_receipt_digest"])))
	if receipt.PreviousReceiptDigest != "genesis" && !frontierT8SHA256Pattern.MatchString(receipt.PreviousReceiptDigest) {
		return receipt, errors.New("previous_receipt_digest must be genesis or sha256:<64-hex>")
	}
	receipt.ReceiptDigest = strings.ToLower(strings.TrimSpace(anyToString(row["receipt_digest"])))
	if !frontierT8SHA256Pattern.MatchString(receipt.ReceiptDigest) || receipt.ReceiptDigest != frontierT8ReceiptDigest(row) {
		return receipt, errors.New("receipt_digest does not match canonical receipt material")
	}
	return receipt, nil
}

func frontierT8ReceiptDigest(row map[string]any) string {
	copyRow := cloneJSONMap(row)
	delete(copyRow, "receipt_digest")
	raw, err := json.Marshal(copyRow)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func frontierT8CommandDigest(command string) string {
	return "sha256:" + sha256Hex(strings.Join(strings.Fields(command), " "))
}

func frontierT8WorkflowSignature(receipt frontierT8WorkflowReceipt) string {
	raw, _ := json.Marshal(map[string]any{
		"workflow_id": receipt.WorkflowID, "steps": receipt.Steps, "checks": receipt.Checks,
		"prerequisites": receipt.Prerequisites, "rollback": receipt.Rollback,
		"side_effects": receipt.SideEffects, "platform_constraints": receipt.PlatformConstraints,
	})
	return "sha256:" + sha256Hex(string(raw))
}

func frontierT8VerifyPartitionSeparation(training, holdouts []frontierT8WorkflowReceipt) error {
	trainingReceipts := map[string]struct{}{}
	trainingFixtures := map[string]struct{}{}
	trainingEnvironments := map[string]struct{}{}
	trainingEvidenceIDs := map[string]struct{}{}
	trainingEvidenceDigests := map[string]struct{}{}
	trainingProducers := map[string]struct{}{}
	for _, receipt := range training {
		trainingReceipts[receipt.ReceiptID] = struct{}{}
		trainingFixtures[receipt.FixtureID] = struct{}{}
		trainingEnvironments[receipt.EnvironmentID] = struct{}{}
		trainingProducers[strings.ToLower(receipt.ProducerID)] = struct{}{}
		for _, ref := range receipt.Evidence {
			trainingEvidenceIDs[ref.RefID] = struct{}{}
			trainingEvidenceDigests[ref.Digest] = struct{}{}
		}
	}
	holdoutFixtures := map[string]struct{}{}
	holdoutEnvironments := map[string]struct{}{}
	for _, receipt := range holdouts {
		if _, leaked := trainingReceipts[receipt.ReceiptID]; leaked {
			return fmt.Errorf("training/holdout leakage: receipt_id %q overlaps", receipt.ReceiptID)
		}
		if _, leaked := trainingFixtures[receipt.FixtureID]; leaked {
			return fmt.Errorf("training/holdout leakage: fixture_id %q overlaps", receipt.FixtureID)
		}
		if _, duplicate := holdoutFixtures[receipt.FixtureID]; duplicate {
			return fmt.Errorf("holdout fixture_id %q is duplicated", receipt.FixtureID)
		}
		holdoutFixtures[receipt.FixtureID] = struct{}{}
		if _, leaked := trainingEnvironments[receipt.EnvironmentID]; leaked {
			return fmt.Errorf("training/holdout leakage: environment_id %q overlaps", receipt.EnvironmentID)
		}
		if _, duplicate := holdoutEnvironments[receipt.EnvironmentID]; duplicate {
			return fmt.Errorf("holdout environment_id %q is duplicated", receipt.EnvironmentID)
		}
		holdoutEnvironments[receipt.EnvironmentID] = struct{}{}
		if _, dependent := trainingProducers[strings.ToLower(receipt.VerifierID)]; dependent {
			return fmt.Errorf("holdout verifier %q is not independent from training producers", receipt.VerifierID)
		}
		for _, ref := range receipt.Evidence {
			if _, leaked := trainingEvidenceIDs[ref.RefID]; leaked {
				return fmt.Errorf("training/holdout leakage: evidence ref_id %q overlaps", ref.RefID)
			}
			if _, leaked := trainingEvidenceDigests[ref.Digest]; leaked {
				return fmt.Errorf("training/holdout leakage: evidence digest %q overlaps", ref.Digest)
			}
		}
	}
	return nil
}

func frontierT8NormalizeEvidence(raw any, defaultProducer, defaultVerifier string) ([]frontierT8EvidenceRef, error) {
	items := contextPackAnyList(raw)
	if len(items) == 0 || len(items) > frontierT8MaxEvidenceRefs {
		return nil, fmt.Errorf("evidence_refs must contain 1-%d bounded refs", frontierT8MaxEvidenceRefs)
	}
	refs := make([]frontierT8EvidenceRef, 0, len(items))
	seenIDs := map[string]struct{}{}
	seenDigests := map[string]struct{}{}
	for index, item := range items {
		row := anyMap(item)
		refID, err := frontierT8Identifier(row["ref_id"], fmt.Sprintf("evidence_refs[%d].ref_id", index))
		if err != nil {
			return nil, err
		}
		digest := strings.ToLower(strings.TrimSpace(anyToString(row["digest"])))
		resolvedDigest := strings.ToLower(strings.TrimSpace(anyToString(row["resolved_digest"])))
		if !frontierT8SHA256Pattern.MatchString(digest) || resolvedDigest != digest || !anyToBool(row["resolved"]) {
			return nil, fmt.Errorf("evidence ref %q is unresolved or has a mismatched digest", refID)
		}
		if !anyToBool(row["verification_passed"]) {
			return nil, fmt.Errorf("evidence ref %q lacks passing verification", refID)
		}
		producer := firstNonEmptyStrings(anyToString(row["producer_id"]), defaultProducer)
		verifier := firstNonEmptyStrings(anyToString(row["verifier_id"]), defaultVerifier)
		producer, err = frontierT8Identifier(producer, fmt.Sprintf("evidence_refs[%d].producer_id", index))
		if err != nil {
			return nil, err
		}
		verifier, err = frontierT8Identifier(verifier, fmt.Sprintf("evidence_refs[%d].verifier_id", index))
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(producer, verifier) {
			return nil, fmt.Errorf("evidence ref %q is not independently verified", refID)
		}
		verificationID, err := frontierT8Identifier(row["verification_id"], fmt.Sprintf("evidence_refs[%d].verification_id", index))
		if err != nil {
			return nil, err
		}
		kind, err := frontierT8BoundedText(row["kind"], fmt.Sprintf("evidence_refs[%d].kind", index), 80, true)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seenIDs[refID]; duplicate {
			return nil, fmt.Errorf("evidence ref_id %q is duplicated", refID)
		}
		if _, duplicate := seenDigests[digest]; duplicate {
			return nil, fmt.Errorf("evidence digest %q is duplicated", digest)
		}
		seenIDs[refID] = struct{}{}
		seenDigests[digest] = struct{}{}
		refs = append(refs, frontierT8EvidenceRef{
			RefID: refID, Kind: kind, Digest: digest, ResolvedDigest: resolvedDigest,
			ProducerID: producer, VerifierID: verifier, VerificationID: verificationID,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].RefID < refs[j].RefID })
	return refs, nil
}

func frontierT8EvidenceMaps(refs []frontierT8EvidenceRef) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]any{
			"ref_id": ref.RefID, "kind": ref.Kind, "digest": ref.Digest, "resolved_digest": ref.ResolvedDigest,
			"resolved": true, "verification_passed": true, "producer_id": ref.ProducerID,
			"verifier_id": ref.VerifierID, "verification_id": ref.VerificationID,
		})
	}
	return out
}

func frontierT8ReceiptProvenance(receipts []frontierT8WorkflowReceipt) []any {
	out := make([]any, 0, len(receipts))
	for _, receipt := range receipts {
		out = append(out, map[string]any{
			"receipt_id": receipt.ReceiptID, "receipt_digest": receipt.ReceiptDigest,
			"previous_receipt_digest": receipt.PreviousReceiptDigest, "fixture_id": receipt.FixtureID,
			"environment_id": receipt.EnvironmentID, "producer_id": receipt.ProducerID,
			"verifier_id": receipt.VerifierID, "verified_at": receipt.VerifiedAtText,
			"evidence_refs": frontierT8EvidenceMaps(receipt.Evidence),
		})
	}
	return out
}

func frontierT8FoundryRun(receipt frontierT8WorkflowReceipt, holdout bool) map[string]any {
	evidence := make([]any, 0, len(receipt.Evidence))
	for _, ref := range receipt.Evidence {
		evidence = append(evidence, ref.RefID+"@"+ref.Digest)
	}
	row := map[string]any{
		"run_id": receipt.ReceiptID, "verified": true, "verification_passed": true,
		"success": true, "checks_passed": true, "steps": stringSliceAny(receipt.Steps),
		"checks": stringSliceAny(receipt.Checks), "evidence_refs": evidence,
	}
	if holdout {
		row["holdout_id"] = receipt.ReceiptID
	}
	return row
}

func frontierT8ReceiptDigestList(receipts []frontierT8WorkflowReceipt) []any {
	out := make([]any, 0, len(receipts))
	for _, receipt := range receipts {
		out = append(out, receipt.ReceiptDigest)
	}
	return out
}

func frontierT8UniqueVerificationCommands(receipts []frontierT8WorkflowReceipt) []any {
	byDigest := map[string]string{}
	for _, receipt := range receipts {
		byDigest[receipt.VerificationCommandDigest] = receipt.VerificationCommand
	}
	digests := make([]string, 0, len(byDigest))
	for digest := range byDigest {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	out := make([]any, 0, len(digests))
	for _, digest := range digests {
		out = append(out, map[string]any{"command": byDigest[digest], "digest": digest})
	}
	return out
}

func frontierT8AggregateEconomics(receipts []frontierT8WorkflowReceipt) map[string]any {
	var total frontierT8ReceiptEconomics
	latencies := make([]int64, 0, len(receipts))
	for _, receipt := range receipts {
		total.InputTokens += receipt.Economics.InputTokens
		total.OutputTokens += receipt.Economics.OutputTokens
		total.ToolCalls += receipt.Economics.ToolCalls
		total.ProviderCostMicros += receipt.Economics.ProviderCostMicros
		total.LatencyMS += receipt.Economics.LatencyMS
		total.NetworkCalls += receipt.Economics.NetworkCalls
		total.ModelCalls += receipt.Economics.ModelCalls
		total.ExecutionCount += receipt.Economics.ExecutionCount
		latencies = append(latencies, receipt.Economics.LatencyMS)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := int64(0)
	if len(latencies) > 0 {
		index := int(math.Ceil(float64(len(latencies))*0.95)) - 1
		p95 = latencies[maxInt(0, minInt(index, len(latencies)-1))]
	}
	meanLatency := 0.0
	if len(receipts) > 0 {
		meanLatency = roundFloat(float64(total.LatencyMS)/float64(len(receipts)), 3)
	}
	return map[string]any{
		"exact": true, "receipt_count": len(receipts), "input_tokens": total.InputTokens,
		"output_tokens": total.OutputTokens, "tool_calls": total.ToolCalls,
		"provider_cost_micros": total.ProviderCostMicros, "latency_ms_total": total.LatencyMS,
		"latency_ms_mean": meanLatency, "latency_ms_p95": p95,
		"network_calls": total.NetworkCalls, "model_calls": total.ModelCalls, "execution_count": total.ExecutionCount,
	}
}

func frontierT8ChangeSignal(raw any, field string) (map[string]any, error) {
	row := anyMap(raw)
	detected := anyToBool(row["detected"])
	severity := strings.ToLower(strings.TrimSpace(anyToString(row["severity"])))
	if severity == "" {
		severity = "none"
	}
	allowed := map[string]struct{}{"none": {}, "low": {}, "medium": {}, "high": {}, "critical": {}}
	if _, ok := allowed[severity]; !ok {
		return nil, fmt.Errorf("%s.severity is invalid", field)
	}
	summary, err := frontierT8BoundedText(row["summary"], field+".summary", 400, detected)
	if err != nil {
		return nil, err
	}
	refs := []frontierT8EvidenceRef{}
	if detected {
		refs, err = frontierT8NormalizeEvidence(row["evidence_refs"], "skill_dependency_inventory", "independent_reviewer")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
	}
	return map[string]any{"detected": detected, "severity": severity, "summary": summary, "evidence_refs": frontierT8EvidenceMaps(refs)}, nil
}

func frontierT8Replacement(raw any) (map[string]any, error) {
	row := anyMap(raw)
	if len(row) == 0 || !anyToBool(firstPresentAny(row["present"], true)) {
		return map[string]any{"present": false, "verified": false, "coverage_ratio": 0.0, "evidence_refs": []any{}}, nil
	}
	skillID, err := frontierT8Identifier(row["skill_id"], "replacement.skill_id")
	if err != nil {
		return nil, err
	}
	coverage, err := frontierT8Rate(row["coverage_ratio"], "replacement.coverage_ratio")
	if err != nil {
		return nil, err
	}
	coverageBasis, err := frontierT8BoundedText(row["coverage_basis"], "replacement.coverage_basis", 400, true)
	if err != nil {
		return nil, err
	}
	refs, err := frontierT8NormalizeEvidence(row["evidence_refs"], "replacement_evaluator", "independent_reviewer")
	if err != nil {
		return nil, fmt.Errorf("replacement: %w", err)
	}
	return map[string]any{
		"present": true, "skill_id": skillID, "verified": anyToBool(row["verified"]),
		"coverage_ratio": coverage, "coverage_basis": coverageBasis, "evidence_refs": frontierT8EvidenceMaps(refs),
	}, nil
}

func frontierT8Impact(raw any) (map[string]any, error) {
	row := anyMap(raw)
	severity := strings.ToLower(strings.TrimSpace(anyToString(row["severity"])))
	allowed := map[string]struct{}{"low": {}, "medium": {}, "high": {}, "critical": {}}
	if _, ok := allowed[severity]; !ok {
		return nil, errors.New("impact.severity must be low, medium, high, or critical")
	}
	summary, err := frontierT8BoundedText(row["summary"], "impact.summary", 500, true)
	if err != nil {
		return nil, err
	}
	affected, err := frontierT8RequiredNonNegativeInt(row, "affected_workflows")
	if err != nil {
		return nil, fmt.Errorf("impact: %w", err)
	}
	valuePerUse, err := frontierT8RequiredNonNegativeInt(row, "value_per_use_micros")
	if err != nil {
		return nil, fmt.Errorf("impact: %w", err)
	}
	return map[string]any{
		"severity": severity, "summary": summary, "affected_workflows": affected,
		"value_per_use_micros": valuePerUse, "user_visible": anyToBool(row["user_visible"]),
	}, nil
}

func frontierT8Identifier(raw any, field string) (string, error) {
	value := strings.TrimSpace(anyToString(raw))
	if !frontierT8IdentifierPattern.MatchString(value) {
		return "", fmt.Errorf("%s must be a bounded identifier", field)
	}
	return value, nil
}

func frontierT8Timestamp(raw any, field string) (time.Time, string, error) {
	value := strings.TrimSpace(anyToString(raw))
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	parsed = parsed.UTC()
	return parsed, parsed.Format(time.RFC3339Nano), nil
}

func frontierT8BoundedText(raw any, field string, maxBytes int, required bool) (string, error) {
	value := strings.Join(strings.Fields(anyToString(raw)), " ")
	if value == "" && required {
		return "", fmt.Errorf("%s is required", field)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	if err := frontierT8RejectUnsafeValue(value, field, 0); err != nil {
		return "", err
	}
	return value, nil
}

func frontierT8StringList(raw any, field string, limit, maxBytes int, required bool) ([]string, error) {
	items := contextPackAnyList(raw)
	if required && len(items) == 0 {
		return nil, fmt.Errorf("%s is required", field)
	}
	if len(items) > limit {
		return nil, fmt.Errorf("%s exceeds %d entries", field, limit)
	}
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for index, item := range items {
		value, err := frontierT8BoundedText(item, fmt.Sprintf("%s[%d]", field, index), maxBytes, true)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(value)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	if required && len(out) == 0 {
		return nil, fmt.Errorf("%s is required", field)
	}
	return out, nil
}

func frontierT8RequiredNonNegativeInt(row map[string]any, field string) (int64, error) {
	raw, ok := row[field]
	if !ok {
		return 0, fmt.Errorf("%s is required", field)
	}
	value, ok := frontierT8Int64(raw)
	if !ok || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", field)
	}
	return value, nil
}

func frontierT8Int64(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case int32:
		return int64(value), true
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case uint64:
		if value > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value > math.MaxInt64 || value < math.MinInt64 {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func frontierT8Rate(raw any, field string) (float64, error) {
	value, ok := frontierT8Float(raw)
	if !ok || value < 0 || value > 1 {
		return 0, fmt.Errorf("%s must be between 0 and 1", field)
	}
	return roundFloat(value, 6), nil
}

func frontierT8Float(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, !math.IsNaN(value) && !math.IsInf(value, 0)
	case float32:
		converted := float64(value)
		return converted, !math.IsNaN(converted) && !math.IsInf(converted, 0)
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		return 0, false
	}
}

func frontierT8ClampedInt(raw any, fallback, minimum, maximum int) int {
	value, ok := frontierT8Int64(raw)
	if !ok {
		return fallback
	}
	return clampInt(int(value), minimum, maximum)
}

func frontierT8ClampedFloat(raw any, fallback, minimum, maximum float64) float64 {
	value, ok := frontierT8Float(raw)
	if !ok {
		return fallback
	}
	return math.Max(minimum, math.Min(maximum, value))
}

func frontierT8EnsureBounded(payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode advisory candidate: %w", err)
	}
	if len(raw) > frontierT8MaxOutputBytes {
		return fmt.Errorf("advisory candidate exceeds %d bytes", frontierT8MaxOutputBytes)
	}
	return nil
}
