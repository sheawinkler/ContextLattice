package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
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
	frontierT8MaxCount                         = int64(1_000_000_000)
	frontierT8MaxCostMicros                    = int64(1_000_000_000_000_000)
	frontierT8MaxLatencyMS                     = int64(31_536_000_000)
)

var (
	frontierT8IdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{1,159}$`)
	frontierT8SHA256Pattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	frontierT8SensitiveKey      = regexp.MustCompile(`(?i)(api[_-]?key|(^|[_-])token($|[_-])|secret|password|credential|private[_-]?key|authorization|bearer)`)
	frontierT8SecretValue       = regexp.MustCompile(`(?i)(bearer\s+[A-Za-z0-9._~+/=-]{12,}|sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{12,}|AKIA[A-Z0-9]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
	frontierT8PersonalPath      = regexp.MustCompile(`(?i)(file://|/(?:Users|home|Volumes|private|tmp)/[^\s]+|[A-Z]:\\Users\\[^\\\s]+)`)
	frontierT8UnsafeMaterial    = regexp.MustCompile(`(?i)(\brm\s+-rf\b|\bgit\s+reset\s+--hard\b|\bgit\s+push\b[^\n]*--force|\bchmod\s+-R\b|\bchown\s+-R\b|\bsudo\b|\bcurl\b[^\n|]*\|\s*(sh|bash)\b|\bwget\b[^\n|]*\|\s*(sh|bash)\b)`)
	frontierT8RawMaterialKey    = regexp.MustCompile(`(?i)^(raw[_-]?)?(prompt|content|log|logs|stdout|stderr|transcript|chain[_-]?of[_-]?thought)$`)
	frontierT8ManualStep        = regexp.MustCompile(`(?i)\b(manual(?:ly)?|unrecorded|hidden\s+step|operator\s+intervention|human\s+intervention)\b`)
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
	if err := frontierT8RejectUnknownFields(payload, "payload",
		"project", "name", "description", "as_of", "minimum_training_receipts",
		"minimum_holdout_receipts", "max_verification_age_days", "training_receipts", "holdout_receipts"); err != nil {
		return nil, err
	}
	projectInput := "contextlattice"
	if rawProject, exists := payload["project"]; exists {
		projectText, ok := rawProject.(string)
		if !ok {
			return nil, errors.New("project must be a string")
		}
		projectInput = projectText
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(projectInput, "contextlattice"))
	if err != nil {
		return nil, err
	}
	nameInput, ok := payload["name"].(string)
	if !ok {
		return nil, errors.New("name must be a string")
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(nameInput), "_", "-"))
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
	minimumTraining, err := frontierT8OptionalBoundedInt(payload, "minimum_training_receipts", 3, 3, 20)
	if err != nil {
		return nil, err
	}
	minimumHoldouts, err := frontierT8OptionalBoundedInt(payload, "minimum_holdout_receipts", 3, 3, 20)
	if err != nil {
		return nil, err
	}
	maxAgeDays, err := frontierT8OptionalBoundedInt(payload, "max_verification_age_days", 30, 1, 365)
	if err != nil {
		return nil, err
	}

	training, err := frontierT8NormalizeReceiptChain(payload["training_receipts"], "training", minimumTraining, asOf, maxAgeDays)
	if err != nil {
		return nil, fmt.Errorf("training receipts: %w", err)
	}
	holdouts, err := frontierT8NormalizeReceiptChain(payload["holdout_receipts"], "holdout", minimumHoldouts, asOf, maxAgeDays)
	if err != nil {
		return nil, fmt.Errorf("holdout receipts: %w", err)
	}
	if len(training)+len(holdouts) > frontierT8MaxReceipts {
		return nil, fmt.Errorf("total receipt count exceeds %d", frontierT8MaxReceipts)
	}
	workflowID := training[0].WorkflowID
	workflowSignature := training[0].WorkflowSignature
	for _, receipt := range append(append([]frontierT8WorkflowReceipt{}, training...), holdouts...) {
		if receipt.WorkflowID != workflowID || receipt.WorkflowSignature != workflowSignature {
			return nil, errors.New("superficially similar workflows are not reusable: exact workflow identity and bounded material must match")
		}
	}
	if err := frontierT8VerifyReceiptSeparation(training, holdouts); err != nil {
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
		"project": project, "name": name, "description": description,
		"workflow_id": workflowID, "workflow_signature": workflowSignature,
		"training_receipts": frontierT8ReceiptDigestList(training), "holdout_receipts": frontierT8ReceiptDigestList(holdouts),
	}
	seedRaw, _ := json.Marshal(seed)
	candidateID := "skillcand_" + sha256Hex(string(seedRaw))[:24]
	candidate := map[string]any{
		"schema_id": frontierT8ReusableSkillCandidateSchemaID, "version": 1,
		"candidate_id": candidateID, "project": project, "name": name, "description": description,
		"candidate_kind": "runbook_and_skill", "status": "inactive", "as_of": asOfText,
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
				"Receipt and hash material are deterministically checked in-kernel; the HTTP boundary authoritatively re-resolves every ref before returning or persisting a candidate.",
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
		"persistence": map[string]any{
			"mode": "advisory_non_persisting", "candidate_persisted": false,
			"performed": false, "explicit_foundry_handoff_required": true,
		},
		"skill_foundry_handoff": map[string]any{
			"target_contract": skillDraftContractID, "target_surface": "skill_foundry_draft",
			"ready": true, "automatic_submit": false, "automatic_export": false,
			"draft_payload": map[string]any{
				"project": project, "name": name, "description": description,
				"minimum_verified_runs": minimumTraining, "workflow_runs": foundryRuns,
				"source_candidate_id": candidateID, "source_workflow_signature": workflowSignature,
				"prerequisites": stringSliceAny(training[0].Prerequisites), "rollback": stringSliceAny(training[0].Rollback),
				"side_effects": stringSliceAny(training[0].SideEffects), "platform_constraints": stringSliceAny(training[0].PlatformConstraints),
				"verification_commands": verificationCommands,
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
	if err := frontierT8RejectUnknownFields(payload, "payload",
		"project", "skill_id", "name", "skill_version", "as_of", "last_verified_at",
		"review_window", "metrics", "thresholds", "evidence_refs", "security_change",
		"dependency_change", "replacement", "impact", "seasonality", "rare_high_value"); err != nil {
		return nil, err
	}
	projectInput := "contextlattice"
	if rawProject, exists := payload["project"]; exists {
		projectText, ok := rawProject.(string)
		if !ok {
			return nil, errors.New("project must be a string")
		}
		projectInput = projectText
	}
	project, err := sanitizeMemoryProject(firstNonEmptyStrings(projectInput, "contextlattice"))
	if err != nil {
		return nil, err
	}
	skillID, err := frontierT8Identifier(payload["skill_id"], "skill_id")
	if err != nil {
		return nil, err
	}
	nameInput, ok := payload["name"].(string)
	if !ok {
		return nil, errors.New("name must be a string")
	}
	name := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(nameInput), "_", "-"))
	if !skillFoundryNamePattern.MatchString(name) {
		return nil, errors.New("name must be 2-64 lowercase letters, digits, or hyphens")
	}
	skillVersion, err := frontierT8OptionalBoundedInt(payload, "skill_version", 1, 1, 1_000_000)
	if err != nil {
		return nil, err
	}
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
	if err := frontierT8RejectUnknownFields(window, "review_window", "start_at", "end_at"); err != nil {
		return nil, err
	}
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
	if windowEnd.Sub(windowStart) > 366*24*time.Hour {
		return nil, errors.New("review_window cannot exceed 366 days")
	}

	metrics := anyMap(payload["metrics"])
	if err := frontierT8RejectUnknownFields(metrics, "metrics",
		"baseline_verified_success_rate", "current_verified_success_rate", "baseline_sample_count",
		"current_sample_count", "use_count", "verified_regression_count",
		"temporary_provider_failure_count", "network_calls", "model_calls", "execution_count",
		"total_cost_micros", "total_latency_ms"); err != nil {
		return nil, err
	}
	baselineRate, err := frontierT8Rate(metrics["baseline_verified_success_rate"], "metrics.baseline_verified_success_rate")
	if err != nil {
		return nil, err
	}
	currentRate, err := frontierT8Rate(metrics["current_verified_success_rate"], "metrics.current_verified_success_rate")
	if err != nil {
		return nil, err
	}
	baselineSamples, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "baseline_sample_count", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	currentSamples, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "current_sample_count", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	useCount, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "use_count", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	regressions, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "verified_regression_count", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	temporaryProviderFailures, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "temporary_provider_failure_count", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	networkCalls, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "network_calls", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	modelCalls, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "model_calls", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	executionCount, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "execution_count", frontierT8MaxCount)
	if err != nil {
		return nil, err
	}
	totalCostMicros, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "total_cost_micros", frontierT8MaxCostMicros)
	if err != nil {
		return nil, err
	}
	totalLatencyMS, err := frontierT8RequiredBoundedNonNegativeInt(metrics, "total_latency_ms", frontierT8MaxLatencyMS)
	if err != nil {
		return nil, err
	}

	thresholds := anyMap(payload["thresholds"])
	if err := frontierT8RejectUnknownFields(thresholds, "thresholds", "minimum_samples", "efficacy_decay", "stale_days", "low_use_count", "verified_regressions", "replacement_coverage", "rare_value_per_use_micros"); err != nil {
		return nil, err
	}
	minimumSamplesValue, err := frontierT8OptionalBoundedInt(thresholds, "minimum_samples", 20, 10, 1000)
	if err != nil {
		return nil, err
	}
	decayThreshold, err := frontierT8OptionalBoundedFloat(thresholds, "efficacy_decay", 0.15, 0.05, 0.50)
	if err != nil {
		return nil, err
	}
	staleDaysValue, err := frontierT8OptionalBoundedInt(thresholds, "stale_days", 90, 30, 730)
	if err != nil {
		return nil, err
	}
	lowUseValue, err := frontierT8OptionalBoundedInt(thresholds, "low_use_count", 2, 0, 100)
	if err != nil {
		return nil, err
	}
	regressionValue, err := frontierT8OptionalBoundedInt(thresholds, "verified_regressions", 3, 1, 100)
	if err != nil {
		return nil, err
	}
	replacementCoverageThreshold, err := frontierT8OptionalBoundedFloat(thresholds, "replacement_coverage", 0.95, 0.80, 1.0)
	if err != nil {
		return nil, err
	}
	rareValueThresholdValue, err := frontierT8OptionalBoundedInt(thresholds, "rare_value_per_use_micros", 1_000_000, 1_000, 1_000_000_000)
	if err != nil {
		return nil, err
	}
	minimumSamples := int64(minimumSamplesValue)
	staleDaysThreshold := int64(staleDaysValue)
	lowUseThreshold := int64(lowUseValue)
	regressionThreshold := int64(regressionValue)
	rareValueThreshold := int64(rareValueThresholdValue)

	evidence, err := frontierT8NormalizeEvidence(payload["evidence_refs"], "skill_telemetry", "independent_reviewer")
	if err != nil {
		return nil, fmt.Errorf("retirement evidence: %w", err)
	}
	securityChange, securityEvidence, err := frontierT8ChangeSignal(payload["security_change"], "security_change")
	if err != nil {
		return nil, err
	}
	dependencyChange, dependencyEvidence, err := frontierT8ChangeSignal(payload["dependency_change"], "dependency_change")
	if err != nil {
		return nil, err
	}
	replacement, replacementEvidence, err := frontierT8Replacement(payload["replacement"])
	if err != nil {
		return nil, err
	}
	if err := frontierT8VerifyEvidenceSeparation(map[string][]frontierT8EvidenceRef{
		"retirement": evidence, "security_change": securityEvidence,
		"dependency_change": dependencyEvidence, "replacement": replacementEvidence,
	}); err != nil {
		return nil, err
	}
	impact, err := frontierT8Impact(payload["impact"])
	if err != nil {
		return nil, err
	}
	seasonality := anyMap(payload["seasonality"])
	if err := frontierT8RejectUnknownFields(seasonality, "seasonality", "seasonal", "full_observation_cycle", "season_id"); err != nil {
		return nil, err
	}
	seasonal, err := frontierT8RequiredBool(seasonality, "seasonal")
	if err != nil {
		return nil, err
	}
	fullSeasonObserved, err := frontierT8RequiredBool(seasonality, "full_observation_cycle")
	if err != nil {
		return nil, err
	}
	seasonID, err := frontierT8BoundedText(seasonality["season_id"], "seasonality.season_id", 120, true)
	if err != nil {
		return nil, err
	}
	rareHighValueDeclared, err := frontierT8RequiredBool(payload, "rare_high_value")
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
	rareHighValue := rareHighValueDeclared || (lowUseSignal && int64(valuePerUseMicros) >= rareValueThreshold)
	temporaryProviderNoise := regressions > 0 && temporaryProviderFailures*2 >= regressions
	replacementPresent := anyToBool(replacement["present"])
	replacementVerified := anyToBool(replacement["verified"])
	replacementCoverage := anyToFloat(replacement["coverage_ratio"])
	narrowerReplacement := replacementPresent && (!replacementVerified || replacementCoverage < replacementCoverageThreshold)

	protections := make([]any, 0, 4)
	if seasonal {
		protections = append(protections, "seasonal_skill")
	}
	if seasonal && !fullSeasonObserved {
		protections = append(protections, "seasonal_evidence_window_incomplete")
	}
	if rareHighValue {
		protections = append(protections, "rare_high_value_skill")
	}
	if temporaryProviderFailures > 0 {
		protections = append(protections, "temporary_provider_failure_observed")
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
		"project": project, "skill_id": skillID, "skill_version": skillVersion,
		"signals": signals, "replacement": replacement, "protections": protections,
		"evidence": map[string]any{
			"retirement": frontierT8EvidenceMaps(evidence), "security_change": frontierT8EvidenceMaps(securityEvidence),
			"dependency_change": frontierT8EvidenceMaps(dependencyEvidence), "replacement": frontierT8EvidenceMaps(replacementEvidence),
		},
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
			if frontierT8RawMaterialKey.MatchString(strings.TrimSpace(key)) {
				return fmt.Errorf("%s.%s contains raw prompt or content material", path, key)
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
	if err := frontierT8RejectUnknownFields(row, "receipt",
		"schema_id", "receipt_id", "workflow_id", "partition", "fixture_id", "environment_id",
		"producer_id", "verifier_id", "success", "verification_passed", "checks_passed",
		"verified_at", "verification_command", "verification_command_digest", "steps", "checks",
		"prerequisites", "rollback", "side_effects", "platform_constraints", "evidence_refs",
		"cost", "latency_ms", "network_calls", "model_calls", "execution_count",
		"previous_receipt_digest", "receipt_digest", "step_inventory_complete", "manual_steps_required"); err != nil {
		return receipt, err
	}
	if anyToString(row["schema_id"]) != "workflow_receipt.v1" {
		return receipt, errors.New("schema_id must be workflow_receipt.v1")
	}
	completeInventory, err := frontierT8RequiredBool(row, "step_inventory_complete")
	if err != nil || !completeInventory {
		return receipt, errors.New("step_inventory_complete=true is required")
	}
	manualStepsRequired, err := frontierT8RequiredBool(row, "manual_steps_required")
	if err != nil {
		return receipt, err
	}
	if manualStepsRequired {
		return receipt, errors.New("manual workflow steps are not reusable")
	}
	receipt.ReceiptID, err = frontierT8Identifier(row["receipt_id"], "receipt_id")
	if err != nil {
		return receipt, err
	}
	receipt.WorkflowID, err = frontierT8Identifier(row["workflow_id"], "workflow_id")
	if err != nil {
		return receipt, err
	}
	partitionInput, ok := row["partition"].(string)
	if !ok {
		return receipt, errors.New("partition must be a string")
	}
	receipt.Partition = strings.ToLower(strings.TrimSpace(partitionInput))
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
	success, successErr := frontierT8RequiredBool(row, "success")
	verified, verifiedErr := frontierT8RequiredBool(row, "verification_passed")
	checksPassed, checksErr := frontierT8RequiredBool(row, "checks_passed")
	if successErr != nil || verifiedErr != nil || checksErr != nil {
		return receipt, errors.New("success, verification_passed, and checks_passed must be booleans")
	}
	if !success || !verified || !checksPassed {
		return receipt, errors.New("receipt must be successful with independent verification and checks passed")
	}
	receipt.VerifiedAt, receipt.VerifiedAtText, err = frontierT8Timestamp(row["verified_at"], "verified_at")
	if err != nil {
		return receipt, err
	}
	if receipt.VerifiedAt.After(asOf) {
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
	for _, value := range append(append(append(append(append([]string{}, receipt.Steps...), receipt.Checks...), receipt.Prerequisites...), receipt.Rollback...), receipt.SideEffects...) {
		if frontierT8ManualStep.MatchString(value) {
			return receipt, errors.New("manual or hidden workflow material is not reusable")
		}
	}
	receipt.WorkflowSignature = frontierT8WorkflowSignature(receipt)
	receipt.Evidence, err = frontierT8NormalizeEvidence(row["evidence_refs"], receipt.ProducerID, receipt.VerifierID)
	if err != nil {
		return receipt, err
	}
	for _, ref := range receipt.Evidence {
		if !strings.EqualFold(ref.ProducerID, receipt.ProducerID) || !strings.EqualFold(ref.VerifierID, receipt.VerifierID) {
			return receipt, fmt.Errorf("evidence ref %q is not bound to the receipt producer and verifier", ref.RefID)
		}
	}
	cost := anyMap(row["cost"])
	if err := frontierT8RejectUnknownFields(cost, "cost", "input_tokens", "output_tokens", "tool_calls", "provider_cost_micros"); err != nil {
		return receipt, err
	}
	if receipt.Economics.InputTokens, err = frontierT8RequiredBoundedNonNegativeInt(cost, "input_tokens", frontierT8MaxCount); err != nil {
		return receipt, err
	}
	if receipt.Economics.OutputTokens, err = frontierT8RequiredBoundedNonNegativeInt(cost, "output_tokens", frontierT8MaxCount); err != nil {
		return receipt, err
	}
	if receipt.Economics.ToolCalls, err = frontierT8RequiredBoundedNonNegativeInt(cost, "tool_calls", frontierT8MaxCount); err != nil {
		return receipt, err
	}
	if receipt.Economics.ProviderCostMicros, err = frontierT8RequiredBoundedNonNegativeInt(cost, "provider_cost_micros", frontierT8MaxCostMicros); err != nil {
		return receipt, err
	}
	if receipt.Economics.LatencyMS, err = frontierT8RequiredBoundedNonNegativeInt(row, "latency_ms", frontierT8MaxLatencyMS); err != nil {
		return receipt, err
	}
	if receipt.Economics.NetworkCalls, err = frontierT8RequiredBoundedNonNegativeInt(row, "network_calls", frontierT8MaxCount); err != nil {
		return receipt, err
	}
	if receipt.Economics.ModelCalls, err = frontierT8RequiredBoundedNonNegativeInt(row, "model_calls", frontierT8MaxCount); err != nil {
		return receipt, err
	}
	if receipt.Economics.ExecutionCount, err = frontierT8RequiredBoundedNonNegativeInt(row, "execution_count", frontierT8MaxCount); err != nil {
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
		"verification_command_digest": receipt.VerificationCommandDigest,
	})
	return "sha256:" + sha256Hex(string(raw))
}

func frontierT8VerifyReceiptSeparation(training, holdouts []frontierT8WorkflowReceipt) error {
	type origin struct {
		partition string
		receiptID string
	}
	claim := func(seen map[string]origin, kind, value string, current origin) error {
		key := strings.ToLower(value)
		if previous, exists := seen[key]; exists {
			if previous.partition != current.partition {
				return fmt.Errorf("training/holdout leakage: %s %q overlaps", kind, value)
			}
			return fmt.Errorf("%s %q overlaps receipts %q and %q", kind, value, previous.receiptID, current.receiptID)
		}
		seen[key] = current
		return nil
	}

	receiptIDs := map[string]origin{}
	receiptDigests := map[string]origin{}
	fixtureIDs := map[string]origin{}
	environmentIDs := map[string]origin{}
	evidenceIDs := map[string]origin{}
	evidenceDigests := map[string]origin{}
	verificationIDs := map[string]origin{}
	producers := map[string]origin{}
	verifiers := map[string]origin{}
	all := append(append([]frontierT8WorkflowReceipt{}, training...), holdouts...)
	for _, receipt := range all {
		current := origin{partition: receipt.Partition, receiptID: receipt.ReceiptID}
		for _, item := range []struct {
			seen  map[string]origin
			kind  string
			value string
		}{
			{receiptIDs, "receipt_id", receipt.ReceiptID},
			{receiptDigests, "receipt_digest", receipt.ReceiptDigest},
			{fixtureIDs, "fixture_id", receipt.FixtureID},
			{environmentIDs, "environment_id", receipt.EnvironmentID},
		} {
			if err := claim(item.seen, item.kind, item.value, current); err != nil {
				return err
			}
		}
		producers[strings.ToLower(receipt.ProducerID)] = current
		verifiers[strings.ToLower(receipt.VerifierID)] = current
		for _, ref := range receipt.Evidence {
			if err := claim(evidenceIDs, "evidence ref_id", ref.RefID, current); err != nil {
				return err
			}
			if err := claim(evidenceDigests, "evidence digest", ref.Digest, current); err != nil {
				return err
			}
			if err := claim(verificationIDs, "evidence verification_id", ref.VerificationID, current); err != nil {
				return err
			}
			producers[strings.ToLower(ref.ProducerID)] = current
			verifiers[strings.ToLower(ref.VerifierID)] = current
		}
	}
	for identity, producer := range producers {
		if verifier, overlaps := verifiers[identity]; overlaps {
			return fmt.Errorf("producer/verifier independence failed for %q across receipts %q and %q", identity, producer.receiptID, verifier.receiptID)
		}
	}
	for identity, fixture := range fixtureIDs {
		if environment, overlaps := environmentIDs[identity]; overlaps {
			return fmt.Errorf("fixture/environment overlap %q across receipts %q and %q", identity, fixture.receiptID, environment.receiptID)
		}
	}
	for identity, receipt := range receiptIDs {
		if evidence, overlaps := evidenceIDs[identity]; overlaps {
			return fmt.Errorf("receipt/evidence overlap %q across receipts %q and %q", identity, receipt.receiptID, evidence.receiptID)
		}
	}
	for digest, receipt := range receiptDigests {
		if evidence, overlaps := evidenceDigests[digest]; overlaps {
			return fmt.Errorf("receipt/evidence digest overlap %q across receipts %q and %q", digest, receipt.receiptID, evidence.receiptID)
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
	seenVerificationIDs := map[string]struct{}{}
	for index, item := range items {
		row := anyMap(item)
		if err := frontierT8RejectUnknownFields(row, fmt.Sprintf("evidence_refs[%d]", index),
			"ref_id", "kind", "digest", "resolved_digest", "resolved", "verification_passed",
			"producer_id", "verifier_id", "verification_id"); err != nil {
			return nil, err
		}
		refID, err := frontierT8Identifier(row["ref_id"], fmt.Sprintf("evidence_refs[%d].ref_id", index))
		if err != nil {
			return nil, err
		}
		digest := strings.ToLower(strings.TrimSpace(anyToString(row["digest"])))
		resolvedDigest := strings.ToLower(strings.TrimSpace(anyToString(row["resolved_digest"])))
		resolved, resolvedErr := frontierT8RequiredBool(row, "resolved")
		if resolvedErr != nil || !frontierT8SHA256Pattern.MatchString(digest) || resolvedDigest != digest || !resolved {
			return nil, fmt.Errorf("evidence ref %q is unresolved or has a mismatched digest", refID)
		}
		verificationPassed, verificationErr := frontierT8RequiredBool(row, "verification_passed")
		if verificationErr != nil || !verificationPassed {
			return nil, fmt.Errorf("evidence ref %q lacks passing verification", refID)
		}
		producerRaw := any(defaultProducer)
		if supplied, exists := row["producer_id"]; exists {
			producerRaw = supplied
		}
		verifierRaw := any(defaultVerifier)
		if supplied, exists := row["verifier_id"]; exists {
			verifierRaw = supplied
		}
		producer, err := frontierT8Identifier(producerRaw, fmt.Sprintf("evidence_refs[%d].producer_id", index))
		if err != nil {
			return nil, err
		}
		verifier, err := frontierT8Identifier(verifierRaw, fmt.Sprintf("evidence_refs[%d].verifier_id", index))
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
		if _, duplicate := seenVerificationIDs[verificationID]; duplicate {
			return nil, fmt.Errorf("evidence verification_id %q is duplicated", verificationID)
		}
		seenIDs[refID] = struct{}{}
		seenDigests[digest] = struct{}{}
		seenVerificationIDs[verificationID] = struct{}{}
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

func frontierT8VerifyEvidenceSeparation(groups map[string][]frontierT8EvidenceRef) error {
	type origin struct{ group, refID string }
	ids := map[string]origin{}
	digests := map[string]origin{}
	verificationIDs := map[string]origin{}
	producers := map[string]origin{}
	verifiers := map[string]origin{}
	groupNames := make([]string, 0, len(groups))
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	sort.Strings(groupNames)
	for _, group := range groupNames {
		for _, ref := range groups[group] {
			current := origin{group: group, refID: ref.RefID}
			for _, item := range []struct {
				seen  map[string]origin
				kind  string
				value string
			}{
				{ids, "ref_id", ref.RefID},
				{digests, "digest", ref.Digest},
				{verificationIDs, "verification_id", ref.VerificationID},
			} {
				key := strings.ToLower(item.value)
				if previous, exists := item.seen[key]; exists {
					return fmt.Errorf("retirement evidence %s %q overlaps %s/%s and %s/%s", item.kind, item.value, previous.group, previous.refID, current.group, current.refID)
				}
				item.seen[key] = current
			}
			producers[strings.ToLower(ref.ProducerID)] = current
			verifiers[strings.ToLower(ref.VerifierID)] = current
		}
	}
	for identity, producer := range producers {
		if verifier, overlaps := verifiers[identity]; overlaps {
			return fmt.Errorf("retirement evidence producer/verifier independence failed for %q across %s and %s", identity, producer.group, verifier.group)
		}
	}
	return nil
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

func frontierT8ChangeSignal(raw any, field string) (map[string]any, []frontierT8EvidenceRef, error) {
	row := anyMap(raw)
	if err := frontierT8RejectUnknownFields(row, field, "detected", "severity", "summary", "evidence_refs"); err != nil {
		return nil, nil, err
	}
	detected, err := frontierT8RequiredBool(row, "detected")
	if err != nil {
		return nil, nil, fmt.Errorf("%s.detected must be a boolean", field)
	}
	severity := strings.ToLower(strings.TrimSpace(anyToString(row["severity"])))
	if severity == "" {
		severity = "none"
	}
	allowed := map[string]struct{}{"none": {}, "low": {}, "medium": {}, "high": {}, "critical": {}}
	if _, ok := allowed[severity]; !ok {
		return nil, nil, fmt.Errorf("%s.severity is invalid", field)
	}
	if detected && severity == "none" {
		return nil, nil, fmt.Errorf("%s.severity must be material when detected=true", field)
	}
	if !detected && severity != "none" {
		return nil, nil, fmt.Errorf("%s.severity must be none when detected=false", field)
	}
	summary, err := frontierT8BoundedText(row["summary"], field+".summary", 400, detected)
	if err != nil {
		return nil, nil, err
	}
	refs := []frontierT8EvidenceRef{}
	if detected {
		refs, err = frontierT8NormalizeEvidence(row["evidence_refs"], "skill_dependency_inventory", "independent_reviewer")
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", field, err)
		}
	} else if frontierT8Meaningful(row["summary"]) || frontierT8Meaningful(row["evidence_refs"]) {
		return nil, nil, fmt.Errorf("%s cannot include change evidence when detected=false", field)
	}
	return map[string]any{"detected": detected, "severity": severity, "summary": summary, "evidence_refs": frontierT8EvidenceMaps(refs)}, refs, nil
}

func frontierT8Replacement(raw any) (map[string]any, []frontierT8EvidenceRef, error) {
	row := anyMap(raw)
	if err := frontierT8RejectUnknownFields(row, "replacement", "present", "skill_id", "verified", "coverage_ratio", "coverage_basis", "evidence_refs"); err != nil {
		return nil, nil, err
	}
	if len(row) == 0 {
		return map[string]any{"present": false, "verified": false, "coverage_ratio": 0.0, "coverage_basis": "", "evidence_refs": []any{}}, nil, nil
	}
	present, err := frontierT8RequiredBool(row, "present")
	if err != nil {
		return nil, nil, errors.New("replacement.present must be a boolean")
	}
	if !present {
		for _, key := range []string{"skill_id", "verified", "coverage_ratio", "coverage_basis", "evidence_refs"} {
			if frontierT8Meaningful(row[key]) {
				return nil, nil, errors.New("replacement cannot include identity or coverage when present=false")
			}
		}
		return map[string]any{"present": false, "verified": false, "coverage_ratio": 0.0, "coverage_basis": "", "evidence_refs": []any{}}, nil, nil
	}
	skillID, err := frontierT8Identifier(row["skill_id"], "replacement.skill_id")
	if err != nil {
		return nil, nil, err
	}
	coverage, err := frontierT8Rate(row["coverage_ratio"], "replacement.coverage_ratio")
	if err != nil {
		return nil, nil, err
	}
	coverageBasis, err := frontierT8BoundedText(row["coverage_basis"], "replacement.coverage_basis", 400, true)
	if err != nil {
		return nil, nil, err
	}
	refs, err := frontierT8NormalizeEvidence(row["evidence_refs"], "replacement_evaluator", "independent_reviewer")
	if err != nil {
		return nil, nil, fmt.Errorf("replacement: %w", err)
	}
	verified, err := frontierT8RequiredBool(row, "verified")
	if err != nil {
		return nil, nil, errors.New("replacement.verified must be a boolean")
	}
	return map[string]any{
		"present": true, "skill_id": skillID, "verified": verified,
		"coverage_ratio": coverage, "coverage_basis": coverageBasis, "evidence_refs": frontierT8EvidenceMaps(refs),
	}, refs, nil
}

func frontierT8Impact(raw any) (map[string]any, error) {
	row := anyMap(raw)
	if err := frontierT8RejectUnknownFields(row, "impact", "severity", "summary", "affected_workflows", "value_per_use_micros", "user_visible"); err != nil {
		return nil, err
	}
	severity := strings.ToLower(strings.TrimSpace(anyToString(row["severity"])))
	allowed := map[string]struct{}{"low": {}, "medium": {}, "high": {}, "critical": {}}
	if _, ok := allowed[severity]; !ok {
		return nil, errors.New("impact.severity must be low, medium, high, or critical")
	}
	summary, err := frontierT8BoundedText(row["summary"], "impact.summary", 500, true)
	if err != nil {
		return nil, err
	}
	affected, err := frontierT8RequiredBoundedNonNegativeInt(row, "affected_workflows", frontierT8MaxCount)
	if err != nil {
		return nil, fmt.Errorf("impact: %w", err)
	}
	valuePerUse, err := frontierT8RequiredBoundedNonNegativeInt(row, "value_per_use_micros", frontierT8MaxCostMicros)
	if err != nil {
		return nil, fmt.Errorf("impact: %w", err)
	}
	userVisible, err := frontierT8RequiredBool(row, "user_visible")
	if err != nil {
		return nil, errors.New("impact.user_visible must be a boolean")
	}
	return map[string]any{
		"severity": severity, "summary": summary, "affected_workflows": affected,
		"value_per_use_micros": valuePerUse, "user_visible": userVisible,
	}, nil
}

func frontierT8Identifier(raw any, field string) (string, error) {
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string identifier", field)
	}
	value = strings.TrimSpace(value)
	if !frontierT8IdentifierPattern.MatchString(value) {
		return "", fmt.Errorf("%s must be a bounded identifier", field)
	}
	return value, nil
}

func frontierT8Timestamp(raw any, field string) (time.Time, string, error) {
	value, ok := raw.(string)
	if !ok {
		return time.Time{}, "", fmt.Errorf("%s must be an RFC3339 string", field)
	}
	value = strings.TrimSpace(value)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%s must be RFC3339: %w", field, err)
	}
	parsed = parsed.UTC()
	if parsed.Year() < 2000 || parsed.Year() > 2200 {
		return time.Time{}, "", fmt.Errorf("%s must be between years 2000 and 2200", field)
	}
	return parsed, parsed.Format(time.RFC3339Nano), nil
}

func frontierT8BoundedText(raw any, field string, maxBytes int, required bool) (string, error) {
	if raw == nil {
		if required {
			return "", fmt.Errorf("%s is required", field)
		}
		return "", nil
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", field)
	}
	value := strings.Join(strings.Fields(text), " ")
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
	if raw != nil {
		switch raw.(type) {
		case []any, []string:
		default:
			return nil, fmt.Errorf("%s must be an array of strings", field)
		}
	}
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
			return nil, fmt.Errorf("%s contains duplicate entry %q", field, value)
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
	return frontierT8RequiredBoundedNonNegativeInt(row, field, math.MaxInt64)
}

func frontierT8RequiredBoundedNonNegativeInt(row map[string]any, field string, maximum int64) (int64, error) {
	raw, ok := row[field]
	if !ok {
		return 0, fmt.Errorf("%s is required", field)
	}
	value, ok := frontierT8Int64(raw)
	if !ok || value < 0 || value > maximum {
		return 0, fmt.Errorf("%s must be a non-negative integer no greater than %d", field, maximum)
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

func frontierT8OptionalBoundedInt(row map[string]any, field string, fallback, minimum, maximum int) (int, error) {
	raw, exists := row[field]
	if !exists {
		return fallback, nil
	}
	value, ok := frontierT8Int64(raw)
	if !ok || value < int64(minimum) || value > int64(maximum) {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", field, minimum, maximum)
	}
	return int(value), nil
}

func frontierT8OptionalBoundedFloat(row map[string]any, field string, fallback, minimum, maximum float64) (float64, error) {
	raw, exists := row[field]
	if !exists {
		return fallback, nil
	}
	value, ok := frontierT8Float(raw)
	if !ok || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %v and %v", field, minimum, maximum)
	}
	return roundFloat(value, 6), nil
}

func frontierT8RequiredBool(row map[string]any, field string) (bool, error) {
	raw, exists := row[field]
	if !exists {
		return false, fmt.Errorf("%s is required", field)
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", field)
	}
	return value, nil
}

func frontierT8RejectUnknownFields(row map[string]any, field string, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	unknown := make([]string, 0)
	for key := range row {
		if _, ok := allowedSet[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("%s contains unknown field %q", field, unknown[0])
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
