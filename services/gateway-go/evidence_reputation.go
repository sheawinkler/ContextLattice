package main

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	evidenceReputationContractID       = "evidence_reputation.v1"
	evidenceReputationPath             = "/telemetry/evidence-reputation"
	evidenceReputationDefaultMinSample = 5
	evidenceReputationMaxRows          = 256
	evidenceReputationMaxEntries       = 100
	evidenceReputationMaxAttributions  = 64
	evidenceReputationHalfLifeDays     = 30.0
)

type evidenceReputationOptions struct {
	Project         string
	TaskClass       string
	RetrievalIntent string
	WorkspaceRef    string
	AsOf            time.Time
	MinimumSamples  int
	MaxEntries      int
}

type evidenceReputationAccumulator struct {
	EntityType         string
	EntityRef          string
	EntityLabel        string
	PositiveWeight     float64
	NegativeWeight     float64
	PositiveCount      int
	NegativeCount      int
	OppositionCount    int
	LastObservedAt     time.Time
	VerificationIDs    map[string]struct{}
	IndependentIssuers map[string]struct{}
}

// normalizeEvidenceAttributions stores only bounded, portable attribution
// fields. Claimed trust fields are intentionally not copied.
func normalizeEvidenceAttributions(sample map[string]any) []any {
	raw := firstPresentAny(sample["evidence_attribution"], sample["evidence_attributions"])
	rows := parseRows(raw)
	if len(rows) > evidenceReputationMaxAttributions {
		rows = rows[:evidenceReputationMaxAttributions]
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		stats := &portableRedactionStats{}
		portable := func(value any, limit int) string {
			return clipText(strings.TrimSpace(portableString(anyToString(value), stats)), limit)
		}
		entityType := strings.ToLower(portable(row["entity_type"], 32))
		if !containsString([]string{"candidate", "source", "file", "agent", "memory"}, entityType) {
			continue
		}
		entityID := portable(row["entity_id"], 360)
		if entityType == "candidate" {
			entityID = contextPackOpaqueCandidateRef(firstPresentAny(row["candidate_ref"], row["entity_id"]))
		}
		if entityID == "" {
			switch entityType {
			case "source":
				entityID = portable(row["source"], 160)
			case "file":
				entityID = portable(row["file"], 360)
			case "agent":
				entityID = portable(row["agent_id"], 160)
			case "memory":
				entityID = portable(row["memory_id"], 360)
			}
		}
		if entityID == "" {
			continue
		}
		method := strings.ToLower(portable(row["attribution_method"], 48))
		if !containsString([]string{"explicit_verified", "counterfactual", "leave_one_out", "citation_loss"}, method) {
			continue
		}
		role := strings.ToLower(portable(row["role"], 32))
		if role != "opposition" {
			role = "support"
		}
		verificationDigest := portable(firstPresentAny(row["verification_evidence_digest"], sample["verification_evidence_digest"], sample["evidence_digest"]), 80)
		verifierID := portable(firstPresentAny(row["verifier_id"], sample["verifier_id"]), 160)
		producerID := portable(firstPresentAny(row["producer_agent_id"], row["agent_id"], sample["agent_id"]), 160)
		issuer := portable(firstPresentAny(row["issuer"], sample["outcome_source"]), 160)
		normalized := map[string]any{
			"entity_type":                  entityType,
			"entity_id":                    entityID,
			"entity_ref":                   evidenceReputationOpaqueRef(entityType, entityID),
			"attribution_method":           method,
			"role":                         role,
			"issuer":                       issuer,
			"producer_agent_id":            producerID,
			"verifier_id":                  verifierID,
			"verification_evidence_digest": verificationDigest,
		}
		if entityType == "candidate" {
			normalized["candidate_ref"] = entityID
		}
		out = append(out, normalized)
	}
	return out
}

func evidenceReputationOpaqueRef(kind, value string) string {
	return "evr_" + sha256Hex(strings.ToLower(strings.TrimSpace(kind)) + "\x00" + strings.TrimSpace(value))[:24]
}

func buildEvidenceReputation(rows []map[string]any, options evidenceReputationOptions) map[string]any {
	asOf := options.AsOf.UTC()
	if options.AsOf.IsZero() {
		asOf = time.Unix(0, 0).UTC()
	}
	minimumSamples := clampInt(options.MinimumSamples, 2, 100)
	if options.MinimumSamples <= 0 {
		minimumSamples = evidenceReputationDefaultMinSample
	}
	maxEntries := clampInt(options.MaxEntries, 1, evidenceReputationMaxEntries)
	if options.MaxEntries <= 0 {
		maxEntries = 32
	}
	inputCount := len(rows)
	if len(rows) > evidenceReputationMaxRows {
		rows = rows[len(rows)-evidenceReputationMaxRows:]
	}
	project := strings.TrimSpace(options.Project)
	taskClass := strings.TrimSpace(options.TaskClass)
	retrievalIntent := strings.TrimSpace(options.RetrievalIntent)
	workspaceRef := contextPackLearnedDigestRef(options.WorkspaceRef)
	accumulators := map[string]*evidenceReputationAccumulator{}
	excluded := map[string]int{}

	for _, row := range rows {
		if project != "" && !strings.EqualFold(project, strings.TrimSpace(anyToString(row["project"]))) {
			continue
		}
		if taskClass != "" && !strings.EqualFold(taskClass, strings.TrimSpace(anyToString(row["task_class"]))) {
			continue
		}
		if retrievalIntent != "" && !strings.EqualFold(retrievalIntent, strings.TrimSpace(anyToString(row["retrieval_intent"]))) {
			continue
		}
		if workspaceRef != "" && contextPackLearnedDigestRef(anyToString(row["workspace_ref"])) != workspaceRef {
			excluded["workspace_scope_mismatch"]++
			continue
		}
		if !anyToBool(row["calibration_eligible"]) {
			excluded["calibration_ineligible"]++
			continue
		}
		utility := anyMap(firstPresentAny(row["verified_utility"], row["utility"]))
		verificationDigest := strings.TrimSpace(firstNonEmptyStrings(
			anyToString(row["verification_evidence_digest"]),
			anyToString(row["evidence_digest"]),
			anyToString(utility["verification_evidence_digest"]),
			anyToString(utility["evidence_digest"]),
		))
		verificationPassed := anyToBool(row["verification_passed"])
		if !verificationPassed {
			verificationPassed = anyToBool(utility["verification_passed"])
		}
		if !verificationPassed || !utilitySHA256DigestValid(verificationDigest) {
			excluded["verification_missing"]++
			continue
		}
		positive, negative := evidenceReputationOutcomePolarity(row)
		if !positive && !negative {
			excluded["outcome_unscored"]++
			continue
		}
		observedAt, ok := parseTimeBestEffort(firstNonEmptyStrings(anyToString(row["capturedAt"]), anyToString(row["captured_at"])))
		if !ok || observedAt.After(asOf.Add(time.Minute)) {
			excluded["timestamp_invalid"]++
			continue
		}
		ageDays := math.Max(0, asOf.Sub(observedAt.UTC()).Hours()/24)
		weight := math.Pow(0.5, ageDays/evidenceReputationHalfLifeDays)
		for _, rawAttribution := range contextPackAnyList(row["evidence_attribution"]) {
			attribution := anyMap(rawAttribution)
			entityType := strings.ToLower(strings.TrimSpace(anyToString(attribution["entity_type"])))
			entityID := strings.TrimSpace(anyToString(attribution["entity_id"]))
			method := strings.ToLower(strings.TrimSpace(anyToString(attribution["attribution_method"])))
			if entityID == "" || !containsString([]string{"candidate", "source", "file", "agent", "memory"}, entityType) ||
				!containsString([]string{"explicit_verified", "counterfactual", "leave_one_out", "citation_loss"}, method) {
				excluded["attribution_invalid"]++
				continue
			}
			verifiedCandidateIssuer := ""
			if entityType == "candidate" {
				if contextPackOpaqueCandidateRef(firstPresentAny(attribution["candidate_ref"], entityID)) == "" ||
					anyToString(attribution["result_level_credit"]) != "selection_receipt_bound" {
					excluded["candidate_unbound_to_selection_receipt"]++
					continue
				}
				if anyToString(attribution["selection_state"]) != "selected" {
					excluded["candidate_not_selected"]++
					continue
				}
				if !evidenceReputationCandidateUtilityVerified(row, attribution) {
					excluded["candidate_utility_verification_missing"]++
					continue
				}
				verifiedCandidateIssuer = strings.TrimSpace(anyToString(anyMap(row["candidate_utility_verification"])["verifier_id"]))
			}
			attributionDigest := strings.TrimSpace(firstNonEmptyStrings(anyToString(attribution["verification_evidence_digest"]), verificationDigest))
			if attributionDigest != verificationDigest {
				excluded["verification_mismatch"]++
				continue
			}
			verifierID := strings.TrimSpace(anyToString(attribution["verifier_id"]))
			if entityType == "candidate" {
				verifierID = verifiedCandidateIssuer
			}
			producerID := strings.TrimSpace(anyToString(attribution["producer_agent_id"]))
			verifierSubject := strings.TrimSpace(anyToString(attribution["verifier_id_subject_ref"]))
			producerSubject := strings.TrimSpace(anyToString(attribution["producer_agent_id_subject_ref"]))
			if (verifierSubject != "" && producerSubject != "" && verifierSubject == producerSubject) ||
				(verifierSubject == "" && producerSubject == "" && verifierID != "" && producerID != "" && strings.EqualFold(verifierID, producerID)) {
				excluded["self_attribution"]++
				continue
			}
			entitySubject := strings.TrimSpace(anyToString(attribution["entity_subject_ref"]))
			if entityType == "agent" && ((verifierSubject != "" && entitySubject != "" && verifierSubject == entitySubject) ||
				(verifierSubject == "" && entitySubject == "" && verifierID != "" && strings.EqualFold(verifierID, entityID))) {
				excluded["self_attribution"]++
				continue
			}
			key := entityType + "\x00" + strings.ToLower(entityID)
			acc := accumulators[key]
			if acc == nil {
				acc = &evidenceReputationAccumulator{
					EntityType: entityType, EntityRef: evidenceReputationOpaqueRef(entityType, entityID),
					EntityLabel: entityID, VerificationIDs: map[string]struct{}{}, IndependentIssuers: map[string]struct{}{},
				}
				accumulators[key] = acc
			}
			if _, duplicate := acc.VerificationIDs[verificationDigest]; duplicate {
				excluded["duplicate_verification"]++
				continue
			}
			acc.VerificationIDs[verificationDigest] = struct{}{}
			// Independence is a verifier property, never a reporter-selected
			// issuer label. Candidate rows tighten this further to the exact
			// reconciled Utility Ledger verifier above.
			issuer := verifierID
			if issuer != "" {
				acc.IndependentIssuers[strings.ToLower(issuer)] = struct{}{}
			}
			if strings.EqualFold(anyToString(attribution["role"]), "opposition") {
				acc.OppositionCount++
			}
			if positive {
				acc.PositiveCount++
				acc.PositiveWeight += weight
			} else {
				acc.NegativeCount++
				acc.NegativeWeight += weight
			}
			if observedAt.After(acc.LastObservedAt) {
				acc.LastObservedAt = observedAt.UTC()
			}
		}
	}

	entries := make([]map[string]any, 0, len(accumulators))
	for _, acc := range accumulators {
		sampleCount := acc.PositiveCount + acc.NegativeCount
		effective := acc.PositiveWeight + acc.NegativeWeight
		posterior := 0.5
		if effective > 0 {
			posterior = (acc.PositiveWeight + 1) / (effective + 2)
		}
		calibrated := sampleCount >= minimumSamples && len(acc.IndependentIssuers) >= 2
		confidence := math.Min(1, effective/float64(minimumSamples))
		proposedMultiplier := 1.0
		if calibrated {
			proposedMultiplier = clampFloat(1+(posterior-0.5)*0.3*confidence, 0.85, 1.15)
		}
		label := "uncalibrated"
		if calibrated {
			switch {
			case posterior >= 0.7:
				label = "reliable"
			case posterior < 0.4:
				label = "degraded"
			default:
				label = "contested"
			}
		}
		resultLevelCredit := "unbound_legacy"
		if acc.EntityType == "candidate" {
			resultLevelCredit = "selection_receipt_bound"
		}
		entries = append(entries, map[string]any{
			"reputation_id":            "rep_" + sha256Hex(acc.EntityType + "\x00" + acc.EntityRef)[:24],
			"entity_type":              acc.EntityType,
			"entity_ref":               acc.EntityRef,
			"entity_label":             acc.EntityLabel,
			"trust_label":              label,
			"sample_count":             sampleCount,
			"positive_count":           acc.PositiveCount,
			"negative_count":           acc.NegativeCount,
			"effective_sample_weight":  roundFloat(effective, 6),
			"reliability":              roundFloat(posterior, 6),
			"confidence":               roundFloat(confidence, 6),
			"independent_issuer_count": len(acc.IndependentIssuers),
			"opposition_count":         acc.OppositionCount,
			"last_observed_at":         acc.LastObservedAt.Format(time.RFC3339Nano),
			"calibrated":               calibrated,
			"result_level_credit":      resultLevelCredit,
			"bounded_influence": map[string]any{
				"proposed_multiplier": roundFloat(proposedMultiplier, 6),
				"minimum":             0.85, "maximum": 1.15, "applied": false, "advisory_only": true,
			},
			"self_awarded_trust_accepted": false,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if anyToBool(entries[i]["calibrated"]) != anyToBool(entries[j]["calibrated"]) {
			return anyToBool(entries[i]["calibrated"])
		}
		if anyToInt(entries[i]["sample_count"], 0) != anyToInt(entries[j]["sample_count"], 0) {
			return anyToInt(entries[i]["sample_count"], 0) > anyToInt(entries[j]["sample_count"], 0)
		}
		return anyToString(entries[i]["reputation_id"]) < anyToString(entries[j]["reputation_id"])
	})
	omitted := maxInt(0, len(entries)-maxEntries)
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}
	calibratedCount := 0
	for _, entry := range entries {
		if anyToBool(entry["calibrated"]) {
			calibratedCount++
		}
	}
	exclusionPayload := make(map[string]any, len(excluded))
	for reason, count := range excluded {
		exclusionPayload[reason] = count
	}
	scope := map[string]any{"project": project, "task_class": taskClass, "retrieval_intent": retrievalIntent}
	if workspaceRef != "" {
		scope["workspace_ref"] = workspaceRef
	}
	return map[string]any{
		"ok": true, "schema_id": evidenceReputationContractID, "version": 1,
		"generated_at": asOf.Format(time.RFC3339Nano),
		"scope":        scope,
		"policy": map[string]any{
			"minimum_samples": minimumSamples, "minimum_independent_issuers": 2,
			"decay_half_life_days": evidenceReputationHalfLifeDays,
			"advisory_only":        true, "ranking_influence_applied": false,
			"self_awarded_trust_accepted": false, "opposition_override_allowed": false,
		},
		"entries": entries,
		"summary": map[string]any{
			"input_row_count": inputCount, "evaluated_row_count": len(rows),
			"entry_count": len(entries), "calibrated_entry_count": calibratedCount,
			"entry_limit_omitted_count": omitted, "exclusions": exclusionPayload,
		},
		"measurement_limit": "Reputation is derived only from independently verified explicit attribution. It is advisory locally, cannot self-award trust, and never overrides contradiction or quarantine.",
	}
}

// Candidate result credit is admitted only after the existing Utility Ledger
// has reconciled a verifier event. Reporter-provided verification fields on an
// outcome are never sufficient for candidate reputation.
func evidenceReputationCandidateUtilityVerified(row, attribution map[string]any) bool {
	verification := anyMap(row["candidate_utility_verification"])
	if !anyToBool(verification["independently_verified"]) || anyToString(verification["verification_status"]) != "verified" {
		return false
	}
	rowBinding, rowBindingKey, rowBindingValid := canonicalTelemetryResponseBinding(row)
	verificationBinding, verificationBindingKey, verificationBindingValid := canonicalTelemetryResponseBinding(verification)
	if !rowBindingValid || !verificationBindingValid {
		return false
	}
	if rowBindingKey != "" || verificationBindingKey != "" {
		// A bound candidate credit must retain the complete canonical binding on
		// both sides of the Utility reconciliation. A digest/key alone cannot
		// turn a partial or missing binding into verified candidate evidence.
		if rowBinding == nil || verificationBinding == nil || rowBindingKey != verificationBindingKey ||
			!recallResponseBindingsEqual(rowBinding, verificationBinding) {
			return false
		}
	}
	if anyToString(verification["outcome_id"]) != anyToString(row["outcome_id"]) ||
		anyToString(verification["sample_id"]) != anyToString(row["sample_id"]) {
		return false
	}
	rowWorkspace := contextPackLearnedDigestRef(anyToString(row["workspace_ref"]))
	verificationWorkspace := contextPackLearnedDigestRef(anyToString(verification["workspace_ref"]))
	if (rowWorkspace != "" || verificationWorkspace != "") && rowWorkspace != verificationWorkspace {
		return false
	}
	digest := strings.TrimSpace(anyToString(verification["evidence_digest"]))
	if !utilitySHA256DigestValid(digest) || digest != anyToString(attribution["verification_evidence_digest"]) {
		return false
	}
	verifierID := strings.TrimSpace(anyToString(verification["verifier_id"]))
	return verifierID != "" && verifierID == anyToString(attribution["verifier_id"])
}

func evidenceReputationOutcomePolarity(row map[string]any) (bool, bool) {
	if anyToBool(row["first_pass_success"]) && !anyToBool(row["repair_required"]) {
		return true, false
	}
	if anyToBool(row["repair_required"]) {
		return false, true
	}
	class := strings.ToLower(strings.TrimSpace(anyToString(row["outcome_class"])))
	if class == "success" || class == "verified_success" {
		return true, false
	}
	if strings.Contains(class, "failure") || class == "repair_required" {
		return false, true
	}
	return false, false
}

func (s *server) evidenceReputationSnapshot(project, taskClass string, minimumSamples, limit int) map[string]any {
	return s.evidenceReputationSnapshotExact(project, taskClass, "", minimumSamples, limit)
}

func (s *server) evidenceReputationSnapshotExact(project, taskClass, retrievalIntent string, minimumSamples, limit int) map[string]any {
	rows := []map[string]any{}
	if s != nil && s.contextPackQuality != nil {
		rows, _ = s.contextPackQuality.receiptDurableOutcomeRows(evidenceReputationMaxRows)
	}
	rows = reconcileCandidateUtilityVerification(rows, utilityFromServer(s))
	return evidenceReputationSnapshotFromReconciledRows(
		rows, project, taskClass, retrievalIntent, minimumSamples, limit, time.Now().UTC(),
	)
}

func evidenceReputationSnapshotFromReconciledRows(
	rows []map[string]any,
	project, taskClass, retrievalIntent string,
	minimumSamples, limit int,
	asOf time.Time,
) map[string]any {
	return buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: project, TaskClass: taskClass, RetrievalIntent: retrievalIntent, AsOf: asOf,
		MinimumSamples: minimumSamples, MaxEntries: limit,
	})
}

func evidenceReputationSnapshotFromReconciledRowsForWorkspace(
	rows []map[string]any,
	project, taskClass, retrievalIntent, workspaceRef string,
	minimumSamples, limit int,
	asOf time.Time,
) map[string]any {
	workspaceRef = contextPackLearnedDigestRef(workspaceRef)
	if workspaceRef == "" {
		return map[string]any{}
	}
	return buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: project, TaskClass: taskClass, RetrievalIntent: retrievalIntent, WorkspaceRef: workspaceRef, AsOf: asOf,
		MinimumSamples: minimumSamples, MaxEntries: limit,
	})
}

func utilityFromServer(s *server) *utilityTelemetry {
	if s == nil {
		return nil
	}
	return s.utility
}

// canonicalTelemetryResponseBinding accepts the canonical top-level response
// binding carried by a durable outcome/Utility row. Search-impact projections
// may retain only response_binding_key, but every durable reputation/Utility
// join separately requires the complete binding.
func canonicalTelemetryResponseBinding(row map[string]any) (map[string]any, string, bool) {
	topLevel, topLevelValid := recallResponseBindingFromSample(row)
	if !topLevelValid {
		return nil, "", false
	}
	binding := topLevel
	key := ""
	if binding != nil {
		key = recallResponseBindingKey(binding)
		if key == "" {
			return nil, "", false
		}
	}

	if rawKey, present := row["response_binding_key"]; present {
		suppliedKey := strings.TrimSpace(anyToString(rawKey))
		if !utilitySHA256DigestValid(suppliedKey) || (key != "" && key != suppliedKey) {
			return nil, "", false
		}
		key = suppliedKey
	}
	return binding, key, true
}

// reconcileCandidateUtilityVerification is the sole join between candidate
// outcome attribution and the independent Utility Ledger verifier receipt.
// Callers may only treat a candidate as verified after the later predicate
// evidenceReputationCandidateUtilityVerified confirms this exact join.
func reconcileCandidateUtilityVerification(rows []map[string]any, utility *utilityTelemetry) []map[string]any {
	for _, row := range rows {
		if !evidenceReputationHasCandidateAttribution(row) {
			continue
		}
		// Any existing verification is reporter-provided until this pass joins
		// it to the current independent Utility observation. Never retain a
		// stale or forged verification when that join cannot be established.
		delete(row, "candidate_utility_verification")
		if utility == nil {
			continue
		}
		observation, found := utility.observation(anyToString(row["outcome_id"]))
		if !found {
			continue
		}
		rowWorkspace := contextPackLearnedDigestRef(anyToString(row["workspace_ref"]))
		observationWorkspace := contextPackLearnedDigestRef(anyToString(observation["workspace_ref"]))
		if (rowWorkspace != "" || observationWorkspace != "") && rowWorkspace != observationWorkspace {
			continue
		}
		rowBinding, rowBindingKey, rowBindingValid := canonicalTelemetryResponseBinding(row)
		observationBinding, observationBindingKey, observationBindingValid := canonicalTelemetryResponseBinding(observation)
		if !rowBindingValid || !observationBindingValid || rowBindingKey != observationBindingKey {
			continue
		}
		if rowBindingKey != "" && (rowBinding == nil || observationBinding == nil ||
			!recallResponseBindingsEqual(rowBinding, observationBinding)) {
			continue
		}
		claim := anyMap(observation["utility"])
		eligibility := anyMap(observation["eligibility"])
		denominator := anyMap(observation["denominator"])
		wireTokensExact := 0
		if anyToBool(denominator["wire_tokens_exact"]) {
			wireTokensExact = anyToInt(denominator["wire_tokens"], 0)
		}
		modelVisibleTokensExact := 0
		if anyToBool(denominator["model_visible_context_tokens_exact"]) {
			modelVisibleTokensExact = anyToInt(denominator["model_visible_context_tokens"], 0)
		}
		verification := map[string]any{
			"outcome_id":                         anyToString(observation["outcome_id"]),
			"sample_id":                          anyToString(observation["sample_id"]),
			"independently_verified":             anyToBool(claim["independently_verified"]),
			"verification_status":                anyToString(claim["verification_status"]),
			"evidence_digest":                    anyToString(claim["evidence_digest"]),
			"verifier_id":                        anyToString(claim["verifier_id"]),
			"observed_yield_eligible":            anyToBool(eligibility["observed_yield_eligible"]),
			"wire_tokens_exact":                  maxInt(0, wireTokensExact),
			"model_visible_context_tokens_exact": maxInt(0, modelVisibleTokensExact),
			"workspace_ref":                      observationWorkspace,
		}
		if observationBinding != nil && !recallResponseCopyBinding(verification, observationBinding) {
			continue
		}
		row["candidate_utility_verification"] = verification
	}
	return rows
}

func evidenceReputationHasCandidateAttribution(row map[string]any) bool {
	for _, attribution := range contextPackAnyList(row["evidence_attribution"]) {
		candidate := anyMap(attribution)
		if anyToString(candidate["entity_type"]) == "candidate" && anyToString(candidate["selection_state"]) == "selected" {
			return true
		}
	}
	return false
}

func evidenceReputationQueryInt(r *http.Request, key string, fallback, minimum, maximum int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, false
	}
	return value, true
}

func (s *server) telemetryEvidenceReputationRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	minimumSamples, ok := evidenceReputationQueryInt(r, "minimum_samples", evidenceReputationDefaultMinSample, 2, 100)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_minimum_samples"})
		return
	}
	limit, ok := evidenceReputationQueryInt(r, "limit", 32, 1, evidenceReputationMaxEntries)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_limit"})
		return
	}
	payload := s.evidenceReputationSnapshotExact(
		r.URL.Query().Get("project"), r.URL.Query().Get("task_class"), r.URL.Query().Get("retrieval_intent"), minimumSamples, limit,
	)
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(evidenceReputationContractID, payload, "", "evidence_reputation", evidenceReputationPath))
}
