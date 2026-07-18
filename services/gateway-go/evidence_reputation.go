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
	Project        string
	TaskClass      string
	AsOf           time.Time
	MinimumSamples int
	MaxEntries     int
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
		if !containsString([]string{"source", "file", "agent", "memory"}, entityType) {
			continue
		}
		entityID := portable(row["entity_id"], 360)
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
		out = append(out, map[string]any{
			"entity_type":                  entityType,
			"entity_id":                    entityID,
			"entity_ref":                   evidenceReputationOpaqueRef(entityType, entityID),
			"attribution_method":           method,
			"role":                         role,
			"issuer":                       issuer,
			"producer_agent_id":            producerID,
			"verifier_id":                  verifierID,
			"verification_evidence_digest": verificationDigest,
		})
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
	accumulators := map[string]*evidenceReputationAccumulator{}
	excluded := map[string]int{}

	for _, row := range rows {
		if project != "" && !strings.EqualFold(project, strings.TrimSpace(anyToString(row["project"]))) {
			continue
		}
		if taskClass != "" && !strings.EqualFold(taskClass, strings.TrimSpace(anyToString(row["task_class"]))) {
			continue
		}
		if !anyToBool(row["calibration_eligible"]) {
			excluded["calibration_ineligible"]++
			continue
		}
		verificationDigest := strings.TrimSpace(firstNonEmptyStrings(
			anyToString(row["verification_evidence_digest"]),
			anyToString(row["evidence_digest"]),
			anyToString(anyMap(firstPresentAny(row["verified_utility"], row["utility"]))["verification_evidence_digest"]),
		))
		verificationPassed := anyToBool(row["verification_passed"])
		if !verificationPassed {
			verificationPassed = anyToBool(anyMap(firstPresentAny(row["verified_utility"], row["utility"]))["verification_passed"])
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
			if entityID == "" || !containsString([]string{"source", "file", "agent", "memory"}, entityType) ||
				!containsString([]string{"explicit_verified", "counterfactual", "leave_one_out", "citation_loss"}, method) {
				excluded["attribution_invalid"]++
				continue
			}
			attributionDigest := strings.TrimSpace(firstNonEmptyStrings(anyToString(attribution["verification_evidence_digest"]), verificationDigest))
			if attributionDigest != verificationDigest {
				excluded["verification_mismatch"]++
				continue
			}
			verifierID := strings.TrimSpace(anyToString(attribution["verifier_id"]))
			producerID := strings.TrimSpace(anyToString(attribution["producer_agent_id"]))
			if verifierID != "" && producerID != "" && strings.EqualFold(verifierID, producerID) {
				excluded["self_attribution"]++
				continue
			}
			if entityType == "agent" && verifierID != "" && strings.EqualFold(verifierID, entityID) {
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
			issuer := strings.TrimSpace(firstNonEmptyStrings(anyToString(attribution["issuer"]), verifierID))
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
	return map[string]any{
		"ok": true, "schema_id": evidenceReputationContractID, "version": 1,
		"generated_at": asOf.Format(time.RFC3339Nano),
		"scope":        map[string]any{"project": project, "task_class": taskClass},
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
	rows := []map[string]any{}
	if s != nil && s.contextPackQuality != nil {
		rows = s.contextPackQuality.outcomeSourceRows(evidenceReputationMaxRows)
	}
	return buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: project, TaskClass: taskClass, AsOf: time.Now().UTC(),
		MinimumSamples: minimumSamples, MaxEntries: limit,
	})
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
	payload := s.evidenceReputationSnapshot(r.URL.Query().Get("project"), r.URL.Query().Get("task_class"), minimumSamples, limit)
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(evidenceReputationContractID, payload, "", "evidence_reputation", evidenceReputationPath))
}
